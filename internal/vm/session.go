package vm

import (
	"context"
	"io"

	"github.com/helmrdotdev/helmr/internal/compute"
)

type Connector interface {
	Connect(context.Context, compute.NetworkPolicy) (Session, error)
}

type RestoringConnector interface {
	Connector
	Restore(context.Context, RestoreRequest) (Session, error)
}

type MaterializingConnector interface {
	Connector
	Materialize(context.Context, MaterializeRequest) (Session, error)
}

type Session interface {
	Stream() io.ReadWriteCloser
	OpenStream(context.Context) (io.ReadWriteCloser, error)
	Wait(context.Context) error
	Close(context.Context) error
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
	RuntimeID           string
	KernelDigest        string
	InitramfsDigest     string
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
	Network              compute.NetworkPolicy
}

type MaterializeRequest struct {
	ID                         string
	RootfsDigest               string
	ImageDigest                string
	ImageFormat                string
	WorkspaceArtifactPath      string
	WorkspaceArtifactDigest    string
	WorkspaceArtifactMediaType string
	WorkspaceArtifactEncoding  string
	WorkspaceMountPath         string
	BaseVersionID              string
	Network                    compute.NetworkPolicy
}

type CheckpointIdentity struct {
	RuntimeBackend      string
	RuntimeArch         string
	RuntimeABI          string
	RuntimeID           string
	KernelDigest        string
	InitramfsDigest     string
	RootfsDigest        string
	RuntimeConfigDigest string
}
