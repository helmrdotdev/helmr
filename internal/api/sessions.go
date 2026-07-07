package api

import (
	"encoding/json"
	"errors"
	"time"
)

type SessionStartRequest struct {
	ProjectID     string              `json:"project_id,omitempty"`
	EnvironmentID string              `json:"environment_id,omitempty"`
	TaskID        string              `json:"task_id,omitempty"`
	Payload       json.RawMessage     `json:"payload,omitempty"`
	ExternalID    string              `json:"external_id,omitempty"`
	Options       SessionStartOptions `json:"-"`
}

type SessionStartOptions struct {
	Queue              *RunQueueOption `json:"queue,omitempty"`
	ConcurrencyKey     string          `json:"concurrency_key,omitempty"`
	Priority           int32           `json:"priority,omitempty"`
	TTL                string          `json:"ttl,omitempty"`
	MaxDurationSeconds int32           `json:"max_duration_seconds,omitempty"`
	Retry              json.RawMessage `json:"retry,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`
	Tags               []string        `json:"tags,omitempty"`
	ExpiresAt          *time.Time      `json:"expires_at,omitempty"`
	WorkspaceID        string          `json:"workspace_id,omitempty"`
}

func (r *SessionStartRequest) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if _, ok := raw["options"]; ok {
		return errors.New("options wrapper is not accepted for session start")
	}
	if _, ok := raw["bundle"]; ok {
		return errors.New("bundle is not accepted for session start")
	}
	if _, ok := raw["source"]; ok {
		return errors.New("source is not accepted for session start")
	}
	if _, ok := raw["deployment_id"]; ok {
		return errors.New("deployment_id is not accepted for session start")
	}
	if _, ok := raw["version"]; ok {
		return errors.New("version is not accepted for session start")
	}
	type sessionStartWire struct {
		ProjectID          string          `json:"project_id,omitempty"`
		EnvironmentID      string          `json:"environment_id,omitempty"`
		TaskID             string          `json:"task_id,omitempty"`
		Payload            json.RawMessage `json:"payload,omitempty"`
		ExternalID         string          `json:"external_id,omitempty"`
		Queue              *RunQueueOption `json:"queue,omitempty"`
		ConcurrencyKey     string          `json:"concurrency_key,omitempty"`
		Priority           int32           `json:"priority,omitempty"`
		TTL                string          `json:"ttl,omitempty"`
		MaxDurationSeconds int32           `json:"max_duration_seconds,omitempty"`
		Retry              json.RawMessage `json:"retry,omitempty"`
		Metadata           json.RawMessage `json:"metadata,omitempty"`
		Tags               []string        `json:"tags,omitempty"`
		ExpiresAt          *time.Time      `json:"expires_at,omitempty"`
		WorkspaceID        string          `json:"workspace_id,omitempty"`
	}
	var decoded sessionStartWire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	r.ProjectID = decoded.ProjectID
	r.EnvironmentID = decoded.EnvironmentID
	r.TaskID = decoded.TaskID
	r.Payload = decoded.Payload
	r.ExternalID = decoded.ExternalID
	r.Options = SessionStartOptions{
		Queue:              decoded.Queue,
		ConcurrencyKey:     decoded.ConcurrencyKey,
		Priority:           decoded.Priority,
		TTL:                decoded.TTL,
		MaxDurationSeconds: decoded.MaxDurationSeconds,
		Retry:              decoded.Retry,
		Metadata:           decoded.Metadata,
		Tags:               decoded.Tags,
		ExpiresAt:          decoded.ExpiresAt,
		WorkspaceID:        decoded.WorkspaceID,
	}
	return nil
}

func (r SessionStartRequest) MarshalJSON() ([]byte, error) {
	type sessionStartWire struct {
		ProjectID          string          `json:"project_id,omitempty"`
		EnvironmentID      string          `json:"environment_id,omitempty"`
		TaskID             string          `json:"task_id,omitempty"`
		Payload            json.RawMessage `json:"payload,omitempty"`
		ExternalID         string          `json:"external_id,omitempty"`
		Queue              *RunQueueOption `json:"queue,omitempty"`
		ConcurrencyKey     string          `json:"concurrency_key,omitempty"`
		Priority           int32           `json:"priority,omitempty"`
		TTL                string          `json:"ttl,omitempty"`
		MaxDurationSeconds int32           `json:"max_duration_seconds,omitempty"`
		Retry              json.RawMessage `json:"retry,omitempty"`
		Metadata           json.RawMessage `json:"metadata,omitempty"`
		Tags               []string        `json:"tags,omitempty"`
		ExpiresAt          *time.Time      `json:"expires_at,omitempty"`
		WorkspaceID        string          `json:"workspace_id,omitempty"`
	}
	return json.Marshal(sessionStartWire{
		ProjectID:          r.ProjectID,
		EnvironmentID:      r.EnvironmentID,
		TaskID:             r.TaskID,
		Payload:            r.Payload,
		ExternalID:         r.ExternalID,
		Queue:              r.Options.Queue,
		ConcurrencyKey:     r.Options.ConcurrencyKey,
		Priority:           r.Options.Priority,
		TTL:                r.Options.TTL,
		MaxDurationSeconds: r.Options.MaxDurationSeconds,
		Retry:              r.Options.Retry,
		Metadata:           r.Options.Metadata,
		Tags:               r.Options.Tags,
		ExpiresAt:          r.Options.ExpiresAt,
		WorkspaceID:        r.Options.WorkspaceID,
	})
}

func (o *SessionStartOptions) UnmarshalJSON(data []byte) error {
	type sessionStartOptions SessionStartOptions
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if _, ok := raw["deployment_id"]; ok {
		return errors.New("deployment_id is not accepted for session start")
	}
	if _, ok := raw["version"]; ok {
		return errors.New("version is not accepted for session start")
	}
	var decoded sessionStartOptions
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*o = SessionStartOptions(decoded)
	return nil
}

type SessionStartResponse struct {
	Session  SessionResponse `json:"session"`
	Run      RunResponse     `json:"run"`
	IsCached bool            `json:"is_cached,omitempty"`
	TimedOut bool            `json:"timed_out,omitempty"`
}

type SessionStartAndWaitRequest struct {
	SessionStartRequest
	TimeoutSeconds int32 `json:"timeout_seconds,omitempty"`
}

type SessionResponse struct {
	ID                  string          `json:"id"`
	ProjectID           string          `json:"project_id"`
	EnvironmentID       string          `json:"environment_id"`
	TaskID              string          `json:"task_id"`
	InitialDeploymentID string          `json:"initial_deployment_id"`
	ActiveDeploymentID  string          `json:"active_deployment_id"`
	ExternalID          string          `json:"external_id,omitempty"`
	Status              string          `json:"status"`
	Activity            string          `json:"activity"`
	CanClose            bool            `json:"can_close"`
	CurrentRunID        string          `json:"current_run_id,omitempty"`
	WorkspaceID         string          `json:"workspace_id,omitempty"`
	Metadata            json.RawMessage `json:"metadata,omitempty"`
	Tags                []string        `json:"tags,omitempty"`
	Result              json.RawMessage `json:"result,omitempty"`
	Error               json.RawMessage `json:"error,omitempty"`
	TimedOut            bool            `json:"timed_out,omitempty"`
	TerminalReason      json.RawMessage `json:"terminal_reason,omitempty"`
	ExpiresAt           *time.Time      `json:"expires_at,omitempty"`
	ExpiredAt           *time.Time      `json:"expired_at,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

type ListSessionsResponse struct {
	Sessions []SessionResponse `json:"sessions"`
}

type SessionAddress struct {
	Type       string `json:"type"`
	ID         string `json:"id,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
}

type PatchSessionRequest struct {
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	Tags      []string        `json:"tags,omitempty"`
	ExpiresAt *time.Time      `json:"expires_at,omitempty"`
}

type CloseSessionRequest struct {
	Reason string `json:"reason,omitempty"`
}

type CancelSessionRequest struct {
	Reason string `json:"reason,omitempty"`
}

type SessionRunResponse struct {
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

type ListSessionRunsResponse struct {
	Runs []SessionRunResponse `json:"runs"`
}
