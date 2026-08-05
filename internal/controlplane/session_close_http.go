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
)

const sessionCloseBodyLimit = int64(8 << 10)

func (s *Server) closeSessionHTTP(w http.ResponseWriter, r *http.Request) {
	request, err := decodeSessionCloseRequest(r)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, tooLarge(codedError{
				code:    "session_close_request_too_large",
				message: "session close request exceeds the 8 KiB limit",
			}))
			return
		}
		var coder errorCoder
		if errors.As(err, &coder) {
			writeError(w, badRequest(err))
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_session_close", message: err.Error()}))
		return
	}
	sessionID, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_session_id", message: err.Error()}))
		return
	}
	if err := api.ValidateCloseSessionRequest(request); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_session_id", message: err.Error()}))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}

	principal := actorFromContext(r.Context())
	if err := authorizeSessionCloseBeforeLookup(principal); err != nil {
		writeError(w, err)
		return
	}
	projectRef, environmentRef, err := environmentScopeRefsFromRequest(r, principal, "", "")
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_session_close", message: err.Error()}))
		return
	}
	_, _, environmentID, err := s.requestEnvironmentScope(
		r.Context(),
		principal,
		projectRef,
		environmentRef,
	)
	if err != nil {
		s.writeSessionCloseScopeError(w, err)
		return
	}
	actor, err := s.db.GetActor(r.Context(), db.GetActorParams{
		EnvironmentID: environmentID,
		ID:            pgvalue.UUID(sessionID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "session_not_found", message: "session not found"}))
		return
	}
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "session_close_authority_unavailable",
			message:   "session close address authority is unavailable",
			retryable: true,
		}))
		return
	}
	environmentUUID, err := pgvalue.UUIDValue(environmentID)
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "session_close_authority_unavailable",
			message:   "session close environment authority is unavailable",
			retryable: true,
		}))
		return
	}
	actorID, err := pgvalue.UUIDValue(actor.ID)
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "session_close_authority_unavailable",
			message:   "session close identity authority is unavailable",
			retryable: true,
		}))
		return
	}
	workspaceID, err := pgvalue.UUIDValue(actor.WorkspaceID)
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "session_close_authority_unavailable",
			message:   "session close workspace authority is unavailable",
			retryable: true,
		}))
		return
	}
	receipt, err := s.closeActor(r.Context(), actorCloseRequest{
		EnvironmentID: environmentUUID,
		SessionID:     actorID,
		WorkspaceID:   workspaceID, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.writeSessionCloseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func decodeSessionCloseRequest(r *http.Request) (api.CloseSessionRequest, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return api.CloseSessionRequest{}, err
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return api.CloseSessionRequest{}, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &members); err != nil || members == nil {
		return api.CloseSessionRequest{}, errors.New("session close request must be a JSON object")
	}
	if value, present := members["idempotency_key"]; present {
		var key string
		if string(value) == "null" || json.Unmarshal(value, &key) != nil ||
			strings.TrimSpace(key) == "" {
			return api.CloseSessionRequest{}, codedError{
				code:    "invalid_idempotency_key",
				message: "idempotency_key must be a non-empty string when present",
			}
		}
	}
	var request api.CloseSessionRequest
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return api.CloseSessionRequest{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return api.CloseSessionRequest{}, errors.New("session close request contains a trailing value")
	}
	return request, nil
}

func authorizeSessionCloseBeforeLookup(principal auth.Actor) error {
	switch principal.Kind {
	case auth.ActorKindAPIKey:
		scope, ok := principal.EnvironmentScope()
		if !ok {
			return unavailable(codedError{
				code:      "session_close_authority_unavailable",
				message:   errAPIKeyEnvironmentScopeRequired.Error(),
				retryable: true,
			})
		}
		if principal.HasPermission(auth.PermissionSessionsClose, scope) {
			return nil
		}
	case auth.ActorKindSession:
		if auth.RoleAllows(principal.Role, auth.PermissionSessionsClose) {
			return nil
		}
	}
	return forbidden(codedError{
		code:    "permission_required",
		message: errPermissionRequired.Error(),
	})
}

func (s *Server) writeSessionCloseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errActorCloseConflict):
		writeError(w, conflict(codedError{
			code:    "session_close_conflict",
			message: errActorCloseConflict.Error(),
		}))
	default:
		writeError(w, unavailable(codedError{
			code:      "session_close_authority_unavailable",
			message:   errActorCloseAuthority.Error(),
			retryable: true,
		}))
	}
}

func (s *Server) writeSessionCloseScopeError(w http.ResponseWriter, err error) {
	if isInvalidEnvironmentScopeReference(err) {
		writeError(w, badRequest(codedError{
			code:    "invalid_session_close",
			message: err.Error(),
		}))
		return
	}
	writeError(w, unavailable(codedError{
		code:      "session_close_authority_unavailable",
		message:   "session close environment scope is unavailable",
		retryable: true,
	}))
}

func writeSessionCloseAuthError(w http.ResponseWriter, log *slog.Logger, err error) {
	if !errors.Is(err, auth.ErrUnauthenticated) {
		log.Error("Session close authentication failed", "error", err)
		writeError(w, unavailable(codedError{
			code:      "session_close_authority_unavailable",
			message:   "session close authentication is unavailable",
			retryable: true,
		}))
		return
	}
	writeError(w, unauthorized(codedError{
		code:    "authentication_required",
		message: "authentication is required",
	}))
}

func limitSessionCloseBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, sessionCloseBodyLimit)
		next.ServeHTTP(w, r)
	})
}
