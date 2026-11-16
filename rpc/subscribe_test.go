package rpc

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"cosmossdk.io/log/v2"

	"github.com/gorilla/websocket"
	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// peer is one websocket client, dialled against the real serveWS, so the
// queue and its writer are on the tested path.
type peer struct {
	t    *testing.T
	conn *websocket.Conn
	next int
}

func dial(t *testing.T, s *Server) *peer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.serveWS))
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Close()
	})
	return &peer{t: t, conn: conn}
}

// subscribe sends one subscribe and returns the id it was acknowledged with.
func (p *peer) subscribe(method string, params ...any) uint64 {
	p.t.Helper()
	p.next++
	req := map[string]any{"jsonrpc": "2.0", "id": p.next, "method": method, "params": params}
	if err := p.conn.WriteJSON(req); err != nil {
		p.t.Fatal(err)
	}
	var resp struct {
		Result uint64    `json:"result"`
		Error  *rpcError `json:"error"`
	}
	p.read(&resp)
	if resp.Error != nil {
		p.t.Fatalf("%s: %v", method, resp.Error)
	}
	return resp.Result
}

func (p *peer) unsubscribe(method string, id uint64) bool {
	p.t.Helper()
	return p.unsubscribeError(method, id) == nil
}

// unsubscribeError returns the refusal an unsubscribe was answered with, or
// nil if it removed what it named.
func (p *peer) unsubscribeError(method string, id uint64) *rpcError {
	p.t.Helper()
	p.next++
	if err := p.conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": p.next, "method": method, "params": []any{id}}); err != nil {
		p.t.Fatal(err)
	}
	var resp struct {
		Result bool      `json:"result"`
		Error  *rpcError `json:"error"`
	}
	p.read(&resp)
	return resp.Error
}

func (p *peer) read(v any) {
	p.t.Helper()
	_ = p.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := p.conn.ReadJSON(v); err != nil {
		p.t.Fatal(err)
	}
}

// wsNotification is Agave's Notification envelope. The result stays raw, so
// each test reads the payload its own subscription defines.
type wsNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		Result       jsontext.Value `json:"result"`
		Subscription uint64         `json:"subscription"`
	} `json:"params"`
}

// collect reads exactly n notifications and groups them by method.
func (p *peer) collect(n int) map[string][]wsNotification {
	p.t.Helper()
	out := map[string][]wsNotification{}
	for i := 0; i < n; i++ {
		var got wsNotification
		p.read(&got)
		if got.JSONRPC != "2.0" || got.Method == "" {
			p.t.Fatalf("notification %d is %+v", i, got)
		}
		out[got.Method] = append(out[got.Method], got)
	}
	return out
}

// quiet asserts that nothing more arrives, which proves an unsubscribe took
// effect.
func (p *peer) quiet() {
	p.t.Helper()
	_ = p.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	var got wsNotification
	if err := p.conn.ReadJSON(&got); err == nil {
		p.t.Fatalf("a cancelled subscription still sent %s", got.Method)
	}
}

type uiResponse struct {
	Context struct {
		Slot uint64 `json:"slot"`
	} `json:"context"`
	Value jsontext.Value `json:"value"`
}

func valueOf(t *testing.T, n wsNotification) (uint64, jsontext.Value) {
	t.Helper()
	var r uiResponse
	if err := json.Unmarshal(n.Params.Result, &r); err != nil {
		t.Fatal(err)
	}
	return r.Context.Slot, r.Value
}

// TestADAppSubscribesToAccountsProgramsLogsAndSlots proves the four
// subscriptions carry mainnet's payloads, and each unsubscribe stops them.
func TestADAppSubscribesToAccountsProgramsLogsAndSlots(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()

	dest := tc.Pub(tc.Key(9))
	system := base58.Encode(svm.SystemProgram[:])
	p := dial(t, s)
	account := p.subscribe("accountSubscribe", base58.Encode(dest[:]), map[string]any{"encoding": "base64"})
	program := p.subscribe("programSubscribe", system, map[string]any{"encoding": "base64"})
	logs := p.subscribe("logsSubscribe", "all")
	slot := p.subscribe("slotSubscribe")

	amount := svm.MinimumBalance(0)
	tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest, amount))
	if res, _ := c.Block(tx); res[0].Code != 0 {
		t.Fatalf("the transfer committed as %d", res[0].Code)
	}
	height := uint64(c.Height)
	if err := s.notify(height); err != nil {
		t.Fatal(err)
	}

	// One account, two program, one log, one slot.
	got := p.collect(5)
	if len(got[accountNotification]) != 1 || len(got[programNotification]) != 2 ||
		len(got[logsNotification]) != 1 || len(got[slotNotification]) != 1 {
		t.Fatalf("the block produced %v", counts(got))
	}

	a := got[accountNotification][0]
	if a.Params.Subscription != account {
		t.Fatalf("the account notification names subscription %d, want %d", a.Params.Subscription, account)
	}
	at, value := valueOf(t, a)
	var ui uiAccount
	if err := json.Unmarshal(value, &ui); err != nil {
		t.Fatal(err)
	}
	if at != height || ui.Lamports != amount || ui.Owner != system || dataPair(t, ui.Data)[1] != "base64" {
		t.Fatalf("the account notification is slot %d %+v", at, ui)
	}

	var keyed struct {
		Pubkey  string    `json:"pubkey"`
		Account uiAccount `json:"account"`
	}
	seen := map[string]uint64{}
	for _, n := range got[programNotification] {
		if n.Params.Subscription != program {
			t.Fatalf("a program notification names subscription %d", n.Params.Subscription)
		}
		_, v := valueOf(t, n)
		if err := json.Unmarshal(v, &keyed); err != nil {
			t.Fatal(err)
		}
		seen[keyed.Pubkey] = keyed.Account.Lamports
	}
	if seen[base58.Encode(dest[:])] != amount {
		t.Fatalf("the program notifications are %v", seen)
	}
	payer := tc.Pub(faucet)
	if _, ok := seen[base58.Encode(payer[:])]; !ok {
		t.Fatalf("the payer's write was not notified: %v", seen)
	}

	_, v := valueOf(t, got[logsNotification][0])
	var logged struct {
		Signature string   `json:"signature"`
		Err       any      `json:"err"`
		Logs      []string `json:"logs"`
	}
	if err := json.Unmarshal(v, &logged); err != nil {
		t.Fatal(err)
	}
	sig := signatureOf(tx)
	if logged.Signature != base58.Encode(sig[:]) || logged.Err != nil || len(logged.Logs) == 0 {
		t.Fatalf("the log notification is %+v", logged)
	}

	var info struct {
		Slot   uint64 `json:"slot"`
		Parent uint64 `json:"parent"`
		Root   uint64 `json:"root"`
	}
	if err := json.Unmarshal(got[slotNotification][0].Params.Result, &info); err != nil {
		t.Fatal(err)
	}
	if info.Slot != height || info.Parent != height-1 || info.Root != height {
		t.Fatalf("the slot notification is %+v at height %d", info, height)
	}

	// Every unsubscribe removes what it names, and an id nobody holds gets
	// Agave's refusal.
	for _, u := range []struct {
		method string
		id     uint64
	}{{"accountUnsubscribe", account}, {"programUnsubscribe", program}, {"logsUnsubscribe", logs}, {"slotUnsubscribe", slot}} {
		if !p.unsubscribe(u.method, u.id) {
			t.Fatalf("%s did not remove %d", u.method, u.id)
		}
	}
	if err := p.unsubscribeError("accountUnsubscribe", 9999); err == nil ||
		err.Code != codeInvalidParams || err.Message != "Invalid subscription id." {
		t.Fatalf("an id nobody holds was answered %v", err)
	}
	tx = tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest, amount))
	if res, _ := c.Block(tx); res[0].Code != 0 {
		t.Fatalf("the second transfer committed as %d", res[0].Code)
	}
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	p.quiet()
}

func counts(got map[string][]wsNotification) map[string]int {
	out := map[string]int{}
	for k, v := range got {
		out[k] = len(v)
	}
	return out
}

// TestASubscribeNamingProcessedIsServed proves the three commitment levels
// collapse. A client that asks for Solana's default level is served, not
// refused or rewritten.
func TestASubscribeNamingProcessedIsServed(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()

	dest := tc.Pub(tc.Key(11))
	p := dial(t, s)
	p.subscribe("accountSubscribe", base58.Encode(dest[:]), map[string]any{"commitment": "processed"})
	p.subscribe("logsSubscribe", "allWithVotes", map[string]any{"commitment": "confirmed"})

	tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest, svm.MinimumBalance(0)))
	if res, _ := c.Block(tx); res[0].Code != 0 {
		t.Fatalf("the transfer committed as %d", res[0].Code)
	}
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	got := p.collect(2)
	if len(got[accountNotification]) != 1 || len(got[logsNotification]) != 1 {
		t.Fatalf("processed and confirmed produced %v", counts(got))
	}
}

// TestAMentionsFilterMatchesAnAddressThatSignsNothing proves logs match on
// the record's whole resolved address set, not on its signers alone.
func TestAMentionsFilterMatchesAnAddressThatSignsNothing(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()

	dest := tc.Pub(tc.Key(12))
	other := tc.Pub(tc.Key(13))
	p := dial(t, s)
	p.subscribe("logsSubscribe", map[string]any{"mentions": []string{base58.Encode(dest[:])}})
	p.subscribe("logsSubscribe", map[string]any{"mentions": []string{base58.Encode(other[:])}})

	tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest, svm.MinimumBalance(0)))
	if res, _ := c.Block(tx); res[0].Code != 0 {
		t.Fatalf("the transfer committed as %d", res[0].Code)
	}
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	got := p.collect(1)
	if len(got[logsNotification]) != 1 {
		t.Fatalf("the mentions filters produced %v", counts(got))
	}
	p.quiet()
}

// TestASubscriberThatStopsReadingIsDroppedAlone proves a peer that stops
// reading loses its own subscriptions and does not block other subscribers.
func TestASubscriberThatStopsReadingIsDroppedAlone(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()

	// The slow peer holds many subscriptions to the account every block
	// writes, so its queue fills in a few blocks, not a few thousand. Each
	// has its own dataSlice, so each is a distinct id. It then reads nothing.
	const slowSubs = 1_000
	dest := tc.Pub(tc.Key(14))
	slow := dial(t, s)
	for i := 0; i < slowSubs; i++ {
		slow.subscribe("accountSubscribe", base58.Encode(dest[:]), map[string]any{"dataSlice": map[string]int{"offset": i, "length": 0}})
	}
	fast := dial(t, s)
	fast.subscribe("slotSubscribe")

	for i := 0; i < 64 && accountSubscribers(s, dest) > 0; i++ {
		if res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest, svm.MinimumBalance(0)))); res[0].Code != 0 {
			t.Fatalf("block %d committed as %d", i, res[0].Code)
		}
		if err := s.notify(uint64(c.Height)); err != nil {
			t.Fatal(err)
		}
		// The fast peer is served every block. A shared writer would hang on
		// this read.
		var got wsNotification
		fast.read(&got)
		if got.Method != slotNotification {
			t.Fatalf("the fast peer received %s", got.Method)
		}
	}
	// The drop happens on the writer's goroutine, so poll until it is done.
	deadline := time.Now().Add(5 * time.Second)
	for accountSubscribers(s, dest) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got, slots := accountSubscribers(s, dest), slotSubscribers(s); got != 0 || slots != 1 {
		t.Fatalf("%d of the slow peer's subscriptions and %d slot subscriptions survive, want the fast peer's alone", got, slots)
	}
}

func accountSubscribers(s *Server, pk [32]byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.accounts[pk])
}

func slotSubscribers(s *Server) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.slots)
}

// TestZeroSubscribersCopyNothing proves that, with nobody holding the feed,
// a block that writes accounts publishes no write set.
func TestZeroSubscribersCopyNothing(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()

	dest := tc.Pub(tc.Key(15))
	transfer := func() {
		if res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest, svm.MinimumBalance(0)))); res[0].Code != 0 {
			t.Fatal("the transfer did not commit")
		}
	}
	transfer()
	if n := c.App.Feed.Buffered(); n != 0 {
		t.Fatalf("a block with no subscriber buffered %d accounts", n)
	}
	if c.App.Feed.Wants() {
		t.Fatal("the feed is held with no subscriber")
	}

	// One account subscription. The same block copies that account and
	// nothing else that it wrote: not the faucet and not the sysvars.
	p := dial(t, s)
	id := p.subscribe("accountSubscribe", base58.Encode(dest[:]))
	if !c.App.Feed.Wants() {
		t.Fatal("a subscriber does not hold the feed")
	}
	transfer()
	if n := c.App.Feed.Buffered(); n != 1 {
		t.Fatalf("a block that wrote one watched account buffered %d accounts", n)
	}
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	p.collect(1)
	if n := c.App.Feed.Buffered(); n != 0 {
		t.Fatalf("the drained feed still holds %d accounts", n)
	}

	// The last unsubscribe releases the feed.
	if !p.unsubscribe("accountUnsubscribe", id) {
		t.Fatal("the subscription was not removed")
	}
	if c.App.Feed.Wants() {
		t.Fatal("the feed is still held after the last unsubscribe")
	}
	transfer()
	if n := c.App.Feed.Buffered(); n != 0 {
		t.Fatalf("an unwatched block buffered %d accounts", n)
	}
}

// TestTheSameParamsShareOneId proves ids count from 0 and the same params
// share one id across connections. On one connection they are one
// subscription that notifies once.
func TestTheSameParamsShareOneId(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()
	dest := tc.Pub(tc.Key(31))
	key := base58.Encode(dest[:])
	a, b := dial(t, s), dial(t, s)
	first := a.subscribe("accountSubscribe", key)
	if again := a.subscribe("accountSubscribe", key, map[string]any{"encoding": "binary"}); again != first {
		t.Fatalf("the same params on one connection got %d and %d", first, again)
	}
	if other := b.subscribe("accountSubscribe", key); other != first {
		t.Fatalf("the same params on another connection got %d, want %d", other, first)
	}
	if distinct := b.subscribe("accountSubscribe", key, map[string]any{"encoding": "base64"}); distinct != first+1 {
		t.Fatalf("other params got %d, want the next id %d", distinct, first+1)
	}
	if first != 0 {
		t.Fatalf("the first id is %d, want 0 as Agave's tracker counts", first)
	}
	if !b.unsubscribe("accountUnsubscribe", first) {
		t.Fatal("the second holder could not cancel its hold")
	}
	if res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest, svm.MinimumBalance(0)))); res[0].Code != 0 {
		t.Fatalf("the transfer committed as %d", res[0].Code)
	}
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	if got := a.collect(1); len(got[accountNotification]) != 1 || got[accountNotification][0].Params.Subscription != first {
		t.Fatalf("the first holder got %v", got)
	}
	// The second connection's other subscription is served, and only once.
	if got := b.collect(1); got[accountNotification][0].Params.Subscription != first+1 {
		t.Fatalf("the second holder got %v", got)
	}
}

// TestLogsSubscribeRefusesAsAgaveDoes pins the logs filter's three refusal
// texts.
func TestLogsSubscribeRefusesAsAgaveDoes(t *testing.T) {
	_, s := server(t)
	p := dial(t, s)
	one, two := tc.Pub(tc.Key(1)), tc.Pub(tc.Key(2))
	for _, tt := range []struct {
		filter any
		want   string
	}{
		{map[string]any{"mentions": []string{base58.Encode(one[:]), base58.Encode(two[:])}}, "Invalid Request: Only 1 address supported"},
		{map[string]any{"mentions": []string{"0OIl"}}, "Invalid Request: Invalid mentions provided"},
		{"some", "Invalid params: unknown variant `some`, expected one of `all`, `allWithVotes`, `mentions`."},
	} {
		p.next++
		if err := p.conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": p.next, "method": "logsSubscribe", "params": []any{tt.filter}}); err != nil {
			t.Fatal(err)
		}
		var resp struct {
			Error *rpcError `json:"error"`
		}
		p.read(&resp)
		if resp.Error == nil || resp.Error.Code != codeInvalidParams || resp.Error.Message != tt.want {
			t.Errorf("%v: %+v, want %q", tt.filter, resp.Error, tt.want)
		}
	}
}

// TestASignatureIsReceivedBeforeItCommits proves that, with
// enableReceivedNotification, the subscriber gets receivedSignature while the
// block executes, then the result once it commits.
func TestASignatureIsReceivedBeforeItCommits(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()
	tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(32)), svm.MinimumBalance(0)))
	p := dial(t, s)
	id := p.subscribe("signatureSubscribe", base58.Encode(tx[1:65]), map[string]any{"enableReceivedNotification": true})
	if res, _ := c.Block(tx); res[0].Code != 0 {
		t.Fatalf("the transfer committed as %d", res[0].Code)
	}
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{`"receivedSignature"`, `{"err":null}`} {
		var n wsNotification
		p.read(&n)
		_, value := valueOf(t, n)
		if n.Params.Subscription != id || string(value) != want {
			t.Fatalf("notification %d is %s, want %s", i, n.Params.Result, want)
		}
	}
	if receivers(s) != 0 {
		t.Fatal("the hook stayed set after its last subscriber was notified")
	}
}

// TestSubscribersShareOneRendering proves that every subscription to one
// account in one encoding gets the same encoded result, whatever dataSlice it
// named. A block then holds one copy of a large account, however many
// subscriptions watch it.
func TestSubscribersShareOneRendering(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	pk := [32]byte{7}
	s.mu.Lock()
	defer s.mu.Unlock()
	for range 2 {
		conn := newWSConn(nil, DefaultWSMaxQueueBytes, time.Minute)
		for j := range 8 {
			cfg := accountConfig{Encoding: "base64", notification: true}
			s.register(&sub{conn: conn, method: accountNotification, key: pk, cfg: cfg, params: fmt.Sprintf("account|%d", j)})
		}
	}
	var owner [32]byte
	value := ffi.EncodeAccount(&owner, 1, 0, false, make([]byte, 20_488))
	sends := s.fanOut(1, []svm.AccountValue{{Pubkey: pk, Value: value}}, nil)
	results := map[*byte]bool{}
	for _, x := range sends {
		results[&x.note.result[0]] = true
	}
	if len(sends) != 16 || len(results) != 1 {
		t.Fatalf("%d notifications carried %d results, want 16 carrying 1", len(sends), len(results))
	}
}

// TestAFanOutAllocatesLittleForEachSubscription proves that one write to
// 10,000 subscriptions allocates little for each one. A subscription costs
// its place in the list, and the result is encoded once for all of them.
func TestAFanOutAllocatesLittleForEachSubscription(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	pk := [32]byte{7}
	conn := newWSConn(nil, DefaultWSMaxQueueBytes, time.Minute)
	const subs = 10_000
	s.mu.Lock()
	for i := range subs {
		cfg := accountConfig{Encoding: "base64", notification: true}
		s.register(&sub{conn: conn, method: accountNotification, key: pk, cfg: cfg, params: fmt.Sprintf("account|%d", i)})
	}
	s.mu.Unlock()
	var owner [32]byte
	writes := []svm.AccountValue{{Pubkey: pk, Value: ffi.EncodeAccount(&owner, 1, 0, false, make([]byte, 1024))}}
	c.App.Feed.Put(1, writes, nil)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	sends, err := s.collect(1, nil, false)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if each := (after.TotalAlloc - before.TotalAlloc) / subs; len(sends) != subs || each > 512 {
		t.Fatalf("%d notifications allocated %d bytes each", len(sends), each)
	}
}

// TestAnIdleConnectionHoldsNoQueue checks the cost of an idle connection. A
// connection used to allocate a channel of 4,096 messages (96 KiB) when it
// opened, also if it never got a message. Now its queue costs only what it
// holds.
func TestAnIdleConnectionHoldsNoQueue(t *testing.T) {
	conns := make([]*wsConn, 1000)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := range conns {
		conns[i] = newWSConn(nil, DefaultWSMaxQueueBytes, time.Minute)
	}
	runtime.ReadMemStats(&after)
	if each := (after.TotalAlloc - before.TotalAlloc) / uint64(len(conns)); each > 1<<10 {
		t.Fatalf("an idle connection allocated %d bytes", each)
	}
	runtime.KeepAlive(conns)
}

// TestANoteIsTheMessageJSONMarshalMakes proves that a notification built
// from its parts has the bytes that json.Marshal makes of the whole message.
func TestANoteIsTheMessageJSONMarshalMakes(t *testing.T) {
	for _, tt := range []struct {
		method string
		id     uint64
		result any
	}{
		{accountNotification, 0, notifyAt(7, map[string]any{"lamports": 1, "data": []string{"<&>", "base64"}})},
		{slotNotification, 1<<64 - 1, map[string]any{"slot": 9, "parent": 8, "root": 9}},
		{rootNotification, 42, 9},
		{logsNotification, 1_234_567, notifyAt(1, map[string]any{"err": nil, "logs": []string{"Program log: \u2028\""}})},
	} {
		result, err := json.Marshal(tt.result, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": tt.method,
			"params": map[string]any{"result": tt.result, "subscription": tt.id}}, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		n := note{method: tt.method, id: tt.id, result: result}
		if got := n.bytes(); string(got) != string(want) || n.size() != len(got) {
			t.Errorf("%s: built %s (size %d), json.Marshal makes %s", tt.method, got, n.size(), want)
		}
	}
}

// TestRemovingASubscriptionDoesNotCopyItsList proves that a removal does not
// copy its index list. A disconnect removes each subscription of the
// connection, and one index list can hold a million.
func TestRemovingASubscriptionDoesNotCopyItsList(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	s.mu.Lock()
	defer s.mu.Unlock()
	others := newWSConn(nil, DefaultWSMaxQueueBytes, time.Minute)
	for i := range 100_000 {
		s.register(&sub{conn: others, method: logsNotification, params: fmt.Sprintf("logs|%d", i)})
	}
	conn := newWSConn(nil, DefaultWSMaxQueueBytes, time.Minute)
	mine := make([]uint64, 10)
	for i := range mine {
		mine[i] = s.register(&sub{conn: conn, method: logsNotification, params: fmt.Sprintf("mine|%d", i)})
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for _, id := range mine {
		s.unregister(conn, id)
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Fatalf("removing 10 subscriptions allocated %d bytes", grew)
	}
	if len(s.logs) != 100_000 {
		t.Fatalf("%d logs subscriptions left, want 100,000", len(s.logs))
	}
	for i, x := range s.logs {
		if x.conn != others {
			t.Fatalf("logs subscription %d is one of the removed ten", i)
		}
	}
}

// TestTheReceivedHookNeverWaitsOnTheRegistry proves that the hook does not
// wait for the registry lock. The hook runs inside FinalizeBlock, and a
// client's subscriptions decide how long the lock is held.
func TestTheReceivedHookNeverWaitsOnTheRegistry(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	p := dial(t, s)
	var sig [64]byte
	p.subscribe("signatureSubscribe", base58.Encode(sig[:]), map[string]any{"enableReceivedNotification": true})
	s.mu.Lock()
	defer s.mu.Unlock()
	c.App.Feed.Received(1, func() [][64]byte { return [][64]byte{sig} })
}

func receivers(s *Server) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.receivers
}

// TestACommittedSignatureIsNotifiedAtTheNextCommit proves the next commit
// notifies a signature already committed, at that height. Agave's notifier
// finds it in its status cache on the next bank.
func TestACommittedSignatureIsNotifiedAtTheNextCommit(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()
	tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(33)), svm.MinimumBalance(0)))
	if res, _ := c.Block(tx); res[0].Code != 0 {
		t.Fatalf("the transfer committed as %d", res[0].Code)
	}
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	p := dial(t, s)
	p.subscribe("signatureSubscribe", base58.Encode(tx[1:65]))
	c.Block()
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	var n wsNotification
	p.read(&n)
	slot, value := valueOf(t, n)
	if slot != uint64(c.Height) || string(value) != `{"err":null}` {
		t.Fatalf("the notification is %s, want the result at the next commit, %d", n.Params.Result, c.Height)
	}
}

// TestNothingIsNotifiedBeforeItCanBeRead checks that the notifier follows the
// readable height, not LastBlockHeight. BaseApp.Commit moves LastBlockHeight
// when the multistore commits, and App.Commit records the readable height only
// after that returns.
func TestNothingIsNotifiedBeforeItCanBeRead(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()
	s.catchUp()
	c.Finalize()
	if _, err := c.App.BaseApp.Commit(); err != nil {
		t.Fatal(err)
	}
	readable := c.App.CommittedHeight()
	if uint64(c.App.LastBlockHeight()) != readable+1 {
		t.Fatalf("LastBlockHeight is %d and the readable height %d, want the window between them", c.App.LastBlockHeight(), readable)
	}
	s.catchUp()
	if s.notified != readable {
		t.Fatalf("notified height %d, but reads serve height %d", s.notified, readable)
	}
}

// TestTheBlockSysvarsAreNotifiedAtEveryBlock proves the begin-block writer's
// sysvars reach accountSubscribe at every block, empty or not, as on Agave.
// An ephemeral rollup validator tracks the base chain's slot through a Clock
// subscription. Each height notifies the Clock (holding that height),
// SlotHashes and RecentBlockhashes.
func TestTheBlockSysvarsAreNotifiedAtEveryBlock(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()
	p := dial(t, s)
	clock := p.subscribe("accountSubscribe", "SysvarC1ock11111111111111111111111111111111", map[string]any{"encoding": "base64"})
	hashes := p.subscribe("accountSubscribe", "SysvarS1otHashes111111111111111111111111111", map[string]any{"encoding": "base64"})
	recent := p.subscribe("accountSubscribe", "SysvarRecentB1ockHashes11111111111111111111", map[string]any{"encoding": "base64"})

	for i := 0; i < 3; i++ {
		var txs [][]byte
		if i == 1 {
			txs = append(txs, tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(9)), svm.MinimumBalance(0))))
		}
		c.Block(txs...)
		height := uint64(c.Height)
		if err := s.notify(height); err != nil {
			t.Fatal(err)
		}
		got := p.collect(3)[accountNotification]
		if len(got) != 3 {
			t.Fatalf("height %d (%d transactions) sent %d account notifications, want the Clock's, SlotHashes' and RecentBlockhashes'", height, len(txs), len(got))
		}
		seen := map[uint64]bool{}
		for _, n := range got {
			seen[n.Params.Subscription] = true
			at, value := valueOf(t, n)
			if at != height {
				t.Fatalf("a sysvar notification at height %d says slot %d", height, at)
			}
			if n.Params.Subscription != clock {
				continue
			}
			var ui uiAccount
			if err := json.Unmarshal(value, &ui); err != nil {
				t.Fatal(err)
			}
			data, err := base64.StdEncoding.DecodeString(dataPair(t, ui.Data)[0])
			if err != nil || len(data) < 8 || binary.LittleEndian.Uint64(data) != height {
				t.Fatalf("the Clock notified at height %d holds %x (%v)", height, data, err)
			}
		}
		if !seen[clock] || !seen[hashes] || !seen[recent] {
			t.Fatalf("height %d notified subscriptions %v, want %d, %d and %d", height, seen, clock, hashes, recent)
		}
	}
}
