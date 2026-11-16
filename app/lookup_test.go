package app_test

import (
	"crypto/ed25519"
	"encoding/binary"
	"testing"

	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

const addressLookupTable = "AddressLookupTab1e1111111111111111111111111"

// createLookupTable returns a ProgramInstruction::CreateLookupTable transaction
// (bincode with a u32 enum tag) and the table address. The table address is
// the PDA of the authority and the recent slot. The program derives it again
// from the bump that the instruction carries.
func createLookupTable(blockhash [32]byte, authority ed25519.PrivateKey, recentSlot uint64, alt [32]byte) ([]byte, [32]byte) {
	who := tc.Pub(authority)
	seed := binary.LittleEndian.AppendUint64(nil, recentSlot)
	table, bump := svm.FindProgramAddressAndBump([][]byte{who[:], seed}, alt)
	data := binary.LittleEndian.AppendUint32(nil, 0)
	data = binary.LittleEndian.AppendUint64(data, recentSlot)
	data = append(data, bump)
	return tc.Tx(blockhash, authority, nil, tc.Instruction{
		Program: alt,
		Accounts: []tc.Meta{
			tc.Writable(table),
			tc.Signer(who, false),
			tc.Signer(who, true),
			tc.Readonly(svm.SystemProgram),
		},
		Data: data,
	}), table
}

// extendLookupTable returns a ProgramInstruction::ExtendLookupTable
// transaction. Its data is the u32 tag, then a bincode Vec<Pubkey>, which is a
// u64 length and the addresses.
func extendLookupTable(blockhash [32]byte, authority ed25519.PrivateKey, table, alt [32]byte, addresses [][32]byte) []byte {
	who := tc.Pub(authority)
	data := binary.LittleEndian.AppendUint32(nil, 2)
	data = binary.LittleEndian.AppendUint64(data, uint64(len(addresses)))
	for _, a := range addresses {
		data = append(data, a[:]...)
	}
	return tc.Tx(blockhash, authority, nil, tc.Instruction{
		Program: alt,
		Accounts: []tc.Meta{
			tc.Writable(table),
			tc.Signer(who, false),
			tc.Signer(who, true),
			tc.Readonly(svm.SystemProgram),
		},
		Data: data,
	})
}

// altSimple returns a transaction with one instruction on the table and the
// authority, which Deactivate and Close both take, plus any extra accounts.
func altSimple(blockhash [32]byte, authority ed25519.PrivateKey, table, alt [32]byte, tag uint32, extra ...tc.Meta) []byte {
	who := tc.Pub(authority)
	accounts := append([]tc.Meta{tc.Writable(table), tc.Signer(who, false)}, extra...)
	return tc.Tx(blockhash, authority, nil, tc.Instruction{
		Program:  alt,
		Accounts: accounts,
		Data:     binary.LittleEndian.AppendUint32(nil, tag),
	})
}

// TestLookupTableIsCreatedExtendedAndUsed walks the whole ALT path. A table is
// created and extended. Then a v0 transaction in a later block executes
// against an address that the table holds, not one that its message carries.
func TestLookupTableIsCreatedExtendedAndUsed(t *testing.T) {
	alt := key(t, addressLookupTable)
	authority := tc.Key(1)
	c := tc.Start(t, tc.Pub(authority), tc.Options{Programs: tc.ManifestPrograms(t)})

	// A slot that never happened is not a recent slot, in block 1 or any other.
	early, _ := createLookupTable(tc.HashAt(1), authority, 9, alt)
	res, _ := c.Block(early)
	if res[0].Code != 1 {
		t.Fatalf("CreateLookupTable naming a slot that does not exist: code %d, want 1", res[0].Code)
	}

	// From block 2, slot 1 is in SlotHashes.
	create, table := createLookupTable(c.Latest(), authority, 1, alt)
	res, _ = c.Block(create)
	if res[0].Code != 0 {
		t.Fatalf("CreateLookupTable in block 2: code %d", res[0].Code)
	}
	account, ok := c.Account(table)
	if !ok || account.Owner != alt {
		t.Fatalf("table %x: present %v, owner %x", table, ok, account.Owner)
	}

	// One address for each destination, so only a resolved key can reach them.
	destinations := [][32]byte{tc.Pub(tc.Key(80)), tc.Pub(tc.Key(81))}
	res, _ = c.Block(extendLookupTable(c.Latest(), authority, table, alt, destinations))
	if res[0].Code != 0 {
		t.Fatalf("ExtendLookupTable: code %d", res[0].Code)
	}

	// An address appended in one block is not visible to a lookup in that same
	// block, so the transfer goes in the next one.
	amount := svm.MinimumBalance(0) + 7
	before := c.Lamports(destinations[0])
	spend := tc.TxV0(c.Latest(), authority, nil,
		[]tc.Lookup{{Table: table, Writable: []tc.LookupEntry{{Index: 0, Key: destinations[0]}}}},
		tc.Transfer(authority, destinations[0], amount))
	res, _ = c.Block(spend)
	if res[0].Code != 0 {
		t.Fatalf("v0 transfer through the lookup table: code %d", res[0].Code)
	}
	if got := c.Lamports(destinations[0]) - before; got != amount {
		t.Fatalf("destination gained %d lamports, want %d", got, amount)
	}

	// loadedAddresses must be filled. An indexer that reads only the raw
	// message keys misses every account from a table, and nothing else fails.
	records, err := c.App.History.AtHeight(uint64(c.Height))
	if err != nil || len(records) != 1 {
		t.Fatalf("history at height %d: %d records, %v", c.Height, len(records), err)
	}
	meta := records[0].Meta
	if len(meta.LoadedWritable) != 1 || meta.LoadedWritable[0] != destinations[0] || len(meta.LoadedReadonly) != 0 {
		t.Fatalf("loadedAddresses: writable %x, readonly %x", meta.LoadedWritable, meta.LoadedReadonly)
	}
}

// TestSlotZeroIsRecentOnlyWhileSlotHashesIsPadded proves that slot 0 is a
// recent slot only while the SlotHashes account has padding. Agave sizes the
// account at `SlotHashes::size_of()`, 8 + 512 * 40 bytes, so the tail after the
// real entries is zero, and x/svm/sysvars.go writes the same shape. An on-chain
// reader takes that tail as entries of slot 0, so block 1 can create a table at
// the address derived from slot 0.
func TestSlotZeroIsRecentOnlyWhileSlotHashesIsPadded(t *testing.T) {
	alt := key(t, addressLookupTable)
	authority := tc.Key(1)
	c := tc.Start(t, tc.Pub(authority), tc.Options{Programs: tc.ManifestPrograms(t)})

	create, table := createLookupTable(tc.HashAt(1), authority, 0, alt)
	if res, _ := c.Block(create); res[0].Code != 0 {
		t.Fatalf("CreateLookupTable at slot 0 in block 1: code %d", res[0].Code)
	}

	// Deactivate the table, then try to close it while its deactivation slot
	// is still recent. A close waits until that slot ages out of the 512
	// entries, and by then the padding is gone. So the padding window cannot
	// create a table again at an address that someone already trusts.
	if res, _ := c.Block(altSimple(c.Latest(), authority, table, alt, 3)); res[0].Code != 0 {
		t.Fatalf("DeactivateLookupTable: code %d", res[0].Code)
	}
	close := altSimple(c.Latest(), authority, table, alt, 4, tc.Writable(tc.Pub(tc.Key(85))))
	if res, _ := c.Block(close); res[0].Code == 0 {
		t.Fatal("CloseLookupTable succeeded while the deactivation slot was still recent")
	}

	// Run past the padding. Slot 0 stops being recent when all 512 entries
	// are real.
	for c.Height < 515 {
		c.Block()
	}
	shrunk, _ := createLookupTable(c.Latest(), tc.Key(86), 0, alt)
	if res, _ := c.Block(shrunk); res[0].Code == 0 {
		t.Fatalf("slot 0 is still a recent slot at height %d, with no padding left", c.Height)
	}
	recent, _ := createLookupTable(c.Latest(), authority, uint64(c.Height)-1, alt)
	if res, _ := c.Block(recent); res[0].Code != 0 {
		t.Fatalf("CreateLookupTable at the parent slot: code %d", res[0].Code)
	}
}

// TestConflictGroupingUsesResolvedKeys proves that conflict grouping reads the
// resolved access set, not the static one. Two v0 transactions from different
// payers share one destination that only a table lookup shows. If grouping read
// static keys, they would be in independent groups and could run in either
// order.
func TestConflictGroupingUsesResolvedKeys(t *testing.T) {
	alt := key(t, addressLookupTable)
	authority := tc.Key(1)
	first, second := tc.Key(90), tc.Key(91)
	c := tc.Start(t, tc.Pub(authority), tc.Options{
		Programs: tc.ManifestPrograms(t),
		Accounts: []svm.GenesisAccount{funded(first), funded(second)},
	})
	c.Block()

	create, table := createLookupTable(c.Latest(), authority, 1, alt)
	if res, _ := c.Block(create); res[0].Code != 0 {
		t.Fatalf("CreateLookupTable: code %d", res[0].Code)
	}
	shared := tc.Pub(tc.Key(92))
	if res, _ := c.Block(extendLookupTable(c.Latest(), authority, table, alt, [][32]byte{shared})); res[0].Code != 0 {
		t.Fatalf("ExtendLookupTable: code %d", res[0].Code)
	}

	hash := c.Latest()
	lookups := []tc.Lookup{{Table: table, Writable: []tc.LookupEntry{{Index: 0, Key: shared}}}}
	res, _ := c.Block(
		tc.TxV0(hash, first, nil, lookups, tc.Transfer(first, shared, svm.MinimumBalance(0)+1)),
		tc.TxV0(hash, second, nil, lookups, tc.Transfer(second, shared, svm.MinimumBalance(0)+2)),
	)
	for i, r := range res {
		if r.Code != 0 {
			t.Fatalf("v0 transfer %d: code %d", i, r.Code)
		}
	}
	a, b := groupOf(res[0].Events), groupOf(res[1].Events)
	if a == "" || a != b {
		t.Fatalf("two transactions writing one table-resolved account ran in groups %q and %q", a, b)
	}
}

func funded(k ed25519.PrivateKey) svm.GenesisAccount {
	pk := tc.Pub(k)
	owner := svm.SystemProgram
	return svm.GenesisAccount{
		Pubkey:    base58.Encode(pk[:]),
		Lamports:  1_000_000_000,
		Owner:     base58.Encode(owner[:]),
		RentEpoch: ^uint64(0),
	}
}
