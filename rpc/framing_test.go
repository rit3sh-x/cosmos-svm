package rpc

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// TestTheArityTableIsAgaves checks every served method and websocket method
// against the parameter counts that tools/rpc-inventory.py reads from Agave's
// source.
func TestTheArityTableIsAgaves(t *testing.T) {
	var inv struct {
		Methods []struct {
			Name     string `json:"name"`
			Required int    `json:"required"`
			Total    int    `json:"total"`
		} `json:"methods"`
		Websocket []struct {
			Name     string `json:"name"`
			Required int    `json:"required"`
			Total    int    `json:"total"`
		} `json:"websocket"`
	}
	readJSON(t, "testdata/inventory.json", &inv)
	agave := map[string]arity{}
	for _, m := range inv.Methods {
		agave[m.Name] = arity{m.Required, m.Total}
	}
	ws := map[string]arity{}
	for _, m := range inv.Websocket {
		ws[m.Name] = arity{m.Required, m.Total}
	}
	for name := range methods {
		if want, ok := agave[name]; !ok || arities[name] != want {
			t.Errorf("%s: arity %v, Agave's %v", name, arities[name], want)
		}
	}
	for name := range subscriptions {
		if want, ok := ws[name]; !ok || arities[name] != want {
			t.Errorf("websocket %s: arity %v, Agave's %v", name, arities[name], want)
		}
	}
}

// TestFramingAnswersAsJsonrpcCore checks each framing rule against the whole
// answer that jsonrpc-core 18.0.0 and jsonrpc-derive 18.0.0 give.
func TestFramingAnswersAsJsonrpcCore(t *testing.T) {
	_, s := server(t)
	for _, tt := range []struct{ body, want string }{
		{`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":{"pubkey":"x"}}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"` + "`params`" + ` should be an array"}}`},
		{`{"jsonrpc":"2.0","id":1,"method":"getBalance"}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"` + "`params`" + ` should have at least 1 argument(s)"}}`},
		{`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["11111111111111111111111111111111",{},3]}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid parameters: Expected from 1 to 2 parameters.","data":"\"Got: 3\""}}`},
		{`{"jsonrpc":"2.0","id":1,"method":"getHealth","params":[[1,"a"],{"k":null},true,1.5]}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid parameters: No parameters were expected","data":"Array([Array [Number(1), String(\"a\")], Object {\"k\": Null}, Bool(true), Number(1.5)])"}}`},
		{`{"jsonrpc":"2.0","id":1,"method":"getHealth","params":{}}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid parameters: No parameters were expected","data":"Map({})"}}`},
		{`{"jsonrpc":"2.0","id":1,"method":"getSlotLeaders","params":[1]}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid params: invalid length 1, expected a tuple of size 2."}}`},
		{`{"jsonrpc":"2.0","id":1,"method":"getSlotLeaders"}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid params: invalid type: null, expected a tuple of size 2."}}`},
		{`{"jsonrpc":"2.0","id":1,"method":"getSlotLeaders","params":[1,2,3]}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid params: invalid length 3, expected fewer elements in array."}}`},
		{`{"id":1,"method":"getSlot"}`,
			`{"id":1,"error":{"code":-32600,"message":"Unsupported JSON-RPC protocol version"}}`},
		{`{"jsonrpc":"1.0","id":"x","method":"getSlot"}`,
			`{"jsonrpc":"2.0","id":"x","error":{"code":-32600,"message":"Invalid request"}}`},
		{`{"jsonrpc":"2.0","id":1,"method":"getSlot","extra":1}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"Invalid request"}}`},
		{`{"jsonrpc":"2.0","id":-1,"method":"getSlot"}`,
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`},
		{`[1]`,
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`},
		{`{"jsonrpc":"2.0","id":1,"method":"getNothing"}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`},
		{`{"jsonrpc":"2.0","id":1,"method":"requestAirdrop","params":["11111111111111111111111111111111"]}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"` + "`params`" + ` should have at least 2 argument(s)"}}`},
	} {
		rec := httptest.NewRecorder()
		s.answer(context.Background(), []byte(tt.body), "", rec)
		got := rec.Body.Bytes()
		if len(got) == 0 {
			t.Errorf("%s: no answer", tt.body)
			continue
		}
		var a, b any
		_ = json.Unmarshal(got, &a)
		_ = json.Unmarshal([]byte(tt.want), &b)
		if string(encode(a)) != string(encode(b)) {
			t.Errorf("%s\n got %s\nwant %s", tt.body, got, tt.want)
		}
	}
	for _, body := range []string{`{"jsonrpc":"2.0","method":"getSlot"}`, `{"method":"getSlot"}`, `[]`} {
		rec := httptest.NewRecorder()
		if s.answer(context.Background(), []byte(body), "", rec); rec.Body.Len() > 0 {
			t.Errorf("%s was answered %s, want nothing", body, rec.Body)
		}
	}
}

// TestAnAnswerIsTheBytesTheEncoderWrote checks that each answer keeps its old
// bytes. The server now encodes each answer once, and a batch one entry at a
// time. The old path ran the encoder on the whole answer, and response had a
// MarshalJSON method. The cases are HTML and line separator escapes, a null
// result, the data of an error, a batch with a notification in it, and an
// answer that is flushed in pieces.
func TestAnAnswerIsTheBytesTheEncoderWrote(t *testing.T) {
	_, s := server(t)
	faucet := tc.Pub(tc.Key(1))
	key := base58.Encode(faucet[:])
	var hundred []string
	for range 100 {
		hundred = append(hundred, key)
	}
	req := func(id any, method string, params ...any) string {
		return string(encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}))
	}
	for _, body := range []string{
		req(1, "getAccountInfo", key, map[string]any{"encoding": "base64"}),
		req(2, "getAccountInfo", base58.Encode(make([]byte, 32)), map[string]any{"encoding": "jsonParsed"}),
		req(3, "getTransaction", base58.Encode(make([]byte, 64))),
		"{\"jsonrpc\":\"2.0\",\"id\":\"<&>\u2028\",\"method\":\"getHealth\",\"params\":[1]}",
		req(4, "getMultipleAccounts", hundred, map[string]any{"encoding": "jsonParsed"}),
		req(5, "getInflationRate"),
		`{"id":6,"method":"getSlot"}`,
		"[" + req(7, "getVersion") + `,{"jsonrpc":"2.0","method":"getSlot"},` + req(8, "getEpochInfo") + "]",
		`[{"jsonrpc":"2.0","method":"getSlot"}]`,
		`[1]`,
		`{`,
	} {
		rec := httptest.NewRecorder()
		s.answer(context.Background(), []byte(body), "", rec)
		var whole any
		var entries []legacy
		calls, batch, ok := parseBody([]byte(body))
		if !ok {
			whole = legacy{JSONRPC: "2.0", ID: jsontext.Value("null"), Error: &rpcError{Code: codeParse, Message: "Parse error"}}
		} else {
			for _, c := range calls {
				s.call(context.Background(), c, "", func(r response) { entries = append(entries, legacy(r)) })
			}
		}
		switch {
		case len(entries) > 0 && batch:
			whole = entries
		case len(entries) > 0:
			whole = entries[0]
		}
		var want bytes.Buffer
		if whole != nil {
			if err := json.MarshalWrite(&want, whole, json.Deterministic(true)); err != nil {
				t.Fatal(err)
			}
			want.WriteByte('\n')
		}
		if !bytes.Equal(rec.Body.Bytes(), want.Bytes()) {
			t.Errorf("%s\n got %q\nwant %q", body, rec.Body, want.Bytes())
		}
	}
}

// legacy is response with its old MarshalJSON method.
type legacy response

func (r legacy) MarshalJSON() ([]byte, error) {
	if r.Error != nil {
		return json.Marshal(struct {
			JSONRPC string         `json:"jsonrpc,omitempty"`
			Error   *rpcError      `json:"error"`
			ID      jsontext.Value `json:"id"`
		}{r.JSONRPC, r.Error, r.ID}, json.Deterministic(true))
	}
	return json.Marshal(struct {
		JSONRPC string         `json:"jsonrpc,omitempty"`
		Result  any            `json:"result"`
		ID      jsontext.Value `json:"id"`
	}{r.JSONRPC, r.Result, r.ID}, json.Deterministic(true))
}

// TestKeysAreRefusedWithAgavesKind checks the two kinds of ParseAddressError.
// A character outside the alphabet is Invalid, and any other fault is WrongSize.
func TestKeysAreRefusedWithAgavesKind(t *testing.T) {
	for raw, want := range map[string]string{
		"0OIl":    "Invalid param: Invalid",
		"notakey": "Invalid param: WrongSize",
		"":        "Invalid param: WrongSize",
		"1111111111111111111111111111111111111111111111": "Invalid param: WrongSize",
	} {
		if _, err := decodeKey(raw); err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %q", raw, err, want)
		}
	}
	if _, err := signatureParam("l"); err == nil || err.Error() != "Invalid param: Invalid" {
		t.Errorf("a signature with a bad character: %v", err)
	}
}

// TestAMethodsErrorReachesTheCaller checks the text that a client reads when
// an UnmarshalJSON method refuses a parameter. The text is the message of the
// method.
func TestAMethodsErrorReachesTheCaller(t *testing.T) {
	var f accountFilter
	err := param([]jsontext.Value{jsontext.Value(`"nope"`)}, 0, &f)
	want := "Invalid params: unknown variant `nope`, expected one of `dataSize`, `memcmp`, `tokenAccountState`."
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %s", err, want)
	}
}

// TestARepeatedKeyIsAnsweredAsJsonrpcCoreAnswers holds the answers to bodies
// that jsonrpc-core 18.0.0 was run on.
func TestARepeatedKeyIsAnsweredAsJsonrpcCoreAnswers(t *testing.T) {
	_, s := server(t)
	parse := `{"jsonrpc":"2.0","error":{"code":-32700,"message":"Parse error"},"id":null}`
	invalid := func(id string) string {
		return `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Invalid request"},"id":` + id + `}`
	}
	answer := func(body string) string {
		rec := httptest.NewRecorder()
		s.answer(context.Background(), []byte(body), "", rec)
		return strings.TrimSpace(rec.Body.String())
	}
	for body, want := range map[string]string{
		`{"jsonrpc":"2.0","id":1,"id":2,"method":"getSlot","params":[]}`:             parse,
		`{"jsonrpc":"2.0","id":1,"method":"getSlot","method":"getSlot","params":[]}`: invalid("1"),
		`{"jsonrpc":"2.0","jsonrpc":"2.0","id":1,"method":"getSlot","params":[]}`:    invalid("1"),
		`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[1],"params":[2]}`:      invalid("1"),
		`{"jsonrpc":"2.0","method":"getSlot","method":"getSlot"}`:                    invalid("null"),
	} {
		if got := answer(body); got != want {
			t.Errorf("%s\n got %s\nwant %s", body, got, want)
		}
	}
	got := answer(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"nope","commitment":"finalized"}]}`)
	if !strings.Contains(got, `"result"`) {
		t.Errorf("the last value of a repeated key inside params did not win: %s", got)
	}
}

// TestAMetadataStringMustBeUTF8 holds a Borsh string to Rust's rule: bytes that
// are not UTF-8 do not parse, so an account falls back to its raw encoding and
// the answer stays valid JSON.
func TestAMetadataStringMustBeUTF8(t *testing.T) {
	if _, _, ok := borshString([]byte{2, 0, 0, 0, 0xff, 0xfe}, 0); ok {
		t.Fatal("bytes that are not UTF-8 parsed as a string")
	}
	if s, at, ok := borshString([]byte{2, 0, 0, 0, 'h', 'i'}, 0); !ok || s != "hi" || at != 6 {
		t.Fatalf("%q %d %v", s, at, ok)
	}
}

// TestANullConfigFieldKeepsItsDefault is what the Solana CLI sends: a config
// whose optional fields are null means the defaults.
func TestANullConfigFieldKeepsItsDefault(t *testing.T) {
	_, s := server(t)
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[1,{"encoding":null,"transactionDetails":null,"rewards":null}]}`,
		`{"jsonrpc":"2.0","id":1,"method":"getTransaction","params":["` + base58.Encode(make([]byte, 64)) + `",{"encoding":null}]}`,
	} {
		rec := httptest.NewRecorder()
		s.answer(context.Background(), []byte(body), "", rec)
		if got := rec.Body.String(); strings.Contains(got, "-32602") {
			t.Errorf("%s\n%s", body, got)
		}
	}
}

func TestKeepNumbersKeepsEveryDigit(t *testing.T) {
	var v any
	in := `[18446744073709551615,{"f":1.50},"s"]`
	if err := json.Unmarshal([]byte(in), &v, keepNumbers); err != nil {
		t.Fatal(err)
	}
	list := v.([]any)
	if list[0] != number("18446744073709551615") || list[1].(map[string]any)["f"] != number("1.50") {
		t.Fatalf("numbers changed: %#v", v)
	}
	out, err := json.Marshal(v, json.Deterministic(true))
	if err != nil || string(out) != in {
		t.Fatalf("%s %v", out, err)
	}
}
