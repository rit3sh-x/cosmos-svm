package svm

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"

	gwruntime "github.com/grpc-ecosystem/grpc-gateway/runtime"
	protov2 "google.golang.org/protobuf/proto"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	txsigning "github.com/cosmos/cosmos-sdk/x/tx/signing"

	"github.com/rit3sh-x/cosmos-svm/ffi"
)

// GetSigners gives MsgSVMTransaction no Cosmos signer. Its signatures are
// Solana's, which the engine verifies, and no x/auth account stands behind them.
var GetSigners = txsigning.CustomGetSigner{
	MsgType: "svm.v1.MsgSVMTransaction",
	Fn:      func(protov2.Message) ([][]byte, error) { return nil, nil },
}

type AppModule struct {
	Stores Stores
	// Authority is the governance module account, the only sender the two gate
	// messages accept.
	Authority string
}

var (
	_ module.AppModuleBasic = AppModule{}
	_ module.HasGenesis     = AppModule{}
	_ module.HasServices    = AppModule{}
)

func (am AppModule) RegisterServices(cfg module.Configurator) {
	RegisterMsgServer(cfg.MsgServer(), MsgService(am))
}

func (AppModule) Name() string                                                  { return ModuleName }
func (AppModule) IsOnePerModuleType()                                           {}
func (AppModule) IsAppModule()                                                  {}
func (AppModule) RegisterLegacyAminoCodec(*codec.LegacyAmino)                   {}
func (AppModule) RegisterGRPCGatewayRoutes(client.Context, *gwruntime.ServeMux) {}

func (AppModule) RegisterInterfaces(registry codectypes.InterfaceRegistry) {
	registry.RegisterImplementations((*sdk.Msg)(nil), &MsgSVMTransaction{}, &MsgScheduleGates{}, &MsgCancelGates{})
}

func (AppModule) DefaultGenesis(codec.JSONCodec) jsontext.Value {
	raw, err := json.Marshal(DefaultGenesis(), json.Deterministic(true))
	if err != nil {
		panic(err)
	}
	return raw
}

func (AppModule) ValidateGenesis(_ codec.JSONCodec, _ client.TxEncodingConfig, raw jsontext.Value) error {
	var g GenesisState
	if err := json.Unmarshal(raw, &g); err != nil {
		return err
	}
	_, err := g.decode()
	return err
}

func (am AppModule) InitGenesis(ctx sdk.Context, _ codec.JSONCodec, raw jsontext.Value) {
	if err := am.Stores.InitGenesis(ctx.MultiStore(), raw); err != nil {
		panic(err)
	}
}

func (am AppModule) ExportGenesis(ctx sdk.Context, _ codec.JSONCodec) jsontext.Value {
	g, err := am.Stores.ExportGenesis(ctx.MultiStore())
	if err != nil {
		panic(err)
	}
	raw, err := json.Marshal(g, json.Deterministic(true))
	if err != nil {
		panic(err)
	}
	return raw
}

// BeginBlock flips any gate scheduled for this height and writes the per-block
// sysvars, both before any transaction runs.
//
// The order matters in one direction only. A rent gate rewrites the Rent
// sysvar, and the per-block writer touches Clock, SlotHashes and
// RecentBlockhashes, so they do not overlap. BeginBlock reconfigures the engine
// after a flip, so the feature set is constant for the whole block.
func (am AppModule) BeginBlock(goCtx context.Context) error {
	ctx := sdk.UnwrapSDKContext(goCtx)
	h := ctx.HeaderInfo()
	ms := ctx.MultiStore()
	flipped, err := am.Stores.beginBlock(ms, uint64(h.Height), h.Time.Unix(), h.Hash)
	if err != nil || !flipped {
		return err
	}
	gates, err := am.Stores.ActiveGates(ms)
	if err != nil {
		return err
	}
	return ffi.Configure(gates)
}
