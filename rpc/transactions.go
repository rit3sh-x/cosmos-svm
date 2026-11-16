package rpc

import (
	"encoding/base64"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// The renderer that getTransaction and getBlock share. It is Anza's
// solana-transaction-status 4.3.0 rendering of a stored transaction. The
// engine's decode kind takes the message apart, and tools/tx-oracle is the
// oracle.

// accountKeys is a message's full key list: its static keys, then the keys
// its lookup tables loaded, writable before readonly.
func accountKeys(static, writable, readonly [][32]byte) [][32]byte {
	return slices.Concat(static, writable, readonly)
}

// renderedEncoding refuses what serde refuses.
func renderedEncoding(encoding string) error {
	return validEncoding(encoding, transactionEncodings)
}

// txVersion is `validate_version`. A legacy transaction has no version for a
// client that names no maximum. A versioned one above the maximum is refused.
// present is false if the field is omitted.
func txVersion(stored uint8, maxVersion *int) (version any, present bool, err error) {
	if stored == 0 {
		return "legacy", maxVersion != nil, nil
	}
	n := int(stored) - 1
	if maxVersion == nil || *maxVersion < n {
		return nil, false, &rpcError{Code: -32015, Message: fmt.Sprintf("Transaction version (%d) is not supported by the requesting client. "+
			"Please try the request again with the following configuration parameter: \"maxSupportedTransactionVersion\": %d", n, n)}
	}
	return n, true, nil
}

// encodedTransaction is Agave's `EncodedTransactionWithStatusMeta`. A client
// that turns rewards off gets a null rewards field in the meta, which is
// what `OptionSerializer::None` writes.
func encodedTransaction(r svm.Record, encoding string, maxVersion *int, showRewards bool) (map[string]any, error) {
	version, present, err := txVersion(r.Meta.Version, maxVersion)
	if err != nil {
		return nil, err
	}
	tx, err := encodeTransaction(r, encoding)
	if err != nil {
		return nil, err
	}
	meta, err := uiMeta(r, encoding == "jsonParsed")
	if err != nil {
		return nil, err
	}
	if !showRewards {
		meta["rewards"] = nil
	}
	out := map[string]any{"transaction": tx, "meta": meta}
	if present {
		out["version"] = version
	}
	return out, nil
}

// blockBody adds a block's transactions, or its signatures, and its rewards
// to out, as `encode_with_options` does.
func blockBody(out map[string]any, records []svm.Record, details, encoding string, maxVersion *int, showRewards bool) error {
	switch details {
	case "full", "accounts":
		txs := []map[string]any{}
		for _, r := range records {
			var one map[string]any
			var err error
			if details == "full" {
				one, err = encodedTransaction(r, encoding, maxVersion, showRewards)
			} else {
				one, err = accountsTransaction(r, maxVersion, showRewards)
			}
			if err != nil {
				return err
			}
			txs = append(txs, one)
		}
		out["transactions"] = txs
	case "signatures":
		sigs := []string{}
		for _, r := range records {
			sigs = append(sigs, base58.Encode(r.Signature[:]))
		}
		out["signatures"] = sigs
	}
	// This chain pays no block rewards. The fee is burned in full, and
	// there is no vote account to credit. The field is an empty list if
	// asked for and absent if not, as on Agave.
	if showRewards {
		out["rewards"] = []any{}
	}
	return nil
}

// accountsTransaction is `build_json_accounts`, the transactionDetails
// accounts rendering. It holds the signatures, the parsed account list, and
// `build_simple_ui_transaction_status_meta`.
func accountsTransaction(r svm.Record, maxVersion *int, showRewards bool) (map[string]any, error) {
	version, present, err := txVersion(r.Meta.Version, maxVersion)
	if err != nil {
		return nil, err
	}
	d, err := ffi.Decode(r.Tx, r.Meta.LoadedWritable, r.Meta.LoadedReadonly)
	if err != nil {
		return nil, err
	}
	keys := accountKeys(d.Keys, r.Meta.LoadedWritable, r.Meta.LoadedReadonly)
	accounts := parsedAccounts(d, keys)
	meta := map[string]any{
		"err":               nil,
		"status":            map[string]any{"Ok": nil},
		"fee":               r.Fee,
		"preBalances":       nonNil(r.Meta.PreBalances),
		"postBalances":      nonNil(r.Meta.PostBalances),
		"preTokenBalances":  storedTokenBalances(r.Meta.PreTokenBalances),
		"postTokenBalances": storedTokenBalances(r.Meta.PostTokenBalances),
	}
	if r.Status != 0 {
		meta["err"] = r.Meta.Err
		meta["status"] = map[string]any{"Err": r.Meta.Err}
	}
	if showRewards {
		meta["rewards"] = []any{}
	}
	out := map[string]any{
		"transaction": map[string]any{"signatures": signatures(d.Signatures), "accountKeys": accounts},
		"meta":        meta,
	}
	if present {
		out["version"] = version
	}
	return out, nil
}

// parsedAccounts is `parse_*_message_accounts`. It lists each key with its
// flags, and whether the message or a lookup table named it.
func parsedAccounts(d ffi.Decoded, keys [][32]byte) []map[string]any {
	accounts := make([]map[string]any, len(d.Writable))
	for i := range accounts {
		source := "transaction"
		if i >= len(d.Keys) {
			source = "lookupTable"
		}
		accounts[i] = map[string]any{"pubkey": base58.Encode(keys[i][:]), "writable": d.Writable[i], "signer": d.Signer[i], "source": source}
	}
	return accounts
}

// encodeTransaction is `encode_with_meta`. binary is a bare base58 string,
// and json is the raw message. jsonParsed is the parsed message, its accounts
// flagged and its instructions parsed over the static and loaded keys.
func encodeTransaction(r svm.Record, encoding string) (any, error) {
	switch encoding {
	case "binary":
		return base58.Encode(r.Tx), nil
	case "base58":
		return []string{base58.Encode(r.Tx), "base58"}, nil
	case "base64":
		return []string{base64.StdEncoding.EncodeToString(r.Tx), "base64"}, nil
	}
	d, err := ffi.Decode(r.Tx, r.Meta.LoadedWritable, r.Meta.LoadedReadonly)
	if err != nil {
		return nil, err
	}
	keys := accountKeys(d.Keys, r.Meta.LoadedWritable, r.Meta.LoadedReadonly)
	instructions := make([]map[string]any, len(d.Instructions))
	for i, ix := range d.Instructions {
		// TRANSACTION_LEVEL_STACK_HEIGHT.
		if encoding == "jsonParsed" {
			instructions[i] = uiInstruction(ix.ProgramIDIndex, ix.Accounts, ix.Data, keys, 1)
		} else {
			instructions[i] = map[string]any{"programIdIndex": ix.ProgramIDIndex, "accounts": indexes(ix.Accounts), "data": base58.Encode(ix.Data), "stackHeight": 1}
		}
	}
	message := map[string]any{
		"accountKeys":     base58Keys(d.Keys),
		"recentBlockhash": base58.Encode(d.Blockhash[:]),
		"instructions":    instructions,
	}
	if encoding == "jsonParsed" {
		message["accountKeys"] = parsedAccounts(d, keys)
	} else {
		message["header"] = map[string]any{
			"numRequiredSignatures":       d.NumRequiredSignatures,
			"numReadonlySignedAccounts":   d.NumReadonlySigned,
			"numReadonlyUnsignedAccounts": d.NumReadonlyUnsigned,
		}
	}
	switch d.Version {
	case 1:
		lookups := make([]map[string]any, len(d.Lookups))
		for i, l := range d.Lookups {
			lookups[i] = map[string]any{"accountKey": base58.Encode(l.Table[:]), "writableIndexes": indexes(l.Writable), "readonlyIndexes": indexes(l.Readonly)}
		}
		message["addressTableLookups"] = lookups
	case 2:
		c := d.Config
		message["transactionConfig"] = map[string]any{
			"priorityFee": c.PriorityFee, "computeUnitLimit": c.ComputeUnitLimit,
			"loadedAccountsDataSizeLimit": c.LoadedAccountsDataSizeLimit, "heapSize": c.HeapSize,
		}
	}
	return map[string]any{"signatures": signatures(d.Signatures), "message": message}, nil
}

// uiMeta is `UiTransactionStatusMeta::from`, or for jsonParsed
// `parse_ui_transaction_status_meta`, which parses the inner instructions
// and leaves loadedAddresses out. costUnits is this chain's cost. It is
// absent if the record does not carry it.
func uiMeta(r svm.Record, parsed bool) (map[string]any, error) {
	var inner []map[string]any
	if parsed {
		d, err := ffi.Decode(r.Tx, r.Meta.LoadedWritable, r.Meta.LoadedReadonly)
		if err != nil {
			return nil, err
		}
		keys := accountKeys(d.Keys, r.Meta.LoadedWritable, r.Meta.LoadedReadonly)
		inner = parsedInner(r.Meta.Inner, keys)
	} else {
		inner = []map[string]any{}
		for index, group := range r.Meta.Inner {
			if len(group) == 0 {
				continue
			}
			instructions := make([]map[string]any, len(group))
			for k, ix := range group {
				instructions[k] = map[string]any{"programIdIndex": ix.ProgramIDIndex, "accounts": indexes(ix.Accounts), "data": base58.Encode(ix.Data), "stackHeight": ix.StackHeight}
			}
			inner = append(inner, map[string]any{"index": index, "instructions": instructions})
		}
	}
	meta := map[string]any{
		"err":                  nil,
		"status":               map[string]any{"Ok": nil},
		"fee":                  r.Fee,
		"preBalances":          nonNil(r.Meta.PreBalances),
		"postBalances":         nonNil(r.Meta.PostBalances),
		"innerInstructions":    inner,
		"logMessages":          nonNilStrings(r.Meta.Logs),
		"preTokenBalances":     storedTokenBalances(r.Meta.PreTokenBalances),
		"postTokenBalances":    storedTokenBalances(r.Meta.PostTokenBalances),
		"rewards":              []any{},
		"loadedAddresses":      map[string]any{"writable": base58Keys(r.Meta.LoadedWritable), "readonly": base58Keys(r.Meta.LoadedReadonly)},
		"computeUnitsConsumed": r.Units,
	}
	if r.Status != 0 {
		meta["err"] = r.Meta.Err
		meta["status"] = map[string]any{"Err": r.Meta.Err}
	}
	if r.Meta.ReturnProgram != nil {
		meta["returnData"] = map[string]any{
			"programId": base58.Encode(r.Meta.ReturnProgram[:]),
			"data":      []string{base64.StdEncoding.EncodeToString(r.Meta.ReturnData), "base64"},
		}
	}
	if r.Meta.CostUnits != nil {
		meta["costUnits"] = *r.Meta.CostUnits
	}
	if parsed {
		delete(meta, "loadedAddresses")
	}
	return meta, nil
}

// uiTokenBalances is `UiTransactionTokenBalance::from` over
// `token_amount_to_ui_amount_v3` with the decimals alone, which is
// tokenAmount's branch.
func uiTokenBalances(list []ffi.TokenBalance) []map[string]any {
	out := make([]map[string]any, len(list))
	for i, b := range list {
		out[i] = map[string]any{
			"accountIndex":  b.AccountIndex,
			"mint":          base58.Encode(b.Mint[:]),
			"uiTokenAmount": tokenAmount(b.Amount, b.Decimals),
			"owner":         base58.Encode(b.Owner[:]),
			"programId":     base58.Encode(b.ProgramID[:]),
		}
	}
	return out
}

// storedTokenBalances are a record's token balances as Agave serves them,
// read back from its protobuf store. That store keeps uiAmount as a plain f64
// and gives a zero back as null. A simulation's balances are not stored and
// keep the zero.
func storedTokenBalances(list []ffi.TokenBalance) []map[string]any {
	out := uiTokenBalances(list)
	for _, b := range out {
		if ui := b["uiTokenAmount"].(uiTokenAmount); ui.UIAmount != nil && math.Abs(*ui.UIAmount) <= epsilon {
			ui.UIAmount = nil
			b["uiTokenAmount"] = ui
		}
	}
	return out
}

// epsilon is Rust's f64::EPSILON.
const epsilon = 2.220446049250313e-16

// parsedInner is `parse_ui_inner_instructions` over each group that holds
// an instruction, every instruction parsed or partially decoded.
func parsedInner(groups [][]ffi.InnerInstruction, keys [][32]byte) []map[string]any {
	out := []map[string]any{}
	for index, group := range groups {
		if len(group) == 0 {
			continue
		}
		instructions := make([]map[string]any, len(group))
		for k, ix := range group {
			instructions[k] = uiInstruction(ix.ProgramIDIndex, ix.Accounts, ix.Data, keys, ix.StackHeight)
		}
		out = append(out, map[string]any{"index": index, "instructions": instructions})
	}
	return out
}

// memoPrograms are SPL Memo v1, v3 and v4.
var memoPrograms = map[string]bool{
	"Memo1UhkJRfHyvLMcVucJwxXeuD728EqVDDwQDxFMNo": true,
	"MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr": true,
	"Memo4c2pN8afCj432Lb7RMVKi9PbQnnW7ewFFaV3oAH": true,
}

// memos is `extract_and_fmt_memos`. It writes each top-level Memo instruction
// as "[length] text", joined by "; ", or returns nil when there is none.
func memos(tx []byte, writable, readonly [][32]byte) (any, error) {
	d, err := ffi.Decode(tx, writable, readonly)
	if err != nil {
		return nil, err
	}
	keys := accountKeys(d.Keys, writable, readonly)
	var out []string
	for _, ix := range d.Instructions {
		if int(ix.ProgramIDIndex) >= len(keys) || !memoPrograms[base58.Encode(keys[ix.ProgramIDIndex][:])] {
			continue
		}
		text := "(unparseable)"
		if utf8.Valid(ix.Data) {
			text = string(ix.Data)
		}
		out = append(out, fmt.Sprintf("[%d] %s", len(ix.Data), text))
	}
	if len(out) == 0 {
		return nil, nil
	}
	return strings.Join(out, "; "), nil
}

// indexes renders account indexes as numbers, because a []byte would marshal
// as base64.
func indexes(b []byte) []int {
	out := make([]int, len(b))
	for i, v := range b {
		out[i] = int(v)
	}
	return out
}

func signatures(sigs [][64]byte) []string {
	out := make([]string, len(sigs))
	for i, s := range sigs {
		out[i] = base58.Encode(s[:])
	}
	return out
}
