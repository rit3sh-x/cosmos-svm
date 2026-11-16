package rpc

import "testing"

// dataPair reads a binary account body out of the `data` union. A decoded
// notification or response carries it as a two-element array. A parsed body
// carries an object instead.
func dataPair(t *testing.T, v any) [2]string {
	t.Helper()
	if direct, ok := v.([2]string); ok {
		return direct
	}
	pair, ok := v.([]any)
	if !ok || len(pair) != 2 {
		t.Fatalf("account data is %#v, want a [body, encoding] pair", v)
	}
	body, okBody := pair[0].(string)
	encoding, okEncoding := pair[1].(string)
	if !okBody || !okEncoding {
		t.Fatalf("account data is %#v, want two strings", v)
	}
	return [2]string{body, encoding}
}
