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

	"github.com/alicebob/miniredis/v2"
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
	"github.com/redis/go-redis/v9"
)

func TestWorkerRunLeaseStartAndRelease(t *testing.T) {
	store := &fakeStore{
		run: db.Run{
			ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			DeploymentID:        testDeploymentID(),
			DeploymentTaskID:    testDeploymentTaskID(),
			TaskID:              "deploy",
			Status:              db.RunStatusQueued,
			Output:              []byte(`{"env":"prod"}`),
			MaxActiveDurationMs: 3600_000,
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
		currentDeploymentTaskSecretDeclarations: []byte(`[{"name":"API_KEY","env":"API_KEY"}]`),
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, Secrets: fakeSecrets{values: api.ResolvedSecrets{"API_KEY": []byte("secret-value")}}, WorkerTokenSecret: []byte("01234567890123456789012345678901"), WorkerTokenTTL: time.Hour})
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000401")

	capabilities := testWorkerCapabilities()
	capabilities.Region = "us-east-1"
	capabilities.Labels = map[string]string{"pool": "snapshot", "dedicated_key": "tenant-a"}
	body, err := json.Marshal(api.WorkerRunLeaseRequest{Capabilities: capabilities})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/lease", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("claim status = %d body=%s", rec.Code, rec.Body.String())
	}
	var claimResponse api.WorkerRunLeaseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &claimResponse); err != nil {
		t.Fatal(err)
	}
	if claimResponse.Lease == nil || claimResponse.Run == nil {
		t.Fatalf("claim response = %+v", claimResponse)
	}
	if store.dequeueRequest.Runtime.ID != capabilities.RuntimeID ||
		store.dequeueRequest.Runtime.Arch != capabilities.RuntimeArch ||
		store.dequeueRequest.Runtime.ABI != capabilities.RuntimeABI ||
		store.dequeueRequest.Runtime.KernelDigest != capabilities.KernelDigest ||
		store.dequeueRequest.Runtime.InitramfsDigest != capabilities.InitramfsDigest ||
		store.dequeueRequest.Runtime.RootfsDigest != capabilities.RootfsDigest ||
		store.dequeueRequest.Runtime.CNIProfile != capabilities.CNIProfile ||
		store.dequeueRequest.Region != capabilities.Region ||
		store.dequeueRequest.Labels["pool"] != "snapshot" ||
		store.dequeueRequest.Labels["dedicated_key"] != "tenant-a" {
		t.Fatalf("dequeue request = %+v", store.dequeueRequest)
	}
	if store.dequeueRequest.QueueName != dispatch.QueueNameForRuntime("queue-a", compute.RuntimeSelector{
		ID:              capabilities.RuntimeID,
		Arch:            capabilities.RuntimeArch,
		ABI:             capabilities.RuntimeABI,
		KernelDigest:    capabilities.KernelDigest,
		InitramfsDigest: capabilities.InitramfsDigest,
		RootfsDigest:    capabilities.RootfsDigest,
		CNIProfile:      capabilities.CNIProfile,
	}) {
		t.Fatalf("dequeue queue name = %q", store.dequeueRequest.QueueName)
	}
	if store.listQueueScopes.WorkerGroupID != testWorkerGroupID() {
		t.Fatalf("list queue scopes worker group = %+v", store.listQueueScopes.WorkerGroupID)
	}
	if store.markStaleWorkspaceMaterializationsLostCalls != 1 {
		t.Fatalf("stale materialization sweeper calls = %d", store.markStaleWorkspaceMaterializationsLostCalls)
	}
	if claimResponse.Run.DeploymentSource.Digest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("deployment source = %+v", claimResponse.Run.DeploymentSource)
	}
	if string(claimResponse.Run.Secrets["API_KEY"]) != "secret-value" {
		t.Fatalf("resolved secrets = %+v", claimResponse.Run.Secrets)
	}

	startBody, err := json.Marshal(api.WorkerStartRequest{Lease: *claimResponse.Lease})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/worker/leases/start", bytes.NewReader(startBody))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d body=%s", rec.Code, rec.Body.String())
	}

	renewBody, err := json.Marshal(api.WorkerRenewRequest{Lease: *claimResponse.Lease})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/worker/leases/renew", bytes.NewReader(renewBody))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("renew status = %d body=%s", rec.Code, rec.Body.String())
	}
	store.renewErr = dispatch.ErrMessageNotFound
	req = httptest.NewRequest(http.MethodPost, "/api/worker/leases/renew", bytes.NewReader(renewBody))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("renew with stale redis lease status = %d body=%s", rec.Code, rec.Body.String())
	}
	store.renewErr = nil

	exitCode := int32(0)
	output := json.RawMessage(`{"ok":true,"count":2}`)
	releaseBody, err := json.Marshal(api.WorkerReleaseRequest{
		Lease:  *claimResponse.Lease,
		Result: api.WorkerReleaseResult{Kind: "completed", ExitCode: &exitCode, Output: output},
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/worker/leases/release", bytes.NewReader(releaseBody))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("release status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.run.Status != db.RunStatusSucceeded {
		t.Fatalf("run status = %s", store.run.Status)
	}
	if string(store.run.Output) != string(output) {
		t.Fatalf("run output = %s", store.run.Output)
	}
	if len(store.events) != 1 || store.events[0].Kind != "run.completed" {
		t.Fatalf("events = %+v", store.events)
	}
	var terminalPayload struct {
		ExitCode int32 `json:"exit_code"`
	}
	if err := json.Unmarshal(store.events[0].Payload, &terminalPayload); err != nil {
		t.Fatalf("terminal payload decode: %v", err)
	}
	if terminalPayload.ExitCode != 0 {
		t.Fatalf("terminal payload = %+v", terminalPayload)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+claimResponse.Lease.RunID, nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get run status = %d body=%s", rec.Code, rec.Body.String())
	}
	var runBody map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &runBody); err != nil {
		t.Fatal(err)
	}
	if string(runBody["output"]) != string(output) {
		t.Fatalf("response output = %s", runBody["output"])
	}
}

func TestWorkerRestoreRunWaitDecisionRejectsInvalidStreamPayload(t *testing.T) {
	_, _, err := workerRestoreRunWaitDecision(db.GetRunRestorePayloadRow{
		RunWaitKind:          db.RunWaitKindStream,
		StreamName:           pgtype.Text{String: "reply", Valid: true},
		StreamRecordSequence: pgtype.Int8{Int64: 1, Valid: true},
		StreamRecordData:     []byte(`{`),
	})
	if err == nil {
		t.Fatal("expected invalid stream resume payload to fail")
	}
}

func TestWorkerRestoreRunWaitUsesUUIDAuthority(t *testing.T) {
	runWaitID := uuid.Must(uuid.NewV7())
	runWait, err := workerRestoreRunWait(db.GetRunRestorePayloadRow{
		RunWaitID:            pgvalue.UUID(runWaitID),
		RunWaitCorrelationID: "1",
		RunWaitKind:          db.RunWaitKindTimer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if runWait.ID != runWaitID.String() {
		t.Fatalf("restore run wait id = %q, want UUID %s", runWait.ID, runWaitID)
	}
}

func TestWorkerRunLeaseRejectsUnsupportedProtocol(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, WorkerTokenSecret: []byte("01234567890123456789012345678901"), WorkerTokenTTL: time.Hour})
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000401")

	capabilities := testWorkerCapabilities()
	capabilities.ProtocolVersion = "helmr.worker.future"
	body, err := json.Marshal(api.WorkerRunLeaseRequest{Capabilities: capabilities})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/lease", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "worker protocol_version helmr.worker.future is not supported") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestWorkerReleaseRejectsUnknownFields(t *testing.T) {
	store := &fakeStore{
		run: db.Run{
			ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			TaskID:              "deploy",
			Status:              db.RunStatusQueued,
			Output:              []byte(`{}`),
			MaxActiveDurationMs: 3600_000,
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
	}
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	eventStream := &EventStream{log: slog.New(slog.NewTextHandler(io.Discard, nil)), db: store, redis: redisClient}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, WorkerTokenSecret: []byte("01234567890123456789012345678901"), WorkerTokenTTL: time.Hour, EventStream: eventStream})
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000401")
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/lease", bytes.NewReader(testWorkerRunLeaseRequestBody(t)))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim status = %d body=%s", rec.Code, rec.Body.String())
	}
	var claimResponse api.WorkerRunLeaseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &claimResponse); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"claim": claimResponse.Lease,
		"result": map[string]any{
			"kind":                "completed",
			"exit_code":           0,
			"workspace_diff_hash": "sha256:" + strings.Repeat("1", 64),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/worker/leases/release", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("release status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerReleaseRejectsMalformedWorkspaceCommit(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	sessionID := uuid.Must(uuid.NewV7())
	workerID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		run: db.Run{
			ID:                pgvalue.UUID(runID),
			OrgID:             pgvalue.UUID(dbtest.DefaultOrgID),
			TaskID:            "deploy",
			Status:            db.RunStatusRunning,
			CurrentRunLeaseID: pgvalue.UUID(sessionID),
			CreatedAt:         testTime(),
			UpdatedAt:         testTime(),
			StartedAt:         testTime(),
		},
		sessionID:                 pgvalue.UUID(sessionID),
		executionWorkerInstanceID: pgvalue.UUID(workerID),
		executionLeaseExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, WorkerTokenSecret: []byte(testWorkerTokenSecret), WorkerTokenTTL: time.Hour})
	workerBearer := mintTestWorkerToken(t, server, workerID.String())
	exitCode := int32(0)
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
		Result: api.WorkerReleaseResult{
			Kind:     "completed",
			ExitCode: &exitCode,
			Workspace: &api.WorkerWorkspace{
				ID:                uuid.Must(uuid.NewV7()).String(),
				WriteLeaseID:      "not-a-uuid",
				WriteFencingToken: "workspace-fence-1",
				MountPath:         "/workspace",
				Artifact: &api.WorkerWorkspaceArtifact{
					Digest:     "sha256:" + strings.Repeat("a", 64),
					SizeBytes:  123,
					MediaType:  "application/vnd.helmr.workspace.v0.tar",
					Encoding:   "tar",
					EntryCount: 1,
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/release", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("release status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "workspace.write_lease_id must be a UUID") {
		t.Fatalf("release body = %s", rec.Body.String())
	}
	if len(store.events) != 0 {
		t.Fatalf("events = %+v", store.events)
	}
}

func TestWorkerReleaseRejectsWorkspaceCommitWithoutCAS(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	sessionID := uuid.Must(uuid.NewV7())
	workerID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		run: db.Run{
			ID:                pgvalue.UUID(runID),
			OrgID:             pgvalue.UUID(dbtest.DefaultOrgID),
			TaskID:            "deploy",
			Status:            db.RunStatusRunning,
			CurrentRunLeaseID: pgvalue.UUID(sessionID),
			CreatedAt:         testTime(),
			UpdatedAt:         testTime(),
			StartedAt:         testTime(),
		},
		sessionID:                 pgvalue.UUID(sessionID),
		executionWorkerInstanceID: pgvalue.UUID(workerID),
		executionLeaseExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, WorkerTokenSecret: []byte(testWorkerTokenSecret), WorkerTokenTTL: time.Hour})
	workerBearer := mintTestWorkerToken(t, server, workerID.String())
	exitCode := int32(0)
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
		Result: api.WorkerReleaseResult{
			Kind:     "completed",
			ExitCode: &exitCode,
			Workspace: &api.WorkerWorkspace{
				ID:                uuid.Must(uuid.NewV7()).String(),
				WriteLeaseID:      uuid.Must(uuid.NewV7()).String(),
				WriteFencingToken: "workspace-fence-1",
				MountPath:         "/workspace",
				Artifact: &api.WorkerWorkspaceArtifact{
					Digest:     "sha256:" + strings.Repeat("b", 64),
					SizeBytes:  123,
					MediaType:  "application/vnd.helmr.workspace.v0.tar",
					Encoding:   "tar",
					EntryCount: 1,
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/release", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("release status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "workspace CAS is not configured") {
		t.Fatalf("release body = %s", rec.Body.String())
	}
	if len(store.events) != 0 {
		t.Fatalf("events = %+v", store.events)
	}
}

func TestWorkerReleaseDoesNotAckWhenDurableReleaseFails(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	sessionID := uuid.Must(uuid.NewV7())
	workerID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		run: db.Run{
			ID:                pgvalue.UUID(runID),
			OrgID:             pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:         testProjectID(),
			EnvironmentID:     testEnvironmentID(),
			DeploymentID:      testDeploymentID(),
			DeploymentTaskID:  testDeploymentTaskID(),
			TaskID:            "deploy",
			Status:            db.RunStatusRunning,
			CurrentRunLeaseID: pgvalue.UUID(sessionID),
			CreatedAt:         testTime(),
			UpdatedAt:         testTime(),
			StartedAt:         testTime(),
		},
		sessionID:                 pgvalue.UUID(sessionID),
		executionWorkerInstanceID: pgvalue.UUID(workerID),
		executionLeaseExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, WorkerTokenSecret: []byte(testWorkerTokenSecret), WorkerTokenTTL: time.Hour})
	workerBearer := mintTestWorkerToken(t, server, workerID.String())
	exitCode := int32(0)
	body, err := json.Marshal(api.WorkerReleaseRequest{
		Lease: api.WorkerRunLease{
			ID:                sessionID.String(),
			OrgID:             dbtest.DefaultOrgID.String(),
			RunID:             runID.String(),
			WorkerInstanceID:  workerID.String(),
			ProtocolVersion:   api.CurrentWorkerProtocolVersion,
			AttemptNumber:     1,
			DispatchMessageID: "stale-message",
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

	if rec.Code != http.StatusConflict {
		t.Fatalf("release status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.ackedLeases) != 0 {
		t.Fatalf("acked leases = %+v", store.ackedLeases)
	}
}

func TestWorkerRoutesRejectUserAPIKey(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, WorkerTokenSecret: []byte("01234567890123456789012345678901"), WorkerTokenTTL: time.Hour})

	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/lease", bytes.NewReader(testWorkerRunLeaseRequestBody(t)))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerBootstrapIssuesCredentialForTokenExchange(t *testing.T) {
	authSecret := []byte(testWorkerTokenSecret)
	bootstrapToken := auth.WorkerBootstrapTokenPrefix + "bootstrap-token"
	bootstrapHash, err := auth.HashToken(authSecret, bootstrapToken)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{workerBootstrapTokenHash: bootstrapHash}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, WorkerTokenSecret: []byte(testWorkerTokenSecret), WorkerTokenTTL: time.Hour, AuthSecret: []byte(string(authSecret)), PublicURL: mustParseTestURL("http://127.0.0.1:8080")})

	registerBody, err := json.Marshal(api.WorkerRegisterRequest{
		BootstrapToken: bootstrapToken,
		ResourceID:     "worker-resource-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/register", bytes.NewReader(registerBody))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register status = %d body=%s", rec.Code, rec.Body.String())
	}
	var registered api.WorkerRegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(registered.WorkerInstanceID); err != nil || registered.WorkerInstanceID == "worker-resource-1" || !strings.HasPrefix(registered.WorkerInstanceSecret, auth.WorkerInstanceSecretPrefix) {
		t.Fatalf("register response = %+v", registered)
	}

	tokenBody, err := json.Marshal(api.WorkerTokenRequest{
		WorkerInstanceID:     registered.WorkerInstanceID,
		WorkerInstanceSecret: registered.WorkerInstanceSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/worker/auth/token", bytes.NewReader(tokenBody))
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("token status = %d body=%s", rec.Code, rec.Body.String())
	}
	var token api.WorkerTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	if token.Token == "" || token.ExpiresInSeconds <= 0 {
		t.Fatalf("token response = %+v", token)
	}
}

func TestWorkerRunLeaseRejectsMismatchedWorkerID(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, WorkerTokenSecret: []byte("01234567890123456789012345678901"), WorkerTokenTTL: time.Hour})
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000402")
	claim := api.WorkerRunLease{
		ID:                uuid.Must(uuid.NewV7()).String(),
		OrgID:             dbtest.DefaultOrgID.String(),
		RunID:             uuid.Must(uuid.NewV7()).String(),
		WorkerInstanceID:  "00000000-0000-0000-0000-000000000401",
		ProtocolVersion:   api.CurrentWorkerProtocolVersion,
		AttemptNumber:     1,
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
	}
	body, err := json.Marshal(api.WorkerStartRequest{Lease: claim})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/start", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerRunLeaseRejectsMissingAttemptNumber(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, WorkerTokenSecret: []byte("01234567890123456789012345678901"), WorkerTokenTTL: time.Hour})
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000401")
	claim := api.WorkerRunLease{
		ID:                uuid.Must(uuid.NewV7()).String(),
		OrgID:             dbtest.DefaultOrgID.String(),
		RunID:             uuid.Must(uuid.NewV7()).String(),
		WorkerInstanceID:  "00000000-0000-0000-0000-000000000401",
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
	}
	body, err := json.Marshal(api.WorkerStartRequest{Lease: claim})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/start", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerRunLeaseRejectsMissingProtocolVersion(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, WorkerTokenSecret: []byte("01234567890123456789012345678901"), WorkerTokenTTL: time.Hour})
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000401")
	claim := api.WorkerRunLease{
		ID:                uuid.Must(uuid.NewV7()).String(),
		OrgID:             dbtest.DefaultOrgID.String(),
		RunID:             uuid.Must(uuid.NewV7()).String(),
		WorkerInstanceID:  "00000000-0000-0000-0000-000000000401",
		AttemptNumber:     1,
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
	}
	body, err := json.Marshal(api.WorkerStartRequest{Lease: claim})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/start", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerRunLeaseRejectsMismatchedProtocolVersion(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	sessionID := uuid.Must(uuid.NewV7())
	workerID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		run: db.Run{
			ID:                  pgvalue.UUID(runID),
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			TaskID:              "deploy",
			Status:              db.RunStatusRunning,
			Output:              []byte(`{}`),
			MaxActiveDurationMs: 3600_000,
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
		sessionID:                 pgvalue.UUID(sessionID),
		executionWorkerInstanceID: pgvalue.UUID(workerID),
		executionLeaseExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, WorkerTokenSecret: []byte(testWorkerTokenSecret), WorkerTokenTTL: time.Hour})
	workerBearer := mintTestWorkerToken(t, server, workerID.String())
	body, err := json.Marshal(api.WorkerRenewRequest{Lease: api.WorkerRunLease{
		ID:                sessionID.String(),
		OrgID:             dbtest.DefaultOrgID.String(),
		RunID:             runID.String(),
		WorkerInstanceID:  workerID.String(),
		ProtocolVersion:   "helmr.worker.future",
		AttemptNumber:     1,
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
		ExpiresAt:         time.Now().Add(time.Minute),
	}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/renew", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerWorkspaceMaterializationClaimSkipsWhenDiskUnavailable(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		workerQueueCapacitySet: true,
		workerQueueCapacity: db.GetWorkerInstanceQueueCapacityRow{
			AvailableMilliCpu:       2000,
			AvailableMemoryMib:      2048,
			AvailableDiskMib:        0,
			AvailableExecutionSlots: 1,
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, WorkerTokenSecret: []byte(testWorkerTokenSecret), WorkerTokenTTL: time.Hour})
	workerBearer := mintTestWorkerToken(t, server, workerID.String())
	body, err := json.Marshal(api.WorkerWorkspaceMaterializationClaimRequest{Capabilities: testWorkerCapabilities()})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/workspaces/materializations/claim", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.WorkerWorkspaceMaterializationClaimResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Materialization != nil {
		t.Fatalf("materialization = %+v, want nil", response.Materialization)
	}
	if store.claimWorkspaceMaterializationCalls != 0 {
		t.Fatalf("workspace materialization claims = %d, want 0", store.claimWorkspaceMaterializationCalls)
	}
}

func TestWorkerRunLeaseRejectsMismatchedAttemptNumber(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	sessionID := uuid.Must(uuid.NewV7())
	workerID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		run: db.Run{
			ID:                  pgvalue.UUID(runID),
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			TaskID:              "deploy",
			Status:              db.RunStatusRunning,
			Output:              []byte(`{}`),
			MaxActiveDurationMs: 3600_000,
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
		sessionID:                 pgvalue.UUID(sessionID),
		executionWorkerInstanceID: pgvalue.UUID(workerID),
		executionLeaseExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, WorkerTokenSecret: []byte(testWorkerTokenSecret), WorkerTokenTTL: time.Hour})
	workerBearer := mintTestWorkerToken(t, server, workerID.String())
	body, err := json.Marshal(api.WorkerRenewRequest{Lease: api.WorkerRunLease{
		ID:                sessionID.String(),
		OrgID:             dbtest.DefaultOrgID.String(),
		RunID:             runID.String(),
		WorkerInstanceID:  workerID.String(),
		ProtocolVersion:   api.CurrentWorkerProtocolVersion,
		AttemptNumber:     2,
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
		ExpiresAt:         time.Now().Add(time.Minute),
	}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/renew", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerUpdateRunMetadataUsesRunLeaseAuthority(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	runLeaseID := uuid.Must(uuid.NewV7())
	workerID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		run: db.Run{
			ID:                  pgvalue.UUID(runID),
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			TaskID:              "metadata-smoke",
			Status:              db.RunStatusRunning,
			Output:              []byte(`{}`),
			MaxActiveDurationMs: 3600_000,
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
		sessionID:                 pgvalue.UUID(runLeaseID),
		executionWorkerInstanceID: pgvalue.UUID(workerID),
		executionLeaseExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{}, WorkerTokenSecret: []byte(testWorkerTokenSecret), WorkerTokenTTL: time.Hour})
	workerBearer := mintTestWorkerToken(t, server, workerID.String())
	body, err := json.Marshal(api.WorkerUpdateRunMetadataRequest{
		Lease: api.WorkerRunLease{
			ID:                runLeaseID.String(),
			OrgID:             dbtest.DefaultOrgID.String(),
			RunID:             runID.String(),
			WorkerInstanceID:  workerID.String(),
			ProtocolVersion:   api.CurrentWorkerProtocolVersion,
			AttemptNumber:     1,
			DispatchMessageID: "message-1",
			DispatchLeaseID:   "lease-1",
			ExpiresAt:         time.Now().Add(time.Minute),
		},
		Operation: "set",
		Key:       "runtimeSmoke",
		Value:     json.RawMessage(`{"ok":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/metadata", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.updateRunMetadata.Operation != "set" || store.updateRunMetadata.Key != "runtimeSmoke" || string(store.updateRunMetadata.Value) != `{"ok":true}` {
		t.Fatalf("metadata update = %+v", store.updateRunMetadata)
	}
}

func mintTestWorkerToken(t *testing.T, server http.Handler, workerID string) string {
	t.Helper()
	token, err := auth.IssueWorkerToken([]byte(testWorkerTokenSecret), auth.WorkerClaims{
		WorkerInstanceID: workerID,
		CredentialID:     testWorkerInstanceCredentialID,
		IssuedAt:         time.Now(),
		ExpiresAt:        time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (f *fakeStore) ListQueueScopes(_ context.Context, arg db.ListQueueScopesParams) ([]db.ListQueueScopesRow, error) {
	f.listQueueScopes = arg
	return []db.ListQueueScopesRow{{
		OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:     fakeRunProjectID(f.run),
		EnvironmentID: fakeRunEnvironmentID(f.run),
		QueueName:     "queue-a",
	}}, nil
}

func (f *fakeStore) UpsertWorkerInstanceHeartbeat(_ context.Context, arg db.UpsertWorkerInstanceHeartbeatParams) (db.UpsertWorkerInstanceHeartbeatRow, error) {
	return db.UpsertWorkerInstanceHeartbeatRow{
		ID:                      arg.ID,
		ResourceID:              arg.ResourceID,
		Status:                  db.WorkerInstanceStatusActive,
		WorkerVersion:           arg.WorkerVersion,
		ProtocolVersion:         arg.ProtocolVersion,
		Region:                  arg.Region,
		TotalMilliCpu:           arg.TotalMilliCpu,
		TotalMemoryMib:          arg.TotalMemoryMib,
		TotalDiskMib:            arg.TotalDiskMib,
		TotalExecutionSlots:     arg.TotalExecutionSlots,
		AvailableMilliCpu:       arg.AvailableMilliCpu,
		AvailableMemoryMib:      arg.AvailableMemoryMib,
		AvailableDiskMib:        arg.AvailableDiskMib,
		AvailableExecutionSlots: arg.AvailableExecutionSlots,
		Labels:                  arg.Labels,
		Heartbeat:               arg.Heartbeat,
		RuntimeID:               arg.RuntimeID,
		RuntimeArch:             arg.RuntimeArch,
		RuntimeABI:              arg.RuntimeABI,
		KernelDigest:            arg.KernelDigest,
		InitramfsDigest:         arg.InitramfsDigest,
		RootfsDigest:            arg.RootfsDigest,
		CniProfile:              arg.CniProfile,
		FirstSeenAt:             testTime(),
		LastSeenAt:              testTime(),
	}, nil
}

func (f *fakeStore) EnsureRuntimeReleaseSelection(context.Context, string) error {
	return nil
}

func (f *fakeStore) MarkStaleWorkspaceMaterializationsLost(context.Context, pgtype.Timestamptz) ([]db.MarkStaleWorkspaceMaterializationsLostRow, error) {
	f.markStaleWorkspaceMaterializationsLostCalls++
	return nil, nil
}

func (f *fakeStore) GetWorkerInstanceState(_ context.Context, id pgtype.UUID) (db.GetWorkerInstanceStateRow, error) {
	return db.GetWorkerInstanceStateRow{
		ID:               id,
		ResourceID:       pgvalue.MustUUIDValue(id).String(),
		Status:           db.WorkerInstanceStatusActive,
		ActiveExecutions: 0,
	}, nil
}

func (f *fakeStore) GetWorkerInstanceQueueCapacity(context.Context, pgtype.UUID) (db.GetWorkerInstanceQueueCapacityRow, error) {
	if f.workerQueueCapacitySet {
		return f.workerQueueCapacity, nil
	}
	return db.GetWorkerInstanceQueueCapacityRow{
		AvailableMilliCpu:       2000,
		AvailableMemoryMib:      2048,
		AvailableDiskMib:        20480,
		AvailableExecutionSlots: 1,
	}, nil
}

func (f *fakeStore) GetWorkerInstanceRunDispatchCapacity(context.Context, pgtype.UUID) (db.GetWorkerInstanceRunDispatchCapacityRow, error) {
	if f.workerQueueCapacitySet {
		return db.GetWorkerInstanceRunDispatchCapacityRow{
			AvailableMilliCpu:       f.workerQueueCapacity.AvailableMilliCpu,
			AvailableMemoryMib:      f.workerQueueCapacity.AvailableMemoryMib,
			AvailableDiskMib:        f.workerQueueCapacity.AvailableDiskMib,
			AvailableExecutionSlots: f.workerQueueCapacity.AvailableExecutionSlots,
		}, nil
	}
	return db.GetWorkerInstanceRunDispatchCapacityRow{
		AvailableMilliCpu:       2000,
		AvailableMemoryMib:      2048,
		AvailableDiskMib:        20480,
		AvailableExecutionSlots: 1,
	}, nil
}

func (f *fakeStore) ClaimWorkspaceMaterialization(_ context.Context, arg db.ClaimWorkspaceMaterializationParams) (db.ClaimWorkspaceMaterializationRow, error) {
	f.claimWorkspaceMaterialization = arg
	f.claimWorkspaceMaterializationCalls++
	return db.ClaimWorkspaceMaterializationRow{}, pgx.ErrNoRows
}

func (f *fakeStore) SetWorkerInstanceStatus(_ context.Context, arg db.SetWorkerInstanceStatusParams) (db.WorkerInstance, error) {
	return db.WorkerInstance{
		ID:         arg.ID,
		ResourceID: pgvalue.MustUUIDValue(arg.ID).String(),
		Status:     arg.Status,
	}, nil
}

func (f *fakeStore) Enqueue(context.Context, dispatch.Message) (dispatch.EnqueueResult, error) {
	return dispatch.EnqueueResult{}, nil
}

func (f *fakeStore) Dequeue(_ context.Context, request dispatch.DequeueRequest) ([]dispatch.Lease, error) {
	f.dequeueRequest = request
	if f.run.Status != db.RunStatusQueued {
		return nil, nil
	}
	return []dispatch.Lease{{
		ID:               "lease-1",
		MessageID:        "message-1",
		WorkerInstanceID: request.WorkerInstanceID,
		Message: dispatch.Message{
			OrgID:     dbtest.DefaultOrgID.String(),
			RunID:     pgvalue.MustUUIDValue(f.run.ID).String(),
			QueueName: "queue-a",
		},
		AttemptNumber: 1,
		ExpiresAt:     testTime().Time.Add(time.Minute),
	}}, nil
}

func (f *fakeStore) ReadyMessageExists(context.Context, string) (bool, error) {
	return false, nil
}

func (f *fakeStore) Ack(_ context.Context, lease dispatch.Lease) error {
	f.ackedLeases = append(f.ackedLeases, lease)
	return nil
}

func (f *fakeStore) Nack(context.Context, dispatch.Lease, dispatch.NackReason) error {
	return nil
}

func (f *fakeStore) Renew(_ context.Context, lease dispatch.Lease, expiresAt time.Time) (dispatch.Lease, error) {
	if f.renewErr != nil {
		return dispatch.Lease{}, f.renewErr
	}
	lease.ExpiresAt = expiresAt
	return lease, nil
}

func (f *fakeStore) CompleteRunQueueItem(_ context.Context, arg db.CompleteRunQueueItemParams) (db.RunQueueItem, error) {
	if f.run.ID != arg.RunID || f.executionWorkerInstanceID != arg.WorkerInstanceID || arg.DispatchMessageID.String != "message-1" {
		return db.RunQueueItem{}, pgx.ErrNoRows
	}
	return db.RunQueueItem{
		RunID:                      arg.RunID,
		OrgID:                      arg.OrgID,
		Status:                     db.RunQueueStatusCompleted,
		QueueName:                  "queue-a",
		DispatchMessageID:          pgtype.Text{String: "message-1", Valid: true},
		ReservedByWorkerInstanceID: arg.WorkerInstanceID,
		ReservationExpiresAt:       f.executionLeaseExpiresAt,
		EnqueuedAt:                 testTime(),
		UpdatedAt:                  testTime(),
		FinishedAt:                 testTime(),
	}, nil
}

func (f *fakeStore) RequeueRunQueueItem(_ context.Context, arg db.RequeueRunQueueItemParams) (db.RunQueueItem, error) {
	if f.run.ID != arg.RunID || f.executionWorkerInstanceID != arg.WorkerInstanceID || arg.DispatchMessageID.String != "message-1" {
		return db.RunQueueItem{}, pgx.ErrNoRows
	}
	return db.RunQueueItem{
		RunID:      arg.RunID,
		OrgID:      arg.OrgID,
		Status:     db.RunQueueStatusQueued,
		QueueName:  "queue-a",
		LastError:  arg.LastError,
		EnqueuedAt: testTime(),
		UpdatedAt:  testTime(),
	}, nil
}

func (f *fakeStore) RenewRunQueueReservation(_ context.Context, arg db.RenewRunQueueReservationParams) (db.RunQueueItem, error) {
	if f.run.ID != arg.RunID || f.executionWorkerInstanceID != arg.WorkerInstanceID || arg.DispatchMessageID.String != "message-1" {
		return db.RunQueueItem{}, pgx.ErrNoRows
	}
	return db.RunQueueItem{
		RunID:                      arg.RunID,
		OrgID:                      arg.OrgID,
		Status:                     db.RunQueueStatusReserved,
		QueueName:                  "queue-a",
		DispatchMessageID:          pgtype.Text{String: "message-1", Valid: true},
		ReservedByWorkerInstanceID: arg.WorkerInstanceID,
		ReservationExpiresAt:       arg.ReservationExpiresAt,
		EnqueuedAt:                 testTime(),
		UpdatedAt:                  testTime(),
	}, nil
}

func (f *fakeStore) GetRunLeaseQueueLease(_ context.Context, arg db.GetRunLeaseQueueLeaseParams) (db.GetRunLeaseQueueLeaseRow, error) {
	if f.activeQueueLeaseMissing {
		return db.GetRunLeaseQueueLeaseRow{}, pgx.ErrNoRows
	}
	if f.run.ID != arg.RunID || f.sessionID != arg.RunLeaseID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.GetRunLeaseQueueLeaseRow{}, pgx.ErrNoRows
	}
	return db.GetRunLeaseQueueLeaseRow{
		ID:                    f.sessionID,
		RunID:                 f.run.ID,
		ProjectID:             fakeRunProjectID(f.run),
		EnvironmentID:         fakeRunEnvironmentID(f.run),
		WorkerInstanceID:      f.executionWorkerInstanceID,
		WorkerProtocolVersion: api.CurrentWorkerProtocolVersion,
		DispatchMessageID:     "message-1",
		DispatchLeaseID:       "lease-1",
		DispatchAttempt:       1,
		AttemptNumber:         1,
		LeaseExpiresAt:        f.executionLeaseExpiresAt,
		QueueName:             "queue-a",
	}, nil
}

func (f *fakeStore) ReserveRunQueueItem(_ context.Context, arg db.ReserveRunQueueItemParams) (db.RunQueueItem, error) {
	if f.run.ID != arg.RunID || f.run.Status != db.RunStatusQueued {
		return db.RunQueueItem{}, pgx.ErrNoRows
	}
	return db.RunQueueItem{
		RunID:                      arg.RunID,
		OrgID:                      arg.OrgID,
		Status:                     db.RunQueueStatusReserved,
		QueueName:                  "queue-a",
		DispatchMessageID:          arg.DispatchMessageID,
		ReservedByWorkerInstanceID: arg.WorkerInstanceID,
		ReservationExpiresAt:       arg.ReservationExpiresAt,
		EnqueuedAt:                 testTime(),
		UpdatedAt:                  testTime(),
	}, nil
}

func (f *fakeStore) DeadLetterRunQueueItem(_ context.Context, arg db.DeadLetterRunQueueItemParams) (db.DeadLetterRunQueueItemRow, error) {
	if f.run.ID != arg.RunID || f.run.Status != db.RunStatusQueued {
		return db.DeadLetterRunQueueItemRow{}, pgx.ErrNoRows
	}
	return db.DeadLetterRunQueueItemRow{
		RunID:             arg.RunID,
		OrgID:             arg.OrgID,
		Status:            db.RunQueueStatusDeadLettered,
		QueueName:         "queue-a",
		DispatchMessageID: arg.DispatchMessageID,
		LastError:         arg.LastError,
		EnqueuedAt:        testTime(),
		UpdatedAt:         testTime(),
		FinishedAt:        testTime(),
	}, nil
}

func (f *fakeStore) AuthenticateWorkerInstanceCredential(_ context.Context, arg db.AuthenticateWorkerInstanceCredentialParams) (db.AuthenticateWorkerInstanceCredentialRow, error) {
	if len(f.workerCredentialSecretHash) == 0 || !bytes.Equal(arg.SecretHash, f.workerCredentialSecretHash) {
		return db.AuthenticateWorkerInstanceCredentialRow{}, pgx.ErrNoRows
	}
	return db.AuthenticateWorkerInstanceCredentialRow{
		ID:               f.workerCredentialID,
		WorkerInstanceID: arg.WorkerInstanceID,
	}, nil
}

func (f *fakeStore) AuthorizeWorkerInstanceCredential(_ context.Context, arg db.AuthorizeWorkerInstanceCredentialParams) (db.AuthorizeWorkerInstanceCredentialRow, error) {
	credentialID, _ := uuid.Parse(testWorkerInstanceCredentialID)
	allowed := pgvalue.UUID(credentialID)
	if f.workerCredentialID.Valid {
		allowed = f.workerCredentialID
	}
	if arg.CredentialID != allowed {
		return db.AuthorizeWorkerInstanceCredentialRow{}, pgx.ErrNoRows
	}
	return db.AuthorizeWorkerInstanceCredentialRow{
		ID:               arg.CredentialID,
		WorkerInstanceID: arg.WorkerInstanceID,
		WorkerGroupID:    testWorkerGroupID(),
		ResourceID:       pgvalue.MustUUIDValue(arg.WorkerInstanceID).String(),
	}, nil
}

func (f *fakeStore) CreateWorkerInstanceCredentialFromBootstrap(_ context.Context, arg db.CreateWorkerInstanceCredentialFromBootstrapParams) (db.CreateWorkerInstanceCredentialFromBootstrapRow, error) {
	if len(f.workerBootstrapTokenHash) == 0 || !bytes.Equal(arg.BootstrapTokenHash, f.workerBootstrapTokenHash) {
		return db.CreateWorkerInstanceCredentialFromBootstrapRow{}, pgx.ErrNoRows
	}
	f.workerCredentialID = arg.CredentialID
	f.workerCredentialSecretHash = append([]byte(nil), arg.SecretHash...)
	return db.CreateWorkerInstanceCredentialFromBootstrapRow{
		ID:               arg.CredentialID,
		WorkerInstanceID: arg.WorkerInstanceID,
		WorkerGroupID:    testWorkerGroupID(),
		KeyPrefix:        arg.KeyPrefix,
		CreatedAt:        testTime(),
	}, nil
}

func (f *fakeStore) LeaseRunLease(_ context.Context, arg db.LeaseRunLeaseParams) (db.LeaseRunLeaseRow, error) {
	if f.run.Status != db.RunStatusQueued {
		return db.LeaseRunLeaseRow{}, pgx.ErrNoRows
	}
	f.sessionID = arg.RunLeaseID
	f.executionWorkerInstanceID = arg.WorkerInstanceID
	f.executionLeaseExpiresAt = arg.LeaseExpiresAt
	f.run.Status = db.RunStatusRunning
	if !f.run.CurrentAttemptID.Valid {
		f.run.CurrentAttemptID = pgvalue.UUID(uuid.Must(uuid.NewV7()))
	}
	f.run.CurrentAttemptNumber = pgtype.Int4{Int32: 1, Valid: true}
	f.run.CurrentRunLeaseID = f.sessionID
	f.run.StateVersion++
	restoreCheckpointID := pgtype.UUID{}
	if f.run.LatestRuntimeCheckpointID.Valid && f.run.LatestRuntimeCheckpointID == f.checkpoint.ID && f.checkpoint.State == db.RuntimeCheckpointStateReady {
		f.checkpoint.State = db.RuntimeCheckpointStateRestoring
		restoreCheckpointID = f.checkpoint.ID
	}
	projectID := f.run.ProjectID
	if !projectID.Valid {
		projectID = testProjectID()
	}
	environmentID := f.run.EnvironmentID
	if !environmentID.Valid {
		environmentID = testEnvironmentID()
	}
	requirements := testRunRuntimeRequirements()
	networkPolicy, _ := json.Marshal(requirements.Network)
	return db.LeaseRunLeaseRow{
		ID:                                 f.run.ID,
		OrgID:                              f.run.OrgID,
		ProjectID:                          projectID,
		EnvironmentID:                      environmentID,
		SessionID:                          fakeRunSessionID(f.run),
		TaskID:                             f.run.TaskID,
		Status:                             f.run.Status,
		Payload:                            f.run.Output,
		CurrentAttemptID:                   f.run.CurrentAttemptID,
		StateVersion:                       f.run.StateVersion,
		DeploymentTaskID:                   testDeploymentTaskID(),
		DeploymentTaskFilePath:             "src/task.ts",
		DeploymentTaskExportName:           "deploy",
		DeploymentTaskSecretDeclarations:   f.currentDeploymentTaskSecretDeclarations,
		DeploymentWorkerProtocolVersion:    api.CurrentWorkerProtocolVersion,
		DeploymentSourceDigest:             "sha256:" + strings.Repeat("a", 64),
		MaxActiveDurationMs:                f.run.MaxActiveDurationMs,
		ExitCode:                           f.run.ExitCode,
		ErrorMessage:                       f.run.ErrorMessage,
		CreatedAt:                          f.run.CreatedAt,
		UpdatedAt:                          f.run.UpdatedAt,
		StartedAt:                          f.run.StartedAt,
		FinishedAt:                         f.run.FinishedAt,
		RequestedMilliCpu:                  requirements.Resources.MilliCPU,
		RequestedMemoryMib:                 requirements.Resources.MemoryMiB,
		RequestedDiskMib:                   requirements.Resources.DiskMiB,
		RequestedExecutionSlots:            requirements.Resources.Slots,
		RequirementsRuntimeID:              requirements.Runtime.ID,
		RequirementsRuntimeArch:            requirements.Runtime.Arch,
		RequirementsRuntimeAbi:             requirements.Runtime.ABI,
		RequirementsKernelDigest:           requirements.Runtime.KernelDigest,
		RequirementsInitramfsDigest:        requirements.Runtime.InitramfsDigest,
		RequirementsRootfsDigest:           requirements.Runtime.RootfsDigest,
		RequirementsCniProfile:             requirements.Runtime.CNIProfile,
		RequirementsNetworkPolicy:          networkPolicy,
		RunLeaseID:                         f.sessionID,
		RunLeaseWorkerInstanceID:           f.executionWorkerInstanceID,
		RunLeaseDispatchMessageID:          arg.DispatchMessageID.String,
		RunLeaseDispatchLeaseID:            arg.DispatchLeaseID,
		RunLeaseDispatchAttempt:            arg.DispatchAttempt,
		RunLeaseAttemptNumber:              1,
		RunLeaseExpiresAt:                  f.executionLeaseExpiresAt,
		RunLeaseWorkerProtocolVersion:      api.CurrentWorkerProtocolVersion,
		RunLeaseRestoreRuntimeCheckpointID: restoreCheckpointID,
		WorkspaceFencingToken:              pgvalue.Text("workspace-fence-1"),
	}, nil
}

func (f *fakeStore) RequeueExpiredLeasedRunLeases(context.Context, pgtype.UUID) error {
	return nil
}

func (f *fakeStore) ExpireDueTokens(context.Context, pgtype.UUID) ([]db.ExpireDueTokensRow, error) {
	return nil, nil
}

func (f *fakeStore) ResolveDueTimerWaits(context.Context, db.ResolveDueTimerWaitsParams) ([]db.ResolveDueTimerWaitsRow, error) {
	return nil, nil
}

func (f *fakeStore) ExpireDueRunWaits(context.Context, pgtype.UUID) ([]db.RunWait, error) {
	return nil, nil
}

func (f *fakeStore) FailStaleResolvedRunWaits(context.Context, db.FailStaleResolvedRunWaitsParams) ([]db.FailStaleResolvedRunWaitsRow, error) {
	return nil, nil
}

func (f *fakeStore) RequeueResolvedRunWaits(context.Context, db.RequeueResolvedRunWaitsParams) ([]db.RequeueResolvedRunWaitsRow, error) {
	return nil, nil
}

func (f *fakeStore) AbandonLeasedRunLease(_ context.Context, arg db.AbandonLeasedRunLeaseParams) error {
	if f.run.ID != arg.RunID || f.sessionID != arg.RunLeaseID || f.executionWorkerInstanceID != arg.WorkerInstanceID || f.run.Status != db.RunStatusRunning {
		return nil
	}
	f.abandonedClaim = true
	f.run.Status = db.RunStatusQueued
	f.run.CurrentRunLeaseID = pgtype.UUID{}
	if f.checkpoint.State == db.RuntimeCheckpointStateRestoring && f.run.LatestRuntimeCheckpointID == f.checkpoint.ID {
		f.checkpoint.State = db.RuntimeCheckpointStateReady
	}
	return nil
}

func (f *fakeStore) GetRunRestorePayload(_ context.Context, arg db.GetRunRestorePayloadParams) (db.GetRunRestorePayloadRow, error) {
	if f.run.ID != arg.RunID || f.sessionID != arg.RunLeaseID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.GetRunRestorePayloadRow{}, pgx.ErrNoRows
	}
	if !f.run.LatestRuntimeCheckpointID.Valid || f.checkpoint.ID != f.run.LatestRuntimeCheckpointID || f.checkpoint.State != db.RuntimeCheckpointStateRestoring {
		return db.GetRunRestorePayloadRow{}, pgx.ErrNoRows
	}
	return db.GetRunRestorePayloadRow{
		RuntimeCheckpointID: f.checkpoint.ID,
		Manifest:            f.checkpoint.Manifest,
	}, nil
}

func (f *fakeStore) ExpireQueuedRuns(context.Context, pgtype.UUID) error {
	return nil
}

func (f *fakeStore) StartRunLease(_ context.Context, arg db.StartRunLeaseParams) (db.RunStatus, error) {
	if f.run.Status != db.RunStatusRunning || f.sessionID != arg.RunLeaseID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return "", pgx.ErrNoRows
	}
	f.run.Status = db.RunStatusRunning
	f.run.StartedAt = testTime()
	f.run.UpdatedAt = testTime()
	return f.run.Status, nil
}

func (f *fakeStore) RenewRunLease(_ context.Context, arg db.RenewRunLeaseParams) (db.RenewRunLeaseRow, error) {
	if f.sessionID != arg.RunLeaseID || f.executionWorkerInstanceID != arg.WorkerInstanceID || arg.DispatchMessageID != "message-1" || arg.DispatchLeaseID != "lease-1" {
		return db.RenewRunLeaseRow{}, pgx.ErrNoRows
	}
	f.executionLeaseExpiresAt = arg.LeaseExpiresAt
	return db.RenewRunLeaseRow{
		ID:                    f.sessionID,
		WorkerInstanceID:      f.executionWorkerInstanceID,
		WorkerProtocolVersion: api.CurrentWorkerProtocolVersion,
		DispatchMessageID:     arg.DispatchMessageID,
		DispatchLeaseID:       arg.DispatchLeaseID,
		DispatchAttempt:       1,
		LeaseExpiresAt:        f.executionLeaseExpiresAt,
	}, nil
}

func (f *fakeStore) ReleaseRunLease(_ context.Context, arg db.ReleaseRunLeaseParams) (db.ReleaseRunLeaseRow, error) {
	if f.sessionID != arg.RunLeaseID || f.executionWorkerInstanceID != arg.WorkerInstanceID || arg.DispatchMessageID != "message-1" || arg.DispatchLeaseID != "lease-1" {
		return db.ReleaseRunLeaseRow{}, pgx.ErrNoRows
	}
	releaseRow := func() db.ReleaseRunLeaseRow {
		return db.ReleaseRunLeaseRow{
			ID:                  f.run.ID,
			OrgID:               f.run.OrgID,
			TaskID:              f.run.TaskID,
			Status:              f.run.Status,
			Payload:             f.run.Output,
			MaxActiveDurationMs: f.run.MaxActiveDurationMs,
			ExitCode:            f.run.ExitCode,
			ErrorMessage:        f.run.ErrorMessage,
			CreatedAt:           f.run.CreatedAt,
			UpdatedAt:           f.run.UpdatedAt,
			StartedAt:           f.run.StartedAt,
			FinishedAt:          f.run.FinishedAt,
		}
	}
	if f.run.Status == arg.RunStatus && !f.run.CurrentRunLeaseID.Valid && f.run.ExitCode == arg.ExitCode && f.run.ErrorMessage == arg.ErrorMessage && bytes.Equal(f.run.Output, arg.Output) {
		return releaseRow(), nil
	}
	if f.run.Status != db.RunStatusRunning || f.run.CurrentRunLeaseID != arg.RunLeaseID {
		return db.ReleaseRunLeaseRow{}, pgx.ErrNoRows
	}
	f.run.Status = arg.RunStatus
	f.run.CurrentRunLeaseID = pgtype.UUID{}
	f.run.ExitCode = arg.ExitCode
	f.run.Output = arg.Output
	f.run.ErrorMessage = arg.ErrorMessage
	f.run.FinishedAt = testTime()
	f.run.UpdatedAt = testTime()
	f.events = append(f.events, db.Event{
		Seq:       int64(len(f.events) + 1),
		OrgID:     arg.OrgID,
		RunID:     arg.RunID,
		Kind:      arg.TerminalEventKind,
		Payload:   arg.TerminalEventPayload,
		CreatedAt: testTime(),
	})
	if f.checkpoint.State == db.RuntimeCheckpointStateRestoring && f.run.LatestRuntimeCheckpointID == f.checkpoint.ID {
		if arg.ErrorMessage.Valid {
			f.checkpoint.State = db.RuntimeCheckpointStateInvalid
			f.checkpoint.ErrorMessage = arg.ErrorMessage
			f.checkpoint.InvalidatedAt = testTime()
		} else {
			f.checkpoint.State = db.RuntimeCheckpointStateReady
			f.checkpoint.ErrorMessage = pgtype.Text{}
			f.checkpoint.InvalidatedAt = pgtype.Timestamptz{}
		}
	}
	return releaseRow(), nil
}
