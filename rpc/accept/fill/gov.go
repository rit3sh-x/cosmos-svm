package main

import (
	"fmt"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	govv1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1"

	"github.com/rit3sh-x/cosmos-svm/app"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// rentCut2575 is SIMD-0437-3, set_lamports_per_byte_to_2575. Mainnet has not
// taken this rent cut, so it is off here, and flip.sh schedules it.
const rentCut2575 = "rntCigrTppP5JdZz7K8TyN9sMzLdAcXp8SejYpVpX6D"

// proposeGate is -mode propose-gate. The operator in home proposes the 2,575
// rent cut at height through x/gov. proposeGate prints the proposal id when
// the proposal is in voting.
func proposeGate(home, node, chain string, height int64) error {
	op := loadOperator(home)
	msg := &svm.MsgScheduleGates{
		Authority: authtypes.NewModuleAddress(govtypes.ModuleName).String(),
		Height:    uint64(height),
		Gates:     []string{rentCut2575},
	}
	submit, err := govv1.NewMsgSubmitProposal([]sdk.Msg{msg}, app.GovParams().MinDeposit, op.account.String(), "",
		"set_lamports_per_byte_to_2575", "Cut rent to 2,575 lamports a byte (SIMD-0437-3).", false)
	if err != nil {
		return err
	}
	if err := broadcast(node, chain, op, submit); err != nil {
		return err
	}
	return printVoting(node, op)
}

// printVoting prints the id of the newest proposal in voting that the operator
// deposited on.
func printVoting(node string, op operator) error {
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		var out govv1.QueryProposalsResponse
		err := abciQuery(node, "/cosmos.gov.v1.Query/Proposals",
			&govv1.QueryProposalsRequest{ProposalStatus: govv1.StatusVotingPeriod, Depositor: op.account.String()}, &out)
		if err == nil && len(out.Proposals) > 0 {
			fmt.Println(out.Proposals[len(out.Proposals)-1].Id)
			return nil
		}
	}
	return fmt.Errorf("the proposal was not in voting a minute after it was sent")
}

// proposalStatus is -mode proposal. It prints the status of one proposal, and
// the reason if the proposal failed.
func proposalStatus(node string, id uint64) error {
	var out govv1.QueryProposalResponse
	if err := abciQuery(node, "/cosmos.gov.v1.Query/Proposal", &govv1.QueryProposalRequest{ProposalId: id}, &out); err != nil {
		return err
	}
	fmt.Println(out.Proposal.Status.String(), out.Proposal.FailedReason)
	return nil
}

// vote is -mode vote. The operator in home votes yes.
func vote(home, node, chain string, id uint64) error {
	op := loadOperator(home)
	return broadcast(node, chain, op, govv1.NewMsgVote(op.account, id, govv1.OptionYes, ""))
}
