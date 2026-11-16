package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestEveryGenesisCarriesSquads proves that a genesis from `svmd init` and
// add-genesis-programs carries Squads v4 and its global config at
// ["multisig", "program_config"]. The config sets a fee of 0, and an authority
// and a treasury at an off-curve address that nobody can sign for.
func TestEveryGenesisCarriesSquads(t *testing.T) {
	squads := svm.SquadsProgram
	home := t.TempDir()
	if err := svmd(t, home, "init", "a", "--chain-id", "svm-squads-test"); err != nil {
		t.Fatal(err)
	}
	if err := svmd(t, home, "add-genesis-programs", "../../genesis"); err != nil {
		t.Fatal(err)
	}
	g := svmGenesis(t, home)
	if !slices.ContainsFunc(g.Programs, func(p svm.GenesisProgram) bool { return p.Address == base58.Encode(squads[:]) }) {
		t.Fatal("Squads v4 is not in genesis")
	}
	config := svm.FindProgramAddress([][]byte{[]byte("multisig"), []byte("program_config")}, squads)
	nobody := svm.FindProgramAddress([][]byte{[]byte("squads-program-config")}, svm.SystemProgram)
	i := slices.IndexFunc(g.Accounts, func(a svm.GenesisAccount) bool { return a.Pubkey == base58.Encode(config[:]) })
	if i < 0 {
		t.Fatal("the Squads global config is not in genesis")
	}
	// ProgramConfig holds an 8-byte discriminator, the authority, the creation
	// fee and the treasury.
	d := g.Accounts[i].Data
	discriminator := sha256.Sum256([]byte("account:ProgramConfig"))
	if g.Accounts[i].Owner != base58.Encode(squads[:]) || len(d) != 144 || !bytes.Equal(d[:8], discriminator[:8]) ||
		!bytes.Equal(d[8:40], nobody[:]) || binary.LittleEndian.Uint64(d[40:48]) != 0 || !bytes.Equal(d[48:80], nobody[:]) {
		t.Fatalf("the Squads global config is owned by %s: %x", g.Accounts[i].Owner, d)
	}
}
