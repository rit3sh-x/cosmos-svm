package rpc

import (
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"os"
	"testing"

	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// The renderer's oracle works in three steps:
//  1. TestWriteGoldenFixtures (golden_test.go) writes testdata/tx_fixtures.json
//     from a chain's own history records.
//  2. tools/tx-oracle, which links Anza's solana-transaction-status 4.3.0,
//     renders the fixtures into testdata/tx_golden.json.
//  3. TestTheRendererIsAnzas compares the Go renderer with that file, in every
//     encoding.
//
// `make golden` runs the first two steps.

const (
	txFixturesPath = "testdata/tx_fixtures.json"
	txGoldenPath   = "testdata/tx_golden.json"
)

type oracleInner struct {
	ProgramIDIndex uint8  `json:"programIdIndex"`
	Accounts       []int  `json:"accounts"`
	Data           string `json:"data"`
	StackHeight    uint8  `json:"stackHeight"`
}

type oracleTokenBalance struct {
	AccountIndex uint8  `json:"accountIndex"`
	Mint         string `json:"mint"`
	Owner        string `json:"owner"`
	ProgramID    string `json:"programId"`
	Amount       uint64 `json:"amount"`
	Decimals     uint8  `json:"decimals"`
}

// oracleTx is a record in the oracle's input shape. Record holds the record
// itself for the Go side, and the oracle ignores it.
type oracleTx struct {
	Tx     string     `json:"tx"`
	Meta   oracleMeta `json:"meta"`
	Record svm.Record `json:"record"`
}

type oracleMeta struct {
	Err                  jsontext.Value       `json:"err"`
	Fee                  uint64               `json:"fee"`
	PreBalances          []uint64             `json:"preBalances"`
	PostBalances         []uint64             `json:"postBalances"`
	Inner                [][]oracleInner      `json:"inner"`
	Logs                 []string             `json:"logs"`
	PreTokenBalances     []oracleTokenBalance `json:"preTokenBalances"`
	PostTokenBalances    []oracleTokenBalance `json:"postTokenBalances"`
	LoadedWritable       []string             `json:"loadedWritable"`
	LoadedReadonly       []string             `json:"loadedReadonly"`
	ReturnProgram        *string              `json:"returnProgram"`
	ReturnData           *string              `json:"returnData"`
	ComputeUnitsConsumed uint64               `json:"computeUnitsConsumed"`
	CostUnits            uint64               `json:"costUnits"`
}

type txFixture struct {
	Name      string `json:"name"`
	Slot      uint64 `json:"slot"`
	BlockTime int64  `json:"blockTime"`
	Index     uint32 `json:"index"`
	oracleTx
	Encodings  []string `json:"encodings"`
	MaxVersion *int     `json:"maxSupportedTransactionVersion"`
}

type blockFixture struct {
	Name              string     `json:"name"`
	PreviousBlockhash string     `json:"previousBlockhash"`
	Blockhash         string     `json:"blockhash"`
	ParentSlot        uint64     `json:"parentSlot"`
	BlockTime         int64      `json:"blockTime"`
	BlockHeight       uint64     `json:"blockHeight"`
	Transactions      []oracleTx `json:"transactions"`
	Encodings         []string   `json:"encodings"`
	Details           []string   `json:"details"`
	MaxVersion        *int       `json:"maxSupportedTransactionVersion"`
}

type txFixtures struct {
	Transactions []txFixture    `json:"transactions"`
	Blocks       []blockFixture `json:"blocks"`
}

type txGolden struct {
	Name     string         `json:"name"`
	Encoding string         `json:"encoding"`
	Rendered jsontext.Value `json:"rendered"`
	Error    *string        `json:"error"`
}

func oracleBalances(list []ffi.TokenBalance) []oracleTokenBalance {
	out := []oracleTokenBalance{}
	for _, b := range list {
		out = append(out, oracleTokenBalance{b.AccountIndex, base58.Encode(b.Mint[:]), base58.Encode(b.Owner[:]),
			base58.Encode(b.ProgramID[:]), b.Amount, b.Decimals})
	}
	return out
}

func toOracle(t *testing.T, r svm.Record) oracleTx {
	t.Helper()
	if r.Meta.CostUnits == nil {
		t.Fatalf("record %d/%d has no cost", r.Height, r.Index)
	}
	m := oracleMeta{
		Err: jsontext.Value("null"), Fee: r.Fee, PreBalances: nonNil(r.Meta.PreBalances), PostBalances: nonNil(r.Meta.PostBalances),
		Inner: [][]oracleInner{}, Logs: nonNilStrings(r.Meta.Logs),
		PreTokenBalances: oracleBalances(r.Meta.PreTokenBalances), PostTokenBalances: oracleBalances(r.Meta.PostTokenBalances),
		LoadedWritable: base58Keys(r.Meta.LoadedWritable), LoadedReadonly: base58Keys(r.Meta.LoadedReadonly),
		ComputeUnitsConsumed: r.Units, CostUnits: *r.Meta.CostUnits,
	}
	if r.Status != 0 {
		m.Err = r.Meta.Err
	}
	for _, group := range r.Meta.Inner {
		list := []oracleInner{}
		for _, ix := range group {
			list = append(list, oracleInner{ix.ProgramIDIndex, indexes(ix.Accounts), base64.StdEncoding.EncodeToString(ix.Data), ix.StackHeight})
		}
		m.Inner = append(m.Inner, list)
	}
	if r.Meta.ReturnProgram != nil {
		program, data := base58.Encode(r.Meta.ReturnProgram[:]), base64.StdEncoding.EncodeToString(r.Meta.ReturnData)
		m.ReturnProgram, m.ReturnData = &program, &data
	}
	return oracleTx{Tx: base64.StdEncoding.EncodeToString(r.Tx), Meta: m, Record: r}
}

// TestTheRendererIsAnzas renders every fixture in every encoding and
// transaction detail and compares it with Anza's rendering. An encoding that
// Anza refuses must get the RPC's version error.
func TestTheRendererIsAnzas(t *testing.T) {
	var in txFixtures
	var want []txGolden
	for path, v := range map[string]any{txFixturesPath: &in, txGoldenPath: &want} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, v); err != nil {
			t.Fatal(err)
		}
	}
	renders := map[[2]string]func() (any, error){}
	for _, f := range in.Transactions {
		for _, enc := range f.Encodings {
			renders[[2]string{f.Name, enc}] = func() (any, error) {
				out, err := encodedTransaction(f.Record, enc, f.MaxVersion, true)
				if err != nil {
					return nil, err
				}
				out["slot"], out["blockTime"], out["transactionIndex"] = f.Slot, f.BlockTime, f.Index
				return out, nil
			}
		}
	}
	for _, b := range in.Blocks {
		records := make([]svm.Record, len(b.Transactions))
		for i, tx := range b.Transactions {
			records[i] = tx.Record
		}
		for _, enc := range b.Encodings {
			for _, details := range b.Details {
				renders[[2]string{b.Name + "/" + details, enc}] = func() (any, error) {
					out := map[string]any{"previousBlockhash": b.PreviousBlockhash, "blockhash": b.Blockhash,
						"parentSlot": b.ParentSlot, "blockTime": b.BlockTime, "blockHeight": b.BlockHeight}
					return out, blockBody(out, records, details, enc, b.MaxVersion, true)
				}
			}
		}
	}
	if len(want) != len(renders) {
		t.Fatalf("%d golden renderings for %d fixture renderings: write the golden file again", len(want), len(renders))
	}
	for _, g := range want {
		render, ok := renders[[2]string{g.Name, g.Encoding}]
		if !ok {
			t.Fatalf("%s/%s: no such fixture", g.Name, g.Encoding)
		}
		got, err := render()
		if g.Error != nil {
			var rpcErr *rpcError
			if !errors.As(err, &rpcErr) || rpcErr.Code != -32015 {
				t.Errorf("%s %s: Anza refused (%s), the renderer answered %v", g.Name, g.Encoding, *g.Error, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s %s: %v", g.Name, g.Encoding, err)
			continue
		}
		data, err := json.Marshal(got, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		if mine, anza := canonical(t, data), canonical(t, g.Rendered); mine != anza {
			t.Errorf("%s %s:\n got %s\nwant %s", g.Name, g.Encoding, mine, anza)
		}
	}
}
