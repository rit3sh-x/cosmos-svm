package svm

import (
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/rit3sh-x/cosmos-svm/ffi"
)

// The chain's collectors, on the process-global Prometheus registry.
// CometBFT's own metrics server serves prometheus.DefaultGatherer, so the
// registration here is all there is: no listener, no port and no config key of
// this chain.
//
// None of this is consensus. Two machines that export different numbers reach
// the same app hash.
//
// Everything is registered even if no operator scrapes, because an unscraped
// counter costs one atomic add per block.
var (
	loadListEntries = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "svm_load_list_entries_total",
		Help: "Accounts pushed to the engine across every block, whether they exist or not.",
	})
	arenaDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "svm_arena_dropped_total",
		Help: "Keys a block's load list named that the 512 MiB arena ceiling left unloaded.",
	})
	arenaBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "svm_arena_bytes",
		Help: "Account bytes the last block loaded, counted against the 512 MiB arena ceiling.",
	})
	pushAccountsSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "svm_push_accounts_seconds",
		Help:    "Time spent reading a block's load list out of the account stores.",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
	})
	admissionRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "svm_admission_rejected_total",
		Help: "Transactions refused at admission, by reason.",
	}, []string{"reason"})
	historyIndexBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "svm_history_index_bytes",
		Help: "Bytes the machine-local history index holds against index-max-bytes.",
	})
	historyHorizon = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "svm_history_purged_height",
		Help: "Highest height the history index has purged; the first available block is one above it.",
	})
	historyCompactionFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "svm_history_compaction_failures_total",
		Help: "Compactions of a purged history range that failed. The range stays deleted; only its disk space is not yet reclaimed.",
	})
	dedupEntries = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "svm_dedup_entries",
		Help: "Signatures held in the replay set over the acceptance window.",
	})
	blockUnits = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "svm_block_units",
		Help: "Cost-model units the last block spent.",
	})
	blockUnitsMax = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "svm_block_units_max",
		Help: "The block unit budget the engine enforces.",
	})
	blockProgramTextMax = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "svm_block_program_text_max_bytes",
		Help: "The per-block program text budget the engine enforces.",
	})
	proposalsRefused = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "svm_proposals_refused_total",
		Help: "Proposals ProcessProposal refused, by the first reason.",
	}, []string{"reason"})
	blockAdmissionFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "svm_block_admission_failed_total",
		Help: "Transactions of a finalized block that phase 1 did not admit and left unprocessed, by reason.",
	}, []string{"reason"})
)

// The engine's counters. A name here must not imply a per-slot reset. Agave
// resets its program cache stats on every new bank, but these accumulate for
// the life of the process.
var (
	engineCounters = map[string]struct {
		help  string
		value func(ffi.Stats) uint64
	}{
		"svm_program_cache_hits_total":            {"Programs a block wanted that the cache held.", func(s ffi.Stats) uint64 { return s.ProgramCacheHits }},
		"svm_program_cache_misses_total":          {"Programs a block wanted that had to be loaded.", func(s ffi.Stats) uint64 { return s.ProgramCacheMisses }},
		"svm_program_cache_evictions_total":       {"Compiled programs unloaded, summed over every program.", func(s ffi.Stats) uint64 { return s.ProgramCacheEvictions }},
		"svm_program_cache_one_hit_wonders_total": {"Programs used once before being unloaded.", func(s ffi.Stats) uint64 { return s.ProgramCacheOneHitWonders }},
		"svm_prewarm_misses_total":                {"Programs a block had to compile because the pre-warm pass did not hold them.", func(s ffi.Stats) uint64 { return s.PrewarmMisses }},
		"svm_signature_verifications_total":       {"Transactions whose signatures ed25519 checked: at CheckTx New, in the RPC preflight, and in phase 1 for a transaction with no stored result.", func(s ffi.Stats) uint64 { return s.SignatureVerifications }},
	}
	engineGauges = map[string]struct {
		help  string
		value func(ffi.Stats) uint64
	}{
		"svm_program_cache_water_level": {"Loaded entries the program cache currently holds.", func(s ffi.Stats) uint64 { return s.ProgramCacheWaterLevel }},
		"svm_program_cache_bytes":       {"Bytes the program cache keeps resident, which program-cache-bytes bounds.", func(s ffi.Stats) uint64 { return s.ProgramCacheBytes }},
		"svm_conflict_groups":           {"Conflict groups in the last block; one group is serial execution.", func(s ffi.Stats) uint64 { return s.ConflictGroups }},
		"svm_conflict_group_widest":     {"Transactions in the last block's largest conflict group.", func(s ffi.Stats) uint64 { return s.WidestConflictGroup }},
		"svm_block_program_text_bytes":  {"Program text the last block was charged against its budget.", func(s ffi.Stats) uint64 { return s.BlockProgramTextBytes }},
	}
)

// The signature-result series: each event and the handler it happens in, and
// each tier. A miss is a lookup that found no result. It is not a failed
// check, because failures count where they happen.
var (
	signatureResultSeries = []struct{ event, handler string }{
		{"hit", "finalize_block"}, {"miss", "finalize_block"},
		{"written", "check_tx"}, {"evicted", "check_tx"},
		{"forgotten", "finalize_block"},
		{"hit", "process_proposal"}, {"miss", "process_proposal"},
		{"written", "process_proposal"},
	}
	signatureResultTiers = []string{"pool", "block"}
)

const (
	signatureResultsName = "svm_signature_results_total"
	signatureHeldName    = "svm_signature_results"
)

// engineCollector reads the engine on scrape, not on every block.
//
// The counters have their own entry point, so nothing they carry can reach a
// consensus result. A read on operator request keeps that guarantee and adds
// one more. The cache byte total walks every cache entry, and a walk of
// unbounded length does not belong on the block path.
type engineCollector struct {
	descs map[string]*prometheus.Desc
	log   func(error)
}

func newEngineCollector(log func(error)) *engineCollector {
	c := &engineCollector{descs: map[string]*prometheus.Desc{}, log: log}
	for name, m := range engineCounters {
		c.descs[name] = prometheus.NewDesc(name, m.help, nil, nil)
	}
	for name, m := range engineGauges {
		c.descs[name] = prometheus.NewDesc(name, m.help, nil, nil)
	}
	c.descs[signatureResultsName] = prometheus.NewDesc(signatureResultsName,
		"Results of successful signature checks: reused (hit), looked up and not held (miss), kept (written), "+
			"pushed out by the cap (evicted) and removed when their transaction committed (forgotten), by the handler.",
		[]string{"event", "handler"}, nil)
	c.descs[signatureHeldName] = prometheus.NewDesc(signatureHeldName,
		"Results of successful signature checks the engine holds, by tier.", []string{"tier"}, nil)
	return c
}

func (c *engineCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range c.descs {
		ch <- d
	}
}

func (c *engineCollector) Collect(ch chan<- prometheus.Metric) {
	stats, err := ffi.EngineStats()
	if err != nil {
		if c.log != nil {
			c.log(err)
		}
		return
	}
	for name, m := range engineCounters {
		ch <- prometheus.MustNewConstMetric(c.descs[name], prometheus.CounterValue, float64(m.value(stats)))
	}
	for name, m := range engineGauges {
		ch <- prometheus.MustNewConstMetric(c.descs[name], prometheus.GaugeValue, float64(m.value(stats)))
	}
	for _, r := range signatureResultSeries {
		e, h := slices.Index(ffi.SignatureEvents[:], r.event), slices.Index(ffi.SignatureHandlers[:], r.handler)
		ch <- prometheus.MustNewConstMetric(c.descs[signatureResultsName], prometheus.CounterValue,
			float64(stats.SignatureResults[e][h]), r.event, r.handler)
	}
	for _, tier := range signatureResultTiers {
		ch <- prometheus.MustNewConstMetric(c.descs[signatureHeldName], prometheus.GaugeValue,
			float64(stats.SignatureResultsHeld[slices.Index(ffi.SignatureTiers[:], tier)]), tier)
	}
}

// mallocCollector reads glibc malloc once a scrape for both gauges: the C heap
// of Pebble and the engine, which neither the Go heap nor GOMEMLIMIT sees.
type mallocCollector struct{ held, used *prometheus.Desc }

func (c mallocCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.held; ch <- c.used }

func (c mallocCollector) Collect(ch chan<- prometheus.Metric) {
	held, used := ffi.Malloc()
	ch <- prometheus.MustNewConstMetric(c.held, prometheus.GaugeValue, float64(held))
	ch <- prometheus.MustNewConstMetric(c.used, prometheus.GaugeValue, float64(used))
}

var registerOnce sync.Once

// RegisterMetrics puts this chain's collectors on the process-global registry,
// which is what CometBFT's metrics server already serves. It is safe to call
// more than once, because a test binary starts several machines in one
// process, and Prometheus refuses a duplicate registration.
func RegisterMetrics(log func(error)) {
	registerOnce.Do(func() {
		// A Go collector that also reads every memory class, the collector
		// and its CPU from runtime/metrics replaces the default one. It does
		// not stop the world. The two size histograms and the pause histogram
		// stay out. They are about 40 series, and go_gc_duration_seconds and
		// the MemStats series already give that data.
		prometheus.Unregister(collectors.NewGoCollector())
		prometheus.MustRegister(
			collectors.NewGoCollector(
				collectors.WithGoCollectorRuntimeMetrics(collectors.GoRuntimeMetricsRule{
					Matcher: regexp.MustCompile(`^/(memory/classes|gc|cpu/classes/gc)/`)}),
				collectors.WithoutGoCollectorRuntimeMetrics(regexp.MustCompile(`^/gc/(heap/(allocs|frees)-by-size|pauses):`)),
			),
			mallocCollector{
				held: prometheus.NewDesc("svm_malloc_held_bytes",
					"Bytes glibc malloc holds from the system for the C heap, its arenas and its mmapped chunks.", nil, nil),
				used: prometheus.NewDesc("svm_malloc_used_bytes",
					"Bytes of svm_malloc_held_bytes that are allocated; the rest is free memory malloc keeps.", nil, nil),
			},
			loadListEntries, arenaDropped, arenaBytes, pushAccountsSeconds, admissionRejected,
			historyIndexBytes, historyHorizon, historyCompactionFailures, dedupEntries,
			blockUnits, blockUnitsMax, blockProgramTextMax, proposalsRefused, blockAdmissionFailed,
			newEngineCollector(log),
		)
		units, _, text := ffi.BlockLimits()
		blockUnitsMax.Set(float64(units))
		blockProgramTextMax.Set(float64(text))
		// A reason is exported as zero before it is first hit, so an operator
		// can alert on a rate without waiting for the first rejection.
		for _, reason := range []string{reasonAdmission, reasonBlockhash, reasonAlreadyProcessed, reasonUnpayable, reasonHost} {
			admissionRejected.WithLabelValues(reason)
		}
		for _, reason := range refusalReasons {
			proposalsRefused.WithLabelValues(reason)
		}
		for _, reason := range []string{droppedSignature, droppedAdmission} {
			blockAdmissionFailed.WithLabelValues(reason)
		}
	})
}

// The admission reasons, one per registered error (x/svm/runner.go).
const (
	reasonAdmission        = "rejected"
	reasonBlockhash        = "blockhash_not_found"
	reasonAlreadyProcessed = "already_processed"
	reasonUnpayable        = "unpayable"
	reasonHost             = "host"
)

// The reasons that ProcessProposal refuses a proposal for, in the order that
// its checks run. A refusal counts once, under the first reason it met.
const (
	refusedDecode       = "decode"
	refusedAdmission    = "admission"
	refusedBudget       = "budget"
	refusedDedup        = "dedup"
	refusedAccountLimit = "account_limit"
	refusedCosmos       = "cosmos"
	refusedSignature    = "signature"
)

var refusalReasons = []string{refusedDecode, refusedAdmission, refusedBudget, refusedDedup,
	refusedAccountLimit, refusedCosmos, refusedSignature}

// The reasons that phase 1 leaves a transaction unprocessed for. Its
// signatures fail, or another admission check fails.
const (
	droppedSignature = "signature"
	droppedAdmission = "admission"
)

// observeBlock records what a finalized block cost. Nothing on the result path
// reads these counters, so a counter that moves changes no app hash.
func observeBlock(loadList, dropped int, arena int64, pushed time.Duration, units uint64) {
	loadListEntries.Add(float64(loadList))
	arenaDropped.Add(float64(dropped))
	arenaBytes.Set(float64(arena))
	pushAccountsSeconds.Observe(pushed.Seconds())
	blockUnits.Set(float64(units))
}
