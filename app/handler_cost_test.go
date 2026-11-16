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

	abci "github.com/cometbft/cometbft/abci/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestHandlerDecodeCost times ProcessProposal, PrepareProposal and CheckTx
// Recheck over a full block's envelopes, and Decoder alone beside them. It
// runs only when SVM_HANDLER_COST names a run count:
//
//	SVM_HANDLER_COST=30 go test ./app -run '^TestHandlerDecodeCost$' -count=1 -v
func TestHandlerDecodeCost(t *testing.T) {
	runs, _ := strconv.Atoi(os.Getenv("SVM_HANDLER_COST"))
	if runs == 0 {
		t.Skip("set SVM_HANDLER_COST to a run count")
	}
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()
	_, next, err := c.App.Next()
	if err != nil {
		t.Fatal(err)
	}
	height, bh := next.Height, c.Latest()
	limit, _, _ := ffi.BlockLimits()
	// The payers hold nothing. PrepareProposal packs nothing, and Recheck
	// refuses each transaction after it reads and prices it. ProcessProposal
	// does not read balances, so it accepts the block. The blocks are those of
	// TestSignatureResultsCost.
	fresh := func() ed25519.PrivateKey {
		_, k, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	loaded := tc.Instruction{Program: key(t, "ComputeBudget111111111111111111111111111111"),
		Data: binary.LittleEndian.AppendUint32([]byte{4}, 32*1024)}
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
	measure := func(f func()) (time.Duration, time.Duration) {
		d := make([]time.Duration, runs)
		for i := -2; i < runs; i++ {
			runtime.GC()
			start := time.Now()
			f()
			if i >= 0 {
				d[i] = time.Since(start)
			}
		}
		slices.Sort(d)
		return d[len(d)/2], d[(len(d)*99+99)/100-1]
	}
	decode := svm.Decoder(c.App.TxConfig)
	for _, b := range []struct {
		name  string
		build func() []byte
	}{
		{"cheapest-transfer", func() []byte { return signed(0, false) }},
		{"most-signatures-one-write", func() []byte { return signed(most, true) }},
	} {
		one := b.build()
		a, err := ffi.CheckTx(one, true, height)
		if err != nil {
			t.Fatal(b.name, err)
		}
		n := min(int(limit/a.Units), svm.MaxBlockBytes/(len(c.Wrap(one))+3))
		raws := make([][]byte, n)
		for i := range raws {
			raws[i] = c.Wrap(b.build())
		}
		report := func(what string, f func()) {
			med, p99 := measure(f)
			fmt.Printf("exp=handlers block=%s what=%s n=%d median_ms=%.3f p99_ms=%.3f per_tx_us=%.3f\n",
				b.name, what, n, ms(med), ms(p99), float64(med.Nanoseconds())/float64(n)/1e3)
		}
		report("decoder", func() {
			for _, raw := range raws {
				if _, err := decode(raw); err != nil {
					t.Fatal(err)
				}
			}
		})
		report("process", func() {
			if got := c.Propose(raws...); got != abci.ResponseProcessProposal_ACCEPT {
				t.Fatalf("the block: %v, want ACCEPT", got)
			}
		})
		report("prepare", func() {
			if got := c.Pack(svm.MaxBlockBytes, raws...); len(got) != 0 {
				t.Fatalf("packed %d transactions whose payers hold nothing", len(got))
			}
		})
		report("recheck", func() {
			for _, raw := range raws {
				if c.Check(raw, true).Code == 0 {
					t.Fatal("rechecked a transaction whose payer holds nothing")
				}
			}
		})
	}
}
