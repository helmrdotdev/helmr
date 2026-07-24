package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
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

type Cleaner interface {
	Cleanup(context.Context, Owner) error
}

type Stream interface {
	io.ReadWriteCloser
	CloseWrite() error
}

type Session interface {
	Stream() Stream
	OpenStream(context.Context) (Stream, error)
	Wait(context.Context) error
	Close(context.Context) error
}

// NetworkFacts are CNI-assigned facts observed after runtime materialization.
// Placement authority must not synthesize them.
type NetworkFacts struct {
	HostInterfaceName string
	GuestAddress      string
	GatewayAddress    string
	Subnet            string
	TapName           string
	NetNSName         string
	GuestMAC          string
}

type NetworkFactSession interface {
	Session
	NetworkFacts() (NetworkFacts, error)
}

type BuildNetworkStatus struct {
	DeniedPackets uint64
	LimitPackets  uint64
}

type BuildNetworkSession interface {
	Session
	BuildNetworkStatus(context.Context) (BuildNetworkStatus, error)
}

type CheckpointableSession interface {
	Session
	CreateSnapshot(context.Context, SnapshotRequest) (SnapshotArtifact, error)
	Resume(context.Context) error
}

type ConnectRequest struct {
	ID             string
	OwnerKind      OwnerKind
	Resources      compute.ResourceVector
	PIDsMax        int64
	Networkless    bool
	Network        compute.NetworkPolicy
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
	ManagerDrive        = "manager"
	ManagedRuntimeDrive = "managed_runtime"
	ToolchainDrive      = "standard_toolchain"
	BuildTreeDrive      = "build_tree"
)

type ReadOnlyDriveSource interface {
	LinkInto(directory string, name string, uid int, gid int) error
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
	RuntimeInstanceID    string
	OwnerKind            OwnerKind
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
	ReadOnlyDrives       []ReadOnlyDrive
	RecordPhase          func(RuntimePhase)
}

type MaterializeRequest struct {
	ID                 string
	OwnerKind          OwnerKind
	RootfsDigest       string
	WorkspaceMountPath string
	BaseVersionID      string
	Resources          compute.ResourceVector
	Network            compute.NetworkPolicy
	Topology           RuntimeTopology
	ReadOnlyDrives     []ReadOnlyDrive
}

type OwnerKind string

const (
	OwnerRuntime OwnerKind = "runtime"
	OwnerBuild   OwnerKind = "build"
)

type Owner struct {
	Kind OwnerKind `json:"kind"`
	ID   string    `json:"id"`
}

func (o Owner) Validate() error {
	if o.Kind != OwnerRuntime && o.Kind != OwnerBuild {
		return errors.New("VM owner kind must be runtime or build")
	}
	id, err := uuid.Parse(o.ID)
	if err != nil || id.String() != o.ID {
		return errors.New("VM owner id must be a canonical UUID")
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
