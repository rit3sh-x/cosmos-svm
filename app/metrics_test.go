package app_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// scrape gathers the process-global registry, which the CometBFT metrics
// server serves. This is what an operator's Prometheus sees.
func scrape(t *testing.T) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*dto.MetricFamily{}
	for _, f := range families {
		out[f.GetName()] = f
	}
	return out
}

func value(t *testing.T, f *dto.MetricFamily) float64 {
	t.Helper()
	if f == nil || len(f.GetMetric()) == 0 {
		t.Fatal("the family carries no metric")
	}
	m := f.GetMetric()[0]
	switch {
	case m.GetCounter() != nil:
		return m.GetCounter().GetValue()
	case m.GetGauge() != nil:
		return m.GetGauge().GetValue()
	case m.GetHistogram() != nil:
		return float64(m.GetHistogram().GetSampleCount())
	}
	t.Fatalf("%s is neither a counter, a gauge nor a histogram", f.GetName())
	return 0
}

// TestAnOperatorCanScrapeTheChain proves that the collectors are on the
// registry that CometBFT serves, and that every listed metric is there. The
// engine metrics come through the engine's own stats entry point.
func TestAnOperatorCanScrapeTheChain(t *testing.T) {
	svm.RegisterMetrics(nil)
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{IndexMaxBytes: 1 << 20})
	c.Block()

	before := scrape(t)
	dest := tc.Pub(tc.Key(60))
	for i := 0; i < 3; i++ {
		if res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest, svm.MinimumBalance(0)))); res[0].Code != 0 {
			t.Fatalf("block %d committed as %d", i, res[0].Code)
		}
	}
	after := scrape(t)

	// Every listed metric name, from both sides of the boundary.
	for _, name := range []string{
		"svm_load_list_entries_total", "svm_push_accounts_seconds",
		"svm_arena_dropped_total", "svm_arena_bytes", "svm_admission_rejected_total", "svm_history_index_bytes",
		"svm_history_purged_height", "svm_dedup_entries",
		"svm_block_units", "svm_block_units_max", "svm_block_program_text_max_bytes",
		"svm_program_cache_hits_total", "svm_program_cache_misses_total",
		"svm_program_cache_evictions_total", "svm_program_cache_one_hit_wonders_total",
		"svm_prewarm_misses_total", "svm_program_cache_water_level",
		"svm_program_cache_bytes", "svm_conflict_groups",
		"svm_conflict_group_widest", "svm_block_program_text_bytes",
		"svm_signature_results_total", "svm_signature_results", "svm_signature_verifications_total",
		"svm_proposals_refused_total", "svm_block_admission_failed_total",
	} {
		if after[name] == nil {
			t.Fatalf("%s is not exported", name)
		}
	}
	// The two numbers that make a naming attack visible must move.
	if value(t, after["svm_load_list_entries_total"]) <= value(t, before["svm_load_list_entries_total"]) {
		t.Fatal("the load list counter did not move")
	}
	if value(t, after["svm_push_accounts_seconds"]) <= value(t, before["svm_push_accounts_seconds"]) {
		t.Fatal("the push-accounts histogram observed nothing")
	}
	if value(t, after["svm_block_units"]) == 0 {
		t.Fatal("the block spent no units")
	}
	if value(t, after["svm_block_units_max"]) == 0 || value(t, after["svm_block_program_text_max_bytes"]) == 0 {
		t.Fatal("the budgets the block is measured against are not exported")
	}
	if value(t, after["svm_dedup_entries"]) == 0 {
		t.Fatal("the replay set is empty after three transactions")
	}
	if value(t, after["svm_history_index_bytes"]) == 0 {
		t.Fatal("the history index reports no bytes")
	}
	// Each block here has one group. The engine counters come through the
	// stats entry point, not on a block result.
	if value(t, after["svm_conflict_groups"]) == 0 {
		t.Fatal("the engine reported no conflict groups")
	}
	// A rejection reason is exported at zero before it is ever hit, so an
	// operator can alert without waiting for the first one.
	rejected := after["svm_admission_rejected_total"]
	seen := map[string]bool{}
	for _, m := range rejected.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "reason" {
				seen[l.GetValue()] = true
			}
		}
	}
	for _, reason := range []string{"rejected", "blockhash_not_found", "already_processed"} {
		if !seen[reason] {
			t.Fatalf("no counter for rejection reason %q", reason)
		}
	}
	// A replayed transaction is counted under its own reason.
	tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest, svm.MinimumBalance(0)))
	if res, _ := c.Block(tx); res[0].Code != 0 {
		t.Fatal("the transaction did not commit")
	}
	if got := c.Check(c.Wrap(tx), false); got.Code == 0 {
		t.Fatal("a replay was admitted")
	}
	if replays := labelled(scrape(t)["svm_admission_rejected_total"], "already_processed"); replays == 0 {
		t.Fatal("the replay was not counted")
	}
}

func labelled(f *dto.MetricFamily, reason string) float64 {
	for _, m := range f.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "reason" && l.GetValue() == reason {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// TestScrapingDoesNotMoveTheAppHash proves that metrics are not consensus. A
// scrape reads the engine counters beside block execution and takes the
// program cache lock. A scraped machine and an unscraped one must agree at
// every height.
func TestScrapingDoesNotMoveTheAppHash(t *testing.T) {
	svm.RegisterMetrics(nil)
	faucet := tc.Key(1)
	quiet := tc.Start(t, tc.Pub(faucet), tc.Options{})
	scraped := tc.Start(t, tc.Pub(faucet), tc.Options{})

	for i := 0; i < 8; i++ {
		dest := tc.Pub(tc.Key(byte(70 + i)))
		tx := tc.Tx(tc.HashAt(int64(i+1)), faucet, nil, tc.Transfer(faucet, dest, svm.MinimumBalance(0)))
		res, want := quiet.Block(tx)
		if res[0].Code != 0 {
			t.Fatalf("block %d committed as %d", i, res[0].Code)
		}
		scrape(t)
		_, got := scraped.Block(tx)
		scrape(t)
		if string(got) != string(want) {
			t.Fatalf("block %d: scraped app hash %x, unscraped %x", i, got, want)
		}
	}
	// Every metric name that an operator reads is in the svm_ namespace.
	for name := range scrape(t) {
		if strings.HasPrefix(name, "svm") && !strings.HasPrefix(name, "svm_") {
			t.Fatalf("%s is outside the svm_ namespace", name)
		}
	}
}
