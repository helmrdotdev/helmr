package control

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	store, scope, projectID, environmentID, ok := s.loadCurrentDeploymentReadScope(w, r)
	if !ok {
		return
	}
	actor := actorFromContext(r.Context())
	if !actor.HasPermission(auth.PermissionRunsRead, scope) && !actor.HasPermission(auth.PermissionTasksDeploy, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	rows, err := store.ListCurrentDeploymentTasks(r.Context(), db.ListCurrentDeploymentTasksParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
	})
	if err != nil {
		s.log.Error("list tasks failed", "error", err)
		writeError(w, errors.New("list tasks"))
		return
	}
	tasks, err := deploymentTaskResponses(r.Context(), store, rows)
	if err != nil {
		s.log.Error("get task artifacts failed", "error", err)
		writeError(w, errors.New("list tasks"))
		return
	}
	writeJSON(w, http.StatusOK, api.ListTasksResponse{Tasks: tasks})
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	store, scope, projectID, environmentID, ok := s.loadCurrentDeploymentReadScope(w, r)
	if !ok {
		return
	}
	actor := actorFromContext(r.Context())
	if !actor.HasPermission(auth.PermissionRunsRead, scope) && !actor.HasPermission(auth.PermissionTasksDeploy, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	taskID := strings.TrimSpace(chi.URLParam(r, "taskID"))
	if err := api.ValidateTaskID(taskID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	row, err := store.GetCurrentDeploymentTask(r.Context(), db.GetCurrentDeploymentTaskParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		TaskID:        taskID,
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("task not found")))
		return
	}
	if err != nil {
		s.log.Error("get task failed", "task_id", taskID, "error", err)
		writeError(w, errors.New("get task"))
		return
	}
	writeJSON(w, http.StatusOK, currentDeploymentTaskResponse(row))
}

func deploymentTaskResponses(ctx context.Context, store artifactLister, tasks []db.DeploymentTask) ([]api.DeploymentTaskResponse, error) {
	if len(tasks) == 0 {
		return []api.DeploymentTaskResponse{}, nil
	}
	artifactIDs := make([]pgtype.UUID, 0, len(tasks))
	for _, task := range tasks {
		artifactIDs = append(artifactIDs, task.BundleArtifactID)
	}
	first := tasks[0]
	artifacts, err := scopedArtifactsByID(ctx, store, first.OrgID, first.ProjectID, first.EnvironmentID, artifactIDs)
	if err != nil {
		return nil, err
	}
	responses := make([]api.DeploymentTaskResponse, 0, len(tasks))
	for _, task := range tasks {
		artifact, err := requiredArtifact(artifacts, task.BundleArtifactID)
		if err != nil {
			return nil, err
		}
		responses = append(responses, deploymentTaskResponse(task, artifact))
	}
	return responses, nil
}

func deploymentTaskResponse(task db.DeploymentTask, artifact db.Artifact) api.DeploymentTaskResponse {
	return api.DeploymentTaskResponse{
		ID:                pgvalue.MustUUIDValue(task.ID).String(),
		TaskID:            task.TaskID,
		FilePath:          task.FilePath,
		ExportName:        task.ExportName,
		HandlerEntrypoint: task.HandlerEntrypoint,
		BundleDigest:      artifact.Digest,
		QueueName:         task.QueueName,
		ConcurrencyLimit:  pgvalue.Int4Response(task.QueueConcurrencyLimit),
		TTL:               task.Ttl,
		CreatedAt:         pgvalue.Time(task.CreatedAt),
	}
}

func currentDeploymentTaskResponse(task db.GetCurrentDeploymentTaskRow) api.DeploymentTaskResponse {
	return api.DeploymentTaskResponse{
		ID:                  pgvalue.MustUUIDValue(task.ID).String(),
		TaskID:              task.TaskID,
		FilePath:            task.FilePath,
		ExportName:          task.ExportName,
		HandlerEntrypoint:   task.HandlerEntrypoint,
		BundleDigest:        task.BundleDigest,
		BundleFormatVersion: task.BundleFormatVersion,
		QueueName:           task.QueueName,
		ConcurrencyLimit:    pgvalue.Int4Response(task.QueueConcurrencyLimit),
		TTL:                 task.Ttl,
		CreatedAt:           pgvalue.Time(task.CreatedAt),
	}
}
