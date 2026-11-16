package svm

import (
	"testing"
	"time"

	dbm "github.com/cosmos/cosmos-db"

	"github.com/rit3sh-x/cosmos-svm/ffi"
)

// heldCompaction is a history database whose range compaction waits until the
// test releases it.
type heldCompaction struct {
	dbm.DB
	started chan [2][]byte
	release chan struct{}
}

func (d heldCompaction) ForceCompact(start, limit []byte) error {
	d.started <- [2][]byte{start, limit}
	<-d.release
	return nil
}

// TestHistoryCompactsOffTheBlockPath proves that the compaction of a purge runs
// off the block path. Write returns before the compaction ends, so no block
// pays for a compaction inside FinalizeBlock.
func TestHistoryCompactsOffTheBlockPath(t *testing.T) {
	db := heldCompaction{DB: dbm.NewMemDB(), started: make(chan [2][]byte, 8), release: make(chan struct{})}
	// Three blocks' entries, so the fourth block purges the first.
	h, err := OpenHistory(db, 0, 3*blockEntryBytes)
	if err != nil {
		t.Fatal(err)
	}
	write := func(height uint64) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- h.Write(height, [32]byte{byte(height)}, 0, nil) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("Write at height %d waited on the compaction", height)
		}
	}
	started := func() [2][]byte {
		t.Helper()
		select {
		case r := <-db.started:
			return r
		case <-time.After(10 * time.Second):
			t.Fatal("no compaction started")
			return [2][]byte{}
		}
	}
	for height := uint64(1); height <= 4; height++ {
		write(height)
	}
	if first := started(); string(first[0]) != string(historyKey(1, 0)) || string(first[1]) != string(historyKey(2, 0)) {
		t.Fatalf("the first compaction covers %x to %x, want block 1", first[0], first[1])
	}
	// Blocks 5 and 6 purge 2 and 3 while the first compaction is held.
	write(5)
	write(6)
	select {
	case r := <-db.started:
		t.Fatalf("a second compaction started beside the first: %x to %x", r[0], r[1])
	default:
	}
	close(db.release)
	if second := started(); string(second[0]) != string(historyKey(2, 0)) || string(second[1]) != string(historyKey(4, 0)) {
		t.Fatalf("the next compaction covers %x to %x, want blocks 2 and 3 in one range", second[0], second[1])
	}
	h.compactions.Wait()
	if first := h.FirstAvailableBlock(); first != 4 {
		t.Fatalf("first available block %d, want 4", first)
	}
}

// TestSamplesArePurgedWithTheirBlocks proves that a performance sample is
// stored with the next block, counts against index-max-bytes, and goes with
// the blocks that a purge removes.
func TestSamplesArePurgedWithTheirBlocks(t *testing.T) {
	h, err := OpenHistory(dbm.NewMemDB(), 0, 2_000)
	if err != nil {
		t.Fatal(err)
	}
	for height := uint64(1); height <= 60; height++ {
		h.Sample(PerfSample{Slot: height, NumSlots: 1, NumTransactions: height, SamplePeriodSecs: 60})
		if err := h.Write(height, [32]byte{byte(height)}, int64(height), nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := h.Samples(1_000)
	if err != nil {
		t.Fatal(err)
	}
	first := h.FirstAvailableBlock()
	if first <= 1 {
		t.Fatal("the budget purged nothing")
	}
	if len(got) == 0 || got[0].Slot != 60 || got[len(got)-1].Slot != first {
		t.Fatalf("samples from %d to %d, want from 60 down to the first available block %d",
			got[0].Slot, got[len(got)-1].Slot, first)
	}
}

// The horizon is persisted where OpenHistory reads it back.
func TestHistoryHorizonSurvivesAReopen(t *testing.T) {
	db := dbm.NewMemDB()
	h, err := OpenHistory(db, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Restored(7); err != nil {
		t.Fatal(err)
	}
	if h, err = OpenHistory(db, 7, 0); err != nil {
		t.Fatal(err)
	}
	if first := h.FirstAvailableBlock(); first != 8 {
		t.Fatalf("first available block %d after a reopen, want 8", first)
	}
}

// A block entry is the header hash, then the block time. Blockhash and
// BlockTime parse it the same way, and a value of any other length is no entry.
func TestBlockEntryIsHashAndTime(t *testing.T) {
	db := dbm.NewMemDB()
	h, err := OpenHistory(db, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Write(1, [32]byte{9}, 5, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Set(blockKey(2), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if hash, ok, err := h.Blockhash(1); err != nil || !ok || hash != [32]byte{9} {
		t.Fatalf("Blockhash(1) = %x, %v, %v", hash, ok, err)
	}
	if bt, ok, err := h.BlockTime(1); err != nil || !ok || bt != 5 {
		t.Fatalf("BlockTime(1) = %d, %v, %v", bt, ok, err)
	}
	if _, ok, _ := h.Blockhash(2); ok {
		t.Fatal("Blockhash read a 32-byte entry")
	}
	if _, ok, _ := h.BlockTime(2); ok {
		t.Fatal("BlockTime read a 32-byte entry")
	}
}

// The counter is the bytes of every entry the history stores, as entries are
// written and as they are deleted, and a reopen reads it back.
func TestHistoryCounterIsItsEntries(t *testing.T) {
	db := dbm.NewMemDB()
	h, err := OpenHistory(db, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	stored := func() uint64 {
		t.Helper()
		it, err := db.Iterator(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer it.Close()
		n := uint64(0)
		for ; it.Valid(); it.Next() {
			if k := it.Key(); len(k) != 1 || (k[0] != 'b' && k[0] != 'z') {
				n += uint64(len(k) + len(it.Value()))
			}
		}
		return n
	}
	var sigs [][64]byte
	for height := uint64(1); height <= 2; height++ {
		sig := [64]byte{byte(height)}
		sigs = append(sigs, sig)
		h.Sample(PerfSample{Slot: height, NumSlots: 1})
		r := Record{Height: height, Signature: sig, Meta: ffi.TxMeta{StaticKeys: ffi.Keys32{{1}, {byte(height)}}}}
		if err := h.Write(height, [32]byte{byte(height)}, 0, []Record{r}); err != nil {
			t.Fatal(err)
		}
		if got, want := h.Bytes(), stored(); got != want {
			t.Fatalf("counter %d at height %d, want the %d bytes stored", got, height, want)
		}
	}
	if _, ok, err := h.Get(sigs[1], 2); err != nil || !ok {
		t.Fatalf("Get of the record at height 2: %v, %v", ok, err)
	}
	// A reopen at height 1 drops block 2 with its record and its sample.
	if h, err = OpenHistory(db, 1, 0); err != nil {
		t.Fatal(err)
	}
	if got, want := h.Bytes(), stored(); got != want {
		t.Fatalf("counter %d after dropping height 2, want the %d bytes stored", got, want)
	}
	if _, ok, err := h.Get(sigs[1], 2); err != nil || ok {
		t.Fatalf("Get of a dropped record: %v, %v", ok, err)
	}
}

// purgeHorizon purges the excess over the budget in whole blocks of the mean
// size, rounded up. It purges at least one block, and never the latest.
func TestPurgeHorizon(t *testing.T) {
	for _, c := range []struct{ used, maxBytes, lowest, latest, want uint64 }{
		{100, 85, 1, 10, 2}, // a mean of 10, 15 over: 2 blocks, rounded up
		{100, 80, 1, 10, 2}, // 20 over: exactly 2
		{100, 95, 6, 10, 6}, // a mean of 20 over 6 to 10, 5 over: 1
		{100, 1, 1, 10, 9},  // 99 over is all 10, but never the latest
		{5, 1, 1, 10, 1},    // a mean of 0 still purges one
	} {
		if got := purgeHorizon(c.used, c.maxBytes, c.lowest, c.latest); got != c.want {
			t.Fatalf("purgeHorizon(%d, %d, %d, %d) = %d, want %d", c.used, c.maxBytes, c.lowest, c.latest, got, c.want)
		}
	}
}

func TestSplitAddressKey(t *testing.T) {
	k := addressKey([32]byte{1}, 9, 3)
	if height, index, ok := splitAddressKey(k); !ok || height != 9 || index != 3 {
		t.Fatalf("splitAddressKey(addressKey(9, 3)) = %d, %d, %v", height, index, ok)
	}
	if _, _, ok := splitAddressKey(k[:addressKeyBytes-1]); ok {
		t.Fatal("a short address key parsed")
	}
}
