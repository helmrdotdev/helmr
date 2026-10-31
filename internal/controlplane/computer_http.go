package controlplane

import (
	"errors"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const computerCreateBodyLimit = int64(256 << 10)

func computerScope(orgID uuid.UUID, projectID, environmentID pgtype.UUID) computer.Scope {
	return computer.Scope{
		OrgID:         orgID,
		ProjectID:     pgvalue.MustUUIDValue(projectID),
		EnvironmentID: pgvalue.MustUUIDValue(environmentID),
	}
}

func (s *Server) createComputerHTTP(w http.ResponseWriter, r *http.Request) {
	var request api.CreateComputerRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		if isRequestBodyTooLarge(err) {
			s.writeOperationError(w, r, err)
			return
		}
		s.writeOperationError(w, r, badRequest(codedError{code: "invalid_computer_create", message: err.Error()}))
		return
	}
	declaredID := request.Template
	if err := api.ValidateDefinitionID(declaredID); err != nil {
		s.writeOperationError(w, r, badRequest(codedError{code: "invalid_computer_create", message: err.Error()}))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		s.writeOperationError(w, r, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		s.writeOperationError(w, r, badRequest(codedError{code: "invalid_computer_create", message: err.Error()}))
		return
	}
	if permissionErr := auth.Require(principal, auth.PermissionComputersCreate, scope,
		codedError{code: "permission_required", message: errPermissionRequired.Error()}); permissionErr != nil {
		s.writeOperationError(w, r, forbidden(permissionErr))
		return
	}
	result, err := s.computers.Create(r.Context(), s.tx, computer.Request{Principal: principal,
		Scope:             computerScope(principal.OrgID, projectID, environmentID),
		DefinitionKey:     declaredID,
		DeleteAfterUnused: request.DeleteAfterUnused,
		Key:               request.Key,
		Secrets:           request.Secrets,
		IdempotencyKey:    idempotencyKey,
	})
	if err != nil {
		s.writeComputerError(w, r, err, computerCreateOperation, "create Computer failed")
		return
	}
	writeJSON(w, http.StatusCreated, apiComputerSnapshot(result.Snapshot))
}

func (s *Server) getComputerHTTP(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		s.writeOperationError(w, r, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	if permissionErr := auth.Require(principal, auth.PermissionComputersRead, scope,
		codedError{code: "permission_required", message: errPermissionRequired.Error()}); permissionErr != nil {
		s.writeOperationError(w, r, forbidden(permissionErr))
		return
	}
	computerID, err := ids.Parse(chi.URLParam(r, "computerID"))
	if err != nil {
		s.writeOperationError(w, r, badRequest(codedError{
			code: "invalid_computer_reference", message: "computerID must be a canonical UUIDv7",
		}))
		return
	}
	snapshot, err := computer.Read(r.Context(), s.tx, computerScope(principal.OrgID, projectID, environmentID), computerID)
	if err != nil {
		s.writeComputerError(w, r, err, computerReadOperation, "read Computer failed")
		return
	}
	view := apiComputerSnapshot(snapshot)
	if !principal.HasPermission(auth.PermissionComputersRead, scope) {
		view.Secrets = nil
		view.Error = nil
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) deleteComputerHTTP(w http.ResponseWriter, r *http.Request) {
	computerID, err := ids.Parse(chi.URLParam(r, "computerID"))
	if err != nil {
		s.writeOperationError(w, r, badRequest(codedError{
			code:    "invalid_computer_reference",
			message: "computer ID is invalid",
		}))
		return
	}
	var request api.DeleteComputerRequest
	if err := decodeOptionalRequestJSON(r, &request); err != nil {
		if isRequestBodyTooLarge(err) {
			s.writeOperationError(w, r, err)
			return
		}
		s.writeOperationError(w, r, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		s.writeOperationError(w, r, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		s.writeOperationError(w, r, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	if permissionErr := auth.Require(principal, auth.PermissionComputersDelete, scope,
		codedError{code: "permission_required", message: errPermissionRequired.Error()}); permissionErr != nil {
		s.writeOperationError(w, r, forbidden(permissionErr))
		return
	}
	dryRun := false
	for name, values := range r.URL.Query() {
		if name != "dry_run" || len(values) != 1 || (values[0] != "true" && values[0] != "false") {
			s.writeOperationError(w, r, badRequest(errors.New("unsupported query parameter")))
			return
		}
		dryRun = values[0] == "true"
	}
	result, err := computer.Delete(r.Context(), s.auditPolicy, s.tx, computer.Deletion{Principal: principal,
		Scope:          computerScope(principal.OrgID, projectID, environmentID),
		ComputerID:     computerID,
		IdempotencyKey: idempotencyKey,
		DryRun:         dryRun,
		CancelSessions: session.ComputerSessionCancellation(s.auditPolicy),
	})
	if err != nil {
		s.writeComputerError(w, r, err, computerDeleteOperation, "delete Computer failed")
		return
	}
	writeJSON(w, http.StatusAccepted, api.DeleteComputerReceipt{ComputerID: result.ComputerID.String(), SessionsCancelled: result.SessionsCancelled, CommandsCancelled: result.CommandsCancelled})
}

// apiComputerSnapshot is the public Computer resource of an owner snapshot.
func apiComputerSnapshot(snapshot computer.Snapshot) api.ComputerSnapshot {
	return api.ComputerSnapshot{ExecutionControl: apiComputerExecutionControl(snapshot.ExecutionControl), ExecutionWait: (*api.ComputerExecutionWait)(snapshot.ExecutionWait), DeleteAfterUnusedMs: snapshot.DeleteAfterUnusedMs, UnusedSince: snapshot.UnusedSince,
		Residency:      api.ComputerResidency(snapshot.Residency),
		Error:          snapshot.Error,
		ID:             snapshot.ID,
		Key:            snapshot.Key,
		DefinitionID:   snapshot.DefinitionKey,
		DeploymentID:   snapshot.DeploymentID,
		Status:         api.ComputerStatus(snapshot.Status),
		Secrets:        snapshot.Secrets,
		LastActivityAt: snapshot.LastActivityAt,
		CreatedAt:      snapshot.CreatedAt,
		UpdatedAt:      snapshot.UpdatedAt,
	}
}

func apiComputerListItem(item computer.ListItem) api.ComputerListItem {
	return api.ComputerListItem{ExecutionControl: apiComputerExecutionControl(item.ExecutionControl), ExecutionWait: (*api.ComputerExecutionWait)(item.ExecutionWait), DeleteAfterUnusedMs: item.DeleteAfterUnusedMs, UnusedSince: item.UnusedSince,
		Residency:      api.ComputerResidency(item.Residency),
		Error:          item.Error,
		ID:             item.ID,
		Key:            item.Key,
		DefinitionID:   item.DefinitionKey,
		DeploymentID:   item.DeploymentID,
		Status:         api.ComputerStatus(item.Status),
		LastActivityAt: item.LastActivityAt,
		CreatedAt:      item.CreatedAt,
		UpdatedAt:      item.UpdatedAt,
		SecretExposure: api.ComputerSecretExposure(item.SecretExposure),
	}
}

func (s *Server) updateComputerHTTP(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		s.writeOperationError(w, r, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	if permissionErr := auth.Require(principal, auth.PermissionComputersUpdate, scope,
		codedError{code: "permission_required", message: errPermissionRequired.Error()}); permissionErr != nil {
		s.writeOperationError(w, r, forbidden(permissionErr))
		return
	}
	id, err := ids.Parse(chi.URLParam(r, "computerID"))
	if err != nil {
		s.writeOperationError(w, r, badRequest(codedError{code: "invalid_computer_reference", message: "computer ID is invalid"}))
		return
	}
	var request api.UpdateComputerRequest
	if err = decodeRequestJSON(r, &request); err != nil {
		if isRequestBodyTooLarge(err) {
			s.writeOperationError(w, r, err)
			return
		}
		s.writeOperationError(w, r, badRequest(codedError{code: "invalid_computer_update", message: err.Error()}))
		return
	}
	snapshot, err := computer.UpdateRetention(r.Context(), s.auditPolicy, s.tx, principal, computerScope(principal.OrgID, projectID, environmentID), id, request.DeleteAfterUnused)
	if err != nil {
		s.writeComputerError(w, r, err, computerCreateOperation, "update Computer failed")
		return
	}
	writeJSON(w, http.StatusOK, apiComputerSnapshot(snapshot))
}

func apiComputerExecutionControl(c *computer.ExecutionControl) *api.ComputerExecutionControl {
	if c == nil {
		return nil
	}
	return &api.ComputerExecutionControl{ID: c.ID.String(), ComputerID: c.ComputerID.String(), Kind: c.Kind, Status: c.Status, ErrorCode: c.ErrorCode, AcceptedAt: c.AcceptedAt, SettledAt: c.SettledAt}
}
