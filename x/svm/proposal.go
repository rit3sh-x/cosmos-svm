package svm

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/mempool"

	"github.com/rit3sh-x/cosmos-svm/ffi"
)

// Proposals is the pair of proposal handlers. PrepareProposal selects and does
// not schedule. Conflict groups are the connected components of the conflict
// predicate over the block's set. Components do not depend on order, so a new
// order gains nothing.
//
// Both handlers charge a transaction the same seven-term cost that the block
// runner charges. Both read the budget from the engine, so the limits live in
// one place.
type Proposals struct {
	Decode sdk.TxDecoder
	Encode sdk.TxEncoder
	// Mempool is the selection seam. ProcessProposal does not use it, on
	// purpose, because validation must not depend on a machine-local choice.
	Mempool mempool.Mempool
	// Stores is where Prepare reads the fee payers' balances.
	Stores Stores
	// Next is the context of the block being proposed, or nil before the
	// first block commits. Both handlers resolve and price against it, under
	// its gate set, as FinalizeBlock will.
	Next func() (storetypes.MultiStore, *Next, error)
	// VerifyPrepared and VerifyProposed run the ante handler of a Cosmos
	// transaction on the state of the block so far. They are BaseApp's
	// PrepareProposalVerifyTx and ProcessProposalVerifyTx, which the SDK's own
	// handlers call. Without them, a proposer could fill a block with Cosmos
	// transactions that nobody signed or paid for, and each machine would run
	// each one in FinalizeBlock.
	VerifyPrepared func(sdk.Tx) ([]byte, error)
	VerifyProposed func([]byte) (sdk.Tx, error)
}

// Prepare packs the mempool's transactions in the order it yields them, up to
// the block's byte, unit and allocation budgets. Overflow defers a transaction
// and continues, which is the packing semantics of the SDK and of Solana.
//
// Both handlers count each budget down and do not sum charges up. The charge
// of a Cosmos transaction is the gas it declares, up to 2^64-1, and a running
// sum can wrap back under the limit. Process would then accept a block whose
// SVM half is over budget, and the FinalizeBlock plan would refuse it on every
// machine.
//
// Prepare also packs no more declared fees for a payer than its committed
// balance covers. CheckTx refuses a transaction that its payer cannot pay
// alone. But a payer that can afford two transactions one at a time cannot
// always afford both together, and the second would run unpaid. The same
// state half resolves the lookup tables of each message as this block's phase
// 2 will, so Prepare never packs a transaction that the state half would drop.
//
// Neither handler lets two transactions with one dedup key share a block.
// Only the first can be processed, and the second would hold block space for
// nothing.
//
// Both keep Solana's per-account limit over the accounts that each SVM
// transaction writes, with its lookup tables resolved as the block's phase 2
// will resolve them. Both price a message that names a table at that resolved
// cost. Prepare skips a transaction that would take an account past its share
// and keeps packing, so the bids of a hot account cannot crowd out the rest.
func (p Proposals) Prepare(ctx sdk.Context, req *abci.RequestPrepareProposal) (*abci.ResponsePrepareProposal, error) {
	budget := newBlockBudget()
	accounts := newAccountUnits()
	height := uint64(req.Height)
	ms, err := proposed(p.Next, height)
	if err != nil {
		return nil, err
	}
	owed := make(map[[32]byte]uint64)
	packed := make(map[[64]byte]bool)
	var txs [][]byte
	var size int64
	for it := p.Mempool.Select(ctx, req.Txs); it != nil; it = it.Next() {
		g, err := p.price(ms, height, it.Tx())
		if err != nil {
			continue
		}
		raw, err := p.bytes(g)
		if err != nil {
			continue
		}
		if size+int64(len(raw)) > req.MaxTxBytes || !budget.fits(g) {
			continue
		}
		if a := g.admission; a != nil {
			if packed[a.DedupKey] {
				continue
			}
			total := *a
			total.Fee += owed[a.Payer]
			if total.Fee < a.Fee || p.Stores.Admissible(ms, height, g.transaction, total, false) != nil {
				continue
			}
			if !g.counted || !accounts.add(g.units, g.writable) {
				continue
			}
			owed[a.Payer] = total.Fee
			packed[a.DedupKey] = true
		} else if _, err := p.VerifyPrepared(g.Tx); err != nil {
			// This check is last, so only a packed transaction leaves its ante
			// effects in the state that the next one is verified against.
			// Process verifies the block in the same way.
			continue
		}
		txs = append(txs, raw)
		size += int64(len(raw))
		budget.spend(g)
	}
	return &abci.ResponsePrepareProposal{Txs: txs}, nil
}

// Process rejects a proposal over either block budget, or one that carries a
// transaction that does not decode, would not be admitted, or has a signature
// that fails. It is registered apart from the mempool, so `max-txs = -1`
// cannot turn it off.
//
// Process is the gate for every budget. It rejects a block over a budget, so
// the round moves to another proposer, as Agave's replay marks such a block
// dead. FinalizeBlock enforces the same budgets per transaction as a backstop.
// It leaves a transaction over a budget unprocessed, and does not halt on a
// block that consensus already decided.
//
// Process checks the signatures last, after every cheap check, in one engine
// call. The call skips each transaction whose exact bytes already passed on
// this node, and stops at the first that fails. A bad signature refuses the
// whole proposal, as Agave's replay marks a block with one dead.
func (p Proposals) Process(ctx sdk.Context, req *abci.RequestProcessProposal) (*abci.ResponseProcessProposal, error) {
	budget := newBlockBudget()
	accounts := newAccountUnits()
	height := uint64(req.Height)
	ms, err := proposed(p.Next, height)
	if err != nil {
		return nil, err
	}
	seen := make(map[[64]byte]bool)
	var svmTxs [][]byte
	var svmIndex []int
	for i, raw := range req.Txs {
		var g gossiped
		if solana, ok := Enveloped(raw); ok {
			g, err = p.Stores.priceSVM(ms, height, solana)
		} else {
			var tx sdk.Tx
			if tx, err = p.Decode(raw); err != nil {
				return refuse(ctx, req, refusedDecode, i), nil
			}
			g, err = p.Stores.price(ms, height, tx, raw)
		}
		if err != nil {
			return refuse(ctx, req, refusedAdmission, i), nil
		}
		if !budget.fits(g) {
			return refuse(ctx, req, refusedBudget, i), nil
		}
		if a := g.admission; a != nil {
			if seen[a.DedupKey] {
				return refuse(ctx, req, refusedDedup, i), nil
			}
			seen[a.DedupKey] = true
			// A transaction whose tables do not resolve is not counted
			// against an account, because FinalizeBlock will not process it.
			if g.counted && !accounts.add(g.units, g.writable) {
				return refuse(ctx, req, refusedAccountLimit, i), nil
			}
			svmTxs = append(svmTxs, g.transaction)
			svmIndex = append(svmIndex, i)
		} else if _, err := p.VerifyProposed(raw); err != nil {
			return refuse(ctx, req, refusedCosmos, i), nil
		}
		budget.spend(g)
	}
	if len(svmTxs) > 0 {
		failed, err := ffi.ProposalSignatures(svmTxs)
		if err != nil {
			return nil, err
		}
		if failed >= 0 {
			return refuse(ctx, req, refusedSignature, svmIndex[failed]), nil
		}
	}
	return accept, nil
}

// refuse counts a refused proposal once, under the first reason it met. It
// logs one error line with the height, the block hash, the proposer's
// address, the reason and the index of the transaction. The request carries
// no round. The line never holds transaction bytes or signatures.
func refuse(ctx sdk.Context, req *abci.RequestProcessProposal, reason string, index int) *abci.ResponseProcessProposal {
	proposalsRefused.WithLabelValues(reason).Inc()
	ctx.Logger().Error("svm: proposal refused", "height", req.Height, "hash", fmt.Sprintf("%X", req.Hash),
		"proposer", fmt.Sprintf("%X", req.ProposerAddress), "reason", reason, "index", index)
	return reject
}

// blockBudget is what is left of the block's unit and allocation budgets.
// Both handlers count it down as they take each transaction.
type blockBudget struct{ units, allocated uint64 }

func newBlockBudget() blockBudget {
	units, allocated, _ := ffi.BlockLimits()
	return blockBudget{units, allocated}
}

// fits is whether g's charge is within what is left of both budgets.
func (b *blockBudget) fits(g gossiped) bool { return g.units <= b.units && g.allocated <= b.allocated }

func (b *blockBudget) spend(g gossiped) {
	b.units -= g.units
	b.allocated -= g.allocated
}

// accountUnits is the per-account limit that both handlers keep, as Agave's
// tracker keeps it. The whole cost of a transaction goes on every account it
// writes. A transaction that would take any of them past the limit adds
// nothing. Each account is checked against what it has left, so the sum cannot
// wrap.
type accountUnits struct {
	limit uint64
	spent map[[32]byte]uint64
}

func newAccountUnits() accountUnits {
	return accountUnits{limit: ffi.MaxWritableAccountUnits(), spent: make(map[[32]byte]uint64)}
}

func (s accountUnits) add(units uint64, writable [][32]byte) bool {
	if units > s.limit {
		return false
	}
	for i, k := range writable {
		if s.spent[k] > s.limit-units {
			for _, k := range writable[:i] {
				s.spent[k] -= units
			}
			return false
		}
		s.spent[k] += units
	}
	return true
}

var (
	reject = &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_REJECT}
	accept = &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_ACCEPT}
)

// price returns the charge that a transaction carries when the mempool ranked
// it, because the ranking had to charge it to sort it. Otherwise it charges the
// transaction. Prepare never costs again what Select already costed.
func (p Proposals) price(ms storetypes.MultiStore, height uint64, tx sdk.Tx) (gossiped, error) {
	if g, ok := tx.(gossiped); ok {
		return g, nil
	}
	return p.Stores.price(ms, height, tx, nil)
}

// proposed returns the context of the block at height, which a proposal for
// that block reads. It returns nil before the first block commits, when no SVM
// transaction can be admitted. A proposal for any height other than the next
// is a bug.
func proposed(next func() (storetypes.MultiStore, *Next, error), height uint64) (storetypes.MultiStore, error) {
	ms, n, err := next()
	if err != nil || ms == nil {
		return nil, err
	}
	if n.Height != height {
		return nil, fmt.Errorf("svm: a proposal for height %d, but the next block is %d", height, n.Height)
	}
	return ms, nil
}

// bytes recovers the encoding that CometBFT gossiped, so a proposed transaction
// keeps the hash its sender polls. Only a mempool that does not carry the
// original bytes pays for a new encode.
func (p Proposals) bytes(g gossiped) ([]byte, error) {
	if g.raw != nil {
		return g.raw, nil
	}
	return p.Encode(g.Tx)
}

// Passthrough is the fee-ordered mempool. CometBFT still owns the pool. Select
// prices what CometBFT handed over and yields it in fee order. The other
// methods do nothing.
type Passthrough struct {
	Decode sdk.TxDecoder
	// Stores prices a message that names a lookup table as it resolves,
	// against Next, the context of the block being proposed.
	Stores Stores
	Next   func() (storetypes.MultiStore, *Next, error)
}

var _ mempool.ExtMempool = Passthrough{}

func (Passthrough) Insert(context.Context, sdk.Tx) error { return nil }
func (Passthrough) Remove(sdk.Tx) error                  { return nil }
func (Passthrough) CountTx() int                         { return 0 }

func (Passthrough) RemoveWithReason(context.Context, sdk.Tx, mempool.RemoveReason) error {
	return nil
}

func (m Passthrough) Select(ctx context.Context, txs [][]byte) mempool.Iterator {
	return over(m.rank(sdk.UnwrapSDKContext(ctx), txs))
}

func (m Passthrough) SelectBy(ctx context.Context, txs [][]byte, keep func(sdk.Tx) bool) {
	for _, tx := range m.rank(sdk.UnwrapSDKContext(ctx), txs) {
		if !keep(tx) {
			return
		}
	}
}

// rank prices every candidate that CometBFT handed over and sorts them by rank,
// descending.
//
// rank finds the transaction of a canonical envelope with Enveloped, as
// Process does, and decodes the rest. It drops undecodable bytes and
// transactions that the engine refuses, so the handler never sees them.
// Process still rejects a proposal that contains either. The sort is stable,
// so equal bids keep arrival order. That is the only tie-break a validator can
// apply without inventing one.
func (m Passthrough) rank(ctx sdk.Context, txs [][]byte) []gossiped {
	height := uint64(ctx.BlockHeight())
	ms, err := proposed(m.Next, height)
	if err != nil {
		return nil
	}
	out := make([]gossiped, 0, len(txs))
	for _, raw := range txs {
		if solana, ok := Enveloped(raw); ok {
			if g, err := m.Stores.priceSVM(ms, height, solana); err == nil {
				g.raw = raw
				out = append(out, g)
			}
			continue
		}
		tx, err := m.Decode(raw)
		if err != nil {
			continue
		}
		g, err := m.Stores.price(ms, height, tx, raw)
		if err != nil {
			continue
		}
		out = append(out, g)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].rank > out[j].rank })
	return out
}

// price charges a transaction what both proposal handlers count against the
// block, and ranks it.
//
// An SVM transaction is charged the same seven-term cost that the block runner
// charges, parsed without signature verification. A validator that did not
// receive it by gossip has no CheckTx-time cache for it, and a cost carried in
// the Cosmos envelope would be a number from the sender. It ranks by
// `total_fee * 1e6 / (cost + 1)`, which is Agave's scheduler key with a
// substitution in the numerator.
//
// A message that names a lookup table is charged as it resolves against ms,
// the context of the block at height, which is how the block prices it. A
// table can supply the loader that a wrapper deploys through. A message whose
// tables do not resolve keeps the cost that admission saw. It writes nothing
// that the per-account limit counts, because FinalizeBlock will not process it.
//
// A Cosmos transaction is charged the gas it declares. It pays usvm, and an
// SVM transaction pays lamports. Lamports are not a bank denom, so there is no
// exchange rate, and an invented one would silently reprice block space.
// Operators are the only senders of a Cosmos transaction, so they always sort
// first.
func (s Stores) price(ms storetypes.MultiStore, height uint64, tx sdk.Tx, raw []byte) (gossiped, error) {
	if m, ok := svmMsg(tx); ok {
		g, err := s.priceSVM(ms, height, m.Transaction)
		g.Tx, g.raw = tx, raw
		return g, err
	}
	g := gossiped{Tx: tx, raw: raw}
	if gas, ok := tx.(interface{ GetGas() uint64 }); ok {
		g.units = gas.GetGas()
	}
	g.rank = math.MaxUint64
	return g, nil
}

// priceSVM is price for an SVM transaction, from its Solana bytes only.
func (s Stores) priceSVM(ms storetypes.MultiStore, height uint64, tx []byte) (gossiped, error) {
	var g gossiped
	a, err := ffi.CheckTx(tx, true, height)
	if err != nil {
		return g, err
	}
	r, counted, err := s.Resolved(ms, height, tx, a)
	if err != nil {
		return g, err
	}
	if !counted {
		r = ffi.Resolved{Units: a.Units, AllocatedBytes: a.AllocatedBytes}
	}
	g.units, g.allocated, g.writable, g.counted = r.Units, r.AllocatedBytes, r.Writable, counted
	g.rank, g.admission, g.transaction = priority(a.Fee, r.Units), &a, tx
	return g, nil
}

// priority is `R * 1e6 / (C + 1)`, saturating as Agave's is. The code has it
// twice on purpose. The measurement runs against the engine's copy, and a
// block is packed by this one.
func priority(fee, units uint64) uint64 {
	product := fee * 1_000_000
	if fee != 0 && product/1_000_000 != fee {
		product = math.MaxUint64
	}
	return product / (units + 1)
}

// gossiped is a transaction beside the bytes it arrived as and the charge
// that ranked it. Its sdk.Tx is nil for an SVM transaction that Enveloped
// found. Nothing reads it there, because Prepare reads it only for a Cosmos
// transaction.
type gossiped struct {
	sdk.Tx
	raw       []byte
	units     uint64
	allocated uint64
	rank      uint64
	// admission is the engine's verdict on an SVM transaction, and
	// transaction is the SVM transaction it was given. Both are nil on a
	// Cosmos transaction.
	admission   *ffi.Admission
	transaction []byte
	// writable are the accounts that an SVM transaction writes. counted is
	// whether the block will process it at all, as far as its lookup tables
	// decide. The per-account limit counts these.
	writable [][32]byte
	counted  bool
}

type cursor struct{ rest []gossiped }

func over(list []gossiped) mempool.Iterator {
	if len(list) == 0 {
		return nil
	}
	return &cursor{list}
}

func (c *cursor) Next() mempool.Iterator { return over(c.rest[1:]) }
func (c *cursor) Tx() sdk.Tx             { return c.rest[0] }

// MaxBlockBytes is the CometBFT block size that a validator must gossip and
// commit inside the block window. It is provisional until the limits are
// calibrated on validator-class hardware.
const MaxBlockBytes = 8_388_608

// BlockParams returns genesis' CometBFT block limits.
//
// MaxGas is -1, not the engine's unit budget. CometBFT's reap walks its list
// front to back and returns at the first transaction that would exceed MaxGas.
// With a finite MaxGas, arrival order would choose the block before the
// proposer sorts it. -1 is CometBFT's own default, and ValidateBasic accepts
// it, so the pool arrives whole and Select chooses from all of it. Prepare and
// Process still bound a block by the engine's budget.
//
// This one line is not machine-local, unlike the ordering it enables.
// Block.MaxGas is in the consensus-params hash of the block header, so it is a
// genesis parameter, movable by governance.
func BlockParams() (maxBytes, maxGas int64) {
	return MaxBlockBytes, -1
}

// EvidenceParams is the evidence window: 2,000 blocks, counted in blocks
// alone. Evidence expires only when it is past both limits, so the smallest
// duration that ValidateBasic accepts leaves the count to decide.
//
// It is also the number of blocks every machine keeps. baseapp folds the
// window into the height it tells CometBFT to retain, and svmd ships
// min-retain-blocks at the same number. CometBFT keeps the header and commit
// of every block inside the window even when it prunes the block, and these
// check a late equivocation. Like BlockParams, it is consensus. It is in the
// consensus params, and only governance can move it.
func EvidenceParams() (maxAgeNumBlocks int64, maxAgeDuration time.Duration) {
	return 2000, time.Nanosecond
}
