package app_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"strings"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"

	"github.com/cosmos/cosmos-sdk/baseapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	govv1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	"github.com/rit3sh-x/cosmos-svm/app"
	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// computeUnitPrice is ComputeBudgetInstruction::SetComputeUnitPrice, in
// microlamports per unit.
func computeUnitPrice(t *testing.T, microlamports uint64) tc.Instruction {
	return tc.Instruction{
		Program: key(t, "ComputeBudget111111111111111111111111111111"),
		Data:    binary.LittleEndian.AppendUint64([]byte{3}, microlamports),
	}
}

// bidder builds a transfer that declares 1,400,000 units and bids for them.
// About 42 of them fill a block, so the bid decides which go in.
func bidder(t *testing.T, c *tc.Chain, payer ed25519.PrivateKey, seed byte, microlamports uint64) []byte {
	return tc.Tx(c.Latest(), payer, nil,
		tc.Transfer(payer, tc.Pub(tc.Key(seed)), 1),
		computeUnitLimit(t, 1_400_000),
		computeUnitPrice(t, microlamports),
	)
}

// TestTheHighestBiddersBuyTheBlock proves that a bid placed in a transaction
// reaches the proposer's sort. The bids arrive in ascending order, so a
// proposer that kept arrival order would fail it. The engine's own test of the
// ranking key is in `engine/src/priority.rs`.
func TestTheHighestBiddersBuyTheBlock(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: spenders(128)})
	c.Block()

	// Three blocks' worth of candidates, bidding 0, 1_000, 2_000 and so on.
	const offered = 128
	txs := make([][]byte, 0, offered)
	for seed := 0; seed < offered; seed++ {
		txs = append(txs, c.Wrap(bidder(t, c, spender(seed), byte(seed+2), uint64(seed)*1_000)))
	}
	packed := c.Pack(svm.MaxBlockBytes, txs...)
	if len(packed) == 0 || len(packed) >= len(txs) {
		t.Fatalf("packed %d of %d candidates", len(packed), len(txs))
	}
	// Each packed transaction outranks each one left out. The test compares
	// ranks, not bids, because the key is a price per unit and these
	// transactions differ in cost as well as in bid.
	lowestIn := ^uint64(0)
	in := map[string]bool{}
	for _, raw := range packed {
		in[string(raw)] = true
		if r := rankOf(t, c, raw); r < lowestIn {
			lowestIn = r
		}
	}
	for i, raw := range txs {
		if in[string(raw)] {
			continue
		}
		if r := rankOf(t, c, raw); r > lowestIn {
			t.Fatalf("candidate %d ranks %d and was left out, below an included %d", i, r, lowestIn)
		}
	}
	// The top bidder is in, and the transaction that bid nothing is not.
	if !in[string(txs[len(txs)-1])] {
		t.Fatal("the highest bidder did not buy a place")
	}
	if in[string(txs[0])] {
		t.Fatal("the candidate that bid nothing took a place from a bidder")
	}
}

// TestACosmosTransactionSortsFirst proves that a Cosmos transaction sorts
// before an SVM transaction. Lamports are not a bank denom, so the two lanes
// have no exchange rate, and the chain does not invent one. Only an operator
// can pay the usvm fee floor of a Cosmos transaction (see
// TestOnlyAnOperatorPaysTheCosmosLane).
func TestACosmosTransactionSortsFirst(t *testing.T) {
	payer := tc.Key(1)
	operator := tc.OperatorKey(0)
	c := tc.Start(t, tc.Pub(payer), tc.Options{Operators: tc.DefaultOperators(1)})
	c.Block()

	send := &banktypes.MsgSend{
		FromAddress: tc.Address(operator).String(),
		ToAddress:   tc.Address(tc.OperatorKey(1)).String(),
		Amount:      nil,
	}
	cosmos := c.Cosmos(operator, send)
	// The SVM transaction bids high and still sorts second.
	svmTx := c.Wrap(bidder(t, c, payer, 9, 1_000_000))
	packed := c.Pack(svm.MaxBlockBytes, svmTx, cosmos)
	if len(packed) != 2 {
		t.Fatalf("packed %d of 2", len(packed))
	}
	if string(packed[0]) != string(cosmos) {
		t.Fatal("the SVM transaction outbid the operator lane")
	}
}

// TestOnlyAnOperatorPaysTheCosmosLane proves that only an operator can pay the
// fee floor of the Cosmos lane. An ICS-20 voucher creates the account it lands
// in, so with a zero floor an outsider could take the lane that outranks every
// SVM transaction for free. The floor is in usvm, which only operators hold,
// and they pay it from their free balance.
func TestOnlyAnOperatorPaysTheCosmosLane(t *testing.T) {
	payer := tc.Key(1)
	outsider := tc.OperatorKey(9)
	voucher := sdk.NewInt64Coin("ibc/27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2", 1_000_000)
	c := tc.Start(t, tc.Pub(payer), tc.Options{
		Operators: tc.DefaultOperators(4),
		Outsiders: []sdk.AccAddress{tc.Address(outsider)},
		BaseApp:   []func(*baseapp.BaseApp){baseapp.SetMinGasPrices(app.MinGasPrices)},
		Bank: func(g *banktypes.GenesisState) {
			g.Balances = append(g.Balances, banktypes.Balance{
				Address: tc.Address(outsider).String(), Coins: sdk.NewCoins(voucher)})
			g.Supply = g.Supply.Add(voucher)
		},
	})
	c.Block()

	send := &banktypes.MsgSend{
		FromAddress: tc.Address(outsider).String(),
		ToAddress:   tc.Address(tc.OperatorKey(0)).String(),
		Amount:      sdk.NewCoins(sdk.NewInt64Coin(voucher.Denom, 1)),
	}
	for _, fee := range []sdk.Coins{nil, sdk.NewCoins(voucher)} {
		res := c.Check(c.CosmosFee(outsider, fee, send), false)
		if res.Codespace != sdkerrors.ErrInsufficientFee.Codespace() || res.Code != sdkerrors.ErrInsufficientFee.ABCICode() {
			t.Fatalf("an outsider offering %q: code %d in %q, %s", fee, res.Code, res.Codespace, res.Log)
		}
	}
	// The floor applies only to the Cosmos lane. An SVM transaction pays
	// lamports and never reaches the ante handler.
	if res := c.Check(c.Wrap(tc.Tx(c.Latest(), payer, nil, tc.Transfer(payer, tc.Pub(tc.Key(2)), svm.MinimumBalance(0)))), false); res.Code != 0 {
		t.Fatalf("an SVM transfer under the floor: code %d, %s", res.Code, res.Log)
	}

	floor := sdk.NewCoins(sdk.NewInt64Coin(app.BondDenom, 1))
	deliver := func(signer ed25519.PrivateKey, msg sdk.Msg) {
		t.Helper()
		raw := c.CosmosFee(signer, floor, msg)
		if res := c.Check(raw, false); res.Code != 0 {
			t.Fatalf("%T at CheckTx: code %d, %s", msg, res.Code, res.Log)
		}
		if res, _ := c.Deliver(raw); res[0].Code != 0 {
			t.Fatalf("%T delivered: code %d, %s", msg, res[0].Code, res[0].Log)
		}
	}
	operator, voter := tc.OperatorKey(0), tc.OperatorKey(1)
	opening := balance(t, c, tc.Address(operator))
	deposit := app.GovParams().MinDeposit
	proposal, err := govv1.NewMsgSubmitProposal(nil, deposit, tc.Address(operator).String(),
		"a signalling proposal", "title", "summary", false)
	if err != nil {
		t.Fatal(err)
	}
	id := next(t, c)
	deliver(operator, proposal)
	deliver(voter, govv1.NewMsgVote(tc.Address(voter), id, govv1.OptionYes, ""))
	bond := sdk.NewInt64Coin(app.BondDenom, 1_000)
	deliver(operator, stakingtypes.NewMsgDelegate(tc.Address(operator).String(),
		sdk.ValAddress(tc.Address(operator)).String(), bond))

	spent := sdk.Coins(deposit).AmountOf(app.BondDenom).Add(bond.Amount).AddRaw(2)
	if got := opening.Sub(balance(t, c, tc.Address(operator))); !got.Equal(spent) {
		t.Fatalf("the operator spent %s usvm, want the deposit, the bond and 1 usvm per transaction, %s", got, spent)
	}
}

// TestBlockMaxGasIsUnset proves that the block MaxGas is -1. The CometBFT reap
// stops at the first transaction that would exceed MaxGas. With MaxGas set,
// the proposer gets a set already chosen in arrival order, and its sort only
// reorders that set.
func TestBlockMaxGasIsUnset(t *testing.T) {
	if _, maxGas := svm.BlockParams(); maxGas != -1 {
		t.Fatalf("BlockParams reports MaxGas %d, want -1", maxGas)
	}
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: spenders(64)})
	c.Block()
	ctx := c.App.NewUncachedContext(true, cmtproto.Header{Height: c.Height})
	params := c.App.GetConsensusParams(ctx)
	if params.Block.MaxGas != -1 {
		t.Fatalf("the chain runs with MaxGas %d, want -1", params.Block.MaxGas)
	}
	if params.Block.MaxBytes != svm.MaxBlockBytes {
		t.Fatalf("the chain runs with MaxBytes %d", params.Block.MaxBytes)
	}
	// The engine's unit budget, which MaxGas would duplicate, still bounds a
	// block.
	maxUnits, _, _ := ffi.BlockLimits()
	txs, _ := fill(t, c, func(seed byte) []byte { return expensive(t, c, spender(int(seed)), seed) },
		func(a ffi.Admission) uint64 { return a.Units }, maxUnits)
	if packed := c.Pack(svm.MaxBlockBytes, txs...); len(packed) >= len(txs) {
		t.Fatalf("PrepareProposal packed all %d candidates with no gas limit", len(txs))
	}
}

// TestAdmissionCarriesTheFee proves that Admission carries the total fee, the
// numerator of the ranking key. The engine computes it beside the cost, so
// ranking the pool costs one FFI crossing per candidate, not two.
func TestAdmissionCarriesTheFee(t *testing.T) {
	payer := tc.Key(1)
	c := tc.Start(t, tc.Pub(payer), tc.Options{})
	c.Block()

	plain, err := c.Admit(tc.Tx(c.Latest(), payer, nil, tc.Transfer(payer, tc.Pub(tc.Key(3)), 1)))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Fee != 5_000 {
		t.Fatalf("an unpriced transfer pays %d, want the 5,000 base fee", plain.Fee)
	}
	// 1,400,000 units at 1,000 microlamports is 1,400 lamports of priority.
	priced, err := c.Admit(bidder(t, c, payer, 3, 1_000))
	if err != nil {
		t.Fatal(err)
	}
	if priced.Fee != 5_000+1_400 {
		t.Fatalf("a priced transfer pays %d, want 6,400", priced.Fee)
	}
	// The same shape without the bid, so the comparison is the bid alone.
	unpriced, err := c.Admit(bidder(t, c, payer, 3, 0))
	if err != nil {
		t.Fatal(err)
	}
	if priced.Fee*1_000_000/(priced.Units+1) <= unpriced.Fee*1_000_000/(unpriced.Units+1) {
		t.Fatal("the bid did not raise the ranking key")
	}
}

// TestAnUnpayableBidNeverReachesTheBlock proves that admission refuses a bid
// that its payer cannot pay. The pool ranks by the declared fee, and a
// transaction whose payer cannot pay is not processed and pays nothing. If
// admission did not read the payer, a key with no lamports could bid any
// price, outrank every funded transaction and take whole blocks for free.
func TestAnUnpayableBidNeverReachesTheBlock(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()

	funded := c.Wrap(tc.Tx(c.Latest(), faucet, nil,
		tc.Transfer(faucet, tc.Pub(tc.Key(2)), svm.MinimumBalance(0)),
		computeUnitLimit(t, 1_400_000),
		computeUnitPrice(t, 1_000),
	))
	var offered [][]byte
	// More than a block of 1,400,000-unit bids, from keys that hold nothing.
	for seed := byte(100); seed < 150; seed++ {
		bid := c.Wrap(bidder(t, c, tc.Key(seed), 2, 1_000_000_000_000))
		for _, recheck := range []bool{false, true} {
			if res := c.Check(bid, recheck); res.Code != svm.ErrUnpayable.ABCICode() {
				t.Fatalf("recheck %v: an unfunded bid: code %d (%s), want %d", recheck, res.Code, res.Log, svm.ErrUnpayable.ABCICode())
			}
		}
		offered = append(offered, bid)
	}
	packed := c.Pack(svm.MaxBlockBytes, append(offered, funded)...)
	if len(packed) != 1 || !bytes.Equal(packed[0], funded) {
		t.Fatalf("packed %d transactions, want the funded one alone", len(packed))
	}
	if res, _ := c.Deliver(packed...); res[0].Code != 0 {
		t.Fatalf("the funded transaction: code %d", res[0].Code)
	}
}

// TestAPayerIsPackedForWhatItCanPay covers the case that CheckTx cannot see.
// The payer holds one fee above its rent-exempt minimum, so each of two
// transactions is payable alone but not both. The proposer packs the first and
// leaves the second.
func TestAPayerIsPackedForWhatItCanPay(t *testing.T) {
	faucet := tc.Key(1)
	payer := tc.Key(20)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	if res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(payer), svm.MinimumBalance(0)+5_000))); res[0].Code != 0 {
		t.Fatalf("funding the payer: code %d", res[0].Code)
	}
	first := c.Wrap(tc.Tx(c.Latest(), payer, nil, tc.Transfer(payer, tc.Pub(tc.Key(21)), 0)))
	second := c.Wrap(tc.Tx(c.Latest(), payer, nil, tc.Transfer(payer, tc.Pub(tc.Key(22)), 0)))
	for i, raw := range [][]byte{first, second} {
		if res := c.Check(raw, false); res.Code != 0 {
			t.Fatalf("transaction %d alone: code %d (%s), want 0", i, res.Code, res.Log)
		}
	}
	packed := c.Pack(svm.MaxBlockBytes, first, second)
	if len(packed) != 1 {
		t.Fatalf("packed %d of two transactions whose payer can pay one fee", len(packed))
	}
	if res, _ := c.Deliver(packed...); res[0].Code != 0 {
		t.Fatalf("the packed transaction: code %d", res[0].Code)
	}
}

// TestAnExpiredBlockhashIsAdmittedOnlyAsItsNonce proves that admission accepts
// an expired blockhash only if it is the nonce that the named account holds.
// A transaction whose first instruction advances a nonce skips the blockhash
// window. Without this check, any hash and any account pass CheckTx, and the
// block does not process the transaction.
func TestAnExpiredBlockhashIsAdmittedOnlyAsItsNonce(t *testing.T) {
	faucet := tc.Key(1)
	nonce := tc.Key(4)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	if res, _ := c.Block(tc.Tx(c.Latest(), faucet, []ed25519.PrivateKey{nonce}, createNonce(faucet, nonce)...)); res[0].Code != 0 {
		t.Fatalf("create nonce account: code %d", res[0].Code)
	}
	if res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, advanceNonce(tc.Pub(nonce), faucet))); res[0].Code != 0 {
		t.Fatalf("advance: code %d", res[0].Code)
	}
	nonced := func(blockhash, account [32]byte) []byte {
		return c.Wrap(tc.Tx(blockhash, faucet, nil, advanceNonce(account, faucet), tc.Transfer(faucet, tc.Pub(tc.Key(5)), 1)))
	}
	if res := c.Check(nonced(nonceValue(t, c, tc.Pub(nonce)), tc.Pub(nonce)), false); res.Code != 0 {
		t.Fatalf("the nonce the account holds: code %d (%s), want 0", res.Code, res.Log)
	}
	for name, raw := range map[string][]byte{
		"a hash the account does not hold": nonced(tc.HashAt(9_999), tc.Pub(nonce)),
		"an account that does not exist":   nonced(tc.HashAt(9_999), tc.Pub(tc.Key(6))),
	} {
		if res := c.Check(raw, false); res.Code != svm.ErrBlockhashNotFound.ABCICode() {
			t.Fatalf("%s: code %d (%s), want %d", name, res.Code, res.Log, svm.ErrBlockhashNotFound.ABCICode())
		}
	}
}

// requestHeapFrame is ComputeBudgetInstruction::RequestHeapFrame, which the
// runtime accepts only as a multiple of 1 KiB from 32 KiB to 256 KiB.
func requestHeapFrame(t *testing.T, bytes uint32) tc.Instruction {
	return tc.Instruction{
		Program: key(t, "ComputeBudget111111111111111111111111111111"),
		Data:    binary.LittleEndian.AppendUint32([]byte{1}, bytes),
	}
}

// TestATransactionTheBlockWouldNotProcessIsNeverAdmitted proves that admission
// refuses each shape that the block would not process. Such a transaction pays
// nothing, so if admission accepted it, the same balance could buy every block
// ahead of paying transactions. The three priced shapes that reach the
// proposer outrank every honest transfer.
func TestATransactionTheBlockWouldNotProcessIsNeverAdmitted(t *testing.T) {
	payer, stranger, honest := tc.Key(1), tc.Key(6), tc.Key(7)
	table, payerTable := namePub("table", 0), namePub("table", 1)
	dest, ghost := tc.Pub(tc.Key(2)), tc.Pub(tc.Key(78))
	c := tc.Start(t, tc.Pub(payer), tc.Options{Accounts: []svm.GenesisAccount{
		funded(stranger), funded(honest),
		lookupTableRow(table, [][32]byte{dest}),
		lookupTableRow(payerTable, [][32]byte{tc.Pub(payer)}),
	}})
	c.Block()
	nonce := tc.Key(5)
	if res, _ := c.Block(tc.Tx(c.Latest(), payer, []ed25519.PrivateKey{nonce}, createNonce(payer, nonce)...)); res[0].Code != 0 {
		t.Fatalf("create nonce account: code %d", res[0].Code)
	}
	durable := nonceValue(t, c, tc.Pub(nonce))
	bid := computeUnitPrice(t, 1_000_000)
	through := func(table [32]byte, index uint8, address [32]byte) []byte {
		return tc.TxV0(c.Latest(), payer, nil,
			[]tc.Lookup{{Table: table, Writable: []tc.LookupEntry{{Index: index, Key: address}}}},
			bid, tc.Transfer(payer, address, svm.MinimumBalance(0)))
	}
	shapes := []struct {
		name   string
		tx     []byte
		code   uint32
		reason string
	}{
		{"a heap frame the runtime refuses", tc.Tx(c.Latest(), payer, nil, requestHeapFrame(t, 33), tc.Transfer(payer, dest, 1)),
			svm.ErrAdmission.ABCICode(), "InvalidInstructionData"},
		{"a loaded accounts limit of zero", tc.Tx(c.Latest(), payer, nil, loadedAccountsDataSizeLimit(t, 0), tc.Transfer(payer, dest, 1)),
			svm.ErrAdmission.ABCICode(), "InvalidLoadedAccountsDataSizeLimit"},
		{"a table that does not exist", through(tc.Pub(tc.Key(77)), 0, ghost),
			svm.ErrAdmission.ABCICode(), "AddressLookupTableNotFound"},
		{"an index past the table's end", through(table, 1, ghost),
			svm.ErrAdmission.ABCICode(), "InvalidAddressLookupTableIndex"},
		{"the payer named again through a table", tc.TxV0(c.Latest(), payer, nil,
			[]tc.Lookup{{Table: payerTable, Readonly: []tc.LookupEntry{{Index: 0, Key: tc.Pub(payer)}}}},
			bid, tc.Transfer(payer, dest, 1)),
			svm.ErrAdmission.ABCICode(), "AccountLoadedTwice"},
		{"a nonce its authority did not sign", tc.Tx(durable, stranger, nil,
			advanceNonce(tc.Pub(nonce), stranger), bid, tc.Transfer(stranger, dest, 1)),
			svm.ErrBlockhashNotFound.ABCICode(), "BlockhashNotFound"},
	}
	transfer := c.Wrap(tc.Tx(c.Latest(), honest, nil, tc.Transfer(honest, tc.Pub(tc.Key(9)), svm.MinimumBalance(0))))

	var offered [][]byte
	for _, s := range shapes {
		raw := c.Wrap(s.tx)
		for _, recheck := range []bool{false, true} {
			if res := c.Check(raw, recheck); res.Code != s.code || !strings.Contains(res.Log, s.reason) {
				t.Fatalf("%s, recheck %v: code %d (%s), want %d naming %s", s.name, recheck, res.Code, res.Log, s.code, s.reason)
			}
		}
		// The proposer leaves an expired blockhash to Recheck, which refuses
		// the nonce shape as it refuses every expired blockhash.
		if s.code != svm.ErrBlockhashNotFound.ABCICode() {
			offered = append(offered, raw)
		}
	}
	if packed := c.Pack(svm.MaxBlockBytes, append(offered, transfer)...); len(packed) != 1 || !bytes.Equal(packed[0], transfer) {
		t.Fatalf("packed %d transactions, want the honest transfer alone", len(packed))
	}
	// The stateless half refuses a budget that the block cannot configure, so
	// every machine rejects a proposal that carries one.
	for _, s := range shapes[:2] {
		if status := c.Propose(c.Wrap(s.tx), transfer); status != abci.ResponseProcessProposal_REJECT {
			t.Fatalf("a proposal carrying %s: %v, want REJECT", s.name, status)
		}
	}

	// The block path stays the same. If a block delivers them anyway, none is
	// processed and none pays. The honest transfer beside them is processed.
	before := c.Lamports(tc.Pub(payer)) + c.Lamports(tc.Pub(stranger))
	block := make([][]byte, 0, len(shapes)+1)
	for _, s := range shapes {
		block = append(block, s.tx)
	}
	res, _ := c.Block(append(block, c.Unwrap(transfer))...)
	for i, s := range shapes {
		if res[i].Code != codeNotProcessed {
			t.Fatalf("%s: code %d in the block, want %d", s.name, res[i].Code, codeNotProcessed)
		}
	}
	if res[len(shapes)].Code != 0 {
		t.Fatalf("the honest transfer: code %d", res[len(shapes)].Code)
	}
	if after := c.Lamports(tc.Pub(payer)) + c.Lamports(tc.Pub(stranger)); after != before {
		t.Fatalf("the shapes' payers paid %d lamports", before-after)
	}

	// The positive halves are a table that resolves and the nonce its
	// authority signed.
	for name, tx := range map[string][]byte{
		"a transfer through a table": through(table, 0, dest),
		"the nonce its authority signed": tc.Tx(nonceValue(t, c, tc.Pub(nonce)), payer, nil,
			advanceNonce(tc.Pub(nonce), payer), tc.Transfer(payer, dest, 1)),
	} {
		if res := c.Check(c.Wrap(tx), false); res.Code != 0 {
			t.Fatalf("%s: code %d (%s), want 0", name, res.Code, res.Log)
		}
	}
}

// TestATableIsAdmittedAsTheNextBlockResolvesIt proves that admission resolves
// a table at the next block's height and against its SlotHashes, as phase 2
// of that block does. An address appended in the last committed block is
// visible there, and a table deactivated in it is still deactivating. Resolution
// at the committed height would refuse both.
func TestATableIsAdmittedAsTheNextBlockResolvesIt(t *testing.T) {
	alt := key(t, addressLookupTable)
	authority := tc.Key(1)
	dest := tc.Pub(tc.Key(80))
	c := tc.Start(t, tc.Pub(authority), tc.Options{Programs: tc.ManifestPrograms(t)})
	c.Block()
	create, table := createLookupTable(c.Latest(), authority, 1, alt)
	if res, _ := c.Block(create); res[0].Code != 0 {
		t.Fatalf("CreateLookupTable: code %d", res[0].Code)
	}
	spend := func() []byte {
		return tc.TxV0(c.Latest(), authority, nil,
			[]tc.Lookup{{Table: table, Writable: []tc.LookupEntry{{Index: 0, Key: dest}}}},
			tc.Transfer(authority, dest, svm.MinimumBalance(0)))
	}

	if res, _ := c.Block(extendLookupTable(c.Latest(), authority, table, alt, [][32]byte{dest})); res[0].Code != 0 {
		t.Fatalf("ExtendLookupTable: code %d", res[0].Code)
	}
	if res := c.Check(c.Wrap(spend()), false); res.Code != 0 {
		t.Fatalf("an address appended in the block just committed: code %d (%s), want 0", res.Code, res.Log)
	}

	if res, _ := c.Block(altSimple(c.Latest(), authority, table, alt, 3)); res[0].Code != 0 {
		t.Fatalf("DeactivateLookupTable: code %d", res[0].Code)
	}
	tx := spend()
	if res := c.Check(c.Wrap(tx), false); res.Code != 0 {
		t.Fatalf("a table deactivated in the block just committed: code %d (%s), want 0", res.Code, res.Log)
	}
	if res, _ := c.Block(tx); res[0].Code != 0 {
		t.Fatalf("the next block: code %d, so admission and phase 2 disagree", res[0].Code)
	}
}
