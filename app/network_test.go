package app_test

import (
	"context"
	json "encoding/json/v2"
	"os/exec"
	"strings"
	"testing"

	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	upgradetypes "github.com/cosmos/cosmos-sdk/x/upgrade/types"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// otherNetwork returns the network that this build is not, so each test here
// runs from both sides of the build tag.
func otherNetwork() string {
	if svm.Network == "devnet" {
		return "production"
	}
	return "devnet"
}

// TestGenesisDeclaringTheOtherNetworkIsRefused proves that a genesis for the
// other network is refused. DefaultGenesis writes the compiled constant, so
// one binary cannot make a mismatched genesis. InitGenesis refuses a mismatch
// from block 1, not from the first upgrade.
func TestGenesisDeclaringTheOtherNetworkIsRefused(t *testing.T) {
	g := svm.DefaultGenesis()
	if g.Network != svm.Network {
		t.Fatalf("the default genesis declares %q, this binary is %q", g.Network, svm.Network)
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("the default genesis is refused by this binary: %v", err)
	}
	g.Network = otherNetwork()
	err := g.Validate()
	if err == nil {
		t.Fatal("a genesis declaring the other network passed validation")
	}
	// The error names both values. An operator with two binaries of the same
	// version and engine digest has nothing else to read.
	if !strings.Contains(err.Error(), otherNetwork()) || !strings.Contains(err.Error(), svm.Network) {
		t.Fatalf("the refusal names neither side: %v", err)
	}
	raw, err := json.Marshal(g, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	c := tc.Start(t, tc.Pub(tc.Key(0)), tc.Options{})
	ms := c.App.CommitMultiStore().CacheMultiStore()
	if err := c.App.Stores.InitGenesis(ms, raw); err == nil {
		t.Fatal("InitGenesis accepted a genesis declaring the other network")
	}
}

// TestGenesisAdoptsThisBuildsNetwork proves that the network value reaches
// state, so it is in the app hash and survives an export. The engine digest
// cannot see a Go build tag, and this value can.
func TestGenesisAdoptsThisBuildsNetwork(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(0)), tc.Options{})
	c.Block()
	if stored := c.App.Stores.ChainNetwork(c.App.CommitMultiStore()); stored != svm.Network {
		t.Fatalf("genesis adopted %q, this binary is %q", stored, svm.Network)
	}
	exported, err := c.App.Stores.ExportGenesis(c.App.CommitMultiStore())
	if err != nil {
		t.Fatal(err)
	}
	if exported.Network != svm.Network {
		t.Fatalf("the export declares %q", exported.Network)
	}
}

// TestABinaryRefusesToStartAgainstTheOtherNetwork proves that a binary does
// not start on a chain of the other network. After the upgrade handler runs,
// the build that runs this test is the wrong binary for the chain. The
// refusal comes before the machine signs anything.
func TestABinaryRefusesToStartAgainstTheOtherNetwork(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(0)), tc.Options{})
	c.Block()
	if err := c.RestartErr(); err != nil {
		t.Fatalf("this binary refused its own chain: %v", err)
	}

	upgradeHeight := c.Height + 2
	if err := c.App.Upgrades.ScheduleUpgrade(c.App.NewUncachedContext(false, cmtproto.Header{Height: c.Height}),
		upgradetypes.Plan{Name: "the-other-network", Height: upgradeHeight}); err != nil {
		t.Fatal(err)
	}
	c.Block()
	c.Restart()
	// The handler makes the chain one that this build is not the binary for.
	c.App.Upgrades.SetUpgradeHandler("the-other-network", func(ctx context.Context, _ upgradetypes.Plan, vm module.VersionMap) (module.VersionMap, error) {
		sdk.UnwrapSDKContext(ctx).MultiStore().GetKVStore(c.App.Stores.Meta).Set(svm.MetaNetworkKey, []byte(otherNetwork()))
		return vm, nil
	})
	c.Block()

	err := c.RestartErr()
	if err == nil {
		t.Fatal("a binary started against the other network's chain")
	}
	if !strings.Contains(err.Error(), "binary, the chain is a") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestTheBuildTagDoesNotReachTheEngineDigest proves that the devnet tag cannot
// change the engine digest. The digest is a Rust constant that engine/build.rs
// makes from a hash of the engine's files, so a Go tag can change it only
// through the Go sources that reach the FFI boundary. The test compares the
// packages and files in the transitive graph of ffi under both tags.
func TestTheBuildTagDoesNotReachTheEngineDigest(t *testing.T) {
	graph := func(tags ...string) string {
		args := append([]string{"list", "-deps", "-f", "{{.ImportPath}} {{.GoFiles}} {{.CgoFiles}}"}, tags...)
		out, err := exec.Command("go", append(args, "github.com/rit3sh-x/cosmos-svm/ffi")...).Output()
		if err != nil {
			t.Fatalf("go list %v: %v", tags, err)
		}
		return string(out)
	}
	if production, devnet := graph(), graph("-tags", "devnet"); production != devnet {
		t.Fatal("the devnet tag changes which sources reach the engine, so the two builds could report different digests")
	}
}
