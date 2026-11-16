package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	cmtcfg "github.com/cometbft/cometbft/config"
	cmttypes "github.com/cometbft/cometbft/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/spf13/viper"

	"cosmossdk.io/log/v2"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/flags"
	"github.com/cosmos/cosmos-sdk/server"
	serverconfig "github.com/cosmos/cosmos-sdk/server/config"
	"github.com/cosmos/cosmos-sdk/store/v2/iavl"
	pruningtypes "github.com/cosmos/cosmos-sdk/store/v2/pruning/types"

	"github.com/rit3sh-x/cosmos-svm/app"
	"github.com/rit3sh-x/cosmos-svm/genesis"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestStartRequiresOnlyTheHistoryBudget proves that start refuses without the
// history budget and needs no min-retain-blocks setting, because init ships it
// at the evidence window.
func TestStartRequiresOnlyTheHistoryBudget(t *testing.T) {
	cfg := defaultSVMConfig()
	if err := requireRetention(cfg); err == nil || !strings.Contains(err.Error(), "index-max-bytes") {
		t.Fatalf("the shipped [svm]: %v", err)
	}
	cfg.IndexMaxBytes = 1 << 30
	if err := requireRetention(cfg); err != nil {
		t.Fatalf("min-retain-blocks unset: %v", err)
	}
	if got := shippedConfig().MinRetainBlocks; got != 2000 {
		t.Fatalf("shipped min-retain-blocks is %d, want 2000, the evidence window", got)
	}
}

// TestTheShippedPruningIntervalIsTheLowestThatValidates proves that app.toml
// ships the lowest interval that `PruningOptions.Validate` accepts. The SDK
// validates it twice, and the app lowers it to 1 after both, which
// app/pruning_test.go proves.
func TestTheShippedPruningIntervalIsTheLowestThatValidates(t *testing.T) {
	cfg := shippedConfig()
	if cfg.Pruning != pruningtypes.PruningOptionCustom {
		t.Fatalf("shipped pruning strategy %q, want custom", cfg.Pruning)
	}
	if cfg.PruningInterval != "10" || cfg.PruningKeepRecent != "2" {
		t.Fatalf("shipped pruning is keep-recent %q interval %q, want 2 and 10",
			cfg.PruningKeepRecent, cfg.PruningInterval)
	}
	v := viper.New()
	v.Set("pruning", cfg.Pruning)
	v.Set("pruning-keep-recent", cfg.PruningKeepRecent)
	v.Set("pruning-interval", cfg.PruningInterval)
	if _, err := server.GetPruningOptionsFromFlags(v); err != nil {
		t.Fatalf("the shipped pruning config does not validate: %v", err)
	}
}

// TestTheShippedConfigTakesSnapshots proves that the shipped config takes
// snapshots and passes ValidateBasic. State sync is the only way to add a
// machine after block 1. The SDK refuses the name `everything` beside a
// snapshot interval, so the shipped config uses `custom` with the same numbers.
func TestTheShippedConfigTakesSnapshots(t *testing.T) {
	cfg := shippedConfig()
	if cfg.Pruning == pruningtypes.PruningOptionEverything {
		t.Fatal("the shipped config prunes everything, which cannot take snapshots")
	}
	if cfg.StateSync.SnapshotInterval == 0 {
		t.Fatal("the shipped config takes no snapshots, so no machine can ever join")
	}
	if err := cfg.ValidateBasic(); err != nil {
		t.Fatalf("the shipped config does not start: %v", err)
	}

	// The SDK refuses the forbidden combination. This shows that the check is
	// live and that the shipped form avoids it.
	forbidden := shippedConfig()
	forbidden.Pruning = pruningtypes.PruningOptionEverything
	if err := forbidden.ValidateBasic(); err == nil {
		t.Fatal("pruning everything beside a snapshot interval was accepted")
	}
}

// TestTheShippedTimeoutCommitSurvivesInterception proves that the shipped
// timeout_commit is 400 ms and differs from CometBFT's default. The SDK rewrites
// exactly that default to 5 s, so a value equal to it would be replaced with
// no warning. The test also proves that the transaction index is off.
func TestTheShippedTimeoutCommitSurvivesInterception(t *testing.T) {
	cfg := shippedComet()
	if cfg.Consensus.TimeoutCommit != 400*time.Millisecond {
		t.Fatalf("timeout_commit is %s, want 400ms", cfg.Consensus.TimeoutCommit)
	}
	if cfg.Consensus.TimeoutCommit == cmtcfg.DefaultConfig().Consensus.TimeoutCommit {
		t.Fatal("timeout_commit equals CometBFT's default, so the SDK rewrites it to 5s")
	}
	if cfg.TxIndex.Indexer != "null" {
		t.Fatalf("the transaction index is %q, want null", cfg.TxIndex.Indexer)
	}
}

// While a node catches up, the blocksync pool holds up to 20 blocks for each
// peer. At the shipped peer limits and svm.MaxBlockBytes that is at most
// 3,200 MiB. The page cache keeps most of the 7,472 MiB that the 20 GiB budget
// gives it.
func TestTheShippedPeerLimitsBoundTheSyncPool(t *testing.T) {
	const blocksPerPeer = 20
	p2p := shippedComet().P2P
	peers := p2p.MaxNumInboundPeers + p2p.MaxNumOutboundPeers
	if pool := int64(peers) * blocksPerPeer * svm.MaxBlockBytes; pool > 3200<<20 {
		t.Fatalf("%d peers can hold %d MiB of blocks while the node catches up, want at most 3,200", peers, pool>>20)
	}
}

// TestTheShippedConfigStartsTheApp proves that the shipped configuration
// builds the app, which then prunes at interval 1 with the operator's
// keep-recent. Two validations come before an interval of 1, because
// `LoadLatestVersion` validates the baseapp options again.
func TestTheShippedConfigStartsTheApp(t *testing.T) {
	cfg := shippedConfig()
	home := t.TempDir()
	v := viper.New()
	v.Set(flags.FlagHome, home)
	v.Set(flags.FlagChainID, "svm-start-test")
	v.Set(server.FlagPruning, cfg.Pruning)
	v.Set(server.FlagPruningKeepRecent, cfg.PruningKeepRecent)
	v.Set(server.FlagPruningInterval, cfg.PruningInterval)
	v.Set(server.FlagIAVLCacheSize, cfg.IAVLCacheSize)
	v.Set(server.FlagStateSyncSnapshotInterval, cfg.StateSync.SnapshotInterval)
	v.Set(server.FlagStateSyncSnapshotKeepRecent, cfg.StateSync.SnapshotKeepRecent)
	v.Set(server.FlagMempoolMaxTxs, cfg.Mempool.MaxTxs)
	v.Set(server.FlagMinGasPrices, cfg.MinGasPrices)
	v.Set(server.FlagInterBlockCache, cfg.InterBlockCache)

	a, err := app.New(log.NewNopLogger(), dbm.NewMemDB(), app.Local{}, home,
		server.DefaultBaseappOptions(v)...)
	if err != nil {
		t.Fatalf("the shipped configuration does not start: %v", err)
	}
	got := a.CommitMultiStore().GetPruning()
	if got.Interval != 1 {
		t.Fatalf("pruning interval %d after start, want 1", got.Interval)
	}
	if got.KeepRecent != 2 {
		t.Fatalf("pruning keep-recent %d, want the operator's 2", got.KeepRecent)
	}
	// A block reads the IAVL store of each shard. No inter-block cache is
	// over it.
	for _, k := range a.Stores.Accounts {
		s := a.CommitMultiStore().GetKVStore(k)
		if _, ok := s.(*iavl.Store); !ok {
			t.Fatalf("shard %s is a %T, want the IAVL store with no inter-block cache", k.Name(), s)
		}
	}
}

// TestTheRelayerServicesAreServed proves that the query router serves
// Simulate and GetNodeInfo, which Hermes uses for its version and
// compatibility checks. Both reach the router only through the two
// registrars. The node service stays off, because no relayer reads it.
func TestTheRelayerServicesAreServed(t *testing.T) {
	a, err := app.New(log.NewNopLogger(), dbm.NewMemDB(), app.Local{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n := node{a}
	n.RegisterTxService(client.Context{})
	n.RegisterTendermintService(client.Context{})
	n.RegisterNodeService(client.Context{}, *serverconfig.DefaultConfig())
	for path, want := range map[string]bool{
		"/cosmos.tx.v1beta1.Service/Simulate":                 true,
		"/cosmos.base.tendermint.v1beta1.Service/GetNodeInfo": true,
		"/ibc.core.channel.v1.Query/UnreceivedPackets":        true,
		"/cosmos.base.node.v1beta1.Service/Config":            false,
	} {
		if got := a.GRPCQueryRouter().Route(path) != nil; got != want {
			t.Errorf("%s served %v, want %v", path, got, want)
		}
	}
}

// TestStartRefusesAProposeTimeoutBelowTheDerivation proves that start refuses
// a timeout_propose below the 3 s budget that block.max_bytes comes from, and
// accepts a higher value.
func TestStartRefusesAProposeTimeoutBelowTheDerivation(t *testing.T) {
	cfg := shippedComet()
	if cfg.Consensus.TimeoutPropose != minTimeoutPropose {
		t.Fatalf("the shipped timeout_propose is %s, want %s", cfg.Consensus.TimeoutPropose, minTimeoutPropose)
	}
	if err := requireProposeTimeout(cfg); err != nil {
		t.Fatalf("the shipped config is refused: %v", err)
	}
	cfg.Consensus.TimeoutPropose = 10 * time.Second
	if err := requireProposeTimeout(cfg); err != nil {
		t.Fatalf("a raised timeout_propose is refused: %v", err)
	}
	cfg.Consensus.TimeoutPropose = minTimeoutPropose - time.Millisecond
	err := requireProposeTimeout(cfg)
	if err == nil {
		t.Fatal("a lowered timeout_propose starts")
	}
	if !strings.Contains(err.Error(), "timeout_propose") || !strings.Contains(err.Error(), "block.max_bytes") {
		t.Fatalf("the refusal does not name the setting and the reason: %v", err)
	}
}

// TestTheWholePoolFitsOneReap proves that a full pool fits one reap, both at
// its byte cap and at its count cap. The refusal trusts a framing bound of 3
// bytes per transaction, so the test measures it with the function that the
// reap calls.
func TestTheWholePoolFitsOneReap(t *testing.T) {
	cfg := shippedComet()
	if err := requirePoolFitsReap(cfg); err != nil {
		t.Fatalf("the shipped config is refused: %v", err)
	}
	if cfg.Mempool.MaxTxsBytes != 8_358_477 {
		t.Fatalf("the shipped max_txs_bytes is %d, want 8,358,477", cfg.Mempool.MaxTxsBytes)
	}

	pool := func(n, length int) []cmttypes.Tx {
		txs := make([]cmttypes.Tx, n)
		for i := range txs {
			txs[i] = make([]byte, length)
		}
		return txs
	}
	// The first pool holds the largest V1 transaction in its Cosmos envelope
	// until the byte cap refuses the next one. The second holds size
	// transactions that share the cap, where the framing is largest.
	v1 := 4_139
	size := cfg.Mempool.Size
	for _, p := range [][]cmttypes.Tx{
		pool(int(cfg.Mempool.MaxTxsBytes)/v1, v1),
		pool(size, int(cfg.Mempool.MaxTxsBytes)/size),
	} {
		if got := cmttypes.ComputeProtoSizeForTxs(p); got > reapWindow() {
			t.Fatalf("a full pool of %d transactions of %d bytes reaps as %d, over the %d byte window",
				len(p), len(p[0]), got, reapWindow())
		}
	}
	if got := cmttypes.ComputeProtoSizeForTxs(pool(1, v1)) - int64(v1); got > reapFraming {
		t.Fatalf("a %d byte transaction reaps with %d bytes of framing, over the %d the refusal assumes", v1, got, reapFraming)
	}

	cfg.Mempool.MaxTxsBytes++
	err := requirePoolFitsReap(cfg)
	if err == nil {
		t.Fatal("a byte cap one over the window starts")
	}
	for _, want := range []string{"8358478", "8373478", "8373477"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %s: %v", want, err)
		}
	}
}

// TestStartRefusesAFreeCosmosLane proves that the shipped fee floor starts.
// A zero floor, a floor without usvm and a floor that lists a voucher beside
// usvm each get a refusal that names the reason.
func TestStartRefusesAFreeCosmosLane(t *testing.T) {
	v := viper.New()
	v.Set(server.FlagMinGasPrices, shippedConfig().MinGasPrices)
	if err := requireFeeFloor(v); err != nil {
		t.Fatalf("the shipped floor is refused: %v", err)
	}
	for _, prices := range []string{
		"", "0usvm", "0.000001stake",
		"1ibc/27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2",
		"0.000001usvm,1ibc/27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2",
	} {
		v.Set(server.FlagMinGasPrices, prices)
		err := requireFeeFloor(v)
		if err == nil || !strings.Contains(err.Error(), "only an operator can pay") {
			t.Fatalf("minimum-gas-prices %q: %v", prices, err)
		}
	}
}

// TestStartRefusesIAVLWithoutFastNodes proves that the shipped app.toml starts
// and that start refuses IAVL's fast-node index turned off. A long scan of
// committed state stays complete only through that index.
func TestStartRefusesIAVLWithoutFastNodes(t *testing.T) {
	v := viper.New()
	v.Set(server.FlagDisableIAVLFastNode, shippedConfig().IAVLDisableFastNode)
	if err := requireFastNodes(v); err != nil {
		t.Fatalf("the shipped app.toml is refused: %v", err)
	}
	v.Set(server.FlagDisableIAVLFastNode, true)
	if err := requireFastNodes(v); err == nil || !strings.Contains(err.Error(), "fast-node index") {
		t.Fatalf("fast nodes off: %v", err)
	}
}

// TestTheGoHeapHasABackstop proves that svmd limits the Go heap to 14 GiB
// unless GOMEMLIMIT is set. With GOMEMLIMIT set, the runtime has applied the
// operator's value, and svmd does not change it.
func TestTheGoHeapHasABackstop(t *testing.T) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(-1))

	t.Setenv("GOMEMLIMIT", "")
	limitGoHeap()
	if got := debug.SetMemoryLimit(-1); got != 14<<30 {
		t.Fatalf("with GOMEMLIMIT unset the limit is %d, want 14 GiB", got)
	}

	t.Setenv("GOMEMLIMIT", "20GiB")
	debug.SetMemoryLimit(20 << 30)
	limitGoHeap()
	if got := debug.SetMemoryLimit(-1); got != 20<<30 {
		t.Fatalf("with GOMEMLIMIT=20GiB svmd changed the limit to %d", got)
	}
}

// TestMallocStartsTuned proves that with nothing set, svmd caps glibc at four
// arenas and trims at 128 KiB. glibc reads its environment when a process
// starts, so each case runs this test in a new process. A variable that the
// environment sets, by its name or through GLIBC_TUNABLES, stays the
// operator's value, and svmd still sets the other one.
func TestMallocStartsTuned(t *testing.T) {
	const probe = "SVMD_MALLOC_PROBE"
	if os.Getenv(probe) != "" {
		fmt.Printf("arenas=%d trims=%t\n", mallocArenas(), mallocTrims())
		return
	}
	const big = "67108864"
	for _, c := range []struct{ env, want string }{
		{"", "arenas=4 trims=true"},
		{"MALLOC_ARENA_MAX=6 MALLOC_TRIM_THRESHOLD_=" + big, "arenas=6 trims=false"},
		{"GLIBC_TUNABLES=glibc.malloc.arena_max=6", "arenas=6 trims=true"},
		{"GLIBC_TUNABLES=glibc.malloc.trim_threshold=" + big, "arenas=4 trims=false"},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestMallocStartsTuned$")
		cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
			return strings.HasPrefix(kv, "MALLOC_") || strings.HasPrefix(kv, "GLIBC_TUNABLES=")
		}), probe+"=1")
		cmd.Env = append(cmd.Env, strings.Fields(c.env)...)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), c.want) {
			t.Errorf("env %q: want %s, got %v:\n%s", c.env, c.want, err, out)
		}
	}
}

// TestTheShippedConfigServesMetrics proves that the shipped config turns on
// CometBFT's Prometheus server. That server serves the process-global
// registry, so this chain needs no listener, port or config key of its own.
func TestTheShippedConfigServesMetrics(t *testing.T) {
	cfg := shippedComet()
	if !cfg.Instrumentation.Prometheus {
		t.Fatal("the shipped config does not turn CometBFT's metrics server on")
	}
	if cfg.Instrumentation.PrometheusListenAddr == "" {
		t.Fatal("the metrics listen address is empty")
	}
}

// TestANodeExportsItsMemoryByClass proves that after start registers the
// collectors and runs newNode, a scrape has the memory classes and the
// collector of the Go runtime, what Pebble holds for each of the four
// databases, and what glibc malloc holds and uses for the C heap.
func TestANodeExportsItsMemoryByClass(t *testing.T) {
	svm.RegisterMetrics(nil)
	home := t.TempDir()
	v := viper.New()
	v.Set(flags.FlagHome, home)
	v.Set(flags.FlagChainID, "svm-memory-test")
	v.Set(server.FlagPruning, pruningtypes.PruningOptionNothing)
	db, err := openApplicationDB(home, dbm.PebbleDBBackend)
	if err != nil {
		t.Fatal(err)
	}
	defer newNode(log.NewNopLogger(), db, v).Close()

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]*dto.Metric{}
	for _, f := range families {
		got[f.GetName()] = f.GetMetric()
	}
	for _, name := range []string{
		"go_memory_classes_heap_objects_bytes", "go_memory_classes_total_bytes",
		"go_gc_heap_live_bytes", "go_gc_heap_goal_bytes", "go_gc_cycles_total_gc_cycles_total",
		"go_gc_limiter_last_enabled_gc_cycle", "go_cpu_classes_gc_total_cpu_seconds_total",
		"go_memstats_heap_inuse_bytes", "go_gc_duration_seconds",
	} {
		if got[name] == nil {
			t.Errorf("%s is not exported", name)
		}
	}
	for _, name := range []string{"go_gc_heap_allocs_by_size_bytes", "go_gc_heap_frees_by_size_bytes", "go_gc_pauses_seconds"} {
		if got[name] != nil {
			t.Errorf("%s is exported", name)
		}
	}
	for _, name := range []string{"svm_pebble_block_cache_bytes", "svm_pebble_memtable_bytes", "svm_pebble_memtable_zombie_bytes"} {
		var dbs []string
		for _, m := range got[name] {
			dbs = append(dbs, m.GetLabel()[0].GetValue())
		}
		slices.Sort(dbs)
		if !slices.Equal(dbs, []string{"application", "dedup", "history", "tokens"}) {
			t.Errorf("%s is exported for %v", name, dbs)
		}
	}
	// Each database's memtable is calloc in a cgo build, so the C heap that
	// malloc reports is not empty.
	held, used := got["svm_malloc_held_bytes"], got["svm_malloc_used_bytes"]
	if len(held) != 1 || len(used) != 1 {
		t.Fatalf("malloc is exported as %d and %d series", len(held), len(used))
	}
	if h, u := held[0].GetGauge().GetValue(), used[0].GetGauge().GetValue(); u == 0 || u > h {
		t.Errorf("malloc reports %v held and %v used", h, u)
	}
}

// TestTheGenesisHashIsTheFilesSha256 proves that the genesis hash is the
// sha256 of the genesis file. getGenesisHash returns this hash, and operators
// compare it across machines before the first start.
func TestTheGenesisHashIsTheFilesSha256(t *testing.T) {
	path := filepath.Join(t.TempDir(), "genesis.json")
	if err := os.WriteFile(path, []byte(`{"chain_id":"svm-hash-test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := genesis.Hash(path)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(`{"chain_id":"svm-hash-test"}`))
	if got != want {
		t.Fatalf("the genesis hash is %x, want %x", got, want)
	}
	if _, err := genesis.Hash(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("a missing genesis file hashes")
	}
}

// TestTheCommandsAnOperatorPipesWriteToStdout proves that `svmd comet
// show-validator`, `svmd comet show-node-id` and `init` write to stdout.
// Cobra writes to stderr unless a writer is set. The test checks the leaf
// commands, because an operator runs the leaf, and cobra takes the writer
// from the nearest parent that has one.
func TestTheCommandsAnOperatorPipesWriteToStdout(t *testing.T) {
	root := rootCmd(t.TempDir())
	for _, path := range [][]string{
		{"comet", "show-validator"},
		{"comet", "show-node-id"},
		{"init"},
	} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatalf("%v: %v", path, err)
		}
		if got := cmd.OutOrStdout(); got != os.Stdout {
			t.Fatalf("%v writes to %T, not os.Stdout, so a script capturing it gets nothing", path, got)
		}
	}
}

// TestTheQuicPortIsOffAndNeedsNoBinary proves that with tpu-quic-address
// empty, svmd starts without svm-tpu and checks nothing.
func TestTheQuicPortIsOffAndNeedsNoBinary(t *testing.T) {
	off := defaultSVMConfig()
	off.TPUQuicBinary = "/nowhere/svm-tpu"
	cfg, err := quicSettings(off, cmtcfg.DefaultConfig(), func() (string, error) { return "/nowhere/svmd", nil })
	if err != nil || cfg.Bind != "" {
		t.Fatalf("the port off gave %+v, %v", cfg, err)
	}
}

// TestTheQuicPortRefusesAPoolThatDoesNotGossip proves that the QUIC port
// refuses a pool that does not gossip, because every leader entry would name
// an ingress that reaches no other validator. It also proves that svmd finds
// the binary beside itself and uses Agave's defaults.
func TestTheQuicPortRefusesAPoolThatDoesNotGossip(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "svm-tpu"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	beside := func() (string, error) { return filepath.Join(dir, "svmd"), nil }
	on := defaultSVMConfig()
	on.TPUQuicAddress = "127.0.0.1:9000"
	comet := cmtcfg.DefaultConfig()
	cfg, err := quicSettings(on, comet, beside)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Binary != filepath.Join(dir, "svm-tpu") || cfg.PerPeer != 8 || cfg.PerIPPerMinute != 8 || cfg.MaxUnstakedConns != 2000 {
		t.Fatalf("the settings are %+v", cfg)
	}
	for _, mutate := range []func(*cmtcfg.Config){
		func(c *cmtcfg.Config) { c.Mempool.Broadcast = false },
		func(c *cmtcfg.Config) { c.Mempool.Type = cmtcfg.MempoolTypeNop },
	} {
		c := cmtcfg.DefaultConfig()
		mutate(c)
		if _, err := quicSettings(on, c, beside); err == nil || !strings.Contains(err.Error(), "does not gossip") {
			t.Fatalf("a pool that does not gossip started: %v", err)
		}
	}
}

// TestTheDatabasesAreOnPebble checks that svmd's databases are on Pebble and
// that the key the SDK's own commands read says so. The shipped app.toml has
// app-db-backend = "pebbledb". The opener refuses any other engine. What the
// opener writes, the SDK's own opener reads back, as rollback and export
// open it.
func TestTheDatabasesAreOnPebble(t *testing.T) {
	v := viper.New()
	v.Set("app-db-backend", shippedConfig().AppDBBackend)
	backend := server.GetAppDBBackend(v)
	if backend != dbm.PebbleDBBackend {
		t.Fatalf("the shipped app-db-backend is %q, want %q", backend, dbm.PebbleDBBackend)
	}
	home := t.TempDir()
	if db, err := openApplicationDB(home, dbm.GoLevelDBBackend); err == nil {
		db.Close()
		t.Fatal("the opener took goleveldb")
	}
	db, err := openApplicationDB(home, backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Set([]byte("k"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	sdk, err := dbm.NewDB("application", backend, filepath.Join(home, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer sdk.Close()
	if got, err := sdk.Get([]byte("k")); err != nil || string(got) != "v" {
		t.Fatalf("the SDK's opener read %q %v, want v", got, err)
	}
}
