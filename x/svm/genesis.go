package svm

import (
	"bytes"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"slices"

	"github.com/mr-tron/base58"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
)

// GenesisAccount is solana-account's field set verbatim.
type GenesisAccount struct {
	Pubkey     string `json:"pubkey"`
	Lamports   uint64 `json:"lamports"`
	Owner      string `json:"owner"`
	Data       []byte `json:"data"`
	Executable bool   `json:"executable"`
	RentEpoch  uint64 `json:"rent_epoch"`
}

// GenesisState is x/svm's half of the genesis file.
type GenesisState struct {
	Accounts []GenesisAccount `json:"accounts"`
	Programs []GenesisProgram `json:"programs"`
	// Gates are the SVMFeatureSet gates active at genesis, written to svmmeta
	// as active/<id> -> 0.
	Gates []string `json:"gates"`
	// EngineDigest is the engine this chain adopts, in lower-case hex. Genesis
	// carries the result, and the release carries the inputs to reproduce it.
	EngineDigest string `json:"engine_digest"`
	// Network is "production" or "devnet". DefaultGenesis writes the compiled
	// constant, as it writes the compiled digest, so one binary cannot make a
	// mismatched genesis. A mismatch takes two binaries.
	Network string `json:"network"`
	// EphemeralValidators are the ephemeral rollup validators that the network
	// admits, each with a fees vault that genesis writes. They turn on the
	// ephemeral-rollup part of the manifest.
	EphemeralValidators []string `json:"ephemeral_validators,omitempty"`
	// VRFOracles are the VRF oracles that the network admits, each in the
	// oracle list that genesis writes. They turn on the vrf part of the
	// manifest.
	VRFOracles []VRFOracle `json:"vrf_oracles,omitempty"`
}

// checkEngine refuses a genesis file that names an engine this binary is not.
// The check applies from block 1, not from the first upgrade, so it covers the
// whole life of a chain that never upgrades.
func (g GenesisState) checkEngine() error {
	compiled := ffi.Digest()
	if g.EngineDigest != hex.EncodeToString(compiled[:]) {
		return fmt.Errorf("svm genesis: engine digest %q, this binary is %x", g.EngineDigest, compiled)
	}
	return nil
}

// checkNetwork refuses a genesis that belongs to the other network. It has the
// shape and place of checkEngine, for the same reason. The check applies from
// block 1, not from the first upgrade.
func (g GenesisState) checkNetwork() error {
	if g.Network != Network {
		return fmt.Errorf("svm genesis: network %q, this binary is %q", g.Network, Network)
	}
	return nil
}

// builtinAccounts are the six builtin program accounts, copied from mainnet:
// 1 lamport, NativeLoader, executable, the builtin's name as data. Their names
// are the engine's builtin table.
func builtinAccounts() []GenesisAccount {
	rows := []struct{ id, name string }{
		{"11111111111111111111111111111111", "solana_system_program"},
		{"BPFLoader1111111111111111111111111111111111", "solana_bpf_loader_program"},
		{"BPFLoader2111111111111111111111111111111111", "solana_bpf_loader_program"},
		{"BPFLoaderUpgradeab1e11111111111111111111111", "solana_bpf_loader_upgradeable_program"},
		{"ComputeBudget111111111111111111111111111111", "compute_budget_program"},
		{"ZkE1Gama1Proof11111111111111111111111111111", "zk_elgamal_proof_program"},
	}
	out := make([]GenesisAccount, len(rows))
	for i, r := range rows {
		out[i] = GenesisAccount{
			Pubkey:     r.id,
			Lamports:   1,
			Owner:      base58.Encode(NativeLoader[:]),
			Data:       []byte(r.name),
			Executable: true,
			RentEpoch:  RentExemptEpoch,
		}
	}
	return out
}

// precompileAccounts are the three precompile program accounts, copied from
// mainnet and not normalized, on purpose. load_transaction_accounts requires an
// account at every program id, with no precompile exception. Without these
// rows a precompile transaction fails at load with ProgramAccountNotFound. It
// never reaches the dispatch in the engine's callback.
//
// secp256r1 differs from the other two in data and rent epoch. Agave writes it
// through Bank::add_precompiled_account_with_owner, which sets empty data and
// takes the rent epoch from inherit_specially_retained_account_fields. Its miss
// case is INITIAL_RENT_EPOCH, which is zero. The other two carry their program
// names and a rent epoch of u64::MAX, so they predate that path. getAccountInfo
// on these addresses is client-visible, so this code copies the difference.
func precompileAccounts() []GenesisAccount {
	rows := []struct {
		id        [32]byte
		name      string
		rentEpoch uint64
	}{
		{Secp256k1Program, "secp256k1_program", RentExemptEpoch},
		{Ed25519Program, "ed25519_program", RentExemptEpoch},
		{Secp256r1Program, "", 0},
	}
	out := make([]GenesisAccount, len(rows))
	for i, r := range rows {
		out[i] = GenesisAccount{
			Pubkey:     base58.Encode(r.id[:]),
			Lamports:   1,
			Owner:      base58.Encode(NativeLoader[:]),
			Data:       []byte(r.name),
			Executable: true,
			RentEpoch:  r.rentEpoch,
		}
	}
	return out
}

// DefaultGenesis returns the accounts every chain needs: the builtins, the
// precompiles, the sysvars, and the feature gate set pinned to mainnet. The
// faucet is a genesis file value.
func DefaultGenesis() GenesisState {
	features, gates := genesisFeatures()
	accounts := append(builtinAccounts(), precompileAccounts()...)
	accounts = append(accounts, genesisSysvars()...)
	digest := ffi.Digest()
	return GenesisState{
		Accounts:     append(accounts, features...),
		Gates:        gates,
		EngineDigest: hex.EncodeToString(digest[:]),
		Network:      Network,
	}
}

type decoded struct {
	pubkey [32]byte
	value  []byte
}

// decode parses, sorts by raw pubkey, and checks the genesis rules. This path
// iterates no map.
func (g GenesisState) decode() ([]decoded, error) {
	accounts := g.Accounts
	for _, p := range g.Programs {
		expanded, err := ProgramAccounts(p, 0)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts[:len(accounts):len(accounts)], expanded...)
	}
	out := make([]decoded, 0, len(accounts))
	for _, a := range accounts {
		pk, err := base58.Decode(a.Pubkey)
		if err != nil || len(pk) != 32 {
			return nil, fmt.Errorf("svm genesis: bad pubkey %q", a.Pubkey)
		}
		owner, err := base58.Decode(a.Owner)
		if err != nil || len(owner) != 32 {
			return nil, fmt.Errorf("svm genesis: %s has bad owner %q", a.Pubkey, a.Owner)
		}
		if err := checkSysvar([32]byte(pk), a.Data); err != nil {
			return nil, err
		}
		if err := checkChainSysvar([32]byte(pk), a.Data); err != nil {
			return nil, err
		}
		// Builtin accounts hold 1 lamport, as on mainnet. They are reserved
		// keys, never writable, so rent never applies to them.
		if [32]byte(owner) != NativeLoader && a.Lamports < MinimumBalance(len(a.Data)) {
			return nil, fmt.Errorf("svm genesis: %s holds %d lamports, below its rent-exempt minimum %d",
				a.Pubkey, a.Lamports, MinimumBalance(len(a.Data)))
		}
		if a.Lamports == 0 {
			return nil, fmt.Errorf("svm genesis: %s holds 0 lamports", a.Pubkey)
		}
		o := [32]byte(owner)
		out = append(out, decoded{[32]byte(pk), ffi.EncodeAccount(&o, a.Lamports, a.RentEpoch, a.Executable, a.Data)})
	}
	slices.SortFunc(out, func(a, b decoded) int { return bytes.Compare(a.pubkey[:], b.pubkey[:]) })
	for i := 1; i < len(out); i++ {
		if out[i].pubkey == out[i-1].pubkey {
			return nil, fmt.Errorf("svm genesis: duplicate pubkey %s", base58.Encode(out[i].pubkey[:]))
		}
	}
	return out, checkGates(g.Gates, out)
}

// SeedPrograms adds programs after genesis, for an upgrade handler. It uses the
// same manifest writer as InitGenesis, deployed at the upgrade height, so the
// programs are visible from the next block. It refuses to overwrite an
// existing account.
func (s Stores) SeedPrograms(ms storetypes.MultiStore, programs []GenesisProgram, height uint64) error {
	var accounts []GenesisAccount
	for _, p := range programs {
		expanded, err := ProgramAccounts(p, height)
		if err != nil {
			return err
		}
		accounts = append(accounts, expanded...)
	}
	decoded, err := GenesisState{Accounts: accounts}.decode()
	if err != nil {
		return err
	}
	for _, a := range decoded {
		if s.GetAccount(ms, a.pubkey) != nil {
			return fmt.Errorf("svm: program account %s already exists", base58.Encode(a.pubkey[:]))
		}
	}
	for _, a := range decoded {
		s.SetAccount(ms, a.pubkey, a.value)
		s.mint(ffi.Lamports(a.value))
	}
	return nil
}

// Validate applies every genesis rule without writing anything.
func (g GenesisState) Validate() error {
	if _, err := g.decode(); err != nil {
		return err
	}
	if err := g.checkEngine(); err != nil {
		return err
	}
	if err := g.checkEphemeral(); err != nil {
		return err
	}
	if err := g.checkVRF(); err != nil {
		return err
	}
	return g.checkNetwork()
}

// InitGenesis writes accounts in ascending raw pubkey order. Each shard is a
// contiguous run of that order, so every store receives ascending keys.
func (s Stores) InitGenesis(ms storetypes.MultiStore, raw jsontext.Value) error {
	var g GenesisState
	if err := json.Unmarshal(raw, &g); err != nil {
		return fmt.Errorf("svm genesis: %w", err)
	}
	accounts, err := g.decode()
	if err != nil {
		return err
	}
	if err := g.checkEngine(); err != nil {
		return err
	}
	if err := g.checkNetwork(); err != nil {
		return err
	}
	if err := g.checkEphemeral(); err != nil {
		return err
	}
	if err := g.checkVRF(); err != nil {
		return err
	}
	s.AdoptEngine(ms)
	s.AdoptNetwork(ms)
	for _, a := range accounts {
		s.SetAccount(ms, a.pubkey, a.value)
	}
	for _, id := range g.Gates {
		k, _ := decodeKey(id) // checked by decode
		s.activate(ms, k, 0)
	}
	return nil
}

// ExportGenesis iterates the shards in order, which is ascending raw pubkey.
func (s Stores) ExportGenesis(ms storetypes.MultiStore) (GenesisState, error) {
	g := GenesisState{EngineDigest: hex.EncodeToString(s.EngineDigest(ms)), Network: s.ChainNetwork(ms)}
	for _, key := range s.Accounts {
		it := ms.GetKVStore(key).Iterator(nil, nil)
		for ; it.Valid(); it.Next() {
			a, err := ffi.DecodeAccount(it.Value())
			if err != nil {
				it.Close()
				return g, err
			}
			g.Accounts = append(g.Accounts, GenesisAccount{
				Pubkey:     base58.Encode(it.Key()),
				Lamports:   a.Lamports,
				Owner:      base58.Encode(a.Owner[:]),
				Data:       append([]byte(nil), a.Data...),
				Executable: a.Executable,
				RentEpoch:  a.RentEpoch,
			})
		}
		it.Close()
	}
	return g, nil
}
