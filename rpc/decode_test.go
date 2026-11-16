package rpc

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"testing"

	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// padded builds a transaction of exactly size bytes by sizing build's data.
func padded(t *testing.T, size int, build func(data []byte) []byte) []byte {
	t.Helper()
	n := size
	for i := 0; i < 3; i++ {
		n += size - len(build(make([]byte, n)))
	}
	tx := build(make([]byte, n))
	if len(tx) != size {
		t.Fatalf("built %d bytes, want %d", len(tx), size)
	}
	return tx
}

// TestTheDecodeCapsAreAgaves checks that simulateTransaction and
// sendTransaction refuse an oversized transaction before they read it, with
// Agave's message. Both decode through one function.
func TestTheDecodeCapsAreAgaves(t *testing.T) {
	c, s := server(t)
	faucet, program := tc.Key(1), tc.Pub(tc.Key(9))
	legacy := func(size int) []byte {
		return padded(t, size, func(data []byte) []byte {
			return tc.Tx(c.Latest(), faucet, nil, tc.Instruction{Program: program, Data: data})
		})
	}
	v1 := func(size int) []byte {
		return padded(t, size, func(data []byte) []byte {
			return tc.TxV1(c.Latest(), tc.V1Config{}, faucet, nil, tc.Instruction{Program: program, Data: data})
		})
	}
	encode := func(tx []byte, encoding string) string {
		if encoding == "base58" {
			return base58.Encode(tx)
		}
		return base64.StdEncoding.EncodeToString(tx)
	}
	const name = "solana_transaction::versioned::VersionedTransaction"
	// The base58 cap is 1,683 characters, the encoding of 1,232 bytes of 0xff.
	// A 1,240-byte transaction is over the cap, so the decode never starts.
	wide := legacy(1240)
	for _, m := range []struct {
		name   string
		method handler
	}{{"simulateTransaction", simulateTransaction}, {"sendTransaction", sendTransaction}} {
		for _, tt := range []struct {
			tx       []byte
			encoding string
			want     string
		}{
			{legacy(1233), "base64", "decoded " + name + " too large: 1233 bytes (max: 1232 bytes)"},
			{wide, "base58", fmt.Sprintf("base58 encoded %s too large: %d bytes (max: encoded/raw 1683/1232)",
				name, len(base58.Encode(wide)))},
			{v1(4097), "base64", "decoded " + name + " too large: 4097 bytes (max: 4096 bytes)"},
		} {
			e, _ := json.Marshal(encode(tt.tx, tt.encoding), json.Deterministic(true))
			cfg, _ := json.Marshal(map[string]any{"encoding": tt.encoding}, json.Deterministic(true))
			_, err := m.method(context.Background(), s, []jsontext.Value{e, cfg})
			var rpcErr *rpcError
			if !errors.As(err, &rpcErr) || rpcErr.Code != codeInvalidParams || rpcErr.Message != tt.want {
				t.Errorf("%s of %d bytes in %s: %v, want %q", m.name, len(tt.tx), tt.encoding, err, tt.want)
			}
		}
	}
	// A legacy transaction may be 1,232 bytes in either encoding. Only a V1
	// transaction, whose base64 starts with "gQ", may be 4,096 bytes.
	for _, tt := range []struct {
		tx       []byte
		encoding string
	}{{legacy(1232), "base64"}, {legacy(1232), "base58"}, {v1(4096), "base64"}} {
		if _, err := decodeTransaction(encode(tt.tx, tt.encoding), tt.encoding); err != nil {
			t.Errorf("%d bytes in %s: %v", len(tt.tx), tt.encoding, err)
		}
	}
}

// TestASimulationChecksItsAccountLocks checks that a simulation with a
// duplicated key returns AccountLoadedTwice, as Agave's simulation batch does.
func TestASimulationChecksItsAccountLocks(t *testing.T) {
	c, s := server(t)
	faucet, dest := tc.Key(1), tc.Pub(tc.Key(2))
	tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest, 1_000_000_000))
	// The message's key list is payer, dest, System. A second dest adds one
	// more copy of its 32 bytes and one more key to the counts.
	tx = duplicateSecondKey(t, tx)
	e, _ := json.Marshal(base64.StdEncoding.EncodeToString(tx), json.Deterministic(true))
	cfg, _ := json.Marshal(map[string]any{"encoding": "base64"}, json.Deterministic(true))
	out, err := simulateTransaction(context.Background(), s, []jsontext.Value{e, cfg})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out, json.Deterministic(true))
	var got struct {
		Value struct {
			Err jsontext.Value `json:"err"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if string(got.Value.Err) != `"AccountLoadedTwice"` {
		t.Fatalf("simulating a duplicated key: %s, want AccountLoadedTwice", raw)
	}
}

// duplicateSecondKey appends a copy of the second key of a one-signature
// legacy transaction as a readonly account. The signature then does not match,
// and a simulation without sigVerify does not check it.
func duplicateSecondKey(t *testing.T, tx []byte) []byte {
	t.Helper()
	const msg = 1 + 64
	if tx[0] != 1 || tx[msg] >= 0x80 || tx[msg+3] >= 0x80 {
		t.Fatal("not a one-signature legacy transaction with a one-byte key count")
	}
	keys, n := msg+4, int(tx[msg+3])
	out := append([]byte(nil), tx[:keys+32*n]...)
	out[msg+2]++ // num_readonly_unsigned_accounts
	out[msg+3]++ // the key count
	out = append(out, tx[keys+32:keys+64]...)
	return append(out, tx[keys+32*n:]...)
}
