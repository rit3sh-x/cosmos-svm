//go:build devnet

package rpc

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// window is one row of the faucet limit table. A devnet faucet is an
// unauthenticated endpoint that hands out lamports, so it ships with limits
// on. Agave's caps are `Option` and default to `None`, which is unlimited.
//
// This limiter departs from Agave's counting in two ways, both for reasons in
// Agave's source:
//   - Agave counts lamports only. This limiter also counts requests.
//   - Agave has one 60-second window. One short window bounds only a burst,
//     and a caller at the cap every minute stays inside it forever. One long
//     window permits the whole day's amount in one second. So the limit is a
//     table of windows, and it ships with two rows.
type window struct {
	dur      time.Duration
	requests uint64
	lamports uint64
}

// defaultWindows returns the two rows that ship. An operator can add a row,
// such as one hour or eight hours. Such a row lies between the two shipped
// bounds and adds no new property.
func defaultWindows() []window {
	return []window{
		{time.Minute, 5, 20 * lamportsPerSOL},
		{24 * time.Hour, 200, 200 * lamportsPerSOL},
	}
}

// defaultPerRequest is the per-request cap, Agave's third setting. It is the
// only one of the three that is not a window. It limits the harm of one large
// request.
const defaultPerRequest = 10 * lamportsPerSOL

// counter is the spend of one key (a caller IP or a recipient) inside one row.
// Each key has its own window start, not a global reset. Agave clears both
// maps at once, so a caller can spend a full cap on each side of the reset,
// which is twice the cap in a moment.
type counter struct {
	start    time.Time
	requests uint64
	lamports uint64
}

// limiter holds the request and lamport counters of each row, for each caller
// IP and each recipient.
//
// The address map has a bound, and Agave's does not. Agave's `address_cache`
// is a `HashMap<Pubkey, u64>` keyed by caller-chosen data, and a fresh keypair
// per request costs nothing. With a 60-second window the map stays small. With
// a 24-hour row it is a free memory attack, so a long window needs a bounded map.
type limiter struct {
	mu         sync.Mutex
	rows       []window
	perRequest uint64
	maxAddrs   int
	ips        map[string][]counter
	addrs      map[[32]byte][]counter
	// ipsSwept is the size of the IP map after the last sweep. The limiter
	// sweeps again when the map has doubled, so callers whose windows have
	// elapsed never use more memory than the live callers.
	ipsSwept int
	// now is time.Now outside tests.
	now func() time.Time
}

// maxAddrs is the bound on the address map. It is large, because the map stops
// an honest client's retry loop, not an attacker. When the map is full, the
// limit falls back to the IP counter, which does the main work.
const maxAddrs = 100_000

// minIPSweep is the smallest IP map that a doubling sweeps. A quiet faucet
// does not sweep on each new caller.
const minIPSweep = 1_024

func newLimiter(rows []window, perRequest uint64) *limiter {
	return &limiter{
		rows:       rows,
		perRequest: perRequest,
		maxAddrs:   maxAddrs,
		ips:        map[string][]counter{},
		addrs:      map[[32]byte][]counter{},
		now:        time.Now,
	}
}

// admit returns nil if a request is inside every limit, and then charges it. A
// refusal is a JSON-RPC error and sends no transaction. Agave answers an
// over-cap request with a signed SPL memo transaction that carries the error
// text, and its RPC submits it like any other. Each refusal then costs the same
// 5,000 lamports as a grant, so the cap itself drains the faucet.
func (l *limiter) admit(peer string, to [32]byte, lamports uint64) error {
	if lamports > l.perRequest {
		return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(
			"airdrop of %d lamports is over this faucet's per-request cap of %d", lamports, l.perRequest)}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	// Loopback is exempt from the caller IP counter only, as in Agave. The
	// machine does not throttle a localnet test loop that runs on it.
	ip := hostOf(peer)
	byIP := ip != "" && !isLoopback(ip)
	if _, seen := l.ips[ip]; byIP && !seen && len(l.ips) >= 2*max(l.ipsSwept, minIPSweep) {
		l.sweep()
	}
	_, known := l.addrs[to]
	byRecipient := known || l.room()

	if byIP {
		if err := check(l.rows, l.ips[ip], now, lamports, "this address"); err != nil {
			return err
		}
	}
	if byRecipient {
		if err := check(l.rows, l.addrs[to], now, lamports, "this recipient"); err != nil {
			return err
		}
	}
	// Both counters are charged only after both pass, so a request that one
	// refuses does not count against the other.
	if byIP {
		l.ips[ip] = charged(l.rows, l.ips[ip], now, lamports)
	}
	if byRecipient {
		l.addrs[to] = charged(l.rows, l.addrs[to], now, lamports)
	}
	return nil
}

// room reports whether the address map can take another key, and sweeps it
// when it is full. On overflow a new recipient gets only the IP counter, not
// nothing. The IP map is bounded by the addresses that can reach the machine,
// and it is the main limit. The recipient counter is weak. Pubkeys are free,
// so a fresh keypair per request defeats it. It stays because it stops
// an honest client's retry loop from emptying the account.
func (l *limiter) room() bool {
	if len(l.addrs) < l.maxAddrs {
		return true
	}
	l.sweep()
	return len(l.addrs) < l.maxAddrs
}

// sweep drops every key whose longest window has elapsed. It runs when the
// address map is full (room), and when the IP map has doubled since the last
// sweep (admit). The IP map needs no bound, because only addresses that can
// reach the machine enter it. But an IPv6 caller has many addresses, and a
// map that is swept only with the address map grows without limit while the
// recipients are few.
func (l *limiter) sweep() {
	now := l.now()
	longest := time.Duration(0)
	for _, r := range l.rows {
		if r.dur > longest {
			longest = r.dur
		}
	}
	for k, c := range l.addrs {
		if elapsed(c, now, longest) {
			delete(l.addrs, k)
		}
	}
	for k, c := range l.ips {
		if elapsed(c, now, longest) {
			delete(l.ips, k)
		}
	}
	l.ipsSwept = len(l.ips)
}

func elapsed(c []counter, now time.Time, longest time.Duration) bool {
	for _, e := range c {
		if now.Sub(e.start) < longest {
			return false
		}
	}
	return true
}

// rolled returns c with each elapsed row reset to start at now.
func rolled(rows []window, c []counter, now time.Time) []counter {
	out := make([]counter, len(rows))
	for i := range rows {
		if i < len(c) && now.Sub(c[i].start) < rows[i].dur {
			out[i] = c[i]
			continue
		}
		out[i] = counter{start: now}
	}
	return out
}

func check(rows []window, c []counter, now time.Time, lamports uint64, who string) error {
	r := rolled(rows, c, now)
	for i, row := range rows {
		if r[i].requests+1 > row.requests {
			return overCap(who, row, "requests", r[i].requests, row.requests)
		}
		if r[i].lamports+lamports > row.lamports {
			return overCap(who, row, "lamports", r[i].lamports, row.lamports)
		}
	}
	return nil
}

func charged(rows []window, c []counter, now time.Time, lamports uint64) []counter {
	r := rolled(rows, c, now)
	for i := range r {
		r[i].requests++
		r[i].lamports += lamports
	}
	return r
}

func overCap(who string, row window, what string, used, limit uint64) error {
	return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(
		"%s has used %d of %d %s in the last %s", who, used, limit, what, row.dur)}
}

// hostOf returns the host part of a "host:port" peer, or the whole string when
// it has no port.
func hostOf(peer string) string {
	if h, _, err := net.SplitHostPort(peer); err == nil {
		return h
	}
	return peer
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
