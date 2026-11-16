// This file benchmarks the cost of one account lookup with the chain's store
// configuration.
//
// Phase 2 loads each distinct account that admitted transactions name, before
// fee validation. This includes transactions that execution later rejects as
// replays. This benchmark isolates the cost of each lookup.
//
// It measures hits and misses separately. Existing accounts stand for normal
// traffic. Missing accounts model requests that name accounts that do not exist.
//
// Each round reads the accounts of a block, then commits a write block, so
// async pruning continues during the read phase. It times only the reads.
//
// The benchmark tests both read paths:
//   - fastnode=true: the shipped configuration. It uses the fast-node index
//     and its separate 100,000-entry cache. A miss at the latest version can
//     return without a tree descent.
//   - fastnode=false: the app.toml iavl-disable-fastnode configuration. It
//     descends the tree and uses the node cache that -caches sets.
//
// The comparison shows which cache serves account lookups and how hits and
// misses differ in cost.
package main

import (
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"strings"
	"time"
)

var (
	readNames  = 200_000                                       // -read-names: account keys looked up per block
	readShards = 8                                             // -read-shards
	readCells  = "fast-hit,fast-miss,descent-hit,descent-miss" // -read-cells: which of the four to run
)

type readResult struct {
	batch []time.Duration
	found int
	rows  int
}

// fastNodeCacheSize is the fast-node cache size that iavl hardcodes. Nothing in
// the SDK can change it.
const fastNodeCacheSize = 100_000

// expRead measures the cost of a lookup in the shipped store: -preload accounts
// across -read-shards trees that share -caches nodes, with pruning to keep every
// -prune-interval blocks.
func (x *storeBench) expRead(cache, keep int) {
	seedDir := seedShards(x.dir, x.backend, x.accts, x.vals, readShards)
	for _, fast := range []bool{true, false} {
		for _, mode := range []string{"hit", "miss"} {
			path := map[bool]string{true: "fast", false: "descent"}[fast]
			if !strings.Contains(","+readCells+",", ","+path+"-"+mode+",") {
				continue
			}
			// The seed keeps its fast index in both cases. This cell only sets
			// whether Get uses it, which is what the app.toml flag does.
			saved := noFastNode
			noFastNode = !fast
			s := openShardStore(x.backend, cellFrom(seedDir), cache, readShards)
			noFastNode = saved

			res := x.runReads(s, keep, mode)
			x.printRead(s, cache, keep, mode, fast, res)
			s.close()
			os.RemoveAll(s.dir)
		}
	}
	os.RemoveAll(seedDir)
}

func (x *storeBench) runReads(s *shardStore, keep int, mode string) readResult {
	n := len(s.trees)
	r := rand.New(rand.NewSource(93))
	wr := rand.New(rand.NewSource(94))
	hot := hotSet(wr, len(x.accts), patterns[0].hotFrac)

	keys := make([][]byte, readNames)
	flat := make([]byte, 32*readNames)
	var wb block
	var res readResult

	for i := 0; i < x.warmup+x.rounds; i++ {
		// Choose the names outside the timed region, because a proposer builds
		// its load list before phase 2 runs.
		if mode == "miss" {
			r.Read(flat)
			for j := range keys {
				keys[j] = flat[j*32 : (j+1)*32 : (j+1)*32]
			}
		} else {
			for j := range keys {
				keys[j] = x.accts[r.Intn(len(x.accts))]
			}
		}

		found := 0
		t0 := time.Now()
		for _, k := range keys {
			v, err := s.trees[topShard(k, n)].Get(k)
			if err != nil {
				panic(err)
			}
			if v != nil {
				found++
			}
		}
		d := time.Since(t0)

		wb.fill(wr, len(x.accts), hot, patterns[0].hotShare, x.raw)
		for j, idx := range wb.idx {
			if _, err := s.trees[topShard(x.accts[idx], n)].Set(x.accts[idx], x.vals[wb.val[j]]); err != nil {
				panic(err)
			}
		}
		s.saveAll()
		if v := s.trees[0].Version(); keep > 0 && v%int64(pruneInterval) == 0 && v-1-int64(keep) > 0 {
			for _, t := range s.trees {
				if err := t.DeleteVersionsTo(v - 1 - int64(keep)); err != nil {
					panic(err)
				}
			}
		}

		if i >= x.warmup {
			res.batch = append(res.batch, d)
			res.found += found
			res.rows++
		}
	}
	return res
}

func (x *storeBench) printRead(s *shardStore, cache, keep int, mode string, fast bool, res readResult) {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	per := func(p float64) float64 {
		return float64(pct(res.batch, p).Microseconds()) / float64(readNames)
	}
	fmt.Printf("exp=read backend=%s accounts=%d shards=%d cache=%d cache_per_tree=%d keep=%d "+
		"fastnode=%t fast_cache_per_tree=%d mode=%s names=%d writes_per_block=%d rounds=%d found_frac=%.4f "+
		"batch_p50_ms=%d batch_p95_ms=%d batch_max_ms=%d "+
		"per_lookup_p50_us=%.2f per_lookup_p95_us=%.2f per_lookup_max_us=%.2f "+
		"prune_interval=%d heap_live_mb=%d rss_mb=%d db_mb=%d\n",
		x.backend, len(x.accts), len(s.trees), cache, cache/len(s.trees), keep,
		fast, fastNodeCacheSize, mode, readNames, x.raw, res.rows, float64(res.found)/float64(res.rows*readNames),
		pct(res.batch, .5).Milliseconds(), pct(res.batch, .95).Milliseconds(), pct(res.batch, 1).Milliseconds(),
		per(.5), per(.95), per(1),
		pruneInterval, ms.HeapAlloc>>20, rssBytes()>>20, dirBytes(s.dir)>>20)
}
