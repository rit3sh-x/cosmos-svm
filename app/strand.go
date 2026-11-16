package app

import (
	errorsmod "cosmossdk.io/errors"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	bankkeeper "github.com/cosmos/cosmos-sdk/x/bank/keeper"
	transfertypes "github.com/cosmos/ibc-go/v11/modules/apps/transfer/types"
	channeltypes "github.com/cosmos/ibc-go/v11/modules/core/04-channel/types"
	porttypes "github.com/cosmos/ibc-go/v11/modules/core/05-port/types"
	"github.com/cosmos/ibc-go/v11/modules/core/exported"
)

// strandGuard sits above transfer and refuses a receive if the receiver holds
// no spendable usvm. ICS-20 mints into any receiver and creates the account.
// Only an account that holds usvm can pay the fee floor, and usvm is
// send-disabled. A voucher sent to any other account acks success and can
// never move again. The error acknowledgement refunds the sender through the
// ICS-20 path, and core discards all writes of the receive.
//
// strandGuard intercepts only the receive. Sends, acknowledgements and
// timeouts pass straight to transfer. If the app mounts x/feegrant, a grant
// also pays the floor, and this check must count it.
type strandGuard struct {
	porttypes.IBCModule
	bank bankkeeper.Keeper
}

func (g strandGuard) OnRecvPacket(ctx sdk.Context, version string, packet channeltypes.Packet, relayer sdk.AccAddress) exported.Acknowledgement {
	// If the packet does not decode or names no address, transfer refuses it
	// with its own error.
	if data, err := transfertypes.UnmarshalPacketData(packet.GetData(), version, ""); err == nil {
		if receiver, err := sdk.AccAddressFromBech32(data.Receiver); err == nil &&
			!g.bank.SpendableCoin(ctx, receiver, BondDenom).IsPositive() {
			return channeltypes.NewErrorAcknowledgement(errorsmod.Wrapf(sdkerrors.ErrInsufficientFunds,
				"%s holds no %s, so it could never move what it receives", data.Receiver, BondDenom))
		}
	}
	return g.IBCModule.OnRecvPacket(ctx, version, packet, relayer)
}
