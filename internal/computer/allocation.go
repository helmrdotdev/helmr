package computer

import (
	"time"
	"uuid"
)

// AllocationShape is the immutable physical reservation and materialization
// profile selected from registered resources and qualified worker supply.
type AllocationShape struct {
	CPUMillis       int64
	MemoryBytes     int64
	ScratchBytes    int64
	VMPlatformID    string
	VCPUCount       int64
	CPUConfigDigest string
}

// Allocation is a durable placement receipt, not permission to start a VM.
// Only delivery to its authenticated Host can issue the channel credential.
type Allocation struct {
	EnvironmentID uuid.UUID
	OwnerID       uuid.UUID
	InstanceID    uuid.UUID
	Epoch         int64
	HostID        uuid.UUID
	HostEpoch     int64
	Shape         AllocationShape
	Status        string
	DeliveredAt   *time.Time
	ExpiresAt     *time.Time
	FencedAt      *time.Time
}

type ComputerAllocation struct {
	Allocation
	BaseVersion   string
	RestoredFrom  *uuid.UUID
	InitializedAt *time.Time
}
