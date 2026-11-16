//go:build devnet

package rpc

import (
	"context"
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/mr-tron/base58"
	"github.com/prometheus/client_golang/prometheus"

	"cosmossdk.io/log/v2"

	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cosmos/cosmos-sdk/client"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// keyFile copies the solana-keygen fixture into a temp dir with the given mode.
// Git does not keep group and other read bits, so the test sets the mode here.
func keyFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "faucet_key.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "faucet_key.json")
	if err := os.WriteFile(path, raw, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// fixturePubkey is `solana-keygen pubkey rpc/testdata/faucet_key.json`.
const fixturePubkey = "EwhQZt3wdG5GVrtVuYRvhFJdRhKsXji1ZSf9pxxYRkq8"

func restore(t *testing.T) {
	t.Helper()
	prev := loaded
	t.Cleanup(func() { loaded = prev })
}

// TestAKeySolanaKeygenWroteLoadsUnmodified checks that a file from
// `solana-keygen new` loads unchanged and opens the pubkey solana-keygen reports.
func TestAKeySolanaKeygenWroteLoadsUnmodified(t *testing.T) {
	restore(t)
	if err := LoadFaucet(keyFile(t, 0o600), nil, 0); err != nil {
		t.Fatal(err)
	}
	if FaucetPubkey() != fixturePubkey {
		t.Fatalf("the key opens %s, solana-keygen pubkey says %s", FaucetPubkey(), fixturePubkey)
	}
}

// TestUnsetMeansNoFaucet checks that a machine without a key gives Agave's
// answer for a node without a faucet. That is the normal state of every devnet
// machine but one.
func TestUnsetMeansNoFaucet(t *testing.T) {
	restore(t)
	if err := LoadFaucet("", nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := devnetMethod("requestAirdrop", "10.0.0.1:1"); ok {
		t.Fatal("a machine with no key served requestAirdrop")
	}
	s := &Server{logger: log.NewNopLogger()}
	params := []jsontext.Value{jsontext.Value(`"11111111111111111111111111111111"`), jsontext.Value(`1`)}
	resp := callOne(s, context.Background(), call{v2: true, id: []byte("1"), method: "requestAirdrop", params: encode(params)}, "10.0.0.1:1")
	if resp.Error == nil || resp.Error.Code != codeInvalidRequest || resp.Error.Message != "Invalid request" {
		t.Fatalf("requestAirdrop answered %+v, want Agave's -32600 Invalid request", resp.Error)
	}
}

// TestABrokenKeyRefusesToStartByName checks that each bad key file fails with
// an error that names the path and the reason.
func TestABrokenKeyRefusesToStartByName(t *testing.T) {
	restore(t)
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	sixtyFour := make([]int, 64)
	full, _ := json.Marshal(sixtyFour, json.Deterministic(true))
	short, _ := json.Marshal(sixtyFour[:32], json.Deterministic(true))

	cases := []struct{ name, path, says string }{
		{"missing", filepath.Join(dir, "absent.json"), "absent.json"},
		{"malformed", write("bad.json", "not json", 0o600), "solana-keygen's format"},
		{"the wrong length", write("short.json", string(short), 0o600), "32 bytes"},
		{"not a byte", write("big.json", strings.Replace(string(full), "0", "300", 1), 0o600), "not a byte"},
		{"group-readable", write("open.json", string(full), 0o640), "chmod 600"},
	}
	for _, c := range cases {
		err := LoadFaucet(c.path, nil, 0)
		if err == nil {
			t.Fatalf("%s: a key that cannot be used was accepted", c.name)
		}
		if !strings.Contains(err.Error(), c.path) || !strings.Contains(err.Error(), c.says) {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
}

// stubNode stands in for the mempool. It records each transaction that the
// faucet sends. The acceptance tests cover the real send path.
type stubNode struct {
	client.CometRPC
	sent []cmttypes.Tx
}

func (n *stubNode) BroadcastTxSync(_ context.Context, tx cmttypes.Tx) (*coretypes.ResultBroadcastTx, error) {
	n.sent = append(n.sent, tx)
	return &coretypes.ResultBroadcastTx{Code: 0}, nil
}

// TestAnAirdropIsAnOrdinaryTransferTheChainExecutes checks that the faucet does
// not mint. The chain executes the faucet transaction through the ordinary
// block path, and the recipient gains exactly the requested lamports.
func TestAnAirdropIsAnOrdinaryTransferTheChainExecutes(t *testing.T) {
	restore(t)
	key := tc.Key(7)
	hot := tc.Pub(key)
	to := tc.Pub(tc.Key(8))
	c := tc.Start(t, hot, tc.Options{})
	c.Block()

	f := &faucet{key: key, pubkey: hot, limits: newLimiter(defaultWindows(), defaultPerRequest)}
	ms := c.App.CommitMultiStore()
	blockhash := [32]byte(ms.GetKVStore(c.App.Stores.Meta).Get(svm.MetaBlockhashKey))
	before := balance(t, c, to)

	res, _ := c.Block(f.transfer(blockhash, to, 10*lamportsPerSOL))
	if len(res) != 1 || res[0].Code != 0 {
		t.Fatalf("the faucet's transfer was rejected: %+v", res)
	}
	if got := balance(t, c, to) - before; got != 10*lamportsPerSOL {
		t.Fatalf("the recipient gained %d lamports, the airdrop was %d", got, 10*lamportsPerSOL)
	}
}

// TestRequestAirdropSignsAndSends checks that requestAirdrop answers with a
// signature and hands one transaction to the send path.
func TestRequestAirdropSignsAndSends(t *testing.T) {
	restore(t)
	key := tc.Key(7)
	hot := tc.Pub(key)
	c := tc.Start(t, hot, tc.Options{})
	c.Block()
	loaded = &faucet{key: key, pubkey: hot, limits: newLimiter(defaultWindows(), defaultPerRequest)}

	node := &stubNode{}
	s := New(c.App, node, log.NewNopLogger())
	h, ok := devnetMethod("requestAirdrop", "10.0.0.1:1")
	if !ok {
		t.Fatal("a machine holding a key does not serve requestAirdrop")
	}
	to := tc.Pub(tc.Key(8))
	sig, err := h(context.Background(), s, jsonParams(t, base58.Encode(to[:]), lamportsPerSOL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base58.Decode(sig.(string)); err != nil {
		t.Fatalf("the answer %q is not a signature", sig)
	}
	if len(node.sent) != 1 {
		t.Fatalf("the faucet sent %d transactions", len(node.sent))
	}
}

func jsonParams(t *testing.T, values ...any) []jsontext.Value {
	t.Helper()
	out := make([]jsontext.Value, len(values))
	for i, v := range values {
		raw, err := json.Marshal(v, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		out[i] = raw
	}
	return out
}

func balance(t *testing.T, c *tc.Chain, pk [32]byte) uint64 {
	t.Helper()
	v := c.App.Stores.GetAccount(c.App.CommitMultiStore(), pk)
	if v == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(v[0:8])
}

// TestEitherCounterInEitherWindowRefuses checks that the request counter and
// the lamport counter each refuse, in each window. Agave counts only lamports,
// in one window, so a flood of one-lamport requests or a caller at the cap
// every minute passes it.
func TestEitherCounterInEitherWindowRefuses(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rows := []window{{time.Minute, 3, 10}, {time.Hour, 5, 100}}
	l := newLimiter(rows, 1_000)
	l.now = func() time.Time { return now }
	to := func(n byte) [32]byte { return [32]byte{n} }

	// The request counter refuses before the volume counter does.
	for i := 0; i < 3; i++ {
		if err := l.admit("10.0.0.1:1", to(byte(i)), 1); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	err := l.admit("10.0.0.1:1", to(9), 1)
	if err == nil || !strings.Contains(err.Error(), "requests") {
		t.Fatalf("a fourth request in the minute was admitted: %v", err)
	}

	// After the minute row rolls over, the hour row still refuses the caller.
	// This is why the table has more than one row.
	now = now.Add(time.Minute)
	if err := l.admit("10.0.0.1:1", to(10), 1); err != nil {
		t.Fatal(err)
	}
	if err := l.admit("10.0.0.1:1", to(11), 1); err != nil {
		t.Fatal(err)
	}
	if err := l.admit("10.0.0.1:1", to(12), 1); err == nil {
		t.Fatal("the hour row did not hold once the minute row had rolled")
	}

	// The volume counter also refuses on its own.
	l = newLimiter([]window{{time.Minute, 100, 10}}, 1_000)
	l.now = func() time.Time { return now }
	if err := l.admit("10.0.0.1:1", to(1), 9); err != nil {
		t.Fatal(err)
	}
	if err := l.admit("10.0.0.1:1", to(2), 2); err == nil || !strings.Contains(err.Error(), "lamports") {
		t.Fatalf("the volume cap did not hold: %v", err)
	}
	// A refusal charges nothing, so a request inside the remaining room still
	// passes.
	if err := l.admit("10.0.0.1:1", to(3), 1); err != nil {
		t.Fatalf("a refused request was charged: %v", err)
	}
}

// TestAPerRequestCapHoldsAndLoopbackIsExemptFromTheAddressHalf checks the
// per-request cap, the third of Agave's settings, and the loopback exemption
// that this faucet copies from Agave.
func TestAPerRequestCapHoldsAndLoopbackIsExemptFromTheAddressHalf(t *testing.T) {
	l := newLimiter([]window{{time.Minute, 1, 1_000}}, 10)
	if err := l.admit("127.0.0.1:1", [32]byte{1}, 11); err == nil {
		t.Fatal("a request over the per-request cap was admitted")
	}
	// Loopback is exempt from the caller half, so the machine does not
	// throttle a localnet test loop that runs on it. The recipient half still
	// applies.
	for i := 0; i < 5; i++ {
		if err := l.admit("127.0.0.1:1", [32]byte{byte(i)}, 1); err != nil {
			t.Fatalf("loopback request %d was throttled: %v", i, err)
		}
	}
	if err := l.admit("127.0.0.1:1", [32]byte{1}, 1); err == nil {
		t.Fatal("the recipient counter did not hold for a loopback caller")
	}
}

// TestACallerThroughTheGatewayIsLimitedByItsOwnAddress proves that the faucet
// limits a client behind the gateway by the client's address. Every request
// from the gateway comes from loopback, so the caller is the address that the
// gateway put in X-Forwarded-For.
func TestACallerThroughTheGatewayIsLimitedByItsOwnAddress(t *testing.T) {
	restore(t)
	key := tc.Key(7)
	hot := tc.Pub(key)
	c := tc.Start(t, hot, tc.Options{})
	c.Block()
	loaded = &faucet{key: key, pubkey: hot, limits: newLimiter([]window{{time.Minute, 1, 100 * lamportsPerSOL}}, defaultPerRequest)}
	node := &stubNode{}
	srv := httptest.NewServer(http.HandlerFunc(New(c.App, node, log.NewNopLogger()).serveHTTP))
	defer srv.Close()
	for _, step := range []struct {
		forwarded string
		to        byte
		sent      int
	}{
		{"203.0.113.7", 20, 1},
		// The same client again, to another recipient, is over its minute.
		{"203.0.113.7", 21, 1},
		// Another client is not.
		{"198.51.100.9", 22, 2},
		// A client cannot set its own address. The gateway adds the last one.
		{"198.51.100.10, 203.0.113.7", 23, 2},
	} {
		to := tc.Pub(tc.Key(step.to))
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"requestAirdrop","params":[%q,%d]}`, base58.Encode(to[:]), lamportsPerSOL)
		req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", step.forwarded)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if len(node.sent) != step.sent {
			t.Fatalf("after an airdrop for %s the faucet had sent %d, want %d", step.forwarded, len(node.sent), step.sent)
		}
	}
}

// TestAFloodOfFreshPubkeysDoesNotGrowTheMapAndTheIPCounterStillHolds runs the
// memory attack that Agave's unbounded `HashMap<Pubkey, u64>` permits. A fresh
// keypair per request costs nothing, so the address map has a bound. When the
// map is full, the limit falls back to the IP counter, not to nothing.
func TestAFloodOfFreshPubkeysDoesNotGrowTheMapAndTheIPCounterStillHolds(t *testing.T) {
	l := newLimiter([]window{{time.Hour, 1_000_000, 1_000_000}}, 10)
	l.maxAddrs = 64
	var refused int
	for i := 0; i < 100_000; i++ {
		var to [32]byte
		binary.LittleEndian.PutUint64(to[:], uint64(i))
		if err := l.admit("10.0.0.1:1", to, 1); err != nil {
			refused++
		}
	}
	if len(l.addrs) > l.maxAddrs {
		t.Fatalf("the address map holds %d keys against a bound of %d", len(l.addrs), l.maxAddrs)
	}
	if refused != 0 {
		t.Fatalf("%d requests inside every limit were refused", refused)
	}
	// The IP counter counted all 100,000 requests, so the fallback is safe.
	if got := l.ips["10.0.0.1"][0].requests; got != 100_000 {
		t.Fatalf("the IP counter saw %d of 100,000 requests", got)
	}
	l.rows[0].requests = 100_000
	if err := l.admit("10.0.0.1:1", [32]byte{255}, 1); err == nil {
		t.Fatal("the IP counter did not hold once the address map had overflowed")
	}
}

// TestTheIPMapIsSweptWhileRecipientsAreFew checks that the IP map sweeps itself.
// IPv6 callers use fewer recipients than the address map holds, so the address
// map never fills and never sweeps. The IP map's doubling sweep drops callers
// whose windows have elapsed.
func TestTheIPMapIsSweptWhileRecipientsAreFew(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newLimiter(defaultWindows(), defaultPerRequest)
	l.now = func() time.Time { return now }
	admit := func(from, to int) {
		for i := from; i < to; i++ {
			var key [32]byte
			binary.LittleEndian.PutUint64(key[:], uint64(i))
			if err := l.admit(fmt.Sprintf("[2001:db8::%x]:1", i), key, 1); err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
		}
	}
	admit(0, 5_000)
	now = now.Add(25 * time.Hour)
	admit(5_000, 10_000)
	if len(l.addrs) >= l.maxAddrs {
		t.Fatalf("the address map filled (%d keys), so this is not the case under test", len(l.addrs))
	}
	if len(l.ips) != 5_000 {
		t.Fatalf("the IP map holds %d callers, want the 5,000 whose windows are live", len(l.ips))
	}
}

// TestTheHotBalanceGaugeCarriesItsFloor checks that the balance gauge comes
// with the rent-exempt floor. A transfer that leaves the account below the
// floor fails, so an alert on zero reports funds while every request fails.
func TestTheHotBalanceGaugeCarriesItsFloor(t *testing.T) {
	restore(t)
	key := tc.Key(7)
	hot := tc.Pub(key)
	c := tc.Start(t, hot, tc.Options{})
	c.Block()
	f := &faucet{key: key, pubkey: hot, limits: newLimiter(defaultWindows(), defaultPerRequest)}
	s := New(c.App, nil, log.NewNopLogger())

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(&faucetCollector{s: s, f: f})
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, fam := range families {
		got[fam.GetName()] = fam.GetMetric()[0].GetGauge().GetValue()
	}
	if got["svm_faucet_lamports"] == 0 {
		t.Fatal("the balance gauge reads zero on a funded faucet")
	}
	if want := float64(svm.MinimumBalance(0)); got["svm_faucet_floor_lamports"] != want {
		t.Fatalf("the floor gauge reads %v, the rent-exempt minimum is %v", got["svm_faucet_floor_lamports"], want)
	}
	if got["svm_faucet_lamports"] <= got["svm_faucet_floor_lamports"] {
		t.Fatal("a funded faucet is already at its floor")
	}
}

// TestAHotFaucetAndAColdReserve checks that a refill from the cold reserve and
// a key rotation are ordinary transfers. Nothing mints, so the cold reserve is
// what survives a stolen faucet key. The chain does not enforce the split.
func TestAHotFaucetAndAColdReserve(t *testing.T) {
	restore(t)
	hotKey, coldKey := tc.Key(7), tc.Key(11)
	hot, cold := tc.Pub(hotKey), tc.Pub(coldKey)
	c := tc.Start(t, cold, tc.Options{Accounts: []svm.GenesisAccount{{
		Pubkey:    base58.Encode(hot[:]),
		Lamports:  50 * lamportsPerSOL,
		Owner:     base58.Encode(svm.SystemProgram[:]),
		RentEpoch: ^uint64(0),
	}}})
	c.Block()

	f := &faucet{key: hotKey, pubkey: hot, limits: newLimiter(defaultWindows(), defaultPerRequest)}
	to := tc.Pub(tc.Key(8))
	blockhash := func() [32]byte {
		return [32]byte(c.App.CommitMultiStore().GetKVStore(c.App.Stores.Meta).Get(svm.MetaBlockhashKey))
	}
	if res, _ := c.Block(f.transfer(blockhash(), to, 10*lamportsPerSOL)); res[0].Code != 0 {
		t.Fatalf("the hot faucet could not pay: %+v", res[0])
	}
	spent := balance(t, c, hot)

	// The refill. The reserve key never touches the faucet machine. The test
	// signs the transfer here as an operator does offline, and submits it
	// through the ordinary path.
	reserve := &faucet{key: coldKey, pubkey: cold}
	if res, _ := c.Block(reserve.transfer(blockhash(), hot, 100*lamportsPerSOL)); res[0].Code != 0 {
		t.Fatalf("the refill was rejected: %+v", res[0])
	}
	if got := balance(t, c, hot) - spent; got != 100*lamportsPerSOL {
		t.Fatalf("the refill moved %d lamports", got)
	}
	// Rotation needs no new genesis and no chain restart. It takes a new key
	// and one transfer from the reserve. After that, nothing points at the
	// compromised account.
	rotated := tc.Pub(tc.Key(12))
	if res, _ := c.Block(reserve.transfer(blockhash(), rotated, 100*lamportsPerSOL)); res[0].Code != 0 {
		t.Fatalf("the rotation transfer was rejected: %+v", res[0])
	}
	if balance(t, c, rotated) != 100*lamportsPerSOL {
		t.Fatal("the rotated faucet was not funded")
	}
}
