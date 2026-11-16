package rpc

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"strconv"
	"strings"
	"testing"

	"cosmossdk.io/log/v2"

	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

type uiBlock struct {
	PreviousBlockhash string  `json:"previousBlockhash"`
	Blockhash         string  `json:"blockhash"`
	ParentSlot        uint64  `json:"parentSlot"`
	BlockTime         *int64  `json:"blockTime"`
	BlockHeight       *uint64 `json:"blockHeight"`
	Transactions      *[]struct {
		Transaction []string `json:"transaction"`
		Version     any      `json:"version"`
		Meta        struct {
			Err  jsontext.Value `json:"err"`
			Fee  uint64         `json:"fee"`
			Logs []string       `json:"logMessages"`
		} `json:"meta"`
	} `json:"transactions"`
	Signatures *[]string `json:"signatures"`
	Rewards    *[]any    `json:"rewards"`
}

func block(t *testing.T, s *Server, height uint64, cfg map[string]any) (uiBlock, error) {
	t.Helper()
	first, _ := json.Marshal(height, json.Deterministic(true))
	params := []jsontext.Value{first}
	if cfg != nil {
		second, _ := json.Marshal(cfg, json.Deterministic(true))
		params = append(params, second)
	}
	out, err := getBlock(context.Background(), s, params)
	if err != nil {
		return uiBlock{}, err
	}
	raw, _ := json.Marshal(out, json.Deterministic(true))
	var b uiBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	return b, nil
}

// TestGetBlockServesAHeight checks getBlock on a committed height, an empty
// height and a height that no block has reached.
func TestGetBlockServesAHeight(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()

	// One block with a success and a failure, in that order.
	good := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), svm.MinimumBalance(0)))
	bad := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(3)), tc.FaucetLamports*2))
	res, _ := c.Block(good, bad)
	if res[0].Code != 0 || res[1].Code == 0 {
		t.Fatalf("the block committed as %d and %d", res[0].Code, res[1].Code)
	}
	height := uint64(c.Height)
	if err := s.notify(height); err != nil {
		t.Fatal(err)
	}

	b, err := block(t, s, height, map[string]any{"encoding": "base64", "maxSupportedTransactionVersion": 0})
	if err != nil {
		t.Fatal(err)
	}
	if b.Blockhash != base58.Encode(c.Hashes[height-1]) {
		t.Fatalf("blockhash is %s", b.Blockhash)
	}
	if b.PreviousBlockhash != base58.Encode(c.Hashes[height-2]) {
		t.Fatalf("previousBlockhash is %s", b.PreviousBlockhash)
	}
	if b.ParentSlot != height-1 || b.BlockHeight == nil || *b.BlockHeight != height {
		t.Fatalf("parentSlot %d, blockHeight %v", b.ParentSlot, b.BlockHeight)
	}
	if b.BlockTime == nil || *b.BlockTime == 0 {
		t.Fatalf("blockTime is %v", b.BlockTime)
	}
	if b.Transactions == nil || len(*b.Transactions) != 2 {
		t.Fatalf("the block carries %v transactions", b.Transactions)
	}
	// The transactions are in block order, not history order, and the failure
	// carries its error.
	txs := *b.Transactions
	if txs[0].Transaction[1] != "base64" || txs[0].Meta.Fee != 5_000 || len(txs[0].Meta.Logs) == 0 {
		t.Fatalf("the first transaction is %+v", txs[0])
	}
	if txs[0].Meta.Err != nil && string(txs[0].Meta.Err) != "null" {
		t.Fatalf("the successful transaction reports %s", txs[0].Meta.Err)
	}
	if want := `{"InstructionError":[0,{"Custom":1}]}`; string(txs[1].Meta.Err) != want {
		t.Fatalf("the failed transaction reports %s, want %s", txs[1].Meta.Err, want)
	}
	// Rewards default on and are empty, because the fee is burned in full.
	if b.Rewards == nil || len(*b.Rewards) != 0 {
		t.Fatalf("rewards are %v", b.Rewards)
	}

	// transactionDetails takes Agave's values and rejects others.
	b, err = block(t, s, height, map[string]any{"transactionDetails": "signatures"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Transactions != nil || b.Signatures == nil || len(*b.Signatures) != 2 {
		t.Fatalf("signatures returned %+v", b)
	}
	goodSig := signatureOf(good)
	if (*b.Signatures)[0] != base58.Encode(goodSig[:]) {
		t.Fatalf("the first signature is %s", (*b.Signatures)[0])
	}
	b, err = block(t, s, height, map[string]any{"transactionDetails": "none", "rewards": false})
	if err != nil {
		t.Fatal(err)
	}
	if b.Transactions != nil || b.Signatures != nil || b.Rewards != nil {
		t.Fatalf("none with rewards off returned %+v", b)
	}
	if _, err := block(t, s, height, map[string]any{"transactionDetails": "nonsense"}); err == nil {
		t.Fatal("an unknown transactionDetails was accepted")
	}

	// An empty height is an empty block, not an error and not null.
	c.Block()
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	b, err = block(t, s, uint64(c.Height), map[string]any{"encoding": "base64"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Transactions == nil || len(*b.Transactions) != 0 {
		t.Fatalf("an empty height returned %v", b.Transactions)
	}
	if b.Blockhash == b.PreviousBlockhash || b.Blockhash == "" {
		t.Fatalf("an empty height has blockhash %s", b.Blockhash)
	}

	// A height that no block has reached is Agave's -32004. A client tells it
	// from the purged -32001 by the code.
	_, err = getBlock(context.Background(), s, []jsontext.Value{jsontext.Value("999999")})
	if re := asRPCError(err); err == nil || re.Code != codeBlockNotAvailable || re.Message != "Block not available for slot 999999" {
		t.Fatalf("an unreached height returned %v", err)
	}
}

// TestGetBlockSaysWhenAHeightIsPurged proves that a purged height is error
// -32001, which names the first available block. It is not the null that
// getTransaction returns for a purged signature.
func TestGetBlockSaysWhenAHeightIsPurged(t *testing.T) {
	const budget = 16 * 1024
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{IndexMaxBytes: budget})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()

	for i := 0; i < 200; i++ {
		tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest(i), svm.MinimumBalance(0)))
		if res, _ := c.Block(tx); res[0].Code != 0 {
			t.Fatalf("block %d: code %d", i, res[0].Code)
		}
		if err := s.notify(uint64(c.Height)); err != nil {
			t.Fatal(err)
		}
	}

	first, err := getFirstAvailableBlock(context.Background(), s, nil)
	if err != nil {
		t.Fatal(err)
	}
	available := first.(uint64)
	if available <= 1 || available > uint64(c.Height) {
		t.Fatalf("the first available block is %d of %d", available, c.Height)
	}
	// A height below it is purged, and the error says so.
	_, err = block(t, s, available-1, nil)
	rpcErr, ok := err.(*rpcError)
	if !ok || rpcErr.Code != blockCleanedUp {
		t.Fatalf("a purged height returned %v", err)
	}
	if want := "First available block: " + strconv.FormatUint(available, 10); !strings.Contains(rpcErr.Message, want) {
		t.Fatalf("the message is %q, want it to name %q", rpcErr.Message, want)
	}
	// The first available block is served.
	if b, err := block(t, s, available, map[string]any{"encoding": "base64"}); err != nil || b.Blockhash == "" {
		t.Fatalf("the first available block is %+v, %v", b, err)
	}
}
