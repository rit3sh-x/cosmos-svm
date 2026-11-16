package app_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mr-tron/base58"

	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	upgradetypes "github.com/cosmos/cosmos-sdk/x/upgrade/types"

	"github.com/rit3sh-x/cosmos-svm/genesis"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// token2022 is the manifest's Token-2022 row.
func token2022(t *testing.T) svm.GenesisProgram {
	for _, p := range tc.ManifestPrograms(t) {
		if p.Address == "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb" {
			return p
		}
	}
	t.Fatal("no Token-2022 row")
	return svm.GenesisProgram{}
}

// TestGenesisLoaderV3ProgramRuns proves that a loader v3 program seeded at
// genesis runs. Its ProgramData account is not a transaction key. Go loads it
// through the pointer in the Program account.
func TestGenesisLoaderV3ProgramRuns(t *testing.T) {
	faucet := tc.Key(1)
	program := token2022(t)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Programs: []svm.GenesisProgram{program}})
	c.Block()

	token := key(t, program.Address)
	mint := tc.Key(7)
	res, _ := c.Block(initializeMint(t, c, c.Latest(), faucet, mint, token))
	if res[0].Code != 0 {
		t.Fatalf("InitializeMint2 on genesis Token-2022: code %d", res[0].Code)
	}
	if a, _ := c.Account(tc.Pub(mint)); a.Owner != token || a.Data[45] != 1 {
		t.Fatalf("mint not initialized: owner %x, is_initialized %d", a.Owner, a.Data[45])
	}
}

// TestGenesisRejectsAlteredELF proves that a program whose ELF does not match
// its pinned sha256 fails genesis validation.
func TestGenesisRejectsAlteredELF(t *testing.T) {
	program := token2022(t)
	program.ELF = append([]byte(nil), program.ELF...)
	program.ELF[1000] ^= 1
	g := svm.DefaultGenesis()
	g.Programs = []svm.GenesisProgram{program}
	if err := g.Validate(); err == nil {
		t.Fatal("genesis with one flipped ELF byte validated")
	}
}

func key(t *testing.T, s string) [32]byte {
	b, err := base58.Decode(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("bad key %s", s)
	}
	return [32]byte(b)
}

// initializeMint creates a mint account owned by token and initializes it, as
// spl-token create-token does first.
func initializeMint(t *testing.T, c *tc.Chain, blockhash [32]byte, payer, mint ed25519.PrivateKey, token [32]byte) []byte {
	create := binary.LittleEndian.AppendUint32(nil, 0) // SystemInstruction::CreateAccount
	create = binary.LittleEndian.AppendUint64(create, svm.MinimumBalance(82))
	create = binary.LittleEndian.AppendUint64(create, 82)
	create = append(create, token[:]...)
	authority := tc.Pub(payer)
	initialize := append([]byte{20, 6}, authority[:]...) // InitializeMint2, 6 decimals
	initialize = append(initialize, 0)                   // no freeze authority
	return tc.Tx(blockhash, payer, []ed25519.PrivateKey{mint},
		tc.Instruction{Program: svm.SystemProgram, Accounts: []tc.Meta{tc.Signer(tc.Pub(payer), true), tc.Signer(tc.Pub(mint), true)}, Data: create},
		tc.Instruction{Program: token, Accounts: []tc.Meta{tc.Writable(tc.Pub(mint))}, Data: initialize},
	)
}

func memo(program [32]byte, text string) tc.Instruction {
	return tc.Instruction{Program: program, Data: []byte(text)}
}

const (
	splToken = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	memoV1   = "Memo1UhkJRfHyvLMcVucJwxXeuD728EqVDDwQDxFMNo"
	memoV3   = "MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr"
	memoV4   = "Memo4c2pN8afCj432Lb7RMVKi9PbQnnW7ewFFaV3oAH"
)

// TestManifestProgramsRunFromBlockOne proves that the manifest programs are
// callable from block 1. SPL Token initializes a mint in block 1, and the
// three Memo programs run in the blocks after it.
func TestManifestProgramsRunFromBlockOne(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Programs: tc.ManifestPrograms(t)})
	res, _ := c.Block(initializeMint(t, c, tc.HashAt(1), faucet, tc.Key(7), key(t, splToken)))
	if res[0].Code != 0 {
		t.Fatalf("SPL Token InitializeMint2 in block 1: code %d", res[0].Code)
	}
	for _, m := range []string{memoV1, memoV3, memoV4} {
		res, _ = c.Block(tc.Tx(c.Latest(), faucet, nil, memo(key(t, m), "memo from "+m)))
		if res[0].Code != 0 {
			t.Fatalf("memo program %s: code %d", m, res[0].Code)
		}
	}
	// Genesis seeds the published v1.0.0 build of Memo v1 under loader v2, the
	// loader that build targets. Mainnet holds an older build under loader v1.
	if a, _ := c.Account(key(t, memoV1)); a.Owner != svm.LoaderV2 || !a.Executable {
		t.Fatalf("Memo v1 owner %s, want BPFLoader2111...", base58.Encode(a.Owner[:]))
	}
}

// TestAMainnetProgramRowRunsFromBlockOne proves that a program row that names
// a mainnet program runs from block 1 under loader v3. genesis.Load reads its
// ProgramData, checks its executable hash and keeps mainnet's padding. Here a
// stand-in for mainnet serves the Memo v3 build for a program at a new address.
func TestAMainnetProgramRowRunsFromBlockOne(t *testing.T) {
	var elf []byte
	for _, p := range tc.ManifestPrograms(t) {
		if p.Address == memoV3 {
			elf = p.ELF
		}
	}
	data := binary.LittleEndian.AppendUint32(nil, 3)
	data = binary.LittleEndian.AppendUint64(data, 456_483_072)
	data = append(append(data, make([]byte, 33)...), elf...)
	data = append(data, make([]byte, 64)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.MarshalWrite(w, map[string]any{"result": map[string]any{"value": map[string]any{
			"owner": base58.Encode(svm.LoaderV3[:]), "data": []string{base64.StdEncoding.EncodeToString(data), "base64"},
		}}}, json.Deterministic(true))
	}))
	defer srv.Close()
	address := tc.Pub(tc.Key(9))
	executable := sha256.Sum256(bytes.TrimRight(elf, "\x00"))
	manifest := filepath.Join(t.TempDir(), "manifest.json")
	os.WriteFile(manifest, []byte(fmt.Sprintf(`{"endpoint": %q, "programs": [{"name": "memo", "address": %q,
		"loader": "BPFLoaderUpgradeab1e11111111111111111111111", "mainnet": {"programdata": "PData",
		"executable_sha256": "%x", "deploy_slot": 456483072}}]}`, srv.URL, base58.Encode(address[:]), executable)), 0o644)
	m, err := genesis.Load(manifest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := m.Programs[0]
	if len(p.ELF) != len(elf)+64 {
		t.Fatalf("the row's ELF is %d bytes, want mainnet's %d with its padding", len(p.ELF), len(elf)+64)
	}

	faucet := tc.Key(1)
	programs := append(tc.ManifestPrograms(t), svm.GenesisProgram{Address: p.Address, Loader: p.Loader, SHA256: p.SHA256, ELF: p.ELF})
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Programs: programs})
	res, _ := c.Block(tc.Tx(tc.HashAt(1), faucet, nil, memo(address, "a memo from a mainnet row")))
	if res[0].Code != 0 {
		t.Fatalf("the mainnet row's program in block 1: code %d", res[0].Code)
	}
	if a, _ := c.Account(address); a.Owner != svm.LoaderV3 || !a.Executable {
		t.Fatalf("the program's owner is %s, want loader v3", base58.Encode(a.Owner[:]))
	}
}

// TestGenesisOrderDoesNotChangeTheRoot proves that the root does not depend on
// the order of accounts[] or programs[], and that a duplicate pubkey is an
// error.
func TestGenesisOrderDoesNotChangeTheRoot(t *testing.T) {
	programs := tc.ManifestPrograms(t)
	forward := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Programs: programs})
	reversed := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Programs: programs, Edit: func(g *svm.GenesisState) {
		slices.Reverse(g.Accounts)
		slices.Reverse(g.Programs)
	}})
	if string(forward.GenesisHash) != string(reversed.GenesisHash) {
		t.Fatalf("genesis root %x forward, %x reversed", forward.GenesisHash, reversed.GenesisHash)
	}
	g := svm.DefaultGenesis()
	g.Programs = programs
	g.Accounts = append(g.Accounts, g.Accounts[0])
	if err := g.Validate(); err == nil {
		t.Fatal("genesis with a duplicate pubkey validated")
	}
}

// TestUpgradeHandlerSeedsAProgram proves that an upgrade handler can add a
// program after genesis through the manifest writer. A program deployed at
// the upgrade height is callable from the next block.
func TestUpgradeHandlerSeedsAProgram(t *testing.T) {
	var memo4 svm.GenesisProgram
	var genesisPrograms []svm.GenesisProgram
	for _, p := range tc.ManifestPrograms(t) {
		if p.Address == memoV4 {
			memo4 = p
		} else {
			genesisPrograms = append(genesisPrograms, p)
		}
	}
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Programs: genesisPrograms})
	c.Block()
	const upgradeHeight = 3
	if err := c.App.Upgrades.ScheduleUpgrade(c.App.NewUncachedContext(false, cmtproto.Header{Height: c.Height}),
		upgradetypes.Plan{Name: "seed-memo-v4", Height: upgradeHeight}); err != nil {
		t.Fatal(err)
	}
	call := func(i int) uint32 {
		res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, memo(key(t, memoV4), fmt.Sprintf("call %d", i))))
		return res[0].Code
	}
	if code := call(2); code == 0 {
		t.Fatalf("Memo v4 ran at height %d, before the upgrade", c.Height)
	}

	// The old binary halts at the upgrade height. The new one carries the handler.
	c.Restart()
	c.App.Upgrades.SetUpgradeHandler("seed-memo-v4", func(ctx context.Context, plan upgradetypes.Plan, vm module.VersionMap) (module.VersionMap, error) {
		return vm, c.App.Stores.SeedPrograms(sdk.UnwrapSDKContext(ctx).MultiStore(), []svm.GenesisProgram{memo4}, uint64(plan.Height))
	})
	if code := call(3); code == 0 {
		t.Fatalf("Memo v4 ran at the upgrade height %d, before it is visible", c.Height)
	}
	if code := call(4); code != 0 {
		t.Fatalf("Memo v4 at height %d, after the upgrade: code %d", c.Height, code)
	}
}

// TestSvmmetaGatesAgreeWithFeatureAccounts proves that after genesis each gate
// in svmmeta has a feature account that says active since 0, and that svmmeta
// holds exactly the genesis gates.
func TestSvmmetaGatesAgreeWithFeatureAccounts(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()
	gates, err := c.App.Stores.ActiveGates(c.App.CommitMultiStore())
	if err != nil {
		t.Fatal(err)
	}
	if len(gates) != len(svm.DefaultGenesis().Gates) {
		t.Fatalf("svmmeta holds %d gates, genesis names %d", len(gates), len(svm.DefaultGenesis().Gates))
	}
	gatesAgreeWithAccounts(t, c)
}

// gatesAgreeWithAccounts checks that each gate that svmmeta calls active has a
// feature account that says active since exactly that height.
func gatesAgreeWithAccounts(t *testing.T, c *tc.Chain) {
	t.Helper()
	gates, err := c.App.Stores.ActiveGates(c.App.CommitMultiStore())
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range gates {
		a, ok := c.Account(g.ID)
		if !ok || a.Owner != svm.FeatureProgram || len(a.Data) != 9 || a.Data[0] != 1 ||
			binary.LittleEndian.Uint64(a.Data[1:9]) != g.Height {
			t.Fatalf("gate %s: account %+v disagrees with activation height %d", base58.Encode(g.ID[:]), a, g.Height)
		}
	}
}

func base58Of(k [32]byte) string { return base58.Encode(k[:]) }
