package app_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	dbm "github.com/cosmos/cosmos-db"

	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestTokenIndexBlockTime measures what the owner index costs a block. It
// asserts nothing about the numbers, because a wall clock on a shared
// development machine is an observation, not a benchmark. The load writes
// token accounts, and each such write costs one local read and two local writes.
func TestTokenIndexBlockTime(t *testing.T) {
	if testing.Short() {
		t.Skip("runs 200 blocks twice")
	}
	const blocks = 200
	run := func(withIndex bool) time.Duration {
		payer := tc.Key(1)
		mint := tc.Pub(tc.Key(80))
		accounts := []svm.GenesisAccount{genesisTokenMint(mint)}
		for i := 0; i < blocks; i++ {
			accounts = append(accounts, genesisTokenAccount(tokenPub(i), mint, tc.Pub(tc.Key(81))))
		}
		c := tc.Start(t, tc.Pub(payer), tc.Options{Accounts: accounts, NoTokens: !withIndex, Programs: tc.ManifestPrograms(t)})
		c.Block()
		owner := tc.Key(81)
		start := time.Now()
		for i := 0; i < blocks; i++ {
			// Transfer one unit out of account i into account (i+1) % blocks,
			// which writes two token accounts per block.
			data := append([]byte{3}, binary.LittleEndian.AppendUint64(nil, 1)...)
			tx := tc.Tx(c.Latest(), payer, []ed25519.PrivateKey{owner}, tc.Instruction{
				Program: svm.SplToken,
				Accounts: []tc.Meta{
					tc.Writable(tokenPub(i)), tc.Writable(tokenPub((i + 1) % blocks)), tc.Signer(tc.Pub(owner), false),
				},
				Data: data,
			})
			if res, _ := c.Block(tx); res[0].Code != 0 {
				t.Fatalf("block %d: code %d", i, res[0].Code)
			}
		}
		return time.Since(start)
	}
	without := run(false)
	with := run(true)
	t.Logf("%d blocks of two token-account writes: %v without the index, %v with it (%.1f%%)",
		blocks, without, with, 100*float64(with-without)/float64(without))
}

// heldIndex is a token index database whose range scans wait while hold is
// open. A rebuild's first read is a range scan, and nothing before it in the
// block that starts it is one, so a test can stop a rebuild there instead of
// racing it.
type heldIndex struct {
	dbm.DB
	hold chan struct{}
}

func (d *heldIndex) Iterator(start, end []byte) (dbm.Iterator, error) {
	if d.hold != nil {
		<-d.hold
	}
	return d.DB.Iterator(start, end)
}

// TestTheTokenIndexRebuildsOffTheCommitPath proves that a token index rebuild
// runs off the commit path. Inside FinalizeBlock, the scan after a release that
// bumps tokenIndexVersion, about 14 us and 7 KiB per account, would hold every
// validator in one block. The rebuild reads committed state on its own
// goroutine, and the index reports that it is behind until it catches up.
func TestTheTokenIndexRebuildsOffTheCommitPath(t *testing.T) {
	const holds = 20_000
	payer, owner, heir := tc.Key(1), tc.Key(81), tc.Pub(tc.Key(82))
	mint := tc.Pub(tc.Key(80))
	accounts := []svm.GenesisAccount{genesisTokenMint(mint)}
	held := func(i int) [32]byte {
		return sha256.Sum256(binary.LittleEndian.AppendUint32([]byte("held"), uint32(i)))
	}
	for i := 0; i < holds; i++ {
		accounts = append(accounts, genesisTokenAccount(held(i), mint, tc.Pub(owner)))
	}
	// The chain is on disk, as svmd's is. The rebuild holds its scan open
	// across commits, and an in-memory database's iterator holds a lock that
	// stalls every commit until it closes.
	index := &heldIndex{DB: dbm.NewMemDB()}
	c := tc.Start(t, tc.Pub(payer), tc.Options{Disk: true, Tokens: index, Accounts: accounts, Programs: tc.ManifestPrograms(t)})
	c.Block()
	if got, err := c.App.Tokens.Accounts(tc.Pub(owner)); err != nil || len(got) != holds {
		t.Fatalf("after the first block the owner holds %d accounts (%v), want %d", len(got), err, holds)
	}

	// A marker from older index rules, as a release that changes the rules
	// leaves on every machine.
	if err := c.TokenIndexDB().Set([]byte{'m'}, append([]byte{0}, binary.BigEndian.AppendUint64(nil, uint64(c.Height))...)); err != nil {
		t.Fatal(err)
	}
	c.Restart()
	// The rebuild waits at its first read until the next block finalizes.
	// Cleanup releases it first if the test fails, because the chain's own
	// cleanup waits for the rebuild.
	index.hold = make(chan struct{})
	release := sync.OnceFunc(func() { close(index.hold) })
	t.Cleanup(release)
	c.Block()
	if _, err := c.App.Tokens.Accounts(tc.Pub(owner)); !errors.Is(err, svm.ErrTokenIndexBehind) {
		t.Fatalf("right after the block that began the rebuild: %v, want ErrTokenIndexBehind", err)
	}
	// SetAuthority (AccountOwner) to the heir, finalized while the rebuild
	// reads the state from before it.
	moved := tc.Tx(c.Latest(), payer, []ed25519.PrivateKey{owner}, tc.Instruction{
		Program:  svm.SplToken,
		Accounts: []tc.Meta{tc.Writable(held(0)), tc.Signer(tc.Pub(owner), false)},
		Data:     append([]byte{6, 2, 1}, heir[:]...),
	})
	if res, _ := c.Block(moved); res[0].Code != 0 {
		t.Fatalf("moving an account to the heir: code %d", res[0].Code)
	}
	release()
	c.App.Tokens.Wait()
	if got, err := c.App.Tokens.Accounts(tc.Pub(owner)); err != nil || len(got) != holds-1 {
		t.Fatalf("after the rebuild the owner holds %d accounts (%v), want %d", len(got), err, holds-1)
	}
	if got, err := c.App.Tokens.Accounts(heir); err != nil || len(got) != 1 || got[0] != held(0) {
		t.Fatalf("after the rebuild the heir holds %d accounts (%v), want the one moved", len(got), err)
	}
	if h := c.App.Tokens.Height(); h != uint64(c.Height) {
		t.Fatalf("the index is current to %d, the chain is at %d", h, c.Height)
	}
}

func tokenPub(i int) [32]byte {
	return tc.Pub(ed25519.NewKeyFromSeed(append(make([]byte, 30), 't', byte(i))))
}

func genesisTokenMint(mint [32]byte) svm.GenesisAccount {
	data := make([]byte, 82)
	binary.LittleEndian.PutUint64(data[36:], 1_000_000_000)
	data[44] = 6
	data[45] = 1
	return svm.GenesisAccount{
		Pubkey: base58.Encode(mint[:]), Lamports: svm.MinimumBalance(len(data)),
		Owner: base58.Encode(svm.SplToken[:]), Data: data, RentEpoch: ^uint64(0),
	}
}

func genesisTokenAccount(pubkey, mint, owner [32]byte) svm.GenesisAccount {
	data := make([]byte, 165)
	copy(data, mint[:])
	copy(data[32:], owner[:])
	binary.LittleEndian.PutUint64(data[64:], 1_000_000)
	data[108] = 1
	return svm.GenesisAccount{
		Pubkey: base58.Encode(pubkey[:]), Lamports: svm.MinimumBalance(len(data)),
		Owner: base58.Encode(svm.SplToken[:]), Data: data, RentEpoch: ^uint64(0),
	}
}

// TestTheTokenIndexIsWhatARefillWrites: a block's write drops the entry that
// the account had before the block, as phase 2 loaded it. It does not read the
// index back. After an owner change, a close and a transfer, the
// index must hold exactly what a rebuild from the same state writes.
func TestTheTokenIndexIsWhatARefillWrites(t *testing.T) {
	payer, owner, heir := tc.Key(1), tc.Key(81), tc.Pub(tc.Key(82))
	mint := tc.Pub(tc.Key(80))
	accounts := []svm.GenesisAccount{genesisTokenMint(mint)}
	for i := 0; i < 4; i++ {
		accounts = append(accounts, genesisTokenAccount(tokenPub(i), mint, tc.Pub(owner)))
	}
	c := tc.Start(t, tc.Pub(payer), tc.Options{Disk: true, Accounts: accounts, Programs: tc.ManifestPrograms(t)})
	c.Block()
	token := func(data []byte, metas ...tc.Meta) []byte {
		return tc.Tx(c.Latest(), payer, []ed25519.PrivateKey{owner}, tc.Instruction{Program: svm.SplToken, Accounts: metas, Data: data})
	}
	amount := func(n uint64) []byte { return append([]byte{3}, binary.LittleEndian.AppendUint64(nil, n)...) }
	signer := tc.Signer(tc.Pub(owner), false)
	for _, tx := range [][]byte{
		// SetAuthority, AccountOwner: the account moves to the heir.
		token(append([]byte{6, 2, 1}, heir[:]...), tc.Writable(tokenPub(0)), signer),
		// Empty the second account into the third, then close it.
		token(amount(1_000_000), tc.Writable(tokenPub(1)), tc.Writable(tokenPub(2)), signer),
		token([]byte{9}, tc.Writable(tokenPub(1)), tc.Writable(tc.Pub(tc.Key(83))), signer),
		token(amount(1), tc.Writable(tokenPub(2)), tc.Writable(tokenPub(3)), signer),
	} {
		if res, _ := c.Block(tx); res[0].Code != 0 {
			t.Fatalf("height %d: code %d", c.Height, res[0].Code)
		}
	}
	if got, err := c.App.Tokens.Accounts(heir); err != nil || len(got) != 1 || got[0] != tokenPub(0) {
		t.Fatalf("the heir holds %x (%v), want the one moved", got, err)
	}
	if got, err := c.App.Tokens.Accounts(tc.Pub(owner)); err != nil || len(got) != 2 {
		t.Fatalf("the owner holds %d accounts (%v), want 2 after one moved and one closed", len(got), err)
	}
	entries := func() string {
		it, err := c.TokenIndexDB().Iterator(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer it.Close()
		d := sha256.New()
		for ; it.Valid(); it.Next() {
			if it.Key()[0] == 'm' {
				continue
			}
			d.Write(binary.BigEndian.AppendUint32(nil, uint32(len(it.Key()))))
			d.Write(it.Key())
			d.Write(it.Value())
		}
		return string(d.Sum(nil))
	}
	kept := entries()
	// A marker from older rules makes the next block rebuild the index from
	// committed state.
	if err := c.TokenIndexDB().Set([]byte{'m'}, append([]byte{0}, binary.BigEndian.AppendUint64(nil, uint64(c.Height))...)); err != nil {
		t.Fatal(err)
	}
	c.Restart()
	c.Block()
	c.App.Tokens.Wait()
	if h := c.App.Tokens.Height(); h != uint64(c.Height) {
		t.Fatalf("the rebuilt index is current to %d, the chain is at %d", h, c.Height)
	}
	if entries() != kept {
		t.Fatal("the index the blocks kept is not the one a rebuild from the same state writes")
	}
}
