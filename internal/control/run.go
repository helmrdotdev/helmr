package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultRunMaxDurationSeconds  = int32(900)
	minRunMaxDurationSeconds      = int32(5)
	maxRunDurationSeconds         = int32(86400)
	defaultIdempotencyKeyTTL      = 30 * 24 * time.Hour
	maxIdempotencyKeyLength       = 512
	maxRunLogSnapshotBytes        = int64(1 << 20)
	runLogStreamBatchSize         = int32(100)
	runLogStreamPollInterval      = time.Second
	runLogStreamFollowMaxDuration = 30 * time.Minute
	runEventsPageSize             = int32(200)
	runEventsFollowMaxDuration    = 30 * time.Minute
)

func isCreateRunConfigError(err error) bool {
	message := err.Error()
	return strings.Contains(message, "storage is not configured") ||
		strings.Contains(message, "resolver is not configured") ||
		strings.Contains(message, "secret store is not configured")
}

func isCreateRunClientError(err error) bool {
	message := err.Error()
	return isNoRows(err) ||
		strings.Contains(message, "must be") ||
		strings.Contains(message, "must match") ||
		strings.Contains(message, "cannot be") ||
		strings.Contains(message, "not accepted") ||
		strings.Contains(message, "not bound") ||
		strings.Contains(message, "invalid") ||
		strings.Contains(message, "unsupported") ||
		strings.Contains(message, "exactly one") ||
		strings.Contains(message, "not deployed") ||
		strings.Contains(message, "not found") ||
		strings.Contains(message, "not enabled") ||
		strings.Contains(message, "not declared")
}

type createRunUpstreamError struct {
	err error
}

func (e createRunUpstreamError) Error() string {
	return e.err.Error()
}

func (e createRunUpstreamError) Unwrap() error {
	return e.err
}

func (s *Server) CreateScheduleRun(ctx context.Context, row db.GetScheduleTriggerCandidateRow) (pgtype.UUID, error) {
	request, err := schedule.RunRequestFromTriggerCandidate(row)
	if err != nil {
		return pgtype.UUID{}, err
	}
	startRequest := api.TaskStartRequest{
		ProjectID:     request.ProjectID,
		EnvironmentID: request.EnvironmentID,
		Payload:       request.Payload,
		Options: api.TaskStartOptions{
			Queue:              request.Options.Queue,
			ConcurrencyKey:     request.Options.ConcurrencyKey,
			Priority:           request.Options.Priority,
			TTL:                request.Options.TTL,
			MaxDurationSeconds: request.Options.MaxDurationSeconds,
			Retry:              request.Options.Retry,
			Metadata:           request.Options.Metadata,
			Tags:               request.Options.Tags,
			IdempotencyKey:     schedule.TriggerIdempotencyKey(row.InstanceID, row.Generation, row.NextFireAt),
			IdempotencyKeyTTL:  schedule.TriggerIdempotencyKeyTTL,
		},
	}
	orgID, err := pgvalue.UUIDValue(row.OrgID)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("schedule trigger org id is invalid: %v", err)
	}
	started, err := s.startTaskSessionFromRequest(ctx, auth.Actor{
		OrgID: orgID,
		Kind:  auth.ActorKindSystem,
		Role:  auth.RoleOwner,
	}, request.TaskID, startRequest, taskStartSource{
		scheduleID:            row.ScheduleID,
		scheduleInstanceID:    row.InstanceID,
		scheduleGeneration:    row.Generation,
		scheduleOrgID:         row.OrgID,
		scheduleProjectID:     row.ProjectID,
		scheduleEnvironmentID: row.EnvironmentID,
		scheduledAt:           row.NextFireAt,
	})
	if err != nil {
		if errors.Is(err, errTaskStartPending) || errors.Is(err, errTaskStartCoordinationUnavailable) {
			return pgtype.UUID{}, fmt.Errorf("%w: %w", schedule.ErrTriggerDeferred, err)
		}
		return pgtype.UUID{}, err
	}
	return started.run.ID, nil
}

type runDeploymentSelection struct {
	deploymentID pgtype.UUID
	version      string
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
			OrgID:         pgvalue.UUID(orgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ID:            deploymentID,
		})
		if isNoRows(err) {
			return db.GetDeploymentTaskRow{}, runDeploymentSelectionErrorf("deployment_id %s was not found in this environment", pgvalue.MustUUIDValue(deploymentID).String())
		}
		if err != nil {
			return db.GetDeploymentTaskRow{}, err
		}
		if deployment.Status != db.DeploymentStatusDeployed {
			return db.GetDeploymentTaskRow{}, runDeploymentSelectionErrorf("deployment_id %s is not deployed", pgvalue.MustUUIDValue(deploymentID).String())
		}
		return s.deploymentTask(ctx, orgID, projectID, environmentID, deployment.ID, taskID)
	}
	if selection.version != "" {
		deployment, err := s.db.GetDeploymentByVersion(ctx, db.GetDeploymentByVersionParams{
			OrgID:         pgvalue.UUID(orgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			Version:       selection.version,
		})
		if isNoRows(err) {
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
		OrgID:         pgvalue.UUID(orgID),
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
		ID:                                task.ID,
		OrgID:                             task.OrgID,
		ProjectID:                         task.ProjectID,
		EnvironmentID:                     task.EnvironmentID,
		DeploymentID:                      task.DeploymentID,
		DeploymentSandboxID:               task.DeploymentSandboxID,
		SandboxID:                         task.SandboxID,
		SandboxFingerprint:                task.SandboxFingerprint,
		WorkspaceMountPath:                task.WorkspaceMountPath,
		DeploymentSandboxResourceFloor:    task.DeploymentSandboxResourceFloor,
		DeploymentSandboxDiskFloorMib:     task.DeploymentSandboxDiskFloorMib,
		DeploymentSandboxNetworkPolicy:    task.DeploymentSandboxNetworkPolicy,
		DeploymentVersion:                 task.DeploymentVersion,
		ApiVersion:                        task.ApiVersion,
		SdkVersion:                        task.SdkVersion,
		CliVersion:                        task.CliVersion,
		TaskID:                            task.TaskID,
		FilePath:                          task.FilePath,
		ExportName:                        task.ExportName,
		HandlerEntrypoint:                 task.HandlerEntrypoint,
		BundleDigest:                      task.BundleDigest,
		BundleFormatVersion:               task.BundleFormatVersion,
		RequestedMilliCpu:                 task.RequestedMilliCpu,
		RequestedMemoryMib:                task.RequestedMemoryMib,
		RequestedDiskMib:                  task.RequestedDiskMib,
		SecretDeclarations:                task.SecretDeclarations,
		ResourceRequirements:              task.ResourceRequirements,
		NetworkPolicy:                     task.NetworkPolicy,
		QueueName:                         task.QueueName,
		QueueConcurrencyLimit:             task.QueueConcurrencyLimit,
		Ttl:                               task.Ttl,
		MaxDurationSeconds:                task.MaxDurationSeconds,
		RetryPolicy:                       task.RetryPolicy,
		CreatedAt:                         task.CreatedAt,
		DeploymentSourceDigest:            task.DeploymentSourceDigest,
		DeploymentSandboxRootfsDigest:     task.DeploymentSandboxRootfsDigest,
		DeploymentSandboxRuntimeAbi:       task.DeploymentSandboxRuntimeAbi,
		DeploymentSandboxGuestdAbi:        task.DeploymentSandboxGuestdAbi,
		DeploymentSandboxAdapterAbi:       task.DeploymentSandboxAdapterAbi,
		DeploymentSandboxFilesystemFormat: task.DeploymentSandboxFilesystemFormat,
		DeploymentSandboxContractVersion:  task.DeploymentSandboxContractVersion,
	}
}

func (s *Server) deploymentTask(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, deploymentID pgtype.UUID, taskID string) (db.GetDeploymentTaskRow, error) {
	return s.db.GetDeploymentTask(ctx, db.GetDeploymentTaskParams{
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		DeploymentID:  deploymentID,
		TaskID:        taskID,
	})
}

func runCreatedEventPayload(taskID string, payload json.RawMessage, maxDurationSeconds int32, secretNames []string, retryPolicy []byte, metadata []byte, tags []string) ([]byte, error) {
	secretNames = append([]string{}, secretNames...)
	sort.Strings(secretNames)
	tags = append([]string{}, tags...)
	return json.Marshal(runCreatedPayload{
		TaskID:             taskID,
		Payload:            payload,
		MaxDurationSeconds: maxDurationSeconds,
		SecretNames:        secretNames,
		RetryPolicy:        json.RawMessage(retryPolicy),
		Metadata:           json.RawMessage(metadata),
		Tags:               tags,
	})
}

type runCreatedPayload struct {
	MaxDurationSeconds int32           `json:"max_duration_seconds"`
	Metadata           json.RawMessage `json:"metadata"`
	Payload            json.RawMessage `json:"payload"`
	RetryPolicy        json.RawMessage `json:"retry_policy"`
	SecretNames        []string        `json:"secret_names"`
	Tags               []string        `json:"tags"`
	TaskID             string          `json:"task_id"`
}

func deploymentTaskSecretNames(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var declarations []api.SecretDeclaration
	if err := json.Unmarshal(raw, &declarations); err != nil {
		return nil, fmt.Errorf("decode deployment task secret declarations: %w", err)
	}
	names := make([]string, 0, len(declarations))
	seen := map[string]struct{}{}
	for _, declaration := range declarations {
		name := strings.TrimSpace(declaration.Name)
		if err := secret.ValidateName(name); err != nil {
			return nil, fmt.Errorf("deployment task secret declaration name: %w", err)
		}
		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("deployment task has duplicate secret declaration %q", name)
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	runID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	run, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{
		OrgID: pgvalue.UUID(actorFromContext(r.Context()).OrgID),
		ID:    pgvalue.UUID(runID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("run not found")))
		return
	}
	if err != nil {
		s.log.Error("get run failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("get run"))
		return
	}
	summary := getRunSummary(run)
	actor := actorFromContext(r.Context())
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     pgvalue.MustUUIDValue(summary.ProjectID).String(),
		EnvironmentID: pgvalue.MustUUIDValue(summary.EnvironmentID).String(),
	}
	if err := s.requireActorScopeForRecord(r, actor, summary.ProjectID, summary.EnvironmentID); err != nil {
		if isNoRows(err) {
			writeError(w, notFound(errors.New("run not found")))
			return
		}
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	response, err := s.runResponse(r.Context(), summary)
	if err != nil {
		s.log.Error("get pending waitpoint failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("get run"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	statusFilter, limit, err := listRunsQuery(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	summaries, err := s.listRunSummaries(r, actor, statusFilter, limit)
	if errors.Is(err, errPermissionRequired) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	if isScopeRequestError(err) || isListRunsRequestError(err) {
		writeError(w, badRequest(err))
		return
	}
	if err != nil {
		s.log.Error("list runs failed", "error", err)
		writeError(w, errors.New("list runs"))
		return
	}
	runs, err := s.runResponses(r.Context(), pgvalue.UUID(actor.OrgID), summaries)
	if err != nil {
		s.log.Error("list pending waitpoints failed", "error", err)
		writeError(w, errors.New("list runs"))
		return
	}
	writeJSON(w, http.StatusOK, api.ListRunsResponse{Runs: runs})
}

func (s *Server) countRuns(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	counts, err := s.countRunStatuses(r, actor)
	if errors.Is(err, errPermissionRequired) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	if isScopeRequestError(err) {
		writeError(w, badRequest(err))
		return
	}
	if err != nil {
		s.log.Error("count runs failed", "error", err)
		writeError(w, errors.New("count runs"))
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

func (s *Server) listRunSummaries(r *http.Request, actor auth.Actor, statusFilter string, limit int32) ([]runSummary, error) {
	requestedScope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		return nil, err
	}
	if !actor.HasPermission(auth.PermissionRunsRead, requestedScope) {
		return nil, errPermissionRequired
	}
	projectID, environmentID, err := runScopeIDs(requestedScope)
	if err != nil {
		return nil, err
	}
	taskSessionID, err := optionalRunSessionIDFilter(r)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.ListScopedRunSummaries(r.Context(), db.ListScopedRunSummariesParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		StatusFilter:  statusFilter,
		TaskSessionID: taskSessionID,
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

func (s *Server) countRunStatuses(r *http.Request, actor auth.Actor) (api.RunCountsResponse, error) {
	requestedScope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		return api.RunCountsResponse{}, err
	}
	if !actor.HasPermission(auth.PermissionRunsRead, requestedScope) {
		return api.RunCountsResponse{}, errPermissionRequired
	}
	projectID, environmentID, err := runScopeIDs(requestedScope)
	if err != nil {
		return api.RunCountsResponse{}, err
	}
	counts, err := s.db.CountScopedRunsByStatus(r.Context(), db.CountScopedRunsByStatusParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
	})
	if err != nil {
		return api.RunCountsResponse{}, err
	}
	return scopedRunCountsResponse(counts), nil
}

func optionalRunSessionIDFilter(r *http.Request) (pgtype.UUID, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("session_id"))
	if raw == "" {
		return pgtype.UUID{}, nil
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return pgtype.UUID{}, errInvalidRunSessionID
	}
	return pgvalue.UUID(parsed), nil
}

var errPermissionRequired = errors.New("permission is required")
var errInvalidRunSessionID = errors.New("session_id must be a UUID")

func isListRunsRequestError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, errInvalidRunSessionID)
}

func listRunsQuery(r *http.Request) (string, int32, error) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		status = "live"
	}
	switch status {
	case "all", "live", "queued", "running", "waiting", "succeeded", "failed", "cancelled", "expired":
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

func normalizedJSONObject(raw json.RawMessage, label string) ([]byte, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return []byte("{}"), nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%s must be valid JSON", label)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%s decode failed: %w", label, err)
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, fmt.Errorf("%s must be a JSON object", label)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%s canonicalization failed: %w", label, err)
	}
	return canonical, nil
}

func normalizedRunTags(tags []string) ([]string, error) {
	if len(tags) == 0 {
		return []string{}, nil
	}
	if len(tags) > 10 {
		return nil, errors.New("tags must contain at most 10 items")
	}
	out := make([]string, 0, len(tags))
	seen := map[string]struct{}{}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return nil, errors.New("tags must not contain empty values")
		}
		if len(tag) > 128 {
			return nil, errors.New("tags must be 128 characters or less")
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	sort.Strings(out)
	return out, nil
}

type runScheduling struct {
	queueName             string
	queueConcurrencyLimit pgtype.Int4
	concurrencyKey        pgtype.Text
	priority              int32
	queueTimestamp        pgtype.Timestamptz
	ttl                   string
	queuedExpiresAt       pgtype.Timestamptz
}

func (s *Server) resolveRunScheduling(options api.CreateRunOptions, task db.GetDeploymentTaskRow) (runScheduling, error) {
	now := time.Now().UTC()
	queueName := strings.TrimSpace(task.QueueName)
	queueLimit := task.QueueConcurrencyLimit
	if queueName == "" {
		queueName = "task/" + task.TaskID
	}
	if options.Queue != nil {
		queueName = strings.TrimSpace(options.Queue.Name)
		if queueName == "" {
			return runScheduling{}, errors.New("queue.name is required")
		}
		if err := api.ValidateQueueName(queueName); err != nil {
			return runScheduling{}, err
		}
	} else if err := api.ValidateQueueName(queueName); err != nil {
		return runScheduling{}, err
	}

	concurrencyKey := pgtype.Text{}
	if key := strings.TrimSpace(options.ConcurrencyKey); key != "" {
		if len(key) > 512 {
			return runScheduling{}, errors.New("concurrency_key must be 512 characters or less")
		}
		concurrencyKey = pgtype.Text{String: key, Valid: true}
	}

	ttl := strings.TrimSpace(options.TTL)
	if ttl == "" {
		ttl = strings.TrimSpace(task.Ttl)
	}
	queuedExpiresAt := pgtype.Timestamptz{}
	if ttl != "" {
		duration, err := api.ParsePositiveDuration(ttl, "ttl")
		if err != nil {
			return runScheduling{}, err
		}
		queuedExpiresAt = pgtype.Timestamptz{Time: now.Add(duration), Valid: true}
	}

	return runScheduling{
		queueName:             queueName,
		queueConcurrencyLimit: queueLimit,
		concurrencyKey:        concurrencyKey,
		priority:              options.Priority,
		queueTimestamp:        pgtype.Timestamptz{Time: now, Valid: true},
		ttl:                   ttl,
		queuedExpiresAt:       queuedExpiresAt,
	}, nil
}

func (s *Server) validateRunQueueOverride(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, options api.CreateRunOptions, task db.GetDeploymentTaskRow, scheduling runScheduling) (runScheduling, error) {
	if options.Queue == nil {
		return scheduling, nil
	}
	queueConfig, err := s.db.GetDeploymentQueueConfig(ctx, db.GetDeploymentQueueConfigParams{
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		DeploymentID:  task.DeploymentID,
		QueueName:     scheduling.queueName,
	})
	if isNoRows(err) {
		return runScheduling{}, fmt.Errorf("queue %q is not declared in the selected deployment", scheduling.queueName)
	}
	if err != nil {
		return runScheduling{}, err
	}
	scheduling.queueConcurrencyLimit = queueConfig.QueueConcurrencyLimit
	return scheduling, nil
}

func publicRunStatus(status db.RunStatus) string {
	return string(status)
}
