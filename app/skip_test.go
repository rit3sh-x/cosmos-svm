package app_test

import (
	"errors"
	"testing"

	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	upgradetypes "github.com/cosmos/cosmos-sdk/x/upgrade/types"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// TestASkippedPlanIsClearedTogether proves that --unsafe-skip-upgrades
// reaches x/upgrade, which clears the plan at its height and runs the block
// on this binary. If nothing reads the flag, every binary halts at the plan
// even with the flag set.
func TestASkippedPlanIsClearedTogether(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()
	// A plan that no binary has a handler for. To the old binary, a failing
	// plan looks the same.
	plan := upgradetypes.Plan{Name: "not-in-this-binary", Height: 3}
	if err := c.App.Upgrades.ScheduleUpgrade(c.App.NewUncachedContext(false, cmtproto.Header{Height: c.Height}), plan); err != nil {
		t.Fatal(err)
	}
	c.Block()
	c.RestartSkipping(3)
	c.Block()
	c.Block()
	ctx := c.App.NewUncachedContext(false, cmtproto.Header{Height: c.Height})
	if _, err := c.App.Upgrades.GetUpgradePlan(ctx); !errors.Is(err, upgradetypes.ErrNoUpgradePlanFound) {
		t.Fatalf("after the skip the plan is %v", err)
	}
	// The name is free again. A skip clears the plan, and x/upgrade refuses
	// only a completed name.
	again := upgradetypes.Plan{Name: "not-in-this-binary", Height: c.Height + 5}
	if err := c.App.Upgrades.ScheduleUpgrade(ctx, again); err != nil {
		t.Fatalf("scheduling the skipped name again: %v", err)
	}
}
