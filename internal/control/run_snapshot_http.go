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
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
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
	publicID             string
	status               db.RunStatus
	entrypointKind       string
	entrypointDeclaredID string
	deploymentPublicID   string
	deploymentVersion    string
	workspacePublicID    string
	actorPublicID        pgtype.Text
	parentRunPublicID    string
	parentOwnsLifecycle  pgtype.Bool
	currentAttemptNumber int32
	causeKind            string
	schedulePublicID     pgtype.Text
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
	runID := strings.TrimSpace(chi.URLParam(r, "runID"))
	if publicid.ValidateFor(publicid.Run, runID) != nil {
		writeError(w, notFound(codedError{code: "run_not_found", message: "Run not found"}))
		return
	}
	row, err := s.db.GetRunSnapshot(r.Context(), db.GetRunSnapshotParams{
		OrgID: pgvalue.UUID(scope.OrgID), ProjectID: projectID,
		EnvironmentID: environmentID, PublicID: runID,
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
	runID := strings.TrimSpace(chi.URLParam(r, "runID"))
	if publicid.ValidateFor(publicid.Run, runID) != nil {
		writeError(w, notFound(codedError{code: "run_not_found", message: "Run not found"}))
		return
	}
	projectUUID, projectErr := pgvalue.UUIDValue(projectID)
	environmentUUID, environmentErr := pgvalue.UUIDValue(environmentID)
	if projectErr != nil || environmentErr != nil || s.tx == nil {
		s.writeRunCancellationAuthorityError(w)
		return
	}
	canceller, err := db.NewRunCanceller(s.tx)
	if err != nil {
		s.writeRunCancellationAuthorityError(w)
		return
	}
	_, err = canceller.Cancel(r.Context(), db.RunCancellationRequest{
		OrgID: scope.OrgID, ProjectID: projectUUID, EnvironmentID: environmentUUID,
		RunPublicID: runID,
	})
	if errors.Is(err, db.ErrRunCancellationNotFound) {
		writeError(w, notFound(codedError{code: "run_not_found", message: "Run not found"}))
		return
	}
	if errors.Is(err, db.ErrRunCancellationConflict) {
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
		EnvironmentID: environmentID, PublicID: runID,
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
	var afterPublicID string
	if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
		cursor, err := parseRunListCursor(raw, scope.ProjectID, scope.EnvironmentID, statuses)
		if err != nil {
			writeError(w, badRequest(codedError{code: "invalid_run_cursor", message: err.Error()}))
			return
		}
		afterCreatedAt = pgvalue.Timestamptz(cursor.createdAt)
		afterPublicID = cursor.runID
	}
	rows, err := s.db.ListRunSnapshots(r.Context(), db.ListRunSnapshotsParams{
		OrgID: pgvalue.UUID(scope.OrgID), ProjectID: projectID, EnvironmentID: environmentID,
		Statuses: statuses, AfterCreatedAt: afterCreatedAt, AfterPublicID: afterPublicID,
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
			RunID: last.PublicID,
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
	runID     string
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
	if publicid.ValidateFor(publicid.Run, cursor.RunID) != nil {
		return parsedRunListCursor{}, errors.New("Run cursor is malformed")
	}
	return parsedRunListCursor{createdAt: createdAt.UTC(), runID: cursor.RunID}, nil
}

func runStatusStrings(statuses []db.RunStatus) []string {
	values := make([]string, len(statuses))
	for index, status := range statuses {
		values[index] = string(status)
	}
	return values
}

func projectRunSnapshot(record runSnapshotRecord) (api.RunSnapshotResponse, error) {
	if publicid.ValidateFor(publicid.Run, record.publicID) != nil ||
		publicid.ValidateFor(publicid.Deployment, record.deploymentPublicID) != nil ||
		publicid.ValidateFor(publicid.Workspace, record.workspacePublicID) != nil ||
		!record.createdAt.Valid {
		return api.RunSnapshotResponse{}, errors.New("Run projection authority is invalid")
	}
	if record.actorPublicID.Valid &&
		publicid.ValidateFor(publicid.Actor, record.actorPublicID.String) != nil {
		return api.RunSnapshotResponse{}, errors.New("Run Actor projection authority is invalid")
	}
	if record.parentRunPublicID != "" &&
		publicid.ValidateFor(publicid.Run, record.parentRunPublicID) != nil {
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
		ID: record.publicID, Status: status,
		Entrypoint: api.RunEntrypointResponse{
			Kind: record.entrypointKind, ID: record.entrypointDeclaredID,
		},
		Deployment: api.RunDeploymentResponse{
			ID: record.deploymentPublicID, Version: record.deploymentVersion,
		},
		WorkspaceID: record.workspacePublicID, CurrentAttemptNumber: record.currentAttemptNumber,
		Cause: cause, Metadata: json.RawMessage(record.metadata),
		Tags: append([]string{}, record.tags...), CreatedAt: record.createdAt.Time.UTC(),
	}
	if record.actorPublicID.Valid {
		response.ActorID = record.actorPublicID.String
	}
	if record.parentRunPublicID != "" {
		response.ParentRunID = record.parentRunPublicID
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
		if record.parentRunPublicID == "" {
			return api.RunCauseResponse{}, errors.New("child Run has no public parent")
		}
		return api.RunCauseResponse{
			Type: "child", ParentRunID: record.parentRunPublicID,
		}, nil
	case "schedule":
		if !record.schedulePublicID.Valid || !record.scheduledAt.Valid ||
			!record.scheduleTimezone.Valid {
			return api.RunCauseResponse{}, errors.New("scheduled Run cause is incomplete")
		}
		if publicid.ValidateFor(publicid.Schedule, record.schedulePublicID.String) != nil {
			return api.RunCauseResponse{}, errors.New("scheduled Run identity is invalid")
		}
		scheduledAt := record.scheduledAt.Time.UTC()
		cause := api.RunCauseResponse{
			Type: "schedule", ScheduleID: record.schedulePublicID.String,
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
		publicID: row.PublicID, status: row.Status,
		entrypointKind: row.EntrypointKind, entrypointDeclaredID: row.EntrypointDeclaredID,
		deploymentPublicID: row.DeploymentPublicID, deploymentVersion: row.DeploymentVersion,
		workspacePublicID: row.WorkspacePublicID, actorPublicID: row.ActorPublicID,
		parentRunPublicID: row.ParentRunPublicID, parentOwnsLifecycle: row.ParentOwnsLifecycle,
		currentAttemptNumber: row.CurrentAttemptNumber, causeKind: row.CauseKind,
		schedulePublicID: row.SchedulePublicID, scheduledAt: row.ScheduledAt,
		previousScheduledAt: row.PreviousScheduledAt, scheduleTimezone: row.ScheduleTimezone,
		metadata: row.Metadata, tags: row.Tags, output: row.Output,
		terminalReasonCode: row.TerminalReasonCode, runError: row.Error,
		createdAt: row.CreatedAt, startedAt: row.StartedAt, terminalAt: row.TerminalAt,
	}
}

func runSnapshotRecordFromList(row db.ListRunSnapshotsRow) runSnapshotRecord {
	return runSnapshotRecord{
		publicID: row.PublicID, status: row.Status,
		entrypointKind: row.EntrypointKind, entrypointDeclaredID: row.EntrypointDeclaredID,
		deploymentPublicID: row.DeploymentPublicID, deploymentVersion: row.DeploymentVersion,
		workspacePublicID: row.WorkspacePublicID, actorPublicID: row.ActorPublicID,
		parentRunPublicID: row.ParentRunPublicID, parentOwnsLifecycle: row.ParentOwnsLifecycle,
		currentAttemptNumber: row.CurrentAttemptNumber, causeKind: row.CauseKind,
		schedulePublicID: row.SchedulePublicID, scheduledAt: row.ScheduledAt,
		previousScheduledAt: row.PreviousScheduledAt, scheduleTimezone: row.ScheduleTimezone,
		metadata: row.Metadata, tags: row.Tags, output: row.Output,
		terminalReasonCode: row.TerminalReasonCode, runError: row.Error,
		createdAt: row.CreatedAt, startedAt: row.StartedAt, terminalAt: row.TerminalAt,
	}
}
