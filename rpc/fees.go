package rpc

import (
	"context"
	"encoding/json/jsontext"
)

// recentPrioritizationBlocks is Agave's window of 150 blocks
// (`PrioritizationFeeCache::MAX_NUM_RECENT_BLOCKS`), a block count and not a
// duration. This chain keeps the count for its own block interval. The
// response carries slot numbers, so a caller who wants a shorter window can
// cut one. Fewer samples only take information away.
const recentPrioritizationBlocks = 150

// maxPrioritizationAddresses is the account-lock limit of a transaction, the
// most accounts a caller can usefully name.
const maxPrioritizationAddresses = 128

// prioritizationFee is one entry of Agave's RpcPrioritizationFee.
type prioritizationFee struct {
	Slot              uint64 `json:"slot"`
	PrioritizationFee uint64 `json:"prioritizationFee"`
}

// observeFees records the minimum declared compute unit price of a block. The
// runner published that price with the block (svm.LowestPrice). The caller
// holds s.mu.
//
// observeFees skips a block with no transactions, as Agave skips a block whose
// minimum is never set. A zero for each idle block tells a caller the market
// is free when no market exists.
//
// The window is in memory and starts empty after a restart, as Agave's cache
// does.
func (s *Server) observeFees(height, lowest uint64, priced bool) {
	if !priced {
		return
	}
	s.fees = append(s.fees, prioritizationFee{Slot: height, PrioritizationFee: lowest})
	if len(s.fees) > recentPrioritizationBlocks {
		s.fees = s.fees[len(s.fees)-recentPrioritizationBlocks:]
	}
}

// getRecentPrioritizationFees reports the minimum compute unit price paid in
// each of the last 150 blocks, oldest first.
//
// The number is not the ranking key. The scheduler orders a block by
// `total_fee * 1e6 / (cost + 1)`. This method returns the declared compute
// unit price, which a wallet feeds back into `setComputeUnitPrice`. A value in
// other units breaks every such wallet. The price is still a sufficient
// statistic. For a fixed message, a higher price gives a higher priority fee,
// total fee and ranking key.
//
// The method accepts and validates the address list, but the list does not
// narrow the answer. Agave returns the maximum of the block minimum and the
// minimum over transactions that write a named account. That needs a
// per-account map for every block in the window. This chain returns the block
// minimum alone, which is never above Agave's answer.
func getRecentPrioritizationFees(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	var addresses []string
	if err := param(params, 0, &addresses); err != nil {
		return nil, err
	}
	if len(addresses) > maxPrioritizationAddresses {
		return nil, invalidParams("Too many inputs provided; max %d", maxPrioritizationAddresses)
	}
	for _, a := range addresses {
		if _, err := decodeKey(a); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]prioritizationFee, len(s.fees))
	copy(out, s.fees)
	return out, nil
}
