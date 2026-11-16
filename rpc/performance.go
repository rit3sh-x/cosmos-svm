package rpc

import (
	"context"
	"encoding/json/jsontext"
	"time"

	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// sampleInterval is Agave's SAMPLE_INTERVAL.
const sampleInterval = 60 * time.Second

// performanceSamplesLimit is Agave's PERFORMANCE_SAMPLES_LIMIT.
const performanceSamplesLimit = 720

// countAt is the committed height and the transaction count there, or zero
// before the first block commits.
func (s *Server) countAt() (height, count uint64) {
	ms, h, err := s.snapshot()
	if err != nil {
		return 0, 0
	}
	return h, s.app.Stores.TransactionCount(ms)
}

// SamplePerformance is Agave's SamplePerformanceService on this machine.
// Every 60 s of wall-clock time, it records how far the committed height and
// the counter moved. The next block stores the sample in the history, which
// purges it with the blocks. It is machine-local.
func (s *Server) SamplePerformance(ctx context.Context) error {
	height, count := s.countAt()
	last := s.perfClock.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.perfClock.After(sampleInterval):
		}
		h, c := s.countAt()
		now := s.perfClock.Now()
		s.history().Sample(svm.PerfSample{
			Slot:             h,
			NumSlots:         h - min(height, h),
			NumTransactions:  c - min(count, c),
			SamplePeriodSecs: uint16(now.Sub(last) / time.Second),
		})
		height, count, last = h, c, now
	}
}

// getTransactionCount returns the counter at the committed height.
func getTransactionCount(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	var cfg contextConfig
	if err := param(params, 0, &cfg); err != nil {
		return nil, err
	}
	ms, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if err := reached(cfg.MinContextSlot, h); err != nil {
		return nil, err
	}
	return s.app.Stores.TransactionCount(ms), nil
}

// getRecentPerformanceSamples returns samples newest first, 720 by default
// and at most.
func getRecentPerformanceSamples(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	limit := uint64(performanceSamplesLimit)
	if err := param(params, 0, &limit); err != nil {
		return nil, err
	}
	if limit > performanceSamplesLimit {
		return nil, invalidParams("Invalid limit; max %d", performanceSamplesLimit)
	}
	samples, err := s.history().Samples(int(limit))
	if err != nil {
		return nil, &rpcError{Code: codeInvalidRequest, Message: "Invalid request"}
	}
	out := []map[string]any{}
	for _, p := range samples {
		out = append(out, map[string]any{
			"slot":                   p.Slot,
			"numTransactions":        p.NumTransactions,
			"numNonVoteTransactions": p.NumTransactions,
			"numSlots":               p.NumSlots,
			"samplePeriodSecs":       p.SamplePeriodSecs,
		})
	}
	return out, nil
}
