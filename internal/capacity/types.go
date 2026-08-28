package capacity

import (
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/runtimeid"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
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
}

const WorkerTemplateSchema = "helmr.worker-template.v0"

const (
	SubstrateFormatExt4   = "ext4"
	SubstrateContractExt4 = "helmr.substrate.ext4.v0"
)

type SubstrateProfile struct {
	Format   string `json:"format,omitempty"`
	Contract string `json:"contract,omitempty"`
}

type WorkerTemplate struct {
	Schema    string               `json:"schema"`
	Runtime   runtimeid.Profile    `json:"runtime"`
	CPUShapes []runtimeid.CPUShape `json:"cpu_shapes"`
	Substrate SubstrateProfile     `json:"substrate"`
	Capacity  ResourceVector       `json:"capacity"`
	PerVM     ResourceVector       `json:"per_vm"`
}

func (t WorkerTemplate) Validate() error {
	var problems []error
	if t.Schema != WorkerTemplateSchema {
		problems = append(problems, fmt.Errorf("schema must be %q", WorkerTemplateSchema))
	}
	if err := t.Runtime.Validate(); err != nil {
		problems = append(problems, err)
	}
	if t.Substrate.Format != SubstrateFormatExt4 || t.Substrate.Contract != SubstrateContractExt4 {
		problems = append(problems, errors.New("run Worker substrate format or contract is not supported"))
	}
	for _, resources := range []struct {
		name   string
		vector ResourceVector
	}{{name: "capacity", vector: t.Capacity}, {name: "per_vm", vector: t.PerVM}} {
		if resources.vector.CPUMillis < 0 || resources.vector.MemoryBytes < 0 || resources.vector.GuestEphemeralDiskBytes < 0 ||
			resources.vector.VMSlots < 0 {
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
			if !sha256sum.ValidDigest(shape.CPUConfigDigest) {
				problems = append(problems, fmt.Errorf("cpu_shapes[%d].cpu_config_digest must be a canonical SHA-256 digest", index))
			}
		}
	}
	if t.Capacity.VMSlots <= 0 {
		problems = append(problems, errors.New("run Workers require positive VM slots"))
	}
	return errors.Join(problems...)
}

type PlanRequest struct {
	Pools []PoolRequest `json:"pools"`
}

type PoolRequest struct {
	PoolID               string `json:"pool_id"`
	MaxAdditionalWorkers int32  `json:"max_additional_workers"`
}

type WorkerGroup struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	RegionID      string            `json:"region_id"`
	Status        WorkerGroupStatus `json:"status"`
	ClaimVersion  int64             `json:"claim_version"`
	PrimaryPoolID string            `json:"primary_pool_id,omitempty"`
}

type ReconcileWorkerGroupPrimaryPoolsRequest struct {
	ExpectedGroupClaimVersion int64  `json:"expected_group_claim_version"`
	PoolID                    string `json:"pool_id"`
}

type ReconcileWorkerGroupPrimaryPoolsResponse struct {
	WorkerGroup WorkerGroup `json:"worker_group"`
	Applied     bool        `json:"applied"`
}

type WorkerPool struct {
	ID            string           `json:"id"`
	WorkerGroupID string           `json:"worker_group_id"`
	Name          string           `json:"name"`
	Status        WorkerPoolStatus `json:"status"`
}

type Incompatibility struct {
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

type PlanResponse struct {
	WorkerGroupID   string            `json:"worker_group_id"`
	WorkerGroupName string            `json:"worker_group_name"`
	RegionID        string            `json:"region_id"`
	GroupStatus     WorkerGroupStatus `json:"group_status"`
	Pools           []PoolPlan        `json:"pools"`
	UnmatchedDemand []Incompatibility `json:"unmatched_demand"`
	Complete        bool              `json:"complete"`
	ComputedAt      time.Time         `json:"computed_at"`
}

type PoolPlan struct {
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
	DrainingAt         *time.Time           `json:"draining_at,omitempty"`
	TerminationReadyAt *time.Time           `json:"termination_ready_at,omitempty"`
	LostAt             *time.Time           `json:"lost_at,omitempty"`
	CreatedAt          time.Time            `json:"created_at"`
	UpdatedAt          time.Time            `json:"updated_at"`
}

type ListWorkerInstancesResponse struct {
	WorkerInstances []WorkerInstance `json:"worker_instances"`
}

type DrainWorkerInstanceRequest struct {
	ExpectedEpoch           int64 `json:"expected_epoch"`
	ExpectedClaimVersion    int64 `json:"expected_claim_version"`
	RequireZeroQueuedDemand bool  `json:"require_zero_queued_demand,omitempty"`
}
