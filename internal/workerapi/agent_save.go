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
	Save       AgentSave                    `json:"save"`
	Inspection blockformat.ObjectInspection `json:"inspection"`
}
type AgentSavePublication struct {
	Save     AgentSave        `json:"save"`
	Root     disk.VersionRoot `json:"root"`
	Evidence string           `json:"evidence"`
}

// AgentSaveDiscovery combines exact allocation authority with the owner's
// periodic trigger. Existing requests always take priority over optional admission.
type AgentSaveDiscovery struct {
	AllocationIdentity
	BackgroundDue bool `json:"background_due"`
}

// A nil Save means no live disk cut is currently available to the allocation.
type AgentSavePending struct {
	Save     *AgentSave `json:"save"`
	Sequence int64      `json:"sequence"`
}
