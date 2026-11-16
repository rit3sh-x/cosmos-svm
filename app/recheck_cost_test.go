package app_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestRecheckCost times CheckTx Recheck of a full block's pool, as CometBFT
// rechecks the pool after each commit, and measures the memory it takes. It
// runs only when SVM_RECHECK_COST gives a run count:
//
//	SVM_RECHECK_COST=30 go test ./app -run '^TestRecheckCost$' -count=1 -v
func TestRecheckCost(t *testing.T) {
	runs, _ := strconv.Atoi(os.Getenv("SVM_RECHECK_COST"))
	if runs == 0 {
		t.Skip("set SVM_RECHECK_COST to a run count")
	}
	payer := func(i int) ed25519.PrivateKey {
		seed := sha256.Sum256(binary.LittleEndian.AppendUint64([]byte("recheck payer"), uint64(i)))
		return ed25519.NewKeyFromSeed(seed[:])
	}
	// Four pools, built from the two full blocks of the cost gate. One holds
	// the cheapest transfers from different funded payers, one the same with
	// five from each payer, one transactions of 11 signatures from different
	// funded payers, and one the cheapest from payers that hold nothing, which
	// recheck refuses. The funded payers are genesis accounts, so the account
	// stores hold about 11,000.
	const funded = 11_000
	accounts := make([]svm.GenesisAccount, funded)
	for i := range accounts {
		pub := tc.Pub(payer(i))
		accounts[i] = svm.GenesisAccount{Pubkey: base58.Encode(pub[:]), Lamports: 1_000_000_000,
			Owner: base58.Encode(svm.SystemProgram[:]), RentEpoch: ^uint64(0)}
	}
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: accounts})
	c.Block()
	_, next, err := c.App.Next()
	if err != nil {
		t.Fatal(err)
	}
	limit, _, _ := ffi.BlockLimits()
	loaded := tc.Instruction{Program: key(t, "ComputeBudget111111111111111111111111111111"),
		Data: binary.LittleEndian.AppendUint32([]byte{4}, 32*1024)}
	recipient := 0
	signed := func(p ed25519.PrivateKey, extra int, self bool) []byte {
		recipient++
		to := tc.Pub(payer(1_000_000 + recipient))
		if self {
			to = tc.Pub(p)
		}
		ix := tc.Transfer(p, to, 1)
		signers := make([]ed25519.PrivateKey, extra)
		for i := range signers {
			signers[i] = payer(2_000_000 + recipient*16 + i)
			ix.Accounts = append(ix.Accounts, tc.Signer(tc.Pub(signers[i]), false))
		}
		return tc.Tx(c.Latest(), p, signers, ix, computeUnitLimit(t, 450), loaded)
	}
	most := 0
	for len(signed(payer(0), most+1, true)) <= 1232 {
		most++
	}
	full := func(one []byte) int {
		a, err := ffi.CheckTx(one, true, next.Height)
		if err != nil {
			t.Fatal(err)
		}
		return min(int(limit/a.Units), svm.MaxBlockBytes/(len(c.Wrap(one))+3))
	}
	type pool struct {
		name  string
		raws  [][]byte
		admit bool
	}
	var pools []pool
	used := 0
	build := func(name string, extra int, self bool, each int, admit bool) {
		n := full(signed(payer(0), extra, self))
		raws := make([][]byte, n)
		for i := range raws {
			p := payer(used + i/each)
			if !admit {
				p = payer(3_000_000 + i)
			}
			raws[i] = c.Wrap(signed(p, extra, self))
		}
		if admit {
			used += (n + each - 1) / each
		}
		if used > funded {
			t.Fatalf("the pools need %d funded payers, genesis has %d", used, funded)
		}
		pools = append(pools, pool{name, raws, admit})
	}
	build("cheapest-distinct-payers", 0, false, 1, true)
	build("cheapest-five-per-payer", 0, false, 5, true)
	build("most-signatures-distinct-payers", most, true, 1, true)
	build("cheapest-payers-hold-nothing", 0, false, 1, false)

	// Each pool reports the allocations and bytes for each transaction, the
	// peak RSS of the process during the recheck, the heap that stays at the
	// same height after it, and the heap after the next block commits.
	status := func(field string) float64 {
		b, err := os.ReadFile("/proc/self/status")
		if err != nil {
			return -1
		}
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) >= 2 && f[0] == field+":" {
				kb, _ := strconv.ParseFloat(f[1], 64)
				return kb / 1024
			}
		}
		return -1
	}
	heap := func() float64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return float64(m.HeapAlloc) / 1024
	}
	for _, p := range pools {
		pass := func() {
			for _, raw := range p.raws {
				if ok := c.Check(raw, true).Code == 0; ok != p.admit {
					t.Fatalf("%s: recheck admitted=%v, want %v", p.name, ok, p.admit)
				}
			}
		}
		before := heap()
		rssBefore := status("VmRSS")
		peakReset := os.WriteFile("/proc/self/clear_refs", []byte("5"), 0) == nil
		d := make([]time.Duration, runs)
		mallocs, bytes := make([]uint64, runs), make([]uint64, runs)
		for i := -2; i < runs; i++ {
			runtime.GC()
			var m0, m1 runtime.MemStats
			runtime.ReadMemStats(&m0)
			start := time.Now()
			pass()
			took := time.Since(start)
			runtime.ReadMemStats(&m1)
			if i >= 0 {
				d[i], mallocs[i], bytes[i] = took, m1.Mallocs-m0.Mallocs, m1.TotalAlloc-m0.TotalAlloc
			}
		}
		peak := status("VmHWM")
		if !peakReset {
			peak = -1
		}
		retained := heap() - before
		c.Block()
		if _, _, err := c.App.Next(); err != nil {
			t.Fatal(err)
		}
		released := heap() - before
		slices.Sort(d)
		slices.Sort(mallocs)
		slices.Sort(bytes)
		n := float64(len(p.raws))
		fmt.Printf("exp=recheck pool=%s n=%d median_ms=%.3f p99_ms=%.3f per_tx_us=%.3f allocs_per_tx=%.1f bytes_per_tx=%.0f rss_before_mb=%.1f peak_rss_mb=%.1f retained_kb=%.1f after_commit_kb=%.1f\n",
			p.name, len(p.raws), float64(d[len(d)/2].Microseconds())/1e3, float64(d[(len(d)*99+99)/100-1].Microseconds())/1e3,
			float64(d[len(d)/2].Nanoseconds())/n/1e3, float64(mallocs[len(mallocs)/2])/n, float64(bytes[len(bytes)/2])/n,
			rssBefore, peak, retained, released)
	}
}
