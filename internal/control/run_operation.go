package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	runID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var request api.CancelRunRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid cancel request JSON: %w", err)))
		return
	}
	request.Reason = strings.TrimSpace(request.Reason)
	idempotencyKey, err := normalizeRunOperationIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}

	actor := actorFromContext(r.Context())
	runRow, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{OrgID: pgvalue.UUID(actor.OrgID), ID: pgvalue.UUID(runID)})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("run not found")))
		return
	}
	if err != nil {
		s.log.Error("get run before cancel failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("cancel run"))
		return
	}
	summary := getRunSummary(runRow)
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     pgvalue.MustUUIDValue(summary.ProjectID).String(),
		EnvironmentID: pgvalue.MustUUIDValue(summary.EnvironmentID).String(),
	}
	if err := s.requireActorScopeForRecord(r, actor, summary.ProjectID, summary.EnvironmentID); err != nil {
		if isNoRows(err) {
			writeError(w, notFound(errors.New("run not found")))
			return
		}
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsManage, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	if err := s.requireRoutableRecordWorkerGroup(r.Context(), s.db, actor.OrgID, summary.ProjectID, summary.EnvironmentID, summary.WorkerGroupID); err != nil {
		writeError(w, err)
		return
	}
	requestBody, err := json.Marshal(request)
	if err != nil {
		writeError(w, errors.New("encode cancel request"))
		return
	}
	operation, err := s.createRunOperation(r.Context(), actor, summary, db.RunOperationKindCancel, request.Reason, requestBody, idempotencyKey)
	if err != nil {
		s.log.Error("create cancel operation failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("cancel run"))
		return
	}
	sameCancelRequest, err := sameJSONValue(operation.Request, requestBody)
	if err != nil {
		s.log.Error("compare cancel operation request failed", "run_id", runID.String(), "operation_id", pgvalue.MustUUIDValue(operation.ID).String(), "error", err)
		writeError(w, errors.New("cancel run"))
		return
	}
	if idempotencyKey != "" && !sameCancelRequest {
		writeError(w, conflict(errors.New("cancel idempotency key was used with a different request")))
		return
	}
	if operation.Status != db.RunOperationStatusRequested {
		response := runResponse(summary)
		writeJSON(w, http.StatusOK, api.CancelRunResponse{Run: response, Operation: runOperationResponse(operation)})
		return
	}
	cancelled, err := s.db.CancelRun(r.Context(), db.CancelRunParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		WorkerGroupID: summary.WorkerGroupID,
		RunID:         pgvalue.UUID(runID),
		Reason:        request.Reason,
		Force:         request.Force,
		OperationID:   operation.ID,
	})
	if err != nil {
		if isNoRows(err) {
			operationID := operation.ID
			operation, err = s.db.GetRunOperation(r.Context(), db.GetRunOperationParams{OrgID: pgvalue.UUID(actor.OrgID), ID: operationID})
			if err != nil {
				s.log.Error("get idempotent cancel operation failed", "run_id", runID.String(), "operation_id", pgvalue.MustUUIDValue(operationID).String(), "error", err)
				writeError(w, errors.New("cancel run"))
				return
			}
			if operation.Status != db.RunOperationStatusRequested {
				runRow, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{OrgID: pgvalue.UUID(actor.OrgID), ID: pgvalue.UUID(runID)})
				if err != nil {
					s.log.Error("get idempotent cancel run failed", "run_id", runID.String(), "operation_id", pgvalue.MustUUIDValue(operationID).String(), "error", err)
					writeError(w, errors.New("cancel run"))
					return
				}
				response := runResponse(getRunSummary(runRow))
				writeJSON(w, http.StatusOK, api.CancelRunResponse{Run: response, Operation: runOperationResponse(operation)})
				return
			}
		}
		s.log.Error("cancel run failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("cancel run"))
		return
	}
	operation, err = s.db.GetRunOperation(r.Context(), db.GetRunOperationParams{OrgID: pgvalue.UUID(actor.OrgID), ID: operation.ID})
	if err != nil {
		s.log.Error("get cancel operation failed", "run_id", runID.String(), "operation_id", pgvalue.MustUUIDValue(operation.ID).String(), "error", err)
		writeError(w, errors.New("cancel run"))
		return
	}
	response := runResponse(cancelRunSummary(cancelled))
	writeJSON(w, http.StatusOK, api.CancelRunResponse{Run: response, Operation: runOperationResponse(operation)})
}

func normalizeRunOperationIdempotencyKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", nil
	}
	if len(key) > maxIdempotencyKeyLength {
		return "", fmt.Errorf("idempotency_key must be at most %d characters", maxIdempotencyKeyLength)
	}
	return key, nil
}

func (s *Server) createRunOperation(ctx context.Context, actor auth.Actor, run runSummary, kind db.RunOperationKind, reason string, requestBody []byte, idempotencyKey string) (db.RunOperation, error) {
	return createRunOperationWithStore(ctx, s.db, actor, run, kind, reason, requestBody, idempotencyKey)
}

func createRunOperationWithStore(ctx context.Context, store db.Querier, actor auth.Actor, run runSummary, kind db.RunOperationKind, reason string, requestBody []byte, idempotencyKey string) (db.RunOperation, error) {
	actorID, err := auth.ActorPrincipalAllowSystem(actor)
	if err != nil {
		return db.RunOperation{}, err
	}
	apiKeyID := pgtype.UUID{}
	if actor.Kind == auth.ActorKindAPIKey {
		apiKeyID = pgvalue.UUID(actor.APIKeyID)
	}
	var publicID string
	return createWithPublicID(ctx, []publicIDSlot{{prefix: publicid.RunOperation, value: &publicID}}, func() (db.RunOperation, error) {
		return store.CreateRunOperation(ctx, db.CreateRunOperationParams{
			ID:             pgvalue.UUID(uuid.Must(uuid.NewV7())),
			PublicID:       publicID,
			OrgID:          run.OrgID,
			WorkerGroupID:  run.WorkerGroupID,
			ProjectID:      run.ProjectID,
			EnvironmentID:  run.EnvironmentID,
			RunID:          run.ID,
			Kind:           kind,
			ActorKind:      string(actor.Kind),
			ActorID:        actorID,
			ApiKeyID:       apiKeyID,
			Reason:         reason,
			Request:        requestBody,
			IdempotencyKey: idempotencyKey,
		})
	})
}

func sameJSONValue(left []byte, right []byte) (bool, error) {
	leftValue, err := decodeJSONValueForComparison(left)
	if err != nil {
		return false, err
	}
	rightValue, err := decodeJSONValueForComparison(right)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(leftValue, rightValue), nil
}

func decodeJSONValueForComparison(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func runOperationResponse(operation db.RunOperation) api.RunOperationResponse {
	var appliedAt *time.Time
	if operation.AppliedAt.Valid {
		value := operation.AppliedAt.Time
		appliedAt = &value
	}
	return api.RunOperationResponse{
		ID:        pgvalue.MustUUIDValue(operation.ID).String(),
		RunID:     pgvalue.MustUUIDValue(operation.RunID).String(),
		Kind:      string(operation.Kind),
		Status:    string(operation.Status),
		Reason:    operation.Reason,
		CreatedAt: pgvalue.Time(operation.CreatedAt),
		AppliedAt: appliedAt,
	}
}
