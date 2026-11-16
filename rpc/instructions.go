package rpc

import (
	"encoding/base64"
	"encoding/binary"
	"unicode/utf8"

	"github.com/mr-tron/base58"
)

// These are the jsonParsed instruction parsers. They port parse_instruction.rs
// from Agave's transaction-status 4.3.0 and the parse_* modules it dispatches
// to, for every program that genesis seeds. tools/tx-oracle renders the same
// instructions with Agave's own code, and the Go test compares the two.

// parsablePrograms is PARSABLE_PROGRAM_IDS without Stake and Vote, which this
// chain does not include. An instruction to either renders partially decoded.
// That is the one documented difference.
var parsablePrograms = map[string]struct {
	name  string
	parse func(ix instruction) (any, bool)
}{
	"AddressLookupTab1e1111111111111111111111111":  {"address-lookup-table", parseAddressLookupTable},
	"ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL": {"spl-associated-token-account", parseAssociatedToken},
	"Memo1UhkJRfHyvLMcVucJwxXeuD728EqVDDwQDxFMNo":  {"spl-memo", parseMemo},
	"MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr":  {"spl-memo", parseMemo},
	"Memo4c2pN8afCj432Lb7RMVKi9PbQnnW7ewFFaV3oAH":  {"spl-memo", parseMemo},
	"BPFLoader2111111111111111111111111111111111":  {"bpf-loader", parseBPFLoader},
	"BPFLoaderUpgradeab1e11111111111111111111111":  {"bpf-upgradeable-loader", parseBPFUpgradeableLoader},
	"11111111111111111111111111111111":             {"system", parseSystem},
	"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA":  {"spl-token", parseTokenInstruction},
	"TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb":  {"spl-token", parseTokenInstruction},
}

// instruction is a compiled instruction and the account keys its indexes
// name, static then loaded.
type instruction struct {
	accounts []byte
	data     []byte
	keys     [][32]byte
}

// key is the account at position i of the instruction, as a string.
func (ix instruction) key(i int) string { return base58.Encode(ix.keys[ix.accounts[i]][:]) }

// keysInRange is the first check of every parser except the BPF loader's. The
// instruction names at least one account, and none past the key list.
func (ix instruction) keysInRange() bool {
	if len(ix.accounts) == 0 {
		return false
	}
	for _, a := range ix.accounts {
		if int(a) >= len(ix.keys) {
			return false
		}
	}
	return true
}

// enum is ParsedInstructionEnum: the type, and the info when there is one.
func enum(kind string, info map[string]any) map[string]any {
	return map[string]any{"type": kind, "info": info}
}

// uiInstruction is parse_ui_instruction. It returns the parsed form for a
// program and instruction that the parsers know, and the partially decoded
// form for everything else. stackHeight is nil for a message's own
// instructions in a parsed message's meta context, and 1 in its instruction
// list, as Agave passes it.
func uiInstruction(programIndex uint8, accounts, data []byte, keys [][32]byte, stackHeight any) map[string]any {
	program := base58.Encode(keys[programIndex][:])
	if p, ok := parsablePrograms[program]; ok {
		if parsed, ok := p.parse(instruction{accounts, data, keys}); ok {
			return map[string]any{"program": p.name, "programId": program, "parsed": parsed, "stackHeight": stackHeight}
		}
	}
	named := make([]string, len(accounts))
	for i, a := range accounts {
		named[i] = base58.Encode(keys[a][:])
	}
	return map[string]any{"programId": program, "accounts": named, "data": base58.Encode(data), "stackHeight": stackHeight}
}

// reader is bincode's fixed-int little-endian decoding, and borsh's for the
// fields the two share. A read past the end sets bad.
type reader struct {
	b   []byte
	bad bool
}

func (r *reader) take(n int) []byte {
	if r.bad || n < 0 || n > len(r.b) {
		r.bad = true
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) u8() uint8 {
	if b := r.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (r *reader) u16() uint16 {
	if b := r.take(2); b != nil {
		return binary.LittleEndian.Uint16(b)
	}
	return 0
}

func (r *reader) u32() uint32 {
	if b := r.take(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (r *reader) u64() uint64 {
	if b := r.take(8); b != nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

func (r *reader) key() string {
	if b := r.take(32); b != nil {
		return base58.Encode(b)
	}
	return ""
}

// bytes is a bincode Vec<u8>: a u64 length, then the bytes.
func (r *reader) bytes() []byte {
	n := r.u64()
	if n > uint64(len(r.b)) {
		r.bad = true
		return nil
	}
	return r.take(int(n))
}

// str is a bincode String, which must be UTF-8.
func (r *reader) str() string {
	b := r.bytes()
	if !utf8.Valid(b) {
		r.bad = true
	}
	return string(b)
}

// parseMemo is parse_memo. It returns the data as text.
func parseMemo(ix instruction) (any, bool) {
	if !utf8.Valid(ix.data) {
		return nil, false
	}
	return string(ix.data), true
}

// parseSystem is parse_system, over SystemInstruction's bincode.
func parseSystem(ix instruction) (any, bool) {
	r := &reader{b: ix.data}
	tag := r.u32()
	var kind string
	var info map[string]any
	var need int
	switch tag {
	case 0:
		lamports, space, owner := r.u64(), r.u64(), r.key()
		kind, need = "createAccount", 2
		info = map[string]any{"lamports": lamports, "space": space, "owner": owner}
	case 1:
		owner := r.key()
		kind, need = "assign", 1
		info = map[string]any{"owner": owner}
	case 2:
		lamports := r.u64()
		kind, need = "transfer", 2
		info = map[string]any{"lamports": lamports}
	case 3:
		base, seed, lamports, space, owner := r.key(), r.str(), r.u64(), r.u64(), r.key()
		kind, need = "createAccountWithSeed", 2
		info = map[string]any{"base": base, "seed": seed, "lamports": lamports, "space": space, "owner": owner}
	case 4:
		kind, need = "advanceNonce", 3
	case 5:
		lamports := r.u64()
		kind, need = "withdrawFromNonce", 5
		info = map[string]any{"lamports": lamports}
	case 6:
		authority := r.key()
		kind, need = "initializeNonce", 3
		info = map[string]any{"nonceAuthority": authority}
	case 7:
		authority := r.key()
		kind, need = "authorizeNonce", 2
		info = map[string]any{"newAuthorized": authority}
	case 8:
		space := r.u64()
		kind, need = "allocate", 1
		info = map[string]any{"space": space}
	case 9:
		base, seed, space, owner := r.key(), r.str(), r.u64(), r.key()
		kind, need = "allocateWithSeed", 2
		info = map[string]any{"base": base, "seed": seed, "space": space, "owner": owner}
	case 10:
		base, seed, owner := r.key(), r.str(), r.key()
		kind, need = "assignWithSeed", 2
		info = map[string]any{"base": base, "seed": seed, "owner": owner}
	case 11:
		lamports, seed, owner := r.u64(), r.str(), r.key()
		kind, need = "transferWithSeed", 3
		info = map[string]any{"lamports": lamports, "sourceSeed": seed, "sourceOwner": owner}
	case 12:
		kind, need = "upgradeNonce", 1
	case 13:
		lamports, space, owner := r.u64(), r.u64(), r.key()
		kind, need = "createAccountAllowPrefund", 2
		info = map[string]any{"lamports": lamports, "space": space, "owner": owner}
		if lamports == 0 {
			need = 1
			delete(info, "lamports")
		}
	default:
		return nil, false
	}
	if r.bad || !ix.keysInRange() || len(ix.accounts) < need {
		return nil, false
	}
	if info == nil {
		info = map[string]any{}
	}
	switch kind {
	case "createAccount", "createAccountWithSeed":
		info["source"], info["newAccount"] = ix.key(0), ix.key(1)
	case "assign", "allocate", "allocateWithSeed", "assignWithSeed":
		info["account"] = ix.key(0)
	case "transfer":
		info["source"], info["destination"] = ix.key(0), ix.key(1)
	case "advanceNonce":
		info["nonceAccount"], info["recentBlockhashesSysvar"], info["nonceAuthority"] = ix.key(0), ix.key(1), ix.key(2)
	case "withdrawFromNonce":
		info["nonceAccount"], info["destination"], info["recentBlockhashesSysvar"] = ix.key(0), ix.key(1), ix.key(2)
		info["rentSysvar"], info["nonceAuthority"] = ix.key(3), ix.key(4)
	case "initializeNonce":
		info["nonceAccount"], info["recentBlockhashesSysvar"], info["rentSysvar"] = ix.key(0), ix.key(1), ix.key(2)
	case "authorizeNonce":
		info["nonceAccount"], info["nonceAuthority"] = ix.key(0), ix.key(1)
	case "transferWithSeed":
		info["source"], info["sourceBase"], info["destination"] = ix.key(0), ix.key(1), ix.key(2)
	case "upgradeNonce":
		info["nonceAccount"] = ix.key(0)
	case "createAccountAllowPrefund":
		info["newAccount"] = ix.key(0)
		if need == 2 {
			info["source"] = ix.key(1)
		}
	}
	return enum(kind, info), true
}

// parseAssociatedToken is parse_associated_token, over
// AssociatedTokenAccountInstruction's borsh. The data is one tag byte and
// nothing after it, or no data at all for Create.
func parseAssociatedToken(ix instruction) (any, bool) {
	if !ix.keysInRange() {
		return nil, false
	}
	tag := byte(0)
	if len(ix.data) > 0 {
		if len(ix.data) != 1 || ix.data[0] > 2 {
			return nil, false
		}
		tag = ix.data[0]
	}
	if tag == 2 {
		if len(ix.accounts) < 7 {
			return nil, false
		}
		return enum("recoverNested", map[string]any{
			"nestedSource": ix.key(0), "nestedMint": ix.key(1), "destination": ix.key(2), "nestedOwner": ix.key(3),
			"ownerMint": ix.key(4), "wallet": ix.key(5), "tokenProgram": ix.key(6),
		}), true
	}
	if len(ix.accounts) < 6 {
		return nil, false
	}
	kind := "create"
	if tag == 1 {
		kind = "createIdempotent"
	}
	return enum(kind, map[string]any{
		"source": ix.key(0), "account": ix.key(1), "wallet": ix.key(2), "mint": ix.key(3),
		"systemProgram": ix.key(4), "tokenProgram": ix.key(5),
	}), true
}

// parseBPFLoader is parse_bpf_loader, over LoaderInstruction's bincode.
// It checks only the first account against the key list.
func parseBPFLoader(ix instruction) (any, bool) {
	r := &reader{b: ix.data}
	tag := r.u32()
	var offset uint32
	var bytes []byte
	switch tag {
	case 0:
		offset, bytes = r.u32(), r.bytes()
	case 1:
	default:
		return nil, false
	}
	if r.bad || len(ix.accounts) == 0 || int(ix.accounts[0]) >= len(ix.keys) {
		return nil, false
	}
	if tag == 0 {
		return enum("write", map[string]any{"offset": offset, "bytes": base64.StdEncoding.EncodeToString(bytes), "account": ix.key(0)}), true
	}
	if len(ix.accounts) < 2 {
		return nil, false
	}
	return enum("finalize", map[string]any{"account": ix.key(0)}), true
}

// parseBPFUpgradeableLoader is parse_bpf_upgradeable_loader, over
// UpgradeableLoaderInstruction's bincode.
func parseBPFUpgradeableLoader(ix instruction) (any, bool) {
	r := &reader{b: ix.data}
	tag := r.u32()
	var offset, additional uint32
	var bytes []byte
	var maxDataLen uint64
	switch tag {
	case 0, 3, 4, 5, 7:
	case 1:
		offset, bytes = r.u32(), r.bytes()
	case 2:
		maxDataLen = r.u64()
	case 6:
		additional = r.u32()
	default:
		return nil, false
	}
	if r.bad || !ix.keysInRange() {
		return nil, false
	}
	n := len(ix.accounts)
	optional := func(i int) any {
		if n > i {
			return ix.key(i)
		}
		return nil
	}
	switch tag {
	case 0:
		info := map[string]any{"account": ix.key(0)}
		if n > 1 {
			info["authority"] = ix.key(1)
		}
		return enum("initializeBuffer", info), true
	case 1:
		if n < 2 {
			return nil, false
		}
		return enum("write", map[string]any{"offset": offset, "bytes": base64.StdEncoding.EncodeToString(bytes),
			"account": ix.key(0), "authority": ix.key(1)}), true
	case 2:
		if n < 8 {
			return nil, false
		}
		return enum("deployWithMaxDataLen", map[string]any{"maxDataLen": maxDataLen, "payerAccount": ix.key(0),
			"programDataAccount": ix.key(1), "programAccount": ix.key(2), "bufferAccount": ix.key(3), "rentSysvar": ix.key(4),
			"clockSysvar": ix.key(5), "systemProgram": ix.key(6), "authority": ix.key(7)}), true
	case 3:
		if n < 7 {
			return nil, false
		}
		return enum("upgrade", map[string]any{"programDataAccount": ix.key(0), "programAccount": ix.key(1),
			"bufferAccount": ix.key(2), "spillAccount": ix.key(3), "rentSysvar": ix.key(4), "clockSysvar": ix.key(5),
			"authority": ix.key(6)}), true
	case 4:
		if n < 2 {
			return nil, false
		}
		return enum("setAuthority", map[string]any{"account": ix.key(0), "authority": ix.key(1), "newAuthority": optional(2)}), true
	case 5:
		if n < 3 {
			return nil, false
		}
		return enum("close", map[string]any{"account": ix.key(0), "recipient": ix.key(1), "authority": ix.key(2),
			"programAccount": optional(3)}), true
	case 6:
		if n < 2 {
			return nil, false
		}
		return enum("extendProgram", map[string]any{"additionalBytes": additional, "programDataAccount": ix.key(0),
			"programAccount": ix.key(1), "systemProgram": optional(2), "payerAccount": optional(3)}), true
	default:
		if n < 3 {
			return nil, false
		}
		return enum("setAuthorityChecked", map[string]any{"account": ix.key(0), "authority": ix.key(1), "newAuthority": ix.key(2)}), true
	}
}

// parseAddressLookupTable is parse_address_lookup_table, over
// ProgramInstruction's bincode.
func parseAddressLookupTable(ix instruction) (any, bool) {
	r := &reader{b: ix.data}
	tag := r.u32()
	var recentSlot uint64
	var bump uint8
	var addresses []string
	switch tag {
	case 0:
		recentSlot, bump = r.u64(), r.u8()
	case 1, 3, 4:
	case 2:
		n := r.u64()
		if n > uint64(len(r.b))/32 {
			return nil, false
		}
		addresses = make([]string, 0, n)
		for i := uint64(0); i < n; i++ {
			addresses = append(addresses, r.key())
		}
	default:
		return nil, false
	}
	if r.bad || !ix.keysInRange() {
		return nil, false
	}
	n := len(ix.accounts)
	switch tag {
	case 0:
		if n < 4 {
			return nil, false
		}
		return enum("createLookupTable", map[string]any{"lookupTableAccount": ix.key(0), "lookupTableAuthority": ix.key(1),
			"payerAccount": ix.key(2), "systemProgram": ix.key(3), "recentSlot": recentSlot, "bumpSeed": bump}), true
	case 2:
		if n < 2 {
			return nil, false
		}
		info := map[string]any{"lookupTableAccount": ix.key(0), "lookupTableAuthority": ix.key(1), "newAddresses": addresses}
		if n >= 4 {
			info["payerAccount"], info["systemProgram"] = ix.key(2), ix.key(3)
		}
		return enum("extendLookupTable", info), true
	case 4:
		if n < 3 {
			return nil, false
		}
		return enum("closeLookupTable", map[string]any{"lookupTableAccount": ix.key(0), "lookupTableAuthority": ix.key(1),
			"recipient": ix.key(2)}), true
	default:
		if n < 2 {
			return nil, false
		}
		kind := "freezeLookupTable"
		if tag == 3 {
			kind = "deactivateLookupTable"
		}
		return enum(kind, map[string]any{"lookupTableAccount": ix.key(0), "lookupTableAuthority": ix.key(1)}), true
	}
}
