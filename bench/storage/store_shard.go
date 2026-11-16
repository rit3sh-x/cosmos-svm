// This file benchmarks account-store sharding with the shipped configuration.
//
// Unlike shard.go, it uses the production store harness, with pruning. It
// measures the two SDK loops that parallel sharding must patch:
//   - CacheMultiStore.Write: flushes stores and applies IAVL updates.
//   - rootmulti.Commit: commits stores.
//
// Serial mode does what the SDK does now. Parallel mode runs each loop
// concurrently, and waits for all stores before it starts the next loop.
//
// Shards share one database behind rootmulti key prefixes. The total IAVL
// node cache is divided evenly across shards, so every configuration uses the
// same cache budget. The public-key prefix assigns each account to a shard.
//
// Serial sharding should gain little. Splitting N accounts and w writes across
// K trees leaves about the same total tree-update work.
package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/iavl"
	idb "github.com/cosmos/iavl/db"
)

var shardCounts = "1,2,4,8" // -shard-counts

// topShard returns the top bits of the key as a shard index. It is exact for
// any n that divides 256.
func topShard(k []byte, n int) int { return int(k[0]) * n / 256 }

type shardStore struct {
	trees []*iavl.MutableTree
	c     *counter
	dir   string
	close func()
}

func openShardStore(backend, dir string, cache, n int) *shardStore {
	base := openBase(backend, dir)
	c := &counter{}
	trees := make([]*iavl.MutableTree, n)
	for i := range trees {
		db := countingDB{idb.NewWrapper(dbm.NewPrefixDB(base, []byte(fmt.Sprintf("s/k:svm%d/", i)))), c}
		trees[i] = iavl.NewMutableTree(db, cache/n, noFastNode, iavl.NewNopLogger(), iavl.AsyncPruningOption(!syncPrune))
		if _, err := trees[i].Load(); err != nil {
			panic(err)
		}
	}
	return &shardStore{trees: trees, c: c, dir: dir, close: func() {
		for _, t := range trees {
			t.Close()
		}
		base.Close()
	}}
}

// saveAll keeps every tree on the same version, as rootmulti does.
func (s *shardStore) saveAll() {
	for _, t := range s.trees {
		if _, _, err := t.SaveVersion(); err != nil {
			panic(err)
		}
	}
}

func seedShards(base, backend string, accts, vals [][]byte, n int) string {
	dir := filepath.Join(base, fmt.Sprintf("seed-%s-%d-shards%d", backend, len(accts), n))
	if _, err := os.Stat(filepath.Join(dir, "DONE")); err == nil {
		return dir
	}
	os.RemoveAll(dir)
	t0 := time.Now()
	s := openShardStore(backend, dir, 1_000_000*n, n)
	for i, k := range accts {
		if _, err := s.trees[topShard(k, n)].Set(k, vals[i%valuePool]); err != nil {
			panic(err)
		}
		if i%50_000 == 49_999 {
			s.saveAll()
		}
	}
	s.saveAll()
	s.close()
	if err := os.WriteFile(filepath.Join(dir, "DONE"), nil, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("# seed backend=%s accounts=%d shards=%d took_s=%.0f db_mb=%d\n",
		backend, len(accts), n, time.Since(t0).Seconds(), dirBytes(dir)>>20)
	return dir
}

func (x *storeBench) expShard(cache, keep int, pats []pattern) {
	for _, n := range ints(shardCounts) {
		seedDir := seedShards(x.dir, x.backend, x.accts, x.vals, n)
		for _, p := range pats {
			for _, parallel := range []bool{false, true} {
				if n == 1 && parallel {
					continue
				}
				s := openShardStore(x.backend, cellFrom(seedDir), cache, n)
				res := x.runShards(s, keep, p, parallel)
				mode := "serial"
				if parallel {
					mode = "parallel"
				}
				x.print("shard", &store{dir: s.dir}, cache, keep, p.name, res,
					fmt.Sprintf(" shards=%d mode=%s cache_per_tree=%d", n, mode, cache/n))
				s.close()
				os.RemoveAll(s.dir)
			}
		}
		os.RemoveAll(seedDir)
	}
}

func (x *storeBench) runShards(s *shardStore, keep int, p pattern, parallel bool) result {
	n := len(s.trees)
	each := func(f func(sh int)) {
		if !parallel {
			for sh := 0; sh < n; sh++ {
				f(sh)
			}
			return
		}
		var wg sync.WaitGroup
		for sh := 0; sh < n; sh++ {
			wg.Add(1)
			go func() { defer wg.Done(); f(sh) }()
		}
		wg.Wait()
	}

	r := rand.New(rand.NewSource(int64(len(p.name))*1000 + 7))
	hot := hotSet(r, len(x.accts), p.hotFrac)
	var b block
	bounds := make([]int, n+1)
	res := result{}
	var ms0, ms1 runtime.MemStats
	for i := 0; i < x.warmup+x.rounds; i++ {
		if i == x.warmup {
			runtime.ReadMemStats(&ms0)
			s.c.nodes.Store(0)
			s.c.nodeBytes.Store(0)
			s.c.deletes.Store(0)
		}
		b.fill(r, len(x.accts), hot, p.hotShare, x.raw)
		// Accounts are sorted and a shard is the top bits, so each shard's writes
		// are one contiguous, ascending run of the block.
		for sh := 0; sh <= n; sh++ {
			bounds[sh] = sort.Search(len(b.idx), func(j int) bool { return topShard(x.accts[b.idx[j]], n) >= sh })
		}

		t0 := time.Now()
		each(func(sh int) {
			for j := bounds[sh]; j < bounds[sh+1]; j++ {
				if _, err := s.trees[sh].Set(x.accts[b.idx[j]], x.vals[b.val[j]]); err != nil {
					panic(err)
				}
			}
		})
		t1 := time.Now()
		each(func(sh int) {
			if _, _, err := s.trees[sh].SaveVersion(); err != nil {
				panic(err)
			}
		})
		t2 := time.Now()
		if v := s.trees[0].Version(); keep > 0 && v%int64(pruneInterval) == 0 && v-1-int64(keep) > 0 {
			for _, t := range s.trees {
				if err := t.DeleteVersionsTo(v - 1 - int64(keep)); err != nil {
					panic(err)
				}
			}
		}
		t3 := time.Now()
		if i >= x.warmup {
			res.set = append(res.set, t1.Sub(t0))
			res.commit = append(res.commit, t2.Sub(t1))
			res.prune = append(res.prune, t3.Sub(t2))
			res.blk = append(res.blk, t3.Sub(t0))
			res.unique += len(b.idx)
		}
	}
	res.retainedEnd = len(s.trees[0].AvailableVersions())
	res.retainedAfter = res.retainedEnd
	runtime.ReadMemStats(&ms1)
	res.nodes, res.nodeBytes, res.dels = s.c.nodes.Load(), s.c.nodeBytes.Load(), s.c.deletes.Load()
	res.gcPause = time.Duration(ms1.PauseTotalNs - ms0.PauseTotalNs)
	return res
}
