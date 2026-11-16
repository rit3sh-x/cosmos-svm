package svm

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"

	"filippo.io/edwards25519"
	"github.com/mr-tron/base58"
)

var (
	LoaderV1 = mustKey("BPFLoader1111111111111111111111111111111111")
	LoaderV2 = mustKey("BPFLoader2111111111111111111111111111111111")
	LoaderV3 = mustKey("BPFLoaderUpgradeab1e11111111111111111111111")
)

// GenesisProgram is one row of genesis programs[]. The genesis tooling builds
// these rows from genesis/manifest.json.
type GenesisProgram struct {
	Address string `json:"address"`
	Loader  string `json:"loader"`
	SHA256  string `json:"sha256"`
	ELF     []byte `json:"elf"`
}

// The UpgradeableLoaderState sizes (solana-loader-v3-interface-7.0.0).
const (
	programLen             = 36
	programDataMetadataLen = 45
)

// ProgramAccounts is the manifest writer: the accounts that a program at its
// mainnet address occupies under its mainnet loader. InitGenesis and
// SeedPrograms both call it, so they cannot encode programs differently.
//
// The shapes are those of solana-program-binaries-4.2.1, with
// `upgrade_authority_address: None`, because every seeded program is immutable.
// slot is the ProgramData deployment slot. It is 0 at genesis, so the program
// is visible in block 1. In an upgrade handler it is the upgrade height, so the
// program is visible from the next block.
func ProgramAccounts(p GenesisProgram, slot uint64) ([]GenesisAccount, error) {
	address, err := decodeKey(p.Address)
	if err != nil {
		return nil, fmt.Errorf("svm program %s: %w", p.Address, err)
	}
	loader, err := decodeKey(p.Loader)
	if err != nil {
		return nil, fmt.Errorf("svm program %s loader: %w", p.Address, err)
	}
	digest := sha256.Sum256(p.ELF)
	if hex.EncodeToString(digest[:]) != p.SHA256 {
		return nil, fmt.Errorf("svm program %s: ELF sha256 %x, pinned %s", p.Address, digest, p.SHA256)
	}
	switch loader {
	case LoaderV1, LoaderV2:
		return []GenesisAccount{rentExempt(address, loader, true, p.ELF)}, nil
	case LoaderV3:
		programData := ProgramDataAddress(address)
		data := binary.LittleEndian.AppendUint32(nil, 2) // UpgradeableLoaderState::Program
		data = append(data, programData[:]...)
		header := binary.LittleEndian.AppendUint32(nil, 3) // ::ProgramData
		header = binary.LittleEndian.AppendUint64(header, slot)
		header = append(header, 0) // upgrade_authority_address: None
		header = append(header, make([]byte, programDataMetadataLen-len(header))...)
		return []GenesisAccount{
			rentExempt(address, loader, true, data),
			rentExempt(programData, loader, false, append(header, p.ELF...)),
		}, nil
	default:
		return nil, fmt.Errorf("svm program %s: loader %s is not v1, v2 or v3", p.Address, p.Loader)
	}
}

// RentExemptEpoch is the rent epoch of an account rent never collects from,
// solana-svm's RENT_EXEMPT_RENT_EPOCH.
const RentExemptEpoch uint64 = math.MaxUint64

// rentExempt is a genesis account holding the rent-exempt minimum for its
// data, marked rent-exempt.
func rentExempt(address, owner [32]byte, executable bool, data []byte) GenesisAccount {
	return GenesisAccount{
		Pubkey:     base58.Encode(address[:]),
		Lamports:   MinimumBalance(len(data)),
		Owner:      base58.Encode(owner[:]),
		Data:       data,
		Executable: executable,
		RentEpoch:  RentExemptEpoch,
	}
}

func decodeKey(s string) ([32]byte, error) {
	b, err := base58.Decode(s)
	if err != nil || len(b) != 32 {
		return [32]byte{}, fmt.Errorf("bad key %q", s)
	}
	return [32]byte(b), nil
}

// ProgramDataAddress is `get_program_data_address`, the loader v3 PDA seeded
// by the program address.
func ProgramDataAddress(program [32]byte) [32]byte {
	return FindProgramAddress([][]byte{program[:]}, LoaderV3)
}

// FindProgramAddress is `Pubkey::find_program_address` without the bump, which
// most callers derive rather than send.
func FindProgramAddress(seeds [][]byte, program [32]byte) [32]byte {
	address, _ := FindProgramAddressAndBump(seeds, program)
	return address
}

// FindProgramAddressAndBump is `Pubkey::find_program_address`. From bump 255
// down, it returns the first
// sha256(seeds || bump || program || "ProgramDerivedAddress") that is not a
// curve point. The bump is part of the instruction data for
// anything that re-derives the address on chain, CreateLookupTable included.
func FindProgramAddressAndBump(seeds [][]byte, program [32]byte) ([32]byte, uint8) {
	for bump := 255; bump >= 0; bump-- {
		h := sha256.New()
		for _, seed := range seeds {
			h.Write(seed)
		}
		h.Write([]byte{byte(bump)})
		h.Write(program[:])
		h.Write([]byte("ProgramDerivedAddress"))
		candidate := [32]byte(h.Sum(nil))
		if _, err := new(edwards25519.Point).SetBytes(candidate[:]); err != nil {
			return candidate, uint8(bump)
		}
	}
	panic("svm: no program derived address")
}
