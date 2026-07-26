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
	var request api.WorkerStartActorRequest
	if err := decodeWorkerActorRequest(r, &request, "Actor start"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	correlationID, err := parseCanonicalUUID("correlation_id", request.CorrelationID)
	if err != nil || correlationID == uuid.Nil {
		writeError(w, badRequest(errors.New("Actor start correlation_id is invalid")))
		return
	}
	if !request.InputPresent && len(request.Input) != 0 {
		writeError(w, badRequest(errors.New("Actor start input is present without input_present")))
		return
	}
	if request.InputPresent && len(request.Input) == 0 {
		writeError(w, badRequest(errors.New("Actor start input_present requires input")))
		return
	}
	start := api.StartActorRequest{
		Key: request.Key, Input: request.Input, IdempotencyKey: request.IdempotencyKey,
		Workspace: request.Workspace, Run: request.Run,
	}
	if err := api.ValidateActorDeclaredID(request.ActorDeclaredID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := api.ValidateStartActorRequest(start); err != nil {
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
		s.writeWorkerActorSourceError(w, "start", request.Lease.RunID, err)
		return
	}
	orgID, orgErr := pgvalue.UUIDValue(source.OrgID)
	projectID, projectErr := pgvalue.UUIDValue(source.ProjectID)
	environmentID, environmentErr := pgvalue.UUIDValue(source.EnvironmentID)
	sourceWorkspaceID, workspaceErr := pgvalue.UUIDValue(source.WorkspaceID)
	if orgErr != nil || projectErr != nil || environmentErr != nil || workspaceErr != nil {
		writeError(w, errors.New("Actor start source locators are invalid"))
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
			s.writeWorkerActorSourceError(w, "start", request.Lease.RunID, err)
			return
		}
		if failure, ok := workerActorStartFailure(err); ok {
			writeJSON(w, http.StatusOK, api.WorkerStartActorResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		s.log.Error("start run-sourced Actor", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("start run-sourced Actor"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerStartActorResponse{
		CorrelationID: request.CorrelationID,
		Completed: &api.StartActorResponse{
			ActorID: result.ActorPublicID, RunID: result.BootRunPublicID,
		},
	})
}

func (s *Server) workerGetActorStatus(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerActorReferenceRequest
	if err := decodeWorkerActorRequest(r, &request, "Actor status"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	address, err := parseWorkerActorReference(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	var status api.ActorStatus
	err = s.inTx(r.Context(), func(work *txWork) error {
		source, err := authorizeWorkerRunSource(r.Context(), work.q, worker, request.Lease)
		if err != nil {
			return err
		}
		status, err = getActorStatus(
			r.Context(), work.q, source.EnvironmentID,
			request.ActorDeclaredID, address,
		)
		return err
	})
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) {
			s.writeWorkerActorSourceError(w, "status", request.Lease.RunID, err)
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, failedWorkerActorReference(
				request.CorrelationID, "actor_not_found", "Actor was not found",
			))
			return
		}
		writeError(w, errors.New("read run-sourced Actor status"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerActorStatusResponse{
		CorrelationID: request.CorrelationID, Completed: &status,
	})
}

func (s *Server) workerCloseActor(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerCloseActorRequest
	if err := decodeWorkerActorRequest(r, &request, "Actor close"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	address, err := parseWorkerActorReference(request.WorkerActorReferenceRequest)
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
	var actor db.Actor
	err = s.inTx(r.Context(), func(work *txWork) error {
		source, err = authorizeWorkerRunSource(r.Context(), work.q, worker, request.Lease)
		if err != nil {
			return err
		}
		actor, err = resolveActorAddress(
			r.Context(), work.q, source.EnvironmentID,
			request.ActorDeclaredID, address,
		)
		return err
	})
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) {
			s.writeWorkerActorSourceError(w, "close", request.Lease.RunID, err)
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, api.WorkerCloseActorResponse{
				CorrelationID: request.CorrelationID,
				Failed: &api.WorkerRuntimeOperationFailure{
					Code: "actor_not_found", Message: "Actor was not found",
				},
			})
			return
		}
		writeError(w, errors.New("resolve run-sourced Actor close"))
		return
	}
	environmentID, _ := pgvalue.UUIDValue(source.EnvironmentID)
	actorID, _ := pgvalue.UUIDValue(actor.ID)
	workspaceID, _ := pgvalue.UUIDValue(actor.WorkspaceID)
	receipt, err := s.closeActor(r.Context(), actorCloseRequest{
		EnvironmentID: environmentID, ActorID: actorID,
		ActorPublicID: actor.PublicID, WorkspaceID: workspaceID,
		IdempotencyKey: idempotencyKey,
		Authorize: func(ctx context.Context, q db.Querier) error {
			_, err := authorizeWorkerRunSource(ctx, q, worker, request.Lease)
			return err
		},
	})
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) {
			s.writeWorkerActorSourceError(w, "close", request.Lease.RunID, err)
			return
		}
		if failure, ok := workerActorCloseFailure(err); ok {
			writeJSON(w, http.StatusOK, api.WorkerCloseActorResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		writeError(w, errors.New("close run-sourced Actor"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerCloseActorResponse{
		CorrelationID: request.CorrelationID, Completed: &receipt,
	})
}

func (s *Server) workerReadActorOutputPage(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerReadActorOutputPageRequest
	if err := decodeWorkerActorRequest(r, &request, "Actor output page"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	address, err := parseWorkerActorReference(request.WorkerActorReferenceRequest)
	if err != nil || request.Limit < 1 || request.Limit > actorOutputReadMaxLimit ||
		(request.After != nil && (*request.After < 0 || *request.After > maxActorOutputSequence)) {
		writeError(w, badRequest(errors.New("Actor output page request is invalid")))
		return
	}
	worker := workerFromContext(r.Context())
	var page api.ActorOutputPage
	err = s.inTx(r.Context(), func(work *txWork) error {
		source, err := authorizeWorkerRunSource(r.Context(), work.q, worker, request.Lease)
		if err != nil {
			return err
		}
		page, err = readActorOutputPage(
			r.Context(), work.q, source.EnvironmentID, request.ActorDeclaredID,
			address, request.After, request.Limit,
		)
		return err
	})
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) {
			s.writeWorkerActorSourceError(w, "output-page", request.Lease.RunID, err)
			return
		}
		code, message := "actor_output_unavailable", "Actor output is unavailable"
		if errors.Is(err, pgx.ErrNoRows) {
			code, message = "actor_not_found", "Actor was not found"
		} else if errors.Is(err, errActorOutputCursorExpired) {
			code, message = "actor_output_cursor_expired", err.Error()
		} else {
			writeError(w, errors.New("read run-sourced Actor output"))
			return
		}
		writeJSON(w, http.StatusOK, api.WorkerReadActorOutputPageResponse{
			CorrelationID: request.CorrelationID,
			Failed:        &api.WorkerRuntimeOperationFailure{Code: code, Message: message},
		})
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerReadActorOutputPageResponse{
		CorrelationID: request.CorrelationID, Completed: &page,
	})
}

func (s *Server) workerRunSource(
	ctx context.Context,
	worker workerActor,
	lease api.WorkerRunLeaseReceipt,
) (workerRunSourceAuthority, error) {
	var source workerRunSourceAuthority
	err := s.inTx(ctx, func(work *txWork) error {
		var err error
		source, err = authorizeWorkerRunSource(ctx, work.q, worker, lease)
		return err
	})
	return source, err
}

func parseWorkerActorReference(
	request api.WorkerActorReferenceRequest,
) (actorReadAddress, error) {
	if _, err := parseCanonicalUUID("correlation_id", request.CorrelationID); err != nil {
		return actorReadAddress{}, err
	}
	if err := api.ValidateActorDeclaredID(request.ActorDeclaredID); err != nil {
		return actorReadAddress{}, err
	}
	if err := api.ValidateActorReference(api.ActorReference{
		ActorID: request.ActorID, ActorKey: request.ActorKey,
	}); err != nil {
		return actorReadAddress{}, err
	}
	return actorReadAddress{publicID: request.ActorID, key: request.ActorKey}, nil
}

func resolveActorAddress(
	ctx context.Context,
	store db.Querier,
	environmentID pgtype.UUID,
	actorDeclaredID string,
	address actorReadAddress,
) (db.Actor, error) {
	if address.publicID != "" {
		return store.GetActorByPublicID(ctx, db.GetActorByPublicIDParams{
			EnvironmentID: environmentID, ActorDeclaredID: actorDeclaredID,
			PublicID: address.publicID,
		})
	}
	return store.GetActorByKey(ctx, db.GetActorByKeyParams{
		EnvironmentID: environmentID, ActorDeclaredID: actorDeclaredID,
		Key: pgvalue.Text(address.key),
	})
}

func workerActorStartFailure(err error) (api.WorkerRuntimeOperationFailure, bool) {
	var claimConflict idempotency.ConflictError
	var keyConflict ActorKeyConflictError
	switch {
	case errors.As(err, &claimConflict):
		return runtimeActorFailure("idempotency_conflict", "idempotency key conflicts with an earlier Actor start", false), true
	case errors.As(err, &keyConflict):
		return runtimeActorFailure("actor_key_conflict", keyConflict.Error(), false), true
	case errors.Is(err, errActorStartNotDeployed):
		return runtimeActorFailure("actor_not_deployed", err.Error(), false), true
	case errors.Is(err, errActorStartWorkspaceNotFound):
		return runtimeActorFailure("workspace_not_found", err.Error(), false), true
	case errors.Is(err, errActorStartWorkspaceConflict):
		return runtimeActorFailure("workspace_unavailable", err.Error(), true), true
	case errors.Is(err, errActorStartSecretUnavailable):
		return runtimeActorFailure("secret_unavailable", err.Error(), false), true
	case errors.Is(err, errActorInputTooLarge), errors.Is(err, errActorStartInvalid):
		return runtimeActorFailure("invalid_actor_start", err.Error(), false), true
	default:
		return api.WorkerRuntimeOperationFailure{}, false
	}
}

func workerActorCloseFailure(err error) (api.WorkerRuntimeOperationFailure, bool) {
	var claimConflict idempotency.ConflictError
	switch {
	case errors.As(err, &claimConflict):
		return runtimeActorFailure("idempotency_conflict", "idempotency key conflicts with an earlier Actor close", false), true
	case errors.Is(err, errActorCloseConflict):
		return runtimeActorFailure("actor_lifecycle_conflict", err.Error(), false), true
	case errors.Is(err, errActorCloseAuthority):
		return runtimeActorFailure("actor_not_found", "Actor was not found", false), true
	default:
		return api.WorkerRuntimeOperationFailure{}, false
	}
}

func runtimeActorFailure(code, message string, retryable bool) api.WorkerRuntimeOperationFailure {
	return api.WorkerRuntimeOperationFailure{
		Code: strings.TrimSpace(code), Message: strings.TrimSpace(message),
		Retryable: retryable,
	}
}

func failedWorkerActorStart(
	correlationID, code, message string,
	retryable bool,
) api.WorkerStartActorResponse {
	failure := runtimeActorFailure(code, message, retryable)
	return api.WorkerStartActorResponse{
		CorrelationID: correlationID, Failed: &failure,
	}
}

func failedWorkerActorReference(
	correlationID, code, message string,
) api.WorkerActorStatusResponse {
	failure := runtimeActorFailure(code, message, false)
	return api.WorkerActorStatusResponse{
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
	writeError(w, errors.New("authorize worker Actor operation source"))
}
