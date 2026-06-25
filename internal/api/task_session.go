package api

import (
	"encoding/json"
	"errors"
	"time"
)

type TaskStartRequest struct {
	ProjectID     string           `json:"project_id,omitempty"`
	EnvironmentID string           `json:"environment_id,omitempty"`
	Payload       json.RawMessage  `json:"payload,omitempty"`
	ExternalID    string           `json:"external_id,omitempty"`
	Options       TaskStartOptions `json:"options"`
}

type TaskStartOptions struct {
	Queue              *RunQueueOption `json:"queue,omitempty"`
	ConcurrencyKey     string          `json:"concurrency_key,omitempty"`
	Priority           int32           `json:"priority,omitempty"`
	TTL                string          `json:"ttl,omitempty"`
	MaxDurationSeconds int32           `json:"max_duration_seconds,omitempty"`
	Retry              json.RawMessage `json:"retry,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`
	Tags               []string        `json:"tags,omitempty"`
	IdempotencyKey     string          `json:"idempotency_key,omitempty"`
	IdempotencyKeyTTL  string          `json:"idempotency_key_ttl,omitempty"`
	ExpiresAt          *time.Time      `json:"expires_at,omitempty"`
	WorkspaceID        string          `json:"workspace_id,omitempty"`
}

func (o *TaskStartOptions) UnmarshalJSON(data []byte) error {
	type taskStartOptions TaskStartOptions
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if _, ok := raw["deployment_id"]; ok {
		return errors.New("deployment_id is not accepted for task start")
	}
	if _, ok := raw["version"]; ok {
		return errors.New("version is not accepted for task start")
	}
	var decoded taskStartOptions
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*o = TaskStartOptions(decoded)
	return nil
}

type TaskStartResponse struct {
	Session  TaskSessionResponse `json:"session"`
	Run      RunResponse         `json:"run"`
	IsCached bool                `json:"is_cached,omitempty"`
}

type TaskStartAndWaitRequest struct {
	TaskStartRequest
	TimeoutSeconds int32 `json:"timeout_seconds,omitempty"`
}

type TaskWaitRequest struct {
	TimeoutSeconds int32 `json:"timeout_seconds,omitempty"`
}

type TaskSessionResponse struct {
	ID                  string          `json:"id"`
	ProjectID           string          `json:"project_id"`
	EnvironmentID       string          `json:"environment_id"`
	TaskID              string          `json:"task_id"`
	InitialDeploymentID string          `json:"initial_deployment_id"`
	ActiveDeploymentID  string          `json:"active_deployment_id"`
	ExternalID          string          `json:"external_id,omitempty"`
	Status              string          `json:"status"`
	CurrentRunID        string          `json:"current_run_id,omitempty"`
	WorkspaceID         string          `json:"workspace_id,omitempty"`
	Metadata            json.RawMessage `json:"metadata,omitempty"`
	Tags                []string        `json:"tags,omitempty"`
	Result              json.RawMessage `json:"result,omitempty"`
	Error               json.RawMessage `json:"error,omitempty"`
	TimedOut            bool            `json:"timed_out,omitempty"`
	TerminalReason      json.RawMessage `json:"terminal_reason,omitempty"`
	ExpiresAt           *time.Time      `json:"expires_at,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

type ListTaskSessionsResponse struct {
	Sessions []TaskSessionResponse `json:"sessions"`
}

type PatchTaskSessionRequest struct {
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	Tags      []string        `json:"tags,omitempty"`
	ExpiresAt *time.Time      `json:"expires_at,omitempty"`
}

type CloseTaskSessionRequest struct {
	Reason string `json:"reason,omitempty"`
}

type CancelTaskSessionRequest struct {
	Reason string `json:"reason,omitempty"`
}

type TaskSessionRunResponse struct {
	ID              string     `json:"id"`
	RunID           string     `json:"run_id"`
	DeploymentID    string     `json:"deployment_id"`
	PreviousRunID   string     `json:"previous_run_id,omitempty"`
	TurnIndex       int32      `json:"turn_index"`
	Status          string     `json:"status"`
	ExecutionStatus string     `json:"execution_status"`
	TerminalOutcome string     `json:"terminal_outcome,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
}

type ListTaskSessionRunsResponse struct {
	Runs []TaskSessionRunResponse `json:"runs"`
}
