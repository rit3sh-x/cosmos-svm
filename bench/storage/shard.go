// This file benchmarks sharded IAVL commits for the x/svm account store.
// Commitment takes 85 to 97% of transfer-block execution, so sharding is the
// next step after one IAVL tree.
//
// The benchmark models what rootmulti does:
//   - All stores share one database, separated by key prefixes. Sharding
//     makes smaller trees, not separate disks.
//   - The current SDK commits stores serially. The serial and parallel
//     variants measure what parallel commits can gain.
//   - Each store gets the same IAVL cache size, so the total cache grows
//     with the shard count.
//
// Commit order is deterministic: rootmulti sorts store keys and commit
// metadata before it computes the app hash.
package main

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/iavl"
	idb "github.com/cosmos/iavl/db"
)

// shardRounds is smaller than the single-tree round count, because the grid is
// 4 times larger and a batch of 10,000 into 16 trees is slow. The result is
// still a median after warmup. shardRounds is a var so that -rounds can drive
// the cache to steady state. The node cache is an LRU keyed by node key. Each
// commit makes about 20 new keys per write, and the LRU evicts nothing until it
// is full. So a short run measures a cache that holds every node written.
var (
	shardRounds = 15
	shardWarmup = 3
)

// keepGC leaves the collector enabled during measurement. Set by -gc.
var keepGC bool

// seqKeys swaps uniformly random account keys for the single-tree benchmark's
// dense sequential ones. Set by -keys=sequential.
var seqKeys bool

// shardOf models shard = f(pubkey). Real Solana pubkeys are uniformly
// distributed, so production can take the top bits of the key. shardOf hashes
// the key instead, because the -keys=sequential keys are not uniform. The output
// reports the shard balance, so the reader can check that it is even.
func shardOf(k []byte, n int) int {
	if n == 1 {
		return 0
	}
	h := fnv.New64a()
	h.Write(k)
	return int(h.Sum64() % uint64(n))
}

type shardSet struct {
	trees []*iavl.MutableTree
	close func()
}

// openShards builds n IAVL trees over one database, each behind its own key
// prefix, the way rootmulti does it.
func openShards(backend, dir string, n, cacheSize int) *shardSet {
	var base dbm.DB
	var closeFn func()

	switch backend {
	case "mem":
		base = dbm.NewMemDB()
		closeFn = func() {}
	case "pebble":
		d, err := dbm.NewPebbleDB("bench", dir, nil)
		if err != nil {
			panic(err)
		}
		base = d
		closeFn = func() { d.Close(); os.RemoveAll(dir) }
	default:
		panic("unknown backend " + backend)
	}

	trees := make([]*iavl.MutableTree, n)
	for i := range trees {
		prefixed := dbm.NewPrefixDB(base, []byte(fmt.Sprintf("s/k:svm%02d/", i)))
		trees[i] = iavl.NewMutableTree(idb.NewWrapper(prefixed), cacheSize, false, iavl.NewNopLogger())
	}
	return &shardSet{trees: trees, close: closeFn}
}

func (s *shardSet) saveAll() {
	for _, t := range s.trees {
		if _, _, err := t.SaveVersion(); err != nil {
			panic(err)
		}
	}
}

type shardResult struct {
	set, commit, block, per time.Duration
	writesPerSec            float64
	allocPerWrite           float64
	heapMB                  uint64
}

// runSharded preloads once and then measures every variant against the same
// trees. A preload of one million accounts per grid cell would dominate the run
// time and change no answer.
func runSharded(backend string, preload, batch, cacheSize, shards int) {
	dir, _ := os.MkdirTemp("", "iavlshard")
	set := openShards(backend, dir, shards, cacheSize)
	defer set.close()

	plan := newPlan(preload, batch, shardRounds+shardWarmup)

	// preload, untimed
	counts := make([]int, shards)
	for i := 0; i < preload; i++ {
		k := plan.acct[i]
		s := shardOf(k, shards)
		counts[s]++
		if _, err := set.trees[s].Set(k, plan.vals[i%valuePool]); err != nil {
			panic(err)
		}
		if i%50_000 == 49_999 {
			set.saveAll()
		}
	}
	set.saveAll()

	lo, hi := counts[0], counts[0]
	for _, c := range counts {
		if c < lo {
			lo = c
		}
		if c > hi {
			hi = c
		}
	}

	for _, p := range patterns {
		plan.route(p, shards)
		for _, parallel := range []bool{false, true} {
			r := measureShards(set, plan, batch, parallel)
			mode := "serial"
			if parallel {
				// Set and commit interleave across goroutines and cannot be
				// separated. The whole cost goes in set_p50_us, and
				// commit_p50_us is 0.
				mode = "parallel"
			}
			// block is the sum of the two medians, as in the single-tree mode.
			fmt.Printf("exp=sharded backend=%s pattern=%s accounts=%d shards=%d mode=%s cache=%d batch=%d rounds=%d "+
				"set_p50_us=%d commit_p50_us=%d block_us=%d per_write_ns=%d writes_per_s=%.0f alloc_b_per_write=%.0f heap_mb=%d "+
				"balance_min=%d balance_max=%d\n",
				backend, p.name, preload, shards, mode, cacheSize, batch, shardRounds,
				r.set.Microseconds(), r.commit.Microseconds(), r.block.Microseconds(), r.per.Nanoseconds(),
				r.writesPerSec, r.allocPerWrite, r.heapMB, lo, hi)
		}
	}
}

func measureShards(set *shardSet, plan *writePlan, batch int, parallel bool) shardResult {
	shards := len(set.trees)

	// Buckets are reused across rounds so the timed region never allocates them.
	buckets := make([][]int, shards)
	for i := range buckets {
		buckets[i] = make([]int, 0, batch)
	}

	apply := func(round int) (time.Duration, time.Duration) {
		off := round * batch
		for i := range buckets {
			buckets[i] = buckets[i][:0]
		}
		for j := 0; j < batch; j++ {
			s := plan.shard[off+j]
			buckets[s] = append(buckets[s], off+j)
		}

		if parallel {
			// One goroutine per shard runs both phases. This is what a patched
			// commitStores can gain. It is the optimistic bound, because the
			// database is still shared.
			t0 := time.Now()
			var wg sync.WaitGroup
			for s := 0; s < shards; s++ {
				wg.Add(1)
				go func(s int) {
					defer wg.Done()
					for _, idx := range buckets[s] {
						if _, err := set.trees[s].Set(plan.keys[idx], plan.vals[plan.vidx[idx]]); err != nil {
							panic(err)
						}
					}
					if _, _, err := set.trees[s].SaveVersion(); err != nil {
						panic(err)
					}
				}(s)
			}
			wg.Wait()
			return time.Since(t0), 0
		}

		t0 := time.Now()
		for s := 0; s < shards; s++ {
			for _, idx := range buckets[s] {
				if _, err := set.trees[s].Set(plan.keys[idx], plan.vals[plan.vidx[idx]]); err != nil {
					panic(err)
				}
			}
		}
		t1 := time.Now()
		set.saveAll()
		return t1.Sub(t0), time.Since(t1)
	}

	for i := 0; i < shardWarmup; i++ {
		apply(i)
	}

	runtime.GC()
	// Long runs cannot disable GC. Allocation is about 13 KB per write, so 150
	// rounds of 10,000 writes need more RAM than the bench machine has. Long runs
	// pay the GC pauses, and the median absorbs them. -gc keeps GC on at any round
	// count, to separate the collector's cost from the LRU reaching steady state.
	// The nodes that the cache keeps are live heap that the collector scans.
	prevGC := debug.SetGCPercent(100)
	if shardRounds <= 15 && !keepGC {
		debug.SetGCPercent(-1)
	}
	var ms0, ms1 runtime.MemStats
	runtime.ReadMemStats(&ms0)

	sets := make([]time.Duration, shardRounds)
	commits := make([]time.Duration, shardRounds)
	for i := 0; i < shardRounds; i++ {
		sets[i], commits[i] = apply(shardWarmup + i)
	}

	runtime.ReadMemStats(&ms1)
	debug.SetGCPercent(prevGC)

	ms := median(sets)
	mc := median(commits)
	block := ms + mc
	return shardResult{
		set:           ms,
		commit:        mc,
		block:         block,
		per:           time.Duration(int64(block) / int64(batch)),
		writesPerSec:  float64(time.Second) / (float64(block) / float64(batch)),
		allocPerWrite: float64(ms1.TotalAlloc-ms0.TotalAlloc) / float64(shardRounds*batch),
		heapMB:        ms1.HeapAlloc >> 20,
	}
}

// writePlan is the write sequence, made before measurement. It holds everything
// the timed loop needs, including the shard of each write, so the measurement
// contains IAVL and nothing else.
type writePlan struct {
	vals  [][]byte
	acct  [][]byte // the account keys themselves, indexed 0..pre-1
	keys  [][]byte
	vidx  []int
	shard []int
	total int
	pre   int
}

func newPlan(preload, batch, rounds int) *writePlan {
	r := rand.New(rand.NewSource(1))
	vals := buildValues(r)

	// Real Solana pubkeys are uniformly distributed 32-byte values, so these
	// account keys are too. The single-tree benchmark uses dense sequential keys.
	// They share a 24-byte zero prefix and divide exactly across any
	// power-of-two shard count. That is fine for one tree, but it misleads when
	// the question is how keys distribute across N trees.
	acct := make([][]byte, preload)
	for i := range acct {
		if seqKeys {
			// Control: the single-tree benchmark's key shape. It separates the
			// effect of key distribution from the effect of the prefix database
			// in front of every rootmulti store.
			acct[i] = key(uint64(i))
			continue
		}
		k := make([]byte, 32)
		r.Read(k)
		acct[i] = k
	}

	return &writePlan{
		vals:  vals,
		acct:  acct,
		total: rounds * batch,
		pre:   preload,
	}
}

// route makes the write sequence again for one access pattern. The seed depends
// on the pattern, so each pattern is reproducible alone, whatever ran before it.
func (p *writePlan) route(pt pattern, shards int) {
	r := rand.New(rand.NewSource(int64(len(pt.name))*1000 + 7))
	p.keys = make([][]byte, p.total)
	p.vidx = make([]int, p.total)
	p.shard = make([]int, p.total)

	hotN := int(float64(p.pre) * pt.hotFrac)
	for i := 0; i < p.total; i++ {
		var idx uint64
		if hotN > 0 && r.Float64() < pt.hotShare {
			idx = uint64(r.Intn(hotN))
		} else {
			idx = uint64(r.Intn(p.pre))
		}
		k := p.acct[idx]
		p.keys[i] = k
		p.vidx[i] = r.Intn(valuePool)
		p.shard[i] = shardOf(k, shards)
	}
}
