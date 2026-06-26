package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

func TestCreateRunReturnsExistingRunForActiveIdempotencyKey(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, RunEnqueuer: runEnqueuer, EventStream: newTestEventStream(t)})

	bodyBytes, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.SessionStartOptions{IdempotencyKey: "deploy-prod", IdempotencyKeyTTL: "24h"},
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
	if len(store.startIdempotency.IdempotencyKey) != sha256.Size*2 {
		t.Fatalf("stored idempotency key = %q", store.startIdempotency.IdempotencyKey)
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
	if second.Run.ID != first.Run.ID || !second.IsCached {
		t.Fatalf("second response = %+v first=%+v", second, first)
	}
	if len(store.events) != 1 || runEnqueuer.count != 1 {
		t.Fatalf("events=%d enqueues=%d", len(store.events), runEnqueuer.count)
	}
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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, RunEnqueuer: runEnqueuer, EventStream: newTestEventStream(t)})

	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"task_id":"deploy","payload":{"env":"prod"}}`))
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

func TestCreateRunRequiresCoordinationForIdempotencyKey(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}, RunEnqueuer: runEnqueuer})

	bodyBytes, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.SessionStartOptions{IdempotencyKey: "deploy-prod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "coordination_unavailable") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "coordination_unavailable")
	if store.run.ID.Valid || store.session.ID.Valid || len(store.events) != 0 || runEnqueuer.count != 0 {
		t.Fatalf("side effects: run=%v session=%v events=%d enqueues=%d", store.run.ID.Valid, store.session.ID.Valid, len(store.events), runEnqueuer.count)
	}
}

func TestCreateRunRequiresCoordinationBeforeBindingExistingExternalIDToIdempotencyKey(t *testing.T) {
	payload := json.RawMessage(`{"env":"prod"}`)
	options := sessionStartFingerprintTestOptions(t, api.CreateRunOptions{IdempotencyKey: "durable-key"})
	startFingerprint, err := sessionStartRequestFingerprint("deploy", payload, options, "durable-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeStore{
		session: db.Session{
			ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			TaskID:              "deploy",
			InitialDeploymentID: testDeploymentID(),
			ActiveDeploymentID:  testDeploymentID(),
			ExternalID:          "durable-1",
			StartFingerprint:    startFingerprint.String,
			Status:              db.SessionStatusOpen,
			CurrentRunID:        runID,
			Metadata:            []byte(`{}`),
			Tags:                []string{},
			TerminalReason:      []byte(`{}`),
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
		run: db.Run{
			ID:               runID,
			OrgID:            pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:        testProjectID(),
			EnvironmentID:    testEnvironmentID(),
			DeploymentID:     testDeploymentID(),
			DeploymentTaskID: testDeploymentTaskID(),
			TaskID:           "deploy",
			Status:           db.RunStatusQueued,
			ExecutionStatus:  db.RunExecutionStatusQueued,
			CreatedAt:        testTime(),
			UpdatedAt:        testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}})

	bodyBytes, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
		ExternalID: "durable-1",
		Payload:    payload,
		Options:    api.SessionStartOptions{IdempotencyKey: "durable-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "coordination_unavailable")
	if store.startIdempotency.ID.Valid {
		t.Fatalf("start idempotency binding was written without Redis coordination: %+v", store.startIdempotency)
	}
}

func TestSessionStartExternalIDDoesNotRequireCoordination(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}})

	bodyBytes, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
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
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !store.session.ID.Valid || !store.run.ID.Valid {
		t.Fatalf("expected session and run to be created, got session=%v run=%v", store.session.ID.Valid, store.run.ID.Valid)
	}
}

func TestSessionStartFingerprintIncludesExpiresAt(t *testing.T) {
	payload := json.RawMessage(`{"env":"prod"}`)
	firstExpiresAt := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	secondExpiresAt := firstExpiresAt.Add(time.Hour)
	first, err := sessionStartRequestFingerprint("deploy", payload, sessionStartFingerprintTestOptions(t, api.CreateRunOptions{}), "durable-1", &firstExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessionStartRequestFingerprint("deploy", payload, sessionStartFingerprintTestOptions(t, api.CreateRunOptions{}), "durable-1", &secondExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if first.String == second.String {
		t.Fatal("fingerprint did not change when expiresAt changed")
	}
}

func TestSessionStartFingerprintCanonicalizesRetryPolicy(t *testing.T) {
	payload := json.RawMessage(`{"env":"prod"}`)
	firstOptions := sessionStartFingerprintTestOptions(t, api.CreateRunOptions{Retry: json.RawMessage(`{"maxAttempts":3,"backoff":{"minMs":1000,"maxMs":60000}}`)})
	first, err := sessionStartRequestFingerprint("deploy", payload, firstOptions, "durable-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	secondOptions := sessionStartFingerprintTestOptions(t, api.CreateRunOptions{Retry: json.RawMessage(`{"backoff":{"maxMs":60000,"minMs":1000},"maxAttempts":3}`)})
	second, err := sessionStartRequestFingerprint("deploy", payload, secondOptions, "durable-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.String != second.String {
		t.Fatalf("fingerprints differ for semantically equal retry JSON: %s != %s", first.String, second.String)
	}
}

func TestCreateRunReturnsIdempotencyHitForTerminalSession(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, RunEnqueuer: runEnqueuer, EventStream: newTestEventStream(t)})

	bodyBytes, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.SessionStartOptions{IdempotencyKey: "deploy-prod"},
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
	store.session.Status = db.SessionStatusOpen
	store.startIdempotency.SessionStatus = db.SessionStatusOpen

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
	if second.Run.ID != first.Run.ID || !second.IsCached || second.Session.Status != string(db.SessionStatusOpen) {
		t.Fatalf("second response = %+v first=%+v", second, first)
	}
	if len(store.events) != 1 || runEnqueuer.count != 1 {
		t.Fatalf("events=%d enqueues=%d", len(store.events), runEnqueuer.count)
	}
}

func TestCreateScheduleRunRejectsStaleTriggerIdempotencyHit(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	server := &Server{
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:          store,
		cas:         &fakeCAS{},
		secrets:     fakeSecrets{},
		runEnqueuer: runEnqueuer,
		eventStream: newTestEventStream(t),
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
	store.scheduleTriggerNotCurrent = true
	if _, err := server.CreateScheduleRun(context.Background(), row); !errors.Is(err, schedule.ErrTriggerSuperseded) {
		t.Fatalf("second schedule run err = %v, want trigger superseded", err)
	}
	if len(store.events) != 1 || runEnqueuer.count != 1 {
		t.Fatalf("events=%d enqueues=%d", len(store.events), runEnqueuer.count)
	}
}

func TestCreateScheduleRunDefersSessionStartCoordinationFailures(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	server := &Server{
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:          store,
		secrets:     fakeSecrets{},
		runEnqueuer: runEnqueuer,
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

	_, err := server.CreateScheduleRun(context.Background(), row)
	if !errors.Is(err, schedule.ErrTriggerDeferred) || !errors.Is(err, errSessionStartCoordinationUnavailable) {
		t.Fatalf("schedule run err = %v, want deferred coordination error", err)
	}
	if len(store.events) != 0 || runEnqueuer.count != 0 {
		t.Fatalf("events=%d enqueues=%d", len(store.events), runEnqueuer.count)
	}
}

func TestCreateRunDoesNotDuplicateWhenResolvedClaimHasNoVisibleIdempotencyRow(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	eventStream := &EventStream{log: slog.New(slog.NewTextHandler(io.Discard, nil)), redis: redisClient}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, RunEnqueuer: runEnqueuer, EventStream: eventStream})

	runOptions := api.CreateRunOptions{IdempotencyKey: "deploy-prod"}
	idempotency, err := normalizeRunIdempotency(runOptions)
	if err != nil {
		t.Fatal(err)
	}
	key := sessionStartClaimKey(dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "idempotency", idempotency.key.String)
	if err := redisClient.Set(context.Background(), key, "resolved:owner", sessionStartClaimResolvedTTL).Err(); err != nil {
		t.Fatal(err)
	}
	bodyBytes, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.SessionStartOptions{IdempotencyKey: "deploy-prod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !store.createRun.ID.Valid || len(store.events) != 1 || runEnqueuer.count != 1 {
		t.Fatalf("run/event/enqueue = %+v/%d/%d, want created once", store.createRun.ID, len(store.events), runEnqueuer.count)
	}
	value, err := redisClient.Get(context.Background(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if value == "resolved:owner" || !strings.HasPrefix(value, "resolved:") {
		t.Fatalf("claim value = %q, want fresh resolved owner", value)
	}
}

func TestCreateRunPendingStartReturnsAccepted(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	eventStream := &EventStream{log: slog.New(slog.NewTextHandler(io.Discard, nil)), redis: redisClient}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, RunEnqueuer: runEnqueuer, EventStream: eventStream})

	runOptions := api.CreateRunOptions{IdempotencyKey: "deploy-prod"}
	idempotency, err := normalizeRunIdempotency(runOptions)
	if err != nil {
		t.Fatal(err)
	}
	key := sessionStartClaimKey(dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "idempotency", idempotency.key.String)
	if err := redisClient.Set(context.Background(), key, "pending:owner", sessionStartClaimTTL).Err(); err != nil {
		t.Fatal(err)
	}
	bodyBytes, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.SessionStartOptions{IdempotencyKey: "deploy-prod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Body.String(), "session_start_pending") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "idempotency_pending")
	if store.createRun.ID.Valid || len(store.events) != 0 || runEnqueuer.count != 0 {
		t.Fatalf("side effects: run=%+v events=%d enqueues=%d", store.createRun.ID, len(store.events), runEnqueuer.count)
	}
}

func TestCreateRunReleasesStartClaimAfterCreationFailure(t *testing.T) {
	store := &fakeStore{createRunErr: errors.New("transient create run failure")}
	runEnqueuer := &fakeRunEnqueuer{}
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	eventStream := &EventStream{log: slog.New(slog.NewTextHandler(io.Discard, nil)), redis: redisClient}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, RunEnqueuer: runEnqueuer, EventStream: eventStream})

	bodyBytes, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.SessionStartOptions{IdempotencyKey: "deploy-prod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("first status = %d body=%s", rec.Code, rec.Body.String())
	}

	store.createRunErr = nil
	req = httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("second status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.events) != 1 || runEnqueuer.count != 1 {
		t.Fatalf("events=%d enqueues=%d", len(store.events), runEnqueuer.count)
	}
}

func TestCreateRunRejectsIdempotencyKeyReuseWithDifferentRequest(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})

	firstBody, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.SessionStartOptions{IdempotencyKey: "deploy"},
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

	secondBody, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
		Payload: json.RawMessage(`{"env":"staging"}`),
		Options: api.SessionStartOptions{IdempotencyKey: "deploy"},
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
	requireErrorCode(t, rec.Body.Bytes(), "idempotency_fingerprint_mismatch")
	if len(store.events) != 1 {
		t.Fatalf("events = %d", len(store.events))
	}
}

func TestCreateRunBindsIdempotencyKeyWhenExternalIDReusesSession(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, RunEnqueuer: runEnqueuer, EventStream: newTestEventStream(t)})

	firstBody, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
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
	firstRunID := store.run.ID

	secondBody, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
		ExternalID: "durable-1",
		Payload:    json.RawMessage(`{"env":"prod"}`),
		Options:    api.SessionStartOptions{IdempotencyKey: "durable-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(secondBody))
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
	if second.Run.ID != pgvalue.MustUUIDValue(firstRunID).String() || !second.IsCached {
		t.Fatalf("second response = %+v, want cached run %s", second, pgvalue.MustUUIDValue(firstRunID))
	}
	if !store.startIdempotency.ID.Valid || store.startIdempotency.SessionID != store.session.ID || store.startIdempotency.FirstRunID != firstRunID {
		t.Fatalf("stored idempotency = %+v session=%s run=%s", store.startIdempotency, pgvalue.MustUUIDValue(store.session.ID), pgvalue.MustUUIDValue(firstRunID))
	}

	thirdBody, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.SessionStartOptions{IdempotencyKey: "durable-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(thirdBody))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("third create status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.events) != 1 || runEnqueuer.count != 1 {
		t.Fatalf("events=%d enqueues=%d", len(store.events), runEnqueuer.count)
	}
}

func TestCreateRunBindsIdempotencyKeyAfterExternalIDUniqueRace(t *testing.T) {
	payload := json.RawMessage(`{"env":"prod"}`)
	startFingerprint, err := sessionStartRequestFingerprint("deploy", payload, sessionStartFingerprintTestOptions(t, api.CreateRunOptions{IdempotencyKey: "durable-key"}), "durable-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeStore{
		createSessionErr:             &pgconn.PgError{Code: "23505"},
		getSessionByExternalIDMisses: 1,
		session: db.Session{
			ID:                  sessionID,
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			TaskID:              "deploy",
			InitialDeploymentID: testDeploymentID(),
			ActiveDeploymentID:  testDeploymentID(),
			ExternalID:          "durable-1",
			StartFingerprint:    startFingerprint.String,
			Status:              db.SessionStatusOpen,
			CurrentRunID:        runID,
			Metadata:            []byte(`{}`),
			Tags:                []string{},
			TerminalReason:      []byte(`{}`),
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
		run: db.Run{
			ID:               runID,
			OrgID:            pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:        testProjectID(),
			EnvironmentID:    testEnvironmentID(),
			DeploymentID:     testDeploymentID(),
			DeploymentTaskID: testDeploymentTaskID(),
			TaskID:           "deploy",
			Status:           db.RunStatusQueued,
			ExecutionStatus:  db.RunExecutionStatusQueued,
			CreatedAt:        testTime(),
			UpdatedAt:        testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	bodyBytes, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
		ExternalID: "durable-1",
		Payload:    payload,
		Options:    api.SessionStartOptions{IdempotencyKey: "durable-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !store.startIdempotency.ID.Valid || store.startIdempotency.SessionID != sessionID || store.startIdempotency.FirstRunID != runID {
		t.Fatalf("idempotency binding = %+v, want session %s run %s", store.startIdempotency, pgvalue.MustUUIDValue(sessionID), pgvalue.MustUUIDValue(runID))
	}
}

func TestCreateRunReclaimsExpiredStartIdempotency(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, RunEnqueuer: runEnqueuer, EventStream: newTestEventStream(t)})

	bodyBytes, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.SessionStartOptions{IdempotencyKey: "deploy-prod", IdempotencyKeyTTL: "1s"},
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
	firstID := store.run.ID
	store.run.Status = db.RunStatusExpired
	store.startIdempotency.ExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(-time.Second), Valid: true}

	req = httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("second create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var second api.SessionStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.Run.ID == first.Run.ID || second.Run.ID == pgvalue.MustUUIDValue(firstID).String() || second.IsCached {
		t.Fatalf("second response = %+v first=%+v", second, first)
	}
	if len(store.events) != 2 || runEnqueuer.count != 2 {
		t.Fatalf("events=%d enqueues=%d", len(store.events), runEnqueuer.count)
	}
}

func TestCreateRunClearsExpiredRunIdempotencyKey(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, RunEnqueuer: runEnqueuer, EventStream: newTestEventStream(t)})

	bodyBytes, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.SessionStartOptions{IdempotencyKey: "deploy-prod", IdempotencyKeyTTL: "24h", TTL: "1s"},
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
	firstID := store.run.ID
	store.run.Status = db.RunStatusExpired
	store.startIdempotency.ExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(-time.Second), Valid: true}

	req = httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("second create status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.run.ID == firstID {
		t.Fatalf("run id was reused after expired idempotency clear")
	}
	if runEnqueuer.count != 2 || len(store.events) != 2 {
		t.Fatalf("events=%d enqueues=%d", len(store.events), runEnqueuer.count)
	}
}

func TestCreateRunHashesLiteralHexIdempotencyKeys(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})

	rawKey := strings.Repeat("a", sha256.Size*2)
	bodyBytes, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.SessionStartOptions{IdempotencyKey: rawKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	digest := sha256.Sum256([]byte(rawKey))
	if got, want := store.startIdempotency.IdempotencyKey, hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("stored key = %s, want %s", got, want)
	}
}

func sessionStartFingerprintTestOptions(t *testing.T, options api.CreateRunOptions) api.SessionStartOptions {
	t.Helper()
	return api.SessionStartOptions{
		Queue:              options.Queue,
		ConcurrencyKey:     options.ConcurrencyKey,
		Priority:           options.Priority,
		TTL:                options.TTL,
		MaxDurationSeconds: options.MaxDurationSeconds,
		Retry:              options.Retry,
		Metadata:           options.Metadata,
		Tags:               options.Tags,
		IdempotencyKey:     options.IdempotencyKey,
		IdempotencyKeyTTL:  options.IdempotencyKeyTTL,
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
