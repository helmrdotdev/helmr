package capacityapi

import "time"

type ResourceVector struct {
	CPUMillis               int64 `json:"cpu_millis"`
	MemoryBytes             int64 `json:"memory_bytes"`
	GuestEphemeralDiskBytes int64 `json:"guest_ephemeral_disk_bytes"`
	VMSlots                 int64 `json:"vm_slots,omitempty"`
	RunConsumers            int64 `json:"run_consumers,omitempty"`
	BuildExecutors          int64 `json:"build_executors,omitempty"`
}

type RoleDemand struct {
	QueuedItems       int64          `json:"queued_items"`
	QueuedResources   ResourceVector `json:"queued_resources"`
	ReadyWorkers      int64          `json:"ready_workers"`
	AvailableCapacity ResourceVector `json:"available_capacity"`
}

type CapacityObservation struct {
	WorkerGroupID      string      `json:"worker_group_id"`
	RegionID           string      `json:"region_id"`
	GroupStatus        string      `json:"group_status"`
	Run                *RoleDemand `json:"run,omitempty"`
	Build              *RoleDemand `json:"build,omitempty"`
	RegisteringWorkers int64       `json:"registering_workers"`
	DrainingWorkers    int64       `json:"draining_workers"`
	ObservedAt         time.Time   `json:"observed_at"`
}

type CapacityObservationsResponse struct {
	Observations []CapacityObservation `json:"observations"`
}

type WorkerInstance struct {
	ID                 string     `json:"id"`
	ResourceID         string     `json:"resource_id"`
	WorkerGroupID      string     `json:"worker_group_id"`
	Status             string     `json:"status"`
	ClaimVersion       int64      `json:"claim_version"`
	CurrentEpoch       *int64     `json:"current_epoch,omitempty"`
	SupportsRun        bool       `json:"supports_run"`
	SupportsBuild      bool       `json:"supports_build"`
	DrainingAt         *time.Time `json:"draining_at,omitempty"`
	TerminationReadyAt *time.Time `json:"termination_ready_at,omitempty"`
	LostAt             *time.Time `json:"lost_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type WorkerInstancesResponse struct {
	WorkerInstances []WorkerInstance `json:"worker_instances"`
}

type DrainWorkerInstanceRequest struct {
	ExpectedEpoch        int64 `json:"expected_epoch"`
	ExpectedClaimVersion int64 `json:"expected_claim_version"`
}
