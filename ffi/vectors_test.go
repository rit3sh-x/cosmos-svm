package ffi_test

import (
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"math/big"
	"testing"

	"filippo.io/edwards25519"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// This file runs ed25519 edge vectors through admission and through the
// ed25519 precompile. The test checks that the vectors are what their names
// say. It also checks that both paths pass the valid signature and refuse the
// flipped one.

// smallOrder is the eight points of order dividing 8, as Monero's and the
// "Taming the many EdDSAs" lists write them.
var smallOrder = []string{
	"0100000000000000000000000000000000000000000000000000000000000000",
	"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
	"0000000000000000000000000000000000000000000000000000000000000000",
	"0000000000000000000000000000000000000000000000000000000000000080",
	"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05",
	"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85",
	"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a",
	"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac03fa",
}

var (
	fieldP = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	orderL = func() *big.Int {
		l, _ := new(big.Int).SetString("7237005577332262213973186563042994240857116359379907606001950938285454250989", 10)
		return l
	}()
)

func le32(n *big.Int) [32]byte {
	var out [32]byte
	b := n.Bytes()
	for i := range b {
		out[i] = b[len(b)-1-i]
	}
	return out
}

func fromLE(b []byte) *big.Int {
	r := make([]byte, len(b))
	for i := range b {
		r[i] = b[len(b)-1-i]
	}
	return new(big.Int).SetBytes(r)
}

func point(s string) [32]byte {
	b, _ := hex.DecodeString(s)
	return [32]byte(b)
}

// vector is a public key and a signature over msg.
type vector struct {
	name string
	key  [32]byte
	sig  func(msg []byte) [64]byte
}

func vectors() []vector {
	real := tc.Key(77)
	realKey := tc.Pub(real)
	sign := func(msg []byte) [64]byte { return [64]byte(ed25519.Sign(real, msg)) }
	identity := point(smallOrder[0])
	zeroSig := func(r [32]byte) func([]byte) [64]byte {
		return func([]byte) [64]byte { var s [64]byte; copy(s[:], r[:]); return s }
	}
	vs := []vector{
		{"valid", realKey, sign},
		{"one bit flipped", realKey, func(m []byte) [64]byte { s := sign(m); s[10] ^= 1; return s }},
		{"malleable: s plus L", realKey, func(m []byte) [64]byte {
			s := sign(m)
			high := le32(new(big.Int).Add(fromLE(s[32:]), orderL))
			copy(s[32:], high[:])
			return s
		}},
		{"s with its top three bits set", realKey, func(m []byte) [64]byte { s := sign(m); s[63] |= 0xe0; return s }},
		{"s equal to L", identity, func([]byte) [64]byte {
			var s [64]byte
			copy(s[:], identity[:])
			l := le32(orderL)
			copy(s[32:], l[:])
			return s
		}},
		// y = p + 1 is the identity again, written non-canonically.
		{"non-canonical public key", le32(new(big.Int).Add(fieldP, big.NewInt(1))), zeroSig(identity)},
		{"non-canonical R", identity, zeroSig(le32(new(big.Int).Add(fieldP, big.NewInt(1))))},
		{"identity with the sign bit set", point("0100000000000000000000000000000000000000000000000000000000000080"), zeroSig(identity)},
		{"small-order R under a real key", realKey, zeroSig(point(smallOrder[1]))},
	}
	for i, p := range smallOrder {
		vs = append(vs, vector{"small-order public key " + string(rune('0'+i)), point(p), zeroSig(identity)})
	}
	return vs
}

// ed25519Instruction is one verification of the ed25519 precompile with its
// key, signature and message inline (Ed25519SignatureOffsets, u16::MAX for
// "this instruction").
func ed25519Instruction(key [32]byte, sig [64]byte, msg []byte) []byte {
	const head = 2 + 14
	data := []byte{1, 0}
	for _, v := range []uint16{head + 32, 0xffff, head, 0xffff, head + 96, uint16(len(msg)), 0xffff} {
		data = binary.LittleEndian.AppendUint16(data, v)
	}
	data = append(data, key[:]...)
	data = append(data, sig[:]...)
	return append(data, msg...)
}

// withSigner returns tx with its one signer and signature replaced, so
// admission verifies the vector over the transaction's own message.
func withSigner(tx []byte, v vector) []byte {
	out := append([]byte(nil), tx...)
	msg := out[65:]
	keyAt := 65 + 3 + 1 // the header, then one byte of key count
	copy(out[keyAt:keyAt+32], v.key[:])
	sig := v.sig(msg)
	copy(out[1:65], sig[:])
	return out
}

func TestTheSignatureVectors(t *testing.T) {
	for _, p := range smallOrder {
		b := point(p)
		pt, err := new(edwards25519.Point).SetBytes(b[:])
		if err != nil {
			continue // the sign-bit variant of x = 0 is not a point
		}
		if new(edwards25519.Point).MultByCofactor(pt).Equal(edwards25519.NewIdentityPoint()) != 1 {
			t.Fatalf("%s is not of small order", p)
		}
	}

	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	_, next, err := c.App.Next()
	if err != nil {
		t.Fatal(err)
	}
	base := tc.Tx(c.Latest(), tc.Key(78), nil, tc.Transfer(tc.Key(78), tc.Pub(tc.Key(79)), 1))
	msg := []byte("svm signature vector")
	var precompiles [][]byte
	admitted := map[string]bool{}
	for _, v := range vectors() {
		tx := withSigner(base, v)
		_, err := ffi.CheckTx(tx, false, next.Height)
		admitted[v.name] = err == nil
		precompiles = append(precompiles, tc.Tx(c.Latest(), faucet, nil,
			tc.Instruction{Program: svm.Ed25519Program, Data: ed25519Instruction(v.key, v.sig(msg), msg)}))
	}
	if !admitted["valid"] || admitted["one bit flipped"] {
		t.Fatalf("admission is not checking the vectors' signatures: %v", admitted)
	}
	res, _ := c.Block(precompiles...)
	if len(res) != len(precompiles) || res[0].Code != 0 || res[1].Code == 0 {
		t.Fatalf("the precompile did not verify the valid vector and refuse the flipped one: %v", res)
	}
}
