package api

import (
	"encoding/json"
	"time"
)

type WorkspaceExecCreateRequest struct {
	Command        []string          `json:"command"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Detached       bool              `json:"detached,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
}

type WorkspaceExecStdinWriteRequest struct {
	Offset int64  `json:"offset"`
	Data   []byte `json:"data"`
}

type WorkspaceExecResponse struct {
	ID               string          `json:"id"`
	WorkspaceID      string          `json:"workspace_id"`
	WorkspaceMountID string          `json:"workspace_mount_id,omitempty"`
	Command          json.RawMessage `json:"command"`
	Cwd              string          `json:"cwd"`
	EnvShape         json.RawMessage `json:"env_shape,omitempty"`
	FilesystemMode   string          `json:"filesystem_mode"`
	State            string          `json:"state"`
	Detached         bool            `json:"detached"`
	ProcessID        string          `json:"process_id,omitempty"`
	ExitCode         *int32          `json:"exit_code,omitempty"`
	Signal           string          `json:"signal,omitempty"`
	Error            json.RawMessage `json:"error,omitempty"`
	StdoutCursor     int64           `json:"stdout_cursor"`
	StderrCursor     int64           `json:"stderr_cursor"`
	StdinCursor      int64           `json:"stdin_cursor"`
	StdinClosedAt    *time.Time      `json:"stdin_closed_at,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	StartedAt        *time.Time      `json:"started_at,omitempty"`
	ExitedAt         *time.Time      `json:"exited_at,omitempty"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type WorkspaceExecEnvelope struct {
	Exec     WorkspaceExecResponse `json:"exec"`
	IsCached bool                  `json:"is_cached,omitempty"`
}

type ListWorkspaceExecsResponse struct {
	Execs []WorkspaceExecResponse `json:"execs"`
}

type WorkspaceExecStreamChunkResponse struct {
	ID          string    `json:"id"`
	Stream      string    `json:"stream"`
	OffsetStart int64     `json:"offset_start"`
	OffsetEnd   int64     `json:"offset_end"`
	Data        []byte    `json:"data"`
	ObservedAt  time.Time `json:"observed_at"`
	CreatedAt   time.Time `json:"created_at"`
}

type ListWorkspaceExecStreamChunksResponse struct {
	Chunks []WorkspaceExecStreamChunkResponse `json:"chunks"`
}
