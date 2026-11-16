package rpc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"

	"github.com/klauspost/compress/zstd"
	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// lastValidBlockHeight is the last height at which a blockhash from h passes
// the age check. That is the window CheckTx and the engine enforce.
func lastValidBlockHeight(h uint64) uint64 { return h + svm.BlockhashWindow }

// accountConfig is Agave's RpcAccountInfoConfig. The dataSlice offset and
// length are unsigned, as Agave's `usize` fields are. The parse refuses a
// negative value with invalid params, so it never reaches the cut of the data,
// where a websocket subscriber could panic the node.
type accountConfig struct {
	Encoding  string `json:"encoding"`
	DataSlice *struct {
		Offset uint `json:"offset"`
		Length uint `json:"length"`
	} `json:"dataSlice"`
	// notification is a subscription's rendering, which encode_ui_account
	// does without the RPC's base58 refusal.
	notification bool
}

// accountEncodings are UiAccountEncoding's names in declaration order.
var accountEncodings = []string{"binary", "base58", "base64", "jsonParsed", "base64+zstd"}

// or returns the config with the method's default encoding in place of none.
// The default is binary for every method except getMultipleAccounts, which
// uses base64. An encoding with no variant in the enum is refused with serde's
// text.
func (c accountConfig) or(encoding string) (accountConfig, error) {
	if err := validEncoding(c.Encoding, accountEncodings); err != nil {
		return c, err
	}
	if c.Encoding == "" {
		c.Encoding = encoding
	}
	return c, nil
}

// param decodes params[i] into v when present. It refuses a value of the wrong
// type with jsonrpc-core's "Invalid params: " and "." around this chain's own
// description of the fault. That description differs from serde's, and the
// difference is kept.
func param(params []jsontext.Value, i int, v any) error {
	if i >= len(params) || string(params[i]) == "null" {
		return nil
	}
	if err := json.Unmarshal(params[i], v, agaveInput); err != nil {
		return invalidParams("Invalid params: %v.", reason(err))
	}
	return nil
}

// orDefault is v, or def when the client sent null or nothing, as an absent
// Option does on Agave.
func orDefault(v *string, def string) string {
	if v == nil {
		return def
	}
	return *v
}

// reason is the text of a failed decode for the caller. The error of an
// UnmarshalJSON method comes without the wrapping of v2.
func reason(err error) error {
	var e *json.SemanticError
	if errors.As(err, &e) && e.Err != nil {
		return e.Err
	}
	return err
}

func pubkeyParam(params []jsontext.Value, i int) ([32]byte, error) {
	var s string
	if err := param(params, i, &s); err != nil {
		return [32]byte{}, err
	}
	return decodeKey(s)
}

// parseBase58 is solana-address 2.8.0's FromStr, which solana-signature and
// solana-hash share. A string longer than the longest encoding, or one that
// decodes to the wrong length, is WrongSize. A character outside the alphabet
// is Invalid. Agave prints the error with {:?}.
func parseBase58(s string, size int) ([]byte, string) {
	if len(s) > base58Len[size] {
		return nil, "WrongSize"
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(base58Alphabet, s[i]) < 0 {
			return nil, "Invalid"
		}
	}
	b, err := base58.Decode(s)
	if err != nil || len(b) != size {
		return nil, "WrongSize"
	}
	return b, ""
}

// base58Len is the longest base58 of each size: MAX_BASE58_LEN of an address
// or a hash, and of a signature.
var base58Len = map[int]int{32: 44, 64: 88}

// decodeKey is Agave's verify_pubkey.
func decodeKey(s string) ([32]byte, error) {
	b, fault := parseBase58(s, 32)
	if fault != "" {
		return [32]byte{}, invalidParams("Invalid param: %s", fault)
	}
	return [32]byte(b), nil
}

// contextConfig is Agave's RpcContextConfig, which every config with a
// commitment carries. The commitment is read as serde reads a flattened
// Option. An unknown level is no level, which means finalized. So only
// "processed" is ever refused, and only by the history methods.
type contextConfig struct {
	Commitment     jsontext.Value `json:"commitment"`
	MinContextSlot *uint64        `json:"minContextSlot"`
}

// reached is Agave's get_bank_with_config. A minContextSlot above the answer's
// height gets -32016 with that height. Every answer here reads the committed
// height.
func reached(floor *uint64, h uint64) error {
	if floor != nil && h < *floor {
		return &rpcError{Code: codeMinContextSlot, Message: "Minimum context slot has not been reached",
			Data: map[string]uint64{"contextSlot": h}}
	}
	return nil
}

// contextAt applies reached to the config at params[i]. The handler refuses a
// config that is not an object.
func contextAt(params []jsontext.Value, i int, h uint64) error {
	var cfg contextConfig
	if i < len(params) && json.Unmarshal(params[i], &cfg, agaveInput) == nil {
		return reached(cfg.MinContextSlot, h)
	}
	return nil
}

// atLeastConfirmed is check_is_at_least_confirmed, which getBlock,
// getTransaction and getSignaturesForAddress run.
func atLeastConfirmed(commitment jsontext.Value) error {
	if string(bytes.TrimSpace(commitment)) == `"processed"` {
		return invalidParams("Method does not support commitment below `confirmed`")
	}
	return nil
}

// uiAccount is Agave's UiAccount. Data is a union. It is a bare string for the
// legacy binary encoding, a two-element `[body, encoding]` array for every
// other binary encoding, and the `{program, parsed, space}` object for a
// jsonParsed body that parsed.
type uiAccount struct {
	Data       any    `json:"data"`
	Executable bool   `json:"executable"`
	Lamports   uint64 `json:"lamports"`
	Owner      string `json:"owner"`
	RentEpoch  uint64 `json:"rentEpoch"`
	Space      uint64 `json:"space"`
}

// zstdEncoder keeps one encoder per CPU. That is how many compressions
// rpc-threads lets run at once by default. Each encoder has a 2 MiB window,
// the window of Agave frames. Agave compresses with libzstd level 3 on a
// stream of unknown size, and the recorded getAccountInfo/mint-zstd frame
// names 2 MiB. The default window was 8 MiB, and each encoder keeps its
// tables and history for life after its first use.
var zstdEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)), zstd.WithWindowSize(2<<20))

// encodeAccount renders an account the way Agave's UiAccount does.
func encodeAccount(pubkey [32]byte, value []byte, cfg accountConfig, m *mintReader) (*uiAccount, error) {
	if value == nil {
		return nil, nil
	}
	a, err := ffi.DecodeAccount(value)
	if err != nil {
		return nil, err
	}
	return renderAccount(pubkey, a, cfg, m)
}

// renderAccount is encodeAccount for a caller that has already decoded the
// account, such as the program scan.
//
// jsonParsed follows `encode_ui_account`. The parser gets the unsliced data,
// and `space` is the unsliced length. A dataSlice applies only where the parse
// fails and the body falls back to base64.
func renderAccount(pubkey [32]byte, a ffi.Account, cfg accountConfig, m *mintReader) (*uiAccount, error) {
	ui := &uiAccount{
		Executable: a.Executable,
		Lamports:   a.Lamports,
		Owner:      base58.Encode(a.Owner[:]),
		RentEpoch:  a.RentEpoch,
		Space:      uint64(len(a.Data)),
	}
	if cfg.Encoding == "jsonParsed" {
		if parsed, ok := parseAccountData(pubkey, a.Owner, a.Data, m.forAccount(a.Owner, a.Data)); ok {
			ui.Data = parsed
			return ui, nil
		}
	}
	data := a.Data
	if slice := cfg.DataSlice; slice != nil {
		// Agave's slice_data, written so that no sum can overflow.
		start := min(slice.Offset, uint(len(data)))
		data = data[start : start+min(slice.Length, uint(len(data))-start)]
	}
	switch cfg.Encoding {
	case "base58", "binary":
		// Above MAX_BASE58_BYTES the RPC refuses the account with -32600.
		// A notification carries Agave's text instead.
		body := base58.Encode(data)
		if len(data) > 128 {
			if !cfg.notification {
				return nil, &rpcError{Code: codeInvalidRequest,
					Message: "Encoded binary (base 58) data should be less than 128 bytes, please use Base64 encoding."}
			}
			body = "error: data too large for bs58 encoding"
		}
		if cfg.Encoding == "binary" {
			ui.Data = body
		} else {
			ui.Data = [2]string{body, "base58"}
		}
	case "base64+zstd":
		ui.Data = [2]string{base64.StdEncoding.EncodeToString(zstdEncoder.EncodeAll(data, nil)), "base64+zstd"}
	default:
		ui.Data = [2]string{base64.StdEncoding.EncodeToString(data), "base64"}
	}
	return ui, nil
}

// parsedBody reports whether the rendering is a parsed object rather than a
// binary body, which is what the plural methods filter on.
func parsedBody(ui *uiAccount) bool {
	_, ok := ui.Data.(*parsedAccount)
	return ok
}

func getAccountInfo(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	pk, err := pubkeyParam(params, 0)
	if err != nil {
		return nil, err
	}
	var cfg accountConfig
	if err := param(params, 1, &cfg); err != nil {
		return nil, err
	}
	if cfg, err = cfg.or("binary"); err != nil {
		return nil, err
	}
	ms, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if err := contextAt(params, 1, h); err != nil {
		return nil, err
	}
	ui, err := encodeAccount(pk, s.app.Stores.GetAccount(ms, pk), cfg, s.mints(ms, cfg, false))
	if err != nil {
		return nil, err
	}
	return at(h, ui), nil
}

func getMultipleAccounts(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	var keys []string
	if err := param(params, 0, &keys); err != nil {
		return nil, err
	}
	if len(keys) > 100 {
		return nil, invalidParams("Too many inputs provided; max 100")
	}
	var cfg accountConfig
	if err := param(params, 1, &cfg); err != nil {
		return nil, err
	}
	cfg, err := cfg.or("base64")
	if err != nil {
		return nil, err
	}
	pks := make([][32]byte, len(keys))
	for i, k := range keys {
		pk, err := decodeKey(k)
		if err != nil {
			return nil, err
		}
		pks[i] = pk
	}
	ms, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if err := contextAt(params, 1, h); err != nil {
		return nil, err
	}
	out := make([]*uiAccount, len(keys))
	mints := s.mints(ms, cfg, false)
	for i, pk := range pks {
		var err error
		if out[i], err = encodeAccount(pk, s.app.Stores.GetAccount(ms, pk), cfg, mints); err != nil {
			return nil, err
		}
	}
	return at(h, out), nil
}

func getBalance(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	pk, err := pubkeyParam(params, 0)
	if err != nil {
		return nil, err
	}
	ms, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if err := contextAt(params, 1, h); err != nil {
		return nil, err
	}
	var lamports uint64
	if v := s.app.Stores.GetAccount(ms, pk); v != nil {
		lamports = ffi.Lamports(v)
	}
	return at(h, lamports), nil
}

func getLatestBlockhash(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	ms, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if err := contextAt(params, 0, h); err != nil {
		return nil, err
	}
	hash := ms.GetKVStore(s.app.Stores.Meta).Get(svm.MetaBlockhashKey)
	return at(h, map[string]any{
		"blockhash":            base58.Encode(hash),
		"lastValidBlockHeight": lastValidBlockHeight(h),
	}), nil
}

// isBlockhashValid answers whether a transaction with the hash passes the age
// check in the next block. The engine's window at height h+1 is h+1's own hash
// plus the 150 RecentBlockhashes entries, ages 1 to 150. So at committed
// height h, a hash is valid when it is h's hash or one of the first 149
// entries. It reads the same committed sysvar as the engine, so the two
// cannot disagree.
func isBlockhashValid(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	var v string
	if err := param(params, 0, &v); err != nil {
		return nil, err
	}
	// Hash::from_str's error alone, with no "Invalid param: " before it.
	want, fault := parseBase58(v, 32)
	if fault != "" {
		return nil, invalidParams("%s", fault)
	}
	ms, next, err := s.app.Next()
	if err != nil {
		return nil, &rpcError{Code: codeInternal, Message: err.Error()}
	}
	h := next.Height - 1
	if err := contextAt(params, 1, h); err != nil {
		return nil, err
	}
	valid, err := s.app.Stores.BlockhashValid(ms, want)
	if err != nil {
		return nil, err
	}
	return at(h, valid), nil
}

func getFeeForMessage(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	var msg string
	if err := param(params, 0, &msg); err != nil {
		return nil, err
	}
	// Base64 always, with the caps a transaction has.
	raw, err := decodeWire(msg, "base64", messageType)
	if err != nil {
		return nil, err
	}
	_, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if err := contextAt(params, 1, h); err != nil {
		return nil, err
	}
	// The engine parses and sanitizes the message in one step. A message
	// that does not deserialize gets wincode's reason.
	fee, known, err := ffi.FeeForMessage(raw)
	if err != nil {
		return nil, invalidParams("failed to deserialize %s: %s", messageType, wincodeReason(raw, true, err))
	}
	if !known {
		return at(h, nil), nil
	}
	return at(h, fee), nil
}

// maxPermittedDataLength is the largest account a program can allocate.
const maxPermittedDataLength = 10 * 1024 * 1024

// getMinimumBalanceForRentExemption refuses a length that no account can have
// as an invalid request, as Agave does. Without the check, a length near 2^64
// overflows the product below.
func getMinimumBalanceForRentExemption(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	var n uint64
	if err := param(params, 0, &n); err != nil {
		return nil, err
	}
	if n > maxPermittedDataLength {
		return nil, &rpcError{Code: codeInvalidRequest, Message: "Invalid request"}
	}
	ms, _, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	perByte, err := s.app.Stores.LamportsPerByte(ms)
	if err != nil {
		return nil, err
	}
	return svm.RentExemptMinimum(perByte, n), nil
}

func getBlockHeight(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	_, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if err := contextAt(params, 0, h); err != nil {
		return nil, err
	}
	return h, nil
}

// simulateTransaction runs a transaction against committed state through the
// engine's simulate entry, never the block path. It answers in Agave's
// RpcSimulateTransactionResult shape, with the requested accounts and inner
// instructions.
func simulateTransaction(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	var encoded string
	var cfg struct {
		SigVerify              bool    `json:"sigVerify"`
		ReplaceRecentBlockhash bool    `json:"replaceRecentBlockhash"`
		Encoding               string  `json:"encoding"`
		MinContextSlot         *uint64 `json:"minContextSlot"`
		Accounts               *struct {
			Addresses []string `json:"addresses"`
			Encoding  string   `json:"encoding"`
		} `json:"accounts"`
		InnerInstructions bool `json:"innerInstructions"`
	}
	if err := param(params, 0, &encoded); err != nil {
		return nil, err
	}
	if err := param(params, 1, &cfg); err != nil {
		return nil, err
	}
	if err := validEncoding(cfg.Encoding, transactionEncodings); err != nil {
		return nil, err
	}
	if cfg.Accounts != nil {
		if err := validEncoding(cfg.Accounts.Encoding, accountEncodings); err != nil {
			return nil, err
		}
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
	if cfg.SigVerify && cfg.ReplaceRecentBlockhash {
		return nil, invalidParams("sigVerify may not be used with replaceRecentBlockhash")
	}
	// Only the accounts that the config names are copied out of the engine.
	// The code below reads no others. An address that does not decode is
	// refused below, and only if the simulation succeeded, as on Agave.
	var want [][32]byte
	if cfg.Accounts != nil {
		for _, address := range cfg.Accounts.Addresses {
			if pk, err := decodeKey(address); err == nil {
				want = append(want, pk)
			}
		}
	}
	value, sim, ms, h, err := s.simulate(raw, cfg.SigVerify, cfg.ReplaceRecentBlockhash, want)
	if err != nil {
		return nil, err
	}
	_, statics, err := ffi.TransactionKeys(raw)
	if err != nil {
		return nil, err
	}
	keys := accountKeys(statics, sim.Writable, sim.Readonly)
	// The balances are present whenever the processor saw the transaction.
	if sim.PreBalances != nil {
		value["preBalances"] = sim.PreBalances
		value["postBalances"] = sim.PostBalances
		value["preTokenBalances"] = uiTokenBalances(sim.PreTokenBalances)
		value["postTokenBalances"] = uiTokenBalances(sim.PostTokenBalances)
	}
	if cfg.InnerInstructions && sim.Inner != nil {
		value["innerInstructions"] = parsedInner(sim.Inner, keys)
	}
	if cfg.Accounts != nil {
		encoding := cfg.Accounts.Encoding
		if encoding == "" {
			encoding = "base64"
		}
		if encoding == "binary" || encoding == "base58" {
			return nil, invalidParams("base58 encoding not supported")
		}
		if len(cfg.Accounts.Addresses) > len(keys) {
			return nil, invalidParams("Too many accounts provided; max %d", len(keys))
		}
		accounts := make([]any, len(cfg.Accounts.Addresses))
		if sim.Status == 0 {
			post := make(map[[32]byte]ffi.Account, len(sim.Accounts))
			for _, a := range sim.Accounts {
				post[a.Pubkey] = a
			}
			acfg := accountConfig{Encoding: encoding}
			mints := s.mints(ms, acfg, false)
			for i, address := range cfg.Accounts.Addresses {
				pk, err := decodeKey(address)
				if err != nil {
					return nil, err
				}
				var ui *uiAccount
				if a, ok := post[pk]; ok {
					ui, err = renderAccount(pk, a, acfg, mints)
				} else {
					ui, err = encodeAccount(pk, s.app.Stores.GetAccount(ms, pk), acfg, mints)
				}
				if err != nil {
					return nil, err
				}
				if ui != nil {
					accounts[i] = ui
				}
			}
		}
		value["accounts"] = accounts
	}
	return at(h, value), nil
}

// simulate runs a transaction as the next block runs it, on committed state as
// that block's begin-block leaves it. It returns the result in Agave's
// RpcSimulateTransactionResult shape, beside the engine's own answer. h is
// the committed height, which the response's context names.
// simulateTransaction and the send path's preflight both use it.
// sim.Accounts holds the accounts in want after the run, and no others.
func (s *Server) simulate(raw []byte, sigVerify, replaceBlockhash bool, want [][32]byte) (value map[string]any, sim ffi.Simulation, ms storetypes.MultiStore, h uint64, err error) {
	ms, next, err := s.app.Next()
	if err != nil {
		return nil, sim, nil, 0, &rpcError{Code: codeInternal, Message: err.Error()}
	}
	h = next.Height - 1
	keys, _, err := ffi.TransactionKeys(raw)
	if err != nil {
		return nil, sim, nil, 0, invalidParams("invalid transaction: %v", err)
	}
	loaded, err := s.lookups(ms, next.Height, raw)
	if err != nil {
		return nil, sim, nil, 0, err
	}
	hash := next.Hash
	// Agave's simulation checks the status cache. A replaced blockhash is the
	// latest one, which no processed transaction can name yet. A transaction
	// that admission refuses has no dedup key to look up.
	processed := false
	if !replaceBlockhash {
		if a, err := ffi.CheckTx(raw, true, next.Height); err == nil {
			processed = s.app.Processed(a.DedupKey)
		}
	}
	w := ffi.NewArenaWriter(ffi.KindSimulateReq, 0)
	w.Push(raw)
	// A simulation loads at most what Anza lets one transaction load. The cut
	// accounts are named as in a block's load list, so a transaction over the
	// limit is starved and answered as Agave answers it.
	dropped, _ := s.app.Stores.PushAccounts(w, ms, union(keys, loaded), svm.SimulationBytes, nil)
	cut := make([]byte, 0, 32*len(dropped))
	for _, k := range dropped {
		cut = append(cut, k[:]...)
	}
	w.Push(cut)
	if sim, err = ffi.Simulate(next.Height, hash, svm.NextOwnHash, sigVerify, replaceBlockhash, processed, w, want...); err != nil {
		return nil, sim, nil, 0, err
	}
	value = map[string]any{
		"err":                    nil,
		"logs":                   sim.Logs,
		"accounts":               nil,
		"unitsConsumed":          sim.Units,
		"loadedAccountsDataSize": sim.LoadedAccountsDataSize,
		"returnData":             nil,
		"innerInstructions":      nil,
		"replacementBlockhash":   nil,
		"fee":                    nil,
		"preBalances":            nil,
		"postBalances":           nil,
		"preTokenBalances":       nil,
		"postTokenBalances":      nil,
		"loadedAddresses":        map[string]any{"readonly": base58Keys(sim.Readonly), "writable": base58Keys(sim.Writable)},
	}
	if sim.Logs == nil {
		value["logs"] = []string{}
	}
	if sim.Status != 0 {
		value["err"] = sim.Err
	}
	if sim.Status != 2 {
		value["fee"] = sim.Fee
	}
	if sim.ReturnProgram != nil {
		value["returnData"] = map[string]any{
			"programId": base58.Encode(sim.ReturnProgram[:]),
			"data":      []string{base64.StdEncoding.EncodeToString(sim.ReturnData), "base64"},
		}
	}
	if replaceBlockhash {
		value["replacementBlockhash"] = map[string]any{"blockhash": base58.Encode(hash[:]), "lastValidBlockHeight": lastValidBlockHeight(h)}
	}
	return value, sim, ms, h, nil
}

// lookups returns the accounts that a v0 transaction's lookup tables add to
// its load list. These are the tables themselves, which the engine resolves
// against, and the addresses they resolve to at height h. A message whose
// tables do not resolve gets invalid params. Agave answers the same from the
// same point, the simulation's sanitize.
func (s *Server) lookups(ms storetypes.MultiStore, h uint64, raw []byte) ([][32]byte, error) {
	tables, err := ffi.LookupTables(raw)
	if err != nil || len(tables) == 0 {
		return nil, err
	}
	values := make(map[[32]byte][]byte, len(tables))
	for _, k := range tables {
		if v := s.app.Stores.GetAccount(ms, k); v != nil {
			values[k] = v
		}
	}
	slotHashes, err := ffi.DecodeAccount(s.app.Stores.GetAccount(ms, svm.SlotHashesID))
	if err != nil {
		return nil, err
	}
	writable, readonly, refused, err := ffi.LookupAddresses(h, raw, slotHashes.Data, values)
	if err != nil {
		return nil, err
	}
	if refused != "" {
		return nil, invalidParams("invalid transaction: %s", refused)
	}
	return append(append(tables, writable...), readonly...), nil
}

// union is a's keys then b's, each once, in first-seen order.
func union(a, b [][32]byte) [][32]byte {
	seen := make(map[[32]byte]bool, len(a)+len(b))
	out := make([][32]byte, 0, len(a)+len(b))
	for _, k := range append(append([][32]byte(nil), a...), b...) {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

type signatureStatus struct {
	Slot               uint64 `json:"slot"`
	Confirmations      *int   `json:"confirmations"`
	Err                any    `json:"err"`
	Status             any    `json:"status"`
	ConfirmationStatus string `json:"confirmationStatus"`
}

// statusOf renders a record's status. Every committed transaction is
// finalized, confirmations is null, and the legacy status field is present.
func statusOf(r svm.Record) signatureStatus {
	out := signatureStatus{Slot: r.Height, ConfirmationStatus: "finalized", Status: map[string]any{"Ok": nil}}
	// The status word is consensus and the error is diagnostics, so the branch
	// tests the word, never the rendering. Status 1 and status 2 both carry an
	// error. They differ in whether the fee was charged and the dedup key spent.
	if r.Status != 0 {
		out.Err = r.Meta.Err
		out.Status = map[string]any{"Err": r.Meta.Err}
	}
	return out
}

// signatureParam is Agave's verify_signature.
func signatureParam(v string) ([64]byte, error) {
	b, fault := parseBase58(v, 64)
	if fault != "" {
		return [64]byte{}, invalidParams("Invalid param: %s", fault)
	}
	return [64]byte(b), nil
}

// getSignatureStatuses answers from the history record. Without
// searchTransactionHistory it answers only inside the replay window, which
// plays the part of Agave's status cache. The config's one field is
// required, as in Agave's RpcSignatureStatusConfig.
func getSignatureStatuses(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	var sigs []string
	if err := param(params, 0, &sigs); err != nil {
		return nil, err
	}
	var cfg struct {
		SearchTransactionHistory *bool `json:"searchTransactionHistory"`
	}
	if err := param(params, 1, &cfg); err != nil {
		return nil, err
	}
	if len(params) > 1 && !isNull(params[1]) && cfg.SearchTransactionHistory == nil {
		return nil, invalidParams("Invalid params: missing field `searchTransactionHistory`.")
	}
	search := cfg.SearchTransactionHistory != nil && *cfg.SearchTransactionHistory
	if len(sigs) > 256 {
		return nil, invalidParams("Too many inputs provided; max 256")
	}
	_, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	committed := s.notified
	s.mu.Unlock()
	out := make([]*signatureStatus, len(sigs))
	for i, v := range sigs {
		sig, err := signatureParam(v)
		if err != nil {
			return nil, err
		}
		r, ok, err := s.history().Get(sig, committed)
		if err != nil {
			return nil, err
		}
		if ok && (search || svm.InBlockhashWindow(committed, r.Height)) {
			status := statusOf(r)
			out[i] = &status
		}
	}
	return at(h, out), nil
}

// maxSignaturesForAddress is Agave's `MAX_GET_CONFIRMED_SIGNATURES_FOR_ADDRESS2_LIMIT`,
// which is also the default.
const maxSignaturesForAddress = 1_000

// getSignaturesForAddress answers in Agave's
// `RpcConfirmedTransactionStatusWithSignature` shape, newest first, with
// `memo` read through the engine's decode kind.
func getSignaturesForAddress(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	address, err := pubkeyParam(params, 0)
	if err != nil {
		return nil, err
	}
	cfg := struct {
		contextConfig
		Before string `json:"before"`
		Until  string `json:"until"`
		Limit  *int   `json:"limit"`
	}{}
	if err := param(params, 1, &cfg); err != nil {
		return nil, err
	}
	if err := atLeastConfirmed(cfg.Commitment); err != nil {
		return nil, err
	}
	limit := maxSignaturesForAddress
	if cfg.Limit != nil {
		limit = *cfg.Limit
	}
	if limit <= 0 || limit > maxSignaturesForAddress {
		return nil, invalidParams("Invalid limit; max %d", maxSignaturesForAddress)
	}
	bound := func(v string) (*[64]byte, error) {
		if v == "" {
			return nil, nil
		}
		sig, err := signatureParam(v)
		if err != nil {
			return nil, err
		}
		return &sig, nil
	}
	before, err := bound(cfg.Before)
	if err != nil {
		return nil, err
	}
	until, err := bound(cfg.Until)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	committed := s.notified
	s.mu.Unlock()
	if err := reached(cfg.MinContextSlot, committed); err != nil {
		return nil, err
	}
	// A bound that history does not hold gets Agave's FilterTransactionNotFound.
	for _, sig := range []*[64]byte{before, until} {
		if sig == nil {
			continue
		}
		if _, ok, err := s.history().Get(*sig, committed); err != nil {
			return nil, err
		} else if !ok {
			return nil, &rpcError{Code: codeFilterTransactionNotFound, Message: fmt.Sprintf("Transaction %s not found", base58.Encode(sig[:]))}
		}
	}
	rows, err := s.history().SignaturesForAddress(address, before, until, limit, committed)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, len(rows))
	for i, r := range rows {
		var failure any
		if r.Status != 0 {
			failure = r.Err
		}
		memo, err := memos(r.Tx, r.LoadedWritable, r.LoadedReadonly)
		if err != nil {
			return nil, err
		}
		out[i] = map[string]any{
			"signature":          base58.Encode(r.Signature[:]),
			"slot":               r.Height,
			"err":                failure,
			"memo":               memo,
			"blockTime":          r.BlockTime,
			"confirmationStatus": "finalized",
			"transactionIndex":   r.Index,
		}
	}
	return out, nil
}

// getTransaction serves a committed transaction above the history horizon, or
// null. It answers in Agave's EncodedConfirmedTransactionWithStatusMeta shape,
// json by default.
func getTransaction(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	var v string
	if err := param(params, 0, &v); err != nil {
		return nil, err
	}
	sig, err := signatureParam(v)
	if err != nil {
		return nil, err
	}
	cfg := struct {
		contextConfig
		Encoding                       *string `json:"encoding"`
		MaxSupportedTransactionVersion *int    `json:"maxSupportedTransactionVersion"`
	}{}
	if len(params) > 1 && len(params[1]) > 0 && params[1][0] == '"' {
		if err := param(params, 1, &cfg.Encoding); err != nil {
			return nil, err
		}
	} else if err := param(params, 1, &cfg); err != nil {
		return nil, err
	}
	encoding := orDefault(cfg.Encoding, "json")
	if err := renderedEncoding(encoding); err != nil {
		return nil, err
	}
	if err := atLeastConfirmed(cfg.Commitment); err != nil {
		return nil, err
	}
	s.mu.Lock()
	committed := s.notified
	s.mu.Unlock()
	r, ok, err := s.history().Get(sig, committed)
	if err != nil || !ok {
		return nil, err
	}
	out, err := encodedTransaction(r, encoding, cfg.MaxSupportedTransactionVersion, true)
	if err != nil {
		return nil, err
	}
	out["slot"] = r.Height
	out["blockTime"] = r.BlockTime
	out["transactionIndex"] = r.Index
	return out, nil
}

// blockCleanedUp is Agave's `RpcCustomError::BlockCleanedUp`, JSON-RPC code
// -32001. getBlock answers a purged height with it, not with null, because
// null means "not yet" and this height never comes back.
const blockCleanedUp = -32001

// getFirstAvailableBlock returns the history horizon plus one, the number that
// the purged-block error names.
func getFirstAvailableBlock(_ context.Context, s *Server, _ []jsontext.Value) (any, error) {
	return s.history().FirstAvailableBlock(), nil
}

// getBlock serves a height from the primary history keyspace, which holds the
// transactions, their meta and their order, keyed by height.
func getBlock(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	var height uint64
	if len(params) == 0 {
		return nil, invalidParams("missing slot")
	}
	if err := param(params, 0, &height); err != nil {
		return nil, err
	}
	cfg := struct {
		contextConfig
		Encoding                       *string `json:"encoding"`
		TransactionDetails             *string `json:"transactionDetails"`
		Rewards                        *bool   `json:"rewards"`
		MaxSupportedTransactionVersion *int    `json:"maxSupportedTransactionVersion"`
	}{}
	// The bare string is RpcEncodingConfigWrapper::Deprecated, an encoding
	// with every other field at its default.
	if len(params) > 1 && len(params[1]) > 0 && params[1][0] == '"' {
		if err := param(params, 1, &cfg.Encoding); err != nil {
			return nil, err
		}
	} else if err := param(params, 1, &cfg); err != nil {
		return nil, err
	}
	encoding := orDefault(cfg.Encoding, "json")
	details := orDefault(cfg.TransactionDetails, "full")
	if err := renderedEncoding(encoding); err != nil {
		return nil, err
	}
	if err := atLeastConfirmed(cfg.Commitment); err != nil {
		return nil, err
	}
	switch details {
	case "full", "signatures", "none", "accounts":
	default:
		return nil, invalidParams("Invalid param: transactionDetails must be one of full, signatures, none, accounts")
	}
	showRewards := cfg.Rewards == nil || *cfg.Rewards

	committed := s.committed()
	if height > committed {
		// Agave's BlockNotAvailable above its root.
		return nil, &rpcError{Code: codeBlockNotAvailable, Message: fmt.Sprintf("Block not available for slot %d", height)}
	}
	hash, ok, err := s.history().Blockhash(height)
	if err != nil {
		return nil, err
	}
	if !ok {
		first := s.history().FirstAvailableBlock()
		return nil, &rpcError{Code: blockCleanedUp, Message: fmt.Sprintf(
			"Block %d cleaned up, does not exist on node. First available block: %d", height, first)}
	}
	// A purged parent leaves no hash to name. That happens only at the oldest
	// height that history still holds.
	parent, _, err := s.history().Blockhash(height - 1)
	if err != nil {
		return nil, err
	}
	records, err := s.history().AtHeight(height)
	if err != nil {
		return nil, err
	}

	out := map[string]any{
		"previousBlockhash": base58.Encode(parent[:]),
		"blockhash":         base58.Encode(hash[:]),
		"parentSlot":        height - 1,
		"blockTime":         nil,
		"blockHeight":       height,
	}
	if t, ok, err := s.history().BlockTime(height); err != nil {
		return nil, err
	} else if ok {
		out["blockTime"] = t
	} else if len(records) > 0 {
		out["blockTime"] = records[0].BlockTime
	}
	if err := blockBody(out, records, details, encoding, cfg.MaxSupportedTransactionVersion, showRewards); err != nil {
		return nil, err
	}
	return out, nil
}

func nonNil(v []uint64) []uint64 {
	if v == nil {
		return []uint64{}
	}
	return v
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// send is one queued notification. Every fan-out builds its list under s.mu
// and queues it after the release, so no code touches a connection's queue
// under the registry lock.
type send struct {
	conn *wsConn
	note note
}

// signatureSubscribe notifies once, when the transaction commits, and then
// drops the subscription, as Agave does. With enableReceivedNotification it
// first sends receivedSignature when the block holding the transaction reaches
// this machine, before it commits. A signature already committed inside the
// replay window gets its notification at the next commit. The replay window
// plays the part of Agave's status cache, and Agave's notifier finds the
// signature there on its next bank.
func signatureSubscribe(s *Server, c *wsConn, params []jsontext.Value) (any, error) {
	var raw string
	if err := param(params, 0, &raw); err != nil {
		return nil, err
	}
	var cfg struct {
		contextConfig
		EnableReceivedNotification bool `json:"enableReceivedNotification"`
	}
	if err := param(params, 1, &cfg); err != nil {
		return nil, err
	}
	key, err := subscriptionKey(raw, 64, "signature")
	if err != nil {
		return nil, err
	}
	sig := [64]byte(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	r, committed, err := s.history().Get(sig, s.notified)
	if err != nil {
		return nil, err
	}
	if err := s.admit(c); err != nil {
		return nil, err
	}
	x := &sub{conn: c, method: signatureNotification, sig: sig, received: cfg.EnableReceivedNotification,
		params: fmt.Sprintf("signature|%x|%s|%v", sig, level(cfg.Commitment), cfg.EnableReceivedNotification)}
	if committed && svm.InBlockhashWindow(s.notified, r.Height) {
		status := statusOf(r)
		x.landed = &status
	}
	return s.register(x), nil
}

// signatureUnsubscribe is unsubscribe. A signature subscription that never
// fires stays held until the client cancels it or the connection closes.
func signatureUnsubscribe(s *Server, c *wsConn, params []jsontext.Value) (any, error) {
	return unsubscribe(s, c, params)
}

// dropConn removes what this connection held, and only that. It walks the
// connection's own subscriptions, not the whole registry, so a disconnect
// costs what that client subscribed to, not what the machine holds.
func (s *Server) dropConn(c *wsConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range c.subs {
		s.unregister(c, id)
	}
}

// history is the app's transaction record.
func (s *Server) history() *svm.History { return s.app.History }

// Follow notifies subscribers for each committed height. The block-end writer
// records a block before the app commits it, so Follow walks only up to the
// app's committed height. It advances the cursor that statuses and
// subscriptions read, under the same lock.
func (s *Server) Follow(ctx context.Context) error {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case r := <-s.receipts:
			s.mu.Lock()
			sends := s.receivedFor(r, nil)
			s.mu.Unlock()
			for _, x := range sends {
				x.conn.deliver(x.note)
			}
		case <-tick.C:
			s.catchUp()
		}
	}
}

// catchUp notifies each height up to the height that reads serve. That height
// is CommittedHeight, which App.Commit records after BaseApp.Commit returns.
// LastBlockHeight moves inside BaseApp.Commit, when the multistore commits. A
// client notified from it can read the block before the one it was told about.
func (s *Server) catchUp() {
	committed := s.app.CommittedHeight()
	for {
		s.mu.Lock()
		height := s.notified + 1
		s.mu.Unlock()
		if height > committed {
			return
		}
		if err := s.notify(height); err != nil {
			s.logger.Error("svm signature notifications", "height", height, "err", err)
			return
		}
	}
}

// notify serves one committed height to every subscriber. Signatures and logs
// come from the records. Accounts and programs come from the block write set
// that the runner published. Slot subscribers get the height itself. Follow
// never runs ahead of the committed height, so a client told about an account
// can always read it back.
//
// Follow decodes the records only when a signature or logs subscriber reads
// them, and it decodes them outside the registry lock. The fee window takes
// the lowest price of the block from the feed. The runner set that price from
// the same records.
func (s *Server) notify(height uint64) error {
	s.mu.Lock()
	read := s.readsRecords()
	s.mu.Unlock()
	var records []svm.Record
	if read {
		var err error
		if records, err = s.history().AtHeight(height); err != nil {
			return err
		}
	}
	sends, err := s.collect(height, records, read)
	if err != nil {
		return err
	}
	for _, x := range sends {
		x.conn.deliver(x.note)
	}
	return nil
}

// readsRecords reports whether a signature or logs subscriber reads the
// records of a height. The caller holds s.mu.
func (s *Server) readsRecords() bool { return len(s.watchers) > 0 || len(s.logs) > 0 }

// collect builds the notifications of one height under the registry lock.
// records holds the records of the height when read is true.
//
// Two cases read the records here, under the lock. One is a subscriber that
// came in after notify looked. The other is a height that the feed does not
// hold, because only the records have its price. Each read comes before
// collect takes anything from the feed, or when there is nothing to take. So
// an error loses nothing, and the next tick serves the height again.
func (s *Server) collect(height uint64, records []svm.Record, read bool) ([]send, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	load := func() (err error) {
		if !read {
			records, err = s.history().AtHeight(height)
			read = err == nil
		}
		return err
	}
	if s.readsRecords() {
		if err := load(); err != nil {
			return nil, err
		}
	}
	block, held := s.app.Feed.Take(height)
	if !held {
		if err := load(); err != nil {
			return nil, err
		}
		block.Lowest, block.Priced = svm.LowestPrice(records)
	}
	var sends []send
	// A receipt that Follow has not taken yet goes first, so that
	// receivedSignature still comes before the result of the same block.
	for queued := true; queued; {
		select {
		case r := <-s.receipts:
			sends = s.receivedFor(r, sends)
		default:
			queued = false
		}
	}
	for _, r := range records {
		var result jsontext.Value
		for _, x := range slices.Clone(s.watchers[r.Signature]) {
			if result == nil {
				result = s.encode(height, notifyAt(height, map[string]any{"err": statusOf(r).Err}))
			}
			if result != nil {
				sends = append(sends, send{x.conn, x.notify(result)})
			}
			s.unregister(x.conn, x.id)
		}
		sends = s.logsFor(height, r, sends)
	}
	var landed []*sub
	for _, list := range s.watchers {
		for _, x := range list {
			if x.landed != nil {
				if b := s.encode(height, notifyAt(height, map[string]any{"err": x.landed.Err})); b != nil {
					sends = append(sends, send{x.conn, x.notify(b)})
				}
				landed = append(landed, x)
			}
		}
	}
	for _, x := range landed {
		s.unregister(x.conn, x.id)
	}
	sends = s.fanOut(height, block.Writes, sends)
	sends = s.slotsFor(height, sends)
	sends = s.updatesFor(height, sends)
	s.observeFees(height, block.Lowest, block.Priced)
	s.notified = height
	return sends, nil
}

// programAccountsConfig is getProgramAccounts' second parameter: the account
// encoding plus the filters and the context wrapper.
type programAccountsConfig struct {
	accountConfig
	WithContext bool            `json:"withContext"`
	Filters     []accountFilter `json:"filters"`
}

// accountFilter is one RpcFilterType: {"dataSize": n}, {"memcmp": {...}} or
// the unit variant tokenAccountState, which serde reads as the bare string or
// as {"tokenAccountState": null}.
type accountFilter struct {
	DataSize          *uint64
	Memcmp            *memcmp
	TokenAccountState bool
}

// memcmp is Agave's Memcmp. Its bytes are a string, base58 unless the
// encoding says base64, or a raw byte array.
type memcmp struct {
	Offset   uint           `json:"offset"`
	Bytes    jsontext.Value `json:"bytes"`
	Encoding *string        `json:"encoding"`
}

func (f *accountFilter) UnmarshalJSON(b []byte) error {
	var word string
	if json.Unmarshal(b, &word, agaveInput) == nil {
		if word != "tokenAccountState" {
			return fmt.Errorf("unknown variant `%s`, expected one of `dataSize`, `memcmp`, `tokenAccountState`", word)
		}
		f.TokenAccountState = true
		return nil
	}
	var keys map[string]jsontext.Value
	if err := json.Unmarshal(b, &keys, agaveInput); err != nil {
		return err
	}
	if v, ok := keys["tokenAccountState"]; ok && len(keys) == 1 {
		if !isNull(v) {
			return errors.New("invalid type: expected unit variant RpcFilterType::TokenAccountState")
		}
		f.TokenAccountState = true
		return nil
	}
	var one struct {
		DataSize *uint64 `json:"dataSize"`
		Memcmp   *memcmp `json:"memcmp"`
	}
	if err := json.Unmarshal(b, &one, agaveInput); err != nil {
		return err
	}
	if (one.DataSize == nil) == (one.Memcmp == nil) {
		return errors.New("expected one of `dataSize`, `memcmp`, `tokenAccountState`")
	}
	if m := one.Memcmp; m != nil {
		if b := bytes.TrimSpace(m.Bytes); len(b) == 0 || b[0] != '"' {
			if _, ok := byteArray(b); !ok {
				return errors.New("data did not match any variant of untagged enum DataType")
			}
		}
		if e := m.Encoding; e != nil && *e != "base58" && *e != "base64" && *e != "bytes" {
			return fmt.Errorf("unknown variant `%s`, expected one of `base58`, `base64`, `bytes`", *e)
		}
	}
	f.DataSize, f.Memcmp = one.DataSize, one.Memcmp
	return nil
}

// byteArray reads the raw form of a memcmp's bytes: a JSON array of numbers
// from 0 to 255. json/v2 reads a []byte only from a base64 string, so the
// array is read as numbers first.
func byteArray(b []byte) ([]byte, bool) {
	var n []uint16
	if json.Unmarshal(b, &n, agaveInput) != nil {
		return nil, false
	}
	out := make([]byte, len(n))
	for i, v := range n {
		if v > 255 {
			return nil, false
		}
		out[i] = byte(v)
	}
	return out, true
}

// maxFilters is Agave's MAX_GET_PROGRAM_ACCOUNT_FILTERS.
const maxFilters = 4

// The memcmp caps.
const (
	maxFilterBytes  = 128
	maxFilterBase58 = 175
	maxFilterBase64 = 172
)

// matcher is a filter with its comparison bytes already decoded, so a scan
// decodes them once rather than once per account.
type matcher struct {
	size   *uint64
	token  bool
	offset uint
	bytes  []byte
}

func (m matcher) match(data []byte) bool {
	switch {
	case m.size != nil:
		return uint64(len(data)) == *m.size
	case m.token:
		return svm.ValidTokenAccountData(data)
	}
	if m.offset > uint(len(data)) || uint(len(m.bytes)) > uint(len(data))-m.offset {
		return false
	}
	return bytes.Equal(data[m.offset:m.offset+uint(len(m.bytes))], m.bytes)
}

// matchers is verify_filters. It checks the count, then each memcmp against
// its caps, with Agave's texts.
func matchers(filters []accountFilter) ([]matcher, error) {
	if len(filters) > maxFilters {
		return nil, invalidParams("Too many filters provided; max %d", maxFilters)
	}
	out := make([]matcher, 0, len(filters))
	for _, f := range filters {
		switch {
		case f.DataSize != nil:
			out = append(out, matcher{size: f.DataSize})
		case f.TokenAccountState:
			out = append(out, matcher{token: true})
		default:
			raw, fault := memcmpBytes(f.Memcmp)
			if fault != "" {
				return nil, invalidParams("Invalid param: %s", fault)
			}
			out = append(out, matcher{offset: f.Memcmp.Offset, bytes: raw})
		}
	}
	return out, nil
}

// memcmpBytes decodes a memcmp's bytes, or names its RpcFilterError by Debug.
func memcmpBytes(m *memcmp) ([]byte, string) {
	var raw []byte
	var text string
	if json.Unmarshal(m.Bytes, &text, agaveInput) != nil {
		// A raw byte array, which UnmarshalJSON has checked.
		raw, _ = byteArray(m.Bytes)
		if len(raw) > maxFilterBytes {
			return nil, "DataTooLarge"
		}
		return raw, ""
	}
	var err error
	if m.Encoding != nil && *m.Encoding == "base64" {
		if len(text) > maxFilterBase64 {
			return nil, "DataTooLarge"
		}
		if raw, err = decodeBase64(text); err != nil {
			return nil, fmt.Sprintf("Base64DecodeError(%v)", err)
		}
	} else {
		if len(text) > maxFilterBase58 {
			return nil, "DataTooLarge"
		}
		if raw, err = decodeBase58(text); err != nil {
			return nil, fmt.Sprintf("Base58DecodeError(%v)", err)
		}
	}
	if len(raw) > maxFilterBytes {
		return nil, "DataTooLarge"
	}
	return raw, ""
}

// tokenKeyFilters is the length check of get_spl_token_owner_filter and
// get_spl_token_mint_filter. On a token program, a memcmp at the owner's
// offset, then at the mint's, must be a whole key.
func tokenKeyFilters(program [32]byte, filters []matcher) error {
	if !svm.IsTokenProgram(program) {
		return nil
	}
	for _, check := range []struct {
		offset uint
		name   string
	}{{32, "owner"}, {0, "mint"}} {
		for _, f := range filters {
			if f.size == nil && !f.token && f.offset == check.offset && len(f.bytes) != 32 {
				return invalidParams("Incorrect byte length %d for SPL token %s filter, expected 32", len(f.bytes), check.name)
			}
		}
	}
	return nil
}

type programAccount struct {
	Account *uiAccount `json:"account"`
	Pubkey  string     `json:"pubkey"`
}

// getProgramAccounts is an unindexed scan of the eight account shards, with
// the filters applied in memory. The owner is in the account value, not the
// key, so no filter pushes down into a key range. Agave does the same by
// default. Its ProgramId index is an operator flag, not a chain property.
//
// The singletons are in svmmeta, which is not an account store, so no scan
// sees them. This is the only served method whose cost grows with total state.
// It is never on a consensus path, so a slow query costs the serving operator
// and delays no block.
func getProgramAccounts(ctx context.Context, s *Server, params []jsontext.Value) (any, error) {
	program, err := pubkeyParam(params, 0)
	if err != nil {
		return nil, err
	}
	var cfg programAccountsConfig
	if err := param(params, 1, &cfg); err != nil {
		return nil, err
	}
	if cfg.accountConfig, err = cfg.accountConfig.or("binary"); err != nil {
		return nil, err
	}
	filters, err := matchers(cfg.Filters)
	if err != nil {
		return nil, err
	}
	if err := tokenKeyFilters(program, filters); err != nil {
		return nil, err
	}
	// The scan opens at the latest version, before another block can commit,
	// so it reads that version whole however long it runs.
	ms, h, scan, err := s.app.ScanLatest()
	if err != nil {
		return nil, err
	}
	out := []programAccount{}
	mints := s.mints(ms, cfg.accountConfig, true)
	// The drop is Agave's, and so is its condition. Only a scan of a token
	// program takes the parsed path that can drop a row.
	drop := mints != nil && svm.IsTokenProgram(program)
	kept := scanned{limit: s.scanLimit}
	err = scan.Each(func(pk [32]byte, value []byte) error {
		// A client that hangs up ends its scan, and the version it holds.
		if err := ctx.Err(); err != nil {
			return err
		}
		a, err := ffi.DecodeAccount(value)
		if err != nil {
			return err
		}
		if a.Owner != program || !matchAll(filters, a.Data) {
			return nil
		}
		if err := kept.keep(len(a.Data)); err != nil {
			return err
		}
		ui, err := renderAccount(pk, a, cfg.accountConfig, mints)
		if err != nil {
			return err
		}
		if !drop || parsedBody(ui) {
			out = append(out, programAccount{Account: ui, Pubkey: base58.Encode(pk[:])})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if cfg.WithContext {
		return at(h, out), nil
	}
	return out, nil
}

// base58Keys renders resolved lookup table addresses the way every other
// account key is rendered, and never as null.
func base58Keys(keys [][32]byte) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, base58.Encode(k[:]))
	}
	return out
}

// uiTokenAmount is Agave's `UiTokenAmount`.
type uiTokenAmount struct {
	UIAmount       *float64 `json:"uiAmount"`
	Decimals       uint8    `json:"decimals"`
	Amount         string   `json:"amount"`
	UIAmountString string   `json:"uiAmountString"`
}

// tokenAmount is the plain branch of `token_amount_to_ui_amount_v3`. The
// string is `real_number_string_trimmed`, the decimal-shifted amount stripped
// of trailing zeros and a bare point. The float is the same value as an f64.
// amountToUI takes the two Token-2022 branches, interest-bearing and scaled
// amounts, before it falls back here.
func tokenAmount(amount uint64, decimals uint8) uiTokenAmount {
	s := strconv.FormatUint(amount, 10)
	if decimals > 0 {
		for len(s) < int(decimals)+1 {
			s = "0" + s
		}
		s = s[:len(s)-int(decimals)] + "." + s[len(s)-int(decimals):]
	}
	out := uiTokenAmount{Decimals: decimals, Amount: strconv.FormatUint(amount, 10), UIAmountString: trimUIAmount(s, decimals)}
	// `10_usize.checked_pow(decimals)` does not fit past nineteen digits, and
	// Agave's answer is then a null float, not an infinity.
	if divisor, ok := pow10(decimals); ok {
		ui := float64(amount) / divisor
		out.UIAmount = &ui
	}
	return out
}

// getTokenAccountsByOwner is a prefix read of the owner index, the reason the
// index exists. A full traversal of 10,000,000 accounts takes 127 seconds and
// cannot serve a wallet.
//
// The `{mint}` filter compares offset 0 of each hit. That costs nothing,
// because the hits are already this owner's token accounts. Agave has the same
// shape.
func getTokenAccountsByOwner(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	owner, err := pubkeyParam(params, 0)
	if err != nil {
		return nil, err
	}
	filter, err := tokenAccountsFilter(params, 1)
	if err != nil {
		return nil, err
	}
	var cfg accountConfig
	if err := param(params, 2, &cfg); err != nil {
		return nil, err
	}
	if cfg, err = cfg.or("binary"); err != nil {
		return nil, err
	}
	ms, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if err := contextAt(params, 2, h); err != nil {
		return nil, err
	}
	program, err := s.tokenProgram(ms, filter)
	if err != nil {
		return nil, err
	}
	if s.app.Tokens == nil {
		return nil, excludedFromIndex(owner)
	}
	keys, err := s.app.Tokens.Accounts(owner)
	if errors.Is(err, svm.ErrTokenIndexBehind) {
		// Agave's node-unhealthy shape, not an empty list that a wallet reads
		// as a zero balance.
		return nil, nodeUnhealthy(nil)
	}
	if err != nil {
		return nil, err
	}
	out := []programAccount{}
	mints := s.mints(ms, cfg, true)
	kept := scanned{limit: s.scanLimit}
	for _, key := range keys {
		v := s.app.Stores.GetAccount(ms, key)
		if v == nil {
			continue
		}
		a, err := ffi.DecodeAccount(v)
		if err != nil {
			return nil, err
		}
		token, ok := svm.ParseTokenAccount(a.Owner, a.Data)
		// The index is maintained, not authoritative. The loop skips an entry
		// whose account is no longer a token account.
		if !ok || token.Owner != owner || a.Owner != program {
			continue
		}
		if filter.mint != nil && token.Mint != *filter.mint {
			continue
		}
		if err := kept.keep(len(a.Data)); err != nil {
			return nil, err
		}
		ui, err := renderAccount(key, a, cfg, mints)
		if err != nil {
			return nil, err
		}
		// Every row here is a token account, so under jsonParsed the plural
		// drop always applies. Mainnet cannot emit a base64 body inside this
		// array.
		if mints != nil && !parsedBody(ui) {
			continue
		}
		out = append(out, programAccount{Account: ui, Pubkey: base58.Encode(key[:])})
	}
	return at(h, out), nil
}

// tokenFilter is a verified RpcTokenAccountsFilter: a mint, or a program.
type tokenFilter struct {
	mint, program *[32]byte
}

// tokenAccountsFilter reads RpcTokenAccountsFilter, which serde reads as a map
// with exactly one key, mint or programId, and verifies its key.
func tokenAccountsFilter(params []jsontext.Value, i int) (tokenFilter, error) {
	var raw map[string]jsontext.Value
	if err := param(params, i, &raw); err != nil {
		return tokenFilter{}, err
	}
	if len(raw) != 1 {
		return tokenFilter{}, invalidParams("Invalid params: invalid value: map, expected map with a single key.")
	}
	for name, v := range raw {
		if name != "mint" && name != "programId" {
			return tokenFilter{}, invalidParams("Invalid params: unknown variant `%s`, expected `mint` or `programId`.", name)
		}
		var text string
		if err := json.Unmarshal(v, &text, agaveInput); err != nil {
			return tokenFilter{}, invalidParams("Invalid params: %v.", reason(err))
		}
		key, err := decodeKey(text)
		if err != nil {
			return tokenFilter{}, err
		}
		if name == "mint" {
			return tokenFilter{mint: &key}, nil
		}
		return tokenFilter{program: &key}, nil
	}
	return tokenFilter{}, nil
}

// tokenProgram is get_token_program_id_and_mint. It returns the named program
// if it is a token program, or else the owner of the named mint.
func (s *Server) tokenProgram(ms storetypes.MultiStore, f tokenFilter) ([32]byte, error) {
	if f.program != nil {
		if !svm.IsTokenProgram(*f.program) {
			return [32]byte{}, invalidParams("Invalid param: unrecognized Token program id")
		}
		return *f.program, nil
	}
	owner, _, err := s.mintOwner(ms, *f.mint)
	if err != nil {
		return [32]byte{}, err
	}
	if !svm.IsTokenProgram(owner) {
		return [32]byte{}, invalidParams("Invalid param: not a Token mint")
	}
	return owner, nil
}

// mintOwner is get_mint_owner_and_additional_data. It answers the wrapped-SOL
// mint without a read. It reads any other mint and unpacks it, whatever its
// owner.
func (s *Server) mintOwner(ms storetypes.MultiStore, mint [32]byte) ([32]byte, *mintData, error) {
	if mint == nativeMint {
		return svm.SplToken, &mintData{decimals: nativeMintDecimals}, nil
	}
	v := s.app.Stores.GetAccount(ms, mint)
	if v == nil {
		return [32]byte{}, nil, invalidParams("Invalid param: could not find mint")
	}
	a, err := ffi.DecodeAccount(v)
	if err != nil {
		return [32]byte{}, nil, err
	}
	reader := &mintReader{s: s, ms: ms, seen: map[[32]byte]*mintData{}}
	d, ok := mintDataFromAccount(a.Data, reader.clock())
	if !ok {
		return [32]byte{}, nil, invalidParams("Invalid param: Token mint could not be unpacked")
	}
	return a.Owner, d, nil
}

// getTokenAccountBalance is Agave's get_token_account_balance. It reads the
// account, which must be a token program's, and its mint, whose extensions
// scale the amount.
func getTokenAccountBalance(_ context.Context, s *Server, params []jsontext.Value) (any, error) {
	pubkey, err := pubkeyParam(params, 0)
	if err != nil {
		return nil, err
	}
	ms, h, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	v := s.app.Stores.GetAccount(ms, pubkey)
	if v == nil {
		return nil, invalidParams("Invalid param: could not find account")
	}
	a, err := ffi.DecodeAccount(v)
	if err != nil {
		return nil, err
	}
	token, ok := svm.ParseTokenAccount(a.Owner, a.Data)
	if !ok {
		return nil, invalidParams("Invalid param: not a Token account")
	}
	_, mint, err := s.mintOwner(ms, token.Mint)
	if err != nil {
		return nil, err
	}
	return at(h, amountToUI(token.Amount, mint)), nil
}
