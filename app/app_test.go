package app_test

import (
	"crypto/ed25519"
	"encoding/binary"
	"testing"

	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

const fee = 5_000

func TestTransferCommits(t *testing.T) {
	faucet := tc.Key(1)
	dest := tc.Pub(tc.Key(2))
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})

	c.Block()
	before := c.Supply()
	res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest, 1_000_000_000)))
	if res[0].Code != 0 {
		t.Fatalf("transfer failed: code %d", res[0].Code)
	}
	if got := c.Lamports(dest); got != 1_000_000_000 {
		t.Fatalf("destination holds %d lamports, want 1,000,000,000", got)
	}
	if got, want := c.Lamports(tc.Pub(faucet)), uint64(tc.FaucetLamports-1_000_000_000-fee); got != want {
		t.Fatalf("faucet holds %d lamports, want %d", got, want)
	}
	// Supply falls by exactly the fees charged, and no account receives them.
	if after := c.Supply(); before-after != fee {
		t.Fatalf("supply fell by %d, want the fee %d", before-after, fee)
	}
}

func TestClockSlotIsHeightAndBlockhashesFollowHeaders(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	for i := 0; i < 3; i++ {
		c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(byte(10+i))), 1_000_000_000)))
		clock, _ := c.Account(svm.ClockID)
		if slot := binary.LittleEndian.Uint64(clock.Data[0:8]); slot != uint64(c.Height) {
			t.Fatalf("Clock.slot %d at height %d", slot, c.Height)
		}
		if ts := int64(binary.LittleEndian.Uint64(clock.Data[32:40])); ts != 1_700_000_000+2*c.Height {
			t.Fatalf("Clock.unix_timestamp %d at height %d", ts, c.Height)
		}
	}
	// At height 4 SlotHashes holds heights 3, 2, 1, newest first.
	sh, _ := c.Account(svm.SlotHashesID)
	if n := binary.LittleEndian.Uint64(sh.Data[0:8]); n != 3 {
		t.Fatalf("SlotHashes has %d entries at height 4, want 3", n)
	}
	for i, h := range []int{3, 2, 1} {
		e := sh.Data[8+40*i:]
		if slot := binary.LittleEndian.Uint64(e[0:8]); slot != uint64(h) || string(e[8:40]) != string(c.Hashes[h-1]) {
			t.Fatalf("SlotHashes entry %d is slot %d, want %d with its header hash", i, slot, h)
		}
	}
}

// transfers runs three blocks of two fresh transfers each and returns the last
// app hash. No transaction repeats, so the dedup set never rejects one.
func transfers(t *testing.T, opts tc.Options) []byte {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), opts)
	c.Block()
	var hash []byte
	for i := 0; i < 3; i++ {
		res, h := c.Block(
			tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(byte(20+i))), 2_000_000_000)),
			tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(byte(40+i))), 3_000_000_000)),
		)
		for _, r := range res {
			if r.Code != 0 {
				t.Fatalf("transfer failed at height %d: code %d", c.Height, r.Code)
			}
		}
		hash = h
	}
	c.Supply()
	return hash
}

// TestSameBlocksSameAppHash proves that two runs of the same blocks reach the
// same app hash. Every account stays in the shard that its pubkey names.
func TestSameBlocksSameAppHash(t *testing.T) {
	if a, b := transfers(t, tc.Options{}), transfers(t, tc.Options{}); string(a) != string(b) {
		t.Fatalf("app hash differs across runs: %x vs %x", a, b)
	}
}

// TestTheDedupSetIsOutsideTheTreeAndItsCommitmentIsInside proves that the
// state tree holds only a 32-byte commitment to the dedup set. A machine with
// no dedup set reaches a different app hash at the next block, not at the next
// replay that someone tries.
func TestTheDedupSetIsOutsideTheTreeAndItsCommitmentIsInside(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()
	commitment := c.App.Stores.DedupCommitment(c.App.CommitMultiStore())
	if len(commitment) != 32 {
		t.Fatalf("commitment is %d bytes, want 32", len(commitment))
	}
	// A block that processes a transaction moves the commitment.
	c.Block(tc.Tx(c.Latest(), tc.Key(1), nil, tc.Transfer(tc.Key(1), tc.Pub(tc.Key(2)), 1_000_000_000)))
	if after := c.App.Stores.DedupCommitment(c.App.CommitMultiStore()); string(after) == string(commitment) {
		t.Fatal("the commitment did not move when a transaction was processed")
	}

	if a, b := transfers(t, tc.Options{}), transfers(t, tc.Options{NoDedup: true}); string(a) == string(b) {
		t.Fatal("a machine with no dedup set reached the same app hash as one with it")
	}
}

func TestSameTransactionExecutesOnce(t *testing.T) {
	faucet := tc.Key(1)
	dest := tc.Pub(tc.Key(2))
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest, 1_000_000_000))

	before := c.Supply()
	res, _ := c.Block(tx, tx)
	if res[0].Code != 0 || res[1].Code != 2 {
		t.Fatalf("same block: codes %d and %d, want 0 and 2 (not processed)", res[0].Code, res[1].Code)
	}
	res, _ = c.Block(tx)
	if res[0].Code != 2 {
		t.Fatalf("next block: code %d, want 2 (not processed)", res[0].Code)
	}
	if got := c.Lamports(dest); got != 1_000_000_000 {
		t.Fatalf("destination holds %d lamports, want one transfer", got)
	}
	if after := c.Supply(); before-after != fee {
		t.Fatalf("supply fell by %d, want one fee", before-after)
	}
}

// TestDedupSetSurvivesRestart proves that a restarted machine reads the dedup
// set back from its database and still refuses a replay.
func TestDedupSetSurvivesRestart(t *testing.T) {
	faucet := tc.Key(1)
	dest := tc.Pub(tc.Key(2))
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest, 1_000_000_000))
	c.Block(tx)
	c.Restart()
	if res, _ := c.Block(tx); res[0].Code != 2 {
		t.Fatalf("replay after restart: code %d, want 2 (not processed)", res[0].Code)
	}
	if got := c.Lamports(dest); got != 1_000_000_000 {
		t.Fatalf("destination holds %d lamports, want one transfer", got)
	}
}

// TestBlockhashWindowEdge proves that a blockhash is accepted at age 150 and
// refused at age 151.
func TestBlockhashWindowEdge(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	first := c.Latest()
	for c.Height < 150 {
		c.Block()
	}
	res, _ := c.Block(tc.Tx(first, faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), 1_000_000_000)))
	if res[0].Code != 0 {
		t.Fatalf("blockhash of height 1 at height %d (age 150): code %d, want accepted", c.Height, res[0].Code)
	}
	res, _ = c.Block(tc.Tx(first, faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(3)), 1_000_000_000)))
	if res[0].Code != 2 {
		t.Fatalf("blockhash of height 1 at height %d (age 151): code %d, want not processed", c.Height, res[0].Code)
	}
}

const nonceLen = 80

func systemIx(accounts []tc.Meta, data []byte) tc.Instruction {
	return tc.Instruction{Program: svm.SystemProgram, Accounts: accounts, Data: data}
}

func createNonce(payer, nonce ed25519.PrivateKey) []tc.Instruction {
	create := binary.LittleEndian.AppendUint32(nil, 0)
	create = binary.LittleEndian.AppendUint64(create, svm.MinimumBalance(nonceLen))
	create = binary.LittleEndian.AppendUint64(create, nonceLen)
	create = append(create, svm.SystemProgram[:]...)
	auth := tc.Pub(payer)
	initialize := append(binary.LittleEndian.AppendUint32(nil, 6), auth[:]...)
	return []tc.Instruction{
		systemIx([]tc.Meta{tc.Signer(tc.Pub(payer), true), tc.Signer(tc.Pub(nonce), true)}, create),
		systemIx([]tc.Meta{tc.Writable(tc.Pub(nonce)), tc.Readonly(svm.RecentBlockhashesID), tc.Readonly(svm.RentID)}, initialize),
	}
}

func advanceNonce(nonce [32]byte, authority ed25519.PrivateKey) tc.Instruction {
	return systemIx([]tc.Meta{tc.Writable(nonce), tc.Readonly(svm.RecentBlockhashesID), tc.Signer(tc.Pub(authority), false)},
		binary.LittleEndian.AppendUint32(nil, 4))
}

// nonceValue returns the durable nonce in an initialized nonce account. The
// account data holds the version and state tags, the authority, then the nonce.
func nonceValue(t *testing.T, c *tc.Chain, nonce [32]byte) [32]byte {
	a, ok := c.Account(nonce)
	if !ok || len(a.Data) != nonceLen {
		t.Fatalf("nonce account missing or %d bytes", len(a.Data))
	}
	return [32]byte(a.Data[40:72])
}

// TestNonceOperationsFailInBlockOne proves that both nonce operations fail in
// block 1, where RecentBlockhashes is empty, and work from block 2.
func TestNonceOperationsFailInBlockOne(t *testing.T) {
	faucet := tc.Key(1)
	genesisNonce := tc.Pub(tc.Key(3))
	data := binary.LittleEndian.AppendUint32(nil, 1) // Versions::Current
	data = binary.LittleEndian.AppendUint32(data, 1) // State::Initialized
	auth := tc.Pub(faucet)
	data = append(data, auth[:]...)
	data = append(data, make([]byte, 32)...)
	data = binary.LittleEndian.AppendUint64(data, 5_000)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Accounts: []svm.GenesisAccount{{
		Pubkey:    base58.Encode(genesisNonce[:]),
		Lamports:  svm.MinimumBalance(nonceLen),
		Owner:     base58.Encode(svm.SystemProgram[:]),
		Data:      data,
		RentEpoch: ^uint64(0),
	}}})

	nonce := tc.Key(4)
	hash1 := tc.HashAt(1)
	res, _ := c.Block(
		tc.Tx(hash1, faucet, []ed25519.PrivateKey{nonce}, createNonce(faucet, nonce)...),
		tc.Tx(hash1, faucet, nil, advanceNonce(genesisNonce, faucet)),
	)
	if res[0].Code != 1 || res[1].Code != 1 {
		t.Fatalf("block 1: initialize code %d, advance code %d, want both 1 (failed)", res[0].Code, res[1].Code)
	}
	// With the hash of block 1, these are the same messages as above, and the
	// dedup set refuses them. The hash of block 2 is in its own window at age 0.
	hash2 := tc.HashAt(2)
	res, _ = c.Block(
		tc.Tx(hash2, faucet, []ed25519.PrivateKey{nonce}, createNonce(faucet, nonce)...),
		tc.Tx(hash2, faucet, nil, advanceNonce(genesisNonce, faucet)),
	)
	if res[0].Code != 0 || res[1].Code != 0 {
		t.Fatalf("block 2: initialize code %d, advance code %d, want both 0", res[0].Code, res[1].Code)
	}
}

func TestDurableNonceTransactionCannotReplay(t *testing.T) {
	faucet := tc.Key(1)
	nonce := tc.Key(4)
	dest := tc.Pub(tc.Key(5))
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	if res, _ := c.Block(tc.Tx(c.Latest(), faucet, []ed25519.PrivateKey{nonce}, createNonce(faucet, nonce)...)); res[0].Code != 0 {
		t.Fatalf("create nonce account: code %d", res[0].Code)
	}
	if res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, advanceNonce(tc.Pub(nonce), faucet))); res[0].Code != 0 {
		t.Fatalf("advance: code %d", res[0].Code)
	}
	nonced := tc.Tx(nonceValue(t, c, tc.Pub(nonce)), faucet, nil,
		advanceNonce(tc.Pub(nonce), faucet), tc.Transfer(faucet, dest, 1_000_000_000))
	if res, _ := c.Block(nonced); res[0].Code != 0 {
		t.Fatalf("nonce transaction: code %d", res[0].Code)
	}
	if res, _ := c.Block(nonced); res[0].Code != 2 {
		t.Fatalf("replayed nonce transaction: code %d, want 2 (not processed)", res[0].Code)
	}
	if got := c.Lamports(dest); got != 1_000_000_000 {
		t.Fatalf("destination holds %d lamports, want one transfer", got)
	}
}
