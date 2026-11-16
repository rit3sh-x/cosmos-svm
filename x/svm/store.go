// Package svm is the Cosmos module that owns SVM accounts, the per-block
// sysvars, and the TxRunner that hands blocks to the engine.
//
// Go is the only component that commits. The engine proposes write sets and
// never touches these stores.
package svm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
)

const ModuleName = "svm"

// MetaStoreName is the store of singletons: the engine digest, the network, the
// dedup commitment, the transaction count, the active and pending gates, and
// the last block hash.
const MetaStoreName = "svmmeta"

// AccountStoreNames are the eight account shards. The key in each is the bare
// 32-byte pubkey, and the value has the ffi.EncodeAccount layout.
var AccountStoreNames = [8]string{"svm0", "svm1", "svm2", "svm3", "svm4", "svm5", "svm6", "svm7"}

// Shard is the account store an account lives in, chosen by the top three
// bits of its pubkey.
func Shard(pubkey [32]byte) int { return int(pubkey[0] >> 5) }

// MetaBlockhashKey is this block's CometBFT header hash, written at the start
// of every block. The next block reads it as its parent. FinalizeBlock carries
// no parent hash, and SDK v0.54 leaves LastBlockID out of the header it builds,
// so SlotHashes and RecentBlockhashes need it here.
var MetaBlockhashKey = []byte("blockhash")

// MetaDigestKey is the engine digest that the chain has adopted. Genesis writes
// it, and an upgrade handler that changes the engine must rewrite it. It is in
// the app hash, so a validator that runs a divergent build forks at that
// height, not silently later.
var MetaDigestKey = []byte("digest")

// MetaNetworkKey is the network that the chain declared at genesis,
// "production" or "devnet". The engine digest cannot see a Go build tag,
// because digest.rs hashes file bytes and a tag changes no file. The app hash
// can see it. Genesis writes the compiled constant here, and Assert reads it
// back before the machine signs anything.
var MetaNetworkKey = []byte("network")

// MetaTransactionCountKey is the count of processed SVM transactions since
// genesis, Agave's bank transaction count.
var MetaTransactionCountKey = []byte("txcount")

// MetaDedupCommitmentKey is the 32-byte commitment to the dedup set over the
// acceptance window. It is written every block, so the app hash covers the set
// that a machine holds, and the set itself stays out of the tree.
var MetaDedupCommitmentKey = []byte("dedup")

// Stores names the module's store keys.
type Stores struct {
	Accounts [8]*storetypes.KVStoreKey
	Meta     *storetypes.KVStoreKey
	// minted is the lamports created outside the engine in the block being
	// finalized. The runner takes it for the token index's total. It is
	// machine-local, and nothing reads it into state.
	minted *atomic.Uint64
}

func NewStores() Stores {
	var s Stores
	for i, n := range AccountStoreNames {
		s.Accounts[i] = storetypes.NewKVStoreKey(n)
	}
	s.Meta = storetypes.NewKVStoreKey(MetaStoreName)
	s.minted = new(atomic.Uint64)
	return s
}

// mint records lamports that a writer outside the engine created: a feature
// account that a gate flip made, or a program that an upgrade seeded.
func (s Stores) mint(lamports uint64) {
	if s.minted != nil {
		s.minted.Add(lamports)
	}
}

// TakeMinted returns what was minted outside the engine since the last call,
// and resets it.
func (s Stores) TakeMinted() uint64 {
	if s.minted == nil {
		return 0
	}
	return s.minted.Swap(0)
}

// All returns every store key, for mounting.
func (s Stores) All() []storetypes.StoreKey {
	keys := make([]storetypes.StoreKey, 0, 9)
	for _, k := range s.Accounts {
		keys = append(keys, k)
	}
	return append(keys, s.Meta)
}

// FlushShards writes the finalize state's shard caches into their trees and
// hashes the trees, all eight at once. SaveShards then saves the trees, all
// eight at once. The SDK does each of these one store after another.
// cachemulti's Write, rootmulti's WorkingHash and commitStores all loop over
// every store. The shards carry nearly every write of a block.
//
// These two functions do the shards' share of each loop first, with no patch
// to the SDK. The app calls FlushShards at the end of FinalizeBlock, before
// the SDK's workingHash. It calls SaveShards at the start of Commit, before
// the SDK's commitStores. Both SDK loops then find the work done. A written
// cache has nothing left to write. IAVL returns the hash it has computed.
// commitStores reuses a store whose last commit is already at the version
// being committed, in its interrupted-commit branch.
//
// The app hash is the same either way. A shard's root depends on its own
// writes alone, and commitStores sorts the store infos by name before hashing
// them. TestTheShardsCommitInParallelToTheSameRoot writes one account set
// through both paths and compares the roots. One goroutine touches one tree,
// and the trees share only the database, which takes concurrent writers.
//
// ms is the finalize state's multistore. A multistore that does not hand out
// cache stores, or a shard that is not a committing store, is left to the
// SDK's loops, which do the same work serially.
func (s Stores) FlushShards(cms storetypes.CommitMultiStore, ms storetypes.MultiStore) {
	var caches [8]storetypes.CacheKVStore
	for i, key := range s.Accounts {
		cache, ok := ms.GetKVStore(key).(storetypes.CacheKVStore)
		if !ok {
			return
		}
		caches[i] = cache
	}
	trees := s.trees(cms)
	s.eachShard(func(i int) {
		caches[i].Write()
		if trees[i] != nil {
			trees[i].WorkingHash()
		}
	})
}

// SaveShards saves every shard's tree as the version Commit is about to
// write. See FlushShards.
func (s Stores) SaveShards(cms storetypes.CommitMultiStore) {
	trees := s.trees(cms)
	s.eachShard(func(i int) {
		if trees[i] != nil {
			trees[i].Commit()
		}
	})
}

// trees looks every shard's committing store up before the goroutines start,
// because a multistore's lookups may fill a map, and the stores stay valid for
// the life of the app. A shard that is not a committing store is nil.
func (s Stores) trees(cms storetypes.CommitMultiStore) [8]storetypes.Committer {
	var trees [8]storetypes.Committer
	for i, key := range s.Accounts {
		trees[i], _ = cms.GetStore(key).(storetypes.Committer)
	}
	return trees
}

// eachShard runs f for every shard at once and waits. A panic in a shard,
// which is how the SDK's stores report a failed write or save, is raised again
// on the caller's goroutine, where baseapp handles it as it handles the serial
// loops' own.
func (s Stores) eachShard(f func(i int)) {
	var wg sync.WaitGroup
	var once sync.Once
	var failure any
	wg.Add(len(s.Accounts))
	for i := range s.Accounts {
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					once.Do(func() { failure = r })
				}
			}()
			f(i)
		}()
	}
	wg.Wait()
	if failure != nil {
		panic(failure)
	}
}

func (s Stores) account(ms storetypes.MultiStore, pubkey [32]byte) storetypes.KVStore {
	return ms.GetKVStore(s.Accounts[Shard(pubkey)])
}

// GetAccount returns an account's stored value, or nil.
func (s Stores) GetAccount(ms storetypes.MultiStore, pubkey [32]byte) []byte {
	return s.account(ms, pubkey).Get(pubkey[:])
}

// maxTableData is the most data a lookup table can hold: 56 bytes of meta and
// 256 addresses.
const maxTableData = 56 + 256*32

// PushTables appends the accounts that keys name to a phase 2 request, in
// order. It reads eight shards at a time, as PushAccounts does. It leaves out
// an account that does not exist and an account with more data than a table
// can hold. The engine drops a transaction that names one of these. Each
// account costs what it costs PushAccounts. The first account that does not
// fit budget ends the request.
func (s Stores) PushTables(w *ffi.ArenaWriter, ms storetypes.MultiStore, keys [][32]byte, budget int64) {
	value := make([][]byte, len(keys))
	flags := make([]uint8, len(keys))
	s.prefetch(ms, keys, budget, maxTableData, value, flags)
	var used int64
	for i := range keys {
		v := value[i]
		if flags[i]&slotRead == 0 {
			v = s.GetAccount(ms, keys[i])
		}
		if v == nil || ffi.DataLen(v) > maxTableData {
			continue
		}
		if used += loadCost(v); used > budget {
			return
		}
		w.PushStored(&keys[i], v)
	}
}

// SetAccount writes a stored value. A zero-lamport account is deleted, because
// Agave's accounts-db loads one as absent.
func (s Stores) SetAccount(ms storetypes.MultiStore, pubkey [32]byte, value []byte) {
	if len(value) < 8 {
		panic(fmt.Sprintf("svm: account value for %x is %d bytes", pubkey, len(value)))
	}
	if ffi.Lamports(value) == 0 {
		s.account(ms, pubkey).Delete(pubkey[:])
		return
	}
	s.account(ms, pubkey).Set(pubkey[:], append([]byte(nil), value...))
}

// accountData reads an account that must exist.
func (s Stores) accountData(ms storetypes.MultiStore, pubkey [32]byte) (ffi.Account, error) {
	v := s.GetAccount(ms, pubkey)
	if v == nil {
		return ffi.Account{}, fmt.Errorf("svm: account %x is missing", pubkey)
	}
	return ffi.DecodeAccount(v)
}

// AccountScan reads every account of a committed view, shard by shard in key
// order. OpenAccountScan opens the iterator of every shard before any is read.
// A caller opens the scan while the view's version is still the latest
// committed one.
//
// This keeps a long scan whole. At the latest version, IAVL serves an iterator
// from its fast-node index. The fast iterator opens a database iterator when
// it is constructed, and goleveldb pins that iterator to a snapshot. The scan
// then reads the version as it was, however many blocks commit and however
// much of it is pruned while it runs.
//
// A version stops being the latest when the next block commits. An iterator
// opened after that walks the version's tree instead. Pruning can delete nodes
// under the walk, and a walk that loses a node ends with no error. If the
// shards opened one at a time, the later shards of a scan that outlived its
// block could read part of the state as all of it.
//
// That is why the fast-node index must be on, and svmd refuses to start without
// it.
type AccountScan struct {
	its []storetypes.Iterator
}

// OpenAccountScan opens every account shard of ms.
func (s Stores) OpenAccountScan(ms storetypes.MultiStore) *AccountScan {
	scan := &AccountScan{its: make([]storetypes.Iterator, 0, len(s.Accounts))}
	for _, key := range s.Accounts {
		scan.its = append(scan.its, ms.GetKVStore(key).Iterator(nil, nil))
	}
	return scan
}

// Each calls fn with every account, and closes the scan. It does not ask the
// iterators for an error afterwards. The cache store's merge iterator answers
// "invalid cacheMergeIterator" to Error when it is exhausted, which is the end
// of every scan.
func (a *AccountScan) Each(fn func(pubkey [32]byte, value []byte) error) error {
	defer a.Close()
	for _, it := range a.its {
		for ; it.Valid(); it.Next() {
			if err := fn([32]byte(it.Key()), it.Value()); err != nil {
				return err
			}
		}
	}
	return nil
}

// Close releases every iterator the scan still holds.
func (a *AccountScan) Close() {
	for _, it := range a.its {
		it.Close()
	}
	a.its = nil
}

// ArenaBytes is the most account data one block loads into the execute
// arena.
const ArenaBytes = 512 << 20

// SimulationBytes is the most account data that one simulation loads: Anza's
// MAX_LOADED_ACCOUNTS_DATA_SIZE_BYTES, the most that any one transaction can
// load. A transaction that it cuts fails in Agave too, so it is neither
// consensus nor a knob. It moves when Anza's does.
const SimulationBytes = 64 << 20

// accountBaseSize is what Anza charges an account beside its data,
// TRANSACTION_ACCOUNT_BASE_SIZE.
const accountBaseSize = 64

// loadCost is what a stored account costs a load budget: the base size plus
// the data length its head declares.
func loadCost(value []byte) int64 { return accountBaseSize + int64(ffi.DataLen(value)) }

// sysvars are loaded whatever the budget, because the engine halts the machine
// on a missing one, and all nine are small.
var sysvars = map[[32]byte]bool{
	ClockID: true, EpochScheduleID: true, EpochRewardsID: true, RentID: true, SlotHashesID: true,
	StakeHistoryID: true, LastRestartSlotID: true, FeesID: true, RecentBlockhashesID: true,
}

// Loaded is what phase 2 read of one account before the block ran. The
// block's write of the account then needs no read of its own. Lamports is
// what the token index's total subtracts. Token is the token index entry that
// the index drops.
type Loaded struct {
	Lamports uint64
	Token    TokenEntry
}

// PushAccounts appends the stored accounts of keys to an arena in order. Right
// after each loader v3 Program it appends the ProgramData that the Program
// points at. The SVM loads ProgramData through its callback when the program
// runs, and it is not a key of the transaction, so Go follows the pointer.
// Each account is read once, and keys with no account are left out.
//
// Every account but a sysvar costs 64 bytes plus its data against budget, as
// Anza counts it. used is what the pushed accounts cost. The first account that
// does not fit ends loading. It and every key after it are returned as
// dropped, unread. A dropped ProgramData brings its Program with it, so no
// transaction runs that program without its text.
//
// Each record points at the value that ms already holds and does not copy it,
// so the arena must reach Execute or Simulate, which unpin it.
//
// pre, when not nil, receives what was read of every key pushed (Loaded).
//
// After every key, the mint of each pushed token account follows, while it
// fits, because solana-svm reads it for the account's token balance and a
// transfer does not name it. A mint is never a key that the ceiling dropped,
// and no transaction names one that was not already pushed, so the mints
// change no outcome.
//
// prefetch runs the reads first, on all eight shards at once. It reads the
// keys, then the ProgramData and mints that the keys' values name. The loop
// that follows alone decides what is pushed and where the block is cut. So
// the order and the cut do not change. The reads, which a chain of 10,000,000
// accounts serves from disk, run concurrently.
//
// Each account has a slot, which indexes what was read of it and its flags.
// A key's slot is where it first appears in keys. So value and flags line up
// with keys. An account that no key names takes the next slot past them
// (extra). Only slot is keyed by address, and the loop gets the keys by index.
func (s Stores) PushAccounts(w *ffi.ArenaWriter, ms storetypes.MultiStore, keys [][32]byte, budget int64, pre map[[32]byte]Loaded) (dropped [][32]byte, used int64) {
	slot := make(map[[32]byte]int32, len(keys))
	value := make([][]byte, len(keys))
	flags := make([]uint8, len(keys))
	for i, k := range keys {
		if _, ok := slot[k]; ok {
			flags[i] = slotDup
		} else {
			slot[k] = int32(i)
		}
	}
	var extra [][32]byte
	var xvalue [][]byte
	var xflags []uint8
	slotOf := func(k [32]byte) int {
		n, ok := slot[k]
		if !ok {
			n = int32(len(keys) + len(extra))
			slot[k] = n
			extra, xvalue, xflags = append(extra, k), append(xvalue, nil), append(xflags, 0)
		}
		return int(n)
	}
	// at is slot n's value and flags.
	at := func(n int) (*[]byte, *uint8) {
		if n < len(keys) {
			return &value[n], &flags[n]
		}
		return &xvalue[n-len(keys)], &xflags[n-len(keys)]
	}
	read := s.prefetch(ms, keys, budget, math.MaxInt, value, flags)
	for _, v := range value {
		if pd, program := programDataOf(v); program {
			slotOf(pd)
		}
		if a, err := ffi.DecodeAccount(v); err == nil {
			if t, ok := ParseTokenAccount(a.Owner, a.Data); ok {
				slotOf(t.Mint)
			}
		}
	}
	// A key that a value names is not read again here. The first prefetch read
	// it, or stopped at the budget, and then this one reads nothing.
	s.prefetch(ms, extra, budget-read, math.MaxInt, xvalue, xflags)
	seen := func(n int) bool {
		_, f := at(n)
		return *f&slotSeen != 0
	}
	var mints [][32]byte
	// fits reads k, whose slot is n, and pushes it if it fits. It returns the
	// value, or nil for an account that does not exist.
	fits := func(k [32]byte, n int) ([]byte, bool) {
		pv, f := at(n)
		*f |= slotSeen
		v := *pv
		if *f&slotRead == 0 {
			v = s.GetAccount(ms, k)
		}
		if v == nil {
			return nil, true
		}
		if !sysvars[k] {
			cost := loadCost(v)
			if used+cost > budget {
				return nil, false
			}
			used += cost
		}
		w.PushPinned(&k, v)
		entry := entryOf(v)
		if pre != nil {
			pre[k] = Loaded{Lamports: ffi.Lamports(v), Token: entry}
		}
		if entry.Token {
			mints = append(mints, entry.Mint)
		}
		return v, true
	}
	// rest drops each key from index from on that the loop has not seen. It
	// drops a key once. A key named earlier was seen where it was named first.
	rest := func(from int) {
		for j := from; j < len(keys); j++ {
			if flags[j]&(slotDup|slotSeen) == 0 {
				flags[j] |= slotSeen
				dropped = append(dropped, keys[j])
			}
		}
	}
load:
	for i, k := range keys {
		if flags[i]&(slotDup|slotSeen) != 0 {
			continue
		}
		v, ok := fits(k, i)
		if !ok {
			dropped = [][32]byte{k}
			rest(i + 1)
			break load
		}
		pd, program := programDataOf(v)
		if !program {
			continue
		}
		n := slotOf(pd)
		if seen(n) {
			continue
		}
		if _, ok := fits(pd, n); !ok {
			dropped = [][32]byte{pd, k}
			rest(i + 1)
			break load
		}
	}
	for _, m := range mints {
		n := slotOf(m)
		if seen(n) {
			continue
		}
		if _, ok := fits(m, n); !ok {
			break
		}
	}
	return dropped, used
}

// The flags of a slot. slotDup is a key named earlier in keys. slotRead is an
// account that prefetch read. slotSeen is one that PushAccounts pushed or
// dropped.
const (
	slotDup uint8 = 1 << iota
	slotRead
	slotSeen
)

// prefetch reads keys from all eight shards at once into value, which lines up
// with keys. It flags each key it read slotRead, and it skips a key flagged
// slotDup. A read key's value is nil for an account that does not exist. It
// stops reading near budget, as PushAccounts' loop stops pushing. So a block
// that names more bytes than the ceiling admits cannot make the machine hold
// them all. A key it did not read is left unflagged. The loop reads it in
// order when it gets there, and cuts the block there as before. An account with
// more than maxData bytes of data reads as nil, so its value is not kept. It
// returns the loadCost of every account it kept.
func (s Stores) prefetch(ms storetypes.MultiStore, keys [][32]byte, budget int64, maxData int, value [][]byte, flags []uint8) int64 {
	var byShard [8][]int32
	for i, k := range keys {
		if flags[i]&slotDup == 0 {
			byShard[Shard(k)] = append(byShard[Shard(k)], int32(i))
		}
	}
	// The lookups come before the goroutines, as in trees.
	var stores [8]storetypes.KVStore
	for i, key := range s.Accounts {
		stores[i] = ms.GetKVStore(key)
	}
	var read [8]int
	var used atomic.Int64
	s.eachShard(func(i int) {
		store := stores[i]
		for _, n := range byShard[i] {
			if used.Load() > budget {
				break
			}
			// A copy, as before. The store may keep the key that it gets.
			k := keys[n]
			v := store.Get(k[:])
			if v != nil && ffi.DataLen(v) > maxData {
				v = nil
			}
			value[n] = v
			read[i]++
			if v != nil {
				used.Add(loadCost(v))
			}
		}
	})
	for i, shard := range byShard {
		for _, n := range shard[:read[i]] {
			flags[n] |= slotRead
		}
	}
	return used.Load()
}

// programDataOf returns the ProgramData address a loader v3 Program account
// points at. UpgradeableLoaderState::Program is a u32 tag of 2, then the
// address.
func programDataOf(value []byte) ([32]byte, bool) {
	a, err := ffi.DecodeAccount(value)
	if err != nil || a.Owner != LoaderV3 || len(a.Data) != programLen || binary.LittleEndian.Uint32(a.Data[0:4]) != 2 {
		return [32]byte{}, false
	}
	return [32]byte(a.Data[4:36]), true
}

// EngineDigest is the digest the chain has adopted, or nil before genesis.
func (s Stores) EngineDigest(ms storetypes.MultiStore) []byte {
	return ms.GetKVStore(s.Meta).Get(MetaDigestKey)
}

// AdoptEngine records the running engine's digest. Genesis calls it, and so
// must every upgrade handler that changes the engine. It makes a divergent
// build fork at a known block, not when a transaction happens to exercise the
// difference.
func (s Stores) AdoptEngine(ms storetypes.MultiStore) {
	digest := ffi.Digest()
	ms.GetKVStore(s.Meta).Set(MetaDigestKey, digest[:])
}

// TransactionCount returns the number of processed SVM transactions since
// genesis.
func (s Stores) TransactionCount(ms storetypes.MultiStore) uint64 {
	v := ms.GetKVStore(s.Meta).Get(MetaTransactionCountKey)
	if len(v) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(v)
}

// countTransactions adds a block's processed SVM transactions to the count. A
// processed transaction left a record. It is a success, or a failure that paid
// its fee, as in Agave's processed_transactions_count in commit_transactions.
// A block that processed none writes nothing.
func (s Stores) countTransactions(ms storetypes.MultiStore, n uint64) {
	if n == 0 {
		return
	}
	ms.GetKVStore(s.Meta).Set(MetaTransactionCountKey, binary.BigEndian.AppendUint64(nil, s.TransactionCount(ms)+n))
}

// ChainNetwork is the network the chain declared at genesis, or "" on a
// machine with no committed block.
func (s Stores) ChainNetwork(ms storetypes.MultiStore) string {
	return string(ms.GetKVStore(s.Meta).Get(MetaNetworkKey))
}

// AdoptNetwork records this build's network. Only genesis calls it. Unlike the
// engine, an upgrade does not change the network, and a binary that wants to
// change it is the binary that Assert refuses.
func (s Stores) AdoptNetwork(ms storetypes.MultiStore) {
	ms.GetKVStore(s.Meta).Set(MetaNetworkKey, []byte(Network))
}

// DedupCommitment is the commitment the tree carries, or nil on a machine with
// no committed block.
func (s Stores) DedupCommitment(ms storetypes.MultiStore) []byte {
	return ms.GetKVStore(s.Meta).Get(MetaDedupCommitmentKey)
}

// SetDedupCommitment writes the commitment. The runner calls it once per block,
// after the block's keys are in the set.
func (s Stores) SetDedupCommitment(ms storetypes.MultiStore, commitment [32]byte) {
	ms.GetKVStore(s.Meta).Set(MetaDedupCommitmentKey, commitment[:])
}

// AssertDedup refuses to start when the dedup set that the machine holds is not
// the one the chain committed to.
//
// The SDK does not make this check. `doRestoreSnapshot` walks whatever
// extensions the stream carries, and accepts a stream that carries none. A
// restore that silently dropped the dedup set would leave a machine that
// accepts replays. This one comparison catches that, a tampered payload, and a
// set that drifted for any other reason.
//
// stored is nil before the first block, when nothing is committed.
func AssertDedup(height uint64, dedup *Dedup, stored []byte) error {
	if stored == nil {
		return nil
	}
	if dedup == nil {
		return fmt.Errorf("svm: the chain committed to a dedup set at height %d and this machine has none", height)
	}
	if have := dedup.Commitment(height); !bytes.Equal(stored, have[:]) {
		return fmt.Errorf("svm: dedup set commits to %x at height %d, the chain committed %x", have, height, stored)
	}
	return nil
}

// Assert is the startup assertion. It has three clauses, and the machine
// refuses to start if any fails.
//
// stored is nil, and storedNetwork is "", on a machine with no committed block,
// where nothing is adopted yet. InitGenesis checks that case.
func Assert(arch string, compiled [32]byte, stored []byte, storedNetwork string) error {
	// solana-sbpf gates its JIT to x86_64, so another architecture is a second
	// execution engine under one consensus.
	if arch != "amd64" {
		return fmt.Errorf("svm: this engine runs on amd64 only, not %s", arch)
	}
	if stored != nil && !bytes.Equal(stored, compiled[:]) {
		return fmt.Errorf("svm: this binary has engine digest %x, the chain adopted %x", compiled, stored)
	}
	// Both directions refuse. A production binary on a devnet chain is
	// harmless to consensus, and that is the case worth refusing. Everything
	// appears to work, and only the faucet does not exist. The two builds
	// report the same version and the same engine digest, so this refusal is
	// the only thing that tells an operator which binary they hold.
	if storedNetwork != "" && storedNetwork != Network {
		return fmt.Errorf("svm: this is a %s binary, the chain is a %s chain", Network, storedNetwork)
	}
	return nil
}
