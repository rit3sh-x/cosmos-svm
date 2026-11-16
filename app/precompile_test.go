package app_test

import (
	"bytes"
	"encoding/hex"
	"testing"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// The instruction data of one verification instruction per precompile. Anza's
// own constructors build each over fixed vectors, and Anza's verifiers accept
// each. ed25519 is RFC 8032 test vector 2. secp256k1 and secp256r1 each use
// one fixed key that signs "svm precompile vector". engine/src/runtime.rs
// holds the same four vectors and runs them against the callback directly.
// This file runs them through the chain.
//
// The secp256k1 offsets name instruction index 0, not u16::MAX, because its
// constructor writes 0. Its instruction must be the first in the message. The
// other two refer to their own instruction.
const (
	ed25519Valid     = "01003000ffff1000ffff70000100ffff3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c92a009a9f0d4cab8720e820b5f642540a2b27b5416503f8fb3762223ebdb69da085ac1e43e15996e458f3613d0f11d8c387b2eaeb4302aeeb00d291612bb0c0072"
	ed25519Malleable = "01003000ffff1000ffff70001000ffff10eb7c3acfb2bed3e0d6ab89bf5a3d6afddd1176ce4812e38d9fd485058fdb1f00000000000000000000000000000000000000000000000000000000000000009472a69cd9a701a50d130ed52189e2455b23767db52cacb8716fb896ffeeac0965643235353139766563746f72732033"
	secp256k1Valid   = "012000000c0000610015000076979e41b566d656524e8c64bd684a7cf1f1eaa1667622e1b94f76aa91a346e511a12777e4c737a3074185881dcc37572c87394f55e4b600564627f0d68e3ec16dd4696e53fe828ffe447d21afa1e4c41610321c0173766d20707265636f6d70696c6520766563746f72"
	secp256r1Valid   = "01003100ffff1000ffff71001500ffff03798953e7e8134fdf3c139f63d3fbccc252a28b6ca5059e618374a81231240f3fc251469f178835e7c3ba38ee77e704e303ede6c2b42f0b256b4d5b0dead56cd375c2eddc2962b25268db2c13ee0118c436ee3ce9c8cb4f6832f0908ffdbe52fe73766d20707265636f6d70696c6520766563746f72"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("test vector: %v", err)
	}
	return b
}

// TestPrecompileInstructionsVerify proves that a verification instruction
// verifies on the chain, and one over a changed message does not. Without the
// genesis rows, the transaction fails at account load with
// ProgramAccountNotFound before the engine reaches its callback.
func TestPrecompileInstructionsVerify(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()

	for _, p := range []struct {
		name   string
		id     [32]byte
		vector string
	}{
		{"ed25519", svm.Ed25519Program, ed25519Valid},
		{"secp256k1", svm.Secp256k1Program, secp256k1Valid},
		{"secp256r1", svm.Secp256r1Program, secp256r1Valid},
	} {
		data := unhex(t, p.vector)
		res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Instruction{Program: p.id, Data: data}))
		if res[0].Code != 0 {
			t.Fatalf("%s: a valid signature failed with code %d: %s", p.name, res[0].Code, res[0].Log)
		}

		// The last byte of each vector is the last byte of the signed message,
		// so a flipped bit makes it a signature over a different message.
		wrong := append([]byte(nil), data...)
		wrong[len(wrong)-1] ^= 1
		res, _ = c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Instruction{Program: p.id, Data: wrong}))
		if res[0].Code == 0 {
			t.Fatalf("%s: a signature over a changed message committed", p.name)
		}
	}

	// The verifiers use Anza's crate, not a port, because of verify_strict. A
	// plain verify accepts this signature.
	malleable := unhex(t, ed25519Malleable)
	res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Instruction{Program: svm.Ed25519Program, Data: malleable}))
	if res[0].Code == 0 {
		t.Fatal("ed25519: a malleable signature committed")
	}
}

// TestPrecompileAccountsAreMainnetsRows proves that the three rows match
// mainnet, asymmetry included. Agave writes secp256r1 through
// Bank::add_precompiled_account_with_owner, with empty data and rent epoch
// zero, and the older two hold their program names and u64::MAX. Clients see
// these accounts through getAccountInfo, so the chain copies the shape.
func TestPrecompileAccountsAreMainnetsRows(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()

	for _, p := range []struct {
		name      string
		id        [32]byte
		data      string
		rentEpoch uint64
	}{
		{"secp256k1", svm.Secp256k1Program, "secp256k1_program", ^uint64(0)},
		{"ed25519", svm.Ed25519Program, "ed25519_program", ^uint64(0)},
		{"secp256r1", svm.Secp256r1Program, "", 0},
	} {
		a, ok := c.Account(p.id)
		if !ok {
			t.Fatalf("%s: no account at the precompile address", p.name)
		}
		if a.Lamports != 1 || !a.Executable || a.Owner != svm.NativeLoader {
			t.Fatalf("%s: %d lamports, executable %v, owner %x", p.name, a.Lamports, a.Executable, a.Owner)
		}
		if !bytes.Equal(a.Data, []byte(p.data)) {
			t.Fatalf("%s: data %q, want %q", p.name, a.Data, p.data)
		}
		if a.RentEpoch != p.rentEpoch {
			t.Fatalf("%s: rent epoch %d, want %d", p.name, a.RentEpoch, p.rentEpoch)
		}
	}
}
