package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const testGitSHA = "0123456789abcdef0123456789abcdef01234567"
const testWorkerTokenSecret = "01234567890123456789012345678901"
const testWorkerInstanceCredentialID = "00000000-0000-0000-0000-00000000c001"

func testWorkerGroupID() pgtype.UUID {
	return ids.ToPG(uuid.MustParse("00000000-0000-0000-0000-000000000201"))
}

func testProjectID() pgtype.UUID {
	return ids.ToPG(uuid.MustParse("00000000-0000-0000-0000-000000000301"))
}

func testEnvironmentID() pgtype.UUID {
	return ids.ToPG(uuid.MustParse("00000000-0000-0000-0000-000000000302"))
}

func otherProjectID() pgtype.UUID {
	return ids.ToPG(uuid.MustParse("00000000-0000-0000-0000-000000000311"))
}

func otherEnvironmentID() pgtype.UUID {
	return ids.ToPG(uuid.MustParse("00000000-0000-0000-0000-000000000312"))
}

func testProjectIDString() string {
	return ids.MustFromPG(testProjectID()).String()
}

func testEnvironmentIDString() string {
	return ids.MustFromPG(testEnvironmentID()).String()
}

func testDeploymentID() pgtype.UUID {
	return ids.ToPG(uuid.MustParse("00000000-0000-0000-0000-000000000304"))
}

func testDeploymentTaskID() pgtype.UUID {
	return ids.ToPG(uuid.MustParse("00000000-0000-0000-0000-000000000305"))
}

func testArtifactID() pgtype.UUID {
	return ids.ToPG(uuid.MustParse("00000000-0000-0000-0000-000000000306"))
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

func TestCreateGetAndListRun(t *testing.T) {
	store := &fakeStore{
		currentDeploymentTaskSecretDeclarations: []byte(`[{"name":"API_KEY","env":"API_KEY"}]`),
	}
	runEnqueuer := &fakeRunEnqueuer{}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithSecrets(fakeSecrets{values: api.ResolvedSecrets{"API_KEY": []byte("secret-value")}}), WithRunEnqueuer(runEnqueuer))

	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:  "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
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
	if store.run.TaskID != "deploy" {
		t.Fatalf("stored task = %s", store.run.TaskID)
	}
	if string(store.createRun.Payload) != `{"env":"prod"}` {
		t.Fatalf("payload = %s", store.createRun.Payload)
	}
	if store.createRun.MaxDurationSeconds != 300 {
		t.Fatalf("max duration = %d", store.createRun.MaxDurationSeconds)
	}
	if store.currentDeploymentTaskCalls != 1 {
		t.Fatalf("current deployment task calls = %d, want 1", store.currentDeploymentTaskCalls)
	}
	if store.getDeploymentTaskCalls != 0 {
		t.Fatalf("deployment task calls = %d, want 0 for unpinned run", store.getDeploymentTaskCalls)
	}
	if store.runEvent.Kind != "run.created" {
		t.Fatalf("run event kind = %s", store.runEvent.Kind)
	}
	var eventPayload struct {
		TaskID             string          `json:"task_id"`
		Payload            json.RawMessage `json:"payload"`
		MaxDurationSeconds int32           `json:"max_duration_seconds"`
		SecretNames        []string        `json:"secret_names"`
	}
	if err := json.Unmarshal(store.runEvent.Payload, &eventPayload); err != nil {
		t.Fatalf("run event payload decode: %v", err)
	}
	if eventPayload.TaskID != "deploy" || string(eventPayload.Payload) != `{"env":"prod"}` || eventPayload.MaxDurationSeconds != 300 {
		t.Fatalf("run event payload = %+v", eventPayload)
	}
	if len(eventPayload.SecretNames) != 1 || eventPayload.SecretNames[0] != "API_KEY" {
		t.Fatalf("run event secret names = %+v", eventPayload.SecretNames)
	}
	if runEnqueuer.orgID != store.run.OrgID || runEnqueuer.runID != store.run.ID {
		t.Fatalf("enqueued org=%+v run=%+v, want org=%+v run=%+v", runEnqueuer.orgID, runEnqueuer.runID, store.run.OrgID, store.run.ID)
	}

	var created api.RunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.DeploymentID != ids.MustFromPG(testDeploymentID()).String() || created.DeploymentTaskID != ids.MustFromPG(testDeploymentTaskID()).String() {
		t.Fatalf("created deployment pin = %s/%s", created.DeploymentID, created.DeploymentTaskID)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+created.ID, nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.listRuns.StatusFilter != "live" || store.listRuns.RowLimit != 100 {
		t.Fatalf("list params = %+v", store.listRuns)
	}
	var list api.ListRunsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Runs) != 1 || list.Runs[0].ID != created.ID {
		t.Fatalf("list = %+v", list)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs/counts", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("counts status = %d body=%s", rec.Code, rec.Body.String())
	}
	var counts api.RunCountsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &counts); err != nil {
		t.Fatal(err)
	}
	if counts.Queued != 1 || counts.Running != 0 || counts.Failed != 0 {
		t.Fatalf("counts = %+v", counts)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs/counts?project_id="+created.ProjectID+"&environment_id="+created.EnvironmentID, nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scoped counts status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.countScopedRuns.ProjectID != testProjectID() || store.countScopedRuns.EnvironmentID != testEnvironmentID() {
		t.Fatalf("scoped count params = %+v", store.countScopedRuns)
	}
}

func TestDeploymentTaskSecretNames(t *testing.T) {
	names, err := deploymentTaskSecretNames([]byte(`[
		{"name":"GITHUB_TOKEN","env":"GITHUB_TOKEN"},
		{"name":"API_KEY","file":"/run/secrets/api_key"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "API_KEY" || names[1] != "GITHUB_TOKEN" {
		t.Fatalf("names = %+v", names)
	}

	_, err = deploymentTaskSecretNames([]byte(`[{"name":"API_KEY","env":"API_KEY"},{"name":"API_KEY","file":"/tmp/key"}]`))
	if err == nil || !strings.Contains(err.Error(), `duplicate secret declaration "API_KEY"`) {
		t.Fatalf("duplicate err = %v", err)
	}

	_, err = deploymentTaskSecretNames([]byte(`[{"name":"/bad","env":"BAD"}]`))
	if err == nil || !strings.Contains(err.Error(), "secret name") {
		t.Fatalf("invalid err = %v", err)
	}
}

func TestCreateRunWithoutSecretsAllowsDeveloper(t *testing.T) {
	store := &fakeStore{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{role: auth.RoleDeveloper}),
		WithSecrets(fakeSecrets{}),
	)
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID: "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer developer-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIKeyRunCreateRejectsOmittedScopeWithoutEnvironmentGrant(t *testing.T) {
	store := &fakeStore{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{kind: auth.ActorKindAPIKey, role: auth.RoleOwner}),
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

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "exactly one environment-scoped runs.create grant") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if store.createRun.ID.Valid {
		t.Fatalf("run was created: %+v", store.createRun)
	}
}

func TestAPIKeyRunCreateInfersEnvironmentGrant(t *testing.T) {
	store := &fakeStore{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{
			kind: auth.ActorKindAPIKey,
			permissions: []auth.PermissionGrant{{
				ProjectID:     testProjectIDString(),
				EnvironmentID: testEnvironmentIDString(),
				Permissions:   []auth.Permission{auth.PermissionRunsCreate},
			}},
		}),
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

func TestAPIKeyRunCreateInfersSingleDefaultGrant(t *testing.T) {
	store := &fakeStore{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{
			kind: auth.ActorKindAPIKey,
			permissions: []auth.PermissionGrant{{
				Permissions: []auth.Permission{auth.PermissionRunsCreate},
			}},
		}),
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

func TestAPIKeyRunCreateRejectsOmittedScopeWithMultipleEnvironmentGrants(t *testing.T) {
	store := &fakeStore{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{
			kind: auth.ActorKindAPIKey,
			permissions: []auth.PermissionGrant{
				{
					ProjectID:     testProjectIDString(),
					EnvironmentID: testEnvironmentIDString(),
					Permissions:   []auth.Permission{auth.PermissionRunsCreate},
				},
				{
					ProjectID:     "00000000-0000-0000-0000-000000000401",
					EnvironmentID: "00000000-0000-0000-0000-000000000402",
					Permissions:   []auth.Permission{auth.PermissionRunsCreate},
				},
			},
		}),
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

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "exactly one environment-scoped runs.create grant") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if store.createRun.ID.Valid {
		t.Fatalf("run was created: %+v", store.createRun)
	}
}

func TestAPIKeyRunCreatePreservesExplicitScopePermission(t *testing.T) {
	store := &fakeStore{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{
			kind: auth.ActorKindAPIKey,
			permissions: []auth.PermissionGrant{{
				ProjectID:     auth.DefaultProjectID,
				EnvironmentID: auth.DefaultEnvironmentID,
				Permissions:   []auth.Permission{auth.PermissionRunsCreate},
			}},
		}),
	)
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:        "deploy",
		ProjectID:     auth.DefaultProjectID,
		EnvironmentID: auth.DefaultEnvironmentID,
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
}

func TestAPIKeyRunCreateAllowsDeclaredTaskSecrets(t *testing.T) {
	store := &fakeStore{
		currentDeploymentTaskSecretDeclarations: []byte(`[{"name":"API_KEY","env":"API_KEY"}]`),
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{
			kind: auth.ActorKindAPIKey,
			permissions: []auth.PermissionGrant{{
				ProjectID:     testProjectIDString(),
				EnvironmentID: testEnvironmentIDString(),
				Permissions:   []auth.Permission{auth.PermissionRunsCreate},
			}},
		}),
		WithSecrets(fakeSecrets{values: api.ResolvedSecrets{"API_KEY": []byte("secret-value")}}),
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
	if !store.createRun.ID.Valid {
		t.Fatalf("run was not created")
	}
}

func TestDeviceAuthorizationRequiresSession(t *testing.T) {
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(&fakeStore{}),
		WithAuthenticator(fakeAuth{}),
		WithUserAuth("abcdefghijabcdefghijabcdefghij12", "https://helmr.example.test"),
	)
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

func TestCreateRunReturnsExistingRunForActiveIdempotencyKey(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithSecrets(fakeSecrets{}), WithRunEnqueuer(runEnqueuer))

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
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithSecrets(fakeSecrets{}))

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
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithSecrets(fakeSecrets{}), WithRunEnqueuer(runEnqueuer))

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
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithSecrets(fakeSecrets{}), WithRunEnqueuer(runEnqueuer))

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
		OrgID:                  ids.ToPG(ids.DefaultOrgID),
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
		ids.DefaultOrgID,
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

func TestCreateScheduleRunUsesDeclaredTaskSecrets(t *testing.T) {
	scheduleID := ids.New()
	instanceID := ids.New()
	scheduledAt := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{
		currentDeploymentTaskSecretDeclarations: []byte(`[{"name":"API_KEY","env":"API_KEY"}]`),
	}
	runEnqueuer := &fakeRunEnqueuer{}
	server := &Server{
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:          store,
		secrets:     fakeSecrets{values: api.ResolvedSecrets{"API_KEY": []byte("secret-value")}},
		runEnqueuer: runEnqueuer,
	}
	runID, err := server.CreateScheduleRun(context.Background(), db.GetScheduleTriggerCandidateRow{
		OrgID:           ids.ToPG(ids.DefaultOrgID),
		ProjectID:       testProjectID(),
		EnvironmentID:   testEnvironmentID(),
		ScheduleID:      ids.ToPG(scheduleID),
		InstanceID:      ids.ToPG(instanceID),
		TaskID:          "deploy",
		Cron:            "0 9 * * *",
		Timezone:        "UTC",
		RunOptions:      []byte(`{}`),
		Generation:      1,
		NextScheduledAt: pgtype.Timestamptz{Time: scheduledAt, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if runID != store.run.ID || runEnqueuer.count != 1 {
		t.Fatalf("runID=%+v stored=%+v enqueues=%d", runID, store.run.ID, runEnqueuer.count)
	}
	if store.createRun.ScheduleID != ids.ToPG(scheduleID) || store.createRun.ScheduleInstanceID != ids.ToPG(instanceID) {
		t.Fatalf("schedule source = %+v/%+v", store.createRun.ScheduleID, store.createRun.ScheduleInstanceID)
	}
	var eventPayload struct {
		SecretNames []string `json:"secret_names"`
	}
	if err := json.Unmarshal(store.runEvent.Payload, &eventPayload); err != nil {
		t.Fatal(err)
	}
	if len(eventPayload.SecretNames) != 1 || eventPayload.SecretNames[0] != "API_KEY" {
		t.Fatalf("secret names = %+v", eventPayload.SecretNames)
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
		OrgID:                  ids.ToPG(ids.DefaultOrgID),
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
		ids.DefaultOrgID,
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
		OrgID:                  ids.ToPG(ids.DefaultOrgID),
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
		ids.DefaultOrgID,
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

func TestCreateRunIdempotencyReplayBypassesRemovedQueueValidation(t *testing.T) {
	orgID := ids.ToPG(ids.DefaultOrgID)
	store := &fakeStore{deploymentTasks: []db.DeploymentTask{{
		ID:                   testDeploymentTaskID(),
		OrgID:                orgID,
		ProjectID:            testProjectID(),
		EnvironmentID:        testEnvironmentID(),
		DeploymentID:         testDeploymentID(),
		TaskID:               "deploy",
		FilePath:             "tasks/deploy.ts",
		ExportName:           "deploy",
		HandlerEntrypoint:    "tasks/deploy.ts#deploy",
		BundleArtifactID:     testArtifactID(),
		RequestedMilliCpu:    2000,
		RequestedMemoryMib:   2048,
		SecretDeclarations:   []byte("[]"),
		ResourceRequirements: []byte("{}"),
		QueueName:            "reports",
		MaxDurationSeconds:   300,
		CreatedAt:            testTime(),
	}}}
	runEnqueuer := &fakeRunEnqueuer{}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithSecrets(fakeSecrets{}), WithRunEnqueuer(runEnqueuer))

	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:  "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.CreateRunOptions{
			Queue:          &api.RunQueueOption{Name: "reports"},
			IdempotencyKey: "deploy-prod",
		},
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
	store.deploymentTasks[0].QueueName = "default"

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
	if runEnqueuer.count != 1 || len(store.events) != 1 {
		t.Fatalf("events=%d enqueues=%d", len(store.events), runEnqueuer.count)
	}
}

func TestCreateRunIdempotencyHitIncludesPendingWaitpoint(t *testing.T) {
	store := &fakeStore{}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithSecrets(fakeSecrets{}))

	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:  "deploy",
		Payload: json.RawMessage(`{"env":"prod"}`),
		Options: api.CreateRunOptions{IdempotencyKey: "deploy-prod"},
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
	waitpointID := ids.New()
	store.run.Status = db.RunStatusWaiting
	store.waitpoint = fakeWaitpoint{
		ID:          ids.ToPG(waitpointID),
		OrgID:       store.run.OrgID,
		RunID:       store.run.ID,
		Kind:        db.WaitpointKindHuman,
		DisplayText: "ship it",
		Status:      db.RunWaitStatusWaiting,
		RequestedAt: testTime(),
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
	if second.PendingWaitpoint == nil || second.PendingWaitpoint.WaitpointID != waitpointID.String() || second.PendingWaitpoint.DisplayText != "ship it" {
		t.Fatalf("pending wait = %+v", second.PendingWaitpoint)
	}
}

func TestCreateRunHashesLiteralHexIdempotencyKeys(t *testing.T) {
	store := &fakeStore{}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithSecrets(fakeSecrets{}))

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

	base, err := runIdempotencyRequestHash(request, payload, deploymentTask, 300, scheduling)
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
	deploymentHash, err := runIdempotencyRequestHash(request, payload, changedTask, 300, scheduling)
	if err != nil {
		t.Fatal(err)
	}
	requireIdempotencyHashChanged("effective deployment", deploymentHash)
	durationHash, err := runIdempotencyRequestHash(request, payload, deploymentTask, 600, scheduling)
	if err != nil {
		t.Fatal(err)
	}
	requireIdempotencyHashChanged("max duration", durationHash)
	changedScheduling := scheduling
	changedScheduling.queueName = "task/deploy-high"
	queueHash, err := runIdempotencyRequestHash(request, payload, deploymentTask, 300, changedScheduling)
	if err != nil {
		t.Fatal(err)
	}
	requireIdempotencyHashChanged("queue name", queueHash)
	changedScheduling = scheduling
	changedScheduling.concurrencyKey = pgText("deploy:prod")
	concurrencyHash, err := runIdempotencyRequestHash(request, payload, deploymentTask, 300, changedScheduling)
	if err != nil {
		t.Fatal(err)
	}
	requireIdempotencyHashChanged("concurrency key", concurrencyHash)
	changedScheduling = scheduling
	changedScheduling.priority = 100
	priorityHash, err := runIdempotencyRequestHash(request, payload, deploymentTask, 300, changedScheduling)
	if err != nil {
		t.Fatal(err)
	}
	requireIdempotencyHashChanged("priority", priorityHash)
	changedScheduling = scheduling
	changedScheduling.ttl = "30m"
	ttlHash, err := runIdempotencyRequestHash(request, payload, deploymentTask, 300, changedScheduling)
	if err != nil {
		t.Fatal(err)
	}
	requireIdempotencyHashChanged("ttl", ttlHash)
}

func TestCreateRunRejectsInvalidTaskID(t *testing.T) {
	store := &fakeStore{}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}))

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

func TestListRunsQuery(t *testing.T) {
	store := &fakeStore{}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithSecrets(fakeSecrets{}))
	runID := ids.New()
	store.run = db.Run{
		ID:               ids.ToPG(runID),
		OrgID:            ids.ToPG(ids.DefaultOrgID),
		ProjectID:        testProjectID(),
		EnvironmentID:    testEnvironmentID(),
		DeploymentID:     testDeploymentID(),
		DeploymentTaskID: testDeploymentTaskID(),
		TaskID:           "deploy",
		Status:           db.RunStatusSucceeded,
		CreatedAt:        testTime(),
		UpdatedAt:        testTime(),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/runs?status=all&limit=25", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.listRuns.StatusFilter != "all" || store.listRuns.RowLimit != 25 {
		t.Fatalf("list params = %+v", store.listRuns)
	}
}

func TestListRunsQueryRejectsLeasedStatus(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/runs?status=leased", nil)

	if _, _, err := listRunsQuery(req); err == nil {
		t.Fatal("listRunsQuery accepted leased status")
	}
}

func TestListRunsRunningFilterReturnsLeasedAsPublicRunning(t *testing.T) {
	for _, tt := range []struct {
		name string
		path string
	}{
		{name: "org", path: "/api/runs?status=running"},
		{name: "scoped", path: "/api/runs?status=running&project_id=default&environment_id=default"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runID := ids.New()
			store := &fakeStore{
				run: db.Run{
					ID:               ids.ToPG(runID),
					OrgID:            ids.ToPG(ids.DefaultOrgID),
					ProjectID:        testProjectID(),
					EnvironmentID:    testEnvironmentID(),
					DeploymentID:     testDeploymentID(),
					DeploymentTaskID: testDeploymentTaskID(),
					TaskID:           "deploy",
					Status:           db.RunStatusRunning,
					CreatedAt:        testTime(),
					UpdatedAt:        testTime(),
				},
			}
			server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}))

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("authorization", "Bearer test-key")
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if store.listRuns.StatusFilter != "running" {
				t.Fatalf("list status filter = %q, want running", store.listRuns.StatusFilter)
			}
			var list api.ListRunsResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
				t.Fatal(err)
			}
			if len(list.Runs) != 1 || list.Runs[0].ID != runID.String() || list.Runs[0].Status != "running" {
				t.Fatalf("list = %+v", list)
			}
		})
	}
}

func TestRunResponseMapsLeasedToRunning(t *testing.T) {
	response := runResponse(runSummary{
		ID:               ids.ToPG(ids.New()),
		ProjectID:        testProjectID(),
		EnvironmentID:    testEnvironmentID(),
		DeploymentID:     testDeploymentID(),
		DeploymentTaskID: testDeploymentTaskID(),
		TaskID:           "deploy",
		Status:           db.RunStatusRunning,
		CreatedAt:        testTime(),
		UpdatedAt:        testTime(),
	})

	if response.Status != "running" {
		t.Fatalf("status = %q, want running", response.Status)
	}
}

func TestRunCountsResponseMapsLeasedToRunning(t *testing.T) {
	counts := runCountsResponse(db.CountRunsByStatusRow{
		Queued:  2,
		Running: 5,
	})
	if counts.Queued != 2 || counts.Running != 5 {
		t.Fatalf("counts = %+v", counts)
	}

	body, err := json.Marshal(counts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "leased") {
		t.Fatalf("counts leaked leased field: %s", body)
	}

	scoped := scopedRunCountsResponse(db.CountScopedRunsByStatusRow{
		Running: 11,
	})
	if scoped.Running != 11 {
		t.Fatalf("scoped counts = %+v", scoped)
	}
}

func TestGetRunLogs(t *testing.T) {
	runID := ids.New()
	store := &fakeStore{
		run: db.Run{
			ID:        ids.ToPG(runID),
			OrgID:     ids.ToPG(ids.DefaultOrgID),
			TaskID:    "deploy",
			Status:    db.RunStatusRunning,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
		stdout: []byte("hello\n"),
		stderr: []byte("warn\n"),
	}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}))

	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String()+"/logs", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.LogSnapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.StdoutBase64 != base64.StdEncoding.EncodeToString([]byte("hello\n")) || response.StderrBase64 != base64.StdEncoding.EncodeToString([]byte("warn\n")) {
		t.Fatalf("logs = %+v", response)
	}
	if store.runLogSnapshot.StdoutLimit != maxRunLogSnapshotBytes || store.runLogSnapshot.StderrLimit != maxRunLogSnapshotBytes {
		t.Fatalf("log snapshot params = %+v", store.runLogSnapshot)
	}
}

func TestGetRunLogsReportsTruncatedSnapshot(t *testing.T) {
	runID := ids.New()
	store := &fakeStore{
		run: db.Run{
			ID:        ids.ToPG(runID),
			OrgID:     ids.ToPG(ids.DefaultOrgID),
			TaskID:    "deploy",
			Status:    db.RunStatusRunning,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
		stdout:       []byte("hello\n"),
		logTruncated: true,
		stdoutCursor: 42,
	}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}))

	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String()+"/logs", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.LogSnapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Truncated {
		t.Fatalf("logs = %+v", response)
	}
	if response.Cursor != "42:0" {
		t.Fatalf("cursor = %q", response.Cursor)
	}
}

func TestCheckpointArtifactParamsValidation(t *testing.T) {
	stateDigest := "sha256:" + strings.Repeat("1", 64)
	memoryDigest := "sha256:" + strings.Repeat("2", 64)
	scratchDigest := "sha256:" + strings.Repeat("3", 64)
	manifestDigest := "sha256:" + strings.Repeat("4", 64)
	valid := api.WorkerCheckpointManifest{
		RuntimeState: api.WorkerCheckpointRuntimeState{
			ConfigArtifact:      testCheckpointArtifact(manifestDigest, 64, cas.CheckpointRuntimeConfigMediaType),
			VMStateArtifact:     testCheckpointArtifact(stateDigest, 128, cas.CheckpointVMStateMediaType),
			ScratchDiskArtifact: testCheckpointArtifact(scratchDigest, 512, cas.CheckpointScratchDiskMediaType),
			MemoryArtifacts:     []api.WorkerCheckpointArtifact{testCheckpointArtifact(memoryDigest, 256, cas.CheckpointMemoryMediaType)},
		},
	}
	if _, err := checkpointArtifactParams(valid); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		manifest api.WorkerCheckpointManifest
		want     string
	}{
		{
			name: "missing state metadata",
			manifest: withCheckpointManifest(valid, func(m *api.WorkerCheckpointManifest) {
				m.RuntimeState.VMStateArtifact = api.WorkerCheckpointArtifact{}
			}),
			want: "manifest.runtime_state.vm_state_artifact.digest",
		},
		{
			name: "wrong memory media type",
			manifest: withCheckpointManifest(valid, func(m *api.WorkerCheckpointManifest) {
				m.RuntimeState.MemoryArtifacts[0].MediaType = cas.CheckpointVMStateMediaType
			}),
			want: "expected",
		},
		{
			name: "wrong manifest media type",
			manifest: withCheckpointManifest(valid, func(m *api.WorkerCheckpointManifest) {
				m.RuntimeState.ConfigArtifact.MediaType = cas.CheckpointMemoryMediaType
			}),
			want: "expected",
		},
		{
			name: "conflicting duplicate metadata",
			manifest: withCheckpointManifest(valid, func(m *api.WorkerCheckpointManifest) {
				m.RuntimeState.MemoryArtifacts = append(m.RuntimeState.MemoryArtifacts, testCheckpointArtifact(memoryDigest, 257, cas.CheckpointMemoryMediaType))
			}),
			want: "conflicting metadata",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := checkpointArtifactParams(tt.manifest)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCheckpointRuntimeBackendFenceMatchesSQL(t *testing.T) {
	source, err := os.ReadFile("../db/query/waitpoints.sql")
	if err != nil {
		t.Fatal(err)
	}
	expectedFence := "sqlc.arg(runtime_backend)::text = '" + checkpointRuntimeBackendFirecracker + "'"
	if !strings.Contains(string(source), expectedFence) {
		t.Fatalf("checkpoint runtime backend SQL fence missing %q", expectedFence)
	}
}

func testCheckpointArtifact(digest string, sizeBytes int64, mediaType string) api.WorkerCheckpointArtifact {
	return api.WorkerCheckpointArtifact{
		Digest:    digest,
		SizeBytes: sizeBytes,
		MediaType: mediaType,
	}
}

func testWorkerCheckpointManifest(runID string, waitpointID string, checkpointID string) api.WorkerCheckpointManifest {
	runtimeConfig := json.RawMessage(`{"recovery_point":{"runtime":{"vcpu_count":1,"memory_mib":1024,"scratch_disk_mib":2048,"network":{"profile":"helmr/v0"}}}}`)
	capabilities := testWorkerCapabilities()
	return api.WorkerCheckpointManifest{
		RecoveryPoint: api.WorkerCheckpointRecoveryPoint{
			ID:          checkpointID,
			RunID:       runID,
			WaitpointID: waitpointID,
			Runtime: api.WorkerCheckpointRuntime{
				Backend:         "firecracker",
				ID:              capabilities.RuntimeID,
				Arch:            capabilities.RuntimeArch,
				ABI:             capabilities.RuntimeABI,
				KernelDigest:    capabilities.KernelDigest,
				InitramfsDigest: capabilities.InitramfsDigest,
				RootfsDigest:    capabilities.RootfsDigest,
				ConfigDigest:    cas.DigestBytes(runtimeConfig),
			},
		},
		RuntimeState: api.WorkerCheckpointRuntimeState{
			ConfigArtifact:      testCheckpointArtifact("sha256:"+strings.Repeat("7", 64), int64(len(runtimeConfig)), cas.CheckpointRuntimeConfigMediaType),
			VMStateArtifact:     testCheckpointArtifact("sha256:"+strings.Repeat("1", 64), 128, cas.CheckpointVMStateMediaType),
			ScratchDiskArtifact: testCheckpointArtifact("sha256:"+strings.Repeat("6", 64), 512, cas.CheckpointScratchDiskMediaType),
			MemoryArtifacts:     []api.WorkerCheckpointArtifact{testCheckpointArtifact("sha256:"+strings.Repeat("2", 64), 256, cas.CheckpointMemoryMediaType)},
			Config:              runtimeConfig,
		},
		WorkspaceState: api.WorkerCheckpointWorkspaceState{Base: api.WorkerCheckpointWorkspaceBase{
			ArtifactDigest:    "sha256:" + strings.Repeat("8", 64),
			ArtifactSizeBytes: 1024,
			ArtifactMediaType: "application/vnd.helmr.workspace.v0.tar",
			ArtifactEncoding:  "tar",
			MountPath:         "/workspace",
			VolumeKind:        "copy-on-write",
		}},
	}
}

func TestVerifyCheckpointReadyArtifactsRejectsCASMetadataMismatch(t *testing.T) {
	manifest := testWorkerCheckpointManifest("run-1", "waitpoint-1", "checkpoint-1")
	objects := checkpointManifestCASObjects(manifest)
	memory := manifest.RuntimeState.MemoryArtifacts[0]
	objects[memory.Digest] = cas.Object{Digest: memory.Digest, SizeBytes: memory.SizeBytes + 1, MediaType: memory.MediaType}
	server := &Server{cas: &fakeCAS{objects: objects}}

	err := server.verifyCheckpointReadyArtifacts(context.Background(), manifest)

	if err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("err = %v, want size mismatch", err)
	}
}

func checkpointManifestCASObjects(manifest api.WorkerCheckpointManifest) map[string]cas.Object {
	objects := map[string]cas.Object{}
	add := func(digest string, sizeBytes int64, mediaType string) {
		objects[digest] = cas.Object{Digest: digest, SizeBytes: sizeBytes, MediaType: mediaType}
	}
	add(manifest.RuntimeState.ConfigArtifact.Digest, manifest.RuntimeState.ConfigArtifact.SizeBytes, manifest.RuntimeState.ConfigArtifact.MediaType)
	add(manifest.RuntimeState.VMStateArtifact.Digest, manifest.RuntimeState.VMStateArtifact.SizeBytes, manifest.RuntimeState.VMStateArtifact.MediaType)
	add(manifest.RuntimeState.ScratchDiskArtifact.Digest, manifest.RuntimeState.ScratchDiskArtifact.SizeBytes, manifest.RuntimeState.ScratchDiskArtifact.MediaType)
	for _, artifact := range manifest.RuntimeState.MemoryArtifacts {
		add(artifact.Digest, artifact.SizeBytes, artifact.MediaType)
	}
	workspace := manifest.WorkspaceState.Base
	add(workspace.ArtifactDigest, workspace.ArtifactSizeBytes, workspace.ArtifactMediaType)
	return objects
}

func withCheckpointManifest(manifest api.WorkerCheckpointManifest, edit func(*api.WorkerCheckpointManifest)) api.WorkerCheckpointManifest {
	manifest.RuntimeState.MemoryArtifacts = append([]api.WorkerCheckpointArtifact(nil), manifest.RuntimeState.MemoryArtifacts...)
	edit(&manifest)
	return manifest
}

func TestCreateRunRejectsClientSuppliedBundle(t *testing.T) {
	store := &fakeStore{}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}))

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

func TestCreateRunRejectsUnavailableDeclaredSecret(t *testing.T) {
	store := &fakeStore{
		currentDeploymentTaskSecretDeclarations: []byte(`[{"name":"API_KEY","env":"API_KEY"}]`),
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithSecrets(fakeSecrets{values: api.ResolvedSecrets{"other": []byte("secret")}}),
	)
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID: "deploy",
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

func TestRunRoutesRequireBearerAuth(t *testing.T) {
	store := &fakeStore{}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}))

	req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSetSecret(t *testing.T) {
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(&fakeStore{}),
		WithAuthenticator(fakeAuth{}),
		WithSecrets(fakeSecrets{}),
	)

	req := httptest.NewRequest(http.MethodPut, "/api/secrets/github-token", bytes.NewBufferString(`{"value":"secret-value"}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.SecretResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Name != "github-token" {
		t.Fatalf("response = %+v", response)
	}
}

func TestGetSecretReturnsMetadataOnly(t *testing.T) {
	store := &fakeStore{
		secret: db.GetScopedSecretMetadataByNameRow{
			ID:            ids.ToPG(ids.New()),
			OrgID:         ids.ToPG(ids.DefaultOrgID),
			ProjectID:     testProjectID(),
			EnvironmentID: testEnvironmentID(),
			Name:          "github-token",
			CreatedAt:     testTime(),
			UpdatedAt:     testTime(),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithSecrets(fakeSecrets{}),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/secrets/github-token?project_id="+testProjectIDString()+"&environment_id="+testEnvironmentIDString(), nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["value"]; ok {
		t.Fatalf("secret response exposed value: %s", rec.Body.String())
	}
	if _, ok := raw["ciphertext"]; ok {
		t.Fatalf("secret response exposed ciphertext: %s", rec.Body.String())
	}
	var response api.SecretResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Name != "github-token" || response.ProjectID != testProjectIDString() || response.EnvironmentID != testEnvironmentIDString() {
		t.Fatalf("response = %+v", response)
	}
}

func TestSetSecretRequiresOwner(t *testing.T) {
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(&fakeStore{}),
		WithAuthenticator(fakeAuth{role: auth.RoleDeveloper}),
		WithSecrets(fakeSecrets{}),
	)

	req := httptest.NewRequest(http.MethodPut, "/api/secrets/github-token", bytes.NewBufferString(`{"value":"secret-value"}`))
	req.Header.Set("authorization", "Bearer developer-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeleteSecret(t *testing.T) {
	store := &fakeStore{deleteSecretRows: 1}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithSecrets(fakeSecrets{}),
	)

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/github-token?project_id="+testProjectIDString()+"&environment_id="+testEnvironmentIDString(), nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.deleteSecret.Name != "github-token" || store.deleteSecret.ProjectID != testProjectID() || store.deleteSecret.EnvironmentID != testEnvironmentID() {
		t.Fatalf("delete scope = %+v", store.deleteSecret)
	}
}

func TestDeleteSecretNotFound(t *testing.T) {
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(&fakeStore{}),
		WithAuthenticator(fakeAuth{}),
		WithSecrets(fakeSecrets{}),
	)

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/github-token", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSecretRoutesAllowScopedAPIKeyGrant(t *testing.T) {
	store := &fakeStore{
		secret: db.GetScopedSecretMetadataByNameRow{
			ID:            ids.ToPG(ids.New()),
			OrgID:         ids.ToPG(ids.DefaultOrgID),
			ProjectID:     testProjectID(),
			EnvironmentID: testEnvironmentID(),
			Name:          "github-token",
			CreatedAt:     testTime(),
			UpdatedAt:     testTime(),
		},
		deleteSecretRows:     1,
		defaultProjectID:     otherProjectID(),
		defaultEnvironmentID: otherEnvironmentID(),
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{
			kind: auth.ActorKindAPIKey,
			role: auth.RoleOwner,
			permissions: []auth.PermissionGrant{{
				ProjectID:     testProjectIDString(),
				EnvironmentID: testEnvironmentIDString(),
				Permissions:   []auth.Permission{auth.PermissionSecretsWrite},
			}},
		}),
		WithSecrets(fakeSecrets{}),
	)

	for _, tt := range []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{
			name:       "list",
			method:     http.MethodGet,
			path:       "/api/secrets?project_id=" + testProjectIDString() + "&environment_id=" + testEnvironmentIDString(),
			wantStatus: http.StatusOK,
		},
		{
			name:       "get",
			method:     http.MethodGet,
			path:       "/api/secrets/github-token?project_id=" + testProjectIDString() + "&environment_id=" + testEnvironmentIDString(),
			wantStatus: http.StatusOK,
		},
		{
			name:       "delete",
			method:     http.MethodDelete,
			path:       "/api/secrets/github-token?project_id=" + testProjectIDString() + "&environment_id=" + testEnvironmentIDString(),
			wantStatus: http.StatusNoContent,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("authorization", "Bearer test-key")
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCreateRunWithUnknownVersionReturnsVersionError(t *testing.T) {
	store := &fakeStore{}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithSecrets(fakeSecrets{}))

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

func TestWorkerRunLeaseStartAndRelease(t *testing.T) {
	store := &fakeStore{
		run: db.Run{
			ID:                 ids.ToPG(ids.New()),
			OrgID:              ids.ToPG(ids.DefaultOrgID),
			ProjectID:          testProjectID(),
			EnvironmentID:      testEnvironmentID(),
			DeploymentID:       testDeploymentID(),
			DeploymentTaskID:   testDeploymentTaskID(),
			TaskID:             "deploy",
			Status:             db.RunStatusQueued,
			Payload:            []byte(`{"env":"prod"}`),
			MaxDurationSeconds: 3600,
			CreatedAt:          testTime(),
			UpdatedAt:          testTime(),
		},
		currentDeploymentTaskSecretDeclarations: []byte(`[{"name":"API_KEY","env":"API_KEY"}]`),
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithSecrets(fakeSecrets{values: api.ResolvedSecrets{"API_KEY": []byte("secret-value")}}),
		WithWorkerAuth("01234567890123456789012345678901", time.Hour),
	)
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000401")

	capabilities := testWorkerCapabilities()
	capabilities.Region = "us-east-1"
	capabilities.Labels = map[string]string{"pool": "snapshot", "dedicated_key": "tenant-a"}
	body, err := json.Marshal(api.WorkerRunLeaseRequest{Capabilities: capabilities})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/executions/lease", bytes.NewReader(body))
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
	req = httptest.NewRequest(http.MethodPost, "/api/worker/executions/start", bytes.NewReader(startBody))
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
	req = httptest.NewRequest(http.MethodPost, "/api/worker/executions/renew", bytes.NewReader(renewBody))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("renew status = %d body=%s", rec.Code, rec.Body.String())
	}
	store.renewErr = dispatch.ErrMessageNotFound
	req = httptest.NewRequest(http.MethodPost, "/api/worker/executions/renew", bytes.NewReader(renewBody))
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
	req = httptest.NewRequest(http.MethodPost, "/api/worker/executions/release", bytes.NewReader(releaseBody))
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

func TestWorkerRunLeaseRejectsUnsupportedProtocol(t *testing.T) {
	store := &fakeStore{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithWorkerAuth("01234567890123456789012345678901", time.Hour),
	)
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000401")

	capabilities := testWorkerCapabilities()
	capabilities.ProtocolVersion = "helmr.worker.future"
	capabilities.SupportedProtocolVersions = []string{"helmr.worker.future"}
	body, err := json.Marshal(api.WorkerRunLeaseRequest{Capabilities: capabilities})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/executions/lease", bytes.NewReader(body))
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

func TestWorkerReleaseRejectsUnknownFields(t *testing.T) {
	store := &fakeStore{
		run: db.Run{
			ID:                 ids.ToPG(ids.New()),
			OrgID:              ids.ToPG(ids.DefaultOrgID),
			TaskID:             "deploy",
			Status:             db.RunStatusQueued,
			Payload:            []byte(`{}`),
			MaxDurationSeconds: 3600,
			CreatedAt:          testTime(),
			UpdatedAt:          testTime(),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithWorkerAuth("01234567890123456789012345678901", time.Hour),
	)
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000401")
	req := httptest.NewRequest(http.MethodPost, "/api/worker/executions/lease", bytes.NewReader(testWorkerRunLeaseRequestBody(t)))
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
	req = httptest.NewRequest(http.MethodPost, "/api/worker/executions/release", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("release status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerReleaseDoesNotAckWhenDurableReleaseFails(t *testing.T) {
	runID := ids.New()
	executionID := ids.New()
	workerID := ids.New()
	store := &fakeStore{
		run: db.Run{
			ID:                 ids.ToPG(runID),
			OrgID:              ids.ToPG(ids.DefaultOrgID),
			ProjectID:          testProjectID(),
			EnvironmentID:      testEnvironmentID(),
			DeploymentID:       testDeploymentID(),
			DeploymentTaskID:   testDeploymentTaskID(),
			TaskID:             "deploy",
			Status:             db.RunStatusRunning,
			CurrentExecutionID: ids.ToPG(executionID),
			CreatedAt:          testTime(),
			UpdatedAt:          testTime(),
			StartedAt:          testTime(),
		},
		executionID:               ids.ToPG(executionID),
		executionWorkerInstanceID: ids.ToPG(workerID),
		executionLeaseExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithWorkerAuth(testWorkerTokenSecret, time.Hour),
	)
	workerBearer := mintTestWorkerToken(t, server, workerID.String())
	exitCode := int32(0)
	body, err := json.Marshal(api.WorkerReleaseRequest{
		Lease: api.WorkerRunLease{
			ID:                executionID.String(),
			OrgID:             ids.DefaultOrgID.String(),
			RunID:             runID.String(),
			WorkerInstanceID:  workerID.String(),
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
	req := httptest.NewRequest(http.MethodPost, "/api/worker/executions/release", bytes.NewReader(body))
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

func TestWorkerReleaseAllowsIdempotentRetryAfterQueueLeaseGone(t *testing.T) {
	runID := ids.New()
	executionID := ids.New()
	workerID := ids.New()
	exitCode := int32(0)
	store := &fakeStore{
		run: db.Run{
			ID:               ids.ToPG(runID),
			OrgID:            ids.ToPG(ids.DefaultOrgID),
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
		executionID:               ids.ToPG(executionID),
		executionWorkerInstanceID: ids.ToPG(workerID),
		executionLeaseExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
		activeQueueLeaseMissing:   true,
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithWorkerAuth(testWorkerTokenSecret, time.Hour),
	)
	workerBearer := mintTestWorkerToken(t, server, workerID.String())
	body, err := json.Marshal(api.WorkerReleaseRequest{
		Lease: api.WorkerRunLease{
			ID:                executionID.String(),
			OrgID:             ids.DefaultOrgID.String(),
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
	req := httptest.NewRequest(http.MethodPost, "/api/worker/executions/release", bytes.NewReader(body))
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

func TestTerminalRunEventDoesNotTrustWorkerFailureKind(t *testing.T) {
	message := "worker failed"
	kind := "source_unavailable"
	eventKind, payload, err := terminalRunEventForFields(db.RunStatusFailed, pgtype.Int4{}, pgtype.Text{String: message, Valid: true}, api.WorkerReleaseResult{Kind: "failed", FailureKind: &kind})
	if err != nil {
		t.Fatal(err)
	}
	if eventKind != "run.failed" {
		t.Fatalf("event kind = %s", eventKind)
	}
	assertJSONBytes(t, payload, `{"detail":{"message":"worker failed"},"failure_kind":"worker_failed"}`)
	var eventPayload struct {
		FailureKind string `json:"failure_kind"`
	}
	if err := json.Unmarshal(payload, &eventPayload); err != nil {
		t.Fatal(err)
	}
	if eventPayload.FailureKind != "worker_failed" {
		t.Fatalf("failure kind = %s", eventPayload.FailureKind)
	}
}

func TestTerminalRunEventPreservesMaxDurationFailureKind(t *testing.T) {
	message := "runtime max_duration exceeded after 30s active time"
	kind := "max_duration"
	limitSeconds := int32(30)
	eventKind, payload, err := terminalRunEventForFields(db.RunStatusFailed, pgtype.Int4{}, pgtype.Text{String: message, Valid: true}, api.WorkerReleaseResult{Kind: "failed", FailureKind: &kind, LimitSeconds: &limitSeconds})
	if err != nil {
		t.Fatal(err)
	}
	if eventKind != "run.failed" {
		t.Fatalf("event kind = %s", eventKind)
	}
	assertJSONBytes(t, payload, `{"detail":{"limit_seconds":30,"message":"runtime max_duration exceeded after 30s active time"},"failure_kind":"max_duration"}`)
	var eventPayload struct {
		FailureKind string `json:"failure_kind"`
		Detail      struct {
			Message      string `json:"message"`
			LimitSeconds int32  `json:"limit_seconds"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(payload, &eventPayload); err != nil {
		t.Fatal(err)
	}
	if eventPayload.FailureKind != "max_duration" {
		t.Fatalf("failure kind = %s", eventPayload.FailureKind)
	}
	if eventPayload.Detail.Message != message || eventPayload.Detail.LimitSeconds != 30 {
		t.Fatalf("detail = %+v", eventPayload.Detail)
	}
}

func TestTerminalRunEventPreservesTaskParseFailureKind(t *testing.T) {
	message := "task not found: deploy"
	kind := "task_not_found"
	eventKind, payload, err := terminalRunEventForFields(db.RunStatusFailed, pgtype.Int4{}, pgtype.Text{String: message, Valid: true}, api.WorkerReleaseResult{Kind: "failed", FailureKind: &kind})
	if err != nil {
		t.Fatal(err)
	}
	if eventKind != "run.failed" {
		t.Fatalf("event kind = %s", eventKind)
	}
	assertJSONBytes(t, payload, `{"detail":{"message":"task not found: deploy"},"failure_kind":"task_not_found"}`)
	var eventPayload struct {
		FailureKind string `json:"failure_kind"`
		Detail      struct {
			Message string `json:"message"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(payload, &eventPayload); err != nil {
		t.Fatal(err)
	}
	if eventPayload.FailureKind != "task_not_found" || eventPayload.Detail.Message != message {
		t.Fatalf("payload = %+v", eventPayload)
	}
}

func TestTerminalRunEventJSONShapes(t *testing.T) {
	tests := []struct {
		name        string
		status      db.RunStatus
		exitCode    pgtype.Int4
		message     pgtype.Text
		result      api.WorkerReleaseResult
		wantKind    string
		wantPayload string
	}{
		{
			name:        "completed",
			status:      db.RunStatusSucceeded,
			exitCode:    pgtype.Int4{Int32: 0, Valid: true},
			wantKind:    "run.completed",
			wantPayload: `{"exit_code":0}`,
		},
		{
			name:        "task failed",
			status:      db.RunStatusFailed,
			exitCode:    pgtype.Int4{Int32: 2, Valid: true},
			wantKind:    "run.failed",
			wantPayload: `{"detail":{"exit_code":2},"failure_kind":"task_failed"}`,
		},
		{
			name:        "cancelled",
			status:      db.RunStatusCancelled,
			message:     pgtype.Text{String: "operator cancelled", Valid: true},
			wantKind:    "run.cancelled",
			wantPayload: `{"reason":"operator cancelled"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eventKind, payload, err := terminalRunEventForFields(tt.status, tt.exitCode, tt.message, tt.result)
			if err != nil {
				t.Fatal(err)
			}
			if eventKind != tt.wantKind {
				t.Fatalf("event kind = %s, want %s", eventKind, tt.wantKind)
			}
			assertJSONBytes(t, payload, tt.wantPayload)
		})
	}
}

func TestWorkerEventPayloadJSONShapes(t *testing.T) {
	payload, err := runCreatedEventPayload("deploy", json.RawMessage(`{"env":"prod"}`), 300, []string{"TOKEN", "API_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONBytes(t, payload, `{"max_duration_seconds":300,"payload":{"env":"prod"},"secret_names":["API_KEY","TOKEN"],"task_id":"deploy"}`)

	payload, err = json.Marshal(workerLogChunkPayload{
		RunID:       "run-1",
		Stream:      api.WorkerLogStreamStdout,
		ObservedSeq: 7,
		Bytes:       12,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONBytes(t, payload, `{"bytes":12,"observed_seq":7,"run_id":"run-1","stream":"stdout"}`)

	payload, err = json.Marshal(workerEmitPayload{
		Type:    "deploy.progress",
		Content: json.RawMessage(`{"step":"build"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONBytes(t, payload, `{"content":{"step":"build"},"type":"deploy.progress"}`)

	params := workerInstanceHeartbeatParams(workerActor{WorkerInstanceID: ids.New(), WorkerGroupID: ids.MustFromPG(testWorkerGroupID()), ResourceID: "worker-resource"}, api.WorkerCapabilities{
		ProtocolVersion:           api.CurrentWorkerProtocolVersion,
		SupportedProtocolVersions: api.SupportedWorkerProtocolVersions,
		RuntimeID:                 "sha256:runtime",
		RuntimeArch:               "arm64",
		RuntimeABI:                "helmr/v1",
		KernelDigest:              "sha256:kernel",
		InitramfsDigest:           "sha256:initramfs",
		RootfsDigest:              "sha256:rootfs",
		CNIProfile:                "helmr/v0",
	})
	assertJSONBytes(t, params.Heartbeat, `{"cni_profile":"helmr/v0","initramfs_digest":"sha256:initramfs","kernel_digest":"sha256:kernel","rootfs_digest":"sha256:rootfs","runtime_abi":"helmr/v1","runtime_arch":"arm64","runtime_id":"sha256:runtime"}`)
}

func assertJSONBytes(t *testing.T, got []byte, want string) {
	t.Helper()
	if string(got) != want {
		t.Fatalf("json = %s, want %s", got, want)
	}
}

func TestWorkerRestoreClaimDoesNotRequireWorkspaceSourceBinding(t *testing.T) {
	runID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	store := &fakeStore{
		run: db.Run{
			ID:                 runID,
			OrgID:              ids.ToPG(ids.DefaultOrgID),
			TaskID:             "deploy",
			Status:             db.RunStatusQueued,
			Payload:            []byte(`{}`),
			MaxDurationSeconds: 3600,
			LatestCheckpointID: checkpointID,
			CreatedAt:          testTime(),
			UpdatedAt:          testTime(),
		},
		checkpoint: db.Checkpoint{
			ID:       checkpointID,
			OrgID:    ids.ToPG(ids.DefaultOrgID),
			RunID:    runID,
			Status:   db.CheckpointStatusReady,
			Manifest: []byte(`{}`),
		},
		waitpoint: fakeWaitpoint{
			ID:             waitpointID,
			OrgID:          ids.ToPG(ids.DefaultOrgID),
			RunID:          runID,
			CheckpointID:   checkpointID,
			Kind:           db.WaitpointKindHuman,
			Status:         db.RunWaitStatusResuming,
			ResolutionKind: pgtype.Text{String: "completed", Valid: true},
			Resolution:     []byte(`{"value":{"approved":true}}`),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithWorkerAuth("01234567890123456789012345678901", time.Hour),
	)
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000401")

	req := httptest.NewRequest(http.MethodPost, "/api/worker/executions/lease", bytes.NewReader(testWorkerRunLeaseRequestBody(t)))
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
	if claimResponse.Run.Restore == nil || claimResponse.Run.Restore.CheckpointID != ids.MustFromPG(checkpointID).String() {
		t.Fatalf("restore payload = %+v", claimResponse.Run.Restore)
	}
}

func TestWorkerRunLeaseFailsRunWhenSecretUnavailable(t *testing.T) {
	store := &fakeStore{
		run: db.Run{
			ID:                 ids.ToPG(ids.New()),
			OrgID:              ids.ToPG(ids.DefaultOrgID),
			TaskID:             "deploy",
			Status:             db.RunStatusQueued,
			Payload:            []byte(`{}`),
			MaxDurationSeconds: 3600,
			CreatedAt:          testTime(),
			UpdatedAt:          testTime(),
		},
		currentDeploymentTaskSecretDeclarations: []byte(`[{"name":"API_KEY","env":"API_KEY"}]`),
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithSecrets(fakeSecrets{values: api.ResolvedSecrets{"other": []byte("secret-value")}}),
		WithWorkerAuth("01234567890123456789012345678901", time.Hour),
	)
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000401")

	req := httptest.NewRequest(http.MethodPost, "/api/worker/executions/lease", bytes.NewReader(testWorkerRunLeaseRequestBody(t)))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("claim status = %d body=%s", rec.Code, rec.Body.String())
	}
	assertTerminalPayloadFailure(t, store, "secret_unavailable")
}

func assertTerminalPayloadFailure(t *testing.T, store *fakeStore, failureKind string) {
	t.Helper()
	if store.abandonedClaim {
		t.Fatal("claim should not be abandoned")
	}
	if store.run.Status != db.RunStatusFailed {
		t.Fatalf("run status = %s", store.run.Status)
	}
	if store.run.CurrentExecutionID.Valid {
		t.Fatalf("current execution id = %+v", store.run.CurrentExecutionID)
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

func TestWorkerRoutesRejectUserAPIKey(t *testing.T) {
	store := &fakeStore{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithWorkerAuth("01234567890123456789012345678901", time.Hour),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/worker/executions/lease", bytes.NewReader(testWorkerRunLeaseRequestBody(t)))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerTokenRejectsWrongSecret(t *testing.T) {
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(&fakeStore{}),
		WithAuthenticator(fakeAuth{}),
		WithWorkerAuth(testWorkerTokenSecret, time.Hour),
		WithUserAuth(testWorkerTokenSecret, "http://127.0.0.1:8080"),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/worker/auth/token", bytes.NewBufferString(`{"worker_instance_id":"00000000-0000-0000-0000-000000000401","worker_instance_secret":"wrong"}`))
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
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithWorkerAuth(testWorkerTokenSecret, time.Hour),
		WithUserAuth(string(authSecret), "http://127.0.0.1:8080"),
	)

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
	if _, err := ids.Parse(registered.WorkerInstanceID); err != nil || registered.WorkerInstanceID == "worker-resource-1" || !strings.HasPrefix(registered.WorkerInstanceSecret, auth.WorkerInstanceSecretPrefix) {
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
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithWorkerAuth("01234567890123456789012345678901", time.Hour),
	)
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000402")
	claim := api.WorkerRunLease{
		ID:                ids.New().String(),
		OrgID:             ids.DefaultOrgID.String(),
		RunID:             ids.New().String(),
		WorkerInstanceID:  "00000000-0000-0000-0000-000000000401",
		AttemptNumber:     1,
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
	}
	body, err := json.Marshal(api.WorkerStartRequest{Lease: claim})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/executions/start", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerRunLeaseRejectsMissingAttemptNumber(t *testing.T) {
	store := &fakeStore{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithWorkerAuth("01234567890123456789012345678901", time.Hour),
	)
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000401")
	claim := api.WorkerRunLease{
		ID:                ids.New().String(),
		OrgID:             ids.DefaultOrgID.String(),
		RunID:             ids.New().String(),
		WorkerInstanceID:  "00000000-0000-0000-0000-000000000401",
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
	}
	body, err := json.Marshal(api.WorkerStartRequest{Lease: claim})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/executions/start", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerRunLeaseRejectsMismatchedAttemptNumber(t *testing.T) {
	runID := ids.New()
	executionID := ids.New()
	workerID := ids.New()
	store := &fakeStore{
		run: db.Run{
			ID:                 ids.ToPG(runID),
			OrgID:              ids.ToPG(ids.DefaultOrgID),
			TaskID:             "deploy",
			Status:             db.RunStatusRunning,
			Payload:            []byte(`{}`),
			MaxDurationSeconds: 3600,
			CreatedAt:          testTime(),
			UpdatedAt:          testTime(),
		},
		executionID:               ids.ToPG(executionID),
		executionWorkerInstanceID: ids.ToPG(workerID),
		executionLeaseExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithWorkerAuth(testWorkerTokenSecret, time.Hour),
	)
	workerBearer := mintTestWorkerToken(t, server, workerID.String())
	body, err := json.Marshal(api.WorkerRenewRequest{Lease: api.WorkerRunLease{
		ID:                executionID.String(),
		OrgID:             ids.DefaultOrgID.String(),
		RunID:             runID.String(),
		WorkerInstanceID:  workerID.String(),
		AttemptNumber:     2,
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
		ExpiresAt:         time.Now().Add(time.Minute),
	}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/executions/renew", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerLogsAndEvents(t *testing.T) {
	store := &fakeStore{
		run: db.Run{
			ID:                 ids.ToPG(ids.New()),
			OrgID:              ids.ToPG(ids.DefaultOrgID),
			TaskID:             "deploy",
			Status:             db.RunStatusQueued,
			Payload:            []byte(`{}`),
			MaxDurationSeconds: 3600,
			CreatedAt:          testTime(),
			UpdatedAt:          testTime(),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithWorkerAuth("01234567890123456789012345678901", time.Hour),
	)
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000401")
	req := httptest.NewRequest(http.MethodPost, "/api/worker/executions/lease", bytes.NewReader(testWorkerRunLeaseRequestBody(t)))
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
	logBody, err := json.Marshal(api.WorkerAppendLogRequest{
		Lease:         *claimResponse.Lease,
		Stream:        api.WorkerLogStreamStdout,
		ContentBase64: "aGVsbG8K",
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/worker/executions/logs", bytes.NewReader(logBody))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logs status = %d body=%s", rec.Code, rec.Body.String())
	}
	emitBody, err := json.Marshal(api.WorkerEmitEventRequest{
		Lease:     *claimResponse.Lease,
		EventType: "deploy.progress",
		Content:   json.RawMessage(`{"step":"build"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/worker/executions/events", bytes.NewReader(emitBody))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("event status = %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+ids.MustFromPG(store.run.ID).String()+"/events", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("events status = %d body=%s", rec.Code, rec.Body.String())
	}
	var events api.RunEventPage
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	if len(events.Events) != 2 || events.Events[0].Message != "log.stdout" || events.Events[1].Message != "emit.deploy.progress" {
		t.Fatalf("events = %+v", events)
	}
	if strings.Contains(string(events.Events[0].Attributes), "hello") || strings.Contains(string(events.Events[0].Attributes), "data") {
		t.Fatalf("log event exposed content: %s", events.Events[0].Attributes)
	}
	if events.NextCursor != nil {
		t.Fatalf("next cursor = %v, want nil for final page", *events.NextCursor)
	}
}

func TestRunEventsPaginationUsesLookahead(t *testing.T) {
	runID := ids.New()
	store := &fakeStore{
		run: db.Run{
			ID:        ids.ToPG(runID),
			OrgID:     ids.ToPG(ids.DefaultOrgID),
			TaskID:    "deploy",
			Status:    db.RunStatusQueued,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
	}
	for i := int64(1); i <= 201; i++ {
		store.events = append(store.events, db.RunEvent{
			ID:        i,
			OrgID:     ids.ToPG(ids.DefaultOrgID),
			RunID:     ids.ToPG(runID),
			Kind:      "run.created",
			Payload:   []byte(`{}`),
			CreatedAt: testTime(),
		})
	}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}))

	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String()+"/events?limit=2", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("events status = %d body=%s", rec.Code, rec.Body.String())
	}
	var limited api.RunEventPage
	if err := json.Unmarshal(rec.Body.Bytes(), &limited); err != nil {
		t.Fatal(err)
	}
	if len(limited.Events) != 2 || limited.NextCursor == nil || *limited.NextCursor != 2 {
		t.Fatalf("limited page len=%d next=%v", len(limited.Events), limited.NextCursor)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String()+"/events", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("events status = %d body=%s", rec.Code, rec.Body.String())
	}
	var first api.RunEventPage
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 200 || first.NextCursor == nil || *first.NextCursor != 200 {
		t.Fatalf("first page len=%d next=%v", len(first.Events), first.NextCursor)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String()+"/events?cursor=200", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("events status = %d body=%s", rec.Code, rec.Body.String())
	}
	var second api.RunEventPage
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 1 || second.Events[0].ID != "201" || second.NextCursor != nil {
		t.Fatalf("second page = %+v", second)
	}
}

func TestEventCursorPrefersLastEventID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/runs/run-1/events?cursor=4", nil)
	req.Header.Set("Last-Event-ID", "9")

	cursor, err := eventCursor(req)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 9 {
		t.Fatalf("cursor = %d, want 9", cursor)
	}
}

func TestWorkerWaitpointLifecycle(t *testing.T) {
	store := &fakeStore{
		run: db.Run{
			ID:                 ids.ToPG(ids.New()),
			OrgID:              ids.ToPG(ids.DefaultOrgID),
			TaskID:             "deploy",
			Status:             db.RunStatusQueued,
			Payload:            []byte(`{}`),
			MaxDurationSeconds: 3600,
			CreatedAt:          testTime(),
			UpdatedAt:          testTime(),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithWorkerAuth("01234567890123456789012345678901", time.Hour),
	)
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000401")
	req := httptest.NewRequest(http.MethodPost, "/api/worker/executions/lease", bytes.NewReader(testWorkerRunLeaseRequestBody(t)))
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
	startBody, err := json.Marshal(api.WorkerStartRequest{Lease: *claimResponse.Lease})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/worker/executions/start", bytes.NewReader(startBody))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d body=%s", rec.Code, rec.Body.String())
	}

	timeout := int32(60)
	createBody, err := json.Marshal(api.WorkerCreateWaitpointRequest{
		Lease:          *claimResponse.Lease,
		CorrelationID:  "1",
		Kind:           api.WorkerWaitpointKindHuman,
		Request:        json.RawMessage(`{"message":"ship it"}`),
		DisplayText:    "ship it",
		TimeoutSeconds: &timeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/worker/executions/waitpoints", bytes.NewReader(createBody))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create waitpoint status = %d body=%s", rec.Code, rec.Body.String())
	}
	var created api.WorkerCreateWaitpointResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+claimResponse.Lease.RunID, nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get run status = %d body=%s", rec.Code, rec.Body.String())
	}
	var run api.RunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.PendingWaitpoint != nil {
		t.Fatalf("pending wait before checkpoint ready = %+v", run.PendingWaitpoint)
	}
	if store.run.Status != db.RunStatusRunning {
		t.Fatalf("run status before durable checkpoint = %s", store.run.Status)
	}

	readyBody, err := json.Marshal(api.WorkerCheckpointReadyRequest{
		Lease:        *claimResponse.Lease,
		RunWaitID:    created.RunWaitID,
		WaitpointID:  created.WaitpointID,
		CheckpointID: created.CheckpointID,
		Manifest:     testWorkerCheckpointManifest(claimResponse.Lease.RunID, created.WaitpointID, created.CheckpointID),
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/worker/executions/checkpoints/ready", bytes.NewReader(readyBody))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("checkpoint ready status = %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+claimResponse.Lease.RunID, nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get run after checkpoint ready status = %d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.PendingWaitpoint == nil || run.PendingWaitpoint.Kind != "human" || run.PendingWaitpoint.WaitpointID != created.WaitpointID || run.PendingWaitpoint.DisplayText != "ship it" {
		t.Fatalf("pending wait = %+v", run.PendingWaitpoint)
	}
	if store.run.Status != db.RunStatusWaiting || store.run.CurrentExecutionID.Valid {
		t.Fatalf("run after checkpoint ready = %+v", store.run)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/runs/"+claimResponse.Lease.RunID+"/waitpoints/"+created.WaitpointID+"/not-a-route", bytes.NewBufferString(`{"reason":"wrong route"}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("wrong-kind resolve status = %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/waitpoints/"+created.WaitpointID+"/respond", bytes.NewBufferString(`{"value":{"action":"approve","reason":"ok"}}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("respond status = %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/worker/executions/lease", bytes.NewReader(testWorkerRunLeaseRequestBody(t)))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore claim status = %d body=%s", rec.Code, rec.Body.String())
	}
	var restoreClaim api.WorkerRunLeaseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &restoreClaim); err != nil {
		t.Fatal(err)
	}
	if restoreClaim.Lease == nil || restoreClaim.Lease.ID == claimResponse.Lease.ID || restoreClaim.Run == nil || restoreClaim.Run.ID != claimResponse.Lease.RunID {
		t.Fatalf("restore claim = %+v", restoreClaim)
	}
	if restoreClaim.Run.Restore == nil || restoreClaim.Run.Restore.CheckpointID != created.CheckpointID || restoreClaim.Run.Restore.Waitpoint.RunWaitID != created.RunWaitID || restoreClaim.Run.Restore.Waitpoint.ID != created.WaitpointID || restoreClaim.Run.Restore.Waitpoint.ResumeKind != "completed" {
		t.Fatalf("restore payload = %+v", restoreClaim.Run.Restore)
	}
	restoreResolution := decodeObject(t, restoreClaim.Run.Restore.Waitpoint.ResumePayloadJSON)
	if _, ok := restoreResolution["principal"].(string); !ok {
		t.Fatalf("restore resolution payload = %+v", restoreResolution)
	}
	if _, err := time.Parse(time.RFC3339Nano, stringField(t, restoreResolution, "at")); err != nil {
		t.Fatalf("restore resolution at = %v", err)
	}
	if len(store.events) < 2 || store.events[len(store.events)-2].Kind != "waitpoint.requested" || store.events[len(store.events)-1].Kind != "waitpoint.resolved" {
		t.Fatalf("events = %+v", store.events)
	}
}

func TestResolveWaitpointPayloadsMatchAdapterResumeContract(t *testing.T) {
	tests := []struct {
		name               string
		waitpointKind      db.WaitpointKind
		action             string
		body               string
		wantResolutionKind string
		assertResolution   func(t *testing.T, payload map[string]any)
		assertEvent        func(t *testing.T, payload map[string]any)
	}{{
		name:               "human responded",
		waitpointKind:      db.WaitpointKindHuman,
		action:             "respond",
		body:               `{"value":{"action":"approve","reason":"looks good"}}`,
		wantResolutionKind: "completed",
		assertResolution: func(t *testing.T, payload map[string]any) {
			t.Helper()
			value, ok := payload["value"].(map[string]any)
			if _, principalOK := payload["principal"].(string); !ok || !principalOK || value["action"] != "approve" || value["reason"] != "looks good" {
				t.Fatalf("resolution payload = %+v", payload)
			}
			assertRFC3339NanoField(t, payload, "at")
		},
		assertEvent: func(t *testing.T, payload map[string]any) {
			t.Helper()
			result, ok := payload["result"].(map[string]any)
			if !ok || payload["kind"] != "human" || payload["resolution_kind"] != "completed" || result["action"] != "approve" {
				t.Fatalf("event payload = %+v", payload)
			}
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runID := ids.New()
			waitpointID := ids.New()
			store := &fakeStore{
				run: db.Run{
					ID:        ids.ToPG(runID),
					OrgID:     ids.ToPG(ids.DefaultOrgID),
					TaskID:    "deploy",
					Status:    db.RunStatusWaiting,
					CreatedAt: testTime(),
					UpdatedAt: testTime(),
				},
				waitpoint: fakeWaitpoint{
					ID:          ids.ToPG(waitpointID),
					OrgID:       ids.ToPG(ids.DefaultOrgID),
					RunID:       ids.ToPG(runID),
					Kind:        tt.waitpointKind,
					Status:      db.RunWaitStatusWaiting,
					RequestedAt: testTime(),
				},
			}
			server := New(
				slog.New(slog.NewTextHandler(io.Discard, nil)),
				WithDB(store),
				WithAuthenticator(fakeAuth{}),
			)
			req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/"+waitpointID.String()+"/respond", strings.NewReader(tt.body))
			req.Header.Set("authorization", "Bearer test-key")
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("resolve status = %d body=%s", rec.Code, rec.Body.String())
			}
			if store.waitpoint.ResolutionKind.String != tt.wantResolutionKind {
				t.Fatalf("resolution kind = %q", store.waitpoint.ResolutionKind.String)
			}
			tt.assertResolution(t, decodeObject(t, store.waitpoint.Resolution))
			if store.run.Status != db.RunStatusQueued || store.run.CurrentExecutionID.Valid {
				t.Fatalf("run after resolve = %+v", store.run)
			}
			if len(store.events) != 1 || store.events[0].Kind != "waitpoint.resolved" {
				t.Fatalf("events = %+v", store.events)
			}
			eventPayload := decodeObject(t, store.events[0].Payload)
			if eventPayload["run_id"] != runID.String() || eventPayload["waitpoint_id"] != waitpointID.String() {
				t.Fatalf("event identity = %+v", eventPayload)
			}
			tt.assertEvent(t, eventPayload)
		})
	}
}

func TestRespondWaitpointReplayIsIdempotent(t *testing.T) {
	runID := ids.New()
	waitpointID := ids.New()
	store := &fakeStore{
		run: db.Run{
			ID:        ids.ToPG(runID),
			OrgID:     ids.ToPG(ids.DefaultOrgID),
			TaskID:    "deploy",
			Status:    db.RunStatusWaiting,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
		waitpoint: fakeWaitpoint{
			ID:          ids.ToPG(waitpointID),
			OrgID:       ids.ToPG(ids.DefaultOrgID),
			RunID:       ids.ToPG(runID),
			Kind:        db.WaitpointKindHuman,
			Status:      db.RunWaitStatusWaiting,
			RequestedAt: testTime(),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
	)
	for i, wantStatus := range []int{http.StatusNoContent, http.StatusAccepted} {
		req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/"+waitpointID.String()+"/respond", strings.NewReader(`{"value":{"action":"approve"}}`))
		req.Header.Set("authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != wantStatus {
			t.Fatalf("respond %d status = %d body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	if len(store.waitpointResponses) != 1 {
		t.Fatalf("waitpoint responses = %+v", store.waitpointResponses)
	}
	if len(store.events) != 1 {
		t.Fatalf("events = %+v", store.events)
	}
}

func TestRespondWaitpointRejectsNonRespondableKindInResolvePath(t *testing.T) {
	runID := ids.New()
	waitpointID := ids.New()
	store := &fakeStore{
		run: db.Run{
			ID:        ids.ToPG(runID),
			OrgID:     ids.ToPG(ids.DefaultOrgID),
			TaskID:    "deploy",
			Status:    db.RunStatusWaiting,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
		waitpoint: fakeWaitpoint{
			ID:          ids.ToPG(waitpointID),
			OrgID:       ids.ToPG(ids.DefaultOrgID),
			RunID:       ids.ToPG(runID),
			Kind:        db.WaitpointKindDelay,
			Status:      db.RunWaitStatusWaiting,
			RequestedAt: testTime(),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
	)
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/"+waitpointID.String()+"/respond", strings.NewReader(`{"value":{"action":"approve"}}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("resolve status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.waitpointResponses) != 0 {
		t.Fatalf("waitpoint responses = %+v", store.waitpointResponses)
	}
}

func TestResolveWaitpointReturnsAcceptedWhenRunWaitIsNotResuming(t *testing.T) {
	runID := ids.New()
	waitpointID := ids.New()
	store := &fakeStore{
		run: db.Run{
			ID:        ids.ToPG(runID),
			OrgID:     ids.ToPG(ids.DefaultOrgID),
			TaskID:    "deploy",
			Status:    db.RunStatusWaiting,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
		waitpoint: fakeWaitpoint{
			ID:          ids.ToPG(waitpointID),
			OrgID:       ids.ToPG(ids.DefaultOrgID),
			RunID:       ids.ToPG(runID),
			Kind:        db.WaitpointKindHuman,
			Status:      db.RunWaitStatusWaiting,
			RequestedAt: testTime(),
		},
		resolveStatus: db.RunWaitStatusWaiting,
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
	)
	req := httptest.NewRequest(http.MethodPost, "/api/waitpoints/"+waitpointID.String()+"/respond", strings.NewReader(`{"value":{"action":"approve"},"external_subject":"reviewer@example.test","metadata":{"source":"api"}}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("resolve status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.waitpointResponses) != 1 {
		t.Fatalf("responses = %+v", store.waitpointResponses)
	}
	response := store.waitpointResponses[0]
	if response.ExternalSubject.String != "reviewer@example.test" || string(response.Metadata) != `{"source":"api"}` {
		t.Fatalf("response audit fields = external_subject:%+v metadata:%s", response.ExternalSubject, response.Metadata)
	}
	if store.waitpoint.Status != db.RunWaitStatusWaiting || store.run.Status != db.RunStatusWaiting || len(store.events) != 0 {
		t.Fatalf("waitpoint=%+v run=%+v events=%+v", store.waitpoint, store.run, store.events)
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

type fakeWaitpoint struct {
	ID             pgtype.UUID
	RunWaitID      pgtype.UUID
	OrgID          pgtype.UUID
	ProjectID      pgtype.UUID
	EnvironmentID  pgtype.UUID
	RunID          pgtype.UUID
	ExecutionID    pgtype.UUID
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

func fakeWaitpointRow(waitpoint fakeWaitpoint) db.GetPendingWaitpointForRunRow {
	return db.GetPendingWaitpointForRunRow{
		ID:             waitpoint.ID,
		RunWaitID:      waitpointRunWaitID(waitpoint),
		OrgID:          waitpoint.OrgID,
		RunID:          waitpoint.RunID,
		ExecutionID:    waitpoint.ExecutionID,
		CheckpointID:   waitpoint.CheckpointID,
		CorrelationID:  waitpoint.CorrelationID,
		Kind:           waitpoint.Kind,
		Request:        waitpoint.Request,
		DisplayText:    waitpoint.DisplayText,
		TimeoutSeconds: waitpoint.TimeoutSeconds,
		PolicyName:     waitpoint.PolicyName,
		PolicySnapshot: waitpoint.PolicySnapshot,
		Status:         waitpoint.Status,
		ResolutionKind: waitpoint.ResolutionKind,
		Resolution:     waitpoint.Resolution,
		CreatedAt:      waitpoint.CreatedAt,
		RequestedAt:    waitpoint.RequestedAt,
		ResolvedAt:     waitpoint.ResolvedAt,
	}
}

func waitpointRunWaitID(waitpoint fakeWaitpoint) pgtype.UUID {
	if waitpoint.RunWaitID.Valid {
		return waitpoint.RunWaitID
	}
	return waitpoint.ID
}

type fakeStore struct {
	db.Querier
	createRun                               db.CreateScopedRunParams
	listRuns                                db.ListRunSummariesParams
	countRunsOrgID                          pgtype.UUID
	countScopedRuns                         db.CountScopedRunsByStatusParams
	run                                     db.Run
	deployment                              db.Deployment
	currentDeploymentTaskSecretDeclarations []byte
	currentDeploymentMissing                bool
	currentDeploymentTaskCalls              int
	getDeploymentTaskCalls                  int
	deploymentPromotions                    []db.PromoteDeploymentParams
	createDeploymentResult                  *db.Deployment
	createDeploymentErr                     error
	deploymentTasks                         []db.DeploymentTask
	artifacts                               []db.Artifact
	runEvent                                db.AppendRunEventParams
	events                                  []db.RunEvent
	stdout                                  []byte
	stderr                                  []byte
	runLogSnapshot                          db.GetRunLogSnapshotParams
	logTruncated                            bool
	secret                                  db.GetScopedSecretMetadataByNameRow
	secrets                                 []db.ListScopedSecretsRow
	deleteSecret                            db.DeleteScopedSecretParams
	deleteSecretRows                        int64
	defaultProjectID                        pgtype.UUID
	defaultEnvironmentID                    pgtype.UUID
	stdoutCursor                            int64
	stderrCursor                            int64
	casObjects                              []db.UpsertCasObjectParams
	getCasObjectErr                         error
	executionID                             pgtype.UUID
	executionWorkerInstanceID               pgtype.UUID
	executionLeaseExpiresAt                 pgtype.Timestamptz
	waitpoint                               fakeWaitpoint
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

func (f fakeSecrets) Put(_ context.Context, orgID uuid.UUID, name string, value []byte) (db.Secret, error) {
	return f.PutScoped(context.Background(), orgID, uuid.Nil, uuid.Nil, name, value)
}

func (f fakeSecrets) PutScoped(_ context.Context, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, name string, value []byte) (db.Secret, error) {
	return db.Secret{
		ID:            ids.ToPG(ids.New()),
		OrgID:         ids.ToPG(orgID),
		ProjectID:     ids.ToPG(projectID),
		EnvironmentID: ids.ToPG(environmentID),
		Name:          name,
		Ciphertext:    append([]byte(nil), value...),
		CreatedAt:     testTime(),
		UpdatedAt:     testTime(),
	}, nil
}

func (f fakeSecrets) CheckNames(_ context.Context, _ uuid.UUID, names []string) error {
	return f.CheckScopedNames(context.Background(), uuid.Nil, uuid.Nil, uuid.Nil, names)
}

func (f fakeSecrets) CheckScopedNames(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ uuid.UUID, names []string) error {
	for _, name := range names {
		if len(f.values) == 0 {
			continue
		}
		if _, ok := f.values[name]; !ok {
			return pgx.ErrNoRows
		}
	}
	return nil
}

func (f fakeSecrets) ResolveNames(_ context.Context, _ uuid.UUID, names []string) (api.ResolvedSecrets, error) {
	return f.ResolveScopedNames(context.Background(), uuid.Nil, uuid.Nil, uuid.Nil, names)
}

func (f fakeSecrets) ResolveScopedNames(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ uuid.UUID, names []string) (api.ResolvedSecrets, error) {
	resolved := api.ResolvedSecrets{}
	for _, name := range names {
		value, ok := f.values[name]
		if !ok {
			return nil, pgx.ErrNoRows
		}
		resolved[name] = append([]byte(nil), value...)
	}
	return resolved, nil
}

func (f *fakeStore) GetScopedSecretMetadataByName(_ context.Context, arg db.GetScopedSecretMetadataByNameParams) (db.GetScopedSecretMetadataByNameRow, error) {
	if f.secret.Name == "" || f.secret.Name != arg.Name {
		return db.GetScopedSecretMetadataByNameRow{}, pgx.ErrNoRows
	}
	return f.secret, nil
}

func (f *fakeStore) ListScopedSecrets(_ context.Context, arg db.ListScopedSecretsParams) ([]db.ListScopedSecretsRow, error) {
	if len(f.secrets) > 0 {
		return f.secrets, nil
	}
	if f.secret.Name == "" {
		return nil, nil
	}
	return []db.ListScopedSecretsRow{{
		ID:            f.secret.ID,
		OrgID:         f.secret.OrgID,
		ProjectID:     f.secret.ProjectID,
		EnvironmentID: f.secret.EnvironmentID,
		Name:          f.secret.Name,
		CreatedAt:     f.secret.CreatedAt,
		UpdatedAt:     f.secret.UpdatedAt,
	}}, nil
}

func (f *fakeStore) DeleteScopedSecret(_ context.Context, arg db.DeleteScopedSecretParams) (int64, error) {
	f.deleteSecret = arg
	return f.deleteSecretRows, nil
}

func (f *fakeStore) GetCurrentDeploymentTask(_ context.Context, arg db.GetCurrentDeploymentTaskParams) (db.GetCurrentDeploymentTaskRow, error) {
	f.currentDeploymentTaskCalls++
	if arg.TaskID != "deploy" {
		return db.GetCurrentDeploymentTaskRow{}, pgx.ErrNoRows
	}
	return db.GetCurrentDeploymentTaskRow{
		ID:                     testDeploymentTaskID(),
		OrgID:                  arg.OrgID,
		ProjectID:              arg.ProjectID,
		EnvironmentID:          arg.EnvironmentID,
		DeploymentID:           testDeploymentID(),
		TaskID:                 arg.TaskID,
		FilePath:               "tasks/deploy.ts",
		ExportName:             "deploy",
		SecretDeclarations:     f.currentDeploymentTaskSecretDeclarations,
		MaxDurationSeconds:     300,
		CreatedAt:              testTime(),
		BundleDigest:           "sha256:" + strings.Repeat("b", 64),
		DeploymentSourceDigest: "sha256:" + strings.Repeat("a", 64),
	}, nil
}

func (f *fakeStore) GetCurrentDeployment(_ context.Context, arg db.GetCurrentDeploymentParams) (db.Deployment, error) {
	if f.currentDeploymentMissing {
		return db.Deployment{}, pgx.ErrNoRows
	}
	if f.deployment.ID == (pgtype.UUID{}) {
		if arg.ProjectID != testProjectID() || arg.EnvironmentID != testEnvironmentID() {
			return db.Deployment{}, pgx.ErrNoRows
		}
		return db.Deployment{
			ID:                         testDeploymentID(),
			OrgID:                      arg.OrgID,
			ProjectID:                  arg.ProjectID,
			EnvironmentID:              arg.EnvironmentID,
			Version:                    "20260101.1",
			ContentHash:                "sha256:" + strings.Repeat("a", 64),
			DeploymentSourceArtifactID: testArtifactID(),
			Status:                     db.DeploymentStatusDeployed,
			CreatedAt:                  testTime(),
			DeployedAt:                 testTime(),
		}, nil
	}
	if f.deployment.Status != db.DeploymentStatusDeployed {
		return db.Deployment{}, pgx.ErrNoRows
	}
	if f.deployment.OrgID != arg.OrgID || f.deployment.ProjectID != arg.ProjectID || f.deployment.EnvironmentID != arg.EnvironmentID {
		return db.Deployment{}, pgx.ErrNoRows
	}
	return db.Deployment{
		ID:                           f.deployment.ID,
		OrgID:                        f.deployment.OrgID,
		ProjectID:                    f.deployment.ProjectID,
		EnvironmentID:                f.deployment.EnvironmentID,
		DeploymentSourceArtifactID:   f.deployment.DeploymentSourceArtifactID,
		BuildManifestArtifactID:      f.deployment.BuildManifestArtifactID,
		DeploymentManifestArtifactID: f.deployment.DeploymentManifestArtifactID,
		Status:                       f.deployment.Status,
		Failure:                      f.deployment.Failure,
		CreatedAt:                    f.deployment.CreatedAt,
		BuildingAt:                   f.deployment.BuildingAt,
		BuiltAt:                      f.deployment.BuiltAt,
		DeployedAt:                   f.deployment.DeployedAt,
		FailedAt:                     f.deployment.FailedAt,
	}, nil
}

func (f *fakeStore) ListDeploymentTasks(_ context.Context, arg db.ListDeploymentTasksParams) ([]db.DeploymentTask, error) {
	tasks := make([]db.DeploymentTask, 0, len(f.deploymentTasks))
	for _, task := range f.deploymentTasks {
		if task.OrgID == arg.OrgID && task.ProjectID == arg.ProjectID && task.EnvironmentID == arg.EnvironmentID && task.DeploymentID == arg.DeploymentID {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}

func (f *fakeStore) ListDeclarativeScheduleSummariesForEnvironment(context.Context, db.ListDeclarativeScheduleSummariesForEnvironmentParams) ([]db.ListDeclarativeScheduleSummariesForEnvironmentRow, error) {
	return nil, nil
}

func (f *fakeStore) ScheduleInstanceTriggerIsCurrent(context.Context, db.ScheduleInstanceTriggerIsCurrentParams) (bool, error) {
	return true, nil
}

func (f *fakeStore) GetEnvironment(_ context.Context, arg db.GetEnvironmentParams) (db.Environment, error) {
	if arg.ProjectID != testProjectID() || arg.ID != testEnvironmentID() {
		return db.Environment{}, pgx.ErrNoRows
	}
	return db.Environment{
		ID:        arg.ID,
		OrgID:     arg.OrgID,
		ProjectID: arg.ProjectID,
		Slug:      "prod",
		Name:      "Production",
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (f *fakeStore) GetProject(_ context.Context, arg db.GetProjectParams) (db.Project, error) {
	if arg.ID != testProjectID() {
		return db.Project{}, pgx.ErrNoRows
	}
	return db.Project{
		ID:        arg.ID,
		OrgID:     arg.OrgID,
		Slug:      "main",
		Name:      "Main",
		IsDefault: true,
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (f *fakeStore) GetProjectBySlug(_ context.Context, arg db.GetProjectBySlugParams) (db.Project, error) {
	if arg.Slug != "main" {
		return db.Project{}, pgx.ErrNoRows
	}
	return db.Project{
		ID:        testProjectID(),
		OrgID:     arg.OrgID,
		Slug:      arg.Slug,
		Name:      "Main",
		IsDefault: true,
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (f *fakeStore) GetDefaultEnvironment(_ context.Context, arg db.GetDefaultEnvironmentParams) (db.Environment, error) {
	if arg.ProjectID != testProjectID() {
		return db.Environment{}, pgx.ErrNoRows
	}
	return db.Environment{
		ID:        testEnvironmentID(),
		OrgID:     arg.OrgID,
		ProjectID: arg.ProjectID,
		Slug:      "production",
		Name:      "Production",
		IsDefault: true,
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (f *fakeStore) GetEnvironmentBySlug(_ context.Context, arg db.GetEnvironmentBySlugParams) (db.Environment, error) {
	if arg.ProjectID != testProjectID() || arg.Slug != "production" {
		return db.Environment{}, pgx.ErrNoRows
	}
	return db.Environment{
		ID:        testEnvironmentID(),
		OrgID:     arg.OrgID,
		ProjectID: arg.ProjectID,
		Slug:      arg.Slug,
		Name:      "Production",
		IsDefault: true,
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (f *fakeStore) CreateDeployment(_ context.Context, arg db.CreateDeploymentParams) (db.Deployment, error) {
	if f.createDeploymentErr != nil {
		return db.Deployment{}, f.createDeploymentErr
	}
	if f.createDeploymentResult != nil {
		f.deployment = *f.createDeploymentResult
		if f.deployment.Version == "" {
			f.deployment.Version = arg.Version
		}
		if f.deployment.ApiVersion == "" {
			f.deployment.ApiVersion = arg.ApiVersion
		}
		if f.deployment.BundleFormatVersion == 0 {
			f.deployment.BundleFormatVersion = arg.BundleFormatVersion
		}
		if f.deployment.WorkerProtocolVersion == "" {
			f.deployment.WorkerProtocolVersion = arg.WorkerProtocolVersion
		}
		if !f.deployment.WorkerGroupID.Valid {
			f.deployment.WorkerGroupID = arg.WorkerGroupID
		}
		return f.deployment, nil
	}
	f.deployment = db.Deployment{
		ID:                         arg.ID,
		OrgID:                      arg.OrgID,
		ProjectID:                  arg.ProjectID,
		EnvironmentID:              arg.EnvironmentID,
		Version:                    arg.Version,
		ApiVersion:                 arg.ApiVersion,
		SdkVersion:                 arg.SdkVersion,
		CliVersion:                 arg.CliVersion,
		BundleFormatVersion:        arg.BundleFormatVersion,
		WorkerProtocolVersion:      arg.WorkerProtocolVersion,
		WorkerGroupID:              arg.WorkerGroupID,
		ContentHash:                arg.ContentHash,
		DeploymentSourceArtifactID: arg.DeploymentSourceArtifactID,
		Status:                     arg.Status,
		CreatedAt:                  testTime(),
		DeployedAt:                 testTime(),
	}
	return f.deployment, nil
}

func (f *fakeStore) CreateArtifact(_ context.Context, arg db.CreateArtifactParams) (db.Artifact, error) {
	artifact := db.Artifact{
		ID:                        arg.ID,
		OrgID:                     arg.OrgID,
		ProjectID:                 arg.ProjectID,
		EnvironmentID:             arg.EnvironmentID,
		Digest:                    arg.Digest,
		Kind:                      arg.Kind,
		SizeBytes:                 arg.SizeBytes,
		MediaType:                 arg.MediaType,
		CreatedByWorkerInstanceID: arg.CreatedByWorkerInstanceID,
		CreatedAt:                 testTime(),
	}
	f.artifacts = append(f.artifacts, artifact)
	return artifact, nil
}

func (f *fakeStore) GetArtifact(_ context.Context, arg db.GetArtifactParams) (db.Artifact, error) {
	for _, artifact := range f.artifacts {
		if artifact.OrgID == arg.OrgID && artifact.ProjectID == arg.ProjectID && artifact.EnvironmentID == arg.EnvironmentID && artifact.ID == arg.ID {
			return artifact, nil
		}
	}
	if arg.ID == testArtifactID() {
		return db.Artifact{
			ID:            arg.ID,
			OrgID:         arg.OrgID,
			ProjectID:     arg.ProjectID,
			EnvironmentID: arg.EnvironmentID,
			Digest:        "sha256:" + strings.Repeat("a", 64),
			Kind:          db.ArtifactKindDeploymentSource,
			SizeBytes:     12,
			MediaType:     api.DeploymentSourceArtifactMediaType,
			CreatedAt:     testTime(),
		}, nil
	}
	return db.Artifact{}, pgx.ErrNoRows
}

func (f *fakeStore) ListArtifactsByIDs(ctx context.Context, arg db.ListArtifactsByIDsParams) ([]db.Artifact, error) {
	artifacts := make([]db.Artifact, 0, len(arg.Ids))
	for _, artifactID := range arg.Ids {
		artifact, err := f.GetArtifact(ctx, db.GetArtifactParams{
			OrgID:         arg.OrgID,
			ProjectID:     arg.ProjectID,
			EnvironmentID: arg.EnvironmentID,
			ID:            artifactID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, nil
}

func (f *fakeStore) LockDeploymentReusableBuildKey(_ context.Context, _ db.LockDeploymentReusableBuildKeyParams) error {
	return nil
}

func (f *fakeStore) AllocateDeploymentVersion(_ context.Context, _ db.AllocateDeploymentVersionParams) (string, error) {
	return "20260101.1", nil
}

func (f *fakeStore) GetDefaultWorkerGroup(context.Context) (db.WorkerGroup, error) {
	return db.WorkerGroup{
		ID:          testWorkerGroupID(),
		Name:        "default",
		Description: "Default worker group",
		CreatedAt:   testTime(),
		UpdatedAt:   testTime(),
	}, nil
}

func (f *fakeStore) GetReusableDeploymentByContentHash(_ context.Context, arg db.GetReusableDeploymentByContentHashParams) (db.Deployment, error) {
	if f.deployment.OrgID == arg.OrgID && f.deployment.ProjectID == arg.ProjectID && f.deployment.EnvironmentID == arg.EnvironmentID && f.deployment.ContentHash == arg.ContentHash && f.deployment.WorkerGroupID == arg.WorkerGroupID {
		return f.deployment, nil
	}
	return db.Deployment{}, pgx.ErrNoRows
}

func (f *fakeStore) PromoteDeployment(_ context.Context, arg db.PromoteDeploymentParams) (db.PromoteDeploymentRow, error) {
	f.deploymentPromotions = append(f.deploymentPromotions, arg)
	return db.PromoteDeploymentRow{
		ID:                  arg.ID,
		OrgID:               arg.OrgID,
		ProjectID:           arg.ProjectID,
		EnvironmentID:       arg.EnvironmentID,
		DeploymentID:        arg.DeploymentID,
		PromotedByPrincipal: arg.PromotedByPrincipal,
		Reason:              arg.Reason,
		CreatedAt:           testTime(),
	}, nil
}

func (f *fakeStore) GetDeploymentByVersion(_ context.Context, arg db.GetDeploymentByVersionParams) (db.Deployment, error) {
	if f.deployment.Version == arg.Version && f.deployment.OrgID == arg.OrgID && f.deployment.ProjectID == arg.ProjectID && f.deployment.EnvironmentID == arg.EnvironmentID {
		return f.deployment, nil
	}
	return db.Deployment{}, pgx.ErrNoRows
}

func (f *fakeStore) GetDeploymentForOrg(_ context.Context, arg db.GetDeploymentForOrgParams) (db.Deployment, error) {
	if f.deployment.ID == arg.ID && f.deployment.OrgID == arg.OrgID {
		return f.deployment, nil
	}
	return db.Deployment{}, pgx.ErrNoRows
}

func (f *fakeStore) ListDeploymentsByVersionForOrg(_ context.Context, arg db.ListDeploymentsByVersionForOrgParams) ([]db.Deployment, error) {
	if f.deployment.Version == arg.Version && f.deployment.OrgID == arg.OrgID {
		return []db.Deployment{f.deployment}, nil
	}
	return nil, nil
}

func (f *fakeStore) GetDeploymentTask(_ context.Context, arg db.GetDeploymentTaskParams) (db.GetDeploymentTaskRow, error) {
	f.getDeploymentTaskCalls++
	if len(f.deploymentTasks) == 0 && arg.TaskID == "deploy" && arg.DeploymentID == testDeploymentID() {
		return db.GetDeploymentTaskRow{
			ID:                     testDeploymentTaskID(),
			OrgID:                  arg.OrgID,
			ProjectID:              arg.ProjectID,
			EnvironmentID:          arg.EnvironmentID,
			DeploymentID:           arg.DeploymentID,
			TaskID:                 arg.TaskID,
			FilePath:               "tasks/deploy.ts",
			ExportName:             "deploy",
			MaxDurationSeconds:     300,
			CreatedAt:              testTime(),
			DeploymentSourceDigest: "sha256:" + strings.Repeat("a", 64),
		}, nil
	}
	for _, task := range f.deploymentTasks {
		if task.OrgID == arg.OrgID && task.ProjectID == arg.ProjectID && task.EnvironmentID == arg.EnvironmentID && task.DeploymentID == arg.DeploymentID && task.TaskID == arg.TaskID {
			return db.GetDeploymentTaskRow{
				ID:                     task.ID,
				OrgID:                  task.OrgID,
				ProjectID:              task.ProjectID,
				EnvironmentID:          task.EnvironmentID,
				DeploymentID:           task.DeploymentID,
				TaskID:                 task.TaskID,
				FilePath:               task.FilePath,
				ExportName:             task.ExportName,
				HandlerEntrypoint:      task.HandlerEntrypoint,
				BundleArtifactID:       task.BundleArtifactID,
				BundleDigest:           "sha256:" + strings.Repeat("b", 64),
				RequestedMilliCpu:      task.RequestedMilliCpu,
				RequestedMemoryMib:     task.RequestedMemoryMib,
				SecretDeclarations:     task.SecretDeclarations,
				ResourceRequirements:   task.ResourceRequirements,
				QueueName:              task.QueueName,
				QueueConcurrencyLimit:  task.QueueConcurrencyLimit,
				Ttl:                    task.Ttl,
				MaxDurationSeconds:     task.MaxDurationSeconds,
				CreatedAt:              task.CreatedAt,
				DeploymentSourceDigest: "sha256:" + strings.Repeat("a", 64),
			}, nil
		}
	}
	return db.GetDeploymentTaskRow{}, pgx.ErrNoRows
}

func (f *fakeStore) GetDeploymentQueueConfig(_ context.Context, arg db.GetDeploymentQueueConfigParams) (db.GetDeploymentQueueConfigRow, error) {
	for _, task := range f.deploymentTasks {
		if task.OrgID == arg.OrgID && task.ProjectID == arg.ProjectID && task.EnvironmentID == arg.EnvironmentID && task.DeploymentID == arg.DeploymentID && task.QueueName == arg.QueueName {
			return db.GetDeploymentQueueConfigRow{
				QueueName:             task.QueueName,
				QueueConcurrencyLimit: task.QueueConcurrencyLimit,
			}, nil
		}
	}
	return db.GetDeploymentQueueConfigRow{}, pgx.ErrNoRows
}

func (f *fakeStore) CreateDeploymentTask(_ context.Context, arg db.CreateDeploymentTaskParams) (db.DeploymentTask, error) {
	task := db.DeploymentTask{
		ID:                    arg.ID,
		OrgID:                 arg.OrgID,
		ProjectID:             arg.ProjectID,
		EnvironmentID:         arg.EnvironmentID,
		DeploymentID:          arg.DeploymentID,
		TaskID:                arg.TaskID,
		FilePath:              arg.FilePath,
		ExportName:            arg.ExportName,
		HandlerEntrypoint:     arg.HandlerEntrypoint,
		BundleArtifactID:      arg.BundleArtifactID,
		BundleFormatVersion:   arg.BundleFormatVersion,
		RequestedMilliCpu:     arg.RequestedMilliCpu,
		RequestedMemoryMib:    arg.RequestedMemoryMib,
		SecretDeclarations:    arg.SecretDeclarations,
		ResourceRequirements:  arg.ResourceRequirements,
		QueueName:             arg.QueueName,
		QueueConcurrencyLimit: arg.QueueConcurrencyLimit,
		Ttl:                   arg.Ttl,
		MaxDurationSeconds:    arg.MaxDurationSeconds,
		CreatedAt:             testTime(),
	}
	f.deploymentTasks = append(f.deploymentTasks, task)
	return task, nil
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
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	f.runEvent = db.AppendRunEventParams{
		OrgID:   arg.OrgID,
		RunID:   arg.ID,
		Kind:    "run.created",
		Payload: arg.EventPayload,
	}
	f.events = append(f.events, db.RunEvent{
		ID:        int64(len(f.events) + 1),
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

func (f *fakeStore) AppendRunEvent(_ context.Context, arg db.AppendRunEventParams) (db.RunEvent, error) {
	f.runEvent = arg
	event := db.RunEvent{
		ID:        int64(len(f.events) + 1),
		OrgID:     arg.OrgID,
		RunID:     arg.RunID,
		Kind:      arg.Kind,
		Payload:   arg.Payload,
		CreatedAt: testTime(),
	}
	f.events = append(f.events, event)
	return event, nil
}

func (f *fakeStore) GetRun(_ context.Context, arg db.GetRunParams) (db.Run, error) {
	if f.run.ID != arg.ID {
		return db.Run{}, pgx.ErrNoRows
	}
	return f.run, nil
}

func fakeRunDeploymentID(run db.Run) pgtype.UUID {
	if run.DeploymentID.Valid {
		return run.DeploymentID
	}
	return testDeploymentID()
}

func fakeRunDeploymentTaskID(run db.Run) pgtype.UUID {
	if run.DeploymentTaskID.Valid {
		return run.DeploymentTaskID
	}
	return testDeploymentTaskID()
}

func (f *fakeStore) GetRunSummary(_ context.Context, arg db.GetRunSummaryParams) (db.GetRunSummaryRow, error) {
	if f.run.ID != arg.ID {
		return db.GetRunSummaryRow{}, pgx.ErrNoRows
	}
	return db.GetRunSummaryRow{
		ID:               f.run.ID,
		OrgID:            f.run.OrgID,
		ProjectID:        f.run.ProjectID,
		EnvironmentID:    f.run.EnvironmentID,
		DeploymentID:     fakeRunDeploymentID(f.run),
		DeploymentTaskID: fakeRunDeploymentTaskID(f.run),
		TaskID:           f.run.TaskID,
		Status:           f.run.Status,
		ExitCode:         f.run.ExitCode,
		Output:           f.run.Output,
		CreatedAt:        f.run.CreatedAt,
		UpdatedAt:        f.run.UpdatedAt,
	}, nil
}

func (f *fakeStore) ListRunSummaries(_ context.Context, arg db.ListRunSummariesParams) ([]db.ListRunSummariesRow, error) {
	f.listRuns = arg
	if !f.run.ID.Valid {
		return nil, nil
	}
	return []db.ListRunSummariesRow{{
		ID:               f.run.ID,
		OrgID:            f.run.OrgID,
		ProjectID:        f.run.ProjectID,
		EnvironmentID:    f.run.EnvironmentID,
		DeploymentID:     fakeRunDeploymentID(f.run),
		DeploymentTaskID: fakeRunDeploymentTaskID(f.run),
		TaskID:           f.run.TaskID,
		Status:           f.run.Status,
		ExitCode:         f.run.ExitCode,
		Output:           f.run.Output,
		CreatedAt:        f.run.CreatedAt,
		UpdatedAt:        f.run.UpdatedAt,
	}}, nil
}

func (f *fakeStore) ListScopedRunSummaries(_ context.Context, arg db.ListScopedRunSummariesParams) ([]db.ListScopedRunSummariesRow, error) {
	f.listRuns = db.ListRunSummariesParams{
		OrgID:        arg.OrgID,
		StatusFilter: arg.StatusFilter,
		RowLimit:     arg.RowLimit,
	}
	if !f.run.ID.Valid || f.run.ProjectID != arg.ProjectID || f.run.EnvironmentID != arg.EnvironmentID {
		return nil, nil
	}
	return []db.ListScopedRunSummariesRow{{
		ID:               f.run.ID,
		OrgID:            f.run.OrgID,
		ProjectID:        f.run.ProjectID,
		EnvironmentID:    f.run.EnvironmentID,
		DeploymentID:     fakeRunDeploymentID(f.run),
		DeploymentTaskID: fakeRunDeploymentTaskID(f.run),
		TaskID:           f.run.TaskID,
		Status:           f.run.Status,
		ExitCode:         f.run.ExitCode,
		Output:           f.run.Output,
		CreatedAt:        f.run.CreatedAt,
		UpdatedAt:        f.run.UpdatedAt,
	}}, nil
}

func (f *fakeStore) CountRunsByStatus(_ context.Context, orgID pgtype.UUID) (db.CountRunsByStatusRow, error) {
	f.countRunsOrgID = orgID
	var counts db.CountRunsByStatusRow
	if !f.run.ID.Valid {
		return counts, nil
	}
	addRunStatusCount(&counts, f.run.Status)
	return counts, nil
}

func (f *fakeStore) CountScopedRunsByStatus(_ context.Context, arg db.CountScopedRunsByStatusParams) (db.CountScopedRunsByStatusRow, error) {
	f.countScopedRuns = arg
	var counts db.CountScopedRunsByStatusRow
	if !f.run.ID.Valid || f.run.ProjectID != arg.ProjectID || f.run.EnvironmentID != arg.EnvironmentID {
		return counts, nil
	}
	switch f.run.Status {
	case db.RunStatusQueued:
		counts.Queued++
	case db.RunStatusRunning:
		counts.Running++
	case db.RunStatusWaiting:
		counts.Waiting++
	case db.RunStatusSucceeded:
		counts.Succeeded++
	case db.RunStatusFailed:
		counts.Failed++
	case db.RunStatusCancelled:
		counts.Cancelled++
	}
	return counts, nil
}

func addRunStatusCount(counts *db.CountRunsByStatusRow, status db.RunStatus) {
	switch status {
	case db.RunStatusQueued:
		counts.Queued++
	case db.RunStatusRunning:
		counts.Running++
	case db.RunStatusWaiting:
		counts.Waiting++
	case db.RunStatusSucceeded:
		counts.Succeeded++
	case db.RunStatusFailed:
		counts.Failed++
	case db.RunStatusCancelled:
		counts.Cancelled++
	}
}

func (f *fakeStore) ListRunEvents(_ context.Context, arg db.ListRunEventsParams) ([]db.RunEvent, error) {
	var events []db.RunEvent
	for _, event := range f.events {
		if event.RunID == arg.RunID && event.ID > arg.ID {
			events = append(events, event)
		}
	}
	if int32(len(events)) > arg.Limit {
		events = events[:arg.Limit]
	}
	return events, nil
}

func (f *fakeStore) ListQueueScopes(_ context.Context, arg db.ListQueueScopesParams) ([]db.ListQueueScopesRow, error) {
	f.listQueueScopes = arg
	return []db.ListQueueScopesRow{{
		OrgID:     ids.ToPG(ids.DefaultOrgID),
		QueueName: "queue-a",
	}}, nil
}

func (f *fakeStore) UpsertWorkerInstanceHeartbeat(_ context.Context, arg db.UpsertWorkerInstanceHeartbeatParams) (db.UpsertWorkerInstanceHeartbeatRow, error) {
	return db.UpsertWorkerInstanceHeartbeatRow{
		ID:                        arg.ID,
		ResourceID:                arg.ResourceID,
		Status:                    db.WorkerInstanceStatusActive,
		WorkerVersion:             arg.WorkerVersion,
		ProtocolVersion:           arg.ProtocolVersion,
		SupportedProtocolVersions: arg.SupportedProtocolVersions,
		Region:                    arg.Region,
		TotalMilliCpu:             arg.TotalMilliCpu,
		TotalMemoryMib:            arg.TotalMemoryMib,
		TotalDiskMib:              arg.TotalDiskMib,
		TotalExecutionSlots:       arg.TotalExecutionSlots,
		AvailableMilliCpu:         arg.AvailableMilliCpu,
		AvailableMemoryMib:        arg.AvailableMemoryMib,
		AvailableDiskMib:          arg.AvailableDiskMib,
		AvailableExecutionSlots:   arg.AvailableExecutionSlots,
		Labels:                    arg.Labels,
		Heartbeat:                 arg.Heartbeat,
		RuntimeID:                 arg.RuntimeID,
		RuntimeArch:               arg.RuntimeArch,
		RuntimeABI:                arg.RuntimeABI,
		KernelDigest:              arg.KernelDigest,
		InitramfsDigest:           arg.InitramfsDigest,
		RootfsDigest:              arg.RootfsDigest,
		CniProfile:                arg.CniProfile,
		FirstSeenAt:               testTime(),
		LastSeenAt:                testTime(),
	}, nil
}

func (f *fakeStore) EnsureRuntimeReleaseSelection(context.Context, string) error {
	return nil
}

func (f *fakeStore) GetWorkerInstanceState(_ context.Context, id pgtype.UUID) (db.GetWorkerInstanceStateRow, error) {
	return db.GetWorkerInstanceStateRow{
		ID:               id,
		ResourceID:       ids.MustFromPG(id).String(),
		Status:           db.WorkerInstanceStatusActive,
		ActiveExecutions: 0,
	}, nil
}

func (f *fakeStore) GetWorkerInstanceQueueCapacity(context.Context, pgtype.UUID) (db.GetWorkerInstanceQueueCapacityRow, error) {
	return db.GetWorkerInstanceQueueCapacityRow{
		AvailableMilliCpu:       2000,
		AvailableMemoryMib:      2048,
		AvailableDiskMib:        20480,
		AvailableExecutionSlots: 1,
	}, nil
}

func (f *fakeStore) SetWorkerInstanceStatus(_ context.Context, arg db.SetWorkerInstanceStatusParams) (db.WorkerInstance, error) {
	return db.WorkerInstance{
		ID:         arg.ID,
		ResourceID: ids.MustFromPG(arg.ID).String(),
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
			OrgID:     ids.DefaultOrgID.String(),
			RunID:     ids.MustFromPG(f.run.ID).String(),
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

func (f *fakeStore) GetRunExecutionQueueLease(_ context.Context, arg db.GetRunExecutionQueueLeaseParams) (db.GetRunExecutionQueueLeaseRow, error) {
	if f.activeQueueLeaseMissing {
		return db.GetRunExecutionQueueLeaseRow{}, pgx.ErrNoRows
	}
	if f.run.ID != arg.RunID || f.executionID != arg.ExecutionID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.GetRunExecutionQueueLeaseRow{}, pgx.ErrNoRows
	}
	return db.GetRunExecutionQueueLeaseRow{
		ID:                f.executionID,
		RunID:             f.run.ID,
		WorkerInstanceID:  f.executionWorkerInstanceID,
		DispatchMessageID: "message-1",
		DispatchLeaseID:   "lease-1",
		DispatchAttempt:   1,
		AttemptNumber:     1,
		LeaseExpiresAt:    f.executionLeaseExpiresAt,
		QueueName:         "queue-a",
	}, nil
}

func (f *fakeStore) GetRunExecutionRuntimeRelease(_ context.Context, arg db.GetRunExecutionRuntimeReleaseParams) (db.GetRunExecutionRuntimeReleaseRow, error) {
	if f.activeQueueLeaseMissing {
		return db.GetRunExecutionRuntimeReleaseRow{}, pgx.ErrNoRows
	}
	if f.run.ID != arg.RunID || f.executionID != arg.ExecutionID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.GetRunExecutionRuntimeReleaseRow{}, pgx.ErrNoRows
	}
	capabilities := testWorkerCapabilities()
	return db.GetRunExecutionRuntimeReleaseRow{
		WorkerRuntimeID: capabilities.RuntimeID,
		RuntimeArch:     capabilities.RuntimeArch,
		RuntimeABI:      capabilities.RuntimeABI,
		KernelDigest:    capabilities.KernelDigest,
		InitramfsDigest: capabilities.InitramfsDigest,
		RootfsDigest:    capabilities.RootfsDigest,
		CniProfile:      capabilities.CNIProfile,
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

func (f *fakeStore) RunExecutionDispatchAttemptsExhausted(context.Context, db.RunExecutionDispatchAttemptsExhaustedParams) (bool, error) {
	return false, nil
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
	credentialID, _ := ids.Parse(testWorkerInstanceCredentialID)
	allowed := ids.ToPG(credentialID)
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
		ResourceID:       ids.MustFromPG(arg.WorkerInstanceID).String(),
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

func (f *fakeStore) LeaseRunExecution(_ context.Context, arg db.LeaseRunExecutionParams) (db.LeaseRunExecutionRow, error) {
	if f.run.Status != db.RunStatusQueued {
		return db.LeaseRunExecutionRow{}, pgx.ErrNoRows
	}
	f.executionID = arg.ExecutionID
	f.executionWorkerInstanceID = arg.WorkerInstanceID
	f.executionLeaseExpiresAt = arg.LeaseExpiresAt
	f.run.Status = db.RunStatusRunning
	f.run.CurrentAttemptNumber = pgtype.Int4{Int32: 1, Valid: true}
	f.run.CurrentExecutionID = f.executionID
	restoreCheckpointID := pgtype.UUID{}
	if f.run.LatestCheckpointID.Valid && f.run.LatestCheckpointID == f.checkpoint.ID && f.checkpoint.Status == db.CheckpointStatusReady && f.waitpoint.Status == db.RunWaitStatusResuming {
		f.checkpoint.Status = db.CheckpointStatusRestoring
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
	return db.LeaseRunExecutionRow{
		ID:                               f.run.ID,
		OrgID:                            f.run.OrgID,
		ProjectID:                        projectID,
		EnvironmentID:                    environmentID,
		TaskID:                           f.run.TaskID,
		Status:                           f.run.Status,
		Payload:                          f.run.Payload,
		DeploymentTaskID:                 testDeploymentTaskID(),
		DeploymentTaskFilePath:           "src/task.ts",
		DeploymentTaskExportName:         "deploy",
		DeploymentTaskSecretDeclarations: f.currentDeploymentTaskSecretDeclarations,
		DeploymentWorkerProtocolVersion:  api.CurrentWorkerProtocolVersion,
		DeploymentSourceDigest:           "sha256:" + strings.Repeat("a", 64),
		MaxDurationSeconds:               f.run.MaxDurationSeconds,
		ExitCode:                         f.run.ExitCode,
		ErrorMessage:                     f.run.ErrorMessage,
		CreatedAt:                        f.run.CreatedAt,
		UpdatedAt:                        f.run.UpdatedAt,
		StartedAt:                        f.run.StartedAt,
		FinishedAt:                       f.run.FinishedAt,
		RequestedMilliCpu:                requirements.Resources.MilliCPU,
		RequestedMemoryMib:               requirements.Resources.MemoryMiB,
		RequestedDiskMib:                 requirements.Resources.DiskMiB,
		RequestedExecutionSlots:          requirements.Resources.Slots,
		RequirementsRuntimeID:            requirements.Runtime.ID,
		RequirementsRuntimeArch:          requirements.Runtime.Arch,
		RequirementsRuntimeAbi:           requirements.Runtime.ABI,
		RequirementsKernelDigest:         requirements.Runtime.KernelDigest,
		RequirementsInitramfsDigest:      requirements.Runtime.InitramfsDigest,
		RequirementsRootfsDigest:         requirements.Runtime.RootfsDigest,
		RequirementsCniProfile:           requirements.Runtime.CNIProfile,
		RequirementsNetworkPolicy:        networkPolicy,
		ExecutionID:                      f.executionID,
		ExecutionWorkerInstanceID:        f.executionWorkerInstanceID,
		ExecutionDispatchMessageID:       arg.DispatchMessageID.String,
		ExecutionDispatchLeaseID:         arg.DispatchLeaseID,
		ExecutionDispatchAttempt:         arg.DispatchAttempt,
		ExecutionAttemptNumber:           1,
		ExecutionLeaseExpiresAt:          f.executionLeaseExpiresAt,
		ExecutionWorkerProtocolVersion:   api.CurrentWorkerProtocolVersion,
		ExecutionRestoreCheckpointID:     restoreCheckpointID,
	}, nil
}

func (f *fakeStore) RequeueExpiredLeasedRunExecutions(context.Context, pgtype.UUID) error {
	return nil
}

func (f *fakeStore) AbandonLeasedRunExecution(_ context.Context, arg db.AbandonLeasedRunExecutionParams) error {
	if f.run.ID != arg.RunID || f.executionID != arg.ExecutionID || f.executionWorkerInstanceID != arg.WorkerInstanceID || f.run.Status != db.RunStatusRunning {
		return nil
	}
	f.abandonedClaim = true
	f.run.Status = db.RunStatusQueued
	f.run.CurrentExecutionID = pgtype.UUID{}
	if f.checkpoint.Status == db.CheckpointStatusRestoring && f.run.LatestCheckpointID == f.checkpoint.ID {
		f.checkpoint.Status = db.CheckpointStatusReady
	}
	return nil
}

func (f *fakeStore) FailExpiredRunningRunExecutions(context.Context, pgtype.UUID) error {
	return nil
}

func (f *fakeStore) ExpireQueuedRuns(context.Context, pgtype.UUID) error {
	return nil
}

func (f *fakeStore) StartRunExecution(_ context.Context, arg db.StartRunExecutionParams) (db.RunStatus, error) {
	if f.run.Status != db.RunStatusRunning || f.executionID != arg.ExecutionID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return "", pgx.ErrNoRows
	}
	f.run.Status = db.RunStatusRunning
	f.run.StartedAt = testTime()
	f.run.UpdatedAt = testTime()
	return f.run.Status, nil
}

func (f *fakeStore) AcknowledgeRestore(_ context.Context, arg db.AcknowledgeRestoreParams) (db.AcknowledgeRestoreRow, error) {
	if f.run.ID != arg.RunID || f.executionID != arg.ExecutionID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.AcknowledgeRestoreRow{}, pgx.ErrNoRows
	}
	if f.checkpoint.ID != arg.CheckpointID || f.waitpoint.ID != arg.WaitpointID {
		return db.AcknowledgeRestoreRow{}, pgx.ErrNoRows
	}
	if waitpointRunWaitID(f.waitpoint) != arg.RunWaitID {
		return db.AcknowledgeRestoreRow{}, pgx.ErrNoRows
	}
	if f.checkpoint.Status == db.CheckpointStatusRestoring && f.waitpoint.Status == db.RunWaitStatusResuming {
		f.checkpoint.Status = db.CheckpointStatusReady
		f.waitpoint.Status = db.RunWaitStatusRestored
	}
	if f.checkpoint.Status != db.CheckpointStatusReady || f.waitpoint.Status != db.RunWaitStatusRestored {
		return db.AcknowledgeRestoreRow{}, pgx.ErrNoRows
	}
	return db.AcknowledgeRestoreRow{
		ID:           f.waitpoint.ID,
		RunWaitID:    waitpointRunWaitID(f.waitpoint),
		OrgID:        f.waitpoint.OrgID,
		RunID:        f.waitpoint.RunID,
		ExecutionID:  f.waitpoint.ExecutionID,
		CheckpointID: f.waitpoint.CheckpointID,
		Status:       f.waitpoint.Status,
	}, nil
}

func (f *fakeStore) RenewRunExecutionLease(_ context.Context, arg db.RenewRunExecutionLeaseParams) (db.RenewRunExecutionLeaseRow, error) {
	if f.executionID != arg.ExecutionID || f.executionWorkerInstanceID != arg.WorkerInstanceID || arg.DispatchMessageID != "message-1" || arg.DispatchLeaseID != "lease-1" {
		return db.RenewRunExecutionLeaseRow{}, pgx.ErrNoRows
	}
	f.executionLeaseExpiresAt = arg.LeaseExpiresAt
	return db.RenewRunExecutionLeaseRow{
		ID:                f.executionID,
		WorkerInstanceID:  f.executionWorkerInstanceID,
		DispatchMessageID: arg.DispatchMessageID,
		DispatchLeaseID:   arg.DispatchLeaseID,
		DispatchAttempt:   1,
		LeaseExpiresAt:    f.executionLeaseExpiresAt,
	}, nil
}

func (f *fakeStore) ReleaseRunExecution(_ context.Context, arg db.ReleaseRunExecutionParams) (db.ReleaseRunExecutionRow, error) {
	if f.executionID != arg.ExecutionID || f.executionWorkerInstanceID != arg.WorkerInstanceID || arg.DispatchMessageID != "message-1" || arg.DispatchLeaseID != "lease-1" {
		return db.ReleaseRunExecutionRow{}, pgx.ErrNoRows
	}
	releaseRow := func() db.ReleaseRunExecutionRow {
		return db.ReleaseRunExecutionRow{
			ID:                 f.run.ID,
			OrgID:              f.run.OrgID,
			TaskID:             f.run.TaskID,
			Status:             f.run.Status,
			Payload:            f.run.Payload,
			Output:             f.run.Output,
			MaxDurationSeconds: f.run.MaxDurationSeconds,
			ExitCode:           f.run.ExitCode,
			ErrorMessage:       f.run.ErrorMessage,
			CreatedAt:          f.run.CreatedAt,
			UpdatedAt:          f.run.UpdatedAt,
			StartedAt:          f.run.StartedAt,
			FinishedAt:         f.run.FinishedAt,
		}
	}
	if f.run.Status == arg.Status && !f.run.CurrentExecutionID.Valid && f.run.ExitCode == arg.ExitCode && f.run.ErrorMessage == arg.ErrorMessage && bytes.Equal(f.run.Output, arg.Output) {
		return releaseRow(), nil
	}
	if f.run.Status != db.RunStatusRunning || f.run.CurrentExecutionID != arg.ExecutionID {
		return db.ReleaseRunExecutionRow{}, pgx.ErrNoRows
	}
	f.run.Status = arg.Status
	f.run.CurrentExecutionID = pgtype.UUID{}
	f.run.ExitCode = arg.ExitCode
	f.run.Output = arg.Output
	f.run.ErrorMessage = arg.ErrorMessage
	f.run.FinishedAt = testTime()
	f.run.UpdatedAt = testTime()
	f.events = append(f.events, db.RunEvent{
		ID:        int64(len(f.events) + 1),
		OrgID:     arg.OrgID,
		RunID:     arg.RunID,
		Kind:      arg.TerminalEventKind,
		Payload:   arg.TerminalEventPayload,
		CreatedAt: testTime(),
	})
	if f.checkpoint.Status == db.CheckpointStatusRestoring && f.run.LatestCheckpointID == f.checkpoint.ID {
		if arg.ErrorMessage.Valid {
			f.checkpoint.Status = db.CheckpointStatusInvalid
			f.checkpoint.ErrorMessage = arg.ErrorMessage
			f.checkpoint.InvalidatedAt = testTime()
		} else {
			f.checkpoint.Status = db.CheckpointStatusReady
			f.checkpoint.ErrorMessage = pgtype.Text{}
			f.checkpoint.InvalidatedAt = pgtype.Timestamptz{}
		}
	}
	return releaseRow(), nil
}

func (f *fakeStore) AppendRunLogChunk(_ context.Context, arg db.AppendRunLogChunkParams) (db.AppendRunLogChunkRow, error) {
	if f.executionID != arg.ExecutionID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.AppendRunLogChunkRow{}, pgx.ErrNoRows
	}
	switch arg.Stream {
	case "stdout":
		f.stdout = append(f.stdout, arg.Content...)
	case "stderr":
		f.stderr = append(f.stderr, arg.Content...)
	}
	event := db.RunEvent{
		ID:            int64(len(f.events) + 1),
		OrgID:         arg.OrgID,
		RunID:         arg.RunID,
		ExecutionID:   arg.ExecutionID,
		AttemptNumber: pgtype.Int4{Int32: 1, Valid: true},
		Kind:          arg.Kind,
		Payload:       arg.Payload,
		CreatedAt:     testTime(),
	}
	f.events = append(f.events, event)
	return db.AppendRunLogChunkRow{
		RunID:         arg.RunID,
		ExecutionID:   arg.ExecutionID,
		AttemptNumber: 1,
		Stream:        arg.Stream,
		Seq:           int64(len(f.events)),
		ObservedSeq:   arg.ObservedSeq,
		Content:       arg.Content,
		CreatedAt:     testTime(),
	}, nil
}

func (f *fakeStore) GetRunLogSnapshot(_ context.Context, arg db.GetRunLogSnapshotParams) (db.GetRunLogSnapshotRow, error) {
	f.runLogSnapshot = arg
	if f.run.ID != arg.RunID || (len(f.stdout) == 0 && len(f.stderr) == 0) {
		return db.GetRunLogSnapshotRow{}, pgx.ErrNoRows
	}
	return db.GetRunLogSnapshotRow{
		RunID:        arg.RunID,
		Stdout:       f.stdout,
		Stderr:       f.stderr,
		Truncated:    pgtype.Bool{Bool: f.logTruncated, Valid: true},
		StdoutCursor: f.stdoutCursor,
		StderrCursor: f.stderrCursor,
		UpdatedAt:    testTime(),
	}, nil
}

func (f *fakeStore) AppendRunEventForExecution(_ context.Context, arg db.AppendRunEventForExecutionParams) (db.RunEvent, error) {
	if f.executionID != arg.ExecutionID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.RunEvent{}, pgx.ErrNoRows
	}
	event := db.RunEvent{
		ID:            int64(len(f.events) + 1),
		OrgID:         arg.OrgID,
		RunID:         arg.RunID,
		ExecutionID:   arg.ExecutionID,
		AttemptNumber: pgtype.Int4{Int32: 1, Valid: true},
		Kind:          arg.Kind,
		Payload:       arg.Payload,
		CreatedAt:     testTime(),
	}
	f.events = append(f.events, event)
	return event, nil
}

func (f *fakeStore) UpsertCasObject(_ context.Context, arg db.UpsertCasObjectParams) (db.CasObject, error) {
	f.casObjects = append(f.casObjects, arg)
	return db.CasObject{
		Digest:    arg.Digest,
		SizeBytes: arg.SizeBytes,
		MediaType: arg.MediaType,
		CreatedAt: testTime(),
	}, nil
}

func (f *fakeStore) GetCasObject(_ context.Context, digest string) (db.CasObject, error) {
	if f.getCasObjectErr != nil {
		return db.CasObject{}, f.getCasObjectErr
	}
	for _, object := range f.casObjects {
		if object.Digest == digest {
			return db.CasObject{
				Digest:    object.Digest,
				SizeBytes: object.SizeBytes,
				MediaType: object.MediaType,
				CreatedAt: testTime(),
			}, nil
		}
	}
	return db.CasObject{}, pgx.ErrNoRows
}

func (f *fakeStore) CreateWaitpointForExecution(_ context.Context, arg db.CreateWaitpointForExecutionParams) (db.CreateWaitpointForExecutionRow, error) {
	if f.executionID != arg.ExecutionID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.CreateWaitpointForExecutionRow{}, pgx.ErrNoRows
	}
	f.waitpoint = fakeWaitpoint{
		ID:             arg.ID,
		RunWaitID:      arg.RunWaitID,
		OrgID:          arg.OrgID,
		RunID:          arg.RunID,
		ExecutionID:    arg.ExecutionID,
		CheckpointID:   arg.CheckpointID,
		CorrelationID:  arg.CorrelationID,
		Kind:           arg.Kind,
		Request:        arg.Request,
		DisplayText:    arg.DisplayText,
		TimeoutSeconds: arg.TimeoutSeconds,
		PolicyName:     arg.PolicyName,
		PolicySnapshot: arg.PolicySnapshot,
		Status:         db.RunWaitStatusOpening,
		RequestedAt:    testTime(),
	}
	return db.CreateWaitpointForExecutionRow{
		ID:             f.waitpoint.ID,
		RunWaitID:      waitpointRunWaitID(f.waitpoint),
		OrgID:          f.waitpoint.OrgID,
		RunID:          f.waitpoint.RunID,
		ExecutionID:    f.waitpoint.ExecutionID,
		CheckpointID:   f.waitpoint.CheckpointID,
		CorrelationID:  f.waitpoint.CorrelationID,
		Kind:           f.waitpoint.Kind,
		Request:        f.waitpoint.Request,
		DisplayText:    f.waitpoint.DisplayText,
		TimeoutSeconds: f.waitpoint.TimeoutSeconds,
		PolicyName:     f.waitpoint.PolicyName,
		PolicySnapshot: f.waitpoint.PolicySnapshot,
		Status:         f.waitpoint.Status,
		ResolutionKind: f.waitpoint.ResolutionKind,
		Resolution:     f.waitpoint.Resolution,
		RequestedAt:    f.waitpoint.RequestedAt,
		ResolvedAt:     f.waitpoint.ResolvedAt,
	}, nil
}

func (f *fakeStore) MarkWaitpointCheckpointDurableReady(_ context.Context, arg db.MarkWaitpointCheckpointDurableReadyParams) (db.MarkWaitpointCheckpointDurableReadyRow, error) {
	if f.executionID != arg.ExecutionID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.MarkWaitpointCheckpointDurableReadyRow{}, pgx.ErrNoRows
	}
	if !f.waitpoint.ID.Valid || f.waitpoint.ID != arg.WaitpointID || waitpointRunWaitID(f.waitpoint) != arg.RunWaitID || f.waitpoint.CheckpointID != arg.CheckpointID || f.waitpoint.Status != db.RunWaitStatusOpening {
		return db.MarkWaitpointCheckpointDurableReadyRow{}, pgx.ErrNoRows
	}
	f.waitpoint.Status = db.RunWaitStatusWaiting
	f.waitpoint.RequestedAt = testTime()
	f.checkpoint = db.Checkpoint{
		ID:          arg.CheckpointID,
		OrgID:       arg.OrgID,
		RunID:       arg.RunID,
		ExecutionID: arg.ExecutionID,
		Status:      db.CheckpointStatusReady,
		Manifest:    arg.Manifest,
		ReadyAt:     testTime(),
	}
	f.run.Status = db.RunStatusWaiting
	f.run.LatestCheckpointID = arg.CheckpointID
	f.run.CurrentExecutionID = pgtype.UUID{}
	f.run.UpdatedAt = testTime()
	f.events = append(f.events, db.RunEvent{
		ID:        int64(len(f.events) + 1),
		OrgID:     arg.OrgID,
		RunID:     arg.RunID,
		Kind:      "checkpoint.ready",
		Payload:   arg.CheckpointPayload,
		CreatedAt: testTime(),
	})
	f.events = append(f.events, db.RunEvent{
		ID:        int64(len(f.events) + 1),
		OrgID:     arg.OrgID,
		RunID:     arg.RunID,
		Kind:      "waitpoint.requested",
		Payload:   []byte(`{"kind":"human"}`),
		CreatedAt: testTime(),
	})
	return db.MarkWaitpointCheckpointDurableReadyRow{
		ID:             f.waitpoint.ID,
		RunWaitID:      waitpointRunWaitID(f.waitpoint),
		OrgID:          f.waitpoint.OrgID,
		RunID:          f.waitpoint.RunID,
		ExecutionID:    f.waitpoint.ExecutionID,
		CheckpointID:   f.waitpoint.CheckpointID,
		CorrelationID:  f.waitpoint.CorrelationID,
		Kind:           f.waitpoint.Kind,
		Request:        f.waitpoint.Request,
		DisplayText:    f.waitpoint.DisplayText,
		TimeoutSeconds: f.waitpoint.TimeoutSeconds,
		PolicyName:     f.waitpoint.PolicyName,
		PolicySnapshot: f.waitpoint.PolicySnapshot,
		Status:         f.waitpoint.Status,
		ResolutionKind: f.waitpoint.ResolutionKind,
		Resolution:     f.waitpoint.Resolution,
		RequestedAt:    f.waitpoint.RequestedAt,
		ResolvedAt:     f.waitpoint.ResolvedAt,
	}, nil
}

func (f *fakeStore) MarkWaitpointCheckpointFailed(_ context.Context, arg db.MarkWaitpointCheckpointFailedParams) (db.MarkWaitpointCheckpointFailedRow, error) {
	if f.executionID != arg.ExecutionID || f.executionWorkerInstanceID != arg.WorkerInstanceID || !f.waitpoint.ID.Valid || f.waitpoint.ID != arg.WaitpointID || waitpointRunWaitID(f.waitpoint) != arg.RunWaitID || f.waitpoint.CheckpointID != arg.CheckpointID || f.waitpoint.Status != db.RunWaitStatusOpening {
		return db.MarkWaitpointCheckpointFailedRow{}, pgx.ErrNoRows
	}
	f.waitpoint.Status = db.RunWaitStatusCancelled
	f.waitpoint.ResolutionKind = pgtype.Text{String: "cancelled", Valid: true}
	f.waitpoint.Resolution = []byte(`{"source":"checkpoint"}`)
	f.waitpoint.ResolvedAt = testTime()
	return db.MarkWaitpointCheckpointFailedRow{
		ID:             f.waitpoint.ID,
		RunWaitID:      waitpointRunWaitID(f.waitpoint),
		OrgID:          f.waitpoint.OrgID,
		RunID:          f.waitpoint.RunID,
		ExecutionID:    f.waitpoint.ExecutionID,
		CheckpointID:   f.waitpoint.CheckpointID,
		CorrelationID:  f.waitpoint.CorrelationID,
		Kind:           f.waitpoint.Kind,
		Request:        f.waitpoint.Request,
		DisplayText:    f.waitpoint.DisplayText,
		TimeoutSeconds: f.waitpoint.TimeoutSeconds,
		PolicyName:     f.waitpoint.PolicyName,
		PolicySnapshot: f.waitpoint.PolicySnapshot,
		Status:         f.waitpoint.Status,
		ResolutionKind: f.waitpoint.ResolutionKind,
		Resolution:     f.waitpoint.Resolution,
		RequestedAt:    f.waitpoint.RequestedAt,
		ResolvedAt:     f.waitpoint.ResolvedAt,
	}, nil
}

func (f *fakeStore) GetPendingWaitpointForRun(_ context.Context, arg db.GetPendingWaitpointForRunParams) (db.GetPendingWaitpointForRunRow, error) {
	if f.waitpoint.ID.Valid && f.waitpoint.OrgID == arg.OrgID && f.waitpoint.RunID == arg.RunID && f.waitpoint.Status == db.RunWaitStatusWaiting {
		return fakeWaitpointRow(f.waitpoint), nil
	}
	return db.GetPendingWaitpointForRunRow{}, pgx.ErrNoRows
}

func (f *fakeStore) GetWaitpointForResponseTokenCreation(_ context.Context, arg db.GetWaitpointForResponseTokenCreationParams) (db.GetWaitpointForResponseTokenCreationRow, error) {
	if f.waitpoint.ID.Valid && f.waitpoint.OrgID == arg.OrgID && f.waitpoint.ID == arg.WaitpointID && f.waitpoint.Status == db.RunWaitStatusWaiting {
		return db.GetWaitpointForResponseTokenCreationRow{ID: f.waitpoint.ID, OrgID: f.waitpoint.OrgID, ProjectID: f.waitpoint.ProjectID, EnvironmentID: f.waitpoint.EnvironmentID, Kind: f.waitpoint.Kind}, nil
	}
	return db.GetWaitpointForResponseTokenCreationRow{}, pgx.ErrNoRows
}

func (f *fakeStore) GetWaitpointForRespond(_ context.Context, arg db.GetWaitpointForRespondParams) (db.Waitpoint, error) {
	if !f.waitpoint.ID.Valid || f.waitpoint.OrgID != arg.OrgID || f.waitpoint.ID != arg.WaitpointID {
		return db.Waitpoint{}, pgx.ErrNoRows
	}
	projectID := f.waitpoint.ProjectID
	if !projectID.Valid {
		projectID = testProjectID()
	}
	environmentID := f.waitpoint.EnvironmentID
	if !environmentID.Valid {
		environmentID = testEnvironmentID()
	}
	return db.Waitpoint{
		ID:            f.waitpoint.ID,
		OrgID:         f.waitpoint.OrgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Kind:          f.waitpoint.Kind,
		Status:        db.WaitpointStatusPending,
		Request:       f.waitpoint.Request,
		DisplayText:   f.waitpoint.DisplayText,
		CreatedAt:     f.waitpoint.CreatedAt,
	}, nil
}

func (f *fakeStore) ListWaitpointDeliveries(context.Context, db.ListWaitpointDeliveriesParams) ([]db.WaitpointDelivery, error) {
	return nil, nil
}

func (f *fakeStore) ResolveWaitpoint(_ context.Context, arg db.ResolveWaitpointParams) (db.ResolveWaitpointRow, error) {
	if !f.waitpoint.ID.Valid || f.waitpoint.OrgID != arg.OrgID || f.waitpoint.ID != arg.ID || f.waitpoint.Kind != arg.Kind {
		return db.ResolveWaitpointRow{}, pgx.ErrNoRows
	}
	if !f.waitpoint.ResolutionKind.Valid {
		if f.waitpoint.Status != db.RunWaitStatusWaiting {
			return db.ResolveWaitpointRow{}, pgx.ErrNoRows
		}
		f.waitpoint.ResolutionKind = arg.ResolutionKind
		f.waitpoint.Output = arg.Output
		f.waitpoint.Resolution = arg.Resolution
		f.waitpoint.ResolvedAt = testTime()
	}
	return db.ResolveWaitpointRow{
		ID:             f.waitpoint.ID,
		OrgID:          f.waitpoint.OrgID,
		ProjectID:      f.waitpoint.ProjectID,
		EnvironmentID:  f.waitpoint.EnvironmentID,
		Kind:           f.waitpoint.Kind,
		Request:        f.waitpoint.Request,
		DisplayText:    f.waitpoint.DisplayText,
		Status:         db.WaitpointStatusCompleted,
		ResolutionKind: f.waitpoint.ResolutionKind,
		Resolution:     f.waitpoint.Resolution,
		CompletedAt:    testTime(),
		UpdatedAt:      testTime(),
	}, nil
}

func (f *fakeStore) UnblockRunWaitsForWaitpoint(_ context.Context, arg db.UnblockRunWaitsForWaitpointParams) ([]db.UnblockRunWaitsForWaitpointRow, error) {
	if !f.waitpoint.ID.Valid || f.waitpoint.OrgID != arg.OrgID || f.waitpoint.ID != arg.WaitpointID || f.waitpoint.Status != db.RunWaitStatusWaiting || !f.waitpoint.ResolutionKind.Valid {
		return nil, nil
	}
	if f.resolveStatus == db.RunWaitStatusWaiting {
		return nil, nil
	}
	f.waitpoint.Status = db.RunWaitStatusResuming
	f.run.Status = db.RunStatusQueued
	f.run.CurrentExecutionID = pgtype.UUID{}
	f.run.UpdatedAt = testTime()
	payload, _ := json.Marshal(map[string]any{
		"run_id":          ids.MustFromPG(f.waitpoint.RunID).String(),
		"waitpoint_id":    ids.MustFromPG(f.waitpoint.ID).String(),
		"kind":            string(f.waitpoint.Kind),
		"resolution_kind": f.waitpoint.ResolutionKind.String,
		"result":          json.RawMessage(f.waitpoint.Output),
	})
	f.events = append(f.events, db.RunEvent{ID: int64(len(f.events) + 1), OrgID: arg.OrgID, RunID: f.waitpoint.RunID, Kind: "waitpoint.resolved", Payload: payload, CreatedAt: testTime()})
	return []db.UnblockRunWaitsForWaitpointRow{{ID: f.waitpoint.ID, RunWaitID: waitpointRunWaitID(f.waitpoint), OrgID: f.waitpoint.OrgID, RunID: f.waitpoint.RunID, Status: f.waitpoint.Status}}, nil
}

func (f *fakeStore) RecordWaitpointResponse(_ context.Context, arg db.RecordWaitpointResponseParams) (db.RecordWaitpointResponseRow, error) {
	if !f.waitpoint.ID.Valid || f.waitpoint.OrgID != arg.OrgID || f.waitpoint.ID != arg.WaitpointID || f.waitpoint.Kind != arg.Kind {
		return db.RecordWaitpointResponseRow{}, pgx.ErrNoRows
	}
	for _, existing := range f.waitpointResponses {
		if existing.ResponseKey == arg.ResponseKey {
			if existing.RequestHash != arg.RequestHash {
				return db.RecordWaitpointResponseRow{}, pgx.ErrNoRows
			}
			return fakeWaitpointResponseRow(f.waitpoint, existing), nil
		}
	}
	if f.waitpoint.Status != db.RunWaitStatusWaiting {
		return db.RecordWaitpointResponseRow{}, pgx.ErrNoRows
	}
	f.waitpointResponses = append(f.waitpointResponses, arg)
	return fakeWaitpointResponseRow(f.waitpoint, arg), nil
}

func fakeWaitpointResponseRow(waitpoint fakeWaitpoint, arg db.RecordWaitpointResponseParams) db.RecordWaitpointResponseRow {
	return db.RecordWaitpointResponseRow{
		ID: arg.ID, OrgID: arg.OrgID, ProjectID: waitpoint.ProjectID, EnvironmentID: waitpoint.EnvironmentID, WaitpointID: arg.WaitpointID,
		ResponseKey: arg.ResponseKey, RequestHash: arg.RequestHash, Action: arg.Action, ResolutionKind: arg.ResolutionKind,
		Resolution: arg.Resolution, EventPayload: arg.EventPayload, CompletedByPrincipal: arg.CompletedByPrincipal,
		CompletedVia: arg.CompletedVia, ExternalSubject: arg.ExternalSubject, Metadata: arg.Metadata,
		CreatedAt: testTime(), UpdatedAt: testTime(),
	}
}

func (f *fakeStore) RecordAndResolveWaitpoint(ctx context.Context, record db.RecordWaitpointResponseParams, resolve db.ResolveWaitpointParams) ([]db.UnblockRunWaitsForWaitpointRow, error) {
	if _, err := f.RecordWaitpointResponse(ctx, record); err != nil {
		return nil, err
	}
	if _, err := f.ResolveWaitpoint(ctx, resolve); err != nil {
		return nil, err
	}
	return f.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: resolve.OrgID, WaitpointID: resolve.ID})
}

func (f *fakeStore) ExpireDuePendingWaitpoints(context.Context, pgtype.UUID) error {
	if f.waitpoint.ID.Valid && f.waitpoint.Status == db.RunWaitStatusWaiting && f.waitpoint.TimeoutSeconds.Valid && f.run.Status == db.RunStatusWaiting && !f.run.CurrentExecutionID.Valid {
		if !testTime().Time.Before(f.waitpoint.RequestedAt.Time.Add(time.Duration(f.waitpoint.TimeoutSeconds.Int32) * time.Second)) {
			f.waitpoint.Status = db.RunWaitStatusResuming
			f.waitpoint.ResolutionKind = pgtype.Text{String: "timed_out", Valid: true}
			f.waitpoint.Resolution = []byte(`{"at":"2026-05-08T12:00:00Z"}`)
			f.waitpoint.ResolvedAt = testTime()
			f.run.Status = db.RunStatusQueued
		}
	}
	return nil
}

func (f *fakeStore) GetRunRestorePayload(_ context.Context, arg db.GetRunRestorePayloadParams) (db.GetRunRestorePayloadRow, error) {
	if f.run.OrgID != arg.OrgID || f.run.ID != arg.RunID || f.run.CurrentExecutionID != arg.ExecutionID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.GetRunRestorePayloadRow{}, pgx.ErrNoRows
	}
	if f.run.LatestCheckpointID != f.checkpoint.ID || f.checkpoint.Status != db.CheckpointStatusRestoring {
		return db.GetRunRestorePayloadRow{}, pgx.ErrNoRows
	}
	if !f.waitpoint.ID.Valid || f.waitpoint.Status != db.RunWaitStatusResuming || !f.waitpoint.ResolutionKind.Valid || f.waitpoint.CheckpointID != f.checkpoint.ID {
		return db.GetRunRestorePayloadRow{}, pgx.ErrNoRows
	}
	return db.GetRunRestorePayloadRow{
		CheckpointID:   f.checkpoint.ID,
		Manifest:       f.checkpoint.Manifest,
		RunWaitID:      waitpointRunWaitID(f.waitpoint),
		WaitpointID:    f.waitpoint.ID,
		WaitpointKind:  f.waitpoint.Kind,
		ResolutionKind: f.waitpoint.ResolutionKind,
		Resolution:     f.waitpoint.Resolution,
	}, nil
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
	role        auth.Role
	kind        auth.ActorKind
	userID      uuid.UUID
	apiKeyID    uuid.UUID
	permissions []auth.PermissionGrant
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
		kind = auth.ActorKindSession
	}
	userID := f.userID
	if kind == auth.ActorKindSession && userID == uuid.Nil {
		userID = uuid.MustParse("00000000-0000-0000-0000-000000000001")
	}
	apiKeyID := f.apiKeyID
	if kind == auth.ActorKindAPIKey && apiKeyID == uuid.Nil {
		apiKeyID = uuid.MustParse("00000000-0000-0000-0000-000000000002")
	}
	return auth.Actor{OrgID: ids.DefaultOrgID, UserID: userID, APIKeyID: apiKeyID, Role: role, Kind: kind, Permissions: f.permissions}, nil
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
