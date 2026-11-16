package svm

import (
	"bytes"
	"crypto/sha256"
	"maps"

	"github.com/cosmos/cosmos-sdk/store/v2/cachekv"
	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
)

// NextOwnHash stands for the next block's own hash in a simulation. No header
// hashes to it, so the durable nonce that a simulated advance writes is one
// that no block writes. That is why a nonce advanced in the committed block is
// not refused as already advanced.
var NextOwnHash = sha256.Sum256([]byte("svm: the next block's own hash"))

// Next is the first block that a transaction sent now can execute in. It is
// the block after the committed height, with state as its begin-block leaves
// it.
type Next struct {
	Height uint64
	// Hash is the committed block's hash, which replaceRecentBlockhash writes
	// and the blockhash window starts at.
	Hash  [32]byte
	Gates []ffi.Gate
	// writes is the write set of the begin-block. Over puts it over the same
	// committed state.
	writes []write
}

type write struct {
	store      storetypes.StoreKey
	key, value []byte // a nil value is a delete
}

// beginBlock is what a block does to state before its first transaction. It
// activates the gates due at height, then writes the per-block sysvars.
// BeginBlock runs it on the block. NextBlock runs it on a branch for the block
// after the committed one, so a simulation starts from the same state as
// execution.
func (s Stores) beginBlock(ms storetypes.MultiStore, height uint64, unixTime int64, hash []byte) (bool, error) {
	flipped, err := s.ActivateGates(ms, height, s.PendingGates(ms, height))
	if err != nil {
		return false, err
	}
	return flipped, s.writeSysvars(ms, height, unixTime, hash)
}

// NextBlock runs the begin-block of height on ms, a branch of the state that
// height-1 committed. The header time is the committed block's, read from its
// Clock, because the next block's time is the median of votes that the
// application never sees. The own hash is NextOwnHash. Nothing is minted,
// because the branch is never committed.
func (s Stores) NextBlock(ms storetypes.MultiStore, height uint64) (*Next, error) {
	s.minted = nil
	clock, err := s.accountData(ms, ClockID)
	if err != nil {
		return nil, err
	}
	n := &Next{Height: height, Hash: [32]byte(ms.GetKVStore(s.Meta).Get(MetaBlockhashKey))}
	rec := recorder{ms, &n.writes}
	if _, err := s.beginBlock(rec, height, clockOf(clock.Data).UnixTimestamp, NextOwnHash[:]); err != nil {
		return nil, err
	}
	if n.Gates, err = s.ActiveGates(ms); err != nil {
		return nil, err
	}
	return n, nil
}

// Over returns stores, the state that NextBlock was built on, with the writes
// of the begin-block over the stores that they touch. This is the context of
// the next block. It is built once for each committed height, and every
// request at that height reads it. Nothing in it changes after Over returns,
// no read is cached in it, and a write to it panics. So readers on many
// goroutines can share it.
func (n *Next) Over(stores map[storetypes.StoreKey]storetypes.KVStore) map[storetypes.StoreKey]storetypes.KVStore {
	over := maps.Clone(stores)
	written := map[storetypes.StoreKey]*overlay{}
	for _, w := range n.writes {
		o := written[w.store]
		if o == nil {
			o = &overlay{KVStore: stores[w.store], writes: map[string][]byte{}, merged: cachekv.NewStore(stores[w.store])}
			written[w.store], over[w.store] = o, o
		}
		o.writes[string(w.key)] = w.value
		if w.value == nil {
			o.merged.Delete(w.key)
		} else {
			o.merged.Set(w.key, w.value)
		}
	}
	return over
}

// overlay is a loaded store with the writes of the begin-block over it. A read
// returns the last write to its key if there is one. If not, it reads the
// loaded store. So no read is cached here. An iterator merges the two through
// merged, a cache store that holds the writes and nothing else. merged locks
// when it makes each iterator, and gives each one a copy of its sorted writes.
type overlay struct {
	storetypes.KVStore
	writes map[string][]byte // a nil value is a delete
	merged *cachekv.Store
}

func (o *overlay) Get(key []byte) []byte {
	if v, ok := o.writes[string(key)]; ok {
		return v
	}
	return o.KVStore.Get(key)
}

func (o *overlay) Has(key []byte) bool { return o.Get(key) != nil }

func (o *overlay) Iterator(start, end []byte) storetypes.Iterator {
	return o.merged.Iterator(start, end)
}

func (o *overlay) ReverseIterator(start, end []byte) storetypes.Iterator {
	return o.merged.ReverseIterator(start, end)
}

func (o *overlay) Set(_, _ []byte) { panic("svm: the next block's context is read-only") }

func (o *overlay) Delete([]byte) { panic("svm: the next block's context is read-only") }

// CacheWrap returns the branch that a request reads. It keeps what the request
// reads and writes, over the overlay.
func (o *overlay) CacheWrap() storetypes.CacheWrap { return cachekv.NewStore(o) }

// recorder is a multistore that keeps every write made through it.
type recorder struct {
	storetypes.MultiStore
	writes *[]write
}

func (r recorder) GetKVStore(key storetypes.StoreKey) storetypes.KVStore {
	return recordingStore{r.MultiStore.GetKVStore(key), key, r.writes}
}

type recordingStore struct {
	storetypes.KVStore
	store  storetypes.StoreKey
	writes *[]write
}

func (r recordingStore) Set(key, value []byte) {
	r.KVStore.Set(key, value)
	*r.writes = append(*r.writes, write{r.store, bytes.Clone(key), bytes.Clone(value)})
}

func (r recordingStore) Delete(key []byte) {
	r.KVStore.Delete(key)
	*r.writes = append(*r.writes, write{r.store, bytes.Clone(key), nil})
}
