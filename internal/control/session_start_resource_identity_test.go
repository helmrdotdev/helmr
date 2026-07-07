package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestSessionStartExternalIDReusesExistingSessionWithoutCoordination(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, RunEnqueuer: runEnqueuer})

	bodyBytes, err := json.Marshal(api.SessionStartRequest{
		TaskID:     "deploy",
		ExternalID: "durable-1",
		Payload:    json.RawMessage(`{"env":"prod"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var first api.SessionStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}

	store.currentDeploymentMissing = true
	req = httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var second api.SessionStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.Session.ID != first.Session.ID || second.Run.ID != first.Run.ID || !second.IsCached {
		t.Fatalf("second response = %+v first=%+v", second, first)
	}
	if len(store.events) != 1 || runEnqueuer.count != 1 {
		t.Fatalf("events=%d enqueues=%d", len(store.events), runEnqueuer.count)
	}
}

func TestSessionStartExternalIDRejectsDifferentPayload(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}})

	firstBody, err := json.Marshal(api.SessionStartRequest{
		TaskID:     "deploy",
		ExternalID: "durable-1",
		Payload:    json.RawMessage(`{"env":"prod"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(firstBody))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d body=%s", rec.Code, rec.Body.String())
	}

	secondBody, err := json.Marshal(api.SessionStartRequest{
		TaskID:     "deploy",
		ExternalID: "durable-1",
		Payload:    json.RawMessage(`{"env":"staging"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(secondBody))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second create status = %d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "session_fingerprint_mismatch")
}

func TestSessionStartDoesNotMaterializeDeploymentStreamsForSession(t *testing.T) {
	store := &fakeStore{
		deploymentStreams: []db.DeploymentStream{
			{
				ID:                pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000401")),
				OrgID:             pgvalue.UUID(dbtest.DefaultOrgID),
				ProjectID:         testProjectID(),
				EnvironmentID:     testEnvironmentID(),
				DeploymentID:      testDeploymentID(),
				Name:              "runtime-smoke.progress",
				Direction:         db.StreamDirectionOutput,
				SchemaFingerprint: "sha256:progress",
				SchemaJson:        []byte(`{"kind":"test"}`),
				Metadata:          []byte(`{}`),
			},
			{
				ID:                pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000402")),
				OrgID:             pgvalue.UUID(dbtest.DefaultOrgID),
				ProjectID:         testProjectID(),
				EnvironmentID:     testEnvironmentID(),
				DeploymentID:      testDeploymentID(),
				Name:              "runtime-smoke.report",
				Direction:         db.StreamDirectionOutput,
				SchemaFingerprint: "sha256:report",
				SchemaJson:        []byte(`{"kind":"test"}`),
				Metadata:          []byte(`{}`),
			},
		},
	}
	runEnqueuer := &fakeRunEnqueuer{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, RunEnqueuer: runEnqueuer})

	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader([]byte(`{"task_id":"deploy","payload":{"env":"prod"}}`)))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.ensuredSessionStreams) != 0 {
		t.Fatalf("ensured session streams = %d, want 0", len(store.ensuredSessionStreams))
	}
}

func TestSessionStartFingerprintIncludesExpiresAt(t *testing.T) {
	payload := json.RawMessage(`{"env":"prod"}`)
	firstExpiresAt := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	secondExpiresAt := firstExpiresAt.Add(time.Hour)
	first, err := sessionStartRequestFingerprint("deploy", payload, api.SessionStartOptions{}, "durable-1", &firstExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessionStartRequestFingerprint("deploy", payload, api.SessionStartOptions{}, "durable-1", &secondExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if first.String == second.String {
		t.Fatal("fingerprint did not change when expiresAt changed")
	}
}

func TestSessionStartFingerprintCanonicalizesRetryPolicy(t *testing.T) {
	payload := json.RawMessage(`{"env":"prod"}`)
	first, err := sessionStartRequestFingerprint("deploy", payload, api.SessionStartOptions{Retry: json.RawMessage(`{"maxAttempts":3,"backoff":{"minMs":1000,"maxMs":60000}}`)}, "durable-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessionStartRequestFingerprint("deploy", payload, api.SessionStartOptions{Retry: json.RawMessage(`{"backoff":{"maxMs":60000,"minMs":1000},"maxAttempts":3}`)}, "durable-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.String != second.String {
		t.Fatalf("fingerprints differ for semantically equal retry JSON: %s != %s", first.String, second.String)
	}
}

func TestSessionStartFingerprintIncludesMetadataAndTags(t *testing.T) {
	payload := json.RawMessage(`{"env":"prod"}`)
	first, err := sessionStartRequestFingerprint("deploy", payload, api.SessionStartOptions{
		Metadata: json.RawMessage(`{"ticket":"123"}`),
		Tags:     []string{"prod"},
	}, "durable-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessionStartRequestFingerprint("deploy", payload, api.SessionStartOptions{
		Metadata: json.RawMessage(`{"ticket":"456"}`),
		Tags:     []string{"prod"},
	}, "durable-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	third, err := sessionStartRequestFingerprint("deploy", payload, api.SessionStartOptions{
		Metadata: json.RawMessage(`{"ticket":"123"}`),
		Tags:     []string{"staging"},
	}, "durable-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.String == second.String {
		t.Fatal("fingerprint did not change when metadata changed")
	}
	if first.String == third.String {
		t.Fatal("fingerprint did not change when tags changed")
	}
}

func TestCreateScheduleRunRejectsStaleTrigger(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	server := &Server{
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:              store,
		cas:             &fakeCAS{},
		secrets:         fakeSecrets{},
		runEnqueuer:     runEnqueuer,
		workerGroupID:   "us-east-1-worker-group-1",
		defaultRegionID: "us-east-1",
	}
	row := db.GetScheduleTriggerCandidateRow{
		OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:     testProjectID(),
		EnvironmentID: testEnvironmentID(),
		ScheduleID:    pgvalue.UUID(uuid.Must(uuid.NewV7())),
		InstanceID:    pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ScheduleType:  db.TaskScheduleTypeImperative,
		TaskID:        "deploy",
		Cron:          "0 9 * * *",
		Timezone:      "UTC",
		RunOptions:    []byte(`{}`),
		Generation:    1,
		NextFireAt:    pgtype.Timestamptz{Time: time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC), Valid: true},
	}

	if _, err := server.CreateScheduleRun(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	firstRunID := store.run.ID
	secondRunID, err := server.CreateScheduleRun(context.Background(), row)
	if err != nil {
		t.Fatal(err)
	}
	if secondRunID != firstRunID {
		t.Fatalf("second schedule run id = %s, want %s", pgvalue.UUIDString(secondRunID), pgvalue.UUIDString(firstRunID))
	}
	if len(store.events) != 1 || runEnqueuer.count != 1 {
		t.Fatalf("after duplicate fire events=%d enqueues=%d", len(store.events), runEnqueuer.count)
	}
	store.scheduleTriggerNotCurrent = true
	if _, err := server.CreateScheduleRun(context.Background(), row); !errors.Is(err, schedule.ErrTriggerSuperseded) {
		t.Fatalf("second schedule run err = %v, want trigger superseded", err)
	}
	if len(store.events) != 1 || runEnqueuer.count != 1 {
		t.Fatalf("events=%d enqueues=%d", len(store.events), runEnqueuer.count)
	}
}

func TestWorkerReleaseAllowsIdempotentRetryAfterQueueLeaseGone(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	sessionID := uuid.Must(uuid.NewV7())
	workerID := uuid.Must(uuid.NewV7())
	exitCode := int32(0)
	store := &fakeStore{
		run: db.Run{
			ID:               pgvalue.UUID(runID),
			OrgID:            pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:        testProjectID(),
			EnvironmentID:    testEnvironmentID(),
			DeploymentID:     testDeploymentID(),
			DeploymentTaskID: testDeploymentTaskID(),
			TaskID:           "deploy",
			Status:           db.RunStatusSucceeded,
			ExitCode:         pgtype.Int4{Int32: exitCode, Valid: true},
			CreatedAt:        testTime(),
			UpdatedAt:        testTime(),
			StartedAt:        testTime(),
			FinishedAt:       testTime(),
		},
		sessionID:                 pgvalue.UUID(sessionID),
		executionWorkerInstanceID: pgvalue.UUID(workerID),
		executionLeaseExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
		activeQueueLeaseMissing:   true,
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, WorkerTokenSecret: []byte(testWorkerTokenSecret), WorkerTokenTTL: time.Hour})
	workerBearer := mintTestWorkerToken(t, server, workerID.String())
	body, err := json.Marshal(api.WorkerReleaseRequest{
		Lease: api.WorkerRunLease{
			ID:                sessionID.String(),
			OrgID:             dbtest.DefaultOrgID.String(),
			RunID:             runID.String(),
			WorkerInstanceID:  workerID.String(),
			ProtocolVersion:   api.CurrentWorkerProtocolVersion,
			AttemptNumber:     1,
			DispatchMessageID: "message-1",
			DispatchLeaseID:   "lease-1",
			ExpiresAt:         time.Now().Add(time.Minute),
		},
		Result: api.WorkerReleaseResult{Kind: "completed", ExitCode: &exitCode},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/release", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("release status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.ackedLeases) != 0 {
		t.Fatalf("acked leases = %+v", store.ackedLeases)
	}
	if len(store.events) != 0 {
		t.Fatalf("events = %+v", store.events)
	}
}
