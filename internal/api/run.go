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

	"github.com/helmrdotdev/helmr/internal/sourceid"
)

var queueNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)

func ValidateDefinitionID(id string) error {
	if !sourceid.Valid(id) {
		return fmt.Errorf("task_id %q must match %s", id, sourceid.Grammar)
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
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Status    SecretStatus `json:"status"`
	CreatedAt time.Time    `json:"created_at"`
	RotatedAt *time.Time   `json:"rotated_at,omitempty"`
	RevokedAt *time.Time   `json:"revoked_at,omitempty"`
}

type SecretStatus string

const (
	SecretStatusActive  SecretStatus = "active"
	SecretStatusRevoked SecretStatus = "revoked"
	SecretStatusDeleted SecretStatus = "deleted"
)

type ListSecretsResponse struct {
	Secrets    []SecretResponse `json:"secrets"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type RunStatus string

const (
	RunStatusQueued          RunStatus = "queued"
	RunStatusRunning         RunStatus = "running"
	RunStatusWaiting         RunStatus = "waiting"
	RunStatusRetryDelayed    RunStatus = "retry_delayed"
	RunStatusCancelRequested RunStatus = "cancel_requested"
	RunStatusSucceeded       RunStatus = "succeeded"
	RunStatusFailed          RunStatus = "failed"
	RunStatusCancelled       RunStatus = "cancelled"
	RunStatusExpired         RunStatus = "expired"
	RunStatusSystemFailed    RunStatus = "system_failed"

	RunEventKindCompleted = "run.completed"
	RunEventKindFailed    = "run.failed"
	RunEventKindCancelled = "run.cancelled"
	RunEventKindExpired   = "run.expired"
)

func RunStatusIsTerminal(status RunStatus) bool {
	switch status {
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
	ID               string
	Kind             string
	RunID            string
	AttemptNumber    int32
	Level            string
	Message          string
	Attributes       json.RawMessage
	ObservedSequence *int64
	ContentBase64    string
	Bytes            *int64
	At               time.Time
}

func (r RunLogRecord) MarshalJSON() ([]byte, error) {
	common := struct {
		ID            string    `json:"id"`
		Kind          string    `json:"kind"`
		RunID         string    `json:"run_id"`
		AttemptNumber int32     `json:"attempt_number"`
		At            time.Time `json:"at"`
	}{r.ID, r.Kind, r.RunID, r.AttemptNumber, r.At}
	switch r.Kind {
	case "structured":
		return json.Marshal(struct {
			ID            string          `json:"id"`
			Kind          string          `json:"kind"`
			RunID         string          `json:"run_id"`
			AttemptNumber int32           `json:"attempt_number"`
			Level         string          `json:"level"`
			Message       string          `json:"message"`
			Attributes    json.RawMessage `json:"attributes"`
			At            time.Time       `json:"at"`
		}{common.ID, common.Kind, common.RunID, common.AttemptNumber, r.Level, r.Message, r.Attributes, common.At})
	case "stdout", "stderr":
		var observedSequence, bytes int64
		if r.ObservedSequence != nil {
			observedSequence = *r.ObservedSequence
		}
		if r.Bytes != nil {
			bytes = *r.Bytes
		}
		return json.Marshal(struct {
			ID               string    `json:"id"`
			Kind             string    `json:"kind"`
			RunID            string    `json:"run_id"`
			AttemptNumber    int32     `json:"attempt_number"`
			ObservedSequence int64     `json:"observed_sequence"`
			ContentBase64    string    `json:"content_base64"`
			Bytes            int64     `json:"bytes"`
			At               time.Time `json:"at"`
		}{common.ID, common.Kind, common.RunID, common.AttemptNumber, observedSequence, r.ContentBase64, bytes, common.At})
	default:
		return nil, fmt.Errorf("run log kind %q is invalid", r.Kind)
	}
}

func (r *RunLogRecord) UnmarshalJSON(data []byte) error {
	var header struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return err
	}
	switch header.Kind {
	case "structured":
		var value struct {
			ID            string          `json:"id"`
			Kind          string          `json:"kind"`
			RunID         string          `json:"run_id"`
			AttemptNumber int32           `json:"attempt_number"`
			Level         string          `json:"level"`
			Message       string          `json:"message"`
			Attributes    json.RawMessage `json:"attributes"`
			At            time.Time       `json:"at"`
		}
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		*r = RunLogRecord{ID: value.ID, Kind: value.Kind, RunID: value.RunID, AttemptNumber: value.AttemptNumber, Level: value.Level, Message: value.Message, Attributes: value.Attributes, At: value.At}
		return nil
	case "stdout", "stderr":
		var value struct {
			ID               string    `json:"id"`
			Kind             string    `json:"kind"`
			RunID            string    `json:"run_id"`
			AttemptNumber    int32     `json:"attempt_number"`
			ObservedSequence int64     `json:"observed_sequence"`
			ContentBase64    string    `json:"content_base64"`
			Bytes            int64     `json:"bytes"`
			At               time.Time `json:"at"`
		}
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		*r = RunLogRecord{ID: value.ID, Kind: value.Kind, RunID: value.RunID, AttemptNumber: value.AttemptNumber, ObservedSequence: &value.ObservedSequence, ContentBase64: value.ContentBase64, Bytes: &value.Bytes, At: value.At}
		return nil
	default:
		return fmt.Errorf("run log kind %q is invalid", header.Kind)
	}
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

type RunEventRecord struct {
	ID            string          `json:"id"`
	RunID         string          `json:"run_id"`
	AttemptNumber *int32          `json:"attempt_number,omitempty"`
	Category      string          `json:"category"`
	Severity      string          `json:"severity"`
	Source        string          `json:"source"`
	Kind          string          `json:"kind"`
	Message       string          `json:"message"`
	Attributes    json.RawMessage `json:"attributes"`
	OccurredAt    time.Time       `json:"occurred_at"`
	At            time.Time       `json:"at"`
}

type RunEventRecordPage struct {
	Events     []RunEventRecord `json:"events"`
	NextCursor string           `json:"next_cursor,omitempty"`
}
