package workerapi

import (
	"context"
	"encoding/json"
	"time"
)

type WorkspaceExecClaimRequest struct {
	OrgID            string `json:"org_id"`
	WorkspaceMountID string `json:"workspace_mount_id"`
}

type WorkspaceExecClaimResponse struct {
	Exec *WorkspaceExec `json:"exec,omitempty"`
}

type WorkspaceExec struct {
	ProcessID           string           `json:"process_id"`
	WorkspaceID         string           `json:"workspace_id"`
	WorkspaceMountID    string           `json:"workspace_mount_id"`
	RequestFingerprint  string           `json:"request_fingerprint"`
	Request             json.RawMessage  `json:"request"`
	Stdin               []byte           `json:"stdin,omitempty"`
	Secrets             []SecretDelivery `json:"secrets"`
	WorkspaceLeaseID    string           `json:"workspace_lease_id"`
	WriteCapability     string           `json:"write_capability"`
	FencingGeneration   int64            `json:"fencing_generation"`
	OwnershipGeneration int64            `json:"ownership_generation"`
	WriterGeneration    int64            `json:"writer_generation"`
	ExpiresAt           time.Time        `json:"expires_at"`
}

type WorkspaceExecCompleteRequest struct {
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

type WorkspaceMaterializerControlClient interface {
	RenewWorkspaceMount(context.Context, WorkspaceMountRenewRequest) (WorkspaceMountResponse, error)
	MarkWorkspaceMountMounted(context.Context, WorkspaceMountMountedRequest) (WorkspaceMountResponse, error)
	CaptureWorkspaceMount(context.Context, WorkspaceMountCaptureRequest) (WorkspaceMountCaptureResponse, error)
	StopWorkspaceMount(context.Context, WorkspaceMountStopRequest) (WorkspaceMountResponse, error)
	FailWorkspaceMount(context.Context, WorkspaceMountFailRequest) (WorkspaceMountResponse, error)
	ClaimWorkspaceExec(context.Context, WorkspaceExecClaimRequest) (WorkspaceExecClaimResponse, error)
	CompleteWorkspaceExec(context.Context, WorkspaceExecCompleteRequest) (WorkspaceMountResponse, error)
}
