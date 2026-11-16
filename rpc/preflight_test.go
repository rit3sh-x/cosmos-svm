package rpc

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"cosmossdk.io/log/v2"

	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	cmttypes "github.com/cometbft/cometbft/types"

	"github.com/cosmos/cosmos-sdk/client"

	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// mempool is the node a send reaches. It records what it was handed and
// reports itself caught up.
type mempool struct {
	client.CometRPC
	sent []cmttypes.Tx
}

func (m *mempool) BroadcastTxSync(_ context.Context, tx cmttypes.Tx) (*coretypes.ResultBroadcastTx, error) {
	m.sent = append(m.sent, tx)
	return &coretypes.ResultBroadcastTx{Code: 0}, nil
}

func (m *mempool) Status(context.Context) (*coretypes.ResultStatus, error) {
	return &coretypes.ResultStatus{}, nil
}

func submit(s *Server, tx []byte, cfg map[string]any) (any, error) {
	encoded, _ := json.Marshal(base64.StdEncoding.EncodeToString(tx), json.Deterministic(true))
	cfg["encoding"] = "base64"
	second, _ := json.Marshal(cfg, json.Deterministic(true))
	return sendTransaction(context.Background(), s, []jsontext.Value{encoded, second})
}

// TestASendIsPreflightedAsAgaveDoes proves a send is simulated unless it sets
// skipPreflight, as on Agave. A failing send is refused with -32002 and the
// simulation in its data, and is not broadcast.
func TestASendIsPreflightedAsAgaveDoes(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	node := &mempool{}
	s := New(c.App, node, log.NewNopLogger())

	// One lamport to a new account leaves it below rent. That fails in a
	// block and is charged.
	short := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), 1))
	_, err := submit(s, short, map[string]any{})
	var refused *rpcError
	if !errors.As(err, &refused) || refused.Code != codeSendTransactionFailure ||
		refused.Message != "Transaction simulation failed: Transaction results in an account (1) with insufficient funds for rent" {
		t.Fatalf("a send that fails its simulation: %v, want -32002 with Agave's message", err)
	}
	if data, ok := refused.Data.(map[string]any); !ok || len(data["logs"].([]string)) == 0 || data["err"] == nil {
		t.Fatalf("the refusal's data is %#v, want the simulation with its logs and error", refused.Data)
	}
	if len(node.sent) != 0 {
		t.Fatal("a send that failed preflight was broadcast")
	}

	if _, err := submit(s, short, map[string]any{"skipPreflight": true}); err != nil || len(node.sent) != 1 {
		t.Fatalf("the same send with skipPreflight: %v, %d broadcast, want it broadcast", err, len(node.sent))
	}

	funded := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(3)), svm.MinimumBalance(0)))
	if got, err := submit(s, funded, map[string]any{}); err != nil || got != base58.Encode(funded[1:65]) || len(node.sent) != 2 {
		t.Fatalf("a send that passes preflight: %v, %v, want its signature and a broadcast", got, err)
	}

	forged := append([]byte(nil), funded...)
	forged[1] ^= 1
	// Agave checks the signatures before its health and its simulation, and
	// answers a failure as a simulation that failed.
	if _, err := submit(s, forged, map[string]any{}); !errors.As(err, &refused) || refused.Code != codeSendTransactionFailure ||
		refused.Message != "Transaction simulation failed: Transaction did not pass signature verification" {
		t.Fatalf("a send with a bad signature: %v, want Agave's -32002", err)
	}
	if data, _ := refused.Data.(map[string]any); data["err"] != "SignatureFailure" || data["loadedAddresses"] != nil {
		t.Fatalf("the refusal's data is %#v", refused.Data)
	}
	if got, err := submit(s, forged, map[string]any{"skipPreflight": true}); err != nil || got != base58.Encode(forged[1:65]) {
		t.Fatalf("a bad signature with skipPreflight: %v, %v, want its signature, as Agave answers", got, err)
	}
}

// TestAProcessedTransactionSimulatesAsAlreadyProcessed proves a processed
// transaction simulates as AlreadyProcessed, and a send of it is refused at
// preflight. Agave's simulation checks its status cache after the age check.
func TestAProcessedTransactionSimulatesAsAlreadyProcessed(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	node := &mempool{}
	s := New(c.App, node, log.NewNopLogger())

	transfer := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), svm.MinimumBalance(0)))
	if res, _ := c.Block(transfer); res[0].Code != 0 {
		t.Fatalf("the transfer: code %d", res[0].Code)
	}
	result, err := simulateTransaction(context.Background(), s, simulationParams(transfer))
	if err != nil {
		t.Fatal(err)
	}
	if got := simulatedErr(t, result); got != `"AlreadyProcessed"` {
		t.Fatalf("a processed transfer simulated with err %s, want \"AlreadyProcessed\"", got)
	}
	_, err = submit(s, transfer, map[string]any{})
	var refused *rpcError
	if !errors.As(err, &refused) || refused.Code != codeSendTransactionFailure ||
		refused.Message != "Transaction simulation failed: This transaction has already been processed" {
		t.Fatalf("a processed transfer sent again: %v, want -32002 with Agave's message", err)
	}
	if data, ok := refused.Data.(map[string]any); !ok {
		t.Fatalf("the refusal's data is %#v, want the simulation", refused.Data)
	} else if got, _ := json.Marshal(data["err"], json.Deterministic(true)); string(got) != `"AlreadyProcessed"` {
		t.Fatalf("the refusal's data has err %s, want \"AlreadyProcessed\"", got)
	}
	if len(node.sent) != 0 {
		t.Fatal("a processed transfer was broadcast again")
	}

	next := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(3)), svm.MinimumBalance(0)))
	if got, err := submit(s, next, map[string]any{}); err != nil || got != base58.Encode(next[1:65]) || len(node.sent) != 1 {
		t.Fatalf("the same payer's next transfer: %v, %v, want its signature and a broadcast", got, err)
	}
}

// TestASimulationResolvesLookupTables proves a simulation resolves a v0
// message's lookup tables, which most DEX routing uses. A table that does not
// exist gets Agave's invalid transaction refusal.
func TestASimulationResolvesLookupTables(t *testing.T) {
	faucet := tc.Key(1)
	table, destination := tc.Pub(tc.Key(60)), tc.Pub(tc.Key(61))
	c := tc.Start(t, tc.Pub(faucet), tc.Options{
		Accounts: []svm.GenesisAccount{lookupTable(table, [][32]byte{destination})},
	})
	c.Block()
	s := New(c.App, &mempool{}, log.NewNopLogger())

	through := func(table [32]byte) []byte {
		return tc.TxV0(c.Latest(), faucet, nil,
			[]tc.Lookup{{Table: table, Writable: []tc.LookupEntry{{Index: 0, Key: destination}}}},
			tc.Transfer(faucet, destination, svm.MinimumBalance(0)))
	}
	result, err := simulateTransaction(context.Background(), s, simulationParams(through(table)))
	if err != nil {
		t.Fatal(err)
	}
	if got := simulatedErr(t, result); got != "null" {
		t.Fatalf("a transfer through a table simulated with err %s", got)
	}
	raw, _ := json.Marshal(result, json.Deterministic(true))
	var answer struct {
		Value struct {
			LoadedAddresses struct {
				Writable []string `json:"writable"`
				Readonly []string `json:"readonly"`
			} `json:"loadedAddresses"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatal(err)
	}
	if w := answer.Value.LoadedAddresses.Writable; len(w) != 1 || w[0] != base58.Encode(destination[:]) {
		t.Fatalf("loadedAddresses.writable is %v, want the table's one address", w)
	}

	_, err = simulateTransaction(context.Background(), s, simulationParams(through(tc.Pub(tc.Key(62)))))
	var refused *rpcError
	if !errors.As(err, &refused) || refused.Code != codeInvalidParams ||
		!strings.Contains(refused.Message, "address table account that doesn't exist") {
		t.Fatalf("a table that does not exist: %v, want Agave's invalid transaction", err)
	}
}

// TestAV0MessageWithoutTablesIsAnOrdinaryTransaction proves the send, CheckTx,
// getFeeForMessage and the block all accept a v0 message that names no table.
// @solana/kit sends that shape by default.
func TestAV0MessageWithoutTablesIsAnOrdinaryTransaction(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	s := New(c.App, &mempool{}, log.NewNopLogger())

	to := tc.Pub(tc.Key(2))
	transfer := tc.TxV0(c.Latest(), faucet, nil, nil, tc.Transfer(faucet, to, svm.MinimumBalance(0)))
	if got, err := submit(s, transfer, map[string]any{}); err != nil || got != base58.Encode(transfer[1:65]) {
		t.Fatalf("a send of it: %v, %v, want its signature", got, err)
	}
	if res := c.Check(c.Wrap(transfer), false); res.Code != 0 {
		t.Fatalf("CheckTx: code %d, %s", res.Code, res.Log)
	}
	message, _ := json.Marshal(base64.StdEncoding.EncodeToString(transfer[65:]), json.Deterministic(true))
	fee, err := getFeeForMessage(context.Background(), s, []jsontext.Value{message})
	if raw, _ := json.Marshal(fee, json.Deterministic(true)); err != nil || !strings.Contains(string(raw), `"value":5000`) {
		t.Fatalf("getFeeForMessage: %s, %v, want 5000", raw, err)
	}
	if res, _ := c.Block(transfer); res[0].Code != 0 {
		t.Fatalf("the block: code %d, %s", res[0].Code, res[0].Log)
	}
	if got := c.Lamports(to); got != svm.MinimumBalance(0) {
		t.Fatalf("the recipient holds %d lamports, want %d", got, svm.MinimumBalance(0))
	}
}

// TestEveryBlockHasATime proves getBlock answers the clock's blockTime for a
// block with no SVM transaction, as Agave answers every block's.
func TestEveryBlockHasATime(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()
	c.Block()
	s := New(c.App, nil, log.NewNopLogger())
	height, _ := json.Marshal(c.Height, json.Deterministic(true))
	out, err := getBlock(context.Background(), s, []jsontext.Value{height, jsontext.Value(`{"encoding":"base64","transactionDetails":"none"}`)})
	if err != nil {
		t.Fatal(err)
	}
	block := out.(map[string]any)
	clock, ok := c.Account(svm.ClockID)
	if !ok || len(clock.Data) < 40 {
		t.Fatal("no clock")
	}
	want := int64(binary.LittleEndian.Uint64(clock.Data[32:40]))
	if block["blockTime"] != want {
		t.Fatalf("an empty block's blockTime is %v, want the clock's %d", block["blockTime"], want)
	}
}
