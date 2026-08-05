package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

var (
	actorDeclaredIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	durationPattern        = regexp.MustCompile(`^([1-9][0-9]*)(ms|s|m|h|d)$`)
)

const (
	MaxQueuedRunTTLMilliseconds = int64(365 * 24 * 60 * 60 * 1000)
	MaxRetryDelayMilliseconds   = int64(24 * 60 * 60 * 1000)
)

type WorkspaceTarget struct {
	ID  *string `json:"id,omitempty"`
	Key *string `json:"key,omitempty"`
}

type WorkspaceIDTarget struct {
	ID string `json:"id"`
}

type StartActorRetryBackoff struct {
	MinDelay string `json:"min_delay,omitempty"`
	MaxDelay string `json:"max_delay,omitempty"`
	Factor   *int64 `json:"factor,omitempty"`
	Jitter   string `json:"jitter,omitempty"`
}

type StartActorRetryPolicy struct {
	Enabled     *bool                   `json:"enabled,omitempty"`
	MaxAttempts *int64                  `json:"max_attempts,omitempty"`
	Backoff     *StartActorRetryBackoff `json:"backoff,omitempty"`
}

type StartActorRunOptions struct {
	Queue          string                 `json:"queue,omitempty"`
	ConcurrencyKey *string                `json:"concurrency_key,omitempty"`
	Priority       int32                  `json:"priority,omitempty"`
	TTL            string                 `json:"ttl,omitempty"`
	Retry          *StartActorRetryPolicy `json:"retry,omitempty"`
	Metadata       json.RawMessage        `json:"metadata,omitempty"`
	Tags           []string               `json:"tags,omitempty"`
}

type StartActorRequest struct {
	Key            *string               `json:"key,omitempty"`
	Input          json.RawMessage       `json:"input,omitempty"`
	IdempotencyKey string                `json:"idempotency_key,omitempty"`
	Workspace      WorkspaceIDTarget     `json:"workspace"`
	Run            *StartActorRunOptions `json:"run,omitempty"`
}

type ActorStartOptions struct {
	Key       *string
	Input     json.RawMessage
	Workspace WorkspaceIDTarget
	Run       *StartActorRunOptions
}

type StartActorResponse struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
}

type SendSessionInputRequest struct {
	Input          json.RawMessage `json:"input"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type SessionInputSource struct {
	Kind  string `json:"kind"`
	RunID string `json:"run_id,omitempty"`
}

type SessionInput struct {
	ID        string             `json:"id"`
	Sequence  int64              `json:"sequence"`
	Data      json.RawMessage    `json:"data"`
	Source    SessionInputSource `json:"source"`
	CreatedAt time.Time          `json:"created_at"`
}

type CloseSessionRequest struct {
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type SessionCloseReceipt struct {
	SessionID  string    `json:"session_id"`
	AcceptedAt time.Time `json:"accepted_at"`
}

type SessionStatus string

const (
	SessionStatusOpen      SessionStatus = "open"
	SessionStatusClosed    SessionStatus = "closed"
	SessionStatusCancelled SessionStatus = "cancelled"
	SessionStatusFailed    SessionStatus = "failed"
)

type SessionFailure struct {
	Code  string `json:"code"`
	RunID string `json:"run_id"`
}

type SessionStatusSnapshot struct {
	ID           string          `json:"id"`
	Key          *string         `json:"key,omitempty"`
	Status       SessionStatus   `json:"status"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	CurrentRunID *string         `json:"current_run_id,omitempty"`
	Failure      *SessionFailure `json:"failure,omitempty"`
}

type Session struct {
	ID           string          `json:"id"`
	ActorID      string          `json:"actor_id"`
	DeploymentID string          `json:"deployment_id"`
	Key          *string         `json:"key,omitempty"`
	Status       SessionStatus   `json:"status"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	CurrentRunID *string         `json:"current_run_id,omitempty"`
	Failure      *SessionFailure `json:"failure,omitempty"`
}

type ListSessionsResponse struct {
	Sessions   []Session `json:"sessions"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

type SessionOutputProvenance struct {
	RunID         string `json:"run_id"`
	AttemptNumber int32  `json:"attempt_number"`
	DeploymentID  string `json:"deployment_id"`
}

type SessionOutput struct {
	ID          string                  `json:"id"`
	Sequence    int64                   `json:"sequence"`
	Data        json.RawMessage         `json:"data"`
	ContentType string                  `json:"content_type"`
	CreatedAt   time.Time               `json:"created_at"`
	Provenance  SessionOutputProvenance `json:"provenance"`
}

type SessionOutputPage struct {
	Records   []SessionOutput `json:"records"`
	NextAfter int64           `json:"next_after"`
	HasMore   bool            `json:"has_more"`
}

func ValidateSessionStatus(status string) error {
	switch SessionStatus(status) {
	case SessionStatusOpen,
		SessionStatusClosed,
		SessionStatusCancelled,
		SessionStatusFailed:
		return nil
	default:
		return fmt.Errorf("invalid session status %q", status)
	}
}

func ValidateCloseSessionRequest(request CloseSessionRequest) error {
	return nil
}

func ValidateActorDeclaredID(id string) error {
	if !actorDeclaredIDPattern.MatchString(id) {
		return fmt.Errorf("actor declared ID %q must match %s", id, actorDeclaredIDPattern.String())
	}
	return nil
}

func ValidateActorID(id string) error {
	if err := ids.Validate(id); err != nil {
		return fmt.Errorf("invalid actor ID: %w", err)
	}
	return nil
}

func ValidateWorkspaceID(id string) error {
	if err := ids.Validate(id); err != nil {
		return fmt.Errorf("invalid workspace ID: %w", err)
	}
	return nil
}

func ValidateWorkspaceKey(key string) error {
	if key == "" ||
		!utf8.ValidString(key) ||
		len(key) > 512 ||
		strings.ContainsRune(key, '\x00') ||
		strings.TrimSpace(key) != key {
		return errors.New("workspace key is outside the exact workspace key domain")
	}
	return nil
}

func ValidateActorKey(key string) error {
	if !utf8.ValidString(key) {
		return errors.New("actor_key must be valid UTF-8")
	}
	if len(key) == 0 || len(key) > 512 {
		return errors.New("actor_key must be between 1 and 512 bytes")
	}
	if strings.IndexByte(key, 0) >= 0 {
		return errors.New("actor_key must not contain NUL")
	}
	first, _ := utf8.DecodeRuneInString(key)
	last, _ := utf8.DecodeLastRuneInString(key)
	if unicode.IsSpace(first) || unicode.IsSpace(last) {
		return errors.New("actor_key must not start or end with whitespace")
	}
	return nil
}

func ValidateSendSessionInputRequest(request SendSessionInputRequest) error {
	if len(request.Input) == 0 {
		return errors.New("input is required")
	}
	if _, err := jsoncanon.Transform(request.Input); err != nil {
		return fmt.Errorf("input must be unambiguous I-JSON: %w", err)
	}
	return nil
}

func ValidateStartActorRequest(request StartActorRequest) error {
	if err := ValidateWorkspaceIDTarget(request.Workspace); err != nil {
		return err
	}
	return validateActorStartOptions(request.Key, request.Input, request.Run)
}

func ValidateActorStartOptions(request ActorStartOptions) error {
	if err := ValidateWorkspaceIDTarget(request.Workspace); err != nil {
		return err
	}
	return validateActorStartOptions(request.Key, request.Input, request.Run)
}

func validateActorStartOptions(
	key *string,
	input json.RawMessage,
	run *StartActorRunOptions,
) error {
	if key != nil {
		if err := ValidateActorKey(*key); err != nil {
			return err
		}
	}
	if len(input) > 0 {
		if _, err := jsoncanon.Transform(input); err != nil {
			return fmt.Errorf("input must be unambiguous I-JSON: %w", err)
		}
	}
	if run == nil {
		return nil
	}
	if run.Queue != "" {
		if err := ValidateQueueName(run.Queue); err != nil {
			return err
		}
	}
	if run.ConcurrencyKey != nil {
		if err := ValidateConcurrencyKey(*run.ConcurrencyKey); err != nil {
			return err
		}
	}
	if run.TTL != "" {
		if _, err := ParseDurationMilliseconds(
			run.TTL,
			"run.ttl",
			1,
			MaxQueuedRunTTLMilliseconds,
		); err != nil {
			return err
		}
	}
	if _, err := NormalizeStartActorRetry(run.Retry); err != nil {
		return err
	}
	if len(run.Metadata) > 0 {
		if err := validateJSONObject(run.Metadata, "run.metadata"); err != nil {
			return err
		}
	}
	return nil
}

func ValidateWorkspaceIDTarget(workspace WorkspaceIDTarget) error {
	if workspace.ID == "" {
		return errors.New("workspace.id is required")
	}
	return ValidateWorkspaceID(workspace.ID)
}

func ValidateWorkspaceTarget(workspace WorkspaceTarget) error {
	hasWorkspaceID := workspace.ID != nil
	hasWorkspaceKey := workspace.Key != nil
	if hasWorkspaceID == hasWorkspaceKey {
		return errors.New("workspace must contain exactly one of id or key")
	}
	if hasWorkspaceID {
		if err := ValidateWorkspaceID(*workspace.ID); err != nil {
			return err
		}
	} else if err := ValidateWorkspaceKey(*workspace.Key); err != nil {
		return err
	}
	return nil
}

func ParseDurationMilliseconds(raw string, label string, minValue int64, maxValue int64) (int64, error) {
	match := durationPattern.FindStringSubmatch(raw)
	if match == nil {
		return 0, fmt.Errorf("%s must match %s", label, durationPattern.String())
	}
	value, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s is outside the supported range", label)
	}
	multiplier := uint64(1)
	switch match[2] {
	case "s":
		multiplier = 1000
	case "m":
		multiplier = 60 * 1000
	case "h":
		multiplier = 60 * 60 * 1000
	case "d":
		multiplier = 24 * 60 * 60 * 1000
	}
	if value > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("%s is outside the supported range", label)
	}
	milliseconds := int64(value * multiplier)
	if milliseconds < minValue || milliseconds > maxValue {
		return 0, fmt.Errorf("%s must be in [%d,%d] milliseconds", label, minValue, maxValue)
	}
	return milliseconds, nil
}

func NormalizeStartActorRetry(retry *StartActorRetryPolicy) (json.RawMessage, error) {
	if retry == nil {
		return nil, nil
	}
	enabled := true
	if retry.Enabled != nil {
		enabled = *retry.Enabled
	}
	if !enabled {
		if retry.MaxAttempts != nil || retry.Backoff != nil {
			return nil, errors.New("disabled run.retry must contain only enabled")
		}
		return json.RawMessage(`{"enabled":false}`), nil
	}
	if retry.MaxAttempts == nil || *retry.MaxAttempts < 1 || *retry.MaxAttempts > 10 {
		return nil, errors.New("enabled run.retry max_attempts must be in [1,10]")
	}
	minDelay := int64(1000)
	maxDelay := int64(30_000)
	factor := int64(2)
	jitter := "full"
	if retry.Backoff != nil {
		var err error
		if retry.Backoff.MinDelay != "" {
			minDelay, err = ParseDurationMilliseconds(
				retry.Backoff.MinDelay,
				"run.retry.backoff.min_delay",
				1,
				MaxRetryDelayMilliseconds,
			)
			if err != nil {
				return nil, err
			}
		}
		if retry.Backoff.MaxDelay != "" {
			maxDelay, err = ParseDurationMilliseconds(
				retry.Backoff.MaxDelay,
				"run.retry.backoff.max_delay",
				1,
				MaxRetryDelayMilliseconds,
			)
			if err != nil {
				return nil, err
			}
		}
		if retry.Backoff.Factor != nil {
			factor = *retry.Backoff.Factor
		}
		if retry.Backoff.Jitter != "" {
			jitter = retry.Backoff.Jitter
		}
	}
	if minDelay > maxDelay {
		return nil, errors.New("run.retry.backoff.min_delay must not exceed max_delay")
	}
	if factor < 1 || factor > 100 {
		return nil, errors.New("run.retry.backoff.factor must be in [1,100]")
	}
	if jitter != "none" && jitter != "full" {
		return nil, errors.New(`run.retry.backoff.jitter must be "none" or "full"`)
	}
	normalized, err := json.Marshal(struct {
		Enabled     bool  `json:"enabled"`
		MaxAttempts int64 `json:"maxAttempts"`
		Backoff     struct {
			MinMS  int64  `json:"minMs"`
			MaxMS  int64  `json:"maxMs"`
			Factor int64  `json:"factor"`
			Jitter string `json:"jitter"`
		} `json:"backoff"`
	}{
		Enabled:     true,
		MaxAttempts: *retry.MaxAttempts,
		Backoff: struct {
			MinMS  int64  `json:"minMs"`
			MaxMS  int64  `json:"maxMs"`
			Factor int64  `json:"factor"`
			Jitter string `json:"jitter"`
		}{
			MinMS: minDelay, MaxMS: maxDelay, Factor: factor, Jitter: jitter,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("normalize run.retry: %w", err)
	}
	return normalized, nil
}

func validateJSONObject(raw json.RawMessage, label string) error {
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return fmt.Errorf("%s must be unambiguous I-JSON: %w", label, err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &object); err != nil || object == nil {
		return fmt.Errorf("%s must be a JSON object", label)
	}
	return nil
}
