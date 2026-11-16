package rpc

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"runtime"
	"strings"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	cmted25519 "github.com/cometbft/cometbft/crypto/ed25519"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/mr-tron/base58"

	"cosmossdk.io/log/v2"

	"github.com/cosmos/cosmos-sdk/client"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

func validator(seed byte, power int64) *cmttypes.Validator {
	return cmttypes.NewValidator(cmted25519.PubKey(tc.OperatorKey(seed).Public().(ed25519.PublicKey)), power)
}

// update is the validator update a block returns.
func update(v *cmttypes.Validator, power int64) abci.ValidatorUpdate {
	return abci.UpdateValidator(v.PubKey.Bytes(), power, "ed25519")
}

// leaderChain starts a chain with the comet stand-in as its CometBFT, run by
// the first validator's machine.
func leaderChain(t *testing.T, vals ...*cmttypes.Validator) (*comet, *Server) {
	t.Helper()
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	m := newComet(c, vals[0].PubKey, vals...)
	return m, New(c.App, m, log.NewNopLogger())
}

// reply calls a method as a request does and returns its JSON result.
func reply(t *testing.T, s *Server, method string, params ...any) (jsontext.Value, *rpcError) {
	t.Helper()
	var raw []jsontext.Value
	for _, p := range params {
		b, _ := json.Marshal(p, json.Deterministic(true))
		raw = append(raw, b)
	}
	out, err := methods[method](context.Background(), s, raw)
	if err != nil {
		return nil, asRPCError(err)
	}
	b, _ := json.Marshal(out, json.Deterministic(true))
	return b, nil
}

func mustAnswer(t *testing.T, s *Server, v any, method string, params ...any) {
	t.Helper()
	raw, err := reply(t, s, method, params...)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v: %s", method, err, raw)
	}
}

func slotLeaders(t *testing.T, s *Server, start, limit uint64) []string {
	t.Helper()
	var out []string
	mustAnswer(t, s, &out, "getSlotLeaders", start, limit)
	if uint64(len(out)) != limit {
		t.Fatalf("getSlotLeaders(%d, %d) gave %d leaders", start, limit, len(out))
	}
	return out
}

// TestTheScheduleIsCometBFTsProposerOrder checks the schedule given at one
// height against the round-0 proposers that CometBFT then uses. It covers
// retained heights and the projection, for sets of equal and unequal power.
func TestTheScheduleIsCometBFTsProposerOrder(t *testing.T) {
	for _, tt := range []struct {
		name string
		vals []*cmttypes.Validator
	}{
		{"two equal", []*cmttypes.Validator{validator(0, 1), validator(1, 1)}},
		{"three to one", []*cmttypes.Validator{validator(0, 3), validator(1, 1)}},
		{"four unequal", []*cmttypes.Validator{validator(0, 7), validator(1, 3), validator(2, 2), validator(3, 1)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, s := leaderChain(t, tt.vals...)
			for range 6 {
				m.block(0)
			}
			got := slotLeaders(t, s, 1, 40)
			for range 40 {
				m.block(0)
			}
			for i, id := range got {
				if want := identity(m.proposer(int64(i + 1))); id != want {
					t.Fatalf("slot %d: the schedule at height 6 named %s, CometBFT proposed with %s", i+1, id, want)
				}
			}
			if tt.name == "two equal" {
				for i := 1; i < len(got); i++ {
					if got[i] == got[i-1] {
						t.Fatalf("two validators of equal power do not alternate at slot %d: %v", i+1, got)
					}
				}
			}
			if tt.name == "three to one" {
				// CometBFT gives 3,3,1,3 repeating.
				strong := identity(tt.vals[0])
				weak := 0
				for _, id := range got[:24] {
					if id != strong {
						weak++
					}
				}
				if weak != 6 {
					t.Fatalf("the power-1 validator led %d of 24 slots, want 6: %v", weak, got[:24])
				}
			}
		})
	}
}

// TestASetChangeTakesEffectTwoHeightsAfterItsBlock adds a validator in block 4
// and removes it in block 10. The validator leads only within heights 6 to 11,
// and the walk through both changes matches CometBFT at every height.
func TestASetChangeTakesEffectTwoHeightsAfterItsBlock(t *testing.T) {
	a, b, c := validator(0, 1), validator(1, 1), validator(2, 2)
	m, s := leaderChain(t, a, b)
	for range 3 {
		m.block(0)
	}
	m.block(0, update(c, 2))
	// At height 4 the schedule through 11 is block 4's update, applied at 6,
	// then projected. No block before block 10 changes the set again.
	projected := slotLeaders(t, s, 1, 11)
	for range 5 {
		m.block(0)
	}
	m.block(0, update(c, 0))
	for range 10 {
		m.block(0)
	}
	for i, id := range projected {
		if want := identity(m.proposer(int64(i + 1))); id != want {
			t.Fatalf("height %d: the schedule at height 4 named %s, CometBFT proposed with %s", i+1, id, want)
		}
	}
	got := slotLeaders(t, s, 1, uint64(m.height+2))
	for i, id := range got {
		height := int64(i + 1)
		if want := identity(m.proposer(height)); id != want {
			t.Fatalf("height %d: the schedule names %s, CometBFT proposed with %s", height, id, want)
		}
		if id == identity(c) && (height < 6 || height > 11) {
			t.Fatalf("the added validator leads height %d, outside its heights 6 to 11", height)
		}
	}
	if !strings.Contains(strings.Join(got[5:11], " "), identity(c)) {
		t.Fatalf("the added validator, with the most power, leads none of heights 6 to 11: %v", got[5:11])
	}
}

// TestABlockFromALaterRoundCountsAsASkip proposes block 5 in round 1. The
// schedule still names the round-0 leader, getSlotLeader names the validator
// that proposed it, and getBlockProduction counts the slot as that leader's
// skip.
func TestABlockFromALaterRoundCountsAsASkip(t *testing.T) {
	a, b := validator(0, 1), validator(1, 1)
	m, s := leaderChain(t, a, b)
	for range 4 {
		m.block(0)
	}
	m.block(1)
	scheduled := identity(m.proposer(5))
	var actual string
	mustAnswer(t, s, &actual, "getSlotLeader")
	if actual == scheduled || actual != identity(m.sets[5].CopyIncrementProposerPriority(1).GetProposer()) {
		t.Fatalf("getSlotLeader is %s; the round-0 leader of 5 is %s", actual, scheduled)
	}
	if got := slotLeaders(t, s, 5, 1)[0]; got != scheduled {
		t.Fatalf("a later round moved the schedule: slot 5 names %s, not %s", got, scheduled)
	}
	var production struct {
		Value struct {
			ByIdentity map[string][2]uint64 `json:"byIdentity"`
			Range      struct {
				FirstSlot uint64 `json:"firstSlot"`
				LastSlot  uint64 `json:"lastSlot"`
			} `json:"range"`
		} `json:"value"`
	}
	mustAnswer(t, s, &production, "getBlockProduction")
	if r := production.Value.Range; r.FirstSlot != 1 || r.LastSlot != 5 {
		t.Fatalf("the default range is %d..%d, want 1..5", r.FirstSlot, r.LastSlot)
	}
	var slots, produced uint64
	for id, e := range production.Value.ByIdentity {
		slots += e[0]
		produced += e[1]
		if id == scheduled && e[0]-e[1] != 1 {
			t.Fatalf("the round-0 leader of 5 has %v, want one skip", e)
		}
	}
	if slots != 5 || produced != 4 {
		t.Fatalf("five slots and four blocks by their leaders, got %d and %d", slots, produced)
	}
}

// TestTheLeaderMethodsRefuseAsAgaveDoes checks each edge case against Agave's
// refusal text.
func TestTheLeaderMethodsRefuseAsAgaveDoes(t *testing.T) {
	m, s := leaderChain(t, validator(0, 1), validator(1, 1))
	for range 8 {
		m.block(0)
	}
	m.prune(5)
	for _, tt := range []struct {
		method string
		params []any
		want   string
	}{
		{"getSlotLeaders", []any{6, 5001}, "Invalid limit; max 5000"},
		{"getSlotLeaders", []any{999999999, 1}, "Invalid slot range: leader schedule for epoch 2314 is unavailable"},
		{"getSlotLeaders", []any{2*432000 - 2, 5}, "Invalid slot range: leader schedule for epoch 2 is unavailable"},
		{"getSlotLeaders", []any{5, 1}, "Invalid slot range: leader schedule for epoch 0 is unavailable"},
		{"getLeaderSchedule", []any{nil, map[string]string{"identity": "notakey"}}, "Invalid param: WrongSize"},
		{"getBlockProduction", []any{map[string]any{"range": map[string]any{"firstSlot": 10, "lastSlot": 5}}}, "lastSlot, 5, cannot be less than firstSlot, 10"},
		{"getBlockProduction", []any{map[string]any{"range": map[string]any{"firstSlot": 5}}}, "firstSlot, 5, is too small; min 6"},
		{"getBlockProduction", []any{map[string]any{"range": map[string]any{"firstSlot": 6, "lastSlot": 9}}}, "lastSlot, 9, is too large; max 8"},
	} {
		_, err := reply(t, s, tt.method, tt.params...)
		if err == nil || err.Code != codeInvalidParams || err.Message != tt.want {
			t.Errorf("%s%v: got %v, want -32602 %q", tt.method, tt.params, err, tt.want)
		}
	}
	if got := slotLeaders(t, s, 6, 5); got[0] != identity(m.proposer(6)) {
		t.Fatalf("the oldest derivable slot names %s, not %s", got[0], identity(m.proposer(6)))
	}
	for _, params := range [][]any{{999999999}, {3 * 432000}} {
		if raw, err := reply(t, s, "getLeaderSchedule", params...); err != nil || string(raw) != "null" {
			t.Errorf("getLeaderSchedule%v is %s %v, want null for an epoch past the schedule", params, raw, err)
		}
	}
}

// TestTheLeaderScheduleGroupsAnEpochByLeader checks that the current epoch's
// schedule holds every slot from the oldest to the epoch's end, once, in the
// order getSlotLeaders gives.
func TestTheLeaderScheduleGroupsAnEpochByLeader(t *testing.T) {
	a, b := validator(0, 1), validator(1, 1)
	m, s := leaderChain(t, a, b)
	for range 3 {
		m.block(0)
	}
	var schedule map[string][]uint64
	mustAnswer(t, s, &schedule, "getLeaderSchedule")
	byIndex := map[uint64]string{}
	for id, slots := range schedule {
		for _, i := range slots {
			if _, twice := byIndex[i]; twice {
				t.Fatalf("slot index %d is in the schedule twice", i)
			}
			byIndex[i] = id
		}
	}
	if len(byIndex) != 432000-1 {
		t.Fatalf("the schedule holds %d slots, want every slot of epoch 0 from height 1", len(byIndex))
	}
	for i, id := range slotLeaders(t, s, 1, 5000) {
		if byIndex[uint64(i+1)] != id {
			t.Fatalf("slot %d: the schedule names %s, getSlotLeaders %s", i+1, byIndex[uint64(i+1)], id)
		}
	}
	var mine map[string][]uint64
	mustAnswer(t, s, &mine, "getLeaderSchedule", nil, map[string]string{"identity": identity(a)})
	if len(mine) != 1 || len(mine[identity(a)]) != len(schedule[identity(a)]) {
		t.Fatalf("the identity filter kept %d leaders", len(mine))
	}
	var none map[string][]uint64
	mustAnswer(t, s, &none, "getLeaderSchedule", map[string]string{"identity": base58.Encode(make([]byte, 32))})
	if none == nil || len(none) != 0 {
		t.Fatalf("a key that leads nothing gives %v, want {}", none)
	}
}

// TestALeaderWalkIsBoundedAsAScan proves that the leader methods take a scan
// slot and stop for a client that has gone. Each one walks up to two epochs of
// proposer steps.
func TestALeaderWalkIsBoundedAsAScan(t *testing.T) {
	for _, method := range []string{"getLeaderSchedule", "getSlotLeaders", "getBlockProduction"} {
		if !scanning(method, nil) {
			t.Errorf("%s takes no scan slot", method)
		}
	}
	m, s := leaderChain(t, validator(0, 1), validator(1, 1))
	m.block(0)
	ctx, hangUp := context.WithCancel(context.Background())
	hangUp()
	for _, method := range []string{"getLeaderSchedule", "getBlockProduction"} {
		if _, err := methods[method](ctx, s, nil); err == nil {
			t.Errorf("%s answered a client that had gone", method)
		}
	}
}

// TestBlockProductionReachesBackAsFarAsSlotHistory proves that the oldest slot
// of getBlockProduction is the oldest slot that Agave's SlotHistory holds.
func TestBlockProductionReachesBackAsFarAsSlotHistory(t *testing.T) {
	for h, want := range map[uint64]uint64{0: 0, 1<<20 - 1: 0, 1 << 20: 1, 2_000_000: 2_000_001 - 1<<20} {
		if got := historyFloor(h); got != want {
			t.Errorf("at height %d the oldest slot is %d, want %d", h, got, want)
		}
	}
}

// steady is a CometBFT whose validator set never changes. It has a header at
// every height up to top and makes each header when it is read. probe runs at
// every 64th read.
type steady struct {
	client.CometRPC
	set   *cmttypes.ValidatorSet
	top   int64
	reads int
	probe func()
}

func (n *steady) Validators(context.Context, *int64, *int, *int) (*coretypes.ResultValidators, error) {
	vals := n.set.Copy().Validators
	return &coretypes.ResultValidators{Validators: vals, Count: len(vals), Total: len(vals)}, nil
}

func (n *steady) BlockchainInfo(_ context.Context, lo, hi int64) (*coretypes.ResultBlockchainInfo, error) {
	if n.reads++; n.reads%64 == 0 {
		n.probe()
	}
	hash := n.set.Hash()
	hi = min(hi, n.top)
	var metas []*cmttypes.BlockMeta
	for h := hi; h >= max(lo, 1, hi-19); h-- {
		metas = append(metas, &cmttypes.BlockMeta{Header: cmttypes.Header{Height: h, ValidatorsHash: hash, NextValidatorsHash: hash}})
	}
	return &coretypes.ResultBlockchainInfo{LastHeight: n.top, BlockMetas: metas}, nil
}

// TestALeaderWalkHoldsOnePageOfHeaders proves that a walk of 2^17 heights
// holds the page it is on, not every header of its range (about 44 MB). The
// test measures the live heap during the walk, at every 64th read.
func TestALeaderWalkHoldsOnePageOfHeaders(t *testing.T) {
	const heights = 1 << 17
	n := &steady{set: cmttypes.NewValidatorSet([]*cmttypes.Validator{validator(0, 1), validator(1, 1)}), top: heights}
	v := &consensusView{s: &Server{node: n}, ctx: context.Background(), h: heights, oldest: 1, schedule: uiEpochSchedule{SlotsPerEpoch: heights}}
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	base, most := ms.HeapAlloc, uint64(0)
	n.probe = func() {
		runtime.GC()
		runtime.ReadMemStats(&ms)
		if ms.HeapAlloc > base {
			most = max(most, ms.HeapAlloc-base)
		}
	}
	leaders, err := v.proposers(1, heights)
	if err != nil {
		t.Fatal(err)
	}
	// The answer is a copied validator for each height. The walk holds that
	// and one page.
	runtime.GC()
	runtime.ReadMemStats(&ms)
	answer := uint64(0)
	if ms.HeapAlloc > base {
		answer = ms.HeapAlloc - base
	}
	runtime.KeepAlive(leaders)
	if len(leaders) != heights || most > answer+2<<20 {
		t.Fatalf("a walk of %d heights gave %d leaders in %d bytes and held %d bytes", heights, len(leaders), answer, most)
	}
}

// The schedule's arithmetic is Agave's with warmup off: slot 100 of 32-slot
// epochs is index 4 of epoch 3, which starts at slot 96.
func TestEpochScheduleArithmetic(t *testing.T) {
	e := uiEpochSchedule{SlotsPerEpoch: 32}
	if e.epoch(100) != 3 || e.slotIndex(100) != 4 || e.firstSlot(3) != 96 {
		t.Fatalf("slot 100 is epoch %d index %d, and epoch 3 starts at %d; want 3, 4 and 96",
			e.epoch(100), e.slotIndex(100), e.firstSlot(3))
	}
}

// The one EpochSchedule reader takes warmup as bincode takes a bool: 0 or 1,
// and nothing above.
func TestTheEpochScheduleWarmupIsABincodeBool(t *testing.T) {
	d := make([]byte, 33)
	for _, c := range []struct {
		warmup byte
		ok     bool
	}{{0, true}, {1, true}, {2, false}, {255, false}} {
		d[16] = c.warmup
		if e, ok := decodeEpochSchedule(d); ok != c.ok || e.Warmup != (c.warmup == 1) {
			t.Errorf("warmup %d decoded as %+v, %t; want %t", c.warmup, e, ok, c.ok)
		}
	}
	if _, ok := decodeEpochSchedule(d[:32]); ok {
		t.Error("a 32-byte EpochSchedule decoded")
	}
}

// TestTheEpochIdentityAndClusterAnswers checks getEpochSchedule, the
// getEpochInfo arithmetic, getIdentity, and a getClusterNodes list that a
// pending update grows.
func TestTheEpochIdentityAndClusterAnswers(t *testing.T) {
	a, b, c := validator(0, 1), validator(1, 1), validator(2, 1)
	m, s := leaderChain(t, a, b)
	for range 4 {
		m.block(0)
	}
	m.block(0, update(c, 1))
	var schedule map[string]any
	mustAnswer(t, s, &schedule, "getEpochSchedule")
	want := map[string]any{"slotsPerEpoch": 432000.0, "leaderScheduleSlotOffset": 432000.0, "warmup": false,
		"firstNormalEpoch": 0.0, "firstNormalSlot": 0.0}
	for k, v := range want {
		if schedule[k] != v {
			t.Errorf("getEpochSchedule %s is %v, want %v", k, schedule[k], v)
		}
	}
	var info map[string]any
	mustAnswer(t, s, &info, "getEpochInfo")
	for k, v := range map[string]any{"absoluteSlot": 5.0, "blockHeight": 5.0, "epoch": 0.0, "slotIndex": 5.0,
		"slotsInEpoch": 432000.0, "transactionCount": 0.0} {
		if got, ok := info[k]; !ok || got != v {
			t.Errorf("getEpochInfo %s is %v, want %v", k, got, v)
		}
	}
	var id map[string]string
	mustAnswer(t, s, &id, "getIdentity")
	if id["identity"] != identity(a) {
		t.Fatalf("getIdentity is %s, want this machine's consensus key %s", id["identity"], identity(a))
	}
	rpcAddr, wsAddr := "127.0.0.1:8899", "127.0.0.1:8900"
	s.addrs.rpc, s.addrs.ws = &rpcAddr, &wsAddr
	var nodes []map[string]any
	mustAnswer(t, s, &nodes, "getClusterNodes")
	if len(nodes) != 3 {
		t.Fatalf("getClusterNodes lists %d machines, want the two validators and the one block 5 adds", len(nodes))
	}
	for _, n := range nodes {
		if len(n) != 15 {
			t.Errorf("%v has %d fields, want RpcContactInfo's 15", n["pubkey"], len(n))
		}
		for _, k := range []string{"tpu", "tpuForwards", "tpuQuic", "tpuForwardsQuic", "gossip", "tvu", "tpuVote", "serveRepair", "clientId", "shredVersion"} {
			if n[k] != nil {
				t.Errorf("%v: %s is %v, want null", n["pubkey"], k, n[k])
			}
		}
		self := n["pubkey"] == identity(a)
		// This machine's featureSet comes from the linked crate. Another
		// machine's is unknown.
		if features, _ := n["featureSet"].(float64); self != (n["featureSet"] != nil && uint32(features) == ffi.FeatureSetID()) {
			t.Errorf("%v: featureSet is %v", n["pubkey"], n["featureSet"])
		}
		if self != (n["rpc"] == rpcAddr && n["pubsub"] == wsAddr && n["version"] == apiVersion) {
			t.Errorf("%v: rpc %v, pubsub %v, version %v", n["pubkey"], n["rpc"], n["pubsub"], n["version"])
		}
	}
}

// TestSlotsUpdatesSendFourTypesAtEachCommit subscribes and commits twice. Each
// height gives completed, createdBank, optimisticConfirmation and root, in
// that order, and the heights ascend.
func TestSlotsUpdatesSendFourTypesAtEachCommit(t *testing.T) {
	m, s := leaderChain(t, validator(0, 1))
	m.block(0)
	p := dial(t, s)
	id := p.subscribe("slotsUpdatesSubscribe")
	first := uint64(m.height + 1)
	for range 2 {
		m.block(0)
		if err := s.notify(uint64(m.height)); err != nil {
			t.Fatal(err)
		}
	}
	types := []string{"completed", "createdBank", "optimisticConfirmation", "root"}
	for i := range 8 {
		var n wsNotification
		p.read(&n)
		var u struct {
			Type      string  `json:"type"`
			Slot      uint64  `json:"slot"`
			Parent    *uint64 `json:"parent"`
			Timestamp uint64  `json:"timestamp"`
		}
		if err := json.Unmarshal(n.Params.Result, &u); err != nil {
			t.Fatal(err)
		}
		slot := first + uint64(i/4)
		if n.Method != "slotsUpdatesNotification" || n.Params.Subscription != id || u.Type != types[i%4] ||
			u.Slot != slot || u.Timestamp == 0 || (u.Parent != nil) != (u.Type == "createdBank") ||
			(u.Parent != nil && *u.Parent != slot-1) {
			t.Fatalf("notification %d is %s %s", i, n.Method, n.Params.Result)
		}
	}
	if !p.unsubscribe("slotsUpdatesUnsubscribe", id) {
		t.Fatal("slotsUpdatesUnsubscribe refused its own subscription")
	}
}
