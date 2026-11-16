package svm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/mr-tron/base58"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
)

// Bincode layouts and canonical account sizes of the sysvars that Go owns. Go
// writes them, and the engine only reads them.
const (
	lamportsPerSignature = 5_000
	slotsPerEpoch        = 432_000
	// Rent::default's price, mainnet's at the pin. The genesis Rent sysvar
	// starts from it, cut by genesisLamportsPerByte.
	pinLamportsPerByte = 6_960

	clockLen             = 40
	slotHashesMax        = 512 // solana-slot-hashes-3.1.0 MAX_ENTRIES
	recentBlockhashesMax = 150 // MAX_ENTRIES
	// sysvarEntryBytes is the width of a SlotHashes or RecentBlockhashes
	// entry: a slot and a hash, or a hash and a fee calculator.
	sysvarEntryBytes = 40
)

func mustKey(s string) [32]byte {
	b, err := base58.Decode(s)
	if err != nil || len(b) != 32 {
		panic("svm: bad key " + s)
	}
	return [32]byte(b)
}

var (
	SysvarOwner   = mustKey("Sysvar1111111111111111111111111111111111111")
	NativeLoader  = mustKey("NativeLoader1111111111111111111111111111111")
	SystemProgram = mustKey("11111111111111111111111111111111")
	// The three precompiles. They are not programs. They have no builtin entry
	// and no loader, and the engine's callback dispatches them.
	Secp256k1Program = mustKey("KeccakSecp256k11111111111111111111111111111")
	Ed25519Program   = mustKey("Ed25519SigVerify111111111111111111111111111")
	Secp256r1Program = mustKey("Secp256r1SigVerify1111111111111111111111111")
	// The ZK ElGamal proof program, a builtin like the other five.
	ZkElGamalProof      = mustKey("ZkE1Gama1Proof11111111111111111111111111111")
	ClockID             = mustKey("SysvarC1ock11111111111111111111111111111111")
	EpochScheduleID     = mustKey("SysvarEpochSchedu1e111111111111111111111111")
	EpochRewardsID      = mustKey("SysvarEpochRewards1111111111111111111111111")
	RentID              = mustKey("SysvarRent111111111111111111111111111111111")
	SlotHashesID        = mustKey("SysvarS1otHashes111111111111111111111111111")
	StakeHistoryID      = mustKey("SysvarStakeHistory1111111111111111111111111")
	LastRestartSlotID   = mustKey("SysvarLastRestartS1ot1111111111111111111111")
	RecentBlockhashesID = mustKey("SysvarRecentB1ockHashes11111111111111111111")
	// Fees is deprecated upstream, and no syscall can serve it. But it is a
	// mainnet account, and a client that asks for it gets bytes, not null.
	// Genesis writes it once, and nothing writes it again.
	FeesID = mustKey("SysvarFees111111111111111111111111111111111")
)

// genesisLamportsPerByte is the Rent sysvar's price at genesis. It is the
// pin's 6,960, cut by each rent gate that genesis activates, in Agave's order.
// With SIMD-0437-1 and -2 active at genesis, it is mainnet's 5,080. Agave
// applies a rent gate only when it newly activates. A gate active from genesis
// has its price in the genesis Rent, which is this value.
var genesisLamportsPerByte = func() uint64 {
	perByte := uint64(pinLamportsPerByte)
	exceptions := GenesisExceptions()
	for _, g := range rentGates {
		if slices.Contains(exceptions, g.id) {
			perByte = g.perByte
		}
	}
	return perByte
}()

// accountStorageOverhead is the bytes Rent charges an account beside its
// data, solana-rent's ACCOUNT_STORAGE_OVERHEAD.
const accountStorageOverhead = 128

// RentExemptMinimum is solana-rent's Rent::minimum_balance at the SIMD-0194
// exemption threshold of 1.0, which every Rent sysvar here carries.
func RentExemptMinimum(lamportsPerByte, dataLen uint64) uint64 {
	return (accountStorageOverhead + dataLen) * lamportsPerByte
}

// MinimumBalance is RentExemptMinimum at the genesis price. A rent gate that
// governance activates later moves the sysvar, which Stores.LamportsPerByte
// reads. It does not move this value.
func MinimumBalance(dataLen int) uint64 {
	return RentExemptMinimum(genesisLamportsPerByte, uint64(dataLen))
}

// genesisSysvars are the nine sysvar accounts that genesis carries. Clock,
// SlotHashes and RecentBlockhashes are rewritten every block. Genesis carries
// them so that their lamports stay inside the genesis supply.
// engine/src/runtime.rs checks the bytes against the pinned crates.
func genesisSysvars() []GenesisAccount {
	rent := make([]byte, 17)
	binary.LittleEndian.PutUint64(rent[0:8], genesisLamportsPerByte)
	copy(rent[8:16], []byte{0, 0, 0, 0, 0, 0, 240, 63}) // SIMD-0194 exemption threshold, 1.0f64
	rent[16] = 50

	// Fees is FeeCalculator { lamports_per_signature }, eight bytes. Mainnet's
	// own bytes decode to 5,000, which is already this chain's fee, so the row
	// copies mainnet and invents no value.
	fees := make([]byte, 8)
	binary.LittleEndian.PutUint64(fees, lamportsPerSignature)

	return []GenesisAccount{
		sysvar(ClockID, make([]byte, clockLen)),
		sysvar(EpochScheduleID, epochSchedule()),
		sysvar(EpochRewardsID, make([]byte, 81)),
		sysvar(RentID, rent),
		sysvar(SlotHashesID, make([]byte, listLen(slotHashesMax, sysvarEntryBytes))),
		sysvar(StakeHistoryID, make([]byte, listLen(512, 32))),
		sysvar(LastRestartSlotID, make([]byte, 8)),
		sysvar(FeesID, fees),
		sysvar(RecentBlockhashesID, make([]byte, listLen(recentBlockhashesMax, sysvarEntryBytes))),
	}
}

// epochSchedule is the EpochSchedule this chain runs, without_warmup. Every
// epoch is slotsPerEpoch long, so first_normal_epoch and first_normal_slot
// are 0.
func epochSchedule() []byte {
	schedule := make([]byte, 33)
	binary.LittleEndian.PutUint64(schedule[0:8], slotsPerEpoch)
	binary.LittleEndian.PutUint64(schedule[8:16], slotsPerEpoch)
	return schedule
}

// Clock is the Clock sysvar of solana-clock, five eight-byte fields in
// bincode.
type Clock struct {
	Slot                uint64
	EpochStartTimestamp int64
	Epoch               uint64
	LeaderScheduleEpoch uint64
	UnixTimestamp       int64
}

// DecodeClock reads the Clock sysvar's data, false when it is shorter than
// the layout.
func DecodeClock(data []byte) (Clock, bool) {
	if len(data) < clockLen {
		return Clock{}, false
	}
	return Clock{
		Slot:                binary.LittleEndian.Uint64(data[0:8]),
		EpochStartTimestamp: int64(binary.LittleEndian.Uint64(data[8:16])),
		Epoch:               binary.LittleEndian.Uint64(data[16:24]),
		LeaderScheduleEpoch: binary.LittleEndian.Uint64(data[24:32]),
		UnixTimestamp:       int64(binary.LittleEndian.Uint64(data[32:40])),
	}, true
}

// clockOf is DecodeClock on the Clock this chain keeps, which genesis writes
// and writeSysvars rewrites at its size. A short one is a broken state, so it
// panics.
func clockOf(data []byte) Clock {
	c, ok := DecodeClock(data)
	if !ok {
		panic(fmt.Sprintf("svm: the Clock sysvar is %d bytes", len(data)))
	}
	return c
}

// sysvarField is one field of a sysvar's bincode layout, named as in its Agave
// struct.
type sysvarField struct {
	name   string
	width  int
	isBool bool
}

// sysvarLayout is a sysvar's bincode layout: a struct's fields, or for a list
// a u64 count and then that many entries, each entry bytes wide. most bounds
// the count, 0 for no bound.
type sysvarLayout struct {
	name   string
	fields []sysvarField
	entry  int
	most   uint64
}

// wincodePreallocation is the most bytes wincode::deserialize allots a Vec
// before reading it, so it bounds the count.
const wincodePreallocation = 4 << 20

// sysvarLayouts are the layouts Agave's sysvar cache reads the genesis sysvars
// with, wincode for SlotHashes and bincode for the rest. Both accept trailing
// bytes.
var sysvarLayouts = map[[32]byte]sysvarLayout{
	ClockID: {name: "Clock", fields: []sysvarField{
		{"slot", 8, false}, {"epoch_start_timestamp", 8, false}, {"epoch", 8, false},
		{"leader_schedule_epoch", 8, false}, {"unix_timestamp", 8, false}}},
	EpochScheduleID: {name: "EpochSchedule", fields: []sysvarField{
		{"slots_per_epoch", 8, false}, {"leader_schedule_slot_offset", 8, false}, {"warmup", 1, true},
		{"first_normal_epoch", 8, false}, {"first_normal_slot", 8, false}}},
	EpochRewardsID: {name: "EpochRewards", fields: []sysvarField{
		{"distribution_starting_block_height", 8, false}, {"num_partitions", 8, false},
		{"parent_blockhash", 32, false}, {"total_points", 16, false}, {"total_rewards", 8, false},
		{"distributed_rewards", 8, false}, {"active", 1, true}}},
	RentID: {name: "Rent", fields: []sysvarField{
		{"lamports_per_byte", 8, false}, {"exemption_threshold", 8, false}, {"burn_percent", 1, false}}},
	SlotHashesID:        {name: "SlotHashes", entry: sysvarEntryBytes, most: wincodePreallocation / sysvarEntryBytes},
	StakeHistoryID:      {name: "StakeHistory", entry: 32},
	LastRestartSlotID:   {name: "LastRestartSlot", fields: []sysvarField{{"last_restart_slot", 8, false}}},
	FeesID:              {name: "Fees", fields: []sysvarField{{"fee_calculator", 8, false}}},
	RecentBlockhashesID: {name: "RecentBlockhashes", entry: sysvarEntryBytes},
}

// checkChainSysvar refuses a sysvar row Agave would decode but this chain
// cannot run on. writeSysvars rewrites the Clock and prepends to SlotHashes
// and RecentBlockhashes in place, at the sizes genesis writes, so another size
// halts the first block or panics the second. The epoch arithmetic here and
// in rpc assumes the schedule genesis writes, so another schedule answers
// wrong epochs, and 0 slots an epoch divides by zero.
func checkChainSysvar(id [32]byte, data []byte) error {
	var want int
	switch id {
	case ClockID:
		want = clockLen
	case SlotHashesID:
		want = listLen(slotHashesMax, sysvarEntryBytes)
	case RecentBlockhashesID:
		want = listLen(recentBlockhashesMax, sysvarEntryBytes)
	case EpochScheduleID:
		if !bytes.Equal(data, epochSchedule()) {
			return fmt.Errorf("svm genesis: the EpochSchedule sysvar is not the schedule this chain runs, %d slots an epoch with no warmup", slotsPerEpoch)
		}
		return nil
	default:
		return nil
	}
	if len(data) != want {
		return fmt.Errorf("svm genesis: the %s sysvar is %d bytes, and this chain keeps it at %d", sysvarLayouts[id].name, len(data), want)
	}
	return nil
}

// checkSysvar refuses a sysvar row the sysvar cache would not deserialize.
// The cache leaves such a sysvar out, and for any but Fees the first block
// halts without it.
func checkSysvar(id [32]byte, data []byte) error {
	l, ok := sysvarLayouts[id]
	if !ok {
		return nil
	}
	if l.entry > 0 {
		if len(data) < 8 {
			return fmt.Errorf("svm genesis: the %s sysvar is %d bytes and ends inside its entries count", l.name, len(data))
		}
		n := binary.LittleEndian.Uint64(data)
		if n > uint64(len(data)-8)/uint64(l.entry) || (l.most > 0 && n > l.most) {
			return fmt.Errorf("svm genesis: the %s sysvar counts %d entries, more than Agave decodes from its %d bytes",
				l.name, n, len(data))
		}
		return nil
	}
	off := 0
	for _, f := range l.fields {
		if len(data) < off+f.width {
			return fmt.Errorf("svm genesis: the %s sysvar is %d bytes and ends inside %s", l.name, len(data), f.name)
		}
		if f.isBool && data[off] > 1 {
			return fmt.Errorf("svm genesis: the %s sysvar's %s is %d, and bincode reads a bool only from 0 or 1",
				l.name, f.name, data[off])
		}
		off += f.width
	}
	return nil
}

func sysvar(id [32]byte, data []byte) GenesisAccount {
	return rentExempt(id, SysvarOwner, false, data)
}

// writeSysvars is the begin-block writer. It writes Clock, SlotHashes and
// RecentBlockhashes from the CometBFT header. It runs before the TxRunner
// builds the phase 3 arena for the same block.
func (s Stores) writeSysvars(ms storetypes.MultiStore, height uint64, unixTime int64, hash []byte) error {
	if len(hash) != 32 {
		return fmt.Errorf("svm: header hash is %d bytes", len(hash))
	}
	meta := ms.GetKVStore(s.Meta)
	parent := meta.Get(MetaBlockhashKey)

	clock, err := s.accountData(ms, ClockID)
	if err != nil {
		return err
	}
	epoch := epochOf(height)
	epochStart := unixTime
	if prev := clockOf(clock.Data); prev.Slot > 0 && prev.Epoch == epoch {
		epochStart = prev.EpochStartTimestamp
	}
	c := make([]byte, clockLen)
	binary.LittleEndian.PutUint64(c[0:8], height)
	binary.LittleEndian.PutUint64(c[8:16], uint64(epochStart))
	binary.LittleEndian.PutUint64(c[16:24], epoch)
	// EpochSchedule::get_leader_schedule_epoch with warmup off and the offset
	// equal to slots_per_epoch.
	binary.LittleEndian.PutUint64(c[24:32], epoch+1)
	binary.LittleEndian.PutUint64(c[32:40], uint64(unixTime))
	if err := s.rewrite(ms, ClockID, clock, c); err != nil {
		return err
	}

	// At height 1 there is no parent, and both lists stay empty.
	if parent != nil {
		if err := s.prepend(ms, SlotHashesID, slotHashEntry(height, parent), slotHashesMax); err != nil {
			return err
		}
		var hashEntry [40]byte
		copy(hashEntry[0:32], parent)
		binary.LittleEndian.PutUint64(hashEntry[32:40], lamportsPerSignature)
		if err := s.prepend(ms, RecentBlockhashesID, hashEntry, recentBlockhashesMax); err != nil {
			return err
		}
	}
	meta.Set(MetaBlockhashKey, append([]byte(nil), hash...))
	return nil
}

// epochOf is EpochSchedule::get_epoch with warmup off. A block's height is its
// slot, and every epoch is slotsPerEpoch long.
func epochOf(height uint64) uint64 { return height / slotsPerEpoch }

// slotHashEntry is the SlotHashes entry the writer adds at height, the
// parent's slot and its hash.
func slotHashEntry(height uint64, parent []byte) [40]byte {
	var e [40]byte
	binary.LittleEndian.PutUint64(e[0:8], height-1)
	copy(e[8:], parent)
	return e
}

// prepend adds a newest-first entry to a sysvar's list, keeping the account's
// canonical size.
func (s Stores) prepend(ms storetypes.MultiStore, id [32]byte, entry [40]byte, limit int) error {
	acct, err := s.accountData(ms, id)
	if err != nil {
		return err
	}
	return s.rewrite(ms, id, acct, prepended(acct.Data, entry, limit))
}

// prepended returns data, a bincode Vec of 40-byte entries, with entry added at
// the front and the list truncated at limit.
func prepended(data []byte, entry [40]byte, limit int) []byte {
	n := int(binary.LittleEndian.Uint64(data[0:8]))
	keep := min(n, limit-1)
	d := make([]byte, len(data))
	binary.LittleEndian.PutUint64(d[0:8], uint64(keep+1))
	copy(listEntry(d, 0), entry[:])
	copy(d[listLen(1, sysvarEntryBytes):listLen(keep+1, sysvarEntryBytes)],
		data[listLen(0, sysvarEntryBytes):listLen(keep, sysvarEntryBytes)])
	return d
}

// listLen is the size of a sysvar list of n entries, each width bytes: a
// bincode Vec, its u64 count and then the entries.
func listLen(n, width int) int { return 8 + n*width }

// listEntry is entry i of a list of sysvarEntryBytes entries.
func listEntry(data []byte, i int) []byte {
	return data[listLen(i, sysvarEntryBytes):listLen(i+1, sysvarEntryBytes)]
}

// rewrite replaces a sysvar's data, keeping its lamports, owner and rent epoch,
// as Agave's update_sysvar_account does for an account that already exists.
func (s Stores) rewrite(ms storetypes.MultiStore, id [32]byte, old ffi.Account, data []byte) error {
	if len(data) != len(old.Data) {
		return fmt.Errorf("svm: sysvar %s changed size", base58.Encode(id[:]))
	}
	s.SetAccount(ms, id, ffi.EncodeAccount(&old.Owner, old.Lamports, old.RentEpoch, old.Executable, data))
	return nil
}

// BlockhashWindow is the acceptance window in blocks. The age check passes for
// the block's own hash plus the 150 entries of RecentBlockhashes.
const BlockhashWindow = recentBlockhashesMax

// Admissible is the state half of admission. It reports whether tx, which
// ffi.CheckTx admitted as a, is one that the block at height would process,
// read against ms, that block's context. The lookup tables must resolve as
// that block's phase 2 will resolve them. An expired blockhash must be the
// durable nonce that its nonce account holds, signed by the nonce authority.
// The fee payer must be able to pay the fee that a declares. A transaction it
// refuses gets ffi.ErrRejected, ffi.ErrNonce or ffi.ErrUnpayable.
func (s Stores) Admissible(ms storetypes.MultiStore, height uint64, tx []byte, a ffi.Admission, expired bool) error {
	rent, err := s.accountData(ms, RentID)
	if err != nil {
		return err
	}
	c := ffi.Committed{Rent: rent.Data, Payer: s.GetAccount(ms, a.Payer), Expired: expired, Height: height}
	if expired {
		c.Nonce = s.GetAccount(ms, a.Nonce)
	}
	if len(a.Tables) > 0 {
		if c.SlotHashes, c.Tables, err = s.lookupTables(ms, a.Tables); err != nil {
			return err
		}
	}
	return ffi.CheckCommitted(tx, a, c)
}

// Resolved returns the cost and the written accounts of tx, which ffi.CheckTx
// admitted as a, as the block at height counts them. It reads against ms, that
// block's context. For a message that names no table, that is a itself. For
// one that names a table, it is the resolved message's, with the tables read
// as that block's phase 2 will read them. ok is false when the tables do not
// resolve or the resolved message names an account twice. The block does not
// process such a transaction.
func (s Stores) Resolved(ms storetypes.MultiStore, height uint64, tx []byte, a ffi.Admission) (r ffi.Resolved, ok bool, err error) {
	if len(a.Tables) == 0 {
		return ffi.Resolved{Units: a.Units, AllocatedBytes: a.AllocatedBytes, Writable: a.Writable}, true, nil
	}
	slotHashes, tables, err := s.lookupTables(ms, a.Tables)
	if err != nil {
		return r, false, err
	}
	r, refused, err := ffi.ResolvedAdmission(height, tx, slotHashes, tables)
	return r, err == nil && refused == "", err
}

// lookupTables returns what the block whose context is ms resolves keys
// against. That is its SlotHashes data, and the stored value of each table
// that exists.
func (s Stores) lookupTables(ms storetypes.MultiStore, keys [][32]byte) ([]byte, map[[32]byte][]byte, error) {
	slotHashes, err := s.accountData(ms, SlotHashesID)
	if err != nil {
		return nil, nil, err
	}
	tables := make(map[[32]byte][]byte, len(keys))
	for _, k := range keys {
		if v := s.GetAccount(ms, k); v != nil {
			tables[k] = v
		}
	}
	return slotHashes.Data, tables, nil
}

// BlockhashValid reports whether a transaction that uses want would pass the
// age check in the block whose context is ms. The engine's window is the
// block's own hash, which no transaction can name yet, plus ages 1 to 150, the
// 150 entries of its RecentBlockhashes.
//
// CheckTx, the RPC and the engine read one window from one sysvar, so they
// cannot disagree.
func (s Stores) BlockhashValid(ms storetypes.MultiStore, want []byte) (bool, error) {
	recent, err := ffi.DecodeAccount(s.GetAccount(ms, RecentBlockhashesID))
	if err != nil {
		return false, err
	}
	n := min(int(binary.LittleEndian.Uint64(recent.Data[0:8])), BlockhashWindow)
	for i := 0; i < n; i++ {
		if bytes.Equal(listEntry(recent.Data, i)[:32], want) {
			return true, nil
		}
	}
	return false, nil
}
