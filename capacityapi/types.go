package capacityapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type WorkerGroupStatus string

const (
	WorkerGroupStatusActive   WorkerGroupStatus = "active"
	WorkerGroupStatusPaused   WorkerGroupStatus = "paused"
	WorkerGroupStatusDraining WorkerGroupStatus = "draining"
	WorkerGroupStatusDisabled WorkerGroupStatus = "disabled"
)

type WorkerInstanceStatus string

const (
	WorkerInstanceStatusRegistering      WorkerInstanceStatus = "registering"
	WorkerInstanceStatusActive           WorkerInstanceStatus = "active"
	WorkerInstanceStatusDraining         WorkerInstanceStatus = "draining"
	WorkerInstanceStatusTerminationReady WorkerInstanceStatus = "termination_ready"
	WorkerInstanceStatusLost             WorkerInstanceStatus = "lost"
)

type ResourceVector struct {
	CPUMillis               int64 `json:"cpu_millis"`
	MemoryBytes             int64 `json:"memory_bytes"`
	GuestEphemeralDiskBytes int64 `json:"guest_ephemeral_disk_bytes"`
	VMSlots                 int64 `json:"vm_slots,omitempty"`
	BuildExecutors          int64 `json:"build_executors,omitempty"`
}

const WorkerReleaseManifestSchema = "helmr.worker-release.v0"

var workerVersionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

const (
	RuntimeContract       = "helmr.vm-runtime.v0"
	SubstrateFormatExt4   = "ext4"
	SubstrateContractExt4 = "helmr.substrate.ext4.v0"
)

type RuntimeProfile struct {
	ID              string `json:"id"`
	Arch            string `json:"arch"`
	Contract        string `json:"contract"`
	KernelDigest    string `json:"kernel_digest"`
	InitramfsDigest string `json:"initramfs_digest"`
	RootfsDigest    string `json:"rootfs_digest"`
}

func (p RuntimeProfile) ExpectedID() (string, error) {
	payload, err := json.Marshal(struct {
		Domain          string `json:"domain"`
		Backend         string `json:"backend"`
		Arch            string `json:"arch"`
		Contract        string `json:"contract"`
		KernelDigest    string `json:"kernel_digest"`
		InitramfsDigest string `json:"initramfs_digest"`
		RootfsDigest    string `json:"rootfs_digest"`
	}{
		Domain: "helmr.vm-runtime-identity.v0", Backend: "firecracker",
		Arch: p.Arch, Contract: p.Contract, KernelDigest: p.KernelDigest,
		InitramfsDigest: p.InitramfsDigest, RootfsDigest: p.RootfsDigest,
	})
	if err != nil {
		return "", err
	}
	return sha256Digest(payload), nil
}

type SubstrateProfile struct {
	Format   string `json:"format,omitempty"`
	Contract string `json:"contract,omitempty"`
}

type WorkerReleaseManifest struct {
	Schema             string           `json:"schema"`
	ReleaseFingerprint string           `json:"release_fingerprint"`
	WorkerVersion      string           `json:"worker_version"`
	SupportsRun        bool             `json:"supports_run"`
	SupportsBuild      bool             `json:"supports_build"`
	Runtime            RuntimeProfile   `json:"runtime"`
	Substrate          SubstrateProfile `json:"substrate"`
	Capacity           ResourceVector   `json:"capacity"`
	PerVM              ResourceVector   `json:"per_vm"`
	BuildCacheBytes    int64            `json:"build_cache_bytes"`
	ArtifactCacheBytes int64            `json:"artifact_cache_bytes"`
}

func (m WorkerReleaseManifest) ExpectedFingerprint() (string, error) {
	m.ReleaseFingerprint = ""
	payload, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (m WorkerReleaseManifest) Validate() error {
	var problems []error
	if m.Schema != WorkerReleaseManifestSchema {
		problems = append(problems, fmt.Errorf("schema must be %q", WorkerReleaseManifestSchema))
	}
	if !workerVersionPattern.MatchString(m.WorkerVersion) {
		problems = append(problems, errors.New("worker_version must be an exact lowercase 40-character Git commit"))
	}
	if !m.SupportsRun && !m.SupportsBuild {
		problems = append(problems, errors.New("at least one Worker role is required"))
	}
	if m.Runtime.Arch != "x86_64" || m.Runtime.Contract != RuntimeContract {
		problems = append(problems, errors.New("runtime architecture or contract is not supported"))
	}
	for _, field := range []struct{ name, value string }{
		{name: "runtime.kernel_digest", value: m.Runtime.KernelDigest},
		{name: "runtime.initramfs_digest", value: m.Runtime.InitramfsDigest},
		{name: "runtime.rootfs_digest", value: m.Runtime.RootfsDigest},
	} {
		if !validSHA256Digest(field.value) {
			problems = append(problems, fmt.Errorf("%s must be a canonical SHA-256 digest", field.name))
		}
	}
	expectedRuntimeID, runtimeErr := m.Runtime.ExpectedID()
	if runtimeErr != nil {
		problems = append(problems, fmt.Errorf("compute runtime identity: %w", runtimeErr))
	} else if m.Runtime.ID != expectedRuntimeID {
		problems = append(problems, errors.New("runtime.id does not match the canonical runtime selector"))
	}
	if m.SupportsRun && (m.Substrate.Format != SubstrateFormatExt4 || m.Substrate.Contract != SubstrateContractExt4) {
		problems = append(problems, errors.New("run Worker substrate format or contract is not supported"))
	}
	if !m.SupportsRun && (m.Substrate.Format != "" || m.Substrate.Contract != "") {
		problems = append(problems, errors.New("build-only Workers must not declare a run substrate"))
	}
	for _, resources := range []struct {
		name   string
		vector ResourceVector
	}{{name: "capacity", vector: m.Capacity}, {name: "per_vm", vector: m.PerVM}} {
		if resources.vector.CPUMillis < 0 || resources.vector.MemoryBytes < 0 || resources.vector.GuestEphemeralDiskBytes < 0 ||
			resources.vector.VMSlots < 0 || resources.vector.BuildExecutors < 0 {
			problems = append(problems, fmt.Errorf("%s resource dimensions must not be negative", resources.name))
		}
	}
	if m.Capacity.CPUMillis <= 0 || m.Capacity.MemoryBytes <= 0 || m.Capacity.GuestEphemeralDiskBytes <= 0 {
		problems = append(problems, errors.New("capacity CPU, memory, and guest disk must be positive"))
	}
	if m.PerVM.CPUMillis <= 0 || m.PerVM.MemoryBytes <= 0 || m.PerVM.GuestEphemeralDiskBytes <= 0 {
		problems = append(problems, errors.New("per_vm CPU, memory, and guest disk must be positive"))
	}
	if m.PerVM.CPUMillis > m.Capacity.CPUMillis || m.PerVM.MemoryBytes > m.Capacity.MemoryBytes ||
		m.PerVM.GuestEphemeralDiskBytes > m.Capacity.GuestEphemeralDiskBytes {
		problems = append(problems, errors.New("per_vm resources must fit within aggregate capacity"))
	}
	if m.SupportsRun && m.Capacity.VMSlots <= 0 {
		problems = append(problems, errors.New("run Workers require positive VM slots"))
	}
	if !m.SupportsRun && m.Capacity.VMSlots != 0 {
		problems = append(problems, errors.New("build-only Workers must not declare run capacity"))
	}
	if m.SupportsBuild && m.Capacity.BuildExecutors != 1 {
		problems = append(problems, errors.New("build Workers require exactly one build executor"))
	}
	if !m.SupportsBuild && m.Capacity.BuildExecutors != 0 {
		problems = append(problems, errors.New("run-only Workers must not declare build executors"))
	}
	if m.BuildCacheBytes < 0 || m.ArtifactCacheBytes < 0 {
		problems = append(problems, errors.New("cache capacities must not be negative"))
	}
	expected, err := m.ExpectedFingerprint()
	if err != nil {
		problems = append(problems, fmt.Errorf("compute release fingerprint: %w", err))
	} else if m.ReleaseFingerprint != expected {
		problems = append(problems, errors.New("release_fingerprint does not match the canonical manifest"))
	}
	return errors.Join(problems...)
}

func validSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func sha256Digest(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}

type CapacityPlanRequest struct {
	Worker               WorkerReleaseManifest `json:"worker"`
	MaxAdditionalWorkers int32                 `json:"max_additional_workers"`
}

type CapacityWorkerGroup struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	RegionID string            `json:"region_id"`
	Status   WorkerGroupStatus `json:"status"`
}

type CapacityIncompatibility struct {
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

type CapacityPlanResponse struct {
	WorkerGroupID                string                    `json:"worker_group_id"`
	WorkerGroupName              string                    `json:"worker_group_name"`
	RegionID                     string                    `json:"region_id"`
	GroupStatus                  WorkerGroupStatus         `json:"group_status"`
	ReleaseFingerprint           string                    `json:"release_fingerprint"`
	RecommendedAdditionalWorkers int32                     `json:"recommended_additional_workers"`
	CompatibleQueuedItems        int64                     `json:"compatible_queued_items"`
	IncompatibleQueuedItems      int64                     `json:"incompatible_queued_items"`
	Incompatibilities            []CapacityIncompatibility `json:"incompatibilities"`
	Complete                     bool                      `json:"complete"`
	Saturated                    bool                      `json:"saturated"`
	ScaleInBlocked               bool                      `json:"scale_in_blocked"`
	ComputedAt                   time.Time                 `json:"computed_at"`
}

type WorkerInstance struct {
	ID                 string               `json:"id"`
	ResourceID         string               `json:"resource_id"`
	WorkerGroupID      string               `json:"worker_group_id"`
	Status             WorkerInstanceStatus `json:"status"`
	ClaimVersion       int64                `json:"claim_version"`
	CurrentEpoch       *int64               `json:"current_epoch,omitempty"`
	SupportsRun        bool                 `json:"supports_run"`
	SupportsBuild      bool                 `json:"supports_build"`
	DrainingAt         *time.Time           `json:"draining_at,omitempty"`
	TerminationReadyAt *time.Time           `json:"termination_ready_at,omitempty"`
	LostAt             *time.Time           `json:"lost_at,omitempty"`
	CreatedAt          time.Time            `json:"created_at"`
	UpdatedAt          time.Time            `json:"updated_at"`
}

type WorkerInstancesResponse struct {
	WorkerInstances []WorkerInstance `json:"worker_instances"`
}

type DrainWorkerInstanceRequest struct {
	ExpectedEpoch        int64 `json:"expected_epoch"`
	ExpectedClaimVersion int64 `json:"expected_claim_version"`
}
