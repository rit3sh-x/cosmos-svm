package app

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"cosmossdk.io/log/v2"
	"github.com/cockroachdb/pebble"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/cosmos-sdk/store/v2/rootmulti"
	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	"golang.org/x/sys/unix"

	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestPebbleLeavesTheRootAlone checks the claim of OpenApplicationDB. The
// test commits the same accounts the way the app commits them: FlushShards,
// Write, WorkingHash, SaveShards, Commit. It does this once on the Pebble
// database that svmd opens and once on a memory database. Both give the same
// root at every version. The Pebble database, opened again, loads what it
// committed. The engine and its options decide how bytes reach the disk.
// Nothing in them may reach what is hashed.
func TestPebbleLeavesTheRootAlone(t *testing.T) {
	const blocks, perBlock = 4, 3000
	// The same keys every block, so a block updates, deletes and recreates
	// accounts, and no key in shard 7 in the last block, so one tree commits
	// a version with nothing written.
	block := func(s svm.Stores, cache storetypes.MultiStore, b int) {
		for i := 0; i < perBlock; i++ {
			k := sha256.Sum256(binary.LittleEndian.AppendUint64(nil, uint64(i)))
			if b == blocks-1 && svm.Shard(k) == 7 {
				continue
			}
			v := make([]byte, 40)
			binary.LittleEndian.PutUint64(v, uint64((i+b)%5))
			s.SetAccount(cache, k, v)
		}
	}
	commit := func(s svm.Stores, cms storetypes.CommitMultiStore, b int) storetypes.CommitID {
		cache := cms.CacheMultiStore()
		block(s, cache, b)
		s.FlushShards(cms, cache)
		cache.Write()
		cms.WorkingHash()
		s.SaveShards(cms)
		return cms.Commit()
	}
	mount := func(db dbm.DB) (svm.Stores, storetypes.CommitMultiStore) {
		s := svm.NewStores()
		rs := rootmulti.NewStore(db, log.NewNopLogger())
		for _, k := range s.All() {
			rs.MountStoreWithDB(k, storetypes.StoreTypeIAVL, nil)
		}
		if err := rs.LoadLatestVersion(); err != nil {
			t.Fatal(err)
		}
		return s, rs
	}

	s, memory := mount(dbm.NewMemDB())
	var want []storetypes.CommitID
	for b := 0; b < blocks; b++ {
		want = append(want, commit(s, memory, b))
	}

	dir := t.TempDir()
	db, err := OpenApplicationDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, disk := mount(db)
	for b := 0; b < blocks; b++ {
		got := commit(s, disk, b)
		if got.Version != want[b].Version || !bytes.Equal(got.Hash, want[b].Hash) {
			t.Fatalf("block %d: on Pebble %d %x, in memory %d %x", b, got.Version, got.Hash, want[b].Version, want[b].Hash)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = OpenApplicationDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, again := mount(db)
	last := again.LastCommitID()
	if last.Version != want[blocks-1].Version || !bytes.Equal(last.Hash, want[blocks-1].Hash) {
		t.Fatalf("reopened at %d %x, committed %d %x", last.Version, last.Hash, want[blocks-1].Version, want[blocks-1].Hash)
	}
}

// TestDatabasesAnswerAfterClose keeps the cosmos-db replace in go.mod. IAVL's
// async pruning, which the SDK's store turns on, reads after its owner closes
// the database. Stock cosmos-db's Pebble panics on that read. The fork
// returns pebble.ErrClosed.
func TestDatabasesAnswerAfterClose(t *testing.T) {
	dir := t.TempDir()
	application, err := OpenApplicationDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	side, err := OpenSidecarDB("svmhistory", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range []dbm.DB{application, side} {
		if err := db.Set([]byte("k"), []byte{}); err != nil {
			t.Fatal(err)
		}
		if ok, err := db.Has([]byte("k")); err != nil || !ok {
			t.Fatalf("an empty value: Has %t %v, want true", ok, err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Get([]byte("k")); !errors.Is(err, pebble.ErrClosed) {
			t.Fatalf("Get after Close: %v, want pebble.ErrClosed", err)
		}
	}
}

// TestWrittenTablesLeaveThePageCache checks that the tables a flush and a
// compaction of the history write leave the page cache, and that every key
// still reads. The dedup record keeps its flush in the page cache, which shows
// that the flush put it there.
func TestWrittenTablesLeaveThePageCache(t *testing.T) {
	dir := t.TempDir()
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil || fs.Type == unix.TMPFS_MAGIC {
		t.Skip("the page cache of tmpfs is its storage")
	}
	// The values do not compress, so a flush writes about a MiB.
	values := make([][]byte, 1024)
	fill := func(db dbm.DB) {
		for i := range values {
			values[i] = make([]byte, 1024)
			rand.Read(values[i])
			if err := db.Set(binary.BigEndian.AppendUint32(nil, uint32(i)), values[i]); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.(*dbm.PebbleDB).DB().Flush(); err != nil {
			t.Fatal(err)
		}
	}

	dedup, err := OpenSidecarDB("svmdedup", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dedup.Close()
	fill(dedup)
	if resident(t, dedup, filepath.Join(dir, "svmdedup.db")) == 0 {
		t.Fatal("the dedup record's flush left nothing in the page cache")
	}

	// The values are only sets. Pebble then takes the stats of each table from
	// its writer, and no background read brings a page back.
	history, drops, err := openIndex("svmhistory", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	fill(history)
	drops.Wait()
	if n := resident(t, history, filepath.Join(dir, "svmhistory.db")); n != 0 {
		t.Fatalf("%d pages of the flushed table still cached", n)
	}
	// Two flushes of the same keys overlap, so the compaction writes a table.
	fill(history)
	if err := history.(*dbm.PebbleDB).DB().Compact([]byte{0}, []byte{1}, false); err != nil {
		t.Fatal(err)
	}
	drops.Wait()
	if n := resident(t, history, filepath.Join(dir, "svmhistory.db")); n != 0 {
		t.Fatalf("%d pages of the compacted table still cached", n)
	}
	for i, want := range values {
		got, err := history.Get(binary.BigEndian.AppendUint32(nil, uint32(i)))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("key %d after the drop: %x, %v", i, got, err)
		}
	}
}

// resident counts the pages of the live tables of db, at path, that are in
// the page cache. It calls mincore on a mapping of each table, which reads
// nothing in.
func resident(t *testing.T, db dbm.DB, path string) int {
	levels, err := db.(*dbm.PebbleDB).DB().SSTables()
	if err != nil {
		t.Fatal(err)
	}
	pages := 0
	for _, level := range levels {
		for _, table := range level {
			pages += residentFile(t, filepath.Join(path, table.FileNum.String()+".sst"))
		}
	}
	return pages
}

func residentFile(t *testing.T, path string) int {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	m, err := unix.Mmap(int(f.Fd()), 0, int(info.Size()), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(m)
	vec := make([]byte, (len(m)+os.Getpagesize()-1)/os.Getpagesize())
	if _, _, e := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&m[0])), uintptr(len(m)), uintptr(unsafe.Pointer(&vec[0]))); e != 0 {
		t.Fatal(e)
	}
	pages := 0
	for _, v := range vec {
		pages += int(v & 1)
	}
	return pages
}
