package app_test

import (
	"crypto/ed25519"
	"strings"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// spender returns the funded key used to pay for a test transaction.
func spender(i int) ed25519.PrivateKey { return nameKey("spender", i) }

// spenders returns n funded accounts for independent transaction payers.
func spenders(n int) []svm.GenesisAccount {
	out := make([]svm.GenesisAccount, n)
	for i := range out {
		out[i] = funded(spender(i))
	}
	return out
}

// toward builds a transaction that transfers one lamport to hot and declares
// the requested compute-unit limit.
func toward(t *testing.T, c *tc.Chain, i int, hot [32]byte, units uint32, extra ...tc.Instruction) []byte {
	ixs := append([]tc.Instruction{tc.Transfer(spender(i), hot, 1), computeUnitLimit(t, units)}, extra...)
	return tc.Tx(c.Latest(), spender(i), nil, ixs...)
}

// share builds transactions whose admitted costs add up to total units. Each
// transaction declares at most 1,400,000 compute units.
func share(t *testing.T, c *tc.Chain, from int, hot [32]byte, total uint64, extra ...tc.Instruction) [][]byte {
	t.Helper()
	const most = 1_400_000
	probe, err := c.Admit(toward(t, c, from, hot, 0, extra...))
	if err != nil {
		t.Fatal(err)
	}
	fixed := probe.Units
	n := (total + fixed + most - 1) / (fixed + most)
	each, rest := (total-n*fixed)/n, (total-n*fixed)%n
	txs := make([][]byte, n)
	var sum uint64
	for i := range txs {
		declared := each
		if uint64(i) < rest {
			declared++
		}
		tx := toward(t, c, from+i, hot, uint32(declared), extra...)
		a, err := c.Admit(tx)
		if err != nil {
			t.Fatal(err)
		}
		sum += a.Units
		txs[i] = c.Wrap(tx)
	}
	if sum != total {
		t.Fatalf("the share costs %d units, want %d", sum, total)
	}
	return txs
}

// TestAWritableAccountSpendsAtMostItsShareOfABlock proves that each written
// account has its own unit share of a block. A block is refused when any
// account that it writes goes over its share.
func TestAWritableAccountSpendsAtMostItsShareOfABlock(t *testing.T) {
	hot, warm, fresh := nameKey("hot", 0), nameKey("hot", 1), tc.Pub(nameKey("hot", 2))
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: append(spenders(40), funded(hot), funded(warm))})
	c.Block()
	limit := ffi.MaxWritableAccountUnits()

	exact := share(t, c, 0, tc.Pub(hot), limit)
	if got := c.Propose(exact...); got != abci.ResponseProcessProposal_ACCEPT {
		t.Fatalf("%d transactions writing one account for exactly %d units: %v, want ACCEPT", len(exact), limit, got)
	}
	if got := c.Propose(share(t, c, 0, tc.Pub(hot), limit+1)...); got != abci.ResponseProcessProposal_REJECT {
		t.Fatalf("one unit over: %v, want REJECT", got)
	}
	both := append(share(t, c, 0, tc.Pub(hot), limit), share(t, c, 20, tc.Pub(warm), limit)...)
	if got := c.Propose(both...); got != abci.ResponseProcessProposal_ACCEPT {
		t.Fatalf("two accounts each at the limit: %v, want ACCEPT", got)
	}
	payer := spender(39)
	two := c.Wrap(tc.Tx(c.Latest(), payer, nil, tc.Transfer(payer, fresh, 1), tc.Transfer(payer, tc.Pub(hot), 1)))
	if got := c.Propose(append(exact, two)...); got != abci.ResponseProcessProposal_REJECT {
		t.Fatalf("a transaction writing a fresh account and the full one: %v, want REJECT", got)
	}
}

// TestPrepareSkipsAFullAccountAndKeepsPacking proves that the proposer skips a
// transaction that goes over an account's remaining share. It still packs
// lower-ranked transactions to other accounts.
func TestPrepareSkipsAFullAccountAndKeepsPacking(t *testing.T) {
	hot := nameKey("hot", 0)
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: append(spenders(40), funded(hot))})
	c.Block()
	limit := ffi.MaxWritableAccountUnits()

	crowd := share(t, c, 0, tc.Pub(hot), limit+limit/4, computeUnitPrice(t, 1_000_000))
	var quiet [][]byte
	for i := 30; i < 40; i++ {
		quiet = append(quiet, c.Wrap(tc.Tx(c.Latest(), spender(i), nil, tc.Transfer(spender(i), tc.Pub(tc.Key(byte(i))), svm.MinimumBalance(0)))))
	}
	if rankOf(t, c, crowd[len(crowd)-1]) <= rankOf(t, c, quiet[0]) {
		t.Fatal("the crowd does not outbid the quiet transactions, so this tests nothing")
	}

	inCrowd := make(map[string]bool, len(crowd))
	for _, raw := range crowd {
		inCrowd[string(c.Unwrap(raw))] = true
	}
	packed := c.Pack(svm.MaxBlockBytes, append(crowd, quiet...)...)
	var spent uint64
	var crowded, quieted int
	for _, raw := range packed {
		tx := c.Unwrap(raw)
		if !inCrowd[string(tx)] {
			quieted++
			continue
		}
		a, err := c.Admit(tx)
		if err != nil {
			t.Fatal(err)
		}
		spent += a.Units
		crowded++
	}
	if crowded == len(crowd) || spent > limit {
		t.Fatalf("packed %d of %d crowd transactions for %d units, past the %d share", crowded, len(crowd), spent, limit)
	}
	if quieted != len(quiet) {
		t.Fatalf("packed %d of %d quiet transactions behind the full account", quieted, len(quiet))
	}
	if got := c.Propose(packed...); got != abci.ResponseProcessProposal_ACCEPT {
		t.Fatalf("a proposal this machine packed: %v, want ACCEPT", got)
	}
}

// TestALookupTableWriteCountsAgainstTheAccount proves that a writable account
// from an address lookup table counts toward its per-account limit. A readonly
// account from a table does not.
func TestALookupTableWriteCountsAgainstTheAccount(t *testing.T) {
	hot, table := nameKey("hot", 0), namePub("table", 0)
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: append(spenders(40),
		funded(hot), lookupTableRow(table, [][32]byte{tc.Pub(hot)}))})
	c.Block()
	limit := ffi.MaxWritableAccountUnits()

	payer := spender(39)
	through := tc.TxV0(c.Latest(), payer, nil, []tc.Lookup{{Table: table, Writable: []tc.LookupEntry{{Index: 0, Key: tc.Pub(hot)}}}},
		tc.Transfer(payer, tc.Pub(hot), 1))
	a, err := c.Admit(through)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Writable) != 0 {
		t.Fatalf("admission named %d writable accounts for a message that names a table", len(a.Writable))
	}
	if got := c.Propose(append(share(t, c, 0, tc.Pub(hot), limit-a.Units), c.Wrap(through))...); got != abci.ResponseProcessProposal_ACCEPT {
		t.Fatalf("the share filled exactly through a table: %v, want ACCEPT", got)
	}
	if got := c.Propose(append(share(t, c, 0, tc.Pub(hot), limit-a.Units+1), c.Wrap(through))...); got != abci.ResponseProcessProposal_REJECT {
		t.Fatalf("one unit over through a table: %v, want REJECT", got)
	}
	read := tc.Transfer(payer, tc.Pub(tc.Key(9)), svm.MinimumBalance(0))
	read.Accounts = append(read.Accounts, tc.Readonly(tc.Pub(hot)))
	reading := tc.TxV0(c.Latest(), payer, nil, []tc.Lookup{{Table: table, Readonly: []tc.LookupEntry{{Index: 0, Key: tc.Pub(hot)}}}}, read)
	if got := c.Propose(append(share(t, c, 0, tc.Pub(hot), limit), c.Wrap(reading))...); got != abci.ResponseProcessProposal_ACCEPT {
		t.Fatalf("a table supplying the full account readonly: %v, want ACCEPT", got)
	}
}

// TestCheckTxRefusesATransactionOverAnAccountsShare proves that CheckTx refuses
// a transaction whose cost is more than the block share of one account.
func TestCheckTxRefusesATransactionOverAnAccountsShare(t *testing.T) {
	payer := tc.Key(1)
	c := tc.Start(t, tc.Pub(payer), tc.Options{})
	c.Block()
	claim := tc.Instruction{Program: key(t, "KeccakSecp256k11111111111111111111111111111"), Data: []byte{255}}
	claims := make([]tc.Instruction, 15)
	for i := range claims {
		claims[i] = claim
	}
	res := c.Check(c.Wrap(tc.Tx(c.Latest(), payer, nil, claims...)), false)
	if res.Code != svm.ErrAdmission.ABCICode() || !strings.Contains(res.Log, "WouldExceedMaxAccountCostLimit") {
		t.Fatalf("code %d (%s), want %d naming WouldExceedMaxAccountCostLimit", res.Code, res.Log, svm.ErrAdmission.ABCICode())
	}
}
