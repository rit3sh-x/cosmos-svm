package app_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// nameKey returns a deterministic keypair for the thousands of accounts that
// a name-shaped block needs. The seed of tc.Key is one byte, which is too few.
func nameKey(prefix string, i int) ed25519.PrivateKey {
	s := sha256.Sum256(append([]byte(prefix), byte(i), byte(i>>8), byte(i>>16)))
	return ed25519.NewKeyFromSeed(s[:])
}

func namePub(prefix string, i int) [32]byte { return tc.Pub(nameKey(prefix, i)) }

// lookupTableRow returns a genesis account that holds an extended lookup
// table. The test writes the bytes because ExtendLookupTable carries 32 bytes
// per address. A block-sized set of names would take thousands of
// transactions to build.
//
// The layout is bincode of ProgramState::LookupTable(LookupTableMeta): the u32
// variant, the two slots, the start index, an absent authority and two padding
// bytes, then the raw addresses from LOOKUP_TABLE_META_SIZE. deactivation_slot
// is u64::MAX, which makes the table Activated. last_extended_slot is 0, so
// every address is visible from block one.
//
// This is not a second decoder. If the bytes are wrong, the engine's
// AddressLookupTable::deserialize refuses them, phase 2 drops the transaction,
// and every assertion below fails.
func lookupTableRow(table [32]byte, addresses [][32]byte) svm.GenesisAccount {
	data := binary.LittleEndian.AppendUint32(nil, 1)
	data = binary.LittleEndian.AppendUint64(data, ^uint64(0))
	data = binary.LittleEndian.AppendUint64(data, 0)
	data = append(data, 0, 0, 0, 0)
	data = append(data, make([]byte, 56-len(data))...)
	for _, a := range addresses {
		data = append(data, a[:]...)
	}
	return svm.GenesisAccount{
		Pubkey:    base58.Encode(table[:]),
		Lamports:  svm.MinimumBalance(len(data)),
		Owner:     addressLookupTable,
		Data:      data,
		RentEpoch: ^uint64(0),
	}
}

// namedTx builds the transaction that a name-priced block holds: one
// transfer, a compute budget too small for execution to matter, and a table
// that supplies `loaded` readonly addresses. Four static keys plus 124 loaded
// ones make the 128 that the lock limit allows.
func namedTx(t *testing.T, blockhash [32]byte, payer ed25519.PrivateKey, dest, table [32]byte, loaded int) []byte {
	entries := make([]tc.LookupEntry, loaded)
	for i := range entries {
		entries[i] = tc.LookupEntry{Index: uint8(i), Key: namePub("filler", i)}
	}
	return tc.TxV0(blockhash, payer, nil, []tc.Lookup{{Table: table, Readonly: entries}},
		tc.Transfer(payer, dest, 1),
		computeUnitLimit(t, 450),
		loadedAccountsDataSizeLimit(t, 32*1024),
	)
}

// fillerTable returns the table that namedTx indexes into. It holds the first
// n addresses that namedTx names, so each transaction resolves as the block
// resolves it.
func fillerTable(table [32]byte, n int) svm.GenesisAccount {
	addresses := make([][32]byte, n)
	for i := range addresses {
		addresses[i] = namePub("filler", i)
	}
	return lookupTableRow(table, addresses)
}

// loadedAccountsDataSizeLimit returns ComputeBudgetInstruction tag 4, which
// the cost model turns into the loaded-accounts term. A 32 KiB limit, not the
// 64 MiB default, leaves the block's budget free for names.
func loadedAccountsDataSizeLimit(t *testing.T, bytes uint32) tc.Instruction {
	return tc.Instruction{
		Program: key(t, "ComputeBudget111111111111111111111111111111"),
		Data:    binary.LittleEndian.AppendUint32([]byte{4}, bytes),
	}
}

// TestANamedAccountCostsNameUnits proves that two transactions that differ by
// one table index differ by the price of one name, NAME_UNITS. The account
// that the extra index names does not exist. The price is GasWanted, which
// both proposal handlers charge.
func TestANamedAccountCostsNameUnits(t *testing.T) {
	faucet := tc.Key(1)
	table := namePub("table", 0)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{
		Accounts: []svm.GenesisAccount{fillerTable(table, 124)},
	})
	c.Block()

	units := func(loaded int) uint64 {
		tx := namedTx(t, c.Latest(), faucet, tc.Pub(tc.Key(2)), table, loaded)
		res := c.Check(c.Wrap(tx), false)
		if res.Code != 0 {
			t.Fatalf("a transaction naming %d table addresses was refused: %s", loaded, res.Log)
		}
		return uint64(res.GasWanted)
	}
	name := nameUnits(t)
	if got := units(11) - units(10); got != name {
		t.Fatalf("one more named account costs %d units, want %d", got, name)
	}

	// Compute the whole term by hand. Four static keys, the table's address
	// and 124 indexes make 129 names, and nothing else in the transaction
	// changes.
	bare := tc.Tx(c.Latest(), faucet, nil,
		tc.Transfer(faucet, tc.Pub(tc.Key(2)), 1),
		computeUnitLimit(t, 450),
		loadedAccountsDataSizeLimit(t, 32*1024),
	)
	a, err := c.Admit(bare)
	if err != nil {
		t.Fatal(err)
	}
	// The legacy transaction names four accounts and the v0 one names 129, so
	// the difference is 125 names. The table's addresses are readonly and add
	// no write lock.
	if got, want := units(124)-a.Units, 125*name; got != want {
		t.Fatalf("125 more names cost %d units, want %d", got, want)
	}
}

// TestATransactionNaming129AccountsIsRefused proves that admission refuses a
// transaction that names 129 accounts. The count is static, so the
// transaction never reaches a block.
func TestATransactionNaming129AccountsIsRefused(t *testing.T) {
	faucet := tc.Key(1)
	table := namePub("table", 0)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{
		Accounts: []svm.GenesisAccount{fillerTable(table, 124)},
	})
	c.Block()

	at := func(loaded int) *abci.ResponseCheckTx {
		return c.Check(c.Wrap(namedTx(t, c.Latest(), faucet, tc.Pub(tc.Key(2)), table, loaded)), false)
	}
	if res := at(124); res.Code != 0 {
		t.Fatalf("128 account keys is the limit, not one over it: %s", res.Log)
	}
	res := at(125)
	if res.Code == 0 {
		t.Fatal("a transaction naming 129 accounts was admitted")
	}
	if !strings.Contains(res.Log, "TooManyAccountLocks") {
		t.Fatalf("129 accounts were refused as %q, want TooManyAccountLocks", res.Log)
	}
}

// TestAKeyNamedStaticallyAndThroughATableIsRejected proves that a key named
// both statically and through a table is refused. `check_tx` resolves lookups
// to placeholders, so the state half of admission resolves the tables as the
// next block does and refuses the duplicate there. A block that delivers it
// anyway does not process it, with no fee, no writes and no dedup key spent.
func TestAKeyNamedStaticallyAndThroughATableIsRejected(t *testing.T) {
	faucet := tc.Key(1)
	// Two tables of one address each. One names the payer, which every message
	// carries statically. The other names a stranger. Nothing else differs, so
	// the second table proves that the refusal is for the duplicate, not for a
	// bad table encoding.
	dup, clean := namePub("table", 0), namePub("table", 1)
	stranger := namePub("stranger", 0)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Accounts: []svm.GenesisAccount{
		lookupTableRow(dup, [][32]byte{tc.Pub(faucet)}),
		lookupTableRow(clean, [][32]byte{stranger}),
	}})
	c.Block()

	build := func(table, address [32]byte, dest [32]byte) []byte {
		return tc.TxV0(c.Latest(), faucet, nil,
			[]tc.Lookup{{Table: table, Readonly: []tc.LookupEntry{{Index: 0, Key: address}}}},
			tc.Transfer(faucet, dest, svm.MinimumBalance(0)),
		)
	}

	dest := tc.Pub(tc.Key(2))
	tx := build(dup, tc.Pub(faucet), dest)
	if res := c.Check(c.Wrap(tx), false); res.Code != svm.ErrAdmission.ABCICode() || !strings.Contains(res.Log, "AccountLoadedTwice") {
		t.Fatalf("admission: code %d (%s), want %d naming AccountLoadedTwice", res.Code, res.Log, svm.ErrAdmission.ABCICode())
	}

	before := c.Lamports(tc.Pub(faucet))
	res, _ := c.Block(tx, build(clean, stranger, tc.Pub(tc.Key(3))))
	if res[0].Code != 2 {
		t.Fatalf("a duplicated account key committed with code %d", res[0].Code)
	}
	if res[1].Code != 0 {
		t.Fatalf("the same shape without the duplicate failed with code %d, so the table is what is broken", res[1].Code)
	}
	if after := c.Lamports(tc.Pub(faucet)); after != before-svm.MinimumBalance(0)-5_000 {
		t.Fatalf("the payer paid %d lamports, want only the second transaction's transfer and fee", before-after)
	}
	if _, ok := c.Account(dest); ok {
		t.Fatal("the transfer happened")
	}
}

func TestNameFloodBlocksAreInadmissible(t *testing.T) {
	faucet := tc.Key(1)
	table := namePub("table", 0)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{
		Accounts: []svm.GenesisAccount{lookupTableRow(table, nil)},
	})
	c.Block()
	maxUnits, _, _ := ffi.BlockLimits()

	// The static-key shape is the attack block. The test fills it to the byte
	// budget, not the unit budget, because without the name term the unit
	// budget lets it through.
	const fillers = 29
	wide := func(i int) []byte {
		accounts := make([]tc.Meta, 0, fillers+1)
		accounts = append(accounts, tc.Signer(tc.Pub(faucet), true))
		for j := 0; j < fillers; j++ {
			accounts = append(accounts, tc.Readonly(namePub("static", i*fillers+j)))
		}
		return tc.Tx(c.Latest(), faucet, nil,
			tc.Instruction{
				Program:  svm.SystemProgram,
				Accounts: accounts,
				Data:     binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 2), 1),
			},
			computeUnitLimit(t, 450),
			loadedAccountsDataSizeLimit(t, 32*1024),
		)
	}
	// The payer, System, ComputeBudget and the fillers. No filler exists.
	const perTx = 3 + fillers
	var txs [][]byte
	var size, units uint64
	for i := 0; size < svm.MaxBlockBytes; i++ {
		tx := wide(i)
		a, err := c.Admit(tx)
		if err != nil {
			t.Fatal(err)
		}
		wrapped := c.Wrap(tx)
		txs, size, units = append(txs, wrapped), size+uint64(len(wrapped)), units+a.Units
	}
	if got := c.Propose(txs...); got != abci.ResponseProcessProposal_REJECT {
		t.Fatalf("a block of %d wide static-key transactions was %v, want REJECT", len(txs), got)
	}
	// The name term, the eighth cost term, refuses it. Without the price of a
	// name, the same block is inside the budget.
	unpriced := units - uint64(perTx*len(txs))*nameUnits(t)
	if unpriced > maxUnits {
		t.Fatalf("the block costs %d units without the name term, over the %d budget: it does not discriminate", unpriced, maxUnits)
	}
	t.Logf("%d transactions, %d names, %d units priced against %d unpriced", len(txs), perTx*len(txs), units, unpriced)

	// In the table shape, admission refuses each transaction alone, before the
	// block is weighed.
	deep := namedTx(t, c.Latest(), faucet, tc.Pub(tc.Key(2)), table, 200)
	if _, err := c.Admit(deep); err == nil {
		t.Fatal("a transaction naming 203 accounts was admitted")
	}
	if got := c.Propose(c.Wrap(deep)); got != abci.ResponseProcessProposal_REJECT {
		t.Fatalf("a block carrying it was %v, want REJECT", got)
	}
}

// TestABlockAtTheNameLimitCommits proves that a block at the name limit
// commits. The test sizes the block from the price that the chain charges and
// the budget that the chain publishes. Each name is a distinct account that
// does not exist.
func TestABlockAtTheNameLimitCommits(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a full block of maximal loads")
	}
	const (
		loaded  = 124
		names   = 4 + 1 + loaded
		maximum = 1_700
	)
	faucet := tc.Key(1)

	// Each transaction reads its own table of its own addresses, so the block's
	// deduplicated load list is as long as its charge. A shared table costs the
	// same but loads once. That is the overcharge on honest traffic, not the
	// shape that sets the bound.
	accounts := make([]svm.GenesisAccount, 0, 3*maximum)
	for i := 0; i < maximum; i++ {
		addresses := make([][32]byte, loaded)
		for j := range addresses {
			addresses[j] = namePub("named", i*loaded+j)
		}
		accounts = append(accounts,
			lookupTableRow(namePub("t", i), addresses),
			genesisLamports(namePub("payer", i), 1_000_000_000),
			genesisLamports(namePub("dest", i), svm.MinimumBalance(0)),
		)
	}
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Accounts: accounts})
	c.Block()

	build := func(i int) []byte {
		entries := make([]tc.LookupEntry, loaded)
		for j := range entries {
			entries[j] = tc.LookupEntry{Index: uint8(j), Key: namePub("named", i*loaded+j)}
		}
		return tc.TxV0(c.Latest(), nameKey("payer", i), nil,
			[]tc.Lookup{{Table: namePub("t", i), Readonly: entries}},
			tc.Transfer(nameKey("payer", i), namePub("dest", i), 1),
			computeUnitLimit(t, 450),
			loadedAccountsDataSizeLimit(t, 32*1024),
		)
	}

	maxUnits, _, _ := ffi.BlockLimits()
	first, err := c.Admit(build(0))
	if err != nil {
		t.Fatal(err)
	}
	n := int(maxUnits / first.Units)
	if n > maximum {
		t.Fatalf("a name-shaped block holds %d transactions, over the %d genesis carries", n, maximum)
	}
	if share := float64(names*int(nameUnits(t))*n) / float64(maxUnits); share < 0.8 {
		t.Fatalf("names are only %.0f%% of the block's units; it is not a name-shaped block", 100*share)
	}

	txs := make([][]byte, n)
	for i := range txs {
		txs[i] = build(i)
	}
	res, _ := c.Block(txs...)
	for i, r := range res {
		if r.Code != 0 {
			t.Fatalf("transaction %d of a maximal naming block failed with code %d", i, r.Code)
		}
	}
	t.Logf("%d transactions, %d names charged, %d units of %d", n, names*n, n*int(first.Units), maxUnits)
}

// nameUnits reads NAME_UNITS from engine/src/cost.rs, as the rpc compatibility
// test reads its constants. A price change then needs no edit here.
func nameUnits(t *testing.T) uint64 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "engine", "src", "cost.rs"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^pub const NAME_UNITS: u64 = ([0-9_]+);`).FindSubmatch(b)
	if m == nil {
		t.Fatal("engine/src/cost.rs no longer declares NAME_UNITS")
	}
	v, err := strconv.ParseUint(strings.ReplaceAll(string(m[1]), "_", ""), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
