package rpc

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The framing copies jsonrpc-core 18.0.0, which Agave's HTTP and websocket
// servers both run. A body is one call or a batch of calls. A call is a method
// call, a notification or an invalid call. Before a handler runs,
// jsonrpc-derive 18.0.0 checks the parameter count against the method's
// signature.

// arity counts a method's positional parameters: the required ones, then the
// trailing Options a client may leave out. rpc/testdata/inventory.json holds
// Agave's arities, read from its source, and a test compares this table with it.
type arity struct{ required, total int }

var arities = map[string]arity{
	"getAccountInfo":                    {1, 2},
	"getMultipleAccounts":               {1, 2},
	"getProgramAccounts":                {1, 2},
	"getBalance":                        {1, 2},
	"getLatestBlockhash":                {0, 1},
	"isBlockhashValid":                  {1, 2},
	"getFeeForMessage":                  {1, 2},
	"getMinimumBalanceForRentExemption": {1, 2},
	"sendTransaction":                   {1, 2},
	"simulateTransaction":               {1, 2},
	"getSignatureStatuses":              {1, 2},
	"getBlockHeight":                    {0, 1},
	"getSlot":                           {0, 1},
	"getTransaction":                    {1, 2},
	"getSignaturesForAddress":           {1, 2},
	"getTokenAccountsByOwner":           {2, 3},
	"getTokenAccountBalance":            {1, 2},
	"getBlock":                          {1, 2},
	"getFirstAvailableBlock":            {0, 0},
	"getRecentPrioritizationFees":       {0, 1},
	"getHealth":                         {0, 0},
	"getVersion":                        {0, 0},
	"getGenesisHash":                    {0, 0},
	"getSlotLeaders":                    {2, 2},
	"getLeaderSchedule":                 {0, 2},
	"getSlotLeader":                     {0, 1},
	"getBlockProduction":                {0, 1},
	"getEpochSchedule":                  {0, 0},
	"getEpochInfo":                      {0, 1},
	"getIdentity":                       {0, 0},
	"getClusterNodes":                   {0, 0},
	"requestAirdrop":                    {2, 3},
	"getVoteAccounts":                   {0, 1},
	"getInflationGovernor":              {0, 1},
	"getInflationRate":                  {0, 0},
	"getInflationReward":                {1, 2},
	"getBlocks":                         {1, 3},
	"getBlocksWithLimit":                {2, 3},
	"getBlockTime":                      {1, 1},
	"minimumLedgerSlot":                 {0, 0},
	"getHighestSnapshotSlot":            {0, 0},
	"getMaxShredInsertSlot":             {0, 0},
	"getMaxRetransmitSlot":              {0, 0},
	"getBlockCommitment":                {1, 1},
	"getAgGenesisCert":                  {0, 0},
	"getTokenSupply":                    {1, 2},
	"getTokenLargestAccounts":           {1, 2},
	"getTokenAccountsByDelegate":        {2, 3},
	"getSupply":                         {0, 1},
	"getLargestAccounts":                {0, 1},
	"getTransactionCount":               {0, 1},
	"getRecentPerformanceSamples":       {0, 1},

	"accountSubscribe":        {1, 2},
	"accountUnsubscribe":      {1, 1},
	"programSubscribe":        {1, 2},
	"programUnsubscribe":      {1, 1},
	"logsSubscribe":           {1, 2},
	"logsUnsubscribe":         {1, 1},
	"signatureSubscribe":      {1, 2},
	"signatureUnsubscribe":    {1, 1},
	"slotSubscribe":           {0, 0},
	"slotUnsubscribe":         {1, 1},
	"slotsUpdatesSubscribe":   {0, 0},
	"slotsUpdatesUnsubscribe": {1, 1},
	"rootSubscribe":           {0, 0},
	"rootUnsubscribe":         {1, 1},
}

// call is one call of a body. id is nil for a notification. v2 is true if the
// call said "jsonrpc": "2.0". The server refuses a method call without it with
// jsonrpc-core's invalid_version, and the answer has no jsonrpc field.
type call struct {
	method  string
	params  jsontext.Value // nil when absent or null, which is Params::None
	id      jsontext.Value
	v2      bool
	invalid bool // jsonrpc-core's Call::Invalid, an object that is not a call
}

// parseBody reads a body as jsonrpc-core's Request: one call or a batch.
// ok is false when jsonrpc-core answers Parse error.
func parseBody(body []byte) (calls []call, batch, ok bool) {
	var raw jsontext.Value
	if json.Unmarshal(body, &raw, agaveInput) != nil {
		return nil, false, false
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '[' {
		var items []jsontext.Value
		if json.Unmarshal(raw, &items, agaveInput) != nil {
			return nil, false, false
		}
		for _, item := range items {
			c, ok := parseCall(item)
			if !ok {
				return nil, false, false
			}
			calls = append(calls, c)
		}
		return calls, true, true
	}
	c, ok := parseCall(raw)
	return []call{c}, false, ok
}

// parseCall tries MethodCall, then Notification, then Invalid, as serde's
// untagged Call does. The first two deny unknown fields. Invalid takes any
// object whose id, if present, is an Id.
func parseCall(raw jsontext.Value) (call, bool) {
	fields, repeated, ok := members(raw)
	if !ok || repeated["id"] {
		return call{}, false
	}
	id, hasID := fields["id"]
	if c, ok := asCall(fields); ok && len(repeated) == 0 {
		if hasID && validID(id) {
			c.id = id
			return c, true
		}
		if !hasID {
			return c, true
		}
	}
	if hasID && !validID(id) {
		return call{}, false
	}
	if !hasID {
		id = jsontext.Value("null")
	}
	return call{id: id, invalid: true}, true
}

// members reads an object's members with their values as written, and says
// which names repeat. Measured on jsonrpc-core 18.0.0: a repeated id fails the
// whole parse, and any other repeated field makes the call Invalid. Inside
// params a repeated name is accepted and its last value wins.
func members(raw jsontext.Value) (fields map[string]jsontext.Value, repeated map[string]bool, ok bool) {
	d := jsontext.NewDecoder(bytes.NewReader(raw), agaveInput)
	if t, err := d.ReadToken(); err != nil || t.Kind() != '{' {
		return nil, nil, false
	}
	fields, repeated = map[string]jsontext.Value{}, map[string]bool{}
	for d.PeekKind() != '}' {
		t, err := d.ReadToken()
		if err != nil {
			return nil, nil, false
		}
		name := t.String()
		v, err := d.ReadValue()
		if err != nil {
			return nil, nil, false
		}
		if _, seen := fields[name]; seen {
			repeated[name] = true
		}
		fields[name] = v.Clone()
	}
	return fields, repeated, true
}

// asCall reads the fields MethodCall and Notification share. It fails on a
// field neither has, a non-string method, a version other than "2.0", or
// params that are not null, an array or an object.
func asCall(fields map[string]jsontext.Value) (call, bool) {
	var c call
	for k, v := range fields {
		switch k {
		case "id":
		case "method":
			if json.Unmarshal(v, &c.method, agaveInput) != nil || isNull(v) {
				return call{}, false
			}
		case "jsonrpc":
			var version *string
			if json.Unmarshal(v, &version, agaveInput) != nil {
				return call{}, false
			}
			if version != nil {
				if *version != "2.0" {
					return call{}, false
				}
				c.v2 = true
			}
		case "params":
			switch {
			case isNull(v):
			case bytes.HasPrefix(v, []byte("[")), bytes.HasPrefix(v, []byte("{")):
				c.params = v
			default:
				return call{}, false
			}
		default:
			return call{}, false
		}
	}
	if _, ok := fields["method"]; !ok {
		return call{}, false
	}
	return c, true
}

func isNull(v jsontext.Value) bool { return string(bytes.TrimSpace(v)) == "null" }

// validID is jsonrpc-core's Id: null, a u64 or a string.
func validID(v jsontext.Value) bool {
	v = bytes.TrimSpace(v)
	if isNull(v) {
		return true
	}
	if len(v) > 0 && v[0] == '"' {
		var s string
		return json.Unmarshal(v, &s, agaveInput) == nil
	}
	_, err := strconv.ParseUint(string(v), 10, 64)
	return err == nil
}

// positional checks a call's parameters against the method's arity and
// returns them, or jsonrpc-derive's refusal.
func positional(params jsontext.Value, a arity) ([]jsontext.Value, error) {
	var list []jsontext.Value
	isMap := bytes.HasPrefix(params, []byte("{"))
	if params != nil && !isMap {
		if err := json.Unmarshal(params, &list, agaveInput); err != nil {
			return nil, invalidParams("`params` should be an array")
		}
	}
	noParams := func() error {
		if params == nil || (!isMap && len(list) == 0) {
			return nil
		}
		return &rpcError{Code: codeInvalidParams, Message: "Invalid parameters: No parameters were expected",
			Data: debugParams(params)}
	}
	switch {
	case a.total == 0:
		return nil, noParams()
	case a.total > a.required:
		if isMap {
			return nil, invalidParams("`params` should be an array")
		}
		switch n := len(list); {
		case n < a.required:
			return nil, invalidParams("`params` should have at least %d argument(s)", a.required)
		case n == 0:
			return nil, noParams()
		case n > a.total:
			return nil, &rpcError{Code: codeInvalidParams,
				Message: fmt.Sprintf("Invalid parameters: Expected from %d to %d parameters.", a.required, a.total),
				Data:    strconv.Quote(fmt.Sprintf("Got: %d", n))}
		}
		return list, nil
	}
	// With every parameter required, serde reads the params as a tuple.
	tuple := fmt.Sprintf("a tuple of size %d", a.total)
	switch {
	case params == nil:
		return nil, invalidParams("Invalid params: invalid type: null, expected %s.", tuple)
	case isMap:
		return nil, invalidParams("Invalid params: invalid type: map, expected %s.", tuple)
	case len(list) < a.total:
		return nil, invalidParams("Invalid params: invalid length %d, expected %s.", len(list), tuple)
	case len(list) > a.total:
		return nil, invalidParams("Invalid params: invalid length %d, expected fewer elements in array.", len(list))
	}
	return list, nil
}

// agaveInput decodes what a client sent the way Agave does: serde_json keeps a
// repeated name's last value, where v2 would reject it.
var agaveInput = jsontext.AllowDuplicateNames(true)

// number is a JSON number exactly as written.
type number string

// MarshalJSON writes the number back as written.
func (n number) MarshalJSON() ([]byte, error) { return []byte(n), nil }

// keepNumbers makes a decode into any give a number instead of a float64, so a
// u64 above 2^53 keeps every digit.
var keepNumbers = json.WithUnmarshalers(json.UnmarshalFromFunc(func(d *jsontext.Decoder, v *any) error {
	if d.PeekKind() != '0' {
		return errors.ErrUnsupported
	}
	raw, err := d.ReadValue()
	if err != nil {
		return err
	}
	*v = number(raw)
	return nil
}))

// debugParams is Rust's {:?} of jsonrpc-core's Params, which
// invalid_params_with_details puts in data.
func debugParams(params jsontext.Value) string {
	var v any
	_ = json.Unmarshal(params, &v, keepNumbers, agaveInput)
	switch x := v.(type) {
	case []any:
		return "Array(" + debugList(x) + ")"
	case map[string]any:
		return "Map(" + debugMap(x) + ")"
	}
	return "None"
}

// debugValue is serde_json 1.0.151's Debug for Value.
func debugValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "Null"
	case bool:
		return fmt.Sprintf("Bool(%t)", x)
	case number:
		return "Number(" + debugNumber(x) + ")"
	case string:
		return "String(" + debugString(x) + ")"
	case []any:
		return "Array " + debugList(x)
	case map[string]any:
		return "Object " + debugMap(x)
	}
	return ""
}

func debugList(items []any) string {
	parts := make([]string, len(items))
	for i, e := range items {
		parts[i] = debugValue(e)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func debugMap(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = debugString(k) + ": " + debugValue(m[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// debugNumber is serde_json's Display for Number. An integer stays as
// written. A float is the shortest round trip, with ".0" on a whole one.
func debugNumber(n number) string {
	if _, err := strconv.ParseInt(string(n), 10, 64); err == nil {
		return string(n)
	}
	if _, err := strconv.ParseUint(string(n), 10, 64); err == nil {
		return string(n)
	}
	f, _ := strconv.ParseFloat(string(n), 64)
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".e") {
		s += ".0"
	}
	return s
}

// debugString is Rust's {:?} for a str.
func debugString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u{%x}`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
