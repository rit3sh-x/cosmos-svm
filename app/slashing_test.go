package app_test

import (
	"bytes"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"

	"github.com/rit3sh-x/cosmos-svm/app"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// removes reports whether updates takes op out of CometBFT's set.
func removes(updates []abci.ValidatorUpdate, op app.Operator) bool {
	for _, u := range updates {
		if bytes.Equal(u.PubKey.GetEd25519(), op.ConsensusPubkey.Bytes()) && u.Power == 0 {
			return true
		}
	}
	return false
}

// TestAStoppedValidatorIsJailedAndBurnsNothing proves the downtime rule at the
// 500-block window that SlashingParams ships. After a full window, a stopped
// operator is jailed and leaves the set in the block that reports it absent
// for the 251st time. Nothing burns, and its own MsgUnjail brings it back.
func TestAStoppedValidatorIsJailedAndBurnsNothing(t *testing.T) {
	ops := tc.DefaultOperators(4)
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Operators: ops})
	window := app.SlashingParams().SignedBlocksWindow
	for c.Height < window {
		if updates := c.Signed(nil); len(updates) != 0 {
			t.Fatalf("height %d changed the set while every operator signed: %v", c.Height, updates)
		}
	}
	supply := c.App.Bank.GetSupply(ctxOf(c), app.BondDenom).Amount
	stopped, valoper := ops[3], sdk.ValAddress(ops[3].Account)

	missed := 0
	for !removes(c.Signed(map[int]bool{3: true}), stopped) {
		if missed++; missed > int(window/2)+10 {
			t.Fatalf("the stopped validator was still in the set after %d missed blocks", missed)
		}
	}
	missed++
	if missed != int(window/2)+1 {
		t.Fatalf("jailed after %d missed blocks, want %d", missed, window/2+1)
	}
	v, err := c.App.Staking.GetValidator(ctxOf(c), valoper)
	if err != nil {
		t.Fatal(err)
	}
	if !v.IsJailed() || !v.Tokens.Equal(stopped.SelfBond) {
		t.Fatalf("the stopped validator: jailed %v, %s tokens of %s", v.IsJailed(), v.Tokens, stopped.SelfBond)
	}
	if got := c.App.Bank.GetSupply(ctxOf(c), app.BondDenom).Amount; !got.Equal(supply) {
		t.Fatalf("supply moved from %s to %s: a jail burned", supply, got)
	}

	res, _ := c.Deliver(c.Cosmos(tc.OperatorKey(3), &slashingtypes.MsgUnjail{ValidatorAddr: valoper.String()}))
	if res[0].Code != 0 {
		t.Fatalf("MsgUnjail: code %d, %s", res[0].Code, res[0].Log)
	}
	if v, _ = c.App.Staking.GetValidator(ctxOf(c), valoper); v.IsJailed() || !v.IsBonded() {
		t.Fatalf("after MsgUnjail: jailed %v, bonded %v", v.IsJailed(), v.IsBonded())
	}
	// The x/slashing hook restarts the window when the validator bonds again,
	// so it has another 500 blocks before it can be jailed.
	info, err := c.App.Slashing.GetValidatorSigningInfo(ctxOf(c), sdk.ConsAddress(stopped.ConsensusPubkey.Address()))
	if err != nil || info.StartHeight != c.Height {
		t.Fatalf("after MsgUnjail the window starts at %d (%v), want %d", info.StartHeight, err, c.Height)
	}
}

// TestADoubleSignerIsTombstonedAndBurnsNothing proves that equivocation
// evidence tombstones the validator through x/evidence. It leaves the set,
// nothing burns, and its MsgUnjail is refused for good.
func TestADoubleSignerIsTombstonedAndBurnsNothing(t *testing.T) {
	ops := tc.DefaultOperators(4)
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Operators: ops})
	c.Signed(nil)
	c.Signed(nil)
	supply := c.App.Bank.GetSupply(ctxOf(c), app.BondDenom).Amount
	signer, valoper := ops[2], sdk.ValAddress(ops[2].Account)
	consAddr := sdk.ConsAddress(signer.ConsensusPubkey.Address())

	updates := c.Signed(nil, abci.Misbehavior{
		Type:             abci.MisbehaviorType_DUPLICATE_VOTE,
		Validator:        abci.Validator{Address: consAddr, Power: 10},
		Height:           c.Height,
		Time:             time.Unix(1_700_000_000, 0),
		TotalVotingPower: 40,
	})
	if !removes(updates, signer) {
		t.Fatalf("the double-signer stayed in the set: %v", updates)
	}
	info, err := c.App.Slashing.GetValidatorSigningInfo(ctxOf(c), consAddr)
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.App.Staking.GetValidator(ctxOf(c), valoper)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Tombstoned || !v.IsJailed() || !v.Tokens.Equal(signer.SelfBond) {
		t.Fatalf("the double-signer: tombstoned %v, jailed %v, %s tokens of %s", info.Tombstoned, v.IsJailed(), v.Tokens, signer.SelfBond)
	}
	if got := c.App.Bank.GetSupply(ctxOf(c), app.BondDenom).Amount; !got.Equal(supply) {
		t.Fatalf("supply moved from %s to %s: a tombstone burned", supply, got)
	}
	res, _ := c.Deliver(c.Cosmos(tc.OperatorKey(2), &slashingtypes.MsgUnjail{ValidatorAddr: valoper.String()}))
	if res[0].Code == 0 {
		t.Fatal("a tombstoned validator unjailed itself")
	}
}
