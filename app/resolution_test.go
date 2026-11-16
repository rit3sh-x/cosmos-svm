package app_test

import (
	"crypto/ed25519"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// hotAccounts returns the spenders and the funded hot accounts that spread
// sends to.
func hotAccounts(spent int) []svm.GenesisAccount {
	accounts := spenders(spent)
	for i := 0; i < 4; i++ {
		accounts = append(accounts, funded(nameKey("hot", i)))
	}
	return accounts
}

// spread builds transfers whose costs sum to exactly total. It splits them
// into equal shares on hot accounts 0, 1 and on, each share under an
// account's limit, paid by spenders from index from onward.
func spread(t *testing.T, c *tc.Chain, from int, total uint64) [][]byte {
	t.Helper()
	const most = 20_000_000
	n := (total + most - 1) / most
	var txs [][]byte
	for i := uint64(0); i < n; i++ {
		part := total / n
		if i == 0 {
			part += total % n
		}
		batch := share(t, c, from, tc.Pub(nameKey("hot", int(i))), part)
		txs = append(txs, batch...)
		from += len(batch)
	}
	return txs
}

// signatureOf is a transaction's first signature, after its one-byte count.
func signatureOf(tx []byte) [64]byte { return [64]byte(tx[1:65]) }

// payerOf is a transaction's first account key. It sits past the signatures,
// a v0 message's version byte, the three-byte header and the one-byte key
// count.
func payerOf(tx []byte) [32]byte {
	m := tx[1+64*int(tx[0]):]
	if m[0]&0x80 != 0 {
		m = m[1:]
	}
	return [32]byte(m[4:36])
}

// TestATransactionOverABudgetIsLeftUnprocessed proves that FinalizeBlock
// leaves a transaction over a budget unprocessed and runs the rest. Both
// proposal handlers refuse such a block, so Deliver hands it to FinalizeBlock
// directly. The transaction over gets code 2 in codespace svm, uses no gas,
// emits no event, pays nothing, leaves no history record and keeps its dedup
// key unspent.
func TestATransactionOverABudgetIsLeftUnprocessed(t *testing.T) {
	limit := ffi.MaxWritableAccountUnits()
	maxUnits, maxAllocated, _ := ffi.BlockLimits()
	for _, shape := range []struct {
		name  string
		block func(t *testing.T, c *tc.Chain) [][]byte
	}{
		{"an account's share", func(t *testing.T, c *tc.Chain) [][]byte {
			return share(t, c, 0, tc.Pub(nameKey("hot", 0)), limit+1)
		}},
		{"the block's units", func(t *testing.T, c *tc.Chain) [][]byte {
			return spread(t, c, 0, maxUnits+1)
		}},
		{"the block's allocation", func(t *testing.T, c *tc.Chain) [][]byte {
			// Ten accounts of 10 MiB pass 100,000,000 bytes at the tenth.
			var txs [][]byte
			for i, allocated := 0, uint64(0); allocated <= maxAllocated; i++ {
				payer, account := spender(i), nameKey("big", i)
				txs = append(txs, c.Wrap(tc.Tx(c.Latest(), payer, []ed25519.PrivateKey{account},
					createAccount(payer, account, 10<<20, svm.SystemProgram))))
				allocated += 10 << 20
			}
			return txs
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: hotAccounts(64)})
			c.Block()
			block := shape.block(t, c)
			if got := c.Propose(block...); got != abci.ResponseProcessProposal_REJECT {
				t.Fatalf("the block over the budget: %v, want REJECT", got)
			}
			last := block[len(block)-1]
			over := c.Unwrap(last)
			payer := payerOf(over)
			before := c.Lamports(payer)

			res, _ := c.Deliver(block...)
			for i, r := range res[:len(res)-1] {
				if r.Code == 2 {
					t.Fatalf("transaction %d of %d, inside the budget, was not processed", i, len(res))
				}
			}
			r := res[len(res)-1]
			if r.Code != 2 || r.Codespace != svm.ModuleName || r.GasUsed != 0 || len(r.Events) != 0 {
				t.Fatalf("the transaction over: code %d in %q, gas %d, %d events; want 2 in %q, none, none",
					r.Code, r.Codespace, r.GasUsed, len(r.Events), svm.ModuleName)
			}
			if got := c.Lamports(payer); got != before {
				t.Fatalf("its payer went from %d to %d lamports", before, got)
			}
			if _, found, err := c.App.History.Get(signatureOf(over), uint64(c.Height)); err != nil || found {
				t.Fatalf("a history record for it: found %v, %v", found, err)
			}
			if res, _ := c.Deliver(last); res[0].Code == 2 {
				t.Fatal("the same bytes in the next block were not processed: its dedup key was spent")
			}
		})
	}
}

// TestADeployThroughALookupTablePaysTheDeployTerm proves that a transaction
// that gets the upgradeable loader through a lookup table pays the deploy term
// on the 64 MiB that it declares by default. ProcessProposal accepts a block
// that fits this resolved cost exactly, and rejects one unit more or one sized
// for the static keys alone. The block records the transaction at that cost.
func TestADeployThroughALookupTablePaysTheDeployTerm(t *testing.T) {
	loader := key(t, "BPFLoaderUpgradeab1e11111111111111111111111")
	table := namePub("table", 0)
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Programs: tc.ManifestPrograms(t),
		Accounts: append(hotAccounts(64), lookupTableRow(table, [][32]byte{loader}))})
	c.Block()
	maxUnits, _, _ := ffi.BlockLimits()

	payer := spender(63)
	call := memo(key(t, memoV4), "handed the loader")
	call.Accounts = append(call.Accounts, tc.Readonly(loader))
	deploy := tc.TxV0(c.Latest(), payer, nil, []tc.Lookup{{Table: table, Readonly: []tc.LookupEntry{{Index: 0, Key: loader}}}}, call)
	a, err := c.Admit(deploy)
	if err != nil {
		t.Fatal(err)
	}
	resolved := a.Units + uint64(64<<20)/11

	if got := c.Propose(append(spread(t, c, 0, maxUnits-resolved), c.Wrap(deploy))...); got != abci.ResponseProcessProposal_ACCEPT {
		t.Fatalf("a block that fits the resolved cost exactly: %v, want ACCEPT", got)
	}
	if got := c.Propose(append(spread(t, c, 0, maxUnits-resolved+1), c.Wrap(deploy))...); got != abci.ResponseProcessProposal_REJECT {
		t.Fatalf("one unit over: %v, want REJECT", got)
	}
	if got := c.Propose(append(spread(t, c, 0, maxUnits-a.Units), c.Wrap(deploy))...); got != abci.ResponseProcessProposal_REJECT {
		t.Fatalf("a block that fits only the cost the static keys see: %v, want REJECT", got)
	}

	res, _ := c.Deliver(append(spread(t, c, 0, maxUnits-resolved), c.Wrap(deploy))...)
	for i, r := range res {
		if r.Code == 2 {
			t.Fatalf("transaction %d of %d was not processed in the block Process accepted", i, len(res))
		}
	}
	record, found, err := c.App.History.Get(signatureOf(deploy), uint64(c.Height))
	if err != nil || !found || record.Meta.CostUnits == nil {
		t.Fatalf("the deploy's record: found %v, %v", found, err)
	}
	if *record.Meta.CostUnits != resolved {
		t.Fatalf("recorded at %d units, counted at %d", *record.Meta.CostUnits, resolved)
	}
}

// TestAProposalAndItsBlockResolveTablesAlike proves that ProcessProposal and
// FinalizeBlock resolve tables alike, at the same slot and before any SVM
// transaction runs. An address extended or a table deactivated in the block
// before resolves for both, so a block at the hot account's share through the
// table is accepted and runs whole. An address extended in the block itself is
// visible to neither, as in Agave.
func TestAProposalAndItsBlockResolveTablesAlike(t *testing.T) {
	alt := key(t, addressLookupTable)
	authority := tc.Key(1)
	hot, late := nameKey("hot", 0), nameKey("hot", 1)
	c := tc.Start(t, tc.Pub(authority), tc.Options{Programs: tc.ManifestPrograms(t), Accounts: hotAccounts(40)})
	c.Block()
	create, table := createLookupTable(c.Latest(), authority, 1, alt)
	if res, _ := c.Block(create); res[0].Code != 0 {
		t.Fatalf("CreateLookupTable: code %d", res[0].Code)
	}
	if res, _ := c.Block(extendLookupTable(c.Latest(), authority, table, alt, [][32]byte{tc.Pub(hot)})); res[0].Code != 0 {
		t.Fatalf("ExtendLookupTable: code %d", res[0].Code)
	}
	limit := ffi.MaxWritableAccountUnits()

	through := func(index uint8, to [32]byte) []byte {
		payer := spender(39)
		return tc.TxV0(c.Latest(), payer, nil, []tc.Lookup{{Table: table, Writable: []tc.LookupEntry{{Index: index, Key: to}}}},
			tc.Transfer(payer, to, 1))
	}
	atTheShare := func() [][]byte {
		tx := through(0, tc.Pub(hot))
		a, err := c.Admit(tx)
		if err != nil {
			t.Fatal(err)
		}
		return append(share(t, c, 0, tc.Pub(hot), limit-a.Units), c.Wrap(tx))
	}
	runsWhole := func(when string) {
		t.Helper()
		block := atTheShare()
		if got := c.Propose(block...); got != abci.ResponseProcessProposal_ACCEPT {
			t.Fatalf("%s: the block at the share through the table: %v, want ACCEPT", when, got)
		}
		res, _ := c.Deliver(block...)
		for i, r := range res {
			if r.Code != 0 {
				t.Fatalf("%s: transaction %d of %d: code %d, want 0", when, i, len(res), r.Code)
			}
		}
	}
	runsWhole("extended in the block before")

	extend := extendLookupTable(c.Latest(), authority, table, alt, [][32]byte{tc.Pub(late)})
	res, _ := c.Block(extend, through(1, tc.Pub(late)))
	if res[0].Code != 0 || res[1].Code != 2 {
		t.Fatalf("extending and using an address in one block: codes %d and %d, want 0 and 2", res[0].Code, res[1].Code)
	}

	if res, _ := c.Block(altSimple(c.Latest(), authority, table, alt, 3)); res[0].Code != 0 {
		t.Fatalf("DeactivateLookupTable: code %d", res[0].Code)
	}
	runsWhole("deactivated in the block before")
}
