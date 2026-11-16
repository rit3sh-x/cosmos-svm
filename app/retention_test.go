package app_test

import (
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"

	"github.com/cosmos/cosmos-sdk/baseapp"
	sdk "github.com/cosmos/cosmos-sdk/types"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestEveryMachineKeepsTheEvidenceWindow proves that a machine with
// min-retain-blocks at the shipped window and a snapshot interval of 100
// keeps exactly 2,000 blocks. x/evidence enforces the same window, so
// equivocation from 1,500 heights back tombstones, and from 2,001 back is too
// old.
func TestEveryMachineKeepsTheEvidenceWindow(t *testing.T) {
	ops := tc.DefaultOperators(4)
	window, _ := svm.EvidenceParams()
	// The IAVL node cache is off. With the cache, the snapshot exporter and the
	// next block's writes share node objects, and IAVL v1.2.8 races on them
	// inside the library. Without it, the exporter loads its own nodes, so the
	// race detector checks this repository's code, including the dedup export.
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Operators: ops, SnapshotInterval: 100,
		BaseApp: []func(*baseapp.BaseApp){baseapp.SetMinRetainBlocks(uint64(window)), baseapp.SetIAVLCacheSize(0)}})
	equivocation := func(op, back int) abci.Misbehavior {
		return abci.Misbehavior{
			Type:             abci.MisbehaviorType_DUPLICATE_VOTE,
			Validator:        abci.Validator{Address: ops[op].ConsensusPubkey.Address(), Power: 10},
			Height:           c.Height - int64(back),
			Time:             time.Unix(1_700_000_000, 0),
			TotalVotingPower: 40,
		}
	}
	tombstoned := func(op int) bool {
		info, err := c.App.Slashing.GetValidatorSigningInfo(ctxOf(c), sdk.ConsAddress(ops[op].ConsensusPubkey.Address()))
		if err != nil {
			t.Fatal(err)
		}
		return info.Tombstoned
	}

	for c.Height < 1600 {
		c.Signed(nil)
	}
	if !removes(c.Signed(nil, equivocation(2, 1500)), ops[2]) || !tombstoned(2) {
		t.Fatal("equivocation from 1,500 heights back was not acted on")
	}
	for c.Height < 2100 {
		c.Signed(nil)
	}
	if updates := c.Signed(nil, equivocation(1, 2001)); len(updates) != 0 || tombstoned(1) {
		t.Fatalf("equivocation from 2,001 heights back was acted on: %v", updates)
	}
	// RetainHeight is the highest that any commit asked for, because CometBFT
	// never restores a block. At min-retain-blocks = 1, RetainHeight is 1,800.
	// At height 2,000 the window term is zero, which baseapp reads as no floor,
	// and the machine prunes to the snapshot floor of 1,800 for good.
	if want := c.Height - window; c.RetainHeight != want {
		t.Fatalf("at height %d the machine holds blocks from %d, want %d", c.Height, c.RetainHeight, want)
	}
}
