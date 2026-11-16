package svm

import (
	"encoding/binary"
	"slices"
	"strings"
	"testing"

	"github.com/mr-tron/base58"
)

// An epoch is 432,000 blocks, the first starting at height 0.
func TestAnEpochIsSlotsPerEpochBlocks(t *testing.T) {
	if epochOf(0) != 0 || epochOf(431_999) != 0 || epochOf(432_000) != 1 {
		t.Fatalf("epochs %d, %d, %d at heights 0, 431,999 and 432,000, want 0, 0, 1",
			epochOf(0), epochOf(431_999), epochOf(432_000))
	}
}

// The Clock's five fields, in Agave's order, eight bytes each.
func TestDecodeClockReadsTheFiveFields(t *testing.T) {
	data := make([]byte, clockLen)
	for i := range 5 {
		binary.LittleEndian.PutUint64(data[8*i:], uint64(i+1))
	}
	want := Clock{Slot: 1, EpochStartTimestamp: 2, Epoch: 3, LeaderScheduleEpoch: 4, UnixTimestamp: 5}
	if c, ok := DecodeClock(data); !ok || c != want {
		t.Fatalf("decoded %+v, want %+v", c, want)
	}
	if _, ok := DecodeClock(data[:clockLen-1]); ok {
		t.Fatal("a short Clock decoded")
	}
}

// A sysvar list is a u64 count and then its entries, newest first. prepended
// adds an entry at the front and keeps the size. Past the limit, it drops the
// oldest entry.
func TestASysvarListKeepsItsNewestEntries(t *testing.T) {
	d := make([]byte, listLen(2, sysvarEntryBytes))
	for i := byte(1); i <= 3; i++ {
		d = prepended(d, [40]byte{i}, 2)
	}
	if len(d) != 88 || binary.LittleEndian.Uint64(d) != 2 || listEntry(d, 0)[0] != 3 || listEntry(d, 1)[0] != 2 {
		t.Fatalf("list %x", d)
	}
}

// A sysvar row Agave's sysvar cache would not deserialize is refused, naming
// the sysvar and the field, and one it would is kept, trailing bytes
// included.
func TestASysvarRowAgaveCannotDecodeIsRefused(t *testing.T) {
	schedule := func(warmup byte) []byte {
		d := epochSchedule()
		d[16] = warmup
		return d
	}
	list := func(count uint64, room, width int) []byte {
		d := make([]byte, listLen(room, width))
		binary.LittleEndian.PutUint64(d, count)
		return d
	}
	rewards := make([]byte, 81)
	rewards[80] = 2
	most := int(wincodePreallocation / sysvarEntryBytes)
	cases := []struct {
		id   [32]byte
		data []byte
		want string // in the error, "" for a row that passes
	}{
		{EpochScheduleID, schedule(0), ""},
		{EpochScheduleID, schedule(1), ""},
		{EpochScheduleID, schedule(2), "the EpochSchedule sysvar's warmup is 2"},
		{EpochScheduleID, schedule(255), "the EpochSchedule sysvar's warmup is 255"},
		{EpochRewardsID, rewards, "the EpochRewards sysvar's active is 2"},
		{ClockID, make([]byte, clockLen-1), "the Clock sysvar is 39 bytes and ends inside unix_timestamp"},
		{ClockID, make([]byte, clockLen+1), ""},
		{StakeHistoryID, make([]byte, 7), "the StakeHistory sysvar is 7 bytes and ends inside its entries count"},
		{StakeHistoryID, list(2, 2, 32), ""},
		{StakeHistoryID, list(3, 2, 32), "the StakeHistory sysvar counts 3 entries"},
		{SlotHashesID, list(uint64(most), most, sysvarEntryBytes), ""},
		{SlotHashesID, list(uint64(most+1), most+1, sysvarEntryBytes), "the SlotHashes sysvar counts 104858 entries"},
	}
	for n, c := range cases {
		err := checkSysvar(c.id, c.data)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("case %d: %v, want %q", n, err, c.want)
		}
	}
}

// Genesis refuses a sysvar row Agave would decode but this chain cannot run
// on, as well as one Agave would not decode, and keeps the rows it writes.
func TestGenesisRefusesASysvarRowThisChainCannotRun(t *testing.T) {
	schedule := func(slots uint64, warmup byte) []byte {
		d := epochSchedule()
		binary.LittleEndian.PutUint64(d[0:8], slots)
		d[16] = warmup
		return d
	}
	recent := make([]byte, 8)
	cases := []struct {
		id   [32]byte
		data []byte
		want string // in the error, "" for a row that passes
	}{
		{EpochScheduleID, epochSchedule(), ""},
		{EpochScheduleID, schedule(slotsPerEpoch, 1), "not the schedule this chain runs"},
		{EpochScheduleID, schedule(0, 0), "not the schedule this chain runs"},
		{EpochScheduleID, schedule(slotsPerEpoch, 2), "the EpochSchedule sysvar's warmup is 2"},
		{ClockID, make([]byte, clockLen+1), "the Clock sysvar is 41 bytes, and this chain keeps it at 40"},
		{RecentBlockhashesID, recent, "the RecentBlockhashes sysvar is 8 bytes, and this chain keeps it at 6008"},
		{SlotHashesID, make([]byte, listLen(slotHashesMax, sysvarEntryBytes)+1), "the SlotHashes sysvar is 20489 bytes"},
	}
	for n, c := range cases {
		g := DefaultGenesis()
		i := slices.IndexFunc(g.Accounts, func(a GenesisAccount) bool { return a.Pubkey == base58.Encode(c.id[:]) })
		g.Accounts[i].Data, g.Accounts[i].Lamports = c.data, MinimumBalance(len(c.data))
		err := g.Validate()
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("case %d: %v, want %q", n, err, c.want)
		}
	}
}
