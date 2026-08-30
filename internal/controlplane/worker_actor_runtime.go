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

func decodeWorkerActorRequest(r *http.Request, destination any, label string) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		return fmt.Errorf("invalid %s JSON: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid %s JSON: trailing value", label)
	}
	return nil
}

func (s *Server) workerStartActor(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.StartActorRequest
	if err := decodeWorkerActorRequest(r, &request, "actor start"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	correlationID, err := parseCanonicalUUID("correlation_id", request.CorrelationID)
	if err != nil || correlationID == uuid.Nil() {
		writeError(w, badRequest(errors.New("actor start correlation_id is invalid")))
		return
	}
	if !request.InputPresent && len(request.Input) != 0 {
		writeError(w, badRequest(errors.New("actor start input is present without input_present")))
		return
	}
	if request.InputPresent && len(request.Input) == 0 {
		writeError(w, badRequest(errors.New("actor start input_present requires input")))
		return
	}
	start := api.ActorStartOptions{
		Key: request.Key, Input: request.Input,
		Workspace: request.Workspace, Run: request.Run,
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
	sourceWorkspaceID, workspaceErr := pgvalue.UUIDValue(source.WorkspaceID)
	if orgErr != nil || projectErr != nil || environmentErr != nil || workspaceErr != nil {
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
	normalized.DisallowedWorkspaceID = sourceWorkspaceID
	normalized.Authorize = func(ctx context.Context, q db.Querier) error {
		_, err := authorizeWorkerRunSource(ctx, q, worker, request.Lease)
		return err
	}
	result, err := s.startActor(r.Context(), normalized)
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) {
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
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.SessionReferenceRequest
	if err := decodeWorkerActorRequest(r, &request, "session status"); err != nil {
		writeError(w, badRequest(err))
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
		source, err := authorizeWorkerRunSource(r.Context(), work.q, worker, request.Lease)
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
		if errors.Is(err, errStaleWorkerRunSource) {
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
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.CloseSessionRequest
	if err := decodeWorkerActorRequest(r, &request, "session close"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	sessionID, err := parseWorkerSessionReference(request.SessionReferenceRequest)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	var source workerRunSourceAuthority
	var actor db.Session
	err = s.inTx(r.Context(), func(work *txWork) error {
		source, err = authorizeWorkerRunSource(r.Context(), work.q, worker, request.Lease)
		if err != nil {
			return err
		}
		actor, err = work.q.GetActor(r.Context(), db.GetActorParams{
			EnvironmentID: source.EnvironmentID, ID: sessionID,
		})
		return err
	})
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) {
			s.writeWorkerActorSourceError(w, "close", request.Lease.ID, err)
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, workerapi.CloseSessionResponse{
				CorrelationID: request.CorrelationID,
				Failed: &workerapi.RuntimeOperationFailure{
					Code: "session_not_found", Message: "Session was not found",
				},
			})
			return
		}
		writeError(w, errors.New("resolve run-sourced session close"))
		return
	}
	environmentID, _ := pgvalue.UUIDValue(source.EnvironmentID)
	actorID, _ := pgvalue.UUIDValue(actor.ID)
	workspaceID, _ := pgvalue.UUIDValue(actor.WorkspaceID)
	receipt, err := s.closeActor(r.Context(), actorCloseRequest{
		EnvironmentID: environmentID, SessionID: actorID,
		WorkspaceID:    workspaceID,
		IdempotencyKey: idempotencyKey,
		Authorize: func(ctx context.Context, q db.Querier) error {
			_, err := authorizeWorkerRunSource(ctx, q, worker, request.Lease)
			return err
		},
	})
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) {
			s.writeWorkerActorSourceError(w, "close", request.Lease.ID, err)
			return
		}
		if failure, ok := workerSessionCloseFailure(err); ok {
			writeJSON(w, http.StatusOK, workerapi.CloseSessionResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		writeError(w, errors.New("close run-sourced actor"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.CloseSessionResponse{
		CorrelationID: request.CorrelationID, Completed: &receipt,
	})
}

func (s *Server) workerReadSessionOutputPage(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.ReadSessionOutputPageRequest
	if err := decodeWorkerActorRequest(r, &request, "session output page"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	sessionID, err := parseWorkerSessionReference(request.SessionReferenceRequest)
	if err != nil || request.Limit < 1 || request.Limit > sessionOutputMaxLimit ||
		(request.After != nil && (*request.After < 0 || *request.After > maxSessionOutputSequence)) {
		writeError(w, badRequest(errors.New("session output page request is invalid")))
		return
	}
	worker := workerFromContext(r.Context())
	var page api.SessionOutputPage
	err = s.inTx(r.Context(), func(work *txWork) error {
		source, err := authorizeWorkerRunSource(r.Context(), work.q, worker, request.Lease)
		if err != nil {
			return err
		}
		page, err = readSessionOutputPage(
			r.Context(), work.q, source.EnvironmentID, sessionID, request.After, request.Limit,
		)
		return err
	})
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) {
			s.writeWorkerActorSourceError(w, "output-page", request.Lease.ID, err)
			return
		}
		code, message := "session_output_unavailable", "Actor output is unavailable"
		if errors.Is(err, pgx.ErrNoRows) {
			code, message = "session_not_found", "Session was not found"
		} else if errors.Is(err, errSessionOutputCursorExpired) {
			code, message = "session_output_cursor_expired", err.Error()
		} else {
			writeError(w, errors.New("read run-sourced session output"))
			return
		}
		writeJSON(w, http.StatusOK, workerapi.ReadSessionOutputPageResponse{
			CorrelationID: request.CorrelationID,
			Failed:        &workerapi.RuntimeOperationFailure{Code: code, Message: message},
		})
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ReadSessionOutputPageResponse{
		CorrelationID: request.CorrelationID, Completed: &page,
	})
}

func (s *Server) workerRunSource(
	ctx context.Context,
	worker workerActor,
	lease workerapi.RunLeaseFence,
) (workerRunSourceAuthority, error) {
	var source workerRunSourceAuthority
	err := s.inTx(ctx, func(work *txWork) error {
		var err error
		source, err = authorizeWorkerRunSource(ctx, work.q, worker, lease)
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
	var claimConflict idempotency.ConflictError
	var keyConflict ActorKeyConflictError
	switch {
	case errors.As(err, &claimConflict):
		return runtimeOperationFailure("idempotency_conflict", "idempotency key conflicts with an earlier Actor start", false), true
	case errors.As(err, &keyConflict):
		return runtimeOperationFailure("actor_key_conflict", keyConflict.Error(), false), true
	case errors.Is(err, errActorStartNotDeployed):
		return runtimeOperationFailure("actor_not_deployed", err.Error(), false), true
	case errors.Is(err, errActorStartWorkspaceNotFound):
		return runtimeOperationFailure("workspace_not_found", err.Error(), false), true
	case errors.Is(err, errActorStartWorkspaceConflict):
		return runtimeOperationFailure("workspace_unavailable", err.Error(), true), true
	case errors.Is(err, errActorStartSecretUnavailable):
		return runtimeOperationFailure("secret_unavailable", err.Error(), false), true
	case errors.Is(err, errActorInputTooLarge), errors.Is(err, errActorStartInvalid):
		return runtimeOperationFailure("invalid_actor_start", err.Error(), false), true
	default:
		return workerapi.RuntimeOperationFailure{}, false
	}
}

func workerSessionCloseFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	var claimConflict idempotency.ConflictError
	switch {
	case errors.As(err, &claimConflict):
		return runtimeOperationFailure("idempotency_conflict", "idempotency key conflicts with an earlier Actor close", false), true
	case errors.Is(err, errActorCloseConflict):
		return runtimeOperationFailure("actor_close_conflict", err.Error(), false), true
	case errors.Is(err, errActorCloseAuthority):
		return runtimeOperationFailure("session_not_found", "Session was not found", false), true
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
	if errors.Is(err, errStaleWorkerRunSource) {
		writeError(w, conflict(errStaleWorkerRunSource))
		return
	}
	s.log.Error("authorize worker Actor operation source",
		"operation", operation, "run_id", runID, "error", err)
	writeError(w, errors.New("authorize worker actor operation source"))
}
