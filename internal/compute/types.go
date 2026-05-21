package compute

import (
	"context"
	"errors"
	"time"
)

var ErrNoCapacity = errors.New("no compute capacity available")

type ResourceVector struct {
	MilliCPU  int64
	MemoryMiB int64
	DiskMiB   int64
	Slots     int32
}

func DefaultRunResources() ResourceVector {
	return ResourceVector{
		MilliCPU:  2000,
		MemoryMiB: 2048,
		Slots:     1,
	}
}

func (r ResourceVector) Validate(requirePositive bool) error {
	var problems []error
	if requirePositive {
		if r.MilliCPU <= 0 {
			problems = append(problems, errors.New("milli_cpu must be positive"))
		}
		if r.MemoryMiB <= 0 {
			problems = append(problems, errors.New("memory_mib must be positive"))
		}
		if r.Slots <= 0 {
			problems = append(problems, errors.New("slots must be positive"))
		}
	}
	if r.MilliCPU < 0 {
		problems = append(problems, errors.New("milli_cpu must not be negative"))
	}
	if r.MemoryMiB < 0 {
		problems = append(problems, errors.New("memory_mib must not be negative"))
	}
	if r.DiskMiB < 0 {
		problems = append(problems, errors.New("disk_mib must not be negative"))
	}
	if r.Slots < 0 {
		problems = append(problems, errors.New("slots must not be negative"))
	}
	return errors.Join(problems...)
}

func (r ResourceVector) Fits(request ResourceVector) bool {
	return r.MilliCPU >= request.MilliCPU &&
		r.MemoryMiB >= request.MemoryMiB &&
		r.DiskMiB >= request.DiskMiB &&
		r.Slots >= request.Slots
}

type RuntimeSelector struct {
	Arch         string
	ABI          string
	KernelDigest string
	RootfsDigest string
	CNIProfile   string
}

type NetworkPolicy struct {
	AllowedIPv4CIDRs  []string
	AllowedIPv6CIDRs  []string
	BlockedIPv4CIDRs  []string
	BlockedIPv6CIDRs  []string
	EgressDefaultDeny bool
}

type Placement struct {
	Region        string
	Tags          map[string]string
	DedicatedKey  string
	SnapshotKey   string
	PreferWarmRun bool
}

type RunRuntimeRequirements struct {
	Resources ResourceVector
	Runtime   RuntimeSelector
	Network   NetworkPolicy
	Placement Placement
}

func (r RunRuntimeRequirements) Validate() error {
	var problems []error
	if err := r.Resources.Validate(true); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

type ArtifactRef struct {
	URI       string
	Digest    string
	MediaType string
	SizeBytes int64
}

type SecretRef struct {
	Name string
	URI  string
}

type SessionAttachment struct {
	Name      string
	Kind      string
	Reference ArtifactRef
	ReadOnly  bool
}

type SandboxRequest struct {
	RunID            string
	ExecutionID      string
	WorkerInstanceID string
	Requirements     RunRuntimeRequirements
	Image            ArtifactRef
	TaskSource       ArtifactRef
	WorkspaceSource  ArtifactRef
	Checkpoint       *ArtifactRef
	Secrets          []SecretRef
	Attachments      []SessionAttachment
	Traceparent      string
	DequeuedAt       time.Time
	MaxDuration      time.Duration
}

type SandboxResult struct {
	ExitCode int32
	Output   []byte
	Detached bool
}

type RestoreRequest struct {
	SandboxRequest
	Snapshot ArtifactRef
}

type SandboxManager interface {
	Create(context.Context, SandboxRequest) (SandboxHandle, error)
	Restore(context.Context, RestoreRequest) (SandboxHandle, error)
}

type SandboxHandle interface {
	ID() string
	Wait(context.Context) (SandboxResult, error)
	Stop(context.Context) error
}
