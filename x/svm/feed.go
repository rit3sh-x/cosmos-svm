package svm

import (
	"math"
	"sync"
	"sync/atomic"

	"github.com/rit3sh-x/cosmos-svm/ffi"
)

// Feed carries a block's account write set from the commit path to the
// machine-local notifier, with the lowest compute unit price that its
// transactions declared. It is outside the commit tree and outside the app
// hash, so nothing read from it can reach a consensus result.
//
// The runner cannot notify a subscriber itself. executeBlock runs inside
// FinalizeBlock, so an account pushed from there reaches a client before the
// height commits, and a client that reads it back does not see it. The runner
// hands the write set over here instead. The loop that refuses to run ahead of
// the committed height picks it up after the commit.
//
// The feed copies a written account only when a subscription watches it. An
// account subscription watches its key. A program subscription watches the
// owner in the account value. With no subscriber, Put gets an empty set and
// the block copies nothing. With subscribers, the copy holds only what they
// watch, not the whole write set of the block.
type Feed struct {
	subs atomic.Int64

	mu  sync.Mutex
	buf map[uint64]Published
	// accounts holds the keys that account subscriptions watch. owners
	// holds the programs that program subscriptions watch. Each key has its
	// holder count.
	accounts, owners map[[32]byte]int

	// received is the notifier's receivedSignature hook. It is called with a
	// block's signatures after the block executes and before it commits. It
	// is nil unless a subscriber asked for one, so a block pays one atomic load.
	received atomic.Pointer[func(height uint64, sigs [][64]byte)]

	// reached is the highest height whose block has reached this machine,
	// before it commits. It is Agave's max shred insert slot.
	reached atomic.Uint64
}

// Reach records that the block at height has reached this machine.
func (f *Feed) Reach(height uint64) {
	if f == nil {
		return
	}
	for {
		old := f.reached.Load()
		if height <= old || f.reached.CompareAndSwap(old, height) {
			return
		}
	}
}

// Reached returns the highest height that Reach recorded, or 0 before the first.
func (f *Feed) Reached() uint64 {
	if f == nil {
		return 0
	}
	return f.reached.Load()
}

// OnReceived sets or clears the receivedSignature hook.
func (f *Feed) OnReceived(fn func(height uint64, sigs [][64]byte)) {
	if f == nil {
		return
	}
	if fn == nil {
		f.received.Store(nil)
		return
	}
	f.received.Store(&fn)
}

// Received hands the hook the signatures of a finalizing block, the first
// signature of each transaction. It calls sigs only when a hook is set.
func (f *Feed) Received(height uint64, sigs func() [][64]byte) {
	if f == nil {
		return
	}
	if fn := f.received.Load(); fn != nil {
		(*fn)(height, sigs())
	}
}

// AccountValue is one entry of the engine's stored set: the pubkey and the
// account bytes. They are copied out of the response arena, which is freed
// when executeBlock returns.
type AccountValue struct {
	Pubkey [32]byte
	Value  []byte
}

// Published is one height as the runner gives it to the notifier. Writes holds
// the written accounts that a subscription watched when the block executed.
// Lowest is the lowest compute unit price that the processed transactions
// declared, and the fee window keeps it. Priced is false for a block that
// processed no transaction.
type Published struct {
	Writes []AccountValue
	Lowest uint64
	Priced bool
}

// LowestPrice returns the lowest compute unit price that records declared, or
// false when there are no records. This is the block minimum of
// getRecentPrioritizationFees.
func LowestPrice(records []Record) (uint64, bool) {
	if len(records) == 0 {
		return 0, false
	}
	lowest := records[0].Price
	for _, r := range records[1:] {
		lowest = min(lowest, r.Price)
	}
	return lowest, true
}

// maxFeedHeights bounds the buffer. The drain ticks every 200 ms and a block
// takes seconds, so a reader more than this many heights behind has stopped.
// Put then drops the oldest heights and does not grow the buffer. A drop loses
// notifications for those heights. It does not lose state, which is committed
// in both cases. It does not lose a price, because the reader then takes the
// price from the history.
const maxFeedHeights = 64

func NewFeed() *Feed {
	return &Feed{buf: map[uint64]Published{}, accounts: map[[32]byte]int{}, owners: map[[32]byte]int{}}
}

// Hold adds a subscriber, and Release removes one. Each also counts the
// watched key: the key of an account subscription, or the program of a
// program subscription, which is the owner that a written account names. A
// nil feed is a machine that serves no RPC.
func (f *Feed) Hold(key [32]byte, program bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watching(program)[key]++
	f.subs.Add(1)
}

func (f *Feed) Release(key [32]byte, program bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := f.watching(program)
	if keys[key]--; keys[key] <= 0 {
		delete(keys, key)
	}
	f.subs.Add(-1)
}

func (f *Feed) watching(program bool) map[[32]byte]int {
	if program {
		return f.owners
	}
	return f.accounts
}

// Watches reports whether a subscription watches a written account, by its
// key or by the owner in its value. The runner calls it only when Wants is
// true.
func (f *Feed) Watches(pubkey [32]byte, value []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.accounts[pubkey] > 0 {
		return true
	}
	a, err := ffi.DecodeAccount(value)
	return err == nil && f.owners[a.Owner] > 0
}

// Wants reports whether any subscriber needs the write set. It is one atomic
// load on the consensus path.
func (f *Feed) Wants() bool { return f != nil && f.subs.Load() > 0 }

// Put publishes a height: the watched writes and the lowest price of its
// records. The runner puts every executed height, empty or not. So the
// notifier decodes the records of a height only when a subscriber reads them.
func (f *Feed) Put(height uint64, writes []AccountValue, records []Record) {
	if f == nil {
		return
	}
	p := Published{Writes: writes}
	p.Lowest, p.Priced = LowestPrice(records)
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.buf) >= maxFeedHeights {
		oldest := uint64(math.MaxUint64)
		for h := range f.buf {
			oldest = min(oldest, h)
		}
		delete(f.buf, oldest)
	}
	f.buf[height] = p
}

// Take removes and returns a height. It returns false when the feed does not
// hold the height: a height that this process did not execute, or a height
// dropped from a full buffer.
func (f *Feed) Take(height uint64) (Published, bool) {
	if f == nil {
		return Published{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.buf[height]
	delete(f.buf, height)
	return p, ok
}

// Buffered returns the number of written accounts held. A test checks that
// it is zero when nobody subscribes, and that it is the watched count when
// somebody does.
func (f *Feed) Buffered() int {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.buf {
		n += len(p.Writes)
	}
	return n
}
