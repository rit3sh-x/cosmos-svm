package svm

import (
	"strings"
	"testing"

	"github.com/mr-tron/base58"
)

// TestAGenesisThatAdmitsAValidatorHoldsWhatItNeeds proves that a genesis that
// lists an ephemeral validator does not start if it lacks the validator's fees
// vault, the protocol's fees vault, or the delegation program.
func TestAGenesisThatAdmitsAValidatorHoldsWhatItNeeds(t *testing.T) {
	validator := mustKey("MAS1Dt9qreoRMQ14YQuhg8UTZMMzDdKhmkZMECCzk57")
	dlp := base58.Encode(DelegationProgram[:])
	g := DefaultGenesis()
	g.EphemeralValidators = []string{base58.Encode(validator[:])}
	if err := g.checkEphemeral(); err == nil || !strings.Contains(err.Error(), "the delegation program is not in programs[]") {
		t.Fatalf("no delegation program: %v", err)
	}
	g.Programs = append(g.Programs, GenesisProgram{Address: dlp})
	if err := g.checkEphemeral(); err == nil || !strings.Contains(err.Error(), "fees vault") {
		t.Fatalf("no fees vaults: %v", err)
	}
	g.Accounts = append(g.Accounts, FeesVault(ProtocolFeesVault()), FeesVault(ValidatorFeesVault(validator)))
	if err := g.checkEphemeral(); err != nil {
		t.Fatal(err)
	}
	g.Accounts[len(g.Accounts)-1].Data = []byte{1, 0, 0, 0, 0, 0, 0, 0}
	if err := g.checkEphemeral(); err == nil {
		t.Fatal("a fees vault with other bytes passed")
	}
}

// TestAGenesisThatAdmitsAnOracleHoldsWhatItNeeds proves the same for VRF: the
// program, the oracle list and the oracle's data.
func TestAGenesisThatAdmitsAnOracleHoldsWhatItNeeds(t *testing.T) {
	identity, key := mustKey("paywJiVATrVDLYLmowJqzG6MsaCt77L8WyTnBb2754t"), mustKey("Vrf1RNUjXmQGjmQrQLvJHs9SNkvDJEsRVFPkfSQUwGz")
	g := DefaultGenesis()
	g.VRFOracles = []VRFOracle{{Identity: base58.Encode(identity[:]), VRFPubkey: base58.Encode(key[:])}}
	if err := g.checkVRF(); err == nil || !strings.Contains(err.Error(), "the VRF program is not in programs[]") {
		t.Fatalf("no VRF program: %v", err)
	}
	g.Programs = append(g.Programs, GenesisProgram{Address: base58.Encode(VRFProgram[:])})
	if err := g.checkVRF(); err == nil {
		t.Fatal("a genesis with no oracle list passed")
	}
	g.Accounts = append(g.Accounts, OracleList([][32]byte{identity}), OracleData(identity, key))
	if err := g.checkVRF(); err != nil {
		t.Fatal(err)
	}
	// The oracle list as run.js writes it for Agave (rpc/accept/er/run.js).
	if d := OracleList([][32]byte{identity}).Data; len(d) != 44 || d[8] != 1 {
		t.Fatalf("the oracle list is %d bytes: %x", len(d), d)
	}
}
