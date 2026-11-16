package app_test

import (
	"testing"

	"github.com/cosmos/cosmos-sdk/baseapp"
	pruningtypes "github.com/cosmos/cosmos-sdk/store/v2/pruning/types"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// TestThePruningIntervalIsOneWhateverAppTomlSays proves that the machine
// prunes at every height, whatever app.toml says. GetPruningOptionsFromFlags
// and LoadLatestVersion each validate the interval, so the app lowers it
// after both. The operator's own keep-recent stays as set.
func TestThePruningIntervalIsOneWhateverAppTomlSays(t *testing.T) {
	for _, interval := range []uint64{10, 100, 5_000} {
		c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{BaseApp: []func(*baseapp.BaseApp){
			baseapp.SetPruning(pruningtypes.NewCustomPruningOptions(2, interval)),
		}})
		got := c.App.CommitMultiStore().GetPruning()
		if got.Interval != 1 {
			t.Fatalf("pruning-interval %d in app.toml gave interval %d, want 1", interval, got.Interval)
		}
		if got.KeepRecent != 2 {
			t.Fatalf("pruning-keep-recent 2 gave %d: the operator's setting must survive", got.KeepRecent)
		}
	}
}
