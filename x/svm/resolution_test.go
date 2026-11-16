package svm_test

import (
	"bytes"
	"testing"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestTheNextBlocksContextIsWhatTheBlockWrites proves that admission and the
// block read the same context. At H's header time, the sysvars that H's
// begin-block writes on a branch of the committed state equal the sysvars that
// block H writes, byte for byte. The comparison starts at the second block,
// because genesis reaches the committed store with the first.
func TestTheNextBlocksContextIsWhatTheBlockWrites(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()
	for range 6 {
		ms, _, err := c.App.Next()
		if err != nil {
			t.Fatal(err)
		}
		c.HoldTime()
		c.Block()
		for _, id := range [][32]byte{svm.ClockID, svm.SlotHashesID, svm.RecentBlockhashesID} {
			want, err := ffi.DecodeAccount(c.App.Stores.GetAccount(ms, id))
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := c.Account(id); !ok || !bytes.Equal(want.Data, got.Data) {
				t.Fatalf("height %d: the block wrote sysvar %x unlike its context", c.Height, id[:4])
			}
		}
	}
}
