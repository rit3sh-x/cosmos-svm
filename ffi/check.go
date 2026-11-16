package ffi

/*
#include <stdint.h>

int      svm_check_tx(uint64_t height, const uint8_t *tx, size_t tx_len, uint8_t mode,
                      uint8_t **out, size_t *out_len);
int      svm_check_committed(uint64_t height, uint64_t fee, const uint8_t *tx, size_t tx_len,
                             const uint8_t *req, size_t req_len,
                             const uint8_t *tables, size_t tables_len,
                             uint8_t **out, size_t *out_len);
int      svm_resolved_admission(uint64_t height, const uint8_t *tx, size_t tx_len,
                                const uint8_t *req, size_t req_len,
                                uint8_t **out, size_t *out_len);
void     svm_block_limits(uint64_t *units, uint64_t *allocated_bytes, uint64_t *program_text);
uint64_t svm_max_writable_account_units(void);
uint64_t svm_signature_verifications(void);
int      svm_record_verified(const uint8_t *key);
void     svm_signature_results_cap(size_t entries);
void     svm_signature_results_clear_for_tests(void);
int      svm_proposal_signatures(const uint8_t *req, size_t req_len, uint64_t *out_failed);
void     svm_signature_results_end_height(void);
int      svm_block_dropped(uint64_t handle, uint8_t **out, size_t *out_len);
*/
import "C"

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unsafe"
)

// ErrRejected reports a transaction that the engine refuses to admit. The
// transaction does not sanitize, exceeds the size or instruction limits, has
// signatures that do not verify, or has a compute budget that does not
// configure. In the state half, its lookup tables do not resolve, or they name
// an account twice. The wrapped text is the Solana TransactionError.
var ErrRejected = errors.New("svm engine: transaction rejected")

// ErrUnpayable and ErrNonce are the two refusals of the state half of
// admission. Each wraps the Solana TransactionError.
var (
	ErrUnpayable = errors.New("svm engine: fee payer cannot pay")
	ErrNonce     = errors.New("svm engine: durable nonce does not verify")
)

// Admission is the verdict of svm_check_tx: the cost-model charge for the
// transaction and the facts that the state-dependent checks need.
type Admission struct {
	// Units is GetGas(), the seven-term cost-model cost. For a message that
	// names a lookup table, it is the cost before the tables resolve.
	// Resolution can only raise it, by the deploy term. A block counts such a
	// message at the ResolvedAdmission cost.
	Units uint64
	// AllocatedBytes is what the transaction adds to the block's account data
	// size delta.
	AllocatedBytes uint64
	// NonceCandidate is true when the first instruction advances Nonce, a
	// nonce account. Then a blockhash past the window can still be valid.
	NonceCandidate bool
	Nonce          [32]byte
	Payer          [32]byte
	// DedupKey is the replay key: the recent blockhash, then the message
	// hash. Its first 32 bytes are the blockhash that the age check reads.
	DedupKey [64]byte
	// Fee is base plus priority, the numerator of the ranking key. The engine
	// computes it beside the cost, so ordering the pool costs one crossing per
	// candidate, not two.
	Fee uint64
	// Tables are the lookup tables that a v0 message names, in message order.
	// The state half reads them.
	Tables [][32]byte
	// Writable are the accounts that the transaction writes. The per-account
	// limit counts them. For a message that names a table it is empty, and
	// ResolvedAdmission supplies it.
	Writable [][32]byte
	// Key is the transaction's signature-result key. Only CheckTxNew returns
	// one, for RecordVerified. Go never computes a key.
	Key [32]byte
}

// Blockhash is the transaction's recent blockhash.
func (a Admission) Blockhash() [32]byte { return [32]byte(a.DedupKey[:32]) }

// CheckTx is the stateless half of admission. It is pure and reentrant and
// uses no handle, so it runs on the mempool connection beside an open block.
// In hashOnly mode it skips ed25519, which makes recheck and ProcessProposal
// cheap. The hash, the cost and the dedup key do not change.
//
// Go does expiry and dedup, from committed state. CheckTx admits for the block
// at height, under the gate set that ConfigureNext named for it.
func CheckTx(tx []byte, hashOnly bool, height uint64) (Admission, error) {
	var mode C.uint8_t
	if hashOnly {
		mode = 1
	}
	return check(tx, mode, height)
}

// CheckTxNew is CheckTx in full, for CheckTx New. A check that passes puts
// the transaction's key in the engine's pending slot and in Admission.Key.
// RecordVerified takes it after Go's own checks pass. Only CheckTx New may
// call it, because the slot holds one key and the ABCI connection runs one
// CheckTx at a time.
func CheckTxNew(tx []byte, height uint64) (Admission, error) {
	return check(tx, 2, height)
}

// RecordVerified keeps the result of the last check that CheckTxNew made. The
// engine keeps key only when its pending slot holds it, and it empties the
// slot in both cases. It reports whether the slot held key.
func RecordVerified(key [32]byte) bool {
	return C.svm_record_verified((*C.uint8_t)(unsafe.Pointer(&key[0]))) == 1
}

// SetSignatureResultsCap sets how many results the pool tier holds. The
// oldest goes out first. svmd sets two times CometBFT's mempool.size. It is
// machine-local, because the store changes how long a block takes and never
// its result.
func SetSignatureResultsCap(entries int) {
	C.svm_signature_results_cap(C.size_t(entries))
}

// ProposalSignatures is ProcessProposal's signature check, after its cheap
// checks. txs are the proposal's SVM transactions in block order.
// The engine checks each one's signatures unless it holds a result for its
// exact bytes, stops at the first that fails, and keeps the results of the
// checks that passed until the height ends. It returns the index in txs of the
// first transaction whose signatures fail, or -1.
func ProposalSignatures(txs [][]byte) (int, error) {
	// Each record is the address and length of a transaction. The transaction
	// bytes stay pinned for the call, so a block crosses without a copy, as
	// accounts do.
	w := NewArenaWriter(KindProposalReq, 0)
	w.Grow(len(txs), 16*len(txs))
	defer w.Unpin()
	var rec [16]byte
	for _, tx := range txs {
		if len(tx) == 0 {
			return 0, ErrBadInput
		}
		w.pins.Pin(&tx[0])
		binary.LittleEndian.PutUint64(rec[0:8], uint64(uintptr(unsafe.Pointer(&tx[0]))))
		binary.LittleEndian.PutUint64(rec[8:16], uint64(len(tx)))
		w.Push(rec[:])
	}
	p, n := reqPtr(w.Finish())
	var failed C.uint64_t
	if err := statusErr(C.svm_proposal_signatures(p, n, &failed)); err != nil {
		return 0, err
	}
	if uint64(failed) == math.MaxUint64 {
		return -1, nil
	}
	return int(failed), nil
}

// EndHeight tells the engine that the height's FinalizeBlock is over, so it
// forgets the results that ProcessProposal kept for the height.
func EndHeight() {
	C.svm_signature_results_end_height()
}

// Dropped is a transaction phase 1 did not admit: its index among the block's
// SVM transactions, and the engine's error.
type Dropped struct {
	Index int
	Err   string
}

// BlockDropped reads phase 1's drops in the open block, in block order.
func BlockDropped(handle uint64) ([]Dropped, error) {
	var out *C.uint8_t
	var outLen C.size_t
	if err := statusErr(C.svm_block_dropped(C.uint64_t(handle), &out, &outLen)); err != nil {
		return nil, err
	}
	buf := &Buf{ptr: out, n: outLen}
	defer buf.Free()
	r, err := NewArenaReader(buf.Bytes(), KindDroppedResp)
	if err != nil {
		return nil, err
	}
	dropped := make([]Dropped, r.Len())
	for i := range dropped {
		rec := r.Record(i)
		if len(rec) < 4 {
			return nil, ErrBadArena
		}
		// A record is padded to 8 bytes, and the error is text.
		dropped[i] = Dropped{Index: int(binary.LittleEndian.Uint32(rec)), Err: string(bytes.TrimRight(rec[4:], "\x00"))}
	}
	return dropped, nil
}

// ClearSignatureResultsForTests empties the engine's store of signature
// results, as a restart does. The engine is process-global. A test of several
// machines in one process uses it to model the store of each machine. Only a
// test may call it.
func ClearSignatureResultsForTests() {
	C.svm_signature_results_clear_for_tests()
}

func check(tx []byte, mode C.uint8_t, height uint64) (Admission, error) {
	var a Admission
	var out *C.uint8_t
	var outLen C.size_t
	p, n := reqPtr(tx)
	if err := statusErr(C.svm_check_tx(C.uint64_t(height), p, n, mode, &out, &outLen)); err != nil {
		return a, err
	}
	buf := &Buf{ptr: out, n: outLen}
	defer buf.Free()
	r, err := NewArenaReader(buf.Bytes(), KindCheckResp)
	if err != nil {
		return a, err
	}
	if r.Len() == 0 || len(r.Record(0)) < 192 {
		return a, ErrBadArena
	}
	head := r.Record(0)
	if binary.LittleEndian.Uint32(head[0:4]) != 0 {
		return a, refusal(r, ErrRejected)
	}
	a.NonceCandidate = binary.LittleEndian.Uint32(head[4:8]) != 0
	a.Units = binary.LittleEndian.Uint64(head[8:16])
	a.AllocatedBytes = binary.LittleEndian.Uint64(head[16:24])
	a.DedupKey = [64]byte(head[24:88])
	a.Fee = binary.LittleEndian.Uint64(head[88:96])
	a.Payer = [32]byte(head[96:128])
	a.Nonce = [32]byte(head[128:160])
	a.Key = [32]byte(head[160:192])
	if int(r.Aux) > r.Len()-1 {
		return a, ErrBadArena
	}
	for i := 1; i < r.Len(); i++ {
		key, err := r.Key32(i)
		if err != nil {
			return a, err
		}
		if i <= int(r.Aux) {
			a.Tables = append(a.Tables, key)
		} else {
			a.Writable = append(a.Writable, key)
		}
	}
	return a, nil
}

// Resolved is a message that names lookup tables as the block counts it:
// its cost over the resolved message and the accounts it writes.
type Resolved struct {
	Units          uint64
	AllocatedBytes uint64
	Writable       [][32]byte
}

// ResolvedAdmission prices tx, a message that names lookup tables, as the
// block at height will. It resolves the tables against slotHashes and tables,
// as LookupAddresses does. refused is the error text when the tables do not
// resolve or the resolved message names an account twice. The block does not
// process such a transaction.
func ResolvedAdmission(height uint64, tx, slotHashes []byte, tables map[[32]byte][]byte) (r Resolved, refused string, err error) {
	var out *C.uint8_t
	var outLen C.size_t
	tp, tn := reqPtr(tx)
	rp, rn := reqPtr(resolveRequest(slotHashes, tables))
	if err := statusErr(C.svm_resolved_admission(C.uint64_t(height), tp, tn, rp, rn, &out, &outLen)); err != nil {
		return r, "", err
	}
	buf := &Buf{ptr: out, n: outLen}
	defer buf.Free()
	rd, err := NewArenaReader(buf.Bytes(), KindCheckResp)
	if err != nil || rd.Len() == 0 || len(rd.Record(0)) < 4 {
		return r, "", ErrBadArena
	}
	head := rd.Record(0)
	if binary.LittleEndian.Uint32(head[0:4]) != 0 {
		if rd.Len() < 2 {
			return r, "", ErrBadArena
		}
		return r, string(bytes.TrimRight(rd.Record(1), "\x00")), nil
	}
	if len(head) < 20 {
		return r, "", ErrBadArena
	}
	r.Units = binary.LittleEndian.Uint64(head[4:12])
	r.AllocatedBytes = binary.LittleEndian.Uint64(head[12:20])
	for i := 1; i < rd.Len(); i++ {
		key, err := rd.Key32(i)
		if err != nil {
			return Resolved{}, "", err
		}
		r.Writable = append(r.Writable, key)
	}
	return r, "", nil
}

// Committed is what the state half of admission reads from committed state.
// Each account is its stored value, nil when it does not exist.
type Committed struct {
	// Rent is the Rent sysvar's data.
	Rent  []byte
	Payer []byte
	// Expired means the blockhash is past the window. The transaction can then
	// use it only as the durable nonce that Nonce, its nonce account, holds.
	Expired bool
	Nonce   []byte
	// Height is the block the transaction is admitted for. SlotHashes is the
	// SlotHashes data that block will hold. Tables holds the stored value of
	// each table the message names, by key, and leaves out a table that does
	// not exist. That block's phase 2 resolves the message against these.
	// They are read only when the message names a table.
	Height     uint64
	SlotHashes []byte
	Tables     map[[32]byte][]byte
}

// CheckCommitted is the state half of admission. It checks that the lookup
// tables resolve and that the resolved message names no account twice. It
// checks that an expired blockhash is the nonce its nonce account holds, and
// that the nonce authority signed. It checks that the fee payer can pay the
// fee in a. Like CheckTx, it reads no state itself. The caller reads c.
func CheckCommitted(tx []byte, a Admission, c Committed) error {
	w := NewArenaWriter(KindCommittedReq, 0)
	w.Push(c.Rent)
	account := func(key [32]byte, value []byte) {
		if value == nil {
			w.Push(nil)
			return
		}
		w.PushStored(&key, value)
	}
	account(a.Payer, c.Payer)
	if c.Expired {
		w.SetAux(1)
		blockhash := a.Blockhash()
		w.Push(blockhash[:])
		account(a.Nonce, c.Nonce)
	}
	var tables []byte
	if len(a.Tables) > 0 {
		tables = resolveRequest(c.SlotHashes, c.Tables)
	}
	var out *C.uint8_t
	var outLen C.size_t
	tp, tn := reqPtr(tx)
	rp, rn := reqPtr(w.Finish())
	lp, ln := reqPtr(tables)
	if err := statusErr(C.svm_check_committed(C.uint64_t(c.Height), C.uint64_t(a.Fee), tp, tn, rp, rn, lp, ln, &out, &outLen)); err != nil {
		return err
	}
	buf := &Buf{ptr: out, n: outLen}
	defer buf.Free()
	r, err := NewArenaReader(buf.Bytes(), KindCheckResp)
	if err != nil {
		return err
	}
	if r.Len() == 0 || len(r.Record(0)) < 4 {
		return ErrBadArena
	}
	switch binary.LittleEndian.Uint32(r.Record(0)) {
	case 0:
		return nil
	case 1:
		return refusal(r, ErrUnpayable)
	case 2:
		return refusal(r, ErrNonce)
	default:
		return refusal(r, ErrRejected)
	}
}

// refusal wraps kind with the TransactionError a check response carries in
// record 1.
func refusal(r *ArenaReader, kind error) error {
	reason := "unknown"
	if r.Len() > 1 {
		reason = string(bytes.TrimRight(r.Record(1), "\x00"))
	}
	return fmt.Errorf("%w: %s", kind, reason)
}

// BlockLimits is the engine's block budget: total cost units, total account
// allocation, and the distinct executable program text a block can reference.
// Handlers read it here and keep no Go copy, so each constant lives once, in
// the engine.
//
// The third limit differs from the other two. ProcessProposal checks units and
// allocation, and its parse reads no state. It cannot check program text,
// because the program size comes from the accounts. FinalizeBlock enforces the
// program text limit, and it fails a transaction, not a block.
func BlockLimits() (units, allocatedBytes, programText uint64) {
	var u, b, t C.uint64_t
	C.svm_block_limits((*C.uint64_t)(unsafe.Pointer(&u)), (*C.uint64_t)(unsafe.Pointer(&b)),
		(*C.uint64_t)(unsafe.Pointer(&t)))
	return uint64(u), uint64(b), uint64(t)
}

// MaxWritableAccountUnits is the most units that the transactions in one block
// that write one account can spend together. Both proposal handlers read it
// here, beside BlockLimits. FinalizeBlock enforces the same constant in the
// engine.
func MaxWritableAccountUnits() uint64 {
	return uint64(C.svm_max_writable_account_units())
}

// SignatureVerifications counts the transactions that the engine has verified
// with ed25519 since startup. A test uses it to prove that recheck verifies
// nothing.
func SignatureVerifications() uint64 {
	return uint64(C.svm_signature_verifications())
}
