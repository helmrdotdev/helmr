package api

import (
	"context"
	"encoding/json"
	"time"
)

type WorkerWorkspacePrimitiveScope struct {
	OrgID                string `json:"org_id"`
	ProjectID            string `json:"project_id"`
	EnvironmentID        string `json:"environment_id"`
	WorkspaceID          string `json:"workspace_id"`
	WorkspaceMountID     string `json:"workspace_mount_id"`
	RuntimeInstanceToken string `json:"runtime_instance_token"`
}

type WorkerWorkspaceExecStartedRequest struct {
	WorkerWorkspacePrimitiveScope
	ExecID    string `json:"exec_id"`
	ProcessID string `json:"process_id"`
}

type WorkerWorkspaceExecExitedRequest struct {
	WorkerWorkspacePrimitiveScope
	ExecID   string          `json:"exec_id"`
	State    string          `json:"state"`
	ExitCode *int32          `json:"exit_code,omitempty"`
	Signal   string          `json:"signal,omitempty"`
	Error    json.RawMessage `json:"error,omitempty"`
}

type WorkerWorkspaceExecOutputChunk struct {
	Stream      string `json:"stream"`
	OffsetStart *int64 `json:"offset_start,omitempty"`
	Data        []byte `json:"data"`
}

type WorkerWorkspaceExecOutputRequest struct {
	WorkerWorkspacePrimitiveScope
	ExecID string                           `json:"exec_id"`
	Chunks []WorkerWorkspaceExecOutputChunk `json:"chunks"`
}

type WorkerWorkspaceExecInputRequest struct {
	WorkerWorkspacePrimitiveScope
	ExecID string `json:"exec_id"`
	Limit  int32  `json:"limit,omitempty"`
}

type WorkerWorkspaceExecInputResponse struct {
	Chunks               []WorkspaceExecStreamChunkResponse `json:"chunks"`
	StdinClosedAt        *time.Time                         `json:"stdin_closed_at,omitempty"`
	StdinCursor          int64                              `json:"stdin_cursor"`
	StdinDeliveredCursor int64                              `json:"stdin_delivered_cursor"`
	State                string                             `json:"state"`
}

type WorkerWorkspaceExecInputDeliveredRequest struct {
	WorkerWorkspacePrimitiveScope
	ExecID      string `json:"exec_id"`
	OffsetStart int64  `json:"offset_start"`
	OffsetEnd   int64  `json:"offset_end"`
}

type WorkerWorkspacePtyOpenedRequest struct {
	WorkerWorkspacePrimitiveScope
	PtyID     string `json:"pty_id"`
	ProcessID string `json:"process_id"`
}

type WorkerWorkspacePtyResizeAppliedRequest struct {
	WorkerWorkspacePrimitiveScope
	PtyID string `json:"pty_id"`
	Cols  int32  `json:"cols"`
	Rows  int32  `json:"rows"`
}

type WorkerWorkspacePtyClosedRequest struct {
	WorkerWorkspacePrimitiveScope
	PtyID  string          `json:"pty_id"`
	Error  json.RawMessage `json:"error,omitempty"`
	Reason string          `json:"reason,omitempty"`
}

type WorkerWorkspacePtyOutputChunk struct {
	OffsetStart *int64 `json:"offset_start,omitempty"`
	Data        []byte `json:"data"`
}

type WorkerWorkspacePtyOutputRequest struct {
	WorkerWorkspacePrimitiveScope
	PtyID  string                          `json:"pty_id"`
	Chunks []WorkerWorkspacePtyOutputChunk `json:"chunks"`
}

type WorkerWorkspacePtyInputRequest struct {
	WorkerWorkspacePrimitiveScope
	PtyID string `json:"pty_id"`
	Limit int32  `json:"limit,omitempty"`
}

type WorkerWorkspacePtyInputResponse struct {
	Chunks               []WorkspacePtyStreamChunkResponse `json:"chunks"`
	InputCursor          int64                             `json:"input_cursor"`
	InputDeliveredCursor int64                             `json:"input_delivered_cursor"`
	State                string                            `json:"state"`
}

type WorkerWorkspacePtyInputDeliveredRequest struct {
	WorkerWorkspacePrimitiveScope
	PtyID       string `json:"pty_id"`
	OffsetStart int64  `json:"offset_start"`
	OffsetEnd   int64  `json:"offset_end"`
}

type WorkerWorkspaceMaterializerControlClient interface {
	RenewWorkspaceMount(context.Context, WorkerWorkspaceMountRenewRequest) (WorkspaceMountResponse, error)
	MarkWorkspaceMountMounted(context.Context, WorkerWorkspaceMountMountedRequest) (WorkspaceMountResponse, error)
	CaptureWorkspaceMount(context.Context, WorkerWorkspaceMountCaptureRequest) (WorkerWorkspaceMountCaptureResponse, error)
	StopWorkspaceMount(context.Context, WorkerWorkspaceMountStopRequest) (WorkspaceMountResponse, error)
	FailWorkspaceMount(context.Context, WorkerWorkspaceMountFailRequest) (WorkspaceMountResponse, error)
	ClaimWorkspaceOperation(context.Context, WorkerWorkspaceOperationClaimRequest) (WorkerWorkspaceOperationClaimResponse, error)
	StartWorkspaceOperation(context.Context, WorkerWorkspaceOperationStartRequest) (WorkspaceOperationResponse, error)
	CompleteWorkspaceOperation(context.Context, WorkerWorkspaceOperationCompleteRequest) (WorkspaceOperationResponse, error)
	MarkWorkspaceExecStarted(context.Context, WorkerWorkspaceExecStartedRequest) (WorkspaceExecEnvelope, error)
	AppendWorkspaceExecOutput(context.Context, WorkerWorkspaceExecOutputRequest) (ListWorkspaceExecStreamChunksResponse, error)
	ListWorkspaceExecInput(context.Context, WorkerWorkspaceExecInputRequest) (WorkerWorkspaceExecInputResponse, error)
	AdvanceWorkspaceExecInputDelivered(context.Context, WorkerWorkspaceExecInputDeliveredRequest) (WorkspaceExecEnvelope, error)
	MarkWorkspaceExecExited(context.Context, WorkerWorkspaceExecExitedRequest) (WorkspaceExecEnvelope, error)
	MarkWorkspacePtyOpened(context.Context, WorkerWorkspacePtyOpenedRequest) (WorkspacePtyEnvelope, error)
	AppendWorkspacePtyOutput(context.Context, WorkerWorkspacePtyOutputRequest) (ListWorkspacePtyStreamChunksResponse, error)
	ListWorkspacePtyInput(context.Context, WorkerWorkspacePtyInputRequest) (WorkerWorkspacePtyInputResponse, error)
	AdvanceWorkspacePtyInputDelivered(context.Context, WorkerWorkspacePtyInputDeliveredRequest) (WorkspacePtyEnvelope, error)
	MarkWorkspacePtyResizeApplied(context.Context, WorkerWorkspacePtyResizeAppliedRequest) (WorkspacePtyEnvelope, error)
	MarkWorkspacePtyClosed(context.Context, WorkerWorkspacePtyClosedRequest) (WorkspacePtyEnvelope, error)
}
