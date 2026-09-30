package controlplane

import (
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func (s *Server) executeComputerHTTP(w http.ResponseWriter, r *http.Request) {
	computerID, err := ids.Parse(chi.URLParam(r, "computerID"))
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: "computer ID is invalid"}))
		return
	}
	var body api.ExecuteComputerRequest
	if err := decodeRequestJSON(r, &body); err != nil {
		if isRequestBodyTooLarge(err) {
			writeError(w, err)
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
	timeout := command.DefaultTimeout
	if body.Timeout != "" {
		timeoutMS, err := api.ParseDurationMilliseconds(
			body.Timeout,
			"timeout",
			1,
			command.MaxTimeout.Milliseconds(),
		)
		if err != nil {
			writeError(w, badRequest(codedError{code: "invalid_computer_command", message: err.Error()}))
			return
		}
		timeout = time.Duration(timeoutMS) * time.Millisecond
	}

	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	if !canAccessComputerCommandOutput(principal, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	created, err := command.Create(r.Context(), s.tx, command.CreateRequest{
		OrgID:          principal.OrgID,
		ProjectID:      pgvalue.MustUUIDValue(projectID),
		EnvironmentID:  pgvalue.MustUUIDValue(environmentID),
		ComputerID:     computerID,
		Creator:        computerCommandCreatorFromPrincipal(principal),
		Argv:           body.Command,
		Cwd:            body.Cwd,
		Env:            body.Env,
		Stdin:          stdin,
		Timeout:        timeout,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.writeCommandError(w, err, commandCreateOperation)
		return
	}
	writeJSON(w, http.StatusAccepted, api.CommandReceipt{CommandID: pgvalue.MustUUIDValue(created.ID).String()})
}

func canAccessComputerCommandOutput(principal auth.Principal, scope auth.Scope) bool {
	return principal.HasPermission(auth.PermissionComputerCommandCreate, scope)
}

func (s *Server) getComputerCommandHTTP(w http.ResponseWriter, r *http.Request) {
	commandID, err := ids.Parse(chi.URLParam(r, "commandID"))
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_command_reference", message: "command ID is invalid"}))
		return
	}
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	if !canAccessComputerCommandOutput(principal, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	process, err := command.Get(r.Context(), s.db, command.Ref{
		OrgID: principal.OrgID, ProjectID: pgvalue.MustUUIDValue(projectID),
		EnvironmentID: pgvalue.MustUUIDValue(environmentID), CommandID: commandID,
	})
	if err != nil {
		s.writeCommandError(w, err, commandGetOperation)
		return
	}
	resource, err := publicCommandInfo(process)
	if err != nil {
		s.writeCommandError(w, err, commandGetOperation)
		return
	}
	writeJSON(w, http.StatusOK, resource)
}
