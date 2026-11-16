package app_test

import (
	"strings"
	"testing"

	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// Engine result statuses, as the ExecTxResult code carries them.
const (
	codeOK           = 0
	codeFailed       = 1
	codeNotProcessed = 2
)

// TestABadSignatureIsNotProcessedAndTheBlockStands proves that a transaction
// with a bad signature is not processed, with no fee and no writes. The rest of
// the block executes, and the alarm counter rises by one. A block reaches
// FinalizeBlock on two thirds of the votes even if this node's ProcessProposal
// refused it, so a halt here would let those votes halt the chain.
func TestABadSignatureIsNotProcessedAndTheBlockStands(t *testing.T) {
	svm.RegisterMetrics(nil)
	faucet := tc.Key(1)
	paid, unpaid := tc.Pub(tc.Key(2)), tc.Pub(tc.Key(3))
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()

	bad := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, unpaid, 1_000_000_000))
	bad[5] ^= 0xff // a signature byte, after the one-byte length prefix
	good := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, paid, 1_000_000_000))

	before := c.Supply()
	alarms := labelled(scrape(t)["svm_block_admission_failed_total"], "signature")
	res, _ := c.Block(bad, good)
	if got := labelled(scrape(t)["svm_block_admission_failed_total"], "signature") - alarms; got != 1 {
		t.Fatalf("the alarm counter rose by %v, want 1", got)
	}
	if res[0].Code != codeNotProcessed {
		t.Fatalf("the bad signature gave code %d, want %d", res[0].Code, codeNotProcessed)
	}
	if res[1].Code != codeOK {
		t.Fatalf("the transaction beside it gave code %d", res[1].Code)
	}
	if got := c.Lamports(unpaid); got != 0 {
		t.Fatalf("the dropped transaction moved %d lamports", got)
	}
	if got := c.Lamports(paid); got != 1_000_000_000 {
		t.Fatalf("the executed transfer moved %d lamports", got)
	}
	// One fee, for one processed transaction.
	if fell := before - c.Supply(); fell != fee {
		t.Fatalf("supply fell by %d, want one fee %d", fell, fee)
	}
	// The chain is still live.
	if res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, paid, 1))); res[0].Code != codeOK {
		t.Fatalf("the block after a dropped transaction: code %d", res[0].Code)
	}
}

// TestAFailedTransferChargesTheFeeAndNothingElse covers the executed-and-failed
// result case. The write set is only the rollback of the fee payer, and the fee
// burns.
func TestAFailedTransferChargesTheFeeAndNothingElse(t *testing.T) {
	faucet := tc.Key(1)
	dest := tc.Pub(tc.Key(2))
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()

	before := c.Supply()
	res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest, tc.FaucetLamports)))
	if res[0].Code != codeFailed {
		t.Fatalf("a transfer beyond the balance gave code %d, want %d", res[0].Code, codeFailed)
	}
	if got, want := c.Lamports(tc.Pub(faucet)), uint64(tc.FaucetLamports-fee); got != want {
		t.Fatalf("the payer holds %d lamports, want %d: the fee and nothing else", got, want)
	}
	if got := c.Lamports(dest); got != 0 {
		t.Fatalf("the destination of a failed transfer holds %d lamports", got)
	}
	if fell := before - c.Supply(); fell != fee {
		t.Fatalf("supply fell by %d, want the burned fee %d", fell, fee)
	}
}

// TestAnUnknownProgramIsChargedTheFeeOnly covers the fees-only result case.
// Loading fails before execution, so the write set is the rollback accounts.
func TestAnUnknownProgramIsChargedTheFeeOnly(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()

	before := c.Supply()
	res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Instruction{
		Program:  tc.Pub(tc.Key(240)),
		Accounts: []tc.Meta{tc.Signer(tc.Pub(faucet), true)},
		Data:     []byte{0},
	}))
	if res[0].Code == codeOK {
		t.Fatal("a transaction naming a program that does not exist succeeded")
	}
	if got, want := c.Lamports(tc.Pub(faucet)), uint64(tc.FaucetLamports-fee); got != want {
		t.Fatalf("the payer holds %d lamports, want %d", got, want)
	}
	if fell := before - c.Supply(); fell != fee {
		t.Fatalf("supply fell by %d, want the burned fee %d", fell, fee)
	}
}

// TestAnEmptiedSysvarSlotHaltsTheBlock proves that a missing sysvar halts the
// machine, which names the slot and commits nothing. A sysvar that the fill
// cannot find means a broken writer or a broken genesis, never a bad block.
func TestAnEmptiedSysvarSlotHaltsTheBlock(t *testing.T) {
	faucet := tc.Key(1)
	missing := base58.Encode(svm.StakeHistoryID[:])
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Edit: func(g *svm.GenesisState) {
		kept := g.Accounts[:0]
		for _, a := range g.Accounts {
			if a.Pubkey != missing {
				kept = append(kept, a)
			}
		}
		g.Accounts = kept
	}})
	c.Block()

	height, hash := c.Height, c.App.LastCommitID()
	err := c.BlockErr(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), 1_000_000_000)))
	if !strings.Contains(err.Error(), "StakeHistory") {
		t.Fatalf("the halt does not name the empty slot: %v", err)
	}
	if c.App.LastBlockHeight() != height {
		t.Fatalf("the halted block committed: height %d, want %d", c.App.LastBlockHeight(), height)
	}
	if got := c.App.LastCommitID(); string(got.Hash) != string(hash.Hash) {
		t.Fatalf("the halted block moved the app hash to %x", got.Hash)
	}
}

// TestABlockAfterARestartMatchesAWarmEngine proves that a restarted machine
// reaches the same app hashes as a warm one. The sysvars are committed state,
// so a cold engine reads the same Clock that a warm one holds.
func TestABlockAfterARestartMatchesAWarmEngine(t *testing.T) {
	run := func(restartAt int) [][]byte {
		faucet := tc.Key(1)
		c := tc.Start(t, tc.Pub(faucet), tc.Options{})
		c.Block()
		var hashes [][]byte
		for i := 0; i < 5; i++ {
			if i == restartAt {
				c.Restart()
			}
			_, hash := c.Block(tc.Tx(c.Latest(), faucet, nil,
				tc.Transfer(faucet, tc.Pub(tc.Key(byte(20+i))), 1_000_000_000)))
			hashes = append(hashes, hash)
		}
		return hashes
	}
	warm, restarted := run(-1), run(3)
	for i := range warm {
		if string(warm[i]) != string(restarted[i]) {
			t.Fatalf("block %d: warm %x, after a restart %x", i+1, warm[i], restarted[i])
		}
	}
}
