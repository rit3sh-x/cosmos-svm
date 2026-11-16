package rpc

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"math"
	"strconv"
	"unicode/utf8"

	"github.com/mr-tron/base58"
)

// The Token-2022 extension instructions, over spl-token-2022-interface 3.1's
// pod layouts. Each takes the instruction data after the TokenInstruction
// tag, which is the extension's own tag, then its data.

// extData is decode_instruction_data. It reads the data after the extension's
// tag, which must be exactly size bytes.
func extData(data []byte, size int) (*reader, bool) {
	if len(data) != size+1 {
		return nil, false
	}
	return &reader{b: data[1:]}, true
}

// maybeKey reads a MaybeNull<Address>, which is null when all 32 bytes are
// zero.
func (r *reader) maybeKey() any {
	b := r.take(32)
	if b == nil {
		return nil
	}
	for _, x := range b {
		if x != 0 {
			return base58.Encode(b)
		}
	}
	return nil
}

// pod is a pod type's Display, the base64 of its bytes. The pod types here are
// an ElGamal pubkey, an ElGamal ciphertext and an authenticated-encryption
// ciphertext.
func (r *reader) pod(n int) string {
	return base64.StdEncoding.EncodeToString(r.take(n))
}

// maybePod reads a MaybeNull pod, which is null when all its bytes are zero.
func (r *reader) maybePod(n int) any {
	b := r.take(n)
	for _, x := range b {
		if x != 0 {
			return base64.StdEncoding.EncodeToString(b)
		}
	}
	return nil
}

func (r *reader) i8() int8 { return int8(r.u8()) }

const (
	elgamalPubkeyLen     = 32
	elgamalCiphertextLen = 64
	aeCiphertextLen      = 36
)

func parseMintCloseAuthority(ix instruction, authority any) (any, bool, bool) {
	if len(ix.accounts) < 1 {
		return nil, false, true
	}
	return enum("initializeMintCloseAuthority", map[string]any{"mint": ix.key(0), "newAuthority": authority}), true, true
}

func parsePermanentDelegate(ix instruction, delegate string) (any, bool, bool) {
	if len(ix.accounts) < 1 {
		return nil, false, true
	}
	return enum("initializePermanentDelegate", map[string]any{"mint": ix.key(0), "delegate": delegate}), true, true
}

func parseReallocate(ix instruction, list []string) (any, bool, bool) {
	if len(ix.accounts) < 4 {
		return nil, false, true
	}
	if list == nil {
		list = []string{}
	}
	info := map[string]any{"account": ix.key(0), "payer": ix.key(1), "systemProgram": ix.key(2), "extensionTypes": list}
	ix.signers(info, 3, "owner", "multisigOwner")
	return enum("reallocate", info), true, true
}

// enableDisable parses the enable and disable instructions of the CPI guard
// and of memo transfer.
func enableDisable(ix instruction, data []byte, name string) (any, bool, bool) {
	if len(ix.accounts) < 2 || len(data) == 0 || data[0] > 1 {
		return nil, false, true
	}
	verb := "enable"
	if data[0] == 1 {
		verb = "disable"
	}
	info := map[string]any{"account": ix.key(0)}
	ix.signers(info, 1, "owner", "multisigOwner")
	return enum(verb+name, info), true, true
}

func parseCPIGuard(ix instruction, data []byte) (any, bool, bool) {
	return enableDisable(ix, data, "CpiGuard")
}

func parseMemoTransfer(ix instruction, data []byte) (any, bool, bool) {
	return enableDisable(ix, data, "RequiredMemoTransfers")
}

func parseDefaultAccountState(ix instruction, data []byte) (any, bool, bool) {
	if len(data) != 2 || data[0] > 1 || int(data[1]) >= len(accountStates) {
		return nil, false, true
	}
	info := map[string]any{"mint": nil, "accountState": accountStates[data[1]]}
	if data[0] == 0 {
		if len(ix.accounts) < 1 {
			return nil, false, true
		}
		info["mint"] = ix.key(0)
		return enum("initializeDefaultAccountState", info), true, true
	}
	if len(ix.accounts) < 2 {
		return nil, false, true
	}
	info["mint"] = ix.key(0)
	ix.signers(info, 1, "freezeAuthority", "multisigFreezeAuthority")
	return enum("updateDefaultAccountState", info), true, true
}

func parseInterestBearingMint(ix instruction, data []byte) (any, bool, bool) {
	switch data[0] {
	case 0:
		if len(ix.accounts) < 1 {
			return nil, false, true
		}
		r, ok := extData(data, 34)
		if !ok {
			return nil, false, true
		}
		authority, rate := r.maybeKey(), int16(r.u16())
		return enum("initializeInterestBearingConfig", map[string]any{"mint": ix.key(0), "rateAuthority": authority, "rate": rate}), true, true
	case 1:
		if len(ix.accounts) < 2 {
			return nil, false, true
		}
		r, ok := extData(data, 2)
		if !ok {
			return nil, false, true
		}
		info := map[string]any{"mint": ix.key(0), "newRate": int16(r.u16())}
		ix.signers(info, 1, "rateAuthority", "multisigRateAuthority")
		return enum("updateInterestBearingConfigRate", info), true, true
	}
	return nil, false, true
}

func parseScaledUIAmount(ix instruction, data []byte) (any, bool, bool) {
	if len(data) == 0 {
		return nil, false, true
	}
	switch data[0] {
	case 0:
		if len(ix.accounts) < 1 {
			return nil, false, true
		}
		r, ok := extData(data, 40)
		if !ok {
			return nil, false, true
		}
		authority, multiplier := r.maybeKey(), math.Float64frombits(r.u64())
		return enum("initializeScaledUiAmountConfig", map[string]any{"mint": ix.key(0), "authority": authority,
			"multiplier": formatF64(multiplier)}), true, true
	case 1:
		if len(ix.accounts) < 2 {
			return nil, false, true
		}
		r, ok := extData(data, 16)
		if !ok {
			return nil, false, true
		}
		multiplier, timestamp := math.Float64frombits(r.u64()), int64(r.u64())
		info := map[string]any{"mint": ix.key(0), "newMultiplier": formatF64(multiplier), "newMultiplierTimestamp": timestamp}
		ix.signers(info, 1, "authority", "multisigAuthority")
		return enum("updateMultiplier", info), true, true
	}
	return nil, false, true
}

func parsePausable(ix instruction, data []byte) (any, bool, bool) {
	if len(data) == 0 {
		return nil, false, true
	}
	switch data[0] {
	case 0:
		if len(ix.accounts) < 1 {
			return nil, false, true
		}
		r, ok := extData(data, 32)
		if !ok {
			return nil, false, true
		}
		return enum("initializePausableConfig", map[string]any{"mint": ix.key(0), "authority": r.key()}), true, true
	case 1, 2:
		if len(ix.accounts) < 2 {
			return nil, false, true
		}
		info := map[string]any{"mint": ix.key(0)}
		ix.signers(info, 1, "authority", "multisigAuthority")
		kind := "pause"
		if data[0] == 2 {
			kind = "resume"
		}
		return enum(kind, info), true, true
	}
	return nil, false, true
}

// parsePointer parses the metadata pointer, group pointer, group member
// pointer and transfer hook instructions. Initialize sets an optional
// authority and address. Update changes the address and needs the authority.
func parsePointer(ix instruction, data []byte, name, field string) (any, bool, bool) {
	switch data[0] {
	case 0:
		if len(ix.accounts) < 1 {
			return nil, false, true
		}
		r, ok := extData(data, 64)
		if !ok {
			return nil, false, true
		}
		info := map[string]any{"mint": ix.key(0)}
		if authority := r.maybeKey(); authority != nil {
			info["authority"] = authority
		}
		if address := r.maybeKey(); address != nil {
			info[field] = address
		}
		return enum("initialize"+name, info), true, true
	case 1:
		if len(ix.accounts) < 2 {
			return nil, false, true
		}
		r, ok := extData(data, 32)
		if !ok {
			return nil, false, true
		}
		info := map[string]any{"mint": ix.key(0)}
		if address := r.maybeKey(); address != nil {
			info[field] = address
		}
		ix.signers(info, 1, "authority", "multisigAuthority")
		return enum("update"+name, info), true, true
	}
	return nil, false, true
}

func parseTransferHook(ix instruction, data []byte) (any, bool, bool) {
	return parsePointer(ix, data, "TransferHook", "programId")
}

// parseTransferFee is parse_transfer_fee_instruction over
// TransferFeeInstruction::unpack, which ignores trailing bytes.
func parseTransferFee(ix instruction, data []byte) (any, bool, bool) {
	if len(data) == 0 {
		return nil, false, true
	}
	r := &reader{b: data[1:]}
	n := len(ix.accounts)
	switch data[0] {
	case 0:
		configAuthority, withdrawAuthority := r.pubkeyOption(), r.pubkeyOption()
		bps, maximum := r.u16(), r.u64()
		if r.bad || n < 1 {
			return nil, false, true
		}
		info := map[string]any{"mint": ix.key(0), "transferFeeBasisPoints": bps, "maximumFee": maximum}
		if configAuthority != nil {
			info["transferFeeConfigAuthority"] = configAuthority
		}
		if withdrawAuthority != nil {
			info["withdrawWithheldAuthority"] = withdrawAuthority
		}
		return enum("initializeTransferFeeConfig", info), true, true
	case 1:
		amount, decimals, fee := r.u64(), r.u8(), r.u64()
		if r.bad || n < 4 {
			return nil, false, true
		}
		info := map[string]any{"source": ix.key(0), "mint": ix.key(1), "destination": ix.key(2),
			"tokenAmount": tokenAmount(amount, decimals), "feeAmount": tokenAmount(fee, decimals)}
		ix.signers(info, 3, "authority", "multisigAuthority")
		return enum("transferCheckedWithFee", info), true, true
	case 2:
		if n < 3 {
			return nil, false, true
		}
		info := map[string]any{"mint": ix.key(0), "feeRecipient": ix.key(1)}
		ix.signers(info, 2, "withdrawWithheldAuthority", "multisigWithdrawWithheldAuthority")
		return enum("withdrawWithheldTokensFromMint", info), true, true
	case 3:
		count := int(r.u8())
		if r.bad || n < 3+count {
			return nil, false, true
		}
		first := n - count
		info := map[string]any{"mint": ix.key(0), "feeRecipient": ix.key(1), "sourceAccounts": ix.keysFrom(first)}
		instruction{ix.accounts[:first], ix.data, ix.keys}.signers(info, 2, "withdrawWithheldAuthority", "multisigWithdrawWithheldAuthority")
		return enum("withdrawWithheldTokensFromAccounts", info), true, true
	case 4:
		if n < 1 {
			return nil, false, true
		}
		return enum("harvestWithheldTokensToMint", map[string]any{"mint": ix.key(0), "sourceAccounts": ix.keysFrom(1)}), true, true
	case 5:
		bps, maximum := r.u16(), r.u64()
		if r.bad || n < 2 {
			return nil, false, true
		}
		info := map[string]any{"mint": ix.key(0), "transferFeeBasisPoints": bps, "maximumFee": maximum}
		ix.signers(info, 1, "transferFeeConfigAuthority", "multisigtransferFeeConfigAuthority")
		return enum("setTransferFee", info), true, true
	}
	return nil, false, true
}

// keysFrom is the instruction's accounts from position i on, as strings.
func (ix instruction) keysFrom(i int) []string {
	out := []string{}
	for ; i < len(ix.accounts); i++ {
		out = append(out, ix.key(i))
	}
	return out
}

// proofAccounts walks the optional accounts between the fixed accounts and the
// signers, as Agave's confidential parsers do. First comes the instructions
// sysvar, if a proof is in the transaction. Then comes a context state account
// for each proof that is not. It stops when only reserve trailing accounts
// remain, and returns the offset where the signers start.
func (ix instruction) proofAccounts(info map[string]any, offset, reserve int, proofs []struct {
	field  string
	offset int8
}) int {
	n := len(ix.accounts)
	room := func() bool { return offset < n-reserve }
	inTransaction := false
	for _, p := range proofs {
		inTransaction = inTransaction || p.offset != 0
	}
	if inTransaction && room() {
		info["instructionsSysvar"] = ix.key(offset)
		offset++
	}
	for _, p := range proofs {
		if p.offset == 0 && room() {
			info[p.field] = ix.key(offset)
			offset++
		}
	}
	return offset
}

type proof = struct {
	field  string
	offset int8
}

// proofOrSysvar is the single proof account of ConfigureAccount, EmptyAccount
// and the fee withdrawals. It is a context state account when the proof is
// not in the transaction, else the instructions sysvar.
func proofOrSysvar(info map[string]any, offset int8, key string) {
	if offset == 0 {
		info["proofContextStateAccount"] = key
	} else {
		info["instructionsSysvar"] = key
	}
}

func parseConfidentialTransfer(ix instruction, data []byte) (any, bool, bool) {
	if len(data) == 0 {
		return nil, false, true
	}
	n := len(ix.accounts)
	fail := func() (any, bool, bool) { return nil, false, true }
	switch data[0] {
	case 0:
		if n < 1 {
			return fail()
		}
		r, ok := extData(data, 32+1+elgamalPubkeyLen)
		if !ok {
			return fail()
		}
		authority, auto, auditor := r.maybeKey(), r.u8() != 0, r.maybePod(elgamalPubkeyLen)
		info := map[string]any{"mint": ix.key(0), "autoApproveNewAccounts": auto, "auditorElGamalPubkey": auditor}
		if authority != nil {
			info["authority"] = authority
		}
		return enum("initializeConfidentialTransferMint", info), true, true
	case 1:
		if n < 2 {
			return fail()
		}
		r, ok := extData(data, 1+elgamalPubkeyLen)
		if !ok {
			return fail()
		}
		auto, auditor := r.u8() != 0, r.maybePod(elgamalPubkeyLen)
		return enum("updateConfidentialTransferMint", map[string]any{"mint": ix.key(0),
			"confidentialTransferMintAuthority": ix.key(1), "autoApproveNewAccounts": auto, "auditorElGamalPubkey": auditor}), true, true
	case 2:
		if n < 4 {
			return fail()
		}
		r, ok := extData(data, aeCiphertextLen+8+1)
		if !ok {
			return fail()
		}
		zero, maxCredits, offset := r.pod(aeCiphertextLen), r.u64(), r.i8()
		info := map[string]any{"account": ix.key(0), "mint": ix.key(1), "decryptableZeroBalance": zero,
			"maximumPendingBalanceCreditCounter": maxCredits, "proofInstructionOffset": offset}
		proofOrSysvar(info, offset, ix.key(2))
		ix.signers(info, 3, "owner", "multisigOwner")
		return enum("configureConfidentialTransferAccount", info), true, true
	case 3:
		if n < 3 {
			return fail()
		}
		return enum("approveConfidentialTransferAccount", map[string]any{"account": ix.key(0), "mint": ix.key(1),
			"confidentialTransferAuditorAuthority": ix.key(2)}), true, true
	case 4:
		if n < 3 {
			return fail()
		}
		r, ok := extData(data, 1)
		if !ok {
			return fail()
		}
		offset := r.i8()
		info := map[string]any{"account": ix.key(0), "proofInstructionOffset": offset}
		proofOrSysvar(info, offset, ix.key(1))
		ix.signers(info, 2, "owner", "multisigOwner")
		return enum("emptyConfidentialTransferAccount", info), true, true
	case 5:
		if n < 3 {
			return fail()
		}
		r, ok := extData(data, 9)
		if !ok {
			return fail()
		}
		info := map[string]any{"account": ix.key(0), "mint": ix.key(1), "amount": r.u64(), "decimals": r.u8()}
		ix.signers(info, 2, "owner", "multisigOwner")
		return enum("depositConfidentialTransfer", info), true, true
	case 6:
		if n < 4 {
			return fail()
		}
		r, ok := extData(data, 9+aeCiphertextLen+2)
		if !ok {
			return fail()
		}
		amount, decimals, balance := r.u64(), r.u8(), r.pod(aeCiphertextLen)
		equality, rng := r.i8(), r.i8()
		info := map[string]any{"account": ix.key(0), "mint": ix.key(1), "amount": amount, "decimals": decimals,
			"newDecryptableAvailableBalance": balance, "equalityProofInstructionOffset": equality, "rangeProofInstructionOffset": rng}
		offset := ix.proofAccounts(info, 2, 1, []proof{{"equalityProofContextStateAccount", equality}, {"rangeProofContextStateAccount", rng}})
		ix.signers(info, offset, "owner", "multisigOwner")
		return enum("withdrawConfidentialTransfer", info), true, true
	case 7:
		if n < 4 {
			return fail()
		}
		r, ok := extData(data, aeCiphertextLen+2*elgamalCiphertextLen+3)
		if !ok {
			return fail()
		}
		balance := r.pod(aeCiphertextLen)
		r.take(2 * elgamalCiphertextLen)
		equality, validity, rng := r.i8(), r.i8(), r.i8()
		info := map[string]any{"source": ix.key(0), "mint": ix.key(1), "destination": ix.key(2),
			"newSourceDecryptableAvailableBalance": balance, "equalityProofInstructionOffset": equality,
			"ciphertextValidityProofInstructionOffset": validity, "rangeProofInstructionOffset": rng}
		offset := ix.proofAccounts(info, 3, 1, []proof{{"equalityProofContextStateAccount", equality},
			{"ciphertextValidityProofContextStateAccount", validity}, {"rangeProofContextStateAccount", rng}})
		ix.signers(info, offset, "owner", "multisigOwner")
		return enum("confidentialTransfer", info), true, true
	case 8:
		if n < 2 {
			return fail()
		}
		r, ok := extData(data, 8+aeCiphertextLen)
		if !ok {
			return fail()
		}
		counter, balance := r.u64(), r.pod(aeCiphertextLen)
		info := map[string]any{"account": ix.key(0), "newDecryptableAvailableBalance": balance, "expectedPendingBalanceCreditCounter": counter}
		ix.signers(info, 1, "owner", "multisigOwner")
		return enum("applyPendingConfidentialTransferBalance", info), true, true
	case 9, 10, 11, 12:
		if n < 2 {
			return fail()
		}
		info := map[string]any{"account": ix.key(0)}
		ix.signers(info, 1, "owner", "multisigOwner")
		kind := map[byte]string{
			9:  "enableConfidentialTransferConfidentialCredits",
			10: "disableConfidentialTransferConfidentialCredits",
			11: "enableConfidentialTransferNonConfidentialCredits",
			12: "disableConfidentialTransferNonConfidentialCredits",
		}[data[0]]
		return enum(kind, info), true, true
	case 13:
		if n < 4 {
			return fail()
		}
		r, ok := extData(data, aeCiphertextLen+2*elgamalCiphertextLen+5)
		if !ok {
			return fail()
		}
		balance := r.pod(aeCiphertextLen)
		r.take(2 * elgamalCiphertextLen)
		equality, amountValidity, sigma, feeValidity, rng := r.i8(), r.i8(), r.i8(), r.i8(), r.i8()
		info := map[string]any{"source": ix.key(0), "mint": ix.key(1), "destination": ix.key(2),
			"newSourceDecryptableAvailableBalance": balance, "equalityProofInstructionOffset": equality,
			"transferAmountCiphertextValidityProofInstructionOffset": amountValidity,
			"feeCiphertextValidityProofInstructionOffset":            feeValidity,
			"feeSigmaProofInstructionOffset":                         sigma, "rangeProofInstructionOffset": rng}
		offset := ix.proofAccounts(info, 3, 1, []proof{{"equalityProofContextStateAccount", equality},
			{"transferAmountCiphertextValidityProofContextStateAccount", amountValidity},
			{"feeSigmaProofContextStateAccount", sigma}, {"feeCiphertextValidityProofContextStateAccount", feeValidity},
			{"rangeProofContextStateAccount", rng}})
		ix.signers(info, offset, "owner", "multisigOwner")
		return enum("confidentialTransferWithFee", info), true, true
	case 14:
		if n < 3 {
			return fail()
		}
		return enum("configureConfidentialAccountWithRegistry", map[string]any{"account": ix.key(0), "mint": ix.key(1),
			"registry": ix.key(2)}), true, true
	}
	return fail()
}

func parseConfidentialTransferFee(ix instruction, data []byte) (any, bool, bool) {
	n := len(ix.accounts)
	fail := func() (any, bool, bool) { return nil, false, true }
	switch data[0] {
	case 0:
		if n < 1 {
			return fail()
		}
		r, ok := extData(data, 32+elgamalPubkeyLen)
		if !ok {
			return fail()
		}
		authority, pubkey := r.maybeKey(), r.pod(elgamalPubkeyLen)
		info := map[string]any{"mint": ix.key(0), "withdrawWithheldAuthorityElGamalPubkey": pubkey}
		if authority != nil {
			info["authority"] = authority
		}
		return enum("initializeConfidentialTransferFeeConfig", info), true, true
	case 1:
		if n < 4 {
			return fail()
		}
		r, ok := extData(data, 1+aeCiphertextLen)
		if !ok {
			return fail()
		}
		offset, balance := r.i8(), r.pod(aeCiphertextLen)
		info := map[string]any{"mint": ix.key(0), "feeRecipient": ix.key(1), "proofInstructionOffset": offset,
			"newDecryptableAvailableBalance": balance}
		at := 2
		if at < n-1 {
			proofOrSysvar(info, offset, ix.key(at))
			at++
		}
		ix.signers(info, at, "withdrawWithheldAuthority", "multisigWithdrawWithheldAuthority")
		return enum("withdrawWithheldConfidentialTransferTokensFromMint", info), true, true
	case 2:
		r, ok := extData(data, 2+aeCiphertextLen)
		if !ok {
			return fail()
		}
		count, offset, balance := int(r.u8()), r.i8(), r.pod(aeCiphertextLen)
		if n < 4+count {
			return fail()
		}
		first := n - count
		info := map[string]any{"mint": ix.key(0), "feeRecipient": ix.key(1), "proofInstructionOffset": offset,
			"newDecryptableAvailableBalance": balance, "sourceAccounts": ix.keysFrom(first)}
		proofOrSysvar(info, offset, ix.key(2))
		instruction{ix.accounts[:first], ix.data, ix.keys}.signers(info, 3, "withdrawWithheldAuthority", "multisigWithdrawWithheldAuthority")
		return enum("withdrawWithheldConfidentialTransferTokensFromAccounts", info), true, true
	case 3:
		if n < 1 {
			return fail()
		}
		return enum("harvestWithheldConfidentialTransferTokensToMint", map[string]any{"mint": ix.key(0), "sourceAccounts": ix.keysFrom(1)}), true, true
	case 4, 5:
		if n < 2 {
			return fail()
		}
		info := map[string]any{"account": ix.key(0)}
		ix.signers(info, 1, "owner", "multisigOwner")
		kind := "enableConfidentialTransferFeeHarvestToMint"
		if data[0] == 5 {
			kind = "disableConfidentialTransferFeeHarvestToMint"
		}
		return enum(kind, info), true, true
	}
	return fail()
}

func parseConfidentialMintBurn(ix instruction, data []byte) (any, bool, bool) {
	if len(data) == 0 {
		return nil, false, true
	}
	n := len(ix.accounts)
	fail := func() (any, bool, bool) { return nil, false, true }
	switch data[0] {
	case 0:
		if n < 1 {
			return fail()
		}
		r, ok := extData(data, elgamalPubkeyLen+aeCiphertextLen)
		if !ok {
			return fail()
		}
		pubkey, supply := r.pod(elgamalPubkeyLen), r.pod(aeCiphertextLen)
		return enum("initializeConfidentialMintBurnMint", map[string]any{"mint": ix.key(0), "supplyElGamalPubkey": pubkey,
			"decryptableSupply": supply}), true, true
	case 1:
		if n < 3 {
			return fail()
		}
		r, ok := extData(data, elgamalPubkeyLen+1)
		if !ok {
			return fail()
		}
		pubkey, offset := r.pod(elgamalPubkeyLen), r.i8()
		info := map[string]any{"mint": ix.key(0), "newSupplyElGamalPubkey": pubkey, "proofInstructionOffset": offset}
		if 1 < n-1 {
			proofOrSysvar(info, offset, ix.key(1))
		}
		ix.signers(info, 2, "owner", "multisigOwner")
		return enum("rotateConfidentialMintBurnSupplyElGamalPubkey", info), true, true
	case 2:
		if n < 2 {
			return fail()
		}
		r, ok := extData(data, aeCiphertextLen)
		if !ok {
			return fail()
		}
		info := map[string]any{"mint": ix.key(0), "newDecryptableSupply": r.pod(aeCiphertextLen)}
		ix.signers(info, 1, "owner", "multisigOwner")
		return enum("updateConfidentialMintBurnDecryptableSupply", info), true, true
	case 3, 4:
		if n < 3 {
			return fail()
		}
		r, ok := extData(data, aeCiphertextLen+2*elgamalCiphertextLen+3)
		if !ok {
			return fail()
		}
		balance := r.pod(aeCiphertextLen)
		r.take(2 * elgamalCiphertextLen)
		equality, validity, rng := r.i8(), r.i8(), r.i8()
		info := map[string]any{"destination": ix.key(0), "mint": ix.key(1), "equalityProofInstructionOffset": equality,
			"ciphertextValidityProofInstructionOffset": validity, "rangeProofInstructionOffset": rng}
		kind := "confidentialMint"
		if data[0] == 3 {
			info["newDecryptableSupply"] = balance
		} else {
			info["newDecryptableAvailableBalance"] = balance
			kind = "confidentialBurn"
		}
		offset := ix.proofAccounts(info, 2, 1, []proof{{"equalityProofContextStateAccount", equality},
			{"ciphertextValidityProofContextStateAccount", validity}, {"rangeProofContextStateAccount", rng}})
		ix.signers(info, offset, "owner", "multisigOwner")
		return enum(kind, info), true, true
	case 5:
		if n < 2 {
			return fail()
		}
		info := map[string]any{"mint": ix.key(0)}
		ix.signers(info, 1, "owner", "multisigOwner")
		return enum("applyPendingBurn", info), true, true
	}
	return fail()
}

func parsePermissionedBurn(ix instruction, data []byte) (any, bool, bool) {
	if len(data) == 0 {
		return nil, false, true
	}
	n := len(ix.accounts)
	fail := func() (any, bool, bool) { return nil, false, true }
	switch data[0] {
	case 0:
		if n < 1 {
			return fail()
		}
		r, ok := extData(data, 32)
		if !ok {
			return fail()
		}
		return enum("initializePermissionedBurnConfig", map[string]any{"mint": ix.key(0), "authority": r.key()}), true, true
	case 1, 2:
		if n < 4 {
			return fail()
		}
		size := 8
		if data[0] == 2 {
			size = 9
		}
		r, ok := extData(data, size)
		if !ok {
			return fail()
		}
		info := map[string]any{"account": ix.key(0), "mint": ix.key(1), "permissionedBurnAuthority": ix.key(2)}
		kind := "permissionedBurn"
		if data[0] == 1 {
			info["amount"] = strconv.FormatUint(r.u64(), 10)
		} else {
			a, d := r.u64(), r.u8()
			info["tokenAmount"] = tokenAmount(a, d)
			kind = "permissionedBurnChecked"
		}
		ix.signers(info, 3, "authority", "multisigAuthority")
		return enum(kind, info), true, true
	case 3:
		if n < 4 {
			return fail()
		}
		r, ok := extData(data, aeCiphertextLen+2*elgamalCiphertextLen+3)
		if !ok {
			return fail()
		}
		balance := r.pod(aeCiphertextLen)
		r.take(2 * elgamalCiphertextLen)
		equality, validity, rng := r.i8(), r.i8(), r.i8()
		info := map[string]any{"account": ix.key(0), "mint": ix.key(1), "newDecryptableAvailableBalance": balance,
			"equalityProofInstructionOffset": equality, "ciphertextValidityProofInstructionOffset": validity,
			"rangeProofInstructionOffset": rng}
		offset := ix.proofAccounts(info, 2, 2, []proof{{"equalityProofContextStateAccount", equality},
			{"ciphertextValidityProofContextStateAccount", validity}, {"rangeProofContextStateAccount", rng}})
		if offset < n-1 {
			info["permissionedBurnAuthority"] = ix.key(offset)
			offset++
		}
		ix.signers(info, offset, "authority", "multisigAuthority")
		return enum("permissionedConfidentialBurn", info), true, true
	}
	return fail()
}

// splDiscriminator is the first eight bytes of the SHA-256 of the
// discriminator's hash input.
func splDiscriminator(input string) string {
	h := sha256.Sum256([]byte(input))
	return string(h[:8])
}

var (
	groupInitialize       = splDiscriminator("spl_token_group_interface:initialize_token_group")
	groupUpdateMaxSize    = splDiscriminator("spl_token_group_interface:update_group_max_size")
	groupUpdateAuthority  = splDiscriminator("spl_token_group_interface:update_authority")
	groupInitializeMember = splDiscriminator("spl_token_group_interface:initialize_member")

	metadataInitialize      = splDiscriminator("spl_token_metadata_interface:initialize_account")
	metadataUpdateField     = splDiscriminator("spl_token_metadata_interface:updating_field")
	metadataRemoveKey       = splDiscriminator("spl_token_metadata_interface:remove_key_ix")
	metadataUpdateAuthority = splDiscriminator("spl_token_metadata_interface:update_the_authority")
	metadataEmit            = splDiscriminator("spl_token_metadata_interface:emitter")
)

// parseTokenGroup is TokenGroupInstruction::unpack, whose pod data must be
// exactly its size, then parse_token_group_instruction.
func parseTokenGroup(ix instruction) (any, bool) {
	if len(ix.data) < 8 {
		return nil, false
	}
	rest, n := ix.data[8:], len(ix.accounts)
	switch string(ix.data[:8]) {
	case groupInitialize:
		if len(rest) != 40 || n < 3 {
			return nil, false
		}
		r := &reader{b: rest}
		authority, size := r.maybeKey(), r.u64()
		return enum("initializeTokenGroup", map[string]any{"group": ix.key(0), "maxSize": size, "mint": ix.key(1),
			"mintAuthority": ix.key(2), "updateAuthority": authority}), true
	case groupUpdateMaxSize:
		if len(rest) != 8 || n < 2 {
			return nil, false
		}
		return enum("updateTokenGroupMaxSize", map[string]any{"group": ix.key(0), "maxSize": binary.LittleEndian.Uint64(rest),
			"updateAuthority": ix.key(1)}), true
	case groupUpdateAuthority:
		if len(rest) != 32 || n < 2 {
			return nil, false
		}
		r := &reader{b: rest}
		return enum("updateTokenGroupAuthority", map[string]any{"group": ix.key(0), "updateAuthority": ix.key(1),
			"newAuthority": r.maybeKey()}), true
	case groupInitializeMember:
		if len(rest) != 0 || n < 5 {
			return nil, false
		}
		return enum("initializeTokenGroupMember", map[string]any{"member": ix.key(0), "memberMint": ix.key(1),
			"memberMintAuthority": ix.key(2), "group": ix.key(3), "groupUpdateAuthority": ix.key(4)}), true
	}
	return nil, false
}

// borshString reads a borsh String, a u32 length and then UTF-8.
func (r *reader) borshString() string {
	n := r.u32()
	if uint64(n) > uint64(len(r.b)) {
		r.bad = true
		return ""
	}
	b := r.take(int(n))
	if !utf8.Valid(b) {
		r.bad = true
	}
	return string(b)
}

// borshOptionU64 is a borsh Option<u64>.
func (r *reader) borshOptionU64() any {
	switch r.u8() {
	case 0:
		return nil
	case 1:
		return r.u64()
	default:
		r.bad = true
		return nil
	}
}

// parseTokenMetadata is TokenMetadataInstruction::unpack, borsh with no
// trailing bytes, then parse_token_metadata_instruction.
func parseTokenMetadata(ix instruction) (any, bool) {
	if len(ix.data) < 8 {
		return nil, false
	}
	r, n := &reader{b: ix.data[8:]}, len(ix.accounts)
	var kind string
	var info map[string]any
	need := 2
	switch string(ix.data[:8]) {
	case metadataInitialize:
		name, symbol, uri := r.borshString(), r.borshString(), r.borshString()
		kind, need = "initializeTokenMetadata", 4
		info = map[string]any{"name": name, "symbol": symbol, "uri": uri}
	case metadataUpdateField:
		var field string
		switch r.u8() {
		case 0:
			field = "name"
		case 1:
			field = "symbol"
		case 2:
			field = "uri"
		case 3:
			field = r.borshString()
		default:
			r.bad = true
		}
		value := r.borshString()
		kind = "updateTokenMetadataField"
		info = map[string]any{"field": field, "value": value}
	case metadataRemoveKey:
		idempotent := r.u8()
		if idempotent > 1 {
			r.bad = true
		}
		key := r.borshString()
		kind = "removeTokenMetadataKey"
		info = map[string]any{"key": key, "idempotent": idempotent == 1}
	case metadataUpdateAuthority:
		kind = "updateTokenMetadataAuthority"
		info = map[string]any{"newAuthority": r.maybeKey()}
	case metadataEmit:
		start, end := r.borshOptionU64(), r.borshOptionU64()
		kind, need = "emitTokenMetadata", 1
		info = map[string]any{}
		if start != nil {
			info["start"] = start
		}
		if end != nil {
			info["end"] = end
		}
	default:
		return nil, false
	}
	if r.bad || len(r.b) != 0 || n < need {
		return nil, false
	}
	info["metadata"] = ix.key(0)
	switch kind {
	case "initializeTokenMetadata":
		info["updateAuthority"], info["mint"], info["mintAuthority"] = ix.key(1), ix.key(2), ix.key(3)
	case "emitTokenMetadata":
	default:
		info["updateAuthority"] = ix.key(1)
	}
	return enum(kind, info), true
}
