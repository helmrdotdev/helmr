package controlplane

import (
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func (s *Server) executeWorkspaceHTTP(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := ids.Parse(chi.URLParam(r, "workspaceID"))
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_workspace_reference", message: "workspace ID is invalid"}))
		return
	}
	var body api.ExecuteWorkspaceRequest
	if err := decodeJSON(r, &body); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, tooLarge(codedError{code: "workspace_exec_request_too_large", message: errWorkspaceExecTooLarge.Error()}))
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_workspace_exec", message: err.Error()}))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(body.IdempotencyKey)
	if err != nil || idempotencyKey == "" {
		if err == nil {
			err = errors.New("idempotency_key is required")
		}
		writeError(w, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}
	var stdin []byte
	if body.StdinBase64 != "" {
		stdin, err = base64.StdEncoding.Strict().DecodeString(body.StdinBase64)
		if err != nil {
			writeError(w, badRequest(codedError{code: "invalid_workspace_exec", message: "stdin_base64 must be canonical padded base64"}))
			return
		}
	}
	timeout := workspaceExecDefaultTimeout
	if body.Timeout != "" {
		timeoutMS, err := api.ParseDurationMilliseconds(
			body.Timeout,
			"timeout",
			1,
			workspaceExecMaxTimeout.Milliseconds(),
		)
		if err != nil {
			writeError(w, badRequest(codedError{code: "invalid_workspace_exec", message: err.Error()}))
			return
		}
		timeout = time.Duration(timeoutMS) * time.Millisecond
	}

	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_workspace_reference", message: err.Error()}))
		return
	}
	if !principal.HasPermission(auth.PermissionWorkspaceExecCreate, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	record, err := s.db.GetWorkspace(r.Context(), db.GetWorkspaceParams{
		OrgID:         pgvalue.UUID(principal.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ID:            pgvalue.UUID(workspaceID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "workspace_not_found", message: errWorkspaceNotFound.Error()}))
		return
	}
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "workspace_authority_unavailable",
			message:   errWorkspaceAuthorityUnavailable.Error(),
			retryable: true,
		}))
		return
	}
	admission, err := s.admitWorkspaceExec(r.Context(), workspaceExecRequest{
		OrgID:          principal.OrgID,
		ProjectID:      pgvalue.MustUUIDValue(projectID),
		EnvironmentID:  pgvalue.MustUUIDValue(environmentID),
		Workspace:      record,
		Creator:        workspaceExecCreatorFromActor(principal),
		Command:        body.Command,
		Cwd:            body.Cwd,
		Env:            body.Env,
		Stdin:          stdin,
		Timeout:        timeout,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.writeWorkspaceExecError(w, err)
		return
	}
	result, err := s.waitWorkspaceExec(r.Context(), admission)
	if err != nil {
		if errors.Is(err, r.Context().Err()) {
			return
		}
		s.writeWorkspaceExecError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) writeWorkspaceExecError(w http.ResponseWriter, err error) {
	var conflictError idempotency.ConflictError
	var classified apiError
	switch {
	case errors.As(err, &classified):
		writeError(w, classified)
	case errors.Is(err, errWorkspaceExecStdinTooLarge):
		writeError(w, tooLarge(codedError{code: "workspace_stdin_too_large", message: err.Error()}))
	case errors.Is(err, errWorkspaceExecTooLarge):
		writeError(w, tooLarge(codedError{code: "workspace_exec_request_too_large", message: err.Error()}))
	case errors.Is(err, errWorkspaceExecInvalid):
		writeError(w, badRequest(codedError{code: "invalid_workspace_exec", message: err.Error()}))
	case errors.Is(err, errWorkspaceSecretUnavailable):
		writeError(w, conflict(codedError{code: "secret_unavailable", message: err.Error()}))
	case errors.Is(err, errWorkspaceNotFound):
		writeError(w, notFound(codedError{code: "workspace_not_found", message: err.Error()}))
	case errors.Is(err, errWorkspaceBusy):
		writeError(w, conflict(codedError{code: "workspace_busy", message: err.Error(), retryable: true}))
	case errors.As(err, &conflictError):
		writeError(w, conflict(codedError{code: "idempotency_conflict", message: err.Error()}))
	default:
		s.log.Error("execute Workspace failed", "error", err)
		writeError(w, unavailable(codedError{
			code:      "workspace_authority_unavailable",
			message:   errWorkspaceAuthorityUnavailable.Error(),
			retryable: true,
		}))
	}
}
