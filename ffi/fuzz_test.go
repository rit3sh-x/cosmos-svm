package ffi_test

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// FuzzUntrustedTransactionBytes proves that every entry point that parses bytes
// from a client answers or refuses, and never panics, whatever the bytes are.
func FuzzUntrustedTransactionBytes(f *testing.F) {
	v1, _ := base58.Decode("txv1aq4pp281K9um3tnPgkfX8UqtFT6wcVW3hNezGLL")
	gates := []ffi.Gate{{ID: [32]byte(v1)}}
	if err := ffi.Configure(gates); err != nil {
		f.Fatal(err)
	}
	// Admission reads the gates of the next block, at the height that the state
	// half below names.
	if err := ffi.ConfigureNext(5, gates); err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = ffi.Configure(nil) })
	// Rent as bincode writes it: lamports per byte-year, the exemption
	// threshold, and the burn percent.
	rent := binary.LittleEndian.AppendUint64(nil, 3480)
	rent = binary.LittleEndian.AppendUint64(rent, math.Float64bits(2))
	rent = append(rent, 50)
	payer := tc.Key(1)
	blockhash := [32]byte{7}
	f.Add(tc.Tx(blockhash, payer, nil, tc.Transfer(payer, tc.Pub(tc.Key(2)), 5)))
	f.Add(tc.TxV0(blockhash, payer, nil,
		[]tc.Lookup{{Table: tc.Pub(tc.Key(3)), Writable: []tc.LookupEntry{{Index: 1, Key: tc.Pub(tc.Key(2))}}}},
		tc.Transfer(payer, tc.Pub(tc.Key(2)), 5)))
	f.Add(tc.TxV1(blockhash, tc.V1Config{ComputeUnitLimit: 1000, LoadedAccountsDataSize: 1 << 16}, payer, nil,
		tc.Transfer(payer, tc.Pub(tc.Key(2)), 5)))
	f.Fuzz(func(t *testing.T, b []byte) {
		panicked := func(name string, err error) {
			if errors.Is(err, ffi.ErrEnginePanic) {
				t.Fatalf("%s panicked", name)
			}
		}
		for _, hashOnly := range []bool{true, false} {
			a, err := ffi.CheckTx(b, hashOnly, 5)
			panicked("check_tx", err)
			if err == nil {
				panicked("check_committed", ffi.CheckCommitted(b, a, ffi.Committed{
					Rent: rent, Payer: b, Expired: true, Nonce: b,
					Height: 5, SlotHashes: b, Tables: map[[32]byte][]byte{{3}: b},
				}))
			}
		}
		_, _, err := ffi.TransactionKeys(b)
		panicked("transaction_keys", err)
		_, err = ffi.LookupTables(b)
		panicked("lookup_tables", err)
		_, _, _, err = ffi.LookupAddresses(5, b, b, map[[32]byte][]byte{{1}: b})
		panicked("lookup_addresses", err)
		if len(b) > 64 {
			_, _, err := ffi.FeeForMessage(b[64:])
			panicked("fee_for_message", err)
		}
		w := ffi.NewArenaWriter(ffi.KindPlanReq, 0)
		w.PushSized(b)
		h, plan, err := ffi.Plan(5, [32]byte{1}, w.Finish())
		panicked("plan", err)
		if err == nil {
			plan.Free()
			ffi.Release(h)
		}
	})
}
