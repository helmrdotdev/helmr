package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const workspaceCreateBodyLimit = int64(256 << 10)

type workspaceReference struct {
	OrgID         uuid.UUID
	ProjectID     pgtype.UUID
	EnvironmentID pgtype.UUID
	DeclaredID    string
	ID            string
	Key           *string
}

func (s *Server) createWorkspaceHTTP(w http.ResponseWriter, r *http.Request) {
	declaredID := chi.URLParam(r, "workspaceDeclaredID")
	if err := api.ValidateWorkspaceDeclaredID(declaredID); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_workspace_create", message: err.Error()}))
		return
	}
	var request api.CreateWorkspaceRequest
	if err := decodeJSON(r, &request); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, tooLarge(codedError{
				code:    "workspace_create_request_too_large",
				message: "Workspace create request is too large",
			}))
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_workspace_create", message: err.Error()}))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal, "", "")
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_workspace_create", message: err.Error()}))
		return
	}
	if !principal.HasPermission(auth.PermissionWorkspacesCreate, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	result, err := s.createWorkspace(r.Context(), workspaceCreateRequest{
		OrgID:          principal.OrgID,
		ProjectID:      pgvalue.MustUUIDValue(projectID),
		EnvironmentID:  pgvalue.MustUUIDValue(environmentID),
		Declaration:    workspaceDeclarationSelector{Kind: workspaceDeclarationPromoted},
		DeclaredID:     declaredID,
		Key:            request.Key,
		Secrets:        request.Secrets,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.writeWorkspaceCreateError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, api.CreateWorkspaceResponse{WorkspaceID: result.WorkspaceID.String()})
}

func (s *Server) getWorkspaceHTTP(w http.ResponseWriter, r *http.Request) {
	s.getWorkspaceByReferenceHTTP(w, r, false)
}

func (s *Server) getWorkspaceByKeyHTTP(w http.ResponseWriter, r *http.Request) {
	s.getWorkspaceByReferenceHTTP(w, r, true)
}

func (s *Server) deleteWorkspaceHTTP(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := ids.Parse(chi.URLParam(r, "workspaceID"))
	if err != nil {
		writeError(w, badRequest(codedError{
			code:    "invalid_workspace_reference",
			message: "Workspace ID is invalid",
		}))
		return
	}
	var request api.DeleteWorkspaceRequest
	if err := decodeOptionalJSON(r.Body, &request); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_workspace_reference", message: err.Error()}))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal, "", "")
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_workspace_reference", message: err.Error()}))
		return
	}
	if !principal.HasPermission(auth.PermissionWorkspacesDelete, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	result, err := s.deleteWorkspace(r.Context(), workspaceDeleteRequest{
		OrgID:          principal.OrgID,
		ProjectID:      pgvalue.MustUUIDValue(projectID),
		EnvironmentID:  pgvalue.MustUUIDValue(environmentID),
		WorkspaceID:    workspaceID,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		var conflictError idempotency.ConflictError
		switch {
		case errors.Is(err, errWorkspaceNotFound):
			writeError(w, notFound(codedError{code: "workspace_not_found", message: err.Error()}))
		case errors.Is(err, errWorkspaceBusy):
			writeError(w, conflict(codedError{
				code:      "workspace_busy",
				message:   err.Error(),
				retryable: true,
			}))
		case errors.As(err, &conflictError):
			writeError(w, conflict(codedError{code: "idempotency_conflict", message: err.Error()}))
		default:
			s.log.Error("delete Workspace failed", "error", err)
			writeError(w, unavailable(codedError{
				code:      "workspace_authority_unavailable",
				message:   errWorkspaceAuthorityUnavailable.Error(),
				retryable: true,
			}))
		}
		return
	}
	writeJSON(w, http.StatusAccepted, api.DeleteWorkspaceReceipt{WorkspaceID: result.WorkspaceID.String()})
}

func (s *Server) getWorkspaceByReferenceHTTP(w http.ResponseWriter, r *http.Request, byKey bool) {
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal, "", "")
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_workspace_reference", message: err.Error()}))
		return
	}
	if !principal.HasPermission(auth.PermissionWorkspacesRead, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	reference := workspaceReference{
		OrgID: principal.OrgID, ProjectID: projectID, EnvironmentID: environmentID,
	}
	if byKey {
		key := r.URL.Query().Get("key")
		reference.DeclaredID = chi.URLParam(r, "workspaceDeclaredID")
		reference.Key = &key
	} else {
		reference.ID = chi.URLParam(r, "workspaceID")
		if err := ids.Validate(reference.ID); err != nil {
			writeError(w, badRequest(codedError{
				code: "invalid_workspace_reference", message: "workspaceID must be a canonical UUIDv7",
			}))
			return
		}
	}
	record, err := s.resolveWorkspaceReference(r.Context(), reference)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "workspace_not_found", message: "Workspace was not found"}))
		return
	}
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "workspace_authority_unavailable",
			message:   "Workspace authority is unavailable",
			retryable: true,
		}))
		return
	}
	snapshot, err := s.workspaceSnapshot(r.Context(), s.db, record)
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "workspace_authority_unavailable",
			message:   "Workspace authority is unavailable",
			retryable: true,
		}))
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) resolveWorkspaceReference(
	ctx context.Context,
	reference workspaceReference,
) (db.Workspace, error) {
	hasID := reference.ID != ""
	hasKey := reference.Key != nil
	if hasID == hasKey {
		return db.Workspace{}, errors.New("Workspace reference requires exactly one of ID or key")
	}
	if hasID {
		id, err := ids.Parse(reference.ID)
		if err != nil {
			return db.Workspace{}, errors.New("Workspace ID is invalid")
		}
		return s.db.GetWorkspace(ctx, db.GetWorkspaceParams{
			OrgID:         pgvalue.UUID(reference.OrgID),
			ProjectID:     reference.ProjectID,
			EnvironmentID: reference.EnvironmentID,
			ID:            pgvalue.UUID(id),
		})
	}
	if err := api.ValidateWorkspaceDeclaredID(reference.DeclaredID); err != nil {
		return db.Workspace{}, err
	}
	if err := validateWorkspaceKey(reference.Key); err != nil {
		return db.Workspace{}, err
	}
	return s.db.GetWorkspaceByDeclaredIDAndKey(ctx, db.GetWorkspaceByDeclaredIDAndKeyParams{
		OrgID:               pgvalue.UUID(reference.OrgID),
		ProjectID:           reference.ProjectID,
		EnvironmentID:       reference.EnvironmentID,
		WorkspaceDeclaredID: pgvalue.Text(reference.DeclaredID),
		Key:                 pgtype.Text{String: *reference.Key, Valid: true},
	})
}

func (s *Server) workspaceSnapshot(
	ctx context.Context,
	q db.Querier,
	record db.Workspace,
) (api.WorkspaceSnapshot, error) {
	bindings, err := q.ListWorkspaceSecrets(ctx, record.ID)
	if err != nil {
		return api.WorkspaceSnapshot{}, err
	}
	secrets := make([]api.WorkspaceSecret, 0, len(bindings))
	for _, binding := range bindings {
		item := api.WorkspaceSecret{Name: binding.SecretName}
		switch binding.PlacementKind {
		case "env":
			item.Env = binding.PlacementTarget
		case "file":
			item.File = binding.PlacementTarget
		default:
			return api.WorkspaceSnapshot{}, fmt.Errorf("unsupported Workspace Secret placement %q", binding.PlacementKind)
		}
		secrets = append(secrets, item)
	}
	var status api.WorkspaceStatus
	switch record.State {
	case db.WorkspaceStateActive:
		status = api.WorkspaceStatusAvailable
	case db.WorkspaceStateRecoveryRequired:
		status = api.WorkspaceStatusRecoveryRequired
	case db.WorkspaceStateDeleting:
		status = api.WorkspaceStatusDeleting
	default:
		return api.WorkspaceSnapshot{}, fmt.Errorf("Workspace state %q has no public projection", record.State)
	}
	var key *string
	if record.Key.Valid {
		value := record.Key.String
		key = &value
	}
	return api.WorkspaceSnapshot{
		ID:             pgvalue.UUIDString(record.ID),
		Key:            key,
		DeclaredID:     record.WorkspaceDeclaredID.String,
		Status:         status,
		Secrets:        secrets,
		LastActivityAt: pgvalue.Time(record.LastActivityAt),
		CreatedAt:      pgvalue.Time(record.CreatedAt),
		UpdatedAt:      pgvalue.Time(record.UpdatedAt),
	}, nil
}

func (s *Server) writeWorkspaceCreateError(w http.ResponseWriter, err error) {
	var conflictError idempotency.ConflictError
	var keyConflict WorkspaceKeyConflictError
	switch {
	case errors.Is(err, errWorkspaceCreateInvalid):
		writeError(w, badRequest(codedError{code: "invalid_workspace_create", message: err.Error()}))
	case errors.Is(err, errWorkspaceNotDeployed):
		writeError(w, notFound(codedError{code: "workspace_not_deployed", message: err.Error()}))
	case errors.Is(err, errWorkspaceSecretUnavailable):
		writeError(w, conflict(codedError{code: "secret_unavailable", message: err.Error()}))
	case errors.As(err, &conflictError):
		writeError(w, conflict(codedError{code: "idempotency_conflict", message: err.Error()}))
	case errors.As(err, &keyConflict):
		writeError(w, conflict(codedError{code: "workspace_key_conflict", message: err.Error()}))
	default:
		s.log.Error("create Workspace failed", "error", err)
		writeError(w, unavailable(codedError{
			code:      "workspace_authority_unavailable",
			message:   errWorkspaceAuthorityUnavailable.Error(),
			retryable: true,
		}))
	}
}
