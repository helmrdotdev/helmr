package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session"
)

const (
	maxTags           = 10
	maxQueuedRunTTLMS = int64(31_536_000_000)
)

type actorStartRequest struct {
	OrgID                 uuid.UUID
	ProjectID             uuid.UUID
	EnvironmentID         uuid.UUID
	ActorDeclaredID       string
	ComputerID            uuid.UUID
	Key                   *string
	IdempotencyKey        string
	ManagedQueueName      string
	ManagedConcurrencyKey *string
	ManagedPriority       int32
	ManagedQueuedTTLMS    *int64
	ManagedRetryPolicy    json.RawMessage
	ManagedRunMetadata    json.RawMessage
	ManagedRunTags        []string
}

type normalizedActorStart struct {
	actorStartRequest
	fingerprint idempotency.ActorStartFingerprint
}

// startActor normalizes a public Actor start and admits it through the
// session owner.
func (s *Server) startActor(ctx context.Context, request actorStartRequest) (session.Started, error) {
	start, claim, err := prepareActorStart(request)
	if err != nil {
		return session.Started{}, err
	}
	return session.Start(ctx, s.tx, claim, start)
}

// prepareActorStart normalizes an Actor start and builds its idempotency
// claim when the request carries a key.
func prepareActorStart(request actorStartRequest) (session.StartRequest, idempotency.Request, error) {
	normalized, err := normalizeActorStart(request)
	if err != nil {
		return session.StartRequest{}, nil, err
	}
	var claim idempotency.Request
	if normalized.IdempotencyKey != "" {
		claim, err = idempotency.NewActorStartRequest(
			normalized.EnvironmentID,
			normalized.ActorDeclaredID,
			normalized.IdempotencyKey,
			normalized.fingerprint,
		)
		if err != nil {
			return session.StartRequest{}, nil, fmt.Errorf("%w: %v", session.ErrStartInvalid, err)
		}
	}
	return session.StartRequest{
		OrgID: normalized.OrgID, ProjectID: normalized.ProjectID, EnvironmentID: normalized.EnvironmentID,
		ActorDeclaredID: normalized.ActorDeclaredID, ComputerID: normalized.ComputerID, Key: normalized.Key,
		QueueName: normalized.ManagedQueueName, ConcurrencyKey: normalized.ManagedConcurrencyKey,
		Priority: normalized.ManagedPriority, QueuedTTLMS: normalized.ManagedQueuedTTLMS,
		RetryPolicy: normalized.ManagedRetryPolicy, Metadata: normalized.ManagedRunMetadata,
		Tags: normalized.ManagedRunTags,
	}, claim, nil
}

func normalizeActorStart(request actorStartRequest) (normalizedActorStart, error) {
	if request.OrgID == uuid.Nil() || request.ProjectID == uuid.Nil() ||
		request.EnvironmentID == uuid.Nil() {
		return normalizedActorStart{}, session.ErrStartInvalid
	}
	if err := api.ValidateActorDeclaredID(request.ActorDeclaredID); err != nil {
		return normalizedActorStart{}, fmt.Errorf("%w: %v", session.ErrStartInvalid, err)
	}
	if request.Key != nil {
		if err := api.ValidateActorKey(*request.Key); err != nil {
			return normalizedActorStart{}, fmt.Errorf("%w: %v", session.ErrStartInvalid, err)
		}
		key := *request.Key
		request.Key = &key
	}
	if request.ComputerID == uuid.Nil() {
		return normalizedActorStart{}, session.ErrStartInvalid
	}
	computerRaw, err := json.Marshal(api.ComputerIDTarget{ID: request.ComputerID.String()})
	if err != nil {
		return normalizedActorStart{}, fmt.Errorf("%w: encode computer address", session.ErrStartInvalid)
	}
	computer, err := canonicalJSON(computerRaw)
	if err != nil {
		return normalizedActorStart{}, fmt.Errorf("%w: canonicalize computer address", session.ErrStartInvalid)
	}
	request.ManagedRunMetadata, err = run.NormalizeMetadata(request.ManagedRunMetadata, run.MaxMetadataBytes, "managed run")
	if err != nil {
		return normalizedActorStart{}, fmt.Errorf("%w: %v", session.ErrStartInvalid, err)
	}
	request.ManagedRunTags, err = normalizeTags(request.ManagedRunTags, maxTags, "managed run")
	if err != nil {
		return normalizedActorStart{}, fmt.Errorf("%w: %v", session.ErrStartInvalid, err)
	}
	if request.ManagedQueueName != "" {
		if err := definition.ValidateQueueName(request.ManagedQueueName); err != nil {
			return normalizedActorStart{}, fmt.Errorf("%w: %v", session.ErrStartInvalid, err)
		}
	}
	if request.ManagedConcurrencyKey != nil {
		value := *request.ManagedConcurrencyKey
		if len(value) == 0 || len(value) > 512 || !utf8.ValidString(value) ||
			strings.IndexByte(value, 0) >= 0 || hasInvalidConcurrencyKeyEdge(value) {
			return normalizedActorStart{}, fmt.Errorf("%w: managed run concurrency key is invalid", session.ErrStartInvalid)
		}
		request.ManagedConcurrencyKey = &value
	}
	if request.ManagedQueuedTTLMS != nil &&
		(*request.ManagedQueuedTTLMS < 1 || *request.ManagedQueuedTTLMS > maxQueuedRunTTLMS) {
		return normalizedActorStart{}, fmt.Errorf(
			"%w: managed run queued TTL must be between 1 and %d ms",
			session.ErrStartInvalid,
			maxQueuedRunTTLMS,
		)
	}
	if len(request.ManagedRetryPolicy) > 0 {
		canonicalRetry, err := canonicalJSON(request.ManagedRetryPolicy)
		if err != nil {
			return normalizedActorStart{}, fmt.Errorf("%w: retry must be unambiguous JSON", session.ErrStartInvalid)
		}
		if _, err := definition.ParseRetry(canonicalRetry); err != nil {
			return normalizedActorStart{}, fmt.Errorf("%w: %v", session.ErrStartInvalid, err)
		}
		request.ManagedRetryPolicy = canonicalRetry
	}
	fingerprint := idempotency.ActorStartFingerprint{
		Key:              request.Key,
		ComputerAddress:  computer,
		ManagedQueueName: request.ManagedQueueName, ManagedConcurrencyKey: request.ManagedConcurrencyKey,
		ManagedPriority: request.ManagedPriority, ManagedQueuedTTLMS: request.ManagedQueuedTTLMS,
		ManagedRetryPolicy: request.ManagedRetryPolicy,
		ManagedRunMetadata: request.ManagedRunMetadata, ManagedRunTags: request.ManagedRunTags,
	}
	return normalizedActorStart{actorStartRequest: request, fingerprint: fingerprint}, nil
}

func normalizeTags(raw []string, limit int, label string) ([]string, error) {
	seen := make(map[string]struct{}, len(raw))
	tags := make([]string, 0, len(raw))
	for _, tag := range raw {
		trimmed := strings.TrimSpace(tag)
		if trimmed == "" || len([]byte(trimmed)) > maxTagBytes || !utf8.ValidString(trimmed) {
			return nil, fmt.Errorf("%s tags must be nonempty UTF-8 strings no larger than %d bytes", label, maxTagBytes)
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		tags = append(tags, trimmed)
	}
	if len(tags) > limit {
		return nil, fmt.Errorf("%s tags must contain at most %d values", label, limit)
	}
	sort.Strings(tags)
	return tags, nil
}

func hasInvalidConcurrencyKeyEdge(value string) bool {
	invalid := func(value byte) bool {
		return value == 0x20 || (value >= 0x09 && value <= 0x0d)
	}
	return invalid(value[0]) || invalid(value[len(value)-1])
}
