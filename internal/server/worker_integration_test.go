package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/runqueue"
	"github.com/helmrdotdev/helmr/internal/runqueue/publisher"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkerHTTPRejectsDetachedExecutionWritesWithPostgres(t *testing.T) {
	ctx := context.Background()
	queries, pool := newServerPostgresTestDB(t, ctx)
	runQueue := newTestRunQueue()
	run := seedServerQueuedRun(t, ctx, queries, runQueue)
	handler := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(queries),
		WithRunQueue(runQueue),
		WithAuthenticator(fakeAuth{}),
		WithGitHubResolver(fakeGitHubResolver{}),
		WithWorkerAuth("01234567890123456789012345678901", time.Hour),
	)
	workerBearer := mintPostgresTestWorkerToken(t, ctx, pool, queries, "worker-1")

	claim := claimRunViaHTTP(t, handler, workerBearer)
	if claim.RunID != ids.MustFromPG(run.ID).String() {
		t.Fatalf("claim = %+v run=%s", claim, ids.MustFromPG(run.ID))
	}
	postWorkerJSON[api.WorkerStartResponse](t, handler, workerBearer, "/api/worker/executions/start", api.WorkerStartRequest{Lease: claim}, http.StatusOK)
	created := postWorkerJSON[api.WorkerCreateWaitpointResponse](t, handler, workerBearer, "/api/worker/executions/waitpoints", api.WorkerCreateWaitpointRequest{
		Lease:         claim,
		CorrelationID: "approval-1",
		Kind:          api.WorkerWaitpointKindApproval,
		Request:       json.RawMessage(`{"message":"ship it"}`),
		DisplayText:   "ship it",
	}, http.StatusOK)
	postWorkerJSON[api.WorkerCreateWaitpointResponse](t, handler, workerBearer, "/api/worker/executions/checkpoints/ready", api.WorkerCheckpointReadyRequest{
		Lease:        claim,
		WaitpointID:  created.WaitpointID,
		CheckpointID: created.CheckpointID,
		Manifest: api.WorkerCheckpointManifest{
			RuntimeBackend:      "firecracker",
			RuntimeArch:         "amd64",
			RuntimeABI:          "helmr.firecracker.snapshot.v0",
			KernelDigest:        stringPtr("sha256:" + strings.Repeat("3", 64)),
			RootfsDigest:        stringPtr("sha256:" + strings.Repeat("4", 64)),
			RuntimeConfigDigest: stringPtr("sha256:" + strings.Repeat("5", 64)),
			VMStateDigest:       stringPtr("sha256:" + strings.Repeat("1", 64)),
			MemoryDigests:       []string{"sha256:" + strings.Repeat("2", 64)},
			CASObjects: []api.CASObject{
				{Digest: "sha256:" + strings.Repeat("1", 64), SizeBytes: 128, MediaType: cas.CheckpointVMStateMediaType},
				{Digest: "sha256:" + strings.Repeat("2", 64), SizeBytes: 256, MediaType: cas.CheckpointMemoryMediaType},
			},
			Manifest: json.RawMessage(`{"runtime":{"backend":"firecracker"}}`),
		},
	}, http.StatusOK)

	postWorkerJSON[api.WorkerEventResponse](t, handler, workerBearer, "/api/worker/executions/logs", api.WorkerAppendLogRequest{
		Lease:         claim,
		Stream:        api.WorkerLogStreamStdout,
		ObservedSeq:   1,
		ContentBase64: base64.StdEncoding.EncodeToString([]byte("stale\n")),
	}, http.StatusConflict)
	postWorkerJSON[api.WorkerEventResponse](t, handler, workerBearer, "/api/worker/executions/events", api.WorkerEmitEventRequest{
		Lease:     claim,
		EventType: "stale.event",
		Content:   json.RawMessage(`{"stale":true}`),
	}, http.StatusConflict)
	exitCode := int32(0)
	postWorkerJSON[api.WorkerReleaseResponse](t, handler, workerBearer, "/api/worker/executions/release", api.WorkerReleaseRequest{
		Lease:  claim,
		Result: api.WorkerReleaseResult{Kind: "completed", ExitCode: &exitCode},
	}, http.StatusConflict)

	events, err := queries.ListRunEvents(ctx, db.ListRunEventsParams{
		OrgID: ids.ToPG(ids.DefaultOrgID),
		RunID: run.ID,
		Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		switch event.Kind {
		case "log.stdout", "emit.stale.event":
			t.Fatalf("stale event persisted: %+v", event)
		}
	}
	updated, err := queries.GetRun(ctx, db.GetRunParams{OrgID: ids.ToPG(ids.DefaultOrgID), ID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != db.RunStatusWaiting || updated.CurrentExecutionID.Valid {
		t.Fatalf("run after stale writes = %+v", updated)
	}
}

func TestWorkerDrainPreventsClaimsUntilReactivatedWithPostgres(t *testing.T) {
	ctx := context.Background()
	queries, pool := newServerPostgresTestDB(t, ctx)
	runQueue := newTestRunQueue()
	first := seedServerQueuedRun(t, ctx, queries, runQueue)
	second := seedServerQueuedRun(t, ctx, queries, runQueue)
	handler := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(queries),
		WithRunQueue(runQueue),
		WithAuthenticator(fakeAuth{}),
		WithGitHubResolver(fakeGitHubResolver{}),
		WithWorkerAuth("01234567890123456789012345678901", time.Hour),
	)
	workerBearer := mintPostgresTestWorkerToken(t, ctx, pool, queries, "worker-1")
	capabilities := testWorkerCapabilities()
	capabilities.MaxVCPUs = 4
	capabilities.MaxMemoryMiB = 4096
	capabilities.ExecutionSlotsAvailable = 2

	activated := postWorkerJSON[api.WorkerStatusResponse](t, handler, workerBearer, "/api/worker/activate", api.WorkerActivateRequest{Capabilities: capabilities}, http.StatusOK)
	if activated.Status != api.WorkerStatusActive || activated.ActiveExecutions != 0 {
		t.Fatalf("activated = %+v", activated)
	}
	claimResponse := postWorkerJSON[api.WorkerRunLeaseResponse](t, handler, workerBearer, "/api/worker/executions/lease", api.WorkerRunLeaseRequest{Capabilities: capabilities}, http.StatusOK)
	if claimResponse.Lease == nil || claimResponse.Lease.RunID != ids.MustFromPG(first.ID).String() {
		t.Fatalf("claim response = %+v first=%s", claimResponse, ids.MustFromPG(first.ID))
	}

	draining := postWorkerJSON[api.WorkerStatusResponse](t, handler, workerBearer, "/api/worker/drain", struct{}{}, http.StatusOK)
	if draining.Status != api.WorkerStatusDraining || draining.ActiveExecutions != 1 {
		t.Fatalf("draining = %+v", draining)
	}
	empty := postWorkerJSON[api.WorkerRunLeaseResponse](t, handler, workerBearer, "/api/worker/executions/lease", api.WorkerRunLeaseRequest{Capabilities: capabilities}, http.StatusOK)
	if empty.Lease != nil || empty.Run != nil {
		t.Fatalf("draining worker run leaseed run = %+v", empty)
	}
	status := getWorkerJSON[api.WorkerStatusResponse](t, handler, workerBearer, "/api/worker/status", http.StatusOK)
	if status.Status != api.WorkerStatusDraining || status.ActiveExecutions != 1 {
		t.Fatalf("status = %+v", status)
	}

	reactivated := postWorkerJSON[api.WorkerStatusResponse](t, handler, workerBearer, "/api/worker/activate", api.WorkerActivateRequest{Capabilities: capabilities}, http.StatusOK)
	if reactivated.Status != api.WorkerStatusActive || reactivated.ActiveExecutions != 1 {
		t.Fatalf("reactivated = %+v", reactivated)
	}
	secondClaim := postWorkerJSON[api.WorkerRunLeaseResponse](t, handler, workerBearer, "/api/worker/executions/lease", api.WorkerRunLeaseRequest{Capabilities: capabilities}, http.StatusOK)
	if secondClaim.Lease == nil || secondClaim.Lease.RunID != ids.MustFromPG(second.ID).String() {
		t.Fatalf("second claim = %+v second=%s", secondClaim, ids.MustFromPG(second.ID))
	}
}

func claimRunViaHTTP(t *testing.T, handler http.Handler, workerBearer string) api.WorkerRunLease {
	t.Helper()
	capabilities := testWorkerCapabilities()
	postWorkerJSON[api.WorkerStatusResponse](t, handler, workerBearer, "/api/worker/activate", api.WorkerActivateRequest{Capabilities: capabilities}, http.StatusOK)
	response := postWorkerJSON[api.WorkerRunLeaseResponse](t, handler, workerBearer, "/api/worker/executions/lease", api.WorkerRunLeaseRequest{Capabilities: capabilities}, http.StatusOK)
	if response.Lease == nil || response.Run == nil {
		t.Fatalf("claim response = %+v", response)
	}
	return *response.Lease
}

func mintPostgresTestWorkerToken(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queries *db.Queries, workerID string) string {
	t.Helper()
	authSecret := []byte(testWorkerTokenSecret)
	registration, err := auth.GenerateWorkerRegistrationToken(authSecret)
	if err != nil {
		t.Fatal(err)
	}
	seedServerTestWorkerRegistrationToken(t, ctx, pool, queries, registration.TokenHash)
	secret, err := auth.GenerateWorkerSecret(authSecret)
	if err != nil {
		t.Fatal(err)
	}
	credentialID, err := ids.Parse(testWorkerCredentialID)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := queries.CreateWorkerCredentialFromRegistration(ctx, db.CreateWorkerCredentialFromRegistrationParams{
		RegistrationTokenHash: registration.TokenHash,
		CredentialID:          ids.ToPG(credentialID),
		WorkerHostID:          ids.ToPG(ids.New()),
		ExternalID:            workerID,
		KeyPrefix:             secret.KeyPrefix,
		SecretHash:            secret.TokenHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	workerPoolID, err := ids.FromPG(credential.WorkerPoolID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.IssueWorkerToken([]byte(testWorkerTokenSecret), auth.WorkerClaims{
		WorkerPoolID: workerPoolID.String(),
		WorkerHostID: credential.WorkerHostID,
		CredentialID: ids.MustFromPG(credential.ID).String(),
		IssuedAt:     time.Now(),
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func postWorkerJSON[T any](t *testing.T, handler http.Handler, workerBearer string, path string, input any, wantStatus int) T {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s status = %d want %d body=%s", path, rec.Code, wantStatus, rec.Body.String())
	}
	var zero T
	if rec.Body.Len() == 0 {
		return zero
	}
	var response T
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func getWorkerJSON[T any](t *testing.T, handler http.Handler, workerBearer string, path string, wantStatus int) T {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s status = %d want %d body=%s", path, rec.Code, wantStatus, rec.Body.String())
	}
	var zero T
	if rec.Body.Len() == 0 {
		return zero
	}
	var response T
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func seedServerQueuedRun(t *testing.T, ctx context.Context, queries *db.Queries, runQueue runqueue.Queue) db.Run {
	t.Helper()
	scope := seedServerTestDefaultScope(t, ctx, queries)
	if _, err := queries.UpsertGitHubInstallation(ctx, db.UpsertGitHubInstallationParams{
		ID:                  ids.ToPG(ids.New()),
		OrgID:               ids.ToPG(ids.DefaultOrgID),
		InstallationID:      123,
		AccountLogin:        "helmrdotdev",
		AccountType:         "Organization",
		RepositorySelection: pgtype.Text{String: "selected", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertGitHubRepository(ctx, db.UpsertGitHubRepositoryParams{
		ID:                 ids.ToPG(ids.New()),
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		InstallationID:     123,
		GithubRepositoryID: 456,
		OwnerLogin:         "helmrdotdev",
		Name:               "helmr",
		FullName:           "helmrdotdev/helmr",
		DefaultBranch:      pgtype.Text{String: "main", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.EnableGitHubRepositoryConnection(ctx, db.EnableGitHubRepositoryConnectionParams{
		ID:                 ids.ToPG(ids.New()),
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		GithubRepositoryID: 456,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.EnableProjectWorkspaceRepositoryAccess(ctx, db.EnableProjectWorkspaceRepositoryAccessParams{
		ID:                 ids.ToPG(ids.New()),
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		ProjectID:          scope.ProjectID,
		GithubRepositoryID: 456,
	}); err != nil {
		t.Fatal(err)
	}
	deploymentTask := ensureServerTestDeploymentTask(t, ctx, queries, scope)
	created, err := queries.CreateRun(ctx, db.CreateRunParams{
		ID:                          ids.ToPG(ids.New()),
		OrgID:                       ids.ToPG(ids.DefaultOrgID),
		DeploymentID:                deploymentTask.DeploymentID,
		DeploymentTaskID:            deploymentTask.ID,
		TaskID:                      "deploy",
		Payload:                     []byte(`{}`),
		SecretBindings:              []byte(`{}`),
		WorkspaceRepository:         "helmrdotdev/helmr",
		WorkspaceInstallationID:     123,
		WorkspaceGithubRepositoryID: 456,
		WorkspaceRef:                "main",
		WorkspaceSha:                testGitSHA,
		MaxDurationSeconds:          3600,
		EventPayload:                []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := publisher.New(queries, runQueue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.EnqueueRun(ctx, created.OrgID, created.ID); err != nil {
		t.Fatal(err)
	}
	run, err := queries.GetRun(ctx, db.GetRunParams{OrgID: created.OrgID, ID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func seedServerTestDefaultScope(t *testing.T, ctx context.Context, queries *db.Queries) db.GetDefaultProjectEnvironmentRow {
	t.Helper()
	orgID := ids.ToPG(ids.DefaultOrgID)
	if _, err := queries.CreateOrganization(ctx, db.CreateOrganizationParams{
		ID:   orgID,
		Name: "Test Organization",
		Slug: "test-organization",
	}); err != nil && !isUniqueViolation(err) {
		t.Fatal(err)
	}
	scope, err := queries.GetDefaultProjectEnvironment(ctx, orgID)
	if err == nil {
		seedServerTestDefaultWorkerPool(t, ctx, queries, orgID)
		return scope
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	if _, err := queries.CreateProjectWithDefaultEnvironment(ctx, db.CreateProjectWithDefaultEnvironmentParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		Slug:          "main",
		Name:          "Main",
		EnvironmentID: ids.ToPG(ids.New()),
	}); err != nil {
		t.Fatal(err)
	}
	scope, err = queries.GetDefaultProjectEnvironment(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	seedServerTestDefaultWorkerPool(t, ctx, queries, orgID)
	return scope
}

func seedServerTestDefaultWorkerPool(t *testing.T, ctx context.Context, queries *db.Queries, orgID pgtype.UUID) db.WorkerPool {
	t.Helper()
	pools, err := queries.ListWorkerPools(ctx, db.ListWorkerPoolsParams{
		OrgID:    orgID,
		RowLimit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, workerPool := range pools {
		if workerPool.Slug == "default" {
			return workerPool
		}
	}
	workerPool, err := queries.CreateWorkerPool(ctx, db.CreateWorkerPoolParams{
		ID:           ids.ToPG(ids.New()),
		Slug:         "default",
		Name:         "Default",
		QueueName:    "default",
		Region:       "",
		Capabilities: []byte(`{}`),
		Metadata:     []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertOrgWorkerPool(ctx, db.UpsertOrgWorkerPoolParams{
		OrgID:        orgID,
		WorkerPoolID: workerPool.ID,
		IsDefault:    true,
	}); err != nil {
		t.Fatal(err)
	}
	return workerPool
}

func seedServerTestWorkerRegistrationToken(t *testing.T, ctx context.Context, _ *pgxpool.Pool, queries *db.Queries, tokenHash []byte) {
	t.Helper()
	seedServerTestDefaultScope(t, ctx, queries)
	pools, err := queries.ListWorkerPools(ctx, db.ListWorkerPoolsParams{
		OrgID:    ids.ToPG(ids.DefaultOrgID),
		RowLimit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) == 0 {
		t.Fatal("default scope has no worker pool")
	}
	if _, err := queries.UpsertWorkerRegistrationToken(ctx, db.UpsertWorkerRegistrationTokenParams{
		ID:           ids.ToPG(ids.New()),
		WorkerPoolID: pools[0].ID,
		TokenHash:    tokenHash,
	}); err != nil {
		t.Fatal(err)
	}
}

func ensureServerTestDeploymentTask(t *testing.T, ctx context.Context, queries *db.Queries, scope db.GetDefaultProjectEnvironmentRow) db.GetCurrentDeploymentTaskRow {
	t.Helper()
	deploymentTask, err := queries.GetCurrentDeploymentTask(ctx, db.GetCurrentDeploymentTaskParams{
		OrgID:         ids.ToPG(ids.DefaultOrgID),
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		TaskID:        "deploy",
	})
	if err == nil {
		return deploymentTask
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	taskSourceDigest := "sha256:" + strings.Repeat("a", 64)
	if _, err := queries.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		Digest:    taskSourceDigest,
		SizeBytes: 1,
		MediaType: "application/vnd.helmr.task-source.v1.tar",
	}); err != nil {
		t.Fatal(err)
	}
	deploymentID := ids.ToPG(ids.New())
	if _, err := queries.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID:            deploymentID,
		OrgID:         ids.ToPG(ids.DefaultOrgID),
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		SourceDigest:  taskSourceDigest,
		Status:        db.DeploymentStatusCreating,
	}); err != nil {
		t.Fatal(err)
	}
	taskID := ids.ToPG(ids.New())
	if _, err := queries.CreateDeploymentTask(ctx, db.CreateDeploymentTaskParams{
		ID:                 taskID,
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		ProjectID:          scope.ProjectID,
		EnvironmentID:      scope.EnvironmentID,
		DeploymentID:       deploymentID,
		TaskID:             "deploy",
		ModulePath:         "tasks/deploy.ts",
		ExportName:         "deploy",
		RequestedMilliCpu:  2000,
		RequestedMemoryMib: 2048,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkDeploymentDeployed(ctx, db.MarkDeploymentDeployedParams{
		OrgID:         ids.ToPG(ids.DefaultOrgID),
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		ID:            deploymentID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AssignDeploymentLabel(ctx, db.AssignDeploymentLabelParams{
		OrgID:         ids.ToPG(ids.DefaultOrgID),
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		Label:         "current",
		DeploymentID:  deploymentID,
	}); err != nil {
		t.Fatal(err)
	}
	deploymentTask, err = queries.GetCurrentDeploymentTask(ctx, db.GetCurrentDeploymentTaskParams{
		OrgID:         ids.ToPG(ids.DefaultOrgID),
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		TaskID:        "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	return deploymentTask
}

type testRunQueue struct {
	mu       sync.Mutex
	next     int
	messages []testQueueMessage
	leases   map[string]runqueue.Lease
}

type testQueueMessage struct {
	id      string
	message runqueue.Message
}

func newTestRunQueue() *testRunQueue {
	return &testRunQueue{leases: map[string]runqueue.Lease{}}
}

func (q *testRunQueue) Enqueue(_ context.Context, message runqueue.Message) (runqueue.EnqueueResult, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.next++
	messageID := fmt.Sprintf("message-%d", q.next)
	q.messages = append(q.messages, testQueueMessage{id: messageID, message: message})
	return runqueue.EnqueueResult{MessageID: messageID, Depth: int64(len(q.messages))}, nil
}

func (q *testRunQueue) Dequeue(_ context.Context, request runqueue.DequeueRequest) ([]runqueue.Lease, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, queued := range q.messages {
		message := queued.message
		if message.OrgID != request.OrgID || message.WorkerPoolID != request.WorkerPoolID || message.QueueName != request.QueueName {
			continue
		}
		q.messages = append(q.messages[:i], q.messages[i+1:]...)
		lease := runqueue.Lease{
			ID:            "lease-" + queued.id,
			MessageID:     queued.id,
			WorkerHostID:  request.WorkerHostID,
			Message:       message,
			AttemptNumber: 1,
			ExpiresAt:     time.Now().Add(time.Minute),
		}
		q.leases[lease.ID] = lease
		return []runqueue.Lease{lease}, nil
	}
	return nil, nil
}

func (q *testRunQueue) ReadyMessageExists(_ context.Context, messageID string) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, message := range q.messages {
		if message.id == messageID {
			return true, nil
		}
	}
	return false, nil
}

func (q *testRunQueue) Ack(_ context.Context, lease runqueue.Lease) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.leases, lease.ID)
	return nil
}

func (q *testRunQueue) Nack(_ context.Context, lease runqueue.Lease, reason runqueue.NackReason) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.leases, lease.ID)
	if reason == runqueue.NackReasonRetry || reason == runqueue.NackReasonNoCapacity || reason == runqueue.NackReasonHostDraining {
		q.messages = append(q.messages, testQueueMessage{id: lease.MessageID, message: lease.Message})
	}
	return nil
}

func (q *testRunQueue) Renew(_ context.Context, lease runqueue.Lease, expiresAt time.Time) (runqueue.Lease, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.leases[lease.ID]; !ok {
		return runqueue.Lease{}, runqueue.ErrMessageNotFound
	}
	lease.ExpiresAt = expiresAt
	q.leases[lease.ID] = lease
	return lease, nil
}

func newServerPostgresTestDB(t *testing.T, ctx context.Context) (*db.Queries, *pgxpool.Pool) {
	t.Helper()
	if dsn := strings.TrimSpace(os.Getenv("HELMR_TEST_DATABASE_URL")); dsn != "" {
		return newExternalServerPostgresTestDB(t, ctx, dsn, filepath.Join("..", "db", "schema", "migrations", "*.up.sql"))
	}
	for _, name := range []string{"initdb", "pg_ctl", "postgres"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not found; skipping Postgres integration test", name)
		}
	}
	tmp, err := os.MkdirTemp("", "helmr-server-pg-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(tmp)
	})
	dataDir := filepath.Join(tmp, "data")
	if output, err := exec.Command("initdb", "-D", dataDir, "-A", "trust").CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, output)
	}
	port := freeServerPostgresPort(t)
	logPath := filepath.Join(tmp, "postgres.log")
	start := exec.Command("pg_ctl", "-D", dataDir, "-l", logPath, "-o", fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1", port), "-w", "start")
	if output, err := start.CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl start: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		_ = exec.Command("pg_ctl", "-D", dataDir, "-m", "fast", "-w", "stop").Run()
	})

	dsn := fmt.Sprintf("postgres://%s@127.0.0.1:%d/postgres?sslmode=disable", os.Getenv("USER"), port)
	dbctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(dbctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var serverVersion int
	if err := pool.QueryRow(dbctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	if serverVersion < 180000 {
		t.Skipf("Postgres %d does not provide uuidv7(); skipping Postgres integration test", serverVersion)
	}
	applyServerPostgresTestMigrations(t, dbctx, pool, filepath.Join("..", "db", "schema", "migrations", "*.up.sql"))
	pool.Close()
	registeredPool, err := pgxpool.New(dbctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registeredPool.Close)
	return db.New(registeredPool), registeredPool
}

func newExternalServerPostgresTestDB(t *testing.T, ctx context.Context, dsn string, migrationsGlob string) (*db.Queries, *pgxpool.Pool) {
	t.Helper()
	adminCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	admin, err := pgxpool.New(adminCtx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	dbName := "helmr_test_" + strings.ReplaceAll(ids.New().String(), "-", "")
	dbIdentifier := pgx.Identifier{dbName}.Sanitize()
	if _, err := admin.Exec(adminCtx, "CREATE DATABASE "+dbIdentifier); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanupCtx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, dbName)
		_, _ = admin.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+dbIdentifier)
		admin.Close()
	})

	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = dbName
	dbctx, dbcancel := context.WithTimeout(ctx, 10*time.Second)
	defer dbcancel()
	pool, err := pgxpool.NewWithConfig(dbctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	applyServerPostgresTestMigrations(t, dbctx, pool, migrationsGlob)
	pool.Close()
	registeredPool, err := pgxpool.NewWithConfig(dbctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registeredPool.Close)
	return db.New(registeredPool), registeredPool
}

func applyServerPostgresTestMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool, migrationsGlob string) {
	t.Helper()
	var serverVersion int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	if serverVersion < 180000 {
		t.Skipf("Postgres %d does not provide uuidv7(); skipping Postgres integration test", serverVersion)
	}
	migrations, err := filepath.Glob(migrationsGlob)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(migrations)
	for _, path := range migrations {
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
}

func freeServerPostgresPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func stringPtr(value string) *string {
	return &value
}
