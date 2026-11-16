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

// tokenLike stands in for an SPL Token account. It is 165 bytes, and its first
// 32 bytes are the mint, which a client filters on.
func tokenLike(seed byte, mint [32]byte, owner [32]byte) svm.GenesisAccount {
	pubkey := tc.Pub(tc.Key(seed))
	data := make([]byte, 165)
	copy(data, mint[:])
	data[64] = seed
	return svm.GenesisAccount{
		Pubkey:    base58.Encode(pubkey[:]),
		Lamports:  svm.MinimumBalance(len(data)),
		Owner:     base58.Encode(owner[:]),
		Data:      data,
		RentEpoch: ^uint64(0),
	}
}

// scan asks for base64 unless cfg names an encoding, because the binary
// default refuses an account over 128 bytes, as Agave does.
func scan(t *testing.T, s *Server, program [32]byte, cfg string) []programAccount {
	t.Helper()
	first, _ := json.Marshal(base58.Encode(program[:]), json.Deterministic(true))
	c := map[string]any{}
	if cfg != "" {
		if err := json.Unmarshal([]byte(cfg), &c); err != nil {
			t.Fatal(err)
		}
	}
	if c["encoding"] == nil {
		c["encoding"] = "base64"
	}
	second, _ := json.Marshal(c, json.Deterministic(true))
	params := []jsontext.Value{first, second}
	out, err := getProgramAccounts(context.Background(), s, params)
	if err != nil {
		t.Fatal(err)
	}
	return out.([]programAccount)
}

// TestProgramAccountsScanFiltersAndSlices proves the scan reads every shard
// and applies the filters and the data slice in memory, because the owner is
// in the value. A client that filters token accounts by mint needs this.
func TestProgramAccountsScanFiltersAndSlices(t *testing.T) {
	token := tc.Pub(tc.Key(200))
	other := tc.Pub(tc.Key(201))
	mint, otherMint := tc.Pub(tc.Key(202)), tc.Pub(tc.Key(203))
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: []svm.GenesisAccount{
		tokenLike(10, mint, token),
		tokenLike(11, mint, token),
		tokenLike(12, otherMint, token),
		tokenLike(13, mint, other),
	}})
	c.Block()
	s := New(c.App, nil, log.NewNopLogger())

	if got := scan(t, s, token, ""); len(got) != 3 {
		t.Fatalf("the token program owns %d accounts, want 3", len(got))
	}
	byMint := `{"encoding":"base64","filters":[{"dataSize":165},{"memcmp":{"offset":0,"bytes":"` + base58.Encode(mint[:]) + `"}}]}`
	got := scan(t, s, token, byMint)
	if len(got) != 2 {
		t.Fatalf("the mint filter returned %d accounts, want 2", len(got))
	}
	for _, a := range got {
		raw, err := base64.StdEncoding.DecodeString(dataPair(t, a.Account.Data)[0])
		if err != nil {
			t.Fatal(err)
		}
		if [32]byte(raw[:32]) != mint {
			t.Fatalf("%s is not a %s account", a.Pubkey, base58.Encode(mint[:]))
		}
		if a.Account.Space != 165 {
			t.Fatalf("%s reports space %d", a.Pubkey, a.Account.Space)
		}
	}

	// The same bytes as a raw array of numbers. Some clients send this form.
	numbers := make([]int, 32)
	for i, b := range mint {
		numbers[i] = int(b)
	}
	array, _ := json.Marshal(numbers, json.Deterministic(true))
	if got := scan(t, s, token, `{"filters":[{"memcmp":{"offset":0,"bytes":`+string(array)+`}}]}`); len(got) != 2 {
		t.Fatalf("the mint filter as a byte array returned %d accounts, want 2", len(got))
	}
	first, _ := json.Marshal(base58.Encode(token[:]), json.Deterministic(true))
	if _, err := getProgramAccounts(context.Background(), s, []jsontext.Value{first, jsontext.Value(`{"filters":[{"memcmp":{"offset":0,"bytes":[256]}}]}`)}); err == nil {
		t.Fatal("a byte of 256 was accepted")
	}

	// Hyperlane's indexer pairs a memcmp with a data slice to find a message
	// PDA it cannot derive.
	sliced := `{"encoding":"base64","dataSlice":{"offset":64,"length":1},` +
		`"filters":[{"memcmp":{"offset":0,"bytes":"` + base58.Encode(mint[:]) + `"}}]}`
	for _, a := range scan(t, s, token, sliced) {
		raw, err := base64.StdEncoding.DecodeString(dataPair(t, a.Account.Data)[0])
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) != 1 || (raw[0] != 10 && raw[0] != 11) {
			t.Fatalf("%s sliced to %x", a.Pubkey, raw)
		}
		if a.Account.Space != 165 {
			t.Fatalf("a slice changed the reported space to %d", a.Account.Space)
		}
	}

	// A base64 memcmp that matches nothing, and one filter too many.
	none := `{"filters":[{"memcmp":{"offset":0,"bytes":"` + base64.StdEncoding.EncodeToString(otherMint[:]) + `","encoding":"base64"}}]}`
	if got := scan(t, s, other, none); len(got) != 0 {
		t.Fatalf("a filter matching nothing returned %d accounts", len(got))
	}
	tooMany := `{"filters":[{"dataSize":1},{"dataSize":2},{"dataSize":3},{"dataSize":4},{"dataSize":5}]}`
	if _, err := getProgramAccounts(context.Background(), s, []jsontext.Value{first, jsontext.Value(tooMany)}); err == nil {
		t.Fatal("five filters were accepted")
	}
}

// TestNoSingletonAppearsInAScan proves no scan reaches the svmmeta store,
// which holds the blockhash, the engine digest and the gate set.
func TestNoSingletonAppearsInAScan(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()
	ms := c.App.CommitMultiStore()

	meta := 0
	it := ms.GetKVStore(c.App.Stores.Meta).Iterator(nil, nil)
	for ; it.Valid(); it.Next() {
		meta++
	}
	it.Close()
	if meta == 0 {
		t.Fatal("svmmeta is empty, so this test would pass vacuously")
	}

	// Every value in every shard is an account, and every key is a 32-byte
	// pubkey. A singleton in a shard would fail one of the two.
	accounts := 0
	for _, key := range c.App.Stores.Accounts {
		it := ms.GetKVStore(key).Iterator(nil, nil)
		for ; it.Valid(); it.Next() {
			if len(it.Key()) != 32 {
				t.Fatalf("shard key %q is not a pubkey", it.Key())
			}
			if _, err := ffi.DecodeAccount(it.Value()); err != nil {
				t.Fatalf("shard key %x does not hold an account: %v", it.Key(), err)
			}
			accounts++
		}
		it.Close()
	}

	// The scan reaches every one of them and nothing else.
	s := New(c.App, nil, log.NewNopLogger())
	owners := map[[32]byte]bool{}
	for _, key := range c.App.Stores.Accounts {
		it := ms.GetKVStore(key).Iterator(nil, nil)
		for ; it.Valid(); it.Next() {
			a, err := ffi.DecodeAccount(it.Value())
			if err != nil {
				t.Fatal(err)
			}
			owners[a.Owner] = true
		}
		it.Close()
	}
	scanned := 0
	for owner := range owners {
		scanned += len(scan(t, s, owner, ""))
	}
	if scanned != accounts {
		t.Fatalf("the scans returned %d accounts, the shards hold %d", scanned, accounts)
	}
}

// TestAScanEndsWithItsClient proves a getProgramAccounts whose client hung up
// stops scanning, and does not read every account for an unread answer.
func TestAScanEndsWithItsClient(t *testing.T) {
	token := tc.Pub(tc.Key(200))
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: []svm.GenesisAccount{tokenLike(10, tc.Pub(tc.Key(202)), token)}})
	c.Block()
	s := New(c.App, nil, log.NewNopLogger())
	first, _ := json.Marshal(base58.Encode(token[:]), json.Deterministic(true))
	gone, hangUp := context.WithCancel(context.Background())
	hangUp()
	if _, err := getProgramAccounts(gone, s, []jsontext.Value{first}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a scan for a client that hung up: %v, want context.Canceled", err)
	}
	if got := scan(t, s, token, ""); len(got) != 1 {
		t.Fatalf("the same scan for a client still there: %d accounts, want 1", len(got))
	}
}
