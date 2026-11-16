package app_test

import (
	"context"
	"encoding/binary"
	"testing"

	dbm "github.com/cosmos/cosmos-db"

	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	upgradetypes "github.com/cosmos/cosmos-sdk/x/upgrade/types"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// scanned returns the sum of the lamports of every account in committed state.
func scanned(t *testing.T, c *tc.Chain) uint64 {
	t.Helper()
	var sum uint64
	if err := c.App.Stores.OpenAccountScan(c.App.CommitMultiStore()).Each(func(_ [32]byte, v []byte) error {
		sum += binary.LittleEndian.Uint64(v[0:8])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return sum
}

// totalIsAScan waits for any rebuild and commits one more block. The block
// check (tc.Chain's lamportsAgree) runs only on a current index, which is why
// totalIsAScan also compares the total with a scan. A case cannot pass because
// the check was skipped.
func totalIsAScan(t *testing.T, c *tc.Chain) {
	t.Helper()
	c.App.Tokens.Wait()
	c.Block()
	c.App.Tokens.Wait()
	total, err := c.App.Tokens.Total()
	if err != nil {
		t.Fatalf("height %d: %v", c.Height, err)
	}
	if want := scanned(t, c); total != want {
		t.Fatalf("height %d: the total is %d, a scan is %d", c.Height, total, want)
	}
}

// TestTheLamportTotalIsAScan proves that the lamport total of the token index
// equals a scan of all accounts. Every block that a test chain commits checks
// this. The cases are the events, other than an ordinary transfer, that move
// lamports or rebuild the index.
func TestTheLamportTotalIsAScan(t *testing.T) {
	t.Run("a fee burned by a success and by a failure", func(t *testing.T) {
		faucet := tc.Key(1)
		c := tc.Start(t, tc.Pub(faucet), tc.Options{})
		c.Block()
		before := scanned(t, c)
		ok := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), svm.MinimumBalance(0)))
		// This transfer is more than the faucet holds. It fails, and its fee
		// is still taken.
		failed := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(3)), ^uint64(0)>>1))
		res, _ := c.Block(ok, failed)
		if res[0].Code != 0 || res[1].Code == 0 || res[1].Code == 2 {
			t.Fatalf("codes %d and %d, want a success and a failure that paid", res[0].Code, res[1].Code)
		}
		if scanned(t, c) >= before {
			t.Fatal("no fee left the total")
		}
		totalIsAScan(t, c)
	})
	t.Run("a gate flip that creates a feature account", func(t *testing.T) {
		c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
		c.Block()
		const flip = 6
		if err := schedule(t, c, flip-svm.MinActivationDelay, flip, cpiNesting); err != nil {
			t.Fatal(err)
		}
		for c.Height < flip {
			c.Block()
		}
		if _, on := activeAt(t, c, cpiNesting); !on {
			t.Fatal("the gate did not flip")
		}
		totalIsAScan(t, c)
	})
	t.Run("SeedPrograms", func(t *testing.T) {
		var memo4 svm.GenesisProgram
		var genesisPrograms []svm.GenesisProgram
		for _, p := range tc.ManifestPrograms(t) {
			if p.Address == memoV4 {
				memo4 = p
			} else {
				genesisPrograms = append(genesisPrograms, p)
			}
		}
		c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Programs: genesisPrograms})
		c.Block()
		if err := c.App.Upgrades.ScheduleUpgrade(c.App.NewUncachedContext(false, cmtproto.Header{Height: c.Height}),
			upgradetypes.Plan{Name: "seed-memo-v4", Height: 3}); err != nil {
			t.Fatal(err)
		}
		c.Block()
		c.Restart()
		c.App.Upgrades.SetUpgradeHandler("seed-memo-v4", func(ctx context.Context, plan upgradetypes.Plan, vm module.VersionMap) (module.VersionMap, error) {
			return vm, c.App.Stores.SeedPrograms(sdk.UnwrapSDKContext(ctx).MultiStore(), []svm.GenesisProgram{memo4}, uint64(plan.Height))
		})
		c.Block()
		if _, ok := c.Account(key(t, memoV4)); !ok {
			t.Fatal("the upgrade seeded nothing")
		}
		totalIsAScan(t, c)
	})
	t.Run("a deleted index", func(t *testing.T) {
		faucet := tc.Key(1)
		c := tc.Start(t, tc.Pub(faucet), tc.Options{})
		c.Block()
		db := c.TokenIndexDB()
		for _, k := range [][]byte{{'m'}, {'s'}} {
			if err := db.Delete(k); err != nil {
				t.Fatal(err)
			}
		}
		c.Restart()
		if _, err := c.App.Tokens.Total(); err == nil {
			t.Fatal("a deleted index has a total before a block rebuilds it")
		}
		c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), svm.MinimumBalance(0))))
		totalIsAScan(t, c)
	})
	t.Run("a version bump with blocks committing during the rebuild", func(t *testing.T) {
		faucet := tc.Key(1)
		index := &heldIndex{DB: dbm.NewMemDB()}
		c := tc.Start(t, tc.Pub(faucet), tc.Options{Disk: true, Tokens: index})
		c.Block()
		if err := c.TokenIndexDB().Set([]byte{'m'}, append([]byte{0}, binary.BigEndian.AppendUint64(nil, uint64(c.Height))...)); err != nil {
			t.Fatal(err)
		}
		c.Restart()
		index.hold = make(chan struct{})
		released := false
		release := func() {
			if !released {
				released = true
				close(index.hold)
			}
		}
		t.Cleanup(release)
		c.Block()
		for i := 0; i < 3; i++ {
			if res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(byte(10+i))), svm.MinimumBalance(0)))); res[0].Code != 0 {
				t.Fatalf("a transfer during the rebuild: code %d", res[0].Code)
			}
		}
		release()
		totalIsAScan(t, c)
	})
	t.Run("a state-sync join", func(t *testing.T) {
		_, snapshot, chunks, _ := served(t)
		joiner, err := tc.Sync(t, snapshot, chunks)
		if err != nil {
			t.Fatal(err)
		}
		totalIsAScan(t, joiner)
	})
}
