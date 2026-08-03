package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestProjectRunSnapshotPreservesTerminalContract(t *testing.T) {
	createdAt := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	runID := uuid.Must(uuid.NewV7())
	record := runSnapshotRecord{
		id: pgvalue.UUID(runID), status: db.RunStatusFailed,
		entrypointKind: "task", entrypointDeclaredID: "resize-image",
		deploymentID:         pgvalue.UUID(uuid.Must(uuid.NewV7())),
		deploymentVersion:    "2026.07.24.1",
		workspaceID:          pgvalue.UUID(uuid.Must(uuid.NewV7())),
		currentAttemptNumber: 3, causeKind: "api",
		metadata: []byte(`{"source":"backend"}`), tags: []string{"image"},
		terminalReasonCode: pgvalue.Text("task_failed"),
		runError:           []byte(`{"details":{"image_id":"image-1"},"message":"resize failed"}`),
		createdAt:          pgvalue.Timestamptz(createdAt),
		terminalAt:         pgvalue.Timestamptz(createdAt.Add(time.Minute)),
	}

	snapshot, err := projectRunSnapshot(record)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ID != runID.String() || snapshot.Status != "failed" ||
		snapshot.Error == nil || snapshot.Error.Code != "task_failed" ||
		snapshot.Error.Message != "resize failed" || snapshot.Error.Retryable ||
		snapshot.Output != nil || snapshot.TerminalReasonCode != "task_failed" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	var details map[string]string
	if err := json.Unmarshal(snapshot.Error.Details, &details); err != nil {
		t.Fatal(err)
	}
	if details["image_id"] != "image-1" {
		t.Fatalf("unexpected details: %v", details)
	}
}

func TestProjectRunSnapshotMapsScheduledCause(t *testing.T) {
	scheduledAt := time.Date(2026, 7, 24, 12, 0, 0, 0, time.FixedZone("offset", 9*60*60))
	record := runSnapshotRecord{
		id: pgvalue.UUID(uuid.Must(uuid.NewV7())), status: db.RunStatusRunning,
		entrypointKind: "task", entrypointDeclaredID: "cleanup",
		deploymentID:         pgvalue.UUID(uuid.Must(uuid.NewV7())),
		deploymentVersion:    "2026.07.24.1",
		workspaceID:          pgvalue.UUID(uuid.Must(uuid.NewV7())),
		currentAttemptNumber: 1, causeKind: "schedule",
		scheduleID:       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		scheduledAt:      pgvalue.Timestamptz(scheduledAt),
		scheduleTimezone: pgvalue.Text("Asia/Tokyo"),
		metadata:         []byte(`{}`),
		createdAt:        pgvalue.Timestamptz(scheduledAt),
	}

	snapshot, err := projectRunSnapshot(record)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Cause.Type != "schedule" || snapshot.Cause.ScheduledAt == nil ||
		snapshot.Cause.ScheduledAt.Location() != time.UTC ||
		snapshot.Cause.Timezone != "Asia/Tokyo" {
		t.Fatalf("unexpected cause: %+v", snapshot.Cause)
	}
}

func TestRunListCursorIsBoundToScopeAndFilter(t *testing.T) {
	createdAt := time.Date(2026, 7, 24, 12, 0, 0, 123, time.UTC)
	runID := uuid.Must(uuid.NewV7())
	statuses := []db.RunStatus{db.RunStatusRunning, db.RunStatusWaiting}
	raw, err := encodeRunListCursor(runListCursor{
		ProjectID: "project", EnvironmentID: "environment",
		Statuses: runStatusStrings(statuses), CreatedAt: createdAt.Format(time.RFC3339Nano),
		RunID: runID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := parseRunListCursor(raw, "project", "environment", statuses)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.runID != runID || !cursor.createdAt.Equal(createdAt) {
		t.Fatalf("unexpected cursor: %+v", cursor)
	}
	if _, err := parseRunListCursor(
		raw, "project", "another-environment", statuses,
	); err == nil {
		t.Fatal("cross-Environment cursor was accepted")
	}
	if _, err := parseRunListCursor(
		raw, "project", "environment", []db.RunStatus{db.RunStatusFailed},
	); err == nil {
		t.Fatal("cursor with another status filter was accepted")
	}
}

func TestRunReadDeniesBeforeScopeLookup(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("projectID", "missing")
	route.URLParams.Add("environmentID", "missing")
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, auth.Actor{
		Kind: auth.ActorKindAPIKey, OrgID: uuid.Must(uuid.NewV7()),
		ProjectID: uuid.Must(uuid.NewV7()).String(), EnvironmentID: uuid.Must(uuid.NewV7()).String(),
	})
	recorder := httptest.NewRecorder()

	(&Server{}).listRunSnapshotsHTTP(recorder, request.WithContext(ctx))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != "permission_required" {
		t.Fatalf("unexpected response: %s", recorder.Body.String())
	}
}
