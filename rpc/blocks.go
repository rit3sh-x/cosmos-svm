package rpc

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
)

// The history and machine-status methods. The history index holds every
// height without a gap, and slot is height. So Agave's blockstore iteration
// is a range here. This chain has no BigTable, so Agave's BigTable branches
// have no counterpart here.

// maxBlocksRange is Agave's MAX_GET_CONFIRMED_BLOCKS_RANGE.
const maxBlocksRange = 500_000

// codeNoSnapshot is Agave's JSON_RPC_SERVER_ERROR_NO_SNAPSHOT.
const codeNoSnapshot = -32008

// committed returns the height that every method here reads. getSlot returns
// this height. The root, the confirmed height and the finalized height are all
// this height. It is not s.notified, because s.notified is late by up to one
// Follow tick, and a client must be able to read the block of a slot it got.
func (s *Server) committed() uint64 { return s.app.CommittedHeight() }

// heights returns first..last, or an empty list. The h >= first test stops the
// loop when h wraps past the largest uint64.
func heights(first, last uint64) []uint64 {
	out := []uint64{}
	for h := first; h <= last && h >= first; h++ {
		out = append(out, h)
	}
	return out
}

// getBlocks copies Agave v4.3.0. The second parameter is an end slot or a
// config (an untagged enum). A config in the third place replaces one in the
// second.
func getBlocks(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	var start uint64
	if err := param(params, 0, &start); err != nil {
		return nil, err
	}
	var end *uint64
	var cfg contextConfig
	if len(params) > 1 {
		switch raw := bytes.TrimSpace(params[1]); {
		case string(raw) == "null":
		case len(raw) > 0 && raw[0] == '{':
			if err := json.Unmarshal(raw, &cfg, agaveInput); err != nil {
				return nil, invalidParams("Invalid params: data did not match any variant of untagged enum RpcBlocksConfigWrapper.")
			}
		default:
			var e uint64
			if err := json.Unmarshal(raw, &e, agaveInput); err != nil {
				return nil, invalidParams("Invalid params: data did not match any variant of untagged enum RpcBlocksConfigWrapper.")
			}
			end = &e
		}
	}
	if len(params) > 2 && string(bytes.TrimSpace(params[2])) != "null" {
		cfg = contextConfig{}
		if err := param(params, 2, &cfg); err != nil {
			return nil, err
		}
	}
	if err := atLeastConfirmed(cfg.Commitment); err != nil {
		return nil, err
	}
	committed := s.committed()
	if err := reached(cfg.MinContextSlot, committed); err != nil {
		return nil, err
	}
	last, err := blockRange(start, end, committed)
	if err != nil {
		return nil, err
	}
	if last < start {
		return []uint64{}, nil
	}
	return heights(max(start, s.history().FirstAvailableBlock()), last), nil
}

// blockRange returns the last height of a getBlocks from start. The range cap
// applies after the end is clamped to the committed height.
func blockRange(start uint64, end *uint64, committed uint64) (uint64, error) {
	last := start + maxBlocksRange
	if last < start {
		last = ^uint64(0)
	}
	if end != nil {
		last = *end
	}
	last = min(last, committed)
	if last >= start && last-start > maxBlocksRange {
		return 0, invalidParams("Slot range too large; max %d", maxBlocksRange)
	}
	return last, nil
}

// getBlocksWithLimit checks the limit before minContextSlot.
func getBlocksWithLimit(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	var start, limit uint64
	if err := param(params, 0, &start); err != nil {
		return nil, err
	}
	if err := param(params, 1, &limit); err != nil {
		return nil, err
	}
	var cfg contextConfig
	if err := param(params, 2, &cfg); err != nil {
		return nil, err
	}
	if err := atLeastConfirmed(cfg.Commitment); err != nil {
		return nil, err
	}
	if limit > maxBlocksRange {
		return nil, invalidParams("Limit too large; max %d", maxBlocksRange)
	}
	committed := s.committed()
	if err := reached(cfg.MinContextSlot, committed); err != nil {
		return nil, err
	}
	first := max(start, s.history().FirstAvailableBlock())
	if limit == 0 || first > committed {
		return []uint64{}, nil
	}
	return heights(first, min(first+limit-1, committed)), nil
}

// getBlockTime returns the genesis time for slot 0 and the block time for a
// retained height. A height above the committed height is error -32004. A
// height below the first available block is error -32001.
func getBlockTime(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	var height uint64
	if err := param(params, 0, &height); err != nil {
		return nil, err
	}
	if height == 0 {
		return s.genesisTime, nil
	}
	if height > s.committed() {
		return nil, &rpcError{Code: codeBlockNotAvailable, Message: fmt.Sprintf("Block not available for slot %d", height)}
	}
	if first := s.history().FirstAvailableBlock(); height < first {
		return nil, &rpcError{Code: blockCleanedUp, Message: fmt.Sprintf(
			"Block %d cleaned up, does not exist on node. First available block: %d", height, first)}
	}
	t, ok, err := s.history().BlockTime(height)
	if err != nil {
		return nil, err
	}
	if !ok {
		// getBlock reads the time from the block's first record, so this
		// method does too.
		records, err := s.history().AtHeight(height)
		if err != nil || len(records) == 0 {
			return nil, err
		}
		return records[0].BlockTime, nil
	}
	return t, nil
}

// minimumLedgerSlot returns the first available block, or Invalid request
// while the history holds no block.
func minimumLedgerSlot(_ context.Context, s *Server, _ []jsontext.Value) (any, error) {
	first := s.history().FirstAvailableBlock()
	if first > s.committed() {
		return nil, &rpcError{Code: codeInvalidRequest, Message: "Invalid request"}
	}
	return first, nil
}

// getHighestSnapshotSlot returns Agave's answer for a machine with no snapshot
// archive. A CometBFT state-sync snapshot is not a snapshot archive.
func getHighestSnapshotSlot(context.Context, *Server, []jsontext.Value) (any, error) {
	return nil, &rpcError{Code: codeNoSnapshot, Message: "No snapshot"}
}

// getMaxShredInsertSlot returns the highest height whose block has reached
// this machine. receivedSignature uses the same event.
func getMaxShredInsertSlot(_ context.Context, s *Server, _ []jsontext.Value) (any, error) {
	return s.app.Feed.Reached(), nil
}

// getMaxRetransmitSlot returns 0. Only the Turbine retransmit stage raises
// it, and this chain has no Turbine.
func getMaxRetransmitSlot(context.Context, *Server, []jsontext.Value) (any, error) {
	return uint64(0), nil
}

// getBlockCommitment returns Agave's answer with no vote accounts. No block
// has an entry, and the total stake is zero.
func getBlockCommitment(_ context.Context, _ *Server, params []jsontext.Value) (any, error) {
	var height uint64
	if err := param(params, 0, &height); err != nil {
		return nil, err
	}
	return map[string]any{"commitment": nil, "totalStake": 0}, nil
}

// getAgGenesisCert returns null, because only the Agave runtime writes the
// Alpenglow genesis certificate account.
func getAgGenesisCert(context.Context, *Server, []jsontext.Value) (any, error) {
	return nil, nil
}
