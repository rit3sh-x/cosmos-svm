package app_test

import (
	"bytes"
	"testing"

	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

func txCount(c *tc.Chain) uint64 { return c.App.Stores.TransactionCount(c.App.CommitMultiStore()) }

// TestTheTransactionCountCountsProcessedTransactions proves that a success and
// a failure that paid its fee count. A transaction that the block did not
// process and a Cosmos-lane transaction do not count.
func TestTheTransactionCountCountsProcessedTransactions(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	if txCount(c) != 0 {
		t.Fatalf("the count is %d at genesis, want 0", txCount(c))
	}
	ok := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), svm.MinimumBalance(0)))
	failed := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(3)), ^uint64(0)>>1))
	stale := tc.Tx(tc.HashAt(999_999), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(4)), 1))
	op := c.Operators[0]
	cosmos := c.Cosmos(tc.OperatorKey(0), &banktypes.MsgSend{FromAddress: op.Account.String(), ToAddress: op.Account.String(),
		Amount: sdk.NewCoins(sdk.NewInt64Coin("stake", 1))})
	res, _ := c.Deliver(c.Wrap(ok), c.Wrap(failed), c.Wrap(stale), cosmos)
	if res[0].Code != 0 || res[1].Code == 0 || res[1].Code == 2 || res[2].Code != 2 {
		t.Fatalf("codes %d %d %d, want a success, a paid failure and one not processed", res[0].Code, res[1].Code, res[2].Code)
	}
	if got := txCount(c); got != 2 {
		t.Fatalf("the count is %d after a success, a paid failure, an unprocessed one and a Cosmos one, want 2", got)
	}
}

// TestTheTransactionCountIsInTheAppHash changes the counter on one of two
// identical chains and checks that the next block's app hash differs.
func TestTheTransactionCountIsInTheAppHash(t *testing.T) {
	faucet := tc.Key(1)
	a := tc.Start(t, tc.Pub(faucet), tc.Options{})
	b := tc.Start(t, tc.Pub(faucet), tc.Options{})
	a.Block()
	b.Block()
	ctx := b.App.NewUncachedContext(false, cmtproto.Header{Height: b.Height})
	ctx.MultiStore().GetKVStore(b.App.Stores.Meta).Set(svm.MetaTransactionCountKey, []byte{0, 0, 0, 0, 0, 0, 0, 7})
	_, ha := a.Block()
	_, hb := b.Block()
	if bytes.Equal(ha, hb) {
		t.Fatal("two chains that differ only in the counter have one app hash")
	}
}
