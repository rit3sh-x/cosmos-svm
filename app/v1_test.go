package app_test

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// enableTxV1 is SIMD-0385. It is inactive at the pin (genesis/features.json)
// and active from genesis by exception (genesis/exceptions.json).
const enableTxV1 = "txv1aq4pp281K9um3tnPgkfX8UqtFT6wcVW3hNezGLL"

// memoProgram returns the manifest's SPL Memo v3 row. It takes arbitrary
// UTF-8 bytes and no accounts, so a test can pad a transaction to an exact
// size and still execute it.
func memoProgram(t *testing.T) svm.GenesisProgram {
	t.Helper()
	for _, p := range tc.ManifestPrograms(t) {
		if p.Address == memoV3 {
			return p
		}
	}
	t.Fatal("no Memo v3 row")
	return svm.GenesisProgram{}
}

// v1Config is what a V1 message must carry. The compute budget is in the
// message, and an absent compute unit limit means zero units, not the default.
var v1Config = tc.V1Config{ComputeUnitLimit: 1_400_000, LoadedAccountsDataSize: 1 << 20}

// pad builds a transaction of exactly size bytes. It holds the caller's
// instructions, then one memo whose data is the padding.
//
// pad measures the base at a padding whose length prefix already has its
// final width, because a legacy instruction's ShortU16 data length grows a
// second byte at 128. A V1 instruction's data length is a fixed two-byte
// field, so the same measurement is exact there too.
func pad(t *testing.T, size int, build func(memo []byte) []byte) []byte {
	t.Helper()
	const measured = 200
	base := len(build(bytes.Repeat([]byte("a"), measured))) - measured
	if base+measured > size {
		t.Fatalf("the transaction is %d bytes before padding, over the %d wanted", base, size)
	}
	tx := build(bytes.Repeat([]byte("a"), size-base))
	if len(tx) != size {
		t.Fatalf("padded to %d bytes, want %d", len(tx), size)
	}
	return tx
}

// TestV1RunsFromGenesis proves that enable_tx_v1 is active from genesis. One
// transaction of exactly 4,096 bytes executes in the first block that can hold
// a transaction, and the 1,232-byte legacy limit stays. The engine test
// a_v1_transaction_is_rejected_while_enable_tx_v1_is_off covers the gate off.
func TestV1RunsFromGenesis(t *testing.T) {
	faucet := tc.Key(1)
	row := memoProgram(t)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Programs: []svm.GenesisProgram{row}})
	program := key(t, row.Address)
	// Genesis reaches the committed store with the first block.
	c.Block()
	if height, on := activeAt(t, c, enableTxV1); !on || height != 0 {
		t.Fatalf("enable_tx_v1 is active %v from height %d, want from genesis", on, height)
	}

	to := tc.Pub(tc.Key(9))
	amount := svm.MinimumBalance(0)
	v1 := pad(t, 4096, func(p []byte) []byte {
		return tc.TxV1(c.Latest(), v1Config, faucet, nil,
			tc.Transfer(faucet, to, amount), memo(program, string(p)))
	})
	// The engine decodes the bytes with `solana-transaction`, serialises the
	// decoded message again with `solana-message`, and verifies the signature
	// over that. If admission passes, the Go builder and the pinned crate agree
	// byte for byte (`engine/src/check.rs`).
	if res := c.Check(c.Wrap(v1), false); res.Code != 0 {
		t.Fatalf("a 4,096-byte V1 transaction was refused at admission: %s", res.Log)
	}
	res, _ := c.Block(v1)
	if c.Height != 2 || res[0].Code != 0 {
		t.Fatalf("the V1 transaction at height %d: code %d, %s", c.Height, res[0].Code, res[0].Log)
	}
	if got := c.Lamports(to); got != amount {
		t.Fatalf("the V1 transfer left %d lamports, want %d", got, amount)
	}

	// The config mask has an effect. The same shape with a compute unit limit
	// of 1,000 runs out of units, which is observable only if the message's
	// inline budget reaches the runtime. A V1 message carries no ComputeBudget
	// instructions, and an absent limit means zero units.
	starved := tc.Pub(tc.Key(13))
	res, _ = c.Block(pad(t, 4096, func(p []byte) []byte {
		return tc.TxV1(c.Latest(), tc.V1Config{ComputeUnitLimit: 1_000, LoadedAccountsDataSize: 1 << 20},
			faucet, nil, tc.Transfer(faucet, starved, amount), memo(program, string(p)))
	}))
	if res[0].Code == 0 {
		t.Fatal("a V1 transaction ran a 3,900-byte memo inside 1,000 compute units")
	}
	if got := c.Lamports(starved); got != 0 {
		t.Fatalf("a transaction that ran out of units still moved %d lamports", got)
	}

	// 4,097 bytes is too big.
	over := pad(t, 4097, func(p []byte) []byte {
		return tc.TxV1(c.Latest(), v1Config, faucet, nil,
			tc.Transfer(faucet, tc.Pub(tc.Key(10)), amount), memo(program, string(p)))
	})
	if res := c.Check(c.Wrap(over), false); res.Code == 0 {
		t.Fatal("a 4,097-byte V1 transaction was admitted")
	}

	// The 4,096-byte limit applies only to V1. Legacy stops at 1,232.
	legacy := func(size int, to ed25519.PrivateKey) []byte {
		return pad(t, size, func(p []byte) []byte {
			return tc.Tx(c.Latest(), faucet, nil,
				tc.Transfer(faucet, tc.Pub(to), amount), memo(program, string(p)))
		})
	}
	if res := c.Check(c.Wrap(legacy(1232, tc.Key(11))), false); res.Code != 0 {
		t.Fatalf("a 1,232-byte legacy transaction was refused: %s", res.Log)
	}
	if res := c.Check(c.Wrap(legacy(1233, tc.Key(12))), false); res.Code == 0 {
		t.Fatal("a 1,233-byte legacy transaction was admitted")
	}
}
