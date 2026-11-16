package app_test

import (
	"testing"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestPushAccountsReadsEachNameOnce proves that PushAccounts writes one arena
// record for an account named more than once, as a ProgramData that a
// transaction also names is. Otherwise the engine sees the account twice, and a
// block pays twice for one name.
func TestPushAccountsReadsEachNameOnce(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	ms := c.App.CommitMultiStore()
	k := tc.Pub(faucet)

	w := ffi.NewArenaWriter(ffi.KindSimulateReq, 0)
	defer w.Unpin()
	c.App.Stores.PushAccounts(w, ms, [][32]byte{k, k, k}, svm.ArenaBytes, nil)
	r, err := ffi.NewArenaReader(w.Finish(), ffi.KindSimulateReq)
	if err != nil {
		t.Fatal(err)
	}
	if r.Len() != 1 {
		t.Fatalf("one account named three times pushed %d records, want 1", r.Len())
	}
}
