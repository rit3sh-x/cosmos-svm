package app_test

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	transfertypes "github.com/cosmos/ibc-go/v11/modules/apps/transfer/types"
	clienttypes "github.com/cosmos/ibc-go/v11/modules/core/02-client/types"
	channeltypes "github.com/cosmos/ibc-go/v11/modules/core/04-channel/types"
	"github.com/cosmos/ibc-go/v11/modules/core/exported"
	localhost "github.com/cosmos/ibc-go/v11/modules/light-clients/09-localhost"

	"github.com/rit3sh-x/cosmos-svm/app"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// TestUsvmCannotLeaveOverAChannel proves that usvm cannot leave over a transfer
// channel. The channel opens through the routed core and the transfer module,
// over 09-localhost, so no counterparty is needed. Bank refuses the
// send-disabled denom inside the transfer handler, so nothing is escrowed.
func TestUsvmCannotLeaveOverAChannel(t *testing.T) {
	c, keys := operators(t)
	op := tc.Address(keys[0]).String()
	openTransfer(t, c, keys[0])

	balance := c.App.Bank.GetBalance(ctxOf(c), tc.Address(keys[0]), app.BondDenom)
	timeout := uint64(time.Now().Add(time.Hour).UnixNano())
	log := deliverLog(c, keys[0], transfertypes.NewMsgTransfer(port, "channel-0", sdk.NewInt64Coin(app.BondDenom, 1), op, op,
		clienttypes.ZeroHeight(), timeout, ""))
	if !strings.Contains(log, app.BondDenom+" transfers are currently disabled") {
		t.Fatalf("a usvm transfer over an open channel: %q, want bank's send-disabled refusal", log)
	}
	if got := c.App.Bank.GetBalance(ctxOf(c), tc.Address(keys[0]), app.BondDenom); !got.Equal(balance) {
		t.Fatalf("the sender's usvm moved from %s to %s", balance, got)
	}
	escrow := transfertypes.GetEscrowAddress(port, "channel-0")
	if got := c.App.Bank.GetAllBalances(ctxOf(c), escrow); !got.IsZero() {
		t.Fatalf("the channel escrow holds %s", got)
	}
	if !c.App.Bank.BlockedAddr(authtypes.NewModuleAddress(transfertypes.ModuleName)) {
		t.Fatal("the transfer module account can receive")
	}
}

const port = transfertypes.PortID

// deliverLog delivers one Cosmos transaction in its own block. It returns the
// failure log, or an empty string if the transaction succeeded.
func deliverLog(c *tc.Chain, key ed25519.PrivateKey, msg sdk.Msg) string {
	res, _ := c.Deliver(c.Cosmos(key, msg))
	if res[0].Code != 0 {
		return res[0].Log
	}
	return ""
}

// openTransfer opens transfer/channel-0 to transfer/channel-1 over
// 09-localhost. An operator signs the four handshake messages that a relayer
// would send.
func openTransfer(t *testing.T, c *tc.Chain, key ed25519.PrivateKey) {
	t.Helper()
	op := tc.Address(key).String()
	hops := []string{exported.LocalhostConnectionID}
	version := transfertypes.V1
	steps := []sdk.Msg{
		channeltypes.NewMsgChannelOpenInit(port, version, channeltypes.UNORDERED, hops, port, op),
		channeltypes.NewMsgChannelOpenTry(port, version, channeltypes.UNORDERED, hops, port, "channel-0", version,
			localhost.SentinelProof, clienttypes.ZeroHeight(), op),
		channeltypes.NewMsgChannelOpenAck(port, "channel-0", "channel-1", version, localhost.SentinelProof,
			clienttypes.ZeroHeight(), op),
		channeltypes.NewMsgChannelOpenConfirm(port, "channel-1", localhost.SentinelProof, clienttypes.ZeroHeight(), op),
	}
	for i, msg := range steps {
		if log := deliverLog(c, key, msg); log != "" {
			t.Fatalf("handshake step %d: %s", i+1, log)
		}
	}
	if ch, ok := c.App.IBC.ChannelKeeper.GetChannel(ctxOf(c), port, "channel-0"); !ok || ch.State != channeltypes.OPEN {
		t.Fatalf("channel-0 is %v, want open", ch.State)
	}
}

// TestAReceiveThatWouldStrandIsRefused proves that a receive to an address
// that holds no usvm gets an error ack, mints nothing and creates no account,
// because core discards the writes of a failed receive. A receive to an
// operator mints the voucher and acks success.
func TestAReceiveThatWouldStrandIsRefused(t *testing.T) {
	c, keys := operators(t)
	openTransfer(t, c, keys[0])
	stranger := tc.Address(tc.Key(99))

	recv := func(seq uint64, receiver sdk.AccAddress) []byte {
		t.Helper()
		if log := deliverLog(c, keys[0], recvPacket(c, keys[0], seq, receiver)); log != "" {
			t.Fatalf("receive %d: %s", seq, log)
		}
		ack, ok := c.App.IBC.ChannelKeeper.GetPacketAcknowledgement(ctxOf(c), port, "channel-1", seq)
		if !ok {
			t.Fatalf("receive %d wrote no acknowledgement", seq)
		}
		return ack
	}
	success := channeltypes.CommitAcknowledgement(channeltypes.NewResultAcknowledgement([]byte{1}).Acknowledgement())

	if ack := recv(1, stranger); bytes.Equal(ack, success) {
		t.Fatal("a receive to an address holding no usvm was acknowledged as a success")
	}
	if got := c.App.Bank.GetBalance(ctxOf(c), stranger, voucher); !got.IsZero() {
		t.Fatalf("the stranded receiver holds %s", got)
	}
	if c.App.Accounts.GetAccount(ctxOf(c), stranger) != nil {
		t.Fatal("the refused receive created the receiver's account")
	}

	if ack := recv(2, tc.Address(keys[1])); !bytes.Equal(ack, success) {
		t.Fatal("a receive to an operator was not acknowledged as a success")
	}
	if got := c.App.Bank.GetBalance(ctxOf(c), tc.Address(keys[1]), voucher); got.Amount.Int64() != 1000 {
		t.Fatalf("the operator holds %s, want 1000%s", got, voucher)
	}
}

var voucher = transfertypes.NewDenom("stake", transfertypes.NewHop(port, "channel-1")).IBCDenom()

// recvPacket returns a relay of 1,000 stake to receiver over the localhost
// pair. It first writes to state the commitment that a send on channel-0 would
// make. A real send needs a coin that this chain can move, and its only coin
// is send-disabled. The packet depends only on its arguments, so two relayers
// relay the same packet.
func recvPacket(c *tc.Chain, relayer ed25519.PrivateKey, seq uint64, receiver sdk.AccAddress) *channeltypes.MsgRecvPacket {
	data := transfertypes.NewFungibleTokenPacketData("stake", "1000", "cosmos1sender", receiver.String(), "")
	packet := channeltypes.NewPacket(data.GetBytes(), seq, port, "channel-0", port, "channel-1",
		clienttypes.ZeroHeight(), uint64(time.Unix(4_000_000_000, 0).UnixNano()))
	c.App.IBC.ChannelKeeper.SetPacketCommitment(ctxOf(c), port, "channel-0", seq, channeltypes.CommitPacket(packet))
	return channeltypes.NewMsgRecvPacket(packet, localhost.SentinelProof, clienttypes.ZeroHeight(),
		tc.Address(relayer).String())
}

// TestARedundantRelayIsRefusedAtCheckTx proves that CheckTx and ReCheckTx
// refuse a second relay of a packet that already landed, so it never takes the
// first-ranked units of a block. A delivered second relay is a no-op in core,
// and the voucher is minted once.
func TestARedundantRelayIsRefusedAtCheckTx(t *testing.T) {
	c, keys := operators(t)
	openTransfer(t, c, keys[0])
	receiver := tc.Address(keys[1])

	first := c.Cosmos(keys[0], recvPacket(c, keys[0], 1, receiver))
	if res := c.Check(first, false); res.Code != 0 {
		t.Fatalf("the first relay at CheckTx: %s", res.Log)
	}
	if res, _ := c.Deliver(first); res[0].Code != 0 {
		t.Fatalf("the first relay: %s", res[0].Log)
	}

	second := c.Cosmos(keys[2], recvPacket(c, keys[2], 1, receiver))
	for _, recheck := range []bool{false, true} {
		if res := c.Check(second, recheck); !strings.Contains(res.Log, "packet messages are redundant") {
			t.Fatalf("the second relay at CheckTx (recheck %v): code %d, %q", recheck, res.Code, res.Log)
		}
	}
	if res, _ := c.Deliver(second); res[0].Code != 0 {
		t.Fatalf("the second relay delivered anyway: %s", res[0].Log)
	}
	if got := c.App.Bank.GetBalance(ctxOf(c), receiver, voucher); got.Amount.Int64() != 1000 {
		t.Fatalf("the receiver holds %s after two relays of one packet, want 1000%s", got, voucher)
	}
}
