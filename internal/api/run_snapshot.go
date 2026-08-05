package api

import (
	"encoding/json"
	"time"
)

type RunSnapshotResponse struct {
	ID                   string                `json:"id"`
	Status               RunStatus             `json:"status"`
	Entrypoint           RunEntrypointResponse `json:"entrypoint"`
	Deployment           DeploymentReference   `json:"deployment"`
	WorkspaceID          string                `json:"workspace_id"`
	SessionID            string                `json:"session_id,omitempty"`
	ParentRunID          string                `json:"parent_run_id,omitempty"`
	CurrentAttemptNumber int32                 `json:"current_attempt_number"`
	Cause                RunCauseResponse      `json:"cause"`
	Metadata             json.RawMessage       `json:"metadata"`
	Tags                 []string              `json:"tags"`
	Output               json.RawMessage       `json:"output,omitempty"`
	Failure              *RunFailureResponse   `json:"failure,omitempty"`
	CreatedAt            time.Time             `json:"created_at"`
	StartedAt            *time.Time            `json:"started_at,omitempty"`
	TerminalAt           *time.Time            `json:"terminal_at,omitempty"`
}

type RunEntrypointResponse struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type DeploymentReference struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type RunCauseResponse struct {
	Type            string     `json:"type"`
	ParentRunID     string     `json:"parent_run_id,omitempty"`
	ScheduleID      string     `json:"schedule_id,omitempty"`
	ScheduledAt     *time.Time `json:"scheduled_at,omitempty"`
	LastScheduledAt *time.Time `json:"last_scheduled_at,omitempty"`
	Timezone        string     `json:"timezone,omitempty"`
}

type RunFailureResponse struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
}

type ListRunSnapshotsResponse struct {
	Runs       []RunSnapshotResponse `json:"runs"`
	NextCursor string                `json:"next_cursor,omitempty"`
}
