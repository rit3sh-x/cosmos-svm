package rpc

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"math"
	"os"
	"testing"

	"github.com/mr-tron/base58"
)

// The oracle for the instruction parsers works in three steps:
//  1. TestWriteInstructionFixtures writes testdata/ix_fixtures.json: every
//     instruction of every program the parsers know, and the edges around each.
//  2. tools/tx-oracle renders each case with Agave's own parse_ui_instruction
//     into testdata/ix_golden.json.
//  3. TestTheInstructionParsersAreAnzas compares uiInstruction with that file.
//
// A case marked to parse must also come back parsed from Agave. A fixture with
// wrong ixData cannot pass by failing on both sides.

const (
	ixFixturesPath = "testdata/ix_fixtures.json"
	ixGoldenPath   = "testdata/ix_golden.json"
)

type ixFixture struct {
	Name           string   `json:"name"`
	Keys           []string `json:"keys"`
	ProgramIDIndex uint8    `json:"programIdIndex"`
	Accounts       []int    `json:"accounts"`
	Data           string   `json:"data"`
	StackHeight    *int     `json:"stackHeight"`
	Parses         bool     `json:"parses"`
}

// corpus builds the fixtures.
type corpus struct{ cases []ixFixture }

// ixKey is a fixed key for account position i of a case.
func ixKey(i int) [32]byte {
	var k [32]byte
	binary.LittleEndian.PutUint64(k[:], uint64(1000+i))
	k[31] = 7
	return k
}

// add writes one case: the program at key 0, then n accounts at 1..n.
func (c *corpus) add(name, program string, n int, data []byte, parses bool) {
	keys := []string{program}
	accounts := []int{}
	for i := 1; i <= n; i++ {
		k := ixKey(i)
		keys = append(keys, base58.Encode(k[:]))
		accounts = append(accounts, i)
	}
	height := 2
	c.cases = append(c.cases, ixFixture{name, keys, 0, accounts, base64.StdEncoding.EncodeToString(data), &height, parses})
}

// ixData builds instruction data.
type ixData []byte

func (b ixData) u8(v uint8) ixData   { return append(b, v) }
func (b ixData) u16(v uint16) ixData { return binary.LittleEndian.AppendUint16(b, v) }
func (b ixData) u32(v uint32) ixData { return binary.LittleEndian.AppendUint32(b, v) }
func (b ixData) u64(v uint64) ixData { return binary.LittleEndian.AppendUint64(b, v) }
func (b ixData) i8(v int8) ixData    { return append(b, byte(v)) }
func (b ixData) key(i int) ixData    { k := ixKey(i); return append(b, k[:]...) }
func (b ixData) zero(n int) ixData   { return append(b, make([]byte, n)...) }
func (b ixData) fill(n int, v byte) ixData {
	for i := 0; i < n; i++ {
		b = append(b, v+byte(i))
	}
	return b
}
func (b ixData) str(s string) ixData   { return append(b.u64(uint64(len(s))), s...) }
func (b ixData) borsh(s string) ixData { return append(b.u32(uint32(len(s))), s...) }
func (b ixData) opt(i int) ixData {
	if i < 0 {
		return b.u8(0)
	}
	return b.u8(1).key(i)
}

const (
	systemID  = "11111111111111111111111111111111"
	tokenID   = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	token22ID = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	ataID     = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
	loaderID  = "BPFLoader2111111111111111111111111111111111"
	upgradeID = "BPFLoaderUpgradeab1e11111111111111111111111"
	altID     = "AddressLookupTab1e1111111111111111111111111"
	memoV1ID  = "Memo1UhkJRfHyvLMcVucJwxXeuD728EqVDDwQDxFMNo"
	memoV3ID  = "MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr"
	memoV4ID  = "Memo4c2pN8afCj432Lb7RMVKi9PbQnnW7ewFFaV3oAH"
)

func (c *corpus) system() {
	s := func(tag uint32) ixData { return ixData{}.u32(tag) }
	for _, v := range []struct {
		name string
		n    int
		data ixData
	}{
		{"createAccount", 2, s(0).u64(1_000_000).u64(165).key(9)},
		{"assign", 1, s(1).key(9)},
		{"transfer", 2, s(2).u64(42)},
		{"createAccountWithSeed", 2, s(3).key(8).str("seed one").u64(5).u64(82).key(9)},
		{"advanceNonce", 3, s(4)},
		{"withdrawFromNonce", 5, s(5).u64(77)},
		{"initializeNonce", 3, s(6).key(9)},
		{"authorizeNonce", 2, s(7).key(9)},
		{"allocate", 1, s(8).u64(10)},
		{"allocateWithSeed", 2, s(9).key(8).str("s").u64(10).key(9)},
		{"assignWithSeed", 2, s(10).key(8).str("assign seed").key(9)},
		{"transferWithSeed", 3, s(11).u64(3).str("from").key(9)},
		{"upgradeNonce", 1, s(12)},
		{"createAccountAllowPrefund/funded", 2, s(13).u64(9).u64(100).key(9)},
		{"createAccountAllowPrefund/prefunded", 1, s(13).u64(0).u64(100).key(9)},
	} {
		c.add("system/"+v.name, systemID, v.n, v.data, true)
		c.add("system/"+v.name+"/extra-account", systemID, v.n+1, v.data, true)
		c.add("system/"+v.name+"/short", systemID, v.n-1, v.data, false)
	}
	c.add("system/transfer/trailing-ixData", systemID, 2, s(2).u64(42).u8(1), true)
	c.add("system/transfer/truncated", systemID, 2, s(2).u32(1), false)
	c.add("system/seed-not-utf8", systemID, 2, s(10).key(8).str("\xff").key(9), false)
	c.add("system/unknown-tag", systemID, 2, s(14), false)
	c.add("system/empty", systemID, 2, ixData{}, false)
}

func (c *corpus) memo() {
	for _, id := range []string{memoV1ID, memoV3ID, memoV4ID} {
		c.add("memo/"+id+"/text", id, 1, ixData("hello memo"), true)
		c.add("memo/"+id+"/emoji", id, 0, ixData("\xf0\x9f\xa6\x96"), true)
		c.add("memo/"+id+"/empty", id, 0, ixData{}, true)
		c.add("memo/"+id+"/not-utf8", id, 1, ixData{0xff, 0xfe}, false)
	}
}

func (c *corpus) ata() {
	c.add("ata/create/no-data", ataID, 6, ixData{}, true)
	c.add("ata/create", ataID, 6, ixData{0}, true)
	c.add("ata/createIdempotent", ataID, 6, ixData{1}, true)
	c.add("ata/recoverNested", ataID, 7, ixData{2}, true)
	c.add("ata/create/short", ataID, 5, ixData{0}, false)
	c.add("ata/recoverNested/short", ataID, 6, ixData{2}, false)
	c.add("ata/unknown-tag", ataID, 6, ixData{3}, false)
	c.add("ata/trailing-ixData", ataID, 6, ixData{0, 0}, false)
}

func (c *corpus) loaders() {
	c.add("loader/write", loaderID, 1, ixData{}.u32(0).u32(16).str("\x01\x02\x03"), true)
	c.add("loader/finalize", loaderID, 2, ixData{}.u32(1), true)
	c.add("loader/finalize/short", loaderID, 1, ixData{}.u32(1), false)
	c.add("loader/unknown", loaderID, 2, ixData{}.u32(2), false)
	u := func(tag uint32) ixData { return ixData{}.u32(tag) }
	for _, v := range []struct {
		name string
		n    int
		data ixData
	}{
		{"initializeBuffer", 1, u(0)},
		{"initializeBuffer/authority", 2, u(0)},
		{"write", 2, u(1).u32(8).str("program ixData")},
		{"deployWithMaxDataLen", 8, u(2).u64(1 << 20)},
		{"upgrade", 7, u(3)},
		{"setAuthority", 2, u(4)},
		{"setAuthority/new", 3, u(4)},
		{"close", 3, u(5)},
		{"close/program", 4, u(5)},
		{"extendProgram", 2, u(6).u32(1024)},
		{"extendProgram/system", 3, u(6).u32(1024)},
		{"extendProgram/payer", 4, u(6).u32(1024)},
		{"setAuthorityChecked", 3, u(7)},
	} {
		c.add("upgradeable/"+v.name, upgradeID, v.n, v.data, true)
	}
	c.add("upgradeable/write/short", upgradeID, 1, u(1).u32(8).str("x"), false)
	c.add("upgradeable/deploy/short", upgradeID, 7, u(2).u64(1), false)
	c.add("upgradeable/unknown", upgradeID, 3, u(8), false)
	c.add("upgradeable/write/truncated", upgradeID, 2, u(1).u32(8).u64(99), false)
}

func (c *corpus) alt() {
	a := func(tag uint32) ixData { return ixData{}.u32(tag) }
	c.add("alt/createLookupTable", altID, 4, a(0).u64(99).u8(254), true)
	c.add("alt/freezeLookupTable", altID, 2, a(1), true)
	c.add("alt/extendLookupTable", altID, 2, a(2).u64(2).key(8).key(9), true)
	c.add("alt/extendLookupTable/payer", altID, 4, a(2).u64(1).key(9), true)
	c.add("alt/deactivateLookupTable", altID, 2, a(3), true)
	c.add("alt/closeLookupTable", altID, 3, a(4), true)
	c.add("alt/create/short", altID, 3, a(0).u64(1).u8(1), false)
	c.add("alt/extend/count-past-end", altID, 2, a(2).u64(5).key(9), false)
	c.add("alt/unknown", altID, 2, a(5), false)
}

// token adds every TokenInstruction for both token programs.
func (c *corpus) token(program, tag string) {
	t := func(v uint8) ixData { return ixData{v} }
	add := func(name string, n int, data ixData, parses bool) {
		c.add(tag+"/"+name, program, n, data, parses)
	}
	// signed adds three cases for an instruction with an authority: exactly
	// the accounts, a multisig with two signers after the authority, and one
	// account too few.
	signed := func(name string, n int, data ixData) {
		add(name, n, data, true)
		add(name+"/multisig", n+2, data, true)
		add(name+"/short", n-1, data, false)
	}
	add("initializeMint", 2, t(0).u8(6).key(8).opt(9), true)
	add("initializeMint/no-freeze", 2, t(0).u8(6).key(8).opt(-1), true)
	add("initializeMint/short", 1, t(0).u8(6).key(8).opt(-1), false)
	add("initializeMint/truncated", 2, t(0).u8(6).zero(10), false)
	add("initializeMint/bad-option", 2, t(0).u8(6).key(8).u8(2), false)
	add("initializeAccount", 4, t(1), true)
	add("initializeMultisig", 5, t(2).u8(2), true)
	add("initializeMultisig/short", 2, t(2).u8(2), false)
	signed("transfer", 3, t(3).u64(1234))
	signed("approve", 3, t(4).u64(55))
	signed("revoke", 2, t(5))
	for a := 0; a < 18; a++ {
		add("setAuthority/"+authorityTypes[a].name, 2, t(6).u8(uint8(a)).opt(9), true)
	}
	add("setAuthority/none", 3, t(6).u8(0).opt(-1), true)
	add("setAuthority/unknown-type", 2, t(6).u8(18).opt(9), false)
	signed("mintTo", 3, t(7).u64(1_000_000))
	signed("burn", 3, t(8).u64(1))
	signed("closeAccount", 3, t(9))
	signed("freezeAccount", 3, t(10))
	signed("thawAccount", 3, t(11))
	signed("transferChecked", 4, t(12).u64(1_500_000).u8(6))
	signed("approveChecked", 4, t(13).u64(1).u8(0))
	signed("mintToChecked", 3, t(14).u64(123_456_789).u8(9))
	signed("burnChecked", 3, t(15).u64(10).u8(2))
	add("transferChecked/twenty-decimals", 4, t(12).u64(5).u8(20), true)
	add("initializeAccount2", 3, t(16).key(9), true)
	add("syncNative", 1, t(17), true)
	add("initializeAccount3", 2, t(18).key(9), true)
	add("initializeMultisig2", 3, t(19).u8(1), true)
	add("initializeMint2", 1, t(20).u8(0).key(8).opt(9), true)
	add("getAccountDataSize", 1, t(21), true)
	add("getAccountDataSize/types", 1, t(21).u16(1).u16(7).u16(28), true)
	add("getAccountDataSize/unknown-type", 1, t(21).u16(29), false)
	add("getAccountDataSize/odd-length", 1, t(21).u16(1).u8(1), false)
	add("initializeImmutableOwner", 1, t(22), true)
	add("amountToUiAmount", 1, t(23).u64(9_999), true)
	add("uiAmountToAmount", 1, append(t(24), "1.25"...), true)
	add("uiAmountToAmount/not-utf8", 1, t(24).u8(0xff), false)
	add("initializeMintCloseAuthority", 1, t(25).opt(9), true)
	add("initializeMintCloseAuthority/none", 1, t(25).opt(-1), true)
	add("createNativeMint", 3, t(31), true)
	add("initializeNonTransferableMint", 1, t(32), true)
	add("initializePermanentDelegate", 1, t(35).key(9), true)
	signed("withdrawExcessLamports", 3, t(38))
	signed("unwrapLamports", 3, t(45).u8(1).u64(500))
	add("unwrapLamports/all", 3, t(45).u8(0), true)
	add("reallocate", 4, t(29).u16(8).u16(11), true)
	add("reallocate/multisig", 6, t(29).u16(8), true)
	add("reallocate/short", 3, t(29).u16(8), false)
	add("unknown-tag", 2, t(47), false)
	add("empty", 2, ixData{}, false)
	// The batch holds a transfer over accounts 1-3 and a sync over account 4.
	inner := ixData{}.u8(3).u8(9).u8(3).u64(77).u8(1).u8(1).u8(17)
	add("batch", 4, append(t(255), inner...), true)
	add("batch/empty", 1, t(255), true)
	add("batch/nested", 4, append(t(255), ixData{}.u8(1).u8(1).u8(255)...), false)
	add("batch/inner-short", 2, append(t(255), ixData{}.u8(1).u8(9).u8(3).u64(77)...), false)
	add("batch/truncated", 4, append(t(255), ixData{}.u8(3).u8(20).u8(3)...), false)
	c.tokenExtensions(program, tag)
}

func (c *corpus) tokenExtensions(program, tag string) {
	add := func(name string, n int, data ixData, parses bool) {
		c.add(tag+"/"+name, program, n, data, parses)
	}
	signed := func(name string, n int, data ixData) {
		add(name, n, data, true)
		add(name+"/multisig", n+2, data, true)
		add(name+"/short", n-1, data, false)
	}
	e := func(main, sub uint8) ixData { return ixData{main, sub} }

	// Transfer fee (26).
	add("initializeTransferFeeConfig", 1, e(26, 0).opt(8).opt(9).u16(50).u64(5000), true)
	add("initializeTransferFeeConfig/no-authorities", 1, e(26, 0).opt(-1).opt(-1).u16(0).u64(0), true)
	signed("transferCheckedWithFee", 4, e(26, 1).u64(1000).u8(2).u64(5))
	signed("withdrawWithheldTokensFromMint", 3, e(26, 2))
	add("withdrawWithheldTokensFromAccounts", 5, e(26, 3).u8(2), true)
	add("withdrawWithheldTokensFromAccounts/multisig", 7, e(26, 3).u8(2), true)
	add("withdrawWithheldTokensFromAccounts/short", 4, e(26, 3).u8(2), false)
	add("harvestWithheldTokensToMint", 3, e(26, 4), true)
	signed("setTransferFee", 2, e(26, 5).u16(10).u64(99))
	add("transferFee/unknown", 2, e(26, 6), false)
	add("transferFee/empty", 2, ixData{26}, false)

	// Confidential transfer (27).
	ct := func(sub uint8) ixData { return e(27, sub) }
	add("ct/initializeMint", 1, ct(0).key(8).u8(1).fill(32, 1), true)
	add("ct/initializeMint/null", 1, ct(0).zero(32).u8(0).zero(32), true)
	add("ct/initializeMint/wrong-size", 1, ct(0).zero(64), false)
	add("ct/updateMint", 2, ct(1).u8(1).fill(32, 3), true)
	add("ct/updateMint/no-auditor", 2, ct(1).u8(0).zero(32), true)
	for _, off := range []int8{0, 1} {
		suffix := map[int8]string{0: "/context", 1: "/sysvar"}[off]
		signed("ct/configureAccount"+suffix, 4, ct(2).fill(36, 5).u64(65536).i8(off))
		signed("ct/emptyAccount"+suffix, 3, ct(4).i8(off))
	}
	add("ct/approveAccount", 3, ct(3), true)
	signed("ct/deposit", 3, ct(5).u64(1000).u8(6))
	withdraw := func(eq, rng int8) ixData { return ct(6).u64(7).u8(2).fill(36, 9).i8(eq).i8(rng) }
	transfer := func(eq, v, rng int8) ixData { return ct(7).fill(36, 11).fill(128, 1).i8(eq).i8(v).i8(rng) }
	withFee := func(o [5]int8) ixData {
		return ct(13).fill(36, 13).fill(128, 2).i8(o[0]).i8(o[1]).i8(o[2]).i8(o[3]).i8(o[4])
	}
	for _, n := range []int{4, 5, 6, 7, 9} {
		name := func(s string) string { return s + "/accounts-" + string(rune('0'+n)) }
		add(name("ct/withdraw/contexts"), n, withdraw(0, 0), true)
		add(name("ct/withdraw/sysvar"), n, withdraw(1, 2), true)
		add(name("ct/withdraw/mixed"), n, withdraw(0, 3), true)
		add(name("ct/transfer/contexts"), n, transfer(0, 0, 0), true)
		add(name("ct/transfer/sysvar"), n, transfer(1, 2, 3), true)
		add(name("ct/transfer/mixed"), n, transfer(1, 0, 0), true)
		add(name("ct/transferWithFee/contexts"), n, withFee([5]int8{}), true)
		add(name("ct/transferWithFee/sysvar"), n, withFee([5]int8{1, 2, 3, 4, 5}), true)
		add(name("ct/transferWithFee/mixed"), n, withFee([5]int8{0, 1, 0, 2, 0}), true)
	}
	add("ct/withdraw/short", 3, withdraw(0, 0), false)
	signed("ct/applyPendingBalance", 2, ct(8).u64(3).fill(36, 17))
	for sub := uint8(9); sub <= 12; sub++ {
		signed("ct/credits-"+string(rune('0'+sub-9)), 2, ct(sub))
	}
	add("ct/configureAccountWithRegistry", 3, ct(14), true)
	add("ct/unknown", 3, ct(15), false)
	add("ct/empty", 3, ixData{27}, false)

	// Default account state (28).
	add("initializeDefaultAccountState", 1, e(28, 0).u8(2), true)
	signed("updateDefaultAccountState", 2, e(28, 1).u8(1))
	add("defaultAccountState/bad-state", 1, e(28, 0).u8(3), false)
	add("defaultAccountState/too-short", 1, e(28, 0), false)
	add("defaultAccountState/too-long", 1, e(28, 0).u8(1).u8(1), false)

	// Memo transfer (30) and CPI guard (34).
	signed("enableRequiredMemoTransfers", 2, e(30, 0))
	signed("disableRequiredMemoTransfers", 2, e(30, 1))
	add("memoTransfer/unknown", 2, e(30, 2), false)
	add("memoTransfer/empty", 2, ixData{30}, false)
	signed("enableCpiGuard", 2, e(34, 0))
	signed("disableCpiGuard", 2, e(34, 1))

	// Interest bearing (33).
	add("initializeInterestBearingConfig", 1, e(33, 0).key(9).u16(uint16(0xffff-49)), true)
	add("initializeInterestBearingConfig/no-authority", 1, e(33, 0).zero(32).u16(300), true)
	signed("updateInterestBearingConfigRate", 2, e(33, 1).u16(25))
	add("interestBearing/wrong-size", 1, e(33, 0).zero(33), false)

	// Pointers (36, 39, 40, 41).
	for _, p := range []struct {
		main uint8
		name string
	}{{36, "transferHook"}, {39, "metadataPointer"}, {40, "groupPointer"}, {41, "groupMemberPointer"}} {
		add(p.name+"/initialize", 1, e(p.main, 0).key(8).key(9), true)
		add(p.name+"/initialize/none", 1, e(p.main, 0).zero(64), true)
		signed(p.name+"/update", 2, e(p.main, 1).key(9))
		add(p.name+"/update/none", 2, e(p.main, 1).zero(32), true)
		add(p.name+"/unknown", 2, e(p.main, 2).zero(32), false)
		add(p.name+"/wrong-size", 1, e(p.main, 0).zero(32), false)
	}

	// Confidential transfer fee (37).
	add("ctFee/initialize", 1, e(37, 0).key(8).fill(32, 4), true)
	add("ctFee/initialize/zeros", 1, e(37, 0).zero(64), true)
	for _, n := range []int{3, 4, 5, 7} {
		add("ctFee/withdrawFromMint/context/accounts-"+string(rune('0'+n)), n, e(37, 1).i8(0).fill(36, 6), n >= 4)
		add("ctFee/withdrawFromMint/sysvar/accounts-"+string(rune('0'+n)), n, e(37, 1).i8(1).fill(36, 6), n >= 4)
	}
	add("ctFee/withdrawFromAccounts", 6, e(37, 2).u8(2).i8(0).fill(36, 7), true)
	add("ctFee/withdrawFromAccounts/sysvar-multisig", 8, e(37, 2).u8(2).i8(1).fill(36, 7), true)
	add("ctFee/withdrawFromAccounts/short", 5, e(37, 2).u8(2).i8(0).fill(36, 7), false)
	add("ctFee/harvest", 3, e(37, 3), true)
	signed("ctFee/enableHarvest", 2, e(37, 4))
	signed("ctFee/disableHarvest", 2, e(37, 5))
	add("ctFee/unknown", 2, e(37, 6), false)

	// Confidential mint and burn (42).
	cmb := func(sub uint8) ixData { return e(42, sub) }
	add("cmb/initializeMint", 1, cmb(0).fill(32, 1).fill(36, 2), true)
	for _, n := range []int{2, 3, 4} {
		add("cmb/rotate/context/accounts-"+string(rune('0'+n)), n, cmb(1).fill(32, 3).i8(0), n >= 3)
		add("cmb/rotate/sysvar/accounts-"+string(rune('0'+n)), n, cmb(1).fill(32, 3).i8(1), n >= 3)
	}
	signed("cmb/updateDecryptableSupply", 2, cmb(2).fill(36, 4))
	mintBurn := func(sub uint8, eq, v, rng int8) ixData {
		return cmb(sub).fill(36, 5).fill(128, 6).i8(eq).i8(v).i8(rng)
	}
	for _, n := range []int{3, 4, 5, 6, 8} {
		suffix := "/accounts-" + string(rune('0'+n))
		add("cmb/mint/contexts"+suffix, n, mintBurn(3, 0, 0, 0), true)
		add("cmb/mint/sysvar"+suffix, n, mintBurn(3, 1, 1, 1), true)
		add("cmb/burn/contexts"+suffix, n, mintBurn(4, 0, 0, 0), true)
		add("cmb/burn/mixed"+suffix, n, mintBurn(4, 0, 2, 0), true)
	}
	signed("cmb/applyPendingBurn", 2, cmb(5))
	add("cmb/unknown", 2, cmb(6), false)
	add("cmb/empty", 2, ixData{42}, false)

	// Scaled UI amount (43).
	add("scaledUiAmount/initialize", 1, e(43, 0).key(9).u64(math.Float64bits(1.5)), true)
	add("scaledUiAmount/initialize/whole", 1, e(43, 0).zero(32).u64(math.Float64bits(2)), true)
	add("scaledUiAmount/initialize/tiny", 1, e(43, 0).zero(32).u64(math.Float64bits(1e-7)), true)
	signed("scaledUiAmount/updateMultiplier", 2, e(43, 1).u64(math.Float64bits(0.25)).u64(1_700_000_000))
	add("scaledUiAmount/unknown", 2, e(43, 2), false)

	// Pausable (44).
	add("pausable/initialize", 1, e(44, 0).key(9), true)
	add("pausable/initialize/zero", 1, e(44, 0).zero(32), true)
	signed("pausable/pause", 2, e(44, 1))
	signed("pausable/resume", 2, e(44, 2))
	add("pausable/unknown", 2, e(44, 3), false)

	// Permissioned burn (46).
	add("permissionedBurn/initialize", 1, e(46, 0).key(9), true)
	signed("permissionedBurn/burn", 4, e(46, 1).u64(88))
	signed("permissionedBurn/burnChecked", 4, e(46, 2).u64(88).u8(3))
	for _, n := range []int{4, 5, 6, 7, 9} {
		suffix := "/accounts-" + string(rune('0'+n))
		burn := func(eq, v, rng int8) ixData { return e(46, 3).fill(36, 8).fill(128, 9).i8(eq).i8(v).i8(rng) }
		add("permissionedBurn/confidential/contexts"+suffix, n, burn(0, 0, 0), true)
		add("permissionedBurn/confidential/sysvar"+suffix, n, burn(1, 2, 3), true)
		add("permissionedBurn/confidential/mixed"+suffix, n, burn(0, 1, 0), true)
	}
	add("permissionedBurn/unknown", 4, e(46, 4), false)

	// Token group and token metadata, which unpack only when TokenInstruction
	// does not.
	d := func(input string) ixData { return ixData(splDiscriminator(input)) }
	add("group/initialize", 3, d("spl_token_group_interface:initialize_token_group").key(9).u64(10), true)
	add("group/initialize/no-authority", 3, d("spl_token_group_interface:initialize_token_group").zero(32).u64(0), true)
	add("group/initialize/short", 2, d("spl_token_group_interface:initialize_token_group").key(9).u64(10), false)
	add("group/initialize/wrong-size", 3, d("spl_token_group_interface:initialize_token_group").key(9), false)
	add("group/updateMaxSize", 2, d("spl_token_group_interface:update_group_max_size").u64(500), true)
	add("group/updateAuthority", 2, d("spl_token_group_interface:update_authority").key(9), true)
	add("group/updateAuthority/none", 2, d("spl_token_group_interface:update_authority").zero(32), true)
	add("group/initializeMember", 5, d("spl_token_group_interface:initialize_member"), true)
	add("group/initializeMember/trailing", 5, d("spl_token_group_interface:initialize_member").u8(0), false)
	add("metadata/initialize", 4, d("spl_token_metadata_interface:initialize_account").borsh("Name").borsh("SYM").borsh("https://example.com/m.json"), true)
	add("metadata/initialize/short", 3, d("spl_token_metadata_interface:initialize_account").borsh("N").borsh("S").borsh("U"), false)
	add("metadata/initialize/trailing", 4, d("spl_token_metadata_interface:initialize_account").borsh("N").borsh("S").borsh("U").u8(1), false)
	add("metadata/updateField/name", 2, d("spl_token_metadata_interface:updating_field").u8(0).borsh("New"), true)
	add("metadata/updateField/symbol", 2, d("spl_token_metadata_interface:updating_field").u8(1).borsh("NS"), true)
	add("metadata/updateField/uri", 2, d("spl_token_metadata_interface:updating_field").u8(2).borsh("ipfs://x"), true)
	add("metadata/updateField/key", 2, d("spl_token_metadata_interface:updating_field").u8(3).borsh("color").borsh("blue"), true)
	add("metadata/updateField/bad-field", 2, d("spl_token_metadata_interface:updating_field").u8(4).borsh("x"), false)
	add("metadata/removeKey", 2, d("spl_token_metadata_interface:remove_key_ix").u8(1).borsh("color"), true)
	add("metadata/removeKey/bad-bool", 2, d("spl_token_metadata_interface:remove_key_ix").u8(2).borsh("color"), false)
	add("metadata/updateAuthority", 2, d("spl_token_metadata_interface:update_the_authority").key(9), true)
	add("metadata/updateAuthority/none", 2, d("spl_token_metadata_interface:update_the_authority").zero(32), true)
	add("metadata/emit", 1, d("spl_token_metadata_interface:emitter").u8(1).u64(3).u8(1).u64(9), true)
	add("metadata/emit/open", 1, d("spl_token_metadata_interface:emitter").u8(0).u8(0), true)
	add("metadata/emit/bad-option", 1, d("spl_token_metadata_interface:emitter").u8(2).u8(0), false)
	add("metadata/unknown-discriminator", 2, d("spl_token_metadata_interface:nothing"), false)
}

func buildInstructionCorpus() []ixFixture {
	c := &corpus{}
	c.system()
	c.memo()
	c.ata()
	c.loaders()
	c.alt()
	c.token(tokenID, "token")
	c.token(token22ID, "token2022")
	unknown := ixKey(999)
	c.add("unknown-program", base58.Encode(unknown[:]), 2, ixData{1, 2, 3}, false)
	// Stake and Vote render partially decoded here, but Agave parses them.
	// That is the documented difference, so neither is in the corpus. The
	// corpus also has no account index past the key list. The runtime never
	// lets one through, and Agave's renderer panics on it.
	return c.cases
}

// TestTheInstructionParsersAreAnzas renders every fixture with uiInstruction
// and compares it with Agave's parse_ui_instruction.
func TestTheInstructionParsersAreAnzas(t *testing.T) {
	var fixtures []ixFixture
	var golden []struct {
		Name     string         `json:"name"`
		Rendered jsontext.Value `json:"rendered"`
	}
	for path, v := range map[string]any{ixFixturesPath: &fixtures, ixGoldenPath: &golden} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, v); err != nil {
			t.Fatal(err)
		}
	}
	if len(fixtures) != len(golden) {
		t.Fatalf("%d fixtures and %d golden renderings: write the golden file again", len(fixtures), len(golden))
	}
	if built := buildInstructionCorpus(); len(built) != len(fixtures) {
		t.Fatalf("the corpus builds %d cases and the fixture file holds %d: write the fixtures again", len(built), len(fixtures))
	}
	for i, f := range fixtures {
		g := golden[i]
		if g.Name != f.Name {
			t.Fatalf("golden %d is %s, fixture %s", i, g.Name, f.Name)
		}
		var anza map[string]any
		if err := json.Unmarshal(g.Rendered, &anza); err != nil {
			t.Fatal(err)
		}
		if _, parsed := anza["parsed"]; parsed != f.Parses {
			t.Errorf("%s: Agave parsed it %v, the fixture says %v", f.Name, parsed, f.Parses)
		}
		keys := make([][32]byte, len(f.Keys))
		for k, s := range f.Keys {
			b, err := base58.Decode(s)
			if err != nil || len(b) != 32 {
				t.Fatalf("%s: key %s", f.Name, s)
			}
			keys[k] = [32]byte(b)
		}
		accounts := make([]byte, len(f.Accounts))
		for k, a := range f.Accounts {
			accounts[k] = byte(a)
		}
		data, _ := base64.StdEncoding.DecodeString(f.Data)
		var height any
		if f.StackHeight != nil {
			height = *f.StackHeight
		}
		mine, err := json.Marshal(uiInstruction(f.ProgramIDIndex, accounts, data, keys, height), json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := canonical(t, mine), canonical(t, g.Rendered); got != want {
			t.Errorf("%s:\n got %s\nwant %s", f.Name, got, want)
		}
	}
}
