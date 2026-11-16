package svm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/mr-tron/base58"
)

// DelegationProgram is MagicBlock's delegation program, which `svmd init
// --ephemeral-validator` turns on with its companions. On this chain it has no
// upgrade authority, so it has no admin. Genesis writes what the admin's
// instructions would write: the fees vaults.
var DelegationProgram = mustKey("DELeGGvXpWV2fqJUhqcF5ZSYMS4JTLjteaAMARRSaeSh")

// ValidatorFeesVault is an admitted ephemeral validator's fees vault,
// ["v-fees-vault", validator]. The delegation program treats a validator as
// admitted when this account exists.
func ValidatorFeesVault(validator [32]byte) [32]byte {
	return FindProgramAddress([][]byte{[]byte("v-fees-vault"), validator[:]}, DelegationProgram)
}

// ProtocolFeesVault is the protocol's fees vault, ["fees-vault"].
func ProtocolFeesVault() [32]byte {
	return FindProgramAddress([][]byte{[]byte("fees-vault")}, DelegationProgram)
}

// FeesVault is a fees vault as InitValidatorFeesVault and
// InitProtocolFeesVault leave it: 8 zero bytes the delegation program owns,
// at the rent-exempt minimum.
func FeesVault(address [32]byte) GenesisAccount {
	return rentExempt(address, DelegationProgram, false, make([]byte, 8))
}

// checkEphemeral refuses a genesis that admits ephemeral validators without
// what they need: the delegation program in programs[], the protocol's fees
// vault and each validator's, as FeesVault writes them.
func (g GenesisState) checkEphemeral() error {
	if len(g.EphemeralValidators) == 0 {
		return nil
	}
	if !slices.ContainsFunc(g.Programs, func(p GenesisProgram) bool { return p.Address == base58.Encode(DelegationProgram[:]) }) {
		return fmt.Errorf("svm genesis: ephemeral validators are admitted, and the delegation program is not in programs[]; run svmd add-genesis-programs")
	}
	vaults := [][32]byte{ProtocolFeesVault()}
	for _, v := range g.EphemeralValidators {
		k, err := decodeKey(v)
		if err != nil {
			return fmt.Errorf("svm genesis: ephemeral validator %q: %w", v, err)
		}
		vaults = append(vaults, ValidatorFeesVault(k))
	}
	for _, vault := range vaults {
		want := FeesVault(vault)
		i := slices.IndexFunc(g.Accounts, func(a GenesisAccount) bool { return a.Pubkey == want.Pubkey })
		if i < 0 || g.Accounts[i].Owner != want.Owner || !bytes.Equal(g.Accounts[i].Data, want.Data) {
			return fmt.Errorf("svm genesis: the fees vault %s is not 8 zero bytes the delegation program owns", want.Pubkey)
		}
	}
	return nil
}

// VRFProgram is MagicBlock's VRF program, which `svmd init --vrf-oracle`
// turns on. It has no upgrade authority here, so it has no admin. Genesis
// writes what the admin's ModifyOracles would write: the oracle list and the
// data of each oracle.
var VRFProgram = mustKey("Vrf1RNUjXmQGjmQrQLvJHs9SNkvDJEsRVFPkfSQUwGz")

// VRFOracle is an oracle that the network admits: its identity and its VRF
// public key. The key is a compressed Ristretto point, which `vrf-cli
// derive-pubkey` prints for the oracle's keypair. They are the two arguments
// of ModifyOracles.
type VRFOracle struct {
	Identity  string `json:"identity"`
	VRFPubkey string `json:"vrf_pubkey"`
}

// OracleList is VRF's oracle list, ["oracles"], as ModifyOracles leaves it:
// the 8-byte discriminator 0, then the identities as a borsh Vec.
func OracleList(identities [][32]byte) GenesisAccount {
	data := binary.LittleEndian.AppendUint32(make([]byte, 8), uint32(len(identities)))
	for _, id := range identities {
		data = append(data, id[:]...)
	}
	return vrfAccount(FindProgramAddress([][]byte{[]byte("oracles")}, VRFProgram), data)
}

// OracleData is an oracle's data, ["oracle", identity]: the discriminator 1,
// the VRF public key, the registration slot and the open queue count. Genesis
// registers the oracle at slot 0, so it can open its queues from slot 200.
func OracleData(identity, vrfPubkey [32]byte) GenesisAccount {
	data := append([]byte{1, 0, 0, 0, 0, 0, 0, 0}, vrfPubkey[:]...)
	data = append(data, make([]byte, 16)...)
	return vrfAccount(FindProgramAddress([][]byte{[]byte("oracle"), identity[:]}, VRFProgram), data)
}

func vrfAccount(address [32]byte, data []byte) GenesisAccount {
	return rentExempt(address, VRFProgram, false, data)
}

// checkVRF refuses a genesis that admits VRF oracles without the VRF program
// in programs[], the oracle list or each oracle's data, as OracleList and
// OracleData write them.
func (g GenesisState) checkVRF() error {
	if len(g.VRFOracles) == 0 {
		return nil
	}
	if !slices.ContainsFunc(g.Programs, func(p GenesisProgram) bool { return p.Address == base58.Encode(VRFProgram[:]) }) {
		return fmt.Errorf("svm genesis: VRF oracles are admitted, and the VRF program is not in programs[]; run svmd add-genesis-programs")
	}
	var identities [][32]byte
	want := []GenesisAccount{}
	for _, o := range g.VRFOracles {
		id, err := decodeKey(o.Identity)
		if err != nil {
			return fmt.Errorf("svm genesis: VRF oracle %q: %w", o.Identity, err)
		}
		key, err := decodeKey(o.VRFPubkey)
		if err != nil {
			return fmt.Errorf("svm genesis: VRF oracle %s's VRF public key %q: %w", o.Identity, o.VRFPubkey, err)
		}
		identities = append(identities, id)
		want = append(want, OracleData(id, key))
	}
	for _, w := range append(want, OracleList(identities)) {
		i := slices.IndexFunc(g.Accounts, func(a GenesisAccount) bool { return a.Pubkey == w.Pubkey })
		if i < 0 || g.Accounts[i].Owner != w.Owner || !bytes.Equal(g.Accounts[i].Data, w.Data) {
			return fmt.Errorf("svm genesis: VRF's account %s is not what ModifyOracles leaves", w.Pubkey)
		}
	}
	return nil
}

// SquadsProgram is Squads v4, which every network carries to hold the
// settlement keys.
var SquadsProgram = mustKey("SQDS4ep65T869zMMBKyuUq6aD6EgTu8psMjkvj52pCf")
