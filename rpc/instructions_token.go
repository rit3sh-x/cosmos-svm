package rpc

import (
	"encoding/binary"
	"strconv"
	"unicode/utf8"
)

// parseTokenInstruction is parse_token for both SPL Token and Token-2022, over
// Token-2022's TokenInstruction. When that does not unpack, it tries the token
// group and token metadata interfaces.
func parseTokenInstruction(ix instruction) (any, bool) {
	if !ix.keysInRange() {
		return nil, false
	}
	if len(ix.data) > 0 {
		if out, ok, known := parseTokenCore(ix); known {
			return out, ok
		}
	}
	if out, ok := parseTokenGroup(ix); ok {
		return out, true
	}
	if out, ok := parseTokenMetadata(ix); ok {
		return out, true
	}
	return nil, false
}

// authorityTypes are AuthorityType's camelCase names, in tag order, and
// whether each is a mint's.
var authorityTypes = []struct {
	name string
	mint bool
}{
	{"mintTokens", true}, {"freezeAccount", true}, {"accountOwner", false}, {"closeAccount", false},
	{"transferFeeConfig", true}, {"withheldWithdraw", true}, {"closeMint", true}, {"interestRate", true},
	{"permanentDelegate", true}, {"confidentialTransferMint", true}, {"transferHookProgramId", true},
	{"confidentialTransferFeeConfig", true}, {"metadataPointer", true}, {"groupPointer", true},
	{"groupMemberPointer", true}, {"scaledUiAmount", true}, {"pause", true}, {"permissionedBurn", true},
}

// extensionTypeNames are ExtensionType's camelCase names, by their u16.
var extensionTypeNames = []string{
	"uninitialized", "transferFeeConfig", "transferFeeAmount", "mintCloseAuthority", "confidentialTransferMint",
	"confidentialTransferAccount", "defaultAccountState", "immutableOwner", "memoTransfer", "nonTransferable",
	"interestBearingConfig", "cpiGuard", "permanentDelegate", "nonTransferableAccount", "transferHook",
	"transferHookAccount", "confidentialTransferFeeConfig", "confidentialTransferFeeAmount", "metadataPointer",
	"tokenMetadata", "groupPointer", "tokenGroup", "groupMemberPointer", "tokenGroupMember",
	"confidentialMintBurn", "scaledUiAmount", "pausable", "pausableAccount", "permissionedBurn",
}

// extensionTypeName is ExtensionType::try_from([u8; 2]).
func extensionTypeName(b []byte) (string, bool) {
	if len(b) != 2 {
		return "", false
	}
	v := int(binary.LittleEndian.Uint16(b))
	if v >= len(extensionTypeNames) {
		return "", false
	}
	return extensionTypeNames[v], true
}

// extensionList is the trailing list of GetAccountDataSize and Reallocate.
func extensionList(rest []byte) ([]string, bool) {
	var out []string
	for i := 0; i < len(rest); i += 2 {
		name, ok := extensionTypeName(rest[i:min(i+2, len(rest))])
		if !ok {
			return nil, false
		}
		out = append(out, name)
	}
	return out, true
}

// pubkeyOption is unpack_pubkey_option: 0, or 1 and a key.
func (r *reader) pubkeyOption() any {
	switch r.u8() {
	case 0:
		return nil
	case 1:
		return r.key()
	default:
		r.bad = true
		return nil
	}
}

// signers is parse_signers. It writes the authority alone, or a multisig and
// the signers after it.
func (ix instruction) signers(info map[string]any, last int, owner, multisig string) {
	if len(ix.accounts) > last+1 {
		var signers []string
		for i := last + 1; i < len(ix.accounts); i++ {
			signers = append(signers, ix.key(i))
		}
		info[multisig] = ix.key(last)
		info["signers"] = signers
	} else {
		info[owner] = ix.key(last)
	}
}

// parseTokenCore parses a TokenInstruction. known is false when the data does
// not unpack as one, so the caller tries the group and metadata interfaces.
func parseTokenCore(ix instruction) (out any, ok, known bool) {
	r := &reader{b: ix.data[1:]}
	tag := ix.data[0]
	n := len(ix.accounts)
	fail := func() (any, bool, bool) { return nil, false, true }
	amount := func(field string) map[string]any {
		return map[string]any{field: strconv.FormatUint(r.u64(), 10)}
	}
	checked := func() uiTokenAmount {
		a, d := r.u64(), r.u8()
		return tokenAmount(a, d)
	}
	switch tag {
	case 0, 20:
		decimals, authority, freeze := r.u8(), r.key(), r.pubkeyOption()
		if r.bad {
			return nil, false, false
		}
		need, kind := 2, "initializeMint"
		if tag == 20 {
			need, kind = 1, "initializeMint2"
		}
		if n < need {
			return fail()
		}
		info := map[string]any{"mint": ix.key(0), "decimals": decimals, "mintAuthority": authority}
		if tag == 0 {
			info["rentSysvar"] = ix.key(1)
		}
		if freeze != nil {
			info["freezeAuthority"] = freeze
		}
		return enum(kind, info), true, true
	case 1:
		if n < 4 {
			return fail()
		}
		return enum("initializeAccount", map[string]any{"account": ix.key(0), "mint": ix.key(1), "owner": ix.key(2),
			"rentSysvar": ix.key(3)}), true, true
	case 2, 19:
		m := r.u8()
		if r.bad {
			return nil, false, false
		}
		first, kind := 2, "initializeMultisig"
		if tag == 19 {
			first, kind = 1, "initializeMultisig2"
		}
		if n < first+1 {
			return fail()
		}
		var signers []string
		for i := first; i < n; i++ {
			signers = append(signers, ix.key(i))
		}
		info := map[string]any{"multisig": ix.key(0), "signers": signers, "m": m}
		if tag == 2 {
			info["rentSysvar"] = ix.key(1)
		}
		return enum(kind, info), true, true
	case 3, 4, 7, 8:
		info := amount("amount")
		if r.bad {
			return nil, false, false
		}
		if n < 3 {
			return fail()
		}
		switch tag {
		case 3:
			info["source"], info["destination"] = ix.key(0), ix.key(1)
			ix.signers(info, 2, "authority", "multisigAuthority")
			return enum("transfer", info), true, true
		case 4:
			info["source"], info["delegate"] = ix.key(0), ix.key(1)
			ix.signers(info, 2, "owner", "multisigOwner")
			return enum("approve", info), true, true
		case 7:
			info["mint"], info["account"] = ix.key(0), ix.key(1)
			ix.signers(info, 2, "mintAuthority", "multisigMintAuthority")
			return enum("mintTo", info), true, true
		default:
			info["account"], info["mint"] = ix.key(0), ix.key(1)
			ix.signers(info, 2, "authority", "multisigAuthority")
			return enum("burn", info), true, true
		}
	case 5:
		if n < 2 {
			return fail()
		}
		info := map[string]any{"source": ix.key(0)}
		ix.signers(info, 1, "owner", "multisigOwner")
		return enum("revoke", info), true, true
	case 6:
		t := int(r.u8())
		if r.bad || t >= len(authorityTypes) {
			return nil, false, false
		}
		newAuthority := r.pubkeyOption()
		if r.bad {
			return nil, false, false
		}
		if n < 2 {
			return fail()
		}
		owned := "account"
		if authorityTypes[t].mint {
			owned = "mint"
		}
		info := map[string]any{owned: ix.key(0), "authorityType": authorityTypes[t].name, "newAuthority": newAuthority}
		ix.signers(info, 1, "authority", "multisigAuthority")
		return enum("setAuthority", info), true, true
	case 9:
		if n < 3 {
			return fail()
		}
		info := map[string]any{"account": ix.key(0), "destination": ix.key(1)}
		ix.signers(info, 2, "owner", "multisigOwner")
		return enum("closeAccount", info), true, true
	case 10, 11:
		if n < 3 {
			return fail()
		}
		info := map[string]any{"account": ix.key(0), "mint": ix.key(1)}
		ix.signers(info, 2, "freezeAuthority", "multisigFreezeAuthority")
		kind := "freezeAccount"
		if tag == 11 {
			kind = "thawAccount"
		}
		return enum(kind, info), true, true
	case 12, 13:
		ui := checked()
		if r.bad {
			return nil, false, false
		}
		if n < 4 {
			return fail()
		}
		info := map[string]any{"source": ix.key(0), "mint": ix.key(1), "tokenAmount": ui}
		if tag == 12 {
			info["destination"] = ix.key(2)
			ix.signers(info, 3, "authority", "multisigAuthority")
			return enum("transferChecked", info), true, true
		}
		info["delegate"] = ix.key(2)
		ix.signers(info, 3, "owner", "multisigOwner")
		return enum("approveChecked", info), true, true
	case 14, 15:
		ui := checked()
		if r.bad {
			return nil, false, false
		}
		if n < 3 {
			return fail()
		}
		if tag == 14 {
			info := map[string]any{"mint": ix.key(0), "account": ix.key(1), "tokenAmount": ui}
			ix.signers(info, 2, "mintAuthority", "multisigMintAuthority")
			return enum("mintToChecked", info), true, true
		}
		info := map[string]any{"account": ix.key(0), "mint": ix.key(1), "tokenAmount": ui}
		ix.signers(info, 2, "authority", "multisigAuthority")
		return enum("burnChecked", info), true, true
	case 16, 18:
		owner := r.key()
		if r.bad {
			return nil, false, false
		}
		if tag == 16 {
			if n < 3 {
				return fail()
			}
			return enum("initializeAccount2", map[string]any{"account": ix.key(0), "mint": ix.key(1), "owner": owner,
				"rentSysvar": ix.key(2)}), true, true
		}
		if n < 2 {
			return fail()
		}
		return enum("initializeAccount3", map[string]any{"account": ix.key(0), "mint": ix.key(1), "owner": owner}), true, true
	case 17:
		if n < 1 {
			return fail()
		}
		return enum("syncNative", map[string]any{"account": ix.key(0)}), true, true
	case 21, 29:
		list, okList := extensionList(r.b)
		if !okList {
			return nil, false, false
		}
		if tag == 29 {
			return parseReallocate(ix, list)
		}
		if n < 1 {
			return fail()
		}
		info := map[string]any{"mint": ix.key(0)}
		if len(list) > 0 {
			info["extensionTypes"] = list
		}
		return enum("getAccountDataSize", info), true, true
	case 22:
		if n < 1 {
			return fail()
		}
		return enum("initializeImmutableOwner", map[string]any{"account": ix.key(0)}), true, true
	case 23:
		info := amount("amount")
		if r.bad {
			return nil, false, false
		}
		if n < 1 {
			return fail()
		}
		info["mint"] = ix.key(0)
		return enum("amountToUiAmount", info), true, true
	case 24:
		if !utf8.Valid(r.b) {
			return nil, false, false
		}
		if n < 1 {
			return fail()
		}
		return enum("uiAmountToAmount", map[string]any{"mint": ix.key(0), "uiAmount": string(r.b)}), true, true
	case 25:
		authority := r.pubkeyOption()
		if r.bad {
			return nil, false, false
		}
		return parseMintCloseAuthority(ix, authority)
	case 26:
		return parseTransferFee(ix, ix.data[1:])
	case 27:
		return parseConfidentialTransfer(ix, ix.data[1:])
	case 28:
		if len(ix.data) <= 2 {
			return fail()
		}
		return parseDefaultAccountState(ix, ix.data[1:])
	case 30:
		if len(ix.data) < 2 {
			return fail()
		}
		return parseMemoTransfer(ix, ix.data[1:])
	case 31:
		if n < 3 {
			return fail()
		}
		return enum("createNativeMint", map[string]any{"payer": ix.key(0), "nativeMint": ix.key(1), "systemProgram": ix.key(2)}), true, true
	case 32:
		if n < 1 {
			return fail()
		}
		return enum("initializeNonTransferableMint", map[string]any{"mint": ix.key(0)}), true, true
	case 33:
		if len(ix.data) < 2 {
			return fail()
		}
		return parseInterestBearingMint(ix, ix.data[1:])
	case 34:
		if len(ix.data) < 2 {
			return fail()
		}
		return parseCPIGuard(ix, ix.data[1:])
	case 35:
		delegate := r.key()
		if r.bad {
			return nil, false, false
		}
		return parsePermanentDelegate(ix, delegate)
	case 36:
		if len(ix.data) < 2 {
			return fail()
		}
		return parseTransferHook(ix, ix.data[1:])
	case 37:
		if len(ix.data) < 2 {
			return fail()
		}
		return parseConfidentialTransferFee(ix, ix.data[1:])
	case 38:
		if n < 3 {
			return fail()
		}
		info := map[string]any{"source": ix.key(0), "destination": ix.key(1)}
		ix.signers(info, 2, "authority", "multisigAuthority")
		return enum("withdrawExcessLamports", info), true, true
	case 39:
		if len(ix.data) < 2 {
			return fail()
		}
		return parsePointer(ix, ix.data[1:], "MetadataPointer", "metadataAddress")
	case 40:
		if len(ix.data) < 2 {
			return fail()
		}
		return parsePointer(ix, ix.data[1:], "GroupPointer", "groupAddress")
	case 41:
		if len(ix.data) < 2 {
			return fail()
		}
		return parsePointer(ix, ix.data[1:], "GroupMemberPointer", "memberAddress")
	case 42:
		return parseConfidentialMintBurn(ix, ix.data[1:])
	case 43:
		return parseScaledUIAmount(ix, ix.data[1:])
	case 44:
		return parsePausable(ix, ix.data[1:])
	case 45:
		var a any
		switch r.u8() {
		case 0:
		case 1:
			a = strconv.FormatUint(r.u64(), 10)
		default:
			r.bad = true
		}
		if r.bad {
			return nil, false, false
		}
		if n < 3 {
			return fail()
		}
		info := map[string]any{"source": ix.key(0), "destination": ix.key(1)}
		if a != nil {
			info["amount"] = a
		}
		ix.signers(info, 2, "authority", "multisigAuthority")
		return enum("unwrapLamports", info), true, true
	case 46:
		return parsePermissionedBurn(ix, ix.data[1:])
	case 255:
		return parseBatch(ix, ix.data[1:])
	}
	return nil, false, false
}

// parseBatch is parse_batch_instruction. Each inner instruction is a u8
// account count, a u8 data length and the data, over the outer accounts
// in order. A nested batch, or an inner instruction that does not parse,
// fails the whole batch.
func parseBatch(ix instruction, data []byte) (any, bool, bool) {
	var instructions []any
	cursor, accounts := 0, 0
	for cursor < len(data) {
		if cursor+2 > len(data) {
			return nil, false, true
		}
		numAccounts, dataLen := int(data[cursor]), int(data[cursor+1])
		cursor += 2
		if cursor+dataLen > len(data) || accounts+numAccounts > len(ix.accounts) {
			return nil, false, true
		}
		inner := data[cursor : cursor+dataLen]
		cursor += dataLen
		innerAccounts := ix.accounts[accounts : accounts+numAccounts]
		accounts += numAccounts
		if len(inner) > 0 && inner[0] == 255 {
			return nil, false, true
		}
		parsed, ok := parseTokenInstruction(instruction{innerAccounts, inner, ix.keys})
		if !ok {
			return nil, false, true
		}
		instructions = append(instructions, parsed)
	}
	if instructions == nil {
		instructions = []any{}
	}
	return enum("batch", map[string]any{"instructions": instructions}), true, true
}
