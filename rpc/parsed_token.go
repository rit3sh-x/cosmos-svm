package rpc

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mr-tron/base58"
)

// The token renderer. It is the one stateful parser. A token account cannot
// render from its own bytes, because `tokenAmount` carries the mint's
// decimals. So `parse_token_v3` refuses without them, and Agave reads the
// mint at every call site.

// Layout constants, copied from `spl-token-2022-interface-3.1.0`, not
// inferred. The base account length is also where an extended mint's account
// type byte lives. That is why a mint is padded out to it.
const (
	baseAccountLength = 165
	mintLength        = 82
	multisigLength    = 355
	accountTypeMint   = 1
	accountTypeAcct   = 2

	accountStateOffset   = 108
	accountAmountOffset  = 64
	accountDelegateAt    = 72
	accountIsNativeAt    = 109
	accountDelegatedAt   = 121
	accountCloseAuthorAt = 129

	mintAuthorityAt   = 0
	mintSupplyOffset  = 36
	mintDecimalsAt    = 44
	mintInitializedAt = 45
	mintFreezeAt      = 46
)

// The extension discriminants that `ExtensionType` defines. Every one
// renders as Agave renders it. A TLV whose unpack fails renders as
// `unparseableExtension`, which is Agave's own output for it and
// part of the published type.
const (
	extTransferFeeConfig             uint16 = 1
	extTransferFeeAmount             uint16 = 2
	extMintCloseAuthority            uint16 = 3
	extConfidentialTransferMint      uint16 = 4
	extConfidentialTransferAccount   uint16 = 5
	extDefaultAccountState           uint16 = 6
	extImmutableOwner                uint16 = 7
	extMemoTransfer                  uint16 = 8
	extNonTransferable               uint16 = 9
	extInterestBearingConfig         uint16 = 10
	extCpiGuard                      uint16 = 11
	extPermanentDelegate             uint16 = 12
	extNonTransferableAccount        uint16 = 13
	extTransferHook                  uint16 = 14
	extTransferHookAccount           uint16 = 15
	extConfidentialTransferFeeConfig uint16 = 16
	extConfidentialTransferFeeAmount uint16 = 17
	extMetadataPointer               uint16 = 18
	extTokenMetadata                 uint16 = 19
	extGroupPointer                  uint16 = 20
	extTokenGroup                    uint16 = 21
	extGroupMemberPointer            uint16 = 22
	extTokenGroupMember              uint16 = 23
	extConfidentialMintBurn          uint16 = 24
	extScaledUIAmount                uint16 = 25
	extPausable                      uint16 = 26
	extPausableAccount               uint16 = 27
	extPermissionedBurn              uint16 = 28
	extHighest                       uint16 = 28
)

// accountExtensions are the ones an account carries. Every other is a
// mint's. `get_extension` refuses an extension on the other base before it
// reads a byte.
var accountExtensions = map[uint16]bool{
	extTransferFeeAmount: true, extConfidentialTransferAccount: true, extImmutableOwner: true,
	extMemoTransfer: true, extCpiGuard: true, extNonTransferableAccount: true,
	extTransferHookAccount: true, extConfidentialTransferFeeAmount: true, extPausableAccount: true,
}

type uiExtension struct {
	Extension string `json:"extension"`
	State     any    `json:"state,omitzero"`
}

var unparseableExtension = uiExtension{Extension: "unparseableExtension"}

type uiTokenAccount struct {
	Mint              string         `json:"mint"`
	Owner             string         `json:"owner"`
	TokenAmount       uiTokenAmount  `json:"tokenAmount"`
	Delegate          *string        `json:"delegate,omitempty"`
	State             string         `json:"state"`
	IsNative          bool           `json:"isNative"`
	RentExemptReserve *uiTokenAmount `json:"rentExemptReserve,omitempty"`
	DelegatedAmount   *uiTokenAmount `json:"delegatedAmount,omitempty"`
	CloseAuthority    *string        `json:"closeAuthority,omitempty"`
	Extensions        []uiExtension  `json:"extensions,omitempty"`
}

type uiMint struct {
	MintAuthority   *string       `json:"mintAuthority"`
	Supply          string        `json:"supply"`
	Decimals        uint8         `json:"decimals"`
	IsInitialized   bool          `json:"isInitialized"`
	FreezeAuthority *string       `json:"freezeAuthority"`
	Extensions      []uiExtension `json:"extensions,omitempty"`
}

type uiMultisig struct {
	NumRequiredSigners uint8    `json:"numRequiredSigners"`
	NumValidSigners    uint8    `json:"numValidSigners"`
	IsInitialized      bool     `json:"isInitialized"`
	Signers            []string `json:"signers"`
}

// parseToken is `parse_token_v3`. It tries account, then mint, then
// multisig, in that order. An extended mint is padded to account length so
// that the account arm cannot claim it.
func parseToken(data []byte, mint *mintData) (any, bool) {
	if base, tlv, ok := unpackTokenAccount(data); ok {
		if mint == nil {
			// AdditionalDataMissing. Agave's generic encoder passes None and
			// falls back to base64. Its six token call sites never pass None.
			return nil, false
		}
		ui := uiTokenAccount{
			Mint:        base58.Encode(base.mint[:]),
			Owner:       base58.Encode(base.owner[:]),
			TokenAmount: amountToUI(base.amount, mint),
			Delegate:    encodeKey(base.delegate),
			State:       accountStates[base.state],
			IsNative:    base.isNative != nil,
			Extensions:  parseExtensions(tlv, false),
		}
		if base.isNative != nil {
			reserve := amountToUI(*base.isNative, mint)
			ui.RentExemptReserve = &reserve
		}
		if base.delegate != nil {
			delegated := amountToUI(base.delegatedAmount, mint)
			ui.DelegatedAmount = &delegated
		}
		ui.CloseAuthority = encodeKey(base.closeAuthority)
		return tagged{Type: "account", Info: ui}, true
	}
	if base, tlv, ok := unpackMint(data); ok {
		return tagged{Type: "mint", Info: uiMint{
			MintAuthority:   encodeKey(base.mintAuthority),
			Supply:          strconv.FormatUint(base.supply, 10),
			Decimals:        base.decimals,
			IsInitialized:   true,
			FreezeAuthority: encodeKey(base.freezeAuthority),
			Extensions:      parseExtensions(tlv, true),
		}}, true
	}
	if len(data) == multisigLength && data[2] == 1 {
		signers := []string{}
		for i := 0; i < 11; i++ {
			key := data[3+i*32 : 35+i*32]
			if isZero(key) {
				continue
			}
			signers = append(signers, base58.Encode(key))
		}
		return tagged{Type: "multisig", Info: uiMultisig{
			NumRequiredSigners: data[0],
			NumValidSigners:    data[1],
			IsInitialized:      true,
			Signers:            signers,
		}}, true
	}
	return nil, false
}

var accountStates = map[uint8]string{0: "uninitialized", 1: "initialized", 2: "frozen"}

type tokenAccountBase struct {
	mint, owner     [32]byte
	amount          uint64
	delegate        *[32]byte
	state           uint8
	isNative        *uint64
	delegatedAmount uint64
	closeAuthority  *[32]byte
}

// unpackTokenAccount is `StateWithExtensions::<Account>::unpack` over
// `Account::unpack`. The length rules match `x/svm/tokens.go`. The COption
// tags and the account state are the rest of `Pack`.
func unpackTokenAccount(data []byte) (tokenAccountBase, []byte, bool) {
	var a tokenAccountBase
	if len(data) == multisigLength || len(data) < baseAccountLength {
		return a, nil, false
	}
	delegate, ok := coption(data, accountDelegateAt)
	if !ok {
		return a, nil, false
	}
	native, ok := coptionAmount(data, accountIsNativeAt)
	if !ok {
		return a, nil, false
	}
	closeAuthority, ok := coption(data, accountCloseAuthorAt)
	if !ok {
		return a, nil, false
	}
	state := data[accountStateOffset]
	// AccountState::try_from, then Pack::unpack's is_initialized check.
	if state == 0 || state > 2 {
		return a, nil, false
	}
	tlv, ok := extensionData(data, baseAccountLength, accountTypeAcct)
	if !ok {
		return a, nil, false
	}
	a = tokenAccountBase{
		mint:            [32]byte(data[0:32]),
		owner:           [32]byte(data[32:64]),
		amount:          binary.LittleEndian.Uint64(data[accountAmountOffset : accountAmountOffset+8]),
		delegate:        delegate,
		state:           state,
		isNative:        native,
		delegatedAmount: binary.LittleEndian.Uint64(data[accountDelegatedAt : accountDelegatedAt+8]),
		closeAuthority:  closeAuthority,
	}
	return a, tlv, true
}

type tokenMintBase struct {
	mintAuthority   *[32]byte
	supply          uint64
	decimals        uint8
	freezeAuthority *[32]byte
}

// unpackMint is `StateWithExtensions::<Mint>::unpack` over `Mint::unpack`,
// which refuses an uninitialized mint.
func unpackMint(data []byte) (tokenMintBase, []byte, bool) {
	var m tokenMintBase
	if len(data) == multisigLength || len(data) < mintLength {
		return m, nil, false
	}
	mintAuthority, ok := coption(data, mintAuthorityAt)
	if !ok {
		return m, nil, false
	}
	freezeAuthority, ok := coption(data, mintFreezeAt)
	if !ok {
		return m, nil, false
	}
	if data[mintInitializedAt] != 1 {
		return m, nil, false
	}
	tlv, ok := extensionData(data, mintLength, accountTypeMint)
	if !ok {
		return m, nil, false
	}
	m = tokenMintBase{
		mintAuthority:   mintAuthority,
		supply:          binary.LittleEndian.Uint64(data[mintSupplyOffset : mintSupplyOffset+8]),
		decimals:        data[mintDecimalsAt],
		freezeAuthority: freezeAuthority,
	}
	return m, tlv, true
}

// extensionData is `type_and_tlv_indices`. No trailing bytes means no
// extensions. With trailing bytes, the padding out to the base account length
// must be zero, the account type byte must match, and at least one byte must
// follow it.
func extensionData(data []byte, baseLen int, accountType byte) ([]byte, bool) {
	rest := data[baseLen:]
	if len(rest) == 0 {
		return nil, true
	}
	typeIndex := baseAccountLength - baseLen
	if len(rest) <= typeIndex+1 {
		return nil, false
	}
	if !isZero(rest[:typeIndex]) || rest[typeIndex] != accountType {
		return nil, false
	}
	return rest[typeIndex+1:], true
}

func coption(data []byte, at int) (*[32]byte, bool) {
	switch binary.LittleEndian.Uint32(data[at : at+4]) {
	case 0:
		return nil, true
	case 1:
		k := [32]byte(data[at+4 : at+36])
		return &k, true
	}
	return nil, false
}

func coptionAmount(data []byte, at int) (*uint64, bool) {
	switch binary.LittleEndian.Uint32(data[at : at+4]) {
	case 0:
		return nil, true
	case 1:
		v := binary.LittleEndian.Uint64(data[at+4 : at+12])
		return &v, true
	}
	return nil, false
}

func encodeKey(k *[32]byte) *string {
	if k == nil {
		return nil
	}
	s := base58.Encode(k[:])
	return &s
}

func isZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// amountToUI is `token_amount_to_ui_amount_v3`. The two time-dependent
// extensions replace the plain shift. They render the float by parsing their
// own string back, so the two fields cannot disagree.
func amountToUI(amount uint64, d *mintData) uiTokenAmount {
	var s string
	switch {
	case d.interest != nil:
		s = d.interest.uiAmount(amount, d.decimals, d.now)
	case d.scaled != nil:
		s = d.scaled.uiAmount(amount, d.decimals, d.now)
	default:
		return tokenAmount(amount, d.decimals)
	}
	out := uiTokenAmount{Decimals: d.decimals, Amount: strconv.FormatUint(amount, 10), UIAmountString: s}
	// serde_json writes a non-finite f64 as null, so Agave's Some(NaN) reaches
	// a client as a null float beside the string that produced it.
	if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
		out.UIAmount = &f
	}
	return out
}

// pow10 is `10_usize.checked_pow(decimals)`. Past nineteen digits it does not
// fit a u64. Agave's answer is then None, and the float field is null.
func pow10(decimals uint8) (float64, bool) {
	if decimals > 19 {
		return 0, false
	}
	v := uint64(1)
	for i := uint8(0); i < decimals; i++ {
		v *= 10
	}
	return float64(v), true
}

// trimUIAmount is `trim_ui_amount_string`.
func trimUIAmount(s string, decimals uint8) string {
	if decimals > 0 {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}

const (
	oneInBasisPoints = 10_000.0
	secondsPerYear   = 60.0 * 60.0 * 24.0 * 365.24
)

// interestBearingConfig is the mint extension whose amount compounds
// continuously.
type interestBearingConfig struct {
	rateAuthority        *[32]byte
	initializationTime   int64
	preUpdateAverageRate int16
	lastUpdateTime       int64
	currentRate          int16
}

func (c *interestBearingConfig) uiAmount(amount uint64, decimals uint8, now int64) string {
	scale, ok := pow10(decimals)
	if !ok {
		return ""
	}
	pre := math.Exp(float64(int64(c.preUpdateAverageRate)*(c.lastUpdateTime-c.initializationTime)) / secondsPerYear / oneInBasisPoints)
	post := math.Exp(float64(int64(c.currentRate)*(now-c.lastUpdateTime)) / secondsPerYear / oneInBasisPoints)
	v := float64(amount) * (pre * post / scale)
	return trimUIAmount(strconv.FormatFloat(v, 'f', int(decimals), 64), decimals)
}

// scaledUIAmountConfig is the mint extension that multiplies the raw amount.
type scaledUIAmountConfig struct {
	authority                       *[32]byte
	multiplier                      float64
	newMultiplierEffectiveTimestamp int64
	newMultiplier                   float64
}

func (c *scaledUIAmountConfig) current(now int64) float64 {
	if now >= c.newMultiplierEffectiveTimestamp {
		return c.newMultiplier
	}
	return c.multiplier
}

func (c *scaledUIAmountConfig) uiAmount(amount uint64, decimals uint8, now int64) string {
	scale, ok := pow10(decimals)
	if !ok {
		return ""
	}
	v := math.Trunc(float64(amount)*c.current(now)) / scale
	return trimUIAmount(strconv.FormatFloat(v, 'f', int(decimals), 64), decimals)
}

// mintDataFromAccount is `get_additional_mint_data`.
func mintDataFromAccount(data []byte, now int64) (*mintData, bool) {
	base, tlv, ok := unpackMint(data)
	if !ok {
		return nil, false
	}
	d := &mintData{decimals: base.decimals, now: now}
	if b, ok := extensionBytes(tlv, extInterestBearingConfig); ok && len(b) == 52 {
		d.interest = readInterestBearing(b)
	}
	if b, ok := extensionBytes(tlv, extScaledUIAmount); ok && len(b) == 56 {
		d.scaled = readScaledUIAmount(b)
	}
	return d, true
}

// extensionTypes is `get_tlv_data_info`. An unknown discriminant or a length
// that runs past the buffer fails the whole walk. Agave's
// `unwrap_or_default` then renders no extensions, not some.
func extensionTypes(tlv []byte) ([]uint16, bool) {
	var out []uint16
	for i := 0; i < len(tlv); {
		if len(tlv) < i+2 {
			return out, true // too few bytes for a type ends the list
		}
		t := binary.LittleEndian.Uint16(tlv[i : i+2])
		if t == 0 {
			return out, true // Uninitialized terminates the list
		}
		if t > extHighest {
			return nil, false
		}
		if len(tlv) < i+4 {
			return nil, false
		}
		out = append(out, t)
		end := i + 4 + int(binary.LittleEndian.Uint16(tlv[i+2:i+4]))
		if end > len(tlv) {
			return nil, false
		}
		i = end
	}
	return out, true
}

// extensionBytes is `get_extension_bytes`. It returns the first entry of that
// type, with the same walk. So a malformed buffer yields nothing, not a guess.
func extensionBytes(tlv []byte, want uint16) ([]byte, bool) {
	for i := 0; i+4 <= len(tlv); {
		t := binary.LittleEndian.Uint16(tlv[i : i+2])
		if t == 0 {
			return nil, false
		}
		if t > extHighest {
			return nil, false
		}
		end := i + 4 + int(binary.LittleEndian.Uint16(tlv[i+2:i+4]))
		if end > len(tlv) {
			return nil, false
		}
		if t == want {
			return tlv[i+4 : end], true
		}
		i = end
	}
	return nil, false
}

// parseExtensions is `parse_extension` over `get_extension_types`.
func parseExtensions(tlv []byte, isMint bool) []uiExtension {
	types, ok := extensionTypes(tlv)
	if !ok || len(types) == 0 {
		return nil
	}
	out := make([]uiExtension, 0, len(types))
	for _, t := range types {
		out = append(out, parseExtension(tlv, t, isMint))
	}
	return out
}

func parseExtension(tlv []byte, t uint16, isMint bool) uiExtension {
	// Four render by their type alone, with no read and no base check.
	switch t {
	case extImmutableOwner:
		return uiExtension{Extension: "immutableOwner"}
	case extNonTransferable:
		return uiExtension{Extension: "nonTransferable"}
	case extNonTransferableAccount:
		return uiExtension{Extension: "nonTransferableAccount"}
	case extPausableAccount:
		return uiExtension{Extension: "pausableAccount"}
	}
	if accountExtensions[t] == isMint {
		return unparseableExtension
	}
	b, ok := extensionBytes(tlv, t)
	if !ok {
		return unparseableExtension
	}
	// As in pod_from_bytes, an extension whose length is not its type's is not
	// read.
	sized := func(n int) bool { return len(b) == n }
	switch t {
	case extTransferFeeAmount:
		if sized(8) {
			return uiExtension{Extension: "transferFeeAmount", State: uiWithheldAmount{binary.LittleEndian.Uint64(b)}}
		}
	case extMintCloseAuthority:
		if sized(32) {
			return uiExtension{Extension: "mintCloseAuthority", State: uiMintCloseAuthority{maybeKey(b)}}
		}
	case extConfidentialTransferMint:
		if sized(65) {
			return uiExtension{Extension: "confidentialTransferMint", State: uiConfidentialTransferMint{
				Authority:              maybeKey(b[0:32]),
				AutoApproveNewAccounts: b[32] != 0,
				AuditorElgamalPubkey:   maybeBase64(b[33:65]),
			}}
		}
	case extConfidentialTransferAccount:
		if sized(295) {
			u64 := func(at int) uint64 { return binary.LittleEndian.Uint64(b[at : at+8]) }
			return uiExtension{Extension: "confidentialTransferAccount", State: uiConfidentialTransferAccount{
				Approved:                            b[0] != 0,
				ElgamalPubkey:                       podBase64(b[1:33]),
				PendingBalanceLo:                    podBase64(b[33:97]),
				PendingBalanceHi:                    podBase64(b[97:161]),
				AvailableBalance:                    podBase64(b[161:225]),
				DecryptableAvailableBalance:         podBase64(b[225:261]),
				AllowConfidentialCredits:            b[261] != 0,
				AllowNonConfidentialCredits:         b[262] != 0,
				PendingBalanceCreditCounter:         u64(263),
				MaximumPendingBalanceCreditCounter:  u64(271),
				ExpectedPendingBalanceCreditCounter: u64(279),
				ActualPendingBalanceCreditCounter:   u64(287),
			}}
		}
	case extDefaultAccountState:
		if sized(1) {
			// With AccountState::try_from(..).unwrap_or_default(), anything
			// past Frozen is Uninitialized.
			states := []string{"uninitialized", "initialized", "frozen"}
			state := states[0]
			if int(b[0]) < len(states) {
				state = states[b[0]]
			}
			return uiExtension{Extension: "defaultAccountState", State: uiDefaultAccountState{state}}
		}
	case extMemoTransfer:
		if sized(1) {
			return uiExtension{Extension: "memoTransfer", State: uiMemoTransfer{b[0] != 0}}
		}
	case extCpiGuard:
		if sized(1) {
			return uiExtension{Extension: "cpiGuard", State: uiCpiGuard{b[0] != 0}}
		}
	case extPermanentDelegate:
		if sized(32) {
			return uiExtension{Extension: "permanentDelegate", State: uiPermanentDelegate{maybeKey(b)}}
		}
	case extTransferHook:
		if sized(64) {
			return uiExtension{Extension: "transferHook", State: uiTransferHook{maybeKey(b[0:32]), maybeKey(b[32:64])}}
		}
	case extTransferHookAccount:
		if sized(1) {
			return uiExtension{Extension: "transferHookAccount", State: uiTransferHookAccount{b[0] != 0}}
		}
	case extConfidentialTransferFeeConfig:
		// The ElGamal key is not optional here, so it renders even when zero.
		if sized(129) {
			return uiExtension{Extension: "confidentialTransferFeeConfig", State: uiConfidentialTransferFeeConfig{
				Authority:                              maybeKey(b[0:32]),
				WithdrawWithheldAuthorityElgamalPubkey: podBase64(b[32:64]),
				HarvestToMintEnabled:                   b[64] != 0,
				WithheldAmount:                         podBase64(b[65:129]),
			}}
		}
	case extConfidentialTransferFeeAmount:
		if sized(64) {
			return uiExtension{Extension: "confidentialTransferFeeAmount", State: uiEncryptedWithheld{podBase64(b)}}
		}
	case extConfidentialMintBurn:
		if sized(196) {
			return uiExtension{Extension: "confidentialMintBurn", State: uiConfidentialMintBurn{
				ConfidentialSupply:  podBase64(b[0:64]),
				DecryptableSupply:   podBase64(b[64:100]),
				SupplyElgamalPubkey: podBase64(b[100:132]),
				PendingBurn:         podBase64(b[132:196]),
			}}
		}
	case extPausable:
		if sized(33) {
			return uiExtension{Extension: "pausableConfig", State: uiPausableConfig{maybeKey(b[0:32]), b[32] != 0}}
		}
	case extPermissionedBurn:
		if sized(32) {
			return uiExtension{Extension: "permissionedBurnConfig", State: uiPermanentDelegateAuthority{maybeKey(b)}}
		}
	}
	switch t {
	case extTransferFeeConfig:
		if len(b) != 108 {
			break
		}
		return uiExtension{Extension: "transferFeeConfig", State: uiTransferFeeConfig{
			TransferFeeConfigAuthority: maybeKey(b[0:32]),
			WithdrawWithheldAuthority:  maybeKey(b[32:64]),
			WithheldAmount:             binary.LittleEndian.Uint64(b[64:72]),
			OlderTransferFee:           readTransferFee(b[72:90]),
			NewerTransferFee:           readTransferFee(b[90:108]),
		}}
	case extInterestBearingConfig:
		if len(b) != 52 {
			break
		}
		c := readInterestBearing(b)
		return uiExtension{Extension: "interestBearingConfig", State: uiInterestBearingConfig{
			RateAuthority:           encodeKey(c.rateAuthority),
			InitializationTimestamp: c.initializationTime,
			PreUpdateAverageRate:    c.preUpdateAverageRate,
			LastUpdateTimestamp:     c.lastUpdateTime,
			CurrentRate:             c.currentRate,
		}}
	case extMetadataPointer:
		if len(b) != 64 {
			break
		}
		return uiExtension{Extension: "metadataPointer", State: uiMetadataPointer{
			Authority:       maybeKey(b[0:32]),
			MetadataAddress: maybeKey(b[32:64]),
		}}
	case extGroupPointer:
		if len(b) != 64 {
			break
		}
		return uiExtension{Extension: "groupPointer", State: uiGroupPointer{
			Authority:    maybeKey(b[0:32]),
			GroupAddress: maybeKey(b[32:64]),
		}}
	case extGroupMemberPointer:
		if len(b) != 64 {
			break
		}
		return uiExtension{Extension: "groupMemberPointer", State: uiGroupMemberPointer{
			Authority:     maybeKey(b[0:32]),
			MemberAddress: maybeKey(b[32:64]),
		}}
	case extTokenGroup:
		if len(b) != 80 {
			break
		}
		return uiExtension{Extension: "tokenGroup", State: uiTokenGroup{
			UpdateAuthority: maybeKey(b[0:32]),
			Mint:            base58.Encode(b[32:64]),
			Size:            binary.LittleEndian.Uint64(b[64:72]),
			MaxSize:         binary.LittleEndian.Uint64(b[72:80]),
		}}
	case extTokenGroupMember:
		if len(b) != 72 {
			break
		}
		return uiExtension{Extension: "tokenGroupMember", State: uiTokenGroupMember{
			Mint:         base58.Encode(b[0:32]),
			Group:        base58.Encode(b[32:64]),
			MemberNumber: binary.LittleEndian.Uint64(b[64:72]),
		}}
	case extScaledUIAmount:
		if len(b) != 56 {
			break
		}
		c := readScaledUIAmount(b)
		return uiExtension{Extension: "scaledUiAmountConfig", State: uiScaledUIAmountConfig{
			Authority:                       encodeKey(c.authority),
			Multiplier:                      formatF64(c.multiplier),
			NewMultiplierEffectiveTimestamp: c.newMultiplierEffectiveTimestamp,
			NewMultiplier:                   formatF64(c.newMultiplier),
		}}
	case extTokenMetadata:
		if m, ok := readTokenMetadata(b); ok {
			return uiExtension{Extension: "tokenMetadata", State: m}
		}
	}
	return unparseableExtension
}

type uiWithheldAmount struct {
	WithheldAmount uint64 `json:"withheldAmount"`
}

type uiMintCloseAuthority struct {
	CloseAuthority *string `json:"closeAuthority"`
}

type uiConfidentialTransferMint struct {
	Authority              *string `json:"authority"`
	AutoApproveNewAccounts bool    `json:"autoApproveNewAccounts"`
	AuditorElgamalPubkey   *string `json:"auditorElgamalPubkey"`
}

type uiConfidentialTransferAccount struct {
	Approved                            bool   `json:"approved"`
	ElgamalPubkey                       string `json:"elgamalPubkey"`
	PendingBalanceLo                    string `json:"pendingBalanceLo"`
	PendingBalanceHi                    string `json:"pendingBalanceHi"`
	AvailableBalance                    string `json:"availableBalance"`
	DecryptableAvailableBalance         string `json:"decryptableAvailableBalance"`
	AllowConfidentialCredits            bool   `json:"allowConfidentialCredits"`
	AllowNonConfidentialCredits         bool   `json:"allowNonConfidentialCredits"`
	PendingBalanceCreditCounter         uint64 `json:"pendingBalanceCreditCounter"`
	MaximumPendingBalanceCreditCounter  uint64 `json:"maximumPendingBalanceCreditCounter"`
	ExpectedPendingBalanceCreditCounter uint64 `json:"expectedPendingBalanceCreditCounter"`
	ActualPendingBalanceCreditCounter   uint64 `json:"actualPendingBalanceCreditCounter"`
}

type uiDefaultAccountState struct {
	AccountState string `json:"accountState"`
}

type uiMemoTransfer struct {
	RequireIncomingTransferMemos bool `json:"requireIncomingTransferMemos"`
}

type uiCpiGuard struct {
	LockCpi bool `json:"lockCpi"`
}

type uiPermanentDelegate struct {
	Delegate *string `json:"delegate"`
}

type uiTransferHook struct {
	Authority *string `json:"authority"`
	ProgramID *string `json:"programId"`
}

type uiTransferHookAccount struct {
	Transferring bool `json:"transferring"`
}

type uiConfidentialTransferFeeConfig struct {
	Authority                              *string `json:"authority"`
	WithdrawWithheldAuthorityElgamalPubkey string  `json:"withdrawWithheldAuthorityElgamalPubkey"`
	HarvestToMintEnabled                   bool    `json:"harvestToMintEnabled"`
	WithheldAmount                         string  `json:"withheldAmount"`
}

type uiEncryptedWithheld struct {
	WithheldAmount string `json:"withheldAmount"`
}

type uiConfidentialMintBurn struct {
	ConfidentialSupply  string `json:"confidentialSupply"`
	DecryptableSupply   string `json:"decryptableSupply"`
	SupplyElgamalPubkey string `json:"supplyElgamalPubkey"`
	PendingBurn         string `json:"pendingBurn"`
}

type uiPausableConfig struct {
	Authority *string `json:"authority"`
	Paused    bool    `json:"paused"`
}

type uiPermanentDelegateAuthority struct {
	Authority *string `json:"authority"`
}

// podBase64 is the Display of the zk-sdk's pod types, base64 of their bytes.
func podBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// maybeBase64 renders an optional ElGamal key. All zeros is None.
func maybeBase64(b []byte) *string {
	if isZero(b) {
		return nil
	}
	s := podBase64(b)
	return &s
}

type uiTransferFee struct {
	Epoch                  uint64 `json:"epoch"`
	MaximumFee             uint64 `json:"maximumFee"`
	TransferFeeBasisPoints uint16 `json:"transferFeeBasisPoints"`
}

type uiTransferFeeConfig struct {
	TransferFeeConfigAuthority *string       `json:"transferFeeConfigAuthority"`
	WithdrawWithheldAuthority  *string       `json:"withdrawWithheldAuthority"`
	WithheldAmount             uint64        `json:"withheldAmount"`
	OlderTransferFee           uiTransferFee `json:"olderTransferFee"`
	NewerTransferFee           uiTransferFee `json:"newerTransferFee"`
}

type uiInterestBearingConfig struct {
	RateAuthority           *string `json:"rateAuthority"`
	InitializationTimestamp int64   `json:"initializationTimestamp"`
	PreUpdateAverageRate    int16   `json:"preUpdateAverageRate"`
	LastUpdateTimestamp     int64   `json:"lastUpdateTimestamp"`
	CurrentRate             int16   `json:"currentRate"`
}

type uiMetadataPointer struct {
	Authority       *string `json:"authority"`
	MetadataAddress *string `json:"metadataAddress"`
}

type uiGroupPointer struct {
	Authority    *string `json:"authority"`
	GroupAddress *string `json:"groupAddress"`
}

type uiGroupMemberPointer struct {
	Authority     *string `json:"authority"`
	MemberAddress *string `json:"memberAddress"`
}

type uiTokenGroup struct {
	UpdateAuthority *string `json:"updateAuthority"`
	Mint            string  `json:"mint"`
	Size            uint64  `json:"size"`
	MaxSize         uint64  `json:"maxSize"`
}

type uiTokenGroupMember struct {
	Mint         string `json:"mint"`
	Group        string `json:"group"`
	MemberNumber uint64 `json:"memberNumber"`
}

type uiScaledUIAmountConfig struct {
	Authority                       *string `json:"authority"`
	Multiplier                      string  `json:"multiplier"`
	NewMultiplierEffectiveTimestamp int64   `json:"newMultiplierEffectiveTimestamp"`
	NewMultiplier                   string  `json:"newMultiplier"`
}

type uiTokenMetadata struct {
	UpdateAuthority    *string     `json:"updateAuthority"`
	Mint               string      `json:"mint"`
	Name               string      `json:"name"`
	Symbol             string      `json:"symbol"`
	URI                string      `json:"uri"`
	AdditionalMetadata [][2]string `json:"additionalMetadata"`
}

func readTransferFee(b []byte) uiTransferFee {
	return uiTransferFee{
		Epoch:                  binary.LittleEndian.Uint64(b[0:8]),
		MaximumFee:             binary.LittleEndian.Uint64(b[8:16]),
		TransferFeeBasisPoints: binary.LittleEndian.Uint16(b[16:18]),
	}
}

func readInterestBearing(b []byte) *interestBearingConfig {
	return &interestBearingConfig{
		rateAuthority:        nonZeroKey(b[0:32]),
		initializationTime:   int64(binary.LittleEndian.Uint64(b[32:40])),
		preUpdateAverageRate: int16(binary.LittleEndian.Uint16(b[40:42])),
		lastUpdateTime:       int64(binary.LittleEndian.Uint64(b[42:50])),
		currentRate:          int16(binary.LittleEndian.Uint16(b[50:52])),
	}
}

func readScaledUIAmount(b []byte) *scaledUIAmountConfig {
	return &scaledUIAmountConfig{
		authority:                       nonZeroKey(b[0:32]),
		multiplier:                      math.Float64frombits(binary.LittleEndian.Uint64(b[32:40])),
		newMultiplierEffectiveTimestamp: int64(binary.LittleEndian.Uint64(b[40:48])),
		newMultiplier:                   math.Float64frombits(binary.LittleEndian.Uint64(b[48:56])),
	}
}

// readTokenMetadata is the one variable-length extension, and the one written
// with borsh rather than as a POD.
func readTokenMetadata(b []byte) (uiTokenMetadata, bool) {
	var m uiTokenMetadata
	if len(b) < 64 {
		return m, false
	}
	m.UpdateAuthority = maybeKey(b[0:32])
	m.Mint = base58.Encode(b[32:64])
	at := 64
	var ok bool
	if m.Name, at, ok = borshString(b, at); !ok {
		return m, false
	}
	if m.Symbol, at, ok = borshString(b, at); !ok {
		return m, false
	}
	if m.URI, at, ok = borshString(b, at); !ok {
		return m, false
	}
	if len(b) < at+4 {
		return m, false
	}
	n := binary.LittleEndian.Uint32(b[at : at+4])
	at += 4
	m.AdditionalMetadata = [][2]string{}
	for i := uint32(0); i < n; i++ {
		var k, v string
		if k, at, ok = borshString(b, at); !ok {
			return m, false
		}
		if v, at, ok = borshString(b, at); !ok {
			return m, false
		}
		m.AdditionalMetadata = append(m.AdditionalMetadata, [2]string{k, v})
	}
	return m, at == len(b)
}

func borshString(b []byte, at int) (string, int, bool) {
	if len(b) < at+4 {
		return "", 0, false
	}
	n := int(binary.LittleEndian.Uint32(b[at : at+4]))
	at += 4
	if n < 0 || len(b) < at+n || !utf8.Valid(b[at:at+n]) {
		return "", 0, false
	}
	return string(b[at : at+n]), at + n, true
}

func nonZeroKey(b []byte) *[32]byte {
	if isZero(b) {
		return nil
	}
	k := [32]byte(b)
	return &k
}

func maybeKey(b []byte) *string { return encodeKey(nonZeroKey(b)) }

// formatF64 is Rust's `f64::to_string` and Display. It writes the shortest
// decimal that round-trips, never in exponent notation. Go's 'f' with
// precision -1 does the same. The three non-finite spellings are Rust's, not
// Go's. A multiplier is arbitrary account bytes, and a corrupt mint must still
// render as on Agave.
func formatF64(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}
