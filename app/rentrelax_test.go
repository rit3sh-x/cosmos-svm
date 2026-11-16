package app_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// rent1322 is SIMD-0437-4, a rent cut that mainnet has not activated, so a
// test can schedule it.
const rent1322 = "rntD7invRBswCAdKtRsh1G4psKjrPdS3BKqtnA78C7N"

// TestAStrandedAccountCanBeCreditedBackToExemption proves that an account
// stranded below its rent minimum can be credited back to exemption. Without
// SIMD-0392, transition_allowed lets a RentPaying account keep or lose lamports
// but never gain them, so a rent raise would brick every account it strands.
// That is why the gate is on at genesis, not left to governance.
func TestAStrandedAccountCanBeCreditedBackToExemption(t *testing.T) {
	faucet := tc.Key(1)
	stranded := tc.Pub(tc.Key(7))
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()

	const reset = 9
	if got := mustPerByte(t, c); got != 5_080 {
		t.Fatalf("lamports per byte is %d at genesis, want 5,080", got)
	}

	// Strand the account as a real one is stranded. Fund it at the genesis
	// price, 5,080, then let the safeguard reset the price to 6,960. The
	// funding is exempt at 5,080 and short of the 890,880 that 6,960 demands.
	cheap := uint64(128 * 5_080)
	afterReset := uint64(128 * 6_960)
	res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, stranded, cheap)))
	if res[0].Code != 0 {
		t.Fatalf("funding at the cheaper minimum failed with code %d: %s", res[0].Code, res[0].Log)
	}

	if err := schedule(t, c, reset-svm.MinActivationDelay, reset, rent6960); err != nil {
		t.Fatal(err)
	}
	for c.Height < reset {
		c.Block()
	}
	if got := mustPerByte(t, c); got != 6_960 {
		t.Fatalf("lamports per byte is %d after the safeguard, want 6,960", got)
	}
	if got, want := c.Lamports(stranded), afterReset; got >= want {
		t.Fatalf("the account holds %d against a minimum of %d, so it is not stranded", got, want)
	}

	// The relaxation allows this credit, a part payment that leaves the
	// account still short. Without SIMD-0392, transition_allowed refuses it
	// as RentPaying to RentPaying with more lamports. The only way back is then
	// to clear the whole shortfall in one transaction. A third party cannot do
	// that for an account whose funding it does not control.
	short := afterReset - cheap
	res, _ = c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, stranded, short/2)))
	if res[0].Code != 0 {
		t.Fatalf("a part payment to a stranded account failed with code %d: %s (SIMD-0392 is off?)",
			res[0].Code, res[0].Log)
	}
	if got, want := c.Lamports(stranded), afterReset; got >= want {
		t.Fatalf("the part payment reached %d, which is not below the minimum %d, so it proves nothing", got, want)
	}

	// Then the rest of the way back to exemption.
	res, _ = c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, stranded, short-short/2)))
	if res[0].Code != 0 {
		t.Fatalf("the final credit failed with code %d: %s", res[0].Code, res[0].Log)
	}
	if got, want := c.Lamports(stranded), afterReset; got != want {
		t.Fatalf("the account holds %d after the credit, want its rent minimum %d", got, want)
	}
}

// TestTheFeesSysvarIsFrozenAtMainnetsValue proves that the Fees sysvar is a
// genesis row of eight FeeCalculator bytes with mainnet's value, and that it
// never changes. Without this row, getAccountInfo returns null, which a client
// cannot interpret.
func TestTheFeesSysvarIsFrozenAtMainnetsValue(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()

	first, ok := c.Account(svm.FeesID)
	if !ok {
		t.Fatal("no Fees sysvar account")
	}
	if len(first.Data) != 8 {
		t.Fatalf("Fees carries %d bytes, want 8", len(first.Data))
	}
	if got := binary.LittleEndian.Uint64(first.Data); got != 5_000 {
		t.Fatalf("lamports_per_signature is %d, want mainnet's 5,000", got)
	}
	if first.Owner != svm.SysvarOwner || first.Lamports != svm.MinimumBalance(8) {
		t.Fatalf("Fees is owned by %x with %d lamports", first.Owner, first.Lamports)
	}

	// After ten blocks with real transactions, the account is unchanged. The
	// row costs its rent deposit once and nothing per block.
	faucet := tc.Key(1)
	for i := 0; i < 10; i++ {
		c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(8)), svm.MinimumBalance(0))))
	}
	after, _ := c.Account(svm.FeesID)
	if !bytes.Equal(after.Data, first.Data) || after.Lamports != first.Lamports ||
		after.Owner != first.Owner || after.RentEpoch != first.RentEpoch {
		t.Fatalf("the Fees row changed: %+v then %+v", first, after)
	}
}
