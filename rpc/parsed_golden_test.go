package rpc

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/mr-tron/base58"
)

// TestParsedRenderingsMatchAnzasDecoder compares the Go parsers with Anza's
// decoder. `tools/parsed-oracle` renders fixtures() with
// `solana-account-decoder`. Run `make golden-parsed` after a change to
// fixtures(): `make golden`.

const (
	fixturesPath = "testdata/parsed_fixtures.json"
	goldenPath   = "testdata/parsed_golden.json"
)

type fixture struct {
	Name          string `json:"name"`
	Pubkey        string `json:"pubkey"`
	Owner         string `json:"owner"`
	Data          string `json:"data"`
	Mint          string `json:"mint,omitempty"`
	UnixTimestamp int64  `json:"unixTimestamp,omitzero"`
}

type golden struct {
	Name   string         `json:"name"`
	Parsed jsontext.Value `json:"parsed"`
}

// divergent names the fixtures where this chain renders something other than
// Agave on purpose, with the reason. Everything else must match byte for
// byte.
var divergent = map[string]string{}

func parsedFixturesJSON(t *testing.T, cases []fixture) []byte {
	t.Helper()
	raw, err := json.Marshal(cases, json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func TestParsedRenderingsMatchAnzasDecoder(t *testing.T) {
	cases := fixtures()
	raw := parsedFixturesJSON(t, cases)
	// The golden file comes from the committed fixtures. If the two drift,
	// the test compares the parsers against the wrong bytes.
	committed, err := os.ReadFile(fixturesPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, raw) {
		t.Fatalf("%s is stale; run make golden", fixturesPath)
	}

	var want []golden
	goldenRaw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(goldenRaw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(cases) {
		t.Fatalf("the golden file holds %d rows for %d fixtures", len(want), len(cases))
	}

	for i, f := range cases {
		if want[i].Name != f.Name {
			t.Fatalf("golden row %d is %q, want %q", i, want[i].Name, f.Name)
		}
		var mint *mintData
		if f.Mint != "" {
			mint, _ = mintDataFromAccount(decode64(t, f.Mint), f.UnixTimestamp)
		}
		parsed, ok := parseAccountData(key58(t, f.Pubkey), key58(t, f.Owner), decode64(t, f.Data), mint)

		reason, diverges := divergent[f.Name]
		anza := canonical(t, want[i].Parsed)
		if !ok {
			if anza != "null" && !diverges {
				t.Errorf("%s: we render base64, Anza renders %s", f.Name, anza)
			}
			continue
		}
		ours := canonical(t, mustJSON(t, parsed))
		switch {
		case diverges && ours == anza:
			t.Errorf("%s no longer diverges from Anza; drop it from the divergent list (%s)", f.Name, reason)
		case diverges:
			t.Logf("%s diverges as decided: %s", f.Name, reason)
		case ours != anza:
			t.Errorf("%s\n ours: %s\nanza: %s", f.Name, ours, anza)
		}
	}
}

// canonical re-serialises with keys sorted, as serde_json's own BTreeMap
// writes the golden file. It normalises float literals, because serde writes
// an integral f64 as `5.0` and Go writes it as `5`. It leaves an integer
// literal verbatim, so a u64 past the float range is not rounded.
func canonical(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v, keepNumbers); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(normaliseNumbers(t, v), json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func normaliseNumbers(t *testing.T, v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, inner := range x {
			x[k] = normaliseNumbers(t, inner)
		}
	case []any:
		for i, inner := range x {
			x[i] = normaliseNumbers(t, inner)
		}
	case number:
		if !strings.ContainsAny(string(x), ".eE") {
			return x
		}
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			t.Fatal(err)
		}
		return number(strconv.FormatFloat(f, 'g', -1, 64))
	}
	return v
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func decode64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func key58(t *testing.T, s string) [32]byte {
	t.Helper()
	b, err := base58.Decode(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("bad key %q", s)
	}
	return [32]byte(b)
}
