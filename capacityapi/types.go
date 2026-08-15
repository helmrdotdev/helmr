package capacityapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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

type WorkerPoolStatus string

const (
	WorkerPoolStatusPending  WorkerPoolStatus = "pending"
	WorkerPoolStatusActive   WorkerPoolStatus = "active"
	WorkerPoolStatusDraining WorkerPoolStatus = "draining"
	WorkerPoolStatusDisabled WorkerPoolStatus = "disabled"
)

type ResourceVector struct {
	CPUMillis               int64 `json:"cpu_millis"`
	MemoryBytes             int64 `json:"memory_bytes"`
	GuestEphemeralDiskBytes int64 `json:"guest_ephemeral_disk_bytes"`
	VMSlots                 int64 `json:"vm_slots,omitempty"`
	BuildExecutors          int64 `json:"build_executors,omitempty"`
}

const WorkerTemplateSchema = "helmr.worker-template.v0"

const (
	RuntimeContract       = "helmr.vm-runtime.v0"
	SubstrateFormatExt4   = "ext4"
	SubstrateContractExt4 = "helmr.substrate.ext4.v0"
)

type CPUTemplateKind string

const (
	CPUTemplateNone   CPUTemplateKind = "none"
	CPUTemplateCustom CPUTemplateKind = "custom"
)

type CPUTemplateSelector struct {
	Kind   CPUTemplateKind `json:"kind"`
	Digest string          `json:"digest,omitempty"`
}

type CPUShape struct {
	VCPUCount       int32  `json:"vcpu_count"`
	CPUConfigDigest string `json:"cpu_config_digest"`
}

type RuntimeProfile struct {
	ID                        string              `json:"id"`
	Arch                      string              `json:"arch"`
	Contract                  string              `json:"contract"`
	VMRuntimeDescriptorDigest string              `json:"vm_runtime_descriptor_digest"`
	FirecrackerDigest         string              `json:"firecracker_digest"`
	FirecrackerVersion        string              `json:"firecracker_version"`
	SnapshotFormatVersion     string              `json:"snapshot_format_version"`
	HostKernelRelease         string              `json:"host_kernel_release"`
	CPUTemplate               CPUTemplateSelector `json:"cpu_template"`
	KernelDigest              string              `json:"kernel_digest"`
	InitramfsDigest           string              `json:"initramfs_digest"`
	RootfsDigest              string              `json:"rootfs_digest"`
}

func (p RuntimeProfile) ExpectedID() (string, error) {
	if err := p.validateSelector(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		Domain                    string              `json:"domain"`
		Backend                   string              `json:"backend"`
		Arch                      string              `json:"arch"`
		Contract                  string              `json:"contract"`
		VMRuntimeDescriptorDigest string              `json:"vm_runtime_descriptor_digest"`
		FirecrackerDigest         string              `json:"firecracker_digest"`
		FirecrackerVersion        string              `json:"firecracker_version"`
		SnapshotFormatVersion     string              `json:"snapshot_format_version"`
		HostKernelRelease         string              `json:"host_kernel_release"`
		CPUTemplate               CPUTemplateSelector `json:"cpu_template"`
		KernelDigest              string              `json:"kernel_digest"`
		InitramfsDigest           string              `json:"initramfs_digest"`
		RootfsDigest              string              `json:"rootfs_digest"`
	}{
		Domain: "helmr.vm-runtime-identity.v0", Backend: "firecracker",
		Arch: p.Arch, Contract: p.Contract,
		VMRuntimeDescriptorDigest: p.VMRuntimeDescriptorDigest,
		FirecrackerDigest:         p.FirecrackerDigest, FirecrackerVersion: p.FirecrackerVersion,
		SnapshotFormatVersion: p.SnapshotFormatVersion, HostKernelRelease: p.HostKernelRelease,
		CPUTemplate: p.CPUTemplate, KernelDigest: p.KernelDigest,
		InitramfsDigest: p.InitramfsDigest, RootfsDigest: p.RootfsDigest,
	})
	if err != nil {
		return "", err
	}
	return sha256Digest(payload), nil
}

func (p RuntimeProfile) Validate() error {
	expected, err := p.ExpectedID()
	if err != nil {
		return err
	}
	if p.ID != expected {
		return errors.New("runtime.id does not match the canonical runtime selector")
	}
	return nil
}

func (p RuntimeProfile) validateSelector() error {
	var problems []error
	if p.Arch != "x86_64" || p.Contract != RuntimeContract {
		problems = append(problems, errors.New("runtime architecture or contract is not supported"))
	}
	for _, field := range []struct{ name, value string }{
		{name: "vm_runtime_descriptor_digest", value: p.VMRuntimeDescriptorDigest},
		{name: "firecracker_digest", value: p.FirecrackerDigest},
		{name: "kernel_digest", value: p.KernelDigest},
		{name: "initramfs_digest", value: p.InitramfsDigest},
		{name: "rootfs_digest", value: p.RootfsDigest},
	} {
		if !validSHA256Digest(field.value) {
			problems = append(problems, fmt.Errorf("runtime.%s must be a canonical SHA-256 digest", field.name))
		}
	}
	if !validSemanticVersion(p.FirecrackerVersion) {
		problems = append(problems, errors.New("runtime.firecracker_version must be a canonical semantic version"))
	}
	if !validSemanticVersion(p.SnapshotFormatVersion) {
		problems = append(problems, errors.New("runtime.snapshot_format_version must be a canonical semantic version"))
	}
	if p.HostKernelRelease == "" || strings.TrimSpace(p.HostKernelRelease) != p.HostKernelRelease || len(p.HostKernelRelease) > 255 {
		problems = append(problems, errors.New("runtime.host_kernel_release must be a non-empty canonical release string"))
	}
	switch p.CPUTemplate.Kind {
	case CPUTemplateNone:
		if p.CPUTemplate.Digest != "" {
			problems = append(problems, errors.New("runtime.cpu_template.digest must be empty for kind none"))
		}
	case CPUTemplateCustom:
		if !validSHA256Digest(p.CPUTemplate.Digest) {
			problems = append(problems, errors.New("runtime.cpu_template.digest must be a canonical SHA-256 digest for kind custom"))
		}
	default:
		problems = append(problems, errors.New("runtime.cpu_template.kind must be none or custom"))
	}
	return errors.Join(problems...)
}

type SubstrateProfile struct {
	Format   string `json:"format,omitempty"`
	Contract string `json:"contract,omitempty"`
}

type WorkerTemplate struct {
	Schema        string           `json:"schema"`
	SupportsRun   bool             `json:"supports_run"`
	SupportsBuild bool             `json:"supports_build"`
	Runtime       RuntimeProfile   `json:"runtime"`
	CPUShapes     []CPUShape       `json:"cpu_shapes"`
	Substrate     SubstrateProfile `json:"substrate"`
	Capacity      ResourceVector   `json:"capacity"`
	PerVM         ResourceVector   `json:"per_vm"`
}

func (t WorkerTemplate) Validate() error {
	var problems []error
	if t.Schema != WorkerTemplateSchema {
		problems = append(problems, fmt.Errorf("schema must be %q", WorkerTemplateSchema))
	}
	if !t.SupportsRun && !t.SupportsBuild {
		problems = append(problems, errors.New("at least one Worker role is required"))
	}
	if err := t.Runtime.Validate(); err != nil {
		problems = append(problems, err)
	}
	if t.SupportsRun && (t.Substrate.Format != SubstrateFormatExt4 || t.Substrate.Contract != SubstrateContractExt4) {
		problems = append(problems, errors.New("run Worker substrate format or contract is not supported"))
	}
	if !t.SupportsRun && (t.Substrate.Format != "" || t.Substrate.Contract != "") {
		problems = append(problems, errors.New("build-only Workers must not declare a run substrate"))
	}
	for _, resources := range []struct {
		name   string
		vector ResourceVector
	}{{name: "capacity", vector: t.Capacity}, {name: "per_vm", vector: t.PerVM}} {
		if resources.vector.CPUMillis < 0 || resources.vector.MemoryBytes < 0 || resources.vector.GuestEphemeralDiskBytes < 0 ||
			resources.vector.VMSlots < 0 || resources.vector.BuildExecutors < 0 {
			problems = append(problems, fmt.Errorf("%s resource dimensions must not be negative", resources.name))
		}
	}
	if t.Capacity.CPUMillis <= 0 || t.Capacity.MemoryBytes <= 0 || t.Capacity.GuestEphemeralDiskBytes <= 0 {
		problems = append(problems, errors.New("capacity CPU, memory, and guest disk must be positive"))
	}
	if t.PerVM.CPUMillis <= 0 || t.PerVM.MemoryBytes <= 0 || t.PerVM.GuestEphemeralDiskBytes <= 0 {
		problems = append(problems, errors.New("per_vm CPU, memory, and guest disk must be positive"))
	}
	if t.PerVM.CPUMillis > t.Capacity.CPUMillis || t.PerVM.MemoryBytes > t.Capacity.MemoryBytes ||
		t.PerVM.GuestEphemeralDiskBytes > t.Capacity.GuestEphemeralDiskBytes {
		problems = append(problems, errors.New("per_vm resources must fit within aggregate capacity"))
	}
	maxVCPUs := int64(0)
	if t.PerVM.CPUMillis > 0 {
		maxVCPUs = (t.PerVM.CPUMillis-1)/1000 + 1
	}
	if maxVCPUs <= 0 || maxVCPUs > int64(^uint32(0)>>1) {
		problems = append(problems, errors.New("per_vm CPU does not define a supported vCPU range"))
	} else if len(t.CPUShapes) != int(maxVCPUs) {
		problems = append(problems, fmt.Errorf("cpu_shapes must contain exactly %d entries", maxVCPUs))
	} else {
		for index, shape := range t.CPUShapes {
			want := int32(index + 1)
			if shape.VCPUCount != want {
				problems = append(problems, fmt.Errorf("cpu_shapes[%d].vcpu_count must be %d", index, want))
			}
			if !validSHA256Digest(shape.CPUConfigDigest) {
				problems = append(problems, fmt.Errorf("cpu_shapes[%d].cpu_config_digest must be a canonical SHA-256 digest", index))
			}
		}
	}
	if t.SupportsRun && t.Capacity.VMSlots <= 0 {
		problems = append(problems, errors.New("run Workers require positive VM slots"))
	}
	if !t.SupportsRun && t.Capacity.VMSlots != 0 {
		problems = append(problems, errors.New("build-only Workers must not declare run capacity"))
	}
	if t.SupportsBuild && t.Capacity.BuildExecutors != 1 {
		problems = append(problems, errors.New("build Workers require exactly one build executor"))
	}
	if !t.SupportsBuild && t.Capacity.BuildExecutors != 0 {
		problems = append(problems, errors.New("run-only Workers must not declare build executors"))
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

func validSemanticVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return false
		}
		if _, err := strconv.ParseUint(part, 10, 32); err != nil {
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
	Pools []CapacityPoolRequest `json:"pools"`
}

type CapacityPoolRequest struct {
	PoolID               string `json:"pool_id"`
	MaxAdditionalWorkers int32  `json:"max_additional_workers"`
}

type CapacityWorkerGroup struct {
	ID                 string            `json:"id"`
	Name               string            `json:"name"`
	RegionID           string            `json:"region_id"`
	Status             WorkerGroupStatus `json:"status"`
	ClaimVersion       int64             `json:"claim_version"`
	AllowsRun          bool              `json:"allows_run"`
	AllowsBuild        bool              `json:"allows_build"`
	PrimaryRunPoolID   string            `json:"primary_run_pool_id,omitempty"`
	PrimaryBuildPoolID string            `json:"primary_build_pool_id,omitempty"`
}

type ReconcileWorkerGroupPrimaryPoolsRequest struct {
	ExpectedGroupClaimVersion int64  `json:"expected_group_claim_version"`
	RunPoolID                 string `json:"run_pool_id,omitempty"`
	BuildPoolID               string `json:"build_pool_id,omitempty"`
}

type ReconcileWorkerGroupPrimaryPoolsResponse struct {
	WorkerGroup CapacityWorkerGroup `json:"worker_group"`
	Applied     bool                `json:"applied"`
}

type CapacityWorkerPool struct {
	ID            string           `json:"id"`
	WorkerGroupID string           `json:"worker_group_id"`
	Name          string           `json:"name"`
	Status        WorkerPoolStatus `json:"status"`
	AllowsRun     bool             `json:"allows_run"`
	AllowsBuild   bool             `json:"allows_build"`
}

type CapacityIncompatibility struct {
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

type CapacityPlanResponse struct {
	WorkerGroupID   string                    `json:"worker_group_id"`
	WorkerGroupName string                    `json:"worker_group_name"`
	RegionID        string                    `json:"region_id"`
	GroupStatus     WorkerGroupStatus         `json:"group_status"`
	Pools           []CapacityPoolPlan        `json:"pools"`
	UnmatchedDemand []CapacityIncompatibility `json:"unmatched_demand"`
	Complete        bool                      `json:"complete"`
	ComputedAt      time.Time                 `json:"computed_at"`
}

type CapacityPoolPlan struct {
	PoolID                       string `json:"pool_id"`
	PoolName                     string `json:"pool_name"`
	RecommendedAdditionalWorkers int32  `json:"recommended_additional_workers"`
	CompatibleQueuedItems        int64  `json:"compatible_queued_items"`
	RegisteringWorkers           int64  `json:"registering_workers"`
	ActiveWorkers                int64  `json:"active_workers"`
	Complete                     bool   `json:"complete"`
	Saturated                    bool   `json:"saturated"`
	ScaleInBlocked               bool   `json:"scale_in_blocked"`
}

type WorkerInstance struct {
	ID                 string               `json:"id"`
	ResourceID         string               `json:"resource_id"`
	WorkerGroupID      string               `json:"worker_group_id"`
	WorkerPoolID       string               `json:"worker_pool_id"`
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
