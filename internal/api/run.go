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

func ValidateDefinitionID(id string) error {
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
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	RotatedAt *time.Time `json:"rotated_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

type ListSecretsResponse struct {
	Secrets    []SecretResponse `json:"secrets"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

const (
	RunStatusQueued          = "queued"
	RunStatusRunning         = "running"
	RunStatusWaiting         = "waiting"
	RunStatusRetryDelayed    = "retry_delayed"
	RunStatusCancelRequested = "cancel_requested"
	RunStatusSucceeded       = "succeeded"
	RunStatusFailed          = "failed"
	RunStatusCancelled       = "cancelled"
	RunStatusExpired         = "expired"
	RunStatusSystemFailed    = "system_failed"

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
