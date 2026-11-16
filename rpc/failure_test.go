package rpc

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"testing"

	"github.com/mr-tron/base58"

	"cosmossdk.io/log/v2"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// negativeLamports is the err of a transfer of more lamports than the payer
// has. Agave's System program returns error 1, ResultWithNegativeLamports, from
// instruction 0, and an Agave RPC renders it as this text.
const negativeLamports = `{"InstructionError":[0,{"Custom":1}]}`

// TestAFailedTransactionSaysWhichErrorItWas checks that getTransaction and
// getSignatureStatuses serve the same error bytes. Both read the text that the
// engine rendered once.
func TestAFailedTransactionSaysWhichErrorItWas(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()

	tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), tc.FaucetLamports*2))
	res, _ := c.Block(tx)
	if res[0].Code == 0 {
		t.Fatal("a transfer of more than the payer holds committed")
	}
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}

	out, err := getTransaction(context.Background(), s, transactionParam(t, signatureOf(tx)))
	if err != nil || out == nil {
		t.Fatalf("getTransaction: %v, %v", out, err)
	}
	raw, _ := json.Marshal(out, json.Deterministic(true))
	var got struct {
		Meta struct {
			Err    jsontext.Value `json:"err"`
			Status jsontext.Value `json:"status"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if string(got.Meta.Err) != negativeLamports {
		t.Fatalf("getTransaction err is %s, want %s", got.Meta.Err, negativeLamports)
	}
	if want := `{"Err":` + negativeLamports + `}`; string(got.Meta.Status) != want {
		t.Fatalf("getTransaction status is %s, want %s", got.Meta.Status, want)
	}

	sig := signatureOf(tx)
	sigs, _ := json.Marshal([]string{base58.Encode(sig[:])}, json.Deterministic(true))
	statuses, err := getSignatureStatuses(context.Background(), s, []jsontext.Value{sigs})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(statuses, json.Deterministic(true))
	var value struct {
		Value []struct {
			Err    jsontext.Value `json:"err"`
			Status jsontext.Value `json:"status"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if len(value.Value) != 1 || string(value.Value[0].Err) != negativeLamports {
		t.Fatalf("getSignatureStatuses returned %s", raw)
	}

	// The stored rendering survives a restart. History returns the text it
	// wrote and never re-executes, because the binary that ran it may be gone.
	c.Restart()
	s = New(c.App, nil, log.NewNopLogger())
	out, err = getTransaction(context.Background(), s, transactionParam(t, signatureOf(tx)))
	if err != nil || out == nil {
		t.Fatalf("getTransaction after a restart: %v, %v", out, err)
	}
	raw, _ = json.Marshal(out, json.Deterministic(true))
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if string(got.Meta.Err) != negativeLamports {
		t.Fatalf("after a restart the err is %s, want %s", got.Meta.Err, negativeLamports)
	}
}

// TestSimulateSaysWhichErrorItWas checks the err that simulateTransaction
// renders for a failure, for an error with a payload and for a success. The
// return data is record 1 after the head and the logs follow, so missing logs
// mean the records moved.
func TestSimulateSaysWhichErrorItWas(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()

	tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), tc.FaucetLamports*2))
	encoded, _ := json.Marshal(base64.StdEncoding.EncodeToString(tx), json.Deterministic(true))
	cfg, _ := json.Marshal(map[string]any{"encoding": "base64"}, json.Deterministic(true))
	out, err := simulateTransaction(context.Background(), s, []jsontext.Value{encoded, cfg})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out, json.Deterministic(true))
	var got struct {
		Value struct {
			Err  jsontext.Value `json:"err"`
			Logs []string       `json:"logs"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if string(got.Value.Err) != negativeLamports {
		t.Fatalf("simulateTransaction err is %s, want %s", got.Value.Err, negativeLamports)
	}
	if len(got.Value.Logs) == 0 {
		t.Fatalf("the log lines moved: %s", raw)
	}

	// An error variant with a payload renders its field. 1,000 lamports is
	// below the rent minimum for a new account, and account_index names it.
	poor := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(3)), 1_000))
	encoded, _ = json.Marshal(base64.StdEncoding.EncodeToString(poor), json.Deterministic(true))
	out, err = simulateTransaction(context.Background(), s, []jsontext.Value{encoded, cfg})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(out, json.Deterministic(true))
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if want := `{"InsufficientFundsForRent":{"account_index":1}}`; string(got.Value.Err) != want {
		t.Fatalf("simulateTransaction err is %s, want %s", got.Value.Err, want)
	}

	// A successful simulation returns a null err.
	ok := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), svm.MinimumBalance(0)))
	encoded, _ = json.Marshal(base64.StdEncoding.EncodeToString(ok), json.Deterministic(true))
	out, err = simulateTransaction(context.Background(), s, []jsontext.Value{encoded, cfg})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(out, json.Deterministic(true))
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Value.Err != nil && string(got.Value.Err) != "null" {
		t.Fatalf("a successful simulation reported %s", got.Value.Err)
	}
}
