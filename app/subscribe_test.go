package app_test

import (
	"testing"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestSubscribersDoNotMoveTheAppHash proves that subscribers never change the
// app hash. Two machines fed identical blocks agree at every height, however
// many subscribers one of them serves, and after the subscribers leave.
func TestSubscribersDoNotMoveTheAppHash(t *testing.T) {
	faucet := tc.Key(1)
	quiet := tc.Start(t, tc.Pub(faucet), tc.Options{})
	watched := tc.Start(t, tc.Pub(faucet), tc.Options{})

	// Subscriber counts, block by block: none, one, many, none, a few, none.
	held := []int{0, 1, 512, 0, 3, 0}
	buffered := 0
	for i, count := range held {
		dest := tc.Pub(tc.Key(byte(40 + i)))
		for j := 0; j < count; j++ {
			watched.App.Feed.Hold(dest, false)
		}
		if watched.App.Feed.Wants() != (count > 0) {
			t.Fatalf("block %d: %d subscribers report Wants %v", i, count, watched.App.Feed.Wants())
		}
		var txs [][]byte
		for k := 0; k < 3; k++ {
			txs = append(txs, tc.Tx(tc.HashAt(int64(i+1)), faucet, nil, tc.Transfer(faucet, dest, svm.MinimumBalance(0)+uint64(k))))
		}
		res, quietHash := quiet.Block(txs...)
		for k, r := range res {
			if r.Code != 0 {
				t.Fatalf("block %d transaction %d: code %d", i, k, r.Code)
			}
		}
		_, watchedHash := watched.Block(txs...)
		if string(quietHash) != string(watchedHash) {
			t.Fatalf("block %d with %d subscribers: app hash %x, want %x", i, count, watchedHash, quietHash)
		}
		// Nothing drains the feed here. So a block adds the one account that
		// its subscribers watch, but only when a subscriber holds the feed.
		if count > 0 {
			buffered++
		}
		if got := watched.App.Feed.Buffered(); got != buffered {
			t.Fatalf("block %d with %d subscribers buffered %d accounts, want %d", i, count, got, buffered)
		}
		for j := 0; j < count; j++ {
			watched.App.Feed.Release(dest, false)
		}
	}
}
