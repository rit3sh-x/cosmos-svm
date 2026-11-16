package main

import (
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestEveryGenesisCarriesBothPythSets proves that a genesis from `svmd init`
// and add-genesis-programs, with no flag, carries Pyth's two program id sets.
// It also carries the three accounts that each set copies from mainnet, each
// with the sha256 that the manifest pins.
func TestEveryGenesisCarriesBothPythSets(t *testing.T) {
	raw, err := os.ReadFile("../../genesis/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		Address string `json:"address"`
		Owner   string `json:"owner"`
		Mainnet struct {
			DataSHA256 string `json:"data_sha256"`
		} `json:"mainnet"`
	}
	var m struct {
		Accounts []row `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := svmd(t, home, "init", "a", "--chain-id", "svm-pyth-test"); err != nil {
		t.Fatal(err)
	}
	if err := svmd(t, home, "add-genesis-programs", "../../genesis"); err != nil {
		t.Fatal(err)
	}
	g := svmGenesis(t, home)
	for _, id := range []string{
		"rec5EKMGg6MxZYaMdyBfgwp4d5rB9T1VQH5pJv5LtFJ", "pythWSnswVUd12oZpeFP8e9CVaEqJg25g1Vtc2biRsT", "HDwcJBJXjL9FpJ7UBsYBtaDjsBUhuLCUYoz3zr8SWWaQ",
		"rec2HHDDnjLfj4kE7VyEtFA1HPGQLK33259532cRyHp", "pyt2F414BA6dPttK6RddPZUdHfapoBN24GL5wbrPCou", "HDw2E7P8X1SkCyjvoGsfBGAVUutKcj874bXjHrpVYrVL",
	} {
		if !slices.ContainsFunc(g.Programs, func(p svm.GenesisProgram) bool { return p.Address == id }) {
			t.Errorf("Pyth's program %s is not in genesis", id)
		}
	}
	copied := slices.DeleteFunc(m.Accounts, func(a row) bool { return a.Mainnet.DataSHA256 == "" })
	// The manifest copies Pyth's six accounts from mainnet and other accounts
	// too, such as tuktuk's config and IDLs. The test checks every copied row.
	pyth := 0
	for _, a := range copied {
		if strings.HasPrefix(a.Owner, "rec") || strings.HasPrefix(a.Owner, "HDw") {
			pyth++
		}
	}
	if pyth != 6 {
		t.Fatalf("the manifest copies %d Pyth accounts from mainnet, want 6", pyth)
	}
	for _, want := range copied {
		i := slices.IndexFunc(g.Accounts, func(a svm.GenesisAccount) bool { return a.Pubkey == want.Address })
		if i < 0 {
			t.Errorf("Pyth's account %s is not in genesis", want.Address)
			continue
		}
		sum := sha256.Sum256(g.Accounts[i].Data)
		if g.Accounts[i].Owner != want.Owner || hex.EncodeToString(sum[:]) != want.Mainnet.DataSHA256 {
			t.Errorf("Pyth's account %s is owned by %s with sha256 %x, want %s and %s",
				want.Address, g.Accounts[i].Owner, sum, want.Owner, want.Mainnet.DataSHA256)
		}
	}
	if len(g.EphemeralValidators) != 0 || len(g.VRFOracles) != 0 ||
		slices.ContainsFunc(g.Programs, func(p svm.GenesisProgram) bool { return p.Address == "DELeGGvXpWV2fqJUhqcF5ZSYMS4JTLjteaAMARRSaeSh" }) {
		t.Fatal("a genesis with no flag holds a part")
	}
}
