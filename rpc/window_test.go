package rpc

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"testing"

	"github.com/mr-tron/base58"

	"cosmossdk.io/log/v2"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// A blockhash from height 1,000 stays valid through height 1,150.
func TestLastValidBlockHeight(t *testing.T) {
	if got := lastValidBlockHeight(1_000); got != 1_150 {
		t.Fatalf("lastValidBlockHeight(1000) = %d, want 1150", got)
	}
}

// TestIsBlockhashValidAgreesWithInclusion proves isBlockhashValid at a
// committed height answers for the next block. So at the window's edge it
// agrees with whether that block includes the transaction.
func TestIsBlockhashValidAgreesWithInclusion(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()
	first := c.Latest()
	valid := func() bool {
		t.Helper()
		param, _ := json.Marshal(base58.Encode(first[:]), json.Deterministic(true))
		out, err := isBlockhashValid(context.Background(), s, []jsontext.Value{param})
		if err != nil {
			t.Fatal(err)
		}
		return out.(withContext).Value.(bool)
	}

	for c.Height < 150 {
		c.Block()
	}
	if !valid() {
		t.Fatalf("isBlockhashValid false at committed height %d", c.Height)
	}
	if res, _ := c.Block(tc.Tx(first, faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), 1_000_000_000))); res[0].Code != 0 {
		t.Fatalf("height %d did not include a transaction isBlockhashValid accepted: code %d", c.Height, res[0].Code)
	}
	if valid() {
		t.Fatalf("isBlockhashValid true at committed height %d", c.Height)
	}
	if res, _ := c.Block(tc.Tx(first, faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(3)), 1_000_000_000))); res[0].Code != 2 {
		t.Fatalf("height %d included a transaction isBlockhashValid rejected: code %d", c.Height, res[0].Code)
	}
}
