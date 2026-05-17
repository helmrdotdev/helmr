package api

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

var taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type CreateRunRequest struct {
	ProjectID          string          `json:"project_id,omitempty"`
	EnvironmentID      string          `json:"environment_id,omitempty"`
	TaskID             string          `json:"task_id"`
	Secrets            SecretBindings  `json:"secrets,omitempty"`
	Payload            json.RawMessage `json:"payload"`
	Workspace          RunWorkspace    `json:"workspace"`
	MaxDurationSeconds int32           `json:"max_duration_seconds"`
}

func ValidateTaskID(id string) error {
	if !taskIDPattern.MatchString(id) {
		return fmt.Errorf("task_id %q must match %s", id, taskIDPattern.String())
	}
	return nil
}

type SecretBindings map[string]string

type SetSecretRequest struct {
	ProjectID     string `json:"project_id,omitempty"`
	EnvironmentID string `json:"environment_id,omitempty"`
	Value         string `json:"value"`
}

type SecretResponse struct {
	ProjectID     string    `json:"project_id"`
	EnvironmentID string    `json:"environment_id"`
	Name          string    `json:"name"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type ListSecretsResponse struct {
	Secrets []SecretResponse `json:"secrets"`
}

type GitHubSource struct {
	Repository string `json:"repository,omitempty"`
	Ref        string `json:"ref,omitempty"`
	SHA        string `json:"sha,omitempty"`
	Subpath    string `json:"subpath,omitempty"`
}

type RunWorkspace struct {
	Repository string `json:"repository,omitempty"`
	Ref        string `json:"ref,omitempty"`
	SHA        string `json:"sha,omitempty"`
	Subpath    string `json:"subpath,omitempty"`
}

type RunResponse struct {
	ID            string          `json:"id"`
	ProjectID     string          `json:"project_id"`
	EnvironmentID string          `json:"environment_id"`
	TaskID        string          `json:"task_id"`
	Status        string          `json:"status"`
	ExitCode      *int32          `json:"exit_code"`
	Output        json.RawMessage `json:"output,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	PendingWait   *PendingWait    `json:"pending_wait,omitempty"`
}

type PendingWait struct {
	Kind        string    `json:"kind"`
	WaitpointID string    `json:"waitpoint_id"`
	Message     *string   `json:"message,omitempty"`
	Prompt      *string   `json:"prompt,omitempty"`
	Timeout     *int32    `json:"timeout,omitempty"`
	RequestedAt time.Time `json:"requested_at"`
}

type ResumeApprovalRequest struct {
	Reason string `json:"reason,omitempty"`
}

type ResumeMessageRequest struct {
	Text        string            `json:"text"`
	Attachments []json.RawMessage `json:"attachments,omitempty"`
}

type ListRunsResponse struct {
	Runs []RunResponse `json:"runs"`
}

type RunCountsResponse struct {
	Queued    int64 `json:"queued"`
	Claimed   int64 `json:"claimed"`
	Running   int64 `json:"running"`
	Waiting   int64 `json:"waiting"`
	Succeeded int64 `json:"succeeded"`
	Failed    int64 `json:"failed"`
	Cancelled int64 `json:"cancelled"`
}

type LogSnapshotResponse struct {
	StdoutBase64 string `json:"stdout_base64"`
	StderrBase64 string `json:"stderr_base64"`
	Cursor       string `json:"cursor"`
	Truncated    bool   `json:"truncated"`
}

type RunEvent struct {
	ID         string          `json:"id"`
	RunID      *string         `json:"run_id,omitempty"`
	Kind       string          `json:"kind"`
	Message    string          `json:"message"`
	At         time.Time       `json:"at"`
	Attributes json.RawMessage `json:"attributes"`
}

type RunEventPage struct {
	Events     []RunEvent `json:"events"`
	Cursor     int64      `json:"cursor"`
	NextCursor *int64     `json:"next_cursor,omitempty"`
}
