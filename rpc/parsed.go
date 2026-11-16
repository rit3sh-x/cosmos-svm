package rpc

import (
	"encoding/base64"
	"encoding/binary"
	"math/big"
	"strconv"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// The jsonParsed rendering. The parsers are Go. Anza's
// `solana-account-decoder` is the test oracle, not a dependency. The crate
// declares thirty-six dependencies, twelve of them absent from this
// repository's lockfile. The workspace has one member, so taking the crate
// would put a JSON renderer inside the digest. `tools/parsed-oracle` emits
// the golden file instead.
//
// Six owners parse. Agave's PARSABLE_PROGRAM_IDS holds nine. Stake and Vote
// are out of scope for this chain, and Config is not one of its builtins.
// Every other owner keeps base64, which is Agave's own answer for a program
// it does not recognise.

func mustKey(s string) [32]byte {
	b, err := base58.Decode(s)
	if err != nil || len(b) != 32 {
		panic("rpc: bad key " + s)
	}
	return [32]byte(b)
}

var (
	addressLookupTable = mustKey("AddressLookupTab1e1111111111111111111111111")
	// nativeMint is the one mint Agave answers for without reading it.
	nativeMint = mustKey("So11111111111111111111111111111111111111112")
)

const nativeMintDecimals = 9

// parsedAccount is Agave's ParsedAccount. `program` is the kebab-cased
// variant name and `space` is the unsliced data length.
type parsedAccount struct {
	Program string `json:"program"`
	Parsed  any    `json:"parsed"`
	Space   uint64 `json:"space"`
}

// tagged is serde's `tag = "type", content = "info"`, which every parser but
// the token one uses for its top-level enum. A unit variant carries no info.
type tagged struct {
	Type string `json:"type"`
	Info any    `json:"info,omitzero"`
}

// uiFeeCalculator is `UiFeeCalculator`.
type uiFeeCalculator struct {
	LamportsPerSignature string `json:"lamportsPerSignature"`
}

// mintData is the mint data that a token account needs to parse. It holds the
// mint's decimals and, when the mint carries them, the two extensions that
// replace `amount / 10^decimals` with a time-dependent value.
type mintData struct {
	decimals uint8
	interest *interestBearingConfig
	scaled   *scaledUIAmountConfig
	now      int64
}

// parseAccountData is `parse_account_data_v3`. The second return is false
// where Agave returns an error. The caller turns that into base64 or a
// dropped row.
func parseAccountData(pubkey, owner [32]byte, data []byte, mint *mintData) (*parsedAccount, bool) {
	var (
		program string
		parsed  any
		ok      bool
	)
	switch owner {
	case svm.SplToken:
		program = "spl-token"
		parsed, ok = parseToken(data, mint)
	case svm.SplToken2022:
		program = "spl-token-2022"
		parsed, ok = parseToken(data, mint)
	case svm.SystemProgram:
		program = "nonce"
		parsed, ok = parseNonce(data)
	case addressLookupTable:
		program = "address-lookup-table"
		parsed, ok = parseLookupTable(data)
	case svm.LoaderV3:
		program = "bpf-upgradeable-loader"
		parsed, ok = parseUpgradeableLoader(data)
	case svm.SysvarOwner:
		program = "sysvar"
		parsed, ok = parseSysvar(pubkey, data)
	default:
		return nil, false
	}
	if !ok {
		return nil, false
	}
	return &parsedAccount{Program: program, Parsed: parsed, Space: uint64(len(data))}, true
}

// nonceLen is `State::size()`: two bincode enum tags, the authority, the
// durable nonce and the fee calculator.
const nonceLen = 4 + 4 + 32 + 32 + 8

type uiNonceData struct {
	Authority     string          `json:"authority"`
	Blockhash     string          `json:"blockhash"`
	FeeCalculator uiFeeCalculator `json:"feeCalculator"`
}

// parseNonce is `parse_nonce`. Uninitialized is an error, not a rendering,
// for the reason Agave's own comment gives. An allocated System-owned
// account of the right length may never have been a nonce.
func parseNonce(data []byte) (any, bool) {
	if len(data) < nonceLen {
		return nil, false
	}
	// Versions is Legacy or Current. Both answer .state().
	if v := binary.LittleEndian.Uint32(data[0:4]); v > 1 {
		return nil, false
	}
	if binary.LittleEndian.Uint32(data[4:8]) != 1 {
		return nil, false
	}
	return tagged{Type: "initialized", Info: uiNonceData{
		Authority: base58.Encode(data[8:40]),
		Blockhash: base58.Encode(data[40:72]),
		FeeCalculator: uiFeeCalculator{
			LamportsPerSignature: strconv.FormatUint(binary.LittleEndian.Uint64(data[72:80]), 10),
		},
	}}, true
}

// lookupTableMetaSize is LOOKUP_TABLE_META_SIZE: the bincode enum tag, the two
// slots, the start index, the optional authority and two padding bytes.
const lookupTableMetaSize = 56

type uiLookupTable struct {
	DeactivationSlot           string   `json:"deactivationSlot"`
	LastExtendedSlot           string   `json:"lastExtendedSlot"`
	LastExtendedSlotStartIndex uint8    `json:"lastExtendedSlotStartIndex"`
	Authority                  *string  `json:"authority,omitempty"`
	Addresses                  []string `json:"addresses"`
}

// parseLookupTable is `parse_address_lookup_table` over
// `AddressLookupTable::deserialize`. An uninitialized table renders. Every
// other refusal is AccountNotParsable, which the caller renders as base64.
func parseLookupTable(data []byte) (any, bool) {
	if len(data) < lookupTableMetaSize {
		return nil, false
	}
	switch binary.LittleEndian.Uint32(data[0:4]) {
	case 0:
		return tagged{Type: "uninitialized"}, true
	case 1:
	default:
		return nil, false
	}
	var authority *string
	switch data[21] {
	case 0:
	case 1:
		s := base58.Encode(data[22:54])
		authority = &s
	default:
		return nil, false
	}
	rest := data[lookupTableMetaSize:]
	if len(rest)%32 != 0 {
		return nil, false
	}
	addresses := make([]string, 0, len(rest)/32)
	for i := 0; i < len(rest); i += 32 {
		addresses = append(addresses, base58.Encode(rest[i:i+32]))
	}
	return tagged{Type: "lookupTable", Info: uiLookupTable{
		DeactivationSlot:           strconv.FormatUint(binary.LittleEndian.Uint64(data[4:12]), 10),
		LastExtendedSlot:           strconv.FormatUint(binary.LittleEndian.Uint64(data[12:20]), 10),
		LastExtendedSlotStartIndex: data[20],
		Authority:                  authority,
		Addresses:                  addresses,
	}}, true
}

type uiBuffer struct {
	Authority *string   `json:"authority"`
	Data      [2]string `json:"data"`
}

type uiProgram struct {
	ProgramData string `json:"programData"`
}

type uiProgramData struct {
	Slot      uint64    `json:"slot"`
	Authority *string   `json:"authority"`
	Data      [2]string `json:"data"`
}

// parseUpgradeableLoader is `parse_bpf_upgradeable_loader`. The trailing
// bytes are the ELF, and the offset they start at depends on whether the
// authority was written, because bincode writes nothing for a None.
func parseUpgradeableLoader(data []byte) (any, bool) {
	if len(data) < 4 {
		return nil, false
	}
	switch binary.LittleEndian.Uint32(data[0:4]) {
	case 0:
		return tagged{Type: "uninitialized"}, true
	case 1:
		authority, offset, ok := bincodeOptionKey(data, 4)
		if !ok {
			return nil, false
		}
		return tagged{Type: "buffer", Info: uiBuffer{
			Authority: authority,
			Data:      base64Data(data[offset:]),
		}}, true
	case 2:
		if len(data) < 36 {
			return nil, false
		}
		return tagged{Type: "program", Info: uiProgram{
			ProgramData: base58.Encode(data[4:36]),
		}}, true
	case 3:
		if len(data) < 12 {
			return nil, false
		}
		authority, offset, ok := bincodeOptionKey(data, 12)
		if !ok {
			return nil, false
		}
		return tagged{Type: "programData", Info: uiProgramData{
			Slot:      binary.LittleEndian.Uint64(data[4:12]),
			Authority: authority,
			Data:      base64Data(data[offset:]),
		}}, true
	}
	return nil, false
}

// bincodeOptionKey reads an Option<Pubkey> at `at` and returns the key, the
// offset after it, and whether the tag is one bincode could have written.
func bincodeOptionKey(data []byte, at int) (*string, int, bool) {
	if len(data) < at+1 {
		return nil, 0, false
	}
	switch data[at] {
	case 0:
		return nil, at + 1, true
	case 1:
		if len(data) < at+33 {
			return nil, 0, false
		}
		s := base58.Encode(data[at+1 : at+33])
		return &s, at + 33, true
	}
	return nil, 0, false
}

// base64Data is UiAccountData::Binary, which serde's untagged enum writes as a
// two-element array.
func base64Data(b []byte) [2]string {
	return [2]string{base64.StdEncoding.EncodeToString(b), "base64"}
}

type uiClock struct {
	Slot                uint64 `json:"slot"`
	Epoch               uint64 `json:"epoch"`
	EpochStartTimestamp int64  `json:"epochStartTimestamp"`
	LeaderScheduleEpoch uint64 `json:"leaderScheduleEpoch"`
	UnixTimestamp       int64  `json:"unixTimestamp"`
}

// uiEpochSchedule is EpochSchedule itself, which parse_sysvar serialises
// without a Ui type.
type uiEpochSchedule struct {
	SlotsPerEpoch            uint64 `json:"slotsPerEpoch"`
	LeaderScheduleSlotOffset uint64 `json:"leaderScheduleSlotOffset"`
	Warmup                   bool   `json:"warmup"`
	FirstNormalEpoch         uint64 `json:"firstNormalEpoch"`
	FirstNormalSlot          uint64 `json:"firstNormalSlot"`
}

// decodeEpochSchedule reads the EpochSchedule sysvar as bincode does, false
// when it is short or its warmup byte is above 1. Bincode refuses a bool byte
// that is not 0 or 1. parseSysvar and Server.epochSchedule both read through
// it, so they refuse the same rows.
func decodeEpochSchedule(data []byte) (uiEpochSchedule, bool) {
	if len(data) < 33 {
		return uiEpochSchedule{}, false
	}
	warmup, ok := readBool(data[16])
	return uiEpochSchedule{
		SlotsPerEpoch:            binary.LittleEndian.Uint64(data[0:8]),
		LeaderScheduleSlotOffset: binary.LittleEndian.Uint64(data[8:16]),
		Warmup:                   warmup,
		FirstNormalEpoch:         binary.LittleEndian.Uint64(data[17:25]),
		FirstNormalSlot:          binary.LittleEndian.Uint64(data[25:33]),
	}, ok
}

type uiFees struct {
	FeeCalculator uiFeeCalculator `json:"feeCalculator"`
}

type uiRent struct {
	LamportsPerByte string `json:"lamportsPerByte"`
}

type uiRecentBlockhashesEntry struct {
	Blockhash     string          `json:"blockhash"`
	FeeCalculator uiFeeCalculator `json:"feeCalculator"`
}

type uiSlotHashEntry struct {
	Slot uint64 `json:"slot"`
	Hash string `json:"hash"`
}

type stakeHistoryEntry struct {
	Effective    uint64 `json:"effective"`
	Activating   uint64 `json:"activating"`
	Deactivating uint64 `json:"deactivating"`
}

type uiStakeHistoryEntry struct {
	Epoch        uint64            `json:"epoch"`
	StakeHistory stakeHistoryEntry `json:"stakeHistory"`
}

type uiLastRestartSlot struct {
	LastRestartSlot uint64 `json:"lastRestartSlot"`
}

type uiEpochRewards struct {
	DistributionStartingBlockHeight uint64 `json:"distributionStartingBlockHeight"`
	NumPartitions                   uint64 `json:"numPartitions"`
	ParentBlockhash                 string `json:"parentBlockhash"`
	TotalPoints                     string `json:"totalPoints"`
	TotalRewards                    string `json:"totalRewards"`
	DistributedRewards              string `json:"distributedRewards"`
	Active                          bool   `json:"active"`
}

// parseSysvar is `parse_sysvar`, over the nine sysvar accounts this chain
// carries. Rewards and SlotHistory are the two Agave branches with no
// account here. This chain does not carry SlotHistory, and Rewards belongs
// to an inflation this chain does not have. Both fall through to base64, as
// an unknown pubkey does.
func parseSysvar(pubkey [32]byte, data []byte) (any, bool) {
	switch pubkey {
	case svm.ClockID:
		c, ok := svm.DecodeClock(data)
		if !ok {
			return nil, false
		}
		return tagged{Type: "clock", Info: uiClock{
			Slot:                c.Slot,
			EpochStartTimestamp: c.EpochStartTimestamp,
			Epoch:               c.Epoch,
			LeaderScheduleEpoch: c.LeaderScheduleEpoch,
			UnixTimestamp:       c.UnixTimestamp,
		}}, true
	case svm.EpochScheduleID:
		e, ok := decodeEpochSchedule(data)
		if !ok {
			return nil, false
		}
		return tagged{Type: "epochSchedule", Info: e}, true
	case svm.FeesID:
		if len(data) < 8 {
			return nil, false
		}
		return tagged{Type: "fees", Info: uiFees{FeeCalculator: lamports(data[0:8])}}, true
	case svm.RecentBlockhashesID:
		entries, ok := vector(data, 40)
		if !ok {
			return nil, false
		}
		out := make([]uiRecentBlockhashesEntry, 0, len(entries))
		for _, e := range entries {
			out = append(out, uiRecentBlockhashesEntry{
				Blockhash:     base58.Encode(e[0:32]),
				FeeCalculator: lamports(e[32:40]),
			})
		}
		return tagged{Type: "recentBlockhashes", Info: out}, true
	case svm.RentID:
		if len(data) < 8 {
			return nil, false
		}
		return tagged{Type: "rent", Info: uiRent{
			LamportsPerByte: strconv.FormatUint(binary.LittleEndian.Uint64(data[0:8]), 10),
		}}, true
	case svm.SlotHashesID:
		entries, ok := vector(data, 40)
		if !ok {
			return nil, false
		}
		out := make([]uiSlotHashEntry, 0, len(entries))
		for _, e := range entries {
			out = append(out, uiSlotHashEntry{
				Slot: binary.LittleEndian.Uint64(e[0:8]),
				Hash: base58.Encode(e[8:40]),
			})
		}
		return tagged{Type: "slotHashes", Info: out}, true
	case svm.StakeHistoryID:
		entries, ok := vector(data, 32)
		if !ok {
			return nil, false
		}
		out := make([]uiStakeHistoryEntry, 0, len(entries))
		for _, e := range entries {
			out = append(out, uiStakeHistoryEntry{
				Epoch: binary.LittleEndian.Uint64(e[0:8]),
				StakeHistory: stakeHistoryEntry{
					Effective:    binary.LittleEndian.Uint64(e[8:16]),
					Activating:   binary.LittleEndian.Uint64(e[16:24]),
					Deactivating: binary.LittleEndian.Uint64(e[24:32]),
				},
			})
		}
		return tagged{Type: "stakeHistory", Info: out}, true
	case svm.LastRestartSlotID:
		if len(data) < 8 {
			return nil, false
		}
		return tagged{Type: "lastRestartSlot", Info: uiLastRestartSlot{
			LastRestartSlot: binary.LittleEndian.Uint64(data[0:8]),
		}}, true
	case svm.EpochRewardsID:
		if len(data) < 81 {
			return nil, false
		}
		active, ok := readBool(data[80])
		if !ok {
			return nil, false
		}
		return tagged{Type: "epochRewards", Info: uiEpochRewards{
			DistributionStartingBlockHeight: binary.LittleEndian.Uint64(data[0:8]),
			NumPartitions:                   binary.LittleEndian.Uint64(data[8:16]),
			ParentBlockhash:                 base58.Encode(data[16:48]),
			TotalPoints:                     u128String(data[48:64]),
			TotalRewards:                    strconv.FormatUint(binary.LittleEndian.Uint64(data[64:72]), 10),
			DistributedRewards:              strconv.FormatUint(binary.LittleEndian.Uint64(data[72:80]), 10),
			Active:                          active,
		}}, true
	}
	return nil, false
}

func lamports(b []byte) uiFeeCalculator {
	return uiFeeCalculator{LamportsPerSignature: strconv.FormatUint(binary.LittleEndian.Uint64(b), 10)}
}

func readBool(b byte) (bool, bool) { return b == 1, b < 2 }

// vector reads a length-prefixed sequence of fixed-width entries. Every
// sysvar collection is a u64 count, then the entries.
func vector(data []byte, width int) ([][]byte, bool) {
	if len(data) < 8 {
		return nil, false
	}
	n := binary.LittleEndian.Uint64(data[0:8])
	if uint64(len(data)-8)/uint64(width) < n {
		return nil, false
	}
	out := make([][]byte, 0, n)
	for i := uint64(0); i < n; i++ {
		start := 8 + int(i)*width
		out = append(out, data[start:start+width])
	}
	return out, true
}

// u128String renders a little-endian u128. EpochRewards' total_points is the
// only one here.
func u128String(b []byte) string {
	be := make([]byte, len(b))
	for i := range b {
		be[len(b)-1-i] = b[i]
	}
	return new(big.Int).SetBytes(be).String()
}

// mintReader reads the mint that a token account needs to parse. It keeps
// the cache Agave keeps across a plural call, so a wallet with twenty
// accounts on one mint pays one read.
//
// A nil reader answers nil. A notification's reader reads committed state,
// as Agave's notifier reads the bank.
type mintReader struct {
	s  *Server
	ms storetypes.MultiStore
	// native answers the wrapped-SOL mint without reading it. The plural
	// path does this and the singular path does not.
	native  bool
	now     int64
	haveNow bool
	seen    map[[32]byte]*mintData
	// reads counts the mint lookups the cache did not answer, so a test can
	// assert the cache.
	reads int
}

// mints builds a reader for one request. Only jsonParsed needs one, so it
// returns nil for every other encoding.
func (s *Server) mints(ms storetypes.MultiStore, cfg accountConfig, native bool) *mintReader {
	if cfg.Encoding != "jsonParsed" {
		return nil
	}
	return &mintReader{s: s, ms: ms, native: native, seen: map[[32]byte]*mintData{}}
}

// tokenAccountMint is `get_token_account_mint` over
// `Account::valid_account_data`.
func tokenAccountMint(data []byte) ([32]byte, bool) {
	if !svm.ValidTokenAccountData(data) {
		return [32]byte{}, false
	}
	return [32]byte(data[0:32]), true
}

func (m *mintReader) forAccount(owner [32]byte, data []byte) *mintData {
	if m == nil || !svm.IsTokenProgram(owner) {
		return nil
	}
	mint, ok := tokenAccountMint(data)
	if !ok {
		return nil
	}
	if d, seen := m.seen[mint]; seen {
		return d
	}
	d := m.read(mint)
	if d != nil {
		m.seen[mint] = d
	}
	return d
}

func (m *mintReader) value(pubkey [32]byte) []byte {
	return m.s.app.Stores.GetAccount(m.ms, pubkey)
}

func (m *mintReader) read(mint [32]byte) *mintData {
	m.reads++
	if m.native && mint == nativeMint {
		return &mintData{decimals: nativeMintDecimals}
	}
	value := m.value(mint)
	if value == nil {
		return nil
	}
	a, err := ffi.DecodeAccount(value)
	if err != nil {
		return nil
	}
	d, ok := mintDataFromAccount(a.Data, m.clock())
	if !ok {
		return nil
	}
	return d
}

// clock is `bank.clock().unix_timestamp`, which the two time-dependent
// extensions read.
func (m *mintReader) clock() int64 {
	if m.haveNow {
		return m.now
	}
	m.haveNow = true
	if v := m.value(svm.ClockID); v != nil {
		if a, err := ffi.DecodeAccount(v); err == nil {
			if c, ok := svm.DecodeClock(a.Data); ok {
				m.now = c.UnixTimestamp
			}
		}
	}
	return m.now
}
