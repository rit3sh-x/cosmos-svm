package svm

import (
	"encoding/binary"
	json "encoding/json/v2"
	"fmt"
	"slices"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/genesis"
)

// FeatureProgram owns every feature account.
var FeatureProgram = mustKey("Feature111111111111111111111111111111111111")

// featureLen is bincode Feature { activated_at: Option<u64> }.
const featureLen = 9

// featureData is a feature account's data saying activated_at Some(height).
func featureData(height uint64) []byte {
	return append([]byte{1}, binary.LittleEndian.AppendUint64(nil, height)...)
}

// activatedAt is the height a feature account's data says it activated at,
// false for None or for data of another length.
func activatedAt(data []byte) (uint64, bool) {
	if len(data) != featureLen || data[0] != 1 {
		return 0, false
	}
	return binary.LittleEndian.Uint64(data[1:9]), true
}

// activePrefix keys an active gate in svmmeta: active/<pubkey> -> activation
// height, u64 big-endian.
var activePrefix = []byte("active/")

func activeKey(id [32]byte) []byte { return append(append([]byte(nil), activePrefix...), id[:]...) }

type pinnedGate struct {
	ID          string `json:"id"`
	Class       string `json:"class"`
	ActiveAtPin bool   `json:"active_at_pin"`
}

// GenesisExceptions are the gates that genesis activates although the pin has
// them inactive. The list lives in genesis/exceptions.json, which is written
// and reviewed by hand. The genesis builder, the engine's differential and the
// drift report all read this one file. The list is not in
// genesis/features.json, because that file is a machine-written read of
// mainnet and must stay exact.
//
// The list holds SIMD-0392 (relax_post_exec_min_balance_check), the two rent
// cuts that mainnet took after the pin (SIMD-0437-1 and -2), and SIMD-0385
// (enable_tx_v1). The file gives the reason for each.
func GenesisExceptions() []string {
	var file struct {
		Gates []struct {
			ID string `json:"id"`
		} `json:"gates"`
	}
	if err := json.Unmarshal(genesis.Exceptions, &file); err != nil {
		panic(fmt.Sprintf("svm: genesis/exceptions.json: %v", err))
	}
	out := make([]string, len(file.Gates))
	for i, g := range file.Gates {
		out[i] = g.ID
	}
	return out
}

func genesisFeatures() ([]GenesisAccount, []string) {
	var pinned struct {
		Gates []pinnedGate `json:"gates"`
	}
	if err := json.Unmarshal(genesis.Features, &pinned); err != nil {
		panic(fmt.Sprintf("svm: genesis/features.json: %v", err))
	}
	exceptions := GenesisExceptions()
	var accounts []GenesisAccount
	var gates []string
	for _, g := range pinned.Gates {
		exception := slices.Contains(exceptions, g.ID)
		if exception && g.Class == "bank" && !slices.Contains(portedBankGates(), g.ID) {
			panic(fmt.Sprintf("svm: genesis/exceptions.json names %s, a bank-side gate this chain does not port", g.ID))
		}
		if !exception && (!g.ActiveAtPin || g.Class == "bank") {
			continue
		}
		accounts = append(accounts, rentExempt(mustKey(g.ID), FeatureProgram, false, featureData(0)))
		if g.Class == "svm" || g.Class == "bank" {
			gates = append(gates, g.ID)
		}
	}
	return accounts, gates
}

// checkGates checks that every gate svmmeta will call active has a feature
// account that says active since block 0.
func checkGates(gates []string, accounts []decoded) error {
	byKey := make(map[[32]byte][]byte, len(accounts))
	for _, a := range accounts {
		byKey[a.pubkey] = a.value
	}
	for _, id := range gates {
		k, err := decodeKey(id)
		if err != nil {
			return fmt.Errorf("svm genesis gate: %w", err)
		}
		v, ok := byKey[k]
		if !ok {
			return fmt.Errorf("svm genesis gate %s has no feature account", id)
		}
		a, err := ffi.DecodeAccount(v)
		at, active := activatedAt(a.Data)
		if err != nil || a.Owner != FeatureProgram || !active || at != 0 {
			return fmt.Errorf("svm genesis gate %s: its account does not say active since 0", id)
		}
	}
	return nil
}

func (s Stores) activate(ms storetypes.MultiStore, id [32]byte, height uint64) {
	ms.GetKVStore(s.Meta).Set(activeKey(id), binary.BigEndian.AppendUint64(nil, height))
}

// ActiveGates reads the active/ keys from svmmeta, for configuring the engine.
func (s Stores) ActiveGates(ms storetypes.MultiStore) ([]ffi.Gate, error) {
	it := ms.GetKVStore(s.Meta).Iterator(activePrefix, prefixEnd(activePrefix))
	defer it.Close()
	var gates []ffi.Gate
	for ; it.Valid(); it.Next() {
		k, v := it.Key(), it.Value()
		if len(k) != len(activePrefix)+32 || len(v) != 8 {
			return nil, fmt.Errorf("svm: malformed svmmeta gate %x", k)
		}
		gates = append(gates, ffi.Gate{ID: [32]byte(k[len(activePrefix):]), Height: binary.BigEndian.Uint64(v)})
	}
	return gates, nil
}
