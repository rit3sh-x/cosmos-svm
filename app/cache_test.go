package app_test

import (
	"bytes"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// ata is the Associated Token Account program in the manifest. It is a loader
// v2 program, so its account data is its ELF.
const ata = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"

// The budget charge of each of the two shipped ELFs in this test is its
// executable text, an eighth of its other bytes, and an 8 KiB fixed part. Memo
// v1 (its published v1.0.0 build) has 13,232 bytes of text in 17,072 bytes. The
// associated token account program has 92,056 bytes of text in 105,032 bytes.
const (
	memoV1Charge = 13_232 + (17_072-13_232)/8 + 8192
	ataCharge    = 92_056 + (105_032-92_056)/8 + 8192
	textBudget   = 30_000 // one of them fits, the two together do not
)

// TestABlockBudgetsProgramText proves that the block program text budget counts
// in block order. The transaction that crosses the budget fails before anything
// loads and pays no fee, and later transactions that add no new text still
// execute. The test lowers the 400 MB budget, because the count is clamped to
// what a program stores and reaching 400 MB needs 400 MB of account data.
func TestABlockBudgetsProgramText(t *testing.T) {
	// Read the shipped budget from the engine, not from a copy here. The budget
	// is consensus, so only a test may change it, and this test restores it.
	if _, _, shipped := ffi.BlockLimits(); shipped != 400_000_000 {
		t.Fatalf("the block program budget is %d, want 400,000,000", shipped)
	}
	defer ffi.SetBlockProgramTextForTests(400_000_000)

	block := func(limit uint64) ([]*abci.ExecTxResult, uint64) {
		ffi.SetBlockProgramTextForTests(limit)
		faucet := tc.Key(1)
		c := tc.Start(t, tc.Pub(faucet), tc.Options{Programs: tc.ManifestPrograms(t)})
		c.Block()
		res, _ := c.Block(
			tc.Tx(c.Latest(), faucet, nil, memo(key(t, memoV1), "under the budget")),
			// The second transaction calls a program whose text does not fit
			// beside the text of the first. If it ran, it would fail on its own.
			// The two runs below use this to tell the two refusals apart.
			tc.Tx(c.Latest(), faucet, nil, tc.Instruction{Program: key(t, ata)}),
			tc.Tx(c.Latest(), faucet, nil, memo(key(t, memoV1), "no new text")),
		)
		return res, c.Lamports(tc.Pub(faucet))
	}

	if memoV1Charge+ataCharge <= textBudget || memoV1Charge > textBudget {
		t.Fatalf("the fixture does not straddle the budget: %d and %d against %d",
			memoV1Charge, ataCharge, textBudget)
	}

	budgeted, paid := block(textBudget)
	if budgeted[0].Code != 0 {
		t.Fatalf("the transaction under the budget failed: code %d", budgeted[0].Code)
	}
	if budgeted[1].Code != 2 {
		t.Fatalf("the transaction that crossed the budget has code %d, want 2, not processed",
			budgeted[1].Code)
	}
	if budgeted[2].Code != 0 {
		t.Fatalf("a transaction that added no new text failed after the budget was hit: code %d",
			budgeted[2].Code)
	}

	// With the real budget, the same transaction reaches the SVM, fails there
	// and pays a fee. So the budget caused the refusal above, not the
	// instruction.
	unbudgeted, paidMore := block(400_000_000)
	if unbudgeted[1].Code != 1 {
		t.Fatalf("under the real budget the second transaction has code %d, want 1, executed and failed",
			unbudgeted[1].Code)
	}
	if paid <= paidMore {
		t.Fatalf("the refused transaction paid a fee: %d lamports left against %d", paid, paidMore)
	}
}

// programBlocks runs the same program-heavy sequence on a fresh chain and
// returns the app hashes, the transaction codes and the cache's accounted
// bytes after every block. restartAt restarts the machine after that block.
func programBlocks(t *testing.T, restartAt int) ([][]byte, []uint32, []uint64) {
	t.Helper()
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Programs: tc.ManifestPrograms(t)})
	c.Block()
	var hashes [][]byte
	var codes []uint32
	var bytesHeld []uint64

	blocks := [][]func() []byte{
		{func() []byte { return tc.Tx(c.Latest(), faucet, nil, memo(key(t, memoV1), "one")) }},
		{func() []byte {
			return initializeMint(t, c, c.Latest(), faucet, tc.Key(70), key(t, splToken))
		}},
		{
			func() []byte { return tc.Tx(c.Latest(), faucet, nil, memo(key(t, memoV3), "three")) },
			func() []byte { return tc.Tx(c.Latest(), faucet, nil, memo(key(t, memoV4), "four")) },
		},
		{func() []byte {
			return initializeMint(t, c, c.Latest(), faucet, tc.Key(71), key(t, "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"))
		}},
		{func() []byte { return tc.Tx(c.Latest(), faucet, nil, memo(key(t, memoV1), "five")) }},
	}
	for i, build := range blocks {
		txs := make([][]byte, len(build))
		for j, b := range build {
			txs[j] = b()
		}
		res, hash := c.Block(txs...)
		for j, r := range res {
			if r.Code != 0 {
				t.Fatalf("block %d transaction %d: code %d, %s", i+1, j, r.Code, r.Log)
			}
			codes = append(codes, r.Code)
		}
		hashes = append(hashes, hash)
		bytesHeld = append(bytesHeld, ffi.ProgramCacheBytes())
		if i == restartAt {
			c.Restart()
		}
	}
	return hashes, codes, bytesHeld
}

// TestTheProgramCacheNeverChangesTheBlock proves that the program cache stays
// within its budget after every block. The budget, the thread count, a restart
// and a warm cache change no app hash and no result. Which programs are
// compiled, and when, is invisible to consensus.
func TestTheProgramCacheNeverChangesTheBlock(t *testing.T) {
	defer func() {
		ffi.SetThreads(0)
		ffi.SetProgramCacheBytes(4 << 30)
	}()

	// The first run in this process is cold and compiles everything it touches.
	ffi.SetThreads(1)
	ffi.SetProgramCacheBytes(4 << 30)
	hashes, codes, held := programBlocks(t, -1)

	// The comparison is useful only if the sequence holds more than the small
	// budget below. The cache counts resident bytes, and the five programs of
	// the sequence keep about 4 MiB resident together, so the small budget is
	// 1 MiB.
	const small = 1 << 20
	peak := uint64(0)
	for _, h := range held {
		if h > peak {
			peak = h
		}
	}
	if peak <= small {
		t.Fatalf("the sequence holds %d cache bytes, never more than the %d budget below", peak, small)
	}

	for _, run := range []struct {
		what      string
		threads   int
		cache     uint64
		restartAt int
	}{
		{"a warm cache", 1, 4 << 30, -1},
		{"four threads", 4, 4 << 30, -1},
		{"a 1 MiB cache", 1, small, -1},
		{"four threads and a 1 MiB cache", 4, small, -1},
		{"a machine restarted mid-sequence", 4, small, 2},
	} {
		ffi.SetThreads(run.threads)
		ffi.SetProgramCacheBytes(run.cache)
		gotHashes, gotCodes, gotHeld := programBlocks(t, run.restartAt)
		for i := range hashes {
			if !bytes.Equal(gotHashes[i], hashes[i]) {
				t.Fatalf("%s: block %d app hash %x, want %x", run.what, i+1, gotHashes[i], hashes[i])
			}
		}
		for i := range codes {
			if gotCodes[i] != codes[i] {
				t.Fatalf("%s: transaction %d code %d, want %d", run.what, i, gotCodes[i], codes[i])
			}
		}
		if run.cache != 0 {
			for i, h := range gotHeld {
				if h > run.cache {
					t.Fatalf("%s: the cache held %d bytes after block %d, over its %d budget",
						run.what, h, i+1, run.cache)
				}
			}
		}
	}
}

// TestADeployedProgramLoadsOnItsFirstUse proves that the first block that
// invokes a deployed program loads it into the global cache. In Agave v4.3.0 a
// deploy stores an unloaded entry, so the deploy block merges nothing compiled.
func TestADeployedProgramLoadsOnItsFirstUse(t *testing.T) {
	defer ffi.SetProgramCacheBytes(4 << 30)
	ffi.SetProgramCacheBytes(4 << 30)

	var elf []byte
	for _, p := range tc.ManifestPrograms(t) {
		if p.Address == memoV3 {
			elf = p.ELF
		}
	}
	payer := tc.Key(1)
	buffer, program := tc.Key(33), tc.Key(34)
	c := tc.Start(t, tc.Pub(payer), tc.Options{})
	c.Block()

	if res, _ := c.Block(newBuffer(c, payer, buffer, len(elf))); res[0].Code != 0 {
		t.Fatalf("buffer: code %d", res[0].Code)
	}
	res, _ := c.Block(writes(c, payer, buffer, elf)...)
	for i, r := range res {
		if r.Code != 0 {
			t.Fatalf("buffer write %d: code %d", i, r.Code)
		}
	}
	if res, _ = c.Block(deployProgram(c, payer, buffer, program, len(elf))); res[0].Code != 0 {
		t.Fatalf("deploy: code %d, %s", res[0].Code, res[0].Log)
	}
	deployed := ffi.ProgramCacheBytes()
	if res, _ = c.Block(tc.Tx(c.Latest(), payer, nil, memo(tc.Pub(program), "first"))); res[0].Code != 0 {
		t.Fatalf("invoking the deployed program: code %d", res[0].Code)
	}
	if loaded := ffi.ProgramCacheBytes(); loaded <= deployed {
		t.Fatalf("the first invocation left the cache at %d bytes, unchanged from %d: nothing was loaded",
			loaded, deployed)
	}
}
