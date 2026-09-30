package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerStartActor(w http.ResponseWriter, r *http.Request) {
	var request workerapi.StartActorRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid actor start JSON: %w", err))
		return
	}
	correlationID, err := parseCanonicalUUID("correlation_id", request.CorrelationID)
	if err != nil || correlationID == uuid.Nil() {
		writeError(w, badRequest(errors.New("actor start correlation_id is invalid")))
		return
	}
	start := api.ActorStartOptions{
		Key:      request.Key,
		Computer: request.Computer, Run: request.Run,
	}
	if err := api.ValidateActorDeclaredID(request.ActorDeclaredID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := api.ValidateActorStartOptions(start); err != nil {
		writeError(w, badRequest(err))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	source, err := s.workerRunSource(r.Context(), worker, request.Lease)
	if err != nil {
		s.writeWorkerActorSourceError(w, "start", request.Lease.ID, err)
		return
	}
	orgID, orgErr := pgvalue.UUIDValue(source.OrgID)
	projectID, projectErr := pgvalue.UUIDValue(source.ProjectID)
	environmentID, environmentErr := pgvalue.UUIDValue(source.EnvironmentID)
	if orgErr != nil || projectErr != nil || environmentErr != nil {
		writeError(w, errors.New("actor start source locators are invalid"))
		return
	}
	normalized, err := actorStartRequestFromScope(
		orgID, projectID, environmentID, request.ActorDeclaredID,
		idempotencyKey, start,
	)
	if err != nil {
		writeJSON(w, http.StatusOK, failedWorkerActorStart(
			request.CorrelationID, "invalid_actor_start", err.Error(), false,
		))
		return
	}
	normalized.Authorize = func(ctx context.Context, tx pgx.Tx) error {
		_, err := authorizeWorkerSessionOperation(ctx, tx, worker, request.Lease, pgtype.UUID{}, pgvalue.UUID(normalized.ComputerID))
		return err
	}
	result, err := s.startActor(r.Context(), normalized)
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) || errors.Is(err, workergroup.ErrStaleClaims) {
			s.writeWorkerActorSourceError(w, "start", request.Lease.ID, err)
			return
		}
		if failure, ok := workerActorStartFailure(err); ok {
			writeJSON(w, http.StatusOK, workerapi.StartActorResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		s.log.Error("start run-sourced Actor", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("start run-sourced actor"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.StartActorResponse{
		CorrelationID: request.CorrelationID,
		Completed: &api.StartActorResponse{
			SessionID: result.SessionID.String(), RunID: result.BootRunID.String(),
		},
	})
}

func (s *Server) workerGetSessionStatus(w http.ResponseWriter, r *http.Request) {
	var request workerapi.SessionReferenceRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid session status JSON: %w", err))
		return
	}
	sessionID, err := parseWorkerSessionReference(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	var status api.Session
	err = s.inTx(r.Context(), func(work *txWork) error {
		source, err := authorizeWorkerRunSource(r.Context(), work.tx, worker, request.Lease)
		if err != nil {
			return err
		}
		row, err := work.q.GetSessionSnapshot(r.Context(), db.GetSessionSnapshotParams{
			OrgID: source.OrgID, ProjectID: source.ProjectID,
			EnvironmentID: source.EnvironmentID, ID: sessionID,
		})
		if err != nil {
			return err
		}
		status, err = projectSession(sessionProjectionFromGetRow(row))
		return err
	})
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) || errors.Is(err, workergroup.ErrStaleClaims) {
			s.writeWorkerActorSourceError(w, "status", request.Lease.ID, err)
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, failedWorkerSessionReference(
				request.CorrelationID, "session_not_found", "Session was not found",
			))
			return
		}
		writeError(w, errors.New("read run-sourced session status"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.SessionStatusResponse{
		CorrelationID: request.CorrelationID, Completed: &status,
	})
}

func (s *Server) workerCloseSession(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CloseSessionRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Session close JSON: %w", err))
		return
	}
	targetID, err := parseWorkerSessionReference(request.SessionReferenceRequest)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var receipt session.ControlReceipt
	err = s.inTx(r.Context(), func(work *txWork) error {
		source, err := authorizeWorkerSessionOperation(r.Context(), work.tx, workerFromContext(r.Context()), request.Lease, targetID, pgtype.UUID{})
		if err != nil {
			return err
		}
		receipt, err = session.Close(r.Context(), work.q, session.ControlRequest{Target: session.Target{EnvironmentID: pgvalue.MustUUIDValue(source.EnvironmentID), SessionID: pgvalue.MustUUIDValue(targetID)}, IdempotencyKey: request.IdempotencyKey})
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &session.OperationError{Code: receipt.Code}
	}
	if err != nil {
		s.writeWorkerSessionCommand(w, request.CorrelationID, err)
		return
	}
	response := api.SessionCloseReceipt{ID: receipt.ID.String(), SessionID: receipt.SessionID.String(), Status: receipt.Status}
	writeJSON(w, http.StatusOK, workerapi.CloseSessionResponse{CorrelationID: request.CorrelationID, Completed: &response})
}
func (s *Server) workerCancelSession(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CancelSessionRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Session cancel JSON: %w", err))
		return
	}
	targetID, err := parseWorkerSessionReference(request.SessionReferenceRequest)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var receipt session.ControlReceipt
	err = s.inTx(r.Context(), func(work *txWork) error {
		source, graph, _, err := lockWorkerSessionControl(r.Context(), work, workerFromContext(r.Context()), request.Lease, targetID, true)
		if err != nil {
			return err
		}
		receipt, err = session.Cancel(r.Context(), work.q, session.ControlRequest{Target: session.Target{EnvironmentID: pgvalue.MustUUIDValue(source.EnvironmentID), SessionID: pgvalue.MustUUIDValue(targetID)}, IdempotencyKey: request.IdempotencyKey}, graph)
		return err
	})
	if err == nil && receipt.Code != "" {
		err = &session.OperationError{Code: receipt.Code}
	}
	if err != nil {
		s.writeWorkerSessionCommand(w, request.CorrelationID, err)
		return
	}
	response := api.SessionCancelReceipt{ID: receipt.ID.String(), SessionID: receipt.SessionID.String(), Status: receipt.Status}
	writeJSON(w, http.StatusOK, workerapi.CancelSessionResponse{CorrelationID: request.CorrelationID, Completed: &response})
}
func (s *Server) workerReadSessionEvents(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ReadSessionEventsRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Session events JSON: %w", err))
		return
	}
	targetID, err := parseWorkerSessionReference(request.SessionReferenceRequest)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var page session.EventPage
	after := int64(0)
	if request.After != nil {
		after = *request.After
	}
	err = s.inTx(r.Context(), func(work *txWork) error {
		source, err := authorizeWorkerSessionOperation(r.Context(), work.tx, workerFromContext(r.Context()), request.Lease, targetID, pgtype.UUID{})
		if err != nil {
			return err
		}
		page, err = session.ReadEvents(r.Context(), work.q, session.Target{EnvironmentID: pgvalue.MustUUIDValue(source.EnvironmentID), SessionID: pgvalue.MustUUIDValue(targetID)}, after, request.Limit)
		return err
	})
	if err != nil {
		s.writeWorkerSessionCommand(w, request.CorrelationID, err)
		return
	}
	response := api.SessionEventPage{Records: make([]api.SessionEvent, 0, len(page.Records)), NextAfter: page.NextAfter, HasMore: page.HasMore, RetainedAfter: page.RetainedAfter}
	for _, row := range page.Records {
		response.Records = append(response.Records, projectWorkerSessionEvent(db.SessionEvent{ID: row.ID, SessionID: row.SessionID, TurnID: row.TurnID, Sequence: row.Sequence, CreatedAt: row.CreatedAt, Kind: row.Kind, Data: row.Data, ProducerRunID: row.ProducerRunID, ProducerAttemptNumber: row.ProducerAttemptNumber, RunGeneration: row.RunGeneration}, row.DeploymentID))
	}
	writeJSON(w, http.StatusOK, workerapi.ReadSessionEventsResponse{CorrelationID: request.CorrelationID, Completed: &response})
}

func (s *Server) workerRunSource(
	ctx context.Context,
	worker workergroup.HostPrincipal,
	lease workerapi.RunLeaseFence,
) (workerRunSourceAuthority, error) {
	var source workerRunSourceAuthority
	err := s.inTx(ctx, func(work *txWork) error {
		var err error
		source, err = authorizeWorkerRunSource(ctx, work.tx, worker, lease)
		return err
	})
	return source, err
}

func parseWorkerSessionReference(
	request workerapi.SessionReferenceRequest,
) (pgtype.UUID, error) {
	if _, err := parseCanonicalUUID("correlation_id", request.CorrelationID); err != nil {
		return pgtype.UUID{}, err
	}
	id, err := ids.Parse(request.SessionID)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgvalue.UUID(id), nil
}

func workerActorStartFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	var operation *session.OperationError
	if errors.As(err, &operation) {
		return runtimeOperationFailure(operation.Code, operation.Code, false), true
	}
	var expired idempotency.ExpiredError
	if errors.As(err, &expired) {
		return workerapi.RuntimeOperationFailure{Code: expired.ErrorCode(), Message: expired.Error()}, true
	}
	var claimConflict idempotency.ConflictError
	var keyConflict ActorKeyConflictError
	switch {
	case errors.As(err, &claimConflict):
		return runtimeOperationFailure("idempotency_conflict", "idempotency key conflicts with an earlier Actor start", false), true
	case errors.As(err, &keyConflict):
		return runtimeOperationFailure("actor_key_conflict", keyConflict.Error(), false), true
	case errors.Is(err, errActorStartNotDeployed):
		return runtimeOperationFailure("actor_not_deployed", err.Error(), false), true
	case errors.Is(err, errActorStartComputerNotFound):
		return runtimeOperationFailure("computer_not_found", err.Error(), false), true
	case errors.Is(err, errActorStartComputerConflict):
		return runtimeOperationFailure("computer_unavailable", err.Error(), true), true
	case errors.Is(err, errActorStartSecretUnavailable):
		return runtimeOperationFailure("secret_unavailable", err.Error(), false), true
	case errors.Is(err, errActorStartInvalid):
		return runtimeOperationFailure("invalid_actor_start", err.Error(), false), true
	default:
		return workerapi.RuntimeOperationFailure{}, false
	}
}

func runtimeOperationFailure(code, message string, retryable bool) workerapi.RuntimeOperationFailure {
	return workerapi.RuntimeOperationFailure{
		Code: strings.TrimSpace(code), Message: strings.TrimSpace(message),
		Retryable: retryable,
	}
}

func failedWorkerActorStart(
	correlationID, code, message string,
	retryable bool,
) workerapi.StartActorResponse {
	failure := runtimeOperationFailure(code, message, retryable)
	return workerapi.StartActorResponse{
		CorrelationID: correlationID, Failed: &failure,
	}
}

func failedWorkerSessionReference(
	correlationID, code, message string,
) workerapi.SessionStatusResponse {
	failure := runtimeOperationFailure(code, message, false)
	return workerapi.SessionStatusResponse{
		CorrelationID: correlationID, Failed: &failure,
	}
}

func (s *Server) writeWorkerActorSourceError(
	w http.ResponseWriter,
	operation string,
	runID string,
	err error,
) {
	if writeStaleWorkerClaims(w, err) {
		return
	}
	if errors.Is(err, errStaleWorkerRunSource) {
		writeError(w, conflict(errStaleWorkerRunSource))
		return
	}
	s.log.Error("authorize worker Actor operation source",
		"operation", operation, "run_id", runID, "error", err)
	writeError(w, errors.New("authorize worker actor operation source"))
}
