package app_test

import (
	"os"
	"strconv"
	"testing"

	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// The load has six transactions a block, sixteen payers, two shared (hot)
// destinations and eight cold destinations. Each payer has one SPL Token mint.
const (
	agreementPerBlock = 6
	agreementPayers   = 16
	agreementHot      = 2
	agreementCold     = 8
)

// agreementBlocks returns SVM_AGREEMENT_BLOCKS if it is more than 1, or else
// 400.
func agreementBlocks() int64 {
	if n, err := strconv.ParseInt(os.Getenv("SVM_AGREEMENT_BLOCKS"), 10, 64); err == nil && n > 1 {
		return n
	}
	return 400
}

// coldKey returns a destination that at most one transaction in a block
// writes.
func coldKey(i int) [32]byte { return tc.Pub(tc.Key(byte(150 + i))) }

// hotKey returns a destination shared by all payers.
func hotKey(i int) [32]byte { return tc.Pub(tc.Key(byte(170 + i))) }

// TestFourValidatorsAgreeOnMixedBlocks proves that four machines reach the same
// app hash at each height of mixed blocks. One machine restarts midway, and one
// runs the engine on a single thread.
func TestFourValidatorsAgreeOnMixedBlocks(t *testing.T) {
	if testing.Short() {
		t.Skip("four machines, one block at a time")
	}
	blocks := agreementBlocks()
	restartAt := blocks / 2
	defer ffi.SetThreads(0)

	accounts := make([]svm.GenesisAccount, 0, agreementPayers+agreementCold+agreementHot)
	for i := 0; i < agreementPayers; i++ {
		accounts = append(accounts, genesisLamports(tc.Pub(payer(i)), 1_000_000_000_000))
	}
	// Destinations start rent exempt, so a transfer can be one lamport. Then
	// the load does not drain the payers, even over 5,000 blocks.
	for i := 0; i < agreementCold; i++ {
		accounts = append(accounts, genesisLamports(coldKey(i), svm.MinimumBalance(0)))
	}
	for i := 0; i < agreementHot; i++ {
		accounts = append(accounts, genesisLamports(hotKey(i), svm.MinimumBalance(0)))
	}

	token := key(t, splToken)
	chains := make([]*tc.Chain, 4)
	for i := range chains {
		chains[i] = tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{
			Accounts: accounts,
			Programs: []svm.GenesisProgram{splTokenProgram(t)},
		})
		if string(chains[i].GenesisHash) != string(chains[0].GenesisHash) {
			t.Fatalf("machine %d: genesis root %x, machine 0 %x", i, chains[i].GenesisHash, chains[0].GenesisHash)
		}
	}

	// One mint and one token account per payer, in block 1.
	setup := make([][]byte, 0, agreementPayers)
	for i := 0; i < agreementPayers; i++ {
		setup = append(setup, openMint(tc.HashAt(1), payer(i), mintKey(i), tokenKey(i), token))
	}
	run(t, chains, 1, setup)

	for height := int64(2); height <= blocks; height++ {
		if height == restartAt {
			// A machine that stops midway replays from its own disk. It must
			// reach the same root as the three that did not stop.
			chains[2].Restart()
		}
		run(t, chains, height, generate(t, height, token))
	}

	// The load ran only if every payer paid a fee.
	for i := 0; i < agreementPayers; i++ {
		if got := chains[0].Lamports(tc.Pub(payer(i))); got >= 1_000_000_000_000 {
			t.Fatalf("payer %d never paid a fee: %d lamports", i, got)
		}
	}
}

// run finalizes one block on every machine and compares app hashes. The last
// machine runs the engine on one thread.
func run(t *testing.T, chains []*tc.Chain, height int64, txs [][]byte) {
	t.Helper()
	var want []byte
	for i, c := range chains {
		if i == len(chains)-1 {
			ffi.SetThreads(1)
		}
		res, appHash := c.Block(txs...)
		if i == len(chains)-1 {
			ffi.SetThreads(0)
		}
		if i == 0 {
			want = appHash
			for k, r := range res {
				if r.Code != 0 {
					t.Fatalf("height %d transaction %d: code %d", height, k, r.Code)
				}
			}
			continue
		}
		if string(appHash) != string(want) {
			t.Fatalf("height %d: machine %d root %x, machine 0 %x", height, i, appHash, want)
		}
	}
}

// generate builds a block of System transfers and SPL Token instructions
// using the block's hash as the recent blockhash.
func generate(t *testing.T, height int64, token [32]byte) [][]byte {
	t.Helper()
	hash := tc.HashAt(height)
	txs := make([][]byte, 0, agreementPerBlock)
	for i := 0; i < agreementPerBlock; i++ {
		p := int(height*agreementPerBlock+int64(i)) % agreementPayers
		from := payer(p)
		switch (int(height) + i) % 3 {
		case 0:
			txs = append(txs, tc.Tx(hash, from, nil, tc.Transfer(from, coldKey(p%agreementCold), 1)))
		case 1:
			txs = append(txs, tc.Tx(hash, from, nil, tc.Transfer(from, hotKey(i%agreementHot), 1)))
		default:
			txs = append(txs, mintTo(hash, from, tc.Pub(mintKey(p)), tc.Pub(tokenKey(p)), token, 1))
		}
	}
	return txs
}

// genesisLamports creates a funded System-owned genesis account.
func genesisLamports(pk [32]byte, lamports uint64) svm.GenesisAccount {
	owner := svm.SystemProgram
	return svm.GenesisAccount{
		Pubkey:    base58.Encode(pk[:]),
		Lamports:  lamports,
		Owner:     base58.Encode(owner[:]),
		RentEpoch: ^uint64(0),
	}
}
