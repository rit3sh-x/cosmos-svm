package app_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/client"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/cosmos/cosmos-sdk/types/mempool"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/app"
	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// computeUnitLimit returns ComputeBudgetInstruction::SetComputeUnitLimit,
// which the cost model charges as declared.
func computeUnitLimit(t *testing.T, units uint32) tc.Instruction {
	return tc.Instruction{
		Program: key(t, "ComputeBudget111111111111111111111111111111"),
		Data:    binary.LittleEndian.AppendUint32([]byte{2}, units),
	}
}

// expensive builds a transfer that declares 1,400,000 compute units, so about
// 42 of them fill a 60,000,000-unit block.
func expensive(t *testing.T, c *tc.Chain, payer ed25519.PrivateKey, seed byte) []byte {
	return tc.Tx(c.Latest(), payer, nil,
		tc.Transfer(payer, tc.Pub(tc.Key(seed)), 1),
		computeUnitLimit(t, 1_400_000),
	)
}

// fill builds transactions until they cost more than one block's budget, and
// returns them wrapped in their Cosmos envelopes. Each build gives its
// transaction its own payer (a spender), so the block budget binds before the
// share of any one payer does.
func fill(t *testing.T, c *tc.Chain, build func(seed byte) []byte, charge func(ffi.Admission) uint64, limit uint64) ([][]byte, uint64) {
	var txs [][]byte
	var total uint64
	for seed := byte(0); total <= limit; seed++ {
		tx := build(seed)
		a, err := c.Admit(tx)
		if err != nil {
			t.Fatal(err)
		}
		txs = append(txs, c.Wrap(tx))
		total += charge(a)
	}
	return txs, total
}

// TestProposalOverTheUnitBudgetIsRejected checks the block budget on a
// validator that did not build the block, also one whose app.toml turns the
// mempool off.
func TestProposalOverTheUnitBudgetIsRejected(t *testing.T) {
	payer := tc.Key(1)
	for _, noMempool := range []bool{false, true} {
		opts := tc.Options{Accounts: spenders(64)}
		if noMempool {
			opts.BaseApp = []func(*baseapp.BaseApp){baseapp.SetMempool(mempool.NoOpMempool{})}
		}
		c := tc.Start(t, tc.Pub(payer), opts)
		c.Block()

		maxUnits, _, _ := ffi.BlockLimits()
		txs, units := fill(t, c, func(seed byte) []byte { return expensive(t, c, spender(int(seed)), seed) },
			func(a ffi.Admission) uint64 { return a.Units }, maxUnits)
		if got := c.Propose(txs...); got != abci.ResponseProcessProposal_REJECT {
			t.Fatalf("no mempool %v: %d transactions costing %d units: %v, want REJECT", noMempool, len(txs), units, got)
		}
		if got := c.Propose(txs[:len(txs)-1]...); got != abci.ResponseProcessProposal_ACCEPT {
			t.Fatalf("no mempool %v: the same block one transaction shorter: %v, want ACCEPT", noMempool, got)
		}
	}
}

// TestAProposalCannotWrapTheUnitBudget proves that one proposer cannot halt
// the chain by wrapping the unit sum. ProcessProposal adds a Cosmos
// transaction's declared gas to the sum of units, so gas of 2^64 minus the SVM
// units before it would wrap the sum to zero. A second block of SVM
// transactions could then follow, which the FinalizeBlock plan refuses on every
// machine.
func TestAProposalCannotWrapTheUnitBudget(t *testing.T) {
	payer := tc.Key(1)
	c := tc.Start(t, tc.Pub(payer), tc.Options{Accounts: spenders(64)})
	c.Block()

	maxUnits, _, _ := ffi.BlockLimits()
	var first, second [][]byte
	var spent, more uint64
	for seed := byte(0); ; seed++ {
		tx := expensive(t, c, spender(int(seed)), seed)
		a, err := c.Admit(tx)
		if err != nil {
			t.Fatal(err)
		}
		if spent+a.Units <= maxUnits {
			first = append(first, c.Wrap(tx))
			spent += a.Units
			continue
		}
		if more+a.Units > maxUnits {
			break
		}
		second = append(second, c.Wrap(tx))
		more += a.Units
	}
	block := func(gas uint64, rest [][]byte) [][]byte {
		return append(append(append([][]byte{}, first...), declaring(t, c, gas)), rest...)
	}

	if got := c.Propose(block(math.MaxUint64-spent+1, second)...); got != abci.ResponseProcessProposal_REJECT {
		t.Fatalf("%d SVM units after gas that wraps the sum: %v, want REJECT", spent+more, got)
	}
	if got := c.Propose(block(maxUnits-spent+1, nil)...); got != abci.ResponseProcessProposal_REJECT {
		t.Fatalf("gas one unit over what the SVM half left: %v, want REJECT", got)
	}
	if got := c.Propose(block(maxUnits-spent, nil)...); got != abci.ResponseProcessProposal_ACCEPT {
		t.Fatalf("gas exactly what the SVM half left: %v, want ACCEPT", got)
	}

	// On the proposer's side, what it packs finalizes, whatever it is offered.
	packed := c.Pack(svm.MaxBlockBytes, block(math.MaxUint64-spent+1, second)...)
	if got := c.Propose(packed...); got != abci.ResponseProcessProposal_ACCEPT {
		t.Fatalf("a proposal this machine packed: %v, want ACCEPT", got)
	}
	c.Deliver(packed...)
}

// declaring builds a Cosmos transaction that declares gas and does nothing
// else. Operator 0 sends itself one usvm. The transaction is signed, because
// ProcessProposal runs its ante handler when the budgets admit it.
func declaring(t *testing.T, c *tc.Chain, gas uint64) []byte {
	t.Helper()
	return c.CosmosGas(tc.OperatorKey(0), gas, selfSend())
}

func selfSend() *banktypes.MsgSend {
	from := tc.Address(tc.OperatorKey(0)).String()
	return &banktypes.MsgSend{FromAddress: from, ToAddress: from, Amount: sdk.NewCoins(sdk.NewInt64Coin(app.BondDenom, 1))}
}

// TestACosmosTransactionInAProposalPassesItsAnteHandler proves that both
// proposal handlers run the ante handler of a Cosmos transaction, as the
// SDK's handlers do. Without it, a proposer could fill a block with Cosmos
// transactions that nobody signed or paid for, about 1.4 million at four bytes
// each, and each machine would run each one in FinalizeBlock.
func TestACosmosTransactionInAProposalPassesItsAnteHandler(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	c.Block()
	b := c.App.TxConfig.NewTxBuilder()
	if err := b.SetMsgs(selfSend()); err != nil {
		t.Fatal(err)
	}
	unsigned, err := c.App.TxConfig.TxEncoder()(b.GetTx())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Propose(unsigned); got != abci.ResponseProcessProposal_REJECT {
		t.Fatalf("a proposal carrying an unsigned Cosmos transaction: %v, want REJECT", got)
	}
	if packed := c.Pack(svm.MaxBlockBytes, unsigned); len(packed) != 0 {
		t.Fatalf("packed %d unsigned Cosmos transactions", len(packed))
	}
	signed := c.Cosmos(tc.OperatorKey(0), selfSend())
	if got := c.Propose(signed); got != abci.ResponseProcessProposal_ACCEPT {
		t.Fatalf("a proposal carrying a signed Cosmos transaction: %v, want ACCEPT", got)
	}
}

// TestAnSVMTransactionHasOneEnvelope proves that an SVM transaction has one
// valid envelope. CometBFT keys its pool on the hash of the envelope, and each
// handler reads only the Solana bytes inside. Without this rule, one transfer
// wrapped four ways makes four pool and block entries, one with a 512 KiB memo,
// and its payer pays once.
func TestAnSVMTransactionHasOneEnvelope(t *testing.T) {
	payer := tc.Key(1)
	c := tc.Start(t, tc.Pub(payer), tc.Options{})
	c.Block()
	transfer := tc.Tx(c.Latest(), payer, nil, tc.Transfer(payer, tc.Pub(tc.Key(2)), svm.MinimumBalance(0)))
	canonical := c.Wrap(transfer)

	rewrap := func(edit func(client.TxBuilder)) []byte {
		b := c.App.TxConfig.NewTxBuilder()
		if err := b.SetMsgs(&svm.MsgSVMTransaction{Transaction: transfer}); err != nil {
			t.Fatal(err)
		}
		edit(b)
		raw, err := c.App.TxConfig.TxEncoder()(b.GetTx())
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	rewraps := map[string][]byte{
		"a memo":           rewrap(func(b client.TxBuilder) { b.SetMemo("copy 1") }),
		"512 KiB of memo":  rewrap(func(b client.TxBuilder) { b.SetMemo(strings.Repeat("x", 512<<10)) }),
		"a timeout height": rewrap(func(b client.TxBuilder) { b.SetTimeoutHeight(100) }),
		"a gas limit":      rewrap(func(b client.TxBuilder) { b.SetGasLimit(1) }),
	}
	for name, raw := range rewraps {
		if res := c.Check(raw, false); res.Code != svm.ErrEnvelope.ABCICode() {
			t.Fatalf("%s: CheckTx code %d (%s), want %d", name, res.Code, res.Log, svm.ErrEnvelope.ABCICode())
		}
		if got := c.Propose(canonical, raw); got != abci.ResponseProcessProposal_REJECT {
			t.Fatalf("%s: a proposal carrying it: %v, want REJECT", name, got)
		}
	}
	if res := c.Check(canonical, false); res.Code != 0 {
		t.Fatalf("the canonical envelope: CheckTx code %d (%s)", res.Code, res.Log)
	}

	// One dedup key under two signatures. A signer can always make a second
	// valid signature, so one envelope per transaction does not give one block
	// entry per message. Process checks signatures, so the twin is a second
	// valid signature, not a flipped bit, and the dedup key refuses it.
	resigned := tc.Resign(transfer, payer)
	if bytes.Equal(resigned, transfer) {
		t.Fatal("the second signature is the first")
	}
	if res := c.Check(c.Wrap(resigned), false); res.Code != 0 {
		t.Fatalf("the re-signed transfer alone: CheckTx code %d (%s)", res.Code, res.Log)
	}
	twin := c.Wrap(resigned)
	for name, txs := range map[string][][]byte{
		"the same bytes twice":       {canonical, canonical},
		"the same message re-signed": {canonical, twin},
	} {
		if got := c.Propose(txs...); got != abci.ResponseProcessProposal_REJECT {
			t.Fatalf("%s: %v, want REJECT", name, got)
		}
	}
	if packed := c.Pack(svm.MaxBlockBytes, canonical, twin); len(packed) != 1 {
		t.Fatalf("packed %d of two transactions sharing a dedup key", len(packed))
	}
	if got := c.Propose(canonical); got != abci.ResponseProcessProposal_ACCEPT {
		t.Fatalf("the canonical envelope alone: %v, want ACCEPT", got)
	}

	// The block runner reads the same bytes. In a block, each rewrap fails to
	// decode, and the canonical envelope runs.
	block := append(slices.Collect(maps.Values(rewraps)), canonical)
	results, _ := c.Deliver(block...)
	for i, r := range results[:len(rewraps)] {
		if r.Codespace != sdkerrors.ErrTxDecode.Codespace() || r.Code != sdkerrors.ErrTxDecode.ABCICode() {
			t.Fatalf("rewrap %d in a block: %s code %d (%s), want a decode failure", i, r.Codespace, r.Code, r.Log)
		}
	}
	if r := results[len(rewraps)]; r.Code != 0 {
		t.Fatalf("the canonical envelope in a block: code %d (%s)", r.Code, r.Log)
	}

	// The largest envelope is a 4,096-byte V1 transaction plus 43 bytes of
	// framing.
	if n := len(c.Wrap(make([]byte, 4096))) - 4096; n != 43 {
		t.Fatalf("a 4,096-byte transaction's envelope adds %d bytes, want 43", n)
	}
}

// TestEnvelopedIsDecoder proves svm.Enveloped, the byte match that every
// handler tries before Decoder. Enveloped must find a transaction exactly
// where Decoder accepts an SVM transaction that is not empty, with the same
// bytes. If not, a fast path changes an answer.
func TestEnvelopedIsDecoder(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	decode := svm.Decoder(c.App.TxConfig)
	agree := func(what string, raw []byte) bool {
		t.Helper()
		got, ok := svm.Enveloped(raw)
		var want []byte
		if tx, err := decode(raw); err == nil {
			if msgs := tx.GetMsgs(); len(msgs) == 1 {
				if m, isSVM := msgs[0].(*svm.MsgSVMTransaction); isSVM {
					want = m.Transaction
				}
			}
		}
		if ok != (len(want) > 0) || !bytes.Equal(got, want) {
			t.Fatalf("%s: Enveloped found %v, %d bytes; Decoder's transaction has %d bytes", what, ok, len(got), len(want))
		}
		return ok
	}
	// frame writes Envelope by hand. The length at depth pad is one varint
	// byte longer than it must be. Depth 0 is the body's length and depth 4
	// is the transaction's.
	frame := func(p []byte, pad int) []byte {
		field := func(depth int, tag byte, v []byte) []byte {
			n := binary.AppendUvarint(nil, uint64(len(v)))
			if depth == pad {
				n[len(n)-1] |= 0x80
				n = append(n, 0)
			}
			return append(append([]byte{tag}, n...), v...)
		}
		value := field(3, 0x12, field(4, 0x0a, p))
		message := field(1, 0x0a, append(field(2, 0x0a, []byte("/svm.v1.MsgSVMTransaction")), value...))
		return append(field(0, 0x0a, message), 0x12, 0x02, 0x12, 0x00)
	}

	// The test runs every transaction length up to 300 bytes, the lengths
	// around the next two varint boundaries, and a whole block. For each
	// length, a hand encoding must be Envelope's. Then Decoder and Enveloped
	// must agree on the envelope with each length padded to a longer varint,
	// truncated at each framing byte, and extended by one byte. Up to the
	// second boundary, the test flips each framing bit. Around the first
	// boundary, it substitutes, inserts and deletes every byte value at each
	// framing position.
	var lengths []int
	for n := 1; n <= 300; n++ {
		lengths = append(lengths, n)
	}
	for _, b := range []int{1 << 14, 1 << 21} {
		for n := b - 48; n <= b+4; n++ {
			lengths = append(lengths, n)
		}
	}
	lengths = append(lengths, svm.MaxBlockBytes)
	for _, n := range lengths {
		p := make([]byte, n)
		for i := range p {
			p[i] = byte(i*131 + n)
		}
		raw := c.Wrap(p)
		if !bytes.Equal(frame(p, -1), raw) {
			t.Fatalf("%d bytes: the hand encoding is not Envelope's", n)
		}
		if !agree(fmt.Sprintf("%d bytes", n), raw) {
			t.Fatalf("%d bytes: Envelope's own bytes were not found", n)
		}
		for pad := 0; pad <= 4; pad++ {
			agree(fmt.Sprintf("%d bytes, length %d padded", n, pad), frame(p, pad))
		}
		// The transaction is raw[head:tail]. Every other byte frames it.
		head, tail := len(raw)-4-n, len(raw)-4
		var framing []int
		for i := range raw {
			if i < head || i >= tail {
				framing = append(framing, i)
			}
		}
		for _, k := range append(framing, head+n/2) {
			agree(fmt.Sprintf("%d bytes, truncated to %d", n, k), raw[:k])
		}
		agree(fmt.Sprintf("%d bytes, extended", n), append(raw[:len(raw):len(raw)], 0))
		if n > 1<<14+4 {
			continue
		}
		for _, i := range framing {
			for bit := range 8 {
				raw[i] ^= 1 << bit
				agree(fmt.Sprintf("%d bytes, bit %d of byte %d flipped", n, bit, i), raw)
				raw[i] ^= 1 << bit
			}
		}
		if n > 2 && (n < 93 || n > 130) {
			continue
		}
		for _, i := range framing {
			for v := range 256 {
				changed := slices.Clone(raw)
				changed[i] = byte(v)
				agree(fmt.Sprintf("%d bytes, byte %d set to %d", n, i, v), changed)
				agree(fmt.Sprintf("%d bytes, %d inserted at %d", n, v, i), slices.Insert(slices.Clone(raw), i, byte(v)))
			}
			agree(fmt.Sprintf("%d bytes, byte %d deleted", n, i), slices.Delete(slices.Clone(raw), i, i+1))
		}
	}

	// Last come rewraps with a memo, a timeout, a gas limit, two messages, an
	// empty transaction and a Cosmos message, then no bytes and an empty body.
	m := &svm.MsgSVMTransaction{Transaction: []byte{1, 2, 3}}
	for name, build := range map[string]func(client.TxBuilder) error{
		"a memo":                 func(b client.TxBuilder) error { b.SetMemo("x"); return b.SetMsgs(m) },
		"a timeout height":       func(b client.TxBuilder) error { b.SetTimeoutHeight(1); return b.SetMsgs(m) },
		"a gas limit":            func(b client.TxBuilder) error { b.SetGasLimit(1); return b.SetMsgs(m) },
		"two messages":           func(b client.TxBuilder) error { return b.SetMsgs(m, m) },
		"an empty transaction":   func(b client.TxBuilder) error { return b.SetMsgs(&svm.MsgSVMTransaction{}) },
		"a Cosmos message":       func(b client.TxBuilder) error { return b.SetMsgs(selfSend()) },
		"the canonical envelope": func(b client.TxBuilder) error { return b.SetMsgs(m) },
	} {
		b := c.App.TxConfig.NewTxBuilder()
		if err := build(b); err != nil {
			t.Fatal(err)
		}
		raw, err := c.App.TxConfig.TxEncoder()(b.GetTx())
		if err != nil {
			t.Fatal(err)
		}
		if agree(name, raw) != (name == "the canonical envelope") {
			t.Fatalf("%s: Enveloped and Decoder agree, but not as this test expects", name)
		}
	}
	for name, raw := range map[string][]byte{
		"no bytes":      nil,
		"an empty body": {0x0a, 0x00, 0x12, 0x02, 0x12, 0x00},
	} {
		if agree(name, raw) {
			t.Fatalf("%s: found a transaction", name)
		}
	}
}

// TestProposalOverTheAllocationBudgetIsRejected proves that a proposal over
// the allocation budget is rejected. The budget is 100,000,000 bytes of new
// account data per block.
func TestProposalOverTheAllocationBudgetIsRejected(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: spenders(64)})
	c.Block()

	const space = 6_000_000
	_, maxAllocated, _ := ffi.BlockLimits()
	build := func(seed byte) []byte {
		payer, account := spender(int(seed)), tc.Key(100+seed)
		return tc.Tx(c.Latest(), payer, []ed25519.PrivateKey{account},
			createAccount(payer, account, space, svm.SystemProgram))
	}
	a, err := c.Admit(build(0))
	if err != nil {
		t.Fatal(err)
	}
	if a.AllocatedBytes != space {
		t.Fatalf("a %d byte allocation is charged %d bytes", space, a.AllocatedBytes)
	}
	// One unit per six allocated bytes.
	if a.Units < space/6 {
		t.Fatalf("a %d byte allocation costs %d units, want at least %d", space, a.Units, space/6)
	}
	txs, allocated := fill(t, c, build, func(a ffi.Admission) uint64 { return a.AllocatedBytes }, maxAllocated)
	if got := c.Propose(txs...); got != abci.ResponseProcessProposal_REJECT {
		t.Fatalf("%d bytes of allocation: %v, want REJECT", allocated, got)
	}
	if got := c.Propose(txs[:len(txs)-1]...); got != abci.ResponseProcessProposal_ACCEPT {
		t.Fatalf("the same block one transaction shorter: %v, want ACCEPT", got)
	}
}

// TestAdmissionForAnActivationHeightReadsItsBeginBlock proves that admission
// reads the next block's context at a gate's height H. The rent6960 gate
// raises rent from 5,080 to 6,960 lamports a byte in the begin-block of H, so
// a payer that holds the old minimum and one fee can pay for H-1 but not for H.
// Admission, the proposer and the block all refuse it for H, where committed
// state would admit and pack it.
func TestAdmissionForAnActivationHeightReadsItsBeginBlock(t *testing.T) {
	faucet, payer := tc.Key(1), tc.Key(5)
	pub := tc.Pub(payer)
	held := svm.MinimumBalance(0) + 5_000
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Accounts: []svm.GenesisAccount{{
		Pubkey: base58.Encode(pub[:]), Lamports: held,
		Owner: base58.Encode(svm.SystemProgram[:]), RentEpoch: ^uint64(0),
	}}})
	c.Block()
	// The next block commits the schedule. The block after it is H-1, and
	// then H.
	flip := c.Height + 3
	if err := schedule(t, c, flip-svm.MinActivationDelay, flip, rent6960); err != nil {
		t.Fatal(err)
	}
	c.Block()
	pay := func() []byte { return tc.Tx(c.Latest(), payer, nil, tc.Transfer(payer, tc.Pub(faucet), 0)) }
	if res := c.Check(c.Wrap(pay()), false); res.Code != 0 {
		t.Fatalf("for height %d, before the raise, CheckTx refused the payer: %s", c.Height+1, res.Log)
	}
	c.Block()

	tx := pay()
	if res := c.Check(c.Wrap(tx), false); res.Code != svm.ErrUnpayable.ABCICode() {
		t.Fatalf("for H, CheckTx answered code %d (%s), want the payer refused", res.Code, res.Log)
	}
	if packed := c.Pack(1<<20, c.Wrap(tx)); len(packed) != 0 {
		t.Fatalf("PrepareProposal(H) packed %d transactions, want none", len(packed))
	}
	if res, _ := c.Block(tx); c.Height != flip || res[0].Code == 0 || c.Lamports(pub) != held {
		t.Fatalf("at height %d the block ran the transaction its payer cannot pay: code %d, the payer holds %d", c.Height, res[0].Code, c.Lamports(pub))
	}
}

// TestAnOversizeTransactionIsRejectedEverywhere proves the packet limit. A
// transaction one byte over 1,232 never enters the mempool and poisons each
// proposal that carries it.
func TestAnOversizeTransactionIsRejectedEverywhere(t *testing.T) {
	payer := tc.Key(1)
	c := tc.Start(t, tc.Pub(payer), tc.Options{})
	c.Block()

	// The padding goes to a program of its own, because the cost model parses
	// a ComputeBudget instruction and rejects one with arbitrary bytes.
	pad := func(n int) []byte {
		return tc.Tx(c.Latest(), payer, nil, tc.Instruction{
			Program: tc.Pub(tc.Key(200)),
			Data:    make([]byte, n),
		})
	}
	// The instruction data length has a compact prefix that grows, so the loop
	// solves for the padding instead of one subtraction.
	sized := func(target int) []byte {
		n := target - len(pad(0))
		for tx := pad(n); ; tx = pad(n) {
			if len(tx) == target {
				return tx
			}
			n += target - len(tx)
		}
	}
	over, fits := sized(1233), sized(1232)
	if len(over) != 1233 || len(fits) != 1232 {
		t.Fatalf("built %d and %d byte transactions", len(over), len(fits))
	}
	if res := c.Check(c.Wrap(over), false); res.Code == 0 {
		t.Fatal("CheckTx admitted a 1,233 byte transaction")
	}
	if got := c.Propose(c.Wrap(over)); got != abci.ResponseProcessProposal_REJECT {
		t.Fatalf("a proposal carrying a 1,233 byte transaction: %v, want REJECT", got)
	}
	if got := c.Propose(c.Wrap(fits)); got != abci.ResponseProcessProposal_ACCEPT {
		t.Fatalf("a proposal carrying a 1,232 byte transaction: %v, want ACCEPT", got)
	}
}

// TestATransactionIsItsExactBytes proves that admission parses exactly, as
// Agave's leader parses a packet, and bounds the bytes it gets. A transfer
// followed by any padding is refused at CheckTx, poisons a proposal and is
// not processed in a block. Padding never rides at the price of the
// transaction before it.
func TestATransactionIsItsExactBytes(t *testing.T) {
	payer := tc.Key(1)
	c := tc.Start(t, tc.Pub(payer), tc.Options{})
	c.Block()

	transfer := tc.Tx(c.Latest(), payer, nil, tc.Transfer(payer, tc.Pub(tc.Key(2)), svm.MinimumBalance(0)))
	for _, n := range []int{1, 1233 - len(transfer), 1 << 20} {
		padded := c.Wrap(slices.Concat(transfer, make([]byte, n)))
		res := c.Check(padded, false)
		if res.Code != svm.ErrAdmission.ABCICode() || !strings.Contains(res.Log, "SanitizeFailure") {
			t.Fatalf("CheckTx of a transfer padded by %d bytes: code %d, log %q", n, res.Code, res.Log)
		}
		if got := c.Propose(padded); got != abci.ResponseProcessProposal_REJECT {
			t.Fatalf("a proposal carrying a transfer padded by %d bytes: %v, want REJECT", n, got)
		}
	}
	res, _ := c.Block(slices.Concat(transfer, []byte{0}), transfer)
	if res[0].Code != codeNotProcessed {
		t.Fatalf("a padded transfer in a block gave code %d, want %d", res[0].Code, codeNotProcessed)
	}
	if res[1].Code != codeOK {
		t.Fatalf("the same transfer's exact bytes beside it gave code %d", res[1].Code)
	}
}

// TestRecheckVerifiesNoSignatures proves that recheck does not verify ed25519
// signatures again. CometBFT rechecks each remaining transaction after each
// block.
func TestRecheckVerifiesNoSignatures(t *testing.T) {
	payer := tc.Key(1)
	c := tc.Start(t, tc.Pub(payer), tc.Options{})
	c.Block()
	raw := c.Wrap(tc.Tx(c.Latest(), payer, nil, tc.Transfer(payer, tc.Pub(tc.Key(2)), 1)))

	before := ffi.SignatureVerifications()
	res := c.Check(raw, false)
	if res.Code != 0 {
		t.Fatalf("CheckTx New: code %d, log %s", res.Code, res.Log)
	}
	if res.GasWanted <= 0 {
		t.Fatalf("CheckTx reported %d gas wanted", res.GasWanted)
	}
	if n := ffi.SignatureVerifications() - before; n != 1 {
		t.Fatalf("CheckTx New verified %d transactions, want 1", n)
	}

	before = ffi.SignatureVerifications()
	if res := c.Check(raw, true); res.Code != 0 {
		t.Fatalf("CheckTx Recheck: code %d, log %s", res.Code, res.Log)
	}
	if n := ffi.SignatureVerifications() - before; n != 0 {
		t.Fatalf("CheckTx Recheck verified %d transactions, want 0", n)
	}
}

// TestCheckTxRejectsAnExpiredOrSpentTransaction covers the state-dependent
// half of admission, which both New and Recheck run.
func TestCheckTxRejectsAnExpiredOrSpentTransaction(t *testing.T) {
	payer := tc.Key(1)
	c := tc.Start(t, tc.Pub(payer), tc.Options{})
	c.Block()

	stale := c.Wrap(tc.Tx(tc.HashAt(9_999), payer, nil, tc.Transfer(payer, tc.Pub(tc.Key(2)), 1)))
	if res := c.Check(stale, false); res.Code != svm.ErrBlockhashNotFound.ABCICode() {
		t.Fatalf("a transaction on an unknown blockhash: code %d, want %d", res.Code, svm.ErrBlockhashNotFound.ABCICode())
	}

	spent := tc.Tx(c.Latest(), payer, nil, tc.Transfer(payer, tc.Pub(tc.Key(2)), svm.MinimumBalance(0)))
	if res, _ := c.Block(spent); res[0].Code != 0 {
		t.Fatalf("the transaction did not execute: code %d", res[0].Code)
	}
	for _, recheck := range []bool{false, true} {
		if res := c.Check(c.Wrap(spent), recheck); res.Code != svm.ErrAlreadyProcessed.ABCICode() {
			t.Fatalf("recheck %v: a processed transaction: code %d, want %d", recheck, res.Code, svm.ErrAlreadyProcessed.ABCICode())
		}
	}
}

// TestPrepareProposalPacksInRankedOrder proves that the proposer selects
// against the budget and packs in ranked order, and that every other
// validator accepts what it packs. Each candidate pays the same 5,000
// lamports, so cost alone sets the rank. Each declares 1,000 units fewer than
// the one before it, so the cheapest, which ranks highest, arrives last.
func TestPrepareProposalPacksInRankedOrder(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{Accounts: spenders(64)})
	c.Block()

	maxUnits, _, _ := ffi.BlockLimits()
	cheaper := func(seed byte) []byte {
		payer := spender(int(seed))
		return tc.Tx(c.Latest(), payer, nil, tc.Transfer(payer, tc.Pub(tc.Key(seed)), 1),
			computeUnitLimit(t, 1_400_000-1_000*uint32(seed)))
	}
	txs, units := fill(t, c, cheaper, func(a ffi.Admission) uint64 { return a.Units }, maxUnits)
	packed := c.Pack(svm.MaxBlockBytes, txs...)
	if len(packed) >= len(txs) {
		t.Fatalf("packed %d of %d transactions costing %d units", len(packed), len(txs), units)
	}
	previous := ^uint64(0)
	for i, raw := range packed {
		rank := rankOf(t, c, raw)
		if rank > previous {
			t.Fatalf("packed transaction %d ranks %d, above the %d before it", i, rank, previous)
		}
		previous = rank
	}
	// The block is the top of the ranking, not a prefix of the arrival order.
	// The cheapest candidate goes first, whenever it arrived.
	if best := rankOf(t, c, packed[0]); best <= rankOf(t, c, txs[0]) {
		t.Fatalf("the first packed transaction ranks %d, no better than the first to arrive", best)
	}
	if got := c.Propose(packed...); got != abci.ResponseProcessProposal_ACCEPT {
		t.Fatalf("a proposal this machine packed: %v, want ACCEPT", got)
	}
}

// rankOf returns `total_fee * 1e6 / (cost + 1)` for a wrapped transaction,
// the key that x/svm sorts by.
func rankOf(t *testing.T, c *tc.Chain, wrapped []byte) uint64 {
	t.Helper()
	a, err := c.Admit(c.Unwrap(wrapped))
	if err != nil {
		t.Fatal(err)
	}
	return a.Fee * 1_000_000 / (a.Units + 1)
}
