// Package main is an IAVL commitment-cost benchmark for the x/svm account store.
//
// It measures the cost to put one Solana account into the Cosmos commitment tree.
// The method:
//   - It makes values and key indices before the timed loop, so the loop measures IAVL only.
//   - Warmup rounds run before measurement.
//   - The result is the median over rounds, not the mean, so one GC pause does not set it.
//   - GC runs once, then stays off during measurement. It reports allocations separately.
//   - Two access patterns run, because the difference between them is the cache and TLB effect.
//   - The IAVL node cache size varies, because it sets the hit rate.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"time"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/iavl"
	idb "github.com/cosmos/iavl/db"
)

const (
	acctSize  = 165 // typical SPL token account
	valuePool = 1 << 16
	rounds    = 25
	warmup    = 5
)

type pattern struct {
	name string
	// hotFrac is the fraction of accounts that are hot. 0 means uniform random.
	hotFrac  float64
	hotShare float64 // fraction of writes that land in the hot set
}

var patterns = []pattern{
	{"uniform", 0, 0},        // worst case: every write is a cold random probe
	{"hot-1pct", 0.01, 0.90}, // realistic: AMM pools, mints, popular PDAs
}

func key(i uint64) []byte {
	k := make([]byte, 32)
	binary.BigEndian.PutUint64(k[24:], i)
	return k
}

func buildValues(r *rand.Rand) [][]byte {
	vals := make([][]byte, valuePool)
	for i := range vals {
		v := make([]byte, acctSize)
		r.Read(v)
		vals[i] = v
	}
	return vals
}

func median(d []time.Duration) time.Duration {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[len(d)/2]
}

func openDB(backend, dir string) (idb.DB, func()) {
	switch backend {
	case "mem":
		return idb.NewMemDB(), func() {}
	case "pebble":
		d, err := dbm.NewPebbleDB("bench", dir, nil)
		if err != nil {
			panic(err)
		}
		return idb.NewWrapper(d), func() { d.Close(); os.RemoveAll(dir) }
	}
	panic("unknown backend " + backend)
}

func run(backend string, preload, batch, cacheSize int, p pattern) {
	dir, _ := os.MkdirTemp("", "storagebench")
	db, closeDB := openDB(backend, dir)
	defer closeDB()

	tree := iavl.NewMutableTree(db, cacheSize, false, iavl.NewNopLogger())
	r := rand.New(rand.NewSource(1))
	vals := buildValues(r)

	// preload, untimed
	for i := 0; i < preload; i++ {
		if _, err := tree.Set(key(uint64(i)), vals[i%valuePool]); err != nil {
			panic(err)
		}
		if i%50_000 == 49_999 {
			if _, _, err := tree.SaveVersion(); err != nil {
				panic(err)
			}
		}
	}
	if _, _, err := tree.SaveVersion(); err != nil {
		panic(err)
	}

	// Make the whole write plan first, so the timed loop touches only IAVL.
	total := (warmup + rounds) * batch
	keys := make([][]byte, total)
	vidx := make([]int, total)
	hotN := int(float64(preload) * p.hotFrac)
	for i := 0; i < total; i++ {
		var idx uint64
		if hotN > 0 && r.Float64() < p.hotShare {
			idx = uint64(r.Intn(hotN))
		} else {
			idx = uint64(r.Intn(preload))
		}
		keys[i] = key(idx)
		vidx[i] = r.Intn(valuePool)
	}

	apply := func(round int) (time.Duration, time.Duration) {
		off := round * batch
		t0 := time.Now()
		for j := 0; j < batch; j++ {
			if _, err := tree.Set(keys[off+j], vals[vidx[off+j]]); err != nil {
				panic(err)
			}
		}
		t1 := time.Now()
		if _, _, err := tree.SaveVersion(); err != nil {
			panic(err)
		}
		return t1.Sub(t0), time.Since(t1)
	}

	for i := 0; i < warmup; i++ {
		apply(i)
	}

	runtime.GC()
	prevGC := debug.SetGCPercent(-1) // no GC pauses inside the measurement
	var ms0, ms1 runtime.MemStats
	runtime.ReadMemStats(&ms0)

	sets := make([]time.Duration, rounds)
	commits := make([]time.Duration, rounds)
	for i := 0; i < rounds; i++ {
		sets[i], commits[i] = apply(warmup + i)
	}

	runtime.ReadMemStats(&ms1)
	debug.SetGCPercent(prevGC)

	ms := median(sets)
	mc := median(commits)
	block := ms + mc
	per := time.Duration(int64(block) / int64(batch))
	allocPerWrite := float64(ms1.TotalAlloc-ms0.TotalAlloc) / float64(rounds*batch)

	// block is the sum of the two medians, not a median of block times. GC is
	// off while measuring, so heap_mb is not a live heap.
	fmt.Printf("exp=single backend=%s pattern=%s accounts=%d batch=%d cache=%d rounds=%d "+
		"set_p50_us=%d commit_p50_us=%d block_us=%d per_write_ns=%d writes_per_s=%.0f alloc_b_per_write=%.0f heap_mb=%d\n",
		backend, p.name, preload, batch, cacheSize, rounds,
		ms.Microseconds(), mc.Microseconds(), block.Microseconds(), per.Nanoseconds(),
		float64(time.Second)/float64(per), allocPerWrite, ms1.HeapAlloc>>20)
}

func main() {
	backend := flag.String("backend", "mem", "mem|pebble")
	preload := flag.Int("preload", 1_000_000, "accounts preloaded")
	mode := flag.String("mode", "single", "single (one tree) | shard (the sharding grid) | store (the shipped store: cache, prune, export, alloc, shard, read)")
	keys := flag.String("keys", "random", "shard mode only: random (real pubkeys) | sequential (the single-tree benchmark's keys)")
	shardsOnly := flag.Int("shards", 0, "shard mode only: run only this shard count, 0 for the full grid")
	cacheFlag := flag.Int("cache", 0, "shard mode only: node cache per tree, 0 for 1000000")
	gcOn := flag.Bool("gc", false, "shard mode only: leave GC enabled during measurement")
	rounds := flag.Int("rounds", 0, "measured rounds; 0 means 15 in shard mode, 150 in store mode")
	exp := flag.String("exp", "cache", "store mode only: cache|prune|export|alloc|shard|read")
	caches := flag.String("caches", "1000000", "store mode only: node cache sizes; prune, export and alloc use the first")
	keeps := flag.String("keeps", "0,2,1000", "store mode only: keep-recent values for -exp prune, 0 retains everything")
	allocs := flag.String("allocs", "10x10485760,1000x104857,606060x165", "store mode only: COUNTxBYTES new-account block shapes for -exp alloc")
	pats := flag.String("patterns", "uniform,hot-1pct", "store mode only: access patterns")
	raw := flag.Int("batch", 10_000, "store mode only: account writes per block before cachekv deduplication")
	warmupFlag := flag.Int("warmup", 10, "store mode only: unmeasured rounds per cell")
	dir := flag.String("dir", os.TempDir(), "store mode only: where seeds and cells live; put it on the disk being measured")
	flag.IntVar(&pruneInterval, "prune-interval", pruneInterval, "store mode only: prune every N blocks (app.toml pruning-interval)")
	flag.BoolVar(&syncPrune, "sync-prune", syncPrune, "store mode only: prune inside commit (app.toml iavl-sync-pruning)")
	flag.BoolVar(&noFastNode, "no-fastnode", noFastNode, "store mode only: disable the fast node index (app.toml iavl-disable-fastnode)")
	flag.StringVar(&seriesDir, "series", seriesDir, "store mode only: write a per-block CSV per cell into this directory")
	flag.StringVar(&shardCounts, "shard-counts", shardCounts, "store -exp shard only: shard counts, each a divisor of 256")
	flag.IntVar(&readNames, "read-names", readNames, "store -exp read only: account keys looked up per block")
	flag.IntVar(&readShards, "read-shards", readShards, "store -exp read only: account shards; the chain ships eight")
	flag.StringVar(&readCells, "read-cells", readCells, "store -exp read only: fast-hit,fast-miss,descent-hit,descent-miss")
	flag.Parse()

	if *mode != "single" && *mode != "shard" && *mode != "store" {
		panic("-mode is single|shard|store, got " + *mode)
	}
	if *mode == "store" {
		if *backend == "mem" {
			*backend = "goleveldb"
		}
		r := *rounds
		if r == 0 {
			r = 150
		}
		runStore(*backend, *dir, *exp, *preload, *raw, r, *warmupFlag, *caches, *keeps, *allocs, *pats)
		return
	}
	keepGC = *gcOn
	if *rounds > 0 {
		shardRounds = *rounds
	}
	seqKeys = *keys == "sequential"

	if *mode == "shard" {
		const batch = 10_000
		const cache = 1_000_000

		if *shardsOnly > 0 {
			c := cache
			if *cacheFlag > 0 {
				c = *cacheFlag
			}
			runSharded(*backend, *preload, batch, c, *shardsOnly)
			return
		}

		for _, n := range []int{1, 2, 4, 8, 16} {
			runSharded(*backend, *preload, batch, cache, n)
		}

		for _, n := range []int{4, 16} {
			runSharded(*backend, *preload, batch, cache/n, n)
		}
		return
	}

	for _, cache := range []int{10_000, 100_000, 1_000_000} {
		for _, batch := range []int{1_000, 10_000} {
			for _, p := range patterns {
				run(*backend, *preload, batch, cache, p)
			}
		}
	}
}
