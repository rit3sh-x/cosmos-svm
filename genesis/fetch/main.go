// Command fetch downloads the pinned programs and accounts and checks each
// against its manifest. It writes the genesis rows to genesis/programs, and
// the rows for the tests, the benchmarks and the acceptance scripts to
// programs/. `make fetch-programs` runs it from the repository root, because
// the Rust code reads those files before any Go code can download them. fetch
// removes each file that its folder's manifest does not name, because the
// benchmarks read every file in programs/. It fetches a part's row only when
// the part is named, and keeps that row's file either way. When
// SVM_MAINNET_RPC is set, fetch reads the mainnet rows through that RPC in
// place of the manifests' RPC.
//
//	go run ./genesis/fetch [PART...]
package main

import (
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rit3sh-x/cosmos-svm/genesis"
)

func main() {
	for _, set := range [][2]string{
		{"genesis/manifest.json", "genesis/programs"},
		{"programs/manifest.json", "programs"},
	} {
		m, err := genesis.LoadFrom(set[0], set[1], os.Getenv("SVM_MAINNET_RPC"), os.Args[1:]...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fetch: %s: %v\n", set[0], err)
			os.Exit(1)
		}
		for _, p := range m.Programs {
			fmt.Printf("%-29s %8d bytes  sha256 %s\n", set[1]+"/"+p.Name+".so", len(p.ELF), p.SHA256[:16])
		}
		for _, a := range m.Accounts {
			fmt.Printf("%-29s %8d bytes  account %s\n", set[1]+"/"+a.Name+".account", len(a.Data), a.Address)
		}
		named, err := rows(set[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "fetch: %s: %v\n", set[0], err)
			os.Exit(1)
		}
		files, _ := filepath.Glob(filepath.Join(set[1], "*.so"))
		accounts, _ := filepath.Glob(filepath.Join(set[1], "*.account"))
		for _, f := range append(files, accounts...) {
			if !named[filepath.Base(f)] {
				if err := os.Remove(f); err != nil {
					fmt.Fprintln(os.Stderr, "fetch:", err)
					os.Exit(1)
				}
				fmt.Printf("%-29s removed: not in %s\n", f, set[0])
			}
		}
	}
}

// rows returns the file name of every row of a manifest, whatever its part.
func rows(manifest string) (map[string]bool, error) {
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return nil, err
	}
	type row struct {
		Name string `json:"name"`
	}
	var m struct {
		Programs []row `json:"programs"`
		Accounts []row `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	named := map[string]bool{}
	for _, p := range m.Programs {
		named[p.Name+".so"] = true
	}
	for _, a := range m.Accounts {
		named[a.Name+".account"] = true
	}
	return named, nil
}
