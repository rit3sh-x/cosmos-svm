package app_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	upgradetypes "github.com/cosmos/cosmos-sdk/x/upgrade/types"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// The gates that these tests flip. All are inactive at genesis. The first two
// rent cuts are not here, because genesis activates them.
const (
	cpiNesting     = "6TkHkRmP7JZy1fdM6fg5uXn76wChQBWGokHBJzrLB3mj" // SIMD-0268, an SVMFeatureSet field
	rent2575       = "rntCigrTppP5JdZz7K8TyN9sMzLdAcXp8SejYpVpX6D"  // SIMD-0437-3, bank side, ported
	rent6960       = "rnt8ZQpz2HYhX3DkYBDGjJS1a36mYq69oXka7JrhEdi"  // SIMD-0438, the safeguard
	blockLimit100m = "P1BCUMpAC7V2GRBRiJCNUgpMyWZhoqt3LKo712ePqsz"  // SIMD-0286, bank side, not ported
)

// schedule writes a scheduled activation as the governance message does. The
// caller chooses `now`, so a test does not run the real 432,000-block delay to
// reach the flip. The delay is a rule of the message, and
// TestAScheduleIsRefused checks it.
func schedule(t *testing.T, c *tc.Chain, now, height int64, gates ...string) error {
	t.Helper()
	ctx := c.App.NewUncachedContext(false, cmtproto.Header{Height: c.Height})
	return c.App.Stores.ScheduleGates(ctx.MultiStore(), uint64(now), uint64(height), gates)
}

func activeAt(t *testing.T, c *tc.Chain, id string) (uint64, bool) {
	t.Helper()
	gates, err := c.App.Stores.ActiveGates(c.App.CommitMultiStore())
	if err != nil {
		t.Fatal(err)
	}
	want := key(t, id)
	for _, g := range gates {
		if g.ID == want {
			return g.Height, true
		}
	}
	return 0, false
}

// TestAGateFlipsAtItsHeight proves that a gate is off through H-1 and on from
// H, and that its feature account agrees with svmmeta. A machine restarted at
// H-1 replays the flip from its own disk. The corpus has no program that nests
// five CPIs deep, so the rent gate tests and engine/src/conformance.rs check
// the engine side of the swap.
func TestAGateFlipsAtItsHeight(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()
	const flip = 6
	if err := schedule(t, c, flip-svm.MinActivationDelay, flip, cpiNesting); err != nil {
		t.Fatal(err)
	}
	for c.Height < flip-1 {
		c.Block()
	}
	if _, on := activeAt(t, c, cpiNesting); on {
		t.Fatalf("the gate is active at height %d, before the flip at %d", c.Height, flip)
	}

	// A machine stopped at H-1 replays the flip from its own disk.
	before := c.Latest()
	c.Restart()
	if got := c.Latest(); got != before {
		t.Fatal("restart moved the chain")
	}
	c.Block()
	if c.Height != flip {
		t.Fatalf("at height %d, want %d", c.Height, flip)
	}
	at, on := activeAt(t, c, cpiNesting)
	if !on || at != flip {
		t.Fatalf("after the flip the gate is active=%v at %d, want true at %d", on, at, flip)
	}
	// The feature account agrees with svmmeta.
	a, ok := c.Account(key(t, cpiNesting))
	if !ok || a.Owner != svm.FeatureProgram || len(a.Data) != 9 || a.Data[0] != 1 ||
		binary.LittleEndian.Uint64(a.Data[1:9]) != flip {
		t.Fatalf("feature account after the flip: %+v", a)
	}
	gatesAgreeWithAccounts(t, c)
}

func TestAScheduleIsRefused(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()

	if err := schedule(t, c, c.Height, c.Height+10, cpiNesting); err == nil {
		t.Fatal("a flip ten blocks away was accepted")
	}
	// A bank-side gate with no ported read site would write a feature account
	// that claims behavior this chain does not run.
	if err := schedule(t, c, 0, svm.MinActivationDelay, blockLimit100m); err == nil {
		t.Fatal("raise_block_limits_to_100m was accepted as a target")
	}
	// Refuse a gate that genesis already activated at block 0.
	active, err := c.App.Stores.ActiveGates(c.App.CommitMultiStore())
	if err != nil || len(active) == 0 {
		t.Fatalf("no active gates to test against: %v", err)
	}
	if err := schedule(t, c, 0, svm.MinActivationDelay, base58Of(active[0].ID)); err == nil {
		t.Fatal("an already-active gate was accepted as a target")
	}

	// A cancel before the height leaves the gate off.
	const flip = 5
	if err := schedule(t, c, flip-svm.MinActivationDelay, flip, cpiNesting); err != nil {
		t.Fatal(err)
	}
	ctx := c.App.NewUncachedContext(false, cmtproto.Header{Height: c.Height})
	// A cancel that matches no schedule drops nothing. It is refused, so it
	// cannot pass as the undo of a flip that still happens.
	if err := c.App.Stores.CancelGates(ctx.MultiStore(), flip+1, []string{cpiNesting}); err == nil ||
		!strings.Contains(err.Error(), "scheduled at height 5, not 6") {
		t.Fatalf("a cancel at the wrong height: %v", err)
	}
	if err := c.App.Stores.CancelGates(ctx.MultiStore(), flip, []string{rent1322}); err == nil ||
		!strings.Contains(err.Error(), "not scheduled") {
		t.Fatalf("a cancel of a gate nothing scheduled: %v", err)
	}
	if err := c.App.Stores.CancelGates(ctx.MultiStore(), flip, []string{cpiNesting}); err != nil {
		t.Fatal(err)
	}
	for c.Height < flip+1 {
		c.Block()
	}
	if _, on := activeAt(t, c, cpiNesting); on {
		t.Fatalf("a cancelled gate activated at height %d", flip)
	}
}

// TestARentGateRepricesAtItsHeight proves that a rent gate changes lamports per
// byte at its height, for the Rent sysvar and getMinimumBalanceForRentExemption
// alike. If the safeguard and a cut flip in the same block, the safeguard wins,
// as in Agave.
func TestARentGateRepricesAtItsHeight(t *testing.T) {
	perByte := func(c *tc.Chain) uint64 { return mustPerByte(t, c) }

	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()
	const flip = 5
	if err := schedule(t, c, flip-svm.MinActivationDelay, flip, rent2575); err != nil {
		t.Fatal(err)
	}
	for c.Height < flip-1 {
		c.Block()
	}
	if got := perByte(c); got != 5_080 {
		t.Fatalf("lamports per byte is %d at height %d, want the genesis 5,080", got, c.Height)
	}
	c.Block()
	if got := perByte(c); got != 2_575 {
		t.Fatalf("lamports per byte is %d after the flip, want 2,575", got)
	}
	if got, want := svm.MinimumBalance(0), uint64(128*5_080); got != want {
		t.Fatalf("the genesis price moved: %d, want %d", got, want)
	}

	// The safeguard in the same block as a cut leaves the legacy value.
	both := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	both.Block()
	if err := schedule(t, both, flip-svm.MinActivationDelay, flip, rent2575, rent6960); err != nil {
		t.Fatal(err)
	}
	for both.Height < flip {
		both.Block()
	}
	if got := perByte(both); got != 6_960 {
		t.Fatalf("a cut and the safeguard in one block left %d, want 6,960", got)
	}
}

// TestGenesisStartsAtMainnetsRent proves that the two rent cuts that mainnet
// activated after the pin are active from block 0. Each has a feature account
// that says active since 0, and the Rent sysvar starts at their price. Neither
// can be scheduled again, so governance cannot apply a cut twice.
func TestGenesisStartsAtMainnetsRent(t *testing.T) {
	const (
		rent6333 = "4a6f7o7iTcA8hRDCrPLkSatnt5Ykxiu36wo5p1Tt12wC" // SIMD-0437-1
		rent5080 = "61BtM7BkDEE8Yq5fskEVAQT9mYA8qCejJWoLe5apqg81" // SIMD-0437-2
	)
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()
	if got := mustPerByte(t, c); got != 5_080 {
		t.Fatalf("lamports per byte is %d at genesis, want mainnet's 5,080", got)
	}
	for _, id := range []string{rent6333, rent5080} {
		if at := mustActive(t, c, id); at != 0 {
			t.Fatalf("gate %s is active since %d, want 0", id, at)
		}
		a, ok := c.Account(key(t, id))
		if !ok || a.Owner != svm.FeatureProgram || len(a.Data) != 9 || a.Data[0] != 1 || binary.LittleEndian.Uint64(a.Data[1:]) != 0 {
			t.Fatalf("gate %s: its feature account does not say active since 0: %+v", id, a)
		}
		if err := schedule(t, c, 5-svm.MinActivationDelay, 5, id); err == nil || !strings.Contains(err.Error(), "already active") {
			t.Fatalf("scheduling gate %s again: %v, want already active", id, err)
		}
	}
}

// TestAnUpgradeHandlerActivatesThroughTheSameHelper proves that an upgrade
// handler and a begin-block flip leave the same gate state, so a handler can
// activate a gate that its crates removed while the gate was still off here.
// Both paths call ActivateGates, so the two cannot drift.
func TestAnUpgradeHandlerActivatesThroughTheSameHelper(t *testing.T) {
	const at = 4
	flipped := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	flipped.Block()
	if err := schedule(t, flipped, at-svm.MinActivationDelay, at, rent2575); err != nil {
		t.Fatal(err)
	}
	for flipped.Height < at {
		flipped.Block()
	}

	upgraded := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	upgraded.Block()
	if err := upgraded.App.Upgrades.ScheduleUpgrade(
		upgraded.App.NewUncachedContext(false, cmtproto.Header{Height: upgraded.Height}),
		upgradetypes.Plan{Name: "activate-rent-2575", Height: at}); err != nil {
		t.Fatal(err)
	}
	// The plan must be committed before the machine restarts. The new binary
	// must not carry a handler for a plan that is still in the future, because
	// x/upgrade treats that as a downgrade and halts. The binary swap happens
	// in the block before the upgrade height, as an operator does it.
	for upgraded.Height < at-1 {
		upgraded.Block()
	}
	upgraded.Restart()
	upgraded.App.Upgrades.SetUpgradeHandler("activate-rent-2575",
		func(ctx context.Context, plan upgradetypes.Plan, vm module.VersionMap) (module.VersionMap, error) {
			ms := sdk.UnwrapSDKContext(ctx).MultiStore()
			_, err := upgraded.App.Stores.ActivateGates(ms, uint64(plan.Height), [][32]byte{key(t, rent2575)})
			return vm, err
		})
	for upgraded.Height < at {
		upgraded.Block()
	}

	// Do not compare app hashes. The upgraded chain also holds the x/upgrade
	// plan, and the flipped chain holds the spent pending key, so the two trees
	// differ by design. Only the state that ActivateGates writes must match.
	for _, want := range []struct {
		what string
		a, b any
	}{
		{"active height", mustActive(t, flipped, rent2575), mustActive(t, upgraded, rent2575)},
		{"feature account", mustAccount(t, flipped, rent2575), mustAccount(t, upgraded, rent2575)},
		{"lamports per byte", mustPerByte(t, flipped), mustPerByte(t, upgraded)},
	} {
		if want.a != want.b {
			t.Fatalf("a flip and an upgrade left different %s: %v and %v", want.what, want.a, want.b)
		}
	}
	if got := mustPerByte(t, upgraded); got != 2_575 {
		t.Fatalf("the upgrade handler left %d lamports per byte, want 2,575", got)
	}
}

func mustActive(t *testing.T, c *tc.Chain, id string) uint64 {
	t.Helper()
	at, on := activeAt(t, c, id)
	if !on {
		t.Fatalf("gate %s is not active", id)
	}
	return at
}

func mustAccount(t *testing.T, c *tc.Chain, id string) string {
	t.Helper()
	a, ok := c.Account(key(t, id))
	if !ok {
		t.Fatalf("gate %s has no feature account", id)
	}
	return fmt.Sprintf("%x owner=%x lamports=%d", a.Data, a.Owner, a.Lamports)
}

func mustPerByte(t *testing.T, c *tc.Chain) uint64 {
	t.Helper()
	v, err := c.App.Stores.LamportsPerByte(c.App.CommitMultiStore())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestAGateIsScheduledOnce proves that ScheduleGates refuses a gate that is
// already pending, and that ActivateGates leaves an active gate as it is.
// Otherwise a gate pending at two heights would, at the second, move the
// recorded activation and rewrite the rent over a cut that activated between.
func TestAGateIsScheduledOnce(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()
	if err := schedule(t, c, 5-svm.MinActivationDelay, 5, rent2575); err != nil {
		t.Fatal(err)
	}
	if err := schedule(t, c, 8-svm.MinActivationDelay, 8, rent2575); err == nil {
		t.Fatal("a gate already scheduled at 5 was scheduled again at 8")
	}
	if err := schedule(t, c, 6-svm.MinActivationDelay, 6, rent1322); err != nil {
		t.Fatal(err)
	}
	for c.Height < 7 {
		c.Block()
	}
	at, _ := activeAt(t, c, rent2575)
	perByte := mustPerByte(t, c)
	if at != 5 || perByte != 1_322 {
		t.Fatalf("at height 7 the first gate is active since %d and rent is %d, want 5 and 1,322", at, perByte)
	}
	// An upgrade handler can also activate a gate directly. ActivateGates
	// leaves an active gate as it is.
	ctx := c.App.NewUncachedContext(false, cmtproto.Header{Height: c.Height})
	if changed, err := c.App.Stores.ActivateGates(ctx.MultiStore(), uint64(c.Height+1), [][32]byte{key(t, rent2575)}); err != nil || changed {
		t.Fatalf("activating an active gate: changed %v, %v", changed, err)
	}
	c.Block()
	if got, _ := activeAt(t, c, rent2575); got != at || mustPerByte(t, c) != perByte {
		t.Fatalf("after activating it again the gate is active since %d and rent is %d, want %d and %d",
			got, mustPerByte(t, c), at, perByte)
	}
}
