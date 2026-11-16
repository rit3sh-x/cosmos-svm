package rpc

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"testing"

	"cosmossdk.io/log/v2"

	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestNoSliceOrOffsetCanPanic proves a negative dataSlice offset or length
// is invalid params on every method and subscription that takes one. A
// length past the end clamps, and a memcmp offset at the top of int matches
// nothing.
func TestNoSliceOrOffsetCanPanic(t *testing.T) {
	owner := tc.Pub(tc.Key(200))
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: []svm.GenesisAccount{tokenLike(10, tc.Pub(tc.Key(202)), owner)}})
	c.Block()
	s := New(c.App, nil, log.NewNopLogger())
	account := tc.Pub(tc.Key(10))
	key, _ := json.Marshal(base58.Encode(account[:]), json.Deterministic(true))
	keys, _ := json.Marshal([]string{base58.Encode(account[:])}, json.Deterministic(true))
	program, _ := json.Marshal(base58.Encode(owner[:]), json.Deterministic(true))

	for _, slice := range []string{`{"offset":-1,"length":1}`, `{"offset":0,"length":-1}`} {
		cfg := jsontext.Value(`{"encoding":"base64","dataSlice":` + slice + `}`)
		for name, params := range map[string][]jsontext.Value{
			"getAccountInfo":      {key, cfg},
			"getMultipleAccounts": {keys, cfg},
			"getProgramAccounts":  {program, cfg},
		} {
			if _, err := methods[name](context.Background(), s, params); !isInvalidParams(err) {
				t.Fatalf("%s with dataSlice %s: %v, want invalid params", name, slice, err)
			}
		}
		p := dial(t, s)
		for _, method := range []string{"accountSubscribe", "programSubscribe"} {
			if err := p.subscribeError(method, base58.Encode(account[:]), cfg); err == nil || err.Code != codeInvalidParams {
				t.Fatalf("%s with dataSlice %s: %v, want invalid params", method, slice, err)
			}
		}
	}

	// A length that runs past the end clamps to it, as Agave's slice_data
	// does, whatever the sum would have been.
	var huge accountConfig
	if err := json.Unmarshal([]byte(`{"encoding":"base64","dataSlice":{"offset":164,"length":18446744073709551615}}`), &huge); err != nil {
		t.Fatal(err)
	}
	ui, err := renderAccount(account, ffi.Account{Lamports: 1, Data: make([]byte, 165)}, huge, nil)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := base64.StdEncoding.DecodeString(dataPair(t, ui.Data)[0]); len(raw) != 1 {
		t.Fatalf("offset 164 of 165 bytes sliced to %d bytes, want 1", len(raw))
	}

	far := `{"filters":[{"memcmp":{"offset":9223372036854775807,"bytes":"2"}}]}`
	if got := scan(t, s, owner, far); len(got) != 0 {
		t.Fatalf("a memcmp past the end of every account matched %d", len(got))
	}
}

// TestARenderingPanicFailsOneNotification proves a panic while rendering a
// notification fails that notification and returns an error. A mint reader
// with no store panics when it reads a token account's mint.
func TestARenderingPanicFailsOneNotification(t *testing.T) {
	data := make([]byte, 165)
	data[108] = 1 // an initialized token account
	_, err := writtenAccount(tc.Pub(tc.Key(10)), ffi.Account{Lamports: 1, Owner: svm.SplToken, Data: data},
		accountConfig{Encoding: "jsonParsed"}, &mintReader{})
	if err == nil {
		t.Fatal("a rendering that panicked returned no error")
	}
}

func isInvalidParams(err error) bool {
	var e *rpcError
	return errors.As(err, &e) && e.Code == codeInvalidParams
}

// subscribeError sends one subscribe and returns the refusal it was answered
// with, or nil if it was acknowledged.
func (p *peer) subscribeError(method string, params ...any) *rpcError {
	p.t.Helper()
	p.next++
	if err := p.conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": p.next, "method": method, "params": params}); err != nil {
		p.t.Fatal(err)
	}
	var resp struct {
		Error *rpcError `json:"error"`
	}
	p.read(&resp)
	return resp.Error
}
