package app_test

import (
	"context"
	"encoding/hex"
	json "encoding/json/v2"
	"strings"
	"testing"

	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	upgradetypes "github.com/cosmos/cosmos-sdk/x/upgrade/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestGenesisDeclaringAnotherEngineIsRefused proves that validation and
// InitGenesis refuse a genesis that declares another engine digest. The check
// applies from block 1, not only from the first upgrade.
func TestGenesisDeclaringAnotherEngineIsRefused(t *testing.T) {
	g := svm.DefaultGenesis()
	if err := g.Validate(); err != nil {
		t.Fatalf("the default genesis declares an engine this binary is not: %v", err)
	}
	g.EngineDigest = strings.Repeat("ab", 32)
	if err := g.Validate(); err == nil {
		t.Fatal("a genesis declaring another engine passed validation")
	}
	raw, err := json.Marshal(g, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	c := tc.Start(t, tc.Pub(tc.Key(0)), tc.Options{})
	ms := c.App.CommitMultiStore().CacheMultiStore()
	if err := c.App.Stores.InitGenesis(ms, raw); err == nil {
		t.Fatal("InitGenesis accepted a genesis declaring another engine")
	}
}

// TestGenesisAdoptsTheRunningEngine proves that genesis writes the running
// engine digest to state, which puts the digest in the app hash and carries it
// through an export.
func TestGenesisAdoptsTheRunningEngine(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(0)), tc.Options{})
	c.Block()
	compiled := ffi.Digest()
	if stored := c.App.Stores.EngineDigest(c.App.CommitMultiStore()); string(stored) != string(compiled[:]) {
		t.Fatalf("genesis adopted %x, this binary is %x", stored, compiled)
	}
	exported, err := c.App.Stores.ExportGenesis(c.App.CommitMultiStore())
	if err != nil {
		t.Fatal(err)
	}
	if exported.EngineDigest != hex.EncodeToString(compiled[:]) {
		t.Fatalf("the export declares %q", exported.EngineDigest)
	}
}

// TestABinaryRefusesToStartAgainstAnotherEngine proves that after an upgrade
// handler adopts another engine, a machine with a different digest does not
// start past that height.
func TestABinaryRefusesToStartAgainstAnotherEngine(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(0)), tc.Options{})
	c.Block()
	if err := c.RestartErr(); err != nil {
		t.Fatalf("this binary refused its own chain: %v", err)
	}

	upgradeHeight := c.Height + 2
	if err := c.App.Upgrades.ScheduleUpgrade(c.App.NewUncachedContext(false, cmtproto.Header{Height: c.Height}),
		upgradetypes.Plan{Name: "another-engine", Height: upgradeHeight}); err != nil {
		t.Fatal(err)
	}
	c.Block()
	c.Restart()
	// The handler acts as an upgrade to a build that this test does not run.
	c.App.Upgrades.SetUpgradeHandler("another-engine", func(ctx context.Context, _ upgradetypes.Plan, vm module.VersionMap) (module.VersionMap, error) {
		other := make([]byte, 32)
		for i := range other {
			other[i] = 0xab
		}
		sdk.UnwrapSDKContext(ctx).MultiStore().GetKVStore(c.App.Stores.Meta).Set(svm.MetaDigestKey, other)
		return vm, nil
	})
	c.Block()

	err := c.RestartErr()
	if err == nil {
		t.Fatal("a binary started against an engine the chain did not adopt")
	}
	if !strings.Contains(err.Error(), "svm: this binary has engine digest") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

func TestTheStartupAssertionHasThreeClauses(t *testing.T) {
	compiled := ffi.Digest()
	other := make([]byte, 32)
	otherNetwork := "devnet"
	if svm.Network == "devnet" {
		otherNetwork = "production"
	}
	cases := []struct {
		name    string
		arch    string
		stored  []byte
		network string
		ok      bool
	}{
		{"an adopted engine on amd64", "amd64", compiled[:], svm.Network, true},
		{"nothing adopted yet", "amd64", nil, "", true},
		{"another architecture", "arm64", compiled[:], svm.Network, false},
		{"another engine", "amd64", other, svm.Network, false},
		{"the other network", "amd64", compiled[:], otherNetwork, false},
	}
	for _, c := range cases {
		err := svm.Assert(c.arch, compiled, c.stored, c.network)
		if (err == nil) != c.ok {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
}
