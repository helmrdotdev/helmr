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

func (s *Server) executeComputerHTTP(w http.ResponseWriter, r *http.Request) {
	computerID, err := ids.Parse(chi.URLParam(r, "computerID"))
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: "computer ID is invalid"}))
		return
	}
	var body api.ExecuteComputerRequest
	if err := decodeJSON(r, &body); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, tooLarge(codedError{code: "computer_command_request_too_large", message: errComputerCommandTooLarge.Error()}))
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_computer_command", message: err.Error()}))
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
			writeError(w, badRequest(codedError{code: "invalid_computer_command", message: "stdin_base64 must be canonical padded base64"}))
			return
		}
	}
	timeout := computerCommandDefaultTimeout
	if body.Timeout != "" {
		timeoutMS, err := api.ParseDurationMilliseconds(
			body.Timeout,
			"timeout",
			1,
			computerCommandMaxTimeout.Milliseconds(),
		)
		if err != nil {
			writeError(w, badRequest(codedError{code: "invalid_computer_command", message: err.Error()}))
			return
		}
		timeout = time.Duration(timeoutMS) * time.Millisecond
	}

	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	if !canAccessComputerCommandOutput(principal, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	admission, err := s.admitComputerCommand(r.Context(), computerCommandRequest{
		OrgID:          principal.OrgID,
		ProjectID:      pgvalue.MustUUIDValue(projectID),
		EnvironmentID:  pgvalue.MustUUIDValue(environmentID),
		ComputerID:     computerID,
		Creator:        computerCommandCreatorFromActor(principal),
		Command:        body.Command,
		Cwd:            body.Cwd,
		Env:            body.Env,
		Stdin:          stdin,
		Timeout:        timeout,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.writeComputerCommandError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, api.CommandReceipt{CommandID: pgvalue.MustUUIDValue(admission.Process.ID).String()})
}

func canAccessComputerCommandOutput(principal auth.Actor, scope auth.Scope) bool {
	return principal.HasPermission(auth.PermissionComputerCommandCreate, scope)
}

func (s *Server) getComputerCommandHTTP(w http.ResponseWriter, r *http.Request) {
	commandID, err := ids.Parse(chi.URLParam(r, "commandID"))
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_command_reference", message: "command ID is invalid"}))
		return
	}
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	if !canAccessComputerCommandOutput(principal, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	process, err := s.db.GetCommand(r.Context(), db.GetCommandParams{
		OrgID:         pgvalue.UUID(principal.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		CommandID:     pgvalue.UUID(commandID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "computer_command_not_found", message: "computer exec process was not found"}))
		return
	}
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "computer_authority_unavailable",
			message:   errComputerAuthorityUnavailable.Error(),
			retryable: true,
		}))
		return
	}
	resource, err := publicCommandInfo(process)
	if err != nil {
		s.writeComputerCommandError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resource)
}

func (s *Server) writeComputerCommandError(w http.ResponseWriter, err error) {
	var expired idempotency.ExpiredError
	if errors.As(err, &expired) {
		writeError(w, gone(expired))
		return
	}
	var conflictError idempotency.ConflictError
	var classified apiError
	switch {
	case errors.As(err, &classified):
		writeError(w, classified)
	case errors.Is(err, errComputerCommandStdinTooLarge):
		writeError(w, tooLarge(codedError{code: "computer_stdin_too_large", message: err.Error()}))
	case errors.Is(err, errComputerCommandTooLarge):
		writeError(w, tooLarge(codedError{code: "computer_command_request_too_large", message: err.Error()}))
	case errors.Is(err, errComputerCommandInvalid):
		writeError(w, badRequest(codedError{code: "invalid_computer_command", message: err.Error()}))
	case errors.Is(err, errComputerSecretUnavailable):
		writeError(w, conflict(codedError{code: "secret_unavailable", message: err.Error()}))
	case errors.Is(err, errComputerNotFound):
		writeError(w, notFound(codedError{code: "computer_not_found", message: err.Error()}))
	case errors.Is(err, errComputerBusy):
		writeError(w, conflict(codedError{code: "computer_busy", message: err.Error(), retryable: true}))
	case errors.As(err, &conflictError):
		writeError(w, conflict(codedError{code: "idempotency_conflict", message: err.Error()}))
	default:
		s.log.Error("execute Computer failed", "error", err)
		writeError(w, unavailable(codedError{
			code:      "computer_authority_unavailable",
			message:   errComputerAuthorityUnavailable.Error(),
			retryable: true,
		}))
	}
}
