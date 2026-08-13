package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/ids"
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

type Cleaner interface {
	Cleanup(context.Context, Owner) error
}

type Stream interface {
	io.ReadWriteCloser
}

type Session interface {
	Stream() Stream
	OpenStream(context.Context) (Stream, error)
	Wait(context.Context) error
	Close(context.Context) error
}

type RunNetworkStatus struct {
	DeniedPackets uint64
}

type RunNetworkSession interface {
	Session
	RunNetworkStatus(context.Context) (RunNetworkStatus, error)
}

type CheckpointableSession interface {
	Session
	CreateSnapshot(context.Context, SnapshotRequest) (SnapshotArtifact, error)
	Resume(context.Context) error
}

type ConnectRequest struct {
	ID             string
	OwnerKind      OwnerKind
	Binding        WorkloadBinding
	Resources      compute.ResourceVector
	PIDsMax        int64
	Topology       RuntimeTopology
	ReadOnlyDrives []ReadOnlyDrive
}

type ReadOnlyDrive struct {
	ID        string
	Digest    string
	SizeBytes int64
	MediaType string
	Source    ReadOnlyDriveSource
}

const (
	ProgramRuntimeDrive = "program_runtime"
	ProgramDrive        = "program"
)

type ReadOnlyDriveSource interface {
	LinkInto(directory string, name string, uid int, gid int) error
}

type RuntimeTopology struct {
	Substrate *RuntimeSubstrate
}

type RuntimeSubstrateSource interface {
	MaterializeInto(
		context.Context,
		string,
		string,
		int,
		int,
	) (string, error)
}

type RuntimeSubstrate struct {
	Path      string
	Source    RuntimeSubstrateSource
	Digest    string
	Format    string
	Contract  string
	SizeBytes int64
}

type SnapshotRequest struct {
	ID string
}

type SnapshotArtifact struct {
	RuntimeBackend      string
	RuntimeArch         string
	VMRuntimeContract   string
	RuntimeID           string
	KernelDigest        string
	InitramfsDigest     string
	RootfsDigest        string
	RuntimeConfigDigest string
	VMVCPUCount         int32
	CPUConfigDigest     string
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
	RuntimeInstanceID    string
	OwnerKind            OwnerKind
	Binding              WorkloadBinding
	VMState              string
	VMStateMediaType     string
	ScratchDisk          string
	ScratchDiskMediaType string
	Memory               []string
	MemoryMediaTypes     []string
	Manifest             []byte
	Checkpoint           CheckpointIdentity
	Topology             RuntimeTopology
	ReadOnlyDrives       []ReadOnlyDrive
	RecordPhase          func(RuntimePhase)
}

type MaterializeRequest struct {
	ID                 string
	OwnerKind          OwnerKind
	Binding            WorkloadBinding
	RootfsDigest       string
	WorkspaceMountPath string
	BaseVersionID      string
	Resources          compute.ResourceVector
	VMVCPUCount        int32
	CPUConfigDigest    string
	Topology           RuntimeTopology
	ReadOnlyDrives     []ReadOnlyDrive
}

// WorkloadBinding is the closed logical authority that a connector binds to
// its locally owned network attachment before a guest can receive input or
// network access. Runtime workloads use their immutable Runtime Instance ID
// with generation 1.
type WorkloadBinding struct {
	WorkerEpoch       int64
	OwnerID           string
	Generation        int64
	RuntimeInstanceID string
	RuntimeIdentityID string
}

func (binding WorkloadBinding) Validate(owner Owner) error {
	if binding.WorkerEpoch <= 0 {
		return errors.New("workload binding worker epoch must be positive")
	}
	if binding.OwnerID != owner.ID {
		return errors.New("workload binding owner does not exact-match VM owner")
	}
	if binding.Generation <= 0 {
		return errors.New("workload binding generation must be positive")
	}
	if strings.TrimSpace(binding.RuntimeIdentityID) == "" {
		return errors.New("workload binding runtime identity is required")
	}
	if owner.Kind != OwnerRuntime {
		return errors.New("workload binding owner kind is invalid")
	}
	if binding.RuntimeInstanceID != owner.ID || binding.Generation != 1 {
		return errors.New("runtime workload binding is incomplete")
	}
	return nil
}

type OwnerKind string

const (
	OwnerRuntime OwnerKind = "runtime"
)

type Owner struct {
	Kind OwnerKind `json:"kind"`
	ID   string    `json:"id"`
}

func (o Owner) Validate() error {
	if o.Kind != OwnerRuntime {
		return errors.New("VM owner kind must be runtime")
	}
	if err := ids.Validate(o.ID); err != nil {
		return errors.New("VM owner id must be a canonical UUIDv7")
	}
	return nil
}

func (o Owner) String() string {
	return string(o.Kind) + ":" + o.ID
}

type CleanupUnprovenError struct {
	Owner Owner
	Cause error
}

func (e *CleanupUnprovenError) Error() string {
	if e == nil {
		return "VM cleanup is unproven"
	}
	if e.Cause == nil {
		return fmt.Sprintf("VM cleanup is unproven for %s", e.Owner)
	}
	return fmt.Sprintf("VM cleanup is unproven for %s: %v", e.Owner, e.Cause)
}

func (e *CleanupUnprovenError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type GuestError struct {
	Cause error
}

func NewGuestError(cause error) error {
	if cause == nil {
		return nil
	}
	return &GuestError{Cause: cause}
}

func (e *GuestError) Error() string {
	if e == nil || e.Cause == nil {
		return "guest execution failed"
	}
	return "guest execution failed: " + e.Cause.Error()
}

func (e *GuestError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type CheckpointIdentity struct {
	RuntimeBackend      string
	RuntimeArch         string
	VMRuntimeContract   string
	RuntimeID           string
	KernelDigest        string
	InitramfsDigest     string
	RootfsDigest        string
	RuntimeConfigDigest string
	VMVCPUCount         int32
	CPUConfigDigest     string
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
