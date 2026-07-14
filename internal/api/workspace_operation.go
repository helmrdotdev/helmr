package api

import (
	"encoding/json"
	"time"
)

type WorkspaceOperationResponse struct {
	ID                 string          `json:"id"`
	OrgID              string          `json:"org_id,omitempty"`
	ProjectID          string          `json:"project_id"`
	EnvironmentID      string          `json:"environment_id"`
	WorkspaceID        string          `json:"workspace_id"`
	WorkspaceMountID   string          `json:"workspace_mount_id"`
	OperationKind      string          `json:"operation_kind"`
	ResourceKind       string          `json:"resource_kind,omitempty"`
	ResourceID         string          `json:"resource_id,omitempty"`
	RequestFingerprint string          `json:"request_fingerprint"`
	OperationExpiresAt time.Time       `json:"operation_expires_at"`
	State              string          `json:"state"`
	Priority           int32           `json:"priority"`
	InstanceLeaseID    string          `json:"instance_lease_id,omitempty"`
	WriteLeaseID       string          `json:"write_lease_id,omitempty"`
	FencingToken       string          `json:"fencing_token,omitempty"`
	FencingGeneration  int64           `json:"fencing_generation"`
	Request            json.RawMessage `json:"request,omitempty"`
	Result             json.RawMessage `json:"result,omitempty"`
	Error              json.RawMessage `json:"error,omitempty"`
	RequestedAt        time.Time       `json:"requested_at"`
	ClaimedAt          *time.Time      `json:"claimed_at,omitempty"`
	CompletedAt        *time.Time      `json:"completed_at,omitempty"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

type WorkerWorkspaceOperation struct {
	WorkspaceOperationResponse
	ClaimedByWorkerInstanceID string     `json:"claimed_by_worker_instance_id"`
	ClaimToken                string     `json:"claim_token"`
	ClaimExpiresAt            *time.Time `json:"claim_expires_at,omitempty"`
}

type WorkerWorkspaceOperationClaimRequest struct {
	OrgID                 string `json:"org_id"`
	WorkspaceMountID      string `json:"workspace_mount_id"`
	ClaimExpiresInSeconds int32  `json:"claim_expires_in_seconds,omitempty"`
}

type WorkerWorkspaceOperationClaimResponse struct {
	Operation *WorkerWorkspaceOperation `json:"operation,omitempty"`
}

type WorkerWorkspaceOperationStartRequest struct {
	OrgID       string `json:"org_id"`
	OperationID string `json:"operation_id"`
	ClaimToken  string `json:"claim_token"`
}

type WorkerWorkspaceOperationCompleteRequest struct {
	OrgID       string          `json:"org_id"`
	OperationID string          `json:"operation_id"`
	ClaimToken  string          `json:"claim_token"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       json.RawMessage `json:"error,omitempty"`
}
