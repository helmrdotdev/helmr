package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const maxActorOutputBytes = 1 << 20

var (
	errStaleActorOutputAppend = errors.New("actor output append source authority is stale")
	errActorOutputTooLarge    = errors.New("actor output exceeds the maximum size")
)

type parsedWorkerActorOutputAppend struct {
	turnID            uuid.UUID
	generation        int64
	messageDeliveryID uuid.UUID
	lease             parsedRunLeaseFence
	correlationID     uuid.UUID
	data              json.RawMessage
	idempotencyKey    string
}

func (s *Server) workerWriteTurnOutput(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.WriteTurnOutputRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeError(w, badRequest(fmt.Errorf("invalid actor output append JSON: %w", err)))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, badRequest(errors.New("invalid actor output append JSON: trailing value")))
		return
	}
	parsed, err := parseWorkerActorOutputAppend(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())

	record, err := s.appendActorOutput(r.Context(), worker, request, parsed)
	if err != nil {
		if failure, ok := actorOutputAppendFailure(err); ok {
			writeJSON(w, http.StatusOK, workerapi.WriteOutputResponse{
				CorrelationID: request.CorrelationID,
				Failed:        &failure,
			})
			return
		}
		if writeStaleWorkerClaims(w, err) {
			return
		}
		if errors.Is(err, errStaleActorOutputAppend) {
			writeError(w, conflict(errStaleActorOutputAppend))
			return
		}
		s.log.Error("append Actor output", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("append actor output"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.WriteOutputResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &record,
	})
}

func parseWorkerActorOutputAppend(
	request workerapi.WriteTurnOutputRequest,
) (parsedWorkerActorOutputAppend, error) {
	lease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		return parsedWorkerActorOutputAppend{}, err
	}
	correlationID, err := parseCanonicalUUID("correlation_id", request.CorrelationID)
	if err != nil {
		return parsedWorkerActorOutputAppend{}, err
	}
	turnID, err := parseCanonicalUUID("turn_id", request.TurnID)
	if err != nil {
		return parsedWorkerActorOutputAppend{}, err
	}
	if request.RunGeneration <= 0 {
		return parsedWorkerActorOutputAppend{}, errors.New("run_generation must be positive")
	}
	var delivery uuid.UUID
	if request.MessageDeliveryID != nil {
		delivery, err = parseCanonicalUUID("message_delivery_id", *request.MessageDeliveryID)
		if err != nil {
			return parsedWorkerActorOutputAppend{}, err
		}
	}
	canonical, err := canonicalJSON(request.Data)
	if err != nil {
		return parsedWorkerActorOutputAppend{}, errors.New("data must be valid JSON")
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		return parsedWorkerActorOutputAppend{}, err
	}
	return parsedWorkerActorOutputAppend{
		lease:  lease,
		turnID: turnID, generation: request.RunGeneration,
		messageDeliveryID: delivery,
		correlationID:     correlationID,
		data:              canonical,
		idempotencyKey:    idempotencyKey,
	}, nil
}

func (s *Server) appendActorOutput(
	ctx context.Context,
	worker workerActor,
	request workerapi.WriteTurnOutputRequest,
	parsed parsedWorkerActorOutputAppend,
) (api.SessionEvent, error) {
	if len(parsed.data) > maxActorOutputBytes {
		return api.SessionEvent{}, errActorOutputTooLarge
	}
	locatorParams := db.GetLiveRunLeaseLocatorsParams{
		ID:               pgvalue.UUID(parsed.lease.leaseID),
		LeaseSequence:    request.Lease.LeaseSequence,
		WorkerGroupID:    pgvalue.UUID(worker.WorkerGroupID),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:      worker.WorkerEpoch,
	}
	discovered, err := s.db.GetLiveRunLeaseLocators(ctx, locatorParams)
	if err != nil || !discovered.SessionID.Valid {
		return api.SessionEvent{}, staleActorOutputAppend(err)
	}
	environmentID, err := pgvalue.UUIDValue(discovered.EnvironmentID)
	if err != nil {
		return api.SessionEvent{}, errStaleActorOutputAppend
	}
	actorID, err := pgvalue.UUIDValue(discovered.SessionID)
	if err != nil {
		return api.SessionEvent{}, errStaleActorOutputAppend
	}
	var response api.SessionEvent
	var rejected error
	err = s.inTx(ctx, func(work *txWork) error {
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, locatorParams)
		if err != nil ||
			locators.EnvironmentID != discovered.EnvironmentID ||
			locators.SessionID != discovered.SessionID {
			return staleActorOutputAppend(err)
		}
		if _, err := secret.LockAttemptDelivery(
			ctx, work.q, locators.RunID, locators.AttemptNumber, locators.WorkspaceID,
		); err != nil {
			return fmt.Errorf("lock actor output secret authority: %w", err)
		}
		owner, err := lockRunFinalizationOwner(ctx, work.q, locators)
		if err != nil || !owner.actor.ID.Valid {
			return staleActorOutputAppend(err)
		}
		authority, err := lockLiveRunLeaseAuthority(
			ctx,
			work.q,
			worker,
			pgvalue.UUID(parsed.lease.leaseID),
			request.Lease.LeaseSequence,
			locators,
		)
		if err != nil {
			return staleActorOutputAppend(err)
		}
		authority.actor = owner.actor
		if authority.run.ParentRunID.Valid ||
			authority.run.EntrypointKind != "actor" ||
			authority.run.SessionID != authority.actor.ID ||
			authority.actor.CurrentRunID != authority.run.ID ||
			(authority.actor.Status != "open" && authority.actor.Status != "closing") ||
			authority.run.Status != db.RunStatusRunning ||
			authority.runLease.Status != db.RunLeaseStatusRunning ||
			!authority.run.ActiveStartedAt.Valid ||
			!authority.attempt.EntrypointEnteredAt.Valid ||
			authority.attempt.TerminalAt.Valid ||
			authority.runLease.FinalizationOperationID.Valid {
			return errStaleActorOutputAppend
		}
		key := parsed.idempotencyKey
		if key == "" {
			key = parsed.correlationID.String()
		}
		receipt, err := session.AppendTurnOutput(ctx, work.q, session.TurnScope{
			EnvironmentID: environmentID, SessionID: actorID, TurnID: parsed.turnID,
			RunID: pgvalue.MustUUIDValue(authority.run.ID), AttemptNumber: authority.attempt.Number, RunGeneration: parsed.generation, MessageDeliveryID: parsed.messageDeliveryID,
		}, key, parsed.data)
		if err != nil {
			return err
		}
		if receipt.Code != "" {
			rejected = &session.OperationError{Code: receipt.Code}
			return nil
		}
		response = projectWorkerSessionEvent(receipt.Event, authority.run.DeploymentID)
		return nil
	})
	if err == nil {
		err = rejected
	}
	return response, err
}

func staleActorOutputAppend(err error) error {
	if err == nil {
		return errStaleActorOutputAppend
	}
	return errors.Join(errStaleActorOutputAppend, err)
}

func actorOutputAppendFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	var conflictError idempotency.ConflictError
	var operation *session.OperationError
	switch {
	case errors.As(err, &operation):
		return runtimeOperationFailure(operation.Code, operation.Error(), false), true
	case errors.Is(err, session.ErrTurnStopped):
		return workerapi.RuntimeOperationFailure{Code: "turn_stopping", Message: err.Error()}, true
	case errors.Is(err, session.ErrTurnNotActive):
		return workerapi.RuntimeOperationFailure{Code: "turn_not_active", Message: err.Error()}, true
	case errors.Is(err, session.ErrTurnScope):
		return workerapi.RuntimeOperationFailure{Code: "stale_execution", Message: err.Error()}, true
	case errors.As(err, &conflictError):
		return workerapi.RuntimeOperationFailure{
			Code: "idempotency_conflict", Message: "idempotency key conflicts with an earlier Actor output",
		}, true
	case errors.Is(err, errActorOutputTooLarge):
		return workerapi.RuntimeOperationFailure{Code: "actor_output_too_large", Message: err.Error()}, true
	default:
		return workerapi.RuntimeOperationFailure{}, false
	}
}
