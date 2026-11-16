package app_test

import (
	"bytes"
	"crypto/sha256"
	"io"
	"strings"
	"testing"

	"github.com/cosmos/cosmos-sdk/store/v2/snapshots"
	snapshottypes "github.com/cosmos/cosmos-sdk/store/v2/snapshots/types"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// served runs a chain that takes snapshots and leaves a transaction inside
// the acceptance window. It returns the chain, a snapshot of it with its
// chunks, and the transaction that a joiner must refuse to run again.
func served(t *testing.T) (*tc.Chain, snapshottypes.Snapshot, [][]byte, []byte) {
	t.Helper()
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{SnapshotInterval: 1000})
	c.Block()
	spent := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), 1_000_000_000))
	if res, _ := c.Block(spent); res[0].Code != 0 {
		t.Fatalf("the transaction to replay: code %d", res[0].Code)
	}
	for i := 0; i < 3; i++ {
		c.Block()
	}
	snapshot, chunks := c.Snapshot()
	return c, snapshot, chunks, spent
}

// TestAMachineJoinsByStateSync proves that a machine with no history of its
// own restores a snapshot, reaches the same app hash, and refuses a
// transaction that executed before the snapshot.
func TestAMachineJoinsByStateSync(t *testing.T) {
	c, snapshot, chunks, spent := served(t)
	joiner, err := tc.Sync(t, snapshot, chunks)
	if err != nil {
		t.Fatalf("state sync: %v", err)
	}
	if got, want := joiner.App.LastCommitID().Hash, c.App.LastCommitID().Hash; string(got) != string(want) {
		t.Fatalf("joiner app hash %x, server %x", got, want)
	}
	if res := joiner.Check(joiner.Wrap(spent), false); res.Code == 0 {
		t.Fatal("the joiner accepted a transaction executed before its snapshot")
	}
	// The block path, which is authoritative, refuses it too.
	if res, _ := joiner.Block(spent); res[0].Code != 2 {
		t.Fatalf("replayed in a block: code %d, want 2 (not processed)", res[0].Code)
	}
}

// TestASnapshotWrittenLateStillRestores proves that a snapshot restores when
// later blocks commit while the SDK writes it. The SDK writes the dedup
// extension last, on its own goroutine, so an extension that read the live set
// could lose the oldest bucket of the snapshot's window to a later block. The
// payload would then no longer hash to the commitment in the snapshot's app
// hash.
func TestASnapshotWrittenLateStillRestores(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{SnapshotInterval: 1000})
	c.Block()
	spent := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), 1_000_000_000))
	if res, _ := c.Block(spent); res[0].Code != 0 {
		t.Fatalf("the spent transaction: code %d", res[0].Code)
	}
	// The window at this height starts with the spent transaction's bucket.
	at := c.Height + 149
	for c.Height < at {
		c.Block()
	}
	var later [][]byte
	for range 3 {
		_, hash := c.Block()
		later = append(later, hash)
	}
	m := c.App.SnapshotManager()
	snapshot, err := m.Create(uint64(at))
	if err != nil {
		t.Fatal(err)
	}
	chunks := make([][]byte, 0, snapshot.Chunks)
	for i := uint32(0); i < snapshot.Chunks; i++ {
		chunk, err := m.LoadChunk(snapshot.Height, snapshot.Format, i)
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, chunk)
	}
	joiner, err := tc.Sync(t, *snapshot, chunks)
	if err != nil {
		t.Fatalf("a snapshot written three blocks late: %v", err)
	}
	for _, want := range later {
		if _, got := joiner.Block(); !bytes.Equal(got, want) {
			t.Fatalf("height %d: joiner app hash %x, server %x", joiner.Height, got, want)
		}
	}
}

// TestAJoinerExecutesWithoutARestart proves that a joiner configures the
// engine without a restart, because CometBFT restores a snapshot into the
// running app and never runs New again. Without an engine configuration,
// CheckTx fails and the first block with an SVM transaction halts the machine.
// The engine is process-global, so the test first drops the serving chain's
// configuration.
func TestAJoinerExecutesWithoutARestart(t *testing.T) {
	c, snapshot, chunks, spent := served(t)
	ffi.UnconfigureForTests()
	joiner, err := tc.Join(t, snapshot, chunks)
	if err != nil {
		t.Fatalf("state sync: %v", err)
	}
	if got, want := joiner.App.LastCommitID().Hash, c.App.LastCommitID().Hash; !bytes.Equal(got, want) {
		t.Fatalf("joiner app hash %x, server %x", got, want)
	}
	faucet := tc.Key(1)
	transfer := tc.Tx(joiner.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(3)), svm.MinimumBalance(0)))
	if res := joiner.Check(joiner.Wrap(transfer), false); res.Code != 0 {
		t.Fatalf("CheckTx on the joiner: code %d (%s)", res.Code, res.Log)
	}
	if res := joiner.Check(joiner.Wrap(spent), false); res.Code != svm.ErrAlreadyProcessed.ABCICode() {
		t.Fatalf("a transaction executed before the snapshot: code %d, want %d", res.Code, svm.ErrAlreadyProcessed.ABCICode())
	}
	if res, _ := joiner.Block(transfer); res[0].Code != 0 {
		t.Fatalf("a block carrying an SVM transfer on the joiner: code %d", res[0].Code)
	}
	if got, want := joiner.App.History.FirstAvailableBlock(), snapshot.Height+1; got != want {
		t.Fatalf("the joiner's first available block is %d, want %d, the first after its snapshot", got, want)
	}
}

// TestAJoinerRefusesATamperedRestore proves that a joiner checks the dedup set
// when the last chunk lands. A set that is not the one the chain committed to
// aborts the restore.
func TestAJoinerRefusesATamperedRestore(t *testing.T) {
	_, snapshot, chunks, _ := served(t)
	snapshot, edited := tamper(t, snapshot, chunks)
	if _, err := tc.Join(t, snapshot, edited); err == nil || !strings.Contains(err.Error(), "ABORT") {
		t.Fatalf("a snapshot with an edited dedup payload: %v, want the restore aborted", err)
	}
}

// TestATamperedDedupPayloadFailsRestore proves that the commitment in the
// restored tree catches a tampered dedup payload. The tampering need not be
// detectable from the payload itself.
func TestATamperedDedupPayloadFailsRestore(t *testing.T) {
	_, snapshot, chunks, _ := served(t)
	snapshot, edited := tamper(t, snapshot, chunks)
	if _, err := tc.Sync(t, snapshot, edited); err == nil {
		t.Fatal("a snapshot with an edited dedup payload restored")
	} else if !strings.Contains(err.Error(), "dedup set commits to") {
		t.Fatalf("restore failed for the wrong reason: %v", err)
	}
}

// TestAnOmittedDedupExtensionFailsRestore proves that a snapshot without the
// dedup extension fails to restore. doRestoreSnapshot walks the extensions
// that the stream carries and never checks that the needed ones are there.
func TestAnOmittedDedupExtensionFailsRestore(t *testing.T) {
	_, snapshot, chunks, _ := served(t)
	snapshot, stripped := strip(t, snapshot, chunks)
	if _, err := tc.Sync(t, snapshot, stripped); err == nil {
		t.Fatal("a snapshot with no dedup extension restored")
	} else if !strings.Contains(err.Error(), "dedup set commits to") {
		t.Fatalf("restore failed for the wrong reason: %v", err)
	}
}

// tamper flips one byte of a dedup key inside the stream. The chunks are a
// zlib-compressed protobuf stream, so the edit works on the decoded bytes. A
// snapshot that an attacker makes is well formed too, which is why the
// commitment is in the tree.
func tamper(t *testing.T, snapshot snapshottypes.Snapshot, chunks [][]byte) (snapshottypes.Snapshot, [][]byte) {
	t.Helper()
	return rewrite(t, snapshot, chunks, func(payload []byte) ([]byte, bool) {
		edited := append([]byte(nil), payload...)
		edited[len(edited)-1] ^= 1
		return edited, true
	})
}

// strip drops the dedup extension's payloads, leaving a stream the SDK
// restores without complaint.
func strip(t *testing.T, snapshot snapshottypes.Snapshot, chunks [][]byte) (snapshottypes.Snapshot, [][]byte) {
	t.Helper()
	return rewrite(t, snapshot, chunks, func([]byte) ([]byte, bool) { return nil, false })
}

// rewrite edits the stream and then fixes the snapshot's chunk hashes, as the
// server of the snapshot would. With stale hashes, the SDK's chunk check
// rejects it, which proves nothing, because an attacker serves metadata that
// matches the bytes it serves.
func rewrite(t *testing.T, snapshot snapshottypes.Snapshot, chunks [][]byte, edit func([]byte) ([]byte, bool)) (snapshottypes.Snapshot, [][]byte) {
	t.Helper()
	items := decodeStream(t, chunks)
	out := make([]snapshottypes.SnapshotItem, 0, len(items))
	inDedup := false
	for _, item := range items {
		if meta := item.GetExtension(); meta != nil {
			inDedup = meta.Name == svm.DedupSnapshotName
			out = append(out, item)
			continue
		}
		if payload := item.GetExtensionPayload(); payload != nil && inDedup {
			edited, keep := edit(payload.Payload)
			if !keep {
				continue
			}
			out = append(out, snapshottypes.SnapshotItem{Item: &snapshottypes.SnapshotItem_ExtensionPayload{
				ExtensionPayload: &snapshottypes.SnapshotExtensionPayload{Payload: edited},
			}})
			continue
		}
		out = append(out, item)
	}
	edited := encodeStream(t, out)
	snapshot.Chunks = uint32(len(edited))
	snapshot.Metadata.ChunkHashes = nil
	for _, chunk := range edited {
		hash := sha256.Sum256(chunk)
		snapshot.Metadata.ChunkHashes = append(snapshot.Metadata.ChunkHashes, hash[:])
	}
	return snapshot, edited
}

// decodeStream turns a snapshot's chunks back into the items they carry. The
// pipeline is the SDK's own, run backwards: chunks to zlib to delimited
// protobuf.
func decodeStream(t *testing.T, chunks [][]byte) []snapshottypes.SnapshotItem {
	t.Helper()
	ch := make(chan io.ReadCloser, len(chunks))
	for _, chunk := range chunks {
		ch <- io.NopCloser(bytes.NewReader(chunk))
	}
	close(ch)
	reader, err := snapshots.NewStreamReader(ch)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var items []snapshottypes.SnapshotItem
	for {
		var item snapshottypes.SnapshotItem
		if err := reader.ReadMsg(&item); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	return items
}

func encodeStream(t *testing.T, items []snapshottypes.SnapshotItem) [][]byte {
	t.Helper()
	ch := make(chan io.ReadCloser)
	done := make(chan [][]byte)
	go func() {
		var out [][]byte
		for r := range ch {
			b, err := io.ReadAll(r)
			if err != nil {
				panic(err)
			}
			r.Close()
			out = append(out, b)
		}
		done <- out
	}()
	writer := snapshots.NewStreamWriter(ch)
	for i := range items {
		if err := writer.WriteMsg(&items[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return <-done
}
