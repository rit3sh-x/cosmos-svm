// Command blockcost measures the cost of block processing on this machine,
// with the IAVL store stack that FinalizeBlock uses. It opens every database
// of a cell on Pebble, as svmd does, or on goleveldb with -backend goleveldb.
//
// It times the phases of each block, one at a time and together:
//   - resolve: read -tables accounts one at a time, as phase 2 reads the
//     lookup tables that a block names
//   - load:    PushAccounts and arena finalization, with the -big accounts in
//     the load of every block
//   - copy:    a Go stand-in for the one copy of each -big account that the
//     engine makes
//   - write:   overwrite existing accounts and allocate new ones
//   - index:   update History, TokenIndex and Dedup sidecars (with -index)
//   - commit:  write the cache and commit the root store, including hashing
//     and pruning, one store after another. With -commit app, FlushShards and
//     SaveShards run first, eight shards at a time, as the app commits.
//     rms_commit is only the rootmulti.Commit part, where the SDK prunes.
//
// The seed holds 10 million accounts by default. The first run builds the
// seed database, and later runs use it again. -gap adds the idle time between
// blocks. -ballast-gib holds memory in place of the program cache under a limit.
// -ballast-mmap holds it outside the Go heap, as the program cache is. A Go
// allocation counts as live heap, so the GC goal of Go (twice the live heap at
// GOGC=100) would grow by the size of the ballast. -inter-block-cache wraps
// every store in the inter-block cache of the SDK, as the inter-block-cache
// setting of app.toml wraps the stores of svmd. Every read then goes through
// its ARC before the IAVL store.
//
// Block times are in seconds. The cost model prices a full block at 7.45 s.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"runtime/trace"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"cosmossdk.io/log/v2"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/cosmos-sdk/store/v2"
	pruningtypes "github.com/cosmos/cosmos-sdk/store/v2/pruning/types"
	"github.com/cosmos/cosmos-sdk/store/v2/rootmulti"
	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"

	"github.com/rit3sh-x/cosmos-svm/app"
	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

var (
	dir      = flag.String("dir", os.TempDir(), "where the seed and the cell live")
	accounts = flag.Int("accounts", 10_000_000, "accounts in state")
	names    = flag.Int("names", 200_000, "account keys in each block's load list")
	writes   = flag.Int("writes", 10_000, "accounts written per block")
	warmup   = flag.Int("warmup", 40, "unmeasured blocks")
	rounds   = flag.Int("rounds", 60, "measured blocks")
	mode     = flag.String("mode", "hit", "hit: names exist | miss: names do not")
	gap      = flag.Duration("gap", 0, "idle between commit and the next block's load")
	ballast  = flag.Int("ballast-gib", 0, "memory to hold, standing in for the program cache")
	offHeap  = flag.Bool("ballast-mmap", false, "hold the ballast as anonymous memory outside the Go heap, where the program cache of the engine is; without it the ballast is a Go allocation, which raises the GC goal of Go by its size as no program cache does")
	label    = flag.String("label", "", "cell label, echoed in the output")
	newAccts = flag.Int("new", 0, "accounts allocated per block, on top of -writes")
	newBytes = flag.Int("new-bytes", dataLen, "data bytes in each allocated account")
	txs      = flag.Int("txs", 0, "transactions the block records in the sidecar indexes")
	txNames  = flag.Int("tx-names", 3, "account keys in each recorded transaction")
	index    = flag.Bool("index", false, "run History, TokenIndex and Dedup, as a block does")
	commit   = flag.String("commit", "serial", "serial: cms.Write then rootmulti.Commit, one store after another, as every run before 2026-11-02 | app: FlushShards and SaveShards, eight shards at a time, as the app commits")
	every    = flag.Int("report-every", 0, "print a # window line every N blocks, warmup included: the window's phase p50s, its block p95 and max, and the database's size")
	tables   = flag.Int("tables", 0, "distinct accounts read one at a time before the load, as phase 2 reads the lookup tables a block names (x/svm/runner.go resolve)")
	big      = flag.Int("big", 0, "accounts of -big-bytes written into the cell before its first block and loaded by every block")
	bigBytes = flag.Int("big-bytes", 10<<20, "data bytes in each -big account")
	bigLoad  = flag.Bool("big-load", true, "load the -big accounts in every block; false plants them and no block reads them")
	bigFar   = flag.Bool("big-far", false, "plant the -big accounts at the top of each shard's key range, where no account in state is")

	agedBlocks    = flag.Int("aged-blocks", 0, "start from an aged seed: history.db filled to its budget, then this many blocks of -age-writes, then tokens.db rebuilt from every account")
	ageWrites     = flag.Int("age-writes", 30_000, "accounts each aging block writes")
	ageTxs        = flag.Int("age-txs", 100, "transactions each aging block records")
	ageTxNames    = flag.Int("age-tx-names", 128, "account keys in each transaction an aging block records")
	historyBytes  = flag.Uint64("history-bytes", 100<<30, "history.db's budget, index-max-bytes")
	historyBlocks = flag.Int("history-blocks", 0, "set history.db's budget to this many blocks of -fill-txs transfers, each measured by the history's own counter, in place of -history-bytes")
	noBufferPool  = flag.Bool("no-buffer-pool", false, "with -backend goleveldb, open the application database with goleveldb's DisableBufferPool")
	interBlock    = flag.Bool("inter-block-cache", false, "wrap every store in the SDK's inter-block cache, an ARC of 1,000 values per store, as svmd's app.toml inter-block-cache = true does (server/util.go DefaultBaseappOptions)")
	backend       = flag.String("backend", "pebble", "pebble: every database of the cell opens as svmd opens it | goleveldb: on the old goleveldb options; each engine builds seeds of its own")
	overlap       = flag.Bool("overlap", false, "with -commit app, run the sidecar writes beside FlushShards and SaveShards and join them before rootmulti.Commit, as App.Commit does")
	profile       = flag.String("profile", "", "write a CPU, mutex and blocking profile of the measured blocks, and a trace of the first two, into this directory under the label")
	heapProfile   = flag.String("heap-profile", "", "write Go heap profiles into this directory under the label, as heap-start, heap-mid and heap-end.pb.gz: at the first measured block, the middle one and after the last")
	fillTxs       = flag.Int("fill-txs", 7_532, "transfers in each block an aged seed fills history.db with; blk-20g's 7,532 fill a block at 1,755 a write lock")
)

// dataLen is the data size of a token account, 165 bytes. The disk figures use
// this size.
const dataLen = 165

func main() {
	flag.Parse()
	if *mode != "hit" && *mode != "miss" {
		panic("-mode is hit|miss, got " + *mode)
	}
	if *commit != "serial" && *commit != "app" {
		panic("-commit is serial|app, got " + *commit)
	}
	if *overlap && *commit != "app" {
		panic("-overlap needs -commit app: the writes join where App.Commit joins them")
	}
	var hold [][]byte
	for i := 0; i < *ballast; i++ {
		var b []byte
		if *offHeap {
			var err error
			if b, err = syscall.Mmap(-1, 0, 1<<30, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE); err != nil {
				panic(err)
			}
		} else {
			b = make([]byte, 1<<30)
		}
		for j := 0; j < len(b); j += 4096 {
			b[j] = 1
		}
		hold = append(hold, b)
	}

	keys := accountKeys(*accounts)
	seedDir := seed(keys)
	budget := historyBudget(keys)
	var base uint64
	if *agedBlocks > 0 {
		seedDir, base = aged(seedDir, keys, budget)
	}
	cell := seedDir + ".cell"
	os.RemoveAll(cell)
	if out, err := exec.Command("cp", "-a", seedDir, cell).CombinedOutput(); err != nil {
		panic(fmt.Sprintf("copy seed: %v: %s", err, out))
	}
	rms, stores, db := open(cell)
	bigKeys, bigValue := plantBig(rms, stores)
	loadBig := bigKeys
	if !*bigLoad {
		loadBig = nil
	}

	sc := openSidecars(cell, "tokens", base, budget)
	r := rand.New(rand.NewSource(93))
	blobs := values()
	var loads, wr, idx, cm, blk, fin, res, cp []time.Duration
	var unique, found, arena int
	// The window lines of a long cell show whether the database settles and
	// whether its blocks slow down as it runs. The p50s at the end of a cell
	// average that away.
	var window [][4]time.Duration
	if *every > 0 {
		go sampleAnon()
	}
	var stopProfile, stopTrace func()
	var history0 lsm
	for i := 0; i < *warmup+*rounds; i++ {
		if i == *warmup {
			history0 = sc.historyLSM()
		}
		if i == *warmup && *profile != "" {
			stopProfile, stopTrace = startProfile(filepath.Join(*profile, *label))
		}
		if i == *warmup+2 && stopTrace != nil {
			stopTrace()
		}
		if *heapProfile != "" {
			switch i {
			case *warmup:
				writeHeap("start")
			case *warmup + *rounds/2:
				writeHeap("mid")
			}
		}
		list := loadList(r, keys)
		if len(loadBig) > 0 {
			list = append(list, loadBig...)
			sort.Slice(list, func(i, j int) bool { return string(list[i][:]) < string(list[j][:]) })
		}
		tbl := tableKeys(r, keys)
		recs, sets := block(r, keys, blobs)
		// What the runner has from phase 2's load of each written account,
		// read here, outside the block's timing.
		prior := sc.before(sets)
		if *gap > 0 {
			time.Sleep(*gap)
		}
		cms := rms.CacheMultiStore()
		w := ffi.NewArenaWriter(ffi.KindExecuteReq, 0)

		tBlock := time.Now()
		t0 := time.Now()
		resolve(stores, cms, tbl)
		dResolve := time.Since(t0)

		t0 = time.Now()
		stores.PushAccounts(w, cms, list, svm.ArenaBytes, nil)
		buf := w.Finish()
		dLoad := time.Since(t0)

		t0 = time.Now()
		copyBig(len(loadBig), bigValue)
		dCopy := time.Since(t0)

		t0 = time.Now()
		for _, set := range sets {
			stores.SetAccount(cms, set.key, set.value)
		}
		dWrite := time.Since(t0)

		t0 = time.Now()
		join := sc.write(stores, cms, base+uint64(i+1), recs, sets, prior, *overlap)
		dIndex := time.Since(t0)

		t0 = time.Now()
		var dJoin time.Duration
		if *commit == "app" {
			flushApp(rms, stores, cms)
			t1 := time.Now()
			if err := join(); err != nil {
				panic(err)
			}
			dJoin = time.Since(t1)
		} else {
			cms.Write()
		}
		tFin := time.Now()
		rms.Commit()
		dFin := time.Since(tFin)
		// The wait for the sidecar writes is theirs, not the commit's.
		dCommit := time.Since(t0) - dJoin
		dIndex += dJoin
		dBlock := time.Since(tBlock)

		if i >= *warmup {
			loads = append(loads, dLoad)
			wr = append(wr, dWrite)
			idx = append(idx, dIndex)
			cm = append(cm, dCommit)
			fin = append(fin, dFin)
			res = append(res, dResolve)
			cp = append(cp, dCopy)
			blk = append(blk, dBlock)
			unique += len(list)
			rd, err := ffi.NewArenaReader(buf, ffi.KindExecuteReq)
			if err != nil {
				panic(err)
			}
			found += rd.Len()
			arena += len(buf)
		}
		w.Unpin()

		window = append(window, [4]time.Duration{dLoad, dIndex, dCommit, dBlock})
		if *every > 0 && (i+1)%*every == 0 {
			report(i+1, window, cell, db, sc.historyBytes())
			memory(i+2-len(window), i+1, cell, append([]dbm.DB{db}, sc.dbs...))
			sc.lsm(i+2-len(window), i+1)
			window = window[:0]
		}
	}
	if stopProfile != nil {
		stopProfile()
	}
	if *heapProfile != "" {
		writeHeap("end")
	}
	runtime.KeepAlive(hold)

	// sec is in seconds, because every block limit is set against a 7.45 s block.
	sec := func(d []time.Duration, p float64) float64 { return quantile(d, p).Seconds() }
	// What the engine reports for the cell: Pebble's metrics by level, or
	// goleveldb's stalls at its L0 triggers, what its two caches hold, and
	// compaction by level.
	if p, ok := db.(*dbm.PebbleDB); ok {
		fmt.Printf("# pebble metrics\n# %s\n", strings.ReplaceAll(strings.TrimSpace(p.DB().Metrics().String()), "\n", "\n# "))
		if len(sc.dbs) > 0 {
			h := sc.dbs[0].(*dbm.PebbleDB).DB().Metrics().String()
			fmt.Printf("# pebble metrics history\n# %s\n", strings.ReplaceAll(strings.TrimSpace(h), "\n", "\n# "))
		}
	} else {
		for _, p := range []string{"writedelay", "openedtables", "cachedblock", "stats"} {
			v, _ := db.(*dbm.GoLevelDB).DB().GetProperty("leveldb." + p)
			fmt.Printf("# leveldb.%s %s\n", p, v)
		}
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	// The settings of the collector, which GOGC and GOMEMLIMIT in the
	// environment of the cell set, and what its last cycle marked live.
	gc := []metrics.Sample{{Name: "/gc/gogc:percent"}, {Name: "/gc/gomemlimit:bytes"}, {Name: "/gc/heap/live:bytes"}}
	metrics.Read(gc)
	limit := "off"
	if l := gc[1].Value.Uint64(); l != math.MaxInt64 {
		limit = fmt.Sprintf("%dMiB", l>>20)
	}
	anon, file := cgroupMemory()
	hist := sc.historyLSM().minus(history0)
	namesPerBlock := unique / len(loads)
	per := func(p float64) float64 {
		return float64(quantile(loads, p).Microseconds()) / float64(namesPerBlock)
	}
	fmt.Printf("exp=phase2 label=%s accounts=%d names=%d writes=%d new=%d new_bytes=%d txs=%d tx_names=%d "+
		"index=%t commit=%s mode=%s gap=%s ballast_gib=%d ballast_mmap=%t rounds=%d tables=%d big=%d big_bytes=%d big_load=%t big_far=%t "+
		"aged_blocks=%d sidecar_base=%d history_budget_mb=%d backend=%s overlap=%t gogc=%d gomemlimit=%s "+
		"found_frac=%.4f arena_mb_per_block=%.1f per_name_p50_us=%.2f per_name_p95_us=%.2f per_name_max_us=%.2f "+
		"resolve_p50_s=%.3f load_p50_s=%.3f copy_p50_s=%.3f write_p50_s=%.3f index_p50_s=%.3f commit_p50_s=%.3f rms_commit_p50_s=%.3f "+
		"block_p50_s=%.3f block_p95_s=%.3f block_max_s=%.3f "+
		"heap_live_mb=%d db_mb=%d gc_live_mb=%d anon_mb=%d file_mb=%d "+
		"history_in_mb=%d history_written_mb=%d history_read_mb=%d history_wamp=%.1f\n",
		*label, *accounts, namesPerBlock, *writes, *newAccts, *newBytes, *txs, *txNames,
		*index, *commit, *mode, *gap, *ballast, *offHeap, len(loads), *tables, len(bigKeys), *bigBytes, *bigLoad, *bigFar,
		*agedBlocks, base, budget>>20, *backend, *overlap, int64(gc[0].Value.Uint64()), limit,
		float64(found)/float64(unique), float64(arena)/float64(len(loads))/(1<<20),
		per(.5), per(.95), per(1),
		sec(res, .5), sec(loads, .5), sec(cp, .5), sec(wr, .5), sec(idx, .5), sec(cm, .5), sec(fin, .5),
		sec(blk, .5), sec(blk, .95), sec(blk, 1),
		ms.HeapAlloc>>20, dirBytes(cell)>>20, gc[2].Value.Uint64()>>20, anon>>20, file>>20,
		hist.in>>20, hist.written>>20, hist.read>>20, float64(hist.written)/float64(max(hist.in, 1)))
	// The program exits without closing or deleting anything. Closing the
	// database while the iavl async pruner runs is a fatal "unlock of unlocked
	// mutex". The next run removes this cell before it copies a new one.
	os.Exit(0)
}

// tableKeys returns the -tables accounts of one block. They are distinct and
// drawn from state, so each read finds a value, as the read of a lookup table
// does. A small table, with one address, is 88 bytes. These are 165-byte
// accounts, so a read here costs at least what the read of a small table costs.
func tableKeys(r *rand.Rand, keys [][32]byte) [][32]byte {
	if *tables == 0 {
		return nil
	}
	set := make(map[[32]byte]struct{}, *tables)
	for len(set) < *tables {
		set[keys[r.Intn(len(keys))]] = struct{}{}
	}
	out := make([][32]byte, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// resolve reads the tables the way Runner.resolve does before phase 2. It uses
// PushTables, eight shards at a time, and copies each value into a resolve
// request. It leaves out the work of the engine on them, which decodes each
// table and builds the load list again.
func resolve(stores svm.Stores, ms storetypes.MultiStore, tbl [][32]byte) {
	if len(tbl) == 0 {
		return
	}
	rw := ffi.NewArenaWriter(ffi.KindResolveReq, 0)
	stores.PushTables(rw, ms, tbl, svm.ArenaBytes)
	rw.Finish()
	rw.Unpin()
}

// plantBig writes -big accounts of -big-bytes into the cell and commits them
// before the first block, untimed. It returns their keys and one value of
// that size. Every block then loads them. This is the shape that the 512 MiB
// arena of the engine bounds: nine transactions that declare 64 MiB each.
// With -big-far, each key starts with the three top bits of its shard set and
// then 0xff three times. That is the top of the key range of the shard, where
// no account in state is. So no write in a block lands near one in the tree or
// in the database.
func plantBig(rms *rootmulti.Store, stores svm.Stores) ([][32]byte, []byte) {
	if *big == 0 {
		return nil, nil
	}
	r := rand.New(rand.NewSource(66))
	data := make([]byte, *bigBytes)
	r.Read(data)
	value := account(data, 1<<40)
	cms := rms.CacheMultiStore()
	keys := make([][32]byte, *big)
	for i := range keys {
		r.Read(keys[i][:])
		if *bigFar {
			keys[i][0] = byte(i%8)<<5 | 0x1f
			keys[i][1], keys[i][2], keys[i][3] = 0xff, 0xff, 0xff
		}
		stores.SetAccount(cms, keys[i], value)
	}
	cms.Write()
	rms.Commit()
	return keys, data
}

// copyBig stands in for the one copy that the engine makes of each loaded
// account into its own memory (engine/src/runtime.rs arena_accounts,
// set_data_from_slice). It copies the data of each big account. The engine's
// copy goes to Rust's heap, so this one reuses one buffer. A new buffer for
// each copy gives Go's collector 500 MiB of garbage a block, and a node makes
// none.
func copyBig(n int, data []byte) {
	if len(sink) != len(data) {
		sink = make([]byte, len(data))
	}
	for range n {
		copy(sink, data)
	}
}

// sink is the buffer of copyBig. It is a package variable, so the compiler
// keeps the copies.
var sink []byte

// quantile is the p-quantile of d, the sorted element at floor(p*(len-1)).
func quantile(d []time.Duration, p float64) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[int(p*float64(len(s)-1))]
}

// report prints a window of blocks that ends at block last. The line holds
// the p50 of each phase, the p95 and max of the block, and the size of the
// cell database and HeapAlloc after the last commit of the window. It also
// holds the write stalls of the engine so far (writeDelay). HeapAlloc is
// heap_live_mb for the runs that came before, but it counts garbage that is
// not yet swept. The gc_live_mb of the memory line is the live heap. Then it
// holds the memory that the MemoryMax of a capped cell splits: what Go holds
// from the system for its heap, the collections and their pauses so far, and
// the anonymous and page-cache bytes of the cgroup. The line starts with #,
// so bench/report.py reads past it.
func report(last int, window [][4]time.Duration, cell string, db dbm.DB, history uint64) {
	col := func(c int) []time.Duration {
		out := make([]time.Duration, len(window))
		for i, w := range window {
			out[i] = w[c]
		}
		return out
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	blocks := col(3)
	anon, file := cgroupMemory()
	delay := writeDelay(db)
	fmt.Printf("# window blocks=%d-%d load_p50_s=%.3f index_p50_s=%.3f commit_p50_s=%.3f "+
		"block_p50_s=%.3f block_p95_s=%.3f block_max_s=%.3f db_mb=%d heap_live_mb=%d "+
		"go_held_mb=%d gc=%d gc_pause_ms=%d gc_cpu=%.3f anon_mb=%d file_mb=%d history_mb=%d writedelay=%q\n",
		last-len(window)+1, last,
		quantile(col(0), .5).Seconds(), quantile(col(1), .5).Seconds(), quantile(col(2), .5).Seconds(),
		quantile(blocks, .5).Seconds(), quantile(blocks, .95).Seconds(), quantile(blocks, 1).Seconds(),
		dirBytes(cell)>>20, ms.HeapAlloc>>20,
		(ms.HeapSys-ms.HeapReleased)>>20, ms.NumGC, ms.PauseTotalNs/1e6, ms.GCCPUFraction, anon>>20, file>>20, history>>20, delay)
}

// cgroupMemory returns the anon and file bytes from memory.stat of the
// cgroup v2 group that this process runs in. It returns zeros when there is no
// such group.
func cgroupMemory() (anon, file uint64) {
	s := cgroupStat()
	return s["anon"], s["file"]
}

// cgroupFile reads a file of the cgroup v2 group that this process runs in. It
// returns nothing when there is no such group.
func cgroupFile(name string) string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	var group string
	for _, line := range strings.Split(string(b), "\n") {
		if g, ok := strings.CutPrefix(line, "0::"); ok {
			group = g
		}
	}
	b, _ = os.ReadFile(filepath.Join("/sys/fs/cgroup", group, name))
	return string(b)
}

// cgroupStat is memory.stat of the group, by key.
func cgroupStat() map[string]uint64 {
	out := map[string]uint64{}
	for _, line := range strings.Split(cgroupFile("memory.stat"), "\n") {
		k, v, _ := strings.Cut(line, " ")
		out[k], _ = strconv.ParseUint(v, 10, 64)
	}
	return out
}

// stalled returns the memory pressure totals of the group in seconds (PSI).
// some is the time that at least one of its tasks waited on memory, on
// reclaim, or on the refault of a page that the cache dropped. full is the
// time that all of them waited.
func stalled() (some, full float64) {
	for _, line := range strings.Split(cgroupFile("memory.pressure"), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		us, _ := strconv.ParseFloat(strings.TrimPrefix(f[len(f)-1], "total="), 64)
		switch f[0] {
		case "some":
			some = us / 1e6
		case "full":
			full = us / 1e6
		}
	}
	return some, full
}

// anonPeak is the most anonymous memory that the cgroup held at any 100 ms
// sample since the last memory line. It shows a peak inside a block, which a
// line read after the commit of the block misses.
var anonPeak atomic.Uint64

func sampleAnon() {
	for range time.Tick(100 * time.Millisecond) {
		a, _ := cgroupMemory()
		for {
			p := anonPeak.Load()
			if a <= p || anonPeak.CompareAndSwap(p, a) {
				break
			}
		}
	}
}

// memory prints the memory line of the window of blocks first to last, after
// its window line, so that every byte of the cgroup has a row. It holds the Go
// runtime by class and its collector, what Pebble and malloc hold outside the
// Go heap, the share of each database in the page cache, and the peak
// anonymous memory of the cgroup inside the window. It also holds the churn of
// the page cache (refaults of pages that it dropped, major faults) and the
// time that its tasks stalled on memory. The refault, fault, stall, alloc and
// CPU figures count from the start of the cgroup or the process. The figure of
// a window is the difference from the line before. The line starts with #, so
// bench/report.py reads past it.
func memory(first, last int, cell string, dbs []dbm.DB) {
	s := cgroupStat()
	some, full := stalled()
	held, used := mallocHeld()
	fmt.Printf("# memory blocks=%d-%d anon_peak_mb=%d active_file_mb=%d inactive_file_mb=%d refault_file=%d pgmajfault=%d "+
		"psi_some_s=%.1f psi_full_s=%.1f malloc_held_mb=%d malloc_used_mb=%d%s%s%s\n",
		first, last, anonPeak.Swap(0)>>20, s["active_file"]>>20, s["inactive_file"]>>20, s["workingset_refault_file"], s["pgmajfault"],
		some, full, held>>20, used>>20, goRuntime(), pebbleMemory(dbs), pageCache(cell))
}

// goRuntime is the account of the Go runtime for itself, from runtime/metrics.
// It holds every class of memory that it holds from the system, in MiB
// (go_heap_objects_mb, go_heap_unused_mb, go_heap_free_mb,
// go_heap_released_mb, the stacks, the metadata classes, go_total_mb), then the
// collector. gc_live_mb is what the last cycle marked live. The heap_live_mb of
// the window line is HeapAlloc, which also counts garbage that is not yet
// swept, so it swings from live to the goal each cycle. limiter_cycle is the
// last cycle that the memory limit drove, or 0 if none.
func goRuntime() string {
	var s []metrics.Sample
	for _, d := range metrics.All() {
		if strings.HasPrefix(d.Name, "/memory/classes/") {
			s = append(s, metrics.Sample{Name: d.Name})
		}
	}
	classes := len(s)
	gc := [][2]string{
		{"/gc/heap/live:bytes", "gc_live_mb"},
		{"/gc/heap/goal:bytes", "gc_goal_mb"},
		{"/gc/cycles/total:gc-cycles", "gc_cycles"},
		{"/gc/limiter/last-enabled:gc-cycle", "limiter_cycle"},
		{"/gc/heap/allocs:bytes", "alloc_total_mb"},
		{"/cpu/classes/gc/total:cpu-seconds", "gc_cpu_total_s"},
		{"/cpu/classes/total:cpu-seconds", "cpu_total_s"},
	}
	for _, g := range gc {
		s = append(s, metrics.Sample{Name: g[0]})
	}
	metrics.Read(s)
	name := strings.NewReplacer("/memory/classes/", "go_", "/", "_", "-", "_", ":bytes", "_mb")
	var b strings.Builder
	for i, x := range s {
		key := name.Replace(x.Name)
		if i >= classes {
			key = gc[i-classes][1]
		}
		switch x.Value.Kind() {
		case metrics.KindUint64:
			v := x.Value.Uint64()
			if strings.HasSuffix(key, "_mb") {
				v >>= 20
			}
			fmt.Fprintf(&b, " %s=%d", key, v)
		case metrics.KindFloat64:
			fmt.Fprintf(&b, " %s=%.1f", key, x.Value.Float64())
		}
	}
	return b.String()
}

// pebbleMemory is what Pebble holds outside the Go heap, over the databases of
// the cell: their block caches, and their memtables, live and zombie. A cgo
// build allocates both with calloc (pebble internal/manual), so both are in
// malloc_used_mb and in none of the classes of Go.
func pebbleMemory(dbs []dbm.DB) string {
	var cache, mem, zombie uint64
	for _, d := range dbs {
		if p, ok := d.(*dbm.PebbleDB); ok {
			m := p.DB().Metrics()
			cache += uint64(m.BlockCache.Size)
			mem += m.MemTable.Size
			zombie += m.MemTable.ZombieSize
		}
	}
	return fmt.Sprintf(" pebble_cache_mb=%d pebble_memtable_mb=%d pebble_zombie_mb=%d", cache>>20, mem>>20, zombie>>20)
}

// lsm holds three byte counts of a Pebble database since it opened: what it
// took in through its WAL, what its flushes and compactions wrote, and what
// its compactions read. written divided by in is its write amplification.
type lsm struct{ in, written, read uint64 }

func lsmOf(d dbm.DB) lsm {
	p, ok := d.(*dbm.PebbleDB)
	if !ok {
		return lsm{}
	}
	m := p.DB().Metrics()
	l := lsm{in: m.WAL.BytesIn}
	for _, v := range m.Levels {
		l.written += v.BytesFlushed + v.BytesCompacted
		l.read += v.BytesRead
	}
	return l
}

func (l lsm) minus(o lsm) lsm { return lsm{l.in - o.in, l.written - o.written, l.read - o.read} }

// historyLSM is the lsm of history.db. It is zero without the sidecars.
func (s sidecars) historyLSM() lsm {
	if len(s.dbs) == 0 {
		return lsm{}
	}
	return lsmOf(s.dbs[0])
}

// lsm prints the lsm line of the window of blocks first to last, next to its
// memory line. The line gives the lsm of each sidecar in MB since it opened.
// The figure of a window is the difference from the line before. The line
// starts with #, so bench/report.py reads past it.
func (s sidecars) lsm(first, last int) {
	var b strings.Builder
	for i, d := range s.dbs {
		name := []string{"history", "tokens", "dedup"}[i]
		l := lsmOf(d)
		fmt.Fprintf(&b, " %s_in_mb=%d %s_written_mb=%d %s_read_mb=%d", name, l.in>>20, name, l.written>>20, name, l.read>>20)
	}
	fmt.Printf("# lsm blocks=%d-%d%s\n", first, last, b.String())
}

// pageCache is the share of each database of the cell in the page cache, as
// cache_NAME_mb: application, history, tokens and dedup.
func pageCache(cell string) string {
	var b strings.Builder
	dbs, _ := filepath.Glob(filepath.Join(cell, "*.db"))
	for _, d := range dbs {
		fmt.Fprintf(&b, " cache_%s_mb=%d", strings.TrimSuffix(filepath.Base(d), ".db"), resident(d)>>20)
	}
	return b.String()
}

// resident is the bytes of the files under dir that are in the page cache. It
// uses mincore over a read-only mapping of each file, which reads nothing in.
func resident(dir string) int64 {
	page := int64(os.Getpagesize())
	var pages int64
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || info.Size() == 0 {
			return nil
		}
		m, err := syscall.Mmap(int(f.Fd()), 0, int(info.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			return nil
		}
		defer syscall.Munmap(m)
		vec := make([]byte, (int64(len(m))+page-1)/page)
		if _, _, e := syscall.Syscall(syscall.SYS_MINCORE, uintptr(unsafe.Pointer(&m[0])), uintptr(len(m)), uintptr(unsafe.Pointer(&vec[0]))); e != 0 {
			return nil
		}
		for _, v := range vec {
			pages += int64(v & 1)
		}
		return nil
	})
	return pages * page
}

// open mounts the app's account and meta stores the way app.New and svmd do.
func open(path string) (*rootmulti.Store, svm.Stores, dbm.DB) {
	var db dbm.DB
	var err error
	if *backend == "pebble" {
		db, err = app.OpenApplicationDB(path)
	} else {
		o := goleveldbOptions()
		o.DisableBufferPool = *noBufferPool
		db, err = dbm.NewGoLevelDBWithOpts("application", path, o)
	}
	if err != nil {
		panic(err)
	}
	rms := rootmulti.NewStore(db, log.NewNopLogger())
	if *interBlock {
		rms.SetInterBlockCache(store.NewCommitKVStoreCacheManager())
	}
	rms.SetIAVLCacheSize(app.IAVLCacheSize)
	rms.SetPruning(pruningtypes.NewCustomPruningOptions(2, 10))
	stores := svm.NewStores()
	for _, k := range stores.All() {
		rms.MountStoreWithDB(k, storetypes.StoreTypeIAVL, nil)
	}
	if err := rms.LoadLatestVersion(); err != nil {
		panic(err)
	}
	p := rms.GetPruning()
	p.Interval = 1
	rms.SetPruning(p)
	return rms, stores, db
}

func seed(keys [][32]byte) string {
	d := filepath.Join(*dir, fmt.Sprintf("phase2-seed-%d", len(keys)))
	if *backend == "pebble" {
		d += "-pebble"
	}
	if _, err := os.Stat(filepath.Join(d, "DONE")); err == nil {
		return d
	}
	os.RemoveAll(d)
	t0 := time.Now()
	rms, stores, db := open(d)
	blobs := values()
	cms := rms.CacheMultiStore()
	for i, k := range keys {
		stores.SetAccount(cms, k, account(blobs[i%len(blobs)], 2_039_280))
		if i%50_000 == 49_999 {
			cms.Write()
			rms.Commit()
			cms = rms.CacheMultiStore()
		}
	}
	cms.Write()
	rms.Commit()
	db.Close()
	if err := os.WriteFile(filepath.Join(d, "DONE"), nil, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("# seed accounts=%d took_s=%.0f db_mb=%d\n", len(keys), time.Since(t0).Seconds(), dirBytes(d)>>20)
	restart()
	return d
}

// startProfile starts a CPU profile and an execution trace. It also samples
// every contended mutex and every blocking event of 10 us or more. It returns
// two functions that write them into dir. The first writes the three
// profiles. The second writes the trace.
func startProfile(dir string) (func(), func()) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	create := func(name string) *os.File {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			panic(err)
		}
		return f
	}
	cpu, tr := create("cpu.pprof"), create("trace.out")
	if err := pprof.StartCPUProfile(cpu); err != nil {
		panic(err)
	}
	if err := trace.Start(tr); err != nil {
		panic(err)
	}
	runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(10_000)
	stopTrace := func() {
		trace.Stop()
		tr.Close()
	}
	return func() {
		pprof.StopCPUProfile()
		cpu.Close()
		for _, name := range []string{"mutex", "block"} {
			f := create(name + ".pprof")
			if err := pprof.Lookup(name).WriteTo(f, 0); err != nil {
				panic(err)
			}
			f.Close()
		}
	}, stopTrace
}

// writeHeap writes the Go heap profile as heap-NAME.pb.gz under -heap-profile
// and the label. Its inuse samples are what the last collection marked live,
// so they hold no garbage, where heap_live_mb of the window line (HeapAlloc)
// does. Its alloc samples count from the start of the process, so
// `go tool pprof -diff_base heap-start.pb.gz heap-end.pb.gz` shows what the
// measured blocks allocated, by site.
func writeHeap(name string) {
	dir := filepath.Join(*heapProfile, *label)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	f, err := os.Create(filepath.Join(dir, "heap-"+name+".pb.gz"))
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := pprof.Lookup("heap").WriteTo(f, 0); err != nil {
		panic(err)
	}
}

// restart runs this command again in place of this process, once a seed is
// built. The build leaves the trees it committed through on the heap of this
// process, with their node caches and pruners. Nothing frees them. Under the
// MemoryMax of a capped cell, that heap is page cache that the cell does not
// get. A wi-20g cell that built the seed held 17 to 19 GB of its 20 GB and ran
// with 1.2 to 3.3 GB of page cache. The new process finds the seed done and
// only opens it.
func restart() {
	self, err := os.Executable()
	if err != nil {
		panic(err)
	}
	err = syscall.Exec(self, os.Args, os.Environ())
	panic(err)
}

// loadList returns one block's load list: -names distinct keys, sorted.
func loadList(r *rand.Rand, keys [][32]byte) [][32]byte {
	set := make(map[[32]byte]struct{}, *names)
	for len(set) < *names {
		var k [32]byte
		if *mode == "miss" {
			r.Read(k[:])
		} else {
			k = keys[r.Intn(len(keys))]
		}
		set[k] = struct{}{}
	}
	list := make([][32]byte, 0, len(set))
	for k := range set {
		list = append(list, k)
	}
	sort.Slice(list, func(i, j int) bool { return string(list[i][:]) < string(list[j][:]) })
	return list
}

func accountKeys(n int) [][32]byte {
	r := rand.New(rand.NewSource(26))
	keys := make([][32]byte, n)
	for i := range keys {
		r.Read(keys[i][:])
	}
	return keys
}

func values() [][]byte {
	r := rand.New(rand.NewSource(1))
	out := make([][]byte, 256)
	for i := range out {
		out[i] = make([]byte, dataLen)
		r.Read(out[i])
	}
	return out
}

// owner is the SPL Token program. account sets byte 108 to the initialized
// state, so every seeded account is an SPL token account. svm.Classify accepts
// it, and the owner index does the same work as in a block. The data length
// stays 165 bytes, so the tree does not change size.
var owner = svm.SplToken

func account(data []byte, lamports uint64) []byte {
	if len(data) == dataLen {
		d := slices.Clone(data)
		d[108] = 1
		data = d
	}
	return ffi.EncodeAccount(&owner, lamports, svm.RentExemptEpoch, false, data)
}

func dirBytes(d string) int64 {
	var n int64
	filepath.Walk(d, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return n
}

// setOp is one account the synthetic block writes.
type setOp struct {
	key   [32]byte
	value []byte
}

// block builds one block's write set: -writes overwritten accounts and -new
// allocated ones. With -index it also builds -txs transaction records.
func block(r *rand.Rand, keys [][32]byte, blobs [][]byte) ([]svm.Record, []setOp) {
	sets := make([]setOp, 0, *writes+*newAccts)
	for i := 0; i < *writes; i++ {
		k := keys[r.Intn(len(keys))]
		sets = append(sets, setOp{k, account(blobs[r.Intn(len(blobs))], uint64(r.Int63n(1e9)+1))})
	}
	data := make([]byte, *newBytes)
	r.Read(data)
	for i := 0; i < *newAccts; i++ {
		var k [32]byte
		r.Read(k[:])
		sets = append(sets, setOp{k, account(data, 2_039_280)})
	}
	if !*index {
		return nil, sets
	}
	recs := make([]svm.Record, 0, *txs)
	for i := 0; i < *txs; i++ {
		tx, sig, _ := transaction(r, keys, *txNames)
		recs = append(recs, svm.Record{
			Index:     uint32(i),
			Tx:        tx,
			Signature: sig,
			Fee:       5000,
			Units:     3000,
		})
	}
	return recs, sets
}

// transaction builds a legacy Solana transaction that names n keys (at least
// two), without an engine round trip. The last key is the System Program.
// Only the message must parse. The sidecars read the keys from it and do not
// verify the signature. It also returns the static keys of the message.
func transaction(r *rand.Rand, keys [][32]byte, n int) ([]byte, [64]byte, [][32]byte) {
	if n < 2 {
		n = 2
	}
	m := []byte{1, 0, 1}
	m = append(m, cu16(n)...)
	static := make([][32]byte, 0, n)
	for i := 0; i < n-1; i++ {
		k := keys[r.Intn(len(keys))]
		m = append(m, k[:]...)
		static = append(static, k)
	}
	m = append(m, svm.SystemProgram[:]...)
	static = append(static, svm.SystemProgram)
	var hash [32]byte
	r.Read(hash[:])
	m = append(m, hash[:]...)
	m = append(m, 1)            // one instruction
	m = append(m, byte(n-1))    // the program is the last key
	m = append(m, cu16(n-1)...) // every other key is an account
	for i := 0; i < n-1; i++ {
		m = append(m, byte(i))
	}
	m = append(m, 12)
	m = append(m, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	var sig [64]byte
	r.Read(sig[:])
	tx := append([]byte{1}, sig[:]...)
	return append(tx, m...), sig, static
}

// cu16 encodes n as Solana's compact-u16 (short_vec).
func cu16(n int) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}

// sidecars holds the three databases beside the commit tree. A block writes
// all three before it commits, so their cost adds to the block latency.
type sidecars struct {
	history *svm.History
	tokens  *svm.TokenIndex
	dedup   *svm.Dedup
	dbs     []dbm.DB
}

// openSidecars opens the sidecars of the cell as svmd opens them at a
// restart. committed is the last height that they hold, so the purge of
// history.db runs at the budget, as it runs on a validator. tokens is the name
// of the database of the token index.
func openSidecars(cell, tokens string, committed, budget uint64) sidecars {
	var s sidecars
	if !*index {
		return s
	}
	open := func(name string) dbm.DB {
		db := sidecarDB(name, cell)
		s.dbs = append(s.dbs, db)
		return db
	}
	var err error
	if s.history, err = svm.OpenHistory(open("history"), committed, budget); err != nil {
		panic(err)
	}
	if s.tokens, err = svm.OpenTokenIndex(open(tokens), log.NewNopLogger()); err != nil {
		panic(err)
	}
	if s.dedup, err = svm.OpenDedup(open("dedup"), committed); err != nil {
		panic(err)
	}
	return s
}

func (s sidecars) historyBytes() uint64 {
	if s.history == nil {
		return 0
	}
	return s.history.Bytes()
}

// before is each written account's token index entry before the block,
// which the runner takes from phase 2's load of the account.
func (s sidecars) before(sets []setOp) []svm.TokenEntry {
	if s.tokens == nil {
		return nil
	}
	out := make([]svm.TokenEntry, len(sets))
	for i, set := range sets {
		e, err := s.tokens.Entry(set.key)
		if err != nil {
			panic(err)
		}
		out[i] = e
	}
	return out
}

// write records a block in the sidecars and returns the wait for what it
// leaves running. With beside, as Runner.Run does, it
// updates the dedup set and puts its commitment in the block, and leaves the
// three database writes on their own goroutine; otherwise it writes them
// itself and leaves nothing.
func (s sidecars) write(stores svm.Stores, cms storetypes.CacheMultiStore, height uint64, recs []svm.Record, sets []setOp, before []svm.TokenEntry, beside bool) func() error {
	none := func() error { return nil }
	if s.history == nil {
		return none
	}
	var hash [32]byte
	binary.LittleEndian.PutUint64(hash[:8], height)
	for i := range recs {
		recs[i].Height = height
	}
	writes := make([]svm.AccountWrite, len(sets))
	for i, set := range sets {
		writes[i] = svm.Classify(set.key, set.value)
		writes[i].Before = before[i]
	}
	if beside {
		save, err := s.dedup.Spend(height, spent(recs))
		if err != nil {
			panic(err)
		}
		stores.SetDedupCommitment(cms, s.dedup.Commitment(height))
		return svm.Background(
			func() error { return s.history.Write(height, hash, 0, recs) },
			func() error { return s.tokens.Write(height, writes) },
			save)
	}
	if err := s.history.Write(height, hash, 0, recs); err != nil {
		panic(err)
	}
	if err := s.tokens.Write(height, writes); err != nil {
		panic(err)
	}
	if err := s.dedup.Commit(height, spent(recs)); err != nil {
		panic(err)
	}
	stores.SetDedupCommitment(cms, s.dedup.Commitment(height))
	return none
}

// spent returns the dedup keys of the records of a block.
func spent(recs []svm.Record) [][64]byte {
	out := make([][64]byte, len(recs))
	for i, rec := range recs {
		out[i] = [64]byte(append(append([]byte{}, rec.Signature[:32]...), sha256.New().Sum(rec.Signature[:])[:32]...))
	}
	return out
}

// flushApp is the commit order of the app before rootmulti.Commit, as in
// app/app.go. FlushShards runs at the end of FinalizeBlock, the SDK computes
// workingHash, and SaveShards runs at the start of Commit. Then the
// commitStores of the SDK finds the shards done.
func flushApp(rms *rootmulti.Store, stores svm.Stores, cms storetypes.CacheMultiStore) {
	stores.FlushShards(rms, cms)
	cms.Write()
	rms.WorkingHash()
	stores.SaveShards(rms)
}

// fillBlock returns one block of -fill-txs system transfers as a node
// records them. Each is a legacy transaction that names a payer, a recipient
// and the System Program, with the meta that the execution of a transfer
// returns. The meta holds the static keys that the address index reads.
func fillBlock(r *rand.Rand, keys [][32]byte, height uint64) []svm.Record {
	units := uint64(24_352)
	recs := make([]svm.Record, *fillTxs)
	for i := range recs {
		tx, sig, static := transaction(r, keys, 3)
		pre := uint64(r.Int63n(1e12) + 1e9)
		recs[i] = svm.Record{
			Height:    height,
			Index:     uint32(i),
			BlockTime: 1_800_000_000 + int64(height),
			Tx:        tx,
			Signature: sig,
			Fee:       5000,
			Units:     150,
			Meta: ffi.TxMeta{
				PreBalances:  []uint64{pre, 2_039_280, 1},
				PostBalances: []uint64{pre - 5000 - 1_000_000, 2_039_280 + 1_000_000, 1},
				Logs: []string{
					"Program 11111111111111111111111111111111 invoke [1]",
					"Program 11111111111111111111111111111111 success",
				},
				CostUnits:  &units,
				StaticKeys: static,
			},
		}
	}
	return recs
}

// historyBudget returns -history-bytes. With -history-blocks, it returns
// that many blocks of the transfers of fillBlock instead. A History counts the
// bytes of each block, as index-max-bytes counts them.
func historyBudget(keys [][32]byte) uint64 {
	if *historyBlocks == 0 {
		return *historyBytes
	}
	h, err := svm.OpenHistory(dbm.NewMemDB(), 0, math.MaxUint64)
	if err != nil {
		panic(err)
	}
	if err := h.Write(1, [32]byte{1}, 0, fillBlock(rand.New(rand.NewSource(320)), keys, 1)); err != nil {
		panic(err)
	}
	fmt.Printf("# history block=%d transfers bytes=%d per_transfer=%d budget_blocks=%d budget_mb=%d\n",
		*fillTxs, h.Bytes(), h.Bytes()/uint64(*fillTxs), *historyBlocks, uint64(*historyBlocks)*h.Bytes()>>20)
	return uint64(*historyBlocks) * h.Bytes()
}

// aged returns the aged seed for these flags and the height that its
// sidecars hold. It builds the seed once from the young seed. First it fills
// history.db with blocks of transfers until the purge has run for 10 blocks.
// Then -aged-blocks blocks of -age-writes accounts and -age-txs transactions
// commit in the order of the app. This ages the application database past
// the point where the 500-block runs settle. Last, it rebuilds tokens.db from
// every account in one pass. svmd makes the same pass when the index is
// missing, because a validator indexes every token account from genesis.
// dedup.db stays as the aging blocks leave it.
func aged(young string, keys [][32]byte, budget uint64) (string, uint64) {
	if !*index {
		panic("-aged-blocks needs -index: the aged seed is the sidecars as much as the tree")
	}
	d := fmt.Sprintf("%s-aged%d-w%d-hist%dmb", young, *agedBlocks, *ageWrites, budget>>20)
	if b, err := os.ReadFile(filepath.Join(d, "DONE")); err == nil {
		h, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		if err != nil {
			panic(err)
		}
		return d, h
	}
	os.RemoveAll(d)
	if out, err := exec.Command("cp", "-a", young, d).CombinedOutput(); err != nil {
		panic(fmt.Sprintf("copy seed: %v: %s", err, out))
	}
	os.Remove(filepath.Join(d, "DONE"))
	t0 := time.Now()
	rms, stores, db := open(d)
	sc := openSidecars(d, "tokens-aging", 0, budget)
	r := rand.New(rand.NewSource(320))
	var height uint64
	for purged := 0; purged < 10; {
		height++
		recs := fillBlock(r, keys, height)
		if err := sc.history.Write(height, [32]byte{byte(height)}, recs[0].BlockTime, recs); err != nil {
			panic(err)
		}
		if err := sc.dedup.Commit(height, spent(recs)); err != nil {
			panic(err)
		}
		if sc.history.FirstAvailableBlock() > 1 {
			purged++
		}
		if height%500 == 0 {
			fmt.Printf("# aged fill height=%d history_mb=%d took_s=%.0f\n", height, sc.history.Bytes()>>20, time.Since(t0).Seconds())
		}
	}
	tFill := time.Since(t0)

	t1 := time.Now()
	w, n, k := *writes, *txs, *txNames
	*writes, *txs, *txNames = *ageWrites, *ageTxs, *ageTxNames
	blobs := values()
	for range *agedBlocks {
		height++
		recs, sets := block(r, keys, blobs)
		prior := sc.before(sets)
		cms := rms.CacheMultiStore()
		for _, set := range sets {
			stores.SetAccount(cms, set.key, set.value)
		}
		sc.write(stores, cms, height, recs, sets, prior, false)
		flushApp(rms, stores, cms)
		rms.Commit()
	}
	*writes, *txs, *txNames = w, n, k
	tAge := time.Since(t1)

	t2 := time.Now()
	tdb := sidecarDB("tokens", d)
	tokens, err := svm.OpenTokenIndex(tdb, log.NewNopLogger())
	if err != nil {
		panic(err)
	}
	// No committed version is passed, so Follow refills the index now from
	// the multistore that it gets. It does the same for the first block of a
	// chain.
	none := func() (storetypes.MultiStore, uint64, error) { return nil, 0, nil }
	if _, err := tokens.Follow(height, nil, svm.Lamports{}, stores, rms.CacheMultiStore(), none); err != nil {
		panic(err)
	}
	tTokens := time.Since(t2)

	for _, x := range append(sc.dbs, tdb, db) {
		if err := x.Close(); err != nil {
			panic(err)
		}
	}
	os.RemoveAll(filepath.Join(d, "tokens-aging.db"))
	sizes := ""
	for _, name := range []string{"application", "history", "tokens", "dedup"} {
		sizes += fmt.Sprintf(" %s_mb=%d", name, dirBytes(filepath.Join(d, name+".db"))>>20)
	}
	fmt.Printf("# aged seed height=%d fill_blocks=%d fill_s=%.0f age_blocks=%d age_s=%.0f tokens_s=%.0f%s\n",
		height, height-uint64(*agedBlocks), tFill.Seconds(), *agedBlocks, tAge.Seconds(), tTokens.Seconds(), sizes)
	if err := os.WriteFile(filepath.Join(d, "DONE"), []byte(strconv.FormatUint(height, 10)), 0o644); err != nil {
		panic(err)
	}
	restart()
	return d, height
}

// sidecarDB opens a sidecar database as svmd does. With -backend goleveldb it
// opens one at cosmos-db's goleveldb defaults with its bloom filter, as svmd
// did before it moved to Pebble.
func sidecarDB(name, dir string) dbm.DB {
	open := app.OpenIndexDB
	if name == "dedup" {
		open = app.OpenSidecarDB
	}
	if *backend != "pebble" {
		open = func(name, dir string) (dbm.DB, error) { return dbm.NewDB(name, dbm.GoLevelDBBackend, dir) }
	}
	db, err := open(name, dir)
	if err != nil {
		panic(err)
	}
	return db
}

// writeDelay is a window's account of write stalls: the shape of Pebble's L0,
// which is what stalls Pebble's writes, or goleveldb's own counter.
func writeDelay(db dbm.DB) string {
	if p, ok := db.(*dbm.PebbleDB); ok {
		m := p.DB().Metrics()
		return fmt.Sprintf("L0 files=%d sublevels=%d", m.Levels[0].NumFiles, m.Levels[0].Sublevels)
	}
	v, _ := db.(*dbm.GoLevelDB).DB().GetProperty("leveldb.writedelay")
	return v
}
