package workerapi

import (
	"github.com/helmrdotdev/helmr/internal/oci"
	"time"
)

const AllocationClosed = "allocation_closed"

// AllocationIdentity is custody evidence, never execution authority. Kind is
// computer or preparation; authentication determines the Host incarnation.
type AllocationIdentity struct {
	Kind          string `json:"kind"`
	EnvironmentID string `json:"environment_id"`
	OwnerID       string `json:"owner_id"`
	Epoch         int64  `json:"epoch"`
	InstanceID    string `json:"instance_id"`
}
type AllocationListRequest struct {
	After *AllocationIdentity `json:"after,omitempty"`
}
type AllocationListResponse struct {
	Allocations []AllocationIdentity `json:"allocations"`
}
type AllocationShape struct {
	CPUMillis       int64  `json:"cpu_millis"`
	MemoryBytes     int64  `json:"memory_bytes"`
	ScratchBytes    int64  `json:"scratch_bytes"`
	VMPlatformID    string `json:"vm_platform_id"`
	VCPUCount       int64  `json:"vcpu_count"`
	CPUConfigDigest string `json:"cpu_config_digest"`
}
type PreparationAllocationDelivery struct {
	Identity          AllocationIdentity `json:"identity"`
	Shape             AllocationShape    `json:"shape"`
	ExpiresAt         time.Time          `json:"expires_at"`
	ChannelCredential []byte             `json:"channel_credential"`
}
type ComputerAllocationDelivery struct {
	Identity          AllocationIdentity `json:"identity"`
	Shape             AllocationShape    `json:"shape"`
	ExpiresAt         time.Time          `json:"expires_at"`
	ChannelCredential string             `json:"channel_credential"`
	BaseVersion       string             `json:"base_version"`
	RestoredFrom      string             `json:"restored_from,omitempty"`
}
type ComputerAllocationReady struct {
	Identity    AllocationIdentity `json:"identity"`
	Shape       AllocationShape    `json:"shape"`
	BaseVersion string             `json:"base_version"`
}

type ProcessIdentity struct {
	SessionID string `json:"session_id"`
	Epoch     int64  `json:"epoch"`
}
type ComputerProcessesRequest struct {
	Identity AllocationIdentity `json:"identity"`
	After    *ProcessIdentity   `json:"after,omitempty"`
}
type ComputerProcessesResponse struct {
	Processes []ProcessIdentity `json:"processes"`
}

type ComputerAllocationSource struct {
	Disk         ComputerSourceMaterial `json:"disk"`
	ImageConfig  oci.RuntimeConfig      `json:"image_config"`
	RootfsDigest string                 `json:"rootfs_digest"`
}
