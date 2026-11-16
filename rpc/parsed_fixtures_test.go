package rpc

import (
	"encoding/base64"
	"encoding/binary"
	"math"

	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// This file builds the fixture set that the oracle renders. It builds each
// account field by field, not as a pasted blob, so each offset of a layout
// shows in the code.

func k(n byte) [32]byte {
	var out [32]byte
	for i := range out {
		out[i] = n
	}
	return out
}

func b58(k [32]byte) string { return base58.Encode(k[:]) }

func kb(n byte) []byte {
	key := k(n)
	return key[:]
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func putCOption(b []byte, at int, key *[32]byte) {
	if key == nil {
		return
	}
	binary.LittleEndian.PutUint32(b[at:at+4], 1)
	copy(b[at+4:at+36], key[:])
}

func tlv(kind uint16, value []byte) []byte {
	out := make([]byte, 4+len(value))
	binary.LittleEndian.PutUint16(out[0:2], kind)
	binary.LittleEndian.PutUint16(out[2:4], uint16(len(value)))
	copy(out[4:], value)
	return out
}

func buildMint(authority, freeze *[32]byte, supply uint64, decimals uint8, ext []byte) []byte {
	n := 82
	if ext != nil {
		n = 166 + len(ext)
	}
	b := make([]byte, n)
	putCOption(b, 0, authority)
	binary.LittleEndian.PutUint64(b[36:44], supply)
	b[44] = decimals
	b[45] = 1
	putCOption(b, 46, freeze)
	if ext != nil {
		b[165] = 1
		copy(b[166:], ext)
	}
	return b
}

func buildTokenAccount(mint, owner [32]byte, amount uint64, delegate *[32]byte, delegated uint64,
	state byte, native *uint64, closeAuthority *[32]byte) []byte {
	b := make([]byte, 165)
	copy(b[0:32], mint[:])
	copy(b[32:64], owner[:])
	binary.LittleEndian.PutUint64(b[64:72], amount)
	putCOption(b, 72, delegate)
	b[108] = state
	if native != nil {
		binary.LittleEndian.PutUint32(b[109:113], 1)
		binary.LittleEndian.PutUint64(b[113:121], *native)
	}
	binary.LittleEndian.PutUint64(b[121:129], delegated)
	putCOption(b, 129, closeAuthority)
	return b
}

func buildMultisig(m, n uint8, signers ...[32]byte) []byte {
	b := make([]byte, 355)
	b[0], b[1], b[2] = m, n, 1
	for i, s := range signers {
		copy(b[3+i*32:35+i*32], s[:])
	}
	return b
}

func extTransferFee(authority, withdraw *[32]byte, withheld uint64,
	olderEpoch, olderMax uint64, olderBps uint16, newerEpoch, newerMax uint64, newerBps uint16) []byte {
	v := make([]byte, 108)
	if authority != nil {
		copy(v[0:32], authority[:])
	}
	if withdraw != nil {
		copy(v[32:64], withdraw[:])
	}
	binary.LittleEndian.PutUint64(v[64:72], withheld)
	binary.LittleEndian.PutUint64(v[72:80], olderEpoch)
	binary.LittleEndian.PutUint64(v[80:88], olderMax)
	binary.LittleEndian.PutUint16(v[88:90], olderBps)
	binary.LittleEndian.PutUint64(v[90:98], newerEpoch)
	binary.LittleEndian.PutUint64(v[98:106], newerMax)
	binary.LittleEndian.PutUint16(v[106:108], newerBps)
	return tlv(extTransferFeeConfig, v)
}

func extInterest(rateAuthority *[32]byte, initTS int64, preRate int16, lastTS int64, rate int16) []byte {
	v := make([]byte, 52)
	if rateAuthority != nil {
		copy(v[0:32], rateAuthority[:])
	}
	binary.LittleEndian.PutUint64(v[32:40], uint64(initTS))
	binary.LittleEndian.PutUint16(v[40:42], uint16(preRate))
	binary.LittleEndian.PutUint64(v[42:50], uint64(lastTS))
	binary.LittleEndian.PutUint16(v[50:52], uint16(rate))
	return tlv(extInterestBearingConfig, v)
}

func extScaled(authority *[32]byte, multiplier float64, effective int64, newMultiplier float64) []byte {
	v := make([]byte, 56)
	if authority != nil {
		copy(v[0:32], authority[:])
	}
	binary.LittleEndian.PutUint64(v[32:40], math.Float64bits(multiplier))
	binary.LittleEndian.PutUint64(v[40:48], uint64(effective))
	binary.LittleEndian.PutUint64(v[48:56], math.Float64bits(newMultiplier))
	return tlv(extScaledUIAmount, v)
}

func extPointer(kind uint16, authority, address *[32]byte) []byte {
	v := make([]byte, 64)
	if authority != nil {
		copy(v[0:32], authority[:])
	}
	if address != nil {
		copy(v[32:64], address[:])
	}
	return tlv(kind, v)
}

func extGroup(updateAuthority *[32]byte, mint [32]byte, size, max uint64) []byte {
	v := make([]byte, 80)
	if updateAuthority != nil {
		copy(v[0:32], updateAuthority[:])
	}
	copy(v[32:64], mint[:])
	binary.LittleEndian.PutUint64(v[64:72], size)
	binary.LittleEndian.PutUint64(v[72:80], max)
	return tlv(extTokenGroup, v)
}

func extGroupMember(mint, group [32]byte, number uint64) []byte {
	v := make([]byte, 72)
	copy(v[0:32], mint[:])
	copy(v[32:64], group[:])
	binary.LittleEndian.PutUint64(v[64:72], number)
	return tlv(extTokenGroupMember, v)
}

func borshStr(s string) []byte {
	out := make([]byte, 4+len(s))
	binary.LittleEndian.PutUint32(out[0:4], uint32(len(s)))
	copy(out[4:], s)
	return out
}

func extMetadata(updateAuthority *[32]byte, mint [32]byte, name, symbol, uri string, extra [][2]string) []byte {
	v := make([]byte, 64)
	if updateAuthority != nil {
		copy(v[0:32], updateAuthority[:])
	}
	copy(v[32:64], mint[:])
	v = append(v, borshStr(name)...)
	v = append(v, borshStr(symbol)...)
	v = append(v, borshStr(uri)...)
	count := make([]byte, 4)
	binary.LittleEndian.PutUint32(count, uint32(len(extra)))
	v = append(v, count...)
	for _, kv := range extra {
		v = append(v, borshStr(kv[0])...)
		v = append(v, borshStr(kv[1])...)
	}
	return tlv(extTokenMetadata, v)
}

// tlvMintCloseAuthority is a Token-2022 MintCloseAuthority extension.
func tlvMintCloseAuthority(authority *[32]byte) []byte {
	return tlv(extMintCloseAuthority, optionalKey(authority))
}

func optionalKey(k *[32]byte) []byte {
	v := make([]byte, 32)
	if k != nil {
		copy(v, k[:])
	}
	return v
}

// pattern is n bytes counting up from start. It stands in for a ciphertext
// or an ElGamal key, because the renderers only move bytes.
func pattern(n int, start byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

// extendAccount is a Token-2022 account with extensions: the base, the
// Account type byte, then the TLVs.
func extendAccount(base, ext []byte) []byte {
	return concat(base, []byte{2}, ext)
}

// confidentialAccount is ConfidentialTransferAccount's 295 bytes.
func confidentialAccount() []byte {
	counters := make([]byte, 32)
	for i := range 4 {
		binary.LittleEndian.PutUint64(counters[8*i:], uint64(i+1)*10)
	}
	return tlv(extConfidentialTransferAccount, concat([]byte{1}, pattern(32, 1), pattern(64, 40), pattern(64, 110),
		pattern(64, 180), pattern(36, 7), []byte{1, 0}, counters))
}

func buildNonce(authority, blockhash [32]byte, lamportsPerSignature uint64) []byte {
	b := make([]byte, 80)
	binary.LittleEndian.PutUint32(b[0:4], 1) // Versions::Current
	binary.LittleEndian.PutUint32(b[4:8], 1) // State::Initialized
	copy(b[8:40], authority[:])
	copy(b[40:72], blockhash[:])
	binary.LittleEndian.PutUint64(b[72:80], lamportsPerSignature)
	return b
}

func buildLookupTable(deactivation, lastExtended uint64, startIndex uint8, authority *[32]byte, addresses ...[32]byte) []byte {
	b := make([]byte, 56+32*len(addresses))
	binary.LittleEndian.PutUint32(b[0:4], 1)
	binary.LittleEndian.PutUint64(b[4:12], deactivation)
	binary.LittleEndian.PutUint64(b[12:20], lastExtended)
	b[20] = startIndex
	if authority != nil {
		b[21] = 1
		copy(b[22:54], authority[:])
	}
	for i, a := range addresses {
		copy(b[56+i*32:88+i*32], a[:])
	}
	return b
}

func buildLoader(tag uint32, slot uint64, key *[32]byte, trailing []byte) []byte {
	var b []byte
	switch tag {
	case 0:
		b = make([]byte, 4)
	case 1:
		b = make([]byte, 4)
		b = appendOption(b, key)
	case 2:
		b = make([]byte, 36)
		copy(b[4:36], key[:])
	case 3:
		b = make([]byte, 12)
		binary.LittleEndian.PutUint64(b[4:12], slot)
		b = appendOption(b, key)
	}
	binary.LittleEndian.PutUint32(b[0:4], tag)
	return append(b, trailing...)
}

func appendOption(b []byte, key *[32]byte) []byte {
	if key == nil {
		return append(b, 0)
	}
	return append(append(b, 1), key[:]...)
}

func u64s(values ...uint64) []byte {
	b := make([]byte, 8*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint64(b[i*8:], v)
	}
	return b
}

func fixtures() []fixture {
	var (
		owner      = k(3)
		delegate   = k(4)
		closeAuth  = k(5)
		authority  = k(6)
		mintKey    = k(2)
		groupKey   = k(7)
		account    = k(9)
		reserve    = uint64(2_039_280)
		token      = b58(svm.SplToken)
		token2022  = b58(svm.SplToken2022)
		system     = b58(svm.SystemProgram)
		loader     = b58(svm.LoaderV3)
		sysvarOwnr = b58(svm.SysvarOwner)
		alt        = b58(addressLookupTable)
	)

	plainMint := buildMint(&authority, &authority, 1_000_000, 6, nil)
	interestMint := buildMint(&authority, nil, 1_000_000, 4,
		extInterest(&authority, 0, 100, 31_556_736, 200))
	scaledMint := buildMint(&authority, nil, 1_000_000, 2,
		extScaled(&authority, 1.5, 1_000, 2.0))
	// A multiplier is arbitrary account bytes, so a mint can carry one that is
	// not a number. The two runtimes spell those differently by default.
	brokenScaledMint := buildMint(&authority, nil, 1, 2,
		extScaled(nil, math.Inf(1), 0, math.NaN()))

	metadataMint := buildMint(&authority, nil, 7, 0, concat(
		extPointer(extMetadataPointer, &authority, &mintKey),
		extMetadata(&authority, mintKey, "Example", "EXM", "https://example.invalid/exm.json",
			[][2]string{{"colour", "blue"}}),
	))
	groupMint := buildMint(&authority, nil, 0, 0, concat(
		extPointer(extGroupPointer, &authority, &groupKey),
		extGroup(&authority, mintKey, 1, 10),
		extPointer(extGroupMemberPointer, &authority, &groupKey),
		extGroupMember(mintKey, groupKey, 1),
	))
	feeMint := buildMint(&authority, nil, 5, 3,
		extTransferFee(&authority, &delegate, 42, 1, 1_000, 50, 2, 2_000, 75))
	tenthMint := buildMint(&authority, nil, 5, 0, tlvMintCloseAuthority(&closeAuth))
	programKey := k(9)
	everyMint := buildMint(&authority, nil, 5, 0, concat(
		tlvMintCloseAuthority(&closeAuth),
		tlv(extConfidentialTransferMint, concat(optionalKey(&authority), []byte{1}, pattern(32, 3))),
		tlv(extDefaultAccountState, []byte{2}),
		tlv(extNonTransferable, nil),
		tlv(extPermanentDelegate, optionalKey(&delegate)),
		tlv(extTransferHook, concat(optionalKey(&authority), optionalKey(&programKey))),
		tlv(extConfidentialTransferFeeConfig, concat(optionalKey(&authority), pattern(32, 9), []byte{1}, pattern(64, 20))),
		tlv(extConfidentialMintBurn, concat(pattern(64, 30), pattern(36, 100), pattern(32, 140), pattern(64, 180))),
		tlv(extPausable, concat(optionalKey(&authority), []byte{1})),
		tlv(extPermissionedBurn, optionalKey(&authority)),
	))
	absentMint := buildMint(&authority, nil, 5, 0, concat(
		tlvMintCloseAuthority(nil),
		tlv(extConfidentialTransferMint, concat(optionalKey(nil), []byte{0}, make([]byte, 32))),
		tlv(extDefaultAccountState, []byte{7}),
		tlv(extConfidentialTransferFeeConfig, concat(optionalKey(nil), make([]byte, 32), []byte{0}, make([]byte, 64))),
		tlv(extPausable, concat(optionalKey(nil), []byte{0})),
	))
	everyAccount := extendAccount(buildTokenAccount(mintKey, owner, 77, nil, 0, 1, nil, nil), concat(
		tlv(extTransferFeeAmount, binary.LittleEndian.AppendUint64(nil, 42)),
		confidentialAccount(),
		tlv(extImmutableOwner, nil),
		tlv(extMemoTransfer, []byte{1}),
		tlv(extCpiGuard, []byte{1}),
		tlv(extNonTransferableAccount, nil),
		tlv(extTransferHookAccount, []byte{0}),
		tlv(extConfidentialTransferFeeAmount, pattern(64, 50)),
		tlv(extPausableAccount, nil),
	))
	// An account extension on a mint, a mint extension on an account, and
	// two extensions of the wrong length.
	wrongMint := buildMint(&authority, nil, 5, 0, concat(
		tlv(extMemoTransfer, []byte{1}),
		tlv(extImmutableOwner, nil),
		tlv(extPermanentDelegate, pattern(31, 1)),
		tlv(extPausable, pattern(34, 1)),
	))
	wrongAccount := extendAccount(buildTokenAccount(mintKey, owner, 77, nil, 0, 1, nil, nil), concat(
		tlv(extPermanentDelegate, optionalKey(&delegate)),
		tlv(extNonTransferable, nil),
		tlv(extCpiGuard, []byte{1, 1}),
	))

	return []fixture{
		{Name: "spl-token account", Pubkey: b58(account), Owner: token,
			Data: b64(buildTokenAccount(mintKey, owner, 123_456, &delegate, 1_000, 1, nil, &closeAuth)),
			Mint: b64(plainMint)},
		{Name: "spl-token account holding wrapped sol", Pubkey: b58(account), Owner: token,
			Data: b64(buildTokenAccount(mintKey, owner, 5_000_000, nil, 0, 1, &reserve, nil)),
			Mint: b64(plainMint)},
		{Name: "spl-token account frozen", Pubkey: b58(account), Owner: token,
			Data: b64(buildTokenAccount(mintKey, owner, 0, nil, 0, 2, nil, nil)),
			Mint: b64(plainMint)},
		{Name: "spl-token account whose mint cannot be read", Pubkey: b58(account), Owner: token,
			Data: b64(buildTokenAccount(mintKey, owner, 1, nil, 0, 1, nil, nil))},
		{Name: "spl-token account uninitialized", Pubkey: b58(account), Owner: token,
			Data: b64(buildTokenAccount(mintKey, owner, 0, nil, 0, 0, nil, nil)),
			Mint: b64(plainMint)},
		{Name: "spl-token mint", Pubkey: b58(mintKey), Owner: token, Data: b64(plainMint)},
		{Name: "spl-token multisig", Pubkey: b58(account), Owner: token,
			Data: b64(buildMultisig(2, 3, owner, delegate, closeAuth))},
		{Name: "token-2022 mint with a transfer fee", Pubkey: b58(mintKey), Owner: token2022,
			Data: b64(feeMint)},
		{Name: "token-2022 mint that bears interest", Pubkey: b58(mintKey), Owner: token2022,
			Data: b64(interestMint)},
		{Name: "token-2022 account on a mint that bears interest", Pubkey: b58(account), Owner: token2022,
			Data:          b64(buildTokenAccount(mintKey, owner, 1_000_000, nil, 0, 1, nil, nil)),
			Mint:          b64(interestMint),
			UnixTimestamp: 63_113_472},
		{Name: "token-2022 mint with a scaled ui amount", Pubkey: b58(mintKey), Owner: token2022,
			Data: b64(scaledMint)},
		{Name: "token-2022 account on a scaled mint before the new multiplier", Pubkey: b58(account), Owner: token2022,
			Data:          b64(buildTokenAccount(mintKey, owner, 12_345, nil, 0, 1, nil, nil)),
			Mint:          b64(scaledMint),
			UnixTimestamp: 999},
		{Name: "token-2022 account on a scaled mint after the new multiplier", Pubkey: b58(account), Owner: token2022,
			Data:          b64(buildTokenAccount(mintKey, owner, 12_345, nil, 0, 1, nil, nil)),
			Mint:          b64(scaledMint),
			UnixTimestamp: 1_000},
		{Name: "token-2022 mint whose scaled multiplier is not a number", Pubkey: b58(mintKey), Owner: token2022,
			Data: b64(brokenScaledMint)},
		{Name: "token-2022 account on a mint whose multiplier is not a number", Pubkey: b58(account), Owner: token2022,
			Data:          b64(buildTokenAccount(mintKey, owner, 3, nil, 0, 1, nil, nil)),
			Mint:          b64(brokenScaledMint),
			UnixTimestamp: 5},
		{Name: "token-2022 mint carrying metadata", Pubkey: b58(mintKey), Owner: token2022,
			Data: b64(metadataMint)},
		{Name: "token-2022 mint carrying a group and a member", Pubkey: b58(mintKey), Owner: token2022,
			Data: b64(groupMint)},
		{Name: "token-2022 mint with an extension outside the named nine", Pubkey: b58(mintKey), Owner: token2022,
			Data: b64(tenthMint)},
		{Name: "token-2022 mint with every other mint extension", Pubkey: b58(mintKey), Owner: token2022,
			Data: b64(everyMint)},
		{Name: "token-2022 mint whose optional keys are absent", Pubkey: b58(mintKey), Owner: token2022,
			Data: b64(absentMint)},
		{Name: "token-2022 account with every account extension", Pubkey: b58(account), Owner: token2022,
			Data: b64(everyAccount), Mint: b64(plainMint)},
		{Name: "token-2022 mint with extensions it cannot carry", Pubkey: b58(mintKey), Owner: token2022,
			Data: b64(wrongMint)},
		{Name: "token-2022 account with extensions it cannot carry", Pubkey: b58(account), Owner: token2022,
			Data: b64(wrongAccount), Mint: b64(plainMint)},
		{Name: "system nonce", Pubkey: b58(account), Owner: system,
			Data: b64(buildNonce(authority, k(8), 5_000))},
		{Name: "system account that is not a nonce", Pubkey: b58(account), Owner: system,
			Data: b64(make([]byte, 80))},
		{Name: "address lookup table", Pubkey: b58(account), Owner: alt,
			Data: b64(buildLookupTable(math.MaxUint64, 12, 3, &authority, owner, delegate))},
		{Name: "address lookup table without an authority", Pubkey: b58(account), Owner: alt,
			Data: b64(buildLookupTable(7, 8, 0, nil, owner))},
		{Name: "address lookup table uninitialized", Pubkey: b58(account), Owner: alt,
			Data: b64(make([]byte, 56))},
		{Name: "upgradeable loader uninitialized", Pubkey: b58(account), Owner: loader,
			Data: b64(buildLoader(0, 0, nil, nil))},
		{Name: "upgradeable loader buffer", Pubkey: b58(account), Owner: loader,
			Data: b64(buildLoader(1, 0, &authority, []byte{7, 7, 7, 7}))},
		{Name: "upgradeable loader buffer without an authority", Pubkey: b58(account), Owner: loader,
			Data: b64(buildLoader(1, 0, nil, []byte{8, 8}))},
		{Name: "upgradeable loader program", Pubkey: b58(account), Owner: loader,
			Data: b64(buildLoader(2, 0, &mintKey, nil))},
		{Name: "upgradeable loader program data", Pubkey: b58(account), Owner: loader,
			Data: b64(buildLoader(3, 42, &authority, []byte{1, 2, 3}))},
		{Name: "upgradeable loader program data without an authority", Pubkey: b58(account), Owner: loader,
			Data: b64(buildLoader(3, 9, nil, []byte{4}))},
		{Name: "sysvar clock", Pubkey: b58(svm.ClockID), Owner: sysvarOwnr,
			Data: b64(u64s(100, 200, 1, 2, 300))},
		{Name: "sysvar epoch schedule", Pubkey: b58(svm.EpochScheduleID), Owner: sysvarOwnr,
			Data: b64(concat(u64s(432_000, 432_000), []byte{0}, u64s(0, 0)))},
		{Name: "sysvar fees", Pubkey: b58(svm.FeesID), Owner: sysvarOwnr, Data: b64(u64s(5_000))},
		{Name: "sysvar rent", Pubkey: b58(svm.RentID), Owner: sysvarOwnr,
			Data: b64(concat(u64s(6_960), []byte{0, 0, 0, 0, 0, 0, 240, 63, 50}))},
		{Name: "sysvar recent blockhashes", Pubkey: b58(svm.RecentBlockhashesID), Owner: sysvarOwnr,
			Data: b64(concat(u64s(1), kb(11), u64s(5_000)))},
		{Name: "sysvar slot hashes", Pubkey: b58(svm.SlotHashesID), Owner: sysvarOwnr,
			Data: b64(concat(u64s(1, 77), kb(12)))},
		{Name: "sysvar stake history", Pubkey: b58(svm.StakeHistoryID), Owner: sysvarOwnr,
			Data: b64(u64s(1, 3, 10, 20, 30))},
		{Name: "sysvar last restart slot", Pubkey: b58(svm.LastRestartSlotID), Owner: sysvarOwnr,
			Data: b64(u64s(0))},
		{Name: "sysvar epoch rewards", Pubkey: b58(svm.EpochRewardsID), Owner: sysvarOwnr,
			Data: b64(concat(u64s(5, 2), kb(13), u64s(7, 0), u64s(11, 13), []byte{1}))},
		{Name: "an owner with no parser", Pubkey: b58(account), Owner: b58(k(31)),
			Data: b64([]byte{1, 2, 3})},
	}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
