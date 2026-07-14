package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

const workspaceMountReservationDuration = 5 * time.Minute

func (s *Server) requestWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := uuid.Parse(strings.TrimSpace(chi.URLParam(r, "workspaceID")))
	if err != nil {
		writeError(w, badRequest(errors.New("workspace_id must be a UUID")))
		return
	}
	actor := actorFromContext(r.Context())
	var request api.WorkspaceMaterializeRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace materialize request JSON: %w", err)))
		return
	}
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actorHasAnyPermission(actor, scope,
		auth.PermissionWorkspaceLifecycleManage,
		auth.PermissionFilesRead,
		auth.PermissionFilesWrite,
		auth.PermissionVersionsRead,
		auth.PermissionVersionsCapture,
		auth.PermissionVersionsRestore,
		auth.PermissionExecRead,
		auth.PermissionExecCreate,
		auth.PermissionExecManage,
		auth.PermissionPtyRead,
		auth.PermissionPtyCreate,
		auth.PermissionPtyManage,
	) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	row, err := s.db.EnsureWorkspaceMountRequested(r.Context(), db.EnsureWorkspaceMountRequestedParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), OrgID: pgvalue.UUID(actor.OrgID),
		WorkspaceID: pgvalue.UUID(workspaceID), Priority: 0, Request: []byte(`{"source":"api"}`),
	})
	if isNoRows(err) {
		if s.log != nil {
			s.log.Info("workspace mount request blocked",
				"source", "api",
				"org_id", actor.OrgID.String(),
				"project_id", pgvalue.UUIDString(projectID),
				"environment_id", pgvalue.UUIDString(environmentID),
				"workspace_id", workspaceID.String(),
				"outcome", "blocked",
			)
		}
		writeError(w, s.workspaceMountPrerequisiteError(r.Context(), pgvalue.UUID(actor.OrgID), projectID, environmentID, pgvalue.UUID(workspaceID)))
		return
	}
	if err != nil {
		s.log.Error("ensure workspace mount failed", "workspace_id", workspaceID.String(), "error", err)
		writeError(w, errors.New("ensure workspace mount"))
		return
	}
	if s.log != nil {
		s.log.Info("workspace mount request ensured",
			"source", "api",
			"org_id", actor.OrgID.String(),
			"project_id", pgvalue.UUIDString(projectID),
			"environment_id", pgvalue.UUIDString(environmentID),
			"workspace_id", workspaceID.String(),
			"workspace_mount_id", pgvalue.UUIDString(row.ID),
			"state", row.State,
			"priority", row.Priority,
			"inserted", row.Inserted,
			"decision", row.Decision,
		)
	}
	status := http.StatusOK
	if row.Inserted {
		status = http.StatusCreated
	}
	writeJSON(w, status, workspaceMountResponse(workspaceMountFromEnsureRow(row)))
}

func actorHasAnyPermission(actor auth.Actor, scope auth.Scope, permissions ...auth.Permission) bool {
	for _, permission := range permissions {
		if actor.HasPermission(permission, scope) {
			return true
		}
	}
	return false
}

func workspaceReadPermissions() []auth.Permission {
	return []auth.Permission{
		auth.PermissionWorkspaceLifecycleManage,
		auth.PermissionFilesRead,
		auth.PermissionFilesWrite,
		auth.PermissionVersionsRead,
		auth.PermissionVersionsCapture,
		auth.PermissionVersionsRestore,
		auth.PermissionVersionsDiff,
		auth.PermissionExecCreate,
		auth.PermissionExecRead,
		auth.PermissionExecManage,
		auth.PermissionPtyCreate,
		auth.PermissionPtyRead,
		auth.PermissionPtyManage,
	}
}

func (s *Server) workspaceMountPrerequisiteError(ctx context.Context, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, workspaceID pgtype.UUID) error {
	return workspaceMountPrerequisiteErrorWithStore(ctx, s.db, orgID, projectID, environmentID, workspaceID)
}

func workspaceMountPrerequisiteErrorWithStore(ctx context.Context, store db.Querier, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, workspaceID pgtype.UUID) error {
	row, err := store.GetWorkspaceMountPrerequisites(ctx, db.GetWorkspaceMountPrerequisitesParams{
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		WorkspaceID:   workspaceID,
	})
	if isNoRows(err) {
		return notFound(errors.New("workspace not found"))
	}
	if err != nil {
		return errors.New("check workspace mount prerequisites")
	}
	if row.ActiveMountState.Valid {
		switch row.ActiveMountState.WorkspaceMountState {
		case db.WorkspaceMountStateMounting, db.WorkspaceMountStateMounted:
		default:
			return conflict(codedError{code: "workspace_mount_not_runnable", message: "workspace has an active mount that is not runnable"})
		}
	}
	if !row.CurrentVersionID.Valid || !row.CurrentWorkspaceVersionID.Valid {
		return conflict(codedError{code: "workspace_version_missing", message: "workspace current version is missing"})
	}
	if row.CurrentWorkspaceVersionState.WorkspaceVersionState != db.WorkspaceVersionStateReady {
		return conflict(codedError{code: "workspace_version_not_ready", message: "workspace current version is not ready"})
	}
	if !row.CurrentWorkspaceArtifactID.Valid || !row.WorkspaceArtifactID.Valid {
		return conflict(codedError{code: "workspace_version_artifact_missing", message: "workspace current version artifact is missing"})
	}
	if !row.WorkspaceArtifactMediaType.Valid || row.WorkspaceArtifactMediaType.String != workspace.ArtifactMediaType {
		return conflict(codedError{code: "workspace_version_artifact_corrupt", message: "workspace current version artifact media type is invalid"})
	}
	if !row.SandboxImageArtifactID.Valid || !row.ImageArtifactID.Valid {
		return conflict(codedError{code: "deployment_sandbox_artifact_missing", message: "deployment sandbox image artifact is missing"})
	}
	if !row.ImageArtifactMediaType.Valid || row.ImageArtifactMediaType.String != api.SandboxImageArtifactMediaType {
		return conflict(codedError{code: "deployment_sandbox_artifact_corrupt", message: "deployment sandbox image artifact media type is invalid"})
	}
	return conflict(codedError{code: "workspace_mount_prerequisite_failed", message: "workspace mount prerequisites are not satisfied"})
}

func workspaceMountResponse(row db.WorkspaceMount) api.WorkspaceMountResponse {
	response := api.WorkspaceMountResponse{
		ID:                  pgvalue.MustUUIDValue(row.ID).String(),
		ProjectID:           pgvalue.MustUUIDValue(row.ProjectID).String(),
		EnvironmentID:       pgvalue.MustUUIDValue(row.EnvironmentID).String(),
		WorkspaceID:         pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		DeploymentSandboxID: pgvalue.MustUUIDValue(row.DeploymentSandboxID).String(),
		State:               string(row.State),
		ClaimAttempt:        row.ClaimAttempt,
		FencingGeneration:   row.FencingGeneration,
		DirtyGeneration:     row.DirtyGeneration,
		CreatedAt:           row.CreatedAt.Time,
		UpdatedAt:           row.UpdatedAt.Time,
	}
	if row.BaseVersionID.Valid {
		response.BaseVersionID = pgvalue.MustUUIDValue(row.BaseVersionID).String()
	}
	return response
}
