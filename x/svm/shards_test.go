package svm

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"slices"
	"testing"

	"cosmossdk.io/log/v2"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/cosmos-sdk/store/v2/rootmulti"
	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
)

// TestTheShardsCommitInParallelToTheSameRoot proves that the same accounts,
// written through the SDK's serial loops and through FlushShards and
// SaveShards, give the same root at every version. The parallel path leaves
// every shard saved at the version that Commit then writes, so commitStores
// reuses the save and does not save again.
func TestTheShardsCommitInParallelToTheSameRoot(t *testing.T) {
	const blocks, perBlock = 4, 3000
	// The same keys every block, so a block updates, deletes and recreates
	// accounts, and no key in shard 7 in the last block, so one tree commits
	// a version with nothing written.
	block := func(s Stores, cache storetypes.MultiStore, b int) {
		for i := 0; i < perBlock; i++ {
			k := sha256.Sum256(binary.LittleEndian.AppendUint64(nil, uint64(i)))
			if b == blocks-1 && Shard(k) == 7 {
				continue
			}
			v := make([]byte, 40)
			binary.LittleEndian.PutUint64(v, uint64((i+b)%5))
			s.SetAccount(cache, k, v)
		}
	}

	s, serial := mountShards(t)
	var want []storetypes.CommitID
	for b := 0; b < blocks; b++ {
		cache := serial.CacheMultiStore()
		block(s, cache, b)
		cache.Write()
		serial.WorkingHash()
		want = append(want, serial.Commit())
	}

	s, parallel := mountShards(t)
	for b := 0; b < blocks; b++ {
		cache := parallel.CacheMultiStore()
		block(s, cache, b)
		s.FlushShards(parallel, cache)
		cache.Write()
		parallel.WorkingHash()
		s.SaveShards(parallel)
		for _, key := range s.Accounts {
			if v := parallel.GetStore(key).(storetypes.Committer).LastCommitID().Version; v != want[b].Version {
				t.Fatalf("block %d: shard %s is at version %d after SaveShards, want %d", b, key.Name(), v, want[b].Version)
			}
		}
		got := parallel.Commit()
		if got.Version != want[b].Version || !bytes.Equal(got.Hash, want[b].Hash) {
			t.Fatalf("block %d: parallel commit is %d %x, serial is %d %x", b, got.Version, got.Hash, want[b].Version, want[b].Hash)
		}
		if b > 0 && bytes.Equal(want[b].Hash, want[b-1].Hash) {
			t.Fatalf("block %d wrote nothing the root can see", b)
		}
	}
}

func mountShards(t *testing.T) (Stores, storetypes.CommitMultiStore) {
	t.Helper()
	s := NewStores()
	rs := rootmulti.NewStore(dbm.NewMemDB(), log.NewNopLogger())
	for _, k := range s.All() {
		rs.MountStoreWithDB(k, storetypes.StoreTypeIAVL, nil)
	}
	if err := rs.LoadLatestVersion(); err != nil {
		t.Fatal(err)
	}
	return s, rs
}

// An account costs a load budget Anza's 64 bytes plus its data, and not its
// stored head.
func TestAnAccountCostsItsDataAndSixtyFour(t *testing.T) {
	var owner [32]byte
	if got := loadCost(ffi.EncodeAccount(&owner, 1, 0, false, make([]byte, 100))); got != 164 {
		t.Fatalf("a 100-byte account costs %d, want 164", got)
	}
}

// The slots do not change PushAccounts' order and cut. A key named twice is
// pushed once. A Program brings its ProgramData. The first account that does
// not fit ends loading, and every key after it is dropped once. A dropped
// ProgramData brings its Program. A token account's mint follows the keys
// while it fits. Each account but the
// sysvar has a shard of its own, so the prefetch reads them concurrently.
func TestPushAccountsKeepsTheOrderAndTheCut(t *testing.T) {
	s, rs := mountShards(t)
	ms := rs.CacheMultiStore()
	key := func(i byte) (k [32]byte) {
		k[0], k[31] = i<<5, i+1
		return k
	}
	a, p, pd, tok, mint, missing, big, after := key(0), key(1), key(2), key(3), key(4), key(5), key(6), key(7)
	put := func(k, owner [32]byte, data []byte) {
		s.SetAccount(ms, k, ffi.EncodeAccount(&owner, 1, 0, false, data))
	}
	tokData, mintData := make([]byte, 165), make([]byte, 82)
	copy(tokData, mint[:])
	tokData[108], mintData[45] = 1, 1
	put(a, SystemProgram, nil)                                                   // costs 64
	put(p, LoaderV3, append(binary.LittleEndian.AppendUint32(nil, 2), pd[:]...)) // 100
	put(pd, LoaderV3, make([]byte, 100))                                         // 164
	put(tok, SplToken, tokData)                                                  // 229
	put(mint, SplToken, mintData)                                                // 146
	put(big, SystemProgram, make([]byte, 1000))                                  // 1,064
	put(after, SystemProgram, nil)                                               // 64
	put(ClockID, SystemProgram, make([]byte, 40))                                // nothing
	keys := [][32]byte{a, p, a, tok, missing, ClockID, pd, big, a, after, after}

	for _, c := range []struct {
		budget          int64
		pushed, dropped [][32]byte
		used            int64
	}{
		{math.MaxInt64, [][32]byte{a, p, pd, tok, ClockID, big, after, mint}, nil, 1831},
		{1000, [][32]byte{a, p, pd, tok, ClockID, mint}, [][32]byte{big, after}, 703},
		{200, [][32]byte{a, p}, [][32]byte{pd, p, tok, missing, ClockID, big, after}, 164},
	} {
		w := ffi.NewArenaWriter(ffi.KindExecuteReq, 0)
		pre := map[[32]byte]Loaded{}
		dropped, used := s.PushAccounts(w, ms, keys, c.budget, pre)
		r, err := ffi.NewArenaReader(w.Finish(), ffi.KindExecuteReq)
		if err != nil {
			t.Fatal(err)
		}
		var pushed [][32]byte
		for i := range r.Len() {
			k, _ := r.Key32(i)
			pushed = append(pushed, k)
		}
		w.Unpin()
		if !slices.Equal(pushed, c.pushed) || !slices.Equal(dropped, c.dropped) || used != c.used {
			t.Fatalf("budget %d: pushed %x dropped %x used %d, want %x, %x, %d", c.budget, pushed, dropped, used, c.pushed, c.dropped, c.used)
		}
		if tl, ok := pre[tok]; len(pre) != len(c.pushed) || pre[a].Lamports != 1 || ok && tl.Token.Mint != mint {
			t.Fatalf("budget %d: pre holds %d accounts, a's lamports %d, tok's mint %x", c.budget, len(pre), pre[a].Lamports, tl.Token.Mint)
		}
	}
}

// TestPushTablesKeepsTheOrderAndTheCut checks what phase 2 needs when it reads
// the lookup tables of a block eight shards at a time. The values and their
// order must be the same as from one GetAccount after another. This applies to
// the committed tree and to the unwritten cache of the block. A key named twice
// is pushed twice. An account that does not exist, a key that the block deleted
// and an account too big to be a table are left out and cost nothing. The first
// account past the budget ends the request.
func TestPushTablesKeepsTheOrderAndTheCut(t *testing.T) {
	s, rs := mountShards(t)
	key := func(i int) [32]byte { return sha256.Sum256(binary.LittleEndian.AppendUint32(nil, uint32(i))) }
	value := func(i int) []byte { return ffi.EncodeAccount(&SystemProgram, uint64(i+1), 0, false, nil) }
	sized := func(n int) []byte { return ffi.EncodeAccount(&SystemProgram, 1, 0, false, make([]byte, n)) }
	big, full := key(400), key(401)
	cms := rs.CacheMultiStore()
	for i := range 300 {
		s.SetAccount(cms, key(i), value(i))
	}
	s.SetAccount(cms, big, sized(maxTableData+1))
	s.SetAccount(cms, full, sized(maxTableData))
	cms.Write()
	rs.Commit()

	block := rs.CacheMultiStore()
	s.SetAccount(block, key(300), value(300))
	s.SetAccount(block, key(7), value(1000))
	s.SetAccount(block, key(8), make([]byte, 8))
	push := func(budget int64, keys [][32]byte) [][32]byte {
		w := ffi.NewArenaWriter(ffi.KindResolveReq, 0)
		s.PushTables(w, block, keys, budget)
		r, err := ffi.NewArenaReader(w.Finish(), ffi.KindResolveReq)
		if err != nil {
			t.Fatal(err)
		}
		got := make([][32]byte, r.Len())
		for i := range got {
			k, v, err := r.StoredAt(i)
			if err != nil {
				t.Fatal(err)
			}
			if want := s.GetAccount(block, k); !bytes.Equal(v, want) {
				t.Fatalf("record %d: pushed %x, GetAccount reads %x", i, v, want)
			}
			got[i] = k
		}
		w.Unpin()
		return got
	}

	keys := [][32]byte{key(300), key(5), key(999), key(7), key(8), key(5), big, full}
	for i := 0; i < 300; i += 3 {
		keys = append(keys, key(i))
	}
	var want [][32]byte
	for _, k := range keys {
		if k != key(999) && k != key(8) && k != big {
			want = append(want, k)
		}
	}
	if got := push(math.MaxInt64, keys); !slices.Equal(got, want) {
		t.Fatalf("pushed %d keys, want %d, or out of order", len(got), len(want))
	}
	// big costs nothing, so the budget buys key(0) and key(1) and stops at key(2).
	got := push(2*accountBaseSize, [][32]byte{big, key(0), key(1), key(2), key(3)})
	if !slices.Equal(got, [][32]byte{key(0), key(1)}) {
		t.Fatalf("the cut pushed %d keys", len(got))
	}
}
