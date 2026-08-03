package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const actorCloseBodyLimit = int64(8 << 10)

func (s *Server) closeActorHTTP(w http.ResponseWriter, r *http.Request) {
	request, err := decodeActorCloseRequest(r)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, tooLarge(codedError{
				code:    "actor_close_request_too_large",
				message: "Actor close request exceeds the 8 KiB limit",
			}))
			return
		}
		var coder errorCoder
		if errors.As(err, &coder) {
			writeError(w, badRequest(err))
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_actor_close", message: err.Error()}))
		return
	}
	actorDeclaredID := chi.URLParam(r, "actorDeclaredID")
	if err := api.ValidateActorDeclaredID(actorDeclaredID); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_actor_reference", message: err.Error()}))
		return
	}
	if err := api.ValidateActorOperationRequest(request); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_actor_reference", message: err.Error()}))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}

	principal := actorFromContext(r.Context())
	if err := authorizeActorCloseBeforeLookup(principal); err != nil {
		writeError(w, err)
		return
	}
	projectRef, environmentRef, err := environmentScopeRefsFromRequest(r, principal, "", "")
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_actor_close", message: err.Error()}))
		return
	}
	_, _, environmentID, err := s.requestEnvironmentScope(
		r.Context(),
		principal,
		projectRef,
		environmentRef,
	)
	if err != nil {
		s.writeActorCloseScopeError(w, err)
		return
	}
	actor, err := s.resolveActorCloseAddress(
		r,
		environmentID,
		actorDeclaredID,
		request,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "actor_not_found", message: "Actor not found"}))
		return
	}
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "actor_close_authority_unavailable",
			message:   "Actor close address authority is unavailable",
			retryable: true,
		}))
		return
	}
	environmentUUID, err := pgvalue.UUIDValue(environmentID)
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "actor_close_authority_unavailable",
			message:   "Actor close Environment authority is unavailable",
			retryable: true,
		}))
		return
	}
	actorID, err := pgvalue.UUIDValue(actor.ID)
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "actor_close_authority_unavailable",
			message:   "Actor close identity authority is unavailable",
			retryable: true,
		}))
		return
	}
	workspaceID, err := pgvalue.UUIDValue(actor.WorkspaceID)
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "actor_close_authority_unavailable",
			message:   "Actor close Workspace authority is unavailable",
			retryable: true,
		}))
		return
	}
	receipt, err := s.closeActor(r.Context(), actorCloseRequest{
		EnvironmentID: environmentUUID,
		ActorID:       actorID,
		WorkspaceID:   workspaceID, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.writeActorCloseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func decodeActorCloseRequest(r *http.Request) (api.ActorOperationRequest, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return api.ActorOperationRequest{}, err
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return api.ActorOperationRequest{}, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &members); err != nil || members == nil {
		return api.ActorOperationRequest{}, errors.New("Actor close request must be a JSON object")
	}
	for _, name := range []string{"actor_id", "actor_key"} {
		if value, present := members[name]; present {
			var address string
			if string(value) == "null" || json.Unmarshal(value, &address) != nil || address == "" {
				return api.ActorOperationRequest{}, codedError{
					code:    "invalid_actor_reference",
					message: name + " must be a non-empty string",
				}
			}
		}
	}
	if value, present := members["idempotency_key"]; present {
		var key string
		if string(value) == "null" || json.Unmarshal(value, &key) != nil ||
			strings.TrimSpace(key) == "" {
			return api.ActorOperationRequest{}, codedError{
				code:    "invalid_idempotency_key",
				message: "idempotency_key must be a non-empty string when present",
			}
		}
	}
	var request api.ActorOperationRequest
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return api.ActorOperationRequest{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return api.ActorOperationRequest{}, errors.New("Actor close request contains a trailing value")
	}
	return request, nil
}

func authorizeActorCloseBeforeLookup(principal auth.Actor) error {
	switch principal.Kind {
	case auth.ActorKindAPIKey:
		scope, ok := principal.EnvironmentScope()
		if !ok {
			return unavailable(codedError{
				code:      "actor_close_authority_unavailable",
				message:   errAPIKeyEnvironmentScopeRequired.Error(),
				retryable: true,
			})
		}
		if principal.HasPermission(auth.PermissionActorsCloseManage, scope) {
			return nil
		}
	case auth.ActorKindSession:
		if auth.RoleAllows(principal.Role, auth.PermissionActorsCloseManage) {
			return nil
		}
	}
	return forbidden(codedError{
		code:    "permission_required",
		message: errPermissionRequired.Error(),
	})
}

func (s *Server) resolveActorCloseAddress(
	r *http.Request,
	environmentID pgtype.UUID,
	actorDeclaredID string,
	request api.ActorOperationRequest,
) (db.Actor, error) {
	address := actorReadAddress{key: request.ActorKey}
	if request.ActorID != "" {
		id, err := ids.Parse(request.ActorID)
		if err != nil {
			return db.Actor{}, err
		}
		address.id = pgvalue.UUID(id)
	}
	return resolveActorAddress(
		r.Context(), s.db, environmentID, actorDeclaredID,
		address,
	)
}

func (s *Server) writeActorCloseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errActorCloseConflict):
		writeError(w, conflict(codedError{
			code:    "actor_close_conflict",
			message: errActorCloseConflict.Error(),
		}))
	default:
		writeError(w, unavailable(codedError{
			code:      "actor_close_authority_unavailable",
			message:   errActorCloseAuthority.Error(),
			retryable: true,
		}))
	}
}

func (s *Server) writeActorCloseScopeError(w http.ResponseWriter, err error) {
	if isInvalidEnvironmentScopeReference(err) {
		writeError(w, badRequest(codedError{
			code:    "invalid_actor_close",
			message: err.Error(),
		}))
		return
	}
	writeError(w, unavailable(codedError{
		code:      "actor_close_authority_unavailable",
		message:   "Actor close Environment scope is unavailable",
		retryable: true,
	}))
}

func writeActorCloseAuthError(w http.ResponseWriter, log *slog.Logger, err error) {
	if !errors.Is(err, auth.ErrUnauthenticated) {
		log.Error("Actor close authentication failed", "error", err)
		writeError(w, unavailable(codedError{
			code:      "actor_close_authority_unavailable",
			message:   "Actor close authentication is unavailable",
			retryable: true,
		}))
		return
	}
	writeError(w, unauthorized(codedError{
		code:    "authentication_required",
		message: "authentication is required",
	}))
}

func limitActorCloseBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, actorCloseBodyLimit)
		next.ServeHTTP(w, r)
	})
}
