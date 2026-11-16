package svm

import (
	"crypto/sha256"
	"encoding/binary"
	"io"
	"runtime"
	"testing"

	dbm "github.com/cosmos/cosmos-db"
)

func key(h uint64, i int) [64]byte {
	a := sha256.Sum256(binary.LittleEndian.AppendUint64(nil, h))
	b := sha256.Sum256(binary.LittleEndian.AppendUint64(a[:], uint64(i)))
	var k [64]byte
	copy(k[:32], a[:])
	copy(k[32:], b[:])
	return k
}

// TestDedupDepthEqualsTheWindow proves that the depth of the set is exactly
// the window. A key spent at height 1 still matches at height 151, where a
// blockhash from height 1 has age 150. It is gone before height 152.
func TestDedupDepthEqualsTheWindow(t *testing.T) {
	db := dbm.NewMemDB()
	d, err := OpenDedup(db, 0)
	if err != nil {
		t.Fatal(err)
	}
	k := key(1, 0)
	for h := uint64(1); h <= 150; h++ {
		var spent [][64]byte
		if h == 1 {
			spent = [][64]byte{k}
		}
		if err := d.Commit(h, spent); err != nil {
			t.Fatal(err)
		}
	}
	if !d.Has(k) {
		t.Fatal("key from height 1 missing before height 151")
	}
	if reopened, _ := OpenDedup(db, 150); !reopened.Has(k) {
		t.Fatal("key from height 1 missing after reopening at committed height 150")
	}
	if err := d.Commit(151, nil); err != nil {
		t.Fatal(err)
	}
	if d.Has(k) {
		t.Fatal("key from height 1 still present before height 152")
	}
	if reopened, _ := OpenDedup(db, 151); reopened.Has(k) {
		t.Fatal("key from height 1 reloaded at committed height 151")
	}
}

// InBlockhashWindow is the window that OpenDedup keeps and Import accepts. At
// height 151, a record from height 2 is inside it and one from height 1 is not.
func TestInBlockhashWindow(t *testing.T) {
	for _, c := range []struct {
		at, height uint64
		in         bool
	}{{0, 0, true}, {0, 1, false}, {151, 1, false}, {151, 2, true}, {151, 151, true}, {151, 152, false}} {
		if got := InBlockhashWindow(c.at, c.height); got != c.in {
			t.Fatalf("InBlockhashWindow(%d, %d) = %v, want %v", c.at, c.height, got, c.in)
		}
	}
}

// The window near genesis. OpenDedup and Import keep rows from
// windowStart, which is 0 below height 150. Export reads from 1: Commit moves
// kept to 1 at height 300, and an export of a height below 150 must still
// find its rows then.
func TestDedupWindowStart(t *testing.T) {
	for _, c := range []struct{ height, start uint64 }{{0, 0}, {149, 0}, {150, 1}, {151, 2}} {
		if got := windowStart(c.height); got != c.start {
			t.Fatalf("windowStart(%d) = %d, want %d", c.height, got, c.start)
		}
	}
	for at := uint64(0); at <= 2*BlockhashWindow; at++ {
		for h := uint64(0); h <= at+1; h++ {
			if InBlockhashWindow(at, h) != (windowStart(at) <= h && h <= at) {
				t.Fatalf("InBlockhashWindow(%d, %d) disagrees with windowStart", at, h)
			}
		}
	}
	d, err := OpenDedup(dbm.NewMemDB(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for h := uint64(1); h <= dedupRetained; h++ {
		var spent [][64]byte
		if h == 2 {
			spent = [][64]byte{key(2, 0)}
		}
		if err := d.Commit(h, spent); err != nil {
			t.Fatal(err)
		}
	}
	if d.kept != 1 {
		t.Fatalf("kept %d at height %d, want 1", d.kept, dedupRetained)
	}
	var payloads [][]byte
	if err := d.Export(149, func(p []byte) error {
		payloads = append(payloads, append([]byte(nil), p...))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 1 || binary.BigEndian.Uint64(payloads[0][:8]) != 2 {
		t.Fatalf("the window at 149 exported %d payloads, want one for height 2", len(payloads))
	}
}

func TestSplitDedupKey(t *testing.T) {
	k := key(7, 1)
	if h, got, ok := splitDedupKey(dedupKey(7, k)); !ok || h != 7 || got != k {
		t.Fatalf("splitDedupKey(dedupKey(7, k)) = %d, %x, %v", h, got, ok)
	}
	if _, _, ok := splitDedupKey(dedupKey(7, k)[:72]); ok {
		t.Fatal("a 72-byte key parsed")
	}
}

// TestDedupExportIsAFunctionOfHeight proves that the payload for a height is
// the same whenever it is written. The SDK writes it after later blocks commit
// and expire the oldest buckets of the window from the set. The rows stay in
// the database one more window. An export after that fails and does not write
// less.
func TestDedupExportIsAFunctionOfHeight(t *testing.T) {
	d, err := OpenDedup(dbm.NewMemDB(), 0)
	if err != nil {
		t.Fatal(err)
	}
	export := func(height uint64) ([][]byte, error) {
		var payloads [][]byte
		err := d.Export(height, func(p []byte) error {
			payloads = append(payloads, append([]byte(nil), p...))
			return nil
		})
		return payloads, err
	}
	for h := uint64(1); h <= 151; h++ {
		var spent [][64]byte
		if h == 2 || h == 151 {
			spent = [][64]byte{key(h, 1), key(h, 0)}
		}
		if err := d.Commit(h, spent); err != nil {
			t.Fatal(err)
		}
	}
	first, err := export(151)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || binary.BigEndian.Uint64(first[0][:8]) != 2 {
		t.Fatalf("the window at 151 exported %d payloads, want heights 2 and 151", len(first))
	}
	for h := uint64(152); h <= 301; h++ {
		if err := d.Commit(h, nil); err != nil {
			t.Fatal(err)
		}
		again, err := export(151)
		if err != nil {
			t.Fatalf("after height %d: %v", h, err)
		}
		if len(again) != len(first) || string(again[0]) != string(first[0]) || string(again[1]) != string(first[1]) {
			t.Fatalf("after height %d the export at 151 changed", h)
		}
	}
	if err := d.Commit(302, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := export(151); err == nil {
		t.Fatal("an export whose window's rows are gone succeeded")
	}
}

// TestDedupImportRefusesARepeatedHeight proves that a snapshot peer cannot add
// keys to the set by sending one height twice. The keys of the first payload
// would stay, but the bucket, and so the commitment that AssertDedup compares,
// would hash only the second.
func TestDedupImportRefusesARepeatedHeight(t *testing.T) {
	d, err := OpenDedup(dbm.NewMemDB(), 0)
	if err != nil {
		t.Fatal(err)
	}
	planted := key(5, 0)
	payloads := [][]byte{
		append(binary.BigEndian.AppendUint64(nil, 5), planted[:]...),
		binary.BigEndian.AppendUint64(nil, 5),
	}
	read := func() ([]byte, error) {
		if len(payloads) == 0 {
			return nil, io.EOF
		}
		p := payloads[0]
		payloads = payloads[1:]
		return p, nil
	}
	if err := d.Import(5, read); err == nil {
		t.Fatalf("an import that gives height 5 twice succeeded, and holds the planted key: %v", d.Has(planted))
	}
}

// TestDedupSpendIsCommitBeforeItsWrite proves three things. Spend leaves the
// set, and so the commitment in the app hash, where Commit leaves it, before
// its write lands. The write is the one Commit makes. A machine that stops
// after Spend and before the height commits reopens to the set it committed.
func TestDedupSpendIsCommitBeforeItsWrite(t *testing.T) {
	whole, split := dbm.NewMemDB(), dbm.NewMemDB()
	a, _ := OpenDedup(whole, 0)
	b, _ := OpenDedup(split, 0)
	rows := func(db dbm.DB) string {
		it, err := db.Iterator(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer it.Close()
		var keys []byte
		for ; it.Valid(); it.Next() {
			keys = append(keys, it.Key()...)
		}
		return string(keys)
	}
	const top = 2 * dedupRetained
	var committed [32]byte
	for h := uint64(1); h <= top; h++ {
		spent := [][64]byte{key(h, 0), key(h, 1)}
		if err := a.Commit(h, spent); err != nil {
			t.Fatal(err)
		}
		save, err := b.Spend(h, spent)
		if err != nil {
			t.Fatal(err)
		}
		if a.Commitment(h) != b.Commitment(h) || a.Entries() != b.Entries() {
			t.Fatalf("height %d: the set Spend leaves is not the one Commit leaves", h)
		}
		if h == top {
			break
		}
		if err := save(); err != nil {
			t.Fatal(err)
		}
		if rows(whole) != rows(split) {
			t.Fatalf("height %d: Spend's write is not Commit's", h)
		}
		committed = a.Commitment(h)
	}
	reopened, err := OpenDedup(split, top-1)
	if err != nil {
		t.Fatal(err)
	}
	if err := AssertDedup(top-1, reopened, committed[:]); err != nil {
		t.Fatal(err)
	}
}

// TestDedupDropsUncommittedBucket proves that OpenDedup drops a bucket from a
// block that finalized but did not commit.
func TestDedupDropsUncommittedBucket(t *testing.T) {
	db := dbm.NewMemDB()
	d, _ := OpenDedup(db, 0)
	if err := d.Commit(1, [][64]byte{key(1, 0)}); err != nil {
		t.Fatal(err)
	}
	if reopened, _ := OpenDedup(db, 0); reopened.Has(key(1, 0)) {
		t.Fatal("bucket above the committed height was loaded")
	}
}

func TestDedupResidentMemoryAtAFullWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a full window")
	}
	const perBlock = 12_700
	db, err := dbm.NewGoLevelDB("dedup", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	d, _ := OpenDedup(db, 0)
	spent := make([][64]byte, perBlock)
	for h := uint64(1); h <= dedupWindow; h++ {
		for i := range spent {
			spent[i] = key(h, i)
		}
		if err := d.Commit(h, spent); err != nil {
			t.Fatal(err)
		}
	}
	spent = nil
	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)
	n := len(d.seen)
	heap := after.HeapAlloc - before.HeapAlloc
	t.Logf("%d keys, %.1f MB heap, %.1f bytes per key", n, float64(heap)/1e6, float64(heap)/float64(n))
	if n != dedupWindow*perBlock {
		t.Fatalf("%d keys in the set, want %d", n, dedupWindow*perBlock)
	}
	runtime.KeepAlive(d)
}
