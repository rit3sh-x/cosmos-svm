package svm

import (
	"context"
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

// MsgService serves the module's two governance messages.
//
// MsgSVMTransaction is not here. It carries no Cosmos signer and never reaches
// a message router. The TxRunner takes it straight to the engine.
type MsgService struct {
	Stores Stores
	// Authority is the governance module account, the only address that can
	// schedule or cancel.
	Authority string
}

var _ MsgServer = MsgService{}

func (m MsgService) check(authority string) error {
	if authority != m.Authority {
		return fmt.Errorf("svm: %s cannot move a feature gate; only %s can", authority, m.Authority)
	}
	return nil
}

func (m MsgService) ScheduleGates(goCtx context.Context, msg *MsgScheduleGates) (*MsgScheduleGatesResponse, error) {
	if err := m.check(msg.Authority); err != nil {
		return nil, err
	}
	ctx := sdk.UnwrapSDKContext(goCtx)
	if err := m.Stores.ScheduleGates(ctx.MultiStore(), uint64(ctx.HeaderInfo().Height), msg.Height, msg.Gates); err != nil {
		return nil, err
	}
	return &MsgScheduleGatesResponse{}, nil
}

func (m MsgService) CancelGates(goCtx context.Context, msg *MsgCancelGates) (*MsgCancelGatesResponse, error) {
	if err := m.check(msg.Authority); err != nil {
		return nil, err
	}
	ctx := sdk.UnwrapSDKContext(goCtx)
	if err := m.Stores.CancelGates(ctx.MultiStore(), msg.Height, msg.Gates); err != nil {
		return nil, err
	}
	return &MsgCancelGatesResponse{}, nil
}
