package app_test

import (
	"crypto/ed25519"
	"encoding/binary"
	"runtime"
	"strings"
	"testing"

	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

func liveHeap() int64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

// TestTheExecuteRequestHoldsNoCopyOfTheAccounts proves that the execute
// request points at the values that the block's cache store already holds.
// Around ffi.Execute, the Go heap holds a 10 MiB account only once, in that
// cache. The writer body and the Finish output hold no copy of it.
func TestTheExecuteRequestHoldsNoCopyOfTheAccounts(t *testing.T) {
	const size = 10 << 20
	faucet, big := tc.Key(1), tc.Pub(tc.Key(2))
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Accounts: []svm.GenesisAccount{{
		Pubkey:    base58.Encode(big[:]),
		Lamports:  svm.MinimumBalance(size),
		Owner:     base58.Encode(svm.SystemProgram[:]),
		Data:      make([]byte, size),
		RentEpoch: ^uint64(0),
	}}})
	c.Block()
	c.Block()

	// The committed sysvars name the parent of the latest block, because the
	// latest block's own hash joins them when the next block begins.
	tx := tc.Tx(tc.HashAt(c.Height-1), faucet, nil, tc.Transfer(faucet, big, 1))
	plan := ffi.NewArenaWriter(ffi.KindPlanReq, 0)
	plan.PushSized(tx)
	handle, planBuf, err := ffi.Plan(uint64(c.Height+1), tc.HashAt(c.Height+1), plan.Finish())
	if err != nil {
		t.Fatal(err)
	}
	defer ffi.Release(handle)
	planBuf.Free()
	keys, _, err := ffi.TransactionKeys(tx)
	if err != nil {
		t.Fatal(err)
	}

	ms := c.App.CommitMultiStore().CacheMultiStore()
	if v := c.App.Stores.GetAccount(ms, big); len(v) < size {
		t.Fatalf("the block's cache holds %d bytes of the account, want %d", len(v), size)
	}
	exec := ffi.NewArenaWriter(ffi.KindExecuteReq, handle)
	exec.Push([]byte{0})
	held := liveHeap()
	c.App.Stores.PushAccounts(exec, ms, keys, svm.ArenaBytes, nil)
	exec.Push(nil)
	atCall := liveHeap()
	buf, err := ffi.Execute(handle, exec)
	if err != nil {
		t.Fatal(err)
	}
	afterCall := liveHeap()
	r, err := ffi.NewArenaReader(buf.Bytes(), ffi.KindExecuteResp)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := r.ResultAt(0); err != nil || res.Status != 0 {
		t.Fatalf("the transfer to the large account: %+v, %v", res, err)
	}
	buf.Free()
	runtime.KeepAlive(ms)

	t.Logf("live Go heap past the cache's copy: %d bytes at the call, %d after it, for a %d-byte account",
		atCall-held, afterCall-held, size)
	if atCall-held > size/4 || afterCall-held > size/4 {
		t.Fatalf("the request added %d bytes at the call and %d after it, want well under one %d-byte copy",
			atCall-held, afterCall-held, size)
	}
}

// cost returns the cost of an account against the arena ceiling: 64 bytes plus
// its data, or 0 if it does not exist.
func cost(c *tc.Chain, k [32]byte) int64 {
	a, ok := c.Account(k)
	if !ok {
		return 0
	}
	return 64 + int64(len(a.Data))
}

// TestATransactionThatLosesAnAccountPaysItsFee proves that a transaction that
// loses an account to a cut load list fails as fees only and pays its fee. A
// transaction whose fee payer did not load is not processed.
func TestATransactionThatLosesAnAccountPaysItsFee(t *testing.T) {
	faucet, payer := tc.Key(1), tc.Key(2)
	fresh, dest, memo4 := tc.Pub(tc.Key(3)), tc.Pub(tc.Key(4)), key(t, memoV4)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Programs: tc.ManifestPrograms(t), Accounts: []svm.GenesisAccount{funded(payer)}})
	c.Block()
	c.Block()
	program, _ := c.Account(memo4)
	programData := [32]byte(program.Data[4:36])

	// The load list is the sysvars, the two fee payers, then fresh and the
	// System program, memo-v4 with its ProgramData, and dest.
	hash := tc.HashAt(c.Height - 1)
	txs := [][]byte{
		tc.Tx(hash, faucet, nil, tc.Transfer(faucet, fresh, 1_000_000_000)),
		tc.Tx(hash, faucet, nil, memo(memo4, "after the cut")),
		tc.Tx(hash, payer, nil, tc.Transfer(payer, dest, 1_000_000)),
	}
	run := func(budget int64) (dropped [][32]byte, results []ffi.TxResult, errs []string) {
		t.Helper()
		plan := ffi.NewArenaWriter(ffi.KindPlanReq, 0)
		for _, tx := range txs {
			plan.PushSized(tx)
		}
		handle, planBuf, err := ffi.Plan(uint64(c.Height+1), tc.HashAt(c.Height+1), plan.Finish())
		if err != nil {
			t.Fatal(err)
		}
		defer ffi.Release(handle)
		defer planBuf.Free()
		pr, err := ffi.NewArenaReader(planBuf.Bytes(), ffi.KindPlanResp)
		if err != nil {
			t.Fatal(err)
		}
		var list [][32]byte
		for i := int(pr.Aux); i < pr.Len()-len(txs); i++ {
			k, _ := pr.Key32(i)
			list = append(list, k)
		}
		exec := ffi.NewArenaWriter(ffi.KindExecuteReq, handle)
		exec.Push(make([]byte, len(txs)))
		dropped, used := c.App.Stores.PushAccounts(exec, c.App.CommitMultiStore().CacheMultiStore(), list, budget, nil)
		if used > budget {
			t.Fatalf("loaded %d bytes against a budget of %d", used, budget)
		}
		var cut []byte
		for _, k := range dropped {
			cut = append(cut, k[:]...)
		}
		exec.Push(cut)
		// Execute succeeds, so every sysvar loaded, whatever the budget.
		buf, err := ffi.Execute(handle, exec)
		if err != nil {
			t.Fatal(err)
		}
		defer buf.Free()
		out, err := ffi.NewArenaReader(buf.Bytes(), ffi.KindExecuteResp)
		if err != nil {
			t.Fatal(err)
		}
		for i := range txs {
			res, err := out.ResultAt(i)
			if err != nil {
				t.Fatal(err)
			}
			meta, err := out.MetaAt(len(txs) + i)
			if err != nil {
				t.Fatal(err)
			}
			results, errs = append(results, res), append(errs, string(meta.Err))
		}
		return dropped, results, errs
	}
	fees := func(results []ffi.TxResult) (out [3]string) {
		for i, r := range results {
			out[i] = []string{"ran", "fees only", "not processed"}[r.Status]
			if r.Status != 2 && r.Fee != 5_000 {
				out[i] += ", unpaid"
			}
		}
		return out
	}
	const lost = "MaxLoadedAccountsDataSizeExceeded"

	// Room for everything up to memo-v4, and not for its ProgramData.
	budget := cost(c, tc.Pub(faucet)) + cost(c, tc.Pub(payer)) + cost(c, svm.SystemProgram) + cost(c, memo4)
	dropped, results, errs := run(budget)
	if want := [][32]byte{programData, memo4, dest}; len(dropped) != 3 || dropped[0] != want[0] || dropped[1] != want[1] || dropped[2] != want[2] {
		t.Fatalf("dropped %x, want memo-v4's ProgramData, memo-v4 and dest", dropped)
	}
	if got := fees(results); got != [3]string{"ran", "fees only", "fees only"} {
		t.Fatalf("with memo-v4's ProgramData cut: %q", got)
	}
	if !strings.Contains(errs[1], lost) || !strings.Contains(errs[2], lost) {
		t.Fatalf("the two that lost an account failed with %s and %s, want %s", errs[1], errs[2], lost)
	}

	// Room for the faucet only. payer is a fee payer that did not load, so
	// its transaction cannot be charged. The other two lost fresh and memo-v4.
	_, results, _ = run(cost(c, tc.Pub(faucet)))
	if got := fees(results); got != [3]string{"fees only", "fees only", "not processed"} {
		t.Fatalf("with payer cut: %q", got)
	}
}

// createBig returns a System program CreateAccount for a 10 MiB account.
func createBig(from, to ed25519.PrivateKey) tc.Instruction {
	const size = 10 << 20
	data := binary.LittleEndian.AppendUint32(nil, 0)
	data = binary.LittleEndian.AppendUint64(data, svm.MinimumBalance(size))
	data = binary.LittleEndian.AppendUint64(data, size)
	data = append(data, svm.SystemProgram[:]...)
	return systemIx([]tc.Meta{tc.Signer(tc.Pub(from), true), tc.Signer(tc.Pub(to), true)}, data)
}

// TestTheArenaCeilingHoldsAgainstTheAttackShape fills a block with transactions
// that each name 126 accounts through a table, at a 32 KiB declared data limit.
// 52 of the accounts hold 10 MiB, which is more than the 512 MiB ceiling. The
// block loads at most the ceiling, and every transaction pays its fee.
func TestTheArenaCeilingHoldsAgainstTheAttackShape(t *testing.T) {
	if testing.Short() {
		t.Skip("loads 512 MiB of accounts")
	}
	svm.RegisterMetrics(nil)
	const big = 52
	faucet := tc.Key(1)
	table := namePub("arena", 0)
	addresses := make([][32]byte, 126)
	for i := range addresses {
		addresses[i] = namePub("big", i)
	}
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Programs: tc.ManifestPrograms(t),
		Accounts: append(spenders(4), lookupTableRow(table, addresses))})
	c.Block()

	// Each transaction creates two 10 MiB accounts, the most that one
	// transaction may allocate. Each block holds two such transactions. The
	// faucet pays for both, and a third would take it past its block share.
	for i := 0; i < big; i += 4 {
		var txs [][]byte
		for j := i; j < i+4 && j < big; j += 2 {
			txs = append(txs, tc.Tx(c.Latest(), faucet, []ed25519.PrivateKey{nameKey("big", j), nameKey("big", j+1)},
				createBig(faucet, nameKey("big", j)), createBig(faucet, nameKey("big", j+1))))
		}
		res, _ := c.Block(txs...)
		for k, r := range res {
			if r.Code != 0 {
				t.Fatalf("creating the 10 MiB accounts: transaction %d failed with %d", k, r.Code)
			}
		}
	}

	// Four payers share the attack. One payer for a whole block would go past
	// its block share about two fifths of the way in.
	attackers := []ed25519.PrivateKey{spender(0), spender(1), spender(2), spender(3)}
	attack := func(i int) []byte {
		entries := make([]tc.LookupEntry, len(addresses))
		for j := range entries {
			entries[j] = tc.LookupEntry{Index: uint8(j), Key: addresses[j]}
		}
		return tc.TxV0(c.Latest(), attackers[i%len(attackers)], nil, []tc.Lookup{{Table: table, Readonly: entries}},
			computeUnitLimit(t, uint32(1_000+i)), loadedAccountsDataSizeLimit(t, 32*1024))
	}
	// Two honest transactions go after them: a transfer to an account first
	// named after the cut, and a call to a program first named after the cut.
	fresh := tc.Pub(tc.Key(5))
	honest := [][]byte{
		tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, fresh, 1_000_000_000)),
		tc.Tx(c.Latest(), faucet, nil, memo(key(t, memoV4), "behind the attack")),
	}
	units := func(tx []byte) uint64 {
		a, err := c.Admit(tx)
		if err != nil {
			t.Fatal(err)
		}
		return a.Units
	}
	left, _, _ := ffi.BlockLimits()
	for _, tx := range honest {
		left -= units(tx)
	}
	var txs [][]byte
	for i := 0; ; i++ {
		tx := attack(i)
		if units(tx) > left {
			break
		}
		left -= units(tx)
		txs = append(txs, tx)
	}
	txs = append(txs, honest...)

	held := func() (sum uint64) {
		for _, k := range append(attackers, faucet) {
			sum += c.Lamports(tc.Pub(k))
		}
		return sum
	}
	before := held()
	droppedBefore := value(t, scrape(t)["svm_arena_dropped_total"])
	res, _ := c.Block(txs...)
	for i, r := range res {
		if r.Code != 1 {
			t.Fatalf("transaction %d of %d has code %d, want 1, fees only", i, len(res), r.Code)
		}
	}
	if paid := before - held(); paid != 5_000*uint64(len(txs)) {
		t.Fatalf("the payers paid %d for %d transactions, want 5,000 each", paid, len(txs))
	}
	if _, ok := c.Account(fresh); ok {
		t.Fatal("the transfer behind the attack ran")
	}
	metrics := scrape(t)
	arena := value(t, metrics["svm_arena_bytes"])
	if arena > svm.ArenaBytes || arena <= svm.ArenaBytes-(64+10<<20) {
		t.Fatalf("the block loaded %.0f bytes, want the most of %d it can hold", arena, svm.ArenaBytes)
	}
	if value(t, metrics["svm_arena_dropped_total"]) <= droppedBefore {
		t.Fatal("svm_arena_dropped_total did not rise")
	}
	t.Logf("%d transactions, %.0f bytes loaded, %.0f keys dropped", len(txs), arena,
		value(t, metrics["svm_arena_dropped_total"])-droppedBefore)
}
