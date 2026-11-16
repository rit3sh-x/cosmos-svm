package svm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"cosmossdk.io/log/v2"

	dbm "github.com/cosmos/cosmos-db"
	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
)

var (
	// SplToken and SplToken2022 are the two programs whose accounts the owner
	// index covers. They are the pair that `spl_generic_token::spl_token_ids`
	// returns (`spl-generic-token-2.0.1`).
	SplToken     = mustKey("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")
	SplToken2022 = mustKey("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb")
)

// IsTokenProgram is spl-generic-token's `is_known_spl_token_id`.
func IsTokenProgram(id [32]byte) bool { return id == SplToken || id == SplToken2022 }

// The token account layout, copied from spl-generic-token's `token.rs` and
// `token_2022.rs`, not inferred. Token-2022 pads an extended mint out to
// account length, so a mint cannot pass as an account. It forbids a mint or
// account of exactly multisig length, so size and one byte tell the three
// apart.
const (
	tokenAccountMintOffset   = 0
	tokenAccountOwnerOffset  = 32
	tokenAccountAmountOffset = 64
	tokenAccountStateOffset  = 108
	tokenAccountLength       = 165
	tokenMultisigLength      = 355
	tokenAccountTypeAccount  = 2

	tokenMintSupplyOffset        = 36
	tokenMintDecimalsOffset      = 44
	tokenMintIsInitializedOffset = 45
	tokenMintLength              = 82
	tokenAccountTypeMint         = 1
)

// TokenAccount is what the index and the two methods need from a token
// account: `generic_token::Account` without the parts that nothing here reads.
type TokenAccount struct {
	Mint   [32]byte
	Owner  [32]byte
	Amount uint64
}

// ParseTokenAccount is `generic_token::Account::unpack`. The program id
// selects the validity rule, and an account that does not pass it is not a
// token account.
//
// It is copied, not linked, because the crate is Rust and this code runs on
// the write path in Go. It is the one place in this repository that reads SPL
// bytes, and it reads only the fields that Agave's own minimal parser reads.
func ParseTokenAccount(program [32]byte, data []byte) (TokenAccount, bool) {
	switch program {
	case SplToken:
		if !validTokenAccount(data) {
			return TokenAccount{}, false
		}
	case SplToken2022:
		if !ValidTokenAccountData(data) {
			return TokenAccount{}, false
		}
	default:
		return TokenAccount{}, false
	}
	return TokenAccount{
		Mint:   [32]byte(data[tokenAccountMintOffset : tokenAccountMintOffset+32]),
		Owner:  [32]byte(data[tokenAccountOwnerOffset : tokenAccountOwnerOffset+32]),
		Amount: binary.LittleEndian.Uint64(data[tokenAccountAmountOffset : tokenAccountAmountOffset+8]),
	}, true
}

// ValidTokenAccountData is Token-2022's `Account::valid_account_data`, which
// also holds every SPL Token account. It comes from spl-generic-token.
func ValidTokenAccountData(data []byte) bool {
	return validTokenAccount(data) || validToken2022Account(data)
}

func validTokenAccount(data []byte) bool {
	return len(data) == tokenAccountLength && data[tokenAccountStateOffset] != 0
}

func validToken2022Account(data []byte) bool {
	t, ok := token2022Type(data)
	return ok && t == tokenAccountTypeAccount && data[tokenAccountStateOffset] != 0
}

// token2022Type is the account-type byte an extended Token-2022 account or
// mint carries right after the base account, at a length no multisig has.
func token2022Type(data []byte) (byte, bool) {
	if len(data) <= tokenAccountLength || len(data) == tokenMultisigLength {
		return 0, false
	}
	return data[tokenAccountLength], true
}

// TokenMint is `generic_token::Mint`: what `getTokenAccountBalance` needs and
// the account alone cannot give.
type TokenMint struct {
	Supply   uint64
	Decimals uint8
}

// ParseTokenMint is `generic_token::Mint::unpack`. The Token-2022 branch tests
// against account length, not mint length. Upstream pads an extended mint out
// to account length, so that an account cannot pass as a mint.
func ParseTokenMint(program [32]byte, data []byte) (TokenMint, bool) {
	valid := len(data) == tokenMintLength && data[tokenMintIsInitializedOffset] != 0
	if !valid && program == SplToken2022 {
		t, ok := token2022Type(data)
		valid = ok && t == tokenAccountTypeMint && data[tokenMintIsInitializedOffset] != 0
	}
	if !valid || !IsTokenProgram(program) {
		return TokenMint{}, false
	}
	return TokenMint{
		Supply:   binary.LittleEndian.Uint64(data[tokenMintSupplyOffset : tokenMintSupplyOffset+8]),
		Decimals: data[tokenMintDecimalsOffset],
	}, true
}

// tokenIndexVersion is the version of the rules this index was built under.
// Increase it when what counts as a token account changes. Every machine then
// rebuilds on its next block, and does not serve a wallet a set built under the
// old rule. Version 2 has the mint keyspace and the lamport total.
const tokenIndexVersion = 2

// TokenIndex is the machine-local index the token and supply methods read.
// It is outside the commit tree:
//
//	index    'o' || owner || pubkey  ->  ()
//	mint     'n' || mint  || pubkey  ->  ()
//	reverse  'r' || pubkey           ->  owner || mint
//	marker   'm'                     ->  version u8 || height u64 BE
//	total    's'                     ->  lamports u64 BE
//
// The reverse keyspace exists because the write path gets the new value and
// not the old one. A token account's SPL owner can change, and an account
// closed and opened again can have another mint. Without the old owner and
// mint, the stale entries could not be deleted. With them, maintenance is one
// local point read and three local writes per token account written, and no
// account-store read.
//
// The total is every account's lamports, Agave's capitalization. Each block
// moves it by what the block's written accounts gained and lost, which the
// runner holds, and by what was created outside the engine (Stores.TakeMinted).
//
// The marker makes the index a pure function of current state, not of the
// history that made it. It records the height the index is current to, so a
// machine rebuilds when it joined by state sync, replayed genesis, ran an older
// version of these rules, or crashed between finalizing a block and committing
// it. It does not answer from a set that nobody maintained.
//
// FinalizeBlock and the RPC share it, so a mutex guards its state.
type TokenIndex struct {
	db     dbm.DB
	logger log.Logger

	mu      sync.Mutex
	version byte
	height  uint64
	total   uint64
	// prev is the total at height-1, so a reader of committed state can ask
	// for its height while the next block has already moved the total.
	prev uint64
	// building is true while a rebuild runs. backlog holds the writes of the
	// blocks that finalized since it began, until it has read every shard.
	building bool
	backlog  []blockWrites
	rebuilds sync.WaitGroup
}

type blockWrites struct {
	height uint64
	writes []AccountWrite
	delta  Lamports
}

// Lamports is what one block added to and took from every account's lamports.
// Two sums keep it unsigned.
type Lamports struct{ Added, Removed uint64 }

// Move adds one account's change from pre to post.
func (l *Lamports) Move(pre, post uint64) {
	l.Added += post
	l.Removed += pre
}

// ErrTokenIndexBehind is what the index answers while it rebuilds, instead of
// an answer from a set it has only partly read.
var ErrTokenIndexBehind = errors.New("svm: the token index is rebuilding")

// rebuildBatch is the number of index entries that a rebuild holds before it
// writes them. This bounds its memory, not the account count.
const rebuildBatch = 10_000

// AccountWrite is one account the block wrote, already classified.
//
// It carries the answer and not the bytes, because the write set lives in the
// response arena, which is freed when the block's phase 3 returns. A slice of
// it held past that point reads freed memory. A copy of every written account
// would cost an allocation per write on the commit path. Its entry is the
// account's entry after the block. Before is its entry before the block, which
// phase 2 read when it loaded the account (Loaded). The index drops Before and
// does not read it back.
type AccountWrite struct {
	Pubkey [32]byte
	TokenEntry
	Before TokenEntry
}

// TokenEntry is what the index holds for one account. Token is false for an
// account that is not a token account and for one that does not exist.
type TokenEntry struct {
	Owner [32]byte
	Mint  [32]byte
	Token bool
}

// entryOf is the index's entry for an account whose stored encoding is value,
// as a refill reads it.
func entryOf(value []byte) TokenEntry {
	a, err := ffi.DecodeAccount(value)
	if err != nil {
		return TokenEntry{}
	}
	t, ok := ParseTokenAccount(a.Owner, a.Data)
	if !ok {
		return TokenEntry{}
	}
	return TokenEntry{Owner: t.Owner, Mint: t.Mint, Token: true}
}

// Classify returns what the runner records for one written account: whether it
// is a token account now, and whose. value is the stored encoding that the
// write set carried. A zero-lamport account is one that SetAccount deletes.
func Classify(pubkey [32]byte, value []byte) AccountWrite {
	w := AccountWrite{Pubkey: pubkey}
	if len(value) < 8 || ffi.Lamports(value) == 0 {
		return w
	}
	w.TokenEntry = entryOf(value)
	return w
}

func ownerKey(owner, pubkey [32]byte) []byte {
	k := make([]byte, 0, 1+32+32)
	return append(append(append(k, 'o'), owner[:]...), pubkey[:]...)
}

func mintKey(mint, pubkey [32]byte) []byte {
	k := make([]byte, 0, 1+32+32)
	return append(append(append(k, 'n'), mint[:]...), pubkey[:]...)
}

func reverseKey(pubkey [32]byte) []byte {
	return append([]byte{'r'}, pubkey[:]...)
}

func totalKey() []byte { return []byte{'s'} }

// OpenTokenIndex reads the marker. It never rebuilds here, because a rebuild
// needs the account stores and this runs before the app has them.
func OpenTokenIndex(db dbm.DB, logger log.Logger) (*TokenIndex, error) {
	t := &TokenIndex{db: db, logger: logger}
	v, err := db.Get([]byte{'m'})
	if err != nil {
		return nil, err
	}
	if len(v) == 9 {
		t.version, t.height = v[0], binary.BigEndian.Uint64(v[1:])
	}
	if v, err = db.Get(totalKey()); err != nil {
		return nil, err
	} else if len(v) == 8 {
		t.total = binary.BigEndian.Uint64(v)
	}
	return t, nil
}

// Height returns the height the index is current to, for tests and operators.
func (t *TokenIndex) Height() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.height
}

// Wait returns once no rebuild is running.
func (t *TokenIndex) Wait() { t.rebuilds.Wait() }

func (t *TokenIndex) setMarker(batch dbm.Batch, height uint64) error {
	return batch.Set([]byte{'m'}, append([]byte{tokenIndexVersion}, binary.BigEndian.AppendUint64(nil, height)...))
}

// apply adds this account to the batch. It drops the entry the account had
// before the block, and writes a new one if it is a token account now. The
// index is a pure function of state and is current to the block before. So
// its entry is Before, and apply reads nothing.
func (t *TokenIndex) apply(batch dbm.Batch, w AccountWrite) error {
	if w.Before.Token {
		if err := batch.Delete(ownerKey(w.Before.Owner, w.Pubkey)); err != nil {
			return err
		}
		if err := batch.Delete(mintKey(w.Before.Mint, w.Pubkey)); err != nil {
			return err
		}
		if err := batch.Delete(reverseKey(w.Pubkey)); err != nil {
			return err
		}
	}
	if !w.Token {
		return nil
	}
	return setToken(batch, w.Pubkey, w.Owner, w.Mint)
}

// Entry is the entry the database holds for pubkey, which is Before when the
// index is current to the block before. The block path never reads it; the
// phase 2 benchmark does, outside its timing, where the runner has the loaded
// value.
func (t *TokenIndex) Entry(pubkey [32]byte) (TokenEntry, error) {
	v, err := t.db.Get(reverseKey(pubkey))
	if err != nil || len(v) != 64 {
		return TokenEntry{}, err
	}
	return TokenEntry{Owner: [32]byte(v[:32]), Mint: [32]byte(v[32:]), Token: true}, nil
}

// setToken writes one token account's three entries.
func setToken(batch dbm.Batch, pubkey, owner, mint [32]byte) error {
	if err := batch.Set(ownerKey(owner, pubkey), []byte{}); err != nil {
		return err
	}
	if err := batch.Set(mintKey(mint, pubkey), []byte{}); err != nil {
		return err
	}
	return batch.Set(reverseKey(pubkey), append(owner[:], mint[:]...))
}

// Follow keeps the index current through the block at height, whose account
// writes are writes. An index current to the block before returns the write
// that applies them. The runner runs that write beside the block's commit,
// with the writes of the history and the dedup set, and waits for it before
// the height commits.
//
// Any other index is rebuilt from committed state on its own goroutine, while
// blocks keep finalizing. Their writes wait in memory until the rebuild has
// read every shard, and are then applied in order.
//
// The rebuild reads the latest committed version, height-1. Follow opens every
// shard here, inside FinalizeBlock, where that version is still the latest.
// This keeps the scan whole however many blocks commit, and however much of
// height-1 is pruned, while it runs. Before the first block commits there is
// no committed version. The stores then hold only genesis and this block, and
// block is that state. The refill then runs here, because its size is that of
// the genesis file.
func (t *TokenIndex) Follow(height uint64, writes []AccountWrite, delta Lamports, s Stores, block storetypes.MultiStore, state func() (storetypes.MultiStore, uint64, error)) (func() error, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.building {
		t.backlog = append(t.backlog, blockWrites{height, writes, delta})
		return nil, nil
	}
	if t.version == tokenIndexVersion && t.height+1 == height {
		return func() error {
			t.mu.Lock()
			defer t.mu.Unlock()
			return t.write(height, writes, delta)
		}, nil
	}
	ms, from, err := state()
	if err != nil {
		return nil, err
	}
	if ms == nil {
		// block already holds this block's writes, so its delta is in the
		// refilled total.
		total, err := t.refill(s.OpenAccountScan(block))
		if err != nil {
			return nil, err
		}
		t.total = total
		return nil, t.write(height, nil, Lamports{})
	}
	// The marker goes first, so a crash during the rebuild leaves an index
	// that rebuilds again rather than one that claims to be current.
	if err := t.db.DeleteSync([]byte{'m'}); err != nil {
		return nil, err
	}
	t.version, t.height = 0, 0
	t.building, t.backlog = true, []blockWrites{{height, writes, delta}}
	t.rebuilds.Add(1)
	go t.rebuild(s.OpenAccountScan(ms), from)
	return nil, nil
}

// write records a block's account writes and lamport change and moves the
// marker. The caller holds mu.
func (t *TokenIndex) write(height uint64, writes []AccountWrite, delta Lamports) error {
	batch := t.db.NewBatch()
	defer batch.Close()
	for _, w := range writes {
		if err := t.apply(batch, w); err != nil {
			return err
		}
	}
	total := t.total + delta.Added - delta.Removed
	if err := batch.Set(totalKey(), binary.BigEndian.AppendUint64(nil, total)); err != nil {
		return err
	}
	if err := t.setMarker(batch, height); err != nil {
		return err
	}
	if err := batch.WriteSync(); err != nil {
		return err
	}
	t.prev = t.total
	if t.height+1 != height {
		t.prev = total
	}
	t.version, t.height, t.total = tokenIndexVersion, height, total
	return nil
}

func (t *TokenIndex) Write(height uint64, writes []AccountWrite) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.write(height, writes, Lamports{})
}

// rebuild refills the index from scan, the committed state at height from,
// then applies the blocks that finalized meanwhile. A failure leaves the index
// stale, and the next block starts another.
func (t *TokenIndex) rebuild(scan *AccountScan, from uint64) {
	defer t.rebuilds.Done()
	total, err := t.refill(scan)
	t.mu.Lock()
	defer t.mu.Unlock()
	backlog := t.backlog
	t.building, t.backlog = false, nil
	if err == nil {
		t.total = total
		err = t.write(from, nil, Lamports{})
	}
	for _, b := range backlog {
		if err != nil {
			break
		}
		err = t.write(b.height, b.writes, b.delta)
	}
	if err != nil {
		t.version = 0
		t.logger.Error("svm token index rebuild failed, the next block starts another", "from", from, "err", err)
	}
}

// refill throws the index away and refills it from scan. This works because
// the index is a pure function of current state. For the same reason the index
// needs no operator setting, because there is no lost history to recover. It
// writes every rebuildBatch entries, so its memory is that batch and not the
// account count. It returns the scan's lamport total, for the caller to set
// under mu.
func (t *TokenIndex) refill(scan *AccountScan) (uint64, error) {
	defer scan.Close()
	for _, prefix := range []byte{'o', 'n', 'r'} {
		if err := t.clear(prefix); err != nil {
			return 0, err
		}
	}
	batch := t.db.NewBatch()
	defer func() { batch.Close() }()
	entries := 0
	var total uint64
	err := scan.Each(func(pubkey [32]byte, value []byte) error {
		a, err := ffi.DecodeAccount(value)
		if err != nil {
			return err
		}
		total += a.Lamports
		token, ok := ParseTokenAccount(a.Owner, a.Data)
		if !ok {
			return nil
		}
		if err := setToken(batch, pubkey, token.Owner, token.Mint); err != nil {
			return err
		}
		if entries++; entries < rebuildBatch {
			return nil
		}
		if err := batch.Write(); err != nil {
			return err
		}
		batch.Close()
		batch, entries = t.db.NewBatch(), 0
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, batch.WriteSync()
}

// clear deletes one keyspace, rebuildBatch keys at a time. The iterator is
// closed before each write, which an in-memory database needs.
func (t *TokenIndex) clear(prefix byte) error {
	for {
		it, err := t.db.Iterator([]byte{prefix}, []byte{prefix + 1})
		if err != nil {
			return err
		}
		var keys [][]byte
		for ; it.Valid() && len(keys) < rebuildBatch; it.Next() {
			keys = append(keys, append([]byte(nil), it.Key()...))
		}
		if err := it.Close(); err != nil {
			return err
		}
		if len(keys) == 0 {
			return nil
		}
		batch := t.db.NewBatch()
		for _, k := range keys {
			if err := batch.Delete(k); err != nil {
				batch.Close()
				return err
			}
		}
		err = batch.Write()
		batch.Close()
		if err != nil {
			return err
		}
	}
}

// Accounts returns the token accounts that an owner holds, ascending by pubkey.
// It is one prefix scan, because the owner is the key prefix. While the index
// rebuilds, it answers ErrTokenIndexBehind, not a partial list.
func (t *TokenIndex) Accounts(owner [32]byte) ([][32]byte, error) {
	return t.under('o', owner)
}

// MintAccounts returns the token accounts of a mint, ascending by pubkey. It
// is Agave's spl-token-mint index.
func (t *TokenIndex) MintAccounts(mint [32]byte) ([][32]byte, error) {
	return t.under('n', mint)
}

// Total returns every account's lamports at the height the index is current
// to, Agave's capitalization.
func (t *TokenIndex) Total() (uint64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.building || t.version != tokenIndexVersion {
		return 0, ErrTokenIndexBehind
	}
	return t.total, nil
}

// TotalAt returns the total at height h, which is the height the index is
// current to or the one before it. The index moves before the block commits,
// so a reader of committed state can be one block behind it.
func (t *TokenIndex) TotalAt(h uint64) (uint64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.building || t.version != tokenIndexVersion:
		return 0, ErrTokenIndexBehind
	case h == t.height:
		return t.total, nil
	case h+1 == t.height:
		return t.prev, nil
	}
	return 0, ErrTokenIndexBehind
}

func (t *TokenIndex) ready() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.building && t.version == tokenIndexVersion
}

// under lists the pubkeys of one keyspace under one 32-byte prefix.
func (t *TokenIndex) under(space byte, prefix [32]byte) ([][32]byte, error) {
	if !t.ready() {
		return nil, ErrTokenIndexBehind
	}
	start := append([]byte{space}, prefix[:]...)
	it, err := t.db.Iterator(start, prefixEnd(start))
	if err != nil {
		return nil, err
	}
	defer it.Close()
	var out [][32]byte
	for ; it.Valid(); it.Next() {
		key := it.Key()
		if len(key) != 1+32+32 {
			return nil, fmt.Errorf("svm token index entry %x", key)
		}
		out = append(out, [32]byte(key[33:]))
	}
	return out, nil
}

// prefixEnd is the first key after every key that starts with prefix. A
// prefix whose last bytes are 0xff carries into the byte before, so a key
// ending in 0xff still has an end above it.
func prefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i]++; end[i] != 0 {
			return end[:i+1]
		}
	}
	return nil
}
