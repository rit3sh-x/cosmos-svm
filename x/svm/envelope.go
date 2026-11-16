package svm

import (
	"bytes"
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"

	errorsmod "cosmossdk.io/errors"

	"github.com/cosmos/cosmos-sdk/client"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// ErrEnvelope refuses an SVM transaction in any Cosmos envelope but its one
// canonical encoding.
var ErrEnvelope = errorsmod.Register(ModuleName, 104, "svm transaction envelope is not canonical")

// Envelope is the one encoding an SVM transaction travels in: a Cosmos
// transaction that carries one MsgSVMTransaction and nothing else, as txConfig
// encodes it. It has no memo, timeout, extension option, fee or Cosmos
// signature. The Solana transaction carries its own signatures and pays its own
// fee, so anything else in the envelope is bytes nobody pays for.
func Envelope(txConfig client.TxConfig, solanaTx []byte) ([]byte, error) {
	b := txConfig.NewTxBuilder()
	if err := b.SetMsgs(&MsgSVMTransaction{Transaction: solanaTx}); err != nil {
		return nil, err
	}
	return txConfig.TxEncoder()(b.GetTx())
}

// Decoder is txConfig's decoder, and it refuses an SVM transaction whose bytes
// are not its Envelope. The app decodes everything with it, so CheckTx, the
// pool, both proposal handlers and the block runner refuse the same bytes.
// Each of them tries Enveloped first. It finds the same SVM transactions
// without a decode.
//
// CometBFT keys its pool and its cache on the hash of the envelope, and every
// handler here reads only the Solana bytes inside it. Without this check, a
// second envelope around the same transaction is a second pool entry and a
// second block entry. Only the first is processed and paid, and anyone who
// sees the transaction in gossip can make one. A memo is padding that no fee
// pays for.
//
// Decoder panics if Enveloped does not find the transaction in what txConfig
// encodes. Enveloped's constants are this SDK's encoding. If an encoder writes
// other bytes, Enveloped finds transactions in envelopes that Decoder refuses,
// so a build with that encoder must not start.
func Decoder(txConfig client.TxConfig) sdk.TxDecoder {
	for _, n := range []int{1, 1232} {
		raw, err := Envelope(txConfig, make([]byte, n))
		if tx, ok := Enveloped(raw); err != nil || !ok || len(tx) != n {
			panic(fmt.Sprintf("svm: Enveloped does not find a %d-byte transaction in its envelope: %v", n, err))
		}
	}
	decode := txConfig.TxDecoder()
	return func(raw []byte) (sdk.Tx, error) {
		tx, err := decode(raw)
		if err != nil {
			return nil, err
		}
		if m, ok := svmMsg(tx); ok {
			canonical, err := Envelope(txConfig, m.Transaction)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(raw, canonical) {
				return nil, ErrEnvelope
			}
		}
		return tx, nil
	}
}

// Enveloped returns the Solana transaction in raw if raw is its Envelope,
// byte for byte. It does not decode or encode. It returns true exactly where
// Decoder accepts an SVM transaction that is not empty, and it returns the
// bytes that Decoder's message carries. The bytes are part of raw. Their
// capacity ends with them, so an append cannot write over raw. If it returns
// false, Decoder decides.
//
// An Envelope puts each length-delimited field inside the one before it, and
// each length is the minimal varint. The fields are TxRaw's body_bytes, the
// body's one message, the Any's type_url and value, and the message's
// transaction. After the body come the auth info bytes. They hold only the
// empty Fee that the SDK's builder writes.
func Enveloped(raw []byte) ([]byte, bool) {
	body, rest, ok := lengthDelimited(raw, 0x0a)
	if !ok || string(rest) != envelopeAuthInfo {
		return nil, false
	}
	message, rest, ok := lengthDelimited(body, 0x0a)
	if !ok || len(rest) != 0 {
		return nil, false
	}
	typeURL, rest, ok := lengthDelimited(message, 0x0a)
	if !ok || string(typeURL) != envelopeTypeURL {
		return nil, false
	}
	value, rest, ok := lengthDelimited(rest, 0x12)
	if !ok || len(rest) != 0 {
		return nil, false
	}
	tx, rest, ok := lengthDelimited(value, 0x0a)
	if !ok || len(rest) != 0 || len(tx) == 0 {
		return nil, false
	}
	return tx[:len(tx):len(tx)], true
}

const (
	envelopeTypeURL = "/svm.v1.MsgSVMTransaction"
	// TxRaw's auth_info_bytes, field 2, with AuthInfo{Fee: &Fee{}}.
	envelopeAuthInfo = "\x12\x02\x12\x00"
)

// lengthDelimited splits b into the field that tag opens and the bytes after
// it. b must start with tag. It refuses a length that is not the minimal
// varint.
func lengthDelimited(b []byte, tag byte) (field, rest []byte, ok bool) {
	if len(b) == 0 || b[0] != tag {
		return nil, nil, false
	}
	field, n := protowire.ConsumeBytes(b[1:])
	if n < 0 || n != protowire.SizeBytes(len(field)) {
		return nil, nil, false
	}
	return field, b[1+n:], true
}
