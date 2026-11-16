package svm

import (
	"bytes"
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"sync"
	"sync/atomic"

	dbm "github.com/cosmos/cosmos-db"

	"github.com/rit3sh-x/cosmos-svm/ffi"
)

// History is the machine-local transaction record the RPC serves. It is outside
// the commit tree:
//
//	primary   'p' || height u64 BE || tx_index u32 BE  ->  record (JSON)
//	pointer   's' || signature                          ->  height u64 BE || tx_index u32 BE
//	address   'a' || pubkey || height u64 BE || tx_index u32 BE  ->  ()
//	block     'h' || height u64 BE                      ->  the CometBFT header hash || block time i64 BE
//	sample    'f' || height u64 BE                      ->  a performance sample, 26 bytes
//	counter   'b'                                       ->  bytes of every entry above, u64 BE
//	horizon   'z'                                       ->  highest purged height, u64 BE
//
// The address keyspace is Agave's layout, so the pubkey is the prefix and the
// height follows it. The reader wants one address newest-first, and it gets
// that from one reverse prefix scan. Agave's key ends in the signature. This
// key stops at the position, which is unique, so the scan is in the same order.
// The reader takes the signature from the record that it reads anyway.
//
// The block keyspace is written for every height, also for a height with no
// transaction, so getBlock can tell an empty block from a purged one. With it,
// the two keyspaces are block storage. The primary keyspace holds the
// transactions, and the block keyspace holds the header. The block time is
// stored beside the hash, so an empty block has one too. No entry holds the
// hash alone, because every network starts at genesis on a binary that writes
// both.
type History struct {
	db       dbm.DB
	maxBytes uint64
	// bytes belongs to the block path alone. The RPC goroutines also read
	// horizon while a block purges, so horizon is atomic.
	bytes   uint64
	horizon atomic.Uint64

	// compactMu guards the compaction state: whether one is running, and the
	// range the next run covers.
	compactMu   sync.Mutex
	compacting  bool
	pending     *[2][]byte
	compactions sync.WaitGroup
	// sample is the last performance sample that the sampler took. The next
	// block's Write stores it, so bytes stays the block path's alone.
	sample atomic.Pointer[PerfSample]
}

// PerfSample is Agave's PerfSample, the counts of one sampling period. This
// chain has no vote transactions, so every transaction is a non-vote one.
type PerfSample struct {
	Slot             uint64
	NumSlots         uint64
	NumTransactions  uint64
	SamplePeriodSecs uint16
}

const sampleBytes = 26

// sampleEntryBytes is what a stored sample costs the counter: its key and its
// value.
const sampleEntryBytes = 1 + 8 + sampleBytes

func sampleKey(height uint64) []byte {
	return binary.BigEndian.AppendUint64([]byte{'f'}, height)
}

func (p PerfSample) encode() []byte {
	b := binary.BigEndian.AppendUint64(nil, p.Slot)
	b = binary.BigEndian.AppendUint64(b, p.NumSlots)
	b = binary.BigEndian.AppendUint64(b, p.NumTransactions)
	return binary.BigEndian.AppendUint16(b, p.SamplePeriodSecs)
}

// Sample hands the next block a sample to store. It is machine-local.
func (h *History) Sample(p PerfSample) { h.sample.Store(&p) }

// Samples returns the stored samples, newest first, at most limit.
func (h *History) Samples(limit int) ([]PerfSample, error) {
	it, err := h.db.ReverseIterator([]byte{'f'}, []byte{'g'})
	if err != nil {
		return nil, err
	}
	defer it.Close()
	var out []PerfSample
	for ; it.Valid() && len(out) < limit; it.Next() {
		v := it.Value()
		if len(v) != sampleBytes {
			return nil, fmt.Errorf("svm history sample %x", it.Key())
		}
		out = append(out, PerfSample{
			Slot:             binary.BigEndian.Uint64(v[0:8]),
			NumSlots:         binary.BigEndian.Uint64(v[8:16]),
			NumTransactions:  binary.BigEndian.Uint64(v[16:24]),
			SamplePeriodSecs: binary.BigEndian.Uint16(v[24:26]),
		})
	}
	return out, nil
}

// Record is one processed transaction.
type Record struct {
	Height    uint64   `json:"height"`
	Index     uint32   `json:"index"`
	BlockTime int64    `json:"block_time"`
	Tx        []byte   `json:"tx"`
	Signature [64]byte `json:"signature"`
	Status    uint32   `json:"status"`
	Fee       uint64   `json:"fee"`
	Units     uint64   `json:"units"`
	// Price is the compute unit price that the transaction declared.
	// getRecentPrioritizationFees reports it as a block minimum. Nothing ranks
	// the transaction by it.
	Price uint64     `json:"price"`
	Meta  ffi.TxMeta `json:"meta"`

	dedupKey [64]byte
}

// blockKey is 'h' || height. With this order, a purge by height is one range
// delete, as in the primary keyspace.
func blockKey(height uint64) []byte {
	return binary.BigEndian.AppendUint64([]byte{'h'}, height)
}

const blockEntryBytes = 1 + 8 + 32 + 8

// blockValue is a block entry's value: the header hash, then the block time.
func blockValue(hash [32]byte, t int64) []byte {
	return binary.BigEndian.AppendUint64(hash[:], uint64(t))
}

// splitBlockValue parses a value blockValue built.
func splitBlockValue(v []byte) (hash [32]byte, t int64, ok bool) {
	if len(v) != 32+8 {
		return hash, 0, false
	}
	return [32]byte(v[:32]), int64(binary.BigEndian.Uint64(v[32:])), true
}

func historyKey(height uint64, index uint32) []byte {
	return binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint64([]byte{'p'}, height), index)
}

// pointerKey is 's' || signature. Its value is the record's key without the
// 'p', so the signature alone finds the record.
func pointerKey(sig [64]byte) []byte {
	return append([]byte{'s'}, sig[:]...)
}

// recordBytes is what a record costs the counter. That is its key, its JSON
// value, its pointer key, and the pointer's value, which is the key without
// its prefix. Write adds it and deleteRange takes it off, so the budget cannot
// drift.
func recordBytes(key, value, pointer []byte) uint64 {
	return uint64(len(key) + len(value) + len(pointer) + len(key) - 1)
}

// addressKey is 'a' || pubkey || height || tx_index. Every field
// has a fixed width, so a prefix scan over one address is ordered by height and
// then by position in the block. The reader serves that order reversed.
func addressKey(address [32]byte, height uint64, index uint32) []byte {
	k := make([]byte, 0, addressKeyBytes)
	k = append(append(k, 'a'), address[:]...)
	return binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint64(k, height), index)
}

const addressKeyBytes = 1 + 32 + 8 + 4

// splitAddressKey parses the position out of a key that addressKey built.
func splitAddressKey(key []byte) (height uint64, index uint32, ok bool) {
	if len(key) != addressKeyBytes {
		return 0, 0, false
	}
	return binary.BigEndian.Uint64(key[33:41]), binary.BigEndian.Uint32(key[41:45]), true
}

// Addresses returns every account that a record named: its static message keys
// and the addresses its lookup tables resolved, deduplicated.
//
// Both come from the engine, which has the only Solana message parser. The
// execute response of the block carries them in the meta record, so the block
// path makes no boundary crossing for a record. For a record without static
// keys, Addresses parses the transaction through the engine.
func (r *Record) Addresses() ([][32]byte, error) {
	static := [][32]byte(r.Meta.StaticKeys)
	if len(static) == 0 {
		var err error
		if _, static, err = ffi.TransactionKeys(r.Tx); err != nil {
			return nil, err
		}
	}
	out := make([][32]byte, 0, len(static)+len(r.Meta.LoadedWritable)+len(r.Meta.LoadedReadonly))
	seen := make(map[[32]byte]struct{}, cap(out))
	for _, group := range [][][32]byte{static, r.Meta.LoadedWritable, r.Meta.LoadedReadonly} {
		for _, a := range group {
			if _, ok := seen[a]; ok {
				continue
			}
			seen[a] = struct{}{}
			out = append(out, a)
		}
	}
	return out, nil
}

func getU64(db dbm.DB, key byte) (uint64, error) {
	v, err := db.Get([]byte{key})
	if err != nil || v == nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(v), nil
}

// putBytes adds the counter to batch, so it commits with the entries it counts.
func (h *History) putBytes(batch dbm.Batch) error {
	return batch.Set([]byte{'b'}, binary.BigEndian.AppendUint64(nil, h.bytes))
}

// OpenHistory loads the counter and horizon, and drops records above the
// committed height. maxBytes is index-max-bytes.
func OpenHistory(db dbm.DB, committed, maxBytes uint64) (*History, error) {
	h := &History{db: db, maxBytes: maxBytes}
	var err error
	if h.bytes, err = getU64(db, 'b'); err != nil {
		return nil, err
	}
	horizon, err := getU64(db, 'z')
	if err != nil {
		return nil, err
	}
	h.horizon.Store(horizon)
	if err := h.deleteBlocks(committed+1, ^uint64(0)); err != nil {
		return nil, err
	}
	if err := h.deleteSamples(committed+1, ^uint64(0)); err != nil {
		return nil, err
	}
	return h, h.deleteRange(historyKey(committed+1, 0), []byte{'q'})
}

// Restored records that this machine holds no block at or below height. On a
// machine that joined by state sync, height is the snapshot's height.
// getFirstAvailableBlock then answers the first block that the machine holds.
func (h *History) Restored(height uint64) error {
	if height <= h.horizon.Load() {
		return nil
	}
	return h.setHorizon(height)
}

// setHorizon moves the horizon and persists it with a sync write. It runs
// before any delete below it, so readers stop serving a height before its
// rows go.
func (h *History) setHorizon(height uint64) error {
	h.horizon.Store(height)
	return h.db.SetSync([]byte{'z'}, binary.BigEndian.AppendUint64(nil, height))
}

// Write records a finalizing block's header hash and time and its processed
// transactions, and enforces the byte budget.
func (h *History) Write(height uint64, hash [32]byte, blockTime int64, records []Record) error {
	batch := h.db.NewBatch()
	defer batch.Close()
	if err := batch.Set(blockKey(height), blockValue(hash, blockTime)); err != nil {
		return err
	}
	h.bytes += blockEntryBytes
	if p := h.sample.Swap(nil); p != nil {
		if err := batch.Set(sampleKey(p.Slot), p.encode()); err != nil {
			return err
		}
		h.bytes += sampleEntryBytes
	}
	for _, r := range records {
		value, err := json.Marshal(r, json.Deterministic(true))
		if err != nil {
			return err
		}
		key := historyKey(r.Height, r.Index)
		pointer := pointerKey(r.Signature)
		if err := batch.Set(key, value); err != nil {
			return err
		}
		if err := batch.Set(pointer, key[1:]); err != nil {
			return err
		}
		h.bytes += recordBytes(key, value, pointer)
		addresses, err := r.Addresses()
		if err != nil {
			return err
		}
		for _, a := range addresses {
			entry := addressKey(a, r.Height, r.Index)
			if err := batch.Set(entry, []byte{}); err != nil {
				return err
			}
			h.bytes += uint64(len(entry))
		}
	}
	if err := h.putBytes(batch); err != nil {
		return err
	}
	if err := batch.WriteSync(); err != nil {
		return err
	}
	return h.purge(height)
}

// purge runs when the record is over budget. It divides the excess by the
// mean bytes per retained block, and first moves the horizon up by that many
// blocks. Then it deletes below the horizon and compacts. The mean counts empty
// blocks, so a pass can purge less than the excess. Passes repeat until the
// record is under budget or only the latest block is left.
func (h *History) purge(latest uint64) error {
	for h.maxBytes != 0 && h.bytes > h.maxBytes {
		lowest := h.horizon.Load() + 1
		if lowest >= latest {
			return nil
		}
		horizon := purgeHorizon(h.bytes, h.maxBytes, lowest, latest)
		if err := h.setHorizon(horizon); err != nil {
			return err
		}
		start, end := historyKey(lowest, 0), historyKey(horizon+1, 0)
		if err := h.deleteRange(start, end); err != nil {
			return err
		}
		if err := h.deleteBlocks(lowest, horizon+1); err != nil {
			return err
		}
		if err := h.deleteSamples(lowest, horizon+1); err != nil {
			return err
		}
		h.compact(start, end)
	}
	return nil
}

// purgeHorizon is the horizon that one pass of purge moves to. used is over
// maxBytes, and lowest through latest are retained, with lowest < latest. The
// pass purges the excess over the mean bytes per retained block, rounded up,
// and at least one block.
func purgeHorizon(used, maxBytes, lowest, latest uint64) uint64 {
	mean := used / (latest - lowest + 1)
	purge := uint64(1)
	if mean > 0 {
		purge = max(1, (used-maxBytes+mean-1)/mean)
	}
	// Never purge the latest block.
	return min(lowest+purge, latest) - 1
}

// compact gives back the disk space of a purged range, on its own goroutine.
// It is off the block path because nothing depends on it. The keys of the
// range are already deleted. A goleveldb compaction flushes the memtable and
// rewrites every table the range overlaps. Inside FinalizeBlock, every purging
// block would pay that cost, and a compaction error would halt svmd. One
// compaction runs at a time, and a range purged while it runs widens the next.
// The Pebble database of svmd has no ForceCompact, so there this does
// nothing. The L0 threshold of the sidecars gives the space back instead.
func (h *History) compact(start, end []byte) {
	c, ok := h.db.(interface {
		ForceCompact(start, limit []byte) error
	})
	if !ok {
		return
	}
	h.compactMu.Lock()
	defer h.compactMu.Unlock()
	if h.pending == nil {
		h.pending = &[2][]byte{start, end}
	} else {
		if bytes.Compare(start, h.pending[0]) < 0 {
			h.pending[0] = start
		}
		if bytes.Compare(end, h.pending[1]) > 0 {
			h.pending[1] = end
		}
	}
	if h.compacting {
		return
	}
	h.compacting = true
	h.compactions.Add(1)
	go func() {
		defer h.compactions.Done()
		for {
			h.compactMu.Lock()
			next := h.pending
			h.pending = nil
			if next == nil {
				h.compacting = false
				h.compactMu.Unlock()
				return
			}
			h.compactMu.Unlock()
			if err := c.ForceCompact(next[0], next[1]); err != nil {
				historyCompactionFailures.Inc()
			}
		}
	}()
}

// deleteRange deletes primary records in [start, end) with their pointers and
// takes their bytes off the counter.
func (h *History) deleteRange(start, end []byte) error {
	it, err := h.db.Iterator(start, end)
	if err != nil {
		return err
	}
	batch := h.db.NewBatch()
	defer batch.Close()
	for ; it.Valid(); it.Next() {
		var r Record
		if err := json.Unmarshal(it.Value(), &r); err != nil {
			it.Close()
			return fmt.Errorf("svm history record %x: %w", it.Key(), err)
		}
		pointer := pointerKey(r.Signature)
		if err := batch.Delete(append([]byte(nil), it.Key()...)); err != nil {
			it.Close()
			return err
		}
		if err := batch.Delete(pointer); err != nil {
			it.Close()
			return err
		}
		size := recordBytes(it.Key(), it.Value(), pointer)
		addresses, err := r.Addresses()
		if err != nil {
			it.Close()
			return err
		}
		for _, a := range addresses {
			entry := addressKey(a, r.Height, r.Index)
			if err := batch.Delete(entry); err != nil {
				it.Close()
				return err
			}
			size += uint64(len(entry))
		}
		h.bytes -= min(size, h.bytes)
	}
	it.Close()
	if err := h.putBytes(batch); err != nil {
		return err
	}
	return batch.WriteSync()
}

// Get returns the record for a signature if it is committed and above the horizon.
func (h *History) Get(sig [64]byte, committed uint64) (Record, bool, error) {
	var r Record
	height, index, ok, err := h.locate(sig, committed)
	if err != nil || !ok {
		return r, false, err
	}
	v, err := h.db.Get(historyKey(height, index))
	if err != nil || v == nil {
		return r, false, err
	}
	return r, true, json.Unmarshal(v, &r)
}

// AtHeight returns a height's records in block order.
func (h *History) AtHeight(height uint64) ([]Record, error) {
	it, err := h.db.Iterator(historyKey(height, 0), historyKey(height+1, 0))
	if err != nil {
		return nil, err
	}
	defer it.Close()
	var out []Record
	for ; it.Valid(); it.Next() {
		var r Record
		if err := json.Unmarshal(it.Value(), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Bytes returns the counter, for tests and operators.
func (h *History) Bytes() uint64 { return h.bytes }

// SignatureInfo is one row of getSignaturesForAddress. It holds what the
// reader needs without decoding the whole record.
type SignatureInfo struct {
	Signature [64]byte
	Height    uint64
	Index     uint32
	Status    uint32
	Err       jsontext.Value
	BlockTime int64
	// Tx and the loaded addresses are the source of the memo.
	Tx                             []byte
	LoadedWritable, LoadedReadonly [][32]byte
}

// locate resolves a signature to its position, or reports that history does
// not hold it.
func (h *History) locate(sig [64]byte, committed uint64) (height uint64, index uint32, ok bool, err error) {
	ptr, err := h.db.Get(pointerKey(sig))
	if err != nil || len(ptr) != 12 {
		return 0, 0, false, err
	}
	height = binary.BigEndian.Uint64(ptr[:8])
	if height <= h.horizon.Load() || height > committed {
		return 0, 0, false, nil
	}
	return height, binary.BigEndian.Uint32(ptr[8:]), true, nil
}

// SignaturesForAddress is getSignaturesForAddress against the address
// keyspace, newest first. It is one reverse prefix scan, because the height
// follows the pubkey in the key.
//
// The bounds are Agave's, from `Blockstore::get_confirmed_signatures_for_address2`:
// an unknown `before` returns nothing, an unknown `until` counts as absent, and
// both signatures are excluded. `until` excludes only what is at or older than
// it, so a later transaction in the same block is still returned.
func (h *History) SignaturesForAddress(address [32]byte, before, until *[64]byte, limit int, committed uint64) ([]SignatureInfo, error) {
	end := addressKey(address, committed+1, 0)
	if before != nil {
		height, index, ok, err := h.locate(*before, committed)
		if err != nil || !ok {
			return nil, err
		}
		end = addressKey(address, height, index)
	}
	start := addressKey(address, h.horizon.Load()+1, 0)
	if until != nil {
		height, index, ok, err := h.locate(*until, committed)
		if err != nil {
			return nil, err
		}
		if ok {
			start = append(addressKey(address, height, index), 0)
		}
	}
	if bytes.Compare(start, end) >= 0 {
		return nil, nil
	}
	it, err := h.db.ReverseIterator(start, end)
	if err != nil {
		return nil, err
	}
	defer it.Close()
	out := make([]SignatureInfo, 0, min(limit, 64))
	for ; it.Valid() && len(out) < limit; it.Next() {
		key := it.Key()
		height, index, ok := splitAddressKey(key)
		if !ok {
			return nil, fmt.Errorf("svm history address entry %x", key)
		}
		info := SignatureInfo{Height: height, Index: index}
		v, err := h.db.Get(historyKey(info.Height, info.Index))
		if err != nil {
			return nil, err
		}
		// A pointer with no record is a purge that raced this scan, not
		// corruption. The horizon moves before the delete, so the caller could
		// not have this row anyway.
		if v == nil {
			continue
		}
		var r Record
		if err := json.Unmarshal(v, &r); err != nil {
			return nil, fmt.Errorf("svm history record %d/%d: %w", info.Height, info.Index, err)
		}
		info.Signature, info.Status, info.BlockTime = r.Signature, r.Status, r.BlockTime
		info.Tx, info.LoadedWritable, info.LoadedReadonly = r.Tx, r.Meta.LoadedWritable, r.Meta.LoadedReadonly
		if r.Status != 0 {
			info.Err = r.Meta.Err
		}
		out = append(out, info)
	}
	return out, nil
}

// AddressEntries counts the address index, and the entries that point at a
// missing primary record. The second number is the important one. It is zero
// unless a purge deleted a record without the entries that name it.
func (h *History) AddressEntries() (total, dangling int, err error) {
	it, err := h.db.Iterator([]byte{'a'}, []byte{'b'})
	if err != nil {
		return 0, 0, err
	}
	defer it.Close()
	for ; it.Valid(); it.Next() {
		key := it.Key()
		height, index, ok := splitAddressKey(key)
		if !ok {
			return 0, 0, fmt.Errorf("svm history address entry %x", key)
		}
		total++
		v, err := h.db.Get(historyKey(height, index))
		if err != nil {
			return 0, 0, err
		}
		if v == nil {
			dangling++
		}
	}
	return total, dangling, nil
}

// deleteSamples deletes the samples at heights in [from, to). They are purged
// with the blocks, as Agave keeps its samples in its blockstore.
func (h *History) deleteSamples(from, to uint64) error {
	end := []byte{'g'}
	if to != ^uint64(0) {
		end = sampleKey(to)
	}
	it, err := h.db.Iterator(sampleKey(from), end)
	if err != nil {
		return err
	}
	batch := h.db.NewBatch()
	defer batch.Close()
	n := uint64(0)
	for ; it.Valid(); it.Next() {
		if err := batch.Delete(append([]byte(nil), it.Key()...)); err != nil {
			it.Close()
			return err
		}
		n++
	}
	it.Close()
	if n == 0 {
		return nil
	}
	h.bytes -= min(n*sampleEntryBytes, h.bytes)
	if err := h.putBytes(batch); err != nil {
		return err
	}
	return batch.WriteSync()
}

// deleteBlocks removes the header hashes for heights in [from, to) and takes
// their bytes off the counter. It is separate from deleteRange because a
// height with no transactions has a header and no records. A purge by records
// alone would never delete the header of an empty block.
func (h *History) deleteBlocks(from, to uint64) error {
	end := []byte{'i'}
	if to != ^uint64(0) {
		end = blockKey(to)
	}
	it, err := h.db.Iterator(blockKey(from), end)
	if err != nil {
		return err
	}
	batch := h.db.NewBatch()
	defer batch.Close()
	n := uint64(0)
	for ; it.Valid(); it.Next() {
		if err := batch.Delete(append([]byte(nil), it.Key()...)); err != nil {
			it.Close()
			return err
		}
		n++
	}
	it.Close()
	if n == 0 {
		return nil
	}
	h.bytes -= min(n*blockEntryBytes, h.bytes)
	if err := h.putBytes(batch); err != nil {
		return err
	}
	return batch.WriteSync()
}

// Blockhash returns the CometBFT header hash of a retained height, which is
// this chain's blockhash. Absent means purged or not yet reached.
func (h *History) Blockhash(height uint64) ([32]byte, bool, error) {
	v, err := h.db.Get(blockKey(height))
	if err != nil {
		return [32]byte{}, false, err
	}
	hash, _, ok := splitBlockValue(v)
	return hash, ok, nil
}

// BlockTime returns the block time of a retained height, the Clock's unix
// timestamp. getBlock answers it for every block, as Agave does. It is absent
// for a purged height.
func (h *History) BlockTime(height uint64) (int64, bool, error) {
	v, err := h.db.Get(blockKey(height))
	if err != nil {
		return 0, false, err
	}
	_, t, ok := splitBlockValue(v)
	return t, ok, nil
}

// FirstAvailableBlock returns the lowest height that history still holds. It is
// the horizon plus one, and the number that a purged getBlock names.
func (h *History) FirstAvailableBlock() uint64 { return h.horizon.Load() + 1 }
