package ffi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// appendShortU16 writes Solana's ShortU16 length prefix.
func appendShortU16(b []byte, n int) []byte {
	for {
		e := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			return append(b, e)
		}
		b = append(b, e|0x80)
	}
}

// legacyMessage builds a transfer-shaped legacy message: one signer, two
// writable accounts, one readonly program, dataLen bytes of instruction data.
func legacyMessage(payer ed25519.PublicKey, dest, program, blockhash [32]byte, dataLen int) []byte {
	var m []byte
	m = append(m, 1, 0, 1) // num_required_signatures, num_readonly_signed, num_readonly_unsigned
	m = appendShortU16(m, 3)
	m = append(m, payer...)
	m = append(m, dest[:]...)
	m = append(m, program[:]...)
	m = append(m, blockhash[:]...)
	m = appendShortU16(m, 1) // one instruction
	m = append(m, 2)         // program_id_index
	m = appendShortU16(m, 2) // account indexes
	m = append(m, 0, 1)
	m = appendShortU16(m, dataLen)
	data := make([]byte, dataLen)
	data[0] = 1
	return append(m, data...)
}

// signedTx serializes a signed VersionedTransaction that carries a legacy
// message.
func signedTx(priv ed25519.PrivateKey, dest, program, blockhash [32]byte, dataLen int) []byte {
	msg := legacyMessage(priv.Public().(ed25519.PublicKey), dest, program, blockhash, dataLen)
	sig := ed25519.Sign(priv, msg)
	out := appendShortU16(nil, 1)
	out = append(out, sig...)
	return append(out, msg...)
}

// budgetedTx is signedTx with a SetComputeUnitLimit of units and a
// SetLoadedAccountsDataSizeLimit of 32 KiB in front. This is the shape of the
// cheapest transactions that a real block carries (rpc/accept/fill). The cost
// model charges both declared limits, not the 200,000 units and 64 MiB that it
// charges an undeclared transaction. So a block holds the most transactions of
// this shape.
func budgetedTx(priv ed25519.PrivateKey, dest, program, blockhash [32]byte, dataLen int, units uint32) []byte {
	budget := [32]byte{3, 6, 70, 111, 229, 33, 23, 50, 255, 236, 173, 186, 114, 195, 155, 231,
		188, 140, 229, 187, 197, 247, 18, 107, 44, 67, 155, 58, 64, 0, 0, 0} // ComputeBudget111111111111111111111111111111
	var m []byte
	m = append(m, 1, 0, 2) // num_required_signatures, num_readonly_signed, num_readonly_unsigned
	m = appendShortU16(m, 4)
	m = append(m, priv.Public().(ed25519.PublicKey)...)
	m = append(m, dest[:]...)
	m = append(m, program[:]...)
	m = append(m, budget[:]...)
	m = append(m, blockhash[:]...)
	m = appendShortU16(m, 3) // three instructions
	m = append(m, 3)         // SetComputeUnitLimit: no accounts, tag 2, a u32
	m = appendShortU16(m, 0)
	m = appendShortU16(m, 5)
	m = binary.LittleEndian.AppendUint32(append(m, 2), units)
	m = append(m, 3) // SetLoadedAccountsDataSizeLimit: tag 4, a u32
	m = appendShortU16(m, 0)
	m = appendShortU16(m, 5)
	m = binary.LittleEndian.AppendUint32(append(m, 4), 32*1024)
	m = append(m, 2) // the transfer-shaped instruction
	m = appendShortU16(m, 2)
	m = append(m, 0, 1)
	m = appendShortU16(m, dataLen)
	data := make([]byte, dataLen)
	data[0] = 1
	m = append(m, data...)
	out := appendShortU16(nil, 1)
	out = append(out, ed25519.Sign(priv, m)...)
	return append(out, m...)
}

// blockFixture builds n transactions. When disjoint is true, each transaction
// writes its own accounts, so the scheduler finds one group. When it is false,
// all of them write the same destination, so the scheduler finds n groups.
func blockFixture(t testing.TB, n int, disjoint bool) (reqs [][]byte, keys [][32]byte) {
	return blockFixtureData(t, n, disjoint, 8)
}

func blockFixtureData(t testing.TB, n int, disjoint bool, dataLen int) (reqs [][]byte, keys [][32]byte) {
	t.Helper()
	var program, blockhash, shared [32]byte
	program[0], blockhash[0], shared[0] = 9, 7, 5
	seen := map[[32]byte]bool{}
	for i := 0; i < n; i++ {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		dest := shared
		if disjoint {
			if _, err := rand.Read(dest[:]); err != nil {
				t.Fatal(err)
			}
		}
		reqs = append(reqs, signedTx(priv, dest, program, blockhash, dataLen))
		var payer [32]byte
		copy(payer[:], pub)
		for _, k := range [][32]byte{payer, dest, program} {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	return reqs, keys
}

func planArena(txs [][]byte) []byte {
	w := NewArenaWriter(KindPlanReq, 0)
	total := 0
	for _, tx := range txs {
		total += len(tx) + 8
	}
	w.Grow(len(txs), total)
	for _, tx := range txs {
		w.PushSized(tx)
	}
	return w.Finish()
}

// executeArena packs every key as an account with dataLen bytes of data. The
// values are pinned, so the writer must reach Execute or be unpinned.
func executeArena(handle uint64, keys [][32]byte, dataLen int) *ArenaWriter {
	w := NewArenaWriter(KindExecuteReq, handle)
	w.Push(make([]byte, len(keys))) // already-processed bitmap, one byte per transaction at most
	w.Grow(len(keys), len(keys)*pinnedLen)
	var owner [32]byte
	owner[0] = 1
	data := make([]byte, dataLen)
	for i := range keys {
		w.PushPinned(&keys[i], EncodeAccount(&owner, 1_000_000, ^uint64(0), false, data))
	}
	w.Push(nil) // nothing dropped at the arena ceiling
	return w
}

var genesisHash = [32]byte{1}

// TestResolveWithoutSlotHashesIsRejected proves that Resolve rejects a request
// without SlotHashes as record 0. An empty default would not fail. It would set
// the deactivation cool-down of every deactivating table to zero.
func TestResolveWithoutSlotHashesIsRejected(t *testing.T) {
	plan := NewArenaWriter(KindPlanReq, 0)
	plan.PushSized([]byte{0})
	handle, buf, err := Plan(1, [32]byte{1}, plan.Finish())
	if err != nil {
		t.Fatal(err)
	}
	defer Release(handle)
	buf.Free()

	if _, err := Resolve(handle, NewArenaWriter(KindResolveReq, handle).Finish()); err != ErrBadInput {
		t.Fatalf("Resolve with no records = %v, want ErrBadInput", err)
	}
	junk := NewArenaWriter(KindResolveReq, handle)
	junk.Push([]byte{0xff, 0xff, 0xff})
	if _, err := Resolve(handle, junk.Finish()); err != ErrBadInput {
		t.Fatalf("Resolve with a record 0 that is not SlotHashes = %v, want ErrBadInput", err)
	}
}

// TestPlanLoadListIsInFirstUseOrder proves the load list order: the nine
// sysvars, then every fee payer in block order, then every other key at its
// first use. The order decides which keys survive the arena ceiling.
func TestPlanLoadListIsInFirstUseOrder(t *testing.T) {
	txs, keys := blockFixture(t, 16, true)

	handle, plan, err := Plan(1, genesisHash, planArena(txs))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	defer Release(handle)
	defer plan.Free()

	pr, err := NewArenaReader(plan.Bytes(), KindPlanResp)
	if err != nil {
		t.Fatalf("plan response: %v", err)
	}
	if pr.Handle != handle {
		t.Fatalf("plan response handle %d, want %d", pr.Handle, handle)
	}
	if pr.Aux != 0 {
		t.Fatalf("legacy-only block reported %d lookup tables; phase 2 should be skippable", pr.Aux)
	}
	// The payer follows the signature count, the signature, the header and
	// the key count (legacyMessage).
	var want [][32]byte
	payer := map[[32]byte]bool{}
	for _, tx := range txs {
		p := [32]byte(tx[69:101])
		payer[p] = true
		want = append(want, p)
	}
	for _, k := range keys {
		if !payer[k] {
			want = append(want, k)
		}
	}
	// The sysvars, the load list, then one dedup key per transaction.
	if pr.Len() != 9+len(want)+len(txs) {
		t.Fatalf("plan returned %d records, want 9 sysvars, %d keys and %d dedup keys", pr.Len(), len(want), len(txs))
	}
	for i, k := range want {
		if got, _ := pr.Key32(9 + i); got != k {
			t.Fatalf("load list entry %d is %x, want %x", 9+i, got, k)
		}
	}
}

// TestMissingSysvarHaltsExecute proves that an arena without sysvars halts
// phase 3 with an error that names the first empty slot. Phase 3 does not run
// transactions that cannot read the sysvars.
func TestMissingSysvarHaltsExecute(t *testing.T) {
	txs, keys := blockFixture(t, 2, true)
	handle, plan, err := Plan(1, genesisHash, planArena(txs))
	if err != nil {
		t.Fatal(err)
	}
	defer Release(handle)
	plan.Free()

	_, err = Execute(handle, executeArena(handle, keys, 165))
	if !errors.Is(err, ErrMissingSysvar) || !strings.HasSuffix(err.Error(), ": Clock") {
		t.Fatalf("execute without sysvars gave %v, want ErrMissingSysvar naming Clock", err)
	}
}

// TestStaleHandleIsRejected proves that a handle from a released block does
// not execute.
func TestStaleHandleIsRejected(t *testing.T) {
	txs, keys := blockFixture(t, 2, true)
	handle, plan, err := Plan(1, genesisHash, planArena(txs))
	if err != nil {
		t.Fatal(err)
	}
	plan.Free()
	Release(handle)

	if _, err := Execute(handle, executeArena(handle, keys, 165)); !errors.Is(err, ErrBadHandle) {
		t.Fatalf("stale handle gave %v, want ErrBadHandle", err)
	}
}

// TestSecondBlockIsRejected proves that the engine refuses a second open block
// with ErrBusy. Two open blocks are a driver bug, and the engine must not serve
// the wrong one.
func TestSecondBlockIsRejected(t *testing.T) {
	txs, _ := blockFixture(t, 2, true)
	handle, plan, err := Plan(1, genesisHash, planArena(txs))
	if err != nil {
		t.Fatal(err)
	}
	defer Release(handle)
	plan.Free()

	if _, _, err := Plan(2, genesisHash, planArena(txs)); !errors.Is(err, ErrBusy) {
		t.Fatalf("second plan gave %v, want ErrBusy", err)
	}
}

func TestMalformedRequestIsRejected(t *testing.T) {
	good := planArena([][]byte{{1, 2, 3}})
	if _, _, err := Plan(1, genesisHash, good[:len(good)-4]); !errors.Is(err, ErrBadInput) {
		t.Fatalf("truncated arena gave %v, want ErrBadInput", err)
	}
	if _, _, err := Plan(1, genesisHash, bytes.Repeat([]byte{0xff}, 64)); !errors.Is(err, ErrBadInput) {
		t.Fatalf("garbage gave %v, want ErrBadInput", err)
	}
}

func TestArenaRejectsWrongKind(t *testing.T) {
	buf := NewArenaWriter(KindPlanReq, 0).Finish()
	if _, err := NewArenaReader(buf, KindExecuteResp); !errors.Is(err, ErrBadArena) {
		t.Fatalf("wrong kind gave %v, want ErrBadArena", err)
	}
}

func TestArenaAccountRoundTripInGo(t *testing.T) {
	var pk, owner [32]byte
	pk[0], owner[31] = 3, 4
	w := NewArenaWriter(KindExecuteResp, 0)
	w.PushAccount(&pk, &owner, 42, 99, true, []byte("hello"))
	r, err := NewArenaReader(w.Finish(), KindExecuteResp)
	if err != nil {
		t.Fatal(err)
	}
	a, err := r.AccountAt(0)
	if err != nil {
		t.Fatal(err)
	}
	if a.Pubkey != pk || a.Owner != owner || a.Lamports != 42 || a.RentEpoch != 99 || !a.Executable || string(a.Data) != "hello" {
		t.Fatalf("account round trip lost data: %+v", a)
	}
}

// Lamports and DataLen read the two head fields of EncodeAccount's layout.
func TestTheStoredHeadFieldsAreRead(t *testing.T) {
	var owner [32]byte
	v := EncodeAccount(&owner, 42, 99, true, []byte("hello"))
	if Lamports(v) != 42 || DataLen(v) != 5 {
		t.Fatalf("lamports %d and data length %d, want 42 and 5", Lamports(v), DataLen(v))
	}
}

// BenchmarkBuildExecuteArena times the Go side of building the execute request
// arena. The boundary causes this work, so it counts in the boundary's cost.
func BenchmarkBuildExecuteArena(b *testing.B) {
	_, keys := blockFixture(b, 7770, true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := executeArena(1, keys, 165)
		_ = w.Finish()
		w.Unpin()
	}
}

var sinkU64 uint64

// BenchmarkEmptyPhaseCrossing times a plan of an empty arena, to separate the
// framing cost from the payload cost.
func BenchmarkEmptyPhaseCrossing(b *testing.B) {
	req := planArena(nil)
	for i := 0; i < b.N; i++ {
		h, p, err := Plan(1, genesisHash, req)
		if err != nil {
			b.Fatal(err)
		}
		sinkU64 += uint64(len(p.Bytes()))
		p.Free()
		Release(h)
	}
}

// BenchmarkPlanPhase times phase 1 alone at the two block shapes that bound
// it: the most transfer-shaped transactions that the unit limit admits, and the
// most packet-size ones. Each count is MAX_BLOCK_UNITS divided by the cost of
// one transaction, so ProcessProposal accepts the block. Phase 1 counts no
// budget, so a larger block would time transactions that no proposal can
// carry. Phase 1 verifies signatures and parses, so the result is an upper
// bound on the parse without signatures that ProcessProposal pays.
func BenchmarkPlanPhase(b *testing.B) {
	limit, _, _ := BlockLimits()
	var program, blockhash [32]byte
	program[0], blockhash[0] = 9, 7
	for _, shape := range []struct {
		name    string
		dataLen int
	}{
		{"transfers", 8},
		// 980 bytes of data makes each transaction exactly PACKET_DATA_SIZE,
		// 1,232 bytes.
		{"packet-size", 980},
	} {
		build := func(n int) [][]byte {
			txs := make([][]byte, n)
			for i := range txs {
				_, priv, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					b.Fatal(err)
				}
				var dest [32]byte
				if _, err := rand.Read(dest[:]); err != nil {
					b.Fatal(err)
				}
				txs[i] = budgetedTx(priv, dest, program, blockhash, shape.dataLen, 300)
			}
			return txs
		}
		admission, err := CheckTx(build(1)[0], true, 1)
		if err != nil {
			b.Fatal(err)
		}
		n := int(limit / admission.Units)
		b.Run(fmt.Sprintf("%s-%d", shape.name, n), func(b *testing.B) {
			txs := build(n)
			if shape.dataLen == 980 && len(txs[0]) != 1232 {
				b.Fatalf("packet-size transaction is %d bytes, want 1232", len(txs[0]))
			}
			req := planArena(txs)
			b.SetBytes(int64(len(req)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h, p, err := Plan(1, genesisHash, req)
				if err != nil {
					b.Fatal(err)
				}
				p.Free()
				Release(h)
			}
		})
	}
}

// BenchmarkPlanArenaBuild times the Go side of building the phase 1 request
// arena.
func BenchmarkPlanArenaBuild(b *testing.B) {
	txs, _ := blockFixture(b, 7770, true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = planArena(txs)
	}
}

func TestReportPayloadSizes(t *testing.T) {
	txs, keys := blockFixture(t, 7770, true)
	planReq := planArena(txs)
	execReq := executeArena(1, keys, 165)
	defer execReq.Unpin()
	t.Logf("7,770 transfer-shaped transactions")
	t.Logf("  distinct accounts            %d", len(keys))
	t.Logf("  phase 1 request (txs)        %.2f MB", float64(len(planReq))/1e6)
	t.Logf("  phase 3 request (pinned)     %.2f MB", float64(len(execReq.Finish()))/1e6)
}

// TestPlanKeyOrderIsStableUnderParallelism proves that the parallel signature
// checks of phase 1 do not change the key order it returns. The order is
// consensus-critical, because Go loads accounts in this order and all
// validators must agree on it.
func TestPlanKeyOrderIsStableUnderParallelism(t *testing.T) {
	// 128 transactions use the parallel map and stay inside one block's unit
	// budget.
	txs, _ := blockFixture(t, 128, true)
	req := planArena(txs)

	var first []byte
	for round := 0; round < 8; round++ {
		handle, plan, err := Plan(1, genesisHash, req)
		if err != nil {
			t.Fatal(err)
		}
		r, err := NewArenaReader(plan.Bytes(), KindPlanResp)
		if err != nil {
			t.Fatal(err)
		}
		var got []byte
		for i := 0; i < r.Len(); i++ {
			k, err := r.Key32(i)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, k[:]...)
		}
		plan.Free()
		Release(handle)

		if round == 0 {
			first = got
			continue
		}
		if !bytes.Equal(first, got) {
			t.Fatalf("round %d returned a different key order", round)
		}
	}
}

// TestABadSignatureIsDroppedNotFatal proves that phase 1 keeps a block with a
// bad signature and drops only that transaction. ProcessProposal refuses such
// a block, but a block reaches FinalizeBlock on two thirds of the votes,
// without this node's. A fatal error would let those votes halt every
// validator. The dropped transaction has a zero dedup key, the load list loses
// the keys that only it names, and phase 1 reports the drop with its error.
func TestABadSignatureIsDroppedNotFatal(t *testing.T) {
	txs, _ := blockFixture(t, 4, true)

	// The load list for the block as built, then for the same block with one
	// signature corrupted. Each fixture transaction has its own payer and
	// destination, so dropping one drops exactly two keys.
	loadList := func(txs [][]byte) (int, [][64]byte, []Dropped) {
		t.Helper()
		handle, plan, err := Plan(1, genesisHash, planArena(txs))
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		defer Release(handle)
		defer plan.Free()
		dropped, err := BlockDropped(handle)
		if err != nil {
			t.Fatal(err)
		}
		r, err := NewArenaReader(plan.Bytes(), KindPlanResp)
		if err != nil {
			t.Fatal(err)
		}
		loadEnd := r.Len() - len(txs)
		keys := make([][64]byte, len(txs))
		for i := range keys {
			keys[i] = [64]byte(r.Record(loadEnd + i)[:64])
		}
		return loadEnd, keys, dropped
	}

	before, keys, dropped := loadList(txs)
	if len(dropped) != 0 {
		t.Fatalf("a valid block reported drops: %v", dropped)
	}
	for i, k := range keys {
		if k == ([64]byte{}) {
			t.Fatalf("transaction %d has no dedup key", i)
		}
	}
	txs[2][10] ^= 0xff // corrupt a signature byte
	after, keys, dropped := loadList(txs)
	if want := []Dropped{{Index: 2, Err: "SignatureFailure"}}; !slices.Equal(dropped, want) {
		t.Fatalf("phase 1 reported drops %v, want %v", dropped, want)
	}
	if after != before-2 {
		t.Fatalf("the load list went from %d keys to %d, want %d", before, after, before-2)
	}
	if keys[2] != ([64]byte{}) {
		t.Fatalf("the dropped transaction carries dedup key %x, so it would spend one", keys[2])
	}
	for _, i := range []int{0, 1, 3} {
		if keys[i] == ([64]byte{}) {
			t.Fatalf("transaction %d was dropped with the bad one", i)
		}
	}
}
