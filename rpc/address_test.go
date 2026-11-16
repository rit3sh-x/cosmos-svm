package rpc

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"slices"
	"testing"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/mr-tron/base58"

	"cosmossdk.io/log/v2"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

type signatureRow struct {
	Signature          string         `json:"signature"`
	Slot               uint64         `json:"slot"`
	Err                jsontext.Value `json:"err"`
	Memo               jsontext.Value `json:"memo"`
	BlockTime          int64          `json:"blockTime"`
	ConfirmationStatus string         `json:"confirmationStatus"`
	TransactionIndex   uint32         `json:"transactionIndex"`
}

func signaturesFor(t *testing.T, s *Server, address [32]byte, cfg map[string]any) []signatureRow {
	t.Helper()
	key, _ := json.Marshal(base58.Encode(address[:]), json.Deterministic(true))
	params := []jsontext.Value{key}
	if cfg != nil {
		raw, _ := json.Marshal(cfg, json.Deterministic(true))
		params = append(params, raw)
	}
	out, err := getSignaturesForAddress(context.Background(), s, params)
	if err != nil {
		t.Fatalf("getSignaturesForAddress: %v", err)
	}
	raw, _ := json.Marshal(out, json.Deterministic(true))
	var rows []signatureRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// dest returns a destination derived from i, outside the seeds the other tests use.
func dest(i int) [32]byte {
	return tc.Pub(ed25519.NewKeyFromSeed(append(make([]byte, 28), 'd', 0, 0, byte(i))))
}

// TestGetSignaturesForAddress checks that getSignaturesForAddress returns the
// address index in the order and shape mainnet returns.
func TestGetSignaturesForAddress(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()

	const blocks = 5
	var sigs [][64]byte
	for i := 0; i < blocks; i++ {
		tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest(i), svm.MinimumBalance(0)))
		if res, _ := c.Block(tx); res[0].Code != 0 {
			t.Fatalf("block %d: code %d", i, res[0].Code)
		}
		if err := s.notify(uint64(c.Height)); err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, signatureOf(tx))
	}
	b58 := func(sig [64]byte) string { return base58.Encode(sig[:]) }

	// The payer signed all five, newest first.
	rows := signaturesFor(t, s, tc.Pub(faucet), nil)
	if len(rows) != blocks {
		t.Fatalf("the payer has %d signatures, want %d", len(rows), blocks)
	}
	for i, r := range rows {
		want := sigs[blocks-1-i]
		if r.Signature != b58(want) {
			t.Fatalf("row %d is %s, want %s: the order is not newest first", i, r.Signature, b58(want))
		}
		if r.ConfirmationStatus != "finalized" || r.TransactionIndex != 0 || r.BlockTime == 0 {
			t.Fatalf("row %d is %+v", i, r)
		}
		if r.Err != nil && string(r.Err) != "null" {
			t.Fatalf("row %d of a block of successes carries %s", i, r.Err)
		}
		// These transfers carry no memo instruction, so memo is null.
		if r.Memo != nil && string(r.Memo) != "null" {
			t.Fatalf("row %d carries a memo: %s", i, r.Memo)
		}
	}

	// Exactly one transaction writes each destination.
	if rows := signaturesFor(t, s, dest(2), nil); len(rows) != 1 || rows[0].Signature != b58(sigs[2]) {
		t.Fatalf("the destination of block 2 has %d signatures", len(rows))
	}
	// An address nothing named has none.
	if rows := signaturesFor(t, s, tc.Pub(tc.Key(9)), nil); len(rows) != 0 {
		t.Fatalf("an unnamed address has %d signatures", len(rows))
	}

	// limit takes the newest.
	rows = signaturesFor(t, s, tc.Pub(faucet), map[string]any{"limit": 2})
	if len(rows) != 2 || rows[0].Signature != b58(sigs[4]) || rows[1].Signature != b58(sigs[3]) {
		t.Fatalf("limit 2 returned %d rows starting at %s", len(rows), rows[0].Signature)
	}

	// before is exclusive and searches backwards from there.
	rows = signaturesFor(t, s, tc.Pub(faucet), map[string]any{"before": b58(sigs[3])})
	if len(rows) != 3 || rows[0].Signature != b58(sigs[2]) {
		t.Fatalf("before returned %d rows starting at %s", len(rows), rows[0].Signature)
	}

	// until is exclusive too, and stops the walk.
	rows = signaturesFor(t, s, tc.Pub(faucet), map[string]any{"until": b58(sigs[1])})
	if len(rows) != 3 || rows[len(rows)-1].Signature != b58(sigs[2]) {
		t.Fatalf("until returned %d rows ending at %s", len(rows), rows[len(rows)-1].Signature)
	}

	// Both together name a window.
	rows = signaturesFor(t, s, tc.Pub(faucet), map[string]any{"before": b58(sigs[4]), "until": b58(sigs[1])})
	if len(rows) != 2 || rows[0].Signature != b58(sigs[3]) || rows[1].Signature != b58(sigs[2]) {
		t.Fatalf("the window returned %d rows", len(rows))
	}

	// An unknown before or until is Agave's FilterTransactionNotFound.
	unknown := b58([64]byte{7})
	for _, bound := range []string{"before", "until"} {
		payer := tc.Pub(faucet)
		key, _ := json.Marshal(base58.Encode(payer[:]), json.Deterministic(true))
		cfg, _ := json.Marshal(map[string]any{bound: unknown}, json.Deterministic(true))
		_, err := getSignaturesForAddress(context.Background(), s, []jsontext.Value{key, cfg})
		if re := asRPCError(err); err == nil || re.Code != codeFilterTransactionNotFound || re.Message != "Transaction "+unknown+" not found" {
			t.Fatalf("an unknown %s returned %v", bound, err)
		}
	}

	// A limit outside Agave's bounds is an error.
	for _, bad := range []int{0, maxSignaturesForAddress + 1} {
		raw, _ := json.Marshal(map[string]any{"limit": bad}, json.Deterministic(true))
		payer := tc.Pub(faucet)
		key, _ := json.Marshal(base58.Encode(payer[:]), json.Deterministic(true))
		if _, err := getSignaturesForAddress(context.Background(), s, []jsontext.Value{key, raw}); err == nil {
			t.Fatalf("limit %d was accepted", bad)
		}
	}
}

// TestAFailedTransactionIsInTheAddressIndex checks that a failed transaction is
// in the address index, with the err rendering that getTransaction serves.
func TestAFailedTransactionIsInTheAddressIndex(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()

	tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest(0), tc.FaucetLamports*2))
	if res, _ := c.Block(tx); res[0].Code == 0 {
		t.Fatal("a transfer of more than the payer holds committed")
	}
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	rows := signaturesFor(t, s, tc.Pub(faucet), nil)
	if len(rows) != 1 {
		t.Fatalf("a failed transaction is %d rows", len(rows))
	}
	if want := `{"InstructionError":[0,{"Custom":1}]}`; string(rows[0].Err) != want {
		t.Fatalf("the row's err is %s, want %s", rows[0].Err, want)
	}
}

// lookupTable returns a genesis account that holds an extended table. The
// layout is bincode of ProgramState::LookupTable(LookupTableMeta), with the
// addresses from LOOKUP_TABLE_META_SIZE.
func lookupTable(table [32]byte, addresses [][32]byte) svm.GenesisAccount {
	data := binary.LittleEndian.AppendUint32(nil, 1)
	data = binary.LittleEndian.AppendUint64(data, ^uint64(0))
	data = append(data, make([]byte, 56-len(data))...)
	for _, a := range addresses {
		data = append(data, a[:]...)
	}
	return svm.GenesisAccount{
		Pubkey:    base58.Encode(table[:]),
		Lamports:  svm.MinimumBalance(len(data)),
		Owner:     "AddressLookupTab1e1111111111111111111111111",
		Data:      data,
		RentEpoch: ^uint64(0),
	}
}

// TestAnAddressReachedOnlyThroughATableAppears checks that the index holds an
// address that only a lookup table loads. An index of the static message keys
// misses such an address, and no error shows the gap.
func TestAnAddressReachedOnlyThroughATableAppears(t *testing.T) {
	faucet := tc.Key(1)
	table, stranger := tc.Pub(tc.Key(60)), tc.Pub(tc.Key(61))
	c := tc.Start(t, tc.Pub(faucet), tc.Options{
		Accounts: []svm.GenesisAccount{lookupTable(table, [][32]byte{stranger})},
	})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()

	tx := tc.TxV0(c.Latest(), faucet, nil,
		[]tc.Lookup{{Table: table, Readonly: []tc.LookupEntry{{Index: 0, Key: stranger}}}},
		tc.Transfer(faucet, dest(0), svm.MinimumBalance(0)),
	)
	if res, _ := c.Block(tx); res[0].Code != 0 {
		t.Fatalf("the v0 transaction failed with code %d", res[0].Code)
	}
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	rows := signaturesFor(t, s, stranger, nil)
	if len(rows) != 1 || rows[0].Signature != base58.Encode(tx[1:65]) {
		t.Fatalf("an address reached only through a table has %d signatures", len(rows))
	}
	// The table account is not an account key of the message, so it has no
	// rows. Agave indexes only the account keys.
	if rows := signaturesFor(t, s, table, nil); len(rows) != 0 {
		t.Fatalf("the table is not an account key of the message but has %d signatures", len(rows))
	}
}

// TestTheAddressIndexIsInTheByteCounter checks that address entries count
// against index-max-bytes.
func TestTheAddressIndexIsInTheByteCounter(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()

	before := c.App.History.Bytes()
	if res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest(0), svm.MinimumBalance(0)))); res[0].Code != 0 {
		t.Fatal("the transfer failed")
	}
	// Payer, destination and the System program make three entries of 45 bytes.
	const entry = 1 + 32 + 8 + 4
	total, dangling, err := c.App.History.AddressEntries()
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || dangling != 0 {
		t.Fatalf("a transfer wrote %d address entries, %d dangling, want 3 and 0", total, dangling)
	}
	if got := c.App.History.Bytes() - before; got < 3*entry {
		t.Fatalf("the transaction added %d bytes, less than the %d its address entries take", got, 3*entry)
	}
}

// TestARecordWithoutStoredKeysStillPurgesItsEntries checks a record whose meta
// has no StaticKeys. History re-derives its address entries through the engine
// on write and on purge. Both paths index the same set of addresses.
func TestARecordWithoutStoredKeysStillPurgesItsEntries(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest(0), svm.MinimumBalance(0)))
	if res, _ := c.Block(tx); res[0].Code != 0 {
		t.Fatal("the transfer failed")
	}
	records, err := c.App.History.AtHeight(uint64(c.Height))
	if err != nil || len(records) != 1 {
		t.Fatalf("%d records (%v), want the transfer's", len(records), err)
	}
	stored := records[0]
	fresh, err := stored.Addresses()
	if err != nil {
		t.Fatal(err)
	}
	old := stored
	old.Meta.StaticKeys = nil
	parsed, err := old.Addresses()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fresh, parsed) {
		t.Fatalf("the stored keys index %x, the engine's parse %x", fresh, parsed)
	}

	// A separate history holds two blocks: the record without keys, then a
	// block that takes the bytes over budget and purges the record.
	h, err := svm.OpenHistory(dbm.NewMemDB(), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	old.Height, old.Index = 1, 0
	if err := h.Write(1, [32]byte{1}, 0, []svm.Record{old}); err != nil {
		t.Fatal(err)
	}
	if err := h.Write(2, [32]byte{2}, 0, nil); err != nil {
		t.Fatal(err)
	}
	total, dangling, err := h.AddressEntries()
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || dangling != 0 {
		t.Fatalf("after purging the old-format record %d address entries remain, %d dangling, want none", total, dangling)
	}
	if h.FirstAvailableBlock() != 2 {
		t.Fatalf("the first available block is %d, want 2", h.FirstAvailableBlock())
	}
}

// TestPurgingRemovesTheAddressEntries checks that a purge deletes the address
// entries of each purged record. The pubkey is the key prefix, so no height
// range covers the entries. The purge re-derives them from each record it deletes.
func TestPurgingRemovesTheAddressEntries(t *testing.T) {
	const budget = 32 * 1024
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{IndexMaxBytes: budget})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()

	for i := 0; i < 300; i++ {
		tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest(i), svm.MinimumBalance(0)))
		if res, _ := c.Block(tx); res[0].Code != 0 {
			t.Fatalf("block %d: code %d", i, res[0].Code)
		}
		if err := s.notify(uint64(c.Height)); err != nil {
			t.Fatal(err)
		}
		if got := c.App.History.Bytes(); got > budget {
			t.Fatalf("height %d: history holds %d bytes over the %d budget", c.Height, got, budget)
		}
	}

	total, dangling, err := c.App.History.AddressEntries()
	if err != nil {
		t.Fatal(err)
	}
	if dangling != 0 {
		t.Fatalf("%d of %d address entries point at a record that was purged", dangling, total)
	}
	// The payer signed all 300 and the index holds only the retained ones, so
	// the purge removed the entries as well as the records.
	rows := signaturesFor(t, s, tc.Pub(faucet), nil)
	if len(rows) == 0 || len(rows) >= 300 {
		t.Fatalf("the payer has %d of 300 signatures after purging under a %d byte budget", len(rows), budget)
	}
	// Three entries per transaction, and the payer is in every one of them.
	if total != 3*len(rows) {
		t.Fatalf("%d address entries for %d retained transactions", total, len(rows))
	}
	// A purged destination reads as nothing, not as a dangling row.
	if got := signaturesFor(t, s, dest(0), nil); len(got) != 0 {
		t.Fatalf("the first destination survived 300 blocks under a %d byte budget", budget)
	}
}
