package svm

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	errorsmod "cosmossdk.io/errors"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

// TestARefusalAnswersAsWrapDoes proves that refused answers, field by field,
// as the SDK answers err.Wrap(desc) with debug off. It covers each error that
// CheckTx wraps, with an empty reason, a reason like the engine gives, and
// reasons that hold a colon, a newline and format verbs.
func TestARefusalAnswersAsWrapDoes(t *testing.T) {
	for _, err := range []*errorsmod.Error{ErrAdmission, ErrBlockhashNotFound, ErrUnpayable, ErrHost} {
		for _, desc := range []string{"", "insufficient funds for fee: 5000 < 10000", "two\nlines", "%s %d %v"} {
			want := sdkerrors.ResponseCheckTxWithEvents(err.Wrap(desc), 0, 0, nil, false)
			if got := refused(err, desc); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s, %q: refused answers %+v, Wrap %+v", err, desc, got, want)
			}
		}
	}
}

// The sidecar writes run in order off the block's goroutine. A failed one
// stops the rest. Its error or panic reaches the goroutine that joins them,
// which is the one that commits the block.
func TestBackgroundAnswersOnTheJoiningGoroutine(t *testing.T) {
	var ran []int
	full := errors.New("disk full")
	join := Background(
		func() error { ran = append(ran, 1); return nil },
		func() error { ran = append(ran, 2); return full },
		func() error { ran = append(ran, 3); return nil })
	if err := join(); !errors.Is(err, full) || !slices.Equal(ran, []int{1, 2}) {
		t.Fatalf("the join answered %v after writes %v, want %v after [1 2]", err, ran, full)
	}
	join = Background(func() error { panic("short write") })
	defer func() {
		if r := recover(); r != "short write" {
			t.Fatalf("the join raised %v, want the write's panic", r)
		}
	}()
	join()
	t.Fatal("the join returned after the write panicked")
}
