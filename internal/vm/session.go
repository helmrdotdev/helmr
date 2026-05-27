package vm

import (
	"context"
	"io"
)

type Connector interface {
	Connect(context.Context) (Session, error)
}

type RestoringConnector interface {
	Connector
	Restore(context.Context, RestoreRequest) (Session, error)
}

type Session interface {
	Stream() io.ReadWriteCloser
	Close() error
}

type CheckpointableSession interface {
	Session
	CreateSnapshot(context.Context, SnapshotRequest) (SnapshotArtifact, error)
	Resume(context.Context) error
}

type SnapshotRequest struct {
	ID string
}

type SnapshotArtifact struct {
	RuntimeBackend      string
	RuntimeArch         string
	RuntimeABI          string
	KernelDigest        string
	RootfsDigest        string
	RuntimeConfigDigest string
	VMState             SnapshotFile
	ScratchDisk         SnapshotFile
	Memory              []SnapshotFile
	Manifest            []byte
}

type SnapshotFile struct {
	Path      string
	MediaType string
}

type RestoreRequest struct {
	ID                   string
	VMState              string
	VMStateMediaType     string
	ScratchDisk          string
	ScratchDiskMediaType string
	Memory               []string
	MemoryMediaTypes     []string
	Manifest             []byte
	Checkpoint           CheckpointIdentity
}

type CheckpointIdentity struct {
	RuntimeBackend      string
	RuntimeArch         string
	RuntimeABI          string
	KernelDigest        string
	RootfsDigest        string
	RuntimeConfigDigest string
}
