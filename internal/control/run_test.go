package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const testGitSHA = "0123456789abcdef0123456789abcdef01234567"
const testWorkerTokenSecret = "01234567890123456789012345678901"
const testWorkerInstanceCredentialID = "00000000-0000-0000-0000-00000000c001"

func testWorkerGroupID() pgtype.UUID {
	return pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000201"))
}

func testProjectID() pgtype.UUID {
	return pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000301"))
}

func testEnvironmentID() pgtype.UUID {
	return pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000302"))
}

func otherProjectID() pgtype.UUID {
	return pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000311"))
}

func otherEnvironmentID() pgtype.UUID {
	return pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000312"))
}

func testProjectIDString() string {
	return pgvalue.MustUUIDValue(testProjectID()).String()
}

func testEnvironmentIDString() string {
	return pgvalue.MustUUIDValue(testEnvironmentID()).String()
}

func testDeploymentID() pgtype.UUID {
	return pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000304"))
}

func testDeploymentTaskID() pgtype.UUID {
	return pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000305"))
}

func testArtifactID() pgtype.UUID {
	return pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000306"))
}

func testWorkerRunLeaseRequestBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(api.WorkerRunLeaseRequest{Capabilities: testWorkerCapabilities()})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func testWorkerCapabilities() api.WorkerCapabilities {
	capabilities := api.WorkerCapabilities{
		ProtocolVersion:           api.CurrentWorkerProtocolVersion,
		SupportedProtocolVersions: api.SupportedWorkerProtocolVersions,
		RuntimeArch:               "arm64",
		RuntimeABI:                "helmr.firecracker.snapshot.v0",
		KernelDigest:              "sha256:kernel",
		InitramfsDigest:           "sha256:initramfs",
		RootfsDigest:              "sha256:rootfs",
		CNIProfile:                "helmr/v0",
		MaxVCPUs:                  2,
		MaxMemoryMiB:              2048,
		MaxDiskMiB:                20480,
		ExecutionSlotsAvailable:   1,
		Network: api.WorkerNetworkCapabilities{
			Internet:      true,
			BlockInternet: true,
			DenyCIDRs:     true,
		},
	}
	runtimeID, err := compute.RuntimeIdentityDigest(compute.RuntimeSelector{
		Arch:            capabilities.RuntimeArch,
		ABI:             capabilities.RuntimeABI,
		KernelDigest:    capabilities.KernelDigest,
		InitramfsDigest: capabilities.InitramfsDigest,
		RootfsDigest:    capabilities.RootfsDigest,
		CNIProfile:      capabilities.CNIProfile,
	})
	if err != nil {
		panic(err)
	}
	capabilities.RuntimeID = runtimeID
	return capabilities
}

func testRunRuntimeRequirements() compute.RunRuntimeRequirements {
	capabilities := testWorkerCapabilities()
	return compute.RunRuntimeRequirements{
		Resources: compute.ResourceVector{
			MilliCPU:  1000,
			MemoryMiB: 512,
			DiskMiB:   1024,
			Slots:     1,
		},
		Runtime: compute.RuntimeSelector{
			ID:              capabilities.RuntimeID,
			Arch:            capabilities.RuntimeArch,
			ABI:             capabilities.RuntimeABI,
			KernelDigest:    capabilities.KernelDigest,
			InitramfsDigest: capabilities.InitramfsDigest,
			RootfsDigest:    capabilities.RootfsDigest,
			CNIProfile:      capabilities.CNIProfile,
		},
		Network: compute.DefaultNetworkPolicy(),
	}
}

func TestAPIKeyRunCreateRejectsActorWithoutEnvironmentScope(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{kind: auth.ActorKindAPIKey, role: auth.RoleOwner}})
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID: "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer machine-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "API key is not bound to an environment") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if store.createRun.ID.Valid {
		t.Fatalf("run was created: %+v", store.createRun)
	}
}

func TestAPIKeyRunCreateUsesActorEnvironmentScope(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{
		kind:          auth.ActorKindAPIKey,
		projectID:     testProjectIDString(),
		environmentID: testEnvironmentIDString(),
		permissions:   []auth.Permission{auth.PermissionRunsCreate},
	}},
	)
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID: "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer machine-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.run.ProjectID != testProjectID() || store.run.EnvironmentID != testEnvironmentID() {
		t.Fatalf("run scope = project %v environment %v", store.run.ProjectID, store.run.EnvironmentID)
	}
}

func TestAPIKeyRunCreateRejectsExplicitScopeOverride(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{
		kind:          auth.ActorKindAPIKey,
		projectID:     testProjectIDString(),
		environmentID: testEnvironmentIDString(),
		permissions:   []auth.Permission{auth.PermissionRunsCreate},
	}},
	)
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:        "deploy",
		ProjectID:     testProjectIDString(),
		EnvironmentID: testEnvironmentIDString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer machine-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "project_id and environment_id are not accepted with API keys") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestDeviceAuthorizationRequiresSession(t *testing.T) {
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: &fakeStore{}, Auth: fakeAuth{}, AuthSecret: []byte("abcdefghijabcdefghijabcdefghij12"), PublicURL: mustParseTestURL("https://helmr.example.test")})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device/approve", strings.NewReader(`{"user_code":"ABCD-EFGH"}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSessionRefreshWriterSetsCookieBeforeHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://helmr.example.test/api/me", nil)
	rec := httptest.NewRecorder()
	writer := newSessionRefreshResponseWriter(rec, req, "raw-session", time.Hour)

	writer.WriteHeader(http.StatusNoContent)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	if cookies[0].Name != "__Host-helmr_session" || cookies[0].Value != "raw-session" {
		t.Fatalf("cookie = %+v", cookies[0])
	}
}

func TestSessionRefreshWriterPassesFlush(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://helmr.example.test/api/runs/run-1/events?follow=1", nil)
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	writer := newSessionRefreshResponseWriter(rec, req, "raw-session", time.Hour)

	writer.Flush()

	if !rec.flushed {
		t.Fatal("expected flush")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(rec.Result().Cookies()) != 1 {
		t.Fatalf("cookies = %v", rec.Result().Cookies())
	}
}

func TestValidatedRetryPolicyRejectsUnsupportedFields(t *testing.T) {
	for name, raw := range map[string]string{
		"retryOn":      `{"maxAttempts":3,"retryOn":["timeout"]}`,
		"backoffField": `{"maxAttempts":3,"backoff":{"minMs":1000,"strategy":"linear"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validatedRetryPolicyJSON([]byte(raw), "retry"); err == nil {
				t.Fatal("retry policy validation succeeded, want error")
			}
		})
	}
}

func TestCreateRunRejectsInvalidTaskID(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})

	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID: "bad task",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.createRun.ID.Valid {
		t.Fatal("run was created")
	}
}

func TestCreateRunRejectsClientSuppliedBundle(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})

	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewBufferString(`{
		"task_id": "deploy",
		"bundle": "dGVzdA==",
		"source": {"kind": "github", "repository": "helmrdotdev/helmr", "ref": "`+testGitSHA+`"}
	}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.createRun.ID.Valid {
		t.Fatal("run was created")
	}
}

func TestRunRoutesRequireBearerAuth(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})

	req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateRunWithUnknownVersionReturnsVersionError(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}})

	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:  "deploy",
		Options: api.CreateRunOptions{Version: "20260101.99"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `deployment version \"20260101.99\" was not found`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestReleaseOutputOnlyForSuccessfulZeroExit(t *testing.T) {
	output := json.RawMessage(`{"ok":true}`)
	zero := pgtype.Int4{Int32: 0, Valid: true}
	one := pgtype.Int4{Int32: 1, Valid: true}
	if got := releaseOutput(api.WorkerReleaseResult{Kind: "completed", Output: output}, db.RunStatusSucceeded, zero); string(got) != string(output) {
		t.Fatalf("successful output = %s", got)
	}
	if got := releaseOutput(api.WorkerReleaseResult{Kind: "completed", Output: output}, db.RunStatusFailed, one); got != nil {
		t.Fatalf("failed output = %s", got)
	}
	if got := releaseOutput(api.WorkerReleaseResult{Kind: "failed", Output: output}, db.RunStatusFailed, pgtype.Int4{}); got != nil {
		t.Fatalf("worker failed output = %s", got)
	}
}

func assertJSONBytes(t *testing.T, got []byte, want string) {
	t.Helper()
	if string(got) != want {
		t.Fatalf("json = %s, want %s", got, want)
	}
}

func assertTerminalPayloadFailure(t *testing.T, store *fakeStore, failureKind string) {
	t.Helper()
	if store.abandonedClaim {
		t.Fatal("claim should not be abandoned")
	}
	if store.run.Status != db.RunStatusFailed {
		t.Fatalf("run status = %s", store.run.Status)
	}
	if store.run.CurrentSessionID.Valid {
		t.Fatalf("current execution id = %+v", store.run.CurrentSessionID)
	}
	if len(store.events) != 1 || store.events[0].Kind != "run.failed" {
		t.Fatalf("events = %+v", store.events)
	}
	var payload struct {
		FailureKind string `json:"failure_kind"`
	}
	if err := json.Unmarshal(store.events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.FailureKind != failureKind {
		t.Fatalf("failure kind = %q", payload.FailureKind)
	}
}

type fakeWaitpoint struct {
	ID             pgtype.UUID
	RunWaitID      pgtype.UUID
	OrgID          pgtype.UUID
	ProjectID      pgtype.UUID
	EnvironmentID  pgtype.UUID
	RunID          pgtype.UUID
	SessionID      pgtype.UUID
	CheckpointID   pgtype.UUID
	CorrelationID  string
	Kind           db.WaitpointKind
	Request        []byte
	DisplayText    string
	TimeoutSeconds pgtype.Int4
	PolicyName     pgtype.Text
	PolicySnapshot []byte
	Status         db.RunWaitStatus
	ResolutionKind pgtype.Text
	Output         []byte
	Resolution     []byte
	CreatedAt      pgtype.Timestamptz
	RequestedAt    pgtype.Timestamptz
	ResolvedAt     pgtype.Timestamptz
}

type fakeListRunsParams struct {
	StatusFilter string
	RowLimit     int32
}

type fakeStore struct {
	db.Querier
	createRun                               db.CreateScopedRunParams
	listRuns                                fakeListRunsParams
	listScopedRuns                          db.ListScopedRunSummariesParams
	countScopedRuns                         db.CountScopedRunsByStatusParams
	run                                     db.Run
	runOperation                            db.RunOperation
	cancelRunErr                            error
	cancelRunCalls                          int
	deployment                              db.Deployment
	currentDeploymentTaskSecretDeclarations []byte
	currentDeploymentMissing                bool
	currentDeploymentTaskCalls              int
	getDeploymentTaskCalls                  int
	deploymentPromotions                    []db.PromoteDeploymentParams
	createDeploymentResult                  *db.Deployment
	createDeploymentErr                     error
	deploymentEvents                        []db.Event
	deploymentTasks                         []db.DeploymentTask
	artifacts                               []db.Artifact
	runEvent                                db.AppendRunEventParams
	events                                  []db.Event
	stdout                                  []byte
	stderr                                  []byte
	runLogSnapshot                          db.GetRunLogSnapshotParams
	runLogChunksAfter                       db.ListRunLogChunksAfterParams
	runLogChunksAfterCalls                  int
	firstRunLogChunksAfterSeq               int64
	deferLogChunksUntilSecondList           bool
	logChunks                               []db.RunLogChunk
	logTruncated                            bool
	secret                                  db.GetScopedSecretMetadataByNameRow
	secrets                                 []db.ListScopedSecretsRow
	deleteSecret                            db.DeleteScopedSecretParams
	deleteSecretRows                        int64
	defaultProjectID                        pgtype.UUID
	defaultEnvironmentID                    pgtype.UUID
	logCursor                               int64
	casObjects                              []db.UpsertCasObjectParams
	getCasObjectErr                         error
	sessionID                               pgtype.UUID
	executionWorkerInstanceID               pgtype.UUID
	executionLeaseExpiresAt                 pgtype.Timestamptz
	waitpoint                               fakeWaitpoint
	waitpointDeliveries                     []db.WaitpointDelivery
	checkpoint                              db.Checkpoint
	abandonedClaim                          bool
	workerBootstrapTokenHash                []byte
	workerCredentialID                      pgtype.UUID
	workerCredentialSecretHash              []byte
	dequeueRequest                          dispatch.DequeueRequest
	ackedLeases                             []dispatch.Lease
	activeQueueLeaseMissing                 bool
	renewErr                                error
	waitpointResponses                      []db.RecordWaitpointResponseParams
	resolveStatus                           db.RunWaitStatus
	listQueueScopes                         db.ListQueueScopesParams
}

type fakeRunEnqueuer struct {
	orgID pgtype.UUID
	runID pgtype.UUID
	count int
	err   error
}

func (f *fakeRunEnqueuer) EnqueueRun(_ context.Context, orgID pgtype.UUID, runID pgtype.UUID) (dispatch.EnqueueResult, error) {
	f.orgID = orgID
	f.runID = runID
	f.count++
	return dispatch.EnqueueResult{QueueName: "queue-a", MessageID: "message-1", Depth: 1}, f.err
}

type fakeSecrets struct {
	values api.ResolvedSecrets
}

func (f *fakeStore) CreateScopedRun(_ context.Context, arg db.CreateScopedRunParams) (db.CreateScopedRunRow, error) {
	f.createRun = arg
	now := testTime()
	f.run = db.Run{
		ID:                      arg.ID,
		OrgID:                   arg.OrgID,
		ProjectID:               arg.ProjectID,
		EnvironmentID:           arg.EnvironmentID,
		DeploymentID:            arg.DeploymentID,
		DeploymentTaskID:        arg.DeploymentTaskID,
		TaskID:                  arg.TaskID,
		Status:                  db.RunStatusQueued,
		Payload:                 arg.Payload,
		IdempotencyKey:          arg.IdempotencyKey,
		IdempotencyKeyExpiresAt: arg.IdempotencyKeyExpiresAt,
		IdempotencyKeyOptions:   arg.IdempotencyKeyOptions,
		IdempotencyRequestHash:  arg.IdempotencyRequestHash,
		QueueName:               arg.QueueName,
		QueueConcurrencyLimit:   arg.QueueConcurrencyLimit,
		ConcurrencyKey:          arg.ConcurrencyKey,
		Priority:                arg.Priority,
		QueueTimestamp:          arg.QueueTimestamp,
		Ttl:                     arg.Ttl,
		QueuedExpiresAt:         arg.QueuedExpiresAt,
		MaxDurationSeconds:      arg.MaxDurationSeconds,
		TraceID:                 arg.TraceID,
		RootSpanID:              arg.RootSpanID,
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	f.runEvent = db.AppendRunEventParams{
		OrgID:   arg.OrgID,
		RunID:   arg.ID,
		Kind:    "run.created",
		Payload: arg.EventPayload,
	}
	f.events = append(f.events, db.Event{
		Seq:       int64(len(f.events) + 1),
		OrgID:     arg.OrgID,
		RunID:     arg.ID,
		Kind:      "run.created",
		Payload:   arg.EventPayload,
		CreatedAt: now,
	})
	return db.CreateScopedRunRow{
		ID:               f.run.ID,
		OrgID:            f.run.OrgID,
		ProjectID:        f.run.ProjectID,
		EnvironmentID:    f.run.EnvironmentID,
		DeploymentID:     f.run.DeploymentID,
		DeploymentTaskID: f.run.DeploymentTaskID,
		TaskID:           f.run.TaskID,
		Status:           f.run.Status,
		ExitCode:         f.run.ExitCode,
		Output:           f.run.Output,
		CreatedAt:        f.run.CreatedAt,
		UpdatedAt:        f.run.UpdatedAt,
	}, nil
}

func (f *fakeStore) GetRun(_ context.Context, arg db.GetRunParams) (db.Run, error) {
	if f.run.ID != arg.ID {
		return db.Run{}, pgx.ErrNoRows
	}
	run := f.run
	run.ProjectID = fakeRunProjectID(run)
	run.EnvironmentID = fakeRunEnvironmentID(run)
	return run, nil
}

func (f *fakeStore) GetRunExecutionSessionRuntimeRelease(_ context.Context, arg db.GetRunExecutionSessionRuntimeReleaseParams) (db.GetRunExecutionSessionRuntimeReleaseRow, error) {
	if f.activeQueueLeaseMissing {
		return db.GetRunExecutionSessionRuntimeReleaseRow{}, pgx.ErrNoRows
	}
	if f.run.ID != arg.RunID || f.sessionID != arg.SessionID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.GetRunExecutionSessionRuntimeReleaseRow{}, pgx.ErrNoRows
	}
	capabilities := testWorkerCapabilities()
	return db.GetRunExecutionSessionRuntimeReleaseRow{
		RuntimeID:       capabilities.RuntimeID,
		RuntimeArch:     capabilities.RuntimeArch,
		RuntimeABI:      capabilities.RuntimeABI,
		KernelDigest:    capabilities.KernelDigest,
		InitramfsDigest: capabilities.InitramfsDigest,
		RootfsDigest:    capabilities.RootfsDigest,
		CniProfile:      capabilities.CNIProfile,
	}, nil
}

func (f *fakeStore) RunExecutionSessionDispatchAttemptsExhausted(context.Context, db.RunExecutionSessionDispatchAttemptsExhaustedParams) (bool, error) {
	return false, nil
}

func (f *fakeStore) FailExpiredRunningRunExecutionSessions(context.Context, pgtype.UUID) error {
	return nil
}

func testTime() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC), Valid: true}
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (r *flushRecorder) Flush() {
	r.flushed = true
}

type fakeAuth struct {
	role          auth.Role
	kind          auth.ActorKind
	userID        uuid.UUID
	apiKeyID      uuid.UUID
	projectID     string
	environmentID string
	permissions   []auth.Permission
}

func (f *fakeStore) GetDefaultProjectEnvironment(context.Context, pgtype.UUID) (db.GetDefaultProjectEnvironmentRow, error) {
	projectID := f.defaultProjectID
	if !projectID.Valid {
		projectID = testProjectID()
	}
	environmentID := f.defaultEnvironmentID
	if !environmentID.Valid {
		environmentID = testEnvironmentID()
	}
	return db.GetDefaultProjectEnvironmentRow{
		ProjectID:     projectID,
		EnvironmentID: environmentID,
	}, nil
}

func (f fakeAuth) Authenticate(context.Context, string) (auth.Actor, error) {
	role := f.role
	if role == "" {
		role = auth.RoleOwner
	}
	kind := f.kind
	if kind == "" {
		kind = auth.ActorKindAPIKey
	}
	userID := f.userID
	if kind == auth.ActorKindSession && userID == uuid.Nil {
		userID = uuid.MustParse("00000000-0000-0000-0000-000000000001")
	}
	apiKeyID := f.apiKeyID
	if kind == auth.ActorKindAPIKey && apiKeyID == uuid.Nil {
		apiKeyID = uuid.MustParse("00000000-0000-0000-0000-000000000002")
	}
	permissions := f.permissions
	if kind == auth.ActorKindAPIKey && f.kind == "" && permissions == nil {
		permissions = []auth.Permission{
			auth.PermissionRunsCreate,
			auth.PermissionRunsRead,
			auth.PermissionRunsManage,
			auth.PermissionSecretsWrite,
			auth.PermissionWaitpointsRespond,
			auth.PermissionWaitpointPolicies,
			auth.PermissionTasksDeploy,
		}
	}
	projectID := f.projectID
	if kind == auth.ActorKindAPIKey && f.kind == "" && projectID == "" {
		projectID = testProjectIDString()
	}
	environmentID := f.environmentID
	if kind == auth.ActorKindAPIKey && f.kind == "" && environmentID == "" {
		environmentID = testEnvironmentIDString()
	}
	return auth.Actor{
		OrgID:         dbtest.DefaultOrgID,
		UserID:        userID,
		APIKeyID:      apiKeyID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Role:          role,
		Kind:          kind,
		Permissions:   permissions,
	}, nil
}

func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode JSON object %q: %v", string(raw), err)
	}
	return payload
}

func stringField(t *testing.T, payload map[string]any, key string) string {
	t.Helper()
	value, ok := payload[key].(string)
	if !ok {
		t.Fatalf("%s field = %+v", key, payload[key])
	}
	return value
}

func assertRFC3339NanoField(t *testing.T, payload map[string]any, key string) {
	t.Helper()
	if _, err := time.Parse(time.RFC3339Nano, stringField(t, payload, key)); err != nil {
		t.Fatalf("%s field = %v", key, err)
	}
}

var _ auth.Authenticator = fakeAuth{}
