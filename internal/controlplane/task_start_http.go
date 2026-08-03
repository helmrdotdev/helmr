package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

const taskStartBodyLimit = int64(maxTaskPayloadBytes + maxRunMetadataBytes + 64<<10)

func (s *Server) startTaskHTTP(w http.ResponseWriter, r *http.Request) {
	request, payloadPresent, err := decodeStartTaskRequest(r)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, tooLarge(codedError{
				code: "task_start_request_too_large", message: "task start request is too large",
			}))
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_task_start", message: err.Error()}))
		return
	}
	taskDeclaredID := chi.URLParam(r, "taskDeclaredID")
	if err := api.ValidateTaskID(taskDeclaredID); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_task_start", message: err.Error()}))
		return
	}
	if err := api.ValidateWorkspaceTarget(request.Workspace); err != nil {
		writeError(w, badRequest(codedError{
			code: "invalid_workspace_reference", message: err.Error(),
		}))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(codedError{
			code: "invalid_idempotency_key", message: err.Error(),
		}))
		return
	}
	principal := actorFromContext(r.Context())
	if err := authorizeTaskStartBeforeLookup(principal); err != nil {
		writeError(w, err)
		return
	}
	projectRef, environmentRef, err := environmentScopeRefsFromRequest(r, principal, "", "")
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_task_start", message: err.Error()}))
		return
	}
	_, projectID, environmentID, err := s.requestEnvironmentScope(
		r.Context(), principal, projectRef, environmentRef,
	)
	if err != nil {
		writeError(w, unavailable(codedError{
			code:    "task_start_authority_unavailable",
			message: "task start environment scope is unavailable", retryable: true,
		}))
		return
	}
	projectUUID, projectErr := pgvalue.UUIDValue(projectID)
	environmentUUID, environmentErr := pgvalue.UUIDValue(environmentID)
	if projectErr != nil || environmentErr != nil {
		writeError(w, unavailable(codedError{
			code:    "task_start_authority_unavailable",
			message: "task start authority is unavailable", retryable: true,
		}))
		return
	}
	ttl, retry, err := taskStartPolicyFromAPI(request)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_task_start", message: err.Error()}))
		return
	}
	result, err := s.startTask(r.Context(), taskStartRequest{
		OrgID: principal.OrgID, ProjectID: projectUUID, EnvironmentID: environmentUUID,
		TaskDeclaredID: taskDeclaredID, PayloadPresent: payloadPresent, Payload: request.Payload,
		Workspace: request.Workspace, IdempotencyKey: idempotencyKey,
		QueueName: request.Queue, ConcurrencyKey: request.ConcurrencyKey,
		Priority: request.Priority, QueuedTTLMS: ttl, RetryPolicy: retry,
		Metadata: request.Metadata, Tags: request.Tags,
	})
	if err != nil {
		s.writeTaskStartError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, api.StartTaskResponse{RunID: result.RunID.String()})
}

func decodeStartTaskRequest(r *http.Request) (api.StartTaskRequest, bool, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return api.StartTaskRequest{}, false, err
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return api.StartTaskRequest{}, false, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &root); err != nil || root == nil {
		return api.StartTaskRequest{}, false, errors.New("task start request must be a JSON object")
	}
	if err := rejectActorStartNullFields(
		root,
		"",
		"idempotency_key",
		"workspace",
		"queue",
		"concurrency_key",
		"priority",
		"ttl",
		"retry",
		"metadata",
		"tags",
	); err != nil {
		return api.StartTaskRequest{}, false, err
	}
	if err := validateActorStartIdempotencyWire(root["idempotency_key"]); err != nil {
		return api.StartTaskRequest{}, false, err
	}
	for _, field := range []string{"queue", "ttl"} {
		raw, present := root[field]
		if !present {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || value == "" {
			return api.StartTaskRequest{}, false, fmt.Errorf("%s must be a nonempty string", field)
		}
	}
	workspace, ok := root["workspace"]
	if !ok {
		return api.StartTaskRequest{}, false, errors.New("workspace is required")
	}
	workspaceObject, err := decodeActorStartObject(workspace, "workspace")
	if err != nil {
		return api.StartTaskRequest{}, false, err
	}
	if err := rejectActorStartNullFields(workspaceObject, "workspace.", "id", "key"); err != nil {
		return api.StartTaskRequest{}, false, err
	}
	if err := rejectActorStartNullTagElements(root["tags"], "tags"); err != nil {
		return api.StartTaskRequest{}, false, err
	}
	if retry := root["retry"]; len(retry) > 0 {
		retryObject, err := decodeActorStartObject(retry, "retry")
		if err != nil {
			return api.StartTaskRequest{}, false, err
		}
		if err := rejectActorStartNullFields(
			retryObject, "retry.", "enabled", "max_attempts", "backoff",
		); err != nil {
			return api.StartTaskRequest{}, false, err
		}
		if backoff := retryObject["backoff"]; len(backoff) > 0 {
			backoffObject, err := decodeActorStartObject(backoff, "retry.backoff")
			if err != nil {
				return api.StartTaskRequest{}, false, err
			}
			if err := rejectActorStartNullFields(
				backoffObject,
				"retry.backoff.",
				"min_delay",
				"max_delay",
				"factor",
				"jitter",
			); err != nil {
				return api.StartTaskRequest{}, false, err
			}
		}
	}
	var request api.StartTaskRequest
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return api.StartTaskRequest{}, false, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return api.StartTaskRequest{}, false, errors.New("task start request contains a trailing value")
	}
	_, payloadPresent := root["payload"]
	return request, payloadPresent, nil
}

func authorizeTaskStartBeforeLookup(principal auth.Actor) error {
	switch principal.Kind {
	case auth.ActorKindAPIKey:
		scope, ok := principal.EnvironmentScope()
		if ok && principal.HasPermission(auth.PermissionRunsCreate, scope) {
			return nil
		}
	case auth.ActorKindSession:
		if auth.RoleAllows(principal.Role, auth.PermissionRunsCreate) {
			return nil
		}
	}
	return forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()})
}

func taskStartPolicyFromAPI(request api.StartTaskRequest) (*int64, json.RawMessage, error) {
	var ttl *int64
	if request.TTL != "" {
		value, err := api.ParseDurationMilliseconds(
			request.TTL, "ttl", 1, api.MaxQueuedRunTTLMilliseconds,
		)
		if err != nil {
			return nil, nil, err
		}
		ttl = &value
	}
	retry, err := api.NormalizeStartActorRetry(request.Retry)
	if err != nil {
		return nil, nil, err
	}
	if len(retry) > 0 {
		if _, err := deployment.ParseRetryManifest(retry); err != nil {
			return nil, nil, fmt.Errorf("normalize retry: %w", err)
		}
	}
	return ttl, retry, nil
}

func (s *Server) writeTaskStartError(w http.ResponseWriter, err error) {
	var idempotencyConflict idempotency.ConflictError
	switch {
	case errors.As(err, &idempotencyConflict):
		writeError(w, conflict(codedError{
			code:    "idempotency_conflict",
			message: "idempotency key conflicts with an earlier task start",
		}))
	case errors.Is(err, errTaskNotDeployed):
		writeError(w, notFound(codedError{code: "task_not_deployed", message: err.Error()}))
	case errors.Is(err, errTaskWorkspaceNotFound):
		writeError(w, notFound(codedError{code: "workspace_not_found", message: err.Error()}))
	case errors.Is(err, errTaskWorkspaceUnavailable):
		writeError(w, conflict(codedError{
			code: "workspace_unavailable", message: err.Error(), retryable: true,
		}))
	case errors.Is(err, errTaskSecretUnavailable):
		writeError(w, conflict(codedError{code: "secret_unavailable", message: err.Error()}))
	case errors.Is(err, errTaskPayloadPresenceInvalid), errors.Is(err, errTaskStartInvalid):
		writeError(w, badRequest(codedError{code: "invalid_task_start", message: err.Error()}))
	default:
		writeError(w, unavailable(codedError{
			code:    "task_start_authority_unavailable",
			message: "task start authority is unavailable", retryable: true,
		}))
	}
}
