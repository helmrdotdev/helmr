package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/ids"
)

// Snapshot format bounds shared by producers and host capacity admission.
const (
	SnapshotStateLimit  int64 = 10_000_000
	MemoryChunkBytes          = 256 << 10
	MemoryReadBytes           = 1 << 20
	SnapshotConfigLimit int64 = 65_536
)

type RestoringBackend interface {
	Restore(context.Context, RestoreRequest) (CheckpointableMachine, error)
}

type MaterializingBackend interface {
	Materialize(context.Context, MaterializeRequest) (CheckpointableMachine, error)
}

type Cleaner interface {
	Cleanup(context.Context, Owner) error
}

// Backend starts machines by restore or materialization and cleans up their
// host state by owner.
type Backend interface {
	RestoringBackend
	MaterializingBackend
	Cleaner
}

type Stream interface {
	io.ReadWriteCloser
}

type Machine interface {
	Stream() Stream
	OpenStream(context.Context) (Stream, error)
	Wait(context.Context) error
	Close(context.Context) error
}

// GuestControlMachine excludes VM pause, snapshot and close while a bounded
// guest control exchange samples authority time and consumes its response.
type GuestControlMachine interface {
	Machine
	WithRunningGuestControl(context.Context, GuestControlStage, func(context.Context) error) error
}

// Installation starts a continuation fence before the clock observation can
// reach the guest. Only acknowledged activation releases it. A lost response
// permits current-process retry or terminal exclusion, never reversible pause.
type GuestControlStage uint8

const (
	GuestControlOrdinary GuestControlStage = iota
	GuestControlInstallation
	GuestControlActivation
)

// ErrCheckpointUncut proves a joined snapshot failure before any disk cut was
// attempted, with the source hold retained. It permits source reconciliation,
// never direct execution or inference from a caller timeout.
var ErrCheckpointUncut = errors.New("checkpoint failed before disk cut")

// CheckpointCapture owns one reversible VM hold. ResumeGuestControl permits only
// guest control processing: the caller must keep member cgroups sealed until it
// installs the Control Plane's exact abort grants. CompleteAbort releases the
// host hold only after the matching guest acknowledgment. Close remains the
// separate terminal operation when the checkpoint has been adopted.
type CheckpointCapture interface {
	// PrepareGuest supplies an owned stream for a bounded freeze exchange under
	// this capture's lifecycle barrier, before serialization or abort has begun.
	// It closes the stream on cancellation and return. Failure retains the hold
	// and blocks CreateSnapshot until a successful identical retry. Only explicit
	// abort or terminal close releases the hold.
	PrepareGuest(context.Context, func(context.Context, Stream) error) error
	CreateSnapshot(context.Context) (SnapshotArtifact, error)
	ResumeGuestControl(context.Context) error
	CompleteAbort(context.Context) error
}

type CheckpointableMachine interface {
	GuestControlMachine
	// PrepareCheckpoint finishes deferred memory reads before capture admission.
	PrepareCheckpoint(context.Context) error
	SnapshotLimits() (SnapshotLimits, error)
	BeginCheckpoint(context.Context, SnapshotRequest) (CheckpointCapture, error)
	CaptureComputer(context.Context) (*ComputerSnapshot, error)
}

// ComputerCaptureMachine holds customer execution and device dispatch until
// source release. It captures no RAM and does not authorize continuation.
type ComputerCaptureMachine interface {
	Machine
	SnapshotLimits() (SnapshotLimits, error)
	PauseComputerForTermination(context.Context) (*ComputerSnapshot, error)
}

type ConnectRequest struct {
	ID             string
	OwnerKind      OwnerKind
	Binding        WorkloadBinding
	Resources      Resources
	Topology       Topology
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

type Topology struct {
	Computer *ComputerDisk
}

// ComputerDisk transfers an exclusively owned working disk to the VM owner.
// File is a private backing inode, valid through Materialize; the backend
// retains its own inode link. For a block device the Instance must also retain its
// attachment and export until the VMM and all device users are proven absent.
// Device transfers that ownership to the backend when BindConsumer succeeds.
// VersionID identifies the published source, not subsequent guest writes.
type ComputerDisk struct {
	ComputerID string
	Path       string
	File       *os.File
	Device     ComputerDevice `json:"-"`
	VersionID  string
	SizeBytes  int64
}

// ComputerDevice transfers the live export to the VM lifecycle owner. The owner
// binds before launch and signals exclusion only after exact physical cleanup.
// Close failure retains the device for another Cleanup attempt. File and Device
// are mutually exclusive; snapshots contain neither process-local capability.
// Wait must return when its context is canceled. A rejected bind leaves ownership
// with the caller; successful binding retains ownership even if launch fails.
type ComputerDevice interface {
	Changes() disk.VersionChanges
	PinCommitted(context.Context) (disk.RetainedVersion, error)
	Capture(context.Context) (disk.CapturedVersion, error)
	BindConsumer(<-chan struct{}) error
	LinkInto(context.Context, string, int, int) (string, error)
	Wait(context.Context) error
	Close(context.Context) error
}

type SnapshotRequest struct {
	ID string
}

// ComputerSnapshot transfers retention of the exact cut to its receiver.
// The receiver releases Capture after all publishers have joined.
type ComputerSnapshot struct {
	ComputerID string
	Capture    disk.CapturedVersion
}

type SnapshotArtifact struct {
	Computer          *ComputerSnapshot
	RuntimeBackend    string
	RuntimeArch       string
	VMRuntimeContract string
	RuntimeID         string
	KernelDigest      string
	InitramfsDigest   string
	RootfsDigest      string
	VMConfigDigest    string
	VMVCPUCount       int32
	CPUConfigDigest   string
	VMState           SnapshotFile
	ScratchDisk       SnapshotFile
	Memory            []SnapshotFile
	Manifest          []byte
	Phases            []Phase
}

type SnapshotFile struct {
	Path      string
	MediaType string
	Filepack  *FilepackStats
}

// MemorySource authenticates each bounded logical read before returning bytes.
// Reads fit within one MemoryReadBytes window of authenticated chunks. Its owner retains it until all
// pager and hydration jobs have joined, including failure teardown.
type MemorySource interface {
	MemoryReadStats() MemoryReadStats
	Size() int64
	ReadAt(context.Context, []byte, int64) (int, error)
}

type RestoreRequest struct {
	Resources            Resources
	ID                   string
	ComputerInstanceID   string
	OwnerKind            OwnerKind
	Binding              WorkloadBinding
	VMState              string
	VMStateMediaType     string
	ScratchDisk          string
	ScratchDiskMediaType string
	MemorySource         MemorySource
	// EagerMemory materializes the same format for controlled diagnostics.
	EagerMemory    bool
	Manifest       []byte
	Checkpoint     CheckpointIdentity
	Topology       Topology
	ReadOnlyDrives []ReadOnlyDrive
	RecordPhase    func(Phase)
}

type MaterializePurpose uint8

const (
	MaterializeWorkload MaterializePurpose = iota
	// MaterializePreparationQualification runs only the fixed prepared-image probe,
	// with fresh physical identity and no Control Plane Secret transport.
	MaterializePreparationQualification
)

type MaterializeRequest struct {
	Purpose                   MaterializePurpose
	ID                        string
	OwnerKind                 OwnerKind
	Binding                   WorkloadBinding
	RootfsDigest              string
	ComputerMountPath         string
	BaseComputerDiskVersionID string
	Resources                 Resources
	VMVCPUCount               int32
	CPUConfigDigest           string
	Topology                  Topology
	ReadOnlyDrives            []ReadOnlyDrive
	RecordPhase               func(Phase)
}

// WorkloadBinding is the closed logical authority that a backend binds to
// its locally owned network attachment before a guest can receive input or
// network access. Instance workloads use their immutable Computer Instance ID
// with generation 1.
type WorkloadBinding struct {
	WorkerEpoch        int64
	OwnerID            string
	Generation         int64
	ComputerInstanceID string
	VMPlatformID       string
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
	if strings.TrimSpace(binding.VMPlatformID) == "" {
		return errors.New("workload binding runtime identity is required")
	}
	if owner.Kind != OwnerInstance {
		return errors.New("workload binding owner kind is invalid")
	}
	if binding.ComputerInstanceID != owner.ID || binding.Generation != 1 {
		return errors.New("instance workload binding is incomplete")
	}
	return nil
}

type OwnerKind string

const (
	OwnerInstance OwnerKind = "instance"
)

type Owner struct {
	Kind OwnerKind `json:"kind"`
	ID   string    `json:"id"`
}

func (o Owner) Validate() error {
	if o.Kind != OwnerInstance {
		return errors.New("VM owner kind must be instance")
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

// ErrInvalidCheckpointContent identifies a proven format/integrity failure or
// intrinsic incompatibility after matching the checkpoint to its runtime. Target
// placement, capacity, transport and startup failures must not use this marker.
var ErrInvalidCheckpointContent = errors.New("checkpoint content is unusable")

type CheckpointIdentity struct {
	RuntimeBackend    string
	RuntimeArch       string
	VMRuntimeContract string
	RuntimeID         string
	KernelDigest      string
	InitramfsDigest   string
	RootfsDigest      string
	VMConfigDigest    string
	VMVCPUCount       int32
	CPUConfigDigest   string
}

type Phase struct {
	Name       string
	DurationMs int64
	Role       string
	MediaType  string
	ErrorClass string
	Filepack   *FilepackStats
	Memory     *MemoryRestoreStats
}

type MemoryReadStats struct {
	RangeRequests    int64
	CiphertextBytes  int64
	DecodeDurationMs int64
	RangeHeaderMs    int64
	RangeBodyMs      int64
	RangeActive      int64
	RangePeak        int64
}

// MemoryFaultObservation records reader/worker timings, not kernel fault latency.
// Only the slowest 256 worker operations lasting at least 1ms are retained.
type MemoryFaultObservation struct {
	Window           int64  `json:"window"`
	ReceivedNs       int64  `json:"received_ns"`
	StartedNs        int64  `json:"started_ns"`
	FinishedNs       int64  `json:"finished_ns"`
	JoinBackgroundNs int64  `json:"join_background_ns"`
	JoinDemandNs     int64  `json:"join_demand_ns"`
	SourceReadNs     int64  `json:"source_read_ns"`
	PopulateNs       int64  `json:"populate_ns"`
	ErrorClass       string `json:"error_class"`
}
type MemoryRestoreStats struct {
	ObservedDemandPages   int64
	ObservedDemandWindows int64
	ObservationStartedAt  time.Time
	MemoryReadStats
	DemandFaults             int64
	DemandLoads              int64
	BackgroundLoads          int64
	DemandLoadErrors         int64
	BackgroundLoadErrors     int64
	JoinBackground           int64
	JoinDemand               int64
	JoinBackgroundMs         int64
	JoinDemandMs             int64
	FaultQueueMs             int64
	FaultServiceMs           int64
	DemandSourceReadMs       int64
	BackgroundSourceReadMs   int64
	DemandPopulateMs         int64
	BackgroundPopulateMs     int64
	LoadedChunks             int64
	TotalChunks              int64
	FaultObservations        []MemoryFaultObservation
	DroppedFaultObservations int64
}

type FilepackStats struct {
	LogicalBytes       int64
	EncodedChunks      int64
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

// SnapshotLimits describes stable source sizes and bounded raw metadata. The
// caller reserves encoded staging in addition to the instance-owned source disks.
type SnapshotLimits struct {
	ComputerBytes int64
	MemoryBytes   int64
	ScratchBytes  int64
	StateBytes    int64
	ConfigBytes   int64
}

// MemoryRestoreOverhead funds four bounded decoder/copy jobs and the source
// index plus pager chunk state. It is charged even on a fresh VM because that
// VM may later produce a checkpoint under the same physical reservation.
func MemoryRestoreOverhead(memoryBytes int64) (int64, error) {
	if memoryBytes <= 0 {
		return 0, errors.New("memory capacity must be positive")
	}
	chunks := (memoryBytes-1)/MemoryChunkBytes + 1
	return 16*(1<<20) + chunks*64, nil
}
