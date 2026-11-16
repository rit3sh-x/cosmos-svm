package rpc

import (
	"encoding/binary"
	json "encoding/json/v2"
	"slices"
	"testing"

	"cosmossdk.io/log/v2"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

func wantErr(t *testing.T, s *Server, code int, message string, method string, params ...any) *rpcError {
	t.Helper()
	_, err := reply(t, s, method, params...)
	if err == nil || err.Code != code || (message != "" && err.Message != message) {
		t.Fatalf("%s %v: %v, want %d %q", method, params, err, code, message)
	}
	return err
}

// TestTheBlockRangeMethods checks getBlocks and getBlocksWithLimit over the
// gapless history, with Agave's caps, error texts and commitment rules.
func TestTheBlockRangeMethods(t *testing.T) {
	c, s := server(t)
	for c.Height < 6 {
		c.Block()
	}
	for h := uint64(1); h <= uint64(c.Height); h++ {
		if err := s.notify(h); err != nil {
			t.Fatal(err)
		}
	}
	committed := uint64(c.Height)
	blocks := func(method string, params ...any) []uint64 {
		t.Helper()
		var out []uint64
		mustAnswer(t, s, &out, method, params...)
		return out
	}
	upTo := func(from, to uint64) []uint64 { return heights(from, to) }
	for _, tt := range []struct {
		name   string
		method string
		params []any
		want   []uint64
	}{
		{"from genesis, which history does not hold", "getBlocks", []any{0}, upTo(1, committed)},
		{"an end above the committed height", "getBlocks", []any{2, 100}, upTo(2, committed)},
		{"an end below the start", "getBlocks", []any{5, 3}, []uint64{}},
		{"a start above the committed height", "getBlocks", []any{committed + 1}, []uint64{}},
		{"the config in the second place", "getBlocks", []any{3, map[string]any{"commitment": "confirmed"}}, upTo(3, committed)},
		{"null in the second place, the config in the third", "getBlocks", []any{3, nil, map[string]any{"minContextSlot": committed}}, upTo(3, committed)},
		{"a limit", "getBlocksWithLimit", []any{0, 2}, []uint64{1, 2}},
		{"a limit past the committed height", "getBlocksWithLimit", []any{4, 100}, upTo(4, committed)},
		{"a limit of zero", "getBlocksWithLimit", []any{1, 0}, []uint64{}},
	} {
		if got := blocks(tt.method, tt.params...); !slices.Equal(got, tt.want) {
			t.Errorf("%s: %s %v answered %v, want %v", tt.name, tt.method, tt.params, got, tt.want)
		}
	}

	below := "Method does not support commitment below `confirmed`"
	wantErr(t, s, codeInvalidParams, below, "getBlocks", 1, map[string]any{"commitment": "processed"})
	wantErr(t, s, codeInvalidParams, below, "getBlocks", 1, nil, map[string]any{"commitment": "processed"})
	wantErr(t, s, codeInvalidParams, below, "getBlocksWithLimit", 1, 2, map[string]any{"commitment": "processed"})
	wantErr(t, s, codeInvalidParams, "Invalid params: data did not match any variant of untagged enum RpcBlocksConfigWrapper.", "getBlocks", 1, "two")
	wantErr(t, s, codeInvalidParams, "Limit too large; max 500000", "getBlocksWithLimit", 1, 500_001)
	e := wantErr(t, s, codeMinContextSlot, "Minimum context slot has not been reached", "getBlocks", 1, nil, map[string]any{"minContextSlot": committed + 1})
	if got, _ := json.Marshal(e.Data, json.Deterministic(true)); string(got) != `{"contextSlot":`+jsonNumber(committed)+`}` {
		t.Fatalf("the -32016 data is %s", got)
	}
	wantErr(t, s, codeMinContextSlot, "", "getBlocksWithLimit", 1, 2, map[string]any{"minContextSlot": committed + 1})

	// The range cap applies after the server clamps the end to the committed
	// height, so the test sets a committed height above the cap.
	end := uint64(500_001)
	if _, err := blockRange(0, &end, 600_000); asRPCError(err).Message != "Slot range too large; max 500000" {
		t.Fatalf("a range past the cap returned %v", err)
	}
}

func jsonNumber(n uint64) string { b, _ := json.Marshal(n, json.Deterministic(true)); return string(b) }

// TestTheSlotGetSlotAnswersIsReadableBeforeFollowRuns checks that a client can
// read the block of a slot that getSlot returned, before Follow notifies any
// subscriber of that slot.
func TestTheSlotGetSlotAnswersIsReadableBeforeFollowRuns(t *testing.T) {
	c, s := server(t)
	c.Block()
	var slot uint64
	mustAnswer(t, s, &slot, "getSlot")
	if s.notified >= slot {
		t.Fatalf("notified is %d at slot %d, so this test proves nothing", s.notified, slot)
	}
	var at int64
	mustAnswer(t, s, &at, "getBlockTime", slot)
	var blocks []uint64
	mustAnswer(t, s, &blocks, "getBlocks", slot)
	if len(blocks) != 1 || blocks[0] != slot {
		t.Fatalf("getBlocks from %d is %v", slot, blocks)
	}
	var block map[string]any
	mustAnswer(t, s, &block, "getBlock", slot)
}

// TestGetBlockTimeAndTheLedgerFloor checks getBlockTime at genesis, inside the
// history, above it and below it. It also checks minimumLedgerSlot before and
// after the first block, and after a restore.
func TestGetBlockTimeAndTheLedgerFloor(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	s.SetGenesisTime(1_700_000_000)
	wantErr(t, s, codeInvalidRequest, "Invalid request", "minimumLedgerSlot")
	for c.Height < 4 {
		c.Block()
	}
	for h := uint64(1); h <= uint64(c.Height); h++ {
		if err := s.notify(h); err != nil {
			t.Fatal(err)
		}
	}
	var got int64
	mustAnswer(t, s, &got, "getBlockTime", 0)
	if got != 1_700_000_000 {
		t.Fatalf("slot 0 is %d, want the genesis time", got)
	}
	clock, ok := c.Account(svm.ClockID)
	if !ok {
		t.Fatal("no Clock")
	}
	mustAnswer(t, s, &got, "getBlockTime", c.Height)
	if want := int64(binary.LittleEndian.Uint64(clock.Data[32:40])); got != want {
		t.Fatalf("the committed block's time is %d, want the Clock's %d", got, want)
	}
	wantErr(t, s, codeBlockNotAvailable, "Block not available for slot 99", "getBlockTime", 99)
	var floor uint64
	mustAnswer(t, s, &floor, "minimumLedgerSlot")
	if floor != 1 {
		t.Fatalf("minimumLedgerSlot is %d, want the first available block", floor)
	}

	// A machine that state-synced at height 3 holds nothing below 4.
	if err := s.history().Restored(3); err != nil {
		t.Fatal(err)
	}
	wantErr(t, s, blockCleanedUp, "Block 2 cleaned up, does not exist on node. First available block: 4", "getBlockTime", 2)
	mustAnswer(t, s, &floor, "minimumLedgerSlot")
	if floor != 4 {
		t.Fatalf("after a restore at 3 minimumLedgerSlot is %d, want 4", floor)
	}
}

func TestTheMachineStatusMethods(t *testing.T) {
	c, s := server(t)
	c.Block()
	c.Block()
	wantErr(t, s, codeNoSnapshot, "No snapshot", "getHighestSnapshotSlot")
	var slot uint64
	mustAnswer(t, s, &slot, "getMaxShredInsertSlot")
	if slot != uint64(c.Height) {
		t.Fatalf("getMaxShredInsertSlot is %d, want the last block to reach the machine, %d", slot, c.Height)
	}
	mustAnswer(t, s, &slot, "getMaxRetransmitSlot")
	if slot != 0 {
		t.Fatalf("getMaxRetransmitSlot is %d", slot)
	}
	for method, want := range map[string]string{
		"getBlockCommitment": `{"commitment":null,"totalStake":0}`,
		"getAgGenesisCert":   `null`,
	} {
		raw, err := reply(t, s, method, 1)
		if err != nil || string(raw) != want {
			t.Errorf("%s answered %s %v, want %s", method, raw, err, want)
		}
	}
}

// TestRootSubscribeSendsEachHeight proves that rootSubscribe sends one bare
// height per commit, in ascending order, and that blockSubscribe is not served.
func TestRootSubscribeSendsEachHeight(t *testing.T) {
	c, s := server(t)
	p := dial(t, s)
	id := p.subscribe("rootSubscribe")
	from := uint64(c.Height) + 1
	for i := 0; i < 3; i++ {
		c.Block()
		if err := s.notify(uint64(c.Height)); err != nil {
			t.Fatal(err)
		}
	}
	for want := from; want < from+3; want++ {
		var n wsNotification
		p.read(&n)
		if n.Method != "rootNotification" || n.Params.Subscription != id || string(n.Params.Result) != jsonNumber(want) {
			t.Fatalf("notification %+v, want rootNotification of %d", n, want)
		}
	}
	if e := p.subscribeError("blockSubscribe", "all"); e == nil || e.Code != codeMethodNotFound {
		t.Fatalf("blockSubscribe answered %v, want -32601", e)
	}
}
