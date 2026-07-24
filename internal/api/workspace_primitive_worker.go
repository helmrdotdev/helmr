package api

import (
	"context"
	"encoding/json"
	"time"
)

type WorkerWorkspaceExecClaimRequest struct {
	OrgID            string `json:"org_id"`
	WorkspaceMountID string `json:"workspace_mount_id"`
}

type WorkerWorkspaceExecClaimResponse struct {
	Exec *WorkerWorkspaceExec `json:"exec,omitempty"`
}

type WorkerWorkspaceExec struct {
	ProcessID           string                 `json:"process_id"`
	WorkspaceID         string                 `json:"workspace_id"`
	WorkspaceMountID    string                 `json:"workspace_mount_id"`
	RequestFingerprint  string                 `json:"request_fingerprint"`
	Request             json.RawMessage        `json:"request"`
	Stdin               []byte                 `json:"stdin,omitempty"`
	Secrets             []WorkerSecretDelivery `json:"secrets"`
	WorkspaceLeaseID    string                 `json:"workspace_lease_id"`
	WriteCapability     string                 `json:"write_capability"`
	FencingGeneration   int64                  `json:"fencing_generation"`
	OwnershipGeneration int64                  `json:"ownership_generation"`
	WriterGeneration    int64                  `json:"writer_generation"`
	ExpiresAt           time.Time              `json:"expires_at"`
}

type WorkerWorkspaceExecCompleteRequest struct {
	OrgID               string          `json:"org_id"`
	ProcessID           string          `json:"process_id"`
	WorkspaceLeaseID    string          `json:"workspace_lease_id"`
	WriteCapability     string          `json:"write_capability"`
	FencingGeneration   int64           `json:"fencing_generation"`
	OwnershipGeneration int64           `json:"ownership_generation"`
	WriterGeneration    int64           `json:"writer_generation"`
	RequestFingerprint  string          `json:"request_fingerprint"`
	Outcome             string          `json:"outcome"`
	ExitCode            *int32          `json:"exit_code,omitempty"`
	Stdout              []byte          `json:"stdout,omitempty"`
	Stderr              []byte          `json:"stderr,omitempty"`
	Error               json.RawMessage `json:"error,omitempty"`
}

type WorkerWorkspaceMaterializerControlClient interface {
	RenewWorkspaceMount(context.Context, WorkerWorkspaceMountRenewRequest) (WorkspaceMountResponse, error)
	MarkWorkspaceMountMounted(context.Context, WorkerWorkspaceMountMountedRequest) (WorkspaceMountResponse, error)
	CaptureWorkspaceMount(context.Context, WorkerWorkspaceMountCaptureRequest) (WorkerWorkspaceMountCaptureResponse, error)
	StopWorkspaceMount(context.Context, WorkerWorkspaceMountStopRequest) (WorkspaceMountResponse, error)
	FailWorkspaceMount(context.Context, WorkerWorkspaceMountFailRequest) (WorkspaceMountResponse, error)
	ClaimWorkspaceExec(context.Context, WorkerWorkspaceExecClaimRequest) (WorkerWorkspaceExecClaimResponse, error)
	CompleteWorkspaceExec(context.Context, WorkerWorkspaceExecCompleteRequest) (WorkspaceMountResponse, error)
}
