package app_test

import (
	"crypto/ed25519"
	"slices"
	"testing"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestHistoryBytesPerTransaction logs the history bytes of each common
// transaction shape, as the history byte counter measures them.
func TestHistoryBytesPerTransaction(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Programs: tc.ManifestPrograms(t)})
	c.Block()
	token := key(t, splToken)
	ata := key(t, "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL")
	mint := tc.Key(40)
	if res, _ := c.Block(initializeMint(t, c, c.Latest(), faucet, mint, token)); res[0].Code != 0 {
		t.Fatalf("mint: code %d", res[0].Code)
	}

	measure := func(name string, tx []byte) uint64 {
		before := c.App.History.Bytes()
		if res, _ := c.Block(tx); res[0].Code != 0 {
			t.Fatalf("%s: code %d", name, res[0].Code)
		}
		n := c.App.History.Bytes() - before
		t.Logf("%-40s %5d bytes", name, n)
		return n
	}
	owner := tc.Key(41)
	measure("system transfer", tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(owner), 1_000_000_000)))
	measure("memo v3, 32 bytes", tc.Tx(c.Latest(), faucet, nil, memo(key(t, memoV3), "thirty-two bytes of memo text!!!")))
	measure("spl token initialize mint", initializeMint(t, c, c.Latest(), faucet, tc.Key(42), token))
	// The associated token account address. Its seeds are the owner, the token
	// program and the mint.
	owned := svm.FindProgramAddress([][]byte{pub(owner), token[:], pub(mint)}, ata)
	measure("ata create idempotent, with CPIs", tc.Tx(c.Latest(), faucet, nil,
		tc.Instruction{Program: ata, Accounts: []tc.Meta{
			tc.Signer(tc.Pub(faucet), true), tc.Writable(owned),
			tc.Readonly(tc.Pub(owner)), tc.Readonly(tc.Pub(mint)), tc.Readonly(svm.SystemProgram), tc.Readonly(token),
		}, Data: []byte{1}}))
}

func pub(k ed25519.PrivateKey) []byte {
	p := tc.Pub(k)
	return p[:]
}

// TestTheHistoryWriterCrossesTheBoundaryForNoRecord proves that the block path
// crosses the FFI a few times a block, never once per transaction. The meta
// record of the execute response carries the keys for the address index, so a
// block of transfers finalizes with no TransactionKeys call.
func TestTheHistoryWriterCrossesTheBoundaryForNoRecord(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	var txs [][]byte
	for i := 0; i < 8; i++ {
		txs = append(txs, tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(byte(50+i))), 1_000_000_000)))
	}
	before := ffi.TransactionKeyCalls()
	res, _ := c.Block(txs...)
	for i, r := range res {
		if r.Code != 0 {
			t.Fatalf("transfer %d: code %d", i, r.Code)
		}
	}
	if got := ffi.TransactionKeyCalls() - before; got != 0 {
		t.Fatalf("finalizing %d transfers crossed TransactionKeys %d times, want none", len(txs), got)
	}
	// The index uses the keys in each record. They must be the static keys of
	// the message.
	records, err := c.App.History.AtHeight(uint64(c.Height))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != len(txs) {
		t.Fatalf("%d records for %d transfers", len(records), len(txs))
	}
	for _, r := range records {
		_, static, err := ffi.TransactionKeys(r.Tx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal([][32]byte(r.Meta.StaticKeys), static) {
			t.Fatalf("the record stores keys %x, the engine parses %x", r.Meta.StaticKeys, static)
		}
	}
}
