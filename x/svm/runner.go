package svm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	errorsmod "cosmossdk.io/errors"
	"cosmossdk.io/log/v2"

	abci "github.com/cometbft/cometbft/abci/types"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"

	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
)

// AttributeKeySignature carries a transaction's base58 first signature.
const AttributeKeySignature = "signature"

// AttributeKeyGroup carries the conflict group the transaction ran in. The
// runner publishes the scheduler's width, so the parallelism can be checked.
// Events are outside the results hash, so this is a metric and not consensus.
const AttributeKeyGroup = "group"

// statusNotProcessed is the engine result status of a transaction that the
// block did not process. 0 is success, and 1 is a processed failure.
const statusNotProcessed = 2

// Runner is the sdk.TxRunner that sends a block's SVM transactions through the
// engine's three phases once per block. Cosmos transactions go through
// deliverTx one by one, as in the SDK's default runner.
type Runner struct {
	Stores Stores
	Decode sdk.TxDecoder
	// Dedup is the replay set. Nil disables it. Only a test that compares app
	// hashes with and without the set does that.
	Dedup *Dedup
	// Tokens is the machine-local token owner index. Nil on a machine that
	// serves no RPC.
	Tokens *TokenIndex
	// Feed publishes the block write set to the notifier. A nil feed, or one
	// that no subscriber holds, costs the block nothing.
	Feed *Feed
	// History is the machine-local transaction record. Nil disables it.
	History *History
	// State returns the latest committed state and its height, or nil before
	// the first block commits. The token index rebuilds from it.
	State func() (storetypes.MultiStore, uint64, error)
	// Next returns the next block's context, or nil before the first block
	// commits. CheckTx admits against it. CheckTx arrives on the mempool
	// connection and can run beside FinalizeBlock, so it reads a branch of a
	// versioned view, not the block being built.
	Next func() (storetypes.MultiStore, *Next, error)
	// Log takes the error line for each transaction that phase 1 leaves
	// unprocessed. Nil logs nothing.
	Log log.Logger

	// wait joins the sidecar writes the last Run started (Wait).
	wait func() error
}

// Admission failures, reported to the mempool. The codes are above the engine
// statuses that a finalized block reports under the same codespace.
var (
	ErrAdmission         = errorsmod.Register(ModuleName, 100, "transaction rejected")
	ErrBlockhashNotFound = errorsmod.Register(ModuleName, 101, "blockhash not found")
	ErrAlreadyProcessed  = errorsmod.Register(ModuleName, 102, "transaction already processed")
	ErrUnpayable         = errorsmod.Register(ModuleName, 103, "fee payer cannot pay")
	ErrHost              = errorsmod.Register(ModuleName, 105, "this machine cannot admit transactions")
)

var _ sdk.TxRunner = &Runner{}

// Run delivers the Cosmos transactions as it decodes them, then executes the
// SVM transactions as one engine block. The two never share a store. Lamports
// are not a bank denom, and gate flips happen at begin block. So their
// relative order does not change state, and each result lands at its
// transaction's index.
func (r *Runner) Run(ctx context.Context, ms storetypes.MultiStore, txs [][]byte, deliverTx sdk.DeliverTxFunc) ([]*abci.ExecTxResult, error) {
	// The results that ProcessProposal kept for this height are spent when
	// the height ends, whether the block carries an SVM transaction or not.
	defer ffi.EndHeight()
	// A block finalized and never committed through App.Commit left its
	// writes running; the next one reads what they write.
	if err := r.Wait(); err != nil {
		return nil, err
	}
	results := make([]*abci.ExecTxResult, len(txs))
	var svmTxs [][]byte
	var svmIndex []int
	for i, raw := range txs {
		if solana, ok := Enveloped(raw); ok {
			svmTxs = append(svmTxs, solana)
			svmIndex = append(svmIndex, i)
			continue
		}
		tx, err := r.Decode(raw)
		if err != nil {
			results[i] = sdkerrors.ResponseExecTxResultWithEvents(sdkerrors.ErrTxDecode, 0, 0, nil, false)
			continue
		}
		if m, ok := svmMsg(tx); ok {
			svmTxs = append(svmTxs, m.Transaction)
			svmIndex = append(svmIndex, i)
			continue
		}
		results[i] = deliverTx(raw, tx, nil, i, nil)
	}
	// The begin-block writer put this block's height in Clock and its hash in
	// svmmeta. The runner reads both from state, not from a context that the
	// SDK takes before it sets the header on finalize state.
	clock, err := r.Stores.accountData(ms, ClockID)
	if err != nil {
		return nil, err
	}
	now := clockOf(clock.Data)
	height, blockTime := now.Slot, now.UnixTimestamp
	// This block has reached the machine, empty or not. getMaxShredInsertSlot
	// reads this mark, and receivedSignature fires on the same event.
	r.Feed.Reach(height)
	var spent [][64]byte
	var records []Record
	var writes []AccountWrite
	// The lamports created outside the engine in this block, before any
	// transaction. They are the feature accounts that a gate flip made.
	delta := Lamports{Added: r.Stores.TakeMinted()}
	sysvars := r.sysvarWrites(ms, height)
	if len(svmTxs) == 0 {
		r.Feed.Put(height, sysvars, nil)
	} else {
		svmResults, recs, w, moved, err := r.executeBlock(ms, height, svmTxs, svmIndex, sysvars)
		if err != nil {
			return nil, err
		}
		writes = w
		delta.Added += moved.Added
		delta.Removed += moved.Removed
		for j, i := range svmIndex {
			results[i] = svmResults[j]
		}
		for _, rec := range recs {
			rec.Index = uint32(svmIndex[rec.Index])
			rec.BlockTime = blockTime
			records = append(records, rec)
			spent = append(spent, rec.dedupKey)
		}
	}
	r.Stores.countTransactions(ms, uint64(len(records)))
	// What a block writes to the three machine-local databases goes on its own
	// goroutine. It lands while the shards flush and save, which leave cores
	// idle at the published minimum. Everything that the app hash reads is
	// computed here first.
	var sidecars []func() error
	if r.History != nil {
		hash := ms.GetKVStore(r.Stores.Meta).Get(MetaBlockhashKey)
		if len(hash) != 32 {
			return nil, fmt.Errorf("svm: block hash missing at height %d", height)
		}
		blockHash := [32]byte(hash)
		sidecars = append(sidecars, atHeight("history", height, func() error {
			if err := r.History.Write(height, blockHash, blockTime, records); err != nil {
				return err
			}
			historyIndexBytes.Set(float64(r.History.Bytes()))
			historyHorizon.Set(float64(r.History.FirstAvailableBlock() - 1))
			return nil
		}))
	}
	// The token index is a pure function of current account state. A marker
	// that is not the previous height means this machine did not maintain it.
	// The cause can be a first start, a state-sync join, a deleted index, a
	// version bump, or a crash between finalizing a block and committing it.
	// One rule covers all of them. Both paths take the write set in hand, so
	// the ordinary path adds no account-store read. The rebuild runs off this
	// path, from committed state.
	if r.Tokens != nil {
		write, err := r.Tokens.Follow(height, writes, delta, r.Stores, ms, r.State)
		if err != nil {
			return nil, fmt.Errorf("svm: token index at height %d: %w", height, err)
		}
		if write != nil {
			sidecars = append(sidecars, atHeight("token index", height, write))
		}
	}
	if r.Dedup != nil {
		save, err := r.Dedup.Spend(height, spent)
		if err != nil {
			return nil, fmt.Errorf("svm: dedup set at height %d: %w", height, err)
		}
		dedupEntries.Set(float64(r.Dedup.Entries()))
		// The commitment is inside the commit tree, so two machines that
		// disagree about the set disagree about the app hash at this block, not
		// when an adversary chooses to probe for it.
		r.Stores.SetDedupCommitment(ms, r.Dedup.Commitment(height))
		sidecars = append(sidecars, atHeight("dedup set", height, save))
	}
	r.wait = Background(sidecars...)
	return results, nil
}

// Wait returns when the sidecar writes of the block that Run last finalized
// have landed. App.Commit calls it after the shards save and before
// BaseApp.Commit moves the committed height. So no reader bounded by that
// height sees a block that the sidecars do not hold, and the sidecars are on
// disk before the state is. Their error is the block's error, and a panic in
// them is raised again here.
func (r *Runner) Wait() error {
	wait := r.wait
	if wait == nil {
		return nil
	}
	r.wait = nil
	return wait()
}

// atHeight names a sidecar write's failure as the block's.
func atHeight(what string, height uint64, write func() error) func() error {
	return func() error {
		if err := write(); err != nil {
			return fmt.Errorf("svm: %s at height %d: %w", what, height, err)
		}
		return nil
	}
}

// Background runs writes in order on their own goroutine and returns the
// join. The join returns the first error, and raises a panic again on the
// caller's goroutine, as eachShard does.
func Background(writes ...func() error) func() error {
	done := make(chan struct{})
	var err error
	var failure any
	go func() {
		defer close(done)
		defer func() { failure = recover() }()
		for _, w := range writes {
			if err = w(); err != nil {
				return
			}
		}
	}()
	return func() error {
		<-done
		if failure != nil {
			panic(failure)
		}
		return err
	}
}

// CheckTx admits a transaction that carries one MsgSVMTransaction to
// CometBFT's mempool. It does not route it to a message handler, because the
// transaction only executes through Run. CheckTx refuses an SVM transaction in
// a non-canonical envelope here, in this module's codespace, not as the SDK's
// parse error. Anything else takes the SDK's own path.
func (r *Runner) CheckTx(runTx sdk.RunTx, req *abci.RequestCheckTx) (*abci.ResponseCheckTx, error) {
	if solana, ok := Enveloped(req.Tx); ok {
		return r.admit(solana, req.Type)
	}
	tx, err := r.Decode(req.Tx)
	if errors.Is(err, ErrEnvelope) {
		return sdkerrors.ResponseCheckTxWithEvents(err, 0, 0, nil, false), nil
	}
	if err == nil {
		if m, ok := svmMsg(tx); ok {
			return r.admit(m.Transaction, req.Type)
		}
	}
	gas, result, events, err := runTx(req.Tx, nil)
	if err != nil {
		return sdkerrors.ResponseCheckTxWithEvents(err, gas.GasWanted, gas.GasUsed, events, false), nil
	}
	return &abci.ResponseCheckTx{GasWanted: int64(gas.GasWanted), GasUsed: int64(gas.GasUsed), Log: result.Log, Data: result.Data, Events: result.Events}, nil
}

// admit handles both CheckTx types. New parses, sanitizes and verifies the
// signatures. When every later check passes, the engine keeps that result for
// phase 1. Recheck parses and sanitizes again, but skips the signature check
// and keeps no result, because signature validity is a pure function of bytes
// that never change. Both admit for the next block, under its gate set. Both
// read its blockhash window, its replay set and its state half
// (Stores.Admissible). The state half checks the lookup tables, the durable
// nonce, and whether the fee payer can pay what the transaction declares. Each
// check reads state as that block will.
//
// The state half goes stale as soon as the payer spends elsewhere. So it is a
// filter, not a guarantee, and execution still decides. It drops a bid that
// the block would never process. The pool ranks by declared fee. Without the
// filter, an unchecked payer, a table that does not resolve, or a nonce that
// its authority did not sign outranks every funded transaction. Such a
// transaction fills blocks for nothing. Agave's leader never allows this. It
// runs the same checks before it buffers a transaction, or it records only the
// transactions that pass them.
func (r *Runner) admit(tx []byte, kind abci.CheckTxType) (*abci.ResponseCheckTx, error) {
	ms, next, err := r.Next()
	if err != nil {
		return host(err), nil
	}
	if ms == nil {
		// Nothing is committed, so no blockhash can be in the window yet.
		admissionRejected.WithLabelValues(reasonBlockhash).Inc()
		return sdkerrors.ResponseCheckTxWithEvents(ErrBlockhashNotFound, 0, 0, nil, false), nil
	}
	recheck := kind == abci.CheckTxType_Recheck
	var a ffi.Admission
	if recheck {
		a, err = ffi.CheckTx(tx, true, next.Height)
	} else {
		a, err = ffi.CheckTxNew(tx, next.Height)
	}
	if err != nil {
		if errors.Is(err, ffi.ErrRejected) {
			admissionRejected.WithLabelValues(reasonAdmission).Inc()
			return refused(ErrAdmission, err.Error()), nil
		}
		return host(err), nil
	}
	blockhash := a.Blockhash()
	fresh, err := r.Stores.BlockhashValid(ms, blockhash[:])
	if err != nil {
		return host(err), nil
	}
	if !fresh && !a.NonceCandidate {
		admissionRejected.WithLabelValues(reasonBlockhash).Inc()
		return sdkerrors.ResponseCheckTxWithEvents(ErrBlockhashNotFound, 0, 0, nil, false), nil
	}
	if r.Dedup != nil && r.Dedup.Has(a.DedupKey) {
		admissionRejected.WithLabelValues(reasonAlreadyProcessed).Inc()
		return sdkerrors.ResponseCheckTxWithEvents(ErrAlreadyProcessed, 0, 0, nil, false), nil
	}
	switch err := r.Stores.Admissible(ms, next.Height, tx, a, !fresh); {
	case errors.Is(err, ffi.ErrRejected):
		admissionRejected.WithLabelValues(reasonAdmission).Inc()
		return refused(ErrAdmission, err.Error()), nil
	case errors.Is(err, ffi.ErrNonce):
		admissionRejected.WithLabelValues(reasonBlockhash).Inc()
		return refused(ErrBlockhashNotFound, err.Error()), nil
	case errors.Is(err, ffi.ErrUnpayable):
		admissionRejected.WithLabelValues(reasonUnpayable).Inc()
		return refused(ErrUnpayable, err.Error()), nil
	case err != nil:
		return host(err), nil
	}
	// The signatures passed, and every check after them passed. So the engine
	// keeps the result for the block that carries this transaction.
	if !recheck {
		ffi.RecordVerified(a.Key)
	}
	return &abci.ResponseCheckTx{GasWanted: int64(a.Units)}, nil
}

// alarm counts and logs each transaction that phase 1 did not admit. Phase 1
// leaves it unprocessed: status 2, no fee, no writes, no record, and its
// dedup key unspent. ProcessProposal refuses a block that carries one. So
// the block passed without this node's vote, or this node and the others
// disagree on a check, for example a gate. It is an alarm, never a halt, and
// the block stands. The log line names the height, the transaction's index in
// the block, the reason and the engine's exact error. It never holds
// transaction bytes or signatures. index maps the engine's order to the
// block's.
func (r *Runner) alarm(handle, height uint64, index []int) error {
	dropped, err := ffi.BlockDropped(handle)
	if err != nil {
		return fmt.Errorf("svm: phase 1 drops at height %d: %w", height, err)
	}
	for _, d := range dropped {
		reason := droppedAdmission
		if d.Err == "SignatureFailure" {
			reason = droppedSignature
		}
		blockAdmissionFailed.WithLabelValues(reason).Inc()
		if r.Log != nil {
			r.Log.Error("svm: block transaction not admitted", "height", height, "index", index[d.Index],
				"reason", reason, "error", d.Err)
		}
	}
	return nil
}

// host answers a CheckTx that failed because of this machine, not the
// transaction. The cause is a panic, an unconfigured engine, or a store that
// does not read. That is a host condition, not a transaction condition, but
// host answers it as a refusal and does not return an error. CometBFT's pool
// panics on an error from CheckTx and drops the peer that gossiped the
// transaction. A machine that state-synced without a configured engine would
// then lose every peer. The block path, which halts on a host condition, still
// stops the machine.
func host(err error) *abci.ResponseCheckTx {
	admissionRejected.WithLabelValues(reasonHost).Inc()
	return refused(ErrHost, err.Error())
}

// refused returns the CheckTx answer when err refuses a transaction for the
// reason that desc gives. It has the codespace, code and log that
// sdkerrors.ResponseCheckTxWithEvents writes for err.Wrap(desc) with debug
// off. Wrap also takes a stack trace. That log never prints it, and it was a
// third of the cost of a refused recheck.
func refused(err *errorsmod.Error, desc string) *abci.ResponseCheckTx {
	return &abci.ResponseCheckTx{Codespace: err.Codespace(), Code: err.ABCICode(), Log: desc + ": " + err.Error()}
}

// svmMsg reports whether tx is one SVM transaction in a Cosmos envelope.
func svmMsg(tx sdk.Tx) (*MsgSVMTransaction, bool) {
	msgs := tx.GetMsgs()
	if len(msgs) != 1 {
		return nil, false
	}
	m, ok := msgs[0].(*MsgSVMTransaction)
	return m, ok
}

// sysvarWrites copies, for the feed, the sysvars that the begin-block writer
// rewrote for height: Clock always, and SlotHashes and RecentBlockhashes when
// there is a parent to prepend (writeSysvars). Agave notifies an account
// subscription on each at every slot, and an ephemeral rollup validator tracks
// the base chain's slot by these notifications. It returns nil with no
// subscriber. With subscribers, it copies only the sysvars that a
// subscription watches.
func (r *Runner) sysvarWrites(ms storetypes.MultiStore, height uint64) []AccountValue {
	if !r.Feed.Wants() {
		return nil
	}
	ids := [][32]byte{ClockID}
	if height > 1 {
		ids = append(ids, SlotHashesID, RecentBlockhashesID)
	}
	out := make([]AccountValue, 0, len(ids))
	for _, id := range ids {
		if v := r.Stores.GetAccount(ms, id); v != nil && r.Feed.Watches(id, v) {
			out = append(out, AccountValue{Pubkey: id, Value: append([]byte(nil), v...)})
		}
	}
	return out
}

// executeBlock runs the block's SVM transactions through the engine. It
// returns their results, a record for each processed transaction (indexed
// within txs), the token writes, and what the lamports of the written accounts
// gained and lost. published, which holds the block's sysvar writes, goes to
// the feed with the engine's stored set appended. index is the position of
// each transaction in the block, which the alarm's log line names.
func (r *Runner) executeBlock(ms storetypes.MultiStore, height uint64, txs [][]byte, index []int, published []AccountValue) ([]*abci.ExecTxResult, []Record, []AccountWrite, Lamports, error) {
	hash := ms.GetKVStore(r.Stores.Meta).Get(MetaBlockhashKey)
	if len(hash) != 32 {
		return nil, nil, nil, Lamports{}, fmt.Errorf("svm: block hash missing at height %d", height)
	}

	w := ffi.NewArenaWriter(ffi.KindPlanReq, 0)
	for _, tx := range txs {
		w.PushSized(tx)
	}
	handle, plan, err := ffi.Plan(height, [32]byte(hash), w.Finish())
	if err != nil {
		return nil, nil, nil, Lamports{}, fmt.Errorf("svm: plan at height %d: %w", height, err)
	}
	defer ffi.Release(handle)
	defer plan.Free()
	keys, err := ffi.NewArenaReader(plan.Bytes(), ffi.KindPlanResp)
	if err != nil {
		return nil, nil, nil, Lamports{}, err
	}
	if err := r.alarm(handle, height, index); err != nil {
		return nil, nil, nil, Lamports{}, err
	}
	// The plan response ends with one 64-byte dedup key per transaction.
	loadEnd := keys.Len() - len(txs)
	dedupKeys := make([][64]byte, len(txs))
	already := make([]byte, len(txs))
	for i := range txs {
		rec := keys.Record(loadEnd + i)
		if len(rec) < 64 {
			return nil, nil, nil, Lamports{}, ffi.ErrBadArena
		}
		dedupKeys[i] = [64]byte(rec[:64])
		if r.Dedup != nil && r.Dedup.Has(dedupKeys[i]) {
			already[i] = 1
		}
	}

	loadList := make([][32]byte, 0, loadEnd-int(keys.Aux))
	for i := int(keys.Aux); i < loadEnd; i++ {
		k, err := keys.Key32(i)
		if err != nil {
			return nil, nil, nil, Lamports{}, err
		}
		loadList = append(loadList, k)
	}

	// Phase 2 runs only when the plan names lookup table accounts. A zero
	// count means the block is legacy-only. Phase 2 returns the load list
	// again, with the resolved addresses in their first-use places. So phase
	// 3 also gets the accounts that a table named.
	if keys.Aux > 0 {
		if loadList, err = r.resolve(ms, handle, keys, height); err != nil {
			return nil, nil, nil, Lamports{}, err
		}
	}

	ew := ffi.NewArenaWriter(ffi.KindExecuteReq, handle)
	ew.Push(already)
	// The length of the load list and the time to read it are the two numbers
	// that show a naming attack as an attack, not as an unexplained block
	// time.
	pushStart := time.Now()
	// What was read of every account pushed. A written account's lamport
	// change and the token index entry that it replaces are then known
	// without another read.
	pre := make(map[[32]byte]Loaded, len(loadList))
	dropped, arena := r.Stores.PushAccounts(ew, ms, loadList, ArenaBytes, pre)
	pushed := time.Since(pushStart)
	// The last record names every key the ceiling dropped, so the engine never
	// mistakes one for an account that does not exist.
	cut := make([]byte, 0, 32*len(dropped))
	for _, k := range dropped {
		cut = append(cut, k[:]...)
	}
	ew.Push(cut)
	exec, err := ffi.Execute(handle, ew)
	if err != nil {
		return nil, nil, nil, Lamports{}, fmt.Errorf("svm: execute at height %d: %w", height, err)
	}
	defer exec.Free()
	out, err := ffi.NewArenaReader(exec.Bytes(), ffi.KindExecuteResp)
	if err != nil {
		return nil, nil, nil, Lamports{}, err
	}

	n := int(out.Aux)
	results := make([]*abci.ExecTxResult, n)
	var records []Record
	for i := range results {
		res, err := out.ResultAt(i)
		if err != nil {
			return nil, nil, nil, Lamports{}, err
		}
		results[i] = &abci.ExecTxResult{Code: res.Status, Codespace: codespace(res.Status)}
		if res.Status == statusNotProcessed {
			continue
		}
		meta, err := out.MetaAt(n + i)
		if err != nil {
			return nil, nil, nil, Lamports{}, err
		}
		records = append(records, Record{
			Height:    height,
			Index:     uint32(i),
			Tx:        txs[i],
			Signature: res.Signature,
			Status:    res.Status,
			Fee:       res.Fee,
			Units:     res.ComputeUnits,
			Price:     res.ComputeUnitPrice,
			Meta:      meta,
			dedupKey:  dedupKeys[i],
		})
		// The module and signature go on the result, for event subscribers.
		// Events are outside the results hash.
		results[i].Events = []abci.Event{{
			Type: sdk.EventTypeMessage,
			Attributes: []abci.EventAttribute{
				{Key: sdk.AttributeKeyModule, Value: ModuleName, Index: true},
				{Key: AttributeKeySignature, Value: base58.Encode(res.Signature[:]), Index: true},
				{Key: AttributeKeyGroup, Value: strconv.FormatUint(uint64(res.Group), 10)},
			},
		}}
	}
	// signatureSubscribe's receivedSignature fires here. The block has reached
	// this machine and has not committed yet.
	r.Feed.Received(height, func() [][64]byte {
		sigs := make([][64]byte, len(records))
		for i, rec := range records {
			sigs[i] = rec.Signature
		}
		return sigs
	})
	var writes []AccountWrite
	var moved Lamports
	// One atomic load decides whether this block copies anything at all. If
	// it does, it copies only the accounts that a subscription watches.
	// value points into the response arena, which defer exec.Free() releases
	// before the notifier runs, so the published value must be a copy.
	watched := r.Feed.Wants()
	for i := 2 * n; i < out.Len(); i++ {
		pk, value, err := out.StoredAt(i)
		if err != nil {
			return nil, nil, nil, Lamports{}, err
		}
		r.Stores.SetAccount(ms, pk, value)
		moved.Move(pre[pk].Lamports, ffi.Lamports(value))
		if r.Tokens != nil {
			w := Classify(pk, value)
			w.Before = pre[pk].Token
			writes = append(writes, w)
		}
		if watched && r.Feed.Watches(pk, value) {
			published = append(published, AccountValue{Pubkey: pk, Value: append([]byte(nil), value...)})
		}
	}
	r.Feed.Put(height, published, records)
	var units uint64
	for _, rec := range records {
		units += rec.Units
	}
	observeBlock(len(loadList), len(dropped), arena, pushed, units)
	return results, records, writes, moved, nil
}

// resolve runs phase 2 and returns the block's whole load list, with the
// addresses its lookup tables named in their first-use places. Record 0 of the
// request is the SlotHashes sysvar. Go wrote it in this block, so it needs no
// IAVL read of its own. Records 1 onward are the table accounts, in the order
// the plan response named them. A table with no account is left out, and the
// engine drops the transactions that named it.
func (r *Runner) resolve(ms storetypes.MultiStore, handle uint64, keys *ffi.ArenaReader, height uint64) ([][32]byte, error) {
	slotHashes, err := r.Stores.accountData(ms, SlotHashesID)
	if err != nil {
		return nil, fmt.Errorf("svm: resolve at height %d: %w", height, err)
	}
	rw := ffi.NewArenaWriter(ffi.KindResolveReq, handle)
	rw.Push(slotHashes.Data)
	tables := make([][32]byte, int(keys.Aux))
	for i := range tables {
		if tables[i], err = keys.Key32(i); err != nil {
			return nil, err
		}
	}
	r.Stores.PushTables(rw, ms, tables, ArenaBytes)
	buf, err := ffi.Resolve(handle, rw.Finish())
	if err != nil {
		return nil, fmt.Errorf("svm: resolve at height %d: %w", height, err)
	}
	defer buf.Free()
	out, err := ffi.NewArenaReader(buf.Bytes(), ffi.KindResolveResp)
	if err != nil {
		return nil, err
	}
	list := make([][32]byte, 0, out.Len())
	for i := 0; i < out.Len(); i++ {
		k, err := out.Key32(i)
		if err != nil {
			return nil, err
		}
		list = append(list, k)
	}
	return list, nil
}

func codespace(status uint32) string {
	if status == 0 {
		return ""
	}
	return ModuleName
}
