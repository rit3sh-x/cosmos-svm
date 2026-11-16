package svm

import (
	json "encoding/json/v2"
	"os"
	"testing"

	"github.com/mr-tron/base58"
)

// TestProgramDataAddressMatchesManifest proves that the derived ProgramData
// addresses match what mainnet reports for every loader v3 program in the
// manifest, including the mainnet rows.
func TestProgramDataAddressMatchesManifest(t *testing.T) {
	raw, err := os.ReadFile("../../genesis/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Programs []struct {
			Address     string `json:"address"`
			ProgramData string `json:"programdata"`
			Mainnet     struct {
				ProgramData string `json:"programdata"`
			} `json:"mainnet"`
		} `json:"programs"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, p := range m.Programs {
		programData := p.ProgramData + p.Mainnet.ProgramData
		if programData == "" {
			continue
		}
		address, _ := decodeKey(p.Address)
		got := ProgramDataAddress(address)
		if base58.Encode(got[:]) != programData {
			t.Errorf("%s: derived %s, mainnet %s", p.Address, base58.Encode(got[:]), programData)
		}
		checked++
	}
	// Every loader v3 row of the manifest: the release programs, the ephemeral
	// rollup and VRF parts, Pyth's six, Squads, and tuktuk and cron.
	if checked != 18 {
		t.Fatalf("checked %d loader v3 programs, want 18", checked)
	}
}

// A seeded account holds the rent-exempt minimum for its data, and its rent
// epoch is Agave's u64::MAX.
func TestASeededAccountIsRentExempt(t *testing.T) {
	a := rentExempt([32]byte{1}, LoaderV3, true, make([]byte, programLen))
	if a.Lamports != MinimumBalance(programLen) || a.RentEpoch != 18_446_744_073_709_551_615 ||
		!a.Executable || a.Owner != "BPFLoaderUpgradeab1e11111111111111111111111" || len(a.Data) != programLen {
		t.Fatalf("%+v", a)
	}
}
