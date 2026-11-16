package rpc

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"math"
)

// A method whose concept this chain excludes answers what Agave's own code
// answers when that concept is absent. This chain has no vote accounts, no
// stake and no inflation. It does not include the Vote and Stake programs, and
// lamports have no issuance. No answer reports CometBFT voting power as stake.
// getStakeMinimumDelegation stays unregistered, because its value is a rule of
// the Stake program.

// maxInflationRewardAddresses is Agave's MAX_GET_INFLATION_REWARD_ADDRESSES.
const maxInflationRewardAddresses = 32

// getVoteAccounts returns two empty lists. It parses votePubkey first, so a bad
// key gets Agave's refusal.
func getVoteAccounts(_ context.Context, _ *Server, params []jsontext.Value) (any, error) {
	var cfg struct {
		VotePubkey *string `json:"votePubkey"`
	}
	if err := param(params, 0, &cfg); err != nil {
		return nil, err
	}
	if cfg.VotePubkey != nil {
		if _, err := decodeKey(*cfg.VotePubkey); err != nil {
			return nil, err
		}
	}
	return map[string][]any{"current": {}, "delinquent": {}}, nil
}

// getInflationGovernor returns the disabled schedule that Agave serves on a
// genesis made with --inflation none.
func getInflationGovernor(context.Context, *Server, []jsontext.Value) (any, error) {
	return map[string]float64{"initial": 0, "terminal": 0, "taper": 0, "foundation": 0, "foundationTerm": 0}, nil
}

// getInflationRate returns zero rates for the current epoch.
func getInflationRate(_ context.Context, s *Server, _ []jsontext.Value) (any, error) {
	ms, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	schedule, err := s.epochSchedule(ms)
	if err != nil {
		return nil, err
	}
	return map[string]any{"total": 0.0, "validator": 0.0, "foundation": 0.0, "epoch": schedule.epoch(h)}, nil
}

// getInflationReward ports Agave's steps in this order:
//   - check the address count and the keys
//   - pick the epoch, by default the previous one
//   - require the boundary block, the first slot of the next epoch, to be
//     retained and committed
//   - return null for every address, because no block here carries a reward
func getInflationReward(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	var addresses []string
	if err := param(params, 0, &addresses); err != nil {
		return nil, err
	}
	if len(addresses) > maxInflationRewardAddresses {
		return nil, invalidParams("Too many inputs provided; max %d", maxInflationRewardAddresses)
	}
	for _, a := range addresses {
		if _, err := decodeKey(a); err != nil {
			return nil, err
		}
	}
	var cfg struct {
		contextConfig
		Epoch *uint64 `json:"epoch"`
	}
	if err := param(params, 1, &cfg); err != nil {
		return nil, err
	}
	ms, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	schedule, err := s.epochSchedule(ms)
	if err != nil {
		return nil, err
	}
	first := s.history().FirstAvailableBlock()
	var epoch uint64
	if cfg.Epoch != nil {
		epoch = *cfg.Epoch
	} else {
		if err := reached(cfg.MinContextSlot, h); err != nil {
			return nil, err
		}
		if epoch = schedule.epoch(h); epoch > 0 {
			epoch--
		}
	}
	boundary := uint64(math.MaxUint64)
	if epoch < math.MaxUint64/schedule.SlotsPerEpoch-1 {
		boundary = schedule.firstSlot(epoch + 1)
	}
	if boundary < first {
		return nil, &rpcError{Code: blockCleanedUp, Message: fmt.Sprintf(
			"Block %d cleaned up, does not exist on node. First available block: %d", boundary, first)}
	}
	committed := s.committed()
	if boundary > committed {
		return nil, &rpcError{Code: codeBlockNotAvailable, Message: fmt.Sprintf("Block not available for slot %d", boundary)}
	}
	return make([]any, len(addresses)), nil
}
