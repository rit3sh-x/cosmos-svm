package app_test

import (
	"bytes"
	"crypto/ed25519"
	"slices"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// result holds the four fields of a transaction result that CometBFT hashes
// into the next header.
type result struct {
	Code      uint32
	Data      string
	GasWanted int64
	GasUsed   int64
}

func results(res []*abci.ExecTxResult, _ []byte) []result {
	out := make([]result, len(res))
	for i, r := range res {
		out[i] = result{r.Code, string(r.Data), r.GasWanted, r.GasUsed}
	}
	return out
}

// heldResults is how many results the engine's pool tier holds.
func heldResults(t *testing.T) uint64 {
	t.Helper()
	stats, err := ffi.EngineStats()
	if err != nil {
		t.Fatal(err)
	}
	return stats.SignatureResultsHeld[0]
}

// payments is n transfers from payer to n new accounts, on the last block's
// hash.
func payments(c *tc.Chain, payer ed25519.PrivateKey, n int) [][]byte {
	txs := make([][]byte, n)
	for i := range txs {
		txs[i] = tc.Tx(c.Latest(), payer, nil, tc.Transfer(payer, tc.Pub(tc.Key(byte(100+i))), 1_000_000_000))
	}
	return txs
}

// verified is how many transactions ed25519 checked while f ran.
func verified(f func()) uint64 {
	before := ffi.SignatureVerifications()
	f()
	return ffi.SignatureVerifications() - before
}

// TestABlockFromThePoolChecksNoSignatureInPhaseOne proves that phase 1 reuses
// what CheckTx New checked, so a block made of this node's own pool checks no
// signature again.
func TestABlockFromThePoolChecksNoSignatureInPhaseOne(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	ffi.ClearSignatureResultsForTests()

	txs := payments(c, faucet, 4)
	if n := verified(func() {
		for _, tx := range txs {
			if res := c.Check(c.Wrap(tx), false); res.Code != 0 {
				t.Fatalf("CheckTx New: code %d, log %s", res.Code, res.Log)
			}
		}
	}); n != 4 {
		t.Fatalf("CheckTx New checked %d transactions, want 4", n)
	}
	var res []result
	if n := verified(func() { res = results(c.Block(txs...)) }); n != 0 {
		t.Fatalf("phase 1 checked %d transactions from this node's pool, want 0", n)
	}
	for i, r := range res {
		if r.Code != codeOK {
			t.Fatalf("transaction %d gave code %d", i, r.Code)
		}
	}
}

// TestABlockWithNoStoredResultChecksEverySignature proves that a node that
// never saw the transactions checks each one in phase 1.
func TestABlockWithNoStoredResultChecksEverySignature(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	ffi.ClearSignatureResultsForTests()

	txs := payments(c, faucet, 4)
	if n := verified(func() { c.Block(txs...) }); n != 4 {
		t.Fatalf("phase 1 checked %d of 4 transactions it held no result for", n)
	}
}

// TestARefusedTransactionLeavesNoResult proves that a result enters only when
// every admission check passed, not only the signatures. A payer that cannot
// pay passes ed25519 at CheckTx New and is refused after it, so a block that
// carries the transaction anyway checks it again.
func TestARefusedTransactionLeavesNoResult(t *testing.T) {
	faucet, broke := tc.Key(1), tc.Key(7)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	ffi.ClearSignatureResultsForTests()

	tx := tc.Tx(c.Latest(), broke, nil, tc.Transfer(broke, tc.Pub(tc.Key(8)), 1))
	if n := verified(func() {
		if res := c.Check(c.Wrap(tx), false); res.Code == 0 {
			t.Fatal("a payer with no account was admitted")
		}
	}); n != 1 {
		t.Fatalf("CheckTx New checked %d transactions, want 1", n)
	}
	if held := heldResults(t); held != 0 {
		t.Fatalf("the pool tier holds %d results after a refusal", held)
	}
	if n := verified(func() { c.Block(tx) }); n != 1 {
		t.Fatalf("phase 1 checked %d transactions, want 1", n)
	}
}

// TestMachinesWithEveryResultNoneOrHalfAgree proves that a result changes how
// long a block takes, never what it answers. Machines that hold a result for
// every transaction, for none as after a restart, and for half finalize the
// same blocks, one with a bad signature, to equal results and app hashes.
func TestMachinesWithEveryResultNoneOrHalfAgree(t *testing.T) {
	faucet := tc.Key(1)
	machines := []struct {
		name string
		keep func(int) bool
		c    *tc.Chain
	}{
		{"every", func(int) bool { return true }, nil},
		{"none", func(int) bool { return false }, nil},
		{"half", func(i int) bool { return i%2 == 0 }, nil},
	}
	for i := range machines {
		machines[i].c = tc.Start(t, tc.Pub(faucet), tc.Options{})
		machines[i].c.Block()
	}
	for b := range 3 {
		first := machines[0].c
		txs := make([][]byte, 6)
		for i := range txs {
			txs[i] = tc.Tx(first.Latest(), faucet, nil,
				tc.Transfer(faucet, tc.Pub(tc.Key(byte(100+i))), uint64(1_000_000_000+b)))
		}
		if b == 1 {
			txs[3][5] ^= 0xff // a signature byte, after the one-byte length prefix
		}
		var want []result
		var wantHash []byte
		for _, m := range machines {
			ffi.ClearSignatureResultsForTests()
			for i, tx := range txs {
				if !m.keep(i) {
					continue
				}
				if res := m.c.Check(m.c.Wrap(tx), false); (res.Code == 0) == (b == 1 && i == 3) {
					t.Fatalf("block %d: CheckTx New of transaction %d on %s gave code %d", b, i, m.name, res.Code)
				}
			}
			res, hash := m.c.Block(txs...)
			got := results(res, hash)
			if want == nil {
				want, wantHash = got, hash
				continue
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("block %d transaction %d: %s gave %+v, %s gave %+v",
						b, i, m.name, got[i], machines[0].name, want[i])
				}
			}
			if !bytes.Equal(hash, wantHash) {
				t.Fatalf("block %d: %s reached app hash %x, %s %x", b, m.name, hash, machines[0].name, wantHash)
			}
		}
		if b == 1 && want[3].Code != codeNotProcessed {
			t.Fatalf("the bad signature gave code %d, want %d", want[3].Code, codeNotProcessed)
		}
	}
}

const (
	accept = abci.ResponseProcessProposal_ACCEPT
	refuse = abci.ResponseProcessProposal_REJECT
)

// wrapped is txs, each in its Cosmos envelope, as a proposal carries them.
func wrapped(c *tc.Chain, txs ...[]byte) [][]byte {
	out := make([][]byte, len(txs))
	for i, tx := range txs {
		out[i] = c.Wrap(tx)
	}
	return out
}

// TestAProposalWithABadSignatureIsRefused proves that ProcessProposal refuses
// a proposal that carries a bad signature beside a valid transaction. The same
// proposal with the bad transaction replaced is accepted. Once that block is
// finalized, the valid transaction has code OK, its lamports moved, and its
// payer paid one fee.
func TestAProposalWithABadSignatureIsRefused(t *testing.T) {
	svm.RegisterMetrics(nil)
	faucet, other := tc.Key(1), tc.Key(4)
	paid, unpaid := tc.Pub(tc.Key(2)), tc.Pub(tc.Key(3))
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(other), 10_000_000_000)))
	ffi.ClearSignatureResultsForTests()

	good := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, paid, 1_000_000_000))
	bad := tc.Tx(c.Latest(), other, nil, tc.Transfer(other, unpaid, 1_000_000_000))
	bad[5] ^= 0xff // a signature byte, after the one-byte length prefix
	refused := labelled(scrape(t)["svm_proposals_refused_total"], "signature")
	if got := c.Propose(wrapped(c, good, bad)...); got != refuse {
		t.Fatalf("a proposal with a bad signature: %v, want REJECT", got)
	}
	if n := labelled(scrape(t)["svm_proposals_refused_total"], "signature") - refused; n != 1 {
		t.Fatalf("the refusal counted %v times under signature, want 1", n)
	}

	replacement := tc.Tx(c.Latest(), other, nil, tc.Transfer(other, unpaid, 1_000_000_000))
	if got := c.Propose(wrapped(c, good, replacement)...); got != accept {
		t.Fatalf("the proposal with the bad transaction replaced: %v, want ACCEPT", got)
	}
	payer := c.Lamports(tc.Pub(faucet))
	res, _ := c.Block(good, replacement)
	if res[0].Code != codeOK || res[1].Code != codeOK {
		t.Fatalf("the accepted block gave codes %d and %d", res[0].Code, res[1].Code)
	}
	if got := c.Lamports(paid); got != 1_000_000_000 {
		t.Fatalf("the valid transfer moved %d lamports", got)
	}
	if spent := payer - c.Lamports(tc.Pub(faucet)); spent != 1_000_000_000+fee {
		t.Fatalf("the valid transfer's payer spent %d, want the transfer and one fee", spent)
	}
}

// TestASignatureByteChangedIsRefusedWhateverTheStoreHolds proves that the same
// message with one signature byte changed is another transaction. A node that
// holds the original's result, from CheckTx or from an accepted proposal,
// refuses it as a node with no result does.
func TestASignatureByteChangedIsRefusedWhateverTheStoreHolds(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	original := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), 1_000_000_000))
	changed := slices.Clone(original)
	changed[5] ^= 1

	for name, store := range map[string]func(){
		"no result":             func() {},
		"the result of CheckTx": func() { c.Check(c.Wrap(original), false) },
		"the result of an accepted proposal": func() {
			if got := c.Propose(c.Wrap(original)); got != accept {
				t.Fatalf("the original alone: %v, want ACCEPT", got)
			}
		},
	} {
		ffi.ClearSignatureResultsForTests()
		store()
		if got := c.Propose(c.Wrap(changed)); got != refuse {
			t.Fatalf("with %s, the changed signature: %v, want REJECT", name, got)
		}
	}
}

// TestAProposalChecksEachSignatureOnce proves that the two handlers check each
// valid signature once. A block from this node's pool takes no check, and a
// block it never saw takes one per transaction in ProcessProposal and none in
// phase 1. A refused block proposed twice checks its bad signature both times,
// because only a check that passed is kept.
func TestAProposalChecksEachSignatureOnce(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()

	ffi.ClearSignatureResultsForTests()
	txs := payments(c, faucet, 4)
	for _, tx := range txs {
		if res := c.Check(c.Wrap(tx), false); res.Code != 0 {
			t.Fatalf("CheckTx New: code %d, log %s", res.Code, res.Log)
		}
	}
	if n := verified(func() {
		if got := c.Propose(wrapped(c, txs...)...); got != accept {
			t.Fatalf("a block from the pool: %v", got)
		}
	}); n != 0 {
		t.Fatalf("ProcessProposal checked %d transactions from this node's pool, want 0", n)
	}
	if n := verified(func() { c.Block(txs...) }); n != 0 {
		t.Fatalf("phase 1 checked %d transactions from this node's pool, want 0", n)
	}

	ffi.ClearSignatureResultsForTests()
	txs = payments(c, faucet, 4)
	if n := verified(func() {
		if got := c.Propose(wrapped(c, txs...)...); got != accept {
			t.Fatalf("a block this node never saw: %v", got)
		}
	}); n != 4 {
		t.Fatalf("ProcessProposal checked %d of 4 transactions it held no result for", n)
	}
	if n := verified(func() { c.Block(txs...) }); n != 0 {
		t.Fatalf("phase 1 checked %d transactions ProcessProposal had checked, want 0", n)
	}

	ffi.ClearSignatureResultsForTests()
	junk := payments(c, faucet, 2)
	junk[1][5] ^= 0xff
	for i, want := range []uint64{2, 1} {
		if n := verified(func() {
			if got := c.Propose(wrapped(c, junk...)...); got != refuse {
				t.Fatalf("the junk block, proposal %d: %v, want REJECT", i, got)
			}
		}); n != want {
			t.Fatalf("the junk block, proposal %d, checked %d transactions, want %d", i, n, want)
		}
	}
}

// TestMachinesAgreeThroughProposalAndBlock proves that machines holding a
// result for every transaction, for none and for half give the same verdict on
// each proposal, and the same results and app hash at each height. All three
// refuse a block with a bad signature and drop the same transaction when it is
// finalized anyway. A machine that wrote results during a refused proposal
// answers the next proposal as a machine that did not.
func TestMachinesAgreeThroughProposalAndBlock(t *testing.T) {
	faucet := tc.Key(1)
	machines := []struct {
		name string
		keep func(int) bool
		c    *tc.Chain
	}{
		{"every", func(int) bool { return true }, nil},
		{"none", func(int) bool { return false }, nil},
		{"half", func(i int) bool { return i%2 == 0 }, nil},
	}
	for i := range machines {
		machines[i].c = tc.Start(t, tc.Pub(faucet), tc.Options{})
		machines[i].c.Block()
	}
	block := func(b int) [][]byte {
		txs := make([][]byte, 6)
		for i := range txs {
			txs[i] = tc.Tx(machines[0].c.Latest(), faucet, nil,
				tc.Transfer(faucet, tc.Pub(tc.Key(byte(100+i))), uint64(1_000_000_000+b)))
		}
		return txs
	}
	broken := func(txs [][]byte) [][]byte {
		out := slices.Clone(txs)
		out[3] = slices.Clone(txs[3])
		out[3][5] ^= 0xff
		return out
	}
	// step runs one height on each machine: its store emptied, then filled
	// as keep says, then whatever before does, then the proposal of txs
	// and the block. The verdict, results and app hash must agree.
	step := func(b int, txs [][]byte, verdict abci.ResponseProcessProposal_ProposalStatus, before func(*tc.Chain)) []result {
		var want []result
		var wantHash []byte
		for _, m := range machines {
			ffi.ClearSignatureResultsForTests()
			for i, tx := range txs {
				if m.keep(i) {
					m.c.Check(m.c.Wrap(tx), false)
				}
			}
			before(m.c)
			if got := m.c.Propose(wrapped(m.c, txs...)...); got != verdict {
				t.Fatalf("height %d: %s gave %v, want %v", b, m.name, got, verdict)
			}
			res, hash := m.c.Block(txs...)
			got := results(res, hash)
			if want == nil {
				want, wantHash = got, hash
				continue
			}
			if !slices.Equal(got, want) {
				t.Fatalf("height %d: %s gave %+v, %s gave %+v", b, m.name, got, machines[0].name, want)
			}
			if !bytes.Equal(hash, wantHash) {
				t.Fatalf("height %d: %s reached app hash %x, %s %x", b, m.name, hash, machines[0].name, wantHash)
			}
		}
		return want
	}
	nothing := func(*tc.Chain) {}

	step(0, block(0), accept, nothing)
	if got := step(1, broken(block(1)), refuse, nothing); got[3].Code != codeNotProcessed {
		t.Fatalf("the bad signature, finalized anyway, gave code %d, want %d", got[3].Code, codeNotProcessed)
	}
	txs := block(2)
	step(2, txs, accept, func(c *tc.Chain) {
		if c != machines[1].c {
			if got := c.Propose(wrapped(c, broken(txs)...)...); got != refuse {
				t.Fatalf("the refused proposal before height 2: %v", got)
			}
		}
	})
}

// TestAProposalIsRefusedAtItsFirstFailure proves that Process refuses at the
// first transaction that fails, in block order, for the reason of that
// transaction, whether svm.Enveloped or Decoder read it. Bytes that are not an
// envelope fail to decode, and a canonical envelope around bytes that the
// engine refuses fails admission. After the first failure of each proposal
// comes one that the walk checks earlier in a transaction's turn.
func TestAProposalIsRefusedAtItsFirstFailure(t *testing.T) {
	svm.RegisterMetrics(nil)
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	txs := wrapped(c, payments(c, faucet, 32)...)
	n := len(txs)
	undecodable := []byte{0xff, 0xff}
	unadmitted := c.Wrap([]byte{1, 2, 3})
	for _, p := range []struct {
		reason string
		at     map[int][]byte
	}{
		{"decode", map[int][]byte{n - 5: undecodable, n - 1: unadmitted}},
		{"admission", map[int][]byte{n / 2: unadmitted, n - 1: undecodable}},
		{"dedup", map[int][]byte{n / 3: txs[1], n - 2: undecodable}},
	} {
		proposal := slices.Clone(txs)
		for i, b := range p.at {
			proposal[i] = b
		}
		before := labelled(scrape(t)["svm_proposals_refused_total"], p.reason)
		if got := c.Propose(proposal...); got != refuse {
			t.Fatalf("a proposal whose first failure is %s: %v, want REJECT", p.reason, got)
		}
		if got := labelled(scrape(t)["svm_proposals_refused_total"], p.reason) - before; got != 1 {
			t.Fatalf("a proposal whose first failure is %s was counted %v times under it", p.reason, got)
		}
	}
	if got := c.Propose(txs...); got != accept {
		t.Fatalf("the proposal with no failure: %v, want ACCEPT", got)
	}
}
