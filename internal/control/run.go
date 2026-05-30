package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ghapp"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultRunMaxDurationSeconds = int32(900)
	minRunMaxDurationSeconds     = int32(5)
	maxRunDurationSeconds        = int32(86400)
	maxRunLogSnapshotBytes       = int64(1 << 20)
	runEventsPageSize            = int32(200)
	runEventsFollowMaxDuration   = 30 * time.Minute
	runEventsFollowFallbackEvery = 15 * time.Second
)

type githubCommitResolver interface {
	ResolveCommit(context.Context, int64, int64, api.GitHubSource) (ghapp.ResolvedSource, error)
	CreateRepositoryToken(context.Context, int64, int64) (ghapp.InstallationToken, error)
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	if s.github == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("github resolver is not configured"))
		return
	}
	actor := actorFromContext(r.Context())

	var request api.CreateRunRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid run request JSON: %w", err))
		return
	}
	request.TaskID = strings.TrimSpace(request.TaskID)
	if err := api.ValidateTaskID(request.TaskID); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	deploymentSelection, err := normalizeRunDeploymentSelection(request.DeploymentID, request.Version)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	scope, projectID, environmentID, err := s.createRunRequestScope(r.Context(), actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !actor.HasPermission(auth.PermissionRunsCreate, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	payload := request.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		writeError(w, http.StatusBadRequest, errors.New("payload must be valid JSON"))
		return
	}
	if request.Secrets == nil {
		request.Secrets = api.SecretBindings{}
	}
	if err := secret.ValidateBindings(request.Secrets); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(request.Secrets) > 0 && !actor.HasPermission(auth.PermissionSecretsUse, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required to bind secrets"))
		return
	}
	if len(request.Secrets) > 0 && s.secrets == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("secret store is not configured"))
		return
	}
	if len(request.Secrets) > 0 {
		if err := s.secrets.CheckScoped(r.Context(), actor.OrgID, ids.MustFromPG(projectID), ids.MustFromPG(environmentID), request.Secrets); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	secretBindingsJSON, err := json.Marshal(request.Secrets)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("secret bindings encode failed: %w", err))
		return
	}

	workspaceInput := api.GitHubSource{
		Repository: request.Workspace.Repository,
		Ref:        request.Workspace.Ref,
		SHA:        request.Workspace.SHA,
		Subpath:    request.Workspace.Subpath,
	}
	normalizedWorkspace, err := ghapp.NormalizeSource(workspaceInput)
	if err != nil {
		writeError(w, http.StatusBadRequest, relabelGitHubSourceError(err, "workspace"))
		return
	}
	workspaceRepository, err := s.db.GetActiveProjectGitHubRepositoryByFullName(r.Context(), db.GetActiveProjectGitHubRepositoryByFullNameParams{
		OrgID:     ids.ToPG(actor.OrgID),
		ProjectID: projectID,
		FullName:  normalizedWorkspace.Repository,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, relabelGitHubSourceError(ghapp.InvalidSourceError{Err: errors.New("github repository is not enabled for this project workspace")}, "workspace"))
		return
	}
	if err != nil {
		s.log.Error("authorize github workspace repository failed", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("authorize github workspace repository"))
		return
	}
	resolvedWorkspace, err := s.github.ResolveCommit(r.Context(), workspaceRepository.InstallationID, workspaceRepository.GithubRepositoryID, normalizedWorkspace)
	if err != nil {
		status := http.StatusBadGateway
		if ghapp.IsInvalidSource(err) || ghapp.IsNotFound(err) {
			status = http.StatusBadRequest
		}
		writeError(w, status, relabelGitHubSourceError(err, "workspace"))
		return
	}
	workspace := resolvedWorkspace.Source
	deploymentTask, err := s.deploymentTaskForRunRequest(r.Context(), actor.OrgID, projectID, environmentID, request.TaskID, deploymentSelection)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("task %q is not deployed in the selected deployment", request.TaskID))
		return
	}
	var runDeploymentErr runDeploymentSelectionError
	if errors.As(err, &runDeploymentErr) {
		writeError(w, http.StatusBadRequest, runDeploymentErr)
		return
	}
	if err != nil {
		s.log.Error("load deployment task failed", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("load deployment task"))
		return
	}
	maxDurationSeconds, err := runMaxDurationSeconds(request.MaxDurationSeconds, deploymentTask.MaxDurationSeconds)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	runID := ids.New()
	createdPayload, err := runCreatedEventPayload(request.TaskID, payload, workspace, maxDurationSeconds, request.Secrets)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode run created event"))
		return
	}
	workspaceFields := workspaceSourceDBFieldsFromAPI(workspace)
	run, err := s.db.CreateScopedRun(r.Context(), db.CreateScopedRunParams{
		ID:                          ids.ToPG(runID),
		OrgID:                       ids.ToPG(actor.OrgID),
		ProjectID:                   projectID,
		EnvironmentID:               environmentID,
		DeploymentID:                deploymentTask.DeploymentID,
		DeploymentTaskID:            deploymentTask.ID,
		TaskID:                      request.TaskID,
		Payload:                     payload,
		SecretBindings:              secretBindingsJSON,
		WorkspaceRepository:         workspace.Repository,
		WorkspaceInstallationID:     resolvedWorkspace.InstallationID,
		WorkspaceGithubRepositoryID: resolvedWorkspace.GitHubRepositoryID,
		WorkspaceRef:                workspace.Ref,
		WorkspaceSha:                workspace.SHA,
		WorkspaceSubpath:            workspace.Subpath,
		WorkspaceRefKind:            workspaceFields.RefKind,
		WorkspaceRefName:            workspaceFields.RefName,
		WorkspaceFullRef:            workspaceFields.FullRef,
		WorkspaceDefaultBranch:      workspaceFields.DefaultBranch,
		WorkspacePrNumber:           workspaceFields.PRNumber,
		WorkspacePrBaseRef:          workspaceFields.PRBaseRef,
		WorkspacePrBaseSha:          workspaceFields.PRBaseSHA,
		WorkspacePrHeadRef:          workspaceFields.PRHeadRef,
		WorkspacePrHeadSha:          workspaceFields.PRHeadSHA,
		MaxDurationSeconds:          maxDurationSeconds,
		EventPayload:                createdPayload,
	})
	if err != nil {
		s.log.Error("create run failed", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("create run"))
		return
	}
	if s.runEnqueuer != nil {
		if _, err := s.runEnqueuer.EnqueueRun(r.Context(), run.OrgID, run.ID); err != nil {
			s.log.Error("enqueue run queue item failed", "run_id", ids.MustFromPG(run.ID).String(), "error", err)
		}
	}

	writeJSON(w, http.StatusCreated, runResponse(createScopedRunSummary(run)))
}

type runDeploymentSelection struct {
	deploymentID pgtype.UUID
	version      string
}

func normalizeRunDeploymentSelection(deploymentID string, version string) (runDeploymentSelection, error) {
	deploymentID = strings.TrimSpace(deploymentID)
	version = strings.TrimSpace(version)
	if deploymentID != "" && version != "" {
		return runDeploymentSelection{}, errors.New("deployment_id and version cannot be combined")
	}
	if deploymentID != "" {
		parsedID, err := ids.Parse(deploymentID)
		if err != nil {
			return runDeploymentSelection{}, errors.New("deployment_id must be a UUID")
		}
		return runDeploymentSelection{deploymentID: ids.ToPG(parsedID)}, nil
	}
	return runDeploymentSelection{version: version}, nil
}

type runDeploymentSelectionError struct {
	err error
}

func (e runDeploymentSelectionError) Error() string {
	return e.err.Error()
}

func (e runDeploymentSelectionError) Unwrap() error {
	return e.err
}

func runDeploymentSelectionErrorf(format string, args ...any) error {
	return runDeploymentSelectionError{err: fmt.Errorf(format, args...)}
}

func (s *Server) deploymentTaskForRunRequest(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, selection runDeploymentSelection) (db.GetDeploymentTaskRow, error) {
	deploymentID := selection.deploymentID
	if deploymentID.Valid {
		deployment, err := s.db.GetDeployment(ctx, db.GetDeploymentParams{
			OrgID:         ids.ToPG(orgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ID:            deploymentID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.GetDeploymentTaskRow{}, runDeploymentSelectionErrorf("deployment_id %s was not found in this environment", ids.MustFromPG(deploymentID).String())
		}
		if err != nil {
			return db.GetDeploymentTaskRow{}, err
		}
		if deployment.Status != db.DeploymentStatusDeployed {
			return db.GetDeploymentTaskRow{}, runDeploymentSelectionErrorf("deployment_id %s is not deployed", ids.MustFromPG(deploymentID).String())
		}
		return s.deploymentTask(ctx, orgID, projectID, environmentID, deployment.ID, taskID)
	}
	if selection.version != "" {
		deployment, err := s.db.GetDeploymentByVersion(ctx, db.GetDeploymentByVersionParams{
			OrgID:         ids.ToPG(orgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			Version:       selection.version,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.GetDeploymentTaskRow{}, runDeploymentSelectionErrorf("deployment version %q was not found in this environment", selection.version)
		}
		if err != nil {
			return db.GetDeploymentTaskRow{}, err
		}
		if deployment.Status != db.DeploymentStatusDeployed {
			return db.GetDeploymentTaskRow{}, runDeploymentSelectionErrorf("deployment version %q is not deployed", selection.version)
		}
		return s.deploymentTask(ctx, orgID, projectID, environmentID, deployment.ID, taskID)
	}
	task, err := s.db.GetCurrentDeploymentTask(ctx, db.GetCurrentDeploymentTaskParams{
		OrgID:         ids.ToPG(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		TaskID:        taskID,
	})
	if err != nil {
		return db.GetDeploymentTaskRow{}, err
	}
	return deploymentTaskRowFromCurrent(task), nil
}

func deploymentTaskRowFromCurrent(task db.GetCurrentDeploymentTaskRow) db.GetDeploymentTaskRow {
	return db.GetDeploymentTaskRow{
		ID:                     task.ID,
		OrgID:                  task.OrgID,
		ProjectID:              task.ProjectID,
		EnvironmentID:          task.EnvironmentID,
		DeploymentID:           task.DeploymentID,
		TaskID:                 task.TaskID,
		FilePath:               task.FilePath,
		ExportName:             task.ExportName,
		HandlerEntrypoint:      task.HandlerEntrypoint,
		BundleDigest:           task.BundleDigest,
		RequestedMilliCpu:      task.RequestedMilliCpu,
		RequestedMemoryMib:     task.RequestedMemoryMib,
		SecretDeclarations:     task.SecretDeclarations,
		ResourceRequirements:   task.ResourceRequirements,
		PayloadSchema:          task.PayloadSchema,
		MaxDurationSeconds:     task.MaxDurationSeconds,
		CreatedAt:              task.CreatedAt,
		DeploymentSourceDigest: task.DeploymentSourceDigest,
	}
}

func (s *Server) deploymentTask(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, deploymentID pgtype.UUID, taskID string) (db.GetDeploymentTaskRow, error) {
	return s.db.GetDeploymentTask(ctx, db.GetDeploymentTaskParams{
		OrgID:         ids.ToPG(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		DeploymentID:  deploymentID,
		TaskID:        taskID,
	})
}

func (s *Server) createRunRequestScope(ctx context.Context, actor auth.Actor, projectID string, environmentID string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	projectID = strings.TrimSpace(projectID)
	environmentID = strings.TrimSpace(environmentID)
	if actor.Kind != auth.ActorKindAPIKey || projectID != "" || environmentID != "" {
		return s.secretRequestScope(ctx, actor.OrgID, projectID, environmentID)
	}
	scope, err := inferAPIKeyCreateRunScope(actor)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	scopeProjectID, scopeEnvironmentID, err := s.runScopeIDs(ctx, actor.OrgID, scope)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return scope, scopeProjectID, scopeEnvironmentID, nil
}

func inferAPIKeyCreateRunScope(actor auth.Actor) (auth.Scope, error) {
	type scopeKey struct {
		projectID     string
		environmentID string
	}
	scopes := map[scopeKey]struct{}{}
	for _, grant := range actor.Permissions {
		if !permissionGrantIncludes(grant, auth.PermissionRunsCreate) {
			continue
		}
		projectID, environmentID, ok := inferableAPIKeyRunScope(grant.ProjectID, grant.EnvironmentID)
		if !ok {
			continue
		}
		scopes[scopeKey{projectID: projectID, environmentID: environmentID}] = struct{}{}
	}
	if len(scopes) != 1 {
		return auth.Scope{}, errors.New("API key run creation requires exactly one environment-scoped runs.create grant when project_id and environment_id are omitted")
	}
	for scope := range scopes {
		return auth.Scope{OrgID: actor.OrgID, ProjectID: scope.projectID, EnvironmentID: scope.environmentID}, nil
	}
	return auth.Scope{}, errors.New("API key run creation requires exactly one environment-scoped runs.create grant when project_id and environment_id are omitted")
}

func permissionGrantIncludes(grant auth.PermissionGrant, permission auth.Permission) bool {
	for _, granted := range grant.Permissions {
		if granted == permission {
			return true
		}
	}
	return false
}

func inferableAPIKeyRunScope(projectValue string, environmentValue string) (string, string, bool) {
	projectValue = strings.TrimSpace(projectValue)
	environmentValue = strings.TrimSpace(environmentValue)
	if projectValue == "*" || environmentValue == "*" {
		return "", "", false
	}
	if (projectValue == "" || projectValue == auth.DefaultProjectID) &&
		(environmentValue == "" || environmentValue == auth.DefaultEnvironmentID) {
		return auth.DefaultProjectID, auth.DefaultEnvironmentID, true
	}
	if projectValue == "" || environmentValue == "" || projectValue == auth.DefaultProjectID || environmentValue == auth.DefaultEnvironmentID {
		return "", "", false
	}
	if _, err := ids.Parse(projectValue); err != nil {
		return "", "", false
	}
	if _, err := ids.Parse(environmentValue); err != nil {
		return "", "", false
	}
	return projectValue, environmentValue, true
}

func runCreatedEventPayload(taskID string, payload json.RawMessage, workspace api.GitHubSource, maxDurationSeconds int32, secrets api.SecretBindings) ([]byte, error) {
	secretNames := make([]string, 0, len(secrets))
	for name := range secrets {
		secretNames = append(secretNames, name)
	}
	sort.Strings(secretNames)
	return json.Marshal(map[string]any{
		"task_id":              taskID,
		"payload":              payload,
		"workspace":            workspace,
		"max_duration_seconds": maxDurationSeconds,
		"secret_names":         secretNames,
	})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	runID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	run, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{
		OrgID: ids.ToPG(actorFromContext(r.Context()).OrgID),
		ID:    ids.ToPG(runID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("run not found"))
		return
	}
	if err != nil {
		s.log.Error("get run failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get run"))
		return
	}
	summary := getRunSummary(run)
	actor := actorFromContext(r.Context())
	scope, err := s.runScope(r.Context(), actor.OrgID, summary)
	if err != nil {
		s.log.Error("resolve run scope failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get run"))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	response, err := s.runResponse(r.Context(), summary)
	if err != nil {
		s.log.Error("get pending waitpoint failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get run"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	statusFilter, limit, err := listRunsQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	summaries, err := s.listRunSummaries(r, actor, statusFilter, limit)
	if errors.Is(err, errPermissionRequired) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	if isScopeRequestError(err) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err != nil {
		s.log.Error("list runs failed", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("list runs"))
		return
	}
	response := api.ListRunsResponse{Runs: make([]api.RunResponse, 0, len(summaries))}
	for _, run := range summaries {
		item, err := s.runResponse(r.Context(), run)
		if err != nil {
			s.log.Error("list pending waitpoint failed", "run_id", ids.MustFromPG(run.ID).String(), "error", err)
			writeError(w, http.StatusInternalServerError, errors.New("list runs"))
			return
		}
		response.Runs = append(response.Runs, item)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) countRuns(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	actor := actorFromContext(r.Context())
	counts, err := s.countRunStatuses(r, actor)
	if errors.Is(err, errPermissionRequired) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	if isScopeRequestError(err) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err != nil {
		s.log.Error("count runs failed", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("count runs"))
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

func (s *Server) getRunLogs(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	runID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	run, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{OrgID: ids.ToPG(actor.OrgID), ID: ids.ToPG(runID)})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("run not found"))
		return
	} else if err != nil {
		s.log.Error("get run before logs failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get run logs"))
		return
	}
	scope, err := s.runScope(r.Context(), actor.OrgID, getRunSummary(run))
	if err != nil {
		s.log.Error("resolve run scope before logs failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get run logs"))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	logs, err := s.db.GetRunLogSnapshot(r.Context(), db.GetRunLogSnapshotParams{
		StdoutLimit: maxRunLogSnapshotBytes,
		StderrLimit: maxRunLogSnapshotBytes,
		OrgID:       ids.ToPG(actor.OrgID),
		RunID:       ids.ToPG(runID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, api.LogSnapshotResponse{Cursor: "0:0"})
		return
	}
	if err != nil {
		s.log.Error("get run logs failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get run logs"))
		return
	}
	writeJSON(w, http.StatusOK, api.LogSnapshotResponse{
		StdoutBase64: base64.StdEncoding.EncodeToString(logs.Stdout),
		StderrBase64: base64.StdEncoding.EncodeToString(logs.Stderr),
		Cursor:       fmt.Sprintf("%d:%d", logs.StdoutCursor, logs.StderrCursor),
		Truncated:    logs.Truncated.Bool,
	})
}

func (s *Server) getRunEvents(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	runID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cursor, err := eventCursor(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	limit, err := eventLimit(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	run, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{OrgID: ids.ToPG(actor.OrgID), ID: ids.ToPG(runID)})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("run not found"))
		return
	} else if err != nil {
		s.log.Error("get run before events failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("list run events"))
		return
	}
	scope, err := s.runScope(r.Context(), actor.OrgID, getRunSummary(run))
	if err != nil {
		s.log.Error("resolve run scope before events failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("list run events"))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	if r.URL.Query().Get("follow") == "1" || strings.Contains(r.Header.Get("accept"), "text/event-stream") {
		s.followRunEvents(w, r, ids.ToPG(actor.OrgID), ids.ToPG(runID), cursor)
		return
	}
	rows, err := s.listRunEvents(r, ids.ToPG(actor.OrgID), ids.ToPG(runID), cursor, limit)
	if err != nil {
		s.log.Error("list run events failed", "run_id", runID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("list run events"))
		return
	}
	hasNext := len(rows) > int(limit)
	if hasNext {
		rows = rows[:limit]
	}
	events := make([]api.RunEvent, 0, len(rows))
	for _, row := range rows {
		events = append(events, runEventResponse(row))
	}
	var nextCursor *int64
	if hasNext {
		value := rows[len(rows)-1].ID
		nextCursor = &value
	}
	writeJSON(w, http.StatusOK, api.RunEventPage{Events: events, Cursor: cursor, NextCursor: nextCursor})
}

func (s *Server) listRunEvents(r *http.Request, orgID pgtype.UUID, runID pgtype.UUID, cursor int64, limit int32) ([]db.RunEvent, error) {
	return s.db.ListRunEvents(r.Context(), db.ListRunEventsParams{
		OrgID: orgID,
		RunID: runID,
		ID:    cursor,
		Limit: limit + 1,
	})
}

func (s *Server) listRunSummaries(r *http.Request, actor auth.Actor, statusFilter string, limit int32) ([]runSummary, error) {
	requestedScope, scopedQuery, err := s.requestedRunListScope(r, actor)
	if err != nil {
		return nil, err
	}
	if !actor.HasPermission(auth.PermissionRunsRead, requestedScope) {
		return nil, errPermissionRequired
	}
	if scopedQuery {
		projectID, environmentID, err := s.runScopeIDs(r.Context(), actor.OrgID, requestedScope)
		if err != nil {
			return nil, err
		}
		rows, err := s.db.ListScopedRunSummaries(r.Context(), db.ListScopedRunSummariesParams{
			OrgID:         ids.ToPG(actor.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			StatusFilter:  statusFilter,
			RowLimit:      limit,
		})
		if err != nil {
			return nil, err
		}
		summaries := make([]runSummary, 0, len(rows))
		for _, row := range rows {
			summaries = append(summaries, listScopedRunSummary(row))
		}
		return summaries, nil
	}
	rows, err := s.db.ListRunSummaries(r.Context(), db.ListRunSummariesParams{
		OrgID:        ids.ToPG(actor.OrgID),
		StatusFilter: statusFilter,
		RowLimit:     limit,
	})
	if err != nil {
		return nil, err
	}
	summaries := make([]runSummary, 0, len(rows))
	for _, row := range rows {
		summaries = append(summaries, listRunSummary(row))
	}
	return summaries, nil
}

func (s *Server) countRunStatuses(r *http.Request, actor auth.Actor) (api.RunCountsResponse, error) {
	requestedScope, scopedQuery, err := s.requestedRunListScope(r, actor)
	if err != nil {
		return api.RunCountsResponse{}, err
	}
	if !actor.HasPermission(auth.PermissionRunsRead, requestedScope) {
		return api.RunCountsResponse{}, errPermissionRequired
	}
	if scopedQuery {
		projectID, environmentID, err := s.runScopeIDs(r.Context(), actor.OrgID, requestedScope)
		if err != nil {
			return api.RunCountsResponse{}, err
		}
		counts, err := s.db.CountScopedRunsByStatus(r.Context(), db.CountScopedRunsByStatusParams{
			OrgID:         ids.ToPG(actor.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
		})
		if err != nil {
			return api.RunCountsResponse{}, err
		}
		return scopedRunCountsResponse(counts), nil
	}
	counts, err := s.db.CountRunsByStatus(r.Context(), ids.ToPG(actor.OrgID))
	if err != nil {
		return api.RunCountsResponse{}, err
	}
	return runCountsResponse(counts), nil
}

var errPermissionRequired = errors.New("permission is required")

func isScopeRequestError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "project_id") || strings.Contains(message, "environment_id")
}

func (s *Server) requestedRunListScope(r *http.Request, actor auth.Actor) (auth.Scope, bool, error) {
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	environmentID := strings.TrimSpace(r.URL.Query().Get("environment_id"))
	if projectID == "" && environmentID == "" {
		if actor.Kind == auth.ActorKindAPIKey {
			return auth.DefaultScope(actor.OrgID), true, nil
		}
		return auth.DefaultScope(actor.OrgID), false, nil
	}
	if projectID == "" || environmentID == "" {
		return auth.Scope{}, false, errors.New("project_id and environment_id must be provided together")
	}
	if projectID == auth.DefaultProjectID && environmentID == auth.DefaultEnvironmentID {
		return auth.DefaultScope(actor.OrgID), true, nil
	}
	scope, _, _, err := s.normalizeProjectEnvironmentScope(r.Context(), actor.OrgID, projectID, environmentID)
	if err != nil {
		return auth.Scope{}, false, err
	}
	return scope, true, nil
}

func (s *Server) runScopeIDs(ctx context.Context, orgID uuid.UUID, scope auth.Scope) (pgtype.UUID, pgtype.UUID, error) {
	if scope.ProjectID == auth.DefaultProjectID && scope.EnvironmentID == auth.DefaultEnvironmentID {
		defaultScope, err := s.db.GetDefaultProjectEnvironment(ctx, ids.ToPG(orgID))
		if err != nil {
			return pgtype.UUID{}, pgtype.UUID{}, err
		}
		return defaultScope.ProjectID, defaultScope.EnvironmentID, nil
	}
	projectID, err := ids.Parse(scope.ProjectID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	environmentID, err := ids.Parse(scope.EnvironmentID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	return ids.ToPG(projectID), ids.ToPG(environmentID), nil
}

func (s *Server) normalizeProjectEnvironmentScope(ctx context.Context, orgID uuid.UUID, projectID string, environmentID string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	if projectID == auth.DefaultProjectID && environmentID == auth.DefaultEnvironmentID {
		return auth.DefaultScope(orgID), pgtype.UUID{}, pgtype.UUID{}, nil
	}
	project, err := s.resolveProjectRef(ctx, orgID, projectID)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	environment, err := s.resolveEnvironmentRef(ctx, orgID, project.ID, environmentID)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	defaultScope, err := s.db.GetDefaultProjectEnvironment(ctx, ids.ToPG(orgID))
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("load default scope: %w", err)
	}
	if project.ID == defaultScope.ProjectID && environment.ID == defaultScope.EnvironmentID {
		return auth.DefaultScope(orgID), pgtype.UUID{}, pgtype.UUID{}, nil
	}
	return auth.Scope{OrgID: orgID, ProjectID: ids.MustFromPG(project.ID).String(), EnvironmentID: ids.MustFromPG(environment.ID).String()}, project.ID, environment.ID, nil
}

func (s *Server) resolveProjectRef(ctx context.Context, orgID uuid.UUID, projectRef string) (db.Project, error) {
	projectRef = strings.TrimSpace(projectRef)
	if projectRef == "" {
		projectRef = auth.DefaultProjectID
	}
	if projectRef == auth.DefaultProjectID {
		defaultScope, err := s.db.GetDefaultProjectEnvironment(ctx, ids.ToPG(orgID))
		if err != nil {
			return db.Project{}, fmt.Errorf("load default scope: %w", err)
		}
		return s.db.GetProject(ctx, db.GetProjectParams{OrgID: ids.ToPG(orgID), ID: defaultScope.ProjectID})
	}
	if parsed, err := ids.Parse(projectRef); err == nil {
		project, err := s.db.GetProject(ctx, db.GetProjectParams{OrgID: ids.ToPG(orgID), ID: ids.ToPG(parsed)})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Project{}, errors.New("project_id must reference an active project")
		}
		if err != nil {
			return db.Project{}, fmt.Errorf("load project: %w", err)
		}
		return project, nil
	}
	project, err := s.db.GetProjectBySlug(ctx, db.GetProjectBySlugParams{OrgID: ids.ToPG(orgID), Slug: strings.ToLower(projectRef)})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Project{}, errors.New("project_id must be \"default\", a project UUID, or a project slug")
	}
	if err != nil {
		return db.Project{}, fmt.Errorf("load project: %w", err)
	}
	return project, nil
}

func (s *Server) resolveEnvironmentRef(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentRef string) (db.Environment, error) {
	environmentRef = strings.TrimSpace(environmentRef)
	if environmentRef == "" {
		environmentRef = auth.DefaultEnvironmentID
	}
	if environmentRef == auth.DefaultEnvironmentID {
		environment, err := s.db.GetDefaultEnvironment(ctx, db.GetDefaultEnvironmentParams{OrgID: ids.ToPG(orgID), ProjectID: projectID})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Environment{}, errors.New("environment_id must reference an active environment")
		}
		if err != nil {
			return db.Environment{}, fmt.Errorf("load environment: %w", err)
		}
		return environment, nil
	}
	if parsed, err := ids.Parse(environmentRef); err == nil {
		environment, err := s.db.GetEnvironment(ctx, db.GetEnvironmentParams{OrgID: ids.ToPG(orgID), ProjectID: projectID, ID: ids.ToPG(parsed)})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Environment{}, errors.New("environment_id must reference an active environment")
		}
		if err != nil {
			return db.Environment{}, fmt.Errorf("load environment: %w", err)
		}
		return environment, nil
	}
	environment, err := s.db.GetEnvironmentBySlug(ctx, db.GetEnvironmentBySlugParams{OrgID: ids.ToPG(orgID), ProjectID: projectID, Slug: strings.ToLower(environmentRef)})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Environment{}, errors.New("environment_id must be \"default\", an environment UUID, or an environment slug")
	}
	if err != nil {
		return db.Environment{}, fmt.Errorf("load environment: %w", err)
	}
	return environment, nil
}

func eventCursor(r *http.Request) (int64, error) {
	value := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if value == "" {
		value = strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	}
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, errors.New("cursor must be a non-negative integer")
	}
	return parsed, nil
}

func eventLimit(r *http.Request) (int32, error) {
	value := strings.TrimSpace(r.URL.Query().Get("limit"))
	if value == "" {
		return runEventsPageSize, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < 1 || parsed > int64(runEventsPageSize) {
		return 0, fmt.Errorf("limit must be an integer between 1 and %d", runEventsPageSize)
	}
	return int32(parsed), nil
}

func (s *Server) followRunEvents(w http.ResponseWriter, r *http.Request, orgID pgtype.UUID, runID pgtype.UUID, cursor int64) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	var events <-chan struct{} = make(chan struct{})
	unsubscribe := func() {}
	if s.runEvents != nil {
		events, unsubscribe = s.runEvents.SubscribeRunEvents(r.Context(), runID)
		defer unsubscribe()
	}
	fallback := time.NewTicker(runEventsFollowFallbackEvery)
	defer fallback.Stop()
	deadline := time.NewTimer(runEventsFollowMaxDuration)
	defer deadline.Stop()
	for {
		rows, err := s.listRunEvents(r, orgID, runID, cursor, runEventsPageSize)
		if err != nil {
			s.log.Warn("follow run events failed", "error", err)
			return
		}
		terminal := false
		for _, row := range rows {
			event := runEventResponse(row)
			cursor = row.ID
			terminal = terminal || runEventKindIsTerminal(row.Kind)
			_, _ = fmt.Fprintf(w, "id: %s\n", event.ID)
			_, _ = fmt.Fprint(w, "event: run_event\n")
			_, _ = fmt.Fprint(w, "data: ")
			_ = encoder.Encode(event)
			_, _ = fmt.Fprint(w, "\n")
		}
		if flusher != nil {
			flusher.Flush()
		}
		if terminal {
			return
		}
		if len(rows) == int(runEventsPageSize) {
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return
		case <-events:
		case <-fallback.C:
			_, _ = fmt.Fprint(w, ": keep-alive\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func runEventKindIsTerminal(kind string) bool {
	switch kind {
	case "run.completed", "run.failed", "run.cancelled":
		return true
	default:
		return false
	}
}

func runEventResponse(event db.RunEvent) api.RunEvent {
	runID := ids.MustFromPG(event.RunID).String()
	kind := "execution"
	if strings.HasPrefix(event.Kind, "emit.") {
		kind = "emit"
	}
	attributes := json.RawMessage(event.Payload)
	if len(attributes) == 0 || !json.Valid(attributes) {
		attributes = json.RawMessage(`{}`)
	}
	return api.RunEvent{
		ID:         strconv.FormatInt(event.ID, 10),
		RunID:      &runID,
		Kind:       kind,
		Message:    event.Kind,
		At:         pgTime(event.CreatedAt),
		Attributes: attributes,
	}
}

func listRunsQuery(r *http.Request) (string, int32, error) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		status = "live"
	}
	switch status {
	case "all", "live", "queued", "running", "waiting", "succeeded", "failed", "cancelled":
	default:
		return "", 0, fmt.Errorf("status must be live, all, or a run status")
	}
	limit := int32(100)
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > 200 {
			return "", 0, errors.New("limit must be an integer between 1 and 200")
		}
		limit = int32(parsed)
	}
	return status, limit, nil
}

func parseUUIDParam(r *http.Request, name string) (uuid.UUID, error) {
	id, err := ids.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, fmt.Errorf("%s must be a UUID", name)
	}
	return id, nil
}

func runMaxDurationSeconds(value int32, defaultValue int32) (int32, error) {
	if value == 0 {
		value = defaultValue
	}
	if value == 0 {
		value = defaultRunMaxDurationSeconds
	}
	if value < minRunMaxDurationSeconds {
		return 0, fmt.Errorf("max_duration_seconds must be >= %d", minRunMaxDurationSeconds)
	}
	if value > maxRunDurationSeconds {
		return 0, fmt.Errorf("max_duration_seconds must be <= %d", maxRunDurationSeconds)
	}
	return value, nil
}

type runSummary struct {
	ID               pgtype.UUID
	OrgID            pgtype.UUID
	ProjectID        pgtype.UUID
	EnvironmentID    pgtype.UUID
	DeploymentID     pgtype.UUID
	DeploymentTaskID pgtype.UUID
	TaskID           string
	Status           db.RunStatus
	ExitCode         pgtype.Int4
	Output           []byte
	CreatedAt        pgtype.Timestamptz
	UpdatedAt        pgtype.Timestamptz
}

func createScopedRunSummary(run db.CreateScopedRunRow) runSummary {
	return runSummary{
		ID:               run.ID,
		OrgID:            run.OrgID,
		ProjectID:        run.ProjectID,
		EnvironmentID:    run.EnvironmentID,
		DeploymentID:     run.DeploymentID,
		DeploymentTaskID: run.DeploymentTaskID,
		TaskID:           run.TaskID,
		Status:           run.Status,
		ExitCode:         run.ExitCode,
		Output:           run.Output,
		CreatedAt:        run.CreatedAt,
		UpdatedAt:        run.UpdatedAt,
	}
}

func getRunSummary(run db.GetRunSummaryRow) runSummary {
	return runSummary{
		ID:               run.ID,
		OrgID:            run.OrgID,
		ProjectID:        run.ProjectID,
		EnvironmentID:    run.EnvironmentID,
		DeploymentID:     run.DeploymentID,
		DeploymentTaskID: run.DeploymentTaskID,
		TaskID:           run.TaskID,
		Status:           run.Status,
		ExitCode:         run.ExitCode,
		Output:           run.Output,
		CreatedAt:        run.CreatedAt,
		UpdatedAt:        run.UpdatedAt,
	}
}

func listRunSummary(run db.ListRunSummariesRow) runSummary {
	return runSummary{
		ID:               run.ID,
		OrgID:            run.OrgID,
		ProjectID:        run.ProjectID,
		EnvironmentID:    run.EnvironmentID,
		DeploymentID:     run.DeploymentID,
		DeploymentTaskID: run.DeploymentTaskID,
		TaskID:           run.TaskID,
		Status:           run.Status,
		ExitCode:         run.ExitCode,
		Output:           run.Output,
		CreatedAt:        run.CreatedAt,
		UpdatedAt:        run.UpdatedAt,
	}
}

func listScopedRunSummary(run db.ListScopedRunSummariesRow) runSummary {
	return runSummary{
		ID:               run.ID,
		OrgID:            run.OrgID,
		ProjectID:        run.ProjectID,
		EnvironmentID:    run.EnvironmentID,
		DeploymentID:     run.DeploymentID,
		DeploymentTaskID: run.DeploymentTaskID,
		TaskID:           run.TaskID,
		Status:           run.Status,
		ExitCode:         run.ExitCode,
		Output:           run.Output,
		CreatedAt:        run.CreatedAt,
		UpdatedAt:        run.UpdatedAt,
	}
}

func runCountsResponse(counts db.CountRunsByStatusRow) api.RunCountsResponse {
	return api.RunCountsResponse{
		Queued:    counts.Queued,
		Running:   counts.Running,
		Waiting:   counts.Waiting,
		Succeeded: counts.Succeeded,
		Failed:    counts.Failed,
		Cancelled: counts.Cancelled,
	}
}

func scopedRunCountsResponse(counts db.CountScopedRunsByStatusRow) api.RunCountsResponse {
	return api.RunCountsResponse{
		Queued:    counts.Queued,
		Running:   counts.Running,
		Waiting:   counts.Waiting,
		Succeeded: counts.Succeeded,
		Failed:    counts.Failed,
		Cancelled: counts.Cancelled,
	}
}

func runResponse(run runSummary) api.RunResponse {
	runID := ids.MustFromPG(run.ID)
	var exitCode *int32
	if run.ExitCode.Valid {
		exitCode = &run.ExitCode.Int32
	}
	var output json.RawMessage
	if len(run.Output) > 0 {
		output = append(json.RawMessage(nil), run.Output...)
	}
	return api.RunResponse{
		ID:               runID.String(),
		ProjectID:        apiKeyScopeID(run.ProjectID, auth.DefaultProjectID),
		EnvironmentID:    apiKeyScopeID(run.EnvironmentID, auth.DefaultEnvironmentID),
		DeploymentID:     ids.MustFromPG(run.DeploymentID).String(),
		DeploymentTaskID: ids.MustFromPG(run.DeploymentTaskID).String(),
		TaskID:           run.TaskID,
		Status:           publicRunStatus(run.Status),
		ExitCode:         exitCode,
		Output:           output,
		CreatedAt:        pgTime(run.CreatedAt),
		UpdatedAt:        pgTime(run.UpdatedAt),
	}
}

func publicRunStatus(status db.RunStatus) string {
	return string(status)
}

func (s *Server) runScope(ctx context.Context, orgID uuid.UUID, run runSummary) (auth.Scope, error) {
	scope := auth.Scope{
		OrgID:         orgID,
		ProjectID:     apiKeyScopeID(run.ProjectID, auth.DefaultProjectID),
		EnvironmentID: apiKeyScopeID(run.EnvironmentID, auth.DefaultEnvironmentID),
	}
	defaultScope, err := s.db.GetDefaultProjectEnvironment(ctx, ids.ToPG(orgID))
	if err != nil {
		return auth.Scope{}, err
	}
	if run.ProjectID == defaultScope.ProjectID && run.EnvironmentID == defaultScope.EnvironmentID {
		return auth.DefaultScope(orgID), nil
	}
	return scope, nil
}

func (s *Server) runResponse(ctx context.Context, run runSummary) (api.RunResponse, error) {
	response := runResponse(run)
	if run.Status != db.RunStatusWaiting {
		return response, nil
	}
	waitpoint, err := s.db.GetPendingWaitpointForRun(ctx, db.GetPendingWaitpointForRunParams{
		OrgID: run.OrgID,
		RunID: run.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return response, nil
	}
	if err != nil {
		return api.RunResponse{}, err
	}
	pending, err := pendingWaitResponse(waitpoint)
	if err != nil {
		return api.RunResponse{}, err
	}
	deliveries, err := s.db.ListWaitpointDeliveries(ctx, db.ListWaitpointDeliveriesParams{
		OrgID:       waitpoint.OrgID,
		RunID:       waitpoint.RunID,
		WaitpointID: waitpoint.ID,
	})
	if err != nil {
		return api.RunResponse{}, err
	}
	pending.Deliveries = make([]api.WaitpointDeliveryResponse, 0, len(deliveries))
	for _, delivery := range deliveries {
		pending.Deliveries = append(pending.Deliveries, waitpointDeliveryResponse(delivery))
	}
	response.PendingWait = &pending
	return response, nil
}

func pendingWaitResponse(waitpoint db.Waitpoint) (api.PendingWait, error) {
	response := api.PendingWait{
		Kind:        string(waitpoint.Kind),
		WaitpointID: ids.MustFromPG(waitpoint.ID).String(),
		RequestedAt: pgTime(waitpoint.RequestedAt),
	}
	if waitpoint.TimeoutSeconds.Valid {
		response.Timeout = &waitpoint.TimeoutSeconds.Int32
	}
	if waitpoint.PolicyName.Valid {
		policy := waitpoint.PolicyName.String
		response.Policy = &policy
	}
	switch waitpoint.Kind {
	case db.WaitpointKindApproval:
		message := waitpoint.DisplayText
		response.Message = &message
	case db.WaitpointKindMessage:
		prompt := waitpoint.DisplayText
		response.Prompt = &prompt
	default:
		return api.PendingWait{}, fmt.Errorf("unsupported waitpoint kind %q", waitpoint.Kind)
	}
	return response, nil
}

func waitpointDeliveryResponse(delivery db.WaitpointDelivery) api.WaitpointDeliveryResponse {
	var lastError *string
	if delivery.LastError.Valid {
		lastError = &delivery.LastError.String
	}
	var sentAt *time.Time
	if delivery.SentAt.Valid {
		value := pgTime(delivery.SentAt)
		sentAt = &value
	}
	return api.WaitpointDeliveryResponse{
		ID:            ids.MustFromPG(delivery.ID).String(),
		Channel:       delivery.Channel,
		RecipientKind: delivery.RecipientKind,
		Recipient:     delivery.Recipient,
		Status:        string(delivery.Status),
		LastError:     lastError,
		SentAt:        sentAt,
		CreatedAt:     pgTime(delivery.CreatedAt),
		UpdatedAt:     pgTime(delivery.UpdatedAt),
	}
}

func pgTime(value pgtype.Timestamptz) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time
}

func pgTimeToPG(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
