package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
)

var ErrNoCapacity = errors.New("no compute capacity available")

type ResourceVector struct {
	MilliCPU  int64 `json:"milli_cpu"`
	MemoryMiB int64 `json:"memory_mib"`
	DiskMiB   int64 `json:"disk_mib"`
	Slots     int32 `json:"execution_slots"`
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
	ID              string `json:"id"`
	Arch            string `json:"arch"`
	ABI             string `json:"abi"`
	KernelDigest    string `json:"kernel_digest"`
	InitramfsDigest string `json:"initramfs_digest"`
	RootfsDigest    string `json:"rootfs_digest"`
	CNIProfile      string `json:"cni_profile"`
}

const RuntimeIdentitySchema = "helmr.runtime.identity.v0"

func RuntimeIdentityDigest(runtime RuntimeSelector) (string, error) {
	payload, err := json.Marshal(struct {
		Schema          string `json:"schema"`
		Backend         string `json:"backend"`
		Arch            string `json:"arch"`
		ABI             string `json:"abi"`
		KernelDigest    string `json:"kernel_digest"`
		InitramfsDigest string `json:"initramfs_digest"`
		RootfsDigest    string `json:"rootfs_digest"`
		CNIProfile      string `json:"cni_profile"`
	}{
		Schema:          RuntimeIdentitySchema,
		Backend:         "firecracker",
		Arch:            runtime.Arch,
		ABI:             runtime.ABI,
		KernelDigest:    runtime.KernelDigest,
		InitramfsDigest: runtime.InitramfsDigest,
		RootfsDigest:    runtime.RootfsDigest,
		CNIProfile:      runtime.CNIProfile,
	})
	if err != nil {
		return "", err
	}
	return cas.DigestBytes(payload), nil
}

type NetworkPolicy struct {
	Internet bool     `json:"internet"`
	Allow    []string `json:"allow,omitempty"`
	Deny     []string `json:"deny,omitempty"`
}

func DefaultNetworkPolicy() NetworkPolicy {
	return NetworkPolicy{Internet: true}
}

func (p NetworkPolicy) Validate() error {
	var problems []error
	for _, entry := range p.Allow {
		if strings.TrimSpace(entry) == "" {
			problems = append(problems, errors.New("network allow entries must not be empty"))
		}
	}
	for _, entry := range p.Deny {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			problems = append(problems, errors.New("network deny entries must not be empty"))
			continue
		}
		if _, err := netip.ParsePrefix(entry); err != nil {
			problems = append(problems, fmt.Errorf("network deny entry %q must be a CIDR prefix: %w", entry, err))
		}
	}
	return errors.Join(problems...)
}

type Placement struct {
	Region        string            `json:"region,omitempty"`
	Tags          map[string]string `json:"tags,omitempty"`
	DedicatedKey  string            `json:"dedicated_key,omitempty"`
	SnapshotKey   string            `json:"snapshot_key,omitempty"`
	PreferWarmRun bool              `json:"prefer_warm_run,omitempty"`
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
	if r.Runtime.ID == "" {
		problems = append(problems, errors.New("runtime id is required"))
	}
	if r.Runtime.Arch == "" {
		problems = append(problems, errors.New("runtime arch is required"))
	}
	if r.Runtime.ABI == "" {
		problems = append(problems, errors.New("runtime abi is required"))
	}
	if r.Runtime.KernelDigest == "" {
		problems = append(problems, errors.New("runtime kernel digest is required"))
	}
	if r.Runtime.InitramfsDigest == "" {
		problems = append(problems, errors.New("runtime initramfs digest is required"))
	}
	if r.Runtime.RootfsDigest == "" {
		problems = append(problems, errors.New("runtime rootfs digest is required"))
	}
	if r.Runtime.CNIProfile == "" {
		problems = append(problems, errors.New("runtime cni profile is required"))
	}
	if err := r.Network.Validate(); err != nil {
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
	RunID             string
	ExecutionID       string
	WorkerInstanceID  string
	Requirements      RunRuntimeRequirements
	Image             ArtifactRef
	DeploymentSource  ArtifactRef
	WorkspaceArtifact ArtifactRef
	Checkpoint        *ArtifactRef
	Secrets           []SecretRef
	Attachments       []SessionAttachment
	Traceparent       string
	DequeuedAt        time.Time
	MaxDuration       time.Duration
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
