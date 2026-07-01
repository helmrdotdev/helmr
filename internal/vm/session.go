package vm

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/compute"
)

type Connector interface {
	Connect(context.Context, ConnectRequest) (Session, error)
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

type ConnectRequest struct {
	Network  compute.NetworkPolicy
	Topology RuntimeTopology
}

type RuntimeTopology struct {
	Substrate *RuntimeSubstrate
}

type RuntimeSubstrate struct {
	Path       string
	Digest     string
	Format     string
	BuilderABI string
	LayoutABI  string
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
	Substrate           *RuntimeSubstrate
	VMState             SnapshotFile
	ScratchDisk         SnapshotFile
	Memory              []SnapshotFile
	Manifest            []byte
	Phases              []RuntimePhase
}

type SnapshotFile struct {
	Path      string
	MediaType string
	Filepack  *FilepackStats
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
	Topology             RuntimeTopology
	RecordPhase          func(RuntimePhase)
}

type MaterializeRequest struct {
	ID                 string
	RootfsDigest       string
	ImageDigest        string
	ImageFormat        string
	WorkspaceMountPath string
	BaseVersionID      string
	Resources          compute.ResourceVector
	Network            compute.NetworkPolicy
	Topology           RuntimeTopology
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

type RuntimePhase struct {
	Name       string
	DurationMs int64
	Role       string
	MediaType  string
	ErrorClass string
	Filepack   *FilepackStats
}

type FilepackStats struct {
	LogicalBytes       int64
	AllocatedBytes     int64
	SparseSupported    *bool
	SparseDataRanges   int64
	SparseDataBytes    int64
	ZeroChunksSkipped  int64
	EncodedChunks      int64
	CompressedBytes    int64
	UnpackWrittenBytes int64
}

func RuntimeDurationMilliseconds(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return value.Milliseconds()
}

func RuntimeErrorClass(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline_exceeded"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "health probe"):
		return "guest_health"
	case strings.Contains(message, "digest") || strings.Contains(message, "manifest") || strings.Contains(message, "media type") || strings.Contains(message, "does not match"):
		return "validation"
	case strings.Contains(message, "cas") || strings.Contains(message, "checkpoint object") || strings.Contains(message, "eof") || strings.Contains(message, "read") || strings.Contains(message, "write") || strings.Contains(message, "open") || strings.Contains(message, "filepack"):
		return "io"
	case strings.Contains(message, "firecracker"):
		return "firecracker"
	default:
		return "unknown"
	}
}
