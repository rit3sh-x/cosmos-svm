package rpc

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"sync"
	"testing"

	"github.com/mr-tron/base58"

	"cosmossdk.io/log/v2"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

func transactionParam(t *testing.T, sig [64]byte) []jsontext.Value {
	raw, _ := json.Marshal(base58.Encode(sig[:]), json.Deterministic(true))
	cfg, _ := json.Marshal(map[string]any{"encoding": "base64"}, json.Deterministic(true))
	return []jsontext.Value{raw, cfg}
}

func signatureOf(tx []byte) [64]byte { return [64]byte(tx[1:65]) }

// TestHistoryStaysUnderBudget checks that history stays inside its byte budget
// on every block. The oldest heights go first, and a purged transaction reads
// as null.
func TestHistoryStaysUnderBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("runs 10,000 blocks")
	}
	const budget = 64 * 1024
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{IndexMaxBytes: budget})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()

	var sigs [][64]byte
	for i := 0; i < 10_000; i++ {
		to := tc.Pub(ed25519.NewKeyFromSeed(append(make([]byte, 28), byte(i>>24), byte(i>>16), byte(i>>8), byte(i))))
		tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, to, 1_000_000_000))
		if res, _ := c.Block(tx); res[0].Code != 0 {
			t.Fatalf("height %d: code %d", c.Height, res[0].Code)
		}
		if err := s.notify(uint64(c.Height)); err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, signatureOf(tx))
		if got := c.App.History.Bytes(); got > budget {
			t.Fatalf("height %d: history holds %d bytes, budget %d", c.Height, got, budget)
		}
	}

	present := func(sig [64]byte) bool {
		out, err := getTransaction(context.Background(), s, transactionParam(t, sig))
		if err != nil {
			t.Fatal(err)
		}
		return out != nil
	}
	if present(sigs[0]) {
		t.Fatal("the first transaction survived 10,000 blocks under a 64 KiB budget")
	}
	if !present(sigs[len(sigs)-1]) {
		t.Fatal("the latest transaction is missing")
	}
	// Purges go oldest first, so once one transaction is present, every later
	// one is.
	seen := false
	for i, sig := range sigs {
		if p := present(sig); seen && !p {
			t.Fatalf("transaction %d purged after an older one was kept", i)
		} else if p {
			seen = true
		}
	}
}

// TestGetTransactionCarriesLogs checks that getTransaction returns logs,
// balances, the fee and the compute units.
func TestGetTransactionCarriesLogs(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()
	tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), 1_000_000_000))
	c.Block(tx)
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	out, err := getTransaction(context.Background(), s, transactionParam(t, signatureOf(tx)))
	if err != nil || out == nil {
		t.Fatalf("getTransaction: %v, %v", out, err)
	}
	raw, _ := json.Marshal(out, json.Deterministic(true))
	var got struct {
		Slot uint64 `json:"slot"`
		Meta struct {
			Fee                  uint64   `json:"fee"`
			PreBalances          []uint64 `json:"preBalances"`
			PostBalances         []uint64 `json:"postBalances"`
			LogMessages          []string `json:"logMessages"`
			ComputeUnitsConsumed uint64   `json:"computeUnitsConsumed"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Slot != uint64(c.Height) || got.Meta.Fee != 5_000 || len(got.Meta.LogMessages) == 0 ||
		len(got.Meta.PreBalances) != 3 || got.Meta.PreBalances[0]-got.Meta.PostBalances[0] != 1_000_005_000 || got.Meta.ComputeUnitsConsumed == 0 {
		t.Fatalf("getTransaction returned %s", raw)
	}
}

// TestTheHistoryIsReadBesideTheBlockPath reads the history horizon on an RPC
// goroutine while FinalizeBlock purges below it. The nightly job runs it under
// -race, which reports an unsynchronized horizon. On any run, the first
// available block only moves up.
func TestTheHistoryIsReadBesideTheBlockPath(t *testing.T) {
	faucet := tc.Key(1)
	// The budget is small, so every block purges and moves the horizon.
	c := tc.Start(t, tc.Pub(faucet), tc.Options{IndexMaxBytes: 4_000})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()
	first := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest(0), svm.MinimumBalance(0)))
	if res, _ := c.Block(first); res[0].Code != 0 {
		t.Fatalf("the first transfer: code %d", res[0].Code)
	}
	payer := tc.Pub(faucet)
	address, _ := json.Marshal(base58.Encode(payer[:]), json.Deterministic(true))
	transaction := transactionParam(t, signatureOf(first))

	stop, reading := make(chan struct{}), make(chan struct{})
	read := sync.OnceFunc(func() { close(reading) })
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer read()
		var seen uint64
		for {
			got, err := getFirstAvailableBlock(context.Background(), s, nil)
			if err != nil {
				t.Error(err)
				return
			}
			if got.(uint64) < seen {
				t.Errorf("the first available block moved down from %d to %d", seen, got)
			}
			seen = got.(uint64)
			at, _ := json.Marshal(seen, json.Deterministic(true))
			_, _ = getBlock(context.Background(), s, []jsontext.Value{at})
			_, _ = getSignaturesForAddress(context.Background(), s, []jsontext.Value{address})
			_, _ = getTransaction(context.Background(), s, transaction)
			read()
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	halt := sync.OnceFunc(func() { close(stop); wg.Wait() })
	defer halt()
	<-reading

	for i := 1; i <= 40; i++ {
		tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest(i), svm.MinimumBalance(0)))
		if res, _ := c.Block(tx); res[0].Code != 0 {
			t.Fatalf("block %d: code %d", i, res[0].Code)
		}
	}
	halt()
	if got := c.App.History.FirstAvailableBlock(); got <= 2 {
		t.Fatalf("40 blocks under a 4,000-byte budget purged nothing: first available block %d", got)
	}
}
