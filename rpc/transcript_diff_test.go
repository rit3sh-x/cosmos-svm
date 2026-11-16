package rpc

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"testing"
)

type difference struct {
	Case   string         `json:"case"`
	Method string         `json:"method"`
	Path   string         `json:"path"`
	Agave  jsontext.Value `json:"agave,omitzero"`
	Chain  jsontext.Value `json:"chain,omitzero"`
}

func (d difference) String() string {
	return fmt.Sprintf("%s at %s: agave %s, chain %s", d.Case, d.Path, show(d.Agave), show(d.Chain))
}

func show(v jsontext.Value) string {
	if v == nil {
		return "<absent>"
	}
	return string(v)
}

// normalization is a value that differs between any two chains, whatever
// their compatibility: a slot, a blockhash, a setup signature. It compares
// presence and JSON type only, and never drops a key. Kind, when set, limits
// the rule to a value of that JSON type, for a path that holds a string in one
// encoding and an object in another.
type normalization struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Kind   string `json:"kind,omitempty"`
	Reason string `json:"reason"`
}

func (r normalization) applies(method, path string, v any) bool {
	return (r.Method == "*" || r.Method == method) && matches(r.Path, path) && (r.Kind == "" || r.Kind == kind(v))
}

// divergence is a deliberate difference. Both values are pinned, so drift on
// either side fails, and an entry whose case no longer differs fails as stale.
type divergence struct {
	Method string         `json:"method"`
	Case   string         `json:"case"`
	Path   string         `json:"path"`
	Agave  jsontext.Value `json:"agave,omitzero"`
	Chain  jsontext.Value `json:"chain,omitzero"`
	Reason string         `json:"reason"`
	Page   string         `json:"page"`
}

// tokens splits `result.value[0].slot` into result, value, [0], slot.
func tokens(path string) []string {
	var out []string
	for _, part := range strings.Split(path, ".") {
		for {
			i := strings.IndexByte(part, '[')
			if i < 0 {
				break
			}
			if i > 0 {
				out = append(out, part[:i])
			}
			j := strings.IndexByte(part, ']')
			out = append(out, part[i:j+1])
			part = part[j+1:]
		}
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// matches reports whether a path fits a pattern, where `*` stands for any one
// key and `[*]` for any one index.
func matches(pattern, path string) bool {
	p, q := tokens(pattern), tokens(path)
	if len(p) != len(q) {
		return false
	}
	for i := range p {
		switch {
		case p[i] == q[i]:
		case p[i] == "*" && !strings.HasPrefix(q[i], "["):
		case p[i] == "[*]" && strings.HasPrefix(q[i], "["):
		default:
			return false
		}
	}
	return true
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	if strings.HasPrefix(key, "[") {
		return path + key
	}
	return path + "." + key
}

// decode reads JSON with numbers kept as written, so a u64 above 2^53
// compares exactly.
func decode(raw jsontext.Value) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v, keepNumbers); err != nil {
		return nil, err
	}
	return v, nil
}

func encode(v any) jsontext.Value {
	b, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		panic(err)
	}
	return b
}

// kind is a value's JSON type, which is all a normalization compares.
func kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	default:
		return "object"
	}
}

func sameNumber(a, b number) bool {
	x, ok1 := new(big.Rat).SetString(string(a))
	y, ok2 := new(big.Rat).SetString(string(b))
	if !ok1 || !ok2 {
		return a == b
	}
	return x.Cmp(y) == 0
}

// equal is the comparison every value gets once no rule applies: key order
// ignored, numbers by value, everything else exact.
func equal(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !equal(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case number:
		y, ok := b.(number)
		return ok && sameNumber(x, y)
	default:
		return a == b
	}
}

// compare walks two answers and lists every place they part that no
// normalization for this method covers.
func compare(name, method string, agave, chain any, rules []normalization) []difference {
	var out []difference
	var walk func(path string, a, c any, hasA, hasC bool)
	walk = func(path string, a, c any, hasA, hasC bool) {
		if hasA != hasC {
			d := difference{Case: name, Method: method, Path: path}
			if hasA {
				d.Agave = encode(masked(method, path, a, rules))
			}
			if hasC {
				d.Chain = encode(masked(method, path, c, rules))
			}
			out = append(out, d)
			return
		}
		for _, r := range rules {
			if r.applies(method, path, a) || r.applies(method, path, c) {
				if kind(a) != kind(c) {
					out = append(out, difference{Case: name, Method: method, Path: path, Agave: encode(a), Chain: encode(c)})
				}
				return
			}
		}
		x, xok := a.(map[string]any)
		y, yok := c.(map[string]any)
		if xok && yok {
			keys := map[string]bool{}
			for k := range x {
				keys[k] = true
			}
			for k := range y {
				keys[k] = true
			}
			sorted := make([]string, 0, len(keys))
			for k := range keys {
				sorted = append(sorted, k)
			}
			sort.Strings(sorted)
			for _, k := range sorted {
				av, ah := x[k]
				cv, ch := y[k]
				walk(join(path, k), av, cv, ah, ch)
			}
			return
		}
		p, pok := a.([]any)
		q, qok := c.([]any)
		if pok && qok && len(p) == len(q) {
			for i := range p {
				walk(join(path, fmt.Sprintf("[%d]", i)), p[i], q[i], true, true)
			}
			return
		}
		if !equal(a, c) {
			out = append(out, difference{Case: name, Method: method, Path: path,
				Agave: encode(masked(method, path, a, rules)), Chain: encode(masked(method, path, c, rules))})
		}
	}
	walk("", agave, chain, true, true)
	return out
}

// masked returns v with each normalized value under it replaced by its JSON
// type. So a difference reported whole, such as two arrays of different
// lengths, pins nothing that differs between any two chains.
func masked(method, path string, v any, rules []normalization) any {
	for _, r := range rules {
		if r.applies(method, path, v) {
			return "normalized " + kind(v)
		}
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = masked(method, join(path, k), e, rules)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = masked(method, join(path, fmt.Sprintf("[%d]", i)), e, rules)
		}
		return out
	}
	return v
}

// pinned is the same value, or the same absence, under equal's rules.
func pinned(want, got jsontext.Value) bool {
	if want == nil || got == nil {
		return want == nil && got == nil
	}
	x, err1 := decode(want)
	y, err2 := decode(got)
	return err1 == nil && err2 == nil && equal(x, y)
}

// reconcile checks every difference against the ledger. It returns the
// differences no entry covers and the entries no difference used.
func reconcile(diffs []difference, ledger []divergence) (unlisted []difference, stale []divergence) {
	used := make([]bool, len(ledger))
	for _, d := range diffs {
		found := false
		for i, e := range ledger {
			if e.Case == d.Case && e.Path == d.Path && pinned(e.Agave, d.Agave) && pinned(e.Chain, d.Chain) {
				used[i], found = true, true
				break
			}
		}
		if !found {
			unlisted = append(unlisted, d)
		}
	}
	for i, e := range ledger {
		if !used[i] {
			stale = append(stale, e)
		}
	}
	return unlisted, stale
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func raw(t *testing.T, s string) any {
	t.Helper()
	v, err := decode(jsontext.Value(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTheTranscriptComparesStructurally(t *testing.T) {
	agave := raw(t, `{"jsonrpc":"2.0","result":{"value":0.0,"context":{"slot":7,"apiVersion":"4.3.0"}},"id":1}`)
	chain := raw(t, `{"id":1,"result":{"context":{"apiVersion":"4.3.0","slot":12},"value":0},"jsonrpc":"2.0"}`)
	rules := []normalization{{Method: "*", Path: "result.context.slot"}}

	if d := compare("getBalance/payer", "getBalance", agave, chain, rules); len(d) != 0 {
		t.Fatalf("key order, 0.0 against 0 and a normalized slot are not differences: %v", d)
	}
	if d := compare("getBalance/payer", "getBalance", agave, chain, nil); len(d) != 1 || d[0].Path != "result.context.slot" {
		t.Fatalf("without the rule the slot is the one difference: %v", d)
	}
	big := raw(t, `{"result":18446744073709551615}`)
	smaller := raw(t, `{"result":18446744073709551614}`)
	if d := compare("c", "m", big, smaller, nil); len(d) != 1 {
		t.Fatalf("two u64s that float64 cannot tell apart are different: %v", d)
	}
}

func TestANormalizationComparesPresenceAndTypeOnly(t *testing.T) {
	rules := []normalization{{Method: "getBlock", Path: "result.blockhash"}, {Method: "*", Path: "result.value[*].slot"}}
	a := raw(t, `{"result":{"blockhash":"abc","value":[{"slot":1},{"slot":2}]}}`)

	if d := compare("c", "getBlock", a, raw(t, `{"result":{"blockhash":"xyz","value":[{"slot":9},{"slot":8}]}}`), rules); len(d) != 0 {
		t.Fatalf("same presence and type: %v", d)
	}
	if d := compare("c", "getBlock", a, raw(t, `{"result":{"blockhash":null,"value":[{"slot":9},{"slot":8}]}}`), rules); len(d) != 1 {
		t.Fatalf("a string against null is a difference: %v", d)
	}
	if d := compare("c", "getBlock", a, raw(t, `{"result":{"value":[{"slot":9},{"slot":8}]}}`), rules); len(d) != 1 || d[0].Chain != nil {
		t.Fatalf("a missing key is a difference, never dropped: %v", d)
	}
	if d := compare("c", "getTransaction", a, raw(t, `{"result":{"blockhash":"xyz","value":[{"slot":1},{"slot":2}]}}`), rules); len(d) != 1 {
		t.Fatalf("a rule for getBlock does not cover getTransaction: %v", d)
	}
	// Arrays of different lengths are one difference, reported whole, and a
	// normalized value inside it is pinned as its type alone.
	d := compare("c", "getBlock", a, raw(t, `{"result":{"blockhash":"xyz","value":[{"slot":9}]}}`), rules)
	if len(d) != 1 || string(d[0].Agave) != `[{"slot":"normalized number"},{"slot":"normalized number"}]` || string(d[0].Chain) != `[{"slot":"normalized number"}]` {
		t.Fatalf("a whole difference pins a normalized value: %v", d)
	}
	// A rule with a kind covers that kind alone. A string normalizes, and an
	// object at the same path is compared key by key.
	byKind := []normalization{{Method: "m", Path: "result.transaction", Kind: "string"}}
	if d := compare("c", "m", raw(t, `{"result":{"transaction":"abc"}}`), raw(t, `{"result":{"transaction":"xyz"}}`), byKind); len(d) != 0 {
		t.Fatalf("two strings under a string rule: %v", d)
	}
	if d := compare("c", "m", raw(t, `{"result":{"transaction":{"k":1}}}`), raw(t, `{"result":{"transaction":{"k":2}}}`), byKind); len(d) != 1 {
		t.Fatalf("an object under a string rule is compared: %v", d)
	}
}

func TestTheLedgerPinsBothValuesAndFailsWhenStale(t *testing.T) {
	agave := raw(t, `{"error":{"code":-32002,"message":"Transaction simulation failed"}}`)
	chain := raw(t, `{"error":{"code":-32003,"message":"Transaction signature verification failure"}}`)
	diffs := compare("sendTransaction/bad-signature", "sendTransaction", agave, chain, nil)
	if len(diffs) != 2 {
		t.Fatalf("code and message differ: %v", diffs)
	}
	ledger := []divergence{
		{Case: "sendTransaction/bad-signature", Path: "error.code", Agave: jsontext.Value(`-32002`), Chain: jsontext.Value(`-32003`)},
		{Case: "sendTransaction/bad-signature", Path: "error.message", Agave: jsontext.Value(`"Transaction simulation failed"`), Chain: jsontext.Value(`"Transaction signature verification failure"`)},
	}
	if unlisted, stale := reconcile(diffs, ledger); len(unlisted)+len(stale) != 0 {
		t.Fatalf("both differences are pinned: %v %v", unlisted, stale)
	}

	drifted := compare("sendTransaction/bad-signature", "sendTransaction", agave,
		raw(t, `{"error":{"code":-32003,"message":"something else"}}`), nil)
	if unlisted, stale := reconcile(drifted, ledger); len(unlisted) != 1 || len(stale) != 1 {
		t.Fatalf("a chain value that drifts fails twice, as unlisted and as stale: %v %v", unlisted, stale)
	}
	if _, stale := reconcile(nil, ledger); len(stale) != 2 {
		t.Fatal("a difference that closed leaves its entries stale")
	}

	missing := compare("c", "m", raw(t, `{"result":{"memo":null}}`), raw(t, `{"result":{}}`), nil)
	if unlisted, _ := reconcile(missing, []divergence{{Case: "c", Path: "result.memo", Agave: jsontext.Value(`null`)}}); len(unlisted) != 0 {
		t.Fatalf("an entry with no chain value pins an absent key: %v", unlisted)
	}
	if unlisted, _ := reconcile(missing, []divergence{{Case: "c", Path: "result.memo", Agave: jsontext.Value(`null`), Chain: jsontext.Value(`null`)}}); len(unlisted) != 1 {
		t.Fatal("null is not absent")
	}
}
