package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	cmtcfg "github.com/cometbft/cometbft/config"
	"github.com/mr-tron/base58"

	svrcmd "github.com/cosmos/cosmos-sdk/server/cmd"
	genutiltypes "github.com/cosmos/cosmos-sdk/x/genutil/types"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// svmGenesis returns the x/svm section of the genesis file in home.
func svmGenesis(t *testing.T, home string) svm.GenesisState {
	t.Helper()
	cfg := cmtcfg.DefaultConfig()
	cfg.SetRoot(home)
	doc, err := genutiltypes.AppGenesisFromFile(cfg.GenesisFile())
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]jsontext.Value
	if err := json.Unmarshal(doc.AppState, &state); err != nil {
		t.Fatal(err)
	}
	var g svm.GenesisState
	if err := json.Unmarshal(state[svm.ModuleName], &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func svmd(t *testing.T, home string, args ...string) error {
	t.Helper()
	root := rootCmd(home)
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})
	root.SetArgs(append(args, "--home", home))
	return svrcmd.Execute(root, "SVMD", home)
}

// TestAnEphemeralValidatorTurnsOnItsPart proves that `svmd init
// --ephemeral-validator` lists each key and writes its fees vault and the
// protocol's. add-genesis-programs then writes the manifest's
// ephemeral-rollup rows. Without the flag, genesis holds neither the vaults
// nor those rows.
func TestAnEphemeralValidatorTurnsOnItsPart(t *testing.T) {
	var elf []byte
	for _, p := range tc.ManifestPrograms(t) {
		if p.Address == "Memo4c2pN8afCj432Lb7RMVKi9PbQnnW7ewFFaV3oAH" {
			elf = p.ELF
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(elf) }))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(elf)
	dir := t.TempDir()
	row := func(name, address, part string) string {
		return `{"name": "` + name + `", "address": "` + address + `", "loader": "BPFLoaderUpgradeab1e11111111111111111111111", "part": "` +
			part + `", "url": "` + srv.URL + `", "sha256": "` + hex.EncodeToString(sum[:]) + `"}`
	}
	dlp := base58.Encode(svm.DelegationProgram[:])
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"programs": [`+
		row("memo", "Memo4c2pN8afCj432Lb7RMVKi9PbQnnW7ewFFaV3oAH", "")+`, `+row("dlp", dlp, "ephemeral-rollup")+`]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	first, second := tc.Pub(tc.Key(70)), tc.Pub(tc.Key(71))
	validators := []string{base58.Encode(first[:]), base58.Encode(second[:])}
	protocol := svm.ProtocolFeesVault()
	vaults := []string{base58.Encode(protocol[:])}
	for _, v := range validators {
		k, _ := base58.Decode(v)
		vault := svm.ValidatorFeesVault([32]byte(k))
		vaults = append(vaults, base58.Encode(vault[:]))
	}
	programs := func(g svm.GenesisState) (out []string) {
		for _, p := range g.Programs {
			out = append(out, p.Address)
		}
		return out
	}
	held := func(g svm.GenesisState, address string) bool {
		return slices.ContainsFunc(g.Accounts, func(a svm.GenesisAccount) bool { return a.Pubkey == address })
	}

	admitted := t.TempDir()
	if err := svmd(t, admitted, "init", "a", "--chain-id", "svm-er-test",
		"--"+flagEphemeralValidator, validators[0], "--"+flagEphemeralValidator, validators[1]); err != nil {
		t.Fatal(err)
	}
	if err := svmd(t, admitted, "add-genesis-programs", dir); err != nil {
		t.Fatal(err)
	}
	g := svmGenesis(t, admitted)
	if !slices.Equal(g.EphemeralValidators, validators) {
		t.Fatalf("ephemeral_validators is %v, want %v", g.EphemeralValidators, validators)
	}
	for _, v := range vaults {
		i := slices.IndexFunc(g.Accounts, func(a svm.GenesisAccount) bool { return a.Pubkey == v })
		if i < 0 || g.Accounts[i].Owner != dlp || len(g.Accounts[i].Data) != 8 || g.Accounts[i].Lamports != svm.MinimumBalance(8) {
			t.Fatalf("fees vault %s is not 8 zero bytes the delegation program owns", v)
		}
	}
	if got := programs(g); !slices.Equal(got, []string{"Memo4c2pN8afCj432Lb7RMVKi9PbQnnW7ewFFaV3oAH", dlp}) {
		t.Fatalf("programs with the flag: %v", got)
	}

	plain := t.TempDir()
	if err := svmd(t, plain, "init", "a", "--chain-id", "svm-er-test"); err != nil {
		t.Fatal(err)
	}
	if err := svmd(t, plain, "add-genesis-programs", dir); err != nil {
		t.Fatal(err)
	}
	g = svmGenesis(t, plain)
	if len(g.EphemeralValidators) != 0 || slices.ContainsFunc(vaults, func(v string) bool { return held(g, v) }) {
		t.Fatal("a genesis without the flag admits an ephemeral validator")
	}
	if got := programs(g); !slices.Equal(got, []string{"Memo4c2pN8afCj432Lb7RMVKi9PbQnnW7ewFFaV3oAH"}) {
		t.Fatalf("programs without the flag: %v", got)
	}

	if err := svmd(t, t.TempDir(), "init", "a", "--chain-id", "svm-er-test", "--"+flagEphemeralValidator, "not-a-key"); err == nil ||
		!strings.Contains(err.Error(), "not a base58 pubkey") {
		t.Fatalf("a bad key: %v", err)
	}
}

// TestAVRFOracleTurnsOnVRFAlone proves that `svmd init --vrf-oracle
// IDENTITY:VRF_PUBKEY` lists the oracle and writes the VRF program's oracle
// list and the oracle's data. add-genesis-programs then writes the vrf part's
// row and not the ephemeral-rollup part's.
func TestAVRFOracleTurnsOnVRFAlone(t *testing.T) {
	var elf []byte
	for _, p := range tc.ManifestPrograms(t) {
		if p.Address == "Memo4c2pN8afCj432Lb7RMVKi9PbQnnW7ewFFaV3oAH" {
			elf = p.ELF
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(elf) }))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(elf)
	row := func(name, address, part string) string {
		return `{"name": "` + name + `", "address": "` + address + `", "loader": "BPFLoaderUpgradeab1e11111111111111111111111", "part": "` +
			part + `", "url": "` + srv.URL + `", "sha256": "` + hex.EncodeToString(sum[:]) + `"}`
	}
	dlp, vrf := base58.Encode(svm.DelegationProgram[:]), base58.Encode(svm.VRFProgram[:])
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"programs": [`+
		row("dlp", dlp, "ephemeral-rollup")+`, `+row("vrf", vrf, "vrf")+`]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	identity, key := tc.Pub(tc.Key(72)), tc.Pub(tc.Key(73))
	home := t.TempDir()
	if err := svmd(t, home, "init", "a", "--chain-id", "svm-er-test",
		"--"+flagVRFOracle, base58.Encode(identity[:])+":"+base58.Encode(key[:])); err != nil {
		t.Fatal(err)
	}
	if err := svmd(t, home, "add-genesis-programs", dir); err != nil {
		t.Fatal(err)
	}
	g := svmGenesis(t, home)
	if len(g.VRFOracles) != 1 || g.VRFOracles[0].Identity != base58.Encode(identity[:]) || g.VRFOracles[0].VRFPubkey != base58.Encode(key[:]) {
		t.Fatalf("vrf_oracles is %+v", g.VRFOracles)
	}
	for _, want := range []svm.GenesisAccount{svm.OracleList([][32]byte{identity}), svm.OracleData(identity, key)} {
		if !slices.ContainsFunc(g.Accounts, func(a svm.GenesisAccount) bool {
			return a.Pubkey == want.Pubkey && a.Owner == vrf && string(a.Data) == string(want.Data)
		}) {
			t.Fatalf("VRF's account %s is not in genesis as ModifyOracles leaves it", want.Pubkey)
		}
	}
	if len(g.Programs) != 1 || g.Programs[0].Address != vrf || len(g.EphemeralValidators) != 0 {
		t.Fatalf("programs %+v, ephemeral validators %v: want VRF alone", g.Programs, g.EphemeralValidators)
	}

	if err := svmd(t, t.TempDir(), "init", "a", "--chain-id", "svm-er-test", "--"+flagVRFOracle, base58.Encode(identity[:])); err == nil ||
		!strings.Contains(err.Error(), "IDENTITY:VRF_PUBKEY") {
		t.Fatalf("an oracle without its VRF key: %v", err)
	}
}
