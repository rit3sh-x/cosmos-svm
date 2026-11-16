package app_test

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	govv1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1"

	"github.com/rit3sh-x/cosmos-svm/app"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// operators starts a chain from a four-machine genesis. Four is the smallest
// set where one vote does not trivially meet a 0.667 quorum.
func operators(t *testing.T) (*tc.Chain, []ed25519.PrivateKey) {
	t.Helper()
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Operators: tc.DefaultOperators(4)})
	c.Block()
	keys := make([]ed25519.PrivateKey, 4)
	for i := range keys {
		keys[i] = tc.OperatorKey(byte(i))
	}
	return c, keys
}

func govAuthority() string {
	return authtypes.NewModuleAddress(govtypes.ModuleName).String()
}

// submit proposes messages with the full deposit and returns the proposal id.
func submit(t *testing.T, c *tc.Chain, proposer ed25519.PrivateKey, expedited bool, msgs ...sdk.Msg) uint64 {
	t.Helper()
	params := app.GovParams()
	deposit := params.MinDeposit
	if expedited {
		deposit = params.ExpeditedMinDeposit
	}
	proposal, err := govv1.NewMsgSubmitProposal(msgs, deposit, tc.Address(proposer).String(),
		"", "title", "summary", expedited)
	if err != nil {
		t.Fatal(err)
	}
	before := next(t, c)
	res, _ := c.Deliver(c.Cosmos(proposer, proposal))
	if res[0].Code != 0 {
		t.Fatalf("submit: code %d, %s", res[0].Code, res[0].Log)
	}
	return before
}

// next returns the id that the next proposal takes.
func next(t *testing.T, c *tc.Chain) uint64 {
	t.Helper()
	id, err := c.App.Gov.ProposalID.Peek(ctxOf(c))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ctxOf(c *tc.Chain) sdk.Context {
	return c.App.NewUncachedContext(false, cmtproto.Header{Height: c.Height})
}

func proposal(t *testing.T, c *tc.Chain, id uint64) govv1.Proposal {
	t.Helper()
	p, err := c.App.Gov.Proposals.Get(ctxOf(c), id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// vote casts one vote for each key. A refused vote fails the test.
func vote(t *testing.T, c *tc.Chain, id uint64, option govv1.VoteOption, keys ...ed25519.PrivateKey) {
	t.Helper()
	txs := make([][]byte, 0, len(keys))
	for _, k := range keys {
		txs = append(txs, c.Cosmos(k, govv1.NewMsgVote(tc.Address(k), id, option, "")))
	}
	res, _ := c.Deliver(txs...)
	for i, r := range res {
		if r.Code != 0 {
			t.Fatalf("vote %d: code %d, %s", i, r.Code, r.Log)
		}
	}
}

// tally moves the clock past the voting period and runs the block whose
// EndBlocker decides the proposal.
func tally(t *testing.T, c *tc.Chain, expedited bool) {
	t.Helper()
	period := *app.GovParams().VotingPeriod
	if expedited {
		period = *app.GovParams().ExpeditedVotingPeriod
	}
	c.Advance(period + time.Minute)
	c.Deliver()
}

func balance(t *testing.T, c *tc.Chain, who sdk.AccAddress) math.Int {
	t.Helper()
	return c.App.Bank.GetBalance(ctxOf(c), who, app.BondDenom).Amount
}

// TestTheParameterBlockIsTheDecisions proves that the gov parameters of the
// started chain have the chosen values. It reads chain state, not
// DefaultParams. Six of the values differ from the defaults beyond the denom.
func TestTheParameterBlockIsTheDecisions(t *testing.T) {
	c, _ := operators(t)
	got, err := c.App.Gov.Params.Get(ctxOf(c))
	if err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	for _, want := range []struct {
		what string
		a, b any
	}{
		{"min deposit", sdk.Coins(got.MinDeposit).String(), "10000000usvm"},
		{"expedited min deposit", sdk.Coins(got.ExpeditedMinDeposit).String(), "20000000usvm"},
		{"voting period", *got.VotingPeriod, day},
		{"expedited voting period", *got.ExpeditedVotingPeriod, day / 2},
		{"quorum", dec(t, got.Quorum), "0.667"},
		{"threshold", dec(t, got.Threshold), "0.5"},
		{"expedited threshold", dec(t, got.ExpeditedThreshold), "0.667"},
		{"veto threshold", dec(t, got.VetoThreshold), "0.334"},
		{"min initial deposit ratio", dec(t, got.MinInitialDepositRatio), "1"},
		{"burn on veto", got.BurnVoteVeto, false},
		{"burn on quorum failure", got.BurnVoteQuorum, false},
		{"cancel destination", got.ProposalCancelDest, ""},
	} {
		if want.a != want.b {
			t.Fatalf("%s is %v, want %v", want.what, want.a, want.b)
		}
	}
}

// dec renders a stored decimal without its trailing zeros, so the table above
// shows the chosen values as written.
func dec(t *testing.T, v string) string {
	t.Helper()
	d, err := math.LegacyNewDecFromStr(v)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(strings.TrimRight(d.String(), "0"), ".")
}

// TestADepositReachesAModuleAccountTheDenomCannotBeSentTo proves that a
// deposit reaches the gov module account although usvm is send-disabled. The
// bank checks IsSendEnabledCoins only on MsgSend and MsgMultiSend. Because no
// account can send usvm, an outsider cannot get any to deposit or vote with.
func TestADepositReachesAModuleAccountTheDenomCannotBeSentTo(t *testing.T) {
	c, keys := operators(t)
	module := authtypes.NewModuleAddress(govtypes.ModuleName)
	if before := balance(t, c, module); !before.IsZero() {
		t.Fatalf("the gov module account opens holding %s", before)
	}

	// The denom must be send-disabled. That makes the rest of this test a
	// permission, not a convention.
	send := banktypes.NewMsgSend(tc.Address(keys[0]), tc.Address(keys[1]),
		sdk.NewCoins(sdk.NewCoin(app.BondDenom, math.NewInt(1))))
	if res, _ := c.Deliver(c.Cosmos(keys[0], send)); res[0].Code == 0 {
		t.Fatal("usvm was sent between accounts")
	}

	id := submit(t, c, keys[0], false, gateProposal(t, c, rent1322))
	if got := balance(t, c, module); !got.Equal(math.NewInt(10_000_000)) {
		t.Fatalf("the module account holds %s after a deposit", got)
	}
	// A deposit in any other denom is refused, because the parameters name
	// only usvm.
	other := govv1.NewMsgDeposit(tc.Address(keys[0]), id,
		sdk.NewCoins(sdk.NewCoin("stake", math.NewInt(1))))
	if res, _ := c.Deliver(c.Cosmos(keys[0], other)); res[0].Code == 0 {
		t.Fatal("a deposit in a denom the chain does not have was accepted")
	}
}

// TestThereIsNoReachableDepositPeriod proves that a proposal enters its voting
// period in the block that submits it. The full deposit at submission makes
// submission and voting start the same event. The cancel-race arithmetic
// assumes this.
func TestThereIsNoReachableDepositPeriod(t *testing.T) {
	c, keys := operators(t)
	id := submit(t, c, keys[0], false, gateProposal(t, c, rent1322))
	p := proposal(t, c, id)
	if p.Status != govv1.StatusVotingPeriod {
		t.Fatalf("a proposal with the full deposit is %s, want the voting period", p.Status)
	}
	if p.VotingStartTime == nil {
		t.Fatal("voting has not started in the block the proposal was submitted in")
	}

	// A partial deposit fails the submission and leaves no pending proposal.
	// That is what having no deposit period means.
	partial, err := govv1.NewMsgSubmitProposal([]sdk.Msg{gateProposal(t, c, rent1322)},
		sdk.NewCoins(sdk.NewCoin(app.BondDenom, math.NewInt(1))),
		tc.Address(keys[0]).String(), "", "title", "summary", false)
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := c.Deliver(c.Cosmos(keys[0], partial)); res[0].Code == 0 {
		t.Fatal("a proposal with one lamport of deposit was accepted")
	}
}

// TestAGateProposalSchedulesAtTheFloor proves that the expedited path halves
// the voting period but cannot shorten the 432,000-block delay. That floor is in
// x/svm and counts from the height at which the proposal executes.
func TestAGateProposalSchedulesAtTheFloor(t *testing.T) {
	for _, expedited := range []bool{false, true} {
		c, keys := operators(t)
		// The floor counts from the execution height, which nobody can predict
		// exactly, so the proposal asks for 10 blocks more than the floor,
		// counted from its submission height.
		at := uint64(c.Height) + svm.MinActivationDelay + 10
		id := submit(t, c, keys[0], expedited, gateProposal(t, c, rent1322, at))
		vote(t, c, id, govv1.OptionYes, keys[0], keys[1], keys[2])
		tally(t, c, expedited)

		if got := proposal(t, c, id).Status; got != govv1.StatusPassed {
			t.Fatalf("expedited %v: the proposal is %s", expedited, got)
		}
		// This test ends at the scheduled entry. TestARentGateRepricesAtItsHeight
		// starts from a pending entry and drives it through the begin-blocker
		// at its height. Together they cover a proposal that passes and a gate
		// that then activates. No test drives 432,000 blocks to join them.
		pending := c.App.Stores.PendingGates(c.App.CommitMultiStore(), at)
		if len(pending) != 1 || pending[0] != key(t, rent1322) {
			t.Fatalf("expedited %v: %d gates are scheduled for %d", expedited, len(pending), at)
		}
		// The same proposal for a height inside the floor fails to execute.
		// The expedited path buys a faster vote, not an earlier flip.
		tooSoon := submit(t, c, keys[0], expedited, gateProposal(t, c, rent1322, uint64(c.Height)+10))
		vote(t, c, tooSoon, govv1.OptionYes, keys[0], keys[1], keys[2])
		tally(t, c, expedited)
		if got := proposal(t, c, tooSoon).Status; got != govv1.StatusFailed {
			t.Fatalf("expedited %v: a flip inside the floor is %s, want failed", expedited, got)
		}
	}
}

// TestAnExpeditedCancelBeatsTheGate proves that an expedited cancel drops a
// scheduled gate long before its height. A cancel takes effect when it
// executes, so the halved voting period halves the time of an emergency undo.
func TestAnExpeditedCancelBeatsTheGate(t *testing.T) {
	c, keys := operators(t)
	at := uint64(c.Height) + svm.MinActivationDelay + 10
	id := submit(t, c, keys[0], false, gateProposal(t, c, rent1322, at))
	vote(t, c, id, govv1.OptionYes, keys[0], keys[1], keys[2])
	tally(t, c, false)
	if len(c.App.Stores.PendingGates(c.App.CommitMultiStore(), at)) != 1 {
		t.Fatal("the flip was not scheduled")
	}

	cancel := &svm.MsgCancelGates{Authority: govAuthority(), Height: at, Gates: []string{rent1322}}
	undo := submit(t, c, keys[1], true, cancel)
	vote(t, c, undo, govv1.OptionYes, keys[0], keys[1], keys[2])
	tally(t, c, true)
	if got := proposal(t, c, undo).Status; got != govv1.StatusPassed {
		t.Fatalf("the cancel is %s", got)
	}
	if got := c.App.Stores.PendingGates(c.App.CommitMultiStore(), at); len(got) != 0 {
		t.Fatalf("%d gates survive the cancel", len(got))
	}
	// The cancel executes well inside the floor, which is the purpose of the
	// fast path.
	if uint64(c.Height) >= at {
		t.Fatalf("the cancel executed at height %d, at or past the gate's %d", c.Height, at)
	}
}

// TestAVetoedProposalKeepsItsDeposit proves that a vetoed proposal returns its
// deposit and leaves the supply unchanged. usvm has no issuance, so a burned
// deposit would leave the permission denom for good.
func TestAVetoedProposalKeepsItsDeposit(t *testing.T) {
	c, keys := operators(t)
	proposer := tc.Address(keys[0])
	opening := balance(t, c, proposer)
	supply := c.App.Bank.GetSupply(ctxOf(c), app.BondDenom).Amount

	id := submit(t, c, keys[0], false, gateProposal(t, c, rent1322))
	if got := balance(t, c, proposer); !got.Equal(opening.SubRaw(10_000_000)) {
		t.Fatalf("the proposer holds %s after depositing", got)
	}
	vote(t, c, id, govv1.OptionNoWithVeto, keys[0], keys[1], keys[2])
	tally(t, c, false)

	if got := proposal(t, c, id).Status; got != govv1.StatusRejected {
		t.Fatalf("a vetoed proposal is %s", got)
	}
	if got := balance(t, c, proposer); !got.Equal(opening) {
		t.Fatalf("the proposer holds %s after the veto, want the %s they started with", got, opening)
	}
	if got := c.App.Bank.GetSupply(ctxOf(c), app.BondDenom).Amount; !got.Equal(supply) {
		t.Fatalf("the veto moved the supply from %s to %s", supply, got)
	}
}

// gateProposal returns a MsgScheduleGates with the governance authority. Its
// height is past the floor, unless the caller gives one.
func gateProposal(t *testing.T, c *tc.Chain, gate string, at ...uint64) sdk.Msg {
	t.Helper()
	height := uint64(c.Height) + svm.MinActivationDelay + 100
	if len(at) == 1 {
		height = at[0]
	}
	return &svm.MsgScheduleGates{Authority: govAuthority(), Height: height, Gates: []string{gate}}
}
