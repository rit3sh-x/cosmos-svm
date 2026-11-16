package svm

import (
	"encoding/binary"
	json "encoding/json/v2"
	"fmt"
	"slices"

	"github.com/mr-tron/base58"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/genesis"
)

// MinActivationDelay is how far ahead a scheduled activation must be, in
// blocks. It is two days of voting period at the shipped timeout_commit of
// 400 ms (172,800 s / 0.4 s). So an ordinary cancel proposal can still pass
// before the flip. It is also one mainnet epoch of slots. A block is never
// shorter than timeout_commit, so this is at least two days at the shipped
// configuration.
const MinActivationDelay = 432_000

// pendingPrefix keys a scheduled activation in svmmeta:
//
//	pending/<H, u64 big-endian><feature pubkey> -> empty
//
// The height comes first, so the begin-block step is a prefix scan up to the
// current height, not a scan of everything pending.
var pendingPrefix = []byte("pending/")

// rentGates are the rent repricing gates, each with the lamports-per-byte value
// that it writes. `the_ported_gate_ids_are_the_crates` in engine/src/runtime.rs
// checks the values and the order against the crate, so this table cannot
// drift from the engine's own feature set.
var rentGates = []struct {
	id      string
	perByte uint64
}{
	{"4a6f7o7iTcA8hRDCrPLkSatnt5Ykxiu36wo5p1Tt12wC", 6_333},
	{"61BtM7BkDEE8Yq5fskEVAQT9mYA8qCejJWoLe5apqg81", 5_080},
	{"rntCigrTppP5JdZz7K8TyN9sMzLdAcXp8SejYpVpX6D", 2_575},
	{"rntD7invRBswCAdKtRsh1G4psKjrPdS3BKqtnA78C7N", 1_322},
	{"rntTjNZ9boq8owDxjGVFHPfWNQPDaKiM5JcjxmDGg47", 696},
	// SIMD-0438's safeguard, applied last so it overrides a cut that activates
	// in the same block.
	{"rnt8ZQpz2HYhX3DkYBDGjJS1a36mYq69oXka7JrhEdi", 6_960},
}

// EnableTxV1 is the one non-rent bank-side gate this chain reads, in
// admission.
const EnableTxV1 = "txv1aq4pp281K9um3tnPgkfX8UqtFT6wcVW3hNezGLL"

// portedBankGates are the bank-side gates for which this chain carries a port
// of the read site, which makes them valid targets. Every other bank-side or
// cleaned-up gate is refused, because its activation would write an account
// that claims behavior this chain does not run.
func portedBankGates() []string {
	ported := []string{EnableTxV1}
	for _, g := range rentGates {
		ported = append(ported, g.id)
	}
	return ported
}

// svmGates returns every gate that the pinned set classes as an SVMFeatureSet
// field. `genesis/features.json` carries the classification, which Anza's own
// projection computes.
func svmGates() map[string]bool {
	var pinned struct {
		Gates []pinnedGate `json:"gates"`
	}
	if err := json.Unmarshal(genesis.Features, &pinned); err != nil {
		panic(fmt.Sprintf("svm: genesis/features.json: %v", err))
	}
	out := map[string]bool{}
	for _, g := range pinned.Gates {
		if g.Class == "svm" {
			out[g.ID] = true
		}
	}
	return out
}

// noReadSiteGates are svm gates with no read site in the crates this chain
// compiles. The set depends on the dependency list, so it is not a constant.
var noReadSiteGates = map[string]string{
	// These three read the leader's vote account, for example for the block
	// revenue collector. This chain has no vote account, because the validator
	// set is Cosmos staking.
	"3HcSrCTGXTUnrTueHi4DAwNuMxZSsm5xui2Ax3mgxHqf": "SIMD-0232 custom commission collector",
	"B1ockRevenueSharing111111111111111111111111":  "SIMD-0123 block revenue sharing",
	"VoteAccount1nitia1izeV211111111111111111111":  "SIMD-0464 vote account initialize v2",
	// No branch is left: MAX_TX_ACCOUNT_LOCKS is a plain constant in
	// solana-transaction at this pin, and no code reads the gate.
	"9LZdXeKGeBV6hRLdxS1rHbHoEUsKqesCC2ZAPTPKJAbK": "increase tx account lock limit",
}

// ValidTarget reports whether a gate can be scheduled at all. The answer
// depends on the gate, not on the chain's state.
func ValidTarget(id string) bool {
	if _, refused := noReadSiteGates[id]; refused {
		return false
	}
	return svmGates()[id] || slices.Contains(portedBankGates(), id)
}

// ScheduleGates records activations for height. It refuses a height that is
// too close, a gate that is not a valid target, and a gate that is already
// active or already scheduled. An active gate is never deactivated, so a second
// schedule for it can only mislead. A gate scheduled at two heights would
// activate at both. The second activation would move its recorded height and
// rewrite its rent over any cut activated between them.
func (s Stores) ScheduleGates(ms storetypes.MultiStore, now, height uint64, gates []string) error {
	if height < now+MinActivationDelay {
		return fmt.Errorf("svm: height %d is %d blocks away, the minimum is %d",
			height, height-min(height, now), uint64(MinActivationDelay))
	}
	store := ms.GetKVStore(s.Meta)
	for _, id := range gates {
		key, err := decodeKey(id)
		if err != nil {
			return fmt.Errorf("svm: gate %q: %w", id, err)
		}
		if !ValidTarget(id) {
			return fmt.Errorf("svm: gate %s is not a valid target: this chain does not read it", id)
		}
		if store.Has(activeKey(key)) {
			return fmt.Errorf("svm: gate %s is already active, and a gate is never deactivated", id)
		}
		if at, ok := s.scheduled(ms, key); ok {
			return fmt.Errorf("svm: gate %s is already scheduled at height %d; cancel that first", id, at)
		}
		store.Set(pendingKey(height, key), []byte{})
	}
	return nil
}

// scheduled reports the height a gate is pending at, if any. Pending entries
// are keyed by height first, so this scans every pending entry. There are as
// many as the activations that governance has scheduled.
func (s Stores) scheduled(ms storetypes.MultiStore, id [32]byte) (uint64, bool) {
	it := ms.GetKVStore(s.Meta).Iterator(pendingPrefix, prefixEnd(pendingPrefix))
	defer it.Close()
	for ; it.Valid(); it.Next() {
		if height, got, ok := splitPendingKey(it.Key()); ok && got == id {
			return height, true
		}
	}
	return 0, false
}

// CancelGates drops scheduled activations before their height. It refuses a
// gate that is not scheduled at height, and names the height it is scheduled
// at, if any. A cancel that drops nothing must fail, because governance would
// read it as an undone flip that still activates at its own height.
func (s Stores) CancelGates(ms storetypes.MultiStore, height uint64, gates []string) error {
	store := ms.GetKVStore(s.Meta)
	for _, id := range gates {
		key, err := decodeKey(id)
		if err != nil {
			return fmt.Errorf("svm: gate %q: %w", id, err)
		}
		if !store.Has(pendingKey(height, key)) {
			if at, ok := s.scheduled(ms, key); ok {
				return fmt.Errorf("svm: gate %s is scheduled at height %d, not %d", id, at, height)
			}
			return fmt.Errorf("svm: gate %s is not scheduled", id)
		}
		store.Delete(pendingKey(height, key))
	}
	return nil
}

// PendingGates are the activations scheduled for height, in key order.
func (s Stores) PendingGates(ms storetypes.MultiStore, height uint64) [][32]byte {
	it := ms.GetKVStore(s.Meta).Iterator(pendingAt(height), pendingAt(height+1))
	defer it.Close()
	var out [][32]byte
	for ; it.Valid(); it.Next() {
		if _, id, ok := splitPendingKey(it.Key()); ok {
			out = append(out, id)
		}
	}
	return out
}

// ActivateGates moves each gate from pending to active, writes its feature
// account with activated_at = height, and rewrites lamports_per_byte in the
// Rent sysvar for any rent gate among them. The caller then hands the engine
// the new set, before any transaction of the block runs. This keeps the
// feature set constant for a whole block, as parallel groups require.
//
// A gate that is already active is dropped from pending and left as it is, on
// either path. A second activation would move its recorded height and rewrite
// its rent.
//
// It returns whether anything changed, so a block with no flip pays nothing.
func (s Stores) ActivateGates(ms storetypes.MultiStore, height uint64, gates [][32]byte) (bool, error) {
	if len(gates) == 0 {
		return false, nil
	}
	store := ms.GetKVStore(s.Meta)
	flipped := map[string]bool{}
	for _, key := range gates {
		store.Delete(pendingKey(height, key))
		if store.Has(activeKey(key)) {
			continue
		}
		s.activate(ms, key, height)
		if err := s.writeFeatureAccount(ms, key, featureData(height)); err != nil {
			return false, err
		}
		flipped[base58.Encode(key[:])] = true
	}
	// Agave's order, with the safeguard last.
	for _, g := range rentGates {
		if flipped[g.id] {
			if err := s.setLamportsPerByte(ms, g.perByte); err != nil {
				return false, err
			}
		}
	}
	return len(flipped) > 0, nil
}

// writeFeatureAccount creates or rewrites a gate's feature account. A gate that
// was inactive at genesis has no account, so it creates one at the rent-exempt
// minimum. It leaves the lamports of an existing account as they are.
func (s Stores) writeFeatureAccount(ms storetypes.MultiStore, key [32]byte, data []byte) error {
	owner := FeatureProgram
	lamports := MinimumBalance(featureLen)
	if existing := s.GetAccount(ms, key); existing != nil {
		had, err := ffi.DecodeAccount(existing)
		if err != nil {
			return err
		}
		lamports = had.Lamports
	} else {
		s.mint(lamports)
	}
	s.SetAccount(ms, key, ffi.EncodeAccount(&owner, lamports, RentExemptEpoch, false, data))
	return nil
}

// setLamportsPerByte rewrites lamports_per_byte in the Rent sysvar, which is
// what a rent repricing gate does. The rest of the sysvar does not change.
// SIMD-0194 deprecated the exemption threshold, and genesis already writes the
// post-deprecation shape, so the cut can be written directly.
func (s Stores) setLamportsPerByte(ms storetypes.MultiStore, perByte uint64) error {
	rent, err := s.accountData(ms, RentID)
	if err != nil {
		return err
	}
	data := append([]byte(nil), rent.Data...)
	if len(data) < 8 {
		return fmt.Errorf("svm: Rent sysvar is %d bytes", len(data))
	}
	binary.LittleEndian.PutUint64(data[0:8], perByte)
	return s.rewrite(ms, RentID, rent, data)
}

// LamportsPerByte returns what the Rent sysvar charges now.
// getMinimumBalanceForRentExemption also reads it, so a rent flip moves both
// at the same height.
func (s Stores) LamportsPerByte(ms storetypes.MultiStore) (uint64, error) {
	rent, err := s.accountData(ms, RentID)
	if err != nil {
		return 0, err
	}
	if len(rent.Data) < 8 {
		return 0, fmt.Errorf("svm: Rent sysvar is %d bytes", len(rent.Data))
	}
	return binary.LittleEndian.Uint64(rent.Data[0:8]), nil
}

// pendingAt is the prefix every activation pending at height shares.
func pendingAt(height uint64) []byte {
	return binary.BigEndian.AppendUint64(append([]byte(nil), pendingPrefix...), height)
}

func pendingKey(height uint64, id [32]byte) []byte { return append(pendingAt(height), id[:]...) }

// splitPendingKey reads a key under pendingPrefix back into the height and
// the gate, false for a key of another length.
func splitPendingKey(k []byte) (height uint64, id [32]byte, ok bool) {
	if len(k) != len(pendingPrefix)+8+32 {
		return 0, id, false
	}
	return binary.BigEndian.Uint64(k[len(pendingPrefix):]), [32]byte(k[len(pendingPrefix)+8:]), true
}
