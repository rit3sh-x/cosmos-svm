package app_test

import (
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestSignatureResultsCost is the signature-result gate. Run it on the 4-vCPU
// reference machine. It runs only when SVM_SIGNATURE_COST names a run count:
//
//	SVM_SIGNATURE_COST=100 go test ./app -run '^TestSignatureResultsCost$' -count=1 -v -timeout 90m
func TestSignatureResultsCost(t *testing.T) {
	runs, _ := strconv.Atoi(os.Getenv("SVM_SIGNATURE_COST"))
	if runs == 0 {
		t.Skip("set SVM_SIGNATURE_COST to a run count")
	}
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()
	_, next, err := c.App.Next()
	if err != nil {
		t.Fatal(err)
	}
	height, bh := next.Height, c.Latest()
	limit, _, _ := ffi.BlockLimits()
	fmt.Printf("exp=sigresults-env cpus=%d gomaxprocs=%d go=%s runs=%d\n",
		runtime.NumCPU(), runtime.GOMAXPROCS(0), runtime.Version(), runs)

	fresh := func() ed25519.PrivateKey {
		_, k, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	loaded := tc.Instruction{Program: key(t, "ComputeBudget111111111111111111111111111111"),
		Data: binary.LittleEndian.AppendUint32([]byte{4}, 32*1024)}
	// A transfer with a declared compute unit limit and a 32 KiB
	// loaded-accounts limit, as rpc/accept/fill sends, and extra readonly
	// signers. With self, the payer pays itself, which leaves one write lock.
	signed := func(extra int, self bool) []byte {
		p := fresh()
		to := tc.Pub(fresh())
		if self {
			to = tc.Pub(p)
		}
		ix := tc.Transfer(p, to, 1)
		signers := make([]ed25519.PrivateKey, extra)
		for i := range signers {
			signers[i] = fresh()
			ix.Accounts = append(ix.Accounts, tc.Signer(tc.Pub(signers[i]), false))
		}
		return tc.Tx(bh, p, signers, ix, computeUnitLimit(t, 450), loaded)
	}
	most := 0
	for len(signed(most+1, true)) <= 1232 {
		most++
	}
	// The two blocks are the cheapest full block, 5,254 one-signature
	// transfers, and the worst, 3,333 transactions with 11 signatures each.
	// Each line is the median and p99 of the runs, after two warm-ups. The
	// gates come from the reference machine. Phase 1 with every result stored
	// fails above a quarter of phase 1 with no stored result, 18.6 ms and
	// 122.6 ms. ProcessProposal with every result stored fails above 5% over
	// its time before it checked signatures, 46.9 ms and 40.8 ms. With no
	// result it fails above that time plus 110% of phase 1, 126.7 ms and
	// 578.2 ms. Phase 1 after a ProcessProposal that held no result fails
	// above the first gate again.
	//
	// What CheckTx New adds to its check is reported, not gated. Half stored
	// is reported: the checks, the handler's time, and the engine call's time,
	// split into hashing and lookup, which every result stored costs, and
	// ed25519, which is the rest.
	blocks := []struct {
		name         string
		build        func() []byte
		gate         time.Duration
		proposalHit  time.Duration
		proposalMiss time.Duration
	}{
		{"cheapest-transfer", func() []byte { return signed(0, false) }, 18_600 * time.Microsecond,
			46_900 * time.Microsecond, 126_700 * time.Microsecond},
		{"most-signatures-one-write", func() []byte { return signed(most, true) }, 122_600 * time.Microsecond,
			40_800 * time.Microsecond, 578_200 * time.Microsecond},
	}

	// measure times f over the runs, after two warm-ups. before runs ahead
	// of each and after behind it, both untimed. A collection after before
	// keeps its garbage, thousands of CheckTx calls, out of the timed run.
	measure := func(before, f, after func()) (time.Duration, time.Duration) {
		d := make([]time.Duration, runs)
		for i := -2; i < runs; i++ {
			before()
			runtime.GC()
			start := time.Now()
			f()
			took := time.Since(start)
			after()
			if i >= 0 {
				d[i] = took
			}
		}
		slices.Sort(d)
		return d[len(d)/2], d[(len(d)*99+99)/100-1]
	}
	nothing := func() {}
	report := func(block, what string, n int, med, p99 time.Duration) {
		fmt.Printf("exp=sigresults block=%s what=%s n=%d median_ms=%.3f p99_ms=%.3f per_tx_us=%.3f\n",
			block, what, n, ms(med), ms(p99), float64(med.Nanoseconds())/float64(n)/1e3)
	}

	for _, b := range blocks {
		one := b.build()
		a, err := ffi.CheckTx(one, true, height)
		if err != nil {
			t.Fatal(b.name, err)
		}
		n := min(int(limit/a.Units), svm.MaxBlockBytes/(len(c.Wrap(one))+3))
		fmt.Printf("exp=sigresults-block block=%s n=%d sigs_per_tx=%d tx_bytes=%d\n", b.name, n, one[0], len(one))
		txs := make([][]byte, n)
		for i := range txs {
			txs[i] = b.build()
		}
		w := ffi.NewArenaWriter(ffi.KindPlanReq, 0)
		for _, tx := range txs {
			w.PushSized(tx)
		}
		req := w.Finish()
		store := func() {
			ffi.ClearSignatureResultsForTests()
			for _, tx := range txs {
				a, err := ffi.CheckTxNew(tx, height)
				if err != nil {
					t.Fatal(err)
				}
				if !ffi.RecordVerified(a.Key) {
					t.Fatal("the pending slot refused the key it produced")
				}
			}
		}
		var handle uint64
		var buf *ffi.Buf
		plan := func() {
			if handle, buf, err = ffi.Plan(height, [32]byte{1}, req); err != nil {
				t.Fatal(err)
			}
		}
		release := func() {
			buf.Free()
			ffi.Release(handle)
		}

		store()
		if got := verified(func() { plan(); release() }); got != 0 {
			t.Fatalf("%s: phase 1 with every result stored checked %d transactions", b.name, got)
		}
		hit, hitP99 := measure(store, plan, release)
		report(b.name, "phase1-every-result", n, hit, hitP99)
		miss, missP99 := measure(ffi.ClearSignatureResultsForTests, plan, release)
		report(b.name, "phase1-no-result", n, miss, missP99)
		if hit > b.gate {
			t.Errorf("%s: phase 1 with every result stored took %v, over the gate of %v", b.name, hit, b.gate)
		}

		check := func(f func([]byte)) func() {
			return func() {
				for _, tx := range txs {
					f(tx)
				}
			}
		}
		hashOnly, p99 := measure(nothing, check(func(tx []byte) { ffi.CheckTx(tx, true, height) }), nothing)
		report(b.name, "checktx-hashonly", n, hashOnly, p99)
		full, p99 := measure(nothing, check(func(tx []byte) { ffi.CheckTx(tx, false, height) }), nothing)
		report(b.name, "checktx-full", n, full, p99)
		kept, p99 := measure(ffi.ClearSignatureResultsForTests, check(func(tx []byte) {
			a, _ := ffi.CheckTxNew(tx, height)
			ffi.RecordVerified(a.Key)
		}), nothing)
		report(b.name, "checktx-new-and-record", n, kept, p99)
		fmt.Printf("exp=sigresults-checktx block=%s ed25519_us=%.3f added_us=%.3f added_pct=%.3f\n", b.name,
			float64((full-hashOnly).Nanoseconds())/float64(n)/1e3, float64((kept-full).Nanoseconds())/float64(n)/1e3,
			100*float64(kept-full)/float64(full-hashOnly))

		half := func() {
			ffi.ClearSignatureResultsForTests()
			for i, tx := range txs {
				if i%2 == 0 {
					a, err := ffi.CheckTxNew(tx, height)
					if err != nil {
						t.Fatal(err)
					}
					ffi.RecordVerified(a.Key)
				}
			}
		}
		proposal := wrapped(c, txs...)
		propose := func() {
			if got := c.Propose(proposal...); got != accept {
				t.Fatalf("%s: the proposal: %v, want ACCEPT", b.name, got)
			}
		}
		batch := func() {
			if failed, err := ffi.ProposalSignatures(txs); err != nil || failed >= 0 {
				t.Fatalf("%s: the engine call: %d, %v", b.name, failed, err)
			}
		}
		ppHit, p99 := measure(store, propose, nothing)
		report(b.name, "proposal-every-result", n, ppHit, p99)
		ppMiss, p99 := measure(ffi.ClearSignatureResultsForTests, propose, nothing)
		report(b.name, "proposal-no-result", n, ppMiss, p99)
		ppHalf, p99 := measure(half, propose, nothing)
		report(b.name, "proposal-half", n, ppHalf, p99)
		callHit, p99 := measure(store, batch, nothing)
		report(b.name, "call-every-result", n, callHit, p99)
		callHalf, p99 := measure(half, batch, nothing)
		report(b.name, "call-half", n, callHalf, p99)
		callMiss, p99 := measure(ffi.ClearSignatureResultsForTests, batch, nothing)
		report(b.name, "call-no-result", n, callMiss, p99)
		half()
		checks := verified(propose)
		fmt.Printf("exp=sigresults-half block=%s hit_rate=%.3f checks=%d handler_ms=%.3f hash_lookup_ms=%.3f ed25519_ms=%.3f\n",
			b.name, 1-float64(checks)/float64(n), checks, ms(ppHalf), ms(callHit), ms(callHalf-callHit))
		after, p99 := measure(func() { ffi.ClearSignatureResultsForTests(); propose() }, plan, release)
		report(b.name, "phase1-after-proposal-with-no-result", n, after, p99)
		ffi.ClearSignatureResultsForTests()
		propose()
		if got := verified(func() { plan(); release() }); got != 0 {
			t.Fatalf("%s: phase 1 after the proposal checked %d transactions", b.name, got)
		}
		for _, g := range []struct {
			what      string
			got, gate time.Duration
		}{{"ProcessProposal with every result stored", ppHit, b.proposalHit},
			{"ProcessProposal with no result", ppMiss, b.proposalMiss},
			{"phase 1 after ProcessProposal", after, b.gate}} {
			if g.got > g.gate {
				t.Errorf("%s: %s took %v, over the gate of %v", b.name, g.what, g.got, g.gate)
			}
		}
	}
	ffi.ClearSignatureResultsForTests()
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1e3 }
