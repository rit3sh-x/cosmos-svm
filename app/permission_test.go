package app_test

import (
	"strings"
	"testing"

	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	"github.com/rit3sh-x/cosmos-svm/app"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

func bond(n int64) sdk.Coin { return sdk.NewCoin(app.BondDenom, math.NewInt(n)) }

// two starts a chain with two bonded operators, as delegation and
// redelegation between operators need.
func two(t *testing.T) *tc.Chain {
	t.Helper()
	return tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Operators: tc.DefaultOperators(2)})
}

func valoper(op app.Operator) string { return sdk.ValAddress(op.Account).String() }

// TestAnOutsiderCannotCreateAValidator proves that an outsider's
// MsgCreateValidator fails for lack of coin, not for lack of permission. No
// ante decorator, wrapping module or allowlist in state refuses it. The
// outsider has an account and no balance, because without an account the
// refusal is about its signature.
func TestAnOutsiderCannotCreateAValidator(t *testing.T) {
	outsider := tc.OperatorKey(200)
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{
		Operators: tc.DefaultOperators(2),
		Outsiders: []sdk.AccAddress{tc.Address(outsider)},
	})
	c.Block()

	msg, err := stakingtypes.NewMsgCreateValidator(
		sdk.ValAddress(tc.Address(outsider)).String(),
		tc.ConsensusPubkey(outsider),
		bond(1_000_000),
		stakingtypes.Description{Moniker: "outsider"},
		stakingtypes.NewCommissionRates(math.LegacyZeroDec(), math.LegacyZeroDec(), math.LegacyZeroDec()),
		math.OneInt(),
	)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := c.Deliver(c.Cosmos(outsider, msg))
	if res[0].Code == 0 {
		t.Fatal("an outsider created a validator")
	}
	if !strings.Contains(res[0].Log, "insufficient funds") {
		t.Fatalf("refused for the wrong reason, which should be the coin: %s", res[0].Log)
	}

	// The outsider stays out of the set.
	validators, err := c.App.Staking.GetAllValidators(c.App.NewUncachedContext(true, cmtproto.Header{Height: c.Height}))
	if err != nil {
		t.Fatal(err)
	}
	if len(validators) != 2 {
		t.Fatalf("x/staking holds %d validators, want the 2 operators", len(validators))
	}
}

// TestTheBondDenomCannotBeSent proves that one x/bank parameter, and no code
// of this chain, blocks a send of the bond denom.
func TestTheBondDenomCannotBeSent(t *testing.T) {
	c := two(t)
	c.Block()
	from, to := c.Operators[0], c.Operators[1]
	res, _ := c.Deliver(c.Cosmos(tc.OperatorKey(0), &banktypes.MsgSend{
		FromAddress: from.Account.String(),
		ToAddress:   to.Account.String(),
		Amount:      sdk.NewCoins(bond(1)),
	}))
	if res[0].Code == 0 {
		t.Fatal("a MsgSend of the bond denom succeeded")
	}
	if !strings.Contains(res[0].Log, "send transactions are disabled") &&
		!strings.Contains(res[0].Log, "not allowed to be sent") {
		t.Fatalf("refused for the wrong reason: %s", res[0].Log)
	}
}

// TestDelegationAmongOperatorsWorks proves that operators can delegate,
// redelegate and undelegate. `IsSendEnabledCoins` is reachable only from
// MsgSend and MsgMultiSend, so the send-disabled denom blocks only a transfer
// to a third party.
func TestDelegationAmongOperatorsWorks(t *testing.T) {
	c := two(t)
	c.Block()
	first, second := c.Operators[0], c.Operators[1]

	res, _ := c.Deliver(c.Cosmos(tc.OperatorKey(0), stakingtypes.NewMsgDelegate(
		first.Account.String(), valoper(second), bond(1_000_000))))
	if res[0].Code != 0 {
		t.Fatalf("delegate to another operator: code %d, %s", res[0].Code, res[0].Log)
	}

	res, _ = c.Deliver(c.Cosmos(tc.OperatorKey(0), stakingtypes.NewMsgBeginRedelegate(
		first.Account.String(), valoper(second), valoper(first), bond(500_000))))
	if res[0].Code != 0 {
		t.Fatalf("redelegate: code %d, %s", res[0].Code, res[0].Log)
	}

	res, _ = c.Deliver(c.Cosmos(tc.OperatorKey(0), stakingtypes.NewMsgUndelegate(
		first.Account.String(), valoper(second), bond(100_000))))
	if res[0].Code != 0 {
		t.Fatalf("undelegate: code %d, %s", res[0].Code, res[0].Log)
	}
}

// TestTheValidatorSetComesFromStaking proves that the x/staking genesis, not
// the list in the CometBFT genesis file, makes a machine a validator.
func TestTheValidatorSetComesFromStaking(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Operators: tc.DefaultOperators(4)})
	c.Block()
	ctx := c.App.NewUncachedContext(true, cmtproto.Header{Height: c.Height})
	validators, err := c.App.Staking.GetAllValidators(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(validators) != 4 {
		t.Fatalf("x/staking holds %d validators, want the 4 genesis bonded", len(validators))
	}
	for _, v := range validators {
		if !v.IsBonded() {
			t.Fatalf("validator %s is %s, want bonded", v.OperatorAddress, v.Status)
		}
	}
	power, err := c.App.Staking.GetLastTotalPower(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !power.Equal(math.NewInt(40)) {
		t.Fatalf("last total power is %s, want 40 for four operators at 10", power)
	}
}

// TestABlockCarriesBothKinds proves that one block carries a Cosmos and an SVM
// transaction. The two halves share the block and nothing else. Lamports are
// not a bank denom, and the engine's cost model charges the SVM half, not
// Cosmos gas.
func TestABlockCarriesBothKinds(t *testing.T) {
	faucet := tc.Key(1)
	c := two(t)
	c.Block()

	solana := c.Wrap(tc.Tx(c.Latest(), faucet, nil,
		tc.Transfer(faucet, tc.Pub(tc.Key(2)), svm.MinimumBalance(0))))
	cosmos := c.Cosmos(tc.OperatorKey(0), stakingtypes.NewMsgDelegate(
		c.Operators[0].Account.String(), valoper(c.Operators[1]), bond(1_000_000)))

	res, _ := c.Deliver(cosmos, solana)
	for i, r := range res {
		if r.Code != 0 {
			t.Fatalf("transaction %d: code %d, %s", i, r.Code, r.Log)
		}
	}
	// The Cosmos half uses Cosmos gas. The SVM half reports compute units as
	// gas wanted and uses no Cosmos gas.
	if res[0].GasUsed == 0 {
		t.Fatal("the Cosmos transaction used no gas")
	}
	if res[1].GasUsed != 0 {
		t.Fatalf("the SVM transaction used %d Cosmos gas, want none", res[1].GasUsed)
	}
	if got := c.Lamports(tc.Pub(tc.Key(2))); got != svm.MinimumBalance(0) {
		t.Fatalf("the SVM transfer did not land: %d lamports", got)
	}
}
