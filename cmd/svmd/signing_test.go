package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoProductionPackageSignsASolanaMessage proves that no package of this
// module in the production svmd build calls ed25519.Sign. The devnet faucet
// and the test-only internal/testchain are the signers, and neither is in that
// build. CometBFT vote signing is Cosmos-side, so the check covers only this
// module's packages.
func TestNoProductionPackageSignsASolanaMessage(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f",
		"{{if and .Module (eq .Module.Path \"github.com/rit3sh-x/cosmos-svm\")}}{{.Dir}} {{.GoFiles}}{{end}}",
		"github.com/rit3sh-x/cosmos-svm/cmd/svmd").Output()
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dir, files, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		for _, name := range strings.Fields(strings.Trim(files, "[]")) {
			path := filepath.Join(dir, name)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "ed25519.Sign(") {
				t.Errorf("%s signs an ed25519 message and is in the production binary", path)
			}
			checked++
		}
	}
	// A graph that came back empty would pass for the wrong reason.
	if checked < 10 {
		t.Fatalf("only %d files were checked, so the graph was not walked", checked)
	}
}
