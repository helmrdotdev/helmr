package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleActorInputSend = errors.New("Actor input send source authority is stale")

type parsedWorkerActorInputSend struct {
	lease          parsedRunLeaseReceipt
	correlationID  uuid.UUID
	idempotencyKey string
}

func (s *Server) workerSendActorInput(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerSendActorInputRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeError(w, badRequest(fmt.Errorf("invalid Actor input send JSON: %w", err)))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, badRequest(errors.New("invalid Actor input send JSON: trailing value")))
		return
	}
	parsed, err := parseWorkerActorInputSend(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerGroupID != worker.WorkerGroupID ||
		parsed.lease.workerInstanceID != worker.WorkerInstanceID {
		writeError(w, forbidden(errors.New("Actor input send belongs to another worker")))
		return
	}
	if request.Lease.WorkerEpoch != worker.WorkerEpoch ||
		request.Lease.WorkerProtocolVersion != worker.ProtocolVersion {
		writeError(w, conflict(errStaleActorInputSend))
		return
	}

	source, err := s.db.GetActorInputSendSource(r.Context(), actorInputSendSourceParams(request, parsed))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errStaleActorInputSend))
		return
	}
	if err != nil {
		s.log.Error("load Actor input send source", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("load Actor input send source"))
		return
	}
	targetRequest := api.SendActorInputRequest{
		ActorID: request.ActorID, ActorKey: request.ActorKey,
		Input: request.Input, IdempotencyKey: parsed.idempotencyKey,
	}
	target, err := s.resolveActorInputAddress(r, source.EnvironmentID, request.ActorDeclaredID, targetRequest)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := s.inTx(r.Context(), func(work *txWork) error {
			return authorizeActorInputSendSource(
				r.Context(), work.q, worker, request, parsed, source.EnvironmentID,
			)
		}); err != nil {
			if errors.Is(err, errStaleActorInputSend) {
				writeError(w, conflict(errStaleActorInputSend))
				return
			}
			s.log.Error("authorize unresolved Actor input send", "run_id", request.Lease.RunID, "error", err)
			writeError(w, errors.New("authorize Actor input send"))
			return
		}
		writeJSON(w, http.StatusOK, failedActorInputSend(
			request.CorrelationID, "actor_not_found", "Actor was not found", false,
		))
		return
	}
	if err != nil {
		s.log.Error("resolve Actor input send target", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("resolve Actor input send target"))
		return
	}
	record, err := s.appendActorInput(r.Context(), appendActorInputRequest{
		EnvironmentID:  pgvalue.MustUUIDValue(source.EnvironmentID),
		ActorID:        pgvalue.MustUUIDValue(target.ID),
		RecordID:       uuid.Must(uuid.NewV7()),
		Data:           request.Input,
		SourceKind:     "run",
		SourceRunID:    parsed.lease.runID,
		IdempotencyKey: parsed.idempotencyKey,
		Authorize: func(ctx context.Context, q db.Querier) error {
			return authorizeActorInputSendSource(ctx, q, worker, request, parsed, source.EnvironmentID)
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
		s.log.Error("append run-sourced Actor input", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("append run-sourced Actor input"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerSendActorInputResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &api.SendActorInputResponse{Sequence: record.Sequence},
	})
}

func parseWorkerActorInputSend(request api.WorkerSendActorInputRequest) (parsedWorkerActorInputSend, error) {
	lease, err := parseRunLeaseReceipt(request.Lease)
	if err != nil {
		return parsedWorkerActorInputSend{}, err
	}
	correlationID, err := parseCanonicalUUID("correlation_id", request.CorrelationID)
	if err != nil {
		return parsedWorkerActorInputSend{}, err
	}
	if err := api.ValidateActorDeclaredID(request.ActorDeclaredID); err != nil {
		return parsedWorkerActorInputSend{}, err
	}
	if err := api.ValidateSendActorInputRequest(api.SendActorInputRequest{
		ActorID: request.ActorID, ActorKey: request.ActorKey, Input: request.Input,
		IdempotencyKey: request.IdempotencyKey,
	}); err != nil {
		return parsedWorkerActorInputSend{}, err
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		return parsedWorkerActorInputSend{}, err
	}
	return parsedWorkerActorInputSend{
		lease: lease, correlationID: correlationID, idempotencyKey: idempotencyKey,
	}, nil
}

func actorInputSendSourceParams(
	request api.WorkerSendActorInputRequest,
	parsed parsedWorkerActorInputSend,
) db.GetActorInputSendSourceParams {
	return db.GetActorInputSendSourceParams{
		ID: pgvalue.UUID(parsed.lease.leaseID), RunID: pgvalue.UUID(parsed.lease.runID),
		WorkspaceID: pgvalue.UUID(parsed.lease.workspaceID), AttemptNumber: request.Lease.AttemptNumber,
		LeaseSequence: request.Lease.LeaseSequence, WorkerGroupID: request.Lease.WorkerGroupID,
		WorkerInstanceID: pgvalue.UUID(parsed.lease.workerInstanceID), WorkerEpoch: request.Lease.WorkerEpoch,
		WorkerProtocolVersion:      request.Lease.WorkerProtocolVersion,
		RuntimeInstanceID:          pgvalue.UUID(parsed.lease.runtimeInstanceID),
		NetworkSlotID:              pgvalue.UUID(parsed.lease.networkSlotID),
		NetworkSlotGeneration:      request.Lease.NetworkSlotGeneration,
		RuntimeIdentityID:          request.Lease.RuntimeIdentityID,
		RequestedCpuMillis:         request.Lease.RequestedCPUMillis,
		RequestedMemoryBytes:       request.Lease.RequestedMemoryBytes,
		RequestedWorkloadDiskBytes: request.Lease.RequestedWorkloadDiskBytes,
		RequestedScratchBytes:      request.Lease.RequestedScratchBytes,
		RequestedExecutionSlots:    request.Lease.RequestedExecutionSlots,
		StartDeadlineAt:            pgtype.Timestamptz{Time: request.Lease.StartDeadlineAt, Valid: true},
		ExpiresAt:                  pgtype.Timestamptz{Time: request.Lease.ExpiresAt, Valid: true},
	}
}

func authorizeActorInputSendSource(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	request api.WorkerSendActorInputRequest,
	parsed parsedWorkerActorInputSend,
	environmentID pgtype.UUID,
) error {
	locators, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
		ID: pgvalue.UUID(parsed.lease.leaseID), LeaseSequence: request.Lease.LeaseSequence,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
	})
	if err != nil || locators.EnvironmentID != environmentID {
		return errStaleActorInputSend
	}
	authority, err := lockLiveRunLeaseAuthority(
		ctx, q, worker, pgvalue.UUID(parsed.lease.leaseID), request.Lease.LeaseSequence, locators,
	)
	if err != nil || authority.run.ID != pgvalue.UUID(parsed.lease.runID) ||
		authority.run.Status != db.RunStatusRunning ||
		authority.runLease.State != db.RunLeaseStateRunning ||
		!authority.run.ActiveStartedAt.Valid ||
		!authority.attempt.EntrypointEnteredAt.Valid ||
		authority.attempt.TerminalAt.Valid ||
		authority.runLease.FinalizationOperationID.Valid {
		return errStaleActorInputSend
	}
	current, err := projectActorTurnLease(authority)
	if err != nil || !equalRunLeaseReceipt(current, request.Lease) {
		return errStaleActorInputSend
	}
	return nil
}

func actorInputSendFailure(err error) (api.WorkerRuntimeOperationFailure, bool) {
	var conflictError idempotency.ConflictError
	switch {
	case errors.As(err, &conflictError):
		return api.WorkerRuntimeOperationFailure{
			Code: "idempotency_conflict", Message: "idempotency key conflicts with an earlier Actor input",
		}, true
	case errors.Is(err, errActorInputTooLarge):
		return api.WorkerRuntimeOperationFailure{Code: "actor_input_too_large", Message: err.Error()}, true
	case errors.Is(err, errActorSequenceExhausted):
		return api.WorkerRuntimeOperationFailure{Code: "actor_sequence_exhausted", Message: err.Error()}, true
	case errors.Is(err, errActorInputUnavailable):
		return api.WorkerRuntimeOperationFailure{Code: "actor_not_open", Message: "Actor does not accept new input"}, true
	case errors.Is(err, errActorInputAppendConflict):
		return api.WorkerRuntimeOperationFailure{Code: "actor_input_conflict", Message: err.Error()}, true
	default:
		return api.WorkerRuntimeOperationFailure{}, false
	}
}

func failedActorInputSend(
	correlationID string,
	code string,
	message string,
	retryable bool,
) api.WorkerSendActorInputResponse {
	return api.WorkerSendActorInputResponse{
		CorrelationID: correlationID,
		Failed: &api.WorkerRuntimeOperationFailure{
			Code: strings.TrimSpace(code), Message: strings.TrimSpace(message), Retryable: retryable,
		},
	}
}
