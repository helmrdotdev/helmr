package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestProjectRunSnapshotPreservesTerminalContract(t *testing.T) {
	createdAt := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	runID := uuid.NewV7()
	record := runSnapshotRecord{
		id: pgvalue.UUID(runID), status: db.RunStatusFailed,
		entrypointKind: "task", entrypointDeclaredID: "resize-image",
		deploymentID:         pgvalue.UUID(uuid.NewV7()),
		deploymentVersion:    "2026.07.24.1",
		workspaceID:          pgvalue.UUID(uuid.NewV7()),
		currentAttemptNumber: 3, causeKind: "api",
		metadata: []byte(`{"source":"backend"}`), tags: []string{"image"},
		failure:    []byte(`{"code":"task_failed","message":"resize failed","details":{"image_id":"image-1"}}`),
		createdAt:  pgvalue.Timestamptz(createdAt),
		terminalAt: pgvalue.Timestamptz(createdAt.Add(time.Minute)),
	}

	snapshot, err := projectRunSnapshot(record)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ID != runID.String() || snapshot.Status != "failed" ||
		snapshot.Failure == nil || snapshot.Failure.Code != "task_failed" ||
		snapshot.Failure.Message != "resize failed" || snapshot.Output != nil {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	var details map[string]string
	if err := json.Unmarshal(snapshot.Failure.Details, &details); err != nil {
		t.Fatal(err)
	}
	if details["image_id"] != "image-1" {
		t.Fatalf("unexpected details: %v", details)
	}
}

func TestProjectRunSnapshotMapsScheduledCause(t *testing.T) {
	scheduledAt := time.Date(2026, 7, 24, 12, 0, 0, 0, time.FixedZone("offset", 9*60*60))
	record := runSnapshotRecord{
		id: pgvalue.UUID(uuid.NewV7()), status: db.RunStatusRunning,
		entrypointKind: "task", entrypointDeclaredID: "cleanup",
		deploymentID:         pgvalue.UUID(uuid.NewV7()),
		deploymentVersion:    "2026.07.24.1",
		workspaceID:          pgvalue.UUID(uuid.NewV7()),
		currentAttemptNumber: 1, causeKind: "schedule",
		scheduleID:       pgvalue.UUID(uuid.NewV7()),
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
	runID := uuid.NewV7()
	sessionID := uuid.NewV7().String()
	statuses := []db.RunStatus{db.RunStatusRunning, db.RunStatusWaiting}
	kinds := []string{"actor"}
	raw, err := encodeRunListCursor(runListCursor{
		ProjectID: "project", EnvironmentID: "environment",
		Statuses: runStatusStrings(statuses), Kinds: kinds, SessionID: sessionID,
		CreatedAt: createdAt.Format(time.RFC3339Nano), RunID: runID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := parseRunListCursor(raw, "project", "environment", statuses, kinds, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.runID != runID || !cursor.createdAt.Equal(createdAt) {
		t.Fatalf("unexpected cursor: %+v", cursor)
	}
	if _, err := parseRunListCursor(
		raw, "project", "another-environment", statuses, kinds, sessionID,
	); err == nil {
		t.Fatal("cross-Environment cursor was accepted")
	}
	if _, err := parseRunListCursor(
		raw, "project", "environment", []db.RunStatus{db.RunStatusFailed}, kinds, sessionID,
	); err == nil {
		t.Fatal("cursor with another status filter was accepted")
	}
	if _, err := parseRunListCursor(
		raw, "project", "environment", statuses, []string{"task"}, sessionID,
	); err == nil {
		t.Fatal("cursor with another kind filter was accepted")
	}
	if _, err := parseRunListCursor(
		raw, "project", "environment", statuses, []string{"actor", "task"}, sessionID,
	); err == nil {
		t.Fatal("cursor with a wider kind filter was accepted")
	}
	if _, err := parseRunListCursor(raw, "project", "environment", statuses, nil, sessionID); err == nil {
		t.Fatal("kind-bound cursor was accepted without the kind filter")
	}
	if _, err := parseRunListCursor(
		raw, "project", "environment", statuses, kinds, uuid.NewV7().String(),
	); err == nil {
		t.Fatal("cursor with another session_id filter was accepted")
	}
	if _, err := parseRunListCursor(raw, "project", "environment", statuses, kinds, ""); err == nil {
		t.Fatal("Session-bound cursor was accepted without the session_id filter")
	}

	unfiltered, err := encodeRunListCursor(runListCursor{
		ProjectID: "project", EnvironmentID: "environment",
		Statuses: []string{}, CreatedAt: createdAt.Format(time.RFC3339Nano), RunID: runID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseRunListCursor(unfiltered, "project", "environment", nil, nil, ""); err != nil {
		t.Fatalf("unfiltered cursor was rejected: %v", err)
	}
	if _, err := parseRunListCursor(unfiltered, "project", "environment", nil, nil, sessionID); err == nil {
		t.Fatal("unfiltered cursor was accepted with a session_id filter")
	}
	if _, err := parseRunListCursor(unfiltered, "project", "environment", nil, []string{"task"}, ""); err == nil {
		t.Fatal("unfiltered cursor was accepted with a kind filter")
	}
}

func TestParseRunKindFilter(t *testing.T) {
	kinds, err := parseRunKindFilter(httptest.NewRequest(
		http.MethodGet, "/v1/runs?kind=task&kind=actor,task&kind=%20actor%20", nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kinds, []string{"actor", "task"}) {
		t.Fatalf("kinds = %v", kinds)
	}
	single, err := parseRunKindFilter(httptest.NewRequest(http.MethodGet, "/v1/runs?kind=task", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(single, []string{"task"}) {
		t.Fatalf("single kind = %v", single)
	}
	none, err := parseRunKindFilter(httptest.NewRequest(http.MethodGet, "/v1/runs", nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("absent kind filter = %v", none)
	}
	for _, target := range []string{
		"/v1/runs?kind=",
		"/v1/runs?kind=task,",
		"/v1/runs?kind=schedule",
		"/v1/runs?kind=Task",
		"/v1/runs?kind=task&kind=job",
	} {
		if _, err := parseRunKindFilter(httptest.NewRequest(http.MethodGet, target, nil)); err == nil {
			t.Fatalf("%s was accepted", target)
		}
	}
}

func TestRunListQueryRejectsEmptyAndRepeatedPagination(t *testing.T) {
	for _, target := range []string{
		"/v1/runs?cursor=",
		"/v1/runs?cursor=+",
		"/v1/runs?cursor=one&cursor=two",
		"/v1/runs?limit=",
		"/v1/runs?limit=10&limit=20",
		"/v1/runs?session_id=",
		"/v1/runs?session_id=" + uuid.NewV7().String() + "&session_id=" + uuid.NewV7().String(),
		"/v1/runs?actor_id=operator",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		if err := validateRunListQuery(request); err == nil {
			t.Fatalf("%s was accepted", target)
		}
	}
}

func TestParseRunSessionFilter(t *testing.T) {
	sessionID := uuid.NewV7()
	got, err := parseRunSessionFilter(httptest.NewRequest(
		http.MethodGet, "/v1/runs?session_id="+sessionID.String(), nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Valid || pgvalue.UUIDString(got) != sessionID.String() {
		t.Fatalf("session filter = %+v", got)
	}
	absent, err := parseRunSessionFilter(httptest.NewRequest(http.MethodGet, "/v1/runs", nil))
	if err != nil {
		t.Fatal(err)
	}
	if absent.Valid {
		t.Fatalf("absent session filter = %+v", absent)
	}
	for _, raw := range []string{"not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
		if _, err := parseRunSessionFilter(httptest.NewRequest(
			http.MethodGet, "/v1/runs?session_id="+raw, nil,
		)); err == nil {
			t.Fatalf("session_id %q was accepted", raw)
		}
	}
}

func TestRunReadDeniesBeforeScopeLookup(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("projectID", "missing")
	route.URLParams.Add("environmentID", "missing")
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, auth.Actor{
		Kind: auth.ActorKindAPIKey, OrgID: uuid.NewV7(),
		ProjectID: uuid.NewV7().String(), EnvironmentID: uuid.NewV7().String(),
	})
	recorder := httptest.NewRecorder()

	(&Server{}).listRunSnapshotsHTTP(recorder, request.WithContext(ctx))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeHTTPError(t, recorder.Body.Bytes())
	if response.Code != "permission_required" {
		t.Fatalf("unexpected response: %s", recorder.Body.String())
	}
}
