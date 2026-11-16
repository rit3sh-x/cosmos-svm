package testchain

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"

	"filippo.io/edwards25519"
)

// Resign signs a one-signer transaction's message again with payer, with a
// random nonce in place of RFC 8032's derived one. The result is a second
// valid signature over the same message, so it is another transaction with the
// same dedup key. Any signer can make one.
func Resign(tx []byte, payer ed25519.PrivateKey) []byte {
	if tx[0] != 1 {
		panic("testchain: Resign takes a transaction with one signature")
	}
	out := append([]byte(nil), tx...)
	copy(out[1:65], signRandom(payer, out[65:]))
	return out
}

// signRandom returns an ed25519 signature with a random nonce r, where R = rB
// and S = r + H(R || A || M)a. Verification never sees how r was chosen.
func signRandom(k ed25519.PrivateKey, msg []byte) []byte {
	h := sha512.Sum512(k.Seed())
	a, err := edwards25519.NewScalar().SetBytesWithClamping(h[:32])
	if err != nil {
		panic(err)
	}
	var seed [64]byte
	if _, err := rand.Read(seed[:]); err != nil {
		panic(err)
	}
	r, err := edwards25519.NewScalar().SetUniformBytes(seed[:])
	if err != nil {
		panic(err)
	}
	R := new(edwards25519.Point).ScalarBaseMult(r).Bytes()
	digest := sha512.New()
	digest.Write(R)
	digest.Write(k.Public().(ed25519.PublicKey))
	digest.Write(msg)
	c, err := edwards25519.NewScalar().SetUniformBytes(digest.Sum(nil))
	if err != nil {
		panic(err)
	}
	return append(R, edwards25519.NewScalar().MultiplyAdd(c, a, r).Bytes()...)
}
