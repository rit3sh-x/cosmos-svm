// This file benchmarks the x/svm account store with the production IAVL
// configuration. The results set consensus parameters and hardware requirements.
//
// The benchmark models the shipped store:
//   - One "svm" store behind the rootmulti key prefix, with async pruning.
//   - Account writes are deduplicated and sorted, as cachekv does.
//   - GoLevelDB is the default. Pebble is optional.
//   - The seed writes accounts in ascending key order. Hot accounts are
//     spread across the keyspace.
//   - GC stays on, as on a validator.
//   - Pruning follows the SDK keep-recent and interval rules.
//   - Each cell starts from a new copy of the same seed database, so no
//     cache or page-cache state carries over from earlier cells.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/iavl"
	idb "github.com/cosmos/iavl/db"
)

// Store options an operator can set, mirrored as flags (main.go). Defaults are
// what a stock SDK machine runs.
var (
	pruneInterval = 10    // pruning-interval
	syncPrune     = false // iavl-sync-pruning
	noFastNode    = false // iavl-disable-fastnode
	seriesDir     = ""    // if set, one CSV per cell with a row per measured block
	drainCap      = 20 * time.Minute
)

// counter sees every write that IAVL batches. Node keys start with 's'. In an
// update-only workload each written node orphans one node, so nodes per block
// is also orphans per block.
type counter struct {
	nodes, nodeBytes, other, deletes atomic.Int64
}

type countingDB struct {
	idb.DB
	c *counter
}

func (d countingDB) NewBatch() idb.Batch { return countingBatch{d.DB.NewBatch(), d.c} }
func (d countingDB) NewBatchWithSize(n int) idb.Batch {
	return countingBatch{d.DB.NewBatchWithSize(n), d.c}
}

type countingBatch struct {
	idb.Batch
	c *counter
}

func (b countingBatch) Set(k, v []byte) error {
	if len(k) > 0 && k[0] == 's' {
		b.c.nodes.Add(1)
		b.c.nodeBytes.Add(int64(len(k) + len(v)))
	} else {
		b.c.other.Add(1)
	}
	return b.Batch.Set(k, v)
}

func (b countingBatch) Delete(k []byte) error {
	b.c.deletes.Add(1)
	return b.Batch.Delete(k)
}

type store struct {
	tree  *iavl.MutableTree
	c     *counter
	dir   string
	close func()
}

func openBase(backend, dir string) dbm.DB {
	var base dbm.DB
	var err error
	switch backend {
	case "goleveldb":
		base, err = dbm.NewGoLevelDB("application", dir, nil)
	case "pebble":
		base, err = dbm.NewPebbleDB("application", dir, nil)
	default:
		err = fmt.Errorf("-mode store needs a disk backend, goleveldb or pebble, not %q", backend)
	}
	if err != nil {
		panic(err)
	}
	return base
}

func openStore(backend, dir string, cache int) *store {
	base := openBase(backend, dir)
	c := &counter{}
	db := countingDB{idb.NewWrapper(dbm.NewPrefixDB(base, []byte("s/k:svm/"))), c}
	tree := iavl.NewMutableTree(db, cache, noFastNode, iavl.NewNopLogger(), iavl.AsyncPruningOption(!syncPrune))
	if _, err := tree.Load(); err != nil {
		panic(err)
	}
	// tree.Close cancels the pruning goroutine and waits for it, so it runs
	// before base.Close. Otherwise the pruner reads a closed DB. goleveldb logs an
	// error, and pebble panics with "pebble: closed".
	return &store{tree: tree, c: c, dir: dir, close: func() { tree.Close(); base.Close() }}
}

// accounts returns n uniformly random 32-byte keys in ascending order. A fixed
// seed makes them, so the preload and every later run agree on them.
func accounts(n int) [][]byte {
	r := rand.New(rand.NewSource(26))
	flat := make([]byte, 32*n)
	r.Read(flat)
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = flat[i*32 : (i+1)*32 : (i+1)*32]
	}
	sort.Slice(keys, func(i, j int) bool { return string(keys[i]) < string(keys[j]) })
	return keys
}

// seed preloads once per backend and account count, and cells copy it. The DONE
// file makes an interrupted preload start again, so no cell uses a half-written seed.
func seed(base, backend string, accts [][]byte, vals [][]byte) string {
	dir := filepath.Join(base, fmt.Sprintf("seed-%s-%d", backend, len(accts)))
	if _, err := os.Stat(filepath.Join(dir, "DONE")); err == nil {
		return dir
	}
	os.RemoveAll(dir)
	t0 := time.Now()
	s := openStore(backend, dir, 1_000_000)
	for i, k := range accts {
		if _, err := s.tree.Set(k, vals[i%valuePool]); err != nil {
			panic(err)
		}
		if i%50_000 == 49_999 {
			if _, _, err := s.tree.SaveVersion(); err != nil {
				panic(err)
			}
		}
	}
	if _, _, err := s.tree.SaveVersion(); err != nil {
		panic(err)
	}
	s.close()
	if err := os.WriteFile(filepath.Join(dir, "DONE"), nil, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("# seed backend=%s accounts=%d took_s=%.0f db_mb=%d\n",
		backend, len(accts), time.Since(t0).Seconds(), dirBytes(dir)>>20)
	return dir
}

func cellFrom(seedDir string) string {
	dir := seedDir + ".cell"
	os.RemoveAll(dir)
	if out, err := exec.Command("cp", "-a", seedDir, dir).CombinedOutput(); err != nil {
		panic(fmt.Sprintf("copy seed: %v: %s", err, out))
	}
	return dir
}

func dirBytes(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

func rssBytes() int64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	pages, _ := strconv.ParseInt(f[1], 10, 64)
	return pages * int64(os.Getpagesize())
}

// block is what cachekv.Write hands IAVL: unique keys, ascending. Account keys
// are already sorted, so sorting indices sorts keys.
type block struct{ idx, val []int }

func (b *block) fill(r *rand.Rand, n int, hot []int, share float64, raw int) {
	b.idx = b.idx[:0]
	for i := 0; i < raw; i++ {
		if len(hot) > 0 && r.Float64() < share {
			b.idx = append(b.idx, hot[r.Intn(len(hot))])
		} else {
			b.idx = append(b.idx, r.Intn(n))
		}
	}
	sort.Ints(b.idx)
	b.idx = slices.Compact(b.idx)
	b.val = b.val[:0]
	for range b.idx {
		b.val = append(b.val, r.Intn(valuePool))
	}
}

func hotSet(r *rand.Rand, n int, frac float64) []int {
	hot := make([]int, int(float64(n)*frac))
	for i := range hot {
		hot[i] = r.Intn(n)
	}
	return hot
}

type storeBench struct {
	backend, dir        string
	accts, vals         [][]byte
	raw, rounds, warmup int
}

type result struct {
	set, commit, prune, blk []time.Duration
	unique                  int
	nodes, nodeBytes, dels  int64
	gcPause                 time.Duration
	// retainedEnd is the number of versions on disk when the last block
	// commits. drain is how long async pruning then takes to get back to
	// keep+interval. A backlog that only grows means pruning cannot keep up.
	retainedEnd, retainedAfter int
	drain                      time.Duration
}

func pct(d []time.Duration, p float64) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[int(p*float64(len(s)-1))]
}

// run drives rounds of blocks against s. prepare builds one block outside the
// timed region and returns the timed part, which reports how many keys it wrote.
func (x *storeBench) run(s *store, keep int, series string, prepare func() func() int) result {
	res := result{}
	var ms0, ms1 runtime.MemStats
	var csv *os.File
	if seriesDir != "" && series != "" {
		os.MkdirAll(seriesDir, 0o755)
		f, err := os.Create(filepath.Join(seriesDir, series+".csv"))
		if err != nil {
			panic(err)
		}
		defer f.Close()
		csv = f
		fmt.Fprintln(csv, "block,wall_s,set_us,commit_us,prune_us,block_us,nodes_cum,deletes_cum,retained_versions,db_mb")
	}
	start := time.Now()
	for i := 0; i < x.warmup+x.rounds; i++ {
		if i == x.warmup {
			runtime.ReadMemStats(&ms0)
			s.c.nodes.Store(0)
			s.c.nodeBytes.Store(0)
			s.c.deletes.Store(0)
		}
		apply := prepare()
		t0 := time.Now()
		n := apply()
		t1 := time.Now()
		_, v, err := s.tree.SaveVersion()
		if err != nil {
			panic(err)
		}
		t2 := time.Now()
		if keep > 0 && v%int64(pruneInterval) == 0 && v-1-int64(keep) > 0 {
			if err := s.tree.DeleteVersionsTo(v - 1 - int64(keep)); err != nil {
				panic(err)
			}
		}
		t3 := time.Now()
		if i >= x.warmup {
			res.set = append(res.set, t1.Sub(t0))
			res.commit = append(res.commit, t2.Sub(t1))
			res.prune = append(res.prune, t3.Sub(t2))
			res.blk = append(res.blk, t3.Sub(t0))
			res.unique += n
			if csv != nil {
				j := i - x.warmup
				retained, dbMB := -1, int64(-1)
				if j%10 == 0 {
					retained = len(s.tree.AvailableVersions())
				}
				if j%100 == 0 {
					dbMB = dirBytes(s.dir) >> 20
				}
				fmt.Fprintf(csv, "%d,%.1f,%d,%d,%d,%d,%d,%d,%d,%d\n", j, time.Since(start).Seconds(),
					t1.Sub(t0).Microseconds(), t2.Sub(t1).Microseconds(), t3.Sub(t2).Microseconds(), t3.Sub(t0).Microseconds(),
					s.c.nodes.Load(), s.c.deletes.Load(), retained, dbMB)
			}
		}
	}
	res.retainedEnd = len(s.tree.AvailableVersions())
	res.retainedAfter = res.retainedEnd
	if keep > 0 && !syncPrune {
		t := time.Now()
		for res.retainedAfter > keep+pruneInterval+1 && time.Since(t) < drainCap {
			time.Sleep(time.Second)
			res.retainedAfter = len(s.tree.AvailableVersions())
		}
		res.drain = time.Since(t)
	}
	runtime.ReadMemStats(&ms1)
	res.nodes, res.nodeBytes, res.dels = s.c.nodes.Load(), s.c.nodeBytes.Load(), s.c.deletes.Load()
	res.gcPause = time.Duration(ms1.PauseTotalNs - ms0.PauseTotalNs)
	return res
}

func (x *storeBench) print(exp string, s *store, cache, keep int, pattern string, res result, extra string) {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	r := int64(len(res.blk))
	us := func(d time.Duration) int64 { return d.Microseconds() }
	unique := res.unique / len(res.blk)
	fmt.Printf("exp=%s backend=%s accounts=%d cache=%d keep=%d pattern=%s raw=%d unique=%d rounds=%d "+
		"set_p50_us=%d commit_p50_us=%d commit_p95_us=%d prune_p50_us=%d prune_max_us=%d "+
		"block_p50_us=%d block_p95_us=%d block_max_us=%d per_write_p50_us=%.1f per_write_p95_us=%.1f "+
		"nodes_per_block=%d node_bytes_per_block=%d deletes_per_block=%d "+
		"heap_live_mb=%d rss_mb=%d db_mb=%d gc_pause_ms=%d "+
		"prune_interval=%d sync_prune=%t fastnode=%t retained_end=%d retained_after_drain=%d drain_s=%.0f%s\n",
		exp, x.backend, len(x.accts), cache, keep, pattern, x.raw, unique, len(res.blk),
		us(pct(res.set, .5)), us(pct(res.commit, .5)), us(pct(res.commit, .95)),
		us(pct(res.prune, .5)), us(pct(res.prune, 1)),
		us(pct(res.blk, .5)), us(pct(res.blk, .95)), us(pct(res.blk, 1)),
		float64(us(pct(res.blk, .5)))/float64(unique), float64(us(pct(res.blk, .95)))/float64(unique),
		res.nodes/r, res.nodeBytes/r, res.dels/r,
		ms.HeapAlloc>>20, rssBytes()>>20, dirBytes(s.dir)>>20, res.gcPause.Milliseconds(),
		pruneInterval, syncPrune, !noFastNode, res.retainedEnd, res.retainedAfter, res.drain.Seconds(), extra)
}

// updates is the account-write workload every experiment except alloc uses.
func (x *storeBench) updates(s *store, p pattern) func() func() int {
	r := rand.New(rand.NewSource(int64(len(p.name))*1000 + 7))
	hot := hotSet(r, len(x.accts), p.hotFrac)
	var b block
	apply := func() int {
		for j, idx := range b.idx {
			if _, err := s.tree.Set(x.accts[idx], x.vals[b.val[j]]); err != nil {
				panic(err)
			}
		}
		return len(b.idx)
	}
	return func() func() int {
		b.fill(r, len(x.accts), hot, p.hotShare, x.raw)
		return apply
	}
}

func (x *storeBench) cell(seedDir string, cache int) *store {
	return openStore(x.backend, cellFrom(seedDir), cache)
}

func (x *storeBench) done(s *store) {
	s.close()
	os.RemoveAll(s.dir)
}

// expCache measures the cache curve, bytes per cached node, per-write cost and,
// with -preload, account scaling. All four are columns of the same row.
func (x *storeBench) expCache(seedDir string, caches []int, pats []pattern) {
	for _, cache := range caches {
		for _, p := range pats {
			s := x.cell(seedDir, cache)
			res := x.run(s, 0, fmt.Sprintf("cache-%s-%d-%d-%s", x.backend, len(x.accts), cache, p.name), x.updates(s, p))
			x.print("cache", s, cache, 0, p.name, res, "")
			x.done(s)
		}
	}
}

// expPrune measures pruning I/O. keep=0 retains everything. The p95 and max
// columns carry the cost, because async pruning moves it off the commit call and
// into contention.
func (x *storeBench) expPrune(seedDir string, cache int, keeps []int, pats []pattern) {
	for _, keep := range keeps {
		for _, p := range pats {
			s := x.cell(seedDir, cache)
			res := x.run(s, keep, fmt.Sprintf("prune-%s-%d-%d-keep%d-int%d-sync%t-fast%t-%s", x.backend, len(x.accts), cache, keep, pruneInterval, syncPrune, !noFastNode, p.name), x.updates(s, p))
			x.print("prune", s, cache, keep, p.name, res, "")
			x.done(s)
		}
	}
}

func exportAll(t *iavl.ImmutableTree) (nodes int, bytes int64) {
	e, err := t.Export()
	if err != nil {
		panic(err)
	}
	defer e.Close()
	for {
		n, err := e.Next()
		if errors.Is(err, iavl.ErrorExportDone) {
			return nodes, bytes
		}
		if err != nil {
			panic(err)
		}
		nodes++
		bytes += int64(len(n.Key) + len(n.Value) + 9)
	}
}

// expExport measures a full tree export, alone and while blocks commit. A
// snapshot at a snapshot-interval height does this.
func (x *storeBench) expExport(seedDir string, cache int) {
	s := x.cell(seedDir, cache)
	imm, err := s.tree.GetImmutable(s.tree.Version())
	if err != nil {
		panic(err)
	}
	t0 := time.Now()
	nodes, bytes := exportAll(imm)
	fmt.Printf("exp=export-alone backend=%s accounts=%d cache=%d nodes=%d export_mb=%d took_ms=%d\n",
		x.backend, len(x.accts), cache, nodes, bytes>>20, time.Since(t0).Milliseconds())
	x.done(s)

	for _, concurrent := range []bool{false, true} {
		s := x.cell(seedDir, cache)
		v := s.tree.Version()
		stop := make(chan struct{})
		exports := make(chan int, 1)
		if concurrent {
			go func() {
				n := 0
				for {
					select {
					case <-stop:
						exports <- n
						return
					default:
					}
					imm, err := s.tree.GetImmutable(v)
					if err != nil {
						panic(err)
					}
					exportAll(imm)
					n++
				}
			}()
		}
		res := x.run(s, 0, "", x.updates(s, patterns[0]))
		label, extra := "export-baseline", ""
		if concurrent {
			close(stop)
			label, extra = "export-concurrent", fmt.Sprintf(" exports_completed=%d", <-exports)
		}
		x.print(label, s, cache, 0, patterns[0].name, res, extra)
		x.done(s)
	}
}

// expAlloc measures new-account allocation at 100 MB per block
// (MAX_BLOCK_ACCOUNTS_DATA_SIZE_DELTA), in shapes from a few large accounts to
// many small ones. The values are random bytes, so compression cannot help.
func (x *storeBench) expAlloc(seedDir string, cache int, shapes []string) {
	for _, shape := range shapes {
		count, size := parseShape(shape)
		s := x.cell(seedDir, cache)
		r := rand.New(rand.NewSource(int64(count)))
		res := x.run(s, 0, "", func() func() int {
			flat := make([]byte, 32*count)
			r.Read(flat)
			keys := make([][]byte, count)
			for i := range keys {
				keys[i] = flat[i*32 : (i+1)*32]
			}
			sort.Slice(keys, func(i, j int) bool { return string(keys[i]) < string(keys[j]) })
			values := make([]byte, size*count)
			r.Read(values)
			return func() int {
				for i, k := range keys {
					if _, err := s.tree.Set(k, values[i*size:(i+1)*size:(i+1)*size]); err != nil {
						panic(err)
					}
				}
				return count
			}
		})
		x.print("alloc", s, cache, 0, shape, res, fmt.Sprintf(" block_mb=%d", count*size>>20))
		x.done(s)
	}
}

func parseShape(s string) (count, size int) {
	a, b, ok := strings.Cut(s, "x")
	count, err1 := strconv.Atoi(a)
	size, err2 := strconv.Atoi(b)
	if !ok || err1 != nil || err2 != nil {
		panic("-allocs entries are COUNTxBYTES, got " + s)
	}
	return count, size
}

func ints(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			panic("bad integer list " + s)
		}
		out = append(out, n)
	}
	return out
}

func runStore(backend, dir, exp string, preload, raw, rounds, warmup int, caches, keeps, allocs, pats string) {
	// Check every argument before the seed, which can take an hour.
	if !slices.Contains([]string{"cache", "prune", "export", "alloc", "shard", "read"}, exp) {
		panic("-exp is cache|prune|export|alloc|shard|read, got " + exp)
	}
	var ps []pattern
	for _, name := range strings.Split(pats, ",") {
		i := slices.IndexFunc(patterns, func(p pattern) bool { return p.name == name })
		if i < 0 {
			panic("unknown pattern " + name)
		}
		ps = append(ps, patterns[i])
	}
	cs, ks, shapes := ints(caches), ints(keeps), strings.Split(allocs, ",")
	for _, s := range shapes {
		parseShape(s)
	}

	x := &storeBench{backend: backend, dir: dir, raw: raw, rounds: rounds, warmup: warmup}
	x.vals = buildValues(rand.New(rand.NewSource(1)))
	x.accts = accounts(preload)
	var seedDir string
	if exp != "shard" && exp != "read" {
		seedDir = seed(dir, backend, x.accts, x.vals)
	}

	switch exp {
	case "cache":
		x.expCache(seedDir, cs, ps)
	case "prune":
		x.expPrune(seedDir, cs[0], ks, ps)
	case "export":
		x.expExport(seedDir, cs[0])
	case "alloc":
		x.expAlloc(seedDir, cs[0], shapes)
	case "shard":
		x.expShard(cs[0], ks[0], ps)
	case "read":
		x.expRead(cs[0], ks[0])
	}
}
