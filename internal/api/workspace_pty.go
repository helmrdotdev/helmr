package api

import (
	"encoding/json"
	"time"
)

type WorkspacePtyCreateRequest struct {
	Cwd            string `json:"cwd,omitempty"`
	Cols           int32  `json:"cols"`
	Rows           int32  `json:"rows"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type WorkspacePtyResizeRequest struct {
	Cols int32 `json:"cols"`
	Rows int32 `json:"rows"`
}

type WorkspacePtyInputWriteRequest struct {
	Offset int64  `json:"offset"`
	Data   []byte `json:"data"`
}

type WorkspacePtyResponse struct {
	ID               string          `json:"id"`
	WorkspaceID      string          `json:"workspace_id"`
	WorkspaceMountID string          `json:"workspace_mount_id,omitempty"`
	Cwd              string          `json:"cwd"`
	Cols             int32           `json:"cols"`
	Rows             int32           `json:"rows"`
	FilesystemMode   string          `json:"filesystem_mode"`
	State            string          `json:"state"`
	ProcessID        string          `json:"process_id,omitempty"`
	OutputCursor     int64           `json:"output_cursor"`
	InputCursor      int64           `json:"input_cursor"`
	Error            json.RawMessage `json:"error,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	StartedAt        *time.Time      `json:"started_at,omitempty"`
	ClosedAt         *time.Time      `json:"closed_at,omitempty"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type WorkspacePtyEnvelope struct {
	Pty      WorkspacePtyResponse `json:"pty"`
	IsCached bool                 `json:"is_cached,omitempty"`
}

type ListWorkspacePtySessionsResponse struct {
	Ptys []WorkspacePtyResponse `json:"ptys"`
}

type WorkspacePtyStreamChunkResponse struct {
	ID          string    `json:"id"`
	Stream      string    `json:"stream"`
	OffsetStart int64     `json:"offset_start"`
	OffsetEnd   int64     `json:"offset_end"`
	Data        []byte    `json:"data"`
	ObservedAt  time.Time `json:"observed_at"`
	CreatedAt   time.Time `json:"created_at"`
}

type ListWorkspacePtyStreamChunksResponse struct {
	Chunks []WorkspacePtyStreamChunkResponse `json:"chunks"`
}
