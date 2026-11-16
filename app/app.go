// Package app wires the Cosmos SDK modules and SVM execution into one chain.
// The two execution paths share blocks but keep separate transaction and fee
// handling.
package app

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	goruntime "runtime"
	"sync"
	"sync/atomic"

	abci "github.com/cometbft/cometbft/abci/types"
	dbm "github.com/cosmos/cosmos-db"
	"google.golang.org/protobuf/reflect/protoreflect"

	"cosmossdk.io/log/v2"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/codec/address"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/runtime"
	"github.com/cosmos/cosmos-sdk/std"
	"github.com/cosmos/cosmos-sdk/store/v2/cachekv"
	"github.com/cosmos/cosmos-sdk/store/v2/cachemulti"
	"github.com/cosmos/cosmos-sdk/store/v2/iavl"
	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	"github.com/cosmos/cosmos-sdk/x/auth"
	authante "github.com/cosmos/cosmos-sdk/x/auth/ante"
	authkeeper "github.com/cosmos/cosmos-sdk/x/auth/keeper"
	authtx "github.com/cosmos/cosmos-sdk/x/auth/tx"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/cosmos/cosmos-sdk/x/bank"
	bankkeeper "github.com/cosmos/cosmos-sdk/x/bank/keeper"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/cosmos-sdk/x/consensus"
	consensuskeeper "github.com/cosmos/cosmos-sdk/x/consensus/keeper"
	consensustypes "github.com/cosmos/cosmos-sdk/x/consensus/types"
	"github.com/cosmos/cosmos-sdk/x/evidence"
	evidencekeeper "github.com/cosmos/cosmos-sdk/x/evidence/keeper"
	evidencetypes "github.com/cosmos/cosmos-sdk/x/evidence/types"
	"github.com/cosmos/cosmos-sdk/x/gov"
	govkeeper "github.com/cosmos/cosmos-sdk/x/gov/keeper"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	govv1beta1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1beta1"
	"github.com/cosmos/cosmos-sdk/x/slashing"
	slashingkeeper "github.com/cosmos/cosmos-sdk/x/slashing/keeper"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	"github.com/cosmos/cosmos-sdk/x/staking"
	stakingkeeper "github.com/cosmos/cosmos-sdk/x/staking/keeper"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	txsigning "github.com/cosmos/cosmos-sdk/x/tx/signing"
	"github.com/cosmos/cosmos-sdk/x/upgrade"
	upgradekeeper "github.com/cosmos/cosmos-sdk/x/upgrade/keeper"
	upgradetypes "github.com/cosmos/cosmos-sdk/x/upgrade/types"
	"github.com/cosmos/gogoproto/proto"
	"github.com/cosmos/ibc-go/v11/modules/apps/transfer"
	ibctransferkeeper "github.com/cosmos/ibc-go/v11/modules/apps/transfer/keeper"
	ibctransfertypes "github.com/cosmos/ibc-go/v11/modules/apps/transfer/types"
	ibc "github.com/cosmos/ibc-go/v11/modules/core"
	porttypes "github.com/cosmos/ibc-go/v11/modules/core/05-port/types"
	ibcante "github.com/cosmos/ibc-go/v11/modules/core/ante"
	ibcapi "github.com/cosmos/ibc-go/v11/modules/core/api"
	ibcexported "github.com/cosmos/ibc-go/v11/modules/core/exported"
	ibckeeper "github.com/cosmos/ibc-go/v11/modules/core/keeper"
	ibctm "github.com/cosmos/ibc-go/v11/modules/light-clients/07-tendermint"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// Name is the app name that baseapp reports.
const Name = "svmd"

// IAVLCacheSize is the node cache size of each store.
const IAVLCacheSize = 500_000

// App is the ABCI application. It embeds baseapp and holds the module
// keepers and the SVM stores.
type App struct {
	*baseapp.BaseApp
	Codec    codec.Codec
	TxConfig client.TxConfig
	Stores   svm.Stores
	Modules  *module.Manager
	// History is the machine-local transaction index, or nil when disabled.
	History *svm.History
	// Tokens is the machine-local token owner index, or nil when disabled.
	Tokens *svm.TokenIndex
	// Feed publishes block write sets to RPC subscribers.
	Feed     *svm.Feed
	Upgrades *upgradekeeper.Keeper
	Accounts authkeeper.AccountKeeper
	Bank     bankkeeper.BaseKeeper
	Staking  *stakingkeeper.Keeper
	Slashing slashingkeeper.Keeper
	Gov      *govkeeper.Keeper
	IBC      *ibckeeper.Keeper
	Transfer *ibctransferkeeper.Keeper
	// dedup is the replay set of processed transactions. vouch checks it
	// against the dedup commitment in state.
	dedup *svm.Dedup
	// runner is the block runner. Commit waits for its sidecar writes.
	runner *svm.Runner
	// chunks is the number of chunks in the snapshot being restored.
	chunks uint32
	// committedHeight caches the latest committed block height.
	committedHeight atomic.Int64
	// keys are the stores that the app mounts. Each one is an IAVL store.
	keys []storetypes.StoreKey
	// committed and next are the two views of the latest committed height.
	// Each is loaded once. next also holds the begin-block of the next block.
	// Every read takes a branch of one of them. nextMu lets only one request
	// at a time build next, so the engine gets the next heights in order.
	committed, next atomic.Pointer[view]
	nextMu          sync.Mutex
	// reading is held for each read of the stores of a view. Commit holds it
	// while the stores save the next version.
	reading sync.RWMutex
	// commitMu stops commits while a scan opens the latest state. Lock order
	// is commitMu, then reading.
	commitMu sync.RWMutex
}

// BondDenom is the staking and governance deposit denomination.
const BondDenom = "usvm"

// MinGasPrices sets the minimum Cosmos transaction fee.
const MinGasPrices = "0.000001" + BondDenom

// moduleAccounts defines the permissions granted to each module account.
var moduleAccounts = map[string][]string{
	authtypes.FeeCollectorName:     nil,
	stakingtypes.BondedPoolName:    {authtypes.Burner, authtypes.Staking},
	stakingtypes.NotBondedPoolName: {authtypes.Burner, authtypes.Staking},
	govtypes.ModuleName:            {authtypes.Burner},
	ibctransfertypes.ModuleName:    {authtypes.Minter, authtypes.Burner},
}

// Local configures machine-local databases and upgrade behavior.
type Local struct {
	Dedup dbm.DB
	// History stores transaction records up to IndexMaxBytes.
	History       dbm.DB
	IndexMaxBytes uint64
	// Tokens stores the token owner index.
	Tokens dbm.DB
	// SkipUpgrades holds the heights from --unsafe-skip-upgrades.
	SkipUpgrades map[int64]bool
}

// New builds the app over db and the databases in local, loads the latest
// state and vouches for it.
func New(logger log.Logger, db dbm.DB, local Local, home string, options ...func(*baseapp.BaseApp)) (*App, error) {
	registry, err := codectypes.NewInterfaceRegistryWithOptions(codectypes.InterfaceRegistryOptions{
		ProtoFiles: proto.HybridResolver,
		SigningOptions: txsigning.Options{
			AddressCodec:          address.NewBech32Codec(sdk.Bech32MainPrefix),
			ValidatorAddressCodec: address.NewBech32Codec(sdk.Bech32PrefixValAddr),
			CustomGetSigners: map[protoreflect.FullName]txsigning.GetSignersFunc{
				svm.GetSigners.MsgType: svm.GetSigners.Fn,
			},
		},
	})
	if err != nil {
		return nil, err
	}
	// Staking genesis carries a validator's consensus pubkey as an Any. The
	// registry must know the crypto types before svmd decodes any genesis.
	cryptocodec.RegisterInterfaces(registry)
	std.RegisterInterfaces(registry)
	cdc := codec.NewProtoCodec(registry)
	txConfig := authtx.NewTxConfig(cdc, authtx.DefaultSignModes)
	// The app decoder refuses an SVM transaction in any envelope other than
	// its canonical one.
	decode := svm.Decoder(txConfig)

	options = append([]func(*baseapp.BaseApp){baseapp.SetIAVLCacheSize(IAVLCacheSize)}, options...)
	base := baseapp.NewBaseApp(Name, logger, db, decode, options...)
	base.SetInterfaceRegistry(registry)
	base.SetTxEncoder(txConfig.TxEncoder())

	a := &App{BaseApp: base, Codec: cdc, TxConfig: txConfig, Stores: svm.NewStores()}
	// Every authority on this chain is the governance module account.
	authority := authtypes.NewModuleAddress(govtypes.ModuleName).String()
	consensusKey := storetypes.NewKVStoreKey(consensustypes.StoreKey)
	consensusKeeper := consensuskeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(consensusKey),
		authority,
		runtime.EventService{},
	)
	base.SetParamStore(consensusKeeper.ParamsStore)

	upgradeKey := storetypes.NewKVStoreKey(upgradetypes.StoreKey)
	skip := local.SkipUpgrades
	if skip == nil {
		skip = map[int64]bool{}
	}
	a.Upgrades = upgradekeeper.NewKeeper(skip, runtime.NewKVStoreService(upgradeKey), cdc, home, base, authority)

	authKey := storetypes.NewKVStoreKey(authtypes.StoreKey)
	bankKey := storetypes.NewKVStoreKey(banktypes.StoreKey)
	stakingKey := storetypes.NewKVStoreKey(stakingtypes.StoreKey)
	a.Accounts = authkeeper.NewAccountKeeper(cdc, runtime.NewKVStoreService(authKey), authtypes.ProtoBaseAccount,
		moduleAccounts, address.NewBech32Codec(sdk.Bech32MainPrefix), sdk.Bech32MainPrefix, authority)
	// Bank blocks only the module account addresses, as on every chain.
	blocked := map[string]bool{}
	for name := range moduleAccounts {
		blocked[authtypes.NewModuleAddress(name).String()] = true
	}
	a.Bank = bankkeeper.NewBaseKeeper(cdc, runtime.NewKVStoreService(bankKey), a.Accounts, blocked, authority, logger)
	a.Staking = stakingkeeper.NewKeeper(cdc, runtime.NewKVStoreService(stakingKey), a.Accounts, a.Bank, authority,
		address.NewBech32Codec(sdk.Bech32PrefixValAddr), address.NewBech32Codec(sdk.Bech32PrefixConsAddr))
	slashingKey := storetypes.NewKVStoreKey(slashingtypes.StoreKey)
	evidenceKey := storetypes.NewKVStoreKey(evidencetypes.StoreKey)
	a.Slashing = slashingkeeper.NewKeeper(cdc, codec.NewLegacyAmino(), runtime.NewKVStoreService(slashingKey), a.Staking, authority)
	// Without these hooks, a validator that bonds after genesis gets no
	// signing info and cannot be jailed.
	a.Staking.SetHooks(a.Slashing.Hooks())
	evidenceKeeper := evidencekeeper.NewKeeper(cdc, runtime.NewKVStoreService(evidenceKey), a.Staking, a.Slashing,
		address.NewBech32Codec(sdk.Bech32MainPrefix), runtime.ProvideCometInfoService())

	evidenceKeeper.SetRouter(evidencetypes.NewRouter())

	govKey := storetypes.NewKVStoreKey(govtypes.StoreKey)
	// The distribution keeper is nil. That is safe because proposal_cancel_dest
	// is empty. The only guard is in the SDK's gov genesis code. It panics only
	// when that destination is the distribution module address.
	a.Gov = govkeeper.NewKeeper(cdc, runtime.NewKVStoreService(govKey), a.Accounts, a.Bank, nil,
		base.MsgServiceRouter(), govtypes.DefaultConfig(), authority,
		govkeeper.NewDefaultCalculateVoteResultsAndVotingPower(a.Staking))
	// MsgSubmitProposal and MsgExecLegacyContent both call
	// `k.legacyRouter.HasRoute`. With no router set, that is a nil dereference
	// on a message that any account can send. An empty sealed router returns
	// ErrNoProposalHandlerExists, which is correct because this chain
	// registers no legacy proposal handlers.
	a.Gov.SetLegacyRouter(govv1beta1.NewRouter())

	// IBC uses classic channels only, because the supported relayer builds no
	// v2 packets. An empty v2 router answers an unknown port with a failed
	// transaction. A nil v2 router causes a nil dereference. The client keeper
	// routes 09-localhost itself.
	ibcKey := storetypes.NewKVStoreKey(ibcexported.StoreKey)
	transferKey := storetypes.NewKVStoreKey(ibctransfertypes.StoreKey)
	a.IBC = ibckeeper.NewKeeper(cdc, runtime.NewKVStoreService(ibcKey), a.Upgrades, authority)
	a.Transfer = ibctransferkeeper.NewKeeper(cdc, address.NewBech32Codec(sdk.Bech32MainPrefix),
		runtime.NewKVStoreService(transferKey), a.IBC.ChannelKeeper, base.MsgServiceRouter(), a.Accounts, a.Bank, authority)
	ibcRouter := porttypes.NewRouter()
	ibcRouter.AddRoute(ibctransfertypes.ModuleName, strandGuard{IBCModule: transfer.NewIBCModule(a.Transfer), bank: a.Bank})
	a.IBC.SetRouter(ibcRouter)
	a.IBC.SetRouterV2(ibcapi.NewRouter())
	tendermint := ibctm.NewLightClientModule(cdc, a.IBC.ClientKeeper.GetStoreProvider())
	a.IBC.ClientKeeper.AddRoute(ibctm.ModuleName, &tendermint)

	svmModule := svm.AppModule{Stores: a.Stores, Authority: authority}
	a.Modules = module.NewManager(
		auth.NewAppModule(cdc, a.Accounts, nil, nil),
		bank.NewAppModule(cdc, a.Bank, a.Accounts, nil),
		staking.NewAppModule(cdc, a.Staking, a.Accounts, a.Bank, nil),
		slashing.NewAppModule(cdc, a.Slashing, a.Accounts, a.Bank, a.Staking, nil, registry),
		evidence.NewAppModule(*evidenceKeeper),
		consensus.NewAppModule(cdc, consensusKeeper),
		upgrade.NewAppModule(a.Upgrades, address.NewBech32Codec(sdk.Bech32MainPrefix)),
		gov.NewAppModule(cdc, a.Gov, a.Accounts, a.Bank, nil),
		ibc.NewAppModule(a.IBC),
		transfer.NewAppModule(a.Transfer),
		ibctm.NewAppModule(tendermint),
		svmModule,
	)
	module.NewBasicManagerFromManager(a.Modules, nil).RegisterInterfaces(registry)
	a.Modules.SetOrderPreBlockers(upgradetypes.ModuleName, authtypes.ModuleName)
	// x/slashing and x/evidence run before x/staking, so a jail takes effect
	// in the validator update of the same block. The only begin-block work of
	// ibc is the consensus state of an upgrade plan, which no earlier module
	// writes.
	a.Modules.SetOrderBeginBlockers(svm.ModuleName, slashingtypes.ModuleName, evidencetypes.ModuleName,
		stakingtypes.ModuleName, ibcexported.ModuleName)
	// x/gov runs before x/staking, so a proposal that changes a staking
	// parameter takes effect in the validator update of the same block, not
	// the next one.
	a.Modules.SetOrderEndBlockers(govtypes.ModuleName, stakingtypes.ModuleName, banktypes.ModuleName)
	// The x/auth accounts come first, then the bank balances that refer to
	// them, then the delegations that move those balances. x/slashing comes
	// next, because it reads the validators that x/staking wrote. transfer
	// writes only its own store, after ibc. x/svm depends on none of them,
	// because lamports are not a bank denom.
	a.Modules.SetOrderInitGenesis(authtypes.ModuleName, banktypes.ModuleName, stakingtypes.ModuleName,
		slashingtypes.ModuleName, evidencetypes.ModuleName,
		govtypes.ModuleName, ibcexported.ModuleName, ibctransfertypes.ModuleName,
		consensustypes.ModuleName, upgradetypes.ModuleName, svm.ModuleName)
	a.Modules.SetOrderExportGenesis(authtypes.ModuleName, banktypes.ModuleName, stakingtypes.ModuleName,
		slashingtypes.ModuleName, evidencetypes.ModuleName,
		govtypes.ModuleName, ibcexported.ModuleName, ibctransfertypes.ModuleName,
		upgradetypes.ModuleName, svm.ModuleName)
	cfg := module.NewConfigurator(cdc, base.MsgServiceRouter(), base.GRPCQueryRouter())
	if err := a.Modules.RegisterServices(cfg); err != nil {
		return nil, err
	}

	// The next block's context, or nil before the first block commits.
	next := func() (storetypes.MultiStore, *svm.Next, error) {
		if a.committedHeight.Load() == 0 {
			return nil, nil, nil
		}
		return a.Next()
	}
	runner := &svm.Runner{Stores: a.Stores, Decode: decode, State: a.Latest, Next: next, Log: logger}
	a.runner = runner
	base.SetBlockSTMTxRunner(runner)
	base.SetCheckTxHandler(runner.CheckTx)
	// svmd sets the mempool and both proposal handlers after the options, so
	// an operator's max-txs cannot replace them with NoOpMempool and turn off
	// block validation.
	pool := svm.Passthrough{Decode: decode, Stores: a.Stores, Next: next}
	proposals := svm.Proposals{Decode: decode, Encode: txConfig.TxEncoder(), Mempool: pool, Stores: a.Stores, Next: next,
		VerifyPrepared: base.PrepareProposalVerifyTx, VerifyProposed: base.ProcessProposalVerifyTx}
	base.SetMempool(pool)
	base.SetPrepareProposal(proposals.Prepare)
	base.SetProcessProposal(proposals.Process)
	base.SetInitChainer(a.initChainer)
	base.SetPreBlocker(func(ctx sdk.Context, _ *abci.RequestFinalizeBlock) (*sdk.ResponsePreBlock, error) {
		return a.Modules.PreBlock(ctx)
	})
	base.SetBeginBlocker(a.Modules.BeginBlock)
	base.SetEndBlocker(func(ctx sdk.Context) (sdk.EndBlock, error) {
		res, err := a.Modules.EndBlock(ctx)
		if err == nil {
			// The block's last writer has run and the SDK's workingHash is
			// next, so the shards flush and hash here, eight at a time
			// (svm.Stores.FlushShards). Commit saves them the same way.
			a.Stores.FlushShards(a.CommitMultiStore(), ctx.MultiStore())
		}
		return res, err
	})
	// The ante handler applies to Cosmos transactions only. An SVM transaction
	// never reaches it, because the TxRunner sends MsgSVMTransaction directly
	// to the engine. The engine charges lamports, not Cosmos gas.
	ante, err := authante.NewAnteHandler(authante.HandlerOptions{
		AccountKeeper:   a.Accounts,
		BankKeeper:      a.Bank,
		SignModeHandler: txConfig.SignModeHandler(),
		SigGasConsumer:  authante.DefaultSigVerificationGasConsumer,
	})
	if err != nil {
		return nil, err
	}
	// The redundant relay decorator refuses a relay that another operator
	// already made, at CheckTx and ReCheckTx. It stops the relay before it
	// uses the units that a Cosmos transaction takes ahead of every SVM
	// transaction. It runs after the auth ante chain, as in the ibc-go
	// reference app, so svmd verifies proofs only for a transaction that paid
	// the fee floor. A redundant relay that still reaches a block is a no-op
	// in IBC core.
	relay := ibcante.NewRedundantRelayDecorator(a.IBC)
	base.SetAnteHandler(func(ctx sdk.Context, tx sdk.Tx, simulate bool) (sdk.Context, error) {
		ctx, err := ante(ctx, tx, simulate)
		if err != nil {
			return ctx, err
		}
		return relay.AnteHandle(ctx, tx, simulate, func(ctx sdk.Context, _ sdk.Tx, _ bool) (sdk.Context, error) {
			return ctx, nil
		})
	})

	keys := append(a.Stores.All(), consensusKey, upgradeKey, authKey, bankKey, stakingKey, slashingKey, evidenceKey, govKey,
		ibcKey, transferKey)
	for _, k := range keys {
		base.MountStore(k, storetypes.StoreTypeIAVL)
	}
	a.keys = keys
	if err := base.LoadLatestVersion(); err != nil {
		return nil, err
	}
	// svmd prunes at every height. app.toml cannot set this value, because
	// `PruningOptions.Validate` sets a minimum interval of 10 and
	// `GetPruningOptionsFromFlags` calls it for the `custom` strategy. A
	// baseapp option cannot set it either, because `LoadLatestVersion`
	// validates the options again at its end. The operator's strategy
	// and keep-recent pass both validations unchanged, and svmd lowers the
	// interval here, after them.
	//
	// Nothing validates the options after this point.
	// `Manager.GetPruningHeight` only calculates `height % Interval`, so an
	// interval of 1 makes every height a prune height.
	pruning := base.CommitMultiStore().GetPruning()
	pruning.Interval = 1
	base.CommitMultiStore().SetPruning(pruning)
	committed := uint64(base.LastBlockHeight())
	a.committedHeight.Store(int64(committed))
	if local.Dedup != nil {
		if runner.Dedup, err = svm.OpenDedup(local.Dedup, committed); err != nil {
			return nil, err
		}
		// DedupSnapshot carries the replay set through state sync. If the
		// operator turned snapshots off, no snapshot manager exists and svmd
		// registers nothing. The config check refuses that setting on a
		// machine that other machines bootstrap from.
		if m := base.SnapshotManager(); m != nil {
			if err := m.RegisterExtensions(svm.DedupSnapshot{Dedup: runner.Dedup}); err != nil {
				return nil, err
			}
		}
	}
	a.dedup = runner.Dedup
	if local.History != nil {
		if runner.History, err = svm.OpenHistory(local.History, committed, local.IndexMaxBytes); err != nil {
			return nil, err
		}
		a.History = runner.History
	}
	a.Feed = svm.NewFeed()
	runner.Feed = a.Feed
	if local.Tokens != nil {
		if runner.Tokens, err = svm.OpenTokenIndex(local.Tokens, logger); err != nil {
			return nil, err
		}
		// The runner rebuilds the index on the first block whose marker does
		// not match, because the account stores are available there.
		a.Tokens = runner.Tokens
	}
	if err := a.vouch(); err != nil {
		return nil, err
	}
	return a, nil
}

// vouch checks the state that a machine holds before the machine signs
// anything against it, and then gives the engine the gates of that state. New
// runs it on the state it loaded. ApplySnapshotChunk runs it on the state that
// a snapshot restored, because a machine that joins by state sync does not
// pass through New again.
func (a *App) vouch() error {
	ms := a.CommitMultiStore()
	committed := a.LastBlockHeight()
	// Refuse the wrong architecture, an engine that the chain did not adopt,
	// or the other network.
	if err := svm.Assert(goruntime.GOARCH, ffi.Digest(), a.Stores.EngineDigest(ms), a.Stores.ChainNetwork(ms)); err != nil {
		return err
	}
	// Refuse a replay set that is not the one the chain committed to. A
	// restore that drops the extension with no error leaves a machine that
	// accepts replays. The SDK does not check for this.
	if err := svm.AssertDedup(uint64(committed), a.dedup, a.Stores.DedupCommitment(ms)); err != nil {
		return err
	}
	if committed > 0 {
		return a.configureEngine(ms)
	}
	return nil
}

// OfferSnapshot calls the baseapp OfferSnapshot and records the chunk count of
// an accepted snapshot. ApplySnapshotChunk uses the count to find the last
// chunk.
func (a *App) OfferSnapshot(req *abci.RequestOfferSnapshot) (*abci.ResponseOfferSnapshot, error) {
	res, err := a.BaseApp.OfferSnapshot(req)
	if err == nil && res.Result == abci.ResponseOfferSnapshot_ACCEPT {
		a.chunks = req.Snapshot.Chunks
	}
	return res, err
}

// ApplySnapshotChunk calls the baseapp ApplySnapshotChunk. After the last
// chunk restores the state, it runs the checks that New runs on a loaded
// state. CometBFT restores a snapshot into the running app and goes directly
// to block sync, so a machine that joins by state sync never reopens.
//
// The SDK returns the last chunk only after the whole restore finishes,
// extensions included. The ACCEPT of that chunk marks the end of the
// restore. An earlier ACCEPT does not, because the multistore can be loaded
// before the dedup set arrives.
func (a *App) ApplySnapshotChunk(req *abci.RequestApplySnapshotChunk) (*abci.ResponseApplySnapshotChunk, error) {
	res, err := a.BaseApp.ApplySnapshotChunk(req)
	if err != nil || res.Result != abci.ResponseApplySnapshotChunk_ACCEPT || req.Index+1 != a.chunks {
		return res, err
	}
	restored := uint64(a.LastBlockHeight())
	a.committedHeight.Store(int64(restored))
	if a.History != nil {
		if err := a.History.Restored(restored); err != nil {
			return nil, err
		}
	}
	if err := a.vouch(); err != nil {
		a.Logger().Error("svm: refusing the state a snapshot restored", "height", restored, "err", err)
		return &abci.ResponseApplySnapshotChunk{Result: abci.ResponseApplySnapshotChunk_ABORT}, nil
	}
	return res, nil
}

// Commit calls the baseapp Commit and then records the committed height. No
// read of a view runs while it saves.
func (a *App) Commit() (*abci.ResponseCommit, error) {
	a.commitMu.Lock()
	defer a.commitMu.Unlock()
	a.reading.Lock()
	defer a.reading.Unlock()
	a.Stores.SaveShards(a.CommitMultiStore())
	// The block's sidecar writes ran beside the flush and the save. They land
	// before the height does (svm.Runner.Wait).
	if err := a.runner.Wait(); err != nil {
		return nil, err
	}
	res, err := a.BaseApp.Commit()
	if err == nil {
		a.committedHeight.Store(a.LastBlockHeight())
	}
	return res, err
}

// Latest returns the latest committed state and its height. It returns nil
// before the first block commits, because no blockhash window exists then to
// check against. CheckTx and the token index rebuild read through it.
func (a *App) Latest() (storetypes.MultiStore, uint64, error) {
	if a.committedHeight.Load() == 0 {
		return nil, 0, nil
	}
	return a.Committed()
}

// ScanLatest opens a scan of every account in the latest committed version.
// No block commits while it opens the scan, so the scan reads all of that
// version, however long it runs.
func (a *App) ScanLatest() (storetypes.MultiStore, uint64, *svm.AccountScan, error) {
	a.commitMu.RLock()
	defer a.commitMu.RUnlock()
	ms, h, err := a.Committed()
	if err != nil {
		return nil, 0, nil, err
	}
	return ms, h, a.Stores.OpenAccountScan(ms), nil
}

// Processed reports whether the replay set holds a dedup key. RPC simulation
// uses it where Agave uses its status cache. The set has its own lock, which
// CheckTx already relies on when it reads the set while a block executes.
func (a *App) Processed(key [64]byte) bool { return a.dedup != nil && a.dedup.Has(key) }

// CommittedHeight returns the latest committed height without opening a
// state. LastBlockHeight returns the same number, but it hashes the merkle
// root of the commit info twice on each call. getHealth runs on every send
// and must not pay that cost.
func (a *App) CommittedHeight() uint64 { return uint64(a.committedHeight.Load()) }

// Committed returns the latest committed state and its height. CheckTx and
// the RPC both read through it, never through the block in progress.
func (a *App) Committed() (storetypes.MultiStore, uint64, error) {
	v, err := a.committedView()
	if err != nil {
		return nil, 0, err
	}
	return v.branch(), uint64(v.height), nil
}

// view is the latest committed state at one height. It is loaded once, and
// every read at that height shares it. To load a version, the store reads the
// root of each IAVL store from the database. A new load for each read, with a
// cache around each store, was three quarters of the cost to recheck a
// transaction. A view holds the loaded stores, which panic on a write. The
// next view also holds the writes of the begin-block over them
// (svm.Next.Over). Nothing in a view changes after it is published, and a
// view caches no read. So readers on many goroutines can share it, and it
// holds no more at the end of a height than at the start.
type view struct {
	height int64
	stores map[storetypes.StoreKey]storetypes.KVStore
	// next is the begin-block of the next block, which is already in stores.
	// It is nil in the committed view.
	next *svm.Next
}

// branch returns the branch of v for one request. The branch puts a cache
// around a store when the request first reads that store. It keeps what the
// request reads and writes.
func (v *view) branch() storetypes.MultiStore {
	return cachemulti.NewFromParent(func(k storetypes.StoreKey) storetypes.CacheWrapper { return v.stores[k] })
}

// held is a loaded store. Each read of it holds App.reading, and Commit holds
// it while the stores save the next version. IAVL reads the latest version
// from its fast-node table. A save writes that table before it moves the
// latest version. Without the lock, a read in between gets the values of the
// next version. An iterator opens under the lock and reads after it. That is
// safe because goleveldb pins a snapshot when IAVL opens one, and the tree
// nodes of a version never change.
type held struct {
	storetypes.KVStore
	reading *sync.RWMutex
}

func (h held) Get(key []byte) []byte {
	h.reading.RLock()
	defer h.reading.RUnlock()
	return h.KVStore.Get(key)
}

func (h held) Has(key []byte) bool {
	h.reading.RLock()
	defer h.reading.RUnlock()
	return h.KVStore.Has(key)
}

func (h held) Iterator(start, end []byte) storetypes.Iterator {
	h.reading.RLock()
	defer h.reading.RUnlock()
	return h.KVStore.Iterator(start, end)
}

func (h held) ReverseIterator(start, end []byte) storetypes.Iterator {
	h.reading.RLock()
	defer h.reading.RUnlock()
	return h.KVStore.ReverseIterator(start, end)
}

// CacheWrap returns the cache of a request over h, so the reads of the
// request go through h.
func (h held) CacheWrap() storetypes.CacheWrap { return cachekv.NewStore(h) }

// committedView returns the view of the latest committed height. If the
// published view is for a different height, it loads the view of the current
// height. Two loads at the same time both publish, and each one holds the
// state of the height.
func (a *App) committedView() (*view, error) {
	h := a.committedHeight.Load()
	if h == 0 {
		return nil, fmt.Errorf("svm: no block committed yet")
	}
	if v := a.committed.Load(); v != nil && v.height == h {
		return v, nil
	}
	stores := make(map[storetypes.StoreKey]storetypes.KVStore, len(a.keys))
	for _, k := range a.keys {
		s, ok := a.CommitMultiStore().GetStore(k).(*iavl.Store)
		if !ok {
			return nil, fmt.Errorf("svm: store %s is not an IAVL store", k.Name())
		}
		loaded, err := s.GetImmutable(h)
		if err != nil {
			return nil, fmt.Errorf("svm: store %s at height %d: %w", k.Name(), h, err)
		}
		stores[k] = held{loaded, &a.reading}
	}
	v := &view{height: h, stores: stores}
	a.committed.Store(v)
	return v, nil
}

// Next returns a branch of committed state as the begin-block of the next
// block leaves it, and that block. A transaction sent now first executes on
// this state. The begin-block runs once for each committed height and gives
// the engine the gate set of that block. Its writes go once over the
// committed view, which gives the next view of that height. Every later
// branch reads that view.
func (a *App) Next() (storetypes.MultiStore, *svm.Next, error) {
	c, err := a.committedView()
	if err != nil {
		return nil, nil, err
	}
	if v := a.next.Load(); v != nil && v.height == c.height {
		return v.branch(), v.next, nil
	}
	a.nextMu.Lock()
	defer a.nextMu.Unlock()
	if v := a.next.Load(); v != nil && v.height == c.height {
		return v.branch(), v.next, nil
	}
	ms := c.branch()
	n, err := a.Stores.NextBlock(ms, uint64(c.height)+1)
	if err != nil {
		return nil, nil, err
	}
	// After a commit, n is for a past height. The engine keeps the roots of
	// the last two heights that it got (engine/src/runtime.rs configure_next).
	// If it gets a past height again, it can drop the root of the current
	// height. So only a request at the latest height gives the engine its
	// height and publishes its view.
	if a.committedHeight.Load() == c.height {
		if err := ffi.ConfigureNext(n.Height, n.Gates); err != nil {
			return nil, nil, err
		}
		a.next.Store(&view{height: c.height, stores: n.Over(c.stores), next: n})
	}
	return ms, n, nil
}

// configureEngine hands the engine the gates svmmeta holds.
func (a *App) configureEngine(ms storetypes.MultiStore) error {
	gates, err := a.Stores.ActiveGates(ms)
	if err != nil {
		return err
	}
	return ffi.Configure(gates)
}

// initChainer runs the genesis of every module and returns the validator set
// from x/staking. svmd does not use the validator list in the CometBFT genesis
// file. A machine that x/staking genesis does not bond is not in the set.
func (a *App) initChainer(ctx sdk.Context, req *abci.RequestInitChain) (*abci.ResponseInitChain, error) {
	var state map[string]jsontext.Value
	if err := json.Unmarshal(req.AppStateBytes, &state); err != nil {
		return nil, fmt.Errorf("app state: %w", err)
	}
	updates, err := a.Modules.InitGenesis(ctx, a.Codec, state)
	if err != nil {
		return nil, err
	}
	if err := a.configureEngine(ctx.MultiStore()); err != nil {
		return nil, err
	}
	if err := a.Upgrades.SetModuleVersionMap(ctx, a.Modules.GetVersionMap()); err != nil {
		return nil, err
	}
	if len(updates.Validators) == 0 {
		return nil, fmt.Errorf("app: genesis bonds no validator, so the chain has no validator set")
	}
	return updates, nil
}

// DefaultGenesis returns the app state for a new chain.
func (a *App) DefaultGenesis() map[string]jsontext.Value {
	return module.NewBasicManagerFromManager(a.Modules, nil).DefaultGenesis(a.Codec)
}
