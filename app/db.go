package app

import (
	"path/filepath"
	"sync"
	"syscall"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/bloom"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sys/unix"
)

// OpenApplicationDB opens dir/application.db on Pebble. svmd, testchain and
// bench/blockcost all use it. Its 256 MiB block cache holds every index and
// filter block of ten million accounts (about 110 MB) and as much again for
// data blocks. It may hold up to half the descriptor limit open.
func OpenApplicationDB(dir string) (dbm.DB, error) {
	return openPebble("application", dir, 256<<20, openFiles(4096, 2), 0, nil)
}

// OpenSidecarDB opens dir/name.db on Pebble for the dedup record, the history
// or the token index. Its block cache is 8 MiB. One 128 MiB cache shared by
// the three did not make their part of a block shorter. Each may hold up to
// an eighth of the descriptor limit open.
//
// A sidecar compacts L0 at 8 files, not at Pebble's 500. Every block deletes
// the keys of an old height, and each batch of deletes flushes as an L0 file
// that overlaps no other. Such files share one sublevel, so the sublevel
// trigger never fires, and the deletes stay above the keys they delete. In a
// replay of the dedup record, 3,000 heights grew it to 1.4 GB at 500 files.
// At 8 files it stayed near 250 MB.
func OpenSidecarDB(name, dir string) (dbm.DB, error) {
	return openPebble(name, dir, 8<<20, openFiles(1000, 8), 8, nil)
}

// OpenIndexDB opens dir/name.db as OpenSidecarDB does, for the history and the
// token index. Each table that a flush or a compaction writes leaves the page
// cache. A block writes these tables. It reads back only the oldest heights of
// the history, to purge them beside the commit. Before, what they wrote stayed
// in the page cache in place of the pages of application.db, which a block
// reads. The dedup record opens with OpenSidecarDB, because Dedup.Spend reads
// an expiring height inside the block.
func OpenIndexDB(name, dir string) (dbm.DB, error) {
	db, _, err := openIndex(name, dir)
	return db, err
}

// openIndex is OpenIndexDB, and also returns the drops that it starts. Its
// test waits on them. Pebble syncs a table before it fires FlushEnd or
// CompactionEnd, so every page of the table is clean when the drop runs, and
// POSIX_FADV_DONTNEED drops them all. Pebble holds its own lock when it fires
// these events, so the listener only starts the drop, on a goroutine of its
// own: three syscalls a table. The inputs of a compaction need nothing. Pebble
// deletes them, and their pages go with them.
func openIndex(name, dir string) (dbm.DB, *sync.WaitGroup, error) {
	path := filepath.Join(dir, name+dbm.DBFileSuffix)
	drops := new(sync.WaitGroup)
	drop := func(tables []pebble.TableInfo) {
		drops.Add(1)
		go func() {
			defer drops.Done()
			for _, t := range tables {
				fd, err := unix.Open(filepath.Join(path, t.FileNum.String()+".sst"), unix.O_RDONLY|unix.O_CLOEXEC, 0)
				if err != nil {
					continue // a compaction deleted it already
				}
				_ = unix.Fadvise(fd, 0, 0, unix.FADV_DONTNEED)
				_ = unix.Close(fd)
			}
		}()
	}
	events := &pebble.EventListener{
		FlushEnd:      func(i pebble.FlushInfo) { drop(i.Output) },
		CompactionEnd: func(i pebble.CompactionInfo) { drop(i.Output.Tables) },
	}
	db, err := openPebble(name, dir, 8<<20, openFiles(1000, 8), 8, events)
	return db, drops, err
}

// openPebble sizes Pebble for ten million accounts, as goleveldb was sized.
// A 10-bit bloom filter on every level means a probe of a file without the
// key reads no data block. A 32 MiB memtable holds the writes of a worst
// block. Files are 8 MiB on every level, where Pebble's first level starts at
// 2 MiB. cosmos-db adds three concurrent compactions and drops Pebble's info
// logs. l0Files, when not 0, is the L0 file count that starts a compaction.
// events, when not nil, is the event listener of the database.
func openPebble(name, dir string, cacheBytes int64, files, l0Files int, events *pebble.EventListener) (dbm.DB, error) {
	cache := pebble.NewCache(cacheBytes)
	defer cache.Unref()
	o := &pebble.Options{
		Cache:        cache,
		MaxOpenFiles: files,
		MemTableSize: 32 << 20,
		Levels:       make([]pebble.LevelOptions, 7),
	}
	o.EventListener = events
	for i := range o.Levels {
		o.Levels[i].FilterPolicy = bloom.FilterPolicy(10)
		o.Levels[i].FilterType = pebble.TableFilter
		o.Levels[i].TargetFileSize = 8 << 20
	}
	if l0Files != 0 {
		o.L0CompactionFileThreshold = l0Files
	}
	db, err := dbm.NewPebbleDBWithOpts(name, dir, o)
	if err != nil {
		return nil, err
	}
	return db, nil
}

var (
	pebbleCacheBytes = prometheus.NewDesc("svm_pebble_block_cache_bytes",
		"Bytes in the database's block cache.", []string{"db"}, nil)
	pebbleMemtableBytes = prometheus.NewDesc("svm_pebble_memtable_bytes",
		"Bytes of the memtables the database reads, the one it writes included.", []string{"db"}, nil)
	pebbleZombieBytes = prometheus.NewDesc("svm_pebble_memtable_zombie_bytes",
		"Bytes of flushed memtables that an open iterator still holds or that wait to be reused.", []string{"db"}, nil)
)

// PebbleCollector exports what Pebble holds outside the Go heap for each
// database that OpenApplicationDB or OpenSidecarDB opened, under the name it
// is given. Its block cache and memtables are calloc in a cgo build, so
// neither the Go heap nor GOMEMLIMIT sees them.
// A scrape makes one Metrics call for each database, which holds that
// database's mutex while it walks its file list.
func PebbleCollector(dbs map[string]dbm.DB) prometheus.Collector {
	c := pebbleCollector{}
	for name, db := range dbs {
		c[name] = db.(*dbm.PebbleDB).DB()
	}
	return c
}

type pebbleCollector map[string]*pebble.DB

func (pebbleCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- pebbleCacheBytes
	ch <- pebbleMemtableBytes
	ch <- pebbleZombieBytes
}

func (c pebbleCollector) Collect(ch chan<- prometheus.Metric) {
	for name, db := range c {
		m := db.Metrics()
		ch <- prometheus.MustNewConstMetric(pebbleCacheBytes, prometheus.GaugeValue, float64(m.BlockCache.Size), name)
		ch <- prometheus.MustNewConstMetric(pebbleMemtableBytes, prometheus.GaugeValue, float64(m.MemTable.Size), name)
		ch <- prometheus.MustNewConstMetric(pebbleZombieBytes, prometheus.GaugeValue, float64(m.MemTable.ZombieSize), name)
	}
}

// openFiles is at most max and at most 1/share of the descriptor limit, so a
// machine with a low limit reopens files rather than failing a read.
func openFiles(max, share uint64) int {
	var lim syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim) == nil {
		max = min(max, lim.Cur/share)
	}
	return int(max)
}
