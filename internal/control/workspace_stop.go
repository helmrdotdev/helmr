package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) stopWorkspace(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := uuid.Parse(strings.TrimSpace(chi.URLParam(r, "workspaceID")))
	if err != nil {
		writeError(w, badRequest(errors.New("workspace_id must be a UUID")))
		return
	}
	actor := actorFromContext(r.Context())
	var request api.WorkspaceStopRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace stop request JSON: %w", err)))
		return
	}
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actorHasAnyPermission(actor, scope,
		auth.PermissionWorkspaceLifecycleManage,
		auth.PermissionFilesWrite,
		auth.PermissionVersionsCapture,
		auth.PermissionVersionsRestore,
		auth.PermissionExecManage,
		auth.PermissionPtyManage,
	) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	if err := s.markStaleWorkspaceMountsLost(r.Context()); err != nil {
		s.log.Error("mark stale workspace mounts lost failed", "workspace_id", workspaceID.String(), "error", err)
		writeError(w, errors.New("reap stale workspace mounts"))
		return
	}
	response, err := s.requestWorkspaceStopForRequest(r.Context(), actor, projectID, environmentID, pgvalue.UUID(workspaceID))
	if err != nil {
		s.writeWorkspaceError(w, "request workspace stop", err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) requestWorkspaceStopForRequest(ctx context.Context, actor auth.Actor, projectID pgtype.UUID, environmentID pgtype.UUID, workspaceID pgtype.UUID) (api.WorkspaceStopResponse, error) {
	var response api.WorkspaceStopResponse
	err := s.inTx(ctx, func(work *txWork) error {
		row, err := work.q.RequestWorkspaceMountStop(ctx, db.RequestWorkspaceMountStopParams{
			OrgID:         pgvalue.UUID(actor.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			WorkspaceID:   workspaceID,
		})
		activeMount := true
		if isNoRows(err) {
			_, err = work.q.SetWorkspaceDesiredStopped(ctx, db.SetWorkspaceDesiredStoppedParams{
				OrgID:         pgvalue.UUID(actor.OrgID),
				ProjectID:     projectID,
				EnvironmentID: environmentID,
				ID:            workspaceID,
			})
			if err != nil {
				return err
			}
			activeMount = false
		} else if err != nil {
			return err
		}
		response = workspaceStopResponse(workspaceID, row, activeMount)
		return nil
	})
	if err != nil {
		return api.WorkspaceStopResponse{}, err
	}
	return response, nil
}

func workspaceStopResponse(workspaceID pgtype.UUID, row db.RequestWorkspaceMountStopRow, activeMount bool) api.WorkspaceStopResponse {
	if !activeMount {
		return api.WorkspaceStopResponse{
			WorkspaceID: pgvalue.MustUUIDValue(workspaceID).String(),
			State:       "no_active_mount",
		}
	}
	mount := workspaceMountResponse(workspaceMountFromStopRow(row))
	return api.WorkspaceStopResponse{
		WorkspaceID: pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		State:       string(row.State),
		Mount:       &mount,
	}
}

func workspaceMountFromStopRow(row db.RequestWorkspaceMountStopRow) db.WorkspaceMount {
	return db.WorkspaceMount(row)
}

func (s *Server) workerStopWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMountStopRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace mount stop request JSON: %w", err)))
		return
	}
	row, err := s.workerStopWorkspaceMountTransition(r.Context(), request.OrgID, request.WorkspaceMountID, request.RuntimeInstanceToken)
	if isNoRows(err) {
		writeError(w, conflict(errors.New("workspace mount is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("stop workspace mount"))
		return
	}
	writeJSON(w, http.StatusOK, workspaceMountResponse(row))
}

func stoppedWorkspaceMount(row db.StopWorkspaceMountRow) db.WorkspaceMount {
	return db.WorkspaceMount(row)
}
