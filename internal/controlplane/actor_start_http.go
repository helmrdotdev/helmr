package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

const actorStartBodyLimit = int64(
	maxRunMetadataBytes +
		64<<10,
)

func (s *Server) startActorHTTP(w http.ResponseWriter, r *http.Request) {
	request, err := decodeStartActorRequest(r)
	if err != nil {
		if isRequestBodyTooLarge(err) {
			writeError(w, err)
			return
		}
		var coder errorCoder
		if errors.As(err, &coder) {
			writeError(w, badRequest(err))
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_actor_start", message: err.Error()}))
		return
	}
	actorDeclaredID := chi.URLParam(r, "actorDeclaredID")
	if err := api.ValidateActorDeclaredID(actorDeclaredID); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_actor_start", message: err.Error()}))
		return
	}
	if err := api.ValidateComputerIDTarget(request.Computer); err != nil {
		writeError(w, badRequest(codedError{
			code:    "invalid_computer_reference",
			message: err.Error(),
		}))
		return
	}
	if err := api.ValidateStartActorRequest(request); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_actor_start", message: err.Error()}))
		return
	}

	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}

	principal := actorFromContext(r.Context())
	if err := authorizeActorStartBeforeLookup(principal); err != nil {
		writeError(w, err)
		return
	}
	projectRef, environmentRef, err := environmentScopeRefsFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{
			code:    "invalid_actor_start",
			message: err.Error(),
		}))
		return
	}
	_, projectID, environmentID, err := s.requestEnvironmentScope(
		r.Context(),
		principal,
		projectRef,
		environmentRef,
	)
	if err != nil {
		s.writeActorStartScopeError(w, err)
		return
	}
	projectUUID, err := pgvalue.UUIDValue(projectID)
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "actor_start_authority_unavailable",
			message:   "actor start project authority is unavailable",
			retryable: true,
		}))
		return
	}
	environmentUUID, err := pgvalue.UUIDValue(environmentID)
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "actor_start_authority_unavailable",
			message:   "actor start environment authority is unavailable",
			retryable: true,
		}))
		return
	}

	startRequest, err := actorStartRequestFromAPI(
		principal,
		projectUUID,
		environmentUUID,
		actorDeclaredID,
		idempotencyKey,
		request,
	)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_actor_start", message: err.Error()}))
		return
	}
	result, err := s.startActor(r.Context(), startRequest)
	if err != nil {
		s.writeActorStartError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, api.StartActorResponse{
		SessionID: result.SessionID.String(),
		RunID:     result.BootRunID.String(),
	})
}

func decodeStartActorRequest(r *http.Request) (api.StartActorRequest, error) {
	raw, err := readRequestBody(r)
	if err != nil {
		return api.StartActorRequest{}, err
	}
	canonical, err := canonicalRequestJSON(raw)
	if err != nil {
		return api.StartActorRequest{}, err
	}
	if err := rejectActorStartNulls(canonical); err != nil {
		return api.StartActorRequest{}, err
	}
	var request api.StartActorRequest
	if err := decodeClosedJSON(canonical, &request); err != nil {
		return api.StartActorRequest{}, err
	}
	return request, nil
}

func authorizeActorStartBeforeLookup(principal auth.Actor) error {
	switch principal.Kind {
	case auth.ActorKindAPIKey:
		scope, ok := principal.EnvironmentScope()
		if !ok {
			return unavailable(codedError{
				code:      "actor_start_authority_unavailable",
				message:   errAPIKeyEnvironmentScopeRequired.Error(),
				retryable: true,
			})
		}
		if principal.HasPermission(auth.PermissionActorsStart, scope) {
			return nil
		}
	case auth.ActorKindSession:
		if auth.RoleAllows(principal.Role, auth.PermissionActorsStart) {
			return nil
		}
	}
	return forbidden(codedError{
		code:    "permission_required",
		message: errPermissionRequired.Error(),
	})
}

func (s *Server) writeActorStartScopeError(w http.ResponseWriter, err error) {
	if isInvalidEnvironmentScopeReference(err) {
		writeError(w, badRequest(codedError{
			code:    "invalid_actor_start",
			message: err.Error(),
		}))
		return
	}
	writeError(w, unavailable(codedError{
		code:      "actor_start_authority_unavailable",
		message:   "actor start environment scope is unavailable",
		retryable: true,
	}))
}

func rejectActorStartNulls(canonical []byte) error {
	root, err := decodeActorStartObject(canonical, "actor start request")
	if err != nil {
		return err
	}
	if err := rejectActorStartNullFields(root, "", "key", "run"); err != nil {
		return err
	}
	if err := validateActorStartIdempotencyWire(root["idempotency_key"]); err != nil {
		return err
	}
	if raw := root["computer"]; len(raw) > 0 {
		computer, err := decodeActorStartObject(raw, "computer")
		if err != nil {
			return codedError{code: "invalid_computer_reference", message: err.Error()}
		}
		if err := rejectActorStartNullFields(computer, "computer.", "id", "key"); err != nil {
			return codedError{code: "invalid_computer_reference", message: err.Error()}
		}
		for _, field := range []string{"id", "key"} {
			if value, ok := computer[field]; ok {
				var decoded string
				if err := json.Unmarshal(value, &decoded); err != nil {
					return codedError{
						code:    "invalid_computer_reference",
						message: "computer." + field + " must be a string",
					}
				}
			}
		}
	}
	if raw := root["run"]; len(raw) > 0 {
		run, err := decodeActorStartObject(raw, "run")
		if err != nil {
			return err
		}
		if err := rejectActorStartNullFields(
			run,
			"run.",
			"queue",
			"concurrency_key",
			"priority",
			"ttl",
			"retry",
			"metadata",
			"tags",
		); err != nil {
			return err
		}
		if err := rejectActorStartNullTagElements(run["tags"], "run.tags"); err != nil {
			return err
		}
		for _, field := range []string{"queue", "ttl"} {
			if err := rejectActorStartEmptyString(run, field, "run."+field); err != nil {
				return err
			}
		}
		if raw := run["retry"]; len(raw) > 0 {
			retry, err := decodeActorStartObject(raw, "run.retry")
			if err != nil {
				return err
			}
			if err := rejectActorStartNullFields(retry, "run.retry.", "enabled", "max_attempts", "backoff"); err != nil {
				return err
			}
			if raw := retry["backoff"]; len(raw) > 0 {
				backoff, err := decodeActorStartObject(raw, "run.retry.backoff")
				if err != nil {
					return err
				}
				if err := rejectActorStartNullFields(
					backoff,
					"run.retry.backoff.",
					"min_delay",
					"max_delay",
					"factor",
					"jitter",
				); err != nil {
					return err
				}
				for _, field := range []string{"min_delay", "max_delay", "jitter"} {
					if err := rejectActorStartEmptyString(
						backoff,
						field,
						"run.retry.backoff."+field,
					); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func validateActorStartIdempotencyWire(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	if bytes.Equal(raw, []byte("null")) {
		return codedError{
			code:    "invalid_idempotency_key",
			message: "idempotency_key must not be null",
		}
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return codedError{
			code:    "invalid_idempotency_key",
			message: "idempotency_key must be a string",
		}
	}
	if strings.TrimSpace(value) == "" {
		return codedError{
			code:    "invalid_idempotency_key",
			message: "idempotency_key must not be empty or whitespace",
		}
	}
	return nil
}

func rejectActorStartEmptyString(
	object map[string]json.RawMessage,
	field string,
	label string,
) error {
	raw, ok := object[field]
	if !ok {
		return nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	if value == "" {
		return fmt.Errorf("%s must not be empty", label)
	}
	return nil
}

func decodeActorStartObject(raw []byte, label string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("%s must be an object", label)
	}
	if object == nil {
		return nil, fmt.Errorf("%s must be an object", label)
	}
	return object, nil
}

func rejectActorStartNullFields(
	object map[string]json.RawMessage,
	prefix string,
	fields ...string,
) error {
	for _, field := range fields {
		if raw, ok := object[field]; ok && bytes.Equal(raw, []byte("null")) {
			return fmt.Errorf("%s%s must not be null", prefix, field)
		}
	}
	return nil
}

func rejectActorStartNullTagElements(raw []byte, label string) error {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var tags []json.RawMessage
	if err := json.Unmarshal(raw, &tags); err != nil {
		return nil
	}
	for _, tag := range tags {
		if bytes.Equal(tag, []byte("null")) {
			return fmt.Errorf("%s entries must not be null", label)
		}
	}
	return nil
}

func actorStartRequestFromAPI(
	principal auth.Actor,
	projectID uuid.UUID,
	environmentID uuid.UUID,
	actorDeclaredID string,
	idempotencyKey string,
	request api.StartActorRequest,
) (actorStartRequest, error) {
	return actorStartRequestFromScope(
		principal.OrgID,
		projectID,
		environmentID,
		actorDeclaredID,
		idempotencyKey,
		api.ActorStartOptions{
			Key:      request.Key,
			Computer: request.Computer, Run: request.Run,
		},
	)
}

func actorStartRequestFromScope(
	orgID uuid.UUID,
	projectID uuid.UUID,
	environmentID uuid.UUID,
	actorDeclaredID string,
	idempotencyKey string,
	request api.ActorStartOptions,
) (actorStartRequest, error) {
	run := api.StartActorRunOptions{}
	if request.Run != nil {
		run = *request.Run
	}
	ttl := (*int64)(nil)
	if run.TTL != "" {
		value, err := api.ParseDurationMilliseconds(
			run.TTL,
			"run.ttl",
			1,
			api.MaxQueuedRunTTLMilliseconds,
		)
		if err != nil {
			return actorStartRequest{}, err
		}
		ttl = &value
	}
	retryPolicy, err := api.NormalizeStartActorRetry(run.Retry)
	if err != nil {
		return actorStartRequest{}, err
	}
	if len(retryPolicy) > 0 {
		if _, err := definition.ParseRetry(retryPolicy); err != nil {
			return actorStartRequest{}, fmt.Errorf("normalize run.retry: %w", err)
		}
	}
	computerID, err := ids.Parse(request.Computer.ID)
	if err != nil {
		return actorStartRequest{}, err
	}
	return actorStartRequest{
		OrgID:                 orgID,
		ProjectID:             projectID,
		EnvironmentID:         environmentID,
		ActorDeclaredID:       actorDeclaredID,
		ComputerID:            computerID,
		Key:                   request.Key,
		IdempotencyKey:        idempotencyKey,
		ManagedQueueName:      run.Queue,
		ManagedConcurrencyKey: run.ConcurrencyKey,
		ManagedPriority:       run.Priority,
		ManagedQueuedTTLMS:    ttl,
		ManagedRetryPolicy:    retryPolicy,
		ManagedRunMetadata:    run.Metadata,
		ManagedRunTags:        run.Tags,
	}, nil
}

func (s *Server) writeActorStartError(w http.ResponseWriter, err error) {
	var expired idempotency.ExpiredError
	if errors.As(err, &expired) {
		writeError(w, gone(expired))
		return
	}
	var idempotencyConflict idempotency.ConflictError
	var keyConflict ActorKeyConflictError
	var classified apiError
	switch {
	case errors.As(err, &classified):
		writeError(w, classified)
	case errors.As(err, &idempotencyConflict):
		writeError(w, conflict(codedError{
			code:    "idempotency_conflict",
			message: "idempotency key conflicts with an earlier actor start",
		}))
	case errors.As(err, &keyConflict):
		writeError(w, conflict(codedError{code: "actor_key_conflict", message: keyConflict.Error()}))
	case errors.Is(err, errActorStartNotDeployed):
		writeError(w, notFound(codedError{code: "actor_not_deployed", message: errActorStartNotDeployed.Error()}))
	case errors.Is(err, errActorStartComputerNotFound):
		writeError(w, notFound(codedError{
			code:    "computer_not_found",
			message: errActorStartComputerNotFound.Error(),
		}))
	case errors.Is(err, errActorStartComputerConflict):
		writeError(w, conflict(codedError{
			code:      "computer_unavailable",
			message:   errActorStartComputerConflict.Error(),
			retryable: true,
		}))
	case errors.Is(err, errActorStartSecretUnavailable):
		writeError(w, conflict(codedError{
			code:    "secret_unavailable",
			message: errActorStartSecretUnavailable.Error(),
		}))
	case errors.Is(err, errActorStartInvalid):
		writeError(w, badRequest(codedError{code: "invalid_actor_start", message: err.Error()}))
	case errors.Is(err, errActorStartAuthority):
		writeError(w, unavailable(codedError{
			code:      "actor_start_authority_unavailable",
			message:   errActorStartAuthority.Error(),
			retryable: true,
		}))
	default:
		writeError(w, unavailable(codedError{
			code:      "actor_start_authority_unavailable",
			message:   "actor start authority is unavailable",
			retryable: true,
		}))
	}
}
