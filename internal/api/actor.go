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

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/publicid"
)

var (
	actorDeclaredIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	durationPattern        = regexp.MustCompile(`^([1-9][0-9]*)(ms|s|m|h|d)$`)
	rfc3339InstantPattern  = regexp.MustCompile(
		`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?(?:Z|[+-][0-9]{2}:[0-9]{2})$`,
	)
)

const (
	MaxQueuedRunTTLMilliseconds = int64(365 * 24 * 60 * 60 * 1000)
	MaxRetryDelayMilliseconds   = int64(24 * 60 * 60 * 1000)
)

type StartActorWorkspaceTarget struct {
	ID  *string `json:"id,omitempty"`
	Key *string `json:"key,omitempty"`
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
	Key            *string                   `json:"key,omitempty"`
	Input          json.RawMessage           `json:"input,omitempty"`
	IdempotencyKey string                    `json:"idempotency_key,omitempty"`
	Workspace      StartActorWorkspaceTarget `json:"workspace"`
	Metadata       json.RawMessage           `json:"metadata,omitempty"`
	Tags           []string                  `json:"tags,omitempty"`
	ExpiresAt      *time.Time                `json:"expires_at,omitempty"`
	Run            *StartActorRunOptions     `json:"run,omitempty"`
}

type StartActorResponse struct {
	ActorID string `json:"actor_id"`
	RunID   string `json:"run_id"`
}

type SendActorInputRequest struct {
	ActorID        string          `json:"actor_id,omitempty"`
	ActorKey       string          `json:"actor_key,omitempty"`
	Input          json.RawMessage `json:"input"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

type SendActorInputResponse struct {
	Sequence int64 `json:"sequence"`
}

type ActorOperationRequest struct {
	ActorID        string `json:"actor_id,omitempty"`
	ActorKey       string `json:"actor_key,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type ActorOperationReceipt struct {
	ActorID    string    `json:"actor_id"`
	Lifecycle  string    `json:"lifecycle"`
	AcceptedAt time.Time `json:"accepted_at"`
}

func ValidateActorOperationRequest(request ActorOperationRequest) error {
	hasID := request.ActorID != ""
	hasKey := request.ActorKey != ""
	if hasID == hasKey {
		return errors.New("exactly one of actor_id or actor_key is required")
	}
	if hasID {
		return ValidateActorPublicID(request.ActorID)
	}
	return ValidateActorKey(request.ActorKey)
}

func ValidateActorDeclaredID(id string) error {
	if !actorDeclaredIDPattern.MatchString(id) {
		return fmt.Errorf("actor declared ID %q must match %s", id, actorDeclaredIDPattern.String())
	}
	return nil
}

func ValidateActorPublicID(id string) error {
	if err := publicid.ValidateFor(publicid.Actor, id); err != nil {
		return fmt.Errorf("invalid actor ID: %w", err)
	}
	return nil
}

func ValidateWorkspacePublicID(id string) error {
	if err := publicid.ValidateFor(publicid.Workspace, id); err != nil {
		return fmt.Errorf("invalid Workspace ID: %w", err)
	}
	return nil
}

func ValidateWorkspaceKey(key string) error {
	if key == "" ||
		!utf8.ValidString(key) ||
		len(key) > 512 ||
		strings.ContainsRune(key, '\x00') ||
		strings.TrimSpace(key) != key {
		return errors.New("Workspace key is outside the exact Workspace key domain")
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

func ValidateSendActorInputRequest(request SendActorInputRequest) error {
	hasID := request.ActorID != ""
	hasKey := request.ActorKey != ""
	if hasID == hasKey {
		return errors.New("exactly one of actor_id or actor_key is required")
	}
	if hasID {
		if err := ValidateActorPublicID(request.ActorID); err != nil {
			return err
		}
	} else if err := ValidateActorKey(request.ActorKey); err != nil {
		return err
	}
	if len(request.Input) == 0 {
		return errors.New("input is required")
	}
	if _, err := jsoncanon.Transform(request.Input); err != nil {
		return fmt.Errorf("input must be unambiguous I-JSON: %w", err)
	}
	return nil
}

func ValidateStartActorRequest(request StartActorRequest) error {
	if err := ValidateStartActorWorkspaceTarget(request.Workspace); err != nil {
		return err
	}
	if request.Key != nil {
		if err := ValidateActorKey(*request.Key); err != nil {
			return err
		}
	}
	if len(request.Input) > 0 {
		if _, err := jsoncanon.Transform(request.Input); err != nil {
			return fmt.Errorf("input must be unambiguous I-JSON: %w", err)
		}
	}
	if len(request.Metadata) > 0 {
		if err := validateJSONObject(request.Metadata, "metadata"); err != nil {
			return err
		}
	}
	if request.Run == nil {
		return nil
	}
	if request.Run.Queue != "" {
		if err := ValidateQueueName(request.Run.Queue); err != nil {
			return err
		}
	}
	if request.Run.ConcurrencyKey != nil {
		if err := ValidateConcurrencyKey(*request.Run.ConcurrencyKey); err != nil {
			return err
		}
	}
	if request.Run.TTL != "" {
		if _, err := ParseDurationMilliseconds(
			request.Run.TTL,
			"run.ttl",
			1,
			MaxQueuedRunTTLMilliseconds,
		); err != nil {
			return err
		}
	}
	if _, err := NormalizeStartActorRetry(request.Run.Retry); err != nil {
		return err
	}
	if len(request.Run.Metadata) > 0 {
		if err := validateJSONObject(request.Run.Metadata, "run.metadata"); err != nil {
			return err
		}
	}
	return nil
}

func ParseRFC3339NanosecondInstant(raw string) (time.Time, error) {
	if !rfc3339InstantPattern.MatchString(raw) {
		return time.Time{}, errors.New(
			"expires_at must be an RFC 3339 instant with an explicit timezone and at most nanosecond precision",
		)
	}
	if !strings.HasSuffix(raw, "Z") {
		offset := raw[len(raw)-6:]
		hours, _ := strconv.Atoi(offset[1:3])
		minutes, _ := strconv.Atoi(offset[4:6])
		if hours > 23 || minutes > 59 {
			return time.Time{}, errors.New("expires_at timezone offset is outside RFC 3339")
		}
	}
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("expires_at must be a valid RFC 3339 instant: %w", err)
	}
	return value, nil
}

func ValidateStartActorWorkspaceTarget(workspace StartActorWorkspaceTarget) error {
	hasWorkspaceID := workspace.ID != nil
	hasWorkspaceKey := workspace.Key != nil
	if hasWorkspaceID == hasWorkspaceKey {
		return errors.New("workspace must contain exactly one of id or key")
	}
	if hasWorkspaceID {
		if err := ValidateWorkspacePublicID(*workspace.ID); err != nil {
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
