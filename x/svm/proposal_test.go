package svm

import (
	"math"
	"testing"
)

// TestAccountUnitsIsAgavesTryAdd proves that the per-account count of both
// proposal handlers fits exactly the limit and not one unit more. A
// transaction refused on one account adds nothing to the accounts before it. A
// transaction over the limit on its own fits nowhere, however close to 2^64 it
// declares.
func TestAccountUnitsIsAgavesTryAdd(t *testing.T) {
	s := accountUnits{limit: 100, spent: map[[32]byte]uint64{}}
	hot, cold := [32]byte{1}, [32]byte{2}
	if !s.add(99, [][32]byte{hot}) || !s.add(1, [][32]byte{hot}) {
		t.Fatal("exactly the limit did not fit")
	}
	if s.add(1, [][32]byte{hot}) {
		t.Fatal("one unit over fitted")
	}
	if s.add(10, [][32]byte{cold, hot}) || s.spent[cold] != 0 {
		t.Fatalf("a transaction refused on the full account left %d units on the fresh one", s.spent[cold])
	}
	if s.add(math.MaxUint64, [][32]byte{{3}}) {
		t.Fatal("a transaction over the limit on its own fitted")
	}
	if !s.add(100, [][32]byte{cold}) {
		t.Fatal("the fresh account lost part of its share")
	}
}

// TestABlockBudgetIsCountedDown checks the budget that both proposal handlers
// keep. A charge fits while each part is at most what is left, and what fits
// is spent. So a charge near 2^64 never wraps a sum back under the limit.
func TestABlockBudgetIsCountedDown(t *testing.T) {
	b := blockBudget{units: 10, allocated: 5}
	if !b.fits(gossiped{units: 10, allocated: 5}) {
		t.Fatal("exactly the budget did not fit")
	}
	b.spend(gossiped{units: 4, allocated: 5})
	if !b.fits(gossiped{units: 6}) || b.fits(gossiped{units: 7}) || b.fits(gossiped{allocated: 1}) ||
		b.fits(gossiped{units: math.MaxUint64}) {
		t.Fatalf("%+v left fits the wrong charges", b)
	}
}
