package api

import "time"

type OperatorResourceVector struct {
	CPUMillis               int64 `json:"cpu_millis"`
	MemoryBytes             int64 `json:"memory_bytes"`
	GuestEphemeralDiskBytes int64 `json:"guest_ephemeral_disk_bytes"`
	VMSlots                 int64 `json:"vm_slots,omitempty"`
	RunConsumers            int64 `json:"run_consumers,omitempty"`
	BuildExecutors          int64 `json:"build_executors,omitempty"`
}

type OperatorRoleDemand struct {
	QueuedItems       int64                  `json:"queued_items"`
	QueuedResources   OperatorResourceVector `json:"queued_resources"`
	ReadyWorkers      int64                  `json:"ready_workers"`
	AvailableCapacity OperatorResourceVector `json:"available_capacity"`
}

type OperatorCapacityObservation struct {
	WorkerGroupID      string              `json:"worker_group_id"`
	RegionID           string              `json:"region_id"`
	GroupState         string              `json:"group_state"`
	Run                *OperatorRoleDemand `json:"run,omitempty"`
	Build              *OperatorRoleDemand `json:"build,omitempty"`
	RegisteringWorkers int64               `json:"registering_workers"`
	DrainingWorkers    int64               `json:"draining_workers"`
	ObservedAt         time.Time           `json:"observed_at"`
}

type OperatorCapacityObservationsResponse struct {
	Observations []OperatorCapacityObservation `json:"observations"`
}

type OperatorWorkerInstance struct {
	ID                 string     `json:"id"`
	ResourceID         string     `json:"resource_id"`
	WorkerGroupID      string     `json:"worker_group_id"`
	State              string     `json:"state"`
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

type OperatorWorkerInstancesResponse struct {
	WorkerInstances []OperatorWorkerInstance `json:"worker_instances"`
}

type OperatorDrainWorkerInstanceRequest struct {
	ExpectedEpoch        int64 `json:"expected_epoch"`
	ExpectedClaimVersion int64 `json:"expected_claim_version"`
}
