// Command svmd runs a CometBFT validator or a full node of this chain.
//
// `init` builds the whole genesis in one command on one machine, as
// solana-genesis does. It uses no gentx, no collection round and no keyring,
// and the app does not mount x/genutil as a module. With no operator flags,
// `init` bonds the machine that ran it.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	cmtcfg "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/privval"
	cmttypes "github.com/cometbft/cometbft/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/mr-tron/base58"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cast"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/sync/errgroup"

	"cosmossdk.io/log/v2"
	"cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/flags"
	"github.com/cosmos/cosmos-sdk/client/grpc/cmtservice"
	"github.com/cosmos/cosmos-sdk/codec"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/server"
	"github.com/cosmos/cosmos-sdk/server/api"
	svrcmd "github.com/cosmos/cosmos-sdk/server/cmd"
	serverconfig "github.com/cosmos/cosmos-sdk/server/config"
	servertypes "github.com/cosmos/cosmos-sdk/server/types"
	pruningtypes "github.com/cosmos/cosmos-sdk/store/v2/pruning/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	"github.com/cosmos/cosmos-sdk/version"
	authtx "github.com/cosmos/cosmos-sdk/x/auth/tx"
	genutilcli "github.com/cosmos/cosmos-sdk/x/genutil/client/cli"
	genutiltypes "github.com/cosmos/cosmos-sdk/x/genutil/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	"github.com/rit3sh-x/cosmos-svm/app"
	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/genesis"
	"github.com/rit3sh-x/cosmos-svm/rpc"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// node implements the server's Application interface. Clients use the Solana
// JSON-RPC. Cosmos gRPC listens on localhost for the operator's own relayer.
// The relayer needs the tx service for Simulate and the CometBFT service for
// GetNodeInfo. The node service and the REST routes stay off, because no
// relayer reads them.
type node struct{ *app.App }

func (node) RegisterAPIRoutes(*api.Server, serverconfig.APIConfig)   {}
func (node) RegisterNodeService(client.Context, serverconfig.Config) {}

func (n node) RegisterTxService(ctx client.Context) {
	authtx.RegisterTxService(n.GRPCQueryRouter(), ctx, n.Simulate, n.Codec.InterfaceRegistry())
}

func (n node) RegisterTendermintService(ctx client.Context) {
	cmtservice.RegisterTendermintService(ctx, n.GRPCQueryRouter(), n.Codec.InterfaceRegistry(),
		server.NewCometABCIWrapper(n).Query)
}

// running is the app that the start command created. The RPC server reads it.
var running *app.App

// pebbleMetrics is the collector newNode registered last.
var pebbleMetrics prometheus.Collector

func newNode(logger log.Logger, db dbm.DB, opts servertypes.AppOptions) servertypes.Application {
	data := filepath.Join(cast.ToString(opts.Get(flags.FlagHome)), "data")
	dedup, err := app.OpenSidecarDB("svmdedup", data)
	if err != nil {
		panic(err)
	}
	history, err := app.OpenIndexDB("svmhistory", data)
	if err != nil {
		panic(err)
	}
	// The token index has no setting of its own. It is a function of current
	// account state, and a machine that lost it rebuilds it on its next block.
	tokens, err := app.OpenIndexDB("svmtokens", data)
	if err != nil {
		panic(err)
	}
	// What Pebble holds outside the Go heap, for each database. Only this
	// function holds all four handles. A later call replaces the collector,
	// so the registry reads the databases that are open, and a second node
	// in one process does not panic.
	if pebbleMetrics != nil {
		prometheus.Unregister(pebbleMetrics)
	}
	pebbleMetrics = app.PebbleCollector(map[string]dbm.DB{
		"application": db, "dedup": dedup, "history": history, "tokens": tokens})
	prometheus.MustRegister(pebbleMetrics)
	cfg, err := readConfig(opts.(*viper.Viper))
	if err != nil {
		panic(err)
	}
	if cfg.ProgramCacheBytes != 0 {
		ffi.SetProgramCacheBytes(cfg.ProgramCacheBytes)
	}
	if cfg.Threads != 0 {
		ffi.SetThreads(cfg.Threads)
	}
	// The SDK registers --unsafe-skip-upgrades on start, and the app passes
	// the heights to x/upgrade.
	skip := map[int64]bool{}
	for _, h := range cast.ToIntSlice(opts.Get(server.FlagUnsafeSkipUpgrades)) {
		skip[int64(h)] = true
	}
	local := app.Local{Dedup: dedup, History: history, Tokens: tokens, IndexMaxBytes: cfg.IndexMaxBytes,
		SkipUpgrades: skip}
	a, err := app.New(logger, db, local, cast.ToString(opts.Get(flags.FlagHome)), server.DefaultBaseappOptions(opts)...)
	if err != nil {
		panic(err)
	}
	running = a
	return node{a}
}

// goMemoryLimit is the Go heap's limit on the published 20 GiB machine. 20 GiB
// less the program cache and the execute arena at their peaks, the engine and
// Pebble leaves about 15 GiB. The limit stays at 14 GiB, so the page cache gets
// the rest. It is machine-local, like the program cache.
const goMemoryLimit = 14 << 30

// limitGoHeap sets goMemoryLimit unless the operator set GOMEMLIMIT, which the
// Go runtime read at process start. Without a limit, the default GOGC lets the
// heap grow to twice its live size before a collection. Go cannot see a
// cgroup cap.
func limitGoHeap() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(goMemoryLimit)
	}
}

// requireRetention refuses to start until the history record's byte budget is
// set. The budget has no default, because it depends on the machine's disk and
// load. Block retention needs no such check: min-retain-blocks ships at the
// evidence window, a floor that every machine keeps and an operator can only
// raise.
func requireRetention(cfg SVMConfig) error {
	if cfg.IndexMaxBytes == 0 {
		return fmt.Errorf("set index-max-bytes under [svm] in app.toml: " +
			"it bounds disk use and has no default")
	}
	return nil
}

// startSolanaRPC runs the machine-local Solana JSON-RPC server beside the
// in-process CometBFT node. The server reads the history record that the
// block-end writer keeps in the data directory.
func startSolanaRPC(svrCtx *server.Context, clientCtx client.Context, ctx context.Context, g *errgroup.Group) error {
	s := rpc.New(running, clientCtx.Client, svrCtx.Logger.With("module", "solana-rpc"))
	hash, err := genesis.Hash(svrCtx.Config.GenesisFile())
	if err != nil {
		return err
	}
	s.SetGenesisHash(hash)
	// getBlockTime at slot 0 is the genesis time.
	doc, err := genutiltypes.AppGenesisFromFile(svrCtx.Config.GenesisFile())
	if err != nil {
		return err
	}
	s.SetGenesisTime(doc.GenesisTime.Unix())
	cfg, err := readConfig(svrCtx.Viper)
	if err != nil {
		return err
	}
	s.SetLimits(cfg.WSMaxConnections, cfg.WSMaxSubscriptions, cfg.WSMaxQueueBytes)
	quic, err := quicSettings(cfg, svrCtx.Config, os.Executable)
	if err != nil {
		return err
	}
	origins, err := rpc.ParseOrigins(cfg.RPCCORSOrigins)
	if err != nil {
		return err
	}
	s.SetOrigins(origins)
	s.SetMaxSimulations(cfg.RPCMaxSimulations)
	svrCtx.Logger.Info("full scans of committed state at once", "rpc-max-scans", s.SetMaxScans(cfg.RPCMaxScans))
	svrCtx.Logger.Info("HTTP requests answered at once", "rpc-threads", s.SetThreads(cfg.RPCThreads))
	s.SetWriteTimeout(cfg.RPCWriteTimeout)
	s.SetScanLimit(cfg.RPCScanLimitMB)
	g.Go(func() error { return s.Follow(ctx) })
	g.Go(func() error { return s.SamplePerformance(ctx) })
	if quic.Bind != "" {
		g.Go(func() error { return s.ServeQUIC(ctx, quic) })
	}
	g.Go(func() error {
		return s.Serve(ctx, cfg.RPCAddress, cfg.WSAddress)
	})
	return nil
}

// quicSettings reads the QUIC port's settings. It refuses a start that would
// advertise an address that nothing serves:
//   - no svm-tpu binary
//   - an unspecified bind with no public address
//   - an IPv6 advertised address
//   - a pool that does not gossip, because then every leader entry names an
//     ingress that reaches no other validator
//
// With the port off, it checks nothing, so a machine without svm-tpu starts.
func quicSettings(c SVMConfig, comet *cmtcfg.Config, executable func() (string, error)) (rpc.QuicConfig, error) {
	cfg := rpc.QuicConfig{
		Bind:             c.TPUQuicAddress,
		Public:           c.PublicTPUAddress,
		Binary:           c.TPUQuicBinary,
		PerPeer:          c.TPUPerPeer,
		PerIPPerMinute:   c.TPUPerIPMinute,
		MaxUnstakedConns: c.TPUMaxUnstaked,
	}
	if cfg.Bind == "" {
		return rpc.QuicConfig{}, nil
	}
	if cfg.PerPeer <= 0 {
		cfg.PerPeer = rpc.DefaultTPUConnectionsPerPeer
	}
	if cfg.PerIPPerMinute <= 0 {
		cfg.PerIPPerMinute = rpc.DefaultTPUConnectionsPerIPMinute
	}
	if cfg.MaxUnstakedConns <= 0 {
		cfg.MaxUnstakedConns = rpc.DefaultTPUMaxUnstakedConnections
	}
	if cfg.Binary == "" {
		if self, err := executable(); err == nil {
			cfg.Binary = filepath.Join(filepath.Dir(self), "svm-tpu")
		}
	}
	if err := cfg.Check(); err != nil {
		return rpc.QuicConfig{}, err
	}
	if !comet.Mempool.Broadcast || comet.Mempool.Type != cmtcfg.MempoolTypeFlood {
		return rpc.QuicConfig{}, fmt.Errorf("%s is set, and the mempool does not gossip (broadcast = %v, type = %q): "+
			"every leader would name an ingress that reaches no other validator; set broadcast = true and type = %q",
			"tpu-quic-address", comet.Mempool.Broadcast, comet.Mempool.Type, cmtcfg.MempoolTypeFlood)
	}
	return cfg, nil
}

// shippedConfig is the app.toml that this binary writes. It is the SDK defaults
// with this chain's database engine, IAVL cache size, no inter-block cache,
// usvm fee floor, and pruning, snapshot and retention numbers.
func shippedConfig() *serverconfig.Config {
	cfg := serverconfig.DefaultConfig()
	// The SDK's own commands and its snapshot store find application.db
	// through this key. An empty key falls back to CometBFT's db_backend,
	// which is goleveldb.
	cfg.AppDBBackend = string(dbm.PebbleDBBackend)
	cfg.IAVLCacheSize = app.IAVLCacheSize
	// No inter-block cache. The ARC of the SDK counts values, not bytes: 1,000
	// for each store. At 10 MiB an account, that is 10 GiB for each store. The
	// node cache and the fast-node cache of IAVL under it already hold what a
	// block reads.
	cfg.InterBlockCache = false
	cfg.MinGasPrices = app.MinGasPrices
	// `custom` with these numbers gives the same PruningOptions struct as
	// `everything`. It is the only form that can ship beside snapshots, because
	// `ValidateBasic` refuses the name `everything` with a snapshot interval.
	// State sync is the only way to add a machine after block 1. The interval
	// here is the lowest that app.toml accepts, and app.New lowers it to 1.
	cfg.Pruning = pruningtypes.PruningOptionCustom
	cfg.PruningKeepRecent = "2"
	cfg.PruningInterval = "10"
	cfg.StateSync.SnapshotInterval = 5000
	cfg.StateSync.SnapshotKeepRecent = 2
	// MinRetainBlocks is the evidence window, not 1. baseapp prunes below the
	// least of its floors, and its minNonZero reads a floor of exactly zero as
	// no floor. At the height equal to the window, the window's own term is
	// zero and baseapp drops it. With a value of 1, the machine then prunes
	// below the window for good. With the window, min-retain-blocks takes the
	// early return up to that height, and past it every term is positive.
	// Zero still means never prune, and an archive raises the value.
	window, _ := svm.EvidenceParams()
	cfg.MinRetainBlocks = uint64(window)
	return cfg
}

// shippedComet is the config.toml that this binary writes. An operator's own
// file overrides it.
func shippedComet() *cmtcfg.Config {
	// CometBFT's transaction index has no purge and no reader on this chain.
	cfg := cmtcfg.DefaultConfig()
	cfg.TxIndex.Indexer = "null"
	// 400 ms of padding after a commit, so an idle block takes about as long as
	// a mainnet slot and every window a program counts in slots runs at
	// mainnet's pace. CometBFT's own default of 1 s cannot be written, because
	// the SDK rewrites exactly that default to 5 s.
	// A test checks that the value survives.
	cfg.Consensus.TimeoutCommit = 400 * time.Millisecond
	// block.max_bytes comes from a propagation budget of exactly 3 s, and this
	// setting holds that budget. The value is CometBFT's own default. svmd
	// writes it explicitly, so an operator who reads config.toml sees it.
	// requireProposeTimeout turns a lowered value into a refusal at start, not
	// missed blocks that look like a slow machine.
	cfg.Consensus.TimeoutPropose = minTimeoutPropose
	// With CometBFT's 1 GiB default, the count is the only cap. Fee order holds
	// only while the whole pool fits in one reap.
	cfg.Mempool.MaxTxsBytes = maxPoolTxsBytes(cfg.Mempool.Size)
	// CometBFT's metrics server is off by default. It serves
	// prometheus.DefaultGatherer, the process-global registry, so turning it on
	// is the whole transport. svmd adds no listener, no port and no config key.
	// The address stays CometBFT's default, and an operator can change it.
	cfg.Instrumentation.Prometheus = true
	// While a node catches up, CometBFT's blocksync pool holds up to 20 blocks
	// for each peer, and each block is up to svm.MaxBlockBytes. CometBFT's 40
	// inbound and 10 outbound peers make that 8,000 MiB beside the blocks the
	// node applies. That is more than the 20 GiB budget leaves the page cache.
	// Ten inbound peers make it 3,200 MiB. An operator with more memory raises
	// the limit in config.toml.
	cfg.P2P.MaxNumInboundPeers = maxInboundPeers
	return cfg
}

// maxInboundPeers is the shipped p2p.max_num_inbound_peers.
const maxInboundPeers = 10

// minTimeoutPropose is the propagation budget that block.max_bytes comes from.
// It is also CometBFT's default.
const minTimeoutPropose = 3 * time.Second

// requireProposeTimeout refuses to start when an operator lowered
// timeout_propose below the budget that the block size comes from. Raising it
// is allowed, because only a lower value breaks the derivation. The symptom
// would be missed blocks under load that look like a slow machine. A check at
// start is enough, because CometBFT reads timeout_propose once at start and
// the value cannot drift.
func requireProposeTimeout(cfg *cmtcfg.Config) error {
	if got := cfg.Consensus.TimeoutPropose; got < minTimeoutPropose {
		return fmt.Errorf("timeout_propose is %s in config.toml, below the %s propagation budget "+
			"block.max_bytes is derived from: lowering it costs missed blocks under load, raising it is legal",
			got, minTimeoutPropose)
	}
	return nil
}

// reapWindow is the transaction budget that CometBFT gives a proposer for a
// block with no evidence. It uses x/staking's default of 100 validators, which
// this chain does not change.
func reapWindow() int64 {
	return cmttypes.MaxDataBytes(svm.MaxBlockBytes, 0, int(stakingtypes.DefaultMaxValidators))
}

// reapFraming is the most bytes that the reap adds to a pooled transaction's
// raw length. For any transaction under 16,384 bytes, that is a field tag and
// a two-byte length varint.
const reapFraming = 3

// maxPoolTxsBytes is the largest max_txs_bytes a pool of size transactions
// can hold and still fit one reap whole: the window less each slot's framing.
func maxPoolTxsBytes(size int) int64 { return reapWindow() - reapFraming*int64(size) }

// requirePoolFitsReap refuses to start when the pool can hold more than one
// block reaps. The proposer's fee sort would then see only an arrival-order
// prefix of the pool. Like timeout_propose, both settings are read once at
// start and cannot drift.
func requirePoolFitsReap(cfg *cmtcfg.Config) error {
	size, limit := int64(cfg.Mempool.Size), maxPoolTxsBytes(cfg.Mempool.Size)
	if cfg.Mempool.MaxTxsBytes > limit {
		return fmt.Errorf("mempool max_txs_bytes is %d and size is %d in config.toml, so max_txs_bytes + 3 x size "+
			"is %d, over the %d bytes one block reaps: the fee sort would see only part of the pool. "+
			"At size %d write max_txs_bytes = %d or less",
			cfg.Mempool.MaxTxsBytes, size, cfg.Mempool.MaxTxsBytes+reapFraming*size, reapWindow(), size, limit)
	}
	return nil
}

// requireFastNodes refuses to start with IAVL's fast-node index turned off. A
// long scan of committed state (getProgramAccounts, or the token index
// rebuild) stays complete only because of that index. IAVL serves an iterator
// at the latest version from it, and that iterator reads a fixed database
// snapshot. Without the index, IAVL walks the tree, and pruning can delete
// nodes under the walk. A walk that loses a node ends with no error and
// returns part of the state as all of it.
func requireFastNodes(v interface{ GetBool(string) bool }) error {
	if v.GetBool(server.FlagDisableIAVLFastNode) {
		return fmt.Errorf("iavl-disable-fastnode is true in app.toml: a scan of committed state " +
			"needs IAVL's fast-node index to stay whole while blocks commit and prune")
	}
	return nil
}

// requireFeeFloor refuses to start unless minimum-gas-prices is a nonzero usvm
// price and nothing else. With zero, anyone who gets an account through ICS-20
// can send free Cosmos transactions that rank above every SVM transaction. Any
// other denom gives the same power to whoever holds it, because the fee check
// accepts any one listed denom.
func requireFeeFloor(v interface{ GetString(string) string }) error {
	got := v.GetString(server.FlagMinGasPrices)
	prices, err := sdk.ParseDecCoins(got)
	if err == nil && len(prices) == 1 && prices[0].Denom == app.BondDenom {
		return nil
	}
	return fmt.Errorf("minimum-gas-prices is %q in app.toml: it has to be a nonzero %s price and nothing else, "+
		"because %s is the fee only an operator can pay and the Cosmos lane outranks every SVM transaction",
		got, app.BondDenom, app.BondDenom)
}

// flagExpectedGenesisHash is Agave's flag name.
const flagExpectedGenesisHash = "expected-genesis-hash"

// requireGenesisHash refuses to start on a genesis that the operator did not
// expect. CometBFT's handshake exchanges no genesis hash, because
// `DefaultNodeInfo.CompatibleWith` compares only the block protocol version,
// the chain id and the channels. Two machines with the same chain id and
// different genesis files connect and gossip. They then fail on an app
// hash at block 1, and no error names genesis. This check gives the only
// clear error.
//
// The message is Agave's `ValidatorError::GenesisHashMismatch`, which names
// both values: "genesis hash mismatch: actual={0}, expected={1}".
func requireGenesisHash(expected, path string) error {
	if expected == "" {
		return nil
	}
	hash, err := genesis.Hash(path)
	if err != nil {
		return err
	}
	if actual := base58.Encode(hash[:]); actual != expected {
		return fmt.Errorf("genesis hash mismatch: actual=%s, expected=%s", actual, expected)
	}
	return nil
}

func main() {
	home := filepath.Join(os.Getenv("HOME"), ".svmd")
	if err := svrcmd.Execute(rootCmd(home), "SVMD", home); err != nil {
		os.Exit(1)
	}
}

// rootCmd builds the whole command tree. It is separate from main, so a test
// can run `init` as an operator does.
func rootCmd(home string) *cobra.Command {
	// The temporary app gets its own home, because app.New reads an upgrade
	// plan from the home's data directory. A shared home can hold another
	// process's plan.
	tmpHome, err := os.MkdirTemp("", "svmd")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmpHome)
	tmp, err := app.New(log.NewNopLogger(), dbm.NewMemDB(), app.Local{}, tmpHome)
	if err != nil {
		panic(err)
	}
	clientCtx := client.Context{}.
		WithCodec(tmp.Codec).
		WithInterfaceRegistry(tmp.Codec.InterfaceRegistry()).
		WithTxConfig(tmp.TxConfig).
		WithLegacyAmino(codec.NewLegacyAmino()).
		WithHomeDir(home)

	// The app.toml that svmd writes sets a 500,000-node cache for each store
	// and a Cosmos gas price in the bond denom. Only an operator can pay that
	// denom. SVM transactions pay in lamports.
	appConfig := &appConfig{Config: *shippedConfig(), SVM: defaultSVMConfig()}
	appTemplate := serverconfig.DefaultConfigTemplate + svmTemplate
	cometConfig := shippedComet()

	root := &cobra.Command{
		Use:   "svmd",
		Short: "The Solana Virtual Machine on the Cosmos SDK",
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := client.SetCmdClientContextHandler(clientCtx, cmd); err != nil {
				return err
			}
			// The SDK's version command reports four fields that ldflags set
			// and the whole Go dependency graph. These two fields belong to
			// this chain, and the SDK cannot know them. They go here, not on
			// the root command, because svrcmd.Execute builds the execution
			// context itself and discards an earlier value.
			//
			// The network comes from the compiled constant, not from
			// version.BuildTags. BuildTags is a string that a Makefile writes,
			// and it can disagree with the tags the compiler got. A
			// build-identity field that can be wrong is worse than no field.
			digest := ffi.Digest()
			cmd.SetContext(context.WithValue(cmd.Context(), version.ContextKey{}, version.ExtraInfo{
				"network":       svm.Network,
				"engine_digest": hex.EncodeToString(digest[:]),
			}))
			if err := server.InterceptConfigsPreRunHandler(cmd, appTemplate, appConfig, cometConfig); err != nil {
				return err
			}
			if cmd.Name() == "start" {
				limitGoHeap()
				svrCtx := server.GetServerContextFromCmd(cmd)
				cfg, err := readConfig(svrCtx.Viper)
				if err != nil {
					return err
				}
				if err := requireRetention(cfg); err != nil {
					return err
				}
				if err := requireProposeTimeout(svrCtx.Config); err != nil {
					return err
				}
				if err := requirePoolFitsReap(svrCtx.Config); err != nil {
					return err
				}
				// The pool tier of the signature results holds twice the
				// mempool size.
				ffi.SetSignatureResultsCap(2 * svrCtx.Config.Mempool.Size)
				if err := requireFeeFloor(svrCtx.Viper); err != nil {
					return err
				}
				if err := requireFastNodes(svrCtx.Viper); err != nil {
					return err
				}
				expected, err := cmd.Flags().GetString(flagExpectedGenesisHash)
				if err != nil {
					return err
				}
				if err := requireGenesisHash(expected, svrCtx.Config.GenesisFile()); err != nil {
					return err
				}
				// A devnet build reads its faucet key here, before anything
				// starts. In a production build, requireFaucet does nothing.
				if err := requireFaucet(cfg); err != nil {
					return err
				}
				// The collectors go on the process-global registry that
				// CometBFT's metrics server serves.
				svm.RegisterMetrics(func(err error) {
					svrCtx.Logger.Error("svm engine metrics", "err", err)
				})
			}
			return nil
		},
	}

	// Cobra prints to stderr by default, and the SDK does not change that.
	// Without this line, `svmd comet show-validator` and `svmd comet
	// show-node-id` write nothing to stdout for a shell to capture.
	root.SetOut(os.Stdout)

	basics := module.NewBasicManagerFromManager(tmp.Modules, nil)
	initCmd := genutilcli.InitCmd(basics, home)
	initCmd.Flags().StringArray(flagValidator, nil,
		"an operator as consensus-pubkey,account,self-bond,free,moniker; repeat for each")
	initCmd.Flags().String(flagValidatorsFile, "",
		"a JSON file naming every operator")
	initCmd.Flags().StringArray(flagEphemeralValidator, nil,
		"admit the ephemeral rollup validator with this pubkey, and turn on the delegation program and its companions; repeat for each")
	initCmd.Flags().StringArray(flagVRFOracle, nil,
		"admit the VRF oracle IDENTITY:VRF_PUBKEY, the key vrf-cli derive-pubkey prints, and turn on the VRF program; repeat for each")
	runInit := initCmd.RunE
	initCmd.RunE = func(cmd *cobra.Command, args []string) error {
		tuples, err := cmd.Flags().GetStringArray(flagValidator)
		if err != nil {
			return err
		}
		admitted, err := cmd.Flags().GetStringArray(flagEphemeralValidator)
		if err != nil {
			return err
		}
		ephemeral, err := keys(admitted)
		if err != nil {
			return fmt.Errorf("--%s: %w", flagEphemeralValidator, err)
		}
		pairs, err := cmd.Flags().GetStringArray(flagVRFOracle)
		if err != nil {
			return err
		}
		oracles, err := vrfOracles(pairs)
		if err != nil {
			return fmt.Errorf("--%s: %w", flagVRFOracle, err)
		}
		path, err := cmd.Flags().GetString(flagValidatorsFile)
		if err != nil {
			return err
		}
		// svmd reads the operators before runInit, so a mistyped operator
		// does not leave a half-written home directory.
		ops, err := operators(clientCtx.Codec, tuples, path)
		if err != nil {
			return err
		}
		if err := runInit(cmd, args); err != nil {
			return err
		}
		cfg := server.GetServerContextFromCmd(cmd).Config
		if err := finishGenesis(clientCtx.Codec, cfg, args[0], ops); err != nil {
			return err
		}
		if len(ephemeral) > 0 || len(oracles) > 0 {
			if err := editSVMGenesis(cfg.GenesisFile(), func(g *svm.GenesisState) error {
				if len(ephemeral) > 0 {
					admitEphemeralValidators(g, ephemeral)
				}
				if len(oracles) > 0 {
					admitVRFOracles(g, oracles)
				}
				return nil
			}); err != nil {
				return err
			}
		}
		hash, err := genesis.Hash(cfg.GenesisFile())
		if err != nil {
			return err
		}
		// getGenesisHash returns the same string, so an operator can compare
		// it with a running machine's answer.
		cmd.Printf("genesis hash: %s\n", base58.Encode(hash[:]))
		return nil
	}

	root.AddCommand(initCmd, addGenesisProgramsCmd(), addGenesisAccountCmd())
	server.AddCommandsWithStartCmdOptions(root, home, newNode, nil, server.StartCmdOptions{
		DBOpener:  openApplicationDB,
		PostSetup: startSolanaRPC,
		AddFlags: func(cmd *cobra.Command) {
			addSVMFlags(cmd)
			cmd.Flags().String(flagExpectedGenesisHash, "", "refuse to start unless genesis.json hashes to this")
		},
	})
	return root
}

// openApplicationDB opens application.db on Pebble, sized for ten million
// accounts. It refuses any other app-db-backend. svmd has one engine. The
// SDK's own commands (rollback, export, bootstrap-state,
// module-hash-by-height) and its snapshot store open their databases through
// that key, not through this opener.
func openApplicationDB(home string, backend dbm.BackendType) (dbm.DB, error) {
	if backend != dbm.PebbleDBBackend {
		return nil, fmt.Errorf("app-db-backend is %q in app.toml: svmd keeps its databases on %q, "+
			"which the SDK's own commands read from that key", backend, dbm.PebbleDBBackend)
	}
	return app.OpenApplicationDB(filepath.Join(home, "data"))
}

// addGenesisProgramsCmd writes x/svm genesis programs[] and the manifest's
// accounts from a genesis directory's manifest.json. InitGenesis checks every
// ELF against its pinned sha256. An account row replaces any genesis account at
// its address and holds the rent-exempt minimum. The command writes the
// ephemeral-rollup part's programs and accounts only when the genesis admits an
// ephemeral validator. It writes the vrf part's only when the genesis admits a
// VRF oracle.
func addGenesisProgramsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add-genesis-programs [genesis-dir]",
		Short: "Set genesis programs[] and the manifest's accounts from genesis/manifest.json",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := server.GetServerContextFromCmd(cmd).Config
			return editSVMGenesis(cfg.GenesisFile(), func(g *svm.GenesisState) error {
				var parts []string
				if len(g.EphemeralValidators) > 0 {
					parts = append(parts, genesis.EphemeralRollup)
				}
				if len(g.VRFOracles) > 0 {
					parts = append(parts, genesis.VRF)
				}
				m, err := genesis.Read(args[0], parts...)
				if err != nil {
					return err
				}
				setGenesisPrograms(g, m)
				return g.Validate()
			})
		},
	}
}

// setGenesisPrograms replaces programs[] with the manifest's programs and
// writes its accounts.
func setGenesisPrograms(g *svm.GenesisState, m genesis.Manifest) {
	g.Programs = g.Programs[:0]
	for _, p := range m.Programs {
		g.Programs = append(g.Programs, svm.GenesisProgram{Address: p.Address, Loader: p.Loader, SHA256: p.SHA256, ELF: p.ELF})
	}
	rows := map[string]bool{}
	for _, a := range m.Accounts {
		rows[a.Address] = true
	}
	g.Accounts = slices.DeleteFunc(g.Accounts, func(a svm.GenesisAccount) bool { return rows[a.Pubkey] })
	for _, a := range m.Accounts {
		g.Accounts = append(g.Accounts, svm.GenesisAccount{
			Pubkey: a.Address, Lamports: svm.MinimumBalance(len(a.Data)), Owner: a.Owner, Data: a.Data, RentEpoch: svm.RentExemptEpoch,
		})
	}
}

// addGenesisAccountCmd funds one address at genesis. A devnet uses it for the
// faucet and its cold reserve (runbook section 10, developers.md section 2).
// Nothing on the chain mints, so these rows are a devnet's whole supply. A
// program or a copied account comes from the manifest through
// add-genesis-programs.
func addGenesisAccountCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add-genesis-account [pubkey] [sol]",
		Short: "Add a System-owned account holding whole SOL to genesis accounts[]",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := server.GetServerContextFromCmd(cmd).Config
			return editSVMGenesis(cfg.GenesisFile(), func(g *svm.GenesisState) error {
				return addGenesisAccount(g, args[0], args[1])
			})
		},
	}
}

const lamportsPerSOL = 1_000_000_000

// addGenesisAccount appends the row. It refuses a pubkey that is not 32 bytes
// of base58, a balance that is not a whole number of SOL, and an address the
// genesis already holds, so a mistyped command cannot replace a program.
func addGenesisAccount(g *svm.GenesisState, pubkey, sol string) error {
	if k, err := base58.Decode(pubkey); err != nil || len(k) != 32 {
		return fmt.Errorf("svm genesis: %q is not a base58 public key", pubkey)
	}
	n, err := strconv.ParseUint(sol, 10, 64)
	if err != nil || n > ^uint64(0)/lamportsPerSOL {
		return fmt.Errorf("svm genesis: %q is not a whole number of SOL", sol)
	}
	if slices.ContainsFunc(g.Accounts, func(a svm.GenesisAccount) bool { return a.Pubkey == pubkey }) {
		return fmt.Errorf("svm genesis: %s is already in accounts[]", pubkey)
	}
	g.Accounts = append(g.Accounts, svm.GenesisAccount{
		Pubkey: pubkey, Lamports: n * lamportsPerSOL, Owner: base58.Encode(svm.SystemProgram[:]), RentEpoch: svm.RentExemptEpoch,
	})
	return g.Validate()
}

// admitEphemeralValidators lists the keys in genesis and writes the fees vault
// of each key and of the protocol. A vault replaces any account at its
// address.
func admitEphemeralValidators(g *svm.GenesisState, keys [][32]byte) {
	vaults := []svm.GenesisAccount{svm.FeesVault(svm.ProtocolFeesVault())}
	for _, k := range keys {
		g.EphemeralValidators = append(g.EphemeralValidators, base58.Encode(k[:]))
		vaults = append(vaults, svm.FeesVault(svm.ValidatorFeesVault(k)))
	}
	g.Accounts = slices.DeleteFunc(g.Accounts, func(a svm.GenesisAccount) bool {
		return slices.ContainsFunc(vaults, func(v svm.GenesisAccount) bool { return v.Pubkey == a.Pubkey })
	})
	g.Accounts = append(g.Accounts, vaults...)
}

// admitVRFOracles lists the oracles in genesis and writes the VRF program's
// oracle list and each oracle's data account. Each written account replaces
// any account at its address.
func admitVRFOracles(g *svm.GenesisState, oracles [][2][32]byte) {
	var identities [][32]byte
	var written []svm.GenesisAccount
	for _, o := range oracles {
		g.VRFOracles = append(g.VRFOracles, svm.VRFOracle{Identity: base58.Encode(o[0][:]), VRFPubkey: base58.Encode(o[1][:])})
		identities = append(identities, o[0])
		written = append(written, svm.OracleData(o[0], o[1]))
	}
	written = append(written, svm.OracleList(identities))
	g.Accounts = slices.DeleteFunc(g.Accounts, func(a svm.GenesisAccount) bool {
		return slices.ContainsFunc(written, func(w svm.GenesisAccount) bool { return w.Pubkey == a.Pubkey })
	})
	g.Accounts = append(g.Accounts, written...)
}

// vrfOracles decodes IDENTITY:VRF_PUBKEY pairs.
func vrfOracles(in []string) ([][2][32]byte, error) {
	out := make([][2][32]byte, 0, len(in))
	for _, s := range in {
		identity, key, ok := strings.Cut(s, ":")
		if !ok {
			return nil, fmt.Errorf("%q is not IDENTITY:VRF_PUBKEY", s)
		}
		pair, err := keys([]string{identity, key})
		if err != nil {
			return nil, err
		}
		out = append(out, [2][32]byte{pair[0], pair[1]})
	}
	return out, nil
}

// keys decodes base58 pubkeys.
func keys(in []string) ([][32]byte, error) {
	out := make([][32]byte, 0, len(in))
	for _, s := range in {
		b, err := base58.Decode(s)
		if err != nil || len(b) != 32 {
			return nil, fmt.Errorf("%q is not a base58 pubkey", s)
		}
		out = append(out, [32]byte(b))
	}
	return out, nil
}

// editSVMGenesis rewrites the x/svm section of the genesis file at path.
func editSVMGenesis(path string, edit func(*svm.GenesisState) error) error {
	appGenesis, err := genutiltypes.AppGenesisFromFile(path)
	if err != nil {
		return err
	}
	var state map[string]jsontext.Value
	if err := json.Unmarshal(appGenesis.AppState, &state); err != nil {
		return err
	}
	var g svm.GenesisState
	if err := json.Unmarshal(state[svm.ModuleName], &g); err != nil {
		return err
	}
	if err := edit(&g); err != nil {
		return err
	}
	if state[svm.ModuleName], err = json.Marshal(g, json.Deterministic(true)); err != nil {
		return err
	}
	if appGenesis.AppState, err = json.Marshal(state, json.Deterministic(true)); err != nil {
		return err
	}
	return appGenesis.SaveAs(path)
}

// finishGenesis bonds the named operators and writes the block and evidence
// limits.
//
// x/staking owns the validator set, so finishGenesis writes each machine into
// staking genesis and into the CometBFT file at matching power. baseapp
// compares the validators that InitGenesis returns with the file's list, so
// the two must agree.
//
// With no operators named, the one operator is this machine, and its account
// comes from its own consensus key. pv.Key.Address is the same truncated hash
// as the address of an ed25519 account key. The ante handler accepts ed25519,
// so the consensus key signs for this account and no other key does. This is
// acceptable only here, where there is no address to supply. A named operator
// supplies its own bech32 account, which keeps the consensus key out of
// transactions.
func finishGenesis(cdc codec.Codec, cfg *cmtcfg.Config, moniker string, operators []app.Operator) error {
	doc, err := genutiltypes.AppGenesisFromFile(cfg.GenesisFile())
	if err != nil {
		return err
	}
	if len(operators) == 0 {
		pv := privval.LoadFilePVEmptyState(cfg.PrivValidatorKeyFile(), cfg.PrivValidatorStateFile())
		pubkey, err := cryptocodec.FromCmtPubKeyInterface(pv.Key.PubKey)
		if err != nil {
			return err
		}
		operators = []app.Operator{{
			Account:         sdk.AccAddress(pv.Key.Address),
			ConsensusPubkey: pubkey,
			SelfBond:        math.NewInt(10_000_000),
			Free:            math.NewInt(90_000_000),
			Moniker:         moniker,
		}}
	}

	var state map[string]jsontext.Value
	if err := json.Unmarshal(doc.AppState, &state); err != nil {
		return err
	}
	if err := app.Permission(cdc, state, operators); err != nil {
		return err
	}
	if doc.AppState, err = json.Marshal(state, json.Deterministic(true)); err != nil {
		return err
	}

	doc.Consensus.Validators = nil
	for _, op := range operators {
		pubkey, err := cryptocodec.ToCmtPubKeyInterface(op.ConsensusPubkey)
		if err != nil {
			return err
		}
		doc.Consensus.Validators = append(doc.Consensus.Validators, cmttypes.GenesisValidator{
			Address: pubkey.Address(),
			PubKey:  pubkey,
			Power:   sdk.TokensToConsensusPower(op.SelfBond, sdk.DefaultPowerReduction),
			Name:    op.Moniker,
		})
	}
	doc.Consensus.Params.Block.MaxBytes, doc.Consensus.Params.Block.MaxGas = svm.BlockParams()
	doc.Consensus.Params.Evidence.MaxAgeNumBlocks, doc.Consensus.Params.Evidence.MaxAgeDuration = svm.EvidenceParams()
	return doc.SaveAs(cfg.GenesisFile())
}
