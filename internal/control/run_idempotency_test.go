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

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCreateRunReturnsExistingRunForActiveIdempotencyKey(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}, RunEnqueuer: runEnqueuer})

	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:  "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.CreateRunOptions{IdempotencyKey: "deploy-prod", IdempotencyKeyTTL: "24h"},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var first api.RunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if !store.run.IdempotencyKey.Valid || len(store.run.IdempotencyKey.String) != sha256.Size*2 {
		t.Fatalf("stored idempotency key = %+v", store.run.IdempotencyKey)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var second api.RunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || !second.IdempotencyHit {
		t.Fatalf("second response = %+v first=%+v", second, first)
	}
	if len(store.events) != 1 || runEnqueuer.count != 1 {
		t.Fatalf("events=%d enqueues=%d", len(store.events), runEnqueuer.count)
	}
}

func TestCreateRunRejectsIdempotencyKeyReuseWithDifferentRequest(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}})

	firstBody, err := json.Marshal(api.CreateRunRequest{
		TaskID:  "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.CreateRunOptions{IdempotencyKey: "deploy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(firstBody))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d body=%s", rec.Code, rec.Body.String())
	}

	secondBody, err := json.Marshal(api.CreateRunRequest{
		TaskID:  "deploy",
		Payload: json.RawMessage(`{"env":"staging"}`),
		Options: api.CreateRunOptions{IdempotencyKey: "deploy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(secondBody))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second create status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.events) != 1 {
		t.Fatalf("events = %d", len(store.events))
	}
}

func TestCreateRunReturnsActiveRunEvenWhenIdempotencyTTLExpired(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}, RunEnqueuer: runEnqueuer})

	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:  "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.CreateRunOptions{IdempotencyKey: "deploy-prod", IdempotencyKeyTTL: "1s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var first api.RunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	store.run.Status = db.RunStatusRunning
	store.run.IdempotencyKeyExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(-time.Second), Valid: true}

	req = httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var second api.RunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Status != "running" || !second.IdempotencyHit {
		t.Fatalf("second response = %+v first=%+v", second, first)
	}
	if len(store.events) != 1 || runEnqueuer.count != 1 {
		t.Fatalf("events=%d enqueues=%d", len(store.events), runEnqueuer.count)
	}
}

func TestCreateRunClearsExpiredRunIdempotencyKey(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}, RunEnqueuer: runEnqueuer})

	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:  "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.CreateRunOptions{IdempotencyKey: "deploy-prod", IdempotencyKeyTTL: "24h", TTL: "1s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d body=%s", rec.Code, rec.Body.String())
	}
	firstID := store.run.ID
	store.run.Status = db.RunStatusExpired

	req = httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
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

func TestExistingIdempotentRunKeepsScheduledTerminalRun(t *testing.T) {
	runID := ids.ToPG(ids.New())
	store := &fakeStore{}
	store.run = db.Run{
		ID:                     runID,
		OrgID:                  ids.ToPG(dbtest.DefaultOrgID),
		ProjectID:              testProjectID(),
		EnvironmentID:          testEnvironmentID(),
		DeploymentID:           testDeploymentID(),
		DeploymentTaskID:       testDeploymentTaskID(),
		TaskID:                 "deploy",
		Status:                 db.RunStatusFailed,
		IdempotencyKey:         pgtype.Text{String: "schedule-key", Valid: true},
		IdempotencyRequestHash: pgtype.Text{String: "request-hash", Valid: true},
		CreatedAt:              testTime(),
		UpdatedAt:              testTime(),
	}
	server := &Server{db: store}

	existing, hit, err := server.existingIdempotentRun(
		context.Background(),
		dbtest.DefaultOrgID,
		testProjectID(),
		testEnvironmentID(),
		"deploy",
		"schedule-key",
		"request-hash",
		runSource{},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !hit || existing.ID != runID {
		t.Fatalf("existing=%+v hit=%v", existing, hit)
	}
	if !store.run.IdempotencyKey.Valid {
		t.Fatal("scheduled idempotency key was cleared")
	}
}

func TestExistingIdempotentRunAllowsScheduledHashMismatch(t *testing.T) {
	runID := ids.ToPG(ids.New())
	scheduleID := ids.ToPG(ids.New())
	scheduleInstanceID := ids.ToPG(ids.New())
	scheduledAt := pgtype.Timestamptz{Time: testTime().Time.Add(time.Minute), Valid: true}
	store := &fakeStore{}
	store.run = db.Run{
		ID:                     runID,
		OrgID:                  ids.ToPG(dbtest.DefaultOrgID),
		ProjectID:              testProjectID(),
		EnvironmentID:          testEnvironmentID(),
		DeploymentID:           testDeploymentID(),
		DeploymentTaskID:       testDeploymentTaskID(),
		TaskID:                 "deploy",
		Status:                 db.RunStatusQueued,
		IdempotencyKey:         pgtype.Text{String: "schedule-key", Valid: true},
		IdempotencyRequestHash: pgtype.Text{String: "previous-hash", Valid: true},
		ScheduleID:             scheduleID,
		ScheduleInstanceID:     scheduleInstanceID,
		ScheduledAt:            scheduledAt,
		CreatedAt:              testTime(),
		UpdatedAt:              testTime(),
	}
	server := &Server{db: store}

	existing, hit, err := server.existingIdempotentRun(
		context.Background(),
		dbtest.DefaultOrgID,
		testProjectID(),
		testEnvironmentID(),
		"deploy",
		"schedule-key",
		"new-hash",
		runSource{
			scheduleID:         scheduleID,
			scheduleInstanceID: scheduleInstanceID,
			scheduledAt:        scheduledAt,
		},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !hit || existing.ID != runID {
		t.Fatalf("existing=%+v hit=%v", existing, hit)
	}
}

func TestExistingIdempotentRunRejectsScheduledSourceMismatch(t *testing.T) {
	store := &fakeStore{}
	store.run = db.Run{
		ID:                     ids.ToPG(ids.New()),
		OrgID:                  ids.ToPG(dbtest.DefaultOrgID),
		ProjectID:              testProjectID(),
		EnvironmentID:          testEnvironmentID(),
		DeploymentID:           testDeploymentID(),
		DeploymentTaskID:       testDeploymentTaskID(),
		TaskID:                 "deploy",
		Status:                 db.RunStatusQueued,
		IdempotencyKey:         pgtype.Text{String: "schedule-key", Valid: true},
		IdempotencyRequestHash: pgtype.Text{String: "previous-hash", Valid: true},
		ScheduleID:             ids.ToPG(ids.New()),
		ScheduleInstanceID:     ids.ToPG(ids.New()),
		ScheduledAt:            pgtype.Timestamptz{Time: testTime().Time.Add(time.Minute), Valid: true},
		CreatedAt:              testTime(),
		UpdatedAt:              testTime(),
	}
	server := &Server{db: store}

	_, _, err := server.existingIdempotentRun(
		context.Background(),
		dbtest.DefaultOrgID,
		testProjectID(),
		testEnvironmentID(),
		"deploy",
		"schedule-key",
		"new-hash",
		runSource{
			scheduleID:         ids.ToPG(ids.New()),
			scheduleInstanceID: ids.ToPG(ids.New()),
			scheduledAt:        pgtype.Timestamptz{Time: testTime().Time.Add(2 * time.Minute), Valid: true},
		},
		false,
	)
	if !errors.Is(err, errIdempotencyKeyConflict) {
		t.Fatalf("err = %v, want idempotency conflict", err)
	}
}

func TestCreateRunHashesLiteralHexIdempotencyKeys(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}})

	rawKey := strings.Repeat("a", sha256.Size*2)
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:  "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.CreateRunOptions{IdempotencyKey: rawKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	digest := sha256.Sum256([]byte(rawKey))
	if got, want := store.run.IdempotencyKey.String, hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("stored key = %s, want %s", got, want)
	}
}

func TestRunIdempotencyRequestHashIncludesEffectiveRunTarget(t *testing.T) {
	request := api.CreateRunRequest{
		TaskID:  "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
	}
	payload := json.RawMessage(`{"env":"prod"}`)
	deploymentTask := db.GetDeploymentTaskRow{
		ID:                     testDeploymentTaskID(),
		DeploymentID:           testDeploymentID(),
		BundleDigest:           "sha256:" + strings.Repeat("b", 64),
		FilePath:               "tasks/deploy.ts",
		ExportName:             "deploy",
		DeploymentSourceDigest: "sha256:" + strings.Repeat("a", 64),
	}
	scheduling := runScheduling{queueName: "task/deploy", ttl: "10m"}
	retryPolicy := []byte("false")
	metadata := []byte("{}")
	tags := []string{}

	base, err := runIdempotencyRequestHash(request, payload, deploymentTask, 300, retryPolicy, metadata, tags, scheduling)
	if err != nil {
		t.Fatal(err)
	}
	requireIdempotencyHashChanged := func(name string, got pgtype.Text) {
		t.Helper()
		if got.String == base.String {
			t.Fatalf("%s did not affect idempotency request hash", name)
		}
	}
	changedTask := deploymentTask
	changedTask.DeploymentID = ids.ToPG(ids.New())
	deploymentHash, err := runIdempotencyRequestHash(request, payload, changedTask, 300, retryPolicy, metadata, tags, scheduling)
	if err != nil {
		t.Fatal(err)
	}
	requireIdempotencyHashChanged("effective deployment", deploymentHash)
	durationHash, err := runIdempotencyRequestHash(request, payload, deploymentTask, 600, retryPolicy, metadata, tags, scheduling)
	if err != nil {
		t.Fatal(err)
	}
	requireIdempotencyHashChanged("max duration", durationHash)
	changedScheduling := scheduling
	changedScheduling.queueName = "task/deploy-high"
	queueHash, err := runIdempotencyRequestHash(request, payload, deploymentTask, 300, retryPolicy, metadata, tags, changedScheduling)
	if err != nil {
		t.Fatal(err)
	}
	requireIdempotencyHashChanged("queue name", queueHash)
	changedScheduling = scheduling
	changedScheduling.concurrencyKey = pgvalue.Text("deploy:prod")
	concurrencyHash, err := runIdempotencyRequestHash(request, payload, deploymentTask, 300, retryPolicy, metadata, tags, changedScheduling)
	if err != nil {
		t.Fatal(err)
	}
	requireIdempotencyHashChanged("concurrency key", concurrencyHash)
	changedScheduling = scheduling
	changedScheduling.priority = 100
	priorityHash, err := runIdempotencyRequestHash(request, payload, deploymentTask, 300, retryPolicy, metadata, tags, changedScheduling)
	if err != nil {
		t.Fatal(err)
	}
	requireIdempotencyHashChanged("priority", priorityHash)
	changedScheduling = scheduling
	changedScheduling.ttl = "30m"
	ttlHash, err := runIdempotencyRequestHash(request, payload, deploymentTask, 300, retryPolicy, metadata, tags, changedScheduling)
	if err != nil {
		t.Fatal(err)
	}
	requireIdempotencyHashChanged("ttl", ttlHash)
}

func TestWorkerReleaseAllowsIdempotentRetryAfterQueueLeaseGone(t *testing.T) {
	runID := ids.New()
	sessionID := ids.New()
	workerID := ids.New()
	exitCode := int32(0)
	store := &fakeStore{
		run: db.Run{
			ID:               ids.ToPG(runID),
			OrgID:            ids.ToPG(dbtest.DefaultOrgID),
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
		sessionID:                 ids.ToPG(sessionID),
		executionWorkerInstanceID: ids.ToPG(workerID),
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
	req := httptest.NewRequest(http.MethodPost, "/api/worker/sessions/release", bytes.NewReader(body))
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

func (f *fakeStore) GetScopedRunByIdempotencyKey(_ context.Context, arg db.GetScopedRunByIdempotencyKeyParams) (db.GetScopedRunByIdempotencyKeyRow, error) {
	if !f.run.ID.Valid || !f.run.IdempotencyKey.Valid || f.run.IdempotencyKey.String != arg.IdempotencyKey.String || f.run.TaskID != arg.TaskID {
		return db.GetScopedRunByIdempotencyKeyRow{}, pgx.ErrNoRows
	}
	return db.GetScopedRunByIdempotencyKeyRow{
		ID:                      f.run.ID,
		OrgID:                   f.run.OrgID,
		ProjectID:               f.run.ProjectID,
		EnvironmentID:           f.run.EnvironmentID,
		DeploymentID:            f.run.DeploymentID,
		DeploymentTaskID:        f.run.DeploymentTaskID,
		TaskID:                  f.run.TaskID,
		Status:                  f.run.Status,
		ExitCode:                f.run.ExitCode,
		Output:                  f.run.Output,
		CreatedAt:               f.run.CreatedAt,
		UpdatedAt:               f.run.UpdatedAt,
		IdempotencyKeyExpiresAt: f.run.IdempotencyKeyExpiresAt,
		IdempotencyRequestHash:  f.run.IdempotencyRequestHash,
		ScheduleID:              f.run.ScheduleID,
		ScheduleInstanceID:      f.run.ScheduleInstanceID,
		ScheduledAt:             f.run.ScheduledAt,
	}, nil
}

func (f *fakeStore) ClearRunIdempotencyKey(_ context.Context, arg db.ClearRunIdempotencyKeyParams) error {
	if f.run.ID == arg.ID {
		f.run.IdempotencyKey = pgtype.Text{}
		f.run.IdempotencyKeyExpiresAt = pgtype.Timestamptz{}
		f.run.IdempotencyKeyOptions = nil
		f.run.IdempotencyRequestHash = pgtype.Text{}
	}
	return nil
}
