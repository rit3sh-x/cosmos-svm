package rpc

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"cosmossdk.io/log/v2"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// delegatedBytes is a token account with its delegate set.
func delegatedBytes(mint, owner, delegate [32]byte, amount uint64) []byte {
	data := tokenAccountBytes(mint, owner, amount, 165)
	binary.LittleEndian.PutUint32(data[72:], 1)
	copy(data[76:], delegate[:])
	return data
}

func answerValue(t *testing.T, out any, v any) {
	t.Helper()
	raw, _ := json.Marshal(out, json.Deterministic(true))
	var body struct {
		Value jsontext.Value `json:"value"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body.Value, v); err != nil {
		t.Fatalf("%v: %s", err, body.Value)
	}
}

func invoke(t *testing.T, s *Server, h handler, params ...any) (any, *rpcError) {
	t.Helper()
	var raw []jsontext.Value
	for _, p := range params {
		b, _ := json.Marshal(p, json.Deterministic(true))
		raw = append(raw, b)
	}
	out, err := h(context.Background(), s, raw)
	if err != nil {
		return nil, asRPCError(err)
	}
	return out, nil
}

// TestGetTokenSupplyAndLargestAccounts checks getTokenSupply, and that
// getTokenLargestAccounts returns a mint's top 20 holders by amount, then
// address. Both refuse with Agave's texts.
func TestGetTokenSupplyAndLargestAccounts(t *testing.T) {
	c, accounts := tokenCast()
	big := tokenKey(40)
	accounts = append(accounts, account(big, svm.SplToken, mintBytes(2, 0, 82)))
	var holders [][32]byte
	for i := 0; i < 25; i++ {
		h := tokenKey(byte(100 + i))
		holders = append(holders, h)
		// Holders come in pairs of equal amounts, so the address breaks ties.
		amount := uint64(i / 2 * 10)
		accounts = append(accounts, account(h, svm.SplToken, tokenAccountBytes(big, c.owner, amount, 165)))
	}
	native := tokenKey(41)
	accounts = append(accounts,
		account(native, svm.SplToken, tokenAccountBytes(nativeMint, c.owner, 5_000_000_000, 165)),
		account(tokenKey(42), svm.SystemProgram, mintBytes(0, 1, 82)),
		account(tokenKey(43), svm.SplToken, make([]byte, 82)))
	chain := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: accounts})
	s := New(chain.App, nil, log.NewNopLogger())
	chain.Block()

	out, err := invoke(t, s, getTokenSupply, b58(c.mint))
	if err != nil {
		t.Fatal(err)
	}
	var amount uiTokenAmount
	answerValue(t, out, &amount)
	if amount.Amount != "1000000" || amount.Decimals != 6 || amount.UIAmountString != "1" {
		t.Fatalf("the supply is %+v", amount)
	}
	for _, tt := range []struct {
		h    handler
		key  [32]byte
		want string
	}{
		{getTokenSupply, tokenKey(44), "Invalid param: could not find account"},
		{getTokenSupply, tokenKey(42), "Invalid param: not a Token mint"},
		{getTokenSupply, tokenKey(43), "Invalid param: mint could not be unpacked"},
		{getTokenLargestAccounts, tokenKey(44), "Invalid param: could not find mint"},
		{getTokenLargestAccounts, tokenKey(42), "Invalid param: not a Token mint"},
		{getTokenLargestAccounts, tokenKey(43), "Invalid param: Token mint could not be unpacked"},
	} {
		if _, err := invoke(t, s, tt.h, b58(tt.key)); err == nil || err.Message != tt.want {
			t.Errorf("%x: %v, want %q", tt.key[:4], err, tt.want)
		}
	}

	out, err = invoke(t, s, getTokenLargestAccounts, b58(big))
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Address string `json:"address"`
		uiTokenAmount
	}
	answerValue(t, out, &rows)
	type row struct {
		key    [32]byte
		amount uint64
	}
	var want []row
	for i, h := range holders {
		want = append(want, row{h, uint64(i / 2 * 10)})
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].amount != want[j].amount {
			return want[i].amount > want[j].amount
		}
		return string(want[i].key[:]) > string(want[j].key[:])
	})
	if len(rows) != numLargestAccounts {
		t.Fatalf("%d rows, want the cap of %d", len(rows), numLargestAccounts)
	}
	for i, r := range rows {
		if r.Address != b58(want[i].key) || r.Decimals != 2 || r.Amount != strconv.FormatUint(want[i].amount, 10) {
			t.Fatalf("row %d is %s %+v, want %s with %d", i, r.Address, r.uiTokenAmount, b58(want[i].key), want[i].amount)
		}
	}

	// The wrapped-SOL mint is answered without a read, at nine decimals.
	out, err = invoke(t, s, getTokenLargestAccounts, b58(nativeMint))
	if err != nil {
		t.Fatal(err)
	}
	answerValue(t, out, &rows)
	if len(rows) != 1 || rows[0].Address != b58(native) || rows[0].UIAmountString != "5" {
		t.Fatalf("the native mint's holders are %+v", rows)
	}

	// Without an index the answer is Agave's excluded-key refusal.
	s.app.Tokens = nil
	if _, err := invoke(t, s, getTokenLargestAccounts, b58(big)); err == nil || err.Code != codeKeyExcludedFromSecondaryIndex {
		t.Fatalf("no index answered %v", err)
	}
}

// TestGetTokenAccountsByDelegate checks the mint and program filters, a
// delegate that Revoke removes, and a client that hangs up mid-scan.
func TestGetTokenAccountsByDelegate(t *testing.T) {
	c, accounts := tokenCast()
	payer, holder := tc.Key(1), tc.Key(61)
	delegate, stranger := tokenKey(62), tokenKey(63)
	held, other, theirs := tokenKey(64), tokenKey(65), tokenKey(66)
	accounts = append(accounts,
		account(held, svm.SplToken, delegatedBytes(c.mint, tc.Pub(holder), delegate, 10)),
		account(other, svm.SplToken, delegatedBytes(c.otherMint, tc.Pub(holder), delegate, 20)),
		account(theirs, svm.SplToken, delegatedBytes(c.mint, tc.Pub(holder), stranger, 30)))
	chain := tc.Start(t, tc.Pub(payer), tc.Options{Accounts: accounts, Programs: tc.ManifestPrograms(t)})
	s := New(chain.App, nil, log.NewNopLogger())
	chain.Block()
	keys := func(filter map[string]any) []string {
		t.Helper()
		out, err := invoke(t, s, getTokenAccountsByDelegate, b58(delegate), filter, map[string]any{"encoding": "base64"})
		if err != nil {
			t.Fatal(err)
		}
		var rows []struct {
			Pubkey string `json:"pubkey"`
		}
		answerValue(t, out, &rows)
		var got []string
		for _, r := range rows {
			got = append(got, r.Pubkey)
		}
		return got
	}
	byMint := map[string]any{"mint": b58(c.mint)}
	byProgram := map[string]any{"programId": b58(svm.SplToken)}
	if got := keys(byMint); !slices.Equal(got, []string{b58(held)}) {
		t.Fatalf("by mint: %v", got)
	}
	both := []string{b58(held), b58(other)}
	slices.Sort(both)
	if got := keys(byProgram); !slices.Equal(got, both) {
		t.Fatalf("by program: %v, want %v sorted", got, both)
	}

	revoke := tc.Tx(chain.Latest(), payer, []ed25519.PrivateKey{holder}, tc.Instruction{
		Program:  svm.SplToken,
		Accounts: []tc.Meta{tc.Writable(held), tc.Signer(tc.Pub(holder), false)},
		Data:     []byte{5}, // Revoke
	})
	if res, _ := chain.Block(revoke); res[0].Code != 0 {
		t.Fatalf("Revoke: code %d", res[0].Code)
	}
	if got := keys(byMint); len(got) != 0 {
		t.Fatalf("a revoked delegate still holds %v", got)
	}
	if got := keys(byProgram); !slices.Equal(got, []string{b58(other)}) {
		t.Fatalf("by program after Revoke: %v", got)
	}

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	first, _ := json.Marshal(b58(delegate), json.Deterministic(true))
	second, _ := json.Marshal(byProgram, json.Deterministic(true))
	if _, err := getTokenAccountsByDelegate(gone, s, []jsontext.Value{first, second}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a scan for a client that hung up ended with %v", err)
	}
}

// nonCirculating sets the non-circulating list to keys for one test. The list
// this chain ships is empty, so the rule needs this to run.
func nonCirculating(t *testing.T, keys ...[32]byte) {
	accounts, decoded := nonCirculatingAccounts, nonCirculatingKeys
	t.Cleanup(func() { nonCirculatingAccounts, nonCirculatingKeys = accounts, decoded })
	nonCirculatingAccounts, nonCirculatingKeys = []string{}, keys
	for _, k := range keys {
		nonCirculatingAccounts = append(nonCirculatingAccounts, b58(k))
	}
}

func TestGetSupply(t *testing.T) {
	faucet := tc.Key(1)
	listed := tc.Pub(tc.Key(70))
	chain := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(chain.App, nil, log.NewNopLogger())
	chain.Block()
	if res, _ := chain.Block(tc.Tx(chain.Latest(), faucet, nil, tc.Transfer(faucet, listed, 3_000_000_000))); res[0].Code != 0 {
		t.Fatalf("funding a listed address: code %d", res[0].Code)
	}
	if err := s.notify(uint64(chain.Height)); err != nil {
		t.Fatal(err)
	}
	var total uint64
	if err := chain.App.Stores.OpenAccountScan(chain.App.CommitMultiStore()).Each(func(_ [32]byte, v []byte) error {
		total += binary.LittleEndian.Uint64(v[0:8])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	type figures struct {
		Total                  uint64   `json:"total"`
		Circulating            uint64   `json:"circulating"`
		NonCirculating         uint64   `json:"nonCirculating"`
		NonCirculatingAccounts []string `json:"nonCirculatingAccounts"`
	}
	var got figures
	out, err := invoke(t, s, getSupply)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(out, json.Deterministic(true)); !bytes.Contains(b, []byte(`"nonCirculatingAccounts":[]`)) {
		t.Fatalf("the shipped list is not an empty one: %s", b)
	}
	answerValue(t, out, &got)
	if got.Total != total || got.NonCirculating != 0 || got.Circulating != total {
		t.Fatalf("the supply is %+v, want all %d circulating", got, total)
	}

	nonCirculating(t, listed)
	out, err = invoke(t, s, getSupply)
	if err != nil {
		t.Fatal(err)
	}
	answerValue(t, out, &got)
	if got.Total != total || got.NonCirculating != 3_000_000_000 || got.Circulating != total-3_000_000_000 {
		t.Fatalf("the supply is %+v, want a total of %d with 3 SOL not circulating", got, total)
	}
	if !slices.Equal(got.NonCirculatingAccounts, []string{b58(listed)}) {
		t.Fatalf("non-circulating accounts %v", got.NonCirculatingAccounts)
	}
	out, _ = invoke(t, s, getSupply, map[string]any{"excludeNonCirculatingAccountsList": true})
	answerValue(t, out, &got)
	if len(got.NonCirculatingAccounts) != 0 {
		t.Fatalf("the list was not excluded: %d", len(got.NonCirculatingAccounts))
	}

	srv := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(srv.Close)
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/v0/total-supply"); code != 200 || body != balanceMessage(total) {
		t.Fatalf("/v0/total-supply: %d %q, want %q", code, body, balanceMessage(total))
	}
	if code, body := get("/v0/circulating-supply"); code != 200 || body != balanceMessage(total-3_000_000_000) {
		t.Fatalf("/v0/circulating-supply: %d %q", code, body)
	}

	// A deleted total is unknown until a block rebuilds it.
	if err := chain.TokenIndexDB().Delete([]byte{'m'}); err != nil {
		t.Fatal(err)
	}
	chain.Restart()
	s = New(chain.App, nil, log.NewNopLogger())
	if _, err := invoke(t, s, getSupply); err == nil || err.Code != codeNodeUnhealthy {
		t.Fatalf("an unknown total answered %v, want -32005", err)
	}
	srv.Config.Handler = http.HandlerFunc(s.serveHTTP)
	if code, body := get("/v0/total-supply"); code != 404 || body != "" {
		t.Fatalf("an unknown total on /v0: %d %q", code, body)
	}
}

func TestTheBalanceMessage(t *testing.T) {
	for lamports, want := range map[uint64]string{
		0: "0", 1: "0.000000001", 1_000_000_000: "1", 1_500_000_000: "1.5", 503000513276368201: "503000513.276368201",
	} {
		if got := balanceMessage(lamports); got != want {
			t.Errorf("%d lamports read %q, want %q", lamports, got, want)
		}
	}
}

// TestGetLargestAccounts proves the order, each filter, the two-hour cache,
// and one scan for callers that arrive together. A caller that hangs up stops
// its wait but not the scan.
func TestGetLargestAccounts(t *testing.T) {
	faucet := tc.Key(1)
	listed := tc.Pub(tc.Key(71))
	nonCirculating(t, listed)
	whale := tokenKey(90)
	chain := tc.Start(t, tc.Pub(faucet), tc.Options{Accounts: []svm.GenesisAccount{
		{Pubkey: b58(listed), Lamports: 9_000_000_000_000_000, Owner: b58(svm.SystemProgram), RentEpoch: ^uint64(0)},
		{Pubkey: b58(whale), Lamports: 8_000_000_000_000_000, Owner: b58(svm.SystemProgram), RentEpoch: ^uint64(0)},
	}})
	s := New(chain.App, nil, log.NewNopLogger())
	clock := &stubClock{now: time.Unix(1_700_000_000, 0)}
	s.largest.clock = clock
	chain.Block()
	largest := func(filter string) []string {
		t.Helper()
		var params []any
		if filter != "" {
			params = append(params, map[string]any{"filter": filter})
		}
		out, err := invoke(t, s, getLargestAccounts, params...)
		if err != nil {
			t.Fatal(err)
		}
		var rows []struct {
			Address  string `json:"address"`
			Lamports uint64 `json:"lamports"`
		}
		answerValue(t, out, &rows)
		var got []string
		for i, r := range rows {
			if r.Lamports == 0 || (i > 0 && r.Lamports > rows[i-1].Lamports) {
				t.Fatalf("%s: rows out of order or empty: %+v", filter, rows)
			}
			got = append(got, r.Address)
		}
		return got
	}
	// The test chain's faucet holds more than both, so it leads.
	if got := largest(""); slices.Index(got, b58(listed)) < 0 || slices.Index(got, b58(listed))+1 != slices.Index(got, b58(whale)) {
		t.Fatalf("no filter: %v", got)
	}
	if got := largest("circulating"); slices.Contains(got, b58(listed)) || !slices.Contains(got, b58(whale)) {
		t.Fatalf("circulating: %v", got)
	}
	if got := largest("nonCirculating"); !slices.Equal(got, []string{b58(listed)}) {
		t.Fatalf("nonCirculating: %v", got)
	}
	if _, err := invoke(t, s, getLargestAccounts, map[string]any{"filter": "all"}); err == nil || err.Code != codeInvalidParams {
		t.Fatalf("an unknown filter answered %v", err)
	}

	// An entry is served until it is two hours old, and scanned again then.
	scannedAt := func() time.Time {
		s.largest.mu.Lock()
		defer s.largest.mu.Unlock()
		return s.largest.entries[""].at
	}
	first := scannedAt()
	clock.advance(largestAccountsTTL - time.Second)
	largest("")
	if scannedAt() != first {
		t.Fatal("the entry was scanned again before two hours")
	}
	clock.advance(time.Second)
	largest("")
	if scannedAt() != clock.Now() {
		t.Fatal("an entry two hours old was served again")
	}

	// Callers that arrive while every scan slot is taken share one scan.
	clock.advance(largestAccountsTTL)
	held := cap(s.scans)
	for i := 0; i < held; i++ {
		s.scans <- struct{}{}
	}
	var wg sync.WaitGroup
	results := make([][]string, 2)
	for i := range results {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = largest("circulating") }()
	}
	var f *flight
	within(t, 30*time.Second, "the scan to start", func() bool {
		s.largest.mu.Lock()
		defer s.largest.mu.Unlock()
		f = s.largest.running
		return f != nil
	})
	// A caller that hangs up leaves. The scan goes on.
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := getLargestAccounts(gone, s, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("a caller that hung up got %v", err)
	}
	s.largest.mu.Lock()
	same := s.largest.running == f
	s.largest.mu.Unlock()
	if !same {
		t.Fatal("a second scan started while the first waited for a slot")
	}
	for i := 0; i < held; i++ {
		<-s.scans
	}
	wg.Wait()
	if !slices.Equal(results[0], results[1]) {
		t.Fatalf("callers sharing a scan got %v and %v", results[0], results[1])
	}
}
