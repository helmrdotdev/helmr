package control

import (
	"context"
	"encoding/json"
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

const (
	workspaceMountReservationDuration             = 5 * time.Minute
	preparedRuntimeReservationDuration            = 2 * time.Minute
	capacityPressureCheckpointCommandPreemptLimit = int32(1)
	capacityPressureIdleStopPreemptLimit          = int32(1)
)

func (s *Server) markStaleWorkspaceMountsLost(ctx context.Context) error {
	_, err := s.db.MarkStaleWorkspaceMountsLost(ctx, pgtype.Timestamptz{
		Time:  time.Now().Add(-workspaceMountReservationDuration),
		Valid: true,
	})
	return err
}

func (s *Server) releaseExpiredPreparedRuntimeReservations(ctx context.Context) error {
	_, err := s.db.ReleaseExpiredPreparedRuntimeReservations(ctx, pgtype.Timestamptz{
		Time:  time.Now(),
		Valid: true,
	})
	return err
}

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
	routeCellID, err := s.requireRoutableEnvironmentCell(r.Context(), s.db, actor.OrgID, projectID, environmentID)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.markStaleWorkspaceMountsLost(r.Context()); err != nil {
		s.log.Error("mark stale workspace mounts lost failed", "workspace_id", workspaceID.String(), "error", err)
		writeError(w, errors.New("reap stale workspace mounts"))
		return
	}
	row, err := s.db.EnsureWorkspaceMountRequested(r.Context(), db.EnsureWorkspaceMountRequestedParams{
		ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:           pgvalue.UUID(actor.OrgID),
		CellID:          routeCellID,
		ProjectID:       projectID,
		EnvironmentID:   environmentID,
		WorkspaceID:     pgvalue.UUID(workspaceID),
		RequestPriority: 0,
		Request:         []byte(`{"source":"api"}`),
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
	writeJSON(w, status, ensuredWorkspaceMountResponse(row))
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

type queuedRunFailer interface {
	ListQueuedRunsForWorkspaceMount(context.Context, db.ListQueuedRunsForWorkspaceMountParams) ([]pgtype.UUID, error)
	FailQueuedRun(context.Context, db.FailQueuedRunParams) error
}

func failQueuedRunsForWorkspaceMountFailure(ctx context.Context, store queuedRunFailer, row db.FailWorkspaceMountRow, errorJSON json.RawMessage) error {
	runIDs, err := store.ListQueuedRunsForWorkspaceMount(ctx, db.ListQueuedRunsForWorkspaceMountParams{
		OrgID:            row.OrgID,
		WorkspaceID:      row.WorkspaceID,
		WorkspaceMountID: row.ID,
	})
	if err != nil {
		return err
	}
	message := workspaceMountFailureRunMessage(errorJSON)
	reason, err := workspaceMountFailureRunReason(row, message, errorJSON)
	if err != nil {
		return err
	}
	for _, runID := range runIDs {
		if err := store.FailQueuedRun(ctx, db.FailQueuedRunParams{
			OrgID:        row.OrgID,
			RunID:        runID,
			ErrorMessage: message,
			Reason:       reason,
		}); err != nil {
			return err
		}
	}
	return nil
}

func workspaceMountFailureRunMessage(errorJSON json.RawMessage) string {
	var body struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if len(errorJSON) > 0 && json.Unmarshal(errorJSON, &body) == nil {
		if message := strings.TrimSpace(body.Message); message != "" {
			return message
		}
		if code := strings.TrimSpace(body.Code); code != "" {
			return code
		}
	}
	return "workspace mount failed"
}

func workspaceMountFailureRunReason(row db.FailWorkspaceMountRow, message string, errorJSON json.RawMessage) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"origin":             "workspace_mount",
		"message":            message,
		"workspace_id":       pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		"workspace_mount_id": pgvalue.MustUUIDValue(row.ID).String(),
		"error":              json.RawMessage(errorJSON),
	})
	if err != nil {
		return nil, err
	}
	return body, nil
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
	if row.LastHeartbeatAt.Valid {
		response.LastHeartbeatAt = &row.LastHeartbeatAt.Time
	}
	return response
}

func ensuredWorkspaceMountResponse(row db.EnsureWorkspaceMountRequestedRow) api.WorkspaceMountResponse {
	return workspaceMountResponse(db.WorkspaceMount{
		ID:                  row.ID,
		ProjectID:           row.ProjectID,
		EnvironmentID:       row.EnvironmentID,
		WorkspaceID:         row.WorkspaceID,
		DeploymentSandboxID: row.DeploymentSandboxID,
		BaseVersionID:       row.BaseVersionID,
		RuntimeInstanceID:   row.RuntimeInstanceID,
		State:               row.State,
		ClaimAttempt:        row.ClaimAttempt,
		FencingGeneration:   row.FencingGeneration,
		DirtyGeneration:     row.DirtyGeneration,
		LastHeartbeatAt:     row.LastHeartbeatAt,
		CreatedAt:           row.CreatedAt,
		UpdatedAt:           row.UpdatedAt,
	})
}
