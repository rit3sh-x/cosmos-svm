// Package ffi is the Go side of the boundary to the Rust SVM execution engine.
package ffi

/*
#cgo LDFLAGS: -L${SRCDIR}/../target/release -lsvm_engine -lm -ldl -lpthread
#include <stdlib.h>
#include <stdint.h>

int   svm_engine_noop(void);
char *svm_engine_version(void);
void  svm_engine_digest(uint8_t *out);
void  svm_engine_threads(size_t threads);
void  svm_engine_cache_bytes(uint64_t bytes);
uint64_t svm_program_cache_bytes(void);
void  svm_block_program_text_for_tests(uint64_t bytes);
void  svm_engine_string_free(char *ptr);
int   svm_engine_panic_test(void);
void  svm_engine_unconfigure_for_tests(void);

int  svm_block_plan(uint64_t height, const uint8_t *blockhash,
                    const uint8_t *req, size_t req_len,
                    uint8_t **out, size_t *out_len, uint64_t *out_handle);
int  svm_block_resolve(uint64_t handle, const uint8_t *req, size_t req_len,
                       uint8_t **out, size_t *out_len);
int  svm_block_execute(uint64_t handle, const uint8_t *req, size_t req_len,
                       uint8_t **out, size_t *out_len);
void svm_block_release(uint64_t handle);
int  svm_engine_configure(const uint8_t *req, size_t req_len);
int  svm_transaction_keys(const uint8_t *tx, size_t tx_len, uint8_t **out, size_t *out_len);
int  svm_engine_stats(uint8_t **out, size_t *out_len);
int  svm_engine_configure_next(uint64_t height, const uint8_t *req, size_t req_len);
int  svm_simulate(uint64_t height, const uint8_t *blockhash, const uint8_t *own, uint8_t flags,
                  const uint8_t *req, size_t req_len, uint8_t **out, size_t *out_len);
int  svm_fee_for_message(const uint8_t *msg, size_t msg_len,
                         uint64_t *out_fee, uint8_t *out_known);
int  svm_lookup_tables(const uint8_t *tx, size_t tx_len, uint8_t **out, size_t *out_len);
int  svm_lookup_addresses(uint64_t height, const uint8_t *tx, size_t tx_len,
                          const uint8_t *req, size_t req_len, uint8_t **out, size_t *out_len);
int  svm_decode_transaction(const uint8_t *tx, size_t tx_len, const uint8_t *loaded, size_t loaded_len,
                            uint8_t **out, size_t *out_len);
uint32_t svm_feature_set_id(void);
char *svm_deserialize_error(uint8_t kind, const uint8_t *bytes, size_t len);
void svm_buf_free(uint8_t *ptr, size_t len);
*/
import "C"

import (
	"bytes"
	"encoding/binary"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"sync/atomic"
	"unsafe"
)

// ErrEnginePanic reports that the engine panicked and the Rust side contained
// the unwind. The process is intact, and no undefined behavior occurred.
//
// The caller must HALT svmd. Do not turn this into a transaction failure. A
// panic reports a host condition (memory-map exhaustion, a poisoned lock, host
// stack depth), not a transaction condition. Host conditions differ between
// validators that run identical input. A validator that records a failed
// transaction here disagrees with a validator that succeeded on the same block,
// and the error handling itself makes a fork.
//
// A halt is a liveness failure for one validator. A made-up result is a safety
// failure for the chain.
var ErrEnginePanic = errors.New("svm engine panicked (unwind contained; node must halt)")

// ErrNullResult reports that the engine returned no value where one was required.
var ErrNullResult = errors.New("svm engine returned a null result")

// Noop crosses the boundary and returns. It measures the crossing cost.
func Noop() int {
	return int(C.svm_engine_noop())
}

// Version returns a string that proves solana-svm and solana-sbpf are linked
// and callable.
func Version() (string, error) {
	ptr := C.svm_engine_version()
	if ptr == nil {
		return "", ErrNullResult
	}
	defer C.svm_engine_string_free(ptr)
	return C.GoString(ptr), nil
}

// Digest is the engine's build digest: a SHA-256 over the engine source, the
// lockfile, both manifests, the toolchain pin and the release profile.
// Genesis declares it, and the startup assertion compares it with the digest
// that the chain adopted.
func Digest() [32]byte {
	var out [32]byte
	C.svm_engine_digest((*C.uint8_t)(unsafe.Pointer(&out[0])))
	return out
}

// SetThreads sets the number of worker threads that run a block's conflict
// groups. Zero means the machine's available parallelism.
func SetThreads(n int) {
	C.svm_engine_threads(C.size_t(n))
}

// SetProgramCacheBytes sets the byte budget of the program cache. Zero means
// unbounded. The default is 1.5 GiB of resident bytes. The budget is
// machine-local, because cache size is not consensus. An operator with more
// memory can raise it, and two machines with different
// budgets reach the same app hash.
func SetProgramCacheBytes(bytes uint64) {
	C.svm_engine_cache_bytes(C.uint64_t(bytes))
}

// SetBlockProgramTextForTests sets the block's program text budget. That
// budget is consensus, so it is not an operator setting. A block needs 400 MB
// of account data to reach the real 400 MB limit, because the count is clamped
// to what each program stores. Only a test may call it.
func SetBlockProgramTextForTests(bytes uint64) {
	C.svm_block_program_text_for_tests(C.uint64_t(bytes))
}

// UnconfigureForTests clears the engine's feature set, as in a process that
// has not configured it yet. The engine is process-global. Without this call, a
// test of a machine that joins by state sync inherits the configuration of the
// chain that served the snapshot. Only a test may call it.
func UnconfigureForTests() {
	C.svm_engine_unconfigure_for_tests()
}

// ProgramCacheBytes is the size that the cache is accounted as holding: what
// its entries keep resident, the pc tables and emitted code of the JIT and the
// ELF and rodata copies. It is not the power-of-two buckets that the JIT
// reserves. The value includes the caches of the roots that simulations run
// on, and it counts each entry once. SetProgramCacheBytes bounds this value.
func ProgramCacheBytes() uint64 {
	return uint64(C.svm_program_cache_bytes())
}

// PanicTest asks the engine to panic on purpose. It proves that the boundary
// contains the unwind, so the panic does not become undefined behavior.
// Containment is a memory-safety mechanism, not a recovery strategy.
// ErrEnginePanic says what the caller must do next.
func PanicTest() error {
	if int(C.svm_engine_panic_test()) == -1 {
		return ErrEnginePanic
	}
	return nil
}

// Engine status codes, mirroring engine/src/lib.rs.
const (
	svmOK              = 0
	svmErrPanic        = -1
	svmErrBadHandle    = -2
	svmErrBadInput     = -3
	svmErrBusy         = -4
	svmErrSysvar       = -5
	svmErrUnconfigured = -6
)

var (
	// ErrBadHandle reports that the handle does not name the open block: a stale
	// plan, or none open.
	ErrBadHandle = errors.New("svm engine: handle does not name the open block")
	// ErrBadInput reports a malformed request arena or transactions that do not
	// deserialise.
	ErrBadInput = errors.New("svm engine: malformed request")
	// ErrBusy reports that a block is already open. This driver did not
	// release the previous one. It is a bug here, not a bad block.
	ErrBusy = errors.New("svm engine: a block is already open")
	// ErrMissingSysvar reports a sysvar cache slot that is empty after the
	// fill. svmd must halt, because the sysvar writer or genesis is broken.
	ErrMissingSysvar = errors.New("svm engine: sysvar cache slot empty after fill (node must halt)")
	// ErrUnconfigured reports a block before Configure. svmd must halt.
	ErrUnconfigured = errors.New("svm engine: no feature set configured (node must halt)")
)

func statusErr(code C.int) error {
	switch int(code) {
	case svmOK:
		return nil
	case svmErrPanic:
		return ErrEnginePanic
	case svmErrBadHandle:
		return ErrBadHandle
	case svmErrBadInput:
		return ErrBadInput
	case svmErrBusy:
		return ErrBusy
	case svmErrSysvar:
		return ErrMissingSysvar
	case svmErrUnconfigured:
		return ErrUnconfigured
	default:
		return fmt.Errorf("svm engine: unknown status %d", int(code))
	}
}

// Buf is a response arena that the engine owns.
//
// Bytes aliases engine memory and does not copy it. A block's account payload
// is megabytes, and this format exists to avoid two copies of it per phase.
// Everything read from it, including Account.Data, is valid only until Free.
// Call Free from a defer, and copy anything that must outlive the call.
type Buf struct {
	ptr *C.uint8_t
	n   C.size_t
}

// Bytes returns the arena. It is invalid after Free.
func (b *Buf) Bytes() []byte {
	if b.ptr == nil || b.n == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(b.ptr)), int(b.n))
}

// Free releases the arena. It is idempotent.
func (b *Buf) Free() {
	if b.ptr != nil {
		C.svm_buf_free(b.ptr, b.n)
		b.ptr = nil
		b.n = 0
	}
}

// reqPtr borrows a request arena for the duration of one call.
//
// This is legal because the arena holds no unpinned pointers. cgo permits Go
// memory passed to C only when every Go pointer in it is pinned, and it forbids
// C code to keep the memory after the call. The engine never stores it.
func reqPtr(req []byte) (*C.uint8_t, C.size_t) {
	if len(req) == 0 {
		return nil, 0
	}
	return (*C.uint8_t)(unsafe.Pointer(&req[0])), C.size_t(len(req))
}

// Plan is phase 1. It hands the block's serialized transactions to the engine,
// one PushSized record each. It gets back every account key that must be
// loaded before execution.
//
// The Aux of the returned reader is the number of leading records that are
// address lookup table accounts. Zero means the block is legacy-only, and
// Resolve is skipped.
//
// blockhash is this block's CometBFT header hash. On success a block is open,
// and the caller MUST Release the handle.
func Plan(height uint64, blockhash [32]byte, req []byte) (handle uint64, buf *Buf, err error) {
	var out *C.uint8_t
	var outLen C.size_t
	var outHandle C.uint64_t
	p, n := reqPtr(req)
	code := C.svm_block_plan(C.uint64_t(height), (*C.uint8_t)(unsafe.Pointer(&blockhash[0])), p, n, &out, &outLen, &outHandle)
	if err := statusErr(code); err != nil {
		return 0, nil, err
	}
	return uint64(outHandle), &Buf{ptr: out, n: outLen}, nil
}

// Resolve is phase 2. It expands address lookup tables against the table
// accounts that Go loaded, and returns the additional keys to load.
func Resolve(handle uint64, req []byte) (*Buf, error) {
	var out *C.uint8_t
	var outLen C.size_t
	p, n := reqPtr(req)
	code := C.svm_block_resolve(C.uint64_t(handle), p, n, &out, &outLen)
	if err := statusErr(code); err != nil {
		return nil, err
	}
	return &Buf{ptr: out, n: outLen}, nil
}

// Execute is phase 3. It hands over every account the block touches, and gets
// back per-transaction results followed by proposed write sets. The accounts in
// the request are pinned records, unpinned when the engine returns.
//
// The engine never writes Cosmos state. The caller applies the write sets in
// deterministic transaction order.
func Execute(handle uint64, req *ArenaWriter) (*Buf, error) {
	defer req.Unpin()
	var out *C.uint8_t
	var outLen C.size_t
	p, n := reqPtr(req.Finish())
	code := C.svm_block_execute(C.uint64_t(handle), p, n, &out, &outLen)
	if code == svmErrSysvar {
		buf := &Buf{ptr: out, n: outLen}
		defer buf.Free()
		if r, err := NewArenaReader(buf.Bytes(), KindExecuteResp); err == nil && r.Len() == 1 {
			return nil, fmt.Errorf("%w: %s", ErrMissingSysvar, bytes.TrimRight(r.Record(0), "\x00"))
		}
		return nil, ErrMissingSysvar
	}
	if err := statusErr(code); err != nil {
		return nil, err
	}
	return &Buf{ptr: out, n: outLen}, nil
}

// Gate is one active feature gate and the height it activated at.
type Gate struct {
	ID     [32]byte
	Height uint64
}

// Configure hands the engine the active feature gates from svmmeta. It
// rebuilds the engine's root processor, so the program cache starts cold.
// Call it at startup and after InitGenesis, before any block.
func Configure(gates []Gate) error {
	p, n := reqPtr(gateRequest(gates))
	return statusErr(C.svm_engine_configure(p, n))
}

// ConfigureNext names gates as the gate set of the block at height, which is
// the block after the committed one. Simulations of that block run under it.
func ConfigureNext(height uint64, gates []Gate) error {
	p, n := reqPtr(gateRequest(gates))
	return statusErr(C.svm_engine_configure_next(C.uint64_t(height), p, n))
}

// gateRequest is a gate set as svm_engine_configure reads it: one record a
// gate, its feature pubkey and its activation height.
func gateRequest(gates []Gate) []byte {
	w := NewArenaWriter(KindConfigureReq, 0)
	for _, g := range gates {
		rec := append(g.ID[:0:0], g.ID[:]...)
		w.Push(binary.LittleEndian.AppendUint64(rec, g.Height))
	}
	return w.Finish()
}

// transactionKeyCalls counts TransactionKeys calls. A test uses it to prove
// that the block path makes none, as SignatureVerifications proves that recheck
// verifies none.
var transactionKeyCalls atomic.Uint64

// TransactionKeyCalls returns how many times TransactionKeys has crossed since
// startup.
func TransactionKeyCalls() uint64 { return transactionKeyCalls.Load() }

// TransactionKeys returns the load list of a transaction for simulation, in the
// order a block of that one transaction loads it. It also returns the static
// keys alone, in message order. The engine parses, and Go only loads and
// indexes.
//
// The history's address index uses the static keys for a record whose meta
// has no StaticKeys. The index does not use the load list, because its sysvar
// entries add nine index entries per transaction for addresses nobody asks
// about. Go does not derive the static keys itself.
func TransactionKeys(tx []byte) (load, static [][32]byte, err error) {
	transactionKeyCalls.Add(1)
	var out *C.uint8_t
	var outLen C.size_t
	p, n := reqPtr(tx)
	if err := statusErr(C.svm_transaction_keys(p, n, &out, &outLen)); err != nil {
		return nil, nil, err
	}
	buf := &Buf{ptr: out, n: outLen}
	defer buf.Free()
	r, err := NewArenaReader(buf.Bytes(), KindKeysResp)
	if err != nil {
		return nil, nil, err
	}
	if int(r.Aux) > r.Len() {
		return nil, nil, fmt.Errorf("svm: keys response claims %d load entries of %d", r.Aux, r.Len())
	}
	keys := make([][32]byte, r.Len())
	for i := range keys {
		if keys[i], err = r.Key32(i); err != nil {
			return nil, nil, err
		}
	}
	return keys[:r.Aux], keys[r.Aux:], nil
}

// Stats are the engine's machine-local counters. They accumulate for
// the life of the process, unlike Agave's, which reset on every new
// bank. The two group numbers describe the last block only.
type Stats struct {
	ProgramCacheHits          uint64
	ProgramCacheMisses        uint64
	ProgramCacheEvictions     uint64
	ProgramCacheOneHitWonders uint64
	ProgramCacheWaterLevel    uint64
	ProgramCacheBytes         uint64
	PrewarmMisses             uint64
	ConflictGroups            uint64
	WidestConflictGroup       uint64
	BlockProgramTextBytes     uint64
	// SignatureResults counts what the engine did with the results of
	// signature checks, by SignatureEvents and then SignatureHandlers.
	// SignatureResultsHeld is what each of SignatureTiers holds.
	SignatureResults     [len(SignatureEvents)][len(SignatureHandlers)]uint64
	SignatureResultsHeld [len(SignatureTiers)]uint64
	// SignatureVerifications is SignatureVerifications().
	SignatureVerifications uint64
}

// The labels of the signature-result counters, in the engine's order
// (engine/src/signatures.rs).
var (
	SignatureEvents   = [...]string{"hit", "miss", "written", "evicted", "forgotten"}
	SignatureHandlers = [...]string{"check_tx", "process_proposal", "finalize_block"}
	SignatureTiers    = [...]string{"pool", "block"}
)

// statsFixedFields is how many counters lead the stats record, ahead of the
// signature-result counters. The engine writes from the same boundary,
// SIGNATURE_RESULTS_AT in engine/src/stats.rs.
const statsFixedFields = 10

// EngineStats reads the counters. It has its own entry point and its own
// arena kind, so nothing it returns can reach a block result.
func EngineStats() (Stats, error) {
	var out *C.uint8_t
	var outLen C.size_t
	if err := statusErr(C.svm_engine_stats(&out, &outLen)); err != nil {
		return Stats{}, err
	}
	buf := &Buf{ptr: out, n: outLen}
	defer buf.Free()
	r, err := NewArenaReader(buf.Bytes(), KindStatsResp)
	if err != nil {
		return Stats{}, err
	}
	rec := r.Record(0)
	// The engine only appends fields, and this code reads them by position.
	// So it can still read an engine that adds one.
	results := len(SignatureEvents) * len(SignatureHandlers)
	want := (statsFixedFields + results + len(SignatureTiers)) * 8
	if len(rec) < want {
		return Stats{}, fmt.Errorf("svm: stats response is %d bytes, want %d", len(rec), want)
	}
	at := func(i int) uint64 { return binary.LittleEndian.Uint64(rec[i*8:]) }
	s := Stats{
		ProgramCacheHits:          at(0),
		ProgramCacheMisses:        at(1),
		ProgramCacheEvictions:     at(2),
		ProgramCacheOneHitWonders: at(3),
		ProgramCacheWaterLevel:    at(4),
		ProgramCacheBytes:         at(5),
		PrewarmMisses:             at(6),
		ConflictGroups:            at(7),
		WidestConflictGroup:       at(8),
		BlockProgramTextBytes:     at(9),
		SignatureVerifications:    SignatureVerifications(),
	}
	for i := range results {
		s.SignatureResults[i/len(SignatureHandlers)][i%len(SignatureHandlers)] = at(statsFixedFields + i)
	}
	for i := range SignatureTiers {
		s.SignatureResultsHeld[i] = at(statsFixedFields + results + i)
	}
	return s, nil
}

// Simulation is simulateTransaction's result. Status is 0 for success, 1 for
// a processed failure and 2 for not processed.
type Simulation struct {
	Status                 uint32
	LoadedAccountsDataSize uint32
	Units                  uint64
	Fee                    uint64
	ReturnProgram          *[32]byte
	ReturnData             []byte
	Logs                   []string
	// Err is the TransactionError as Agave's RPC renders it. The engine
	// renders it, and Go forwards it unparsed. It is nil on success.
	Err jsontext.Value
	// Message is the display text of the error, which Agave's preflight
	// failure quotes. It is empty on success.
	Message string
	// Writable and Readonly are the addresses the message's lookup tables
	// resolved to.
	Writable, Readonly [][32]byte
	// These fields are what Agave's simulation also returns: the balances
	// (nil when not collected), the token balances, the inner instructions
	// (nil unless the transaction executed), and the accounts that Simulate
	// was asked for, as they are after it ran, in message order.
	PreBalances, PostBalances           []uint64
	PreTokenBalances, PostTokenBalances []TokenBalance
	Inner                               [][]InnerInstruction
	Accounts                            []Account
}

// Simulate runs one transaction as the next block, height, on a child
// processor of the root that ConfigureNext named for it. It never touches an
// open block. blockhash is the committed block's hash, which
// replaceRecentBlockhash writes. own stands for the next block's own hash.
//
// Record 0 of req is the transaction. The accounts follow as pinned records,
// unpinned when the engine returns. The last record is the keys that the
// 64 MiB cut dropped. processed says that the replay set holds the
// transaction. The engine answers it after the age check, as Agave's status
// cache does.
//
// want holds the accounts whose state after the run Accounts carries. The
// engine copies out only these accounts, so it copies none for a preflight or
// a simulateTransaction without accounts. Simulate pushes them as the last
// record of the request.
func Simulate(height uint64, blockhash, own [32]byte, sigVerify, replaceBlockhash, processed bool, req *ArenaWriter, want ...[32]byte) (Simulation, error) {
	defer req.Unpin()
	keys := make([]byte, 0, 32*len(want))
	for _, k := range want {
		keys = append(keys, k[:]...)
	}
	req.Push(keys)
	var sim Simulation
	var flags C.uint8_t
	if sigVerify {
		flags |= 1
	}
	if replaceBlockhash {
		flags |= 2
	}
	if processed {
		flags |= 4
	}
	var out *C.uint8_t
	var outLen C.size_t
	p, n := reqPtr(req.Finish())
	code := C.svm_simulate(C.uint64_t(height), (*C.uint8_t)(unsafe.Pointer(&blockhash[0])), (*C.uint8_t)(unsafe.Pointer(&own[0])),
		flags, p, n, &out, &outLen)
	if err := statusErr(code); err != nil {
		return sim, err
	}
	buf := &Buf{ptr: out, n: outLen}
	defer buf.Free()
	return decodeSimulation(buf.Bytes())
}

// decodeSimulation reads the svm_simulate response (engine/src/lib.rs). Every
// variable-length field carries its length, so no field depends on trimmed
// padding.
func decodeSimulation(b []byte) (Simulation, error) {
	var sim Simulation
	r, err := NewArenaReader(b, KindSimulateResp)
	if err != nil || r.Len() < 2 {
		return sim, ErrBadArena
	}
	head := &cursor{b: r.Record(0)}
	sim.Status = head.u32()
	sim.LoadedAccountsDataSize = head.u32()
	sim.Units = head.u64()
	sim.Fee = head.u64()
	if e := head.bytes(int(head.u32())); len(e) > 0 {
		sim.Err = append(jsontext.Value(nil), e...)
	}
	sim.Message = string(head.bytes(int(head.u32())))
	writable, readonly := int(head.u32()), int(head.u32())
	for i := 0; i < writable+readonly; i++ {
		key := head.bytes(32)
		if key == nil {
			break
		}
		if i < writable {
			sim.Writable = append(sim.Writable, [32]byte(key))
		} else {
			sim.Readonly = append(sim.Readonly, [32]byte(key))
		}
	}
	if head.bad {
		return sim, ErrBadArena
	}
	m := &metaReader{b: head.b}
	if m.take1() == 1 {
		sim.PreBalances = m.balances()
		sim.PostBalances = m.balances()
	}
	sim.PreTokenBalances = m.tokenBalances()
	sim.PostTokenBalances = m.tokenBalances()
	if m.take1() == 1 {
		if sim.Inner = m.inner(); sim.Inner == nil {
			sim.Inner = [][]InnerInstruction{}
		}
	}
	for n := m.u32(); n > 0 && m.err == nil; n-- {
		key := m.take(32)
		fixed := m.take(accountHeadLen - pubkeyLen)
		if fixed == nil {
			break
		}
		data := m.take(int(binary.LittleEndian.Uint32(fixed[16:20])))
		a, err := DecodeAccount(append(append([]byte(nil), fixed...), data...))
		if err != nil {
			return sim, err
		}
		a.Pubkey = [32]byte(key)
		sim.Accounts = append(sim.Accounts, a)
	}
	if m.err != nil {
		return sim, ErrBadArena
	}
	if ret := r.Record(1); len(ret) >= 36 {
		program := [32]byte(ret[:32])
		size := int(binary.LittleEndian.Uint32(ret[32:36]))
		if len(ret) < 36+size {
			return sim, ErrBadArena
		}
		sim.ReturnProgram = &program
		sim.ReturnData = append([]byte(nil), ret[36:36+size]...)
	}
	for i := 2; i < r.Len(); i++ {
		line := &cursor{b: r.Record(i)}
		text := line.bytes(int(line.u32()))
		if line.bad {
			return sim, ErrBadArena
		}
		sim.Logs = append(sim.Logs, string(text))
	}
	return sim, nil
}

// cursor reads little-endian fields off the front of a record. A read past
// its end sets bad and returns nil or zero, so a decoder checks once.
type cursor struct {
	b   []byte
	bad bool
}

func (c *cursor) bytes(n int) []byte {
	if c.bad || n < 0 || n > len(c.b) {
		c.bad = true
		return nil
	}
	out := c.b[:n]
	c.b = c.b[n:]
	return out
}

func (c *cursor) u32() uint32 {
	if b := c.bytes(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (c *cursor) u64() uint64 {
	if b := c.bytes(8); b != nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

// Decoded is a stored transaction that the engine's decode kind takes apart,
// for the json and accounts renderings.
type Decoded struct {
	// Version is 0 for legacy and 1 + n for version n, as in TxMeta.
	Version                                                       uint8
	NumRequiredSignatures, NumReadonlySigned, NumReadonlyUnsigned uint8
	Blockhash                                                     [32]byte
	Signatures                                                    [][64]byte
	Keys                                                          [][32]byte
	Instructions                                                  []Instruction
	Lookups                                                       []Lookup
	// Signer and Writable are Agave's parsed-account flags, for the static
	// keys and then the loaded addresses.
	Signer, Writable []bool
	// Config is the config of a v1 message: priority fee, compute unit limit,
	// loaded accounts data size limit and heap size. Each is nil when not set.
	Config *TransactionConfig
}

// Instruction is a compiled instruction of the message.
type Instruction struct {
	ProgramIDIndex uint8
	Accounts       []byte
	Data           []byte
}

// Lookup is a v0 message's address table lookup.
type Lookup struct {
	Table              [32]byte
	Writable, Readonly []byte
}

// TransactionConfig is a v1 message's TransactionConfig.
type TransactionConfig struct {
	PriorityFee                                             *uint64
	ComputeUnitLimit, LoadedAccountsDataSizeLimit, HeapSize *uint32
}

// Decode runs the engine's decode kind on a stored transaction and the
// addresses its lookups resolved to. It is machine-local and uses no handle.
func Decode(tx []byte, writable, readonly [][32]byte) (Decoded, error) {
	var d Decoded
	loaded := binary.LittleEndian.AppendUint32(nil, uint32(len(writable)))
	for _, k := range append(append([][32]byte(nil), writable...), readonly...) {
		loaded = append(loaded, k[:]...)
	}
	var out *C.uint8_t
	var outLen C.size_t
	p, n := reqPtr(tx)
	lp, ln := reqPtr(loaded)
	if err := statusErr(C.svm_decode_transaction(p, n, lp, ln, &out, &outLen)); err != nil {
		return d, err
	}
	buf := &Buf{ptr: out, n: outLen}
	defer buf.Free()
	r, err := NewArenaReader(buf.Bytes(), KindDecodeResp)
	if err != nil || r.Len() != 1 {
		return d, ErrBadArena
	}
	m := &metaReader{b: r.Record(0)}
	head := m.take(1 + 3 + 32)
	if head == nil {
		return d, ErrBadArena
	}
	d.Version = head[0]
	d.NumRequiredSignatures, d.NumReadonlySigned, d.NumReadonlyUnsigned = head[1], head[2], head[3]
	d.Blockhash = [32]byte(head[4:36])
	for i := m.u32(); i > 0 && m.err == nil; i-- {
		if s := m.take(64); s != nil {
			d.Signatures = append(d.Signatures, [64]byte(s))
		}
	}
	d.Keys = m.keys()
	for i := m.u32(); i > 0 && m.err == nil; i-- {
		program := m.take(1)
		if program == nil {
			break
		}
		accounts := append([]byte(nil), m.take(m.u32())...)
		data := append([]byte(nil), m.take(m.u32())...)
		d.Instructions = append(d.Instructions, Instruction{program[0], accounts, data})
	}
	for i := m.u32(); i > 0 && m.err == nil; i-- {
		table := m.take(32)
		if table == nil {
			break
		}
		w := append([]byte(nil), m.take(m.u32())...)
		ro := append([]byte(nil), m.take(m.u32())...)
		d.Lookups = append(d.Lookups, Lookup{[32]byte(table), w, ro})
	}
	for _, f := range m.take(m.u32()) {
		d.Signer = append(d.Signer, f&1 != 0)
		d.Writable = append(d.Writable, f&2 != 0)
	}
	if d.Version == 2 {
		var c TransactionConfig
		if m.take1() == 1 {
			if b := m.take(8); b != nil {
				v := binary.LittleEndian.Uint64(b)
				c.PriorityFee = &v
			}
		}
		for _, field := range []**uint32{&c.ComputeUnitLimit, &c.LoadedAccountsDataSizeLimit, &c.HeapSize} {
			if m.take1() == 1 {
				if b := m.take(4); b != nil {
					v := binary.LittleEndian.Uint32(b)
					*field = &v
				}
			}
		}
		d.Config = &c
	}
	if m.err != nil {
		return d, ErrBadArena
	}
	return d, nil
}

// DeserializeError returns wincode's reason why the bytes are not a
// VersionedTransaction, or not a VersionedMessage when message is set. Agave's
// RPC quotes it. It is empty when the bytes deserialize.
func DeserializeError(b []byte, message bool) string {
	var kind C.uint8_t
	if message {
		kind = 1
	}
	p, n := reqPtr(b)
	s := C.svm_deserialize_error(kind, p, n)
	if s == nil {
		return ""
	}
	defer C.svm_engine_string_free(s)
	return C.GoString(s)
}

// FeatureSetID is getVersion's feature-set: the first four bytes of the
// agave_feature_set::ID this engine links, little-endian.
func FeatureSetID() uint32 { return uint32(C.svm_feature_set_id()) }

// LookupTables returns the address lookup tables that a transaction names, in
// message order. simulateTransaction reads them for LookupAddresses.
func LookupTables(tx []byte) ([][32]byte, error) {
	var out *C.uint8_t
	var outLen C.size_t
	p, n := reqPtr(tx)
	if err := statusErr(C.svm_lookup_tables(p, n, &out, &outLen)); err != nil {
		return nil, err
	}
	buf := &Buf{ptr: out, n: outLen}
	defer buf.Free()
	r, err := NewArenaReader(buf.Bytes(), KindKeysResp)
	if err != nil {
		return nil, err
	}
	keys := make([][32]byte, r.Len())
	for i := range keys {
		if keys[i], err = r.Key32(i); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// resolveRequest is the phase 2 request for the tables of one transaction:
// slotHashes (the SlotHashes account's data), then the stored value of each
// table. The engine keys the tables by address, so their order does not matter.
func resolveRequest(slotHashes []byte, tables map[[32]byte][]byte) []byte {
	w := NewArenaWriter(KindResolveReq, 0)
	w.Push(slotHashes)
	for key, value := range tables {
		w.PushStored(&key, value)
	}
	return w.Finish()
}

// LookupAddresses resolves the lookup tables of a transaction at height.
// slotHashes is the SlotHashes account's data. tables holds the stored value of
// each table by key, and leaves out an absent table. A message whose tables do
// not resolve is refused, with the display text of the error.
func LookupAddresses(height uint64, tx, slotHashes []byte, tables map[[32]byte][]byte) (writable, readonly [][32]byte, refused string, err error) {
	var out *C.uint8_t
	var outLen C.size_t
	tp, tn := reqPtr(tx)
	rp, rn := reqPtr(resolveRequest(slotHashes, tables))
	if err := statusErr(C.svm_lookup_addresses(C.uint64_t(height), tp, tn, rp, rn, &out, &outLen)); err != nil {
		return nil, nil, "", err
	}
	buf := &Buf{ptr: out, n: outLen}
	defer buf.Free()
	r, err := NewArenaReader(buf.Bytes(), KindCheckResp)
	if err != nil || r.Len() == 0 || len(r.Record(0)) < 8 {
		return nil, nil, "", ErrBadArena
	}
	head := r.Record(0)
	if binary.LittleEndian.Uint32(head[0:4]) != 0 {
		if r.Len() < 2 {
			return nil, nil, "", ErrBadArena
		}
		return nil, nil, string(bytes.TrimRight(r.Record(1), "\x00")), nil
	}
	n := int(binary.LittleEndian.Uint32(head[4:8]))
	if n > r.Len()-1 {
		return nil, nil, "", ErrBadArena
	}
	for i := 1; i < r.Len(); i++ {
		key, err := r.Key32(i)
		if err != nil {
			return nil, nil, "", err
		}
		if i <= n {
			writable = append(writable, key)
		} else {
			readonly = append(readonly, key)
		}
	}
	return writable, readonly, "", nil
}

// FeeForMessage is the engine query for getFeeForMessage. known is false when
// the message has no fee. ErrBadInput means it does not sanitize. It takes no
// block handle, so it is safe beside an open block.
func FeeForMessage(msg []byte) (fee uint64, known bool, err error) {
	var out C.uint64_t
	var k C.uint8_t
	p, n := reqPtr(msg)
	if err := statusErr(C.svm_fee_for_message(p, n, &out, &k)); err != nil {
		return 0, false, err
	}
	return uint64(out), k != 0, nil
}

// Release drops the open block. It is idempotent, so it belongs in a defer.
func Release(handle uint64) {
	C.svm_block_release(C.uint64_t(handle))
}
