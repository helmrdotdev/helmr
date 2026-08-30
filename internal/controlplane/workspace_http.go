package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"uuid"

	"github.com/go-chi/chi/v5"
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
	ID            string
}

func (s *Server) createWorkspaceHTTP(w http.ResponseWriter, r *http.Request) {
	declaredID := chi.URLParam(r, "sandboxID")
	if err := api.ValidateSandboxDeclaredID(declaredID); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_workspace_create", message: err.Error()}))
		return
	}
	var request api.CreateWorkspaceRequest
	if err := decodeJSON(r, &request); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, tooLarge(codedError{
				code:    "workspace_create_request_too_large",
				message: "workspace create request is too large",
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
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
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
	writeJSON(w, http.StatusCreated, result.Snapshot)
}

func (s *Server) getWorkspaceHTTP(w http.ResponseWriter, r *http.Request) {
	s.getWorkspaceByReferenceHTTP(w, r)
}

func (s *Server) deleteWorkspaceHTTP(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := ids.Parse(chi.URLParam(r, "workspaceID"))
	if err != nil {
		writeError(w, badRequest(codedError{
			code:    "invalid_workspace_reference",
			message: "workspace ID is invalid",
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
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
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

func (s *Server) getWorkspaceByReferenceHTTP(w http.ResponseWriter, r *http.Request) {
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
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
		ID: chi.URLParam(r, "workspaceID"),
	}
	if err := ids.Validate(reference.ID); err != nil {
		writeError(w, badRequest(codedError{
			code: "invalid_workspace_reference", message: "workspaceID must be a canonical UUIDv7",
		}))
		return
	}
	record, err := s.resolveWorkspaceReference(r.Context(), reference)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "workspace_not_found", message: "workspace was not found"}))
		return
	}
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "workspace_authority_unavailable",
			message:   "workspace authority is unavailable",
			retryable: true,
		}))
		return
	}
	snapshot, err := s.workspaceSnapshot(r.Context(), s.db, record)
	if err != nil {
		writeError(w, unavailable(codedError{
			code:      "workspace_authority_unavailable",
			message:   "workspace authority is unavailable",
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
	id, err := ids.Parse(reference.ID)
	if err != nil {
		return db.Workspace{}, errors.New("workspace ID is invalid")
	}
	return s.db.GetWorkspace(ctx, db.GetWorkspaceParams{
		OrgID:         pgvalue.UUID(reference.OrgID),
		ProjectID:     reference.ProjectID,
		EnvironmentID: reference.EnvironmentID,
		ID:            pgvalue.UUID(id),
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
	definition, err := q.GetWorkspaceDefinitionIdentity(ctx, db.GetWorkspaceDefinitionIdentityParams{
		EnvironmentID: record.EnvironmentID, DeploymentDefinitionID: record.DeploymentDefinitionID,
	})
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
			return api.WorkspaceSnapshot{}, fmt.Errorf("unsupported workspace secret placement %q", binding.PlacementKind)
		}
		secrets = append(secrets, item)
	}
	status, err := workspacePublicStatus(record.State)
	if err != nil {
		return api.WorkspaceSnapshot{}, err
	}
	var key *string
	if record.Key.Valid {
		value := record.Key.String
		key = &value
	}
	return api.WorkspaceSnapshot{
		ID:             pgvalue.UUIDString(record.ID),
		Key:            key,
		SandboxID:      definition.DeclaredID,
		DeploymentID:   pgvalue.UUIDString(definition.DeploymentID),
		Status:         status,
		Secrets:        secrets,
		LastActivityAt: pgvalue.Time(record.LastActivityAt),
		CreatedAt:      pgvalue.Time(record.CreatedAt),
		UpdatedAt:      pgvalue.Time(record.UpdatedAt),
	}, nil
}

func workspacePublicStatus(state string) (api.WorkspaceStatus, error) {
	switch state {
	case db.WorkspaceStateActive:
		return api.WorkspaceStatusAvailable, nil
	case db.WorkspaceStateRecoveryRequired:
		return api.WorkspaceStatusRecoveryRequired, nil
	case db.WorkspaceStateDeleting:
		return api.WorkspaceStatusDeleting, nil
	default:
		return "", fmt.Errorf("workspace state %q has no public projection", state)
	}
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
