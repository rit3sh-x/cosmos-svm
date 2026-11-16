package rpc

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"testing"

	"cosmossdk.io/log/v2"

	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// jsonParsed is the config every test here asks for.
var jsonParsed = accountConfig{Encoding: "jsonParsed"}

func parsedConfig(t *testing.T) jsontext.Value {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"encoding": "jsonParsed"}, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestAPluralMethodDropsWhatASingularMethodRendersAsBase64 checks a token
// account whose mint is not on the chain. Without the mint's decimals it
// cannot parse. As on Agave, getAccountInfo renders it as base64 and
// getTokenAccountsByOwner drops it.
func TestAPluralMethodDropsWhatASingularMethodRendersAsBase64(t *testing.T) {
	owner := tokenKey(1)
	mint := tokenKey(2)
	orphan := tokenKey(3)
	known := tokenKey(4)
	held := tokenKey(5)

	chain := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: []svm.GenesisAccount{
		account(known, svm.SplToken, mintBytes(6, 1_000_000, 82)),
		account(held, svm.SplToken, tokenAccountBytes(known, owner, 7, 165)),
		// Its mint is never written, so nothing can supply the decimals.
		account(orphan, svm.SplToken, tokenAccountBytes(mint, owner, 1, 165)),
	}})
	s := New(chain.App, nil, log.NewNopLogger())
	chain.Block()

	first, _ := json.Marshal(base58.Encode(orphan[:]), json.Deterministic(true))
	out, err := getAccountInfo(context.Background(), s, []jsontext.Value{first, parsedConfig(t)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out, json.Deterministic(true))
	var single struct {
		Value uiAccount `json:"value"`
	}
	if err := json.Unmarshal(raw, &single); err != nil {
		t.Fatal(err)
	}
	if pair := dataPair(t, single.Value.Data); pair[1] != "base64" {
		t.Fatalf("getAccountInfo returned %v, want a base64 fallback", pair)
	}

	served := byOwner(t, s, owner, map[string]any{"programId": base58.Encode(svm.SplToken[:])})
	if len(served) != 2 {
		t.Fatalf("the base64 call served %v, want both accounts", served)
	}
	parsedOwner, _ := json.Marshal(base58.Encode(owner[:]), json.Deterministic(true))
	filter, _ := json.Marshal(map[string]any{"programId": base58.Encode(svm.SplToken[:])}, json.Deterministic(true))
	value, err := getTokenAccountsByOwner(context.Background(), s, []jsontext.Value{parsedOwner, filter, parsedConfig(t)})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := json.Marshal(value, json.Deterministic(true))
	var plural struct {
		Value []struct {
			Pubkey string `json:"pubkey"`
		} `json:"value"`
	}
	if err := json.Unmarshal(rows, &plural); err != nil {
		t.Fatal(err)
	}
	if len(plural.Value) != 1 || plural.Value[0] != struct {
		Pubkey string `json:"pubkey"`
	}{base58.Encode(held[:])} {
		t.Fatalf("the parsed call served %v, want only the account whose mint is known", plural.Value)
	}
}

// TestTwentyAccountsOnOneMintCostOneRead checks that twenty token accounts on
// one mint cost one mint read, as with Agave's `mint_data` map.
func TestTwentyAccountsOnOneMintCostOneRead(t *testing.T) {
	owner := tokenKey(1)
	mint := tokenKey(2)
	accounts := []svm.GenesisAccount{account(mint, svm.SplToken, mintBytes(6, 1_000_000, 82))}
	keys := make([][32]byte, 0, 20)
	for i := 0; i < 20; i++ {
		key := tc.Pub(ed25519.NewKeyFromSeed(append(make([]byte, 30), 9, byte(i))))
		keys = append(keys, key)
		accounts = append(accounts, account(key, svm.SplToken, tokenAccountBytes(mint, owner, uint64(i), 165)))
	}
	chain := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: accounts})
	s := New(chain.App, nil, log.NewNopLogger())
	chain.Block()

	ms, _, err := s.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	reader := s.mints(ms, jsonParsed, true)
	rendered := 0
	for _, key := range keys {
		a, err := ffi.DecodeAccount(s.app.Stores.GetAccount(ms, key))
		if err != nil {
			t.Fatal(err)
		}
		ui, err := renderAccount(key, a, jsonParsed, reader)
		if err != nil {
			t.Fatal(err)
		}
		if !parsedBody(ui) {
			t.Fatalf("%s did not parse", base58.Encode(key[:]))
		}
		rendered++
	}
	if rendered != 20 || reader.reads != 1 {
		t.Fatalf("%d accounts rendered from %d mint reads, want 20 from 1", rendered, reader.reads)
	}

	// The same twenty through the method a wallet calls.
	served := byOwner(t, s, owner, map[string]any{"programId": base58.Encode(svm.SplToken[:])})
	if len(served) != 20 {
		t.Fatalf("getTokenAccountsByOwner served %d accounts, want 20", len(served))
	}
}

// TestEveryExtensionTypeRenders proves each of the twenty-eight
// ExtensionTypes renders under its own name in the fixtures Anza's decoder
// rendered. None falls back to unparseableExtension.
func TestEveryExtensionTypeRenders(t *testing.T) {
	names := map[string]bool{}
	for _, f := range fixtures() {
		data, _ := base64.StdEncoding.DecodeString(f.Data)
		owner, _ := base58.Decode(f.Owner)
		pubkey, _ := base58.Decode(f.Pubkey)
		var m *mintData
		if f.Mint != "" {
			mint, _ := base64.StdEncoding.DecodeString(f.Mint)
			m, _ = mintDataFromAccount(mint, f.UnixTimestamp)
		}
		parsed, ok := parseAccountData([32]byte(pubkey), [32]byte(owner), data, m)
		if !ok {
			continue
		}
		body, _ := json.Marshal(parsed, json.Deterministic(true))
		var out struct {
			Parsed struct {
				Info struct {
					Extensions []uiExtension `json:"extensions"`
				} `json:"info"`
			} `json:"parsed"`
		}
		_ = json.Unmarshal(body, &out)
		for _, e := range out.Parsed.Info.Extensions {
			names[e.Extension] = true
		}
	}
	delete(names, unparseableExtension.Extension)
	if len(names) != int(extHighest) {
		t.Fatalf("%d extension types render, want all %d: %v", len(names), extHighest, names)
	}
}

// TestAParsedSubscriptionReadsItsMintFromCommittedState proves a block that
// writes a token account but not its mint still notifies a parsed body. The
// mint is read from committed state, as Agave reads it from the bank.
func TestAParsedSubscriptionReadsItsMintFromCommittedState(t *testing.T) {
	faucet := tc.Key(1)
	owner, mint, tokenAccount := k(3), k(2), k(9)
	value := tokenAccountBytes(mint, owner, 500_000, 165)
	account := func(pk [32]byte, data []byte) svm.GenesisAccount {
		return svm.GenesisAccount{Pubkey: base58.Encode(pk[:]), Lamports: svm.MinimumBalance(len(data)),
			Owner: base58.Encode(svm.SplToken[:]), Data: data, RentEpoch: ^uint64(0)}
	}
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Accounts: []svm.GenesisAccount{
		account(mint, mintBytes(6, 1_000_000, 82)), account(tokenAccount, value)}})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()
	p := dial(t, s)
	p.subscribe("accountSubscribe", base58.Encode(tokenAccount[:]), map[string]any{"encoding": "jsonParsed"})
	if res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tokenAccount, 1))); res[0].Code != 0 {
		t.Fatalf("the transfer committed as %d", res[0].Code)
	}
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	var n wsNotification
	p.read(&n)
	var got struct {
		Value struct {
			Data struct {
				Parsed struct {
					Info struct {
						TokenAmount struct {
							Decimals int `json:"decimals"`
						} `json:"tokenAmount"`
					} `json:"info"`
				} `json:"parsed"`
			} `json:"data"`
		} `json:"value"`
	}
	if err := json.Unmarshal(n.Params.Result, &got); err != nil || got.Value.Data.Parsed.Info.TokenAmount.Decimals != 6 {
		t.Fatalf("the notification is %s, want the parsed account with its mint's 6 decimals", n.Params.Result)
	}
}
