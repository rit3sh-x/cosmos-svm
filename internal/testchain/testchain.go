// Package testchain drives one in-process validator of this chain through ABCI
// for tests. It also encodes the Solana transactions that the tests submit.
package testchain

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	cmttypes "github.com/cometbft/cometbft/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/mr-tron/base58"

	"cosmossdk.io/log/v2"
	"cosmossdk.io/math"

	cometed25519 "github.com/cosmos/cosmos-sdk/crypto/keys/ed25519"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	authsigning "github.com/cosmos/cosmos-sdk/x/auth/signing"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"

	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/store/v2/snapshots"
	snapshottypes "github.com/cosmos/cosmos-sdk/store/v2/snapshots/types"

	"github.com/rit3sh-x/cosmos-svm/app"
	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/genesis"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// FaucetLamports is the faucet's genesis balance, 10 million SOL.
const FaucetLamports = 10_000_000 * 1_000_000_000

// Chain is one validator. Hashes[h-1] is the header hash of height h.
type Chain struct {
	T      testing.TB
	App    *app.App
	Height int64
	Hashes [][]byte
	// GenesisHash is the app hash InitChain returned.
	GenesisHash []byte
	// Operators are the bonded machines this chain's genesis names.
	Operators []app.Operator
	// RetainHeight is the highest retain height that any commit gave
	// CometBFT. Zero means that no commit pruned blocks. CometBFT never
	// restores a block it pruned, so this is the lowest block the machine
	// still holds.
	RetainHeight int64

	db, dedup, history, tokens dbm.DB
	// home is where x/upgrade reads and writes the plan file. It survives a
	// restart, as a machine's own home does.
	home     string
	maxBytes uint64
	options  []func(*baseapp.BaseApp)
	// skip is the --unsafe-skip-upgrades value that the next open passes.
	skip   map[int64]bool
	offset time.Duration
}

// Key is a deterministic keypair.
func Key(seed byte) ed25519.PrivateKey {
	s := sha256.Sum256([]byte{'k', seed})
	return ed25519.NewKeyFromSeed(s[:])
}

// Pub returns k's public key.
func Pub(k ed25519.PrivateKey) [32]byte { return [32]byte(k.Public().(ed25519.PublicKey)) }

// Options changes how Start builds the chain.
type Options struct {
	// NoDedup builds the app without the replay set.
	NoDedup bool
	// NoTokens builds the app without the token owner index. A test
	// measures block time with the index against this baseline.
	NoTokens bool
	// Tokens replaces the token owner index's in-memory database, so a test
	// can stop a rebuild where it wants.
	Tokens dbm.DB
	// IndexMaxBytes bounds the history record. Zero means no bound.
	IndexMaxBytes uint64
	// Accounts are added to the default genesis beside the faucet.
	Accounts []svm.GenesisAccount
	Programs []svm.GenesisProgram
	// Edit changes the genesis state last, for tests of genesis itself.
	Edit func(*svm.GenesisState)
	// BaseApp are operator options, applied after the app's own defaults, as
	// server.DefaultBaseappOptions applies an app.toml.
	BaseApp []func(*baseapp.BaseApp)
	// SnapshotInterval turns on the snapshot store, which state sync needs.
	// Zero leaves it off, as on a machine whose operator turned snapshots off.
	SnapshotInterval uint64
	// Operators are the machines allowed into the validator set. Empty means
	// the one DefaultOperators returns.
	Operators []app.Operator
	// Outsiders get an account and no coin, like every address that is not
	// an operator.
	Outsiders []sdk.AccAddress
	// Bank changes the bank genesis last, so a test can give an outsider a
	// coin that is not usvm.
	Bank func(*banktypes.GenesisState)
	// Disk keeps the application database and the sidecars on disk, on
	// Pebble, opened as svmd opens them. A test of a scan that runs while
	// blocks commit needs it, because an in-memory database's iterator holds a
	// read lock that stalls every commit until the iterator closes. A Tokens
	// database that the test passes stays its own.
	Disk bool
	// InitialHeight is the chain's first block, or 1 when zero. A test uses
	// it to reach an epoch boundary without the blocks before it.
	InitialHeight int64
}

// OperatorKey is a deterministic operator account key.
func OperatorKey(seed byte) ed25519.PrivateKey {
	s := sha256.Sum256([]byte{'o', seed})
	return ed25519.NewKeyFromSeed(s[:])
}

// Address is an operator key's Cosmos account address.
func Address(k ed25519.PrivateKey) sdk.AccAddress {
	pub := cometed25519.PubKey{Key: k.Public().(ed25519.PublicKey)}
	return sdk.AccAddress(pub.Address())
}

// ConsensusPubkey is an operator key's CometBFT signing key.
func ConsensusPubkey(k ed25519.PrivateKey) *cometed25519.PubKey {
	return &cometed25519.PubKey{Key: k.Public().(ed25519.PublicKey)}
}

// DefaultOperators returns n bonded operators with deterministic keys. One
// operator is the smallest genesis that gives the chain a validator set.
func DefaultOperators(n int) []app.Operator {
	out := make([]app.Operator, 0, n)
	for i := 0; i < n; i++ {
		k := OperatorKey(byte(i))
		out = append(out, app.Operator{
			Account:         Address(k),
			ConsensusPubkey: ConsensusPubkey(k),
			SelfBond:        math.NewInt(10_000_000),
			Free:            math.NewInt(90_000_000),
			Moniker:         fmt.Sprintf("operator-%d", i),
		})
	}
	return out
}

// Start runs InitChain with the default genesis plus a faucet.
func Start(t testing.TB, faucet [32]byte, opts Options) *Chain {
	t.Helper()
	c := &Chain{T: t, db: dbm.NewMemDB(), dedup: dbm.NewMemDB(), history: dbm.NewMemDB(), tokens: dbm.NewMemDB(), maxBytes: opts.IndexMaxBytes, options: opts.BaseApp}
	if opts.Tokens != nil {
		c.tokens = opts.Tokens
	}
	if opts.NoTokens {
		c.tokens = nil
	}
	if opts.NoDedup {
		c.dedup = nil
	}
	if opts.Disk {
		dir := t.TempDir()
		db, err := app.OpenApplicationDB(dir)
		if err != nil {
			t.Fatal(err)
		}
		c.db = db
		dbs := []dbm.DB{db}
		for _, side := range []struct {
			db   *dbm.DB
			name string
			open func(name, dir string) (dbm.DB, error)
		}{{&c.dedup, "svmdedup", app.OpenSidecarDB}, {&c.history, "svmhistory", app.OpenIndexDB}, {&c.tokens, "svmtokens", app.OpenIndexDB}} {
			if *side.db == nil || side.db == &c.tokens && opts.Tokens != nil {
				continue
			}
			if *side.db, err = side.open(side.name, dir); err != nil {
				t.Fatal(err)
			}
			dbs = append(dbs, *side.db)
		}
		t.Cleanup(func() {
			if c.App != nil && c.App.Tokens != nil {
				c.App.Tokens.Wait()
			}
			for _, db := range dbs {
				db.Close()
			}
		})
	}
	if opts.SnapshotInterval > 0 {
		store, err := snapshots.NewStore(dbm.NewMemDB(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		c.options = append(append([]func(*baseapp.BaseApp){}, c.options...),
			baseapp.SetSnapshot(store, snapshottypes.NewSnapshotOptions(opts.SnapshotInterval, 2)))
	}
	c.open()
	a := c.App
	g := svm.DefaultGenesis()
	g.Accounts = append(g.Accounts, svm.GenesisAccount{
		Pubkey:    base58.Encode(faucet[:]),
		Lamports:  FaucetLamports,
		Owner:     base58.Encode(svm.SystemProgram[:]),
		RentEpoch: ^uint64(0),
	})
	g.Accounts = append(g.Accounts, opts.Accounts...)
	g.Programs = opts.Programs
	if opts.Edit != nil {
		opts.Edit(&g)
	}
	state := a.DefaultGenesis()
	var err error
	if state[svm.ModuleName], err = json.Marshal(g, json.Deterministic(true)); err != nil {
		t.Fatal(err)
	}
	operators := opts.Operators
	if len(operators) == 0 {
		operators = DefaultOperators(1)
	}
	c.Operators = operators
	if err := app.Permission(a.Codec, state, operators, opts.Outsiders...); err != nil {
		t.Fatal(err)
	}
	if opts.Bank != nil {
		var bank banktypes.GenesisState
		a.Codec.MustUnmarshalJSON(state[banktypes.ModuleName], &bank)
		opts.Bank(&bank)
		state[banktypes.ModuleName] = a.Codec.MustMarshalJSON(&bank)
	}
	raw, err := json.Marshal(state, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	params := cmttypes.DefaultConsensusParams().ToProto()
	params.Block.MaxBytes, params.Block.MaxGas = svm.BlockParams()
	params.Evidence.MaxAgeNumBlocks, params.Evidence.MaxAgeDuration = svm.EvidenceParams()
	c.Height = max(opts.InitialHeight, 1) - 1
	res, err := a.InitChain(&abci.RequestInitChain{
		ChainId:         "svm-test",
		InitialHeight:   c.Height + 1,
		Time:            time.Unix(1_700_000_000, 0),
		ConsensusParams: &params,
		AppStateBytes:   raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.GenesisHash = res.AppHash
	return c
}

// open builds the app over the chain's databases. Its stores prune inside
// Commit. IAVL's asynchronous pruning runs a goroutine for each store, which
// exits only when the store closes. Nothing closes the stores, so those
// goroutines would keep every finished test's chain reachable. A test that
// wants the background goroutine passes the option itself, after this one.
func (c *Chain) open() {
	options := append([]func(*baseapp.BaseApp){baseapp.SetChainID("svm-test"), baseapp.SetIAVLSyncPruning(true)}, c.options...)
	a, err := app.New(log.NewNopLogger(), c.db, app.Local{Dedup: c.dedup, History: c.history, Tokens: c.tokens, IndexMaxBytes: c.maxBytes,
		SkipUpgrades: c.skip}, c.Home(), options...)
	if err != nil {
		c.T.Fatal(err)
	}
	c.App = a
}

// RestartSkipping is Restart with --unsafe-skip-upgrades at heights.
func (c *Chain) RestartSkipping(heights ...int64) {
	c.skip = map[int64]bool{}
	for _, h := range heights {
		c.skip[h] = true
	}
	c.Restart()
}

// Restart builds a new app over the same databases, as a machine restart does.
func (c *Chain) Restart() {
	// A restarting machine ends its token index rebuild. A test's two app
	// instances share the database, so Restart waits for the first one's
	// rebuild.
	if c.App != nil && c.App.Tokens != nil {
		c.App.Tokens.Wait()
	}
	c.open()
}

// Home returns the machine's home directory, where x/upgrade writes its plan
// file.
func (c *Chain) Home() string {
	if c.home == "" {
		c.home = c.T.TempDir()
	}
	return c.home
}

// TokenIndexDB returns the database of the machine-local token owner index. A
// test can delete it or write an old version marker and watch the chain
// rebuild it.
func (c *Chain) TokenIndexDB() dbm.DB { return c.tokens }

// RestartErr is Restart for a machine that is expected to refuse to start.
func (c *Chain) RestartErr() error {
	_, err := app.New(log.NewNopLogger(), c.db, app.Local{Dedup: c.dedup, History: c.history, Tokens: c.tokens, IndexMaxBytes: c.maxBytes},
		c.Home(), append([]func(*baseapp.BaseApp){baseapp.SetChainID("svm-test")}, c.options...)...)
	return err
}

// HashAt returns the header hash that tests give height h.
func HashAt(h int64) [32]byte {
	return sha256.Sum256(binary.LittleEndian.AppendUint64(nil, uint64(h)))
}

// Block finalizes and commits one block of SVM transactions, each wrapped in
// MsgSVMTransaction.
func (c *Chain) Block(solanaTxs ...[]byte) ([]*abci.ExecTxResult, []byte) {
	c.T.Helper()
	res := c.Finalize(solanaTxs...)
	commit, err := c.App.Commit()
	if err != nil {
		c.T.Fatal(err)
	}
	c.RetainHeight = max(c.RetainHeight, commit.RetainHeight)
	c.lamportsAgree()
	return res.TxResults, res.AppHash
}

// Finalize finalizes the next block of SVM transactions and leaves it
// uncommitted.
func (c *Chain) Finalize(solanaTxs ...[]byte) *abci.ResponseFinalizeBlock {
	c.T.Helper()
	c.Height++
	hash := HashAt(c.Height)
	c.Hashes = append(c.Hashes, hash[:])
	txs := make([][]byte, len(solanaTxs))
	for i, s := range solanaTxs {
		txs[i] = c.Wrap(s)
	}
	res, err := c.App.FinalizeBlock(&abci.RequestFinalizeBlock{
		Height: c.Height,
		Time:   c.blockTime(c.Height),
		Hash:   hash[:],
		Txs:    txs,
	})
	if err != nil {
		c.T.Fatalf("height %d: %v", c.Height, err)
	}
	return res
}

// Signed finalizes and commits an empty block for x/slashing and x/evidence to
// read. Its last commit carries a vote from every operator except the indexes
// in absent, and the given misbehavior. It returns the block's validator
// updates.
func (c *Chain) Signed(absent map[int]bool, misbehavior ...abci.Misbehavior) []abci.ValidatorUpdate {
	c.T.Helper()
	votes := make([]abci.VoteInfo, len(c.Operators))
	for i, op := range c.Operators {
		flag := cmtproto.BlockIDFlagCommit
		if absent[i] {
			flag = cmtproto.BlockIDFlagAbsent
		}
		votes[i] = abci.VoteInfo{BlockIdFlag: flag, Validator: abci.Validator{
			Address: op.ConsensusPubkey.Address(),
			Power:   sdk.TokensToConsensusPower(op.SelfBond, sdk.DefaultPowerReduction),
		}}
	}
	c.Height++
	hash := HashAt(c.Height)
	c.Hashes = append(c.Hashes, hash[:])
	res, err := c.App.FinalizeBlock(&abci.RequestFinalizeBlock{
		Height:            c.Height,
		Time:              c.blockTime(c.Height),
		Hash:              hash[:],
		DecidedLastCommit: abci.CommitInfo{Votes: votes},
		Misbehavior:       misbehavior,
	})
	if err != nil {
		c.T.Fatalf("height %d: %v", c.Height, err)
	}
	commit, err := c.App.Commit()
	if err != nil {
		c.T.Fatal(err)
	}
	c.RetainHeight = max(c.RetainHeight, commit.RetainHeight)
	c.lamportsAgree()
	return res.ValidatorUpdates
}

// lamportsAgree checks, after every block that a test commits, that the token
// index's running lamport total equals a full scan of committed state. While
// the index rebuilds, it has no total to check.
func (c *Chain) lamportsAgree() {
	c.T.Helper()
	if c.App.Tokens == nil {
		return
	}
	total, err := c.App.Tokens.Total()
	if errors.Is(err, svm.ErrTokenIndexBehind) {
		return
	}
	if err != nil {
		c.T.Fatalf("height %d: the lamport total: %v", c.Height, err)
	}
	var scanned uint64
	if err := c.App.Stores.OpenAccountScan(c.App.CommitMultiStore()).Each(func(_ [32]byte, v []byte) error {
		scanned += binary.LittleEndian.Uint64(v[0:8])
		return nil
	}); err != nil {
		c.T.Fatal(err)
	}
	if total != scanned {
		c.T.Fatalf("height %d: the token index's lamport total is %d, a full scan is %d", c.Height, total, scanned)
	}
}

// BlockErr finalizes a block that is expected to halt the machine. Nothing is
// committed, and the height stays where it was.
func (c *Chain) BlockErr(solanaTxs ...[]byte) error {
	c.T.Helper()
	height := c.Height + 1
	hash := HashAt(height)
	txs := make([][]byte, len(solanaTxs))
	for i, s := range solanaTxs {
		txs[i] = c.Wrap(s)
	}
	_, err := c.App.FinalizeBlock(&abci.RequestFinalizeBlock{
		Height: height,
		Time:   c.blockTime(height),
		Hash:   hash[:],
		Txs:    txs,
	})
	if err == nil {
		c.T.Fatalf("height %d finalized, want a halt", height)
	}
	return err
}

// blockTime returns two seconds for each block, plus what Advance added.
func (c *Chain) blockTime(height int64) time.Time {
	return time.Unix(1_700_000_000+2*height, 0).Add(c.offset)
}

// Advance moves the clock forward without producing the blocks that would
// have passed.
func (c *Chain) Advance(d time.Duration) { c.offset += d }

// HoldTime gives the next block the last block's header time. A simulation
// runs the next block at that time.
func (c *Chain) HoldTime() { c.offset -= 2 * time.Second }

// Admit runs admission without the signatures, for the next block. CheckTx
// and the proposal handlers also admit for the next block.
func (c *Chain) Admit(tx []byte) (ffi.Admission, error) {
	c.T.Helper()
	_, next, err := c.App.Next()
	if err != nil {
		c.T.Fatal(err)
	}
	return ffi.CheckTx(tx, true, next.Height)
}

// Latest is the last committed block's hash, which getLatestBlockhash returns.
func (c *Chain) Latest() [32]byte { return [32]byte(c.Hashes[len(c.Hashes)-1]) }

// Wrap puts a Solana transaction in the Cosmos envelope that CometBFT gossips.
func (c *Chain) Wrap(solanaTx []byte) []byte {
	raw, err := svm.Envelope(c.App.TxConfig, solanaTx)
	if err != nil {
		c.T.Fatal(err)
	}
	return raw
}

// Unwrap is Wrap's inverse. It returns the Solana transaction inside a Cosmos
// envelope.
func (c *Chain) Unwrap(raw []byte) []byte {
	c.T.Helper()
	tx, err := c.App.TxConfig.TxDecoder()(raw)
	if err != nil {
		c.T.Fatal(err)
	}
	msgs := tx.GetMsgs()
	if len(msgs) != 1 {
		c.T.Fatalf("a wrapped transaction carries %d messages", len(msgs))
	}
	msg, ok := msgs[0].(*svm.MsgSVMTransaction)
	if !ok {
		c.T.Fatalf("a wrapped transaction carries %T", msgs[0])
	}
	return msg.Transaction
}

// Check runs one wrapped transaction through CheckTx, as the mempool does.
func (c *Chain) Check(raw []byte, recheck bool) *abci.ResponseCheckTx {
	c.T.Helper()
	kind := abci.CheckTxType_New
	if recheck {
		kind = abci.CheckTxType_Recheck
	}
	res, err := c.App.CheckTx(&abci.RequestCheckTx{Tx: raw, Type: kind})
	if err != nil {
		c.T.Fatal(err)
	}
	return res
}

// Propose runs ProcessProposal over wrapped transactions, as a validator that
// did not build the block does.
func (c *Chain) Propose(txs ...[]byte) abci.ResponseProcessProposal_ProposalStatus {
	c.T.Helper()
	res, err := c.App.ProcessProposal(&abci.RequestProcessProposal{
		Height: c.Height + 1,
		Time:   c.blockTime(c.Height + 1),
		Txs:    txs,
	})
	if err != nil {
		c.T.Fatal(err)
	}
	return res.Status
}

// Pack runs PrepareProposal over wrapped transactions in arrival order.
func (c *Chain) Pack(maxTxBytes int64, txs ...[]byte) [][]byte {
	c.T.Helper()
	res, err := c.App.PrepareProposal(&abci.RequestPrepareProposal{
		Height:     c.Height + 1,
		Time:       c.blockTime(c.Height + 1),
		MaxTxBytes: maxTxBytes,
		Txs:        txs,
	})
	if err != nil {
		c.T.Fatal(err)
	}
	return res.Txs
}

// Account reads an account from committed state.
func (c *Chain) Account(pk [32]byte) (ffi.Account, bool) {
	v := c.App.CommitMultiStore().GetKVStore(c.App.Stores.Accounts[svm.Shard(pk)]).Get(pk[:])
	if v == nil {
		return ffi.Account{}, false
	}
	a, err := ffi.DecodeAccount(v)
	if err != nil {
		c.T.Fatal(err)
	}
	return a, true
}

// Lamports returns the committed balance of pk, or 0 for no account.
func (c *Chain) Lamports(pk [32]byte) uint64 {
	a, _ := c.Account(pk)
	return a.Lamports
}

// Supply sums every account in every shard, and fails the test if any account
// sits outside the shard its pubkey names.
func (c *Chain) Supply() uint64 {
	c.T.Helper()
	var total uint64
	ms := c.App.CommitMultiStore()
	for shard, k := range c.App.Stores.Accounts {
		it := ms.GetKVStore(k).Iterator(nil, nil)
		for ; it.Valid(); it.Next() {
			if len(it.Key()) != 32 || svm.Shard([32]byte(it.Key())) != shard {
				c.T.Fatalf("key %x is in store svm%d, want svm%d", it.Key(), shard, it.Key()[0]>>5)
			}
			a, err := ffi.DecodeAccount(it.Value())
			if err != nil {
				c.T.Fatal(err)
			}
			total += a.Lamports
		}
		it.Close()
	}
	return total
}

// Meta is one account an instruction names.
type Meta struct {
	Key      [32]byte
	Signer   bool
	Writable bool
}

// Instruction is one uncompiled instruction.
type Instruction struct {
	Program  [32]byte
	Accounts []Meta
	Data     []byte
}

// compiled is the account list a message carries: the keys in Solana's four
// role classes, the header that counts them, and each key's index.
type compiled struct {
	keys   [][32]byte
	header [3]byte
	index  map[[32]byte]byte
}

// compile orders the accounts as every version of the format does: writable
// signers with the payer first, readonly signers, writable non-signers,
// readonly non-signers. A key in loaded is reached through a lookup table and
// is not listed. Only v0 does this.
func compile(payer [32]byte, loaded map[[32]byte]int, ixs []Instruction) compiled {
	type slot struct{ signer, writable bool }
	order := [][32]byte{payer}
	flags := map[[32]byte]*slot{payer: {true, true}}
	add := func(k [32]byte, signer, writable bool) {
		if _, ok := loaded[k]; ok {
			return
		}
		if f, ok := flags[k]; ok {
			f.signer, f.writable = f.signer || signer, f.writable || writable
			return
		}
		flags[k] = &slot{signer, writable}
		order = append(order, k)
	}
	for _, ix := range ixs {
		for _, m := range ix.Accounts {
			add(m.Key, m.Signer, m.Writable)
		}
		add(ix.Program, false, false)
	}
	var c compiled
	for _, class := range []slot{{true, true}, {true, false}, {false, true}, {false, false}} {
		for _, k := range order {
			if *flags[k] == class {
				c.keys = append(c.keys, k)
				switch class {
				case slot{true, true}:
					c.header[0]++
				case slot{true, false}:
					c.header[0]++
					c.header[1]++
				case slot{false, false}:
					c.header[2]++
				}
			}
		}
	}
	c.index = map[[32]byte]byte{}
	for i, k := range c.keys {
		c.index[k] = byte(i)
	}
	return c
}

// sign signs the message with the key of each account the header counts as a
// signer, in the order the message lists them.
func sign(m []byte, c compiled, payer ed25519.PrivateKey, signers []ed25519.PrivateKey) []byte {
	byKey := map[[32]byte]ed25519.PrivateKey{Pub(payer): payer}
	for _, s := range signers {
		byKey[Pub(s)] = s
	}
	var out []byte
	for _, k := range c.keys[:c.header[0]] {
		out = append(out, ed25519.Sign(byKey[k], m)...)
	}
	return out
}

// Tx compiles a legacy message with payer first and signs it with signers,
// in the order the message lists its signers.
func Tx(blockhash [32]byte, payer ed25519.PrivateKey, signers []ed25519.PrivateKey, ixs ...Instruction) []byte {
	c := compile(Pub(payer), nil, ixs)
	m := append([]byte(nil), c.header[:]...)
	m = shortU16(m, len(c.keys))
	for _, k := range c.keys {
		m = append(m, k[:]...)
	}
	m = append(m, blockhash[:]...)
	m = shortU16(m, len(ixs))
	for _, ix := range ixs {
		m = append(m, c.index[ix.Program])
		m = shortU16(m, len(ix.Accounts))
		for _, a := range ix.Accounts {
			m = append(m, c.index[a.Key])
		}
		m = shortU16(m, len(ix.Data))
		m = append(m, ix.Data...)
	}
	tx := shortU16(nil, int(c.header[0]))
	tx = append(tx, sign(m, c, payer, signers)...)
	return append(tx, m...)
}

// Lookup names the accounts that a v0 message reaches through one address
// lookup table. An entry's Key is what the table holds at Index. The builder
// needs both, because a v0 message carries table indexes and the compiled
// instructions carry account positions.
type Lookup struct {
	Table    [32]byte
	Writable []LookupEntry
	Readonly []LookupEntry
}

// LookupEntry is one address and its position in the table.
type LookupEntry struct {
	Index uint8
	Key   [32]byte
}

// TxV0 compiles a v0 message. An instruction account that a lookup names is
// reached through the table, not listed with the static keys.
//
// The account index order is Anza's: the static keys, then every lookup's
// writable addresses in lookup order, then every lookup's readonly addresses.
func TxV0(blockhash [32]byte, payer ed25519.PrivateKey, signers []ed25519.PrivateKey, lookups []Lookup, ixs ...Instruction) []byte {
	loadedAt := map[[32]byte]int{}
	var writable, readonly [][32]byte
	for _, l := range lookups {
		for _, e := range l.Writable {
			writable = append(writable, e.Key)
		}
	}
	for _, l := range lookups {
		for _, e := range l.Readonly {
			readonly = append(readonly, e.Key)
		}
	}
	for i, k := range writable {
		loadedAt[k] = i
	}
	for i, k := range readonly {
		loadedAt[k] = len(writable) + i
	}

	c := compile(Pub(payer), loadedAt, ixs)
	at := func(k [32]byte) byte {
		if i, ok := loadedAt[k]; ok {
			return byte(len(c.keys) + i)
		}
		return c.index[k]
	}

	m := []byte{0x80} // version 0
	m = append(m, c.header[:]...)
	m = shortU16(m, len(c.keys))
	for _, k := range c.keys {
		m = append(m, k[:]...)
	}
	m = append(m, blockhash[:]...)
	m = shortU16(m, len(ixs))
	for _, ix := range ixs {
		m = append(m, c.index[ix.Program])
		m = shortU16(m, len(ix.Accounts))
		for _, a := range ix.Accounts {
			m = append(m, at(a.Key))
		}
		m = shortU16(m, len(ix.Data))
		m = append(m, ix.Data...)
	}
	m = shortU16(m, len(lookups))
	for _, l := range lookups {
		m = append(m, l.Table[:]...)
		m = shortU16(m, len(l.Writable))
		for _, e := range l.Writable {
			m = append(m, e.Index)
		}
		m = shortU16(m, len(l.Readonly))
		for _, e := range l.Readonly {
			m = append(m, e.Index)
		}
	}

	tx := shortU16(nil, int(c.header[0]))
	tx = append(tx, sign(m, c, payer, signers)...)
	return append(tx, m...)
}

// V1Config is the compute budget that a V1 message carries inline, not in
// ComputeBudget instructions. A zero field is absent from the message, with
// the same effect. The runtime reads an absent priority fee and compute unit
// limit as zero, and an absent heap size as the 32 KiB default. A transaction
// that asks for no compute units cannot execute, so a usable V1 message
// sets at least ComputeUnitLimit and LoadedAccountsDataSize.
type V1Config struct {
	PriorityFee            uint64
	ComputeUnitLimit       uint32
	LoadedAccountsDataSize uint32
	HeapSize               uint32
}

// mask is the bitmask that says which config values follow the addresses.
// The priority fee owns two bits and both must be set.
func (c V1Config) mask() uint32 {
	var mask uint32
	if c.PriorityFee != 0 {
		mask |= 0b11
	}
	if c.ComputeUnitLimit != 0 {
		mask |= 0b100
	}
	if c.LoadedAccountsDataSize != 0 {
		mask |= 0b1000
	}
	if c.HeapSize != 0 {
		mask |= 0b10000
	}
	return mask
}

// TxV1 compiles a V1 message (SIMD-0385). The format is not v0 with another
// prefix:
//   - the compute budget moves into the message as a mask and its values
//   - the two length prefixes are plain bytes, not ShortU16
//   - each instruction has a fixed four-byte header, apart from its payload
//   - the signatures follow the message with no length prefix, and the header
//     counts them
//
// V1 carries no address lookup tables, so TxV1 takes no lookups.
func TxV1(blockhash [32]byte, cfg V1Config, payer ed25519.PrivateKey, signers []ed25519.PrivateKey, ixs ...Instruction) []byte {
	c := compile(Pub(payer), nil, ixs)

	m := []byte{0x81} // V1_PREFIX, MESSAGE_VERSION_PREFIX | 1
	m = append(m, c.header[:]...)
	m = binary.LittleEndian.AppendUint32(m, cfg.mask())
	m = append(m, blockhash[:]...) // the lifetime specifier
	m = append(m, byte(len(ixs)), byte(len(c.keys)))
	for _, k := range c.keys {
		m = append(m, k[:]...)
	}
	if cfg.PriorityFee != 0 {
		m = binary.LittleEndian.AppendUint64(m, cfg.PriorityFee)
	}
	if cfg.ComputeUnitLimit != 0 {
		m = binary.LittleEndian.AppendUint32(m, cfg.ComputeUnitLimit)
	}
	if cfg.LoadedAccountsDataSize != 0 {
		m = binary.LittleEndian.AppendUint32(m, cfg.LoadedAccountsDataSize)
	}
	if cfg.HeapSize != 0 {
		m = binary.LittleEndian.AppendUint32(m, cfg.HeapSize)
	}
	for _, ix := range ixs {
		m = append(m, c.index[ix.Program], byte(len(ix.Accounts)))
		m = binary.LittleEndian.AppendUint16(m, uint16(len(ix.Data)))
	}
	for _, ix := range ixs {
		for _, a := range ix.Accounts {
			m = append(m, c.index[a.Key])
		}
		m = append(m, ix.Data...)
	}
	return append(m, sign(m, c, payer, signers)...)
}

// shortU16 appends Solana's compact length prefix.
func shortU16(b []byte, n int) []byte {
	for {
		e := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			return append(b, e)
		}
		b = append(b, e|0x80)
	}
}

// Transfer is SystemInstruction::Transfer.
func Transfer(from ed25519.PrivateKey, to [32]byte, lamports uint64) Instruction {
	data := binary.LittleEndian.AppendUint32(nil, 2)
	return Instruction{
		Program:  svm.SystemProgram,
		Accounts: []Meta{{Pub(from), true, true}, {to, false, true}},
		Data:     binary.LittleEndian.AppendUint64(data, lamports),
	}
}

// Writable is a writable account that does not sign.
func Writable(k [32]byte) Meta { return Meta{Key: k, Writable: true} }

// Readonly is a readonly account that does not sign.
func Readonly(k [32]byte) Meta { return Meta{Key: k} }

// Signer is a signing account, writable or not.
func Signer(k [32]byte, writable bool) Meta {
	return Meta{Key: k, Signer: true, Writable: writable}
}

// ManifestPrograms returns genesis programs[] built from
// genesis/manifest.json, as the genesis tooling builds it.
func ManifestPrograms(t testing.TB) []svm.GenesisProgram {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	m, err := genesis.Read(filepath.Join(filepath.Dir(file), "..", "..", "genesis"))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]svm.GenesisProgram, len(m.Programs))
	for i, p := range m.Programs {
		out[i] = svm.GenesisProgram{Address: p.Address, Loader: p.Loader, SHA256: p.SHA256, ELF: p.ELF}
	}
	return out
}

// Snapshot takes a state sync snapshot at the committed height and returns it
// with its chunks, as a machine that serves snapshots does.
func (c *Chain) Snapshot() (snapshottypes.Snapshot, [][]byte) {
	c.T.Helper()
	m := c.App.SnapshotManager()
	if m == nil {
		c.T.Fatal("the chain was started without a snapshot store")
	}
	snapshot, err := m.Create(uint64(c.Height))
	if err != nil {
		c.T.Fatal(err)
	}
	chunks := make([][]byte, 0, snapshot.Chunks)
	for i := uint32(0); i < snapshot.Chunks; i++ {
		chunk, err := m.LoadChunk(snapshot.Height, snapshot.Format, i)
		if err != nil {
			c.T.Fatal(err)
		}
		chunks = append(chunks, chunk)
	}
	return *snapshot, chunks
}

// Sync joins a machine by state sync. It restores the snapshot into a fresh
// set of databases through the snapshot manager, then opens the app again
// over the result. It returns an error and does not fail the test.
func Sync(t testing.TB, snapshot snapshottypes.Snapshot, chunks [][]byte) (*Chain, error) {
	t.Helper()
	joiner := &Chain{T: t, db: dbm.NewMemDB(), dedup: dbm.NewMemDB(), history: dbm.NewMemDB(), tokens: dbm.NewMemDB(), Height: int64(snapshot.Height)}
	store, err := snapshots.NewStore(dbm.NewMemDB(), t.TempDir())
	if err != nil {
		return nil, err
	}
	joiner.options = []func(*baseapp.BaseApp){baseapp.SetSnapshot(store, snapshottypes.NewSnapshotOptions(1000, 2))}
	joiner.open()

	m := joiner.App.SnapshotManager()
	if err := m.Restore(snapshot); err != nil {
		return nil, err
	}
	for _, chunk := range chunks {
		if _, err := m.RestoreChunk(chunk); err != nil {
			return nil, err
		}
	}
	// A restored machine reopens next, and the reopen compares the dedup
	// commitment.
	a, err := app.New(log.NewNopLogger(), joiner.db,
		app.Local{Dedup: joiner.dedup, History: joiner.history, Tokens: joiner.tokens},
		t.TempDir(), append([]func(*baseapp.BaseApp){baseapp.SetChainID("svm-test")}, joiner.options...)...)
	if err != nil {
		return nil, err
	}
	joiner.App = a
	for h := int64(1); h <= joiner.Height; h++ {
		hash := HashAt(h)
		joiner.Hashes = append(joiner.Hashes, hash[:])
	}
	return joiner, nil
}

// Join joins a machine by state sync as CometBFT drives it. A fresh app gets
// the snapshot offer and applies its chunks through ABCI. Join does not
// restart the app, because CometBFT goes straight from the restore to block
// sync. Sync is the same restore followed by a restart.
func Join(t testing.TB, snapshot snapshottypes.Snapshot, chunks [][]byte) (*Chain, error) {
	t.Helper()
	joiner := &Chain{T: t, db: dbm.NewMemDB(), dedup: dbm.NewMemDB(), history: dbm.NewMemDB(), tokens: dbm.NewMemDB(), Height: int64(snapshot.Height)}
	store, err := snapshots.NewStore(dbm.NewMemDB(), t.TempDir())
	if err != nil {
		return nil, err
	}
	joiner.options = []func(*baseapp.BaseApp){baseapp.SetSnapshot(store, snapshottypes.NewSnapshotOptions(1000, 2))}
	joiner.open()

	offered, err := snapshot.ToABCI()
	if err != nil {
		return nil, err
	}
	offer, err := joiner.App.OfferSnapshot(&abci.RequestOfferSnapshot{Snapshot: &offered})
	if err != nil {
		return nil, err
	}
	if offer.Result != abci.ResponseOfferSnapshot_ACCEPT {
		return nil, fmt.Errorf("snapshot at %d offered: %v", snapshot.Height, offer.Result)
	}
	for i, chunk := range chunks {
		res, err := joiner.App.ApplySnapshotChunk(&abci.RequestApplySnapshotChunk{Index: uint32(i), Chunk: chunk})
		if err != nil {
			return nil, err
		}
		if res.Result != abci.ResponseApplySnapshotChunk_ACCEPT {
			return nil, fmt.Errorf("chunk %d of the snapshot at %d: %v", i, snapshot.Height, res.Result)
		}
	}
	for h := int64(1); h <= joiner.Height; h++ {
		hash := HashAt(h)
		joiner.Hashes = append(joiner.Hashes, hash[:])
	}
	return joiner, nil
}

// Cosmos signs a Cosmos transaction from one of the chain's operators, with no
// fee. SVM transactions do not use this path, because MsgSVMTransaction
// carries Solana's own signatures and never reaches the ante handler.
func (c *Chain) Cosmos(signer ed25519.PrivateKey, msgs ...sdk.Msg) []byte {
	c.T.Helper()
	return c.CosmosFee(signer, nil, msgs...)
}

// CosmosFee is Cosmos with fee offered for the transaction's 1,000,000 gas.
func (c *Chain) CosmosFee(signer ed25519.PrivateKey, fee sdk.Coins, msgs ...sdk.Msg) []byte {
	c.T.Helper()
	return c.cosmos(signer, fee, 1_000_000, msgs)
}

// CosmosGas is Cosmos with the given gas and no fee.
func (c *Chain) CosmosGas(signer ed25519.PrivateKey, gas uint64, msgs ...sdk.Msg) []byte {
	c.T.Helper()
	return c.cosmos(signer, nil, gas, msgs)
}

func (c *Chain) cosmos(signer ed25519.PrivateKey, fee sdk.Coins, gas uint64, msgs []sdk.Msg) []byte {
	c.T.Helper()
	priv := &cometed25519.PrivKey{Key: signer}
	address := sdk.AccAddress(priv.PubKey().Address())
	ctx := c.App.NewUncachedContext(true, cmtproto.Header{Height: c.Height})
	account := c.App.Accounts.GetAccount(ctx, address)
	if account == nil {
		c.T.Fatalf("no account for %s", address)
	}

	b := c.App.TxConfig.NewTxBuilder()
	if err := b.SetMsgs(msgs...); err != nil {
		c.T.Fatal(err)
	}
	b.SetGasLimit(gas)
	b.SetFeeAmount(fee)
	sig := signing.SignatureV2{
		PubKey:   priv.PubKey(),
		Data:     &signing.SingleSignatureData{SignMode: signing.SignMode_SIGN_MODE_DIRECT},
		Sequence: account.GetSequence(),
	}
	if err := b.SetSignatures(sig); err != nil {
		c.T.Fatal(err)
	}
	data := authsigning.SignerData{
		ChainID:       "svm-test",
		AccountNumber: account.GetAccountNumber(),
		Sequence:      account.GetSequence(),
		PubKey:        priv.PubKey(),
		Address:       address.String(),
	}
	bytes, err := authsigning.GetSignBytesAdapter(ctx, c.App.TxConfig.SignModeHandler(),
		signing.SignMode_SIGN_MODE_DIRECT, data, b.GetTx())
	if err != nil {
		c.T.Fatal(err)
	}
	signature, err := priv.Sign(bytes)
	if err != nil {
		c.T.Fatal(err)
	}
	sig.Data = &signing.SingleSignatureData{SignMode: signing.SignMode_SIGN_MODE_DIRECT, Signature: signature}
	if err := b.SetSignatures(sig); err != nil {
		c.T.Fatal(err)
	}
	raw, err := c.App.TxConfig.TxEncoder()(b.GetTx())
	if err != nil {
		c.T.Fatal(err)
	}
	return raw
}

// Deliver finalizes and commits a block of encoded transactions, Cosmos or
// SVM, in the order given.
func (c *Chain) Deliver(txs ...[]byte) ([]*abci.ExecTxResult, []byte) {
	c.T.Helper()
	c.Height++
	hash := HashAt(c.Height)
	c.Hashes = append(c.Hashes, hash[:])
	res, err := c.App.FinalizeBlock(&abci.RequestFinalizeBlock{
		Height: c.Height,
		Time:   c.blockTime(c.Height),
		Hash:   hash[:],
		Txs:    txs,
	})
	if err != nil {
		c.T.Fatalf("height %d: %v", c.Height, err)
	}
	commit, err := c.App.Commit()
	if err != nil {
		c.T.Fatal(err)
	}
	c.RetainHeight = max(c.RetainHeight, commit.RetainHeight)
	c.lamportsAgree()
	return res.TxResults, res.AppHash
}
