package rpc

import (
	"context"
	"testing"
	"time"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestTheCounterAndTheSampler checks that getTransactionCount and getEpochInfo
// read the counter, and that each sample records how far the height and the
// counter moved. getRecentPerformanceSamples answers newest first, at most 720.
func TestTheCounterAndTheSampler(t *testing.T) {
	c, s := server(t)
	faucet := tc.Key(1)
	clock := &stubClock{now: time.Unix(1_700_000_000, 0), waits: make(chan time.Duration), fire: make(chan time.Time)}
	s.perfClock = clock
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.SamplePerformance(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	// The sampler has read its starting point once it asks to wait.
	if d := <-clock.waits; d != sampleInterval {
		t.Fatalf("the sampler waits %v, want %v", d, sampleInterval)
	}
	from := uint64(c.Height)
	for i := 0; i < 3; i++ {
		c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(byte(20+i))), svm.MinimumBalance(0))))
	}
	clock.advance(61 * time.Second)
	clock.fire <- clock.Now()
	// It has handed the sample over once it asks to wait again.
	<-clock.waits
	c.Block()
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}

	var count uint64
	mustAnswer(t, s, &count, "getTransactionCount")
	if count != 3 {
		t.Fatalf("getTransactionCount is %d, want 3", count)
	}
	var epoch map[string]any
	mustAnswer(t, s, &epoch, "getEpochInfo")
	if epoch["transactionCount"] != float64(3) {
		t.Fatalf("getEpochInfo.transactionCount is %v", epoch["transactionCount"])
	}
	wantErr(t, s, codeMinContextSlot, "", "getTransactionCount", map[string]any{"minContextSlot": c.Height + 1})

	var samples []map[string]uint64
	mustAnswer(t, s, &samples, "getRecentPerformanceSamples")
	if len(samples) != 1 {
		t.Fatalf("%d samples, want 1", len(samples))
	}
	got := samples[0]
	if got["slot"] != from+3 || got["numSlots"] != 3 || got["numTransactions"] != 3 ||
		got["numNonVoteTransactions"] != 3 || got["samplePeriodSecs"] != 61 {
		t.Fatalf("the sample is %v, want 3 slots and 3 transactions from %d over 61 s", got, from)
	}

	// A second period, and the newest comes first.
	clock.advance(60 * time.Second)
	clock.fire <- clock.Now()
	<-clock.waits
	c.Block()
	mustAnswer(t, s, &samples, "getRecentPerformanceSamples")
	if len(samples) != 2 || samples[0]["slot"] <= samples[1]["slot"] {
		t.Fatalf("samples %v, want two, newest first", samples)
	}
	mustAnswer(t, s, &samples, "getRecentPerformanceSamples", 1)
	if len(samples) != 1 || samples[0]["numTransactions"] != 0 {
		t.Fatalf("the limit of one gave %v", samples)
	}
	wantErr(t, s, codeInvalidParams, "Invalid limit; max 720", "getRecentPerformanceSamples", 721)
}
