package svm

import (
	"math"
	"testing"
)

// TestPrepareReadsTheCostSelectAlreadyCharged proves that the cost rides the
// iterator with the gossiped bytes, so a block costs one engine crossing per
// candidate, not two.
//
// The transaction here is nil, so the proof is structural. Anything that
// reached the engine would panic and not return.
func TestPrepareReadsTheCostSelectAlreadyCharged(t *testing.T) {
	var p Proposals
	g, err := p.price(nil, 0, gossiped{units: 7, allocated: 9})
	if err != nil || g.units != 7 || g.allocated != 9 {
		t.Fatalf("a ranked candidate was re-costed as %d, %d, %v", g.units, g.allocated, err)
	}
}

// TestTheRankingKeyIsAgavesWithOurNumerator proves that the key uses the same
// formula as `engine/src/priority.rs`. It saturates where Agave's saturates, so
// a fee large enough to overflow the multiply cannot wrap around and rank last.
func TestTheRankingKeyIsAgavesWithOurNumerator(t *testing.T) {
	for _, c := range []struct {
		fee, units, want uint64
	}{
		{5_000, 28_109, 177_872},
		{0, 28_109, 0},
		{5_001, 97_110, 51_497},
		// A zero-cost transaction divides by one rather than by zero.
		{5_000, 0, 5_000_000_000},
		// Past the multiply, the product saturates instead of wrapping.
		{math.MaxUint64, 999, math.MaxUint64 / 1_000},
	} {
		if got := priority(c.fee, c.units); got != c.want {
			t.Fatalf("priority(%d, %d) is %d, want %d", c.fee, c.units, got, c.want)
		}
	}
	// The property the market depends on: the same fee over a larger cost ranks
	// lower, and a larger fee over the same cost ranks higher.
	if priority(5_000, 97_110) >= priority(5_000, 28_109) {
		t.Fatal("a wasteful shape did not rank below a cheap one at the same fee")
	}
	if priority(6_400, 28_109) <= priority(5_000, 28_109) {
		t.Fatal("a bid did not raise the key")
	}
}
