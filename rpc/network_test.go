package rpc

import (
	"context"
	"encoding/json/jsontext"
	"testing"

	"cosmossdk.io/log/v2"

	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestTheSharedTableHoldsTheNoFaucetRefusal checks that the method table holds
// only Agave's answer for a node without a faucet. The faucet uses a per-build
// hook, because it is the only method that reads the caller's address. The
// table is one map that every Server in the process shares.
func TestTheSharedTableHoldsTheNoFaucetRefusal(t *testing.T) {
	_, err := methods["requestAirdrop"](context.Background(), nil, nil)
	if re := asRPCError(err); err == nil || re.Code != codeInvalidRequest || re.Message != "Invalid request" {
		t.Fatalf("the table's requestAirdrop answered %v", err)
	}
}

// TestAProductionBuildRefusesRequestAirdropAsAgaveWithoutAFaucet checks that a
// production build answers requestAirdrop as Agave does without a faucet. A
// production build does not compile the faucet.
func TestAProductionBuildRefusesRequestAirdropAsAgaveWithoutAFaucet(t *testing.T) {
	if svm.Network == "devnet" {
		t.Skip("this is the devnet build")
	}
	if _, ok := devnetMethod("requestAirdrop", "10.0.0.1:1"); ok {
		t.Fatal("a production build served requestAirdrop")
	}
	// Agave registers the method in every build and refuses it before it
	// reads a parameter when it has no faucet.
	s := &Server{logger: log.NewNopLogger()}
	params := []jsontext.Value{jsontext.Value(`"11111111111111111111111111111111"`), jsontext.Value(`1`)}
	resp := callOne(s, context.Background(), call{v2: true, id: []byte("1"), method: "requestAirdrop", params: encode(params)}, "10.0.0.1:1")
	if resp.Error == nil || resp.Error.Code != codeInvalidRequest || resp.Error.Message != "Invalid request" {
		t.Fatalf("requestAirdrop answered %+v, want Agave's -32600 Invalid request", resp.Error)
	}
}
