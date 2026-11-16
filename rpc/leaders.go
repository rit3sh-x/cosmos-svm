package rpc

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"fmt"

	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/mr-tron/base58"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// A leader is the CometBFT validator that proposes round 0 at a height, named
// by its consensus key in base58. Every answer here comes from what CometBFT
// keeps: the validator sets with their priorities, the headers and the
// validator updates. The schedule has three parts:
//   - Exact from the oldest retained height to h+1. Each step is CometBFT's
//     own IncrementProposerPriority, checked against the header's
//     ValidatorsHash. A set change is read back from the set CometBFT kept.
//   - Certain at h+2: V(h+1) with block h's updates applied, then one step,
//     as CometBFT's updateState does.
//   - Projected from h+3 to the end of the next epoch. The projection holds
//     until a block returns a validator update, which takes effect two
//     heights later.

// maxSlotLeaders is Agave's MAX_GET_SLOT_LEADERS.
const maxSlotLeaders = 5000

// slotHistoryEntries is the length of the SlotHistory sysvar. It sets how far
// back Agave's getBlockProduction reaches.
const slotHistoryEntries = 1024 * 1024

// historyFloor is the oldest slot that Agave's SlotHistory holds at height h.
func historyFloor(h uint64) uint64 {
	if h+1 > slotHistoryEntries {
		return h + 1 - slotHistoryEntries
	}
	return 0
}

// walkCheck is how many heights a walk goes between checks of its context.
// The local CometBFT client ignores the context. Without the check, a walk of
// two epochs for a client that has gone would run to the end.
const walkCheck = 4096

// cometPage is CometBFT's largest validators page.
const cometPage = 100

// consensusView is one answer's view of CometBFT: the committed height h, the
// oldest height whose round-0 proposer can be derived, the EpochSchedule
// sysvar, and this machine's own consensus key.
type consensusView struct {
	s        *Server
	ctx      context.Context
	h        int64
	oldest   int64
	schedule uiEpochSchedule
	self     string

	// page holds the last headers read, those of [base, base+len(page)).
	page []cmttypes.Header
	base int64
}

func (s *Server) consensus(ctx context.Context) (*consensusView, error) {
	if s.node == nil {
		return nil, &rpcError{Code: codeInternal, Message: "no consensus client"}
	}
	ms, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	schedule, err := s.epochSchedule(ms)
	if err != nil {
		return nil, err
	}
	status, err := s.node.Status(ctx)
	if err != nil {
		return nil, internal(err)
	}
	// The walk starts from V(oldest-1). CometBFT prunes the sets below the
	// block store's base and keeps the base's own set. So the oldest derivable
	// proposer is at the height after the base. Genesis is the exception. Its
	// set before height 1 is every validator at priority zero.
	oldest := max(status.SyncInfo.EarliestBlockHeight, 1)
	if oldest > 1 {
		oldest++
	}
	return &consensusView{s: s, ctx: ctx, h: int64(h), oldest: oldest, schedule: schedule,
		self: selfKey(status)}, nil
}

func internal(err error) error { return &rpcError{Code: codeInternal, Message: err.Error()} }

// epochSchedule reads the EpochSchedule sysvar, which genesis writes once.
// Warmup is false, so an epoch is a slot divided by slots per epoch.
func (s *Server) epochSchedule(ms storetypes.MultiStore) (uiEpochSchedule, error) {
	a, err := ffi.DecodeAccount(s.app.Stores.GetAccount(ms, svm.EpochScheduleID))
	e, ok := decodeEpochSchedule(a.Data)
	if err != nil || !ok {
		return uiEpochSchedule{}, &rpcError{Code: codeInternal, Message: "no EpochSchedule sysvar"}
	}
	return e, nil
}

// epoch is EpochSchedule::get_epoch with warmup off.
func (e uiEpochSchedule) epoch(slot uint64) uint64 { return slot / e.SlotsPerEpoch }

// slotIndex is a slot's offset into its epoch, the second half of
// EpochSchedule::get_epoch_and_slot_index with warmup off.
func (e uiEpochSchedule) slotIndex(slot uint64) uint64 { return slot % e.SlotsPerEpoch }

// firstSlot is EpochSchedule::get_first_slot_in_epoch with warmup off. Agave
// saturates. This wraps, so a caller that can reach 2^64 guards it.
func (e uiEpochSchedule) firstSlot(epoch uint64) uint64 { return epoch * e.SlotsPerEpoch }

// last returns the highest slot the schedule reaches. That is the last slot
// of the epoch after the committed height's epoch.
func (v *consensusView) last() int64 {
	return int64(v.schedule.firstSlot(v.epochOf(uint64(v.h))+2)) - 1
}

// set returns V(height) as CometBFT kept it, read across all pages.
func (v *consensusView) set(height int64) (*cmttypes.ValidatorSet, error) {
	var vals []*cmttypes.Validator
	perPage := cometPage
	for page := 1; ; page++ {
		res, err := v.s.node.Validators(v.ctx, &height, &page, &perPage)
		if err != nil {
			return nil, internal(err)
		}
		vals = append(vals, res.Validators...)
		if len(vals) >= res.Total || len(res.Validators) == 0 {
			break
		}
	}
	return (&cmttypes.ValidatorSet{Validators: vals}).Copy(), nil
}

// header returns the header at height. On a miss it reads the twenty headers
// from height up, as many as CometBFT's BlockchainInfo returns at once, into
// the one page the view keeps. A walk goes up one height at a time, so it
// holds twenty headers, not its whole range.
func (v *consensusView) header(height int64) (cmttypes.Header, error) {
	if i := height - v.base; i >= 0 && i < int64(len(v.page)) {
		return v.page[i], nil
	}
	if err := v.ctx.Err(); err != nil {
		return cmttypes.Header{}, err
	}
	top := min(height+19, v.h)
	if top < height {
		return cmttypes.Header{}, &rpcError{Code: codeInternal, Message: fmt.Sprintf("no header at height %d", height)}
	}
	res, err := v.s.node.BlockchainInfo(v.ctx, height, top)
	if err != nil {
		return cmttypes.Header{}, internal(err)
	}
	n := int(top - height + 1)
	if cap(v.page) < n {
		v.page = make([]cmttypes.Header, n)
	}
	v.page = v.page[:n]
	clear(v.page)
	v.base = height
	for _, m := range res.BlockMetas {
		if m != nil && m.Header.Height >= height && m.Header.Height <= top {
			v.page[m.Header.Height-height] = m.Header
		}
	}
	for i := range v.page {
		if v.page[i].Height != height+int64(i) {
			v.page = v.page[:0]
			return cmttypes.Header{}, &rpcError{Code: codeInternal, Message: fmt.Sprintf("no header at height %d", height+int64(i))}
		}
	}
	return v.page[0], nil
}

// proposers returns the round-0 proposers of [lo, hi], with oldest <= lo and
// hi <= last().
func (v *consensusView) proposers(lo, hi int64) ([]*cmttypes.Validator, error) {
	out := make([]*cmttypes.Validator, 0, hi-lo+1)
	var set *cmttypes.ValidatorSet
	var err error
	if lo <= v.h+1 {
		if set, err = v.before(lo); err != nil {
			return nil, err
		}
		top := min(hi, v.h+1)
		prev := set.Hash()
		for height := lo; height <= top; height++ {
			if height%walkCheck == 0 && v.ctx.Err() != nil {
				return nil, v.ctx.Err()
			}
			// V(height)'s hash is in its own header, and V(h+1)'s in the
			// committed header's NextValidatorsHash. Before height 1 there
			// is no header to check against.
			want := prev
			if v.h > 0 {
				hd, err := v.header(min(height, v.h))
				if err != nil {
					return nil, err
				}
				if want = hd.ValidatorsHash; height > v.h {
					want = hd.NextValidatorsHash
				}
			}
			if bytes.Equal(want, prev) {
				set.IncrementProposerPriority(1)
			} else if set, err = v.changed(set, height); err != nil {
				return nil, err
			}
			prev = want
			out = append(out, set.GetProposer())
		}
	} else if set, err = v.set(v.h + 1); err != nil {
		return nil, err
	}
	if hi <= v.h+1 {
		return out, nil
	}
	if err := v.following(set); err != nil {
		return nil, err
	}
	for height := v.h + 2; height <= hi; height++ {
		if height%walkCheck == 0 && v.ctx.Err() != nil {
			return nil, v.ctx.Err()
		}
		if height > v.h+2 {
			set.IncrementProposerPriority(1)
		}
		if height >= lo {
			out = append(out, set.GetProposer())
		}
	}
	return out, nil
}

// following turns V(h+1) into V(h+2). It applies block h's updates, then
// steps once, as CometBFT's updateState does.
func (v *consensusView) following(set *cmttypes.ValidatorSet) error {
	if v.h > 0 {
		res, err := v.s.node.BlockResults(v.ctx, &v.h)
		if err != nil {
			return internal(err)
		}
		updates, err := cmttypes.PB2TM.ValidatorUpdates(res.ValidatorUpdates)
		if err != nil {
			return internal(err)
		}
		if len(updates) > 0 {
			if err := set.UpdateWithChangeSet(updates); err != nil {
				return internal(err)
			}
		}
	}
	set.IncrementProposerPriority(1)
	return nil
}

// before returns V(lo-1), the set that the walk to lo starts from. Before
// height 1 it is the genesis validators at priority zero. NewValidatorSet
// steps that set once to make V(1).
func (v *consensusView) before(lo int64) (*cmttypes.ValidatorSet, error) {
	if lo > 1 {
		return v.set(lo - 1)
	}
	set, err := v.set(1)
	if err != nil {
		return nil, err
	}
	for _, val := range set.Validators {
		val.ProposerPriority = 0
	}
	set.Proposer = nil
	return set, nil
}

// changed returns V(height) when the set changed at height. It reads the
// change from the set CometBFT kept and applies it as updateState does. The
// resulting priorities must equal the kept ones exactly.
func (v *consensusView) changed(prev *cmttypes.ValidatorSet, height int64) (*cmttypes.ValidatorSet, error) {
	kept, err := v.set(height)
	if err != nil {
		return nil, err
	}
	var changes []*cmttypes.Validator
	for _, val := range kept.Validators {
		if _, old := prev.GetByAddress(val.Address); old == nil || old.VotingPower != val.VotingPower {
			changes = append(changes, cmttypes.NewValidator(val.PubKey, val.VotingPower))
		}
	}
	for _, old := range prev.Validators {
		if _, val := kept.GetByAddress(old.Address); val == nil {
			changes = append(changes, cmttypes.NewValidator(old.PubKey, 0))
		}
	}
	next := prev.Copy()
	if err := next.UpdateWithChangeSet(changes); err != nil {
		return nil, internal(err)
	}
	next.IncrementProposerPriority(1)
	for i, val := range next.Validators {
		k := kept.Validators[i]
		if !bytes.Equal(val.Address, k.Address) || val.ProposerPriority != k.ProposerPriority {
			return nil, &rpcError{Code: codeInternal, Message: fmt.Sprintf("validator set at height %d does not follow from its parent", height)}
		}
	}
	return next, nil
}

func identity(val *cmttypes.Validator) string { return base58.Encode(val.PubKey.Bytes()) }

func (v *consensusView) epochOf(slot uint64) uint64 { return v.schedule.epoch(slot) }

// unavailable is getSlotLeaders' refusal.
func unavailable(epoch uint64) error {
	return invalidParams("Invalid slot range: leader schedule for epoch %d is unavailable", epoch)
}

func getSlotLeaders(ctx context.Context, s *Server, params []jsontext.Value) (any, error) {
	var start, limit uint64
	if err := param(params, 0, &start); err != nil {
		return nil, err
	}
	if err := param(params, 1, &limit); err != nil {
		return nil, err
	}
	if limit > maxSlotLeaders {
		return nil, invalidParams("Invalid limit; max %d", maxSlotLeaders)
	}
	v, err := s.consensus(ctx)
	if err != nil {
		return nil, err
	}
	if limit == 0 {
		return []string{}, nil
	}
	end := start + limit - 1
	if int64(start) < v.oldest {
		return nil, unavailable(v.epochOf(start))
	}
	if end > uint64(v.last()) {
		return nil, unavailable(max(v.epochOf(start), v.epochOf(uint64(v.last())+1)))
	}
	leaders, err := v.proposers(int64(start), int64(end))
	if err != nil {
		return nil, err
	}
	out := make([]string, len(leaders))
	for i, l := range leaders {
		out[i] = identity(l)
	}
	return out, nil
}

// leaderScheduleConfig is Agave's RpcLeaderScheduleConfig.
type leaderScheduleConfig struct {
	Identity *string `json:"identity"`
}

// getLeaderSchedule groups the computable slots of an epoch by leader, as slot
// indices from the epoch's first slot. It answers null for an epoch with no
// computable slot. The first parameter is a slot or the config, as in Agave's
// untagged wrapper.
func getLeaderSchedule(ctx context.Context, s *Server, params []jsontext.Value) (any, error) {
	var slot *uint64
	var cfg leaderScheduleConfig
	if len(params) > 0 && bytes.HasPrefix(bytes.TrimSpace(params[0]), []byte("{")) {
		if err := param(params, 0, &cfg); err != nil {
			return nil, err
		}
	} else {
		if err := param(params, 0, &slot); err != nil {
			return nil, err
		}
		if err := param(params, 1, &cfg); err != nil {
			return nil, err
		}
	}
	if cfg.Identity != nil {
		if _, err := decodeKey(*cfg.Identity); err != nil {
			return nil, err
		}
	}
	v, err := s.consensus(ctx)
	if err != nil {
		return nil, err
	}
	slotAt := uint64(v.h)
	if slot != nil {
		slotAt = *slot
	}
	first := int64(v.schedule.firstSlot(v.epochOf(slotAt)))
	lo, hi := max(first, v.oldest), first+int64(v.schedule.SlotsPerEpoch)-1
	if hi > v.last() || lo > hi {
		return nil, nil
	}
	leaders, err := v.proposers(lo, hi)
	if err != nil {
		return nil, err
	}
	schedule := map[string][]int64{}
	for i, l := range leaders {
		id := identity(l)
		if cfg.Identity != nil && id != *cfg.Identity {
			continue
		}
		schedule[id] = append(schedule[id], lo+int64(i)-first)
	}
	return schedule, nil
}

// getSlotLeader returns the validator that proposed the committed block. That
// can differ from the round-0 leader when the block took a later round. It is
// Agave's bank.leader_id().
func getSlotLeader(ctx context.Context, s *Server, params []jsontext.Value) (any, error) {
	v, err := s.consensus(ctx)
	if err != nil {
		return nil, err
	}
	if err := contextAt(params, 0, uint64(v.h)); err != nil {
		return nil, err
	}
	id, err := v.proposerOf(v.h)
	if err != nil {
		return nil, err
	}
	return id, nil
}

// proposerOf returns the identity of the validator whose address is in the
// header's ProposerAddress.
func (v *consensusView) proposerOf(height int64) (string, error) {
	hd, err := v.header(height)
	if err != nil {
		return "", err
	}
	set, err := v.set(height)
	if err != nil {
		return "", err
	}
	_, val := set.GetByAddress(hd.ProposerAddress)
	if val == nil {
		return "", &rpcError{Code: codeInternal, Message: fmt.Sprintf("proposer of height %d is not in its set", height)}
	}
	return identity(val), nil
}

// blockProductionConfig is Agave's RpcBlockProductionConfig.
type blockProductionConfig struct {
	Identity *string `json:"identity"`
	Range    *struct {
		FirstSlot uint64  `json:"firstSlot"`
		LastSlot  *uint64 `json:"lastSlot"`
	} `json:"range"`
}

// getBlockProduction counts, for each leader, its scheduled slots and the
// scheduled slots whose block it proposed itself. A block from a later round
// counts as a skip, so blocksProduced <= leaderSlots holds, as on Agave. The
// oldest derivable height takes the place of the oldest slot in Agave's
// SlotHistory.
func getBlockProduction(ctx context.Context, s *Server, params []jsontext.Value) (any, error) {
	var cfg blockProductionConfig
	if err := param(params, 0, &cfg); err != nil {
		return nil, err
	}
	if cfg.Identity != nil {
		if _, err := decodeKey(*cfg.Identity); err != nil {
			return nil, err
		}
	}
	v, err := s.consensus(ctx)
	if err != nil {
		return nil, err
	}
	h := uint64(v.h)
	// Go back no further than Agave's SlotHistory. Without this limit, a
	// machine that never prunes would walk every height it keeps.
	oldest := max(uint64(v.oldest), historyFloor(h))
	first, last := max(v.schedule.firstSlot(v.epochOf(h)), oldest), h
	if cfg.Range != nil {
		first = cfg.Range.FirstSlot
		if cfg.Range.LastSlot != nil {
			last = *cfg.Range.LastSlot
		}
		if last < first {
			return nil, invalidParams("lastSlot, %d, cannot be less than firstSlot, %d", last, first)
		}
	}
	if first < oldest {
		return nil, invalidParams("firstSlot, %d, is too small; min %d", first, oldest)
	}
	if last > h {
		return nil, invalidParams("lastSlot, %d, is too large; max %d", last, h)
	}
	leaders, err := v.proposers(int64(first), int64(last))
	if err != nil {
		return nil, err
	}
	production := map[string][2]uint64{}
	for i, l := range leaders {
		id := identity(l)
		if cfg.Identity != nil && id != *cfg.Identity {
			continue
		}
		hd, err := v.header(int64(first) + int64(i))
		if err != nil {
			return nil, err
		}
		e := production[id]
		e[0]++
		if bytes.Equal(hd.ProposerAddress, l.Address) {
			e[1]++
		}
		production[id] = e
	}
	return at(h, map[string]any{
		"byIdentity": production,
		"range":      map[string]uint64{"firstSlot": first, "lastSlot": last},
	}), nil
}

func getEpochSchedule(_ context.Context, s *Server, _ []jsontext.Value) (any, error) {
	ms, _, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	return s.epochSchedule(ms)
}

// getEpochInfo is exact arithmetic on the height, because a slot is a height.
// transactionCount is the stored counter, 0 when it is unset.
func getEpochInfo(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	ms, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if err := contextAt(params, 0, h); err != nil {
		return nil, err
	}
	schedule, err := s.epochSchedule(ms)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"absoluteSlot":     h,
		"blockHeight":      h,
		"epoch":            schedule.epoch(h),
		"slotIndex":        schedule.slotIndex(h),
		"slotsInEpoch":     schedule.SlotsPerEpoch,
		"transactionCount": s.app.Stores.TransactionCount(ms),
	}, nil
}

// getIdentity returns this machine's consensus key, which is its leader
// identity when it is a validator.
func getIdentity(ctx context.Context, s *Server, _ []jsontext.Value) (any, error) {
	if s.node == nil {
		return nil, &rpcError{Code: codeInternal, Message: "no consensus client"}
	}
	status, err := s.node.Status(ctx)
	if err != nil {
		return nil, internal(err)
	}
	return map[string]string{"identity": selfKey(status)}, nil
}

// selfKey is this machine's consensus key from CometBFT's status. Every
// machine has one, a validator or not.
func selfKey(status *coretypes.ResultStatus) string {
	if status.ValidatorInfo.PubKey == nil {
		return ""
	}
	return base58.Encode(status.ValidatorInfo.PubKey.Bytes())
}

// contactInfo is Agave's RpcContactInfo.
type contactInfo struct {
	Pubkey          string  `json:"pubkey"`
	Gossip          *string `json:"gossip"`
	TVU             *string `json:"tvu"`
	TPU             *string `json:"tpu"`
	TPUQuic         *string `json:"tpuQuic"`
	TPUForwards     *string `json:"tpuForwards"`
	TPUForwardsQuic *string `json:"tpuForwardsQuic"`
	TPUVote         *string `json:"tpuVote"`
	ServeRepair     *string `json:"serveRepair"`
	RPC             *string `json:"rpc"`
	Pubsub          *string `json:"pubsub"`
	Version         *string `json:"version"`
	ClientID        *string `json:"clientId"`
	FeatureSet      *uint32 `json:"featureSet"`
	ShredVersion    *uint16 `json:"shredVersion"`
}

// getClusterNodes lists every validator of the sets at h+1 and h+2, which is
// every identity the certain schedule names, plus this machine. A validator's
// own ports are not on the chain, so each field except its key is null. This
// machine's entry carries its own RPC addresses, version and feature set.
//
// tpuQuic and tpuForwardsQuic are this machine's QUIC address on every entry,
// because this machine is the ingress for every leader. They are null while
// the QUIC port is off or its child is not ready. tpu and tpuForwards are
// always null, as on Agave.
func getClusterNodes(ctx context.Context, s *Server, _ []jsontext.Value) (any, error) {
	v, err := s.consensus(ctx)
	if err != nil {
		return nil, err
	}
	set, err := v.set(v.h + 1)
	if err != nil {
		return nil, err
	}
	next := set.Copy()
	if err := v.following(next); err != nil {
		return nil, err
	}
	var nodes []contactInfo
	quic := s.quicAddress()
	seen := map[string]bool{}
	add := func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		n := contactInfo{Pubkey: id, TPUQuic: quic, TPUForwardsQuic: quic}
		if id == v.self {
			version, features := apiVersion, ffi.FeatureSetID()
			n.RPC, n.Pubsub, n.Version, n.FeatureSet = s.addrs.rpc, s.addrs.ws, &version, &features
		}
		nodes = append(nodes, n)
	}
	for _, val := range append(set.Validators, next.Validators...) {
		add(identity(val))
	}
	add(v.self)
	return nodes, nil
}
