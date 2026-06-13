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
		response, err := s.runResponse(r.Context(), summary)
		if err != nil {
			s.log.Error("build idempotent cancel response failed", "run_id", runID.String(), "error", err)
			writeError(w, errors.New("cancel run"))
			return
		}
		writeJSON(w, http.StatusOK, api.CancelRunResponse{Run: response, Operation: runOperationResponse(operation)})
		return
	}
	cancelled, err := s.db.CancelRun(r.Context(), db.CancelRunParams{
		OrgID:       pgvalue.UUID(actor.OrgID),
		RunID:       pgvalue.UUID(runID),
		Reason:      request.Reason,
		Force:       request.Force,
		OperationID: operation.ID,
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
				response, err := s.runResponse(r.Context(), getRunSummary(runRow))
				if err != nil {
					s.log.Error("build idempotent cancel response failed", "run_id", runID.String(), "operation_id", pgvalue.MustUUIDValue(operationID).String(), "error", err)
					writeError(w, errors.New("cancel run"))
					return
				}
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
	response, err := s.runResponse(r.Context(), cancelRunSummary(cancelled))
	if err != nil {
		s.log.Error("build cancel response failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("cancel run"))
		return
	}
	writeJSON(w, http.StatusOK, api.CancelRunResponse{Run: response, Operation: runOperationResponse(operation)})
}

func (s *Server) replayRun(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	runID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var request api.ReplayRunRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid replay request JSON: %w", err)))
		return
	}
	request.Version = strings.TrimSpace(request.Version)
	request.Reason = strings.TrimSpace(request.Reason)
	idempotencyKey, err := normalizeRunOperationIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}

	actor := actorFromContext(r.Context())
	original, err := s.db.GetRun(r.Context(), db.GetRunParams{OrgID: pgvalue.UUID(actor.OrgID), ID: pgvalue.UUID(runID)})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("run not found")))
		return
	}
	if err != nil {
		s.log.Error("get run before replay failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("replay run"))
		return
	}
	originalSummary := runRecordSummary(original)
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     pgvalue.MustUUIDValue(originalSummary.ProjectID).String(),
		EnvironmentID: pgvalue.MustUUIDValue(originalSummary.EnvironmentID).String(),
	}
	if err := s.requireActorScopeForRecord(r, actor, originalSummary.ProjectID, originalSummary.EnvironmentID); err != nil {
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
	replayRequest, err := replayCreateRunRequest(original, request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if idempotencyKey != "" {
		replayRequest.Options.IdempotencyKey = "replay:" + runID.String() + ":" + idempotencyKey
	}
	requestBody, err := json.Marshal(request)
	if err != nil {
		writeError(w, errors.New("encode replay request"))
		return
	}
	operation, err := s.createRunOperation(r.Context(), actor, originalSummary, db.RunOperationKindReplay, request.Reason, requestBody, idempotencyKey)
	if err != nil {
		s.log.Error("create replay operation failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("replay run"))
		return
	}
	sameReplayRequest, err := sameJSONValue(operation.Request, requestBody)
	if err != nil {
		s.log.Error("compare replay operation request failed", "run_id", runID.String(), "operation_id", pgvalue.MustUUIDValue(operation.ID).String(), "error", err)
		writeError(w, errors.New("replay run"))
		return
	}
	if idempotencyKey != "" && !sameReplayRequest {
		writeError(w, conflict(errors.New("replay idempotency key was used with a different request")))
		return
	}
	if operation.Status != db.RunOperationStatusRequested {
		replayed, err := s.idempotentReplayRun(r.Context(), actor, operation, requestBody)
		if err != nil {
			if errors.Is(err, errIdempotencyKeyConflict) {
				writeError(w, conflict(err))
				return
			}
			s.log.Error("resolve idempotent replay failed", "run_id", runID.String(), "operation_id", pgvalue.MustUUIDValue(operation.ID).String(), "error", err)
			writeError(w, errors.New("replay run"))
			return
		}
		response, err := s.runResponse(r.Context(), replayed)
		if err != nil {
			s.log.Error("build idempotent replay response failed", "run_id", pgvalue.MustUUIDValue(replayed.ID).String(), "error", err)
			writeError(w, errors.New("replay run"))
			return
		}
		writeJSON(w, http.StatusOK, api.ReplayRunResponse{
			Run:       response,
			Operation: runOperationResponse(operation),
		})
		return
	}
	replayed, _, err := s.createRunFromRequest(contextWithRequestVersionMetadata(r.Context(), r), actor, replayRequest, runSource{
		replayedFromRunID: original.ID,
		replayOperationID: operation.ID,
	})
	if err != nil {
		markRejected := func() {
			_, _ = s.db.MarkRunOperationRejected(context.WithoutCancel(r.Context()), db.MarkRunOperationRejectedParams{
				Result: fmt.Appendf(nil, `{"error":%q}`, err.Error()),
				ID:     operation.ID,
				OrgID:  pgvalue.UUID(actor.OrgID),
			})
		}
		if errors.Is(err, errIdempotencyKeyConflict) {
			markRejected()
			writeError(w, conflict(err))
			return
		}
		var upstreamErr createRunUpstreamError
		if errors.As(err, &upstreamErr) {
			writeError(w, badGateway(upstreamErr))
			return
		}
		if isCreateRunConfigError(err) {
			writeError(w, unavailable(err))
			return
		}
		if errors.Is(err, errPermissionRequired) {
			markRejected()
			writeError(w, forbidden(err))
			return
		}
		var runDeploymentErr runDeploymentSelectionError
		if errors.As(err, &runDeploymentErr) {
			markRejected()
			writeError(w, badRequest(runDeploymentErr))
			return
		}
		if isCreateRunClientError(err) {
			markRejected()
			writeError(w, badRequest(err))
			return
		}
		s.log.Error("replay run failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("replay run"))
		return
	}
	operationResult, err := json.Marshal(map[string]string{
		"source_run_id": runID.String(),
		"run_id":        pgvalue.MustUUIDValue(replayed.ID).String(),
	})
	if err != nil {
		writeError(w, errors.New("encode replay result"))
		return
	}
	operationID := operation.ID
	operation, err = s.db.MarkRunOperationApplied(r.Context(), db.MarkRunOperationAppliedParams{
		Result: operationResult,
		ID:     operationID,
		OrgID:  pgvalue.UUID(actor.OrgID),
	})
	if isNoRows(err) {
		operation, err = s.db.GetRunOperation(r.Context(), db.GetRunOperationParams{OrgID: pgvalue.UUID(actor.OrgID), ID: operationID})
		if err == nil && operation.Status != db.RunOperationStatusRequested {
			replayed, replayErr := s.idempotentReplayRun(r.Context(), actor, operation, requestBody)
			if replayErr == nil {
				response, responseErr := s.runResponse(r.Context(), replayed)
				if responseErr != nil {
					s.log.Error("build raced replay response failed", "run_id", pgvalue.MustUUIDValue(replayed.ID).String(), "error", responseErr)
					writeError(w, errors.New("replay run"))
					return
				}
				writeJSON(w, http.StatusOK, api.ReplayRunResponse{
					Run:       response,
					Operation: runOperationResponse(operation),
				})
				return
			}
		}
	}
	if err != nil {
		s.log.Error("mark replay operation applied failed", "run_id", runID.String(), "operation_id", pgvalue.MustUUIDValue(operationID).String(), "error", err)
		writeError(w, errors.New("replay run"))
		return
	}
	response, err := s.runResponse(r.Context(), replayed)
	if err != nil {
		s.log.Error("build replay response failed", "run_id", pgvalue.MustUUIDValue(replayed.ID).String(), "error", err)
		writeError(w, errors.New("replay run"))
		return
	}
	writeJSON(w, http.StatusCreated, api.ReplayRunResponse{
		Run:       response,
		Operation: runOperationResponse(operation),
	})
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
	actorID, err := auth.ActorPrincipalAllowSystem(actor)
	if err != nil {
		return db.RunOperation{}, err
	}
	apiKeyID := pgtype.UUID{}
	if actor.Kind == auth.ActorKindAPIKey {
		apiKeyID = pgvalue.UUID(actor.APIKeyID)
	}
	return s.db.CreateRunOperation(ctx, db.CreateRunOperationParams{
		ID:             pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:          run.OrgID,
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
}

func (s *Server) idempotentReplayRun(ctx context.Context, actor auth.Actor, operation db.RunOperation, requestBody []byte) (runSummary, error) {
	sameRequest, err := sameJSONValue(operation.Request, requestBody)
	if err != nil {
		return runSummary{}, fmt.Errorf("compare replay operation request: %w", err)
	}
	if !sameRequest {
		return runSummary{}, errIdempotencyKeyConflict
	}
	if operation.Status != db.RunOperationStatusApplied {
		return runSummary{}, errIdempotencyKeyConflict
	}
	var result struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(operation.Result, &result); err != nil {
		return runSummary{}, fmt.Errorf("decode replay operation result: %w", err)
	}
	runID, err := uuid.Parse(strings.TrimSpace(result.RunID))
	if err != nil {
		return runSummary{}, fmt.Errorf("decode replay operation run_id: %w", err)
	}
	run, err := s.db.GetRunSummary(ctx, db.GetRunSummaryParams{OrgID: pgvalue.UUID(actor.OrgID), ID: pgvalue.UUID(runID)})
	if err != nil {
		return runSummary{}, err
	}
	return getRunSummary(run), nil
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

func replayCreateRunRequest(original db.Run, replay api.ReplayRunRequest) (api.CreateRunRequest, error) {
	payload := json.RawMessage(original.Payload)
	if len(replay.Payload) > 0 {
		payload = replay.Payload
	}
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return api.CreateRunRequest{}, errors.New("payload must be valid JSON")
	}
	metadata := json.RawMessage(original.Metadata)
	if len(replay.Metadata) > 0 {
		metadata = replay.Metadata
	}
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	tags := append([]string(nil), original.Tags...)
	if replay.Tags != nil {
		tags = append([]string(nil), replay.Tags...)
	}
	request := api.CreateRunRequest{
		ProjectID:     pgvalue.MustUUIDValue(original.ProjectID).String(),
		EnvironmentID: pgvalue.MustUUIDValue(original.EnvironmentID).String(),
		TaskID:        original.TaskID,
		Payload:       payload,
		Options: api.CreateRunOptions{
			Queue:              &api.RunQueueOption{Name: original.QueueName},
			Priority:           original.Priority,
			TTL:                original.Ttl,
			MaxDurationSeconds: original.MaxDurationSeconds,
			Retry:              json.RawMessage(original.LockedRetryPolicy),
			Metadata:           metadata,
			Tags:               tags,
		},
	}
	if original.ConcurrencyKey.Valid {
		request.Options.ConcurrencyKey = original.ConcurrencyKey.String
	}
	switch replay.Version {
	case "", "original":
		request.Options.DeploymentID = pgvalue.MustUUIDValue(original.DeploymentID).String()
	case "latest":
	default:
		request.Options.Version = replay.Version
	}
	return request, nil
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
