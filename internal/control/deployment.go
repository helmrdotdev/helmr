package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type deploymentStore interface {
	AllocateDeploymentVersion(context.Context, db.AllocateDeploymentVersionParams) (string, error)
	AppendDeploymentEvent(context.Context, db.AppendDeploymentEventParams) (db.AppendDeploymentEventRow, error)
	CreateArtifact(context.Context, db.CreateArtifactParams) (db.Artifact, error)
	CreateDeployment(context.Context, db.CreateDeploymentParams) (db.Deployment, error)
	GetDefaultWorkerGroup(context.Context) (db.WorkerGroup, error)
	GetReusableDeploymentByContentHash(context.Context, db.GetReusableDeploymentByContentHashParams) (db.Deployment, error)
	ListArtifactsByIDs(context.Context, db.ListArtifactsByIDsParams) ([]db.Artifact, error)
	LockDeploymentReusableBuildKey(context.Context, db.LockDeploymentReusableBuildKeyParams) error
	UpsertCasObject(context.Context, db.UpsertCasObjectParams) (db.CasObject, error)
}

type deploymentEventAppender interface {
	AppendDeploymentEvent(context.Context, db.AppendDeploymentEventParams) (db.AppendDeploymentEventRow, error)
}

type deploymentVersionMetadata struct {
	APIVersion            string
	SDKVersion            string
	CLIVersion            string
	BundleFormatVersion   int32
	WorkerProtocolVersion string
}

type currentDeploymentStore interface {
	GetCurrentDeployment(context.Context, db.GetCurrentDeploymentParams) (db.Deployment, error)
	ListArtifactsByIDs(context.Context, db.ListArtifactsByIDsParams) ([]db.Artifact, error)
	ListDeploymentTasks(context.Context, db.ListDeploymentTasksParams) ([]db.DeploymentTask, error)
}

type deploymentStatusStore interface {
	GetDeployment(context.Context, db.GetDeploymentParams) (db.Deployment, error)
	GetDeploymentForOrg(context.Context, db.GetDeploymentForOrgParams) (db.Deployment, error)
	GetDeploymentByVersion(context.Context, db.GetDeploymentByVersionParams) (db.Deployment, error)
	ListArtifactsByIDs(context.Context, db.ListArtifactsByIDsParams) ([]db.Artifact, error)
	ListScopedDeployments(context.Context, db.ListScopedDeploymentsParams) ([]db.Deployment, error)
	ListDeploymentsByVersionForOrg(context.Context, db.ListDeploymentsByVersionForOrgParams) ([]db.Deployment, error)
	ListDeploymentTasks(context.Context, db.ListDeploymentTasksParams) ([]db.DeploymentTask, error)
	PromoteDeployment(context.Context, db.PromoteDeploymentParams) (db.PromoteDeploymentRow, error)
}

type currentDeploymentReadStore interface {
	ListArtifactsByIDs(context.Context, db.ListArtifactsByIDsParams) ([]db.Artifact, error)
	ListCurrentDeploymentTasks(context.Context, db.ListCurrentDeploymentTasksParams) ([]db.DeploymentTask, error)
	GetCurrentDeploymentTask(context.Context, db.GetCurrentDeploymentTaskParams) (db.GetCurrentDeploymentTaskRow, error)
	ListCurrentDeploymentSandboxes(context.Context, db.ListCurrentDeploymentSandboxesParams) ([]db.DeploymentSandbox, error)
	GetCurrentDeploymentSandbox(context.Context, db.GetCurrentDeploymentSandboxParams) (db.DeploymentSandbox, error)
}

type casObjectLookupStore interface {
	GetCasObject(context.Context, string) (db.CasObject, error)
}

func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	store, ok := s.db.(deploymentStatusStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) && !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	projectID, environmentID, err := runScopeIDs(scope)
	if err != nil {
		writeError(w, errors.New("list deployments"))
		return
	}
	limit := int32(50)
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed <= 0 || parsed > 200 {
			writeError(w, badRequest(errors.New("limit must be an integer between 1 and 200")))
			return
		}
		limit = int32(parsed)
	}
	rows, err := store.ListScopedDeployments(r.Context(), db.ListScopedDeploymentsParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		RowLimit:      limit,
	})
	if err != nil {
		s.log.Error("list deployments failed", "error", err)
		writeError(w, errors.New("list deployments"))
		return
	}
	response := make([]api.DeploymentResponse, 0, len(rows))
	for _, row := range rows {
		item, err := deploymentResponseWithArtifacts(r.Context(), store, row)
		if err != nil {
			s.log.Error("get deployment artifacts failed", "deployment_id", pgvalue.MustUUIDValue(row.ID).String(), "error", err)
			writeError(w, errors.New("list deployments"))
			return
		}
		item.Tasks = []api.DeploymentTaskResponse{}
		response = append(response, item)
	}
	writeJSON(w, http.StatusOK, api.ListDeploymentsResponse{Deployments: response})
}

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

func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request) {
	store, scope, projectID, environmentID, ok := s.loadCurrentDeploymentReadScope(w, r)
	if !ok {
		return
	}
	actor := actorFromContext(r.Context())
	if !actor.HasPermission(auth.PermissionRunsRead, scope) && !actor.HasPermission(auth.PermissionTasksDeploy, scope) && !actorHasAnyPermission(actor, scope, workspaceReadPermissions()...) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	rows, err := store.ListCurrentDeploymentSandboxes(r.Context(), db.ListCurrentDeploymentSandboxesParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
	})
	if err != nil {
		s.log.Error("list sandboxes failed", "error", err)
		writeError(w, errors.New("list sandboxes"))
		return
	}
	response := make([]api.SandboxResponse, 0, len(rows))
	for _, row := range rows {
		response = append(response, sandboxResponse(row))
	}
	writeJSON(w, http.StatusOK, api.ListSandboxesResponse{Sandboxes: response})
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request) {
	store, scope, projectID, environmentID, ok := s.loadCurrentDeploymentReadScope(w, r)
	if !ok {
		return
	}
	actor := actorFromContext(r.Context())
	if !actor.HasPermission(auth.PermissionRunsRead, scope) && !actor.HasPermission(auth.PermissionTasksDeploy, scope) && !actorHasAnyPermission(actor, scope, workspaceReadPermissions()...) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	sandboxID := strings.TrimSpace(chi.URLParam(r, "sandboxID"))
	if sandboxID == "" {
		writeError(w, badRequest(errors.New("sandbox_id is required")))
		return
	}
	row, err := store.GetCurrentDeploymentSandbox(r.Context(), db.GetCurrentDeploymentSandboxParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		SandboxID:     sandboxID,
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("sandbox not found")))
		return
	}
	if err != nil {
		s.log.Error("get sandbox failed", "sandbox_id", sandboxID, "error", err)
		writeError(w, errors.New("get sandbox"))
		return
	}
	writeJSON(w, http.StatusOK, sandboxResponse(row))
}

func (s *Server) loadCurrentDeploymentReadScope(w http.ResponseWriter, r *http.Request) (currentDeploymentReadStore, auth.Scope, pgtype.UUID, pgtype.UUID, bool) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return nil, auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	store, ok := s.db.(currentDeploymentReadStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return nil, auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	actor := actorFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return nil, auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	projectID, environmentID, err := runScopeIDs(scope)
	if err != nil {
		writeError(w, errors.New("resolve current deployment read scope"))
		return nil, auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	return store, scope, projectID, environmentID, true
}

func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("project storage is not configured")))
		return
	}
	store, ok := s.db.(deploymentStatusStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	deploymentID, err := parseUUIDParam(r, "deploymentID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) && !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	projectID, environmentID, err := runScopeIDs(scope)
	if err != nil {
		s.log.Error("resolve deployment scope failed", "error", err)
		writeError(w, errors.New("get deployment"))
		return
	}
	deployment, err := store.GetDeployment(r.Context(), db.GetDeploymentParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ID:            pgvalue.UUID(deploymentID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("deployment not found")))
		return
	}
	if err != nil {
		s.log.Error("get deployment failed", "deployment_id", deploymentID.String(), "error", err)
		writeError(w, errors.New("get deployment"))
		return
	}
	response, err := deploymentResponseWithArtifacts(r.Context(), store, deployment)
	if err != nil {
		s.log.Error("get deployment artifacts failed", "deployment_id", deploymentID.String(), "error", err)
		writeError(w, errors.New("get deployment"))
		return
	}
	response.Tasks = []api.DeploymentTaskResponse{}
	if deployment.Status == db.DeploymentStatusDeployed {
		tasks, err := store.ListDeploymentTasks(r.Context(), db.ListDeploymentTasksParams{
			OrgID:         pgvalue.UUID(actor.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			DeploymentID:  deployment.ID,
		})
		if err != nil {
			s.log.Error("list deployment tasks failed", "deployment_id", deploymentID.String(), "error", err)
			writeError(w, errors.New("list deployment tasks"))
			return
		}
		response.Tasks = make([]api.DeploymentTaskResponse, 0, len(tasks))
		taskResponses, err := deploymentTaskResponses(r.Context(), store, tasks)
		if err != nil {
			s.log.Error("get deployment task artifacts failed", "deployment_id", deploymentID.String(), "error", err)
			writeError(w, errors.New("list deployment tasks"))
			return
		}
		response.Tasks = taskResponses
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getDeploymentEvents(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("project storage is not configured")))
		return
	}
	store, ok := s.db.(deploymentStatusStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	deploymentID, err := parseUUIDParam(r, "deploymentID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	cursor, err := eventCursor(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	limit, err := eventLimit(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) && !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	projectID, environmentID, err := runScopeIDs(scope)
	if err != nil {
		writeError(w, errors.New("get deployment events"))
		return
	}
	deployment, err := store.GetDeployment(r.Context(), db.GetDeploymentParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ID:            pgvalue.UUID(deploymentID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("deployment not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("get deployment"))
		return
	}
	if r.URL.Query().Get("follow") == "1" || strings.Contains(r.Header.Get("accept"), "text/event-stream") {
		s.followDeploymentEvents(w, r, actor.OrgID, deploymentID, cursor)
		return
	}
	rows, err := s.db.ListSubjectEvents(r.Context(), db.ListSubjectEventsParams{
		OrgID:       pgvalue.UUID(actor.OrgID),
		SubjectType: db.EventSubjectTypeDeployment,
		SubjectID:   deployment.ID,
		Seq:         cursor,
		RowLimit:    limit + 1,
	})
	if err != nil {
		writeError(w, errors.New("list deployment events"))
		return
	}
	hasNext := len(rows) > int(limit)
	if hasNext {
		rows = rows[:limit]
	}
	events := make([]api.RunEvent, 0, len(rows))
	for _, row := range rows {
		events = append(events, eventResponseFromRecord(row))
	}
	var nextCursor *int64
	if hasNext {
		value := rows[len(rows)-1].Seq
		nextCursor = &value
	}
	writeJSON(w, http.StatusOK, api.RunEventPage{Events: events, Cursor: cursor, NextCursor: nextCursor})
}

func (s *Server) followDeploymentEvents(w http.ResponseWriter, r *http.Request, orgID uuid.UUID, deploymentID uuid.UUID, cursor int64) {
	if s.eventStream == nil {
		writeError(w, unavailable(errors.New("event stream is not configured")))
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	ctx, cancel := context.WithTimeout(r.Context(), runEventsFollowMaxDuration)
	defer cancel()
	err := s.eventStream.ReadSubject(ctx, orgID, db.EventSubjectTypeDeployment, deploymentID, cursor, func(event api.RunEvent) error {
		_, _ = fmt.Fprintf(w, "id: %s\n", event.ID)
		_, _ = fmt.Fprint(w, "event: deployment_event\n")
		_, _ = fmt.Fprint(w, "data: ")
		if err := encoder.Encode(event); err != nil {
			return err
		}
		_, _ = fmt.Fprint(w, "\n")
		if flusher != nil {
			flusher.Flush()
		}
		if deploymentEventKindIsTerminal(event.Kind) {
			cancel()
		}
		return nil
	}, func() error {
		_, _ = fmt.Fprint(w, ": keep-alive\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.log.Warn("follow deployment events failed", "error", err)
	}
}

func deploymentEventKindIsTerminal(kind string) bool {
	switch kind {
	case "deployment.deployed", "deployment.failed":
		return true
	default:
		return false
	}
}

func (s *Server) getCurrentDeployment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("project storage is not configured")))
		return
	}
	store, ok := s.db.(currentDeploymentStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	projectID, environmentID, err := runScopeIDs(scope)
	if err != nil {
		s.log.Error("resolve deployment scope failed", "error", err)
		writeError(w, errors.New("get current deployment"))
		return
	}
	deployment, err := store.GetCurrentDeployment(r.Context(), db.GetCurrentDeploymentParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
	})
	if isNoRows(err) {
		writeJSON(w, http.StatusOK, api.GetCurrentDeploymentResponse{})
		return
	}
	if err != nil {
		s.log.Error("get current deployment failed", "error", err)
		writeError(w, errors.New("get current deployment"))
		return
	}
	tasks, err := store.ListDeploymentTasks(r.Context(), db.ListDeploymentTasksParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		DeploymentID:  deployment.ID,
	})
	if err != nil {
		s.log.Error("list deployment tasks failed", "deployment_id", pgvalue.MustUUIDValue(deployment.ID).String(), "error", err)
		writeError(w, errors.New("list deployment tasks"))
		return
	}
	response, err := deploymentResponseWithArtifacts(r.Context(), store, deployment)
	if err != nil {
		s.log.Error("get current deployment artifacts failed", "deployment_id", pgvalue.MustUUIDValue(deployment.ID).String(), "error", err)
		writeError(w, errors.New("get current deployment"))
		return
	}
	response.Tasks = make([]api.DeploymentTaskResponse, 0, len(tasks))
	taskResponses, err := deploymentTaskResponses(r.Context(), store, tasks)
	if err != nil {
		s.log.Error("get current deployment task artifacts failed", "deployment_id", pgvalue.MustUUIDValue(deployment.ID).String(), "error", err)
		writeError(w, errors.New("list deployment tasks"))
		return
	}
	response.Tasks = taskResponses
	writeJSON(w, http.StatusOK, api.GetCurrentDeploymentResponse{Deployment: &response})
}

func (s *Server) promoteDeployment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("project storage is not configured")))
		return
	}
	store, ok := s.db.(deploymentStatusStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	var request api.PromoteDeploymentRequest
	if err := decodeOptionalJSON(r.Body, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid promotion request JSON: %w", err)))
		return
	}
	actor := actorFromContext(r.Context())
	deploymentRef := strings.TrimSpace(chi.URLParam(r, "deployment"))
	if deploymentRef == "" {
		writeError(w, badRequest(errors.New("deployment is required")))
		return
	}
	projectRef, environmentRef, err := environmentScopeRefsFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if projectRef == "" && environmentRef == "" && actor.Kind == auth.ActorKindAPIKey {
		scope, ok := actor.EnvironmentScope()
		if !ok {
			writeError(w, badRequest(errors.New("API key is not bound to an environment")))
			return
		}
		projectRef = scope.ProjectID
		environmentRef = scope.EnvironmentID
	}
	request.ProjectID = projectRef
	request.EnvironmentID = environmentRef
	deployment, scope, projectID, environmentID, err := s.resolvePromotionTarget(r.Context(), store, actor.OrgID, deploymentRef, request)
	if isNoRows(err) {
		writeError(w, notFound(errors.New("deployment not found")))
		return
	}
	if errors.Is(err, errAmbiguousDeploymentVersion) {
		writeError(w, badRequest(err))
		return
	}
	if err != nil {
		s.log.Error("get deployment for promotion failed", "deployment", deploymentRef, "error", err)
		writeError(w, errors.New("get deployment"))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	principal, err := auth.ActorPrincipal(actor)
	if err != nil {
		writeError(w, forbidden(err))
		return
	}
	params := db.PromoteDeploymentParams{
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:               pgvalue.UUID(actor.OrgID),
		ProjectID:           projectID,
		EnvironmentID:       environmentID,
		DeploymentID:        deployment.ID,
		PromotedByPrincipal: principal,
		Reason:              strings.TrimSpace(request.Reason),
	}
	promoteStore := interface {
		PromoteDeployment(context.Context, db.PromoteDeploymentParams) (db.PromoteDeploymentRow, error)
	}(store)
	err = s.inTx(r.Context(), func(work *txWork) error {
		txPromoteStore, ok := work.q.(interface {
			PromoteDeployment(context.Context, db.PromoteDeploymentParams) (db.PromoteDeploymentRow, error)
		})
		if !ok {
			return unavailable(errors.New("deployment storage is not configured"))
		}
		promoteStore = txPromoteStore
		changedSchedules, err := promoteDeploymentAndSyncSchedules(r.Context(), promoteStore, params)
		if isNoRows(err) {
			return badRequest(errors.New("deployment is not deployable"))
		} else if errors.Is(err, errPermissionRequired) {
			return forbidden(err)
		} else if err != nil {
			s.log.Error("promote deployment failed", "deployment", deploymentRef, "error", err)
			return errors.New("promote deployment")
		}
		work.AfterCommit(func(ctx context.Context) error {
			s.registerChangedScheduleInstances(ctx, params.OrgID, params.ProjectID, changedSchedules)
			s.reconcilePreparedRuntimeSupplyAsync(ctx, "deployment_promotion")
			return nil
		})
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	response, err := deploymentResponseWithArtifacts(r.Context(), store, deployment)
	if err != nil {
		s.log.Error("get promoted deployment artifacts failed", "deployment", deploymentRef, "error", err)
		writeError(w, errors.New("promote deployment"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) createDeployment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("project storage is not configured")))
		return
	}
	if s.cas == nil {
		writeError(w, unavailable(errors.New("deployment source artifact storage is not configured")))
		return
	}
	reader, request, err := s.receiveDeploymentMetadata(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	metadata, err := deploymentMetadataFromRequest(r, request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	archivePath, cleanup, err := receiveDeploymentArchive(reader)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	defer cleanup()
	if err := validateDeploymentSourceArtifactArchive(archivePath); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid deployment source artifact: %w", err)))
		return
	}
	if err := validateDeploymentContentHash(archivePath, request.ContentHash); err != nil {
		writeError(w, badRequest(err))
		return
	}
	file, err := os.Open(archivePath)
	if err != nil {
		writeError(w, errors.New("open deployment source artifact"))
		return
	}
	artifactObject, err := s.cas.Put(r.Context(), api.DeploymentSourceArtifactMediaType, file)
	closeErr := file.Close()
	if err != nil {
		writeError(w, fmt.Errorf("store deployment source artifact: %w", err))
		return
	}
	if closeErr != nil {
		writeError(w, fmt.Errorf("close deployment source artifact: %w", closeErr))
		return
	}
	artifact := api.DeploymentSourceArtifact{
		Digest:    artifactObject.Digest,
		SizeBytes: artifactObject.SizeBytes,
		MediaType: artifactObject.MediaType,
	}
	cleanupArtifact := func() {
		s.deleteUnreferencedDeploymentSourceArtifact(r.Context(), artifact.Digest)
	}
	var response api.DeploymentResponse
	err = s.inTx(r.Context(), func(work *txWork) error {
		store, ok := work.q.(deploymentStore)
		if !ok {
			return unavailable(errors.New("deployment storage is not configured"))
		}
		var createErr error
		response, createErr = createDeploymentRecords(r.Context(), store, actor.OrgID, projectID, environmentID, strings.TrimSpace(request.ContentHash), artifact, metadata)
		return createErr
	})
	if err != nil {
		cleanupArtifact()
		writeDeploymentError(w, s, err)
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func deploymentMetadataFromRequest(r *http.Request, request api.CreateDeploymentRequest) (deploymentVersionMetadata, error) {
	apiVersion := firstNonEmptyString(request.APIVersion, requestAPIVersion(r))
	if apiVersion != api.CurrentAPIVersion {
		return deploymentVersionMetadata{}, fmt.Errorf("unsupported deployment api_version %q; current version is %s", apiVersion, api.CurrentAPIVersion)
	}
	bundleFormatVersion := request.BundleFormatVersion
	if bundleFormatVersion == 0 {
		bundleFormatVersion = api.CurrentBundleFormatVersion
	}
	if bundleFormatVersion != api.CurrentBundleFormatVersion {
		return deploymentVersionMetadata{}, fmt.Errorf("unsupported bundle_format_version %d; current version is %d", bundleFormatVersion, api.CurrentBundleFormatVersion)
	}
	workerProtocolVersion := firstNonEmptyString(request.WorkerProtocolVersion, api.CurrentWorkerProtocolVersion)
	if workerProtocolVersion != api.CurrentWorkerProtocolVersion {
		return deploymentVersionMetadata{}, fmt.Errorf("unsupported worker_protocol_version %q; current version is %s", workerProtocolVersion, api.CurrentWorkerProtocolVersion)
	}
	return deploymentVersionMetadata{
		APIVersion:            apiVersion,
		SDKVersion:            firstNonEmptyString(request.SDKVersion, r.Header.Get(api.SDKVersionHeader)),
		CLIVersion:            firstNonEmptyString(request.CLIVersion, r.Header.Get(api.CLIVersionHeader), r.Header.Get(api.ClientVersionHeader)),
		BundleFormatVersion:   bundleFormatVersion,
		WorkerProtocolVersion: workerProtocolVersion,
	}, nil
}

func (s *Server) receiveDeploymentMetadata(r *http.Request) (*multipart.Reader, api.CreateDeploymentRequest, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, api.CreateDeploymentRequest{}, fmt.Errorf("invalid deployment multipart form: %w", err)
	}
	var request api.CreateDeploymentRequest
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, api.CreateDeploymentRequest{}, errors.New("deployment metadata is required")
		}
		if err != nil {
			return nil, api.CreateDeploymentRequest{}, fmt.Errorf("read deployment multipart form: %w", err)
		}
		name := part.FormName()
		switch name {
		case "metadata":
			metadata, err := readLimitedFormField(part, 1<<20)
			if err != nil {
				return nil, api.CreateDeploymentRequest{}, fmt.Errorf("read deployment metadata: %w", err)
			}
			decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(metadata)))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				return nil, api.CreateDeploymentRequest{}, fmt.Errorf("invalid deployment metadata JSON: %w", err)
			}
			if err := decoder.Decode(&struct{}{}); err != io.EOF {
				return nil, api.CreateDeploymentRequest{}, errors.New("deployment metadata must contain a single JSON value")
			}
			return reader, request, nil
		case "deployment_source":
			return nil, api.CreateDeploymentRequest{}, errors.New("deployment metadata must precede deployment_source")
		default:
			return nil, api.CreateDeploymentRequest{}, fmt.Errorf("unexpected deployment multipart field %q", name)
		}
	}
}

func receiveDeploymentArchive(reader *multipart.Reader) (string, func(), error) {
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return "", func() {}, errors.New("deployment_source file is required")
		}
		if err != nil {
			return "", func() {}, fmt.Errorf("read deployment multipart form: %w", err)
		}
		if part.FormName() != "deployment_source" {
			part.Close()
			continue
		}
		defer part.Close()
		tmp, err := os.CreateTemp("", "helmr-deployment-source-*.tar")
		if err != nil {
			return "", func() {}, fmt.Errorf("create deployment source temp file: %w", err)
		}
		path := tmp.Name()
		cleanup := func() { _ = os.Remove(path) }
		if _, err := io.Copy(tmp, part); err != nil {
			_ = tmp.Close()
			cleanup()
			return "", func() {}, fmt.Errorf("copy deployment source artifact: %w", err)
		}
		if err := tmp.Close(); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("close deployment source artifact: %w", err)
		}
		return path, cleanup, nil
	}
}

func readLimitedFormField(part *multipart.Part, limit int64) (string, error) {
	defer part.Close()
	bytes, err := io.ReadAll(io.LimitReader(part, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(bytes)) > limit {
		return "", errors.New("field is too large")
	}
	return string(bytes), nil
}

func (s *Server) deleteUnreferencedDeploymentSourceArtifact(ctx context.Context, digest string) {
	digest = strings.TrimSpace(digest)
	if digest == "" || s.cas == nil {
		return
	}
	if store, ok := s.db.(casObjectLookupStore); ok {
		if _, err := store.GetCasObject(ctx, digest); err == nil {
			return
		} else if !isNoRows(err) {
			if s.log != nil {
				s.log.Warn("skip deployment source artifact cleanup after CAS lookup failure", "digest", digest, "error", err)
			}
			return
		}
	}
	if err := s.cas.Delete(ctx, digest); err != nil && s.log != nil {
		s.log.Warn("delete unreferenced deployment source artifact", "digest", digest, "error", err)
	}
}

func validateDeploymentSourceArtifactArchive(archivePath string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open deployment source artifact: %w", err)
	}
	defer file.Close()
	destination, err := os.MkdirTemp("", "helmr-deployment-source-validate-*")
	if err != nil {
		return fmt.Errorf("create deployment source validation directory: %w", err)
	}
	defer os.RemoveAll(destination)
	if err := archive.ExtractTar(file, destination); err != nil {
		return err
	}
	return nil
}

func validateDeploymentContentHash(archivePath string, contentHash string) error {
	contentHash = strings.TrimSpace(contentHash)
	if contentHash == "" {
		return errors.New("deployment content_hash is required")
	}
	digest, err := deploymentArchiveDigest(archivePath)
	if err != nil {
		return fmt.Errorf("hash deployment source artifact: %w", err)
	}
	if digest != contentHash {
		return fmt.Errorf("deployment source content_hash %s does not match uploaded archive digest %s", contentHash, digest)
	}
	return nil
}

func deploymentArchiveDigest(archivePath string) (string, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func createDeploymentRecords(ctx context.Context, store deploymentStore, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, contentHash string, artifact api.DeploymentSourceArtifact, metadata deploymentVersionMetadata) (api.DeploymentResponse, error) {
	if _, err := store.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		Digest:    artifact.Digest,
		SizeBytes: artifact.SizeBytes,
		MediaType: artifact.MediaType,
	}); err != nil {
		return api.DeploymentResponse{}, err
	}
	workerGroup, err := store.GetDefaultWorkerGroup(ctx)
	if err != nil {
		return api.DeploymentResponse{}, fmt.Errorf("get default worker group: %w", err)
	}
	if err := store.LockDeploymentReusableBuildKey(ctx, db.LockDeploymentReusableBuildKeyParams{
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		WorkerGroupID: workerGroup.ID,
		ContentHash:   contentHash,
	}); err != nil {
		return api.DeploymentResponse{}, err
	}
	deployment, err := store.GetReusableDeploymentByContentHash(ctx, db.GetReusableDeploymentByContentHashParams{
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ContentHash:   contentHash,
		WorkerGroupID: workerGroup.ID,
	})
	if isNoRows(err) {
		deployment, err = createQueuedDeployment(ctx, store, orgID, projectID, environmentID, workerGroup.ID, contentHash, artifact, metadata)
	}
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	response, err := deploymentResponseWithArtifacts(ctx, store, deployment)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	return response, nil
}

func createQueuedDeployment(ctx context.Context, store deploymentStore, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, workerGroupID pgtype.UUID, contentHash string, artifact api.DeploymentSourceArtifact, metadata deploymentVersionMetadata) (db.Deployment, error) {
	version, err := nextDeploymentVersion(ctx, store, orgID, projectID, environmentID)
	if err != nil {
		return db.Deployment{}, err
	}
	sourceArtifact, err := store.CreateArtifact(ctx, db.CreateArtifactParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Digest:        artifact.Digest,
		Kind:          db.ArtifactKindDeploymentSource,
		SizeBytes:     artifact.SizeBytes,
		MediaType:     artifact.MediaType,
	})
	if err != nil {
		return db.Deployment{}, err
	}
	deployment, err := store.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID:                         pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                      pgvalue.UUID(orgID),
		ProjectID:                  projectID,
		EnvironmentID:              environmentID,
		Version:                    version,
		ApiVersion:                 metadata.APIVersion,
		SdkVersion:                 metadata.SDKVersion,
		CliVersion:                 metadata.CLIVersion,
		BundleFormatVersion:        metadata.BundleFormatVersion,
		WorkerProtocolVersion:      metadata.WorkerProtocolVersion,
		WorkerGroupID:              workerGroupID,
		ContentHash:                contentHash,
		DeploymentSourceArtifactID: sourceArtifact.ID,
		Status:                     db.DeploymentStatusQueued,
	})
	if err != nil {
		return db.Deployment{}, err
	}
	if err := appendDeploymentLifecycleEvent(ctx, store, deployment.OrgID, deployment.ProjectID, deployment.EnvironmentID, deployment.ID, "deployment.queued", "info", "control", "queued", "Deployment queued"); err != nil {
		return db.Deployment{}, err
	}
	return deployment, nil
}

func appendDeploymentLifecycleEvent(ctx context.Context, store deploymentEventAppender, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, deploymentID pgtype.UUID, kind string, severity string, source string, status string, message string) error {
	payload, err := json.Marshal(map[string]string{"status": status})
	if err != nil {
		return err
	}
	_, err = store.AppendDeploymentEvent(ctx, db.AppendDeploymentEventParams{
		OrgID:          orgID,
		ProjectID:      projectID,
		EnvironmentID:  environmentID,
		DeploymentID:   deploymentID,
		Category:       "lifecycle",
		Severity:       severity,
		Source:         source,
		Kind:           kind,
		Message:        message,
		Payload:        payload,
		RedactionClass: "internal",
	})
	return err
}

func nextDeploymentVersion(ctx context.Context, store deploymentStore, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID) (string, error) {
	return store.AllocateDeploymentVersion(ctx, db.AllocateDeploymentVersionParams{
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Prefix:        deploymentVersionPrefix(),
	})
}

func deploymentVersionPrefix() string {
	return time.Now().UTC().Format("20060102")
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

var errAmbiguousDeploymentVersion = errors.New("deployment version is ambiguous; provide project_id and environment_id")

func (s *Server) resolvePromotionTarget(ctx context.Context, store deploymentStatusStore, orgID uuid.UUID, deploymentRef string, request api.PromoteDeploymentRequest) (db.Deployment, auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	projectRef := strings.TrimSpace(request.ProjectID)
	environmentRef := strings.TrimSpace(request.EnvironmentID)
	if projectRef != "" || environmentRef != "" {
		scope, projectID, environmentID, err := s.secretRequestScope(ctx, orgID, projectRef, environmentRef)
		if err != nil {
			return db.Deployment{}, auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
		}
		deployment, err := deploymentByIDOrVersion(ctx, store, orgID, projectID, environmentID, deploymentRef)
		return deployment, scope, projectID, environmentID, err
	}
	deployment, err := deploymentByIDOrVersionForOrg(ctx, store, orgID, deploymentRef)
	if err != nil {
		return db.Deployment{}, auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return deployment, auth.Scope{
		OrgID:         orgID,
		ProjectID:     pgvalue.MustUUIDValue(deployment.ProjectID).String(),
		EnvironmentID: pgvalue.MustUUIDValue(deployment.EnvironmentID).String(),
	}, deployment.ProjectID, deployment.EnvironmentID, nil
}

func deploymentByIDOrVersionForOrg(ctx context.Context, store deploymentStatusStore, orgID uuid.UUID, deploymentRef string) (db.Deployment, error) {
	if deploymentID, err := uuid.Parse(deploymentRef); err == nil {
		return store.GetDeploymentForOrg(ctx, db.GetDeploymentForOrgParams{
			OrgID: pgvalue.UUID(orgID),
			ID:    pgvalue.UUID(deploymentID),
		})
	}
	deployments, err := store.ListDeploymentsByVersionForOrg(ctx, db.ListDeploymentsByVersionForOrgParams{
		OrgID:   pgvalue.UUID(orgID),
		Version: strings.TrimSpace(deploymentRef),
	})
	if err != nil {
		return db.Deployment{}, err
	}
	switch len(deployments) {
	case 0:
		return db.Deployment{}, errRecordNotFound
	case 1:
		return deployments[0], nil
	default:
		return db.Deployment{}, errAmbiguousDeploymentVersion
	}
}

func deploymentByIDOrVersion(ctx context.Context, store deploymentStatusStore, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, deploymentRef string) (db.Deployment, error) {
	if deploymentID, err := uuid.Parse(deploymentRef); err == nil {
		return store.GetDeployment(ctx, db.GetDeploymentParams{
			OrgID:         pgvalue.UUID(orgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ID:            pgvalue.UUID(deploymentID),
		})
	}
	return store.GetDeploymentByVersion(ctx, db.GetDeploymentByVersionParams{
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Version:       strings.TrimSpace(deploymentRef),
	})
}

func deploymentResponse(deployment db.Deployment, artifact api.DeploymentSourceArtifact, buildManifestDigest string, deploymentManifestDigest string) api.DeploymentResponse {
	return api.DeploymentResponse{
		ID:                       pgvalue.MustUUIDValue(deployment.ID).String(),
		Version:                  deployment.Version,
		APIVersion:               deployment.ApiVersion,
		SDKVersion:               deployment.SdkVersion,
		CLIVersion:               deployment.CliVersion,
		BundleFormatVersion:      deployment.BundleFormatVersion,
		WorkerProtocolVersion:    deployment.WorkerProtocolVersion,
		ProjectID:                pgvalue.MustUUIDValue(deployment.ProjectID).String(),
		EnvironmentID:            pgvalue.MustUUIDValue(deployment.EnvironmentID).String(),
		ContentHash:              deployment.ContentHash,
		DeploymentSource:         artifact,
		BuildManifestDigest:      buildManifestDigest,
		DeploymentManifestDigest: deploymentManifestDigest,
		Status:                   string(deployment.Status),
		Error:                    deploymentErrorResponse(deployment.Failure),
		CreatedAt:                pgvalue.Time(deployment.CreatedAt),
		BuildingAt:               pgvalue.Time(deployment.BuildingAt),
		BuiltAt:                  pgvalue.Time(deployment.BuiltAt),
		DeployedAt:               pgvalue.Time(deployment.DeployedAt),
		FailedAt:                 pgvalue.Time(deployment.FailedAt),
	}
}

type artifactLister interface {
	ListArtifactsByIDs(context.Context, db.ListArtifactsByIDsParams) ([]db.Artifact, error)
}

func deploymentResponseWithArtifacts(ctx context.Context, store artifactLister, deployment db.Deployment) (api.DeploymentResponse, error) {
	idsToResolve := []pgtype.UUID{deployment.DeploymentSourceArtifactID}
	if deployment.BuildManifestArtifactID.Valid {
		idsToResolve = append(idsToResolve, deployment.BuildManifestArtifactID)
	}
	if deployment.DeploymentManifestArtifactID.Valid {
		idsToResolve = append(idsToResolve, deployment.DeploymentManifestArtifactID)
	}
	artifacts, err := scopedArtifactsByID(ctx, store, deployment.OrgID, deployment.ProjectID, deployment.EnvironmentID, idsToResolve)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	sourceArtifact, err := deploymentSourceArtifact(artifacts, deployment.DeploymentSourceArtifactID)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	buildManifestDigest, err := optionalDeploymentArtifactDigest(artifacts, deployment.BuildManifestArtifactID)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	deploymentManifestDigest, err := optionalDeploymentArtifactDigest(artifacts, deployment.DeploymentManifestArtifactID)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	return deploymentResponse(deployment, sourceArtifact, buildManifestDigest, deploymentManifestDigest), nil
}

func deploymentSourceArtifact(artifacts map[pgtype.UUID]db.Artifact, artifactID pgtype.UUID) (api.DeploymentSourceArtifact, error) {
	artifact, err := requiredArtifact(artifacts, artifactID)
	if err != nil {
		return api.DeploymentSourceArtifact{}, err
	}
	return api.DeploymentSourceArtifact{
		Digest:    artifact.Digest,
		SizeBytes: artifact.SizeBytes,
		MediaType: artifact.MediaType,
	}, nil
}

func optionalDeploymentArtifactDigest(artifacts map[pgtype.UUID]db.Artifact, artifactID pgtype.UUID) (string, error) {
	if !artifactID.Valid {
		return "", nil
	}
	artifact, err := requiredArtifact(artifacts, artifactID)
	if err != nil {
		return "", err
	}
	return artifact.Digest, nil
}

func deploymentErrorResponse(raw []byte) *api.DeploymentErrorResponse {
	message := strings.TrimSpace(string(raw))
	if message == "" || message == "null" {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return &api.DeploymentErrorResponse{Message: message}
	}
	if value, ok := payload["message"].(string); ok && strings.TrimSpace(value) != "" {
		return &api.DeploymentErrorResponse{Message: strings.TrimSpace(value)}
	}
	if value, ok := payload["error"].(string); ok && strings.TrimSpace(value) != "" {
		return &api.DeploymentErrorResponse{Message: strings.TrimSpace(value)}
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		if value, ok := nested["message"].(string); ok && strings.TrimSpace(value) != "" {
			return &api.DeploymentErrorResponse{Message: strings.TrimSpace(value)}
		}
	}
	return nil
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

func sandboxResponse(row db.DeploymentSandbox) api.SandboxResponse {
	return api.SandboxResponse{
		ID:                  pgvalue.MustUUIDValue(row.ID).String(),
		DeploymentID:        pgvalue.MustUUIDValue(row.DeploymentID).String(),
		SandboxID:           row.SandboxID,
		Fingerprint:         row.Fingerprint,
		ImageArtifactID:     pgvalue.MustUUIDValue(row.ImageArtifactID).String(),
		ImageArtifactFormat: row.ImageArtifactFormat,
		RootfsDigest:        row.RootfsDigest,
		ImageDigest:         row.ImageDigest,
		ImageFormat:         row.ImageFormat,
		WorkspaceMountPath:  row.WorkspaceMountPath,
		ResourceFloor:       json.RawMessage(row.ResourceFloor),
		DiskFloorMib:        int32(row.DiskFloorMib),
		NetworkPolicy:       json.RawMessage(row.NetworkPolicy),
		RuntimeABI:          row.RuntimeABI,
		GuestdABI:           row.GuestdAbi,
		AdapterABI:          row.AdapterAbi,
		FilesystemFormat:    row.FilesystemFormat,
		DefaultUID:          int8Response(row.DefaultUid),
		DefaultGID:          int8Response(row.DefaultGid),
		DefaultWorkdir:      row.DefaultWorkdir,
		ContractVersion:     row.ContractVersion,
		CreatedAt:           pgvalue.Time(row.CreatedAt),
	}
}

func int8Response(value pgtype.Int8) *int32 {
	if !value.Valid {
		return nil
	}
	out := int32(value.Int64)
	return &out
}

func scopedArtifactsByID(ctx context.Context, store artifactLister, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, artifactIDs []pgtype.UUID) (map[pgtype.UUID]db.Artifact, error) {
	unique := make([]pgtype.UUID, 0, len(artifactIDs))
	seen := map[pgtype.UUID]struct{}{}
	for _, artifactID := range artifactIDs {
		if !artifactID.Valid {
			continue
		}
		if _, ok := seen[artifactID]; ok {
			continue
		}
		seen[artifactID] = struct{}{}
		unique = append(unique, artifactID)
	}
	if len(unique) == 0 {
		return map[pgtype.UUID]db.Artifact{}, nil
	}
	rows, err := store.ListArtifactsByIDs(ctx, db.ListArtifactsByIDsParams{
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Ids:           unique,
	})
	if err != nil {
		return nil, err
	}
	artifacts := make(map[pgtype.UUID]db.Artifact, len(rows))
	for _, artifact := range rows {
		artifacts[artifact.ID] = artifact
	}
	for _, artifactID := range unique {
		if _, ok := artifacts[artifactID]; !ok {
			return nil, errRecordNotFound
		}
	}
	return artifacts, nil
}

func requiredArtifact(artifacts map[pgtype.UUID]db.Artifact, artifactID pgtype.UUID) (db.Artifact, error) {
	artifact, ok := artifacts[artifactID]
	if !ok {
		return db.Artifact{}, errRecordNotFound
	}
	return artifact, nil
}

func writeDeploymentError(w http.ResponseWriter, s *Server, err error) {
	if isUniqueViolation(err) {
		writeError(w, badRequest(errors.New("deployment conflicts with existing task metadata")))
		return
	}
	s.log.Error("create deployment failed", "error", err)
	writeError(w, errors.New("create deployment"))
}
