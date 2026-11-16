//go:build golden

package rpc

import (
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"os"
	"testing"

	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestWriteGoldenFixtures rewrites the fixtures that tools/parsed-oracle and
// tools/tx-oracle render into golden files. `make golden` runs it and then
// both oracles.
func TestWriteGoldenFixtures(t *testing.T) {
	t.Run("parsed", func(t *testing.T) { writeFile(t, fixturesPath, parsedFixturesJSON(t, fixtures())) })
	t.Run("instructions", func(t *testing.T) { writeFile(t, ixFixturesPath, indented(t, buildInstructionCorpus())) })
	t.Run("transactions", writeTransactionFixtures)
}

func indented(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v, json.Deterministic(true), jsontext.WithIndent(" "))
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeTransactionFixtures writes the history records of the transcript's
// setup as fixtures. It adds a token transfer that does not name its mint, an
// associated token account that two CPIs create, and a v0 transfer through a
// lookup table.
func writeTransactionFixtures(t *testing.T) {
	table, stranger := tc.Pub(tc.Key(60)), tc.Pub(tc.Key(61))
	cs := newChainSideWith(t, tc.Options{Programs: tc.ManifestPrograms(t),
		Accounts: []svm.GenesisAccount{lookupTable(table, [][32]byte{stranger})}})
	setup(t, cs)
	payer := payerKey
	payerPub, recipientPub := tc.Pub(payer), tc.Pub(recipientKey)
	mint, a, b := tc.Pub(mintKey), tc.Pub(tokenAKey), tc.Pub(tokenBKey)

	cs.send(tc.Tx(cs.latest(), payer, nil,
		tokenIx(binary.LittleEndian.AppendUint64([]byte{3}, 1_000), tc.Writable(a), tc.Writable(b), tc.Signer(payerPub, false))), false)
	ataProgram := mustBase58("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL")
	ata := svm.FindProgramAddress([][]byte{recipientPub[:], svm.SplToken[:], mint[:]}, ataProgram)
	cs.send(tc.Tx(cs.latest(), payer, nil, tc.Instruction{Program: ataProgram, Data: []byte{1}, Accounts: []tc.Meta{
		tc.Signer(payerPub, true), tc.Writable(ata), tc.Readonly(recipientPub), tc.Readonly(mint),
		tc.Readonly(svm.SystemProgram), tc.Readonly(svm.SplToken)}}), false)
	cs.send(tc.TxV0(cs.latest(), payer, nil,
		[]tc.Lookup{{Table: table, Writable: []tc.LookupEntry{{Index: 0, Key: stranger}}}},
		tc.Transfer(payer, stranger, svm.MinimumBalance(0))), false)

	var out txFixtures
	var legacy, all []oracleTx
	encodings := []string{"json", "jsonParsed", "base64", "base58", "binary"}
	zero := 0
	for h := uint64(1); h <= uint64(cs.c.Height); h++ {
		records, err := cs.c.App.History.AtHeight(h)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range records {
			o := toOracle(t, r)
			name := base58.Encode(r.Signature[:8])
			out.Transactions = append(out.Transactions,
				txFixture{name + "/max-none", r.Height, r.BlockTime, r.Index, o, encodings, nil},
				txFixture{name + "/max-0", r.Height, r.BlockTime, r.Index, o, encodings, &zero})
			all = append(all, o)
			if r.Meta.Version == 0 {
				legacy = append(legacy, o)
			}
		}
	}
	hash := tc.HashAt(cs.c.Height)
	block := func(name string, txs []oracleTx, max *int) blockFixture {
		return blockFixture{name, base58.Encode(make([]byte, 32)), base58.Encode(hash[:]), uint64(cs.c.Height) - 1,
			1_700_000_000, uint64(cs.c.Height), txs, []string{"json", "jsonParsed", "base64"}, []string{"full", "accounts", "signatures", "none"}, max}
	}
	out.Blocks = []blockFixture{block("legacy", legacy, nil), block("all", all, &zero)}
	writeFile(t, txFixturesPath, indented(t, out))
}
