package app_test

import (
	"crypto/ed25519"
	"encoding/binary"
	json "encoding/json/v2"
	"testing"

	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/genesis"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestNoSchedulableGateRaisesACost proves that no schedulable gate at this pin
// raises the cost of a transaction. If ProcessProposal(H) and the
// FinalizeBlock(H) plan ever cost a block under different gate sets, such a
// gate could pass a block that the plan refuses over budget and halt the chain.
// A crate bump that adds such a gate fails here, by name, before anyone can
// schedule it.
func TestNoSchedulableGateRaisesACost(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()
	base, err := c.App.Stores.ActiveGates(c.App.CommitMultiStore())
	if err != nil {
		t.Fatal(err)
	}
	// Admission costs transactions for the next block, under the gate set for
	// that block.
	next := uint64(c.Height + 1)
	defer func() {
		if err := ffi.ConfigureNext(next, base); err != nil {
			t.Fatal(err)
		}
	}()
	corpus := costCorpus(t, c)
	measure := func(gates []ffi.Gate) []*ffi.Admission {
		if err := ffi.ConfigureNext(next, gates); err != nil {
			t.Fatal(err)
		}
		out := make([]*ffi.Admission, len(corpus))
		for i, tx := range corpus {
			if a, err := ffi.CheckTx(tx, true, next); err == nil {
				out[i] = &a
			}
		}
		return out
	}
	before := measure(base)
	for i, a := range before {
		if a == nil {
			t.Fatalf("transaction %d of the corpus is refused without any gate, so it checks nothing", i)
		}
	}

	var pinned struct {
		Gates []struct {
			ID          string `json:"id"`
			ActiveAtPin bool   `json:"active_at_pin"`
		} `json:"gates"`
	}
	if err := json.Unmarshal(genesis.Features, &pinned); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, g := range pinned.Gates {
		if g.ActiveAtPin || !svm.ValidTarget(g.ID) {
			continue
		}
		id, err := base58.Decode(g.ID)
		if err != nil {
			t.Fatal(err)
		}
		after := measure(append(append([]ffi.Gate(nil), base...), ffi.Gate{ID: [32]byte(id), Height: uint64(c.Height + 1)}))
		for i := range corpus {
			if after[i] == nil {
				continue
			}
			if after[i].Units > before[i].Units || after[i].AllocatedBytes > before[i].AllocatedBytes {
				t.Errorf("gate %s raises transaction %d from %d units and %d bytes to %d and %d",
					g.ID, i, before[i].Units, before[i].AllocatedBytes, after[i].Units, after[i].AllocatedBytes)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no schedulable gate was checked")
	}
	t.Logf("%d schedulable gates over %d transaction shapes", checked, len(corpus))
}

// costCorpus returns one transaction for each thing that the cost model charges:
// signatures of every kind, write locks, instruction data, allocation, the
// compute budget declared and defaulted for builtin and other programs,
// loaded accounts data, a heap frame, a deploy, and names through a table.
func costCorpus(t *testing.T, c *tc.Chain) [][]byte {
	payer := tc.Key(1)
	blockhash := c.Latest()
	computeBudget := key(t, "ComputeBudget111111111111111111111111111111")
	budget := func(tag byte, value uint32) tc.Instruction {
		return tc.Instruction{Program: computeBudget, Data: binary.LittleEndian.AppendUint32([]byte{tag}, value)}
	}
	precompile := func(program string, signatures byte) tc.Instruction {
		return tc.Instruction{Program: key(t, program), Data: append([]byte{signatures}, make([]byte, 15)...)}
	}
	dest := tc.Pub(tc.Key(2))
	created := tc.Key(3)
	program := tc.Pub(tc.Key(4))
	programData := svm.ProgramDataAddress(program)
	return [][]byte{
		tc.Tx(blockhash, payer, nil, tc.Transfer(payer, dest, 1)),
		tc.Tx(blockhash, payer, nil, tc.Transfer(payer, dest, 2), computeUnitLimit(t, 300_000), computeUnitPrice(t, 1_000)),
		tc.Tx(blockhash, payer, nil, memo(key(t, memoV4), "an ordinary program, budgeted by default")),
		tc.Tx(blockhash, payer, []ed25519.PrivateKey{created}, createAccount(payer, created, 10_000, svm.SystemProgram)),
		tc.Tx(blockhash, payer, nil, tc.Transfer(payer, dest, 3), budget(4, 64*1024)),
		tc.Tx(blockhash, payer, nil, memo(key(t, memoV4), "with a heap frame"), budget(1, 64*1024)),
		tc.TxV0(blockhash, payer, nil,
			[]tc.Lookup{{Table: tc.Pub(tc.Key(5)), Writable: []tc.LookupEntry{{Index: 0, Key: dest}}}},
			tc.Transfer(payer, dest, 4)),
		tc.Tx(blockhash, payer, nil, loaderIx([]tc.Meta{tc.Writable(programData), tc.Writable(program), tc.Writable(tc.Pub(tc.Key(6))),
			tc.Writable(tc.Pub(payer)), tc.Readonly(svm.RentID), tc.Readonly(svm.ClockID), tc.Signer(tc.Pub(payer), false)},
			binary.LittleEndian.AppendUint32(nil, 3))),
		tc.Tx(blockhash, payer, nil, precompile("Ed25519SigVerify111111111111111111111111111", 2), tc.Transfer(payer, dest, 5)),
		tc.Tx(blockhash, payer, nil, precompile("KeccakSecp256k11111111111111111111111111111", 2), tc.Transfer(payer, dest, 6)),
		tc.Tx(blockhash, payer, nil, precompile("Secp256r1SigVerify1111111111111111111111111", 2), tc.Transfer(payer, dest, 7)),
	}
}
