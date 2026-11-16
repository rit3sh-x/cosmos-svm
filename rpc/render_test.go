package rpc

import (
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"slices"
	"testing"

	"cosmossdk.io/log/v2"
	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

type uiBalance struct {
	AccountIndex  int    `json:"accountIndex"`
	Mint          string `json:"mint"`
	Owner         string `json:"owner"`
	ProgramID     string `json:"programId"`
	UITokenAmount struct {
		Amount   string `json:"amount"`
		Decimals int    `json:"decimals"`
	} `json:"uiTokenAmount"`
}

func amounts(list []uiBalance) []string {
	out := []string{}
	for _, b := range list {
		out = append(out, b.UITokenAmount.Amount)
	}
	slices.Sort(out)
	return out
}

// TestATokenTransferRecordsItsBalances checks that a token Transfer, which
// names no mint, records both token balances with the mint's decimals, because
// PushAccounts brings the mint. It also checks the recorded cost and the
// default json rendering of the message.
func TestATokenTransferRecordsItsBalances(t *testing.T) {
	owner := tc.Key(20)
	mint, from, to := tokenKey(30), tokenKey(31), tokenKey(32)
	c := tc.Start(t, tc.Pub(owner), tc.Options{Programs: tc.ManifestPrograms(t), Accounts: []svm.GenesisAccount{
		account(mint, svm.SplToken, mintBytes(6, 1_000_000, 82)),
		account(from, svm.SplToken, tokenAccountBytes(mint, tc.Pub(owner), 1_000_000, 165)),
		account(to, svm.SplToken, tokenAccountBytes(mint, tokenKey(33), 0, 165)),
	}})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()
	tx := tc.Tx(c.Latest(), owner, nil, tokenIx(binary.LittleEndian.AppendUint64([]byte{3}, 250_000),
		tc.Writable(from), tc.Writable(to), tc.Signer(tc.Pub(owner), false)))
	if res, _ := c.Block(tx); res[0].Code != 0 {
		t.Fatalf("transfer: code %d", res[0].Code)
	}
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	out, rerr := invoke(t, s, getTransaction, sigOf(tx))
	if rerr != nil {
		t.Fatal(rerr)
	}
	raw, _ := json.Marshal(out, json.Deterministic(true))
	var got struct {
		Meta struct {
			PreTokenBalances  []uiBalance `json:"preTokenBalances"`
			PostTokenBalances []uiBalance `json:"postTokenBalances"`
			CostUnits         *uint64     `json:"costUnits"`
		} `json:"meta"`
		Transaction struct {
			Message struct {
				Instructions []struct {
					StackHeight int `json:"stackHeight"`
				} `json:"instructions"`
			} `json:"message"`
		} `json:"transaction"`
		TransactionIndex *int           `json:"transactionIndex"`
		Version          jsontext.Value `json:"version"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	m := got.Meta
	if len(m.PreTokenBalances) != 2 || len(m.PostTokenBalances) != 2 {
		t.Fatalf("token balances %d and %d, want 2 and 2: %s", len(m.PreTokenBalances), len(m.PostTokenBalances), raw)
	}
	for _, b := range append(m.PreTokenBalances, m.PostTokenBalances...) {
		if b.Mint != base58.Encode(mint[:]) || b.UITokenAmount.Decimals != 6 || b.ProgramID != base58.Encode(svm.SplToken[:]) {
			t.Fatalf("balance %+v", b)
		}
	}
	if pre, post := amounts(m.PreTokenBalances), amounts(m.PostTokenBalances); !slices.Equal(pre, []string{"0", "1000000"}) ||
		!slices.Equal(post, []string{"250000", "750000"}) {
		t.Fatalf("amounts %v then %v", pre, post)
	}
	if m.CostUnits == nil || *m.CostUnits == 0 {
		t.Fatal("no cost recorded")
	}
	if got.TransactionIndex == nil || *got.TransactionIndex != 0 || got.Version != nil {
		t.Fatalf("transactionIndex %v, version %s: want 0 and absent for legacy without a maximum", got.TransactionIndex, got.Version)
	}
	if len(got.Transaction.Message.Instructions) != 1 || got.Transaction.Message.Instructions[0].StackHeight != 1 {
		t.Fatalf("instructions %s", raw)
	}
}

// TestSimulateServesAccountsInnerInstructionsAndBalances simulates an
// associated token account created through two CPIs. It proves the account
// comes from the simulation (committed state has none), the inner
// instructions come only if asked for, and the balances come always.
func TestSimulateServesAccountsInnerInstructionsAndBalances(t *testing.T) {
	payer := tc.Key(1)
	mint, wallet := tokenKey(30), tokenKey(34)
	c := tc.Start(t, tc.Pub(payer), tc.Options{Programs: tc.ManifestPrograms(t), Accounts: []svm.GenesisAccount{
		account(mint, svm.SplToken, mintBytes(6, 0, 82)),
	}})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()
	ataProgram := mustBase58("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL")
	ata := svm.FindProgramAddress([][]byte{wallet[:], svm.SplToken[:], mint[:]}, ataProgram)
	create := tc.Tx(c.Latest(), payer, nil, tc.Instruction{Program: ataProgram, Data: []byte{1}, Accounts: []tc.Meta{
		tc.Signer(tc.Pub(payer), true), tc.Writable(ata), tc.Readonly(wallet), tc.Readonly(mint),
		tc.Readonly(svm.SystemProgram), tc.Readonly(svm.SplToken)}})
	encoded := base58.Encode(create)
	stranger := tokenKey(35)
	nowhere := base58.Encode(stranger[:])
	simulate := func(cfg map[string]any) map[string]jsontext.Value {
		t.Helper()
		out, rerr := invoke(t, s, simulateTransaction, encoded, cfg)
		if rerr != nil {
			t.Fatal(rerr)
		}
		var value map[string]jsontext.Value
		answerValue(t, out, &value)
		return value
	}

	v := simulate(map[string]any{"innerInstructions": true,
		"accounts": map[string]any{"addresses": []string{base58.Encode(ata[:]), nowhere}, "encoding": "base64"}})
	if string(v["err"]) != "null" {
		t.Fatalf("err %s", v["err"])
	}
	var accounts []*struct {
		Owner    string `json:"owner"`
		Lamports uint64 `json:"lamports"`
	}
	if err := json.Unmarshal(v["accounts"], &accounts); err != nil || len(accounts) != 2 || accounts[0] == nil || accounts[1] != nil {
		t.Fatalf("accounts %s", v["accounts"])
	}
	if accounts[0].Owner != base58.Encode(svm.SplToken[:]) || accounts[0].Lamports == 0 {
		t.Fatalf("the simulated account %+v", accounts[0])
	}
	var inner []struct {
		Instructions []struct {
			ProgramID string `json:"programId"`
		} `json:"instructions"`
	}
	if err := json.Unmarshal(v["innerInstructions"], &inner); err != nil || len(inner) != 1 {
		t.Fatalf("inner instructions %s", v["innerInstructions"])
	}
	programs := map[string]bool{}
	for _, ix := range inner[0].Instructions {
		programs[ix.ProgramID] = true
	}
	if !programs[base58.Encode(svm.SystemProgram[:])] || !programs[base58.Encode(svm.SplToken[:])] {
		t.Fatalf("the CPIs reached %v", programs)
	}
	var pre, post []uint64
	var preTokens, postTokens []uiBalance
	for field, into := range map[string]any{"preBalances": &pre, "postBalances": &post, "preTokenBalances": &preTokens, "postTokenBalances": &postTokens} {
		if err := json.Unmarshal(v[field], into); err != nil {
			t.Fatalf("%s %s", field, v[field])
		}
	}
	if len(pre) != 7 || len(post) != 7 || len(preTokens) != 0 || len(postTokens) != 1 || postTokens[0].UITokenAmount.Decimals != 6 {
		t.Fatalf("balances %v %v, token balances %v %v", pre, post, preTokens, postTokens)
	}

	if v := simulate(nil); string(v["innerInstructions"]) != "null" || string(v["accounts"]) != "null" {
		t.Fatalf("unasked: inner instructions %s, accounts %s", v["innerInstructions"], v["accounts"])
	}
	for cfg, want := range map[string]string{
		`{"accounts":{"addresses":[],"encoding":"base58"}}`:            "base58 encoding not supported",
		`{"accounts":{"addresses":["a","b","c","d","e","f","g","h"]}}`: "Too many accounts provided; max 7",
	} {
		var config map[string]any
		_ = json.Unmarshal([]byte(cfg), &config)
		if _, rerr := invoke(t, s, simulateTransaction, encoded, config); rerr == nil || rerr.Message != want {
			t.Errorf("%s: %v, want %q", cfg, rerr, want)
		}
	}

	// A failed simulation answers every address with null, unread.
	failed := base58.Encode(tc.Tx(c.Latest(), payer, nil, tc.Transfer(payer, tokenKey(36), ^uint64(0)>>1)))
	out, rerr := invoke(t, s, simulateTransaction, failed, map[string]any{"accounts": map[string]any{"addresses": []string{"not a key"}}})
	if rerr != nil {
		t.Fatal(rerr)
	}
	var value map[string]jsontext.Value
	answerValue(t, out, &value)
	if string(value["accounts"]) != "[null]" || string(value["preBalances"]) == "null" {
		t.Fatalf("failed: accounts %s, balances %s", value["accounts"], value["preBalances"])
	}
}

// TestTheMemoIsAgaves checks that the memo matches extract_and_fmt_memos. Each
// Memo instruction renders as "[length] text", the parts are joined, and a
// transaction without one has null.
func TestTheMemoIsAgaves(t *testing.T) {
	payer := tc.Key(1)
	c := tc.Start(t, tc.Pub(payer), tc.Options{Programs: tc.ManifestPrograms(t)})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()
	memoV1 := mustBase58("Memo1UhkJRfHyvLMcVucJwxXeuD728EqVDDwQDxFMNo")
	memo := func(program [32]byte, text string) tc.Instruction {
		return tc.Instruction{Program: program, Accounts: []tc.Meta{tc.Signer(tc.Pub(payer), false)}, Data: []byte(text)}
	}
	c.Block(tc.Tx(c.Latest(), payer, nil, memo(memoV3, "hello"), memo(memoV1, "\xff")))
	c.Block(tc.Tx(c.Latest(), payer, nil, tc.Transfer(payer, tokenKey(37), svm.MinimumBalance(0))))
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	payerPub := tc.Pub(payer)
	out, rerr := invoke(t, s, getSignaturesForAddress, base58.Encode(payerPub[:]))
	if rerr != nil {
		t.Fatal(rerr)
	}
	raw, _ := json.Marshal(out, json.Deterministic(true))
	var rows []struct {
		Memo *string `json:"memo"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) != 2 {
		t.Fatalf("rows %s", raw)
	}
	if rows[0].Memo != nil || rows[1].Memo == nil || *rows[1].Memo != "[5] hello; [1] (unparseable)" {
		t.Fatalf("memos %s", raw)
	}
}
