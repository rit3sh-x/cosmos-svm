package app_test

import (
	"bytes"
	json "encoding/json/v2"
	"testing"

	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/genesis"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// pubkeyValidityProof is VerifyPubkeyValidity with the proof in instruction
// data and no context state, which the program documents as taking no
// accounts. The first byte is the instruction discriminant, 4. Anza's own
// ProofInstruction::encode_verify_proof made it over a random ElGamal keypair.
// Proving is randomised and verifying is not, so the fixed bytes stay valid.
const pubkeyValidityProof = "04ea2e8bc60acc119e8e73d7beda31058e861e98b037a0218389d3c385a5d0a60326262996dbe83bf06cabd5e32957696b8385c38787a71b0e58c6ed32e6d2ba0498291a0c182033a73b7ac5258c114ed3358500c60ef8ad7ced4cd79662f74c00"

// TestZkElGamalProofProgramVerifies proves that the ZK ElGamal proof program
// is registered and verifies a proof. The genesis feature account says the
// program is enabled from block zero, and this test proves that it is.
func TestZkElGamalProofProgramVerifies(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()

	// The account that the feature set claims, and the program account that
	// the loader needs before it can dispatch.
	var pinned struct {
		Gates []struct {
			ID          string `json:"id"`
			Class       string `json:"class"`
			ActiveAtPin bool   `json:"active_at_pin"`
		} `json:"gates"`
	}
	if err := json.Unmarshal(genesis.Features, &pinned); err != nil {
		t.Fatal(err)
	}
	const zkElGamalProofProgramEnabled = "zkhiy5oLowR7HY4zogXjCjeMXyruLqBwSWH21qcFtnv"
	claimed := false
	for _, g := range pinned.Gates {
		if g.ID == zkElGamalProofProgramEnabled {
			claimed = g.ActiveAtPin
		}
	}
	if !claimed {
		t.Fatal("the pinned set no longer says the ZK ElGamal proof program is enabled")
	}
	gate, err := base58.Decode(zkElGamalProofProgramEnabled)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Account([32]byte(gate)); !ok {
		t.Fatal("no feature account for zk_elgamal_proof_program_enabled")
	}
	program, ok := c.Account(svm.ZkElGamalProof)
	if !ok {
		t.Fatal("no program account at the ZK ElGamal proof address")
	}
	if program.Lamports != 1 || !program.Executable || program.Owner != svm.NativeLoader ||
		!bytes.Equal(program.Data, []byte("zk_elgamal_proof_program")) || program.RentEpoch != ^uint64(0) {
		t.Fatalf("the program account is not this chain's builtin shape: %+v", program)
	}

	// The feature account says the program is enabled, and this part proves
	// it. A valid proof commits, and a proof with one changed byte does not.
	proof := unhex(t, pubkeyValidityProof)
	res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Instruction{Program: svm.ZkElGamalProof, Data: proof}))
	if res[0].Code != 0 {
		t.Fatalf("a valid pubkey validity proof failed with code %d: %s", res[0].Code, res[0].Log)
	}

	wrong := append([]byte(nil), proof...)
	wrong[len(wrong)-1] ^= 1
	res, _ = c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Instruction{Program: svm.ZkElGamalProof, Data: wrong}))
	if res[0].Code == 0 {
		t.Fatal("a corrupted proof verified")
	}
}
