package control

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	rundomain "github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	runListDefaultLimit = int32(50)
	runListMaxLimit     = int32(100)
	runListCursorPrefix = "rn1."
)

type runListCursor struct {
	ProjectID     string   `json:"project_id"`
	EnvironmentID string   `json:"environment_id"`
	Statuses      []string `json:"statuses"`
	CreatedAt     string   `json:"created_at"`
	RunID         string   `json:"run_id"`
}

type runSnapshotRecord struct {
	id                   pgtype.UUID
	status               db.RunStatus
	entrypointKind       string
	entrypointDeclaredID string
	deploymentID         pgtype.UUID
	deploymentVersion    string
	workspaceID          pgtype.UUID
	actorID              pgtype.UUID
	parentRunID          pgtype.UUID
	parentOwnsLifecycle  pgtype.Bool
	currentAttemptNumber int32
	causeKind            string
	scheduleID           pgtype.UUID
	scheduledAt          pgtype.Timestamptz
	previousScheduledAt  pgtype.Timestamptz
	scheduleTimezone     pgtype.Text
	metadata             []byte
	tags                 []string
	output               []byte
	terminalReasonCode   pgtype.Text
	runError             []byte
	createdAt            pgtype.Timestamptz
	startedAt            pgtype.Timestamptz
	terminalAt           pgtype.Timestamptz
}

func (s *Server) getRunSnapshotHTTP(w http.ResponseWriter, r *http.Request) {
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, ok := s.authorizeRunRequest(
		w, r, principal, auth.PermissionRunsRead,
	)
	if !ok {
		return
	}
	runID, err := ids.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		writeError(w, notFound(codedError{code: "run_not_found", message: "Run not found"}))
		return
	}
	row, err := s.db.GetRunSnapshot(r.Context(), db.GetRunSnapshotParams{
		OrgID: pgvalue.UUID(scope.OrgID), ProjectID: projectID,
		EnvironmentID: environmentID, ID: pgvalue.UUID(runID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "run_not_found", message: "Run not found"}))
		return
	}
	if err != nil {
		s.writeRunReadAuthorityError(w)
		return
	}
	snapshot, err := projectRunSnapshot(runSnapshotRecordFromGet(row))
	if err != nil {
		s.writeRunReadAuthorityError(w)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) cancelRunHTTP(w http.ResponseWriter, r *http.Request) {
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, ok := s.authorizeRunRequest(
		w, r, principal, auth.PermissionRunsManage,
	)
	if !ok {
		return
	}
	runID, err := ids.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		writeError(w, notFound(codedError{code: "run_not_found", message: "Run not found"}))
		return
	}
	projectUUID, projectErr := pgvalue.UUIDValue(projectID)
	environmentUUID, environmentErr := pgvalue.UUIDValue(environmentID)
	if projectErr != nil || environmentErr != nil || s.tx == nil {
		s.writeRunCancellationAuthorityError(w)
		return
	}
	canceler, err := rundomain.NewCanceler(s.tx)
	if err != nil {
		s.writeRunCancellationAuthorityError(w)
		return
	}
	_, err = canceler.Cancel(r.Context(), rundomain.CancellationRequest{
		OrgID: scope.OrgID, ProjectID: projectUUID, EnvironmentID: environmentUUID,
		RunID: runID,
	})
	if errors.Is(err, rundomain.ErrCancellationNotFound) {
		writeError(w, notFound(codedError{code: "run_not_found", message: "Run not found"}))
		return
	}
	if errors.Is(err, rundomain.ErrCancellationConflict) {
		writeError(w, conflict(codedError{
			code: "run_lifecycle_conflict", message: "Run already has another terminal outcome",
		}))
		return
	}
	if err != nil {
		s.writeRunCancellationAuthorityError(w)
		return
	}
	row, err := s.db.GetRunSnapshot(r.Context(), db.GetRunSnapshotParams{
		OrgID: pgvalue.UUID(scope.OrgID), ProjectID: projectID,
		EnvironmentID: environmentID, ID: pgvalue.UUID(runID),
	})
	if err != nil {
		s.writeRunCancellationAuthorityError(w)
		return
	}
	snapshot, err := projectRunSnapshot(runSnapshotRecordFromGet(row))
	if err != nil {
		s.writeRunCancellationAuthorityError(w)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) listRunSnapshotsHTTP(w http.ResponseWriter, r *http.Request) {
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, ok := s.authorizeRunRequest(
		w, r, principal, auth.PermissionRunsRead,
	)
	if !ok {
		return
	}
	statuses, err := parseRunStatusFilter(r)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_run_list", message: err.Error()}))
		return
	}
	limit, err := parseRunListLimit(r)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_run_list", message: err.Error()}))
		return
	}
	var afterCreatedAt pgtype.Timestamptz
	var afterID pgtype.UUID
	if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
		cursor, err := parseRunListCursor(raw, scope.ProjectID, scope.EnvironmentID, statuses)
		if err != nil {
			writeError(w, badRequest(codedError{code: "invalid_run_cursor", message: err.Error()}))
			return
		}
		afterCreatedAt = pgvalue.Timestamptz(cursor.createdAt)
		afterID = pgvalue.UUID(cursor.runID)
	}
	rows, err := s.db.ListRunSnapshots(r.Context(), db.ListRunSnapshotsParams{
		OrgID: pgvalue.UUID(scope.OrgID), ProjectID: projectID, EnvironmentID: environmentID,
		Statuses: statuses, AfterCreatedAt: afterCreatedAt, AfterID: afterID,
		LimitCount: limit + 1,
	})
	if err != nil {
		s.writeRunReadAuthorityError(w)
		return
	}
	hasMore := len(rows) > int(limit)
	if hasMore {
		rows = rows[:limit]
	}
	snapshots := make([]api.RunSnapshotResponse, 0, len(rows))
	for _, row := range rows {
		snapshot, err := projectRunSnapshot(runSnapshotRecordFromList(row))
		if err != nil {
			s.writeRunReadAuthorityError(w)
			return
		}
		snapshots = append(snapshots, snapshot)
	}
	var nextCursor string
	if hasMore {
		last := rows[len(rows)-1]
		nextCursor, err = encodeRunListCursor(runListCursor{
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
			Statuses: runStatusStrings(statuses), CreatedAt: last.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
			RunID: pgvalue.UUIDString(last.ID),
		})
		if err != nil {
			s.writeRunReadAuthorityError(w)
			return
		}
	}
	writeJSON(w, http.StatusOK, api.ListRunSnapshotsResponse{
		Runs: snapshots, NextCursor: nextCursor,
	})
}

func (s *Server) authorizeRunRequest(
	w http.ResponseWriter,
	r *http.Request,
	principal auth.Actor,
	permission auth.Permission,
) (auth.Scope, pgtype.UUID, pgtype.UUID, bool) {
	if err := authorizeRunBeforeLookup(principal, permission); err != nil {
		writeError(w, err)
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	projectRef, environmentRef, err := environmentScopeRefsFromRequest(r, principal, "", "")
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_run_scope", message: err.Error()}))
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	scope, projectID, environmentID, err := s.requestEnvironmentScope(
		r.Context(), principal, projectRef, environmentRef,
	)
	if err != nil {
		s.writeRunReadAuthorityError(w)
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	if !principal.HasPermission(permission, scope) {
		writeError(w, forbidden(codedError{
			code: "permission_required", message: errPermissionRequired.Error(),
		}))
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	if s.db == nil {
		s.writeRunReadAuthorityError(w)
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, false
	}
	return scope, projectID, environmentID, true
}

func authorizeRunBeforeLookup(principal auth.Actor, permission auth.Permission) error {
	switch principal.Kind {
	case auth.ActorKindAPIKey:
		scope, ok := principal.EnvironmentScope()
		if !ok {
			return unavailable(codedError{
				code: "run_authority_unavailable", message: errAPIKeyEnvironmentScopeRequired.Error(),
				retryable: true,
			})
		}
		if principal.HasPermission(permission, scope) {
			return nil
		}
	case auth.ActorKindSession:
		if auth.RoleAllows(principal.Role, permission) {
			return nil
		}
	}
	return forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()})
}

func (s *Server) writeRunReadAuthorityError(w http.ResponseWriter) {
	writeError(w, unavailable(codedError{
		code: "run_authority_unavailable", message: "Run authority is unavailable", retryable: true,
	}))
}

func (s *Server) writeRunCancellationAuthorityError(w http.ResponseWriter) {
	writeError(w, unavailable(codedError{
		code: "run_cancellation_unavailable", message: "Run cancellation is unavailable",
		retryable: true,
	}))
}

func parseRunStatusFilter(r *http.Request) ([]db.RunStatus, error) {
	var values []string
	for _, raw := range r.URL.Query()["status"] {
		values = append(values, strings.Split(raw, ",")...)
	}
	seen := make(map[db.RunStatus]struct{}, len(values))
	statuses := make([]db.RunStatus, 0, len(values))
	for _, raw := range values {
		status := db.RunStatus(strings.ReplaceAll(strings.TrimSpace(raw), "-", "_"))
		switch status {
		case db.RunStatusQueued, db.RunStatusRunning, db.RunStatusWaiting,
			db.RunStatusRetryDelayed, db.RunStatusCancelRequested, db.RunStatusSucceeded,
			db.RunStatusFailed, db.RunStatusCancelled, db.RunStatusExpired,
			db.RunStatusSystemFailed:
		default:
			return nil, fmt.Errorf("status %q is invalid", raw)
		}
		if _, ok := seen[status]; ok {
			continue
		}
		seen[status] = struct{}{}
		statuses = append(statuses, status)
	}
	slices.Sort(statuses)
	return statuses, nil
}

func parseRunListLimit(r *http.Request) (int32, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return runListDefaultLimit, nil
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || value < 1 || value > int64(runListMaxLimit) {
		return 0, fmt.Errorf("limit must be between 1 and %d", runListMaxLimit)
	}
	return int32(value), nil
}

type parsedRunListCursor struct {
	createdAt time.Time
	runID     uuid.UUID
}

func encodeRunListCursor(cursor runListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return runListCursorPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func parseRunListCursor(
	raw string,
	projectID string,
	environmentID string,
	statuses []db.RunStatus,
) (parsedRunListCursor, error) {
	if !strings.HasPrefix(raw, runListCursorPrefix) {
		return parsedRunListCursor{}, errors.New("Run cursor has an unsupported version")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, runListCursorPrefix))
	if err != nil {
		return parsedRunListCursor{}, errors.New("Run cursor is malformed")
	}
	var cursor runListCursor
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return parsedRunListCursor{}, errors.New("Run cursor is malformed")
	}
	if cursor.ProjectID != projectID || cursor.EnvironmentID != environmentID {
		return parsedRunListCursor{}, errors.New("Run cursor belongs to another Environment")
	}
	if !slices.Equal(cursor.Statuses, runStatusStrings(statuses)) {
		return parsedRunListCursor{}, errors.New("Run cursor does not match the status filter")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt)
	if err != nil {
		return parsedRunListCursor{}, errors.New("Run cursor is malformed")
	}
	runID, err := ids.Parse(cursor.RunID)
	if err != nil {
		return parsedRunListCursor{}, errors.New("Run cursor is malformed")
	}
	return parsedRunListCursor{createdAt: createdAt.UTC(), runID: runID}, nil
}

func runStatusStrings(statuses []db.RunStatus) []string {
	values := make([]string, len(statuses))
	for index, status := range statuses {
		values[index] = string(status)
	}
	return values
}

func projectRunSnapshot(record runSnapshotRecord) (api.RunSnapshotResponse, error) {
	runID := pgvalue.UUIDString(record.id)
	deploymentID := pgvalue.UUIDString(record.deploymentID)
	workspaceID := pgvalue.UUIDString(record.workspaceID)
	if ids.Validate(runID) != nil ||
		ids.Validate(deploymentID) != nil ||
		ids.Validate(workspaceID) != nil ||
		!record.createdAt.Valid {
		return api.RunSnapshotResponse{}, errors.New("Run projection authority is invalid")
	}
	actorID := pgvalue.UUIDString(record.actorID)
	if record.actorID.Valid && ids.Validate(actorID) != nil {
		return api.RunSnapshotResponse{}, errors.New("Run Actor projection authority is invalid")
	}
	parentRunID := pgvalue.UUIDString(record.parentRunID)
	if record.parentRunID.Valid && ids.Validate(parentRunID) != nil {
		return api.RunSnapshotResponse{}, errors.New("Run parent projection authority is invalid")
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(record.metadata, &metadata); err != nil || metadata == nil {
		return api.RunSnapshotResponse{}, errors.New("Run metadata projection is invalid")
	}
	status := strings.ReplaceAll(string(record.status), "_", "-")
	cause, err := projectRunCause(record)
	if err != nil {
		return api.RunSnapshotResponse{}, err
	}
	response := api.RunSnapshotResponse{
		ID: runID, Status: status,
		Entrypoint: api.RunEntrypointResponse{
			Kind: record.entrypointKind, ID: record.entrypointDeclaredID,
		},
		Deployment: api.RunDeploymentResponse{
			ID: deploymentID, Version: record.deploymentVersion,
		},
		WorkspaceID: workspaceID, CurrentAttemptNumber: record.currentAttemptNumber,
		Cause: cause, Metadata: json.RawMessage(record.metadata),
		Tags: append([]string{}, record.tags...), CreatedAt: record.createdAt.Time.UTC(),
	}
	if record.actorID.Valid {
		response.ActorID = actorID
	}
	if record.parentRunID.Valid {
		response.ParentRunID = parentRunID
	}
	if record.parentOwnsLifecycle.Valid {
		value := record.parentOwnsLifecycle.Bool
		response.ParentOwnsLifecycle = &value
	}
	if record.startedAt.Valid {
		value := record.startedAt.Time.UTC()
		response.StartedAt = &value
	}
	if record.terminalAt.Valid {
		value := record.terminalAt.Time.UTC()
		response.TerminalAt = &value
	}
	if record.status == db.RunStatusSucceeded {
		response.Output = json.RawMessage(record.output)
	}
	if record.terminalReasonCode.Valid {
		response.TerminalReasonCode = record.terminalReasonCode.String
		runError, err := projectRunError(record.terminalReasonCode.String, record.runError)
		if err != nil {
			return api.RunSnapshotResponse{}, err
		}
		response.Error = &runError
	}
	return response, nil
}

func projectRunCause(record runSnapshotRecord) (api.RunCauseResponse, error) {
	switch record.causeKind {
	case "api", "manual":
		return api.RunCauseResponse{Type: record.causeKind}, nil
	case "child":
		if !record.parentRunID.Valid {
			return api.RunCauseResponse{}, errors.New("child Run has no parent")
		}
		return api.RunCauseResponse{
			Type: "child", ParentRunID: pgvalue.UUIDString(record.parentRunID),
		}, nil
	case "schedule":
		if !record.scheduleID.Valid || !record.scheduledAt.Valid ||
			!record.scheduleTimezone.Valid {
			return api.RunCauseResponse{}, errors.New("scheduled Run cause is incomplete")
		}
		scheduleID := pgvalue.UUIDString(record.scheduleID)
		if ids.Validate(scheduleID) != nil {
			return api.RunCauseResponse{}, errors.New("scheduled Run identity is invalid")
		}
		scheduledAt := record.scheduledAt.Time.UTC()
		cause := api.RunCauseResponse{
			Type: "schedule", ScheduleID: scheduleID,
			ScheduledAt: &scheduledAt, Timezone: record.scheduleTimezone.String,
		}
		if record.previousScheduledAt.Valid {
			value := record.previousScheduledAt.Time.UTC()
			cause.LastScheduledAt = &value
		}
		return cause, nil
	case "actor_start":
		return api.RunCauseResponse{Type: "actor-start"}, nil
	case "continuation":
		return api.RunCauseResponse{Type: "continuation"}, nil
	default:
		return api.RunCauseResponse{}, errors.New("Run cause is invalid")
	}
}

func projectRunError(reason string, raw []byte) (api.RunErrorResponse, error) {
	response := api.RunErrorResponse{
		Code: reason, Message: strings.ReplaceAll(reason, "_", " "), Retryable: false,
	}
	if len(raw) == 0 {
		return response, nil
	}
	var stored struct {
		Code      string          `json:"code"`
		Message   string          `json:"message"`
		Retryable *bool           `json:"retryable"`
		Details   json.RawMessage `json:"details"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return api.RunErrorResponse{}, errors.New("Run error projection is invalid")
	}
	if stored.Code != "" {
		response.Code = stored.Code
	}
	if stored.Message != "" {
		response.Message = stored.Message
	}
	if stored.Retryable != nil {
		response.Retryable = *stored.Retryable
	}
	if len(stored.Details) > 0 && string(stored.Details) != "null" {
		response.Details = stored.Details
	}
	return response, nil
}

func runSnapshotRecordFromGet(row db.GetRunSnapshotRow) runSnapshotRecord {
	return runSnapshotRecord{
		id: row.ID, status: row.Status,
		entrypointKind: row.EntrypointKind, entrypointDeclaredID: row.EntrypointDeclaredID,
		deploymentID: row.DeploymentID, deploymentVersion: row.DeploymentVersion,
		workspaceID: row.WorkspaceID, actorID: row.ActorID,
		parentRunID: row.ParentRunID, parentOwnsLifecycle: row.ParentOwnsLifecycle,
		currentAttemptNumber: row.CurrentAttemptNumber, causeKind: row.CauseKind,
		scheduleID: row.ScheduleID, scheduledAt: row.ScheduledAt,
		previousScheduledAt: row.PreviousScheduledAt, scheduleTimezone: row.ScheduleTimezone,
		metadata: row.Metadata, tags: row.Tags, output: row.Output,
		terminalReasonCode: row.TerminalReasonCode, runError: row.Error,
		createdAt: row.CreatedAt, startedAt: row.StartedAt, terminalAt: row.TerminalAt,
	}
}

func runSnapshotRecordFromList(row db.ListRunSnapshotsRow) runSnapshotRecord {
	return runSnapshotRecord{
		id: row.ID, status: row.Status,
		entrypointKind: row.EntrypointKind, entrypointDeclaredID: row.EntrypointDeclaredID,
		deploymentID: row.DeploymentID, deploymentVersion: row.DeploymentVersion,
		workspaceID: row.WorkspaceID, actorID: row.ActorID,
		parentRunID: row.ParentRunID, parentOwnsLifecycle: row.ParentOwnsLifecycle,
		currentAttemptNumber: row.CurrentAttemptNumber, causeKind: row.CauseKind,
		scheduleID: row.ScheduleID, scheduledAt: row.ScheduledAt,
		previousScheduledAt: row.PreviousScheduledAt, scheduleTimezone: row.ScheduleTimezone,
		metadata: row.Metadata, tags: row.Tags, output: row.Output,
		terminalReasonCode: row.TerminalReasonCode, runError: row.Error,
		createdAt: row.CreatedAt, startedAt: row.StartedAt, terminalAt: row.TerminalAt,
	}
}
