package app_test

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestSimulateDuringAnOpenBlock proves that Simulate uses no block handle. It
// returns while a block is open, and the block's handle still executes.
func TestSimulateDuringAnOpenBlock(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	ms := c.App.CommitMultiStore()
	height := uint64(c.Height)
	hash := c.Latest()

	blockTx := tc.Tx(hash, faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), 1_000_000_000))
	plan := ffi.NewArenaWriter(ffi.KindPlanReq, 0)
	plan.PushSized(blockTx)
	handle, planBuf, err := ffi.Plan(height+1, tc.HashAt(c.Height+1), plan.Finish())
	if err != nil {
		t.Fatal(err)
	}
	defer ffi.Release(handle)
	planBuf.Free()

	simTx := tc.Tx(hash, faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(3)), 1_000_000_000))
	keys, _, err := ffi.TransactionKeys(simTx)
	if err != nil {
		t.Fatal(err)
	}
	branch, next, err := c.App.Next()
	if err != nil {
		t.Fatal(err)
	}
	req := ffi.NewArenaWriter(ffi.KindSimulateReq, 0)
	req.Push(simTx)
	c.App.Stores.PushAccounts(req, branch, keys, math.MaxInt64, nil)
	req.Push(nil)
	sim, err := ffi.Simulate(next.Height, next.Hash, svm.NextOwnHash, false, false, false, req)
	if err != nil {
		t.Fatalf("simulate with a block open: %v", err)
	}
	if sim.Status != 0 || sim.Units == 0 || sim.Fee != 5_000 || len(sim.Logs) == 0 {
		t.Fatalf("simulated transfer: status %d, %d units, fee %d, %d log lines", sim.Status, sim.Units, sim.Fee, len(sim.Logs))
	}

	// The open block's handle is unchanged. A stale or stolen handle returns
	// ErrBadHandle, and a second open block returns ErrBusy.
	keys, _, _ = ffi.TransactionKeys(blockTx)
	exec := ffi.NewArenaWriter(ffi.KindExecuteReq, handle)
	exec.Push([]byte{0})
	c.App.Stores.PushAccounts(exec, ms, keys, svm.ArenaBytes, nil)
	exec.Push(nil)
	buf, err := ffi.Execute(handle, exec)
	if errors.Is(err, ffi.ErrBadHandle) || errors.Is(err, ffi.ErrBusy) {
		t.Fatalf("simulate disturbed the open block: %v", err)
	}
	if err != nil {
		t.Fatalf("execute after simulate: %v", err)
	}
	buf.Free()
}

// TestASimulationNaming129AccountsIsRefused proves that a simulation that
// names 129 accounts is not processed, with TooManyAccountLocks, as Agave's
// simulation batch reports it. Such a transaction is over the 1,232-byte
// packet, so the RPC refuses it at decode, and only a direct call reaches this
// check.
func TestASimulationNaming129AccountsIsRefused(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	ms, next, err := c.App.Next()
	if err != nil {
		t.Fatal(err)
	}

	accounts := []tc.Meta{tc.Signer(tc.Pub(faucet), true)}
	for i := 0; len(accounts) < 128; i++ {
		accounts = append(accounts, tc.Readonly(namePub("lock", i)))
	}
	// The payer, 127 readonly names and the System program: 129 keys.
	tx := tc.Tx(c.Latest(), faucet, nil, tc.Instruction{Program: svm.SystemProgram, Accounts: accounts,
		Data: binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 2), 1)})
	keys, _, err := ffi.TransactionKeys(tx)
	if err != nil {
		t.Fatal(err)
	}
	req := ffi.NewArenaWriter(ffi.KindSimulateReq, 0)
	req.Push(tx)
	c.App.Stores.PushAccounts(req, ms, keys, math.MaxInt64, nil)
	req.Push(nil)
	sim, err := ffi.Simulate(next.Height, next.Hash, svm.NextOwnHash, false, false, false, req)
	if err != nil {
		t.Fatal(err)
	}
	if sim.Status != 2 || string(sim.Err) != `"TooManyAccountLocks"` {
		t.Fatalf("simulating 129 keys: status %d, err %s, want not processed with TooManyAccountLocks", sim.Status, sim.Err)
	}
}
