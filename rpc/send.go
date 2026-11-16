package rpc

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"

	"github.com/mr-tron/base58"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// The send path is Agave's send_transaction, in its order:
//  1. Decode, with Agave's encodings, size caps and texts.
//  2. Check the minContextSlot against the preflight bank.
//  3. Sanitize, also with skipPreflight. A failure is -32602
//     "invalid transaction: {err}".
//  4. Without skipPreflight, verify the signatures, then check health only if
//     they pass, then simulate. A failure is -32002 with the simulation as data.
//  5. Answer the signature, whatever CheckTx then says. Agave hands the
//     transaction to its sender and never reports a refusal there.

// sendConfig is Agave's RpcSendTransactionConfig. svmd accepts
// preflightCommitment and maxRetries and ignores them. Every answer is
// finalized, and CometBFT's pool, not the RPC, decides whether to gossip a
// transaction again.
type sendConfig struct {
	Encoding       string  `json:"encoding"`
	SkipPreflight  bool    `json:"skipPreflight"`
	MinContextSlot *uint64 `json:"minContextSlot"`
}

func sendTransaction(ctx context.Context, s *Server, params []jsontext.Value) (any, error) {
	var encoded string
	var cfg sendConfig
	if err := param(params, 0, &encoded); err != nil {
		return nil, err
	}
	if err := param(params, 1, &cfg); err != nil {
		return nil, err
	}
	if err := validEncoding(cfg.Encoding, transactionEncodings); err != nil {
		return nil, err
	}
	raw, err := decodeTransaction(encoded, cfg.Encoding)
	if err != nil {
		return nil, err
	}
	if err := deserializes(raw); err != nil {
		return nil, err
	}
	_, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if err := reached(cfg.MinContextSlot, h); err != nil {
		return nil, err
	}
	if err := s.sanitize(raw); err != nil {
		return nil, err
	}
	if !cfg.SkipPreflight {
		if err := s.preflight(ctx, raw); err != nil {
			return nil, err
		}
	}
	return s.broadcast(raw, "rpc")
}

// deserializes is decode_and_deserialize's last step. The engine is the only
// parser. The refusal quotes wincode's reason, as Agave does.
func deserializes(raw []byte) error {
	if _, _, err := ffi.TransactionKeys(raw); err != nil {
		return invalidParams("failed to deserialize %s: %s", transactionType, wincodeReason(raw, false, err))
	}
	return nil
}

// wincodeReason is the reason wincode gives, which Agave quotes. For bytes
// that wincode reads but the engine still refuses, it is the engine's refusal.
func wincodeReason(raw []byte, message bool, err error) string {
	if reason := ffi.DeserializeError(raw, message); reason != "" {
		return reason
	}
	return err.Error()
}

// sanitize is the Agave sanitize_transaction. It runs admission
// without the signature check, and returns its refusal if it refuses the
// message. A check beyond Agave's sanitize, such as a compute budget, belongs
// to the pool. If only that check fails, the send answers the signature.
func (s *Server) sanitize(raw []byte) error {
	height, err := s.nextHeight()
	if err != nil {
		return err
	}
	if _, err := ffi.CheckTx(raw, true, height); err != nil {
		if display, ok := sanitizeErrors[refusalReason(err)]; ok {
			return invalidParams("invalid transaction: %s", display)
		}
	}
	return nil
}

// sanitizeErrors are the TransactionErrors RuntimeTransaction::try_create
// gives, with their Display.
var sanitizeErrors = map[string]string{
	"SanitizeFailure":                "Transaction failed to sanitize accounts offsets correctly",
	"UnsupportedVersion":             "Transaction version is unsupported",
	"AddressLookupTableNotFound":     "Transaction loads an address table account that doesn't exist",
	"InvalidAddressLookupTableOwner": "Transaction loads an address table account with an invalid owner",
	"InvalidAddressLookupTableData":  "Transaction loads an address table account with invalid data",
	"InvalidAddressLookupTableIndex": "Transaction address table lookup uses an invalid index",
}

// refusalReason is the TransactionError an engine refusal names.
func refusalReason(err error) string {
	if !errors.Is(err, ffi.ErrRejected) {
		return ""
	}
	reason := strings.TrimPrefix(err.Error(), ffi.ErrRejected.Error()+": ")
	return strings.TrimSuffix(reason, ": "+ffi.ErrRejected.Error())
}

// preflight is Agave's. It verifies the signatures, checks the node's health
// only if they pass, then simulates the next block. A transaction that fails
// is refused before it is broadcast. Preflight is on by default in web3.js
// and the Solana CLI, so such a wallet gets an error and pays no fee for a
// transaction that would fail.
func (s *Server) preflight(ctx context.Context, raw []byte) error {
	height, err := s.nextHeight()
	if err != nil {
		return err
	}
	if _, err := ffi.CheckTx(raw, false, height); refusalReason(err) == "SignatureFailure" {
		// This is the Agave TransactionSimulationResult::new_error.
		return &rpcError{Code: codeSendTransactionFailure,
			Message: "Transaction simulation failed: Transaction did not pass signature verification",
			Data: preflightData(map[string]any{"err": "SignatureFailure", "logs": []string{},
				"unitsConsumed": 0, "loadedAccountsDataSize": 0, "returnData": nil, "fee": nil})}
	}
	if _, err := getHealth(ctx, s, nil); err != nil {
		return err
	}
	value, sim, _, _, err := s.simulate(raw, true, false, nil)
	if err != nil {
		return err
	}
	if sim.Status == 0 {
		return nil
	}
	return &rpcError{Code: codeSendTransactionFailure, Message: "Transaction simulation failed: " + sim.Message,
		Data: preflightData(value)}
}

// preflightData is the RpcSimulateTransactionResult a refused send carries.
// It holds what the simulation found, with null for each field Agave leaves
// None.
func preflightData(value map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range value {
		out[k] = v
	}
	for _, k := range []string{"accounts", "innerInstructions", "replacementBlockhash", "preBalances",
		"postBalances", "preTokenBalances", "postTokenBalances", "loadedAddresses"} {
		out[k] = nil
	}
	return out
}

// ingress counts every transaction handed to the pool, by path and by the
// pool's outcome. Both label sets are fixed, so a sender cannot grow them.
var ingress = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "svm_ingress_total",
	Help: "Transactions handed to the pool, by path (rpc, quic) and outcome (admitted, rejected, duplicate).",
}, []string{"path", "outcome"})

// broadcast wraps a signed Solana transaction in MsgSVMTransaction and hands
// it to CometBFT's mempool by path (rpc or quic). It answers the first
// signature whatever the pool says, as Agave answers once its own checks
// pass. It counts a refusal and logs it at debug level, so a flood cannot
// fill the log. sendTransaction does nothing else with bytes a client signed.
// On a devnet build the faucet sends the bytes it signs through it too, as
// on Agave.
func (s *Server) broadcast(raw []byte, path string) (any, error) {
	sig, err := firstSignature(raw)
	if err != nil {
		// Only the QUIC path gets here, because sendTransaction deserializes
		// and sanitizes first. The engine would refuse it too.
		ingress.WithLabelValues(path, "rejected").Inc()
		return nil, err
	}
	tx, err := svm.Envelope(s.app.TxConfig, raw)
	if err != nil {
		return nil, err
	}
	outcome, reason := "admitted", ""
	res, err := s.node.BroadcastTxSync(context.Background(), tx)
	switch {
	case err != nil && strings.Contains(err.Error(), "tx already exists in cache"):
		outcome, reason = "duplicate", err.Error()
	case err != nil:
		outcome, reason = "rejected", err.Error()
	case res.Code != 0:
		outcome, reason = "rejected", res.Log
	}
	ingress.WithLabelValues(path, outcome).Inc()
	if outcome != "admitted" {
		s.logger.Debug("the pool refused a sent transaction", "signature", base58.Encode(sig), "outcome", outcome, "log", reason)
	}
	return base58.Encode(sig), nil
}

// firstSignature is the signature a sendTransaction response echoes back.
//
// Reading it is framing, not a message parse. The engine is still the only
// parser, and it rejects the transaction if this framing is wrong. The wire
// format has two framings. Legacy and v0 open with a ShortU16 signature count,
// one byte below 128, then the signatures. A V1 transaction opens with the
// message and its 0x81 prefix. Its signatures come last with no length
// prefix, counted by the message header's num_required_signatures.
func firstSignature(raw []byte) ([]byte, error) {
	switch {
	case len(raw) >= 65 && raw[0] > 0 && raw[0] < 0x80:
		return raw[1:65], nil
	case len(raw) >= 65 && raw[0] == 0x81:
		n := int(raw[1])
		if at := len(raw) - 64*n; n > 0 && at > 0 {
			return raw[at : at+64], nil
		}
	}
	return nil, invalidParams("invalid transaction: no signature")
}

// Agave's caps on an encoded transaction. Base58 stays at the legacy packet.
// Base64 does too, unless its first two characters are "gQ" or later. That
// is a message that starts with 0x81, V1 or above.
const (
	packetDataSize      = 1232 // PACKET_DATA_SIZE
	maxV1Size           = 4096 // solana_message::v1::MAX_TRANSACTION_SIZE
	maxBase58Size       = 1683 // the base58 of 1,232 bytes of 0xff
	maxBase64Size       = 5464 // ceil(4096 / 3) * 4
	maxBase64LegacySize = 1644 // ceil(1232 / 3) * 4
	v1Base64Prefix      = "gQ"
	// transactionType is what Agave's messages call the type they decode,
	// type_name::<VersionedTransaction>().
	transactionType = "solana_transaction::versioned::VersionedTransaction"
	messageType     = "solana_message::versions::VersionedMessage"
)

// transactionEncodings are UiTransactionEncoding's names in declaration
// order, as serde lists them when it refuses one it does not know.
var transactionEncodings = []string{"binary", "base64", "base58", "json", "jsonParsed"}

// validEncoding refuses an encoding the enum has no variant for. It uses
// serde's text, which jsonrpc-core wraps.
func validEncoding(encoding string, names []string) error {
	if encoding == "" {
		return nil
	}
	for _, n := range names {
		if encoding == n {
			return nil
		}
	}
	return invalidParams("Invalid params: unknown variant `%s`, expected one of `%s`.", encoding, strings.Join(names, "`, `"))
}

// decodeTransaction is Agave's decode_and_deserialize without the
// deserialize. The default and binary are base58, and json and jsonParsed are
// refused. It checks the encoded length before decoding and the decoded
// length after, with Agave's messages. simulateTransaction and
// sendTransaction both decode through it.
func decodeTransaction(encoded, encoding string) ([]byte, error) {
	return decodeWire(encoded, encoding, transactionType)
}

// decodeWire is decode_and_deserialize for the type Agave names: a
// transaction, or getFeeForMessage's message.
func decodeWire(encoded, encoding, typeName string) ([]byte, error) {
	var raw []byte
	var err error
	maxRaw := packetDataSize
	switch encoding {
	case "base64":
		maxEncoded := maxBase64LegacySize
		if len(encoded) >= 2 && encoded[:2] >= v1Base64Prefix {
			maxEncoded, maxRaw = maxBase64Size, maxV1Size
		}
		if len(encoded) > maxEncoded {
			return nil, invalidParams("base64 encoded %s too large: %d bytes (max: encoded/raw %d/%d)",
				typeName, len(encoded), maxEncoded, maxRaw)
		}
		if raw, err = decodeBase64(encoded); err != nil {
			return nil, invalidParams("invalid base64 encoding: %v", err)
		}
	case "", "base58", "binary":
		if len(encoded) > maxBase58Size {
			return nil, invalidParams("base58 encoded %s too large: %d bytes (max: encoded/raw %d/%d)",
				typeName, len(encoded), maxBase58Size, packetDataSize)
		}
		if raw, err = decodeBase58(encoded); err != nil {
			return nil, invalidParams("invalid base58 encoding: %v", err)
		}
	default:
		return nil, invalidParams("unsupported encoding: %s. Supported encodings: base58, base64", encoding)
	}
	if len(raw) > maxRaw {
		return nil, invalidParams("decoded %s too large: %d bytes (max: %d bytes)", typeName, len(raw), maxRaw)
	}
	return raw, nil
}

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// decodeBase58 is bs58's decode, whose errors Agave prints with {:?}. A byte
// past ASCII is NonAsciiCharacter, and a byte outside the alphabet is
// InvalidCharacter, both at the first such index.
func decodeBase58(s string) ([]byte, error) {
	for i := 0; i < len(s); i++ {
		switch b := s[i]; {
		case b >= 0x80:
			return nil, fmt.Errorf("NonAsciiCharacter { index: %d }", i)
		case strings.IndexByte(base58Alphabet, b) < 0:
			return nil, fmt.Errorf("InvalidCharacter { character: %s, index: %d }", debugChar(b), i)
		}
	}
	return base58.Decode(s)
}

// debugChar is Rust's {:?} for an ASCII char.
func debugChar(b byte) string {
	switch b {
	case '\'':
		return `'\''`
	case '\\':
		return `'\\'`
	case '\n':
		return `'\n'`
	case '\r':
		return `'\r'`
	case '\t':
		return `'\t'`
	}
	if b < 0x20 || b == 0x7f {
		return fmt.Sprintf(`'\u{%x}'`, b)
	}
	return "'" + string(b) + "'"
}

// decodeBase64 is base64 0.22's STANDARD decode, whose errors Agave prints
// with {:?}. A byte outside the alphabet is InvalidByte at its offset. A
// length of 1 mod 4 without pads is InvalidLength. A misplaced or missing pad
// is InvalidPadding.
func decodeBase64(s string) ([]byte, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	pad := strings.IndexByte(s, '=')
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b != '=' && strings.IndexByte(alphabet, b) < 0 {
			return nil, fmt.Errorf("InvalidByte(%d, %d)", i, b)
		}
		if pad >= 0 && i > pad && b != '=' {
			return nil, fmt.Errorf("InvalidByte(%d, %d)", pad, '=')
		}
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		if len(strings.TrimRight(s, "="))%4 == 1 {
			return nil, fmt.Errorf("InvalidLength(%d)", len(strings.TrimRight(s, "=")))
		}
		return nil, errors.New("InvalidPadding")
	}
	return raw, nil
}
