package svm

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	dbm "github.com/cosmos/cosmos-db"
)

// dedupWindow is the acceptance window in blocks, and the depth of the dedup
// set. At height h a transaction passes the age check only with a blockhash
// from h-150 or later. A transaction that used such a blockhash was processed
// at h-150 or later. Only the buckets for h-150 through h-1 can still match.
const dedupWindow = BlockhashWindow

// InBlockhashWindow reports whether height is inside the window that ends at
// at: no later than at, and fewer than BlockhashWindow blocks before it.
func InBlockhashWindow(at, height uint64) bool {
	return height <= at && height+BlockhashWindow > at
}

// windowStart is the first height of the window that ends at height. It is 0
// while the chain is younger than the window.
func windowStart(height uint64) uint64 {
	if height < BlockhashWindow {
		return 0
	}
	return height - BlockhashWindow + 1
}

// dedupRetained is how long the keys of a height stay in the database, in
// blocks. It is two windows. The set drops a height when it leaves the
// window. But the SDK writes a state sync snapshot of height S on its own
// goroutine while later blocks commit, and its payload is the window that
// ends at S. The extra window of rows lets Export read that window after the
// set has moved on.
const dedupRetained = 2 * dedupWindow

// Dedup is the set of processed (recent_blockhash, message_hash) keys over the
// window. It is outside the commit tree, in a machine-local database beside the
// app's, with one bucket per height:
//
//	'd' || height u64 BE || key  ->  empty
//
// A sync write stores a bucket beside the block's commit. The write lands
// before the app commits the height (Runner.Wait). On startup, Dedup deletes
// buckets above the committed height. They come from a block that finalized
// but did not commit, and CometBFT replays that block. CheckTx reads the set
// on the mempool connection, and FinalizeBlock writes it on the consensus
// connection. So a mutex guards the map.
type Dedup struct {
	db dbm.DB
	mu sync.RWMutex
	// seen is each key's height, indexed by seenKey: 32 bytes, not 64.
	seen map[[32]byte]uint64
	// buckets holds one hash per height in the window, so the per-block
	// commitment hashes 150 * 32 bytes, not the set.
	buckets map[uint64][32]byte
	// kept is the lowest height Export may read. The rows of every height
	// from kept up are still in the database.
	kept uint64
}

// OpenDedup loads the buckets that can still match at the block after
// committed, and deletes every other bucket. A restart ends any export that
// was running, so nothing needs the rows of a height outside the window.
func OpenDedup(db dbm.DB, committed uint64) (*Dedup, error) {
	d := &Dedup{db: db, seen: map[[32]byte]uint64{}, buckets: map[uint64][32]byte{}, kept: windowStart(committed)}
	live := map[uint64][][64]byte{}
	it, err := db.Iterator([]byte{'d'}, []byte{'e'})
	if err != nil {
		return nil, err
	}
	var stale [][]byte
	for ; it.Valid(); it.Next() {
		k := it.Key()
		h, key, ok := splitDedupKey(k)
		if !ok {
			it.Close()
			return nil, fmt.Errorf("svm dedup: malformed key %x", k)
		}
		if !InBlockhashWindow(committed, h) {
			stale = append(stale, append([]byte(nil), k...))
			continue
		}
		d.seen[seenKey(key)] = h
		live[h] = append(live[h], key)
	}
	it.Close()
	for h, keys := range live {
		d.buckets[h] = bucket(keys)
	}
	if len(stale) > 0 {
		batch := db.NewBatch()
		defer batch.Close()
		for _, k := range stale {
			if err := batch.Delete(k); err != nil {
				return nil, err
			}
		}
		if err := batch.WriteSync(); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// Entries returns the size of the replay set, which an operator watches
// against the acceptance window.
func (d *Dedup) Entries() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.seen)
}

// Has reports whether key was processed in the window.
func (d *Dedup) Has(key [64]byte) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.seen[seenKey(key)]
	return ok
}

// Commit records the keys processed at height and writes them. It is Spend,
// then the write that Spend returns.
func (d *Dedup) Commit(height uint64, keys [][64]byte) error {
	save, err := d.Spend(height, keys)
	if err != nil {
		return err
	}
	return save()
}

// Spend records the keys processed at height in the set. It expires from the
// set the bucket that can no longer match at height+1. That is everything the
// commitment at height reads. It returns the database write, which runs beside
// the block's commit. The write stores the keys, and deletes the rows of the
// height that has been out of the window for a whole window.
func (d *Dedup) Spend(height uint64, keys [][64]byte) (func() error, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, k := range keys {
		d.seen[seenKey(k)] = height
	}
	d.buckets[height] = bucket(keys)
	if height >= dedupWindow {
		expired := height - dedupWindow
		old, err := d.keysAt(expired)
		if err != nil {
			return nil, err
		}
		for _, k := range old {
			if s := seenKey(k); d.seen[s] == expired {
				delete(d.seen, s)
			}
		}
		delete(d.buckets, expired)
	}
	return func() error { return d.save(height, keys) }, nil
}

// save is Spend's write. It holds the lock Export opens its iterator under, so
// kept moves with the rows it deletes.
func (d *Dedup) save(height uint64, keys [][64]byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	batch := d.db.NewBatch()
	defer batch.Close()
	for _, k := range keys {
		if err := batch.Set(dedupKey(height, k), []byte{}); err != nil {
			return err
		}
	}
	kept := d.kept
	if height >= dedupRetained {
		gone := height - dedupRetained
		old, err := d.keysAt(gone)
		if err != nil {
			return err
		}
		for _, k := range old {
			if err := batch.Delete(dedupKey(gone, k)); err != nil {
				return err
			}
		}
		kept = max(kept, gone+1)
	}
	if err := batch.WriteSync(); err != nil {
		return err
	}
	d.kept = kept
	return nil
}

// keysAt reads one height's keys from the database, in key order.
func (d *Dedup) keysAt(height uint64) ([][64]byte, error) {
	it, err := d.db.Iterator(dedupKey(height, [64]byte{}), dedupKey(height+1, [64]byte{}))
	if err != nil {
		return nil, err
	}
	defer it.Close()
	var keys [][64]byte
	for ; it.Valid(); it.Next() {
		_, k, ok := splitDedupKey(it.Key())
		if !ok {
			return nil, fmt.Errorf("svm dedup: malformed key %x", it.Key())
		}
		keys = append(keys, k)
	}
	return keys, it.Error()
}

// seenKey is the index of a key in the set: its second half, the message
// hash. The message hash covers the message, and the message holds the recent
// blockhash. So the half is as unique as the whole key. Agave's status cache
// also rejects a replay on the message hash. The rows, the buckets and the
// payloads keep the whole key.
func seenKey(key [64]byte) [32]byte { return [32]byte(key[32:]) }

func dedupKey(height uint64, key [64]byte) []byte {
	k := binary.BigEndian.AppendUint64([]byte{'d'}, height)
	return append(k, key[:]...)
}

// splitDedupKey parses a row key that dedupKey built.
func splitDedupKey(k []byte) (height uint64, key [64]byte, ok bool) {
	if len(k) != 1+8+64 {
		return 0, key, false
	}
	return binary.BigEndian.Uint64(k[1:9]), [64]byte(k[9:]), true
}

// bucket is one height's part of the commitment, the SHA-256 of its sorted
// dedup keys. The sort makes it a function of the set, not of the
// order in which a block processed the keys. The snapshot payload carries the
// same order.
func bucket(keys [][64]byte) [32]byte {
	sorted := append([][64]byte(nil), keys...)
	slices.SortFunc(sorted, func(a, b [64]byte) int { return bytes.Compare(a[:], b[:]) })
	h := sha256.New()
	for _, k := range sorted {
		h.Write(k[:])
	}
	return [32]byte(h.Sum(nil))
}

// emptyBucket is the bucket of a height that processed nothing, and of a height
// before the chain began. It keeps the roll-up exactly 150 hashes wide at every
// height, so the rule needs no special case near genesis.
var emptyBucket = bucket(nil)

// Commitment is:
//
//	bucket[h]   = H( sorted dedup keys processed at height h )
//	commitment  = H( bucket[h-149] || ... || bucket[h] )
//
// It goes in the commit tree, so the app hash covers the set that the machine
// holds, and the set itself stays out of the tree. It costs one write per
// block, whatever the number of transactions in the block.
func (d *Dedup) Commitment(height uint64) [32]byte {
	d.mu.RLock()
	defer d.mu.RUnlock()
	h := sha256.New()
	for i := int64(height) - dedupWindow + 1; i <= int64(height); i++ {
		b := emptyBucket
		if i > 0 {
			if have, ok := d.buckets[uint64(i)]; ok {
				b = have
			}
		}
		h.Write(b[:])
	}
	return [32]byte(h.Sum(nil))
}

// Export writes the window that ends at height as one payload per height, in
// height order, each the big-endian height followed by its sorted 64-byte
// keys.
//
// Export skips heights with no transactions. An absent bucket and an empty one
// hash the same, so the payload does not need them.
//
// The SDK calls Export on its own goroutine after the IAVL export, while later
// blocks commit, so Export reads the database and never the live set. The
// database keeps the rows of a height for a window after the set drops them,
// which makes the payload a function of height alone.
func (d *Dedup) Export(height uint64, write func([]byte) error) error {
	// No block has height 0. A floor of 0 here would fail an export below
	// height 150 once kept reaches 1.
	from := max(windowStart(height), 1)
	// Export opens the iterator under the lock that Commit writes under, so
	// the iterator reads exactly the rows that kept promises.
	d.mu.RLock()
	if from < d.kept {
		kept := d.kept
		d.mu.RUnlock()
		return fmt.Errorf("svm dedup: the window at %d starts at %d, and the rows below %d are gone", height, from, kept)
	}
	it, err := d.db.Iterator(dedupKey(from, [64]byte{}), dedupKey(height+1, [64]byte{}))
	d.mu.RUnlock()
	if err != nil {
		return err
	}
	defer it.Close()

	var payload []byte
	at := uint64(0)
	for ; it.Valid(); it.Next() {
		h, key, ok := splitDedupKey(it.Key())
		if !ok {
			return fmt.Errorf("svm dedup: malformed key %x", it.Key())
		}
		if h != at && payload != nil {
			if err := write(payload); err != nil {
				return err
			}
			payload = nil
		}
		if payload == nil {
			payload, at = binary.BigEndian.AppendUint64(nil, h), h
		}
		payload = append(payload, key[:]...)
	}
	if err := it.Error(); err != nil {
		return err
	}
	if payload != nil {
		return write(payload)
	}
	return nil
}

// Import replaces the set with the payloads of a snapshot and persists it. The
// caller checks the result against the commitment that the restored tree
// carries, before the machine signs anything. Import does not check it,
// because a payload that is only well formed proves nothing.
func (d *Dedup) Import(height uint64, read func() ([]byte, error)) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	batch := d.db.NewBatch()
	defer batch.Close()
	it, err := d.db.Iterator([]byte{'d'}, []byte{'e'})
	if err != nil {
		return err
	}
	for ; it.Valid(); it.Next() {
		if err := batch.Delete(append([]byte(nil), it.Key()...)); err != nil {
			it.Close()
			return err
		}
	}
	it.Close()
	d.seen = map[[32]byte]uint64{}
	d.buckets = map[uint64][32]byte{}

	var last uint64
	for {
		payload, err := read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if len(payload) < 8 || (len(payload)-8)%64 != 0 {
			return fmt.Errorf("svm dedup: payload is %d bytes", len(payload))
		}
		h := binary.BigEndian.Uint64(payload[:8])
		if !InBlockhashWindow(height, h) {
			return fmt.Errorf("svm dedup: payload for height %d, outside the window at %d", h, height)
		}
		// Export writes each height once, in ascending order. If a height
		// came twice, the keys of the first payload would stay in the set,
		// but the bucket would hash only the second. The commitment would then
		// vouch for replay keys that the chain never spent. Agave refuses a
		// snapshot whose status cache repeats a slot in the same way.
		if h <= last {
			return fmt.Errorf("svm dedup: payload for height %d after one for height %d", h, last)
		}
		last = h
		var keys [][64]byte
		for at := 8; at < len(payload); at += 64 {
			key := [64]byte(payload[at : at+64])
			keys = append(keys, key)
			d.seen[seenKey(key)] = h
			if err := batch.Set(dedupKey(h, key), []byte{}); err != nil {
				return err
			}
		}
		d.buckets[h] = bucket(keys)
	}
	if err := batch.WriteSync(); err != nil {
		return err
	}
	d.kept = windowStart(height)
	return nil
}
