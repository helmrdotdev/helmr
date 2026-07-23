package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const actorUpdateBodyLimit = int64(maxActorMetadataBytes + 16<<10)

func (s *Server) updateActorHTTP(w http.ResponseWriter, r *http.Request) {
	request, err := decodeUpdateActorRequest(r)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, tooLarge(codedError{
				code:    "actor_update_request_too_large",
				message: "Actor update request is too large",
			}))
			return
		}
		var coder errorCoder
		if errors.As(err, &coder) {
			writeError(w, badRequest(err))
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_actor_update", message: err.Error()}))
		return
	}

	actorDeclaredID := chi.URLParam(r, "actorDeclaredID")
	if err := api.ValidateActorDeclaredID(actorDeclaredID); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_actor_reference", message: err.Error()}))
		return
	}
	if err := api.ValidateActorReference(api.ActorReference{
		ActorID:  request.ActorID,
		ActorKey: request.ActorKey,
	}); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_actor_reference", message: err.Error()}))
		return
	}
	if err := api.ValidateUpdateActorRequest(request); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_actor_update", message: err.Error()}))
		return
	}

	principal := actorFromContext(r.Context())
	if err := authorizeActorUpdateBeforeLookup(principal); err != nil {
		writeError(w, err)
		return
	}
	scope, _, environmentID, err := s.actorReadScope(r, principal)
	if err != nil {
		s.writeActorUpdateScopeError(w, err)
		return
	}
	if !principal.HasPermission(auth.PermissionActorsUpdate, scope) {
		writeError(w, forbidden(codedError{
			code: "permission_required", message: errPermissionRequired.Error(),
		}))
		return
	}

	update, err := actorUpdateRequestFromAPI(environmentID, actorDeclaredID, request)
	if err != nil {
		s.writeActorUpdateError(w, err)
		return
	}
	status, err := s.updateActor(r.Context(), update)
	if err != nil {
		s.writeActorUpdateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func decodeUpdateActorRequest(r *http.Request) (api.UpdateActorRequest, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return api.UpdateActorRequest{}, err
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return api.UpdateActorRequest{}, codedError{
			code: "invalid_actor_update", message: err.Error(),
		}
	}
	root, err := decodeActorStartObject(canonical, "Actor update request")
	if err != nil {
		return api.UpdateActorRequest{}, codedError{
			code: "invalid_actor_update", message: err.Error(),
		}
	}
	if err := rejectActorStartNullFields(root, "", "actor_id", "actor_key"); err != nil {
		return api.UpdateActorRequest{}, codedError{
			code: "invalid_actor_reference", message: err.Error(),
		}
	}
	if err := rejectActorStartNullFields(root, "", "metadata", "tags", "expires_at"); err != nil {
		return api.UpdateActorRequest{}, codedError{
			code: "invalid_actor_update", message: err.Error(),
		}
	}
	if err := rejectActorStartNullTagElements(root["tags"], "tags"); err != nil {
		return api.UpdateActorRequest{}, codedError{
			code: "invalid_actor_update", message: err.Error(),
		}
	}
	if _, metadata := root["metadata"]; !metadata {
		if _, tags := root["tags"]; !tags {
			if _, expiresAt := root["expires_at"]; !expiresAt {
				return api.UpdateActorRequest{}, codedError{
					code:    "invalid_actor_update",
					message: "at least one of metadata, tags, or expires_at is required",
				}
			}
		}
	}
	if rawExpiresAt := root["expires_at"]; len(rawExpiresAt) > 0 {
		var value string
		if err := json.Unmarshal(rawExpiresAt, &value); err != nil {
			return api.UpdateActorRequest{}, codedError{
				code: "invalid_actor_update", message: "expires_at must be a string",
			}
		}
		if _, err := api.ParseRFC3339NanosecondInstant(value); err != nil {
			return api.UpdateActorRequest{}, codedError{
				code: "invalid_actor_update", message: err.Error(),
			}
		}
	}

	var request api.UpdateActorRequest
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return api.UpdateActorRequest{}, codedError{
			code: "invalid_actor_update", message: err.Error(),
		}
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return api.UpdateActorRequest{}, codedError{
			code: "invalid_actor_update", message: "Actor update request contains a trailing value",
		}
	}
	return request, nil
}

func authorizeActorUpdateBeforeLookup(principal auth.Actor) error {
	switch principal.Kind {
	case auth.ActorKindAPIKey:
		scope, ok := principal.EnvironmentScope()
		if !ok {
			return unavailable(codedError{
				code:      "actor_update_authority_unavailable",
				message:   errAPIKeyEnvironmentScopeRequired.Error(),
				retryable: true,
			})
		}
		if principal.HasPermission(auth.PermissionActorsUpdate, scope) {
			return nil
		}
	case auth.ActorKindSession:
		if auth.RoleAllows(principal.Role, auth.PermissionActorsUpdate) {
			return nil
		}
	}
	return forbidden(codedError{
		code: "permission_required", message: errPermissionRequired.Error(),
	})
}

func (s *Server) writeActorUpdateScopeError(w http.ResponseWriter, err error) {
	if isInvalidEnvironmentScopeReference(err) || isScopeRequestError(err) {
		writeError(w, badRequest(codedError{
			code: "invalid_actor_reference", message: err.Error(),
		}))
		return
	}
	s.writeActorUpdateAuthorityError(w)
}

func (s *Server) writeActorUpdateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errActorUpdateInvalid):
		writeError(w, badRequest(codedError{
			code: "invalid_actor_update", message: err.Error(),
		}))
	case errors.Is(err, errActorUpdateNotFound):
		writeError(w, notFound(codedError{
			code: "actor_not_found", message: errActorUpdateNotFound.Error(),
		}))
	case errors.Is(err, errActorUpdateConflict):
		writeError(w, conflict(codedError{
			code: "actor_update_conflict", message: errActorUpdateConflict.Error(),
		}))
	default:
		s.writeActorUpdateAuthorityError(w)
	}
}

func (s *Server) writeActorUpdateAuthorityError(w http.ResponseWriter) {
	writeError(w, unavailable(codedError{
		code:      "actor_update_authority_unavailable",
		message:   errActorUpdateAuthority.Error(),
		retryable: true,
	}))
}

func writeActorUpdateAuthError(w http.ResponseWriter, log *slog.Logger, err error) {
	if !errors.Is(err, auth.ErrUnauthenticated) {
		log.Error("Actor update authentication failed", "error", err)
		writeError(w, unavailable(codedError{
			code:      "actor_update_authority_unavailable",
			message:   "Actor update authentication is unavailable",
			retryable: true,
		}))
		return
	}
	writeError(w, unauthorized(codedError{
		code: "authentication_required", message: "authentication is required",
	}))
}

func limitActorUpdateBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, actorUpdateBodyLimit)
		next.ServeHTTP(w, r)
	})
}
