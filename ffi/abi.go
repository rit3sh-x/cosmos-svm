package ffi

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"runtime"
	"unsafe"
)

// The arena format. engine/src/abi.rs explains why it holds no pointers.
//
// cgo forbids a Go pointer stored in C memory. It also forbids Go memory passed
// to C that holds Go pointers. So a descriptor array of {ptr, len} per account
// needs a copy per account. One flat buffer is legal and is one allocation, and
// both sides read records in place.
//
// The exception is the account records of the execute and simulate requests.
// They point at pinned values instead of carrying them.

const (
	arenaMagic     = 0x304d5653 // "SVM0"
	arenaVersion   = 0
	arenaHeaderLen = 32

	KindPlanReq      = 1
	KindPlanResp     = 2
	KindResolveReq   = 3
	KindResolveResp  = 4
	KindExecuteReq   = 5
	KindExecuteResp  = 6
	KindConfigureReq = 7
	KindKeysResp     = 8
	KindSimulateReq  = 9
	KindSimulateResp = 10
	KindCheckResp    = 11
	KindStatsResp    = 12
	KindCommittedReq = 13
	KindDecodeResp   = 14
	KindDroppedResp  = 15
	KindProposalReq  = 16

	// accountHeadLen is the fixed head of an account record, and the data
	// follows it. The pubkey comes first. The rest of the record is the value
	// stored under it (engine/src/abi.rs).
	accountHeadLen = 88
	pubkeyLen      = 32

	// pinnedLen is the size of a pinned account record: the pubkey, then the
	// address and length of the stored value, each a little-endian u64.
	pinnedLen = pubkeyLen + 16
)

// ErrBadArena reports a response buffer that does not parse. It is a bug in this
// package or the engine, never in the block being executed.
var ErrBadArena = errors.New("svm engine returned a malformed arena")

// ArenaWriter builds a request buffer. The Push methods append records, then
// Finish writes the header and offset table in front of them.
//
// The result is an ordinary Go []byte with no pointers inside, so it is legal
// to hand to C for the duration of a call.
type ArenaWriter struct {
	kind    uint16
	handle  uint64
	aux     uint32
	offsets []uint64
	body    []byte
	pins    runtime.Pinner
}

func NewArenaWriter(kind uint16, handle uint64) *ArenaWriter {
	return &ArenaWriter{kind: kind, handle: handle}
}

// Grow preallocates for records totalling bytes, so a block-sized arena is one
// allocation.
//
// It keeps the records already pushed and reallocates around them. The execute
// request pushes the already-processed bitmap before it knows how many accounts
// follow (x/svm/runner.go). A Grow that reset the slices would drop record 0,
// and every later offset would name the wrong bytes.
func (w *ArenaWriter) Grow(records, bytes int) {
	if cap(w.offsets) < records+1 {
		w.offsets = append(make([]uint64, 0, records+1), w.offsets...)
	}
	if cap(w.body) < bytes {
		w.body = append(make([]byte, 0, bytes), w.body...)
	}
}

func (w *ArenaWriter) SetAux(aux uint32) { w.aux = aux }

func (w *ArenaWriter) pad() {
	for len(w.body)%8 != 0 {
		w.body = append(w.body, 0)
	}
}

// Push appends an opaque record.
func (w *ArenaWriter) Push(record []byte) {
	w.offsets = append(w.offsets, uint64(len(w.body)))
	w.body = append(w.body, record...)
	w.pad()
}

// PushSized appends a record after its length, a little-endian u32. It is for
// a reader that needs the exact bytes, because the length of a record includes
// its padding to 8. The plan request pushes its transactions this way, so the
// plan parses the bytes that CheckTx parsed and not their padding.
func (w *ArenaWriter) PushSized(record []byte) {
	w.offsets = append(w.offsets, uint64(len(w.body)))
	w.body = binary.LittleEndian.AppendUint32(w.body, uint32(len(record)))
	w.body = append(w.body, record...)
	w.pad()
}

// PushAccount appends an account record in the layout engine/src/abi.rs parses.
func (w *ArenaWriter) PushAccount(pubkey, owner *[32]byte, lamports, rentEpoch uint64, executable bool, data []byte) {
	w.PushStored(pubkey, EncodeAccount(owner, lamports, rentEpoch, executable, data))
}

// PushStored appends an account record from a pubkey and its stored value,
// which is the record minus its leading pubkey.
func (w *ArenaWriter) PushStored(pubkey *[32]byte, value []byte) {
	w.offsets = append(w.offsets, uint64(len(w.body)))
	w.body = append(w.body, pubkey[:]...)
	w.body = append(w.body, value...)
	w.pad()
}

// PushPinned appends an account record that points at a stored value instead
// of carrying it, so the request holds no second copy of the payload. The value
// stays pinned until Unpin. A pointer to a pinned object "can be contained in
// Go memory passed to C functions" (runtime.Pinner), and pinning also keeps it
// alive.
func (w *ArenaWriter) PushPinned(pubkey *[32]byte, value []byte) {
	w.pins.Pin(&value[0])
	var rec [pinnedLen]byte
	copy(rec[:], pubkey[:])
	binary.LittleEndian.PutUint64(rec[32:40], uint64(uintptr(unsafe.Pointer(&value[0]))))
	binary.LittleEndian.PutUint64(rec[40:48], uint64(len(value)))
	w.Push(rec[:])
}

// Unpin releases every value PushPinned pinned. Execute and Simulate call it
// when the engine returns. A writer that never reaches them must call it
// itself, or the runtime panics when the writer is collected.
func (w *ArenaWriter) Unpin() { w.pins.Unpin() }

// EncodeAccount is the stored value of an account: the arena record without
// its pubkey. The bytes are in the app hash.
func EncodeAccount(owner *[32]byte, lamports, rentEpoch uint64, executable bool, data []byte) []byte {
	v := make([]byte, accountHeadLen-pubkeyLen+len(data))
	binary.LittleEndian.PutUint64(v[0:8], lamports)
	binary.LittleEndian.PutUint64(v[8:16], rentEpoch)
	binary.LittleEndian.PutUint32(v[16:20], uint32(len(data)))
	if executable {
		v[20] = 1
	}
	copy(v[24:56], owner[:])
	copy(v[56:], data)
	return v
}

// Lamports is a stored value's lamports, its first eight bytes. Like the
// slicing it names, it panics on a value shorter than that.
func Lamports(value []byte) uint64 { return binary.LittleEndian.Uint64(value[0:8]) }

// DataLen is the data length a stored value's head declares. It panics on a
// value shorter than the field.
func DataLen(value []byte) int { return int(binary.LittleEndian.Uint32(value[16:20])) }

// Finish seals the arena. The returned slice is valid until the next Finish.
func (w *ArenaWriter) Finish() []byte {
	count := len(w.offsets)
	tableLen := (count + 1) * 8
	base := arenaHeaderLen + tableLen
	total := base + len(w.body)

	out := make([]byte, 0, total)
	var h [arenaHeaderLen]byte
	binary.LittleEndian.PutUint32(h[0:4], arenaMagic)
	binary.LittleEndian.PutUint16(h[4:6], arenaVersion)
	binary.LittleEndian.PutUint16(h[6:8], w.kind)
	binary.LittleEndian.PutUint64(h[8:16], w.handle)
	binary.LittleEndian.PutUint32(h[16:20], uint32(count))
	binary.LittleEndian.PutUint32(h[20:24], w.aux)
	binary.LittleEndian.PutUint64(h[24:32], uint64(total))
	out = append(out, h[:]...)

	var scratch [8]byte
	for _, off := range w.offsets {
		binary.LittleEndian.PutUint64(scratch[:], uint64(base)+off)
		out = append(out, scratch[:]...)
	}
	binary.LittleEndian.PutUint64(scratch[:], uint64(total))
	out = append(out, scratch[:]...)
	out = append(out, w.body...)
	return out
}

// ArenaReader is a borrowed view over a response buffer. Record returns a
// subslice and does not allocate. No value read from it may outlive the Buf it
// came from.
type ArenaReader struct {
	buf    []byte
	Kind   uint16
	Handle uint64
	Aux    uint32
	count  int
	table  []byte
}

func NewArenaReader(buf []byte, expectKind uint16) (*ArenaReader, error) {
	if len(buf) < arenaHeaderLen {
		return nil, fmt.Errorf("%w: short header", ErrBadArena)
	}
	if binary.LittleEndian.Uint32(buf[0:4]) != arenaMagic {
		return nil, fmt.Errorf("%w: bad magic", ErrBadArena)
	}
	if binary.LittleEndian.Uint16(buf[4:6]) != arenaVersion {
		return nil, fmt.Errorf("%w: bad version", ErrBadArena)
	}
	kind := binary.LittleEndian.Uint16(buf[6:8])
	if kind != expectKind {
		return nil, fmt.Errorf("%w: kind %d, want %d", ErrBadArena, kind, expectKind)
	}
	count := int(binary.LittleEndian.Uint32(buf[16:20]))
	if total := binary.LittleEndian.Uint64(buf[24:32]); total != uint64(len(buf)) {
		return nil, fmt.Errorf("%w: length %d, header says %d", ErrBadArena, len(buf), total)
	}

	tableEnd := arenaHeaderLen + (count+1)*8
	if tableEnd < arenaHeaderLen || len(buf) < tableEnd {
		return nil, fmt.Errorf("%w: offset table truncated", ErrBadArena)
	}
	table := buf[arenaHeaderLen:tableEnd]

	// Validate the table once so Record is a plain slice afterwards.
	prev := uint64(tableEnd)
	for i := 0; i <= count; i++ {
		off := binary.LittleEndian.Uint64(table[i*8:])
		if off < prev || off > uint64(len(buf)) {
			return nil, fmt.Errorf("%w: offset %d out of order or out of range", ErrBadArena, i)
		}
		prev = off
	}

	return &ArenaReader{
		buf:    buf,
		Kind:   kind,
		Handle: binary.LittleEndian.Uint64(buf[8:16]),
		Aux:    binary.LittleEndian.Uint32(buf[20:24]),
		count:  count,
		table:  table,
	}, nil
}

func (r *ArenaReader) Len() int { return r.count }

// Record returns record i as a subslice of the underlying buffer.
func (r *ArenaReader) Record(i int) []byte {
	start := binary.LittleEndian.Uint64(r.table[i*8:])
	end := binary.LittleEndian.Uint64(r.table[(i+1)*8:])
	return r.buf[start:end]
}

// Key32 returns record i interpreted as a 32-byte pubkey.
func (r *ArenaReader) Key32(i int) ([32]byte, error) {
	var k [32]byte
	rec := r.Record(i)
	if len(rec) < 32 {
		return k, fmt.Errorf("%w: record %d is %d bytes, want 32", ErrBadArena, i, len(rec))
	}
	copy(k[:], rec[:32])
	return k, nil
}

// Account is one proposed write, read in place out of a response arena.
type Account struct {
	Pubkey     [32]byte
	Owner      [32]byte
	Lamports   uint64
	RentEpoch  uint64
	Executable bool
	Data       []byte
}

// AccountAt parses record i as an account. Data aliases the response buffer.
func (r *ArenaReader) AccountAt(i int) (Account, error) {
	var a Account
	rec := r.Record(i)
	if len(rec) < accountHeadLen {
		return a, fmt.Errorf("%w: account record %d too short", ErrBadArena, i)
	}
	copy(a.Pubkey[:], rec[:pubkeyLen])
	v, err := DecodeAccount(rec[pubkeyLen:])
	if err != nil {
		return a, fmt.Errorf("%w: account record %d: %w", ErrBadArena, i, err)
	}
	v.Pubkey = a.Pubkey
	return v, nil
}

// StoredAt returns record i split into its pubkey and stored value, with the
// trailing padding dropped. Both alias the response buffer, so copy them
// before Free.
func (r *ArenaReader) StoredAt(i int) (pubkey [32]byte, value []byte, err error) {
	rec := r.Record(i)
	if len(rec) < accountHeadLen {
		return pubkey, nil, fmt.Errorf("%w: account record %d too short", ErrBadArena, i)
	}
	dataLen := int(binary.LittleEndian.Uint32(rec[48:52]))
	if len(rec) < accountHeadLen+dataLen {
		return pubkey, nil, fmt.Errorf("%w: account record %d claims %d data bytes", ErrBadArena, i, dataLen)
	}
	copy(pubkey[:], rec[:pubkeyLen])
	return pubkey, rec[pubkeyLen : accountHeadLen+dataLen], nil
}

// DecodeAccount parses a stored value. Data aliases v.
func DecodeAccount(v []byte) (Account, error) {
	var a Account
	head := accountHeadLen - pubkeyLen
	if len(v) < head {
		return a, fmt.Errorf("account value is %d bytes, head is %d", len(v), head)
	}
	dataLen := DataLen(v)
	if len(v) < head+dataLen {
		return a, fmt.Errorf("account value claims %d data bytes, has %d", dataLen, len(v)-head)
	}
	a.Lamports = Lamports(v)
	a.RentEpoch = binary.LittleEndian.Uint64(v[8:16])
	a.Executable = v[20] != 0
	copy(a.Owner[:], v[24:56])
	a.Data = v[head : head+dataLen]
	return a, nil
}

// TxResult is one transaction's outcome. Status is 0 on success. Group is the
// conflict-free group that the scheduler put it in, which gives the measured
// parallel width. Fee is the lamports debited from the fee payer, and zero when
// the transaction was not processed. Signature is the transaction's first
// signature, as the engine parsed it.
type TxResult struct {
	Status       uint32
	Group        uint32
	ComputeUnits uint64
	Fee          uint64
	Signature    [64]byte
	// ComputeUnitPrice is the price the transaction declared, in microlamports
	// per unit. getRecentPrioritizationFees reports it. Nothing ranks by it.
	ComputeUnitPrice uint64
}

// ResultAt parses record i as a transaction result.
func (r *ArenaReader) ResultAt(i int) (TxResult, error) {
	var t TxResult
	rec := r.Record(i)
	if len(rec) < 96 {
		return t, fmt.Errorf("%w: result record %d too short", ErrBadArena, i)
	}
	t.Status = binary.LittleEndian.Uint32(rec[0:4])
	t.Group = binary.LittleEndian.Uint32(rec[4:8])
	t.ComputeUnits = binary.LittleEndian.Uint64(rec[8:16])
	t.Fee = binary.LittleEndian.Uint64(rec[16:24])
	copy(t.Signature[:], rec[24:88])
	t.ComputeUnitPrice = binary.LittleEndian.Uint64(rec[88:96])
	return t, nil
}

// InnerInstruction is one CPI a transaction made, as the message compiled it.
type InnerInstruction struct {
	ProgramIDIndex uint8
	StackHeight    uint8
	Accounts       []byte
	Data           []byte
}

// TxMeta is what getTransaction serves for one transaction. Version is 0 for
// legacy and 1 + n for version n.
type TxMeta struct {
	Version       uint8
	PreBalances   []uint64
	PostBalances  []uint64
	ReturnProgram *[32]byte
	ReturnData    []byte
	Inner         [][]InnerInstruction
	Logs          []string
	// LoadedWritable and LoadedReadonly are the addresses a lookup table
	// resolved for this transaction, empty when it named no table.
	LoadedWritable [][32]byte
	LoadedReadonly [][32]byte
	// Err is the TransactionError as Agave's RPC renders it. The engine makes
	// it with Anza's own derive. Go never parses it, because only that derive
	// defines the shape of `err`. It is nil on success, which marshals as
	// null. getTransaction serves it as meta.err. json/v2 validates a
	// jsontext.Value at marshal time, so bytes that are not JSON fail the
	// RPC response and do not go on the wire.
	Err jsontext.Value
	// CostUnits is `cost().units`, the transaction's cost against
	// MAX_BLOCK_UNITS. getTransaction serves it as costUnits. It is nil in a
	// record written before the engine reported it.
	CostUnits *uint64 `json:",omitempty"`
	// PreTokenBalances and PostTokenBalances are what solana-svm collected
	// for the transaction's token accounts.
	PreTokenBalances  []TokenBalance `json:",omitempty"`
	PostTokenBalances []TokenBalance `json:",omitempty"`
	// StaticKeys are the message's own account keys, in message order. With
	// the loaded addresses they are every account the transaction named. The
	// history builds its address index from them, with no second parse of the
	// transaction. They are nil in a record written before the engine reported
	// them, and then Record.Addresses parses the transaction.
	StaticKeys Keys32 `json:",omitempty"`
}

// Keys32 is a list of keys stored as one base64 string of their bytes. That is
// about 44 characters a key, where a JSON array of byte arrays costs about 100.
type Keys32 [][32]byte

func (k Keys32) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 32*len(k))
	for _, key := range k {
		b = append(b, key[:]...)
	}
	return json.Marshal(base64.StdEncoding.EncodeToString(b), json.Deterministic(true))
}

func (k *Keys32) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b)%32 != 0 {
		return fmt.Errorf("svm: a key list of %d bytes", len(b))
	}
	out := make(Keys32, 0, len(b)/32)
	for i := 0; i < len(b); i += 32 {
		out = append(out, [32]byte(b[i:i+32]))
	}
	*k = out
	return nil
}

// TokenBalance is solana-svm's `SvmTokenInfo`.
type TokenBalance struct {
	AccountIndex uint8
	Mint         [32]byte
	Owner        [32]byte
	ProgramID    [32]byte
	Amount       uint64
	Decimals     uint8
}

const tokenBalanceBytes = 1 + 32 + 32 + 32 + 8 + 1

func (b TokenBalance) bytes() []byte {
	out := make([]byte, 0, tokenBalanceBytes)
	out = append(out, b.AccountIndex)
	out = append(out, b.Mint[:]...)
	out = append(out, b.Owner[:]...)
	out = append(out, b.ProgramID[:]...)
	out = binary.LittleEndian.AppendUint64(out, b.Amount)
	return append(out, b.Decimals)
}

func tokenBalanceOf(v []byte) TokenBalance {
	return TokenBalance{
		AccountIndex: v[0],
		Mint:         [32]byte(v[1:33]),
		Owner:        [32]byte(v[33:65]),
		ProgramID:    [32]byte(v[65:97]),
		Amount:       binary.LittleEndian.Uint64(v[97:105]),
		Decimals:     v[105],
	}
}

// MarshalJSON stores a balance as base64 of the engine's layout. The history
// counts these bytes against index-max-bytes. The default encoding, with three
// keys as JSON byte arrays, costs about three times as much.
func (b TokenBalance) MarshalJSON() ([]byte, error) {
	return json.Marshal(base64.StdEncoding.EncodeToString(b.bytes()), json.Deterministic(true))
}

func (b *TokenBalance) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(v) != tokenBalanceBytes {
		return fmt.Errorf("svm: a token balance of %d bytes", len(v))
	}
	*b = tokenBalanceOf(v)
	return nil
}

type metaReader struct {
	b   []byte
	err error
}

func (m *metaReader) take(n int) []byte {
	if m.err != nil || n < 0 || len(m.b) < n {
		m.err = ErrBadArena
		return nil
	}
	out := m.b[:n]
	m.b = m.b[n:]
	return out
}

// take1 returns one byte, or 0 past the end.
func (m *metaReader) take1() byte {
	if b := m.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (m *metaReader) u32() int {
	b := m.take(4)
	if b == nil {
		return 0
	}
	return int(binary.LittleEndian.Uint32(b))
}

func (m *metaReader) balances() []uint64 {
	n := m.u32()
	b := m.take(8 * n)
	out := make([]uint64, 0, n)
	for i := 0; i+8 <= len(b); i += 8 {
		out = append(out, binary.LittleEndian.Uint64(b[i:]))
	}
	return out
}

func (m *metaReader) tokenBalances() []TokenBalance {
	n := m.u32()
	var out []TokenBalance
	for i := 0; i < n; i++ {
		if b := m.take(tokenBalanceBytes); b != nil {
			out = append(out, tokenBalanceOf(b))
		}
	}
	return out
}

func (m *metaReader) inner() [][]InnerInstruction {
	outer := m.u32()
	var out [][]InnerInstruction
	for o := 0; o < outer && m.err == nil; o++ {
		inner := make([]InnerInstruction, m.u32())
		for k := range inner {
			head := m.take(2)
			if head == nil {
				break
			}
			inner[k].ProgramIDIndex, inner[k].StackHeight = head[0], head[1]
			inner[k].Accounts = append([]byte(nil), m.take(m.u32())...)
			inner[k].Data = append([]byte(nil), m.take(m.u32())...)
		}
		out = append(out, inner)
	}
	return out
}

func (m *metaReader) keys() [][32]byte {
	n := m.u32()
	out := make([][32]byte, 0, n)
	for i := 0; i < n; i++ {
		if b := m.take(32); b != nil {
			out = append(out, [32]byte(b))
		}
	}
	return out
}

// MetaAt parses record i as a metadata record, in the layout
// engine/src/schedule.rs push_meta writes. Everything is copied out.
func (r *ArenaReader) MetaAt(i int) (TxMeta, error) {
	m := &metaReader{b: r.Record(i)}
	var t TxMeta
	t.Version = m.take1()
	t.PreBalances = m.balances()
	t.PostBalances = m.balances()
	if m.u32() == 1 {
		if b := m.take(32); b != nil {
			program := [32]byte(b)
			t.ReturnProgram = &program
			t.ReturnData = append([]byte(nil), m.take(m.u32())...)
		}
	}
	t.Inner = m.inner()
	logs := m.u32()
	for l := 0; l < logs && m.err == nil; l++ {
		t.Logs = append(t.Logs, string(m.take(m.u32())))
	}
	t.LoadedWritable = m.keys()
	t.LoadedReadonly = m.keys()
	if n := m.u32(); n > 0 {
		t.Err = append([]byte(nil), m.take(n)...)
	}
	if b := m.take(8); b != nil {
		cost := binary.LittleEndian.Uint64(b)
		t.CostUnits = &cost
	}
	t.PreTokenBalances = m.tokenBalances()
	t.PostTokenBalances = m.tokenBalances()
	t.StaticKeys = m.keys()
	return t, m.err
}
