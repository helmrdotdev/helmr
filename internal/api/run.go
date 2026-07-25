package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var queueNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)

type CreateRunRequest struct {
	ProjectID     string           `json:"project_id,omitempty"`
	EnvironmentID string           `json:"environment_id,omitempty"`
	TaskID        string           `json:"task_id"`
	Payload       json.RawMessage  `json:"payload"`
	Options       CreateRunOptions `json:"options"`
}

type CreateRunOptions struct {
	Queue              *RunQueueOption `json:"queue,omitempty"`
	ConcurrencyKey     string          `json:"concurrency_key,omitempty"`
	Priority           int32           `json:"priority,omitempty"`
	TTL                string          `json:"ttl,omitempty"`
	MaxDurationSeconds int32           `json:"max_duration_seconds,omitempty"`
	Retry              json.RawMessage `json:"retry,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`
	Tags               []string        `json:"tags,omitempty"`
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

func ValidateConcurrencyKey(key string) error {
	if !utf8.ValidString(key) {
		return errors.New("concurrency_key must be valid UTF-8")
	}
	if len(key) == 0 || len(key) > 512 {
		return errors.New("concurrency_key must be between 1 and 512 bytes")
	}
	if strings.IndexByte(key, 0) >= 0 {
		return errors.New("concurrency_key must not contain NUL")
	}
	if invalidConcurrencyKeyBoundary(key[0]) || invalidConcurrencyKeyBoundary(key[len(key)-1]) {
		return errors.New("concurrency_key must not start or end with ASCII whitespace")
	}
	return nil
}

func invalidConcurrencyKeyBoundary(value byte) bool {
	return value == 0x20 || (value >= 0x09 && value <= 0x0d)
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

type CreateSecretRequest struct {
	Name           string `json:"name"`
	Value          string `json:"value"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type RotateSecretRequest struct {
	Value          string `json:"value"`
	IdempotencyKey string `json:"idempotency_key"`
}

type RevokeSecretRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
}

type SecretResponse struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"created_at"`
	RotatedAt *time.Time `json:"rotated_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

type ListSecretsResponse struct {
	Secrets    []SecretResponse `json:"secrets"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type RunResponse struct {
	ID                string          `json:"id"`
	ProjectID         string          `json:"project_id"`
	EnvironmentID     string          `json:"environment_id"`
	DeploymentID      string          `json:"deployment_id"`
	DeploymentTaskID  string          `json:"deployment_task_id"`
	Version           string          `json:"version"`
	DeploymentVersion string          `json:"deployment_version"`
	APIVersion        string          `json:"api_version"`
	SDKVersion        string          `json:"sdk_version,omitempty"`
	CLIVersion        string          `json:"cli_version,omitempty"`
	TaskID            string          `json:"task_id"`
	Status            string          `json:"status"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
	AttemptNumber     *int32          `json:"attempt_number,omitempty"`
	ExitCode          *int32          `json:"exit_code"`
	Output            json.RawMessage `json:"output,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	PendingWait       *PendingWait    `json:"pending_wait,omitempty"`
}

const (
	RunStatusQueued          = "queued"
	RunStatusRunning         = "running"
	RunStatusWaiting         = "waiting"
	RunStatusRetryDelayed    = "retry-delayed"
	RunStatusCancelRequested = "cancel-requested"
	RunStatusSucceeded       = "succeeded"
	RunStatusFailed          = "failed"
	RunStatusCancelled       = "cancelled"
	RunStatusExpired         = "expired"
	RunStatusSystemFailed    = "system-failed"

	RunEventKindCompleted = "run.completed"
	RunEventKindFailed    = "run.failed"
	RunEventKindCancelled = "run.cancelled"
	RunEventKindExpired   = "run.expired"
)

func RunStatusIsTerminal(status string) bool {
	switch strings.TrimSpace(status) {
	case RunStatusSucceeded, RunStatusFailed, RunStatusCancelled, RunStatusExpired, RunStatusSystemFailed:
		return true
	default:
		return false
	}
}

func RunEventKindIsTerminal(kind string) bool {
	switch strings.TrimSpace(kind) {
	case RunEventKindCompleted, RunEventKindFailed, RunEventKindCancelled, RunEventKindExpired:
		return true
	default:
		return false
	}
}

type PendingWait struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind,omitempty"`
	Status    string          `json:"status,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	Tags      []string        `json:"tags,omitempty"`
	Timeout   *int32          `json:"timeout,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
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

type RunLogChunk struct {
	ID            string    `json:"id"`
	RunID         string    `json:"run_id"`
	AttemptNumber int32     `json:"attempt_number"`
	Stream        string    `json:"stream"`
	ContentBase64 string    `json:"content_base64"`
	Bytes         int64     `json:"bytes"`
	ObservedSeq   int64     `json:"observed_seq"`
	At            time.Time `json:"at"`
}

type RunLogRecord struct {
	ID               string          `json:"id"`
	Kind             string          `json:"kind"`
	RunID            string          `json:"run_id"`
	AttemptNumber    int32           `json:"attempt_number"`
	Level            string          `json:"level,omitempty"`
	Message          string          `json:"message,omitempty"`
	Attributes       json.RawMessage `json:"attributes,omitempty"`
	ObservedSequence *int64          `json:"observed_sequence,omitempty"`
	ContentBase64    string          `json:"content_base64,omitempty"`
	Bytes            *int64          `json:"bytes,omitempty"`
	At               time.Time       `json:"at"`
}

type RunLogPage struct {
	Logs       []RunLogRecord `json:"logs"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

type RunEvent struct {
	ID             string          `json:"id"`
	RunID          *string         `json:"run_id,omitempty"`
	DeploymentID   *string         `json:"deployment_id,omitempty"`
	AttemptNumber  *int32          `json:"attempt_number,omitempty"`
	Trace          TraceContext    `json:"trace"`
	Category       string          `json:"category"`
	Severity       string          `json:"severity"`
	Source         string          `json:"source"`
	Kind           string          `json:"kind"`
	Message        string          `json:"message"`
	At             time.Time       `json:"at"`
	OccurredAt     time.Time       `json:"occurred_at"`
	RedactionClass string          `json:"redaction_class"`
	Attributes     json.RawMessage `json:"attributes"`
}

type RunEventPage struct {
	Events     []RunEvent `json:"events"`
	NextCursor *string    `json:"next_cursor,omitempty"`
}
