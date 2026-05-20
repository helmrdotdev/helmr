package compute

import (
	"context"
	"errors"
	"time"
)

var ErrNoCapacity = errors.New("no compute capacity available")

type WorkerGroupProvisioningMode string

const (
	WorkerGroupProvisioningModeHelmrManaged    WorkerGroupProvisioningMode = "helmr_managed"
	WorkerGroupProvisioningModeCustomerManaged WorkerGroupProvisioningMode = "customer_managed"
)

type WorkerHostStatus string

const (
	WorkerHostStatusActive        WorkerHostStatus = "active"
	WorkerHostStatusDraining      WorkerHostStatus = "draining"
	WorkerHostStatusUnschedulable WorkerHostStatus = "unschedulable"
	WorkerHostStatusOffline       WorkerHostStatus = "offline"
)

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

type RunRequirements struct {
	Resources ResourceVector
	Runtime   RuntimeSelector
	Network   NetworkPolicy
	Placement Placement
}

func (r RunRequirements) Validate() error {
	var problems []error
	if err := r.Resources.Validate(true); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

type WorkerGroup struct {
	ID            string
	OrgID         string
	ProjectID     string
	EnvironmentID string
	Slug          string
	Name          string
	Mode          WorkerGroupProvisioningMode
	QueueName     string
	Region        string
	Capabilities  map[string]string
}

type WorkerHost struct {
	ID            string
	WorkerGroupID string
	Status        WorkerHostStatus
	Region        string
	Total         ResourceVector
	Available     ResourceVector
	Runtime       RuntimeSelector
	Labels        map[string]string
	LastSeenAt    time.Time
}

func (h WorkerHost) CanSchedule(requirements RunRequirements) bool {
	if h.Status != WorkerHostStatusActive {
		return false
	}
	if requirements.Placement.Region != "" && h.Region != requirements.Placement.Region {
		return false
	}
	if !h.Runtime.matches(requirements.Runtime) {
		return false
	}
	if !matchesLabels(h.Labels, requirements.Placement.Tags) {
		return false
	}
	if requirements.Placement.DedicatedKey != "" && h.Labels["dedicated_key"] != requirements.Placement.DedicatedKey {
		return false
	}
	if requirements.Placement.SnapshotKey != "" && h.Labels["snapshot_key"] != requirements.Placement.SnapshotKey {
		return false
	}
	return h.Available.Fits(requirements.Resources)
}

func (s RuntimeSelector) matches(requirements RuntimeSelector) bool {
	return matchesOptional(s.Arch, requirements.Arch) &&
		matchesOptional(s.ABI, requirements.ABI) &&
		matchesOptional(s.KernelDigest, requirements.KernelDigest) &&
		matchesOptional(s.RootfsDigest, requirements.RootfsDigest) &&
		matchesOptional(s.CNIProfile, requirements.CNIProfile)
}

func matchesOptional(value, requirement string) bool {
	return requirement == "" || value == requirement
}

func matchesLabels(labels map[string]string, requirements map[string]string) bool {
	for key, value := range requirements {
		if labels[key] != value {
			return false
		}
	}
	return true
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
	RunID           string
	ExecutionID     string
	WorkerGroupID   string
	WorkerHostID    string
	Requirements    RunRequirements
	Image           ArtifactRef
	TaskSource      ArtifactRef
	WorkspaceSource ArtifactRef
	Checkpoint      *ArtifactRef
	Secrets         []SecretRef
	Attachments     []SessionAttachment
	Traceparent     string
	DequeuedAt      time.Time
	MaxDuration     time.Duration
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
