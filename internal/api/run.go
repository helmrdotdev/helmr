package api

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var queueNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)

type CreateRunRequest struct {
	ProjectID     string           `json:"project_id,omitempty"`
	EnvironmentID string           `json:"environment_id,omitempty"`
	TaskID        string           `json:"task_id"`
	Payload       json.RawMessage  `json:"payload"`
	Options       CreateRunOptions `json:"options,omitempty"`
}

type CreateRunOptions struct {
	DeploymentID          string          `json:"deployment_id,omitempty"`
	Version               string          `json:"version,omitempty"`
	Queue                 *RunQueueOption `json:"queue,omitempty"`
	ConcurrencyKey        string          `json:"concurrency_key,omitempty"`
	Priority              int32           `json:"priority,omitempty"`
	TTL                   string          `json:"ttl,omitempty"`
	MaxDurationSeconds    int32           `json:"max_duration_seconds,omitempty"`
	IdempotencyKey        string          `json:"idempotency_key,omitempty"`
	IdempotencyKeyTTL     string          `json:"idempotency_key_ttl,omitempty"`
	IdempotencyKeyOptions json.RawMessage `json:"idempotency_key_options,omitempty"`
}

type RunQueueOption struct {
	Name string `json:"name,omitempty"`
}

func ValidateTaskID(id string) error {
	if !taskIDPattern.MatchString(id) {
		return fmt.Errorf("task_id %q must match %s", id, taskIDPattern.String())
	}
	return nil
}

func ValidateQueueName(name string) error {
	if !queueNamePattern.MatchString(name) {
		return fmt.Errorf("queue name %q must match %s", name, queueNamePattern.String())
	}
	return nil
}

func ParsePositiveDuration(raw string, label string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if before, ok := strings.CutSuffix(raw, "d"); ok {
		days, err := strconv.ParseInt(before, 10, 32)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("%s must be a positive duration", label)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", label)
	}
	return duration, nil
}

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

type RunResponse struct {
	ID                string            `json:"id"`
	ProjectID         string            `json:"project_id"`
	EnvironmentID     string            `json:"environment_id"`
	DeploymentID      string            `json:"deployment_id"`
	DeploymentTaskID  string            `json:"deployment_task_id"`
	Version           string            `json:"version"`
	DeploymentVersion string            `json:"deployment_version"`
	APIVersion        string            `json:"api_version"`
	SDKVersion        string            `json:"sdk_version,omitempty"`
	CLIVersion        string            `json:"cli_version,omitempty"`
	TaskID            string            `json:"task_id"`
	Status            string            `json:"status"`
	AttemptNumber     *int32            `json:"attempt_number,omitempty"`
	ExitCode          *int32            `json:"exit_code"`
	Output            json.RawMessage   `json:"output,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
	PendingWaitpoint  *PendingWaitpoint `json:"pending_waitpoint,omitempty"`
	IdempotencyHit    bool              `json:"idempotency_hit,omitempty"`
}

type PendingWaitpoint struct {
	Kind        string                      `json:"kind"`
	WaitpointID string                      `json:"waitpoint_id"`
	Request     json.RawMessage             `json:"request,omitempty"`
	DisplayText string                      `json:"display_text,omitempty"`
	Timeout     *int32                      `json:"timeout,omitempty"`
	Policy      *string                     `json:"policy,omitempty"`
	Deliveries  []WaitpointDeliveryResponse `json:"deliveries,omitempty"`
	RequestedAt time.Time                   `json:"requested_at"`
}

type ListRunsResponse struct {
	Runs []RunResponse `json:"runs"`
}

type RunCountsResponse struct {
	Queued    int64 `json:"queued"`
	Running   int64 `json:"running"`
	Waiting   int64 `json:"waiting"`
	Succeeded int64 `json:"succeeded"`
	Failed    int64 `json:"failed"`
	Cancelled int64 `json:"cancelled"`
	Expired   int64 `json:"expired"`
}

type LogSnapshotResponse struct {
	StdoutBase64 string `json:"stdout_base64"`
	StderrBase64 string `json:"stderr_base64"`
	Cursor       string `json:"cursor"`
	Truncated    bool   `json:"truncated"`
}

type RunEvent struct {
	ID            string          `json:"id"`
	RunID         *string         `json:"run_id,omitempty"`
	ExecutionID   *string         `json:"execution_id,omitempty"`
	AttemptNumber *int32          `json:"attempt_number,omitempty"`
	Kind          string          `json:"kind"`
	Message       string          `json:"message"`
	At            time.Time       `json:"at"`
	Attributes    json.RawMessage `json:"attributes"`
}

type RunEventPage struct {
	Events     []RunEvent `json:"events"`
	Cursor     int64      `json:"cursor"`
	NextCursor *int64     `json:"next_cursor,omitempty"`
}
