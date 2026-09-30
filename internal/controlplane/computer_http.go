package controlplane

import (
	"net/http"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/definition"
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
	declaredID := chi.URLParam(r, "sandboxID")
	if err := definition.ValidateSandboxDeclaredID(declaredID); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_create", message: err.Error()}))
		return
	}
	var request api.CreateComputerRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		if isRequestBodyTooLarge(err) {
			writeError(w, err)
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_computer_create", message: err.Error()}))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_create", message: err.Error()}))
		return
	}
	if !principal.HasPermission(auth.PermissionComputersCreate, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	result, err := s.computers.Create(r.Context(), s.tx, computer.Request{
		Scope:          computerScope(principal.OrgID, projectID, environmentID),
		DeclaredID:     declaredID,
		Key:            request.Key,
		Secrets:        request.Secrets,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.writeComputerError(w, err, computerCreateOperation, "create Computer failed")
		return
	}
	writeJSON(w, http.StatusCreated, apiComputerSnapshot(result.Snapshot))
}

func (s *Server) getComputerHTTP(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	if !principal.HasPermission(auth.PermissionComputersRead, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	computerID, err := ids.Parse(chi.URLParam(r, "computerID"))
	if err != nil {
		writeError(w, badRequest(codedError{
			code: "invalid_computer_reference", message: "computerID must be a canonical UUIDv7",
		}))
		return
	}
	snapshot, err := computer.Read(r.Context(), s.db, computerScope(principal.OrgID, projectID, environmentID), computerID)
	if err != nil {
		s.writeComputerError(w, err, computerReadOperation, "read Computer failed")
		return
	}
	writeJSON(w, http.StatusOK, apiComputerSnapshot(snapshot))
}

func (s *Server) deleteComputerHTTP(w http.ResponseWriter, r *http.Request) {
	computerID, err := ids.Parse(chi.URLParam(r, "computerID"))
	if err != nil {
		writeError(w, badRequest(codedError{
			code:    "invalid_computer_reference",
			message: "computer ID is invalid",
		}))
		return
	}
	var request api.DeleteComputerRequest
	if err := decodeOptionalRequestJSON(r, &request); err != nil {
		if isRequestBodyTooLarge(err) {
			writeError(w, err)
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_computer_reference", message: err.Error()}))
		return
	}
	if !principal.HasPermission(auth.PermissionComputersDelete, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	result, err := computer.Delete(r.Context(), s.tx, computer.Deletion{
		Scope:          computerScope(principal.OrgID, projectID, environmentID),
		ComputerID:     computerID,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.writeComputerError(w, err, computerDeleteOperation, "delete Computer failed")
		return
	}
	writeJSON(w, http.StatusAccepted, api.DeleteComputerReceipt{ComputerID: result.ComputerID.String()})
}

// apiComputerSnapshot is the public Computer resource of an owner snapshot.
func apiComputerSnapshot(snapshot computer.Snapshot) api.ComputerSnapshot {
	return api.ComputerSnapshot{
		Residency:      api.ComputerResidency(snapshot.Residency),
		Error:          snapshot.Error,
		ID:             snapshot.ID,
		Key:            snapshot.Key,
		SandboxID:      snapshot.SandboxID,
		DeploymentID:   snapshot.DeploymentID,
		Status:         api.ComputerStatus(snapshot.Status),
		Secrets:        snapshot.Secrets,
		LastActivityAt: snapshot.LastActivityAt,
		CreatedAt:      snapshot.CreatedAt,
		UpdatedAt:      snapshot.UpdatedAt,
	}
}

func apiComputerListItem(item computer.ListItem) api.ComputerListItem {
	return api.ComputerListItem{
		Residency:      api.ComputerResidency(item.Residency),
		Error:          item.Error,
		ID:             item.ID,
		Key:            item.Key,
		SandboxID:      item.SandboxID,
		DeploymentID:   item.DeploymentID,
		Status:         api.ComputerStatus(item.Status),
		LastActivityAt: item.LastActivityAt,
		CreatedAt:      item.CreatedAt,
		UpdatedAt:      item.UpdatedAt,
	}
}

func apiComputerMembers(page computer.MembersPage) api.ListComputerMembersResponse {
	response := api.ListComputerMembersResponse{Members: make([]api.ComputerMember, 0, len(page.Members)), NextCursor: page.NextCursor}
	for _, member := range page.Members {
		response.Members = append(response.Members, api.ComputerMember{
			Kind: member.Kind, ID: member.ID, RunID: member.RunID, State: member.State, CreatedAt: member.CreatedAt,
		})
	}
	return response
}
