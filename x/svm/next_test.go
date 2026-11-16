package svm

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"cosmossdk.io/log/v2"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/cosmos-sdk/store/v2/cachekv"
	"github.com/cosmos/cosmos-sdk/store/v2/iavl"
	"github.com/cosmos/cosmos-sdk/store/v2/rootmulti"
	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
)

// TestOverReadsAsTheSDKsCacheStore proves that Next.Over reads the same as the
// SDK's cache store with the same writes over the same loaded store. A branch
// of each reads the same after the same writes, nested branches, merges, and
// writes under an open iterator. A branch that writes back panics and leaves
// the overlay as it was.
func TestOverReadsAsTheSDKsCacheStore(t *testing.T) {
	// Each seed loads an IAVL store with about half of 16 keys and puts 20 sets
	// and deletes over it. So a write goes to a new key, an existing key, a key
	// written before, a key deleted and set again, and a key that never
	// existed. Each comparison reads Get and Has for each key, and both
	// iterators over ranges that start or end at nil, at a key, or between
	// keys. It reads every range for the overlay, and 16 random ranges for a
	// branch.
	universe := make([][]byte, 16)
	for i := range universe {
		universe[i] = []byte{byte('a' + 2*i)}
	}
	var bounds [][]byte
	for _, k := range universe {
		bounds = append(bounds, k, []byte{k[0] + 1})
	}
	for seed := range uint64(30) {
		rng := rand.New(rand.NewPCG(seed, 120))
		value := func() []byte { return []byte(fmt.Sprintf("v%d", rng.IntN(1000))) }
		loaded := loadedStore(t, func(s storetypes.KVStore) {
			for _, k := range universe {
				if rng.IntN(2) == 0 {
					s.Set(k, value())
				}
			}
		})
		key := storetypes.NewKVStoreKey("s")
		n, want := &Next{}, cachekv.NewStore(loaded)
		for range 20 {
			k := universe[rng.IntN(len(universe))]
			if rng.IntN(3) == 0 {
				n.writes = append(n.writes, write{key, k, nil})
				want.Delete(k)
			} else {
				v := value()
				n.writes = append(n.writes, write{key, k, v})
				want.Set(k, v)
			}
		}
		over := n.Over(map[storetypes.StoreKey]storetypes.KVStore{key: loaded})[key]
		ranges := allRanges(bounds)
		sameStore(t, fmt.Sprintf("seed %d: the overlay", seed), over, want, universe, ranges)

		// The same writes, in the same order, to a request's branch of each.
		same := func(a, b storetypes.KVStore) {
			k := universe[rng.IntN(len(universe))]
			if rng.IntN(3) == 0 {
				a.Delete(k)
				b.Delete(k)
			} else {
				v := value()
				a.Set(k, v)
				b.Set(k, v)
			}
		}
		got, ref := over.CacheWrap().(storetypes.CacheKVStore), want.CacheWrap().(storetypes.CacheKVStore)
		for step := range 40 {
			what := fmt.Sprintf("seed %d step %d: a branch", seed, step)
			switch rng.IntN(4) {
			case 0, 1:
				same(got, ref)
			case 2:
				// A nested branch, merged into the branch or dropped.
				g, r := got.CacheWrap().(storetypes.CacheKVStore), ref.CacheWrap().(storetypes.CacheKVStore)
				for range rng.IntN(6) + 1 {
					same(g, r)
				}
				sameStore(t, what+", nested", g, r, universe, randomRanges(rng, bounds))
				if rng.IntN(2) == 0 {
					g.Write()
					r.Write()
				}
			case 3:
				// A write while an iterator is open, which the cache store
				// allows, then the rest of each iteration.
				gi, ri := got.Iterator(nil, nil), ref.Iterator(nil, nil)
				for range rng.IntN(4) {
					if gi.Valid() != ri.Valid() || (gi.Valid() && (!bytes.Equal(gi.Key(), ri.Key()) || !bytes.Equal(gi.Value(), ri.Value()))) {
						t.Fatalf("%s: an open iterator differs", what)
					}
					if gi.Valid() {
						gi.Next()
						ri.Next()
					}
				}
				same(got, ref)
				if g, r := rest(gi), rest(ri); !slices.EqualFunc(g, r, pairEqual) {
					t.Fatalf("%s: an iterator written under reads %q, want %q", what, g, r)
				}
			}
			sameStore(t, what, got, ref, universe, randomRanges(rng, bounds))
		}
		sameStore(t, fmt.Sprintf("seed %d: the overlay after its branches", seed), over, want, universe, ranges)

		b := over.CacheWrap().(storetypes.CacheKVStore)
		b.Delete(universe[0])
		b.Set(universe[1], []byte("written back"))
		if !panics(b.Write) {
			t.Fatalf("seed %d: a branch wrote back into the overlay", seed)
		}
		sameStore(t, fmt.Sprintf("seed %d: the overlay after a write back", seed), over, want, universe, ranges)
	}
}

// TestOverCachesNoRead proves that a wrong answer from the loaded store does
// not go to the next request, through an overlay or through a store that the
// begin-block did not write. While IAVL saves the next version, a read at the
// old version can return not found for a key that the new block deletes. A
// cache that all requests at a height share would keep that answer for every
// later request.
func TestOverCachesNoRead(t *testing.T) {
	loaded := loadedStore(t, func(s storetypes.KVStore) {
		s.Set([]byte("k"), []byte("v"))
	})
	written, untouched := storetypes.NewKVStoreKey("written"), storetypes.NewKVStoreKey("untouched")
	n := &Next{writes: []write{{written, []byte("other"), []byte("w")}}}
	for name, k := range map[string]storetypes.StoreKey{"an overlay": written, "an untouched store": untouched} {
		wrong := &wrongOnce{KVStore: loaded}
		s := n.Over(map[storetypes.StoreKey]storetypes.KVStore{written: wrong, untouched: wrong})[k]
		if got := s.CacheWrap().(storetypes.KVStore).Get([]byte("k")); got != nil {
			t.Fatalf("%s: the first read is %q, want the wrong answer", name, got)
		}
		if got := s.CacheWrap().(storetypes.KVStore).Get([]byte("k")); string(got) != "v" {
			t.Fatalf("%s: the next request read %q, want %q", name, got, "v")
		}
	}
}

// wrongOnce answers its first read not found.
type wrongOnce struct {
	storetypes.KVStore
	answered bool
}

func (w *wrongOnce) Get(key []byte) []byte {
	if !w.answered {
		w.answered = true
		return nil
	}
	return w.KVStore.Get(key)
}

func (w *wrongOnce) CacheWrap() storetypes.CacheWrap { return cachekv.NewStore(w) }

// loadedStore returns an IAVL store with what fill writes, saved as version 1
// and loaded at it. The committed view of App loads each store the same way.
func loadedStore(t *testing.T, fill func(storetypes.KVStore)) storetypes.KVStore {
	t.Helper()
	key := storetypes.NewKVStoreKey("s")
	rs := rootmulti.NewStore(dbm.NewMemDB(), log.NewNopLogger())
	rs.MountStoreWithDB(key, storetypes.StoreTypeIAVL, nil)
	if err := rs.LoadLatestVersion(); err != nil {
		t.Fatal(err)
	}
	fill(rs.GetKVStore(key))
	rs.Commit()
	loaded, err := rs.GetStore(key).(*iavl.Store).GetImmutable(1)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

type span struct {
	start, end []byte
	reverse    bool
}

// allRanges is every range bounds make, both ways, nil at either end.
func allRanges(bounds [][]byte) []span {
	ends := append([][]byte{nil}, bounds...)
	var out []span
	for _, s := range ends {
		for _, e := range ends {
			if s != nil && e != nil && bytes.Compare(s, e) >= 0 {
				continue
			}
			out = append(out, span{s, e, false}, span{s, e, true})
		}
	}
	return out
}

func randomRanges(rng *rand.Rand, bounds [][]byte) []span {
	all := allRanges(bounds)
	out := []span{{nil, nil, false}, {nil, nil, true}}
	for range 16 {
		out = append(out, all[rng.IntN(len(all))])
	}
	return out
}

// sameStore fails t unless got reads as want, for each key's Get and Has and
// each range's pairs.
func sameStore(t *testing.T, what string, got, want storetypes.KVStore, keys [][]byte, ranges []span) {
	t.Helper()
	for _, k := range keys {
		g, w := got.Get(k), want.Get(k)
		if !bytes.Equal(g, w) || (g == nil) != (w == nil) {
			t.Fatalf("%s: Get(%q) is %q, want %q", what, k, g, w)
		}
		if got.Has(k) != want.Has(k) {
			t.Fatalf("%s: Has(%q) is %v, want %v", what, k, got.Has(k), want.Has(k))
		}
	}
	for _, r := range ranges {
		open := func(s storetypes.KVStore) storetypes.Iterator {
			if r.reverse {
				return s.ReverseIterator(r.start, r.end)
			}
			return s.Iterator(r.start, r.end)
		}
		if g, w := rest(open(got)), rest(open(want)); !slices.EqualFunc(g, w, pairEqual) {
			t.Fatalf("%s: [%q, %q) reverse=%v reads %q, want %q", what, r.start, r.end, r.reverse, g, w)
		}
	}
}

// rest returns the key and value pairs that it has left, and closes it.
func rest(it storetypes.Iterator) [][2][]byte {
	defer it.Close()
	var out [][2][]byte
	for ; it.Valid(); it.Next() {
		out = append(out, [2][]byte{bytes.Clone(it.Key()), bytes.Clone(it.Value())})
	}
	return out
}

func pairEqual(a, b [2][]byte) bool { return bytes.Equal(a[0], b[0]) && bytes.Equal(a[1], b[1]) }

func panics(f func()) (did bool) {
	defer func() { did = recover() != nil }()
	f()
	return false
}
