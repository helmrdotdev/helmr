package workerapi

import (
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

// ComputerSaveBeginRequest reserves one host publication. Exactly one execution
// authority is supplied: a Run lease, or an org-scoped direct-exec mount.
// The server derives the Computer, Runtime, predecessor and writer identity.
type ComputerSaveBeginRequest struct {
	Lease            *RunLeaseFence `json:"lease,omitempty"`
	OrgID            string         `json:"org_id,omitempty"`
	WorkspaceMountID string         `json:"workspace_mount_id,omitempty"`
	SaveID           string         `json:"save_id"`
	Sequence         int64          `json:"sequence"`
}

type ComputerSaveBeginResponse struct {
	RuntimeInstanceID string `json:"runtime_instance_id"`
	WorkspaceLeaseID  string `json:"workspace_lease_id"`
	PredecessorID     string `json:"predecessor_id"`
	DesiredVersion    int64  `json:"desired_version"`
	SaveID            string `json:"save_id"`
	Sequence          int64  `json:"sequence"`
}

type ComputerSaveObjectRequest struct {
	Save       ComputerSaveBeginRequest     `json:"save"`
	Inspection blockformat.ObjectInspection `json:"inspection"`
}

type ComputerSavePublicationRequest struct {
	Save ComputerSaveBeginRequest `json:"save"`
	Root computer.GenerationRoot  `json:"root"`
}

type ComputerSavePublicationResponse struct {
	ComputerID string `json:"computer_id"`
	VersionID  string `json:"version_id"`
}
