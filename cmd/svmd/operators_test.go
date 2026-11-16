package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cmtcfg "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/privval"
	cmttypes "github.com/cometbft/cometbft/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/mr-tron/base58"

	"cosmossdk.io/log/v2"
	"cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/codec"
	svrcmd "github.com/cosmos/cosmos-sdk/server/cmd"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	genutiltypes "github.com/cosmos/cosmos-sdk/x/genutil/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	govv1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	"github.com/rit3sh-x/cosmos-svm/app"
	"github.com/rit3sh-x/cosmos-svm/genesis"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// tmp is an app built only for its codec and its default genesis.
func tmp(t *testing.T) *app.App {
	t.Helper()
	a, err := app.New(log.NewNopLogger(), dbm.NewMemDB(), app.Local{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// home writes the genesis file that `init` leaves before finishGenesis runs.
// It uses a fixed genesis time, so two runs give comparable bytes.
func home(t *testing.T, a *app.App) *cmtcfg.Config {
	t.Helper()
	cfg := cmtcfg.DefaultConfig()
	cfg.SetRoot(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(cfg.GenesisFile()), 0o750); err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(a.DefaultGenesis(), json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	doc := &genutiltypes.AppGenesis{
		AppName:       "svmd",
		ChainID:       "svm-genesis-test",
		GenesisTime:   time.Unix(1_700_000_000, 0).UTC(),
		InitialHeight: 1,
		AppState:      state,
		Consensus:     genutiltypes.NewConsensusGenesis(cmttypes.DefaultConsensusParams().ToProto(), nil),
	}
	if err := doc.SaveAs(cfg.GenesisFile()); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// four is the operator set that both input forms describe. Each operator has
// its own keys and account.
func four() []app.Operator { return tc.DefaultOperators(4) }

func flagsFor(ops []app.Operator) []string {
	out := make([]string, 0, len(ops))
	for i, op := range ops {
		out = append(out, strings.Join([]string{
			base64.StdEncoding.EncodeToString(op.ConsensusPubkey.Bytes()),
			op.Account.String(),
			op.SelfBond.String(),
			op.Free.String(),
			fmt.Sprintf("operator-%d", i),
		}, ","))
	}
	return out
}

// fileFor writes the --validators-file form, with the consensus pubkey in the
// Any JSON form that `svmd comet show-validator` prints.
func fileFor(t *testing.T, cdc codec.Codec, ops []app.Operator) string {
	t.Helper()
	var file validatorsFile
	for i, op := range ops {
		pubkey, err := cdc.MarshalInterfaceJSON(op.ConsensusPubkey)
		if err != nil {
			t.Fatal(err)
		}
		file.Validators = append(file.Validators, validatorEntry{
			ConsensusPubkey: pubkey,
			Account:         op.Account.String(),
			SelfBond:        op.SelfBond,
			Free:            op.Free,
			Moniker:         fmt.Sprintf("operator-%d", i),
		})
	}
	raw, err := json.Marshal(file, json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "validators.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// build runs finishGenesis, which `init` adds to genutil's InitCmd, and
// returns the finished genesis bytes.
func build(t *testing.T, a *app.App, ops []app.Operator) []byte {
	t.Helper()
	cfg := home(t, a)
	if err := finishGenesis(a.Codec, cfg, "solo", ops); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfg.GenesisFile())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestFourValidatorsFromEitherFormAreTheSameBytes proves that the flag form
// and the file form build the same genesis bytes, as Agave sends both forms
// through one builder.
func TestFourValidatorsFromEitherFormAreTheSameBytes(t *testing.T) {
	a := tmp(t)
	ops := four()

	fromFlags, err := operators(a.Codec, flagsFor(ops), "")
	if err != nil {
		t.Fatal(err)
	}
	fromFile, err := operators(a.Codec, nil, fileFor(t, a.Codec, ops))
	if err != nil {
		t.Fatal(err)
	}
	if len(fromFlags) != 4 || len(fromFile) != 4 {
		t.Fatalf("parsed %d from flags and %d from the file, want 4 of each", len(fromFlags), len(fromFile))
	}

	flags := build(t, a, fromFlags)
	file := build(t, a, fromFile)
	if !bytes.Equal(flags, file) {
		t.Fatal("the two input forms produce different genesis bytes")
	}
	if again := build(t, a, fromFile); !bytes.Equal(file, again) {
		t.Fatal("the same file produces different genesis bytes twice")
	}

	// The CometBFT list and staking genesis hold the same four validators at
	// matching power, because baseapp compares the validators that
	// InitGenesis returns with the file's list.
	doc, state := decode(t, a, flags)
	if len(doc.Consensus.Validators) != 4 {
		t.Fatalf("%d CometBFT validators, want 4", len(doc.Consensus.Validators))
	}
	var staking stakingtypes.GenesisState
	if err := a.Codec.UnmarshalJSON(state[stakingtypes.ModuleName], &staking); err != nil {
		t.Fatal(err)
	}
	if len(staking.Validators) != 4 || len(staking.Delegations) != 4 {
		t.Fatalf("%d staking validators and %d delegations, want 4 of each",
			len(staking.Validators), len(staking.Delegations))
	}
	for i, v := range doc.Consensus.Validators {
		if v.Name != fmt.Sprintf("operator-%d", i) {
			t.Fatalf("validator %d is %q", i, v.Name)
		}
		if v.Power != sdk.TokensToConsensusPower(ops[i].SelfBond, sdk.DefaultPowerReduction) {
			t.Fatalf("validator %d has power %d", i, v.Power)
		}
	}
}

func decode(t *testing.T, a *app.App, raw []byte) (*genutiltypes.AppGenesis, map[string]jsontext.Value) {
	t.Helper()
	var doc genutiltypes.AppGenesis
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var state map[string]jsontext.Value
	if err := json.Unmarshal(doc.AppState, &state); err != nil {
		t.Fatal(err)
	}
	return &doc, state
}

// TestTheBondDenomReachesTheOperatorsAndNobodyElse proves that genesis gives
// the bond denom only to the operators and the bonded pool, and that the denom
// cannot be sent.
func TestTheBondDenomReachesTheOperatorsAndNobodyElse(t *testing.T) {
	a := tmp(t)
	ops := four()
	parsed, err := operators(a.Codec, nil, fileFor(t, a.Codec, ops))
	if err != nil {
		t.Fatal(err)
	}
	_, state := decode(t, a, build(t, a, parsed))

	var bank struct {
		Params struct {
			SendEnabled []struct {
				Denom   string `json:"denom"`
				Enabled bool   `json:"enabled"`
			} `json:"send_enabled"`
		} `json:"params"`
		Balances []struct {
			Address string `json:"address"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(state["bank"], &bank); err != nil {
		t.Fatal(err)
	}
	if len(bank.Params.SendEnabled) != 1 ||
		bank.Params.SendEnabled[0].Denom != app.BondDenom ||
		bank.Params.SendEnabled[0].Enabled {
		t.Fatalf("send_enabled is %+v", bank.Params.SendEnabled)
	}
	held := map[string]bool{}
	for _, b := range bank.Balances {
		held[b.Address] = true
	}
	for _, op := range ops {
		if !held[op.Account.String()] {
			t.Fatalf("%s holds no %s", op.Account, app.BondDenom)
		}
	}
	if len(bank.Balances) != 5 {
		t.Fatalf("%d balances, want 4 operators and the bonded pool", len(bank.Balances))
	}
}

// TestTheSlashingParamsJailAndBurnNothing proves the slashing genesis. It has a
// 500-block window with half of it signed, no burn for either infraction and a
// 1ns jail. Each operator's signing info starts at height 0, as x/slashing's
// own hook writes it for a validator that x/staking bonds.
func TestTheSlashingParamsJailAndBurnNothing(t *testing.T) {
	a := tmp(t)
	ops := four()
	parsed, err := operators(a.Codec, nil, fileFor(t, a.Codec, ops))
	if err != nil {
		t.Fatal(err)
	}
	_, state := decode(t, a, build(t, a, parsed))
	var slashing slashingtypes.GenesisState
	if err := a.Codec.UnmarshalJSON(state["slashing"], &slashing); err != nil {
		t.Fatal(err)
	}
	p := slashing.Params
	if p.SignedBlocksWindow != 500 || !p.MinSignedPerWindow.Equal(math.LegacyNewDecWithPrec(5, 1)) ||
		p.DowntimeJailDuration != time.Nanosecond ||
		!p.SlashFractionDowntime.IsZero() || !p.SlashFractionDoubleSign.IsZero() {
		t.Fatalf("slashing params are %+v", p)
	}
	infos := map[string]slashingtypes.ValidatorSigningInfo{}
	for _, info := range slashing.SigningInfos {
		infos[info.Address] = info.ValidatorSigningInfo
	}
	for _, op := range parsed {
		info, ok := infos[sdk.ConsAddress(op.ConsensusPubkey.Address()).String()]
		if !ok || info.StartHeight != 0 || info.Tombstoned || info.MissedBlocksCounter != 0 {
			t.Fatalf("%s has signing info %+v (%v)", op.Moniker, info, ok)
		}
	}
	if len(infos) != len(parsed) {
		t.Fatalf("%d signing infos for %d operators", len(infos), len(parsed))
	}
}

// TestTheGenesisCarriesTheEvidenceWindow proves that the evidence window is
// 2,000 blocks with the least duration CometBFT accepts, so the block count
// decides.
func TestTheGenesisCarriesTheEvidenceWindow(t *testing.T) {
	a := tmp(t)
	doc, _ := decode(t, a, build(t, a, four()))
	if e := doc.Consensus.Params.Evidence; e.MaxAgeNumBlocks != 2000 || e.MaxAgeDuration != time.Nanosecond {
		t.Fatalf("evidence params are %+v, want 2000 blocks and 1ns", e)
	}
}

// TestTheSuppliedAccountCanSignAGovernanceDeposit proves that the account the
// file names is a bech32 address that its own key signs for. The zero-flag
// default derives the account from the consensus key, so only the key that
// signs blocks can vote for it in x/gov. A governance deposit shows the
// difference.
func TestTheSuppliedAccountCanSignAGovernanceDeposit(t *testing.T) {
	a := tmp(t)
	key := tc.OperatorKey(0)
	ops := four()
	parsed, err := operators(a.Codec, nil, fileFor(t, a.Codec, ops))
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed[0].Account.String(); got != tc.Address(key).String() {
		t.Fatalf("genesis account is %s, want the supplied %s", got, tc.Address(key))
	}

	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Operators: parsed})
	c.Block()
	params := app.GovParams()
	update := &govv1.MsgUpdateParams{
		Authority: authtypes.NewModuleAddress(govtypes.ModuleName).String(),
		Params:    params,
	}
	proposal, err := govv1.NewMsgSubmitProposal([]sdk.Msg{update}, params.MinDeposit,
		tc.Address(key).String(), "", "title", "summary", false)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := c.Deliver(c.Cosmos(key, proposal))
	if res[0].Code != 0 {
		t.Fatalf("the supplied account could not deposit: code %d, %s", res[0].Code, res[0].Log)
	}
}

// TestADuplicatedOperatorIsRefused proves that svmd refuses a repeated
// consensus pubkey, as Agave exits non-zero on a repeated pubkey in the
// --bootstrap-validator list. It also refuses a repeated account, which is
// the same mistake in the other field.
func TestADuplicatedOperatorIsRefused(t *testing.T) {
	a := tmp(t)
	ops := four()
	flags := flagsFor(ops)

	repeated := append(append([]string{}, flags...), flags[1])
	if _, err := operators(a.Codec, repeated, ""); err == nil ||
		!strings.Contains(err.Error(), "consensus pubkey") {
		t.Fatalf("a repeated operator was accepted: %v", err)
	}

	// The same account under a second consensus key.
	fields := strings.SplitN(flags[2], ",", 5)
	fields[0] = base64.StdEncoding.EncodeToString(tc.ConsensusPubkey(tc.OperatorKey(9)).Bytes())
	if _, err := operators(a.Codec, append(append([]string{}, flags...), strings.Join(fields, ",")), ""); err == nil ||
		!strings.Contains(err.Error(), "same account") {
		t.Fatalf("a repeated account was accepted: %v", err)
	}

	// Malformed input fails before either check: a bad pubkey, a bad account
	// and a short tuple.
	for _, bad := range []string{
		"not-base64,cosmos1,1,1,m",
		strings.Join([]string{base64.StdEncoding.EncodeToString(ops[0].ConsensusPubkey.Bytes()), "not-bech32", "1", "1", "m"}, ","),
		"a,b,c",
	} {
		if _, err := operators(a.Codec, []string{bad}, ""); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}

	// app.Permission refuses a self-bond below one unit of voting power, this
	// chain's analogue of Agave's rent-exempt floor. The refusal must stay
	// reachable from finishGenesis.
	small := []app.Operator{{
		Account:         ops[0].Account,
		ConsensusPubkey: ops[0].ConsensusPubkey,
		SelfBond:        math.NewInt(1),
		Free:            math.NewInt(1),
		Moniker:         "dust",
	}}
	cfg := home(t, a)
	if err := finishGenesis(a.Codec, cfg, "solo", small); err == nil ||
		!strings.Contains(err.Error(), "voting power") {
		t.Fatalf("a dust self-bond was bonded: %v", err)
	}
}

// TestNoOperatorFlagsBondsThisMachine proves that `init` with no operator
// flags bonds this machine alone. internal/testchain, the localnet
// instructions and the other tests depend on this default.
func TestNoOperatorFlagsBondsThisMachine(t *testing.T) {
	a := tmp(t)
	cfg := home(t, a)
	if err := os.MkdirAll(filepath.Dir(cfg.PrivValidatorKeyFile()), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.PrivValidatorStateFile()), 0o750); err != nil {
		t.Fatal(err)
	}
	pv := privval.GenFilePV(cfg.PrivValidatorKeyFile(), cfg.PrivValidatorStateFile())
	pv.Save()

	parsed, err := operators(a.Codec, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 0 {
		t.Fatalf("no flags parsed %d operators", len(parsed))
	}
	if err := finishGenesis(a.Codec, cfg, "solo", parsed); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfg.GenesisFile())
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := decode(t, a, raw)
	if len(doc.Consensus.Validators) != 1 {
		t.Fatalf("%d validators, want this machine alone", len(doc.Consensus.Validators))
	}
	if got := doc.Consensus.Validators[0].Address.String(); got != pv.Key.Address.String() {
		t.Fatalf("the bonded validator is %s, want this machine's %s", got, pv.Key.Address)
	}
	if doc.Consensus.Validators[0].Name != "solo" {
		t.Fatalf("moniker %q", doc.Consensus.Validators[0].Name)
	}
}

// TestStartRefusesAGenesisItDidNotExpect proves that start refuses a genesis
// that does not match --expected-genesis-hash, with an error that names both
// values. CometBFT's handshake compares no genesis hash, so without this check
// a mismatch shows only as a failed app hash at block 1.
func TestStartRefusesAGenesisItDidNotExpect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "genesis.json")
	if err := os.WriteFile(path, []byte(`{"chain_id":"svm-expect-test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := genesis.Hash(path)
	if err != nil {
		t.Fatal(err)
	}
	expected := base58.Encode(hash[:])

	// init prints the same string that getGenesisHash returns, so the two can
	// be compared.
	if err := requireGenesisHash(expected, path); err != nil {
		t.Fatalf("a matching hash was refused: %v", err)
	}
	if err := requireGenesisHash("", path); err != nil {
		t.Fatalf("an unset flag was refused: %v", err)
	}
	wrong := base58.Encode(make([]byte, 32))
	err = requireGenesisHash(wrong, path)
	if err == nil {
		t.Fatal("a mismatched genesis started")
	}
	if !strings.Contains(err.Error(), expected) || !strings.Contains(err.Error(), wrong) {
		t.Fatalf("the refusal names neither value: %v", err)
	}
}

// TestThereIsNoGentxAndNoKeyring proves that the app does not mount
// x/genutil, although `init` uses its CLI. svmd collects no genesis
// transaction, and no account key reaches this binary.
func TestThereIsNoGentxAndNoKeyring(t *testing.T) {
	a := tmp(t)
	for _, name := range a.Modules.ModuleNames() {
		if name == "genutil" {
			t.Fatal("x/genutil is mounted")
		}
	}
	if _, ok := a.DefaultGenesis()["genutil"]; ok {
		t.Fatal("genesis carries a genutil section")
	}
}

// TestInitBondsFourAndPrintsTheHash runs `init` as an operator does, to prove
// the --validator flags and the printed hash. The printed hash is the string that
// getGenesisHash returns, so the runbook's comparison step works.
func TestInitBondsFourAndPrintsTheHash(t *testing.T) {
	dir := t.TempDir()
	a := tmp(t)
	ops := four()

	out := &bytes.Buffer{}
	root := rootCmd(dir)
	root.SetOut(out)
	root.SetErr(out)
	args := []string{"init", "coordinator", "--home", dir, "--chain-id", "svm-init-test"}
	for _, tuple := range flagsFor(ops) {
		args = append(args, "--"+flagValidator, tuple)
	}
	root.SetArgs(args)
	if err := svrcmd.Execute(root, "SVMD", dir); err != nil {
		t.Fatalf("init: %v", err)
	}

	cfg := cmtcfg.DefaultConfig()
	cfg.SetRoot(dir)
	hash, err := genesis.Hash(cfg.GenesisFile())
	if err != nil {
		t.Fatal(err)
	}
	printed := "genesis hash: " + base58.Encode(hash[:])
	if !strings.Contains(out.String(), printed) {
		t.Fatalf("init did not print %q", printed)
	}

	raw, err := os.ReadFile(cfg.GenesisFile())
	if err != nil {
		t.Fatal(err)
	}
	doc, state := decode(t, a, raw)
	if len(doc.Consensus.Validators) != 4 {
		t.Fatalf("%d validators, want 4", len(doc.Consensus.Validators))
	}
	if doc.Consensus.Params.Block.MaxGas != -1 {
		t.Fatalf("block.max_gas is %d, want -1", doc.Consensus.Params.Block.MaxGas)
	}
	var staking stakingtypes.GenesisState
	if err := a.Codec.UnmarshalJSON(state[stakingtypes.ModuleName], &staking); err != nil {
		t.Fatal(err)
	}
	if staking.Params.BondDenom != app.BondDenom {
		t.Fatalf("bond denom %q", staking.Params.BondDenom)
	}
	for i, op := range ops {
		if got := staking.Delegations[i].DelegatorAddress; got != op.Account.String() {
			t.Fatalf("delegation %d is from %s, want the supplied %s", i, got, op.Account)
		}
	}

	// A second machine that hashes the same file gets the same string.
	if err := requireGenesisHash(base58.Encode(hash[:]), cfg.GenesisFile()); err != nil {
		t.Fatalf("start refused the genesis init wrote: %v", err)
	}
}
