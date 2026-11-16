package rpc

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// The token and supply methods. They read the mint keyspace and the lamport
// total of the token index. Two of them scan committed state, where Agave
// scans too.

// numLargestAccounts is Agave's NUM_LARGEST_ACCOUNTS.
const numLargestAccounts = 20

// largestAccountsTTL is Agave's LARGEST_ACCOUNTS_CACHE_DURATION, two hours.
const largestAccountsTTL = 2 * time.Hour

var nonCirculatingKeys = func() [][32]byte {
	out := make([][32]byte, len(nonCirculatingAccounts))
	for i, a := range nonCirculatingAccounts {
		b, err := base58.Decode(a)
		if err != nil || len(b) != 32 {
			panic("non-circulating address " + a)
		}
		out[i] = [32]byte(b)
	}
	return out
}()

func isNonCirculating(k [32]byte) bool { return slices.Contains(nonCirculatingKeys, k) }

func excludedFromIndex(key [32]byte) error {
	return &rpcError{Code: codeKeyExcludedFromSecondaryIndex, Message: fmt.Sprintf(
		"%s excluded from account secondary indexes; this RPC method unavailable for key", base58.Encode(key[:]))}
}

// getTokenSupply reads the mint once. It renders the supply as
// getTokenAccountBalance renders an amount.
func getTokenSupply(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	mint, err := pubkeyParam(params, 0)
	if err != nil {
		return nil, err
	}
	ms, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	v := s.app.Stores.GetAccount(ms, mint)
	if v == nil {
		return nil, invalidParams("Invalid param: could not find account")
	}
	a, err := ffi.DecodeAccount(v)
	if err != nil {
		return nil, err
	}
	if !svm.IsTokenProgram(a.Owner) {
		return nil, invalidParams("Invalid param: not a Token mint")
	}
	base, _, ok := unpackMint(a.Data)
	if !ok {
		return nil, invalidParams("Invalid param: mint could not be unpacked")
	}
	reader := &mintReader{s: s, ms: ms, seen: map[[32]byte]*mintData{}}
	d, _ := mintDataFromAccount(a.Data, reader.clock())
	return at(h, amountToUI(base.supply, d)), nil
}

// tokenBalance is one row of getTokenLargestAccounts.
type tokenBalance struct {
	key    [32]byte
	amount uint64
}

// getTokenLargestAccounts reads the mint keyspace and reads and checks each
// hit again. It returns the top 20 by (amount, address), descending.
func getTokenLargestAccounts(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	mint, err := pubkeyParam(params, 0)
	if err != nil {
		return nil, err
	}
	ms, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	program, d, err := s.mintOwner(ms, mint)
	if err != nil {
		return nil, err
	}
	if !svm.IsTokenProgram(program) {
		return nil, invalidParams("Invalid param: not a Token mint")
	}
	accounts, err := s.mintAccounts(ms, program, mint)
	if err != nil {
		return nil, err
	}
	var rows []tokenBalance
	kept := scanned{limit: s.scanLimit}
	for _, x := range accounts {
		if err := kept.keep(len(x.account.Data)); err != nil {
			return nil, err
		}
		rows = append(rows, tokenBalance{x.key, x.token.Amount})
	}
	slices.SortFunc(rows, func(a, b tokenBalance) int {
		if a.amount != b.amount {
			return cmp.Compare(b.amount, a.amount)
		}
		return bytes.Compare(b.key[:], a.key[:])
	})
	// RpcTokenAccountBalance flattens the amount into the row.
	out := []map[string]any{}
	for _, r := range rows[:min(len(rows), numLargestAccounts)] {
		ui := amountToUI(r.amount, d)
		out = append(out, map[string]any{"address": base58.Encode(r.key[:]), "amount": ui.Amount,
			"decimals": ui.Decimals, "uiAmount": ui.UIAmount, "uiAmountString": ui.UIAmountString})
	}
	return at(h, out), nil
}

// mintHit is one token account the mint keyspace named, read again.
type mintHit struct {
	key     [32]byte
	account ffi.Account
	token   svm.TokenAccount
}

// mintAccounts is get_filtered_spl_token_accounts_by_mint on the mint
// keyspace. Each hit must still be a token account of program and of mint.
// With no index the answer is -32010. While the index rebuilds it is -32005.
func (s *Server) mintAccounts(ms storetypes.MultiStore, program, mint [32]byte) ([]mintHit, error) {
	if s.app.Tokens == nil {
		return nil, excludedFromIndex(mint)
	}
	keys, err := s.app.Tokens.MintAccounts(mint)
	if errors.Is(err, svm.ErrTokenIndexBehind) {
		return nil, nodeUnhealthy(nil)
	}
	if err != nil {
		return nil, err
	}
	var out []mintHit
	for _, key := range keys {
		v := s.app.Stores.GetAccount(ms, key)
		if v == nil {
			continue
		}
		a, err := ffi.DecodeAccount(v)
		if err != nil {
			return nil, err
		}
		token, ok := svm.ParseTokenAccount(a.Owner, a.Data)
		if !ok || a.Owner != program || token.Mint != mint {
			continue
		}
		out = append(out, mintHit{key, a, token})
	}
	return out, nil
}

// delegated applies Agave's two delegate filters. The delegate must be set, a
// u32 1 at offset 72, and must be the one asked for, at offset 76.
func delegated(data []byte, delegate [32]byte) bool {
	return len(data) >= 108 && binary.LittleEndian.Uint32(data[72:76]) == 1 && bytes.Equal(data[76:108], delegate[:])
}

// getTokenAccountsByDelegate reads the mint keyspace for {mint}. For
// {programId} it scans committed state for the program, as Agave does. The
// scan stops when the client hangs up.
func getTokenAccountsByDelegate(ctx context.Context, s *Server, params []jsontext.Value) (any, error) {
	delegate, err := pubkeyParam(params, 0)
	if err != nil {
		return nil, err
	}
	filter, err := tokenAccountsFilter(params, 1)
	if err != nil {
		return nil, err
	}
	var cfg accountConfig
	if err := param(params, 2, &cfg); err != nil {
		return nil, err
	}
	if cfg, err = cfg.or("binary"); err != nil {
		return nil, err
	}
	type hit struct {
		key     [32]byte
		account ffi.Account
	}
	var hits []hit
	var ms storetypes.MultiStore
	var h uint64
	kept := scanned{limit: s.scanLimit}
	if filter.mint != nil {
		if ms, h, err = s.snapshot(); err != nil {
			return nil, err
		}
		if err := contextAt(params, 2, h); err != nil {
			return nil, err
		}
		program, err := s.tokenProgram(ms, filter)
		if err != nil {
			return nil, err
		}
		found, err := s.mintAccounts(ms, program, *filter.mint)
		if err != nil {
			return nil, err
		}
		for _, x := range found {
			if delegated(x.account.Data, delegate) {
				if err := kept.keep(len(x.account.Data)); err != nil {
					return nil, err
				}
				hits = append(hits, hit{x.key, x.account})
			}
		}
	} else {
		program, err := s.tokenProgram(nil, filter)
		if err != nil {
			return nil, err
		}
		var scan *svm.AccountScan
		if ms, h, scan, err = s.app.ScanLatest(); err != nil {
			return nil, err
		}
		if err := contextAt(params, 2, h); err != nil {
			scan.Close()
			return nil, err
		}
		err = scan.Each(func(pk [32]byte, value []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			a, err := ffi.DecodeAccount(value)
			if err != nil {
				return err
			}
			if a.Owner == program && validTokenAccountFor(program, a.Data) && delegated(a.Data, delegate) {
				hits = append(hits, hit{pk, a})
				return kept.keep(len(a.Data))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		// Agave sorts the scan's answer by address. Each shard alone is sorted.
		slices.SortFunc(hits, func(a, b hit) int { return bytes.Compare(a.key[:], b.key[:]) })
	}
	out := []programAccount{}
	mints := s.mints(ms, cfg, true)
	for _, x := range hits {
		ui, err := renderAccount(x.key, x.account, cfg, mints)
		if err != nil {
			return nil, err
		}
		if mints != nil && !parsedBody(ui) {
			continue
		}
		out = append(out, programAccount{Account: ui, Pubkey: base58.Encode(x.key[:])})
	}
	return at(h, out), nil
}

// validTokenAccountFor is Agave's TokenAccountState filter for one program.
func validTokenAccountFor(program [32]byte, data []byte) bool {
	_, ok := svm.ParseTokenAccount(program, data)
	return ok
}

// supply holds getSupply's figures at the committed height h. They are the
// total from the token index and the lamports the non-circulating addresses
// hold.
type supply struct {
	total, nonCirculating uint64
}

// supplyAt reads the figures at ms, height h. It fails with
// ErrTokenIndexBehind while the total is unknown.
func (s *Server) supplyAt(ms storetypes.MultiStore, h uint64) (supply, error) {
	if s.app.Tokens == nil {
		return supply{}, svm.ErrTokenIndexBehind
	}
	total, err := s.app.Tokens.TotalAt(h)
	if err != nil {
		return supply{}, err
	}
	out := supply{total: total}
	for _, k := range nonCirculatingKeys {
		if v := s.app.Stores.GetAccount(ms, k); v != nil {
			out.nonCirculating += ffi.Lamports(v)
		}
	}
	return out, nil
}

func getSupply(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	var cfg struct {
		Commitment                        jsontext.Value `json:"commitment"`
		ExcludeNonCirculatingAccountsList bool           `json:"excludeNonCirculatingAccountsList"`
	}
	if err := param(params, 0, &cfg); err != nil {
		return nil, err
	}
	ms, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	sup, err := s.supplyAt(ms, h)
	if errors.Is(err, svm.ErrTokenIndexBehind) {
		return nil, nodeUnhealthy(nil)
	}
	if err != nil {
		return nil, err
	}
	list := []string{}
	if !cfg.ExcludeNonCirculatingAccountsList {
		list = nonCirculatingAccounts
	}
	return at(h, map[string]any{
		"total":                  sup.total,
		"circulating":            sup.total - sup.nonCirculating,
		"nonCirculating":         sup.nonCirculating,
		"nonCirculatingAccounts": list,
	}), nil
}

// lamportsPerSOL is LAMPORTS_PER_SOL.
const lamportsPerSOL = 1_000_000_000

// balanceMessage is Agave's build_balance_message(lamports, false, false). It
// writes SOL with nine places and trims trailing zeros.
func balanceMessage(lamports uint64) string {
	return trimUIAmount(strconv.FormatFloat(float64(lamports)/lamportsPerSOL, 'f', 9, 64), 9)
}

// v0Supply answers /v0/circulating-supply and /v0/total-supply. It returns
// false, for Agave's 404, if the figure is unknown.
func (s *Server) v0Supply(path string) (string, bool) {
	ms, h, err := s.snapshot()
	if err != nil {
		return "", false
	}
	sup, err := s.supplyAt(ms, h)
	if err != nil {
		return "", false
	}
	if path == "/v0/total-supply" {
		return balanceMessage(sup.total), true
	}
	return balanceMessage(sup.total - sup.nonCirculating), true
}

// largestAccounts is one cached getLargestAccounts answer.
type largestAccounts struct {
	height uint64
	rows   []map[string]any
	at     time.Time
}

// largest holds the getLargestAccounts cache, one entry per filter, and the
// one scan that refills it.
type largest struct {
	mu      sync.Mutex
	clock   quicClock
	entries map[string]largestAccounts
	// running is the scan in flight, if any.
	running *flight
}

// flight is one scan. done closes once the scan fills the cache or fails
// with err.
type flight struct {
	done chan struct{}
	err  error
}

// accountBalance is one account's lamports, for the ordering.
type accountBalance struct {
	key      [32]byte
	lamports uint64
}

// largerFirst orders by (lamports, address), descending.
func largerFirst(a, b accountBalance) int {
	if a.lamports != b.lamports {
		return cmp.Compare(b.lamports, a.lamports)
	}
	return bytes.Compare(b.key[:], a.key[:])
}

// top keeps the 20 largest by (lamports, address), descending.
func top(rows []accountBalance) []map[string]any {
	slices.SortFunc(rows, largerFirst)
	out := []map[string]any{}
	for _, r := range rows[:min(len(rows), numLargestAccounts)] {
		out = append(out, map[string]any{"address": base58.Encode(r.key[:]), "lamports": r.lamports})
	}
	return out
}

// scanLargest is the one pass that fills the unfiltered and circulating
// entries. It takes a scan slot. A zero-lamport account is absent here.
func (s *Server) scanLargest(f *flight) {
	l := &s.largest
	f.err = func() error {
		s.scans <- struct{}{}
		defer func() { <-s.scans }()
		_, h, scan, err := s.app.ScanLatest()
		if err != nil {
			return err
		}
		var all, circulating []accountBalance
		keep := func(rows []accountBalance, r accountBalance) []accountBalance {
			rows = append(rows, r)
			if len(rows) > 4*numLargestAccounts {
				rows = trim(rows)
			}
			return rows
		}
		err = scan.Each(func(pk [32]byte, value []byte) error {
			r := accountBalance{pk, ffi.Lamports(value)}
			all = keep(all, r)
			if !isNonCirculating(pk) {
				circulating = keep(circulating, r)
			}
			return nil
		})
		if err != nil {
			return err
		}
		now := l.clock.Now()
		l.mu.Lock()
		l.entries[""] = largestAccounts{h, top(all), now}
		l.entries["circulating"] = largestAccounts{h, top(circulating), now}
		l.mu.Unlock()
		return nil
	}()
	l.mu.Lock()
	l.running = nil
	l.mu.Unlock()
	close(f.done)
}

// trim keeps the 20 largest of rows, so a scan holds a bounded list.
func trim(rows []accountBalance) []accountBalance {
	slices.SortFunc(rows, largerFirst)
	return rows[:numLargestAccounts]
}

// getLargestAccounts keeps a cached answer for each filter for two hours. A
// hit ignores commitment and sortResults.
func getLargestAccounts(ctx context.Context, s *Server, params []jsontext.Value) (any, error) {
	var cfg struct {
		Commitment  jsontext.Value `json:"commitment"`
		Filter      *string        `json:"filter"`
		SortResults *bool          `json:"sortResults"`
	}
	if err := param(params, 0, &cfg); err != nil {
		return nil, err
	}
	filter := ""
	if cfg.Filter != nil {
		switch *cfg.Filter {
		case "circulating", "nonCirculating":
			filter = *cfg.Filter
		default:
			return nil, invalidParams("Invalid params: unknown variant `%s`, expected `circulating` or `nonCirculating`.", *cfg.Filter)
		}
	}
	l := &s.largest
	for {
		l.mu.Lock()
		if e, ok := l.entries[filter]; ok && l.clock.Now().Sub(e.at) < largestAccountsTTL {
			l.mu.Unlock()
			return at(e.height, e.rows), nil
		}
		if filter == "nonCirculating" {
			l.mu.Unlock()
			ms, h, err := s.snapshot()
			if err != nil {
				return nil, err
			}
			var rows []accountBalance
			for _, k := range nonCirculatingKeys {
				if v := s.app.Stores.GetAccount(ms, k); v != nil {
					rows = append(rows, accountBalance{k, ffi.Lamports(v)})
				}
			}
			e := largestAccounts{h, top(rows), l.clock.Now()}
			l.mu.Lock()
			l.entries[filter] = e
			l.mu.Unlock()
			return at(e.height, e.rows), nil
		}
		f := l.running
		if f == nil {
			f = &flight{done: make(chan struct{})}
			l.running = f
			go s.scanLargest(f)
		}
		l.mu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if f.err != nil {
			return nil, f.err
		}
	}
}
