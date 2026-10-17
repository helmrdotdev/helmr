package workerapi

import (
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

// AgentSave identifies the durable request issued for one Computer lease.
// Host identity is supplied only by authenticated worker transport.
type AgentSave struct {
	EnvironmentID string `json:"environment_id"`
	SaveID        string `json:"save_id"`
	LeaseEpoch    int64  `json:"lease_epoch"`
}
type AgentSaveObject struct {
	Save        AgentSave                      `json:"save"`
	Inspections []blockformat.ObjectInspection `json:"inspections"`
}
type AgentSavePublication struct {
	Save     AgentSave        `json:"save"`
	Root     disk.VersionRoot `json:"root"`
	Evidence string           `json:"evidence"`
}

// AgentSaveDiscovery combines exact allocation authority with the owner's
// admission decision. Metadata-only discovery leaves pending coverage unfrozen.
type AgentSaveDiscovery struct {
	AllocationIdentity
	Admit         bool                 `json:"admit"`
	BackgroundDue bool                 `json:"background_due"`
	Retirement    *AgentSaveRetirement `json:"retirement,omitempty"`
}

// A nil Save means no live disk cut is currently available to the allocation.
type AgentSavePending struct {
	Save             *AgentSave `json:"save"`
	Sequence         int64      `json:"sequence"`
	PendingForMillis *int64     `json:"pending_for_millis,omitempty"`
}

// The worker reports presence; the server owns the elapsed pressure budget.
type AgentStoragePressure struct {
	AllocationIdentity
	Active bool `json:"active"`
}

// AgentSaveRetirement identifies an exact published and durably adopted cut.
// The server derives the monotonic floor from its immutable save sequence.
type AgentSaveRetirement struct {
	SaveID string           `json:"save_id"`
	Root   disk.VersionRoot `json:"root"`
}
