package svm

import (
	snapshottypes "github.com/cosmos/cosmos-sdk/store/v2/snapshots/types"
)

// DedupSnapshotName is the extension's name in the snapshot stream. It must be
// unique in the manager, and a machine restoring a snapshot matches on it.
const DedupSnapshotName = "svm/dedup"

// DedupSnapshotFormat is the payload format. A change to the payload layout
// takes a new number and joins SupportedFormats, so an old snapshot still
// restores.
const DedupSnapshotFormat uint32 = 1

// DedupSnapshot carries the dedup set through state sync.
//
// It only moves the bytes. At startup, AssertDedup checks the bytes against the
// commitment that the restored tree carries. That check catches an omitted
// extension as well as a tampered one, and the SDK checks neither.
type DedupSnapshot struct {
	Dedup *Dedup
}

var _ snapshottypes.ExtensionSnapshotter = DedupSnapshot{}

func (DedupSnapshot) SnapshotName() string { return DedupSnapshotName }

func (DedupSnapshot) SnapshotFormat() uint32 { return DedupSnapshotFormat }

func (DedupSnapshot) SupportedFormats() []uint32 { return []uint32{DedupSnapshotFormat} }

func (s DedupSnapshot) SnapshotExtension(height uint64, write snapshottypes.ExtensionPayloadWriter) error {
	return s.Dedup.Export(height, write)
}

func (s DedupSnapshot) RestoreExtension(height uint64, format uint32, read snapshottypes.ExtensionPayloadReader) error {
	if format != DedupSnapshotFormat {
		return snapshottypes.ErrUnknownFormat
	}
	return s.Dedup.Import(height, read)
}
