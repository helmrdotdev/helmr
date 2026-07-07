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
	"github.com/helmrdotdev/helmr/internal/sha256sum"
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
	fingerprint, err := workspaceStopFingerprint()
	if err != nil {
		writeError(w, errors.New("fingerprint workspace stop request"))
		return
	}
	if err := s.markStaleWorkspaceMountsLost(r.Context()); err != nil {
		s.log.Error("mark stale workspace mounts lost failed", "workspace_id", workspaceID.String(), "error", err)
		writeError(w, errors.New("reap stale workspace mounts"))
		return
	}
	response, err := s.requestWorkspaceStopForRequest(r.Context(), actor, projectID, environmentID, pgvalue.UUID(workspaceID), request, fingerprint)
	if err != nil {
		s.writeWorkspaceError(w, "request workspace stop", err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) requestWorkspaceStopForRequest(ctx context.Context, actor auth.Actor, projectID pgtype.UUID, environmentID pgtype.UUID, workspaceID pgtype.UUID, request api.WorkspaceStopRequest, fingerprint string) (api.WorkspaceStopResponse, error) {
	idempotencyKey := strings.TrimSpace(request.IdempotencyKey)
	var response api.WorkspaceStopResponse
	err := s.inTx(ctx, func(work *txWork) error {
		createdIdempotency := false
		if idempotencyKey != "" {
			idempotencyTTL, err := workspaceIdempotencyTTL(request.IdempotencyKeyTTL)
			if err != nil {
				return err
			}
			idempotency, err := ensureWorkspaceOperationIdempotency(ctx, work.q, db.EnsureWorkspaceOperationIdempotencyParams{
				ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:              pgvalue.UUID(actor.OrgID),
				ProjectID:          projectID,
				EnvironmentID:      environmentID,
				WorkspaceID:        workspaceID,
				OperationKind:      workspaceStopOperationKind,
				IdempotencyKey:     idempotencyKey,
				RequestFingerprint: fingerprint,
				ResultResourceID:   pgtype.UUID{},
				ResponseBody:       []byte(`{}`),
				ExpiresAt:          pgvalue.Timestamptz(time.Now().Add(idempotencyTTL)),
			})
			if err != nil {
				return err
			}
			var replayed bool
			response, replayed, err = workspaceStopIdempotencyResponse(idempotency, fingerprint)
			if err != nil {
				return err
			}
			if replayed {
				return nil
			}
			createdIdempotency = true
		}
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
		responseBody, err := json.Marshal(response)
		if err != nil {
			return err
		}
		if createdIdempotency {
			_, err = work.q.CompleteWorkspaceScopedOperationIdempotency(ctx, db.CompleteWorkspaceScopedOperationIdempotencyParams{
				OrgID:              pgvalue.UUID(actor.OrgID),
				ProjectID:          projectID,
				EnvironmentID:      environmentID,
				OperationKind:      workspaceStopOperationKind,
				WorkspaceID:        workspaceID,
				IdempotencyKey:     idempotencyKey,
				RequestFingerprint: fingerprint,
				ResultResourceID:   workspaceID,
				ResponseBody:       responseBody,
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return api.WorkspaceStopResponse{}, err
	}
	return response, nil
}

func workspaceStopIdempotencyResponse(idempotency db.EnsureWorkspaceOperationIdempotencyRow, fingerprint string) (api.WorkspaceStopResponse, bool, error) {
	if idempotency.Inserted {
		return api.WorkspaceStopResponse{}, false, nil
	}
	if idempotency.RequestFingerprint != fingerprint {
		return api.WorkspaceStopResponse{}, false, errWorkspaceOperationIdempotencyUsed
	}
	if !idempotency.ResultResourceID.Valid {
		return api.WorkspaceStopResponse{}, false, errWorkspaceOperationPending
	}
	var response api.WorkspaceStopResponse
	if err := json.Unmarshal(idempotency.ResponseBody, &response); err != nil {
		return api.WorkspaceStopResponse{}, false, fmt.Errorf("decode workspace stop idempotency response: %w", err)
	}
	return response, true, nil
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

func workspaceStopFingerprint() (string, error) {
	payload, err := json.Marshal(struct {
		Operation string `json:"operation"`
	}{
		Operation: string(workspaceStopOperationKind),
	})
	if err != nil {
		return "", err
	}
	return sha256sum.HexBytes(payload), nil
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
