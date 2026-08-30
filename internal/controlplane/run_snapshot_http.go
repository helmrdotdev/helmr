package controlplane

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	runListDefaultLimit = int32(50)
	runListMaxLimit     = int32(100)
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
	currentAttemptNumber int32
	causeKind            string
	scheduleID           pgtype.UUID
	scheduledAt          pgtype.Timestamptz
	previousScheduledAt  pgtype.Timestamptz
	scheduleTimezone     pgtype.Text
	metadata             []byte
	tags                 []string
	output               []byte
	failure              []byte
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
		writeError(w, notFound(codedError{code: "run_not_found", message: "run not found"}))
		return
	}
	row, err := s.db.GetRunSnapshot(r.Context(), db.GetRunSnapshotParams{
		OrgID: pgvalue.UUID(scope.OrgID), ProjectID: projectID,
		EnvironmentID: environmentID, ID: pgvalue.UUID(runID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "run_not_found", message: "run not found"}))
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
		writeError(w, notFound(codedError{code: "run_not_found", message: "run not found"}))
		return
	}
	projectUUID, projectErr := pgvalue.UUIDValue(projectID)
	environmentUUID, environmentErr := pgvalue.UUIDValue(environmentID)
	if projectErr != nil || environmentErr != nil || s.tx == nil {
		s.writeRunCancellationAuthorityError(w)
		return
	}
	canceler, err := run.NewCanceler(s.tx)
	if err != nil {
		s.writeRunCancellationAuthorityError(w)
		return
	}
	_, err = canceler.Cancel(r.Context(), run.CancellationRequest{
		OrgID: scope.OrgID, ProjectID: projectUUID, EnvironmentID: environmentUUID,
		RunID: runID,
	})
	if errors.Is(err, run.ErrCancellationNotFound) {
		writeError(w, notFound(codedError{code: "run_not_found", message: "run not found"}))
		return
	}
	if errors.Is(err, run.ErrCancellationConflict) {
		writeError(w, conflict(codedError{
			code: "run_lifecycle_conflict", message: "run already has another terminal outcome",
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
	if err := validateRunListQuery(r); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_run_list", message: err.Error()}))
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
	rows, err := s.db.ListRunListItems(r.Context(), db.ListRunListItemsParams{
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
	items := make([]api.RunListItem, 0, len(rows))
	for _, row := range rows {
		item, err := projectRunListItem(row)
		if err != nil {
			s.writeRunReadAuthorityError(w)
			return
		}
		items = append(items, item)
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
	writeJSON(w, http.StatusOK, api.ListRunsResponse{
		Runs: items, NextCursor: nextCursor,
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
	projectRef, environmentRef, err := environmentScopeRefsFromRequest(r, principal)
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
		code: "run_authority_unavailable", message: "run authority is unavailable", retryable: true,
	}))
}

func (s *Server) writeRunCancellationAuthorityError(w http.ResponseWriter) {
	writeError(w, unavailable(codedError{
		code: "run_cancellation_unavailable", message: "run cancellation is unavailable",
		retryable: true,
	}))
}

func validateRunListQuery(r *http.Request) error {
	query := r.URL.Query()
	for name := range query {
		switch name {
		case "status", "cursor", "limit":
		default:
			return fmt.Errorf("query parameter %q is not supported", name)
		}
	}
	for _, name := range []string{"cursor", "limit"} {
		if len(query[name]) > 1 {
			return fmt.Errorf("%s must not be repeated", name)
		}
		if len(query[name]) == 1 && strings.TrimSpace(query[name][0]) == "" {
			return fmt.Errorf("%s must not be empty", name)
		}
	}
	return nil
}

func parseRunStatusFilter(r *http.Request) ([]db.RunStatus, error) {
	var values []string
	for _, raw := range r.URL.Query()["status"] {
		values = append(values, strings.Split(raw, ",")...)
	}
	seen := make(map[db.RunStatus]struct{}, len(values))
	statuses := make([]db.RunStatus, 0, len(values))
	for _, raw := range values {
		status, ok := runStatusFilter(strings.TrimSpace(raw))
		if !ok {
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

func runStatusFilter(raw string) (db.RunStatus, bool) {
	switch api.RunStatus(raw) {
	case api.RunStatusQueued:
		return db.RunStatusQueued, true
	case api.RunStatusRunning:
		return db.RunStatusRunning, true
	case api.RunStatusWaiting:
		return db.RunStatusWaiting, true
	case api.RunStatusRetryDelayed:
		return db.RunStatusRetryDelayed, true
	case api.RunStatusCancelRequested:
		return db.RunStatusCancelRequested, true
	case api.RunStatusSucceeded:
		return db.RunStatusSucceeded, true
	case api.RunStatusFailed:
		return db.RunStatusFailed, true
	case api.RunStatusCancelled:
		return db.RunStatusCancelled, true
	case api.RunStatusExpired:
		return db.RunStatusExpired, true
	case api.RunStatusSystemFailed:
		return db.RunStatusSystemFailed, true
	default:
		return "", false
	}
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
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func parseRunListCursor(
	raw string,
	projectID string,
	environmentID string,
	statuses []db.RunStatus,
) (parsedRunListCursor, error) {
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return parsedRunListCursor{}, errors.New("run cursor is malformed")
	}
	var cursor runListCursor
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return parsedRunListCursor{}, errors.New("run cursor is malformed")
	}
	if cursor.ProjectID != projectID || cursor.EnvironmentID != environmentID {
		return parsedRunListCursor{}, errors.New("run cursor belongs to another environment")
	}
	if !slices.Equal(cursor.Statuses, runStatusStrings(statuses)) {
		return parsedRunListCursor{}, errors.New("run cursor does not match the status filter")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt)
	if err != nil {
		return parsedRunListCursor{}, errors.New("run cursor is malformed")
	}
	runID, err := ids.Parse(cursor.RunID)
	if err != nil {
		return parsedRunListCursor{}, errors.New("run cursor is malformed")
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
		return api.RunSnapshotResponse{}, errors.New("run projection authority is invalid")
	}
	actorID := pgvalue.UUIDString(record.actorID)
	if record.actorID.Valid && ids.Validate(actorID) != nil {
		return api.RunSnapshotResponse{}, errors.New("run actor projection authority is invalid")
	}
	parentRunID := pgvalue.UUIDString(record.parentRunID)
	if record.parentRunID.Valid && ids.Validate(parentRunID) != nil {
		return api.RunSnapshotResponse{}, errors.New("run parent projection authority is invalid")
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(record.metadata, &metadata); err != nil || metadata == nil {
		return api.RunSnapshotResponse{}, errors.New("run metadata projection is invalid")
	}
	status, err := runPublicStatus(record.status)
	if err != nil {
		return api.RunSnapshotResponse{}, err
	}
	cause, err := projectRunCause(record)
	if err != nil {
		return api.RunSnapshotResponse{}, err
	}
	response := api.RunSnapshotResponse{
		ID: runID, Status: status,
		Entrypoint: api.RunEntrypointResponse{
			Kind: record.entrypointKind, ID: record.entrypointDeclaredID,
		},
		Deployment: api.DeploymentReference{
			ID: deploymentID, Version: record.deploymentVersion,
		},
		WorkspaceID: workspaceID, CurrentAttemptNumber: record.currentAttemptNumber,
		Cause: cause, Metadata: json.RawMessage(record.metadata),
		Tags: append([]string{}, record.tags...), CreatedAt: record.createdAt.Time.UTC(),
	}
	if record.actorID.Valid {
		response.SessionID = actorID
	}
	if record.parentRunID.Valid {
		response.ParentRunID = parentRunID
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
	terminalFailure := record.status == db.RunStatusFailed ||
		record.status == db.RunStatusCancelled ||
		record.status == db.RunStatusExpired ||
		record.status == db.RunStatusSystemFailed
	if terminalFailure != (len(record.failure) > 0) {
		return api.RunSnapshotResponse{}, errors.New("run failure projection is inconsistent")
	}
	if len(record.failure) > 0 {
		failure, err := projectRunFailure(record.failure)
		if err != nil {
			return api.RunSnapshotResponse{}, err
		}
		response.Failure = &failure
	}
	return response, nil
}

func runPublicStatus(status db.RunStatus) (api.RunStatus, error) {
	switch status {
	case db.RunStatusQueued:
		return api.RunStatusQueued, nil
	case db.RunStatusRunning:
		return api.RunStatusRunning, nil
	case db.RunStatusWaiting:
		return api.RunStatusWaiting, nil
	case db.RunStatusRetryDelayed:
		return api.RunStatusRetryDelayed, nil
	case db.RunStatusCancelRequested:
		return api.RunStatusCancelRequested, nil
	case db.RunStatusSucceeded:
		return api.RunStatusSucceeded, nil
	case db.RunStatusFailed:
		return api.RunStatusFailed, nil
	case db.RunStatusCancelled:
		return api.RunStatusCancelled, nil
	case db.RunStatusExpired:
		return api.RunStatusExpired, nil
	case db.RunStatusSystemFailed:
		return api.RunStatusSystemFailed, nil
	default:
		return "", fmt.Errorf("run status %q has no public projection", status)
	}
}

func projectRunCause(record runSnapshotRecord) (api.RunCauseResponse, error) {
	switch record.causeKind {
	case "api", "manual":
		return api.RunCauseResponse{Type: record.causeKind}, nil
	case "child":
		if !record.parentRunID.Valid {
			return api.RunCauseResponse{}, errors.New("child run has no parent")
		}
		return api.RunCauseResponse{
			Type: "child", ParentRunID: pgvalue.UUIDString(record.parentRunID),
		}, nil
	case "schedule":
		if !record.scheduleID.Valid || !record.scheduledAt.Valid ||
			!record.scheduleTimezone.Valid {
			return api.RunCauseResponse{}, errors.New("scheduled run cause is incomplete")
		}
		scheduleID := pgvalue.UUIDString(record.scheduleID)
		if ids.Validate(scheduleID) != nil {
			return api.RunCauseResponse{}, errors.New("scheduled run identity is invalid")
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
		return api.RunCauseResponse{Type: "actor_start"}, nil
	case "continuation":
		return api.RunCauseResponse{Type: "continuation"}, nil
	default:
		return api.RunCauseResponse{}, errors.New("run cause is invalid")
	}
}

func projectRunFailure(raw []byte) (api.RunFailureResponse, error) {
	var response api.RunFailureResponse
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || response.Code == "" ||
		response.Message == "" || len(response.Details) == 0 {
		return api.RunFailureResponse{}, errors.New("run failure projection is invalid")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return api.RunFailureResponse{}, errors.New("run failure projection is invalid")
	}
	var details map[string]json.RawMessage
	if err := json.Unmarshal(response.Details, &details); err != nil || details == nil {
		return api.RunFailureResponse{}, errors.New("run failure details are invalid")
	}
	return response, nil
}

func runSnapshotRecordFromGet(row db.GetRunSnapshotRow) runSnapshotRecord {
	return runSnapshotRecord{
		id: row.ID, status: row.Status,
		entrypointKind: row.EntrypointKind, entrypointDeclaredID: row.EntrypointDeclaredID,
		deploymentID: row.DeploymentID, deploymentVersion: row.DeploymentVersion,
		workspaceID: row.WorkspaceID, actorID: row.SessionID,
		parentRunID:          row.ParentRunID,
		currentAttemptNumber: row.CurrentAttemptNumber, causeKind: row.CauseKind,
		scheduleID: row.ScheduleID, scheduledAt: row.ScheduledAt,
		previousScheduledAt: row.PreviousScheduledAt, scheduleTimezone: row.ScheduleTimezone,
		metadata: row.Metadata, tags: row.Tags, output: row.Output,
		failure:   row.Failure,
		createdAt: row.CreatedAt, startedAt: row.StartedAt, terminalAt: row.TerminalAt,
	}
}

func projectRunListItem(row db.ListRunListItemsRow) (api.RunListItem, error) {
	status, err := runPublicStatus(row.Status)
	if err != nil {
		return api.RunListItem{}, err
	}
	runID := pgvalue.UUIDString(row.ID)
	workspaceID := pgvalue.UUIDString(row.WorkspaceID)
	if ids.Validate(runID) != nil || ids.Validate(workspaceID) != nil || !row.CreatedAt.Valid {
		return api.RunListItem{}, errors.New("run list projection authority is invalid")
	}
	item := api.RunListItem{
		ID: runID, Status: status,
		Entrypoint:  api.RunEntrypointResponse{Kind: row.EntrypointKind, ID: row.EntrypointDeclaredID},
		WorkspaceID: workspaceID, CurrentAttemptNumber: row.CurrentAttemptNumber,
		CreatedAt: row.CreatedAt.Time.UTC(),
	}
	if row.SessionID.Valid {
		item.SessionID = pgvalue.UUIDString(row.SessionID)
		if ids.Validate(item.SessionID) != nil {
			return api.RunListItem{}, errors.New("run Session list projection authority is invalid")
		}
	}
	if row.StartedAt.Valid {
		value := row.StartedAt.Time.UTC()
		item.StartedAt = &value
	}
	if row.TerminalAt.Valid {
		value := row.TerminalAt.Time.UTC()
		item.TerminalAt = &value
	}
	return item, nil
}
