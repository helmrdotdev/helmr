package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/run"
)

const maxTaskPayloadBytes = 16 << 20

type taskStartRequest struct {
	OrgID          uuid.UUID
	ProjectID      uuid.UUID
	EnvironmentID  uuid.UUID
	TaskDeclaredID string
	PayloadPresent bool
	Payload        json.RawMessage
	ComputerID     uuid.UUID
	IdempotencyKey string
	QueueName      string
	ConcurrencyKey *string
	Priority       int32
	QueuedTTLMS    *int64
	RetryPolicy    json.RawMessage
	Metadata       json.RawMessage
	Tags           []string
}

type normalizedTaskStart struct {
	taskStartRequest
	fingerprint idempotency.TaskStartFingerprint
}

func (s *Server) startTask(ctx context.Context, request taskStartRequest) (run.TaskStarted, error) {
	normalized, err := normalizeTaskStart(request)
	if err != nil {
		return run.TaskStarted{}, err
	}
	start := normalized.runTaskStart()
	if normalized.IdempotencyKey != "" {
		start.Claim, err = idempotency.NewTaskStartRequest(
			normalized.EnvironmentID,
			normalized.TaskDeclaredID,
			normalized.IdempotencyKey,
			normalized.fingerprint,
		)
		if err != nil {
			return run.TaskStarted{}, fmt.Errorf("%w: %v", run.ErrTaskStartInvalid, err)
		}
	}
	return run.StartTask(ctx, s.tx, start)
}

// runTaskStart is the normalized start the run owner admits.
func (n normalizedTaskStart) runTaskStart() run.TaskStart {
	return run.TaskStart{
		OrgID: n.OrgID, ProjectID: n.ProjectID, EnvironmentID: n.EnvironmentID,
		TaskDeclaredID: n.TaskDeclaredID, PayloadPresent: n.PayloadPresent, Payload: n.Payload,
		ComputerID: n.ComputerID, QueueName: n.QueueName, ConcurrencyKey: n.ConcurrencyKey,
		Priority: n.Priority, QueuedTTLMS: n.QueuedTTLMS, RetryPolicy: n.RetryPolicy,
		Metadata: n.Metadata, Tags: n.Tags,
	}
}

func normalizeTaskStart(request taskStartRequest) (normalizedTaskStart, error) {
	if request.OrgID == uuid.Nil() || request.ProjectID == uuid.Nil() ||
		request.EnvironmentID == uuid.Nil() {
		return normalizedTaskStart{}, run.ErrTaskStartInvalid
	}
	if err := api.ValidateDefinitionID(request.TaskDeclaredID); err != nil {
		return normalizedTaskStart{}, fmt.Errorf("%w: %v", run.ErrTaskStartInvalid, err)
	}
	if request.ComputerID == uuid.Nil() {
		return normalizedTaskStart{}, run.ErrTaskStartInvalid
	}
	computerRaw, err := json.Marshal(api.ComputerIDTarget{ID: request.ComputerID.String()})
	if err != nil {
		return normalizedTaskStart{}, fmt.Errorf("%w: encode computer", run.ErrTaskStartInvalid)
	}
	computer, err := canonicalJSON(computerRaw)
	if err != nil {
		return normalizedTaskStart{}, fmt.Errorf("%w: canonicalize computer", run.ErrTaskStartInvalid)
	}
	if request.PayloadPresent {
		payload, err := canonicalJSON(request.Payload)
		if err != nil || len(payload) > maxTaskPayloadBytes {
			return normalizedTaskStart{}, fmt.Errorf(
				"%w: payload must be unambiguous JSON no larger than %d bytes",
				run.ErrTaskStartInvalid,
				maxTaskPayloadBytes,
			)
		}
		request.Payload = payload
	} else {
		request.Payload = nil
	}
	request.Metadata, err = run.NormalizeMetadata(request.Metadata, run.MaxMetadataBytes, "run")
	if err != nil {
		return normalizedTaskStart{}, fmt.Errorf("%w: %v", run.ErrTaskStartInvalid, err)
	}
	request.Tags, err = normalizeTags(request.Tags, maxTags, "run")
	if err != nil {
		return normalizedTaskStart{}, fmt.Errorf("%w: %v", run.ErrTaskStartInvalid, err)
	}
	if request.QueueName != "" {
		if err := definition.ValidateQueueName(request.QueueName); err != nil {
			return normalizedTaskStart{}, fmt.Errorf("%w: %v", run.ErrTaskStartInvalid, err)
		}
	}
	if request.ConcurrencyKey != nil {
		value := *request.ConcurrencyKey
		if len(value) == 0 || len(value) > 512 || !utf8.ValidString(value) ||
			strings.IndexByte(value, 0) >= 0 || hasInvalidConcurrencyKeyEdge(value) {
			return normalizedTaskStart{}, fmt.Errorf("%w: concurrency key is invalid", run.ErrTaskStartInvalid)
		}
		request.ConcurrencyKey = &value
	}
	if request.QueuedTTLMS != nil &&
		(*request.QueuedTTLMS < 1 || *request.QueuedTTLMS > maxQueuedRunTTLMS) {
		return normalizedTaskStart{}, fmt.Errorf("%w: queued TTL is invalid", run.ErrTaskStartInvalid)
	}
	if len(request.RetryPolicy) > 0 {
		retryPolicy, err := canonicalJSON(request.RetryPolicy)
		if err != nil {
			return normalizedTaskStart{}, fmt.Errorf("%w: retry is invalid", run.ErrTaskStartInvalid)
		}
		if _, err := definition.ParseRetry(retryPolicy); err != nil {
			return normalizedTaskStart{}, fmt.Errorf("%w: retry is invalid: %v", run.ErrTaskStartInvalid, err)
		}
		request.RetryPolicy = retryPolicy
	}
	return normalizedTaskStart{
		taskStartRequest: request,
		fingerprint: idempotency.TaskStartFingerprint{
			PayloadPresent: request.PayloadPresent, Payload: request.Payload,
			Computer: computer, QueueName: request.QueueName,
			ConcurrencyKey: request.ConcurrencyKey, Priority: request.Priority,
			QueuedTTLMS: request.QueuedTTLMS, RetryPolicy: request.RetryPolicy,
			Metadata: request.Metadata, Tags: request.Tags,
		},
	}, nil
}
