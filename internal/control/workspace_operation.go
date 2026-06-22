package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultWorkspaceOperationClaimTTL  = 30 * time.Second
	maxWorkspaceOperationClaimTTL      = 5 * time.Minute
	maxWorkspaceOperationClaimAttempts = 3
)

func (s *Server) workerClaimWorkspaceMaterializationOperation(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceOperationClaimRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace operation claim request JSON: %w", err)))
		return
	}
	ttl, err := workspaceOperationClaimTTL(request.ClaimExpiresInSeconds)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	orgID, err := parseWorkspaceOperationStringUUID("org_id", request.OrgID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	materializationID, err := parseWorkspaceOperationStringUUID("materialization_id", request.MaterializationID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	reservationToken := strings.TrimSpace(request.ReservationToken)
	if reservationToken == "" {
		writeError(w, badRequest(errors.New("reservation_token is required")))
		return
	}
	token, err := newWorkspaceMaterializationReservationToken()
	if err != nil {
		writeError(w, errors.New("generate workspace operation claim token"))
		return
	}
	worker := workerFromContext(r.Context())
	row, err := s.db.ClaimWorkspaceMaterializationOperation(r.Context(), db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(worker.WorkerInstanceID),
		OrgID:             orgID,
		MaterializationID: materializationID,
		ReservationToken:  reservationToken,
		ClaimToken:        token,
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(ttl)),
		MaxClaimAttempts:  maxWorkspaceOperationClaimAttempts,
	})
	if isNoRows(err) {
		writeJSON(w, http.StatusOK, api.WorkerWorkspaceOperationClaimResponse{})
		return
	}
	if err != nil {
		s.log.Error("claim workspace operation failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("claim workspace operation"))
		return
	}
	operation, err := workerWorkspaceOperationResponse(row)
	if err != nil {
		s.log.Error("encode workspace operation failed", "worker_instance_id", worker.WorkerInstanceID.String(), "operation_id", pgvalue.MustUUIDValue(row.ID).String(), "error", err)
		writeError(w, errors.New("encode workspace operation"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerWorkspaceOperationClaimResponse{Operation: &operation})
}

func (s *Server) workerStartWorkspaceMaterializationOperation(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceOperationStartRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace operation start request JSON: %w", err)))
		return
	}
	orgID, err := parseWorkspaceOperationStringUUID("org_id", request.OrgID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	operationID, err := parseWorkspaceOperationStringUUID("operation_id", request.OperationID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	claimToken := strings.TrimSpace(request.ClaimToken)
	if claimToken == "" {
		writeError(w, badRequest(errors.New("claim_token is required")))
		return
	}
	worker := workerFromContext(r.Context())
	row, err := s.db.StartWorkspaceMaterializationOperation(r.Context(), db.StartWorkspaceMaterializationOperationParams{
		OrgID:            orgID,
		ID:               operationID,
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		ClaimToken:       claimToken,
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("workspace operation claim is stale")))
		return
	}
	if err != nil {
		s.log.Error("start workspace operation failed", "worker_instance_id", worker.WorkerInstanceID.String(), "operation_id", request.OperationID, "error", err)
		writeError(w, errors.New("start workspace operation"))
		return
	}
	writeJSON(w, http.StatusOK, workspaceOperationResponse(startedWorkspaceOperation(row)))
}

func (s *Server) workerCompleteWorkspaceMaterializationOperation(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceOperationCompleteRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace operation complete request JSON: %w", err)))
		return
	}
	orgID, err := parseWorkspaceOperationStringUUID("org_id", request.OrgID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	operationID, err := parseWorkspaceOperationStringUUID("operation_id", request.OperationID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	claimToken := strings.TrimSpace(request.ClaimToken)
	if claimToken == "" {
		writeError(w, badRequest(errors.New("claim_token is required")))
		return
	}
	result, err := normalizedJSONObject(request.Result, "result")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	failure, err := normalizedOptionalJSONObject(request.Error, "error")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if len(failure) > 0 && len(bytes.TrimSpace(request.Result)) > 0 {
		writeError(w, badRequest(errors.New("result and error cannot both be set")))
		return
	}
	worker := workerFromContext(r.Context())
	var row db.WorkspaceMaterializationOperation
	if s.tx == nil {
		writeError(w, errors.New("workspace operation complete requires transactional store"))
		return
	}
	tx, err := s.tx.Begin(r.Context())
	if err != nil {
		writeError(w, errors.New("complete workspace operation"))
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	store := db.New(tx)
	if len(failure) > 0 {
		failed, failErr := store.FailWorkspaceMaterializationOperation(r.Context(), db.FailWorkspaceMaterializationOperationParams{
			OrgID:            orgID,
			ID:               operationID,
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			ClaimToken:       claimToken,
			Error:            failure,
		})
		row = failedWorkspaceOperation(failed)
		err = failErr
		if err == nil {
			err = failWorkspacePrimitiveForOperation(r.Context(), store, row, failure)
		}
	} else {
		completed, completeErr := store.CompleteWorkspaceMaterializationOperation(r.Context(), db.CompleteWorkspaceMaterializationOperationParams{
			OrgID:            orgID,
			ID:               operationID,
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			ClaimToken:       claimToken,
			Result:           result,
		})
		row = completedWorkspaceOperation(completed)
		err = completeErr
		if err == nil {
			err = completeWorkspacePrimitiveForOperation(r.Context(), store, row)
		}
	}
	if isNoRows(err) {
		writeError(w, conflict(errors.New("workspace operation claim is stale")))
		return
	}
	if err != nil {
		s.log.Error("complete workspace operation failed", "worker_instance_id", worker.WorkerInstanceID.String(), "operation_id", request.OperationID, "error", err)
		writeError(w, errors.New("complete workspace operation"))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.log.Error("commit workspace operation completion failed", "worker_instance_id", worker.WorkerInstanceID.String(), "operation_id", request.OperationID, "error", err)
		writeError(w, errors.New("complete workspace operation"))
		return
	}
	writeJSON(w, http.StatusOK, workspaceOperationResponse(row))
}

type workspacePrimitiveFailureStore interface {
	MarkWorkspaceExecExited(context.Context, db.MarkWorkspaceExecExitedParams) (db.MarkWorkspaceExecExitedRow, error)
	MarkWorkspacePtyFailed(context.Context, db.MarkWorkspacePtyFailedParams) (db.MarkWorkspacePtyFailedRow, error)
	RollbackWorkspacePtyControlOperation(context.Context, db.RollbackWorkspacePtyControlOperationParams) (db.WorkspacePtySession, error)
}

type workspacePrimitiveCompletionStore interface {
	GetWorkspacePtySession(context.Context, db.GetWorkspacePtySessionParams) (db.WorkspacePtySession, error)
	MarkWorkspacePtyResizeApplied(context.Context, db.MarkWorkspacePtyResizeAppliedParams) (db.WorkspacePtySession, error)
}

func completeWorkspacePrimitiveForOperation(ctx context.Context, store workspacePrimitiveCompletionStore, operation db.WorkspaceMaterializationOperation) error {
	switch operation.OperationKind {
	case workspaceOperationKindResizePty:
		return completeWorkspacePtyResizeOperation(ctx, store, operation)
	default:
		return nil
	}
}

func failWorkspacePrimitiveForOperation(ctx context.Context, store workspacePrimitiveFailureStore, operation db.WorkspaceMaterializationOperation, failure []byte) error {
	if !operation.ResourceID.Valid {
		return nil
	}
	switch operation.OperationKind {
	case workspaceOperationKindStartExec:
		if operation.ResourceKind != workspaceOperationResourceExec {
			return fmt.Errorf("StartExec operation resource_kind = %q, want %q", workspace.ResourceKindString(operation.ResourceKind), workspaceOperationResourceExec)
		}
		_, err := store.MarkWorkspaceExecExited(ctx, db.MarkWorkspaceExecExitedParams{
			State:             db.WorkspaceExecStateFailed,
			ExitCode:          pgtype.Int4{},
			Signal:            "",
			Error:             failure,
			OrgID:             operation.OrgID,
			ProjectID:         operation.ProjectID,
			EnvironmentID:     operation.EnvironmentID,
			WorkspaceID:       operation.WorkspaceID,
			ID:                operation.ResourceID,
			MaterializationID: operation.MaterializationID,
		})
		if isNoRows(err) {
			return nil
		}
		return err
	case workspaceOperationKindCreatePty:
		if operation.ResourceKind != workspaceOperationResourcePty {
			return fmt.Errorf("CreatePty operation resource_kind = %q, want %q", workspace.ResourceKindString(operation.ResourceKind), workspaceOperationResourcePty)
		}
		_, err := store.MarkWorkspacePtyFailed(ctx, db.MarkWorkspacePtyFailedParams{
			Error:             failure,
			OrgID:             operation.OrgID,
			ProjectID:         operation.ProjectID,
			EnvironmentID:     operation.EnvironmentID,
			WorkspaceID:       operation.WorkspaceID,
			ID:                operation.ResourceID,
			MaterializationID: operation.MaterializationID,
		})
		if isNoRows(err) {
			return nil
		}
		return err
	case workspaceOperationKindResizePty, workspaceOperationKindClosePty:
		if operation.ResourceKind != workspaceOperationResourcePty {
			operationKind, err := workspace.OperationGuestVerb(operation.OperationKind)
			if err != nil {
				return err
			}
			return fmt.Errorf("%s operation resource_kind = %q, want %q", operationKind, workspace.ResourceKindString(operation.ResourceKind), workspaceOperationResourcePty)
		}
		cols, rows, err := workspacePtyControlRollbackTarget(operation)
		if err != nil {
			return err
		}
		_, err = store.RollbackWorkspacePtyControlOperation(ctx, db.RollbackWorkspacePtyControlOperationParams{
			OrgID:             operation.OrgID,
			ProjectID:         operation.ProjectID,
			EnvironmentID:     operation.EnvironmentID,
			WorkspaceID:       operation.WorkspaceID,
			ID:                operation.ResourceID,
			MaterializationID: operation.MaterializationID,
			OperationKind:     operation.OperationKind,
			Cols:              cols,
			Rows:              rows,
		})
		if isNoRows(err) {
			return nil
		}
		return err
	default:
		return nil
	}
}

func workspacePtyControlRollbackTarget(operation db.WorkspaceMaterializationOperation) (pgtype.Int4, pgtype.Int4, error) {
	if operation.OperationKind != workspaceOperationKindResizePty {
		return pgtype.Int4{}, pgtype.Int4{}, nil
	}
	var request struct {
		Cols int32 `json:"cols"`
		Rows int32 `json:"rows"`
	}
	if err := json.Unmarshal(operation.Request, &request); err != nil {
		return pgtype.Int4{}, pgtype.Int4{}, fmt.Errorf("decode ResizePty rollback request: %w", err)
	}
	return pgtype.Int4{Int32: request.Cols, Valid: true}, pgtype.Int4{Int32: request.Rows, Valid: true}, nil
}

func completeWorkspacePtyResizeOperation(ctx context.Context, store workspacePrimitiveCompletionStore, operation db.WorkspaceMaterializationOperation) error {
	if !operation.ResourceID.Valid {
		return errors.New("ResizePty operation resource_id is required")
	}
	var request struct {
		Cols int32 `json:"cols"`
		Rows int32 `json:"rows"`
	}
	if err := json.Unmarshal(operation.Request, &request); err != nil {
		return fmt.Errorf("decode ResizePty request: %w", err)
	}
	row, err := store.MarkWorkspacePtyResizeApplied(ctx, db.MarkWorkspacePtyResizeAppliedParams{
		OrgID:             operation.OrgID,
		ProjectID:         operation.ProjectID,
		EnvironmentID:     operation.EnvironmentID,
		WorkspaceID:       operation.WorkspaceID,
		ID:                operation.ResourceID,
		MaterializationID: operation.MaterializationID,
		Cols:              pgtype.Int4{Int32: request.Cols, Valid: true},
		Rows:              pgtype.Int4{Int32: request.Rows, Valid: true},
	})
	if err == nil {
		if row.Cols != request.Cols || row.Rows != request.Rows {
			return conflict(errWorkspaceLifecycleEventConflict)
		}
		return nil
	}
	if !isNoRows(err) {
		return err
	}
	existing, getErr := store.GetWorkspacePtySession(ctx, db.GetWorkspacePtySessionParams{
		OrgID:         operation.OrgID,
		ProjectID:     operation.ProjectID,
		EnvironmentID: operation.EnvironmentID,
		WorkspaceID:   operation.WorkspaceID,
		ID:            operation.ResourceID,
	})
	if getErr == nil && workspacePtyResizeAppliedEventMatches(existing, operation.MaterializationID, request.Cols, request.Rows) {
		return nil
	}
	if getErr == nil {
		return conflict(errWorkspaceLifecycleEventConflict)
	}
	return getErr
}

func startedWorkspaceOperation(row db.StartWorkspaceMaterializationOperationRow) db.WorkspaceMaterializationOperation {
	return db.WorkspaceMaterializationOperation(row)
}

func completedWorkspaceOperation(row db.CompleteWorkspaceMaterializationOperationRow) db.WorkspaceMaterializationOperation {
	return db.WorkspaceMaterializationOperation(row)
}

func failedWorkspaceOperation(row db.FailWorkspaceMaterializationOperationRow) db.WorkspaceMaterializationOperation {
	return db.WorkspaceMaterializationOperation(row)
}

func workerWorkspaceOperationResponse(row db.WorkspaceMaterializationOperation) (api.WorkerWorkspaceOperation, error) {
	response := api.WorkerWorkspaceOperation{
		WorkspaceOperationResponse: workspaceOperationResponse(row),
		ClaimToken:                 row.ClaimToken,
	}
	operationKind, err := workspace.OperationGuestVerb(row.OperationKind)
	if err != nil {
		return api.WorkerWorkspaceOperation{}, err
	}
	response.OperationKind = operationKind
	if row.ClaimedByWorkerInstanceID.Valid {
		response.ClaimedByWorkerInstanceID = pgvalue.MustUUIDValue(row.ClaimedByWorkerInstanceID).String()
	}
	response.ClaimExpiresAt = optionalWorkspaceTime(row.ClaimExpiresAt)
	return response, nil
}

func workspaceOperationResponse(row db.WorkspaceMaterializationOperation) api.WorkspaceOperationResponse {
	response := api.WorkspaceOperationResponse{
		ID:                 pgvalue.MustUUIDValue(row.ID).String(),
		OrgID:              pgvalue.MustUUIDValue(row.OrgID).String(),
		ProjectID:          pgvalue.MustUUIDValue(row.ProjectID).String(),
		EnvironmentID:      pgvalue.MustUUIDValue(row.EnvironmentID).String(),
		WorkspaceID:        pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		MaterializationID:  pgvalue.MustUUIDValue(row.MaterializationID).String(),
		OperationKind:      string(row.OperationKind),
		ResourceKind:       workspace.ResourceKindString(row.ResourceKind),
		RequestFingerprint: row.RequestFingerprint,
		OperationExpiresAt: row.OperationExpiresAt.Time,
		State:              string(row.State),
		Priority:           row.Priority,
		FencingToken:       row.FencingToken,
		FencingGeneration:  row.FencingGeneration,
		Request:            json.RawMessage(row.Request),
		RequestedAt:        row.RequestedAt.Time,
		UpdatedAt:          row.UpdatedAt.Time,
	}
	switch row.State {
	case db.WorkspaceMaterializationOperationStateCompleted:
		response.Result = optionalRawMessage(row.Result)
	case db.WorkspaceMaterializationOperationStateFailed, db.WorkspaceMaterializationOperationStateCancelled, db.WorkspaceMaterializationOperationStateLost, db.WorkspaceMaterializationOperationStateExpired:
		response.Error = optionalRawMessage(row.Error)
	}
	if row.InstanceLeaseID.Valid {
		response.InstanceLeaseID = pgvalue.MustUUIDValue(row.InstanceLeaseID).String()
	}
	if row.ResourceID.Valid {
		response.ResourceID = pgvalue.MustUUIDValue(row.ResourceID).String()
	}
	if row.WriteLeaseID.Valid {
		response.WriteLeaseID = pgvalue.MustUUIDValue(row.WriteLeaseID).String()
	}
	response.ClaimedAt = optionalWorkspaceTime(row.ClaimedAt)
	response.CompletedAt = optionalWorkspaceTime(row.CompletedAt)
	return response
}

func workspaceOperationClaimTTL(seconds int32) (time.Duration, error) {
	if seconds <= 0 {
		return defaultWorkspaceOperationClaimTTL, nil
	}
	ttl := time.Duration(seconds) * time.Second
	if ttl > maxWorkspaceOperationClaimTTL {
		return 0, fmt.Errorf("claim_expires_in_seconds must be %d or less", int(maxWorkspaceOperationClaimTTL/time.Second))
	}
	return ttl, nil
}

func parseWorkspaceOperationStringUUID(field string, raw string) (pgtype.UUID, error) {
	return parseWorkspaceUUID(field, raw)
}

func normalizedOptionalJSONObject(raw json.RawMessage, label string) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	return normalizedJSONObject(raw, label)
}

func optionalRawMessage(raw []byte) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.RawMessage(raw)
}
