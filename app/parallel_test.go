package app_test

import (
	"crypto/ed25519"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/mr-tron/base58"

	abci "github.com/cometbft/cometbft/abci/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// The shape of the randomised block generator. Twelve payers make a block
// partition. Two shared destinations make some groups merge. One SPL Token
// mint per payer makes the work more than System transfers.
const (
	payers    = 12
	hotKeys   = 2
	perBlock  = 24
	blocks    = 4
	mintSize  = 82
	tokenSize = 165
)

// parallel holds one chain's results for the generated blocks.
type parallel struct {
	hashes [][32]byte
	// codes and groups are per block, per transaction.
	codes  [][]uint32
	groups [][]string
}

// TestThreadCountNeverChangesTheBlock proves that the engine's worker thread
// count never changes a block. Four chains that differ only in thread count
// run the same randomised blocks of System transfers and SPL Token MintTo, and
// every app hash and transaction result matches. The blocks must partition, or
// the comparison proves nothing.
func TestThreadCountNeverChangesTheBlock(t *testing.T) {
	defer ffi.SetThreads(0)
	runs := make(map[int]parallel)
	for _, threads := range []int{1, 2, 4, 0} {
		ffi.SetThreads(threads)
		runs[threads] = generatedRun(t)
	}

	base := runs[1]
	if width := distinct(base.groups[0]); width < 4 {
		t.Fatalf("the generated blocks partition into %d groups; the comparison proves nothing", width)
	}
	for _, threads := range []int{2, 4, 0} {
		got := runs[threads]
		for b := range base.hashes {
			if got.hashes[b] != base.hashes[b] {
				t.Fatalf("block %d: app hash %x at %d threads, %x at one", b, got.hashes[b], threads, base.hashes[b])
			}
			for i := range base.codes[b] {
				if got.codes[b][i] != base.codes[b][i] {
					t.Fatalf("block %d transaction %d: code %d at %d threads, %d at one",
						b, i, got.codes[b][i], threads, base.codes[b][i])
				}
			}
			if strings.Join(got.groups[b], ",") != strings.Join(base.groups[b], ",") {
				t.Fatalf("block %d: groups %v at %d threads, %v at one", b, got.groups[b], threads, base.groups[b])
			}
		}
	}
}

// generatedRun starts a chain, sets up one mint and one token account per
// payer, then runs the generated blocks.
func generatedRun(t *testing.T) parallel {
	t.Helper()
	token := key(t, splToken)
	accounts := make([]svm.GenesisAccount, 0, payers)
	for i := 0; i < payers; i++ {
		pk := tc.Pub(payer(i))
		accounts = append(accounts, svm.GenesisAccount{
			Pubkey:    base58.Encode(pk[:]),
			Lamports:  1_000_000_000,
			Owner:     base58.Encode(svm.SystemProgram[:]),
			RentEpoch: ^uint64(0),
		})
	}
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{
		Accounts: accounts,
		Programs: []svm.GenesisProgram{splTokenProgram(t)},
	})

	setup := make([][]byte, 0, payers)
	for i := 0; i < payers; i++ {
		setup = append(setup, openMint(tc.HashAt(1), payer(i), mintKey(i), tokenKey(i), token))
	}
	res, _ := c.Block(setup...)
	for i, r := range res {
		if r.Code != 0 {
			t.Fatalf("payer %d: mint setup code %d", i, r.Code)
		}
	}

	var run parallel
	// One deterministic stream of choices, so every chain sees the same block.
	seed := uint64(0x9e3779b97f4a7c15)
	next := func(n int) int {
		seed = seed*6364136223846793005 + 1442695040888963407
		return int((seed >> 33) % uint64(n))
	}
	for b := 0; b < blocks; b++ {
		hash := c.Latest()
		txs := make([][]byte, 0, perBlock)
		for i := 0; i < perBlock; i++ {
			p := next(payers)
			// Transfers must leave the destination rent exempt.
			amount := svm.MinimumBalance(0) + uint64(b*perBlock+i)
			switch next(3) {
			case 0:
				txs = append(txs, tc.Tx(hash, payer(p), nil,
					tc.Transfer(payer(p), tc.Pub(tc.Key(byte(100+p))), amount)))
			case 1:
				// A destination that two payers share, so their groups merge.
				txs = append(txs, tc.Tx(hash, payer(p), nil,
					tc.Transfer(payer(p), tc.Pub(tc.Key(byte(200+p%hotKeys))), amount)))
			default:
				txs = append(txs, mintTo(hash, payer(p), tc.Pub(mintKey(p)), tc.Pub(tokenKey(p)),
					key(t, splToken), amount))
			}
		}
		results, appHash := c.Block(txs...)
		codes := make([]uint32, len(results))
		groups := make([]string, len(results))
		for i, r := range results {
			codes[i] = r.Code
			groups[i] = groupOf(r.Events)
		}
		run.hashes = append(run.hashes, [32]byte(appHash))
		run.codes = append(run.codes, codes)
		run.groups = append(run.groups, groups)
	}
	return run
}

func payer(i int) ed25519.PrivateKey   { return tc.Key(byte(20 + i)) }
func mintKey(i int) ed25519.PrivateKey { return tc.Key(byte(40 + i)) }

func tokenKey(i int) ed25519.PrivateKey { return tc.Key(byte(60 + i)) }

func splTokenProgram(t *testing.T) svm.GenesisProgram {
	t.Helper()
	for _, p := range tc.ManifestPrograms(t) {
		if p.Address == splToken {
			return p
		}
	}
	t.Fatal("no SPL Token row")
	return svm.GenesisProgram{}
}

// openMint creates and initializes a mint and a token account for one payer,
// who is both mint authority and account owner.
func openMint(blockhash [32]byte, payer, mint, account ed25519.PrivateKey, token [32]byte) []byte {
	authority := tc.Pub(payer)
	create := func(size int) []byte {
		d := binary.LittleEndian.AppendUint32(nil, 0) // SystemInstruction::CreateAccount
		d = binary.LittleEndian.AppendUint64(d, svm.MinimumBalance(size))
		d = binary.LittleEndian.AppendUint64(d, uint64(size))
		return append(d, token[:]...)
	}
	initializeMint := append([]byte{20, 6}, authority[:]...) // InitializeMint2, 6 decimals
	initializeMint = append(initializeMint, 0)               // no freeze authority
	initializeAccount := append([]byte{18}, authority[:]...) // InitializeAccount3
	return tc.Tx(blockhash, payer, []ed25519.PrivateKey{mint, account},
		tc.Instruction{Program: svm.SystemProgram, Accounts: []tc.Meta{tc.Signer(authority, true), tc.Signer(tc.Pub(mint), true)}, Data: create(mintSize)},
		tc.Instruction{Program: token, Accounts: []tc.Meta{tc.Writable(tc.Pub(mint))}, Data: initializeMint},
		tc.Instruction{Program: svm.SystemProgram, Accounts: []tc.Meta{tc.Signer(authority, true), tc.Signer(tc.Pub(account), true)}, Data: create(tokenSize)},
		tc.Instruction{Program: token, Accounts: []tc.Meta{tc.Writable(tc.Pub(account)), tc.Readonly(tc.Pub(mint))}, Data: initializeAccount},
	)
}

func mintTo(blockhash [32]byte, payer ed25519.PrivateKey, mint, account, token [32]byte, amount uint64) []byte {
	data := binary.LittleEndian.AppendUint64([]byte{7}, amount) // MintTo
	return tc.Tx(blockhash, payer, nil, tc.Instruction{
		Program:  token,
		Accounts: []tc.Meta{tc.Writable(mint), tc.Writable(account), tc.Signer(tc.Pub(payer), false)},
		Data:     data,
	})
}

// groupOf reads the conflict group the runner published.
func groupOf(events []abci.Event) string {
	for _, e := range events {
		for _, a := range e.Attributes {
			if a.Key == svm.AttributeKeyGroup {
				return a.Value
			}
		}
	}
	return ""
}

func distinct(groups []string) int {
	seen := map[string]bool{}
	for _, g := range groups {
		if g != "" {
			seen[g] = true
		}
	}
	return len(seen)
}
