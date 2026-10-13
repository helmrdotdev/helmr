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
// Its identity binds the credential digest; only delivery to its authenticated
// Host issues the secret credential. The kinds retain their own lifecycle and
// fence transitions. Cross-table allocation identity uniqueness is provided by
// server-issued UUIDv7 generation, not a relational constraint.
type Allocation struct {
	EnvironmentID    uuid.UUID
	OwnerID          uuid.UUID
	InstanceID       uuid.UUID
	Epoch            int64
	HostID           uuid.UUID
	HostEpoch        int64
	CredentialDigest []byte
	Shape            AllocationShape
	Status           string
	DeliveredAt      *time.Time
	ExpiresAt        *time.Time
	FencedAt         *time.Time
}

type ComputerAllocation struct {
	Allocation
	BaseVersion   string
	RestoredFrom  *uuid.UUID
	InitializedAt *time.Time
}
