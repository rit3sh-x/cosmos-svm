package rpc

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"testing"

	"cosmossdk.io/log/v2"

	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// account is a genesis row that holds raw bytes under a named program. The
// tests build real byte layouts with it, so they check the copied state rule
// and not a mock of it.
func account(pubkey, program [32]byte, data []byte) svm.GenesisAccount {
	return svm.GenesisAccount{
		Pubkey:    base58.Encode(pubkey[:]),
		Lamports:  svm.MinimumBalance(len(data)),
		Owner:     base58.Encode(program[:]),
		Data:      data,
		RentEpoch: ^uint64(0),
	}
}

// tokenAccountBytes is spl_token::state::Account: mint, owner, amount, and the
// state byte at 108, which must be non-zero. A size over 165 makes it a
// Token-2022 extended account, which needs its account type at 165.
func tokenAccountBytes(mint, owner [32]byte, amount uint64, size int) []byte {
	data := make([]byte, size)
	copy(data, mint[:])
	copy(data[32:], owner[:])
	binary.LittleEndian.PutUint64(data[64:], amount)
	data[108] = 1
	if size > 165 {
		data[165] = 2 // AccountType::Account
	}
	return data
}

// mintBytes is spl_token::state::Mint: supply at 36, decimals at 44, the
// initialized flag at 45. A size over 165 makes it a Token-2022 extended
// mint, padded out to account length so it cannot be read as an account.
func mintBytes(decimals uint8, supply uint64, size int) []byte {
	data := make([]byte, size)
	binary.LittleEndian.PutUint64(data[36:], supply)
	data[44] = decimals
	data[45] = 1
	if size > 165 {
		data[165] = 1 // AccountType::Mint
	}
	return data
}

func tokenKey(seed byte) [32]byte {
	return tc.Pub(ed25519.NewKeyFromSeed(append(make([]byte, 31), seed)))
}

// byOwner calls the method and returns the pubkeys it served, in order. It
// asks for base64, because the binary default refuses a token account's 165
// bytes, as Agave does.
func byOwner(t *testing.T, s *Server, owner [32]byte, filter map[string]any) []string {
	t.Helper()
	first, _ := json.Marshal(base58.Encode(owner[:]), json.Deterministic(true))
	second, _ := json.Marshal(filter, json.Deterministic(true))
	out, err := getTokenAccountsByOwner(context.Background(), s, []jsontext.Value{first, second, jsontext.Value(`{"encoding":"base64"}`)})
	if err != nil {
		t.Fatalf("getTokenAccountsByOwner %v: %v", filter, err)
	}
	raw, _ := json.Marshal(out, json.Deterministic(true))
	var body struct {
		Value []struct {
			Pubkey  string `json:"pubkey"`
			Account struct {
				Owner string `json:"owner"`
			} `json:"account"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, len(body.Value))
	for i, v := range body.Value {
		keys[i] = v.Pubkey
	}
	return keys
}

func has(keys []string, k [32]byte) bool {
	for _, v := range keys {
		if v == base58.Encode(k[:]) {
			return true
		}
	}
	return false
}

// cast is the set of accounts that the token tests share: one owner with one
// Token account and one Token-2022 account of the same mint, a second mint,
// and the three shapes the state check must refuse.
type cast struct {
	owner, stranger              [32]byte
	mint, otherMint              [32]byte
	plain, second, extended      [32]byte
	multisig, extMint, mintAsAcc [32]byte
	strangers                    [32]byte
}

func tokenCast() (cast, []svm.GenesisAccount) {
	c := cast{
		owner: tokenKey(1), stranger: tokenKey(2),
		mint: tokenKey(3), otherMint: tokenKey(4),
		plain: tokenKey(5), second: tokenKey(6), extended: tokenKey(7),
		multisig: tokenKey(8), extMint: tokenKey(9), mintAsAcc: tokenKey(10),
		strangers: tokenKey(11),
	}
	return c, []svm.GenesisAccount{
		account(c.mint, svm.SplToken, mintBytes(6, 1_000_000, 82)),
		account(c.otherMint, svm.SplToken, mintBytes(0, 42, 82)),
		account(c.plain, svm.SplToken, tokenAccountBytes(c.mint, c.owner, 1_500_000, 165)),
		account(c.second, svm.SplToken, tokenAccountBytes(c.otherMint, c.owner, 7, 165)),
		account(c.extended, svm.SplToken2022, tokenAccountBytes(c.mint, c.owner, 9, 200)),
		// Exactly multisig length. Token-2022 forbids a mint or account of
		// that length, so size tells the three apart.
		account(c.multisig, svm.SplToken2022, tokenAccountBytes(c.mint, c.owner, 1, 355)),
		// An extended mint is account length or more, with AccountType::Mint.
		account(c.extMint, svm.SplToken2022, mintBytes(6, 5, 200)),
		// A plain mint owned by the token program, whose bytes 32 to 64 happen
		// to be the owner. Its length alone refuses it.
		account(c.mintAsAcc, svm.SplToken, mintBytes(2, 1, 82)),
		account(c.strangers, svm.SplToken, tokenAccountBytes(c.mint, c.stranger, 3, 165)),
	}
}

// TestGetTokenAccountsByOwner proves one prefix read of the owner index
// covers exactly the accounts that spl-generic-token's state check admits.
func TestGetTokenAccountsByOwner(t *testing.T) {
	c, accounts := tokenCast()
	chain := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: accounts})
	s := New(chain.App, nil, log.NewNopLogger())
	// The index is a function of current state, so the first block rebuilds
	// it from genesis. Genesis accounts come from no write set.
	chain.Block()

	tokens := byOwner(t, s, c.owner, map[string]any{"programId": base58.Encode(svm.SplToken[:])})
	if len(tokens) != 2 || !has(tokens, c.plain) || !has(tokens, c.second) {
		t.Fatalf("programId Token returned %v", tokens)
	}
	t2022 := byOwner(t, s, c.owner, map[string]any{"programId": base58.Encode(svm.SplToken2022[:])})
	if len(t2022) != 1 || !has(t2022, c.extended) {
		t.Fatalf("programId Token-2022 returned %v", t2022)
	}
	// A mint filter reads only the program that owns the mint, as Agave's
	// get_token_program_id_and_mint does. So the Token-2022 account that
	// names the same mint is not served.
	sameMint := byOwner(t, s, c.owner, map[string]any{"mint": base58.Encode(c.mint[:])})
	if len(sameMint) != 1 || !has(sameMint, c.plain) {
		t.Fatalf("mint filter returned %v", sameMint)
	}

	// The three refused shapes are in nobody's index, under any filter.
	for _, filter := range []map[string]any{
		{"programId": base58.Encode(svm.SplToken[:])},
		{"programId": base58.Encode(svm.SplToken2022[:])},
		{"mint": base58.Encode(c.mint[:])},
	} {
		got := byOwner(t, s, c.owner, filter)
		for name, key := range map[string][32]byte{"multisig": c.multisig, "extended mint": c.extMint, "mint": c.mintAsAcc} {
			if has(got, key) {
				t.Fatalf("a %s was served as a token account under %v", name, filter)
			}
		}
	}

	// Another owner's account is another owner's.
	if got := byOwner(t, s, c.stranger, map[string]any{"mint": base58.Encode(c.mint[:])}); len(got) != 1 || !has(got, c.strangers) {
		t.Fatalf("the stranger has %v", got)
	}

	// Agave requires exactly one of the two filters.
	first, _ := json.Marshal(base58.Encode(c.owner[:]), json.Deterministic(true))
	for _, bad := range []map[string]any{{}, {"mint": base58.Encode(c.mint[:]), "programId": base58.Encode(svm.SplToken[:])}} {
		second, _ := json.Marshal(bad, json.Deterministic(true))
		if _, err := getTokenAccountsByOwner(context.Background(), s, []jsontext.Value{first, second}); err == nil {
			t.Fatalf("the filter %v was accepted", bad)
		}
	}
}

// TestGetTokenAccountBalance proves the two point reads: the account for the
// amount, and the mint for the decimals.
func TestGetTokenAccountBalance(t *testing.T) {
	c, accounts := tokenCast()
	chain := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: accounts})
	s := New(chain.App, nil, log.NewNopLogger())
	chain.Block()

	balance := func(key [32]byte) (uiTokenAmount, error) {
		first, _ := json.Marshal(base58.Encode(key[:]), json.Deterministic(true))
		out, err := getTokenAccountBalance(context.Background(), s, []jsontext.Value{first})
		if err != nil {
			return uiTokenAmount{}, err
		}
		raw, _ := json.Marshal(out, json.Deterministic(true))
		var body struct {
			Value uiTokenAmount `json:"value"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		return body.Value, nil
	}

	got, err := balance(c.plain)
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount != "1500000" || got.Decimals != 6 || got.UIAmountString != "1.5" || got.UIAmount == nil || *got.UIAmount != 1.5 {
		t.Fatalf("a six-decimal balance rendered as %+v", got)
	}
	// A zero-decimal amount has no point, and its string is the amount.
	if got, err = balance(c.second); err != nil || got.Decimals != 0 || got.UIAmountString != "7" || got.Amount != "7" {
		t.Fatalf("a zero-decimal balance rendered as %+v, %v", got, err)
	}
	// Agave refuses a mint and an account that does not exist.
	if _, err := balance(c.mintAsAcc); err == nil {
		t.Fatal("a mint was accepted as a token account")
	}
	if _, err := balance(tokenKey(99)); err == nil {
		t.Fatal("an account that does not exist was accepted")
	}
}

// TestTheTokenIndexRebuilds proves the index is a pure function of current
// account state. Losing it, or building it under older rules, costs one pass
// and never a wrong answer.
func TestTheTokenIndexRebuilds(t *testing.T) {
	c, accounts := tokenCast()
	chain := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: accounts})
	s := New(chain.App, nil, log.NewNopLogger())
	chain.Block()
	want := byOwner(t, s, c.owner, map[string]any{"programId": base58.Encode(svm.SplToken[:])})
	if len(want) != 2 {
		t.Fatalf("the owner holds %v before anything is broken", want)
	}

	// Delete every key, as on a machine that joined by state sync or lost
	// the directory.
	db := chain.TokenIndexDB()
	it, err := db.Iterator(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var keys [][]byte
	for ; it.Valid(); it.Next() {
		keys = append(keys, append([]byte(nil), it.Key()...))
	}
	it.Close()
	for _, k := range keys {
		if err := db.Delete(k); err != nil {
			t.Fatal(err)
		}
	}
	chain.Restart()
	s = New(chain.App, nil, log.NewNopLogger())
	// Until a rebuild has read every shard, the index answers that it is
	// behind, never with a partial list.
	first, _ := json.Marshal(base58.Encode(c.owner[:]), json.Deterministic(true))
	if _, err := getTokenAccountsByOwner(context.Background(), s, []jsontext.Value{first, jsontext.Value(`{"programId":"` + base58.Encode(svm.SplToken[:]) + `"}`)}); err == nil {
		t.Fatal("a deleted index answered before a block rebuilt it")
	} else if e, ok := err.(*rpcError); !ok || e.Code != codeNodeUnhealthy {
		t.Fatalf("a deleted index answered %v, want node unhealthy", err)
	}
	chain.Block()
	chain.App.Tokens.Wait()
	if got := byOwner(t, s, c.owner, map[string]any{"programId": base58.Encode(svm.SplToken[:])}); len(got) != len(want) {
		t.Fatalf("the rebuilt index answers %v, want %v", got, want)
	}

	// A version marker from older rules, with the entries still in place. A
	// stale entry must not survive the rebuild.
	ghost := tokenKey(77)
	if err := db.Set(append([]byte{'o'}, append(c.owner[:], ghost[:]...)...), []byte{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Set([]byte{'m'}, append([]byte{0}, binary.BigEndian.AppendUint64(nil, uint64(chain.Height))...)); err != nil {
		t.Fatal(err)
	}
	chain.Restart()
	s = New(chain.App, nil, log.NewNopLogger())
	chain.Block()
	chain.App.Tokens.Wait()
	got := byOwner(t, s, c.owner, map[string]any{"programId": base58.Encode(svm.SplToken[:])})
	if has(got, ghost) || len(got) != 2 {
		t.Fatalf("a version bump left %v behind", got)
	}
}

// TestTheWritePathMaintainsTheIndex proves the write path maintains the index
// from the block's write set. The reverse keyspace lets an account move to a
// new owner without the commit path reading anything back.
func TestTheWritePathMaintainsTheIndex(t *testing.T) {
	c, accounts := tokenCast()
	payer, ownerKey, newOwnerKey, fresh := tc.Key(1), tc.Key(51), tc.Key(52), tc.Key(53)
	owner, newOwner, account := tc.Pub(ownerKey), tc.Pub(newOwnerKey), tc.Pub(fresh)
	chain := tc.Start(t, tc.Pub(payer), tc.Options{Accounts: accounts, Programs: tc.ManifestPrograms(t)})
	s := New(chain.App, nil, log.NewNopLogger())
	chain.Block()

	mintFilter := map[string]any{"mint": base58.Encode(c.mint[:])}
	create := binary.LittleEndian.AppendUint32(nil, 0) // SystemInstruction::CreateAccount
	create = binary.LittleEndian.AppendUint64(create, svm.MinimumBalance(165))
	create = binary.LittleEndian.AppendUint64(create, 165)
	create = append(create, svm.SplToken[:]...)
	initialize := append([]byte{18}, owner[:]...) // InitializeAccount3
	commit := func(name string, tx []byte) {
		t.Helper()
		if res, _ := chain.Block(tx); res[0].Code != 0 {
			t.Fatalf("%s: code %d", name, res[0].Code)
		}
	}
	commit("create", tc.Tx(chain.Latest(), payer, []ed25519.PrivateKey{fresh},
		tc.Instruction{Program: svm.SystemProgram, Accounts: []tc.Meta{tc.Signer(tc.Pub(payer), true), tc.Signer(account, true)}, Data: create},
		tc.Instruction{Program: svm.SplToken, Accounts: []tc.Meta{tc.Writable(account), tc.Readonly(c.mint)}, Data: initialize},
	))
	if got := byOwner(t, s, owner, mintFilter); len(got) != 1 || !has(got, account) {
		t.Fatalf("a token account the block created is %v", got)
	}

	// SetAuthority, AuthorityType::AccountOwner, to a second wallet. Without
	// the reverse keyspace the old entry would survive and both owners would
	// claim it.
	authority := append([]byte{6, 2, 1}, newOwner[:]...)
	commit("set authority", tc.Tx(chain.Latest(), payer, []ed25519.PrivateKey{ownerKey},
		tc.Instruction{Program: svm.SplToken, Accounts: []tc.Meta{tc.Writable(account), tc.Signer(owner, false)}, Data: authority},
	))
	if got := byOwner(t, s, owner, mintFilter); len(got) != 0 {
		t.Fatalf("the old owner still holds %v", got)
	}
	if got := byOwner(t, s, newOwner, mintFilter); len(got) != 1 || !has(got, account) {
		t.Fatalf("the new owner holds %v", got)
	}

	// CloseAccount zeroes the lamports, so the write set deletes the account.
	// The index must forget it, not serve a pubkey with nothing behind it.
	commit("close", tc.Tx(chain.Latest(), payer, []ed25519.PrivateKey{newOwnerKey},
		tc.Instruction{Program: svm.SplToken, Accounts: []tc.Meta{
			tc.Writable(account), tc.Writable(tc.Pub(payer)), tc.Signer(newOwner, false),
		}, Data: []byte{9}},
	))
	if got := byOwner(t, s, newOwner, mintFilter); len(got) != 0 {
		t.Fatalf("a closed account is still indexed: %v", got)
	}
}
