package main

import (
	"bytes"
	json "encoding/json/v2"
	"os"
	"slices"
	"testing"

	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestAddGenesisAccountWritesASystemRow proves the command that developers.md
// section 2 runs to fund the faucet and its reserve. It writes a System-owned
// row that holds whole SOL, and it refuses any other input without a change to
// the file.
func TestAddGenesisAccountWritesASystemRow(t *testing.T) {
	a := tmp(t)
	cfg := home(t, a)
	add := func(pubkey, sol string) error {
		return editSVMGenesis(cfg.GenesisFile(), func(g *svm.GenesisState) error {
			return addGenesisAccount(g, pubkey, sol)
		})
	}
	faucetKey := tc.Pub(tc.Key(7))
	faucet := base58.Encode(faucetKey[:])
	if err := add(faucet, "1000"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfg.GenesisFile())
	if err != nil {
		t.Fatal(err)
	}
	_, state := decode(t, a, raw)
	var g svm.GenesisState
	if err := json.Unmarshal(state[svm.ModuleName], &g); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(g.Accounts, func(r svm.GenesisAccount) bool { return r.Pubkey == faucet })
	if i < 0 {
		t.Fatalf("accounts[] has no row for %s", faucet)
	}
	if r := g.Accounts[i]; r.Lamports != 1000*lamportsPerSOL || r.Owner != base58.Encode(svm.SystemProgram[:]) ||
		len(r.Data) != 0 || r.Executable || r.RentEpoch != ^uint64(0) {
		t.Fatalf("the row is %+v", r)
	}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}

	otherKey := tc.Pub(tc.Key(8))
	other := base58.Encode(otherKey[:])
	for _, bad := range [][2]string{
		{faucet, "1"},                   // already funded
		{"not-a-key", "1"},              // not base58
		{other[:len(other)-1], "1"},     // 31 bytes
		{other, "1.5"},                  // not whole SOL
		{other, "-1"},                   // negative
		{other, "99999999999999999999"}, // overflows lamports
	} {
		if add(bad[0], bad[1]) == nil {
			t.Fatalf("add-genesis-account %q %q was accepted", bad[0], bad[1])
		}
	}
	after, err := os.ReadFile(cfg.GenesisFile())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, after) {
		t.Fatal("a refused add-genesis-account changed the genesis file")
	}
}
