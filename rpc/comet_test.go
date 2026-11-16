package rpc

import (
	"context"
	"errors"
	"fmt"
	"sync"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/crypto"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	cmttypes "github.com/cometbft/cometbft/types"

	"github.com/cosmos/cosmos-sdk/client"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// comet stands in for the in-process CometBFT. It keeps each height's validator
// set with its priorities, and each block's header and validator updates. It
// advances the sets as CometBFT's updateState does, with CometBFT's types.
// V(1) is NewValidatorSet and V(2) is V(1) stepped once. V(H+2) is V(H+1) with
// block H's updates and one IncrementProposerPriority. BroadcastTxSync runs
// CheckTx, as the mempool does, and nothing after.
type comet struct {
	client.CometRPC
	c       *tc.Chain
	self    crypto.PubKey
	base    int64
	height  int64
	sets    map[int64]*cmttypes.ValidatorSet
	headers map[int64]cmttypes.Header
	updates map[int64][]abci.ValidatorUpdate

	// cache and admitted are the parts of the pool that a send can observe.
	// cache is CometBFT's cache. It refuses an exact duplicate and forgets a
	// transaction that CheckTx refused. admitted holds what CheckTx admitted.
	poolMu   sync.Mutex
	cache    map[string]bool
	admitted [][]byte
}

func newComet(c *tc.Chain, self crypto.PubKey, vals ...*cmttypes.Validator) *comet {
	first := cmttypes.NewValidatorSet(vals)
	return &comet{c: c, self: self, base: 1,
		sets:    map[int64]*cmttypes.ValidatorSet{1: first, 2: first.CopyIncrementProposerPriority(1)},
		headers: map[int64]cmttypes.Header{},
		updates: map[int64][]abci.ValidatorUpdate{},
	}
}

// commit records the next block, proposed in round, with the validator updates
// it returns.
func (m *comet) commit(round int32, updates ...abci.ValidatorUpdate) {
	m.height++
	h := m.height
	set := m.sets[h]
	proposer := set.GetProposer()
	if round > 0 {
		proposer = set.CopyIncrementProposerPriority(round).GetProposer()
	}
	m.headers[h] = cmttypes.Header{Height: h, ValidatorsHash: set.Hash(),
		NextValidatorsHash: m.sets[h+1].Hash(), ProposerAddress: proposer.Address}
	changes, err := cmttypes.PB2TM.ValidatorUpdates(updates)
	if err != nil {
		panic(err)
	}
	next := m.sets[h+1].Copy()
	if len(changes) > 0 {
		if err := next.UpdateWithChangeSet(changes); err != nil {
			panic(err)
		}
	}
	next.IncrementProposerPriority(1)
	m.sets[h+2] = next
	m.updates[h] = updates
}

// block commits an empty app block and records it.
func (m *comet) block(round int32, updates ...abci.ValidatorUpdate) {
	m.c.Block()
	m.commit(round, updates...)
}

// catchUp records each block that the app committed outside block as a round-0
// block with no updates. The transcript setup commits through the test chain alone.
func (m *comet) catchUp() {
	for m.height < m.c.Height {
		m.commit(0)
	}
}

// prune acts as CometBFT pruning its block store and state store below base.
func (m *comet) prune(base int64) {
	m.catchUp()
	for h := m.base; h < base; h++ {
		delete(m.sets, h)
		delete(m.headers, h)
	}
	m.base = base
}

// proposer is the round-0 proposer CometBFT used, or will use, at height h.
func (m *comet) proposer(h int64) *cmttypes.Validator { return m.sets[h].GetProposer() }

func (m *comet) Validators(_ context.Context, height *int64, page, perPage *int) (*coretypes.ResultValidators, error) {
	m.catchUp()
	h := *height
	if h > m.height+1 {
		return nil, fmt.Errorf("height %d must be less than or equal to the current blockchain height %d", h, m.height+1)
	}
	set, ok := m.sets[h]
	if !ok {
		return nil, errors.New("value retrieved from db is empty")
	}
	vals := set.Copy().Validators
	skip := (*page - 1) * *perPage
	end := min(skip+*perPage, len(vals))
	return &coretypes.ResultValidators{BlockHeight: h, Validators: vals[skip:end], Count: end - skip, Total: len(vals)}, nil
}

// BlockchainInfo bounds its range as CometBFT's filterMinMax does. It returns
// at most twenty metas, highest first.
func (m *comet) BlockchainInfo(_ context.Context, lo, hi int64) (*coretypes.ResultBlockchainInfo, error) {
	m.catchUp()
	hi = min(hi, m.height)
	lo = max(lo, m.base, hi-19)
	if lo > hi {
		return nil, fmt.Errorf("min height %d can't be greater than max height %d", lo, hi)
	}
	var metas []*cmttypes.BlockMeta
	for h := hi; h >= lo; h-- {
		metas = append(metas, &cmttypes.BlockMeta{Header: m.headers[h]})
	}
	return &coretypes.ResultBlockchainInfo{LastHeight: m.height, BlockMetas: metas}, nil
}

func (m *comet) BlockResults(_ context.Context, height *int64) (*coretypes.ResultBlockResults, error) {
	m.catchUp()
	return &coretypes.ResultBlockResults{Height: *height, ValidatorUpdates: m.updates[*height]}, nil
}

func (m *comet) Status(context.Context) (*coretypes.ResultStatus, error) {
	m.catchUp()
	return &coretypes.ResultStatus{
		SyncInfo:      coretypes.SyncInfo{EarliestBlockHeight: m.base, LatestBlockHeight: m.height},
		ValidatorInfo: coretypes.ValidatorInfo{Address: m.self.Address(), PubKey: m.self},
	}, nil
}

func (m *comet) BroadcastTxSync(_ context.Context, tx cmttypes.Tx) (*coretypes.ResultBroadcastTx, error) {
	m.poolMu.Lock()
	defer m.poolMu.Unlock()
	if m.cache == nil {
		m.cache = map[string]bool{}
	}
	if m.cache[string(tx)] {
		return nil, errors.New("error on broadcastTxSync: tx already exists in cache")
	}
	m.cache[string(tx)] = true
	res := m.c.Check(tx, false)
	if res.Code == 0 {
		m.admitted = append(m.admitted, m.c.Unwrap(tx))
	} else {
		delete(m.cache, string(tx))
	}
	return &coretypes.ResultBroadcastTx{Code: res.Code, Log: res.Log}, nil
}

// pool returns the Solana transactions that CheckTx admitted, in order.
func (m *comet) pool() [][]byte {
	m.poolMu.Lock()
	defer m.poolMu.Unlock()
	return append([][]byte(nil), m.admitted...)
}
