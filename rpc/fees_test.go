package rpc

import (
	"context"
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"runtime"
	"testing"

	"cosmossdk.io/log/v2"

	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// computeUnitPrice is ComputeBudgetInstruction::SetComputeUnitPrice.
func computeUnitPrice(microlamports uint64) tc.Instruction {
	return tc.Instruction{
		Program: mustKey("ComputeBudget111111111111111111111111111111"),
		Data:    binary.LittleEndian.AppendUint64([]byte{3}, microlamports),
	}
}

// TestRecentPrioritizationFeesReportTheBlockMinimum checks that each value is
// the block's minimum declared compute unit price, not the ranking key. A
// wallet feeds the value back into setComputeUnitPrice.
func TestRecentPrioritizationFeesReportTheBlockMinimum(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()

	// An empty block has no market, so the method does not report it.
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	if got := fees(t, s, nil); len(got) != 0 {
		t.Fatalf("an empty block reported %v", got)
	}

	dest := tc.Pub(tc.Key(7))
	amount := svm.MinimumBalance(0)
	for _, block := range [][]uint64{{2_000, 5_000}, {9_000}} {
		var txs [][]byte
		for _, price := range block {
			txs = append(txs, tc.Tx(c.Latest(), faucet, nil,
				tc.Transfer(faucet, dest, amount), computeUnitPrice(price)))
		}
		if res, _ := c.Block(txs...); res[0].Code != 0 {
			t.Fatalf("a priced transfer committed as %d", res[0].Code)
		}
		if err := s.notify(uint64(c.Height)); err != nil {
			t.Fatal(err)
		}
	}

	got := fees(t, s, nil)
	if len(got) != 2 {
		t.Fatalf("two blocks with transactions reported %v", got)
	}
	if got[0].PrioritizationFee != 2_000 || got[1].PrioritizationFee != 9_000 {
		t.Fatalf("the reported minima are %v", got)
	}
	if got[0].Slot >= got[1].Slot {
		t.Fatalf("the window is not oldest first: %v", got)
	}

	// The method accepts and validates the address list. It refuses 129
	// addresses, as getMultipleAccounts refuses 101 keys.
	if _, err := getRecentPrioritizationFees(context.Background(), s, params(t, []string{base58.Encode(dest[:])})); err != nil {
		t.Fatalf("an address list was refused: %v", err)
	}
	if _, err := getRecentPrioritizationFees(context.Background(), s, params(t, []string{"not a key"})); err == nil {
		t.Fatal("a malformed address was accepted")
	}
	many := make([]string, 129)
	for i := range many {
		many[i] = base58.Encode(dest[:])
	}
	if _, err := getRecentPrioritizationFees(context.Background(), s, params(t, many)); err == nil {
		t.Fatal("129 addresses were accepted")
	}
}

// TestFollowDecodesNoRecordsNobodyReads checks that Follow decodes none of the
// records of a height when no signature or logs subscriber exists. The fee
// window then takes the block minimum that the runner published. For a height
// that the feed does not hold, the window takes the minimum from the records.
func TestFollowDecodesNoRecordsNobodyReads(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()
	dest := tc.Pub(tc.Key(7))
	block := func(prices ...uint64) uint64 {
		var txs [][]byte
		for _, price := range prices {
			txs = append(txs, tc.Tx(c.Latest(), faucet, nil,
				tc.Transfer(faucet, dest, svm.MinimumBalance(0)), computeUnitPrice(price)))
		}
		if res, _ := c.Block(txs...); res[0].Code != 0 {
			t.Fatalf("a priced transfer committed as %d", res[0].Code)
		}
		return uint64(c.Height)
	}
	// The prices are whole lamports apart. Agave derives the price back from
	// the priority fee in lamports, so 8,985 to 9,000 all read 9,000.
	prices := make([]uint64, 16)
	for i := range prices {
		prices[i] = 35_000 - 1_000*uint64(i)
	}
	height := block(prices...)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	records, err := s.history().AtHeight(height)
	runtime.ReadMemStats(&after)
	if err != nil || len(records) != len(prices) {
		t.Fatalf("the history holds %d records at %d: %v", len(records), height, err)
	}
	decode := after.TotalAlloc - before.TotalAlloc
	runtime.ReadMemStats(&before)
	if err := s.notify(height); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	notified := after.TotalAlloc - before.TotalAlloc
	t.Logf("%d records decode in %d bytes; notify with no subscriber allocated %d", len(records), decode, notified)
	if notified*uint64(len(records)) > decode {
		t.Fatalf("notify allocated %d bytes, more than decoding one of the %d records (%d bytes for all)", notified, len(records), decode)
	}

	// The height that the runner published is gone, as when Follow falls
	// behind by more than the feed bound. The price then comes from the
	// records.
	dropped := block(4_000, 3_000)
	if _, held := c.App.Feed.Take(dropped); !held {
		t.Fatal("the feed did not hold the height it just executed")
	}
	if err := s.notify(dropped); err != nil {
		t.Fatal(err)
	}
	got := fees(t, s, nil)
	want := []prioritizationFee{{Slot: height, PrioritizationFee: 20_000}, {Slot: dropped, PrioritizationFee: 3_000}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the window is %v, want %v", got, want)
	}
}

// TestTheFeeWindowIsAHundredAndFiftyBlocks checks that the window holds the
// last 150 blocks, a block count and not a duration.
func TestTheFeeWindowIsAHundredAndFiftyBlocks(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())

	s.mu.Lock()
	for height := uint64(1); height <= 400; height++ {
		s.observeFees(height, height, true)
	}
	s.mu.Unlock()
	got := fees(t, s, nil)
	if len(got) != recentPrioritizationBlocks {
		t.Fatalf("the window holds %d blocks, want %d", len(got), recentPrioritizationBlocks)
	}
	if got[0].Slot != 400-recentPrioritizationBlocks+1 || got[len(got)-1].Slot != 400 {
		t.Fatalf("the window is slots %d to %d", got[0].Slot, got[len(got)-1].Slot)
	}
}

func fees(t *testing.T, s *Server, addresses []string) []prioritizationFee {
	t.Helper()
	out, err := getRecentPrioritizationFees(context.Background(), s, params(t, addresses))
	if err != nil {
		t.Fatal(err)
	}
	// The round trip through JSON makes the test check the wire shape.
	raw, err := json.Marshal(out, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	var got []prioritizationFee
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func params(t *testing.T, addresses []string) []jsontext.Value {
	t.Helper()
	if addresses == nil {
		return nil
	}
	raw, err := json.Marshal(addresses, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return []jsontext.Value{raw}
}

// TestRentExemptionRefusesALengthNoAccountHas checks Agave's bound on
// getMinimumBalanceForRentExemption. A length above the largest account a
// program can allocate is an invalid request. Without the bound, 2^64-1
// overflows the price and returns a small number.
func TestRentExemptionRefusesALengthNoAccountHas(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()
	s := New(c.App, nil, log.NewNopLogger())
	rent := func(n uint64) (any, error) {
		raw, _ := json.Marshal(n, json.Deterministic(true))
		return getMinimumBalanceForRentExemption(context.Background(), s, []jsontext.Value{raw})
	}
	if got, err := rent(maxPermittedDataLength); err != nil || got != svm.MinimumBalance(maxPermittedDataLength) {
		t.Fatalf("the largest account: %v, %v, want %d", got, err, svm.MinimumBalance(maxPermittedDataLength))
	}
	for _, n := range []uint64{maxPermittedDataLength + 1, ^uint64(0)} {
		_, err := rent(n)
		var refused *rpcError
		if !errors.As(err, &refused) || refused.Code != codeInvalidRequest || refused.Message != "Invalid request" {
			t.Fatalf("%d bytes: %v, want Agave's invalid request", n, err)
		}
	}
}
