package genesis

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Manifest holds the programs and the accounts that a manifest.json seeds.
type Manifest struct {
	Programs []Program
	Accounts []Account
}

// Program is one genesis program row built from manifest.json.
type Program struct {
	Name    string
	Address string
	Loader  string
	SHA256  string
	ELF     []byte
}

// Account is one genesis account row built from manifest.json. Its data is
// copied from mainnet or held in the row itself.
type Account struct {
	Name    string
	Address string
	Owner   string
	Data    []byte
}

// maxProgram bounds a download. The runtime allows a program of at most
// 10 MiB, and an RPC answer carries it in base64, a third larger.
const maxProgram = 16 << 20

const loaderV3 = "BPFLoaderUpgradeab1e11111111111111111111111"

var client = &http.Client{Timeout: 2 * time.Minute}

// The parts that a flag of `svmd init` turns on:
//   - EphemeralRollup: the delegation program and its companions, turned on
//     by --ephemeral-validator
//   - VRF: the VRF program, turned on by --vrf-oracle
const (
	EphemeralRollup = "ephemeral-rollup"
	VRF             = "vrf"
)

// Read reads manifest.json in dir and loads each row. It caches what it
// fetches in dir/programs. It loads a row of a part only when parts names
// that part.
func Read(dir string, parts ...string) (Manifest, error) {
	return Load(filepath.Join(dir, "manifest.json"), filepath.Join(dir, "programs"), parts...)
}

// manifestFile is the shape of manifest.json. A program row is one of two
// kinds:
//   - a release, downloaded from url and pinned by its file's sha256
//   - a mainnet program, read from its ProgramData and pinned by its
//     executable hash
//
// An account row is copied from mainnet and pinned by its data's sha256, or
// it holds its data. The deploy slot and the mainnet slot record where a pin
// was read. Mainnet serves only the current bytes, so the loader checks the
// hash, not the slot. A row with a part is seeded only on a network that
// turns the part on.
type manifestFile struct {
	Endpoint string `json:"endpoint"`
	Programs []struct {
		Name    string `json:"name"`
		Address string `json:"address"`
		Loader  string `json:"loader"`
		Part    string `json:"part"`
		SHA256  string `json:"sha256"`
		URL     string `json:"url"`
		Mainnet *struct {
			ProgramData      string `json:"programdata"`
			ExecutableSHA256 string `json:"executable_sha256"`
			DeploySlot       uint64 `json:"deploy_slot"`
		} `json:"mainnet"`
	} `json:"programs"`
	Accounts []struct {
		Name    string `json:"name"`
		Address string `json:"address"`
		Owner   string `json:"owner"`
		Part    string `json:"part"`
		Data    []byte `json:"data"`
		Mainnet *struct {
			DataSHA256 string `json:"data_sha256"`
			Slot       uint64 `json:"slot"`
		} `json:"mainnet"`
	} `json:"accounts"`
}

// Load reads a manifest and loads each row from cache. It fetches a row first
// when the cached file is missing or does not match the pin, so the cache
// holds only pinned bytes. A program is cache/NAME.so, and a copied account is
// cache/NAME.account. genesis/manifest.json and programs/manifest.json share
// this shape. Load does not fetch or return a row whose part is not in parts.
func Load(manifest, cache string, parts ...string) (Manifest, error) {
	return LoadFrom(manifest, cache, "", parts...)
}

// LoadFrom is Load, but it reads the mainnet rows through endpoint in place
// of the manifest's endpoint when endpoint is not empty. The pins fix the
// bytes, so any mainnet RPC serves. `make fetch-programs` passes the RPC that
// SVM_MAINNET_RPC names, because the manifest's RPC refuses GitHub's runners.
// No other caller passes one, so a shell's variable reaches no test.
func LoadFrom(manifest, cache, endpoint string, parts ...string) (Manifest, error) {
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return Manifest{}, err
	}
	var m manifestFile
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", manifest, err)
	}
	if endpoint != "" {
		m.Endpoint = endpoint
	}
	var out Manifest
	for _, p := range m.Programs {
		if p.Part != "" && !slices.Contains(parts, p.Part) {
			continue
		}
		path := filepath.Join(cache, p.Name+".so")
		var elf []byte
		if p.Mainnet != nil {
			elf, err = fetchProgram(path, m.Endpoint, p.Mainnet.ProgramData, p.Mainnet.ExecutableSHA256, p.Mainnet.DeploySlot)
		} else {
			elf, err = fetch(path, p.URL, p.SHA256)
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("program %s: %w", p.Name, err)
		}
		out.Programs = append(out.Programs, Program{Name: p.Name, Address: p.Address, Loader: p.Loader, SHA256: sum(elf), ELF: elf})
	}
	for _, a := range m.Accounts {
		if a.Part != "" && !slices.Contains(parts, a.Part) {
			continue
		}
		data := a.Data
		if a.Mainnet != nil {
			if data, err = fetchAccount(filepath.Join(cache, a.Name+".account"), m.Endpoint, a.Address, a.Owner, a.Mainnet.DataSHA256); err != nil {
				return Manifest{}, fmt.Errorf("account %s: %w", a.Name, err)
			}
		}
		out.Accounts = append(out.Accounts, Account{Name: a.Name, Address: a.Address, Owner: a.Owner, Data: data})
	}
	return out, nil
}

// fetch returns the file at path when it holds the pinned bytes, and
// otherwise downloads url, checks it against the pin and keeps it at path.
func fetch(path, url, pin string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && sum(b) == pin {
		return b, nil
	}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxProgram))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	if got := sum(b); got != pin {
		return nil, fmt.Errorf("%s has sha256 %s, and the manifest pins %s", url, got, pin)
	}
	return b, keep(path, b)
}

// fetchProgram returns the program at path when its executable hash matches
// the pin. Otherwise it reads the ProgramData from mainnet, checks it and
// keeps the ELF at path. The executable hash is the sha256 of the ELF without
// its trailing zeros, the hash that solana-verify reports. fetchProgram keeps
// the ELF that mainnet holds, padding included, because mainnet runs that
// program.
func fetchProgram(path, endpoint, programData, pin string, slot uint64) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && executableHash(b) == pin {
		return b, nil
	}
	owner, data, err := mainnetAccount(endpoint, programData)
	if err != nil {
		return nil, err
	}
	// UpgradeableLoaderState::ProgramData is a u32 tag of 3, the deploy slot
	// and an Option<Pubkey> authority, 45 bytes before the ELF. The layout is
	// that of solana-loader-v3-interface.
	if owner != loaderV3 || len(data) < 45 || binary.LittleEndian.Uint32(data) != 3 {
		return nil, fmt.Errorf("mainnet's %s is not a loader v3 ProgramData", programData)
	}
	elf := data[45:]
	if got := executableHash(elf); got != pin {
		return nil, fmt.Errorf("mainnet's ProgramData %s, deployed at slot %d, has executable sha256 %s, and the manifest pins %s, deployed at slot %d",
			programData, binary.LittleEndian.Uint64(data[4:12]), got, pin, slot)
	}
	return elf, keep(path, elf)
}

// fetchAccount returns the account data at path when its sha256 is the pin,
// and otherwise reads the account from mainnet, checks its owner and data and
// keeps the data at path.
func fetchAccount(path, endpoint, address, owner, pin string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && sum(b) == pin {
		return b, nil
	}
	got, data, err := mainnetAccount(endpoint, address)
	if err != nil {
		return nil, err
	}
	if got != owner {
		return nil, fmt.Errorf("mainnet's %s is owned by %s, and the manifest names %s", address, got, owner)
	}
	if s := sum(data); s != pin {
		return nil, fmt.Errorf("mainnet's %s has data sha256 %s, and the manifest pins %s", address, s, pin)
	}
	return data, keep(path, data)
}

// mainnetAccount reads an account's owner and data with getAccountInfo.
func mainnetAccount(endpoint, address string) (string, []byte, error) {
	req, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{address, map[string]string{"encoding": "base64", "commitment": "finalized"}},
	}, json.Deterministic(true))
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(req))
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("%s: getAccountInfo %s: %s", endpoint, address, resp.Status)
	}
	var r struct {
		Result *struct {
			Value *struct {
				Owner string    `json:"owner"`
				Data  [2]string `json:"data"`
			} `json:"value"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, 2*maxProgram), &r); err != nil {
		return "", nil, fmt.Errorf("%s: getAccountInfo %s: %w", endpoint, address, err)
	}
	if r.Error != nil {
		return "", nil, fmt.Errorf("%s: getAccountInfo %s: %s", endpoint, address, r.Error.Message)
	}
	if r.Result == nil || r.Result.Value == nil {
		return "", nil, fmt.Errorf("%s has no account %s", endpoint, address)
	}
	data, err := base64.StdEncoding.DecodeString(r.Result.Value.Data[0])
	if err != nil {
		return "", nil, fmt.Errorf("%s: getAccountInfo %s: %w", endpoint, address, err)
	}
	return r.Result.Value.Owner, data, nil
}

// keep writes b to path under another name and renames it into place, so a
// process reading path never sees half a file.
func keep(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// executableHash is solana-verify's hash of a program, the sha256 of its ELF
// without trailing zeros.
func executableHash(elf []byte) string { return sum(bytes.TrimRight(elf, "\x00")) }
