package api

import (
	"encoding/json"
	"time"
)

type WorkspaceCreateRequest struct {
	ProjectID         string          `json:"project_id,omitempty"`
	EnvironmentID     string          `json:"environment_id,omitempty"`
	SandboxID         string          `json:"sandbox_id"`
	DeploymentID      string          `json:"deployment_id,omitempty"`
	ExternalID        string          `json:"external_id,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
	Tags              []string        `json:"tags,omitempty"`
	IdempotencyKey    string          `json:"idempotency_key,omitempty"`
	IdempotencyKeyTTL string          `json:"idempotency_key_ttl,omitempty"`
}

type WorkspacePatchRequest struct {
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Tags     []string        `json:"tags,omitempty"`
}

type WorkspaceResponse struct {
	ID                  string          `json:"id"`
	ProjectID           string          `json:"project_id"`
	EnvironmentID       string          `json:"environment_id"`
	DeploymentSandboxID string          `json:"deployment_sandbox_id"`
	SandboxID           string          `json:"sandbox_id"`
	SandboxFingerprint  string          `json:"sandbox_fingerprint"`
	ExternalID          string          `json:"external_id,omitempty"`
	CurrentVersionID    string          `json:"current_version_id,omitempty"`
	State               string          `json:"state"`
	DesiredState        string          `json:"desired_state"`
	DirtyState          string          `json:"dirty_state"`
	Metadata            json.RawMessage `json:"metadata,omitempty"`
	Tags                []string        `json:"tags,omitempty"`
	LastActivityAt      time.Time       `json:"last_activity_at"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
	ArchivedAt          *time.Time      `json:"archived_at,omitempty"`
	DeletedAt           *time.Time      `json:"deleted_at,omitempty"`
}

type WorkspaceEnvelope struct {
	Workspace WorkspaceResponse `json:"workspace"`
	IsCached  bool              `json:"is_cached,omitempty"`
}

type ListWorkspacesResponse struct {
	Workspaces []WorkspaceResponse `json:"workspaces"`
}
