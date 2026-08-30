package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleActorInputSend = errors.New("actor input send source authority is stale")

type parsedWorkerActorInputSend struct {
	lease           parsedRunLeaseFence
	correlationID   uuid.UUID
	idempotencyKey  string
	targetSessionID uuid.UUID
}

func (s *Server) workerSendActorInput(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.SendActorInputRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeError(w, badRequest(fmt.Errorf("invalid actor input send JSON: %w", err)))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, badRequest(errors.New("invalid actor input send JSON: trailing value")))
		return
	}
	parsed, err := parseWorkerActorInputSend(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())

	source, err := s.db.GetActorInputSendSource(r.Context(), actorInputSendSourceParams(request, parsed, worker))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errStaleActorInputSend))
		return
	}
	if err != nil {
		s.log.Error("load Actor input send source", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("load actor input send source"))
		return
	}
	target, err := s.db.GetActor(r.Context(), db.GetActorParams{
		EnvironmentID: source.EnvironmentID, ID: pgvalue.UUID(parsed.targetSessionID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		if err := s.inTx(r.Context(), func(work *txWork) error {
			return authorizeActorInputSendSource(
				r.Context(), work.q, worker, request, source.EnvironmentID,
			)
		}); err != nil {
			if errors.Is(err, errStaleActorInputSend) {
				writeError(w, conflict(errStaleActorInputSend))
				return
			}
			s.log.Error("authorize unresolved Actor input send", "run_lease_id", request.Lease.ID, "error", err)
			writeError(w, errors.New("authorize actor input send"))
			return
		}
		writeJSON(w, http.StatusOK, failedActorInputSend(
			request.CorrelationID, "actor_not_found", "Actor was not found", false,
		))
		return
	}
	if err != nil {
		s.log.Error("resolve Actor input send target", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("resolve actor input send target"))
		return
	}
	record, err := s.appendActorInput(r.Context(), appendActorInputRequest{
		EnvironmentID:  pgvalue.MustUUIDValue(source.EnvironmentID),
		SessionID:      pgvalue.MustUUIDValue(target.ID),
		RecordID:       uuid.NewV7(),
		Data:           request.Input,
		SourceKind:     "run",
		SourceRunID:    pgvalue.MustUUIDValue(source.RunID),
		IdempotencyKey: parsed.idempotencyKey,
		Authorize: func(ctx context.Context, q db.Querier) error {
			return authorizeActorInputSendSource(ctx, q, worker, request, source.EnvironmentID)
		},
	})
	if err != nil {
		if failure, ok := actorInputSendFailure(err); ok {
			writeJSON(w, http.StatusOK, failedActorInputSend(
				request.CorrelationID, failure.Code, failure.Message, failure.Retryable,
			))
			return
		}
		if errors.Is(err, errStaleActorInputSend) {
			writeError(w, conflict(errStaleActorInputSend))
			return
		}
		s.log.Error("append run-sourced Actor input", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("append run-sourced actor input"))
		return
	}
	completed, err := projectSessionInput(record)
	if err != nil {
		writeError(w, errors.New("project run-sourced session input"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.SendActorInputResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &completed,
	})
}

func parseWorkerActorInputSend(request workerapi.SendActorInputRequest) (parsedWorkerActorInputSend, error) {
	lease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		return parsedWorkerActorInputSend{}, err
	}
	correlationID, err := parseCanonicalUUID("correlation_id", request.CorrelationID)
	if err != nil {
		return parsedWorkerActorInputSend{}, err
	}
	targetSessionID, err := ids.Parse(request.SessionID)
	if err != nil {
		return parsedWorkerActorInputSend{}, err
	}
	if err := api.ValidateSendSessionInputRequest(api.SendSessionInputRequest{
		Input: request.Input, IdempotencyKey: request.IdempotencyKey,
	}); err != nil {
		return parsedWorkerActorInputSend{}, err
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		return parsedWorkerActorInputSend{}, err
	}
	return parsedWorkerActorInputSend{
		lease: lease, correlationID: correlationID, idempotencyKey: idempotencyKey,
		targetSessionID: targetSessionID,
	}, nil
}

func actorInputSendSourceParams(
	request workerapi.SendActorInputRequest,
	parsed parsedWorkerActorInputSend,
	worker workerActor,
) db.GetActorInputSendSourceParams {
	return db.GetActorInputSendSourceParams{
		ID: pgvalue.UUID(parsed.lease.leaseID), LeaseSequence: request.Lease.LeaseSequence,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch}
}

func authorizeActorInputSendSource(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	request workerapi.SendActorInputRequest,
	environmentID pgtype.UUID,
) error {
	authority, err := authorizeWorkerRunSource(ctx, q, worker, request.Lease)
	if err != nil || authority.EnvironmentID != environmentID {
		if err != nil {
			return fmt.Errorf("%w: %v", errStaleActorInputSend, err)
		}
		return errStaleActorInputSend
	}
	return nil
}

func actorInputSendFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	var conflictError idempotency.ConflictError
	switch {
	case errors.As(err, &conflictError):
		return workerapi.RuntimeOperationFailure{
			Code: "idempotency_conflict", Message: "idempotency key conflicts with an earlier Actor input",
		}, true
	case errors.Is(err, errActorInputTooLarge):
		return workerapi.RuntimeOperationFailure{Code: "actor_input_too_large", Message: err.Error()}, true
	case errors.Is(err, errActorSequenceExhausted):
		return workerapi.RuntimeOperationFailure{Code: "actor_sequence_exhausted", Message: err.Error()}, true
	case errors.Is(err, errActorInputUnavailable):
		return workerapi.RuntimeOperationFailure{Code: "actor_not_open", Message: "Actor does not accept new input"}, true
	case errors.Is(err, errActorInputAppendConflict):
		return workerapi.RuntimeOperationFailure{Code: "actor_input_conflict", Message: err.Error()}, true
	default:
		return workerapi.RuntimeOperationFailure{}, false
	}
}

func failedActorInputSend(
	correlationID string,
	code string,
	message string,
	retryable bool,
) workerapi.SendActorInputResponse {
	return workerapi.SendActorInputResponse{
		CorrelationID: correlationID,
		Failed: &workerapi.RuntimeOperationFailure{
			Code: strings.TrimSpace(code), Message: strings.TrimSpace(message), Retryable: retryable,
		},
	}
}
