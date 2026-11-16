package app

import (
	"encoding/json/jsontext"
	"fmt"
	"time"

	"cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/codec"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	govv1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// GovParams returns the governance parameters. Six of them differ from
// DefaultParams beyond the denom. Each one follows from a number this chain
// already fixes.
//
// The denom matters most. DefaultParams sets the minimum deposit in `stake`,
// which no account here holds. With it, every proposal can be submitted, none
// can reach a vote, and governance looks healthy but does not work.
func GovParams() govv1.Params {
	p := govv1.DefaultParams()
	p.MinDeposit = sdk.NewCoins(sdk.NewCoin(BondDenom, math.NewInt(10_000_000)))
	// 2x the minimum deposit, not the SDK's 5x. With a full initial deposit,
	// 5x is 56% of an operator's free balance. An operator with an ordinary
	// proposal open could then not pay for the emergency path.
	p.ExpeditedMinDeposit = sdk.NewCoins(sdk.NewCoin(BondDenom, math.NewInt(20_000_000)))
	// x/svm.MinActivationDelay (432,000 blocks) is 48 h on an idle chain at
	// 400 ms a block, which a new chain is. The SDK's 48 h voting period leaves
	// no margin for the cancel proposal that the delay exists for.
	p.VotingPeriod = duration(24 * 60 * 60)
	// The expedited path can rush a cancel but not an activation, because
	// x/svm measures the delay floor from execution.
	p.ExpeditedVotingPeriod = duration(12 * 60 * 60)
	// The operator set is the electorate, and voting power stays at the genesis
	// bonds. With the SDK's 0.334, two of four operators could move a feature
	// gate.
	p.Quorum = "0.667"
	// The full deposit at submission removes the deposit period. AddDeposit
	// starts voting in the same block, so the inactive queue, MaxDepositPeriod
	// and the sweep of dead proposals are unreachable. Crowdfunding a deposit
	// needs a public that this chain does not have.
	p.MinInitialDepositRatio = "1.0"
	// The bond denom has a fixed supply and is the permission, so nothing
	// replaces a burn. A veto on a four-operator chain already has a name
	// attached.
	p.BurnVoteVeto = false
	return p
}

// SlashingParams returns the x/slashing parameters. The window is the SDK's
// 100 blocks scaled to the 400 ms pacing. A validator in the set for 500 blocks
// that misses more than 250 of the last 500 is jailed. That is a machine that
// is down for minutes. Neither infraction burns, because without issuance a
// burn permanently cuts an operator's share of the set and of governance.
// The jail lasts 1ns, the least the SDK accepts, so the operator returns with
// its own MsgUnjail and no wait.
func SlashingParams() slashingtypes.Params {
	return slashingtypes.NewParams(500, math.LegacyNewDecWithPrec(5, 1), time.Nanosecond,
		math.LegacyZeroDec(), math.LegacyZeroDec())
}

func duration(seconds int64) *time.Duration {
	d := time.Duration(seconds) * time.Second
	return &d
}

// protoMessage is what the codec marshals, which is every genesis type here.
type protoMessage = interface {
	Reset()
	String() string
	ProtoMessage()
}

// Operator is one machine allowed into the validator set. Only operators hold
// the bond denom, so an operator is an account that genesis mints to.
type Operator struct {
	// Account is the operator's own address. Its validator address is the same
	// bytes under the valoper prefix, as self-delegation requires.
	Account sdk.AccAddress
	// ConsensusPubkey is the CometBFT signing key from priv_validator_key.json.
	ConsensusPubkey cryptotypes.PubKey
	// SelfBond is bonded at genesis and sits in the bonded pool.
	SelfBond math.Int
	// Free stays in the operator's account, to delegate or to pay fees.
	Free    math.Int
	Moniker string
}

// Permission writes the permissioned validator set into a genesis state.
// Genesis mints the bond denom only to these operators, sets `SendEnabled`
// false for it, and bonds each operator as a self-delegated validator. It also
// writes the x/slashing parameters and the signing info of each validator,
// because only the path that bonds them knows they need it.
//
// `x/staking` and `MsgCreateValidator` stay unchanged. An outsider can submit
// MsgCreateValidator, and it fails for lack of coin. The result is the same as
// a permission check, with no code to keep correct across SDK upgrades.
//
// Each outsider gets an account and no balance, as every address that is
// not an operator has on a running chain.
func Permission(cdc codec.Codec, state map[string]jsontext.Value, operators []Operator, outsiders ...sdk.AccAddress) error {
	if len(operators) == 0 {
		return fmt.Errorf("app: a permissioned genesis needs at least one operator")
	}

	var auth authtypes.GenesisState
	var bank banktypes.GenesisState
	var staking stakingtypes.GenesisState
	var gov govv1.GenesisState
	var slashing slashingtypes.GenesisState
	for _, m := range []struct {
		name string
		into any
	}{
		{authtypes.ModuleName, &auth},
		{banktypes.ModuleName, &bank},
		{stakingtypes.ModuleName, &staking},
		{govtypes.ModuleName, &gov},
		{slashingtypes.ModuleName, &slashing},
	} {
		// Use the codec, not encoding/json, because these types carry Any fields.
		if err := cdc.UnmarshalJSON(state[m.name], m.into.(protoMessage)); err != nil {
			return fmt.Errorf("app: %s genesis: %w", m.name, err)
		}
	}

	staking.Params.BondDenom = BondDenom
	// The deposit denom is the permission, so Permission writes the governance
	// parameters too. Every path that makes a permissioned genesis then gets
	// them.
	params := GovParams()
	gov.Params = &params
	// These two lines enforce the permission. The rest of the permissioned set
	// follows, because nobody else holds the denom.
	bank.Params.SendEnabled = []*banktypes.SendEnabled{{Denom: BondDenom, Enabled: false}}
	slashing.Params = SlashingParams()

	bonded := math.ZeroInt()
	supply := math.ZeroInt()
	total := math.ZeroInt()
	for i, op := range operators {
		if op.ConsensusPubkey == nil {
			return fmt.Errorf("app: operator %d has no consensus pubkey", i)
		}
		valoper := sdk.ValAddress(op.Account).String()
		validator, err := stakingtypes.NewValidator(valoper, op.ConsensusPubkey,
			stakingtypes.Description{Moniker: op.Moniker})
		if err != nil {
			return err
		}
		validator.Status = stakingtypes.Bonded
		validator.Tokens = op.SelfBond
		validator.DelegatorShares = math.LegacyNewDecFromInt(op.SelfBond)
		power := sdk.TokensToConsensusPower(op.SelfBond, sdk.DefaultPowerReduction)
		if power <= 0 {
			return fmt.Errorf("app: operator %s self-bonds %s, which is below one unit of voting power", op.Moniker, op.SelfBond)
		}

		staking.Validators = append(staking.Validators, validator)
		staking.Delegations = append(staking.Delegations, stakingtypes.Delegation{
			DelegatorAddress: op.Account.String(),
			ValidatorAddress: valoper,
			Shares:           validator.DelegatorShares,
		})
		staking.LastValidatorPowers = append(staking.LastValidatorPowers, stakingtypes.LastValidatorPower{
			Address: valoper,
			Power:   power,
		})
		total = total.AddRaw(power)
		// This is the signing info that the x/slashing AfterValidatorBonded hook
		// writes at height 0. Genesis bonds these validators, not x/staking, so
		// the hook never runs. Without it, the first vote that x/slashing counts
		// fails the block.
		consAddr := sdk.ConsAddress(op.ConsensusPubkey.Address())
		slashing.SigningInfos = append(slashing.SigningInfos, slashingtypes.SigningInfo{
			Address:              consAddr.String(),
			ValidatorSigningInfo: slashingtypes.NewValidatorSigningInfo(consAddr, 0, 0, time.Unix(0, 0), false, 0),
		})

		base := authtypes.NewBaseAccount(op.Account, nil, 0, 0)
		packed, err := authtypes.PackAccounts(authtypes.GenesisAccounts{base})
		if err != nil {
			return err
		}
		auth.Accounts = append(auth.Accounts, packed...)

		if op.Free.IsPositive() {
			bank.Balances = append(bank.Balances, banktypes.Balance{
				Address: op.Account.String(),
				Coins:   sdk.NewCoins(sdk.NewCoin(BondDenom, op.Free)),
			})
		}
		bonded = bonded.Add(op.SelfBond)
		supply = supply.Add(op.SelfBond).Add(op.Free)
	}
	// The account makes an outsider's MsgCreateValidator fail for lack of
	// coin. Without an account, it fails on the signature instead.
	for _, address := range outsiders {
		packed, err := authtypes.PackAccounts(authtypes.GenesisAccounts{
			authtypes.NewBaseAccount(address, nil, 0, 0),
		})
		if err != nil {
			return err
		}
		auth.Accounts = append(auth.Accounts, packed...)
	}
	staking.LastTotalPower = total

	// The x/staking genesis requires that the bonded pool holds exactly the
	// bonded tokens, so the pool gets an ordinary balance here.
	bank.Balances = append(bank.Balances, banktypes.Balance{
		Address: authtypes.NewModuleAddress(stakingtypes.BondedPoolName).String(),
		Coins:   sdk.NewCoins(sdk.NewCoin(BondDenom, bonded)),
	})
	bank.Supply = sdk.NewCoins(sdk.NewCoin(BondDenom, supply))

	for _, m := range []struct {
		name string
		from any
	}{
		{authtypes.ModuleName, &auth},
		{banktypes.ModuleName, &bank},
		{stakingtypes.ModuleName, &staking},
		{govtypes.ModuleName, &gov},
		{slashingtypes.ModuleName, &slashing},
	} {
		raw, err := cdc.MarshalJSON(m.from.(protoMessage))
		if err != nil {
			return err
		}
		state[m.name] = raw
	}
	return nil
}
