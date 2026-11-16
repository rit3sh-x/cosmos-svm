package app_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"sync"
	"testing"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestAViewIsItsHeightsState proves that each view holds the state of its
// height, up to a gate's activation, after other requests write to their own
// branches. A committed branch reads what a new load of that version reads,
// and a next branch reads what the begin-block leaves on a new load. The next
// view before the activation deletes the schedule's key, so the test also
// reads the overlay's deletes.
func TestAViewIsItsHeightsState(t *testing.T) {
	payer := tc.Key(1)
	c := tc.Start(t, tc.Pub(payer), tc.Options{})
	c.Block()
	const flip = 6
	if err := schedule(t, c, flip-svm.MinActivationDelay, flip, cpiNesting); err != nil {
		t.Fatal(err)
	}
	cms := c.App.CommitMultiStore()
	keys := cms.(interface {
		StoreKeysByName() map[string]storetypes.StoreKey
	}).StoreKeysByName()
	committed := func() storetypes.MultiStore {
		ms, _, err := c.App.Committed()
		if err != nil {
			t.Fatal(err)
		}
		return ms
	}
	next := func() storetypes.MultiStore {
		ms, _, err := c.App.Next()
		if err != nil {
			t.Fatal(err)
		}
		return ms
	}
	for c.Height < flip {
		h := c.Height
		want, err := cms.CacheMultiStoreWithVersion(h)
		if err != nil {
			t.Fatal(err)
		}
		wantNext, err := cms.CacheMultiStoreWithVersion(h)
		if err != nil {
			t.Fatal(err)
		}
		n, err := c.App.Stores.NextBlock(wantNext, uint64(h)+1)
		if err != nil {
			t.Fatal(err)
		}
		// The first request at a height runs the begin-block itself. A later
		// request reads the begin-block's writes over the committed view.
		first, got, err := c.App.Next()
		if err != nil {
			t.Fatal(err)
		}
		if got.Height != n.Height || got.Hash != n.Hash || !slices.Equal(got.Gates, n.Gates) {
			t.Fatalf("height %d: the next block is %d %x %v, want %d %x %v", h, got.Height, got.Hash, got.Gates, n.Height, n.Hash, n.Gates)
		}
		sameState(t, fmt.Sprintf("height %d: the first next branch", h), first, wantNext, keys)
		for range 3 {
			scribble(t, committed(), keys)
			scribble(t, next(), keys)
		}
		sameState(t, fmt.Sprintf("height %d: a committed branch", h), committed(), want, keys)
		sameState(t, fmt.Sprintf("height %d: a next branch", h), next(), wantNext, keys)
		c.Block(tc.Tx(c.Latest(), payer, nil, tc.Transfer(payer, tc.Pub(tc.Key(2)), 1_000_000)))
	}
	if at, on := activeAt(t, c, cpiNesting); !on || at != flip {
		t.Fatalf("the gate is active=%v at %d, want true at %d", on, at, flip)
	}
}

// TestReadsStayAtTheirHeightAndApartAcrossCommits proves that the views hold
// under load. Eight goroutines read both views while ten blocks commit, on
// goleveldb as svmd runs, and each branch reads one height and keeps its own
// writes. Run it with -race.
func TestReadsStayAtTheirHeightAndApartAcrossCommits(t *testing.T) {
	payer := tc.Key(1)
	c := tc.Start(t, tc.Pub(payer), tc.Options{Disk: true})
	c.Block()
	to := tc.Pub(tc.Key(2))
	start := uint64(c.Height)
	stores := []storetypes.StoreKey{c.App.Stores.Accounts[svm.Shard(to)], c.App.Stores.Meta}

	var mu sync.Mutex
	loads := map[uint64]*load{}
	loadAt := func(h uint64) *load {
		mu.Lock()
		defer mu.Unlock()
		if loads[h] == nil {
			loads[h] = &load{taken: make(chan struct{})}
		}
		return loads[h]
	}
	// take loads a height between commits, when no commit saves. The readers
	// read while later blocks commit.
	take := func() {
		h := uint64(c.Height)
		l := loadAt(h)
		for _, next := range []bool{false, true} {
			ms, err := c.App.CommitMultiStore().CacheMultiStoreWithVersion(int64(h))
			if err != nil {
				t.Fatal(err)
			}
			if next {
				if _, err := c.App.Stores.NextBlock(ms, h+1); err != nil {
					t.Fatal(err)
				}
			}
			for i, k := range stores {
				l.entries[b2i(next)][i] = entries(ms.GetKVStore(k).Iterator(nil, nil))
			}
		}
		close(l.taken)
	}
	take()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	defer func() {
		close(stop)
		wg.Wait()
	}()
	for r := range 8 {
		mine := tc.Pub(tc.Key(byte(100 + r)))
		wg.Go(func() {
			for {
				for _, next := range []bool{false, true} {
					select {
					case <-stop:
						return
					default:
					}
					if err := readOneHeight(c, next, start, to, mine, stores, loadAt, stop); err != nil {
						t.Error(err)
						return
					}
				}
			}
		})
	}
	// CheckTx admits a transfer before each block, so the engine holds the root
	// of the next height in whatever order the readers give it heights.
	for range 10 {
		tx := tc.Tx(c.Latest(), payer, nil, tc.Transfer(payer, to, 1_000_000))
		if res := c.Check(c.Wrap(tx), false); res.Code != 0 {
			t.Fatalf("height %d: the transfer was not admitted: %s", c.Height, res.Log)
		}
		if res, _ := c.Block(tx); res[0].Code != 0 {
			t.Fatalf("height %d: the transfer failed: %s", c.Height, res[0].Log)
		}
		take()
	}
}

// load holds what a new load of one height reads from each store that the
// readers check: entries[0] for committed state, entries[1] for the context of
// the next block. taken closes when it is filled.
type load struct {
	taken   chan struct{}
	entries [2][2][][2][]byte
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// readOneHeight reads a branch of one view and checks it against its height.
// The Clock is at that height, or one more in the next view, and the recipient
// holds what the committed transfers sent. The recipient's shard and svmmeta
// read, key by key, what a new load of that version read, with the begin-block
// run on it for the next view. A write to the branch must not reach the next
// request's branch.
func readOneHeight(c *tc.Chain, next bool, start uint64, to, mine [32]byte, stores []storetypes.StoreKey,
	loadAt func(uint64) *load, stop <-chan struct{}) error {
	open := func() (storetypes.MultiStore, uint64, error) {
		if next {
			ms, n, err := c.App.Next()
			if err != nil {
				return nil, 0, err
			}
			return ms, n.Height - 1, nil
		}
		return c.App.Committed()
	}
	ms, h, err := open()
	if err != nil {
		return err
	}
	what, slot := fmt.Sprintf("height %d, committed", h), h
	if next {
		what, slot = fmt.Sprintf("height %d, next", h), h+1
	}
	clock, err := ffi.DecodeAccount(c.App.Stores.GetAccount(ms, svm.ClockID))
	if err != nil {
		return fmt.Errorf("%s: the Clock: %w", what, err)
	}
	if got := binary.LittleEndian.Uint64(clock.Data[0:8]); got != slot {
		return fmt.Errorf("%s: the Clock is at slot %d, want %d", what, got, slot)
	}
	held := c.App.Stores.GetAccount(ms, to)
	if sent := (h - start) * 1_000_000; sent == 0 && held != nil {
		return fmt.Errorf("%s: the recipient exists before any transfer", what)
	} else if sent > 0 {
		if a, err := ffi.DecodeAccount(held); err != nil || a.Lamports != sent {
			return fmt.Errorf("%s: the recipient holds %d (%v), want %d", what, a.Lamports, err, sent)
		}
	}
	got := make([][][2][]byte, len(stores))
	for i, k := range stores {
		got[i] = entries(ms.GetKVStore(k).Iterator(nil, nil))
	}
	l := loadAt(h)
	select {
	case <-l.taken:
	case <-stop:
		return nil
	}
	for i, k := range stores {
		if want := l.entries[b2i(next)][i]; !slices.EqualFunc(got[i], want, func(a, b [2][]byte) bool {
			return bytes.Equal(a[0], b[0]) && bytes.Equal(a[1], b[1])
		}) {
			return fmt.Errorf("%s: store %s reads %d entries unlike a fresh load's %d", what, k.Name(), len(got[i]), len(want))
		}
	}

	c.App.Stores.SetAccount(ms, mine, []byte{1, 0, 0, 0, 0, 0, 0, 0})
	again, _, err := open()
	if err != nil {
		return err
	}
	if c.App.Stores.GetAccount(again, mine) != nil {
		return fmt.Errorf("%s: one request's write is in the next request's branch", what)
	}
	return nil
}

// entries returns what it reads, as key and value pairs, and closes it.
func entries(it storetypes.Iterator) [][2][]byte {
	defer it.Close()
	var out [][2][]byte
	for ; it.Valid(); it.Next() {
		out = append(out, [2][]byte{bytes.Clone(it.Key()), bytes.Clone(it.Value())})
	}
	return out
}

// scribble writes to each store of the branch of one request: a new key, a
// delete of the first key in the store, and a new value for the second key.
func scribble(t *testing.T, ms storetypes.MultiStore, keys map[string]storetypes.StoreKey) {
	t.Helper()
	for _, k := range keys {
		s := ms.GetKVStore(k)
		it := s.Iterator(nil, nil)
		var held [][]byte
		for ; it.Valid() && len(held) < 2; it.Next() {
			held = append(held, bytes.Clone(it.Key()))
		}
		it.Close()
		s.Set([]byte("scribbled"), []byte("by one request"))
		if len(held) > 0 {
			s.Delete(held[0])
		}
		if len(held) > 1 {
			s.Set(held[1], []byte("by one request"))
		}
	}
}

// sameState checks every store of got against want: both iterators over the
// whole store and over the keys between its first and second third, and Get
// and Has for each key that want holds and for one key that it does not.
func sameState(t *testing.T, what string, got, want storetypes.MultiStore, keys map[string]storetypes.StoreKey) {
	t.Helper()
	for name, k := range keys {
		g, w := got.GetKVStore(k), want.GetKVStore(k)
		var held [][]byte
		it := w.Iterator(nil, nil)
		for ; it.Valid(); it.Next() {
			held = append(held, bytes.Clone(it.Key()))
		}
		it.Close()
		ranges := [][2][]byte{{nil, nil}}
		if len(held) >= 3 {
			ranges = append(ranges, [2][]byte{held[len(held)/3], held[2*len(held)/3]})
		}
		for _, r := range ranges {
			if err := sameIteration(g.Iterator(r[0], r[1]), w.Iterator(r[0], r[1])); err != nil {
				t.Fatalf("%s: store %s, [%x, %x): %v", what, name, r[0], r[1], err)
			}
			if err := sameIteration(g.ReverseIterator(r[0], r[1]), w.ReverseIterator(r[0], r[1])); err != nil {
				t.Fatalf("%s: store %s, [%x, %x) reversed: %v", what, name, r[0], r[1], err)
			}
		}
		for _, key := range append(held, []byte("scribbled")) {
			gv, wv := g.Get(key), w.Get(key)
			if !bytes.Equal(gv, wv) || (gv == nil) != (wv == nil) || g.Has(key) != w.Has(key) {
				t.Fatalf("%s: store %s: key %x reads %d bytes, want %d", what, name, key, len(gv), len(wv))
			}
		}
	}
}

// sameIteration reads a and b together and closes both.
func sameIteration(a, b storetypes.Iterator) error {
	defer a.Close()
	defer b.Close()
	for i := 0; ; i++ {
		if a.Valid() != b.Valid() {
			return fmt.Errorf("entry %d: one iterator ended before the other", i)
		}
		if !a.Valid() {
			return nil
		}
		if !bytes.Equal(a.Key(), b.Key()) || !bytes.Equal(a.Value(), b.Value()) {
			return fmt.Errorf("entry %d: key %x, want %x", i, a.Key(), b.Key())
		}
		a.Next()
		b.Next()
	}
}
