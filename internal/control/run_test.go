package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const testGitSHA = "0123456789abcdef0123456789abcdef01234567"
const testWorkerTokenSecret = "01234567890123456789012345678901"
const testWorkerInstanceCredentialID = "00000000-0000-0000-0000-00000000c001"

func requireErrorCode(t *testing.T, body []byte, want string) {
	t.Helper()
	var response struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode error response %q: %v", string(body), err)
	}
	if response.Code != want {
		t.Fatalf("error response code = %q, want %q; body=%s", response.Code, want, string(body))
	}
}

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

func testDeploymentSandboxID() pgtype.UUID {
	return pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000307"))
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
		ProtocolVersion:         api.CurrentWorkerProtocolVersion,
		RuntimeArch:             "arm64",
		RuntimeABI:              "helmr.firecracker.snapshot.v0",
		KernelDigest:            "sha256:kernel",
		InitramfsDigest:         "sha256:initramfs",
		RootfsDigest:            "sha256:rootfs",
		CNIProfile:              "helmr/v0",
		MaxVCPUs:                2,
		MaxMemoryMiB:            2048,
		MaxDiskMiB:              20480,
		ExecutionSlotsAvailable: 1,
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

func testSandboxFingerprint() string {
	return "sha256:" + strings.Repeat("7", 64)
}

func fakeWorkspaceForTaskStart(workspaceID pgtype.UUID) db.GetWorkspaceForTaskStartRow {
	return db.GetWorkspaceForTaskStartRow{
		ID:                                workspaceID,
		OrgID:                             pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:                         testProjectID(),
		EnvironmentID:                     testEnvironmentID(),
		DeploymentSandboxID:               testDeploymentSandboxID(),
		SandboxID:                         "default",
		SandboxFingerprint:                testSandboxFingerprint(),
		State:                             db.WorkspaceStateActive,
		WorkspaceMountPath:                "/workspace",
		DeploymentSandboxResourceFloor:    []byte(`{"milli_cpu":1000,"memory_mib":512,"disk_mib":1024}`),
		DeploymentSandboxDiskFloorMib:     1024,
		DeploymentSandboxNetworkPolicy:    []byte(`{"internet":"egress"}`),
		DeploymentSandboxRootfsDigest:     "sha256:" + strings.Repeat("f", 64),
		DeploymentSandboxRuntimeAbi:       testWorkerCapabilities().RuntimeABI,
		DeploymentSandboxGuestdAbi:        "helmr.guestd.v0",
		DeploymentSandboxAdapterAbi:       "helmr.adapter.v0",
		DeploymentSandboxFilesystemFormat: "tar",
		DeploymentSandboxContractVersion:  1,
		DeploymentSandboxFingerprint:      testSandboxFingerprint(),
	}
}

func TestAPIKeyRunCreateRejectsActorWithoutEnvironmentScope(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, DispatchQueue: store, Auth: fakeAuth{kind: auth.ActorKindAPIKey, role: auth.RoleOwner}})
	bodyBytes, err := json.Marshal(api.TaskStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
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
	}, CAS: &fakeCAS{}},
	)
	bodyBytes, err := json.Marshal(api.TaskStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
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
	bodyBytes, err := json.Marshal(api.TaskStartRequest{
		ProjectID:     testProjectIDString(),
		EnvironmentID: testEnvironmentIDString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
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

	bodyBytes, err := json.Marshal(api.TaskStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/bad%20task/start", bytes.NewReader(bodyBytes))
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

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewBufferString(`{
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

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTaskStartRejectsDeploymentSelection(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", strings.NewReader(`{"options":{"version":"20260101.99"}}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "version is not accepted for task start") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestTaskStartRejectsOversizedExternalID(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", strings.NewReader(`{"external_id":"`+strings.Repeat("x", maxTaskSessionExternalIDBytes+1)+`"}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "external_id must be at most") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestWorkspaceMaterializeEnsuresRunnableMaterializationCreated(t *testing.T) {
	workspaceID := uuid.Must(uuid.NewV7())
	store := &fakeStore{ensureWorkspaceMaterializationInserted: true}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspaceID.String()+"/materialize", strings.NewReader(`{}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.ensureWorkspaceMaterializationCalls != 1 {
		t.Fatalf("EnsureWorkspaceMaterializationRequested calls = %d, want 1", store.ensureWorkspaceMaterializationCalls)
	}
	if got := pgvalue.MustUUIDValue(store.ensureWorkspaceMaterialization.WorkspaceID); got != workspaceID {
		t.Fatalf("materialization workspace_id = %s, want %s", got, workspaceID)
	}
	if store.ensureWorkspaceMaterialization.Priority != 0 {
		t.Fatalf("materialization priority = %d, want 0", store.ensureWorkspaceMaterialization.Priority)
	}
	if string(store.ensureWorkspaceMaterialization.Request) != `{"source":"api"}` {
		t.Fatalf("request = %s", string(store.ensureWorkspaceMaterialization.Request))
	}
	var response api.WorkspaceMaterializationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.WorkspaceID != workspaceID.String() {
		t.Fatalf("response workspace_id = %q, want %s", response.WorkspaceID, workspaceID)
	}
	if response.DeploymentSandboxID != pgvalue.MustUUIDValue(testDeploymentSandboxID()).String() {
		t.Fatalf("response deployment_sandbox_id = %q, want %s", response.DeploymentSandboxID, pgvalue.MustUUIDValue(testDeploymentSandboxID()))
	}
	if response.State != string(db.WorkspaceMaterializationStateRequested) {
		t.Fatalf("response state = %q, want %s", response.State, db.WorkspaceMaterializationStateRequested)
	}
}

func TestWorkspaceMaterializeAllowsPrimitiveReadPermissions(t *testing.T) {
	for _, tt := range []struct {
		name       string
		permission auth.Permission
	}{
		{name: "exec read", permission: auth.PermissionExecRead},
		{name: "pty read", permission: auth.PermissionPtyRead},
		{name: "ports read", permission: auth.PermissionPortsRead},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workspaceID := uuid.Must(uuid.NewV7())
			store := &fakeStore{ensureWorkspaceMaterializationInserted: true}
			server := newTestServer(testServerConfig{
				Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				DB:          store,
				Auth:        fakeAuth{permissions: []auth.Permission{tt.permission}},
				CAS:         &fakeCAS{},
				Secrets:     fakeSecrets{},
				EventStream: newTestEventStream(t),
			})
			req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspaceID.String()+"/materialize", strings.NewReader(`{}`))
			req.Header.Set("authorization", "Bearer test-key")
			rec := httptest.NewRecorder()

			server.ServeHTTP(rec, req)

			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if store.ensureWorkspaceMaterializationCalls != 1 {
				t.Fatalf("EnsureWorkspaceMaterializationRequested calls = %d, want 1", store.ensureWorkspaceMaterializationCalls)
			}
		})
	}
}

func TestWorkspaceListAndGetAllowPrimitiveWorkspacePermissions(t *testing.T) {
	for _, tt := range []struct {
		name       string
		permission auth.Permission
	}{
		{name: "files read", permission: auth.PermissionFilesRead},
		{name: "exec create", permission: auth.PermissionExecCreate},
		{name: "ports close", permission: auth.PermissionPortsClose},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workspaceID := uuid.Must(uuid.NewV7())
			store := &fakeStore{workspace: testWorkspaceRow(workspaceID)}
			server := newTestServer(testServerConfig{
				Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				DB:          store,
				Auth:        fakeAuth{permissions: []auth.Permission{tt.permission}},
				CAS:         &fakeCAS{},
				Secrets:     fakeSecrets{},
				EventStream: newTestEventStream(t),
			})

			for _, path := range []string{
				"/api/workspaces",
				"/api/workspaces/" + workspaceID.String(),
			} {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set("authorization", "Bearer test-key")
				rec := httptest.NewRecorder()

				server.ServeHTTP(rec, req)

				if rec.Code != http.StatusOK {
					t.Fatalf("%s status = %d body=%s", path, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

func TestWorkspaceListAndGetRejectUnrelatedPermission(t *testing.T) {
	workspaceID := uuid.Must(uuid.NewV7())
	store := &fakeStore{workspace: testWorkspaceRow(workspaceID)}
	server := newTestServer(testServerConfig{
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:          store,
		Auth:        fakeAuth{permissions: []auth.Permission{auth.PermissionRunsRead}},
		CAS:         &fakeCAS{},
		Secrets:     fakeSecrets{},
		EventStream: newTestEventStream(t),
	})

	for _, path := range []string{
		"/api/workspaces",
		"/api/workspaces/" + workspaceID.String(),
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("authorization", "Bearer test-key")
		rec := httptest.NewRecorder()

		server.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestWorkspacePrimitiveCreateFingerprintsUseCanonicalProtocol(t *testing.T) {
	envShape := []byte(`{"B":"2","A":"1"}`)
	execFingerprint, err := workspace.ExecCreateFingerprint([]string{"echo", "ok"}, "/workspace", envShape, false, db.WorkspaceFilesystemModeWrite)
	if err != nil {
		t.Fatal(err)
	}
	execFingerprintAgain, err := workspace.ExecCreateFingerprint([]string{"echo", "ok"}, "/workspace", []byte(`{"A":"1","B":"2"}`), false, db.WorkspaceFilesystemModeWrite)
	if err != nil {
		t.Fatal(err)
	}
	if execFingerprint != execFingerprintAgain {
		t.Fatalf("exec fingerprint changed with JSON field order: %q != %q", execFingerprint, execFingerprintAgain)
	}
	ptyFingerprint, err := workspace.PtyCreateFingerprint("/workspace", 80, 24, db.WorkspaceFilesystemModeWrite)
	if err != nil {
		t.Fatal(err)
	}
	ptyFingerprintAgain, err := workspace.PtyCreateFingerprint("/workspace", 80, 24, db.WorkspaceFilesystemModeWrite)
	if err != nil {
		t.Fatal(err)
	}
	if ptyFingerprintAgain != ptyFingerprint {
		t.Fatalf("pty fingerprint changed across identical inputs: %q != %q", ptyFingerprint, ptyFingerprintAgain)
	}
}

func testWorkspaceRow(id uuid.UUID) db.Workspace {
	now := testTime()
	return db.Workspace{
		ID:                  pgvalue.UUID(id),
		OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:           testProjectID(),
		EnvironmentID:       testEnvironmentID(),
		DeploymentSandboxID: testDeploymentSandboxID(),
		SandboxID:           "test-sandbox",
		SandboxFingerprint:  "test-sandbox-fingerprint",
		State:               db.WorkspaceStateActive,
		DesiredState:        db.WorkspaceDesiredStateActive,
		DirtyState:          db.WorkspaceDirtyStateClean,
		Metadata:            []byte(`{}`),
		Tags:                []string{},
		LastActivityAt:      now,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
}

func TestWorkspaceCreateResolvesSandboxSelectorAndReplaysIdempotency(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	body := `{"sandbox_id":"test-sandbox","deployment_id":"` + pgvalue.MustUUIDValue(testDeploymentID()).String() + `","external_id":"case-1","metadata":{"owner":"platform"},"tags":["smoke"],"idempotency_key":"workspace-key"}`

	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.resolveDeploymentSandboxCalls != 1 {
		t.Fatalf("ResolveDeploymentSandboxForWorkspaceCreate calls = %d, want 1", store.resolveDeploymentSandboxCalls)
	}
	if store.resolveDeploymentSandbox.SandboxID != "test-sandbox" {
		t.Fatalf("resolved sandbox_id = %q, want test-sandbox", store.resolveDeploymentSandbox.SandboxID)
	}
	if store.resolveDeploymentSandbox.DeploymentID != testDeploymentID() {
		t.Fatalf("resolved deployment_id = %s, want %s", pgvalue.MustUUIDValue(store.resolveDeploymentSandbox.DeploymentID), pgvalue.MustUUIDValue(testDeploymentID()))
	}
	if store.createWorkspaceCalls != 1 {
		t.Fatalf("CreateWorkspaceFromSandbox calls = %d, want 1", store.createWorkspaceCalls)
	}
	if store.workspace.DeploymentSandboxID != testDeploymentSandboxID() {
		t.Fatalf("created deployment_sandbox_id = %s, want %s", pgvalue.MustUUIDValue(store.workspace.DeploymentSandboxID), pgvalue.MustUUIDValue(testDeploymentSandboxID()))
	}
	if len(store.createdWorkspaceOperationIdempotencies) != 1 {
		t.Fatalf("workspace operation idempotencies = %d, want 1", len(store.createdWorkspaceOperationIdempotencies))
	}
	idempotency := store.createdWorkspaceOperationIdempotencies[0]
	if idempotency.WorkspaceID.Valid {
		t.Fatalf("workspace.create idempotency workspace_id = %s, want null", pgvalue.MustUUIDValue(idempotency.WorkspaceID))
	}
	if idempotency.ResponseResourceID.Valid {
		t.Fatalf("pending idempotency response_resource_id = %s, want null", pgvalue.MustUUIDValue(idempotency.ResponseResourceID))
	}
	if store.workspaceOperationIdempotency.ResponseResourceID != store.workspace.ID {
		t.Fatalf("completed idempotency response_resource_id = %s, want workspace %s", pgvalue.MustUUIDValue(store.workspaceOperationIdempotency.ResponseResourceID), pgvalue.MustUUIDValue(store.workspace.ID))
	}

	req = httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("retry status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.createWorkspaceCalls != 1 {
		t.Fatalf("CreateWorkspaceFromSandbox calls after retry = %d, want 1", store.createWorkspaceCalls)
	}
	var response api.WorkspaceEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.IsCached {
		t.Fatalf("retry response is_cached = false, want true")
	}
	if response.Workspace.ID != pgvalue.MustUUIDValue(store.workspace.ID).String() {
		t.Fatalf("retry workspace id = %q, want %s", response.Workspace.ID, pgvalue.MustUUIDValue(store.workspace.ID))
	}
}

func TestWorkspaceCreateRejectsIdempotencyFingerprintMismatch(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	firstBody := `{"sandbox_id":"test-sandbox","external_id":"case-1","idempotency_key":"workspace-key"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(firstBody))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first status = %d body=%s", rec.Code, rec.Body.String())
	}

	secondBody := `{"sandbox_id":"test-sandbox","external_id":"case-2","idempotency_key":"workspace-key"}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(secondBody))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("mismatch status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "idempotency_fingerprint_mismatch") {
		t.Fatalf("mismatch body = %s", rec.Body.String())
	}
	if store.createWorkspaceCalls != 1 {
		t.Fatalf("CreateWorkspaceFromSandbox calls = %d, want 1", store.createWorkspaceCalls)
	}
}

func TestWorkspaceCreateReturnsPendingForInFlightIdempotency(t *testing.T) {
	store := &fakeStore{}
	fingerprint, err := workspaceCreateFingerprint(api.WorkspaceCreateRequest{SandboxID: "test-sandbox", ExternalID: "case-1"}, []byte(`{}`), []string{})
	if err != nil {
		t.Fatal(err)
	}
	store.workspaceOperationIdempotency = db.WorkspaceOperationIdempotency{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:          testProjectID(),
		EnvironmentID:      testEnvironmentID(),
		OperationKind:      workspaceCreateOperationKind,
		IdempotencyKey:     "workspace-key",
		RequestFingerprint: fingerprint,
		ExpiresAt:          pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(`{"sandbox_id":"test-sandbox","external_id":"case-1","idempotency_key":"workspace-key"}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Body.String(), "workspace_operation_pending") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if store.createWorkspaceCalls != 0 {
		t.Fatalf("CreateWorkspaceFromSandbox calls = %d, want 0", store.createWorkspaceCalls)
	}
}

func TestWorkspaceStopIdempotencyResponseReplaysCompletedResponse(t *testing.T) {
	fingerprint, err := workspaceStopFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	response := api.WorkspaceStopResponse{WorkspaceID: pgvalue.MustUUIDValue(workspaceID).String(), State: "no_active_materialization"}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	replayed, ok, err := workspaceStopIdempotencyResponse(db.EnsureWorkspaceOperationIdempotencyRow{
		RequestFingerprint: fingerprint,
		ResponseResourceID: workspaceID,
		ResponseBody:       body,
		Inserted:           false,
	}, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("replayed = false, want true")
	}
	if replayed.WorkspaceID != response.WorkspaceID || replayed.State != response.State {
		t.Fatalf("replayed response = %+v, want %+v", replayed, response)
	}
}

func TestWorkspaceStopIdempotencyResponseRejectsPendingAndMismatch(t *testing.T) {
	fingerprint, err := workspaceStopFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	_, replayed, err := workspaceStopIdempotencyResponse(db.EnsureWorkspaceOperationIdempotencyRow{
		RequestFingerprint: fingerprint,
		Inserted:           false,
	}, fingerprint)
	if !errors.Is(err, errWorkspaceOperationPending) || replayed {
		t.Fatalf("pending err = %v replayed=%v, want idempotency pending without replay", err, replayed)
	}
	_, replayed, err = workspaceStopIdempotencyResponse(db.EnsureWorkspaceOperationIdempotencyRow{
		RequestFingerprint: "different",
		ResponseResourceID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ResponseBody:       []byte(`{"workspace_id":"w","state":"no_active_materialization"}`),
		Inserted:           false,
	}, fingerprint)
	if !errors.Is(err, errWorkspaceOperationIdempotencyUsed) || replayed {
		t.Fatalf("mismatch err = %v replayed=%v, want fingerprint mismatch without replay", err, replayed)
	}
}

func TestWorkspaceCreateRejectsDeploymentSandboxSelector(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(`{"deployment_sandbox_id":"`+pgvalue.MustUUIDValue(testDeploymentSandboxID()).String()+`"}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown field") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if store.createWorkspaceCalls != 0 {
		t.Fatalf("CreateWorkspaceFromSandbox calls = %d, want 0", store.createWorkspaceCalls)
	}
}

func TestWorkspaceCreateRejectsUndeployedSandboxAsConflict(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(`{"sandbox_id":"missing-sandbox"}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "sandbox_not_deployed") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if store.createWorkspaceCalls != 0 {
		t.Fatalf("CreateWorkspaceFromSandbox calls = %d, want 0", store.createWorkspaceCalls)
	}
}

func TestWorkspaceMaterializeRejectsPublicPriority(t *testing.T) {
	workspaceID := uuid.Must(uuid.NewV7())
	store := &fakeStore{ensureWorkspaceMaterializationInserted: true}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspaceID.String()+"/materialize", strings.NewReader(`{"priority":5}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown field") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if store.ensureWorkspaceMaterializationCalls != 0 {
		t.Fatalf("EnsureWorkspaceMaterializationRequested calls = %d, want 0", store.ensureWorkspaceMaterializationCalls)
	}
}

func TestWorkspaceConnectReturnsExistingRunnableMaterialization(t *testing.T) {
	workspaceID := uuid.Must(uuid.NewV7())
	store := &fakeStore{ensureWorkspaceMaterializationState: db.WorkspaceMaterializationStateRunning}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspaceID.String()+"/connect", strings.NewReader(`{}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.ensureWorkspaceMaterializationCalls != 1 {
		t.Fatalf("EnsureWorkspaceMaterializationRequested calls = %d, want 1", store.ensureWorkspaceMaterializationCalls)
	}
	if got := pgvalue.MustUUIDValue(store.ensureWorkspaceMaterialization.WorkspaceID); got != workspaceID {
		t.Fatalf("materialization workspace_id = %s, want %s", got, workspaceID)
	}
	var response api.WorkspaceMaterializationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.WorkspaceID != workspaceID.String() {
		t.Fatalf("response workspace_id = %q, want %s", response.WorkspaceID, workspaceID)
	}
	if response.State != string(db.WorkspaceMaterializationStateRunning) {
		t.Fatalf("response state = %q, want %s", response.State, db.WorkspaceMaterializationStateRunning)
	}
}

func TestWorkspaceMaterializeReturnsServerErrorWhenEnsureFails(t *testing.T) {
	workspaceID := uuid.Must(uuid.NewV7())
	store := &fakeStore{ensureWorkspaceMaterializationErr: errors.New("database unavailable")}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspaceID.String()+"/materialize", strings.NewReader(`{}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.ensureWorkspaceMaterializationCalls != 1 {
		t.Fatalf("EnsureWorkspaceMaterializationRequested calls = %d, want 1", store.ensureWorkspaceMaterializationCalls)
	}
	if !strings.Contains(rec.Body.String(), "ensure workspace materialization") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestTaskStartAttachesCompatibleWorkspace(t *testing.T) {
	workspaceID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeStore{attachedWorkspace: fakeWorkspaceForTaskStart(workspaceID)}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	bodyBytes, err := json.Marshal(api.TaskStartRequest{
		Options: api.TaskStartOptions{WorkspaceID: pgvalue.MustUUIDValue(workspaceID).String()},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.TaskStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Session.WorkspaceID != pgvalue.MustUUIDValue(workspaceID).String() {
		t.Fatalf("workspace_id = %q, want %s", response.Session.WorkspaceID, pgvalue.MustUUIDValue(workspaceID).String())
	}
	if store.createWorkspaceCalls != 0 {
		t.Fatalf("CreateWorkspace calls = %d, want 0", store.createWorkspaceCalls)
	}
	if store.ensureWorkspaceMaterializationCalls != 1 {
		t.Fatalf("EnsureWorkspaceMaterializationRequested calls = %d, want 1", store.ensureWorkspaceMaterializationCalls)
	}
	if store.ensureWorkspaceMaterialization.WorkspaceID != workspaceID {
		t.Fatalf("materialization workspace_id = %s, want %s", pgvalue.MustUUIDValue(store.ensureWorkspaceMaterialization.WorkspaceID), pgvalue.MustUUIDValue(workspaceID))
	}
	if store.ensureWorkspaceMaterialization.Priority != 0 {
		t.Fatalf("materialization priority = %d, want 0", store.ensureWorkspaceMaterialization.Priority)
	}
	if store.setQueuedRunWorkspaceMaterializationCalls != 1 {
		t.Fatalf("SetQueuedRunWorkspaceMaterialization calls = %d, want 1", store.setQueuedRunWorkspaceMaterializationCalls)
	}
	if store.setQueuedRunWorkspaceMaterialization.WorkspaceMaterializationID != store.ensureWorkspaceMaterialization.ID {
		t.Fatalf("run materialization_id = %s, want %s", pgvalue.MustUUIDValue(store.setQueuedRunWorkspaceMaterialization.WorkspaceMaterializationID), pgvalue.MustUUIDValue(store.ensureWorkspaceMaterialization.ID))
	}
}

func TestTaskStartCreatesArtifactBackedWorkspaceForColdStart(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", strings.NewReader(`{}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.createWorkspaceCalls != 1 {
		t.Fatalf("CreateWorkspaceFromSandbox calls = %d, want 1", store.createWorkspaceCalls)
	}
	if !store.workspace.CurrentVersionID.Valid {
		t.Fatalf("created workspace current_version_id is not set: %+v", store.workspace)
	}
	if len(store.artifacts) != 1 {
		t.Fatalf("created artifacts = %d, want 1", len(store.artifacts))
	}
	if len(store.casObjects) != 1 {
		t.Fatalf("upserted CAS objects = %d, want 1", len(store.casObjects))
	}
	if store.artifacts[0].Digest != store.casObjects[0].Digest {
		t.Fatalf("artifact digest = %s, want CAS object digest %s", store.artifacts[0].Digest, store.casObjects[0].Digest)
	}
	if store.artifacts[0].Kind != db.ArtifactKindWorkspaceVersion {
		t.Fatalf("artifact kind = %s, want %s", store.artifacts[0].Kind, db.ArtifactKindWorkspaceVersion)
	}
	casEvent := "cas:" + store.artifacts[0].Digest
	artifactEvent := "artifact:" + store.artifacts[0].Digest
	casEventIndex, artifactEventIndex := -1, -1
	for i, event := range store.artifactAuthorityEvents {
		if event == casEvent {
			casEventIndex = i
		}
		if event == artifactEvent {
			artifactEventIndex = i
		}
	}
	if casEventIndex == -1 || artifactEventIndex == -1 || casEventIndex > artifactEventIndex {
		t.Fatalf("artifact authority event order = %v, want %q before %q", store.artifactAuthorityEvents, casEvent, artifactEvent)
	}
	if store.ensureWorkspaceMaterializationCalls != 1 {
		t.Fatalf("EnsureWorkspaceMaterializationRequested calls = %d, want 1", store.ensureWorkspaceMaterializationCalls)
	}
	if store.ensureWorkspaceMaterialization.WorkspaceID != store.workspace.ID {
		t.Fatalf("materialization workspace_id = %s, want %s", pgvalue.MustUUIDValue(store.ensureWorkspaceMaterialization.WorkspaceID), pgvalue.MustUUIDValue(store.workspace.ID))
	}
	if store.setQueuedRunWorkspaceMaterializationCalls != 1 {
		t.Fatalf("SetQueuedRunWorkspaceMaterialization calls = %d, want 1", store.setQueuedRunWorkspaceMaterializationCalls)
	}
	if store.setQueuedRunWorkspaceMaterialization.WorkspaceMaterializationID != store.ensureWorkspaceMaterialization.ID {
		t.Fatalf("run materialization_id = %s, want %s", pgvalue.MustUUIDValue(store.setQueuedRunWorkspaceMaterialization.WorkspaceMaterializationID), pgvalue.MustUUIDValue(store.ensureWorkspaceMaterialization.ID))
	}
}

func TestTaskStartRejectsIncompatibleWorkspaceBeforeSessionCreation(t *testing.T) {
	workspaceID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	workspace := fakeWorkspaceForTaskStart(workspaceID)
	workspace.SandboxFingerprint = "sha256:" + strings.Repeat("9", 64)
	store := &fakeStore{attachedWorkspace: workspace}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	bodyBytes, err := json.Marshal(api.TaskStartRequest{
		Options: api.TaskStartOptions{WorkspaceID: pgvalue.MustUUIDValue(workspaceID).String()},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "workspace_sandbox_incompatible")
	if store.taskSession.ID.Valid || store.run.ID.Valid || store.createWorkspaceCalls != 0 {
		t.Fatalf("unexpected DB side effects: session=%v run=%v createWorkspaceCalls=%d", store.taskSession.ID.Valid, store.run.ID.Valid, store.createWorkspaceCalls)
	}
}

func TestTaskStartRejectsWorkspaceResourceFloorBeforeSessionCreation(t *testing.T) {
	workspaceID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	workspace := fakeWorkspaceForTaskStart(workspaceID)
	workspace.DeploymentSandboxResourceFloor = []byte(`{"milli_cpu":100,"memory_mib":512,"disk_mib":1024}`)
	store := &fakeStore{attachedWorkspace: workspace}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	bodyBytes, err := json.Marshal(api.TaskStartRequest{
		Options: api.TaskStartOptions{WorkspaceID: pgvalue.MustUUIDValue(workspaceID).String()},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "workspace_resource_floor_unsatisfied")
	if store.taskSession.ID.Valid || store.run.ID.Valid || store.createWorkspaceCalls != 0 {
		t.Fatalf("unexpected DB side effects: session=%v run=%v createWorkspaceCalls=%d", store.taskSession.ID.Valid, store.run.ID.Valid, store.createWorkspaceCalls)
	}
}

func TestTaskStartIdempotencyRequiresCoordinationBeforeDBSideEffects(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}})
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", strings.NewReader(`{"options":{"idempotency_key":"retry-1"}}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "coordination_unavailable")
	if store.taskSession.ID.Valid || store.run.ID.Valid || store.startIdempotency.ID.Valid {
		t.Fatalf("unexpected DB side effects: session=%v run=%v idempotency=%v", store.taskSession.ID.Valid, store.run.ID.Valid, store.startIdempotency.ID.Valid)
	}
}

func TestTaskStartExternalIDRejectsDifferentFingerprint(t *testing.T) {
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeStore{
		taskSession: db.TaskSession{
			ID:                  sessionID,
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			TaskID:              "deploy",
			InitialDeploymentID: testDeploymentID(),
			ActiveDeploymentID:  testDeploymentID(),
			ExternalID:          "durable-1",
			StartFingerprint:    "old-fingerprint",
			Status:              db.TaskSessionStatusOpen,
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
			CreatedAt:        testTime(),
			UpdatedAt:        testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	bodyBytes, err := json.Marshal(api.TaskStartRequest{
		ExternalID: "durable-1",
		Payload:    json.RawMessage(`{"env":"prod"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "session_fingerprint_mismatch")
}

func TestTaskStartExternalIDReturnsExistingSessionOK(t *testing.T) {
	store := &fakeStore{}
	eventStream := newTestEventStream(t)
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: eventStream})
	bodyBytes, err := json.Marshal(api.TaskStartRequest{
		ExternalID: "durable-1",
		Payload:    json.RawMessage(`{"env":"prod"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first status = %d body=%s", rec.Code, rec.Body.String())
	}
	firstRunID := pgvalue.MustUUIDValue(store.run.ID).String()
	staleExternalKey := taskStartClaimKey(dbtest.DefaultOrgID, testProjectID(), testEnvironmentID(), "deploy", "external", "durable-1")
	if err := eventStream.redis.Set(context.Background(), staleExternalKey, "pending:stale-owner", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}

	store.currentDeploymentMissing = true
	req = httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.TaskStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Session.ExternalID != "durable-1" || response.Run.ID != firstRunID || !response.IsCached {
		t.Fatalf("response session/run = %+v / %+v, want existing run %s", response.Session, response.Run, firstRunID)
	}
}

func TestTaskStartExternalIDRejectsExpiredOpenSession(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	bodyBytes, err := json.Marshal(api.TaskStartRequest{
		ExternalID: "durable-1",
		Payload:    json.RawMessage(`{"env":"prod"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first status = %d body=%s", rec.Code, rec.Body.String())
	}
	store.taskSession.ExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}

	req = httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second status = %d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "session_terminal")
}

func TestTaskStartExternalIDUniqueRaceReturnsExistingSessionOK(t *testing.T) {
	payload := json.RawMessage(`{"env":"prod"}`)
	startFingerprint, err := taskStartRequestFingerprint("deploy", payload, taskStartFingerprintTestOptions(t, api.CreateRunOptions{}), []byte(`{}`), nil, "durable-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeStore{
		createTaskSessionErr:             &pgconn.PgError{Code: "23505"},
		getTaskSessionByExternalIDMisses: 1,
		taskSession: db.TaskSession{
			ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			TaskID:              "deploy",
			InitialDeploymentID: testDeploymentID(),
			ActiveDeploymentID:  testDeploymentID(),
			ExternalID:          "durable-1",
			StartFingerprint:    startFingerprint.String,
			Status:              db.TaskSessionStatusOpen,
			CurrentRunID:        runID,
			CurrentRunVersion:   1,
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
	bodyBytes, err := json.Marshal(api.TaskStartRequest{
		ExternalID: "durable-1",
		Payload:    payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.TaskStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Run.ID != pgvalue.MustUUIDValue(runID).String() || !response.IsCached {
		t.Fatalf("response = %+v, want cached existing run %s", response, pgvalue.MustUUIDValue(runID))
	}
}

func TestTaskStartExternalIDWithoutCurrentRunReturnsConflict(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
	bodyBytes, err := json.Marshal(api.TaskStartRequest{
		ExternalID: "durable-1",
		Payload:    json.RawMessage(`{"env":"prod"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first status = %d body=%s", rec.Code, rec.Body.String())
	}
	store.taskSession.CurrentRunID = pgtype.UUID{}

	req = httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "session_has_no_current_run") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestScopedTaskSessionRouteRejectsWrongPathScope(t *testing.T) {
	sessionID := uuid.Must(uuid.NewV7())
	store := &fakeStore{
		taskSession: db.TaskSession{
			ID:                  pgvalue.UUID(sessionID),
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           otherProjectID(),
			EnvironmentID:       otherEnvironmentID(),
			TaskID:              "deploy",
			InitialDeploymentID: testDeploymentID(),
			ActiveDeploymentID:  testDeploymentID(),
			Status:              db.TaskSessionStatusOpen,
			Metadata:            []byte(`{}`),
			Tags:                []string{},
			TerminalReason:      []byte(`{}`),
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
	}
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil)), db: store}
	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+testProjectIDString()+"/environments/"+testEnvironmentIDString()+"/sessions/"+sessionID.String(), nil)
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("projectID", testProjectIDString())
	routeCtx.URLParams.Add("environmentID", testEnvironmentIDString())
	routeCtx.URLParams.Add("sessionID", sessionID.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
	req = req.WithContext(context.WithValue(req.Context(), actorContextKey{}, auth.Actor{OrgID: dbtest.DefaultOrgID, Role: auth.RoleOwner, Kind: auth.ActorKindSession}))
	rec := httptest.NewRecorder()
	server.getTaskSession(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTaskStartRejectsArchivedTask(t *testing.T) {
	store := &fakeStore{archivedTask: true}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}})
	bodyBytes, err := json.Marshal(api.TaskStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "task_archived") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestTaskStartRejectsUndeployedTask(t *testing.T) {
	store := &fakeStore{currentDeploymentMissing: true}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}})
	bodyBytes, err := json.Marshal(api.TaskStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/deploy/start", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "task_not_deployed") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestListTaskSessionsRejectsOverMaxLimit(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodGet, "/api/sessions?limit=201", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "limit must be an integer between 1 and 200") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestTopLevelTaskSessionRouteRejectsSessionActor(t *testing.T) {
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeStore{
		taskSession: db.TaskSession{
			ID:                  sessionID,
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			TaskID:              "deploy",
			InitialDeploymentID: testDeploymentID(),
			ActiveDeploymentID:  testDeploymentID(),
			Status:              db.TaskSessionStatusOpen,
			Metadata:            []byte(`{}`),
			Tags:                []string{},
			TerminalReason:      []byte(`{}`),
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{kind: auth.ActorKindSession}})
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+pgvalue.MustUUIDValue(sessionID).String(), nil)
	req.Header.Set("authorization", "Bearer session-token")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCloseTaskSessionRejectsActiveRun(t *testing.T) {
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeStore{
		taskSession: db.TaskSession{
			ID:                  sessionID,
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			TaskID:              "deploy",
			InitialDeploymentID: testDeploymentID(),
			ActiveDeploymentID:  testDeploymentID(),
			Status:              db.TaskSessionStatusOpen,
			CurrentRunID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Metadata:            []byte(`{}`),
			Tags:                []string{},
			TerminalReason:      []byte(`{}`),
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+pgvalue.MustUUIDValue(sessionID).String()+"/close", strings.NewReader(`{"reason":"done"}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "close_run_active") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestGetTaskSessionUnwrapsStoredResult(t *testing.T) {
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeStore{
		taskSession: db.TaskSession{
			ID:                  sessionID,
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			TaskID:              "deploy",
			InitialDeploymentID: testDeploymentID(),
			ActiveDeploymentID:  testDeploymentID(),
			Status:              db.TaskSessionStatusCompleted,
			Metadata:            []byte(`{}`),
			Tags:                []string{},
			Result:              []byte(`{"ok":true,"value":{"answer":42}}`),
			TerminalReason:      []byte(`{}`),
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+pgvalue.MustUUIDValue(sessionID).String(), nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.TaskSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got := string(response.Result); got != `{"answer":42}` {
		t.Fatalf("result = %s", got)
	}
	if len(response.Error) != 0 {
		t.Fatalf("error = %s", response.Error)
	}
}

func TestCloseTaskSessionReportsActiveRunAfterAttachRace(t *testing.T) {
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeStore{
		taskSession: db.TaskSession{
			ID:                  sessionID,
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			TaskID:              "deploy",
			InitialDeploymentID: testDeploymentID(),
			ActiveDeploymentID:  testDeploymentID(),
			Status:              db.TaskSessionStatusOpen,
			Metadata:            []byte(`{}`),
			Tags:                []string{},
			TerminalReason:      []byte(`{}`),
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
		closeTaskSessionAttachesRun: runID,
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+pgvalue.MustUUIDValue(sessionID).String()+"/close", strings.NewReader(`{"reason":"done"}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "close_run_active")
}

func TestPatchTaskSessionAllowsActiveRun(t *testing.T) {
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeStore{
		taskSession: db.TaskSession{
			ID:                  sessionID,
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			TaskID:              "deploy",
			InitialDeploymentID: testDeploymentID(),
			ActiveDeploymentID:  testDeploymentID(),
			Status:              db.TaskSessionStatusOpen,
			CurrentRunID:        runID,
			Metadata:            []byte(`{}`),
			Tags:                []string{},
			TerminalReason:      []byte(`{}`),
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/"+pgvalue.MustUUIDValue(sessionID).String(), strings.NewReader(`{"metadata":{"owner":"release"},"tags":["phase3"]}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.TaskSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got := string(response.Metadata); got != `{"owner":"release"}` {
		t.Fatalf("metadata = %s", got)
	}
	if len(response.Tags) != 1 || response.Tags[0] != "phase3" {
		t.Fatalf("tags = %+v", response.Tags)
	}
	if response.CurrentRunID != pgvalue.MustUUIDValue(runID).String() {
		t.Fatalf("current run id = %q", response.CurrentRunID)
	}
}

func TestPatchTaskSessionRejectsExpiresAtWithoutExistingExpiry(t *testing.T) {
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeStore{
		taskSession: db.TaskSession{
			ID:                  sessionID,
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			TaskID:              "deploy",
			InitialDeploymentID: testDeploymentID(),
			ActiveDeploymentID:  testDeploymentID(),
			Status:              db.TaskSessionStatusOpen,
			Metadata:            []byte(`{}`),
			Tags:                []string{},
			TerminalReason:      []byte(`{}`),
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})
	expiresAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/"+pgvalue.MustUUIDValue(sessionID).String(), strings.NewReader(`{"expires_at":`+strconv.Quote(expiresAt)+`}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "session_expires_at_not_extendable")
}

func TestPatchTaskSessionRejectsExpiresAtShortening(t *testing.T) {
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	existingExpiry := time.Now().Add(2 * time.Hour).UTC()
	store := &fakeStore{
		taskSession: db.TaskSession{
			ID:                  sessionID,
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			TaskID:              "deploy",
			InitialDeploymentID: testDeploymentID(),
			ActiveDeploymentID:  testDeploymentID(),
			Status:              db.TaskSessionStatusOpen,
			ExpiresAt:           pgvalue.Timestamptz(existingExpiry),
			Metadata:            []byte(`{}`),
			Tags:                []string{},
			TerminalReason:      []byte(`{}`),
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})
	shorterExpiry := existingExpiry.Add(-time.Hour).Format(time.RFC3339Nano)
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/"+pgvalue.MustUUIDValue(sessionID).String(), strings.NewReader(`{"expires_at":`+strconv.Quote(shorterExpiry)+`}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "session_expires_at_not_extendable")
}

func TestCancelTaskSessionIsIdempotent(t *testing.T) {
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeStore{
		taskSession: db.TaskSession{
			ID:                  sessionID,
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			TaskID:              "deploy",
			InitialDeploymentID: testDeploymentID(),
			ActiveDeploymentID:  testDeploymentID(),
			Status:              db.TaskSessionStatusOpen,
			Metadata:            []byte(`{}`),
			Tags:                []string{},
			TerminalReason:      []byte(`{}`),
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})
	for attempt := 0; attempt < 2; attempt++ {
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+pgvalue.MustUUIDValue(sessionID).String()+"/cancel", strings.NewReader(`{"reason":"retry"}`))
		req.Header.Set("authorization", "Bearer test-key")
		rec := httptest.NewRecorder()

		server.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d body=%s", attempt+1, rec.Code, rec.Body.String())
		}
		var response api.TaskSessionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Status != string(db.TaskSessionStatusCancelled) {
			t.Fatalf("attempt %d status = %s", attempt+1, response.Status)
		}
	}
}

func TestCancelTaskSessionDoesNotCancelStaleCurrentRunAfterConcurrentCancel(t *testing.T) {
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	now := testTime()
	openSession := db.TaskSession{
		ID:                  sessionID,
		OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:           testProjectID(),
		EnvironmentID:       testEnvironmentID(),
		TaskID:              "deploy",
		InitialDeploymentID: testDeploymentID(),
		ActiveDeploymentID:  testDeploymentID(),
		Status:              db.TaskSessionStatusOpen,
		CurrentRunID:        runID,
		Metadata:            []byte(`{}`),
		Tags:                []string{},
		TerminalReason:      []byte(`{}`),
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	cancelledSession := openSession
	cancelledSession.Status = db.TaskSessionStatusCancelled
	cancelledSession.CurrentRunID = pgtype.UUID{}
	cancelledSession.CancelledAt = now
	cancelledSession.TerminalReason = []byte(`{"origin":"api","reason":"first"}`)
	cancelledSession.Result = []byte(`{"ok":false,"error":{"name":"TaskCancelled","message":"first","details":{"origin":"api"}}}`)
	store := &fakeStore{
		taskSession:     openSession,
		lockTaskSession: cancelledSession,
		run: db.Run{
			ID:              runID,
			OrgID:           pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:       testProjectID(),
			EnvironmentID:   testEnvironmentID(),
			DeploymentID:    testDeploymentID(),
			TaskID:          "deploy",
			Status:          db.RunStatusRunning,
			ExecutionStatus: db.RunExecutionStatusExecuting,
			CreatedAt:       now,
			UpdatedAt:       now,
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+pgvalue.MustUUIDValue(sessionID).String()+"/cancel", strings.NewReader(`{"reason":"retry"}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.TaskSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != string(db.TaskSessionStatusCancelled) || response.CurrentRunID != "" {
		t.Fatalf("response = %+v, want cancelled session without current run", response)
	}
	if store.cancelRunCalls != 0 || store.runOperation.ID.Valid {
		t.Fatalf("cancel side effects = calls:%d operation:%v", store.cancelRunCalls, store.runOperation.ID.Valid)
	}
}

func TestCancelTaskSessionTerminalizesPendingCancelRun(t *testing.T) {
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &fakeStore{
		taskSession: db.TaskSession{
			ID:                  sessionID,
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			TaskID:              "deploy",
			InitialDeploymentID: testDeploymentID(),
			ActiveDeploymentID:  testDeploymentID(),
			Status:              db.TaskSessionStatusOpen,
			CurrentRunID:        runID,
			Metadata:            []byte(`{}`),
			Tags:                []string{},
			TerminalReason:      []byte(`{}`),
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
		run: db.Run{
			ID:              runID,
			OrgID:           pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:       testProjectID(),
			EnvironmentID:   testEnvironmentID(),
			DeploymentID:    testDeploymentID(),
			TaskID:          "deploy",
			Status:          db.RunStatusRunning,
			ExecutionStatus: db.RunExecutionStatusExecuting,
			CreatedAt:       testTime(),
			UpdatedAt:       testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+pgvalue.MustUUIDValue(sessionID).String()+"/cancel", strings.NewReader(`{"reason":"interrupt"}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.TaskSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != string(db.TaskSessionStatusCancelled) || response.CurrentRunID != "" {
		t.Fatalf("response = %+v, want cancelled session without current run", response)
	}
	if store.cancelRunCalls != 1 || store.run.ExecutionStatus != db.RunExecutionStatusPendingCancel {
		t.Fatalf("cancel calls/status = %d/%s", store.cancelRunCalls, store.run.ExecutionStatus)
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
	if store.run.CurrentRunLeaseID.Valid {
		t.Fatalf("current execution id = %+v", store.run.CurrentRunLeaseID)
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

type fakeListRunsParams struct {
	StatusFilter string
	RowLimit     int32
}

type fakeStore struct {
	db.Querier
	createRun                                   db.CreateScopedRunParams
	listRuns                                    fakeListRunsParams
	listScopedRuns                              db.ListScopedRunSummariesParams
	countScopedRuns                             db.CountScopedRunsByStatusParams
	run                                         db.Run
	createRunErr                                error
	runOperation                                db.RunOperation
	cancelRunErr                                error
	cancelRunCalls                              int
	deployment                                  db.Deployment
	currentDeploymentTaskSecretDeclarations     []byte
	currentDeploymentMissing                    bool
	archivedTask                                bool
	currentDeploymentTaskCalls                  int
	getDeploymentTaskCalls                      int
	deploymentPromotions                        []db.PromoteDeploymentParams
	createDeploymentResult                      *db.Deployment
	createDeploymentErr                         error
	deploymentEvents                            []db.Event
	deploymentTasks                             []db.DeploymentTask
	deploymentStreams                           []db.DeploymentStream
	ensuredSessionStreams                       []db.EnsureSessionStreamParams
	artifacts                                   []db.Artifact
	runEvent                                    db.AppendRunEventParams
	events                                      []db.Event
	stdout                                      []byte
	stderr                                      []byte
	runLogSnapshot                              db.GetRunLogSnapshotParams
	runLogChunksAfter                           db.ListRunLogChunksAfterParams
	runLogChunksAfterCalls                      int
	firstRunLogChunksAfterSeq                   int64
	deferLogChunksUntilSecondList               bool
	logChunks                                   []db.RunLogChunk
	logTruncated                                bool
	updateRunMetadata                           db.UpdateRunMetadataForExecutionParams
	secret                                      db.GetScopedSecretMetadataByNameRow
	secrets                                     []db.ListScopedSecretsRow
	deleteSecret                                db.DeleteScopedSecretParams
	deleteSecretRows                            int64
	defaultProjectID                            pgtype.UUID
	defaultEnvironmentID                        pgtype.UUID
	logCursor                                   int64
	casObjects                                  []db.UpsertCasObjectParams
	artifactAuthorityEvents                     []string
	getCasObjectErr                             error
	sessionID                                   pgtype.UUID
	executionWorkerInstanceID                   pgtype.UUID
	executionLeaseExpiresAt                     pgtype.Timestamptz
	checkpoint                                  db.RuntimeCheckpoint
	abandonedClaim                              bool
	workerBootstrapTokenHash                    []byte
	workerCredentialID                          pgtype.UUID
	workerCredentialSecretHash                  []byte
	dequeueRequest                              dispatch.DequeueRequest
	ackedLeases                                 []dispatch.Lease
	activeQueueLeaseMissing                     bool
	renewErr                                    error
	listQueueScopes                             db.ListQueueScopesParams
	markStaleWorkspaceMaterializationsLostCalls int
	workerQueueCapacity                         db.GetWorkerInstanceQueueCapacityRow
	workerQueueCapacitySet                      bool
	claimWorkspaceMaterialization               db.ClaimWorkspaceMaterializationParams
	claimWorkspaceMaterializationCalls          int
	taskSession                                 db.TaskSession
	lockTaskSession                             db.TaskSession
	createTaskSessionErr                        error
	ensureWorkspaceMaterialization              db.EnsureWorkspaceMaterializationRequestedParams
	ensureWorkspaceMaterializationCalls         int
	ensureWorkspaceMaterializationInserted      bool
	ensureWorkspaceMaterializationState         db.WorkspaceMaterializationState
	ensureWorkspaceMaterializationErr           error
	setQueuedRunWorkspaceMaterialization        db.SetQueuedRunWorkspaceMaterializationParams
	setQueuedRunWorkspaceMaterializationCalls   int
	resolveDeploymentSandbox                    db.ResolveDeploymentSandboxForWorkspaceCreateParams
	resolveDeploymentSandboxCalls               int
	workspaceOperationIdempotency               db.WorkspaceOperationIdempotency
	createdWorkspaceOperationIdempotencies      []db.EnsureWorkspaceOperationIdempotencyParams
	getTaskSessionByExternalIDMisses            int
	workspace                                   db.Workspace
	attachedWorkspace                           db.GetWorkspaceForTaskStartRow
	createWorkspaceCalls                        int
	startIdempotency                            db.GetTaskStartIdempotencyRow
	taskSessionRuns                             []db.TaskSessionRun
	scheduleTriggerNotCurrent                   bool
	closeTaskSessionAttachesRun                 pgtype.UUID
}

type fakeControlTransaction struct{}

func (fakeControlTransaction) Commit(context.Context) error {
	return nil
}

func (fakeControlTransaction) Rollback(context.Context) error {
	return nil
}

func (f *fakeStore) BeginQuerier(context.Context) (db.Querier, controlTransaction, error) {
	return f, fakeControlTransaction{}, nil
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
	if f.createRunErr != nil {
		return db.CreateScopedRunRow{}, f.createRunErr
	}
	f.createRun = arg
	now := testTime()
	f.run = db.Run{
		ID:                    arg.ID,
		OrgID:                 arg.OrgID,
		ProjectID:             arg.ProjectID,
		EnvironmentID:         arg.EnvironmentID,
		DeploymentID:          arg.DeploymentID,
		DeploymentTaskID:      arg.DeploymentTaskID,
		WorkspaceID:           arg.WorkspaceID,
		DeploymentVersion:     arg.DeploymentVersion,
		ApiVersion:            arg.ApiVersion,
		SdkVersion:            arg.SdkVersion,
		CliVersion:            arg.CliVersion,
		TaskSessionID:         arg.TaskSessionID,
		TaskID:                arg.TaskID,
		Status:                db.RunStatusQueued,
		ExecutionStatus:       db.RunExecutionStatusQueued,
		Payload:               arg.Payload,
		QueueName:             arg.QueueName,
		QueueConcurrencyLimit: arg.QueueConcurrencyLimit,
		ConcurrencyKey:        arg.ConcurrencyKey,
		Priority:              arg.Priority,
		QueueTimestamp:        arg.QueueTimestamp,
		Ttl:                   arg.Ttl,
		QueuedExpiresAt:       arg.QueuedExpiresAt,
		MaxActiveDurationMs:   arg.MaxActiveDurationMs,
		TraceID:               arg.TraceID,
		RootSpanID:            arg.RootSpanID,
		CreatedAt:             now,
		UpdatedAt:             now,
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
		ID:                f.run.ID,
		OrgID:             f.run.OrgID,
		ProjectID:         f.run.ProjectID,
		EnvironmentID:     f.run.EnvironmentID,
		DeploymentID:      f.run.DeploymentID,
		DeploymentTaskID:  f.run.DeploymentTaskID,
		WorkspaceID:       f.run.WorkspaceID,
		TaskSessionID:     f.run.TaskSessionID,
		DeploymentVersion: f.run.DeploymentVersion,
		ApiVersion:        f.run.ApiVersion,
		SdkVersion:        f.run.SdkVersion,
		CliVersion:        f.run.CliVersion,
		TaskID:            f.run.TaskID,
		Status:            f.run.Status,
		ExecutionStatus:   f.run.ExecutionStatus,
		Metadata:          f.run.Metadata,
		Tags:              f.run.Tags,
		LockedRetryPolicy: f.run.LockedRetryPolicy,
		ExitCode:          f.run.ExitCode,
		Output:            f.run.Output,
		CreatedAt:         f.run.CreatedAt,
		UpdatedAt:         f.run.UpdatedAt,
	}, nil
}

func (f *fakeStore) GetTaskForStart(_ context.Context, arg db.GetTaskForStartParams) (db.Task, error) {
	for _, task := range f.deploymentTasks {
		if task.OrgID == arg.OrgID && task.ProjectID == arg.ProjectID && task.EnvironmentID == arg.EnvironmentID && task.TaskID == arg.TaskID {
			return db.Task{
				ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:         arg.OrgID,
				ProjectID:     arg.ProjectID,
				EnvironmentID: arg.EnvironmentID,
				TaskID:        arg.TaskID,
				Metadata:      []byte(`{}`),
				CreatedAt:     testTime(),
				UpdatedAt:     testTime(),
			}, nil
		}
	}
	if arg.TaskID == "deploy" && !f.currentDeploymentMissing {
		task := db.Task{
			ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:         arg.OrgID,
			ProjectID:     arg.ProjectID,
			EnvironmentID: arg.EnvironmentID,
			TaskID:        arg.TaskID,
			Metadata:      []byte(`{}`),
			CreatedAt:     testTime(),
			UpdatedAt:     testTime(),
		}
		if f.archivedTask {
			task.ArchivedAt = testTime()
		}
		return task, nil
	}
	return db.Task{}, pgx.ErrNoRows
}

func (f *fakeStore) CreateTaskSession(_ context.Context, arg db.CreateTaskSessionParams) (db.TaskSession, error) {
	if f.createTaskSessionErr != nil {
		return db.TaskSession{}, f.createTaskSessionErr
	}
	now := testTime()
	f.taskSession = db.TaskSession{
		ID:                  arg.ID,
		OrgID:               arg.OrgID,
		ProjectID:           arg.ProjectID,
		EnvironmentID:       arg.EnvironmentID,
		TaskID:              arg.TaskID,
		InitialDeploymentID: arg.InitialDeploymentID,
		ActiveDeploymentID:  arg.ActiveDeploymentID,
		ExternalID:          arg.ExternalID,
		StartFingerprint:    arg.StartFingerprint,
		Status:              db.TaskSessionStatusOpen,
		CurrentRunVersion:   1,
		WorkspaceID:         arg.WorkspaceID,
		Metadata:            arg.Metadata,
		Tags:                arg.Tags,
		TerminalReason:      []byte(`{}`),
		ExpiresAt:           arg.ExpiresAt,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	return f.taskSession, nil
}

func (f *fakeStore) CreateWorkspace(_ context.Context, arg db.CreateWorkspaceParams) (db.Workspace, error) {
	f.createWorkspaceCalls++
	now := testTime()
	f.workspace = db.Workspace{
		ID:                  arg.ID,
		OrgID:               arg.OrgID,
		ProjectID:           arg.ProjectID,
		EnvironmentID:       arg.EnvironmentID,
		DeploymentSandboxID: arg.DeploymentSandboxID,
		SandboxID:           arg.SandboxID,
		SandboxFingerprint:  arg.SandboxFingerprint,
		ExternalID:          arg.ExternalID,
		State:               db.WorkspaceStateActive,
		DesiredState:        db.WorkspaceDesiredStateActive,
		DirtyState:          db.WorkspaceDirtyStateClean,
		Metadata:            arg.Metadata,
		Tags:                arg.Tags,
		RetentionPolicy:     arg.RetentionPolicy,
		LastActivityAt:      now,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	return f.workspace, nil
}

func (f *fakeStore) CreateWorkspaceFromSandbox(_ context.Context, arg db.CreateWorkspaceFromSandboxParams) (db.CreateWorkspaceFromSandboxRow, error) {
	f.createWorkspaceCalls++
	now := testTime()
	f.workspace = db.Workspace{
		ID:                  arg.ID,
		OrgID:               arg.OrgID,
		ProjectID:           arg.ProjectID,
		EnvironmentID:       arg.EnvironmentID,
		DeploymentSandboxID: arg.DeploymentSandboxID,
		SandboxID:           "test-sandbox",
		SandboxFingerprint:  "test-sandbox-fingerprint",
		ExternalID:          arg.ExternalID,
		State:               db.WorkspaceStateActive,
		DesiredState:        db.WorkspaceDesiredStateActive,
		DirtyState:          db.WorkspaceDirtyStateClean,
		CurrentVersionID:    arg.InitialVersionID,
		Metadata:            arg.Metadata,
		Tags:                arg.Tags,
		RetentionPolicy:     arg.RetentionPolicy,
		LastActivityAt:      now,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	return db.CreateWorkspaceFromSandboxRow{
		ID:                    f.workspace.ID,
		OrgID:                 f.workspace.OrgID,
		ProjectID:             f.workspace.ProjectID,
		EnvironmentID:         f.workspace.EnvironmentID,
		DeploymentSandboxID:   f.workspace.DeploymentSandboxID,
		SandboxID:             f.workspace.SandboxID,
		SandboxFingerprint:    f.workspace.SandboxFingerprint,
		ExternalID:            f.workspace.ExternalID,
		CurrentVersionID:      f.workspace.CurrentVersionID,
		State:                 f.workspace.State,
		DesiredState:          f.workspace.DesiredState,
		DirtyState:            f.workspace.DirtyState,
		LastMaterializationID: f.workspace.LastMaterializationID,
		Metadata:              f.workspace.Metadata,
		Tags:                  f.workspace.Tags,
		RetentionPolicy:       f.workspace.RetentionPolicy,
		AutoStopAt:            f.workspace.AutoStopAt,
		AutoArchiveAt:         f.workspace.AutoArchiveAt,
		AutoDeleteAt:          f.workspace.AutoDeleteAt,
		LastActivityAt:        f.workspace.LastActivityAt,
		CreatedAt:             f.workspace.CreatedAt,
		UpdatedAt:             f.workspace.UpdatedAt,
		ArchivedAt:            f.workspace.ArchivedAt,
		DeletedAt:             f.workspace.DeletedAt,
	}, nil
}

func (f *fakeStore) ResolveDeploymentSandboxForWorkspaceCreate(_ context.Context, arg db.ResolveDeploymentSandboxForWorkspaceCreateParams) (db.DeploymentSandbox, error) {
	f.resolveDeploymentSandbox = arg
	f.resolveDeploymentSandboxCalls++
	if arg.SandboxID != "test-sandbox" {
		return db.DeploymentSandbox{}, pgx.ErrNoRows
	}
	return db.DeploymentSandbox{
		ID:            testDeploymentSandboxID(),
		OrgID:         arg.OrgID,
		ProjectID:     arg.ProjectID,
		EnvironmentID: arg.EnvironmentID,
		DeploymentID:  testDeploymentID(),
		SandboxID:     arg.SandboxID,
		Fingerprint:   "test-sandbox-fingerprint",
		CreatedAt:     testTime(),
	}, nil
}

func (f *fakeStore) GetWorkspace(_ context.Context, arg db.GetWorkspaceParams) (db.Workspace, error) {
	if f.workspace.ID.Valid &&
		f.workspace.OrgID == arg.OrgID &&
		f.workspace.ProjectID == arg.ProjectID &&
		f.workspace.EnvironmentID == arg.EnvironmentID &&
		f.workspace.ID == arg.ID {
		return f.workspace, nil
	}
	return db.Workspace{}, pgx.ErrNoRows
}

func (f *fakeStore) ListWorkspaces(_ context.Context, arg db.ListWorkspacesParams) ([]db.Workspace, error) {
	if f.workspace.ID.Valid &&
		f.workspace.OrgID == arg.OrgID &&
		f.workspace.ProjectID == arg.ProjectID &&
		f.workspace.EnvironmentID == arg.EnvironmentID {
		return []db.Workspace{f.workspace}, nil
	}
	return []db.Workspace{}, nil
}

func (f *fakeStore) GetWorkspaceOperationIdempotency(_ context.Context, arg db.GetWorkspaceOperationIdempotencyParams) (db.WorkspaceOperationIdempotency, error) {
	row := f.workspaceOperationIdempotency
	if row.ID.Valid &&
		row.OrgID == arg.OrgID &&
		row.ProjectID == arg.ProjectID &&
		row.EnvironmentID == arg.EnvironmentID &&
		!row.WorkspaceID.Valid &&
		row.OperationKind == arg.OperationKind &&
		row.IdempotencyKey == arg.IdempotencyKey {
		return row, nil
	}
	return db.WorkspaceOperationIdempotency{}, pgx.ErrNoRows
}

func (f *fakeStore) EnsureWorkspaceOperationIdempotency(_ context.Context, arg db.EnsureWorkspaceOperationIdempotencyParams) (db.EnsureWorkspaceOperationIdempotencyRow, error) {
	if f.workspaceOperationIdempotency.ID.Valid &&
		f.workspaceOperationIdempotency.OrgID == arg.OrgID &&
		f.workspaceOperationIdempotency.ProjectID == arg.ProjectID &&
		f.workspaceOperationIdempotency.EnvironmentID == arg.EnvironmentID &&
		!f.workspaceOperationIdempotency.WorkspaceID.Valid &&
		f.workspaceOperationIdempotency.OperationKind == arg.OperationKind &&
		f.workspaceOperationIdempotency.IdempotencyKey == arg.IdempotencyKey {
		row := f.workspaceOperationIdempotency
		return db.EnsureWorkspaceOperationIdempotencyRow{
			ID:                   row.ID,
			OrgID:                row.OrgID,
			ProjectID:            row.ProjectID,
			EnvironmentID:        row.EnvironmentID,
			WorkspaceID:          row.WorkspaceID,
			OperationKind:        row.OperationKind,
			IdempotencyKey:       row.IdempotencyKey,
			RequestFingerprint:   row.RequestFingerprint,
			ResponseResourceType: row.ResponseResourceType,
			ResponseResourceID:   row.ResponseResourceID,
			ResponseBody:         row.ResponseBody,
			ExpiresAt:            row.ExpiresAt,
			CreatedAt:            row.CreatedAt,
			LastUsedAt:           row.LastUsedAt,
			Inserted:             false,
		}, nil
	}
	f.createdWorkspaceOperationIdempotencies = append(f.createdWorkspaceOperationIdempotencies, arg)
	row := db.EnsureWorkspaceOperationIdempotencyRow{
		ID:                   arg.ID,
		OrgID:                arg.OrgID,
		ProjectID:            arg.ProjectID,
		EnvironmentID:        arg.EnvironmentID,
		WorkspaceID:          arg.WorkspaceID,
		OperationKind:        arg.OperationKind,
		IdempotencyKey:       arg.IdempotencyKey,
		RequestFingerprint:   arg.RequestFingerprint,
		ResponseResourceType: arg.ResponseResourceType,
		ResponseResourceID:   arg.ResponseResourceID,
		ResponseBody:         arg.ResponseBody,
		ExpiresAt:            arg.ExpiresAt,
		CreatedAt:            testTime(),
		LastUsedAt:           testTime(),
		Inserted:             true,
	}
	f.workspaceOperationIdempotency = db.WorkspaceOperationIdempotency{
		ID:                   row.ID,
		OrgID:                row.OrgID,
		ProjectID:            row.ProjectID,
		EnvironmentID:        row.EnvironmentID,
		WorkspaceID:          row.WorkspaceID,
		OperationKind:        row.OperationKind,
		IdempotencyKey:       row.IdempotencyKey,
		RequestFingerprint:   row.RequestFingerprint,
		ResponseResourceType: row.ResponseResourceType,
		ResponseResourceID:   row.ResponseResourceID,
		ResponseBody:         row.ResponseBody,
		ExpiresAt:            row.ExpiresAt,
		CreatedAt:            row.CreatedAt,
		LastUsedAt:           row.LastUsedAt,
	}
	return row, nil
}

func (f *fakeStore) CompleteWorkspaceOperationIdempotency(_ context.Context, arg db.CompleteWorkspaceOperationIdempotencyParams) (db.WorkspaceOperationIdempotency, error) {
	row := f.workspaceOperationIdempotency
	if row.ID.Valid &&
		row.OrgID == arg.OrgID &&
		row.ProjectID == arg.ProjectID &&
		row.EnvironmentID == arg.EnvironmentID &&
		!row.WorkspaceID.Valid &&
		row.OperationKind == arg.OperationKind &&
		row.IdempotencyKey == arg.IdempotencyKey &&
		row.RequestFingerprint == arg.RequestFingerprint &&
		!row.ResponseResourceID.Valid {
		row.ResponseResourceType = arg.ResponseResourceType
		row.ResponseResourceID = arg.ResponseResourceID
		row.ResponseBody = arg.ResponseBody
		row.LastUsedAt = testTime()
		f.workspaceOperationIdempotency = row
		return row, nil
	}
	return db.WorkspaceOperationIdempotency{}, pgx.ErrNoRows
}

func (f *fakeStore) GetWorkspaceForTaskStart(_ context.Context, arg db.GetWorkspaceForTaskStartParams) (db.GetWorkspaceForTaskStartRow, error) {
	if f.attachedWorkspace.ID.Valid &&
		f.attachedWorkspace.OrgID == arg.OrgID &&
		f.attachedWorkspace.ProjectID == arg.ProjectID &&
		f.attachedWorkspace.EnvironmentID == arg.EnvironmentID &&
		f.attachedWorkspace.ID == arg.WorkspaceID {
		return f.attachedWorkspace, nil
	}
	return db.GetWorkspaceForTaskStartRow{}, pgx.ErrNoRows
}

func (f *fakeStore) EnsureWorkspaceMaterializationRequested(_ context.Context, arg db.EnsureWorkspaceMaterializationRequestedParams) (db.EnsureWorkspaceMaterializationRequestedRow, error) {
	f.ensureWorkspaceMaterialization = arg
	f.ensureWorkspaceMaterializationCalls++
	if f.ensureWorkspaceMaterializationErr != nil {
		return db.EnsureWorkspaceMaterializationRequestedRow{}, f.ensureWorkspaceMaterializationErr
	}
	state := f.ensureWorkspaceMaterializationState
	if state == "" {
		state = db.WorkspaceMaterializationStateRequested
	}
	now := testTime()
	return db.EnsureWorkspaceMaterializationRequestedRow{
		ID:                  arg.ID,
		OrgID:               arg.OrgID,
		ProjectID:           arg.ProjectID,
		EnvironmentID:       arg.EnvironmentID,
		WorkspaceID:         arg.WorkspaceID,
		DeploymentSandboxID: testDeploymentSandboxID(),
		Priority:            arg.Priority,
		State:               state,
		Request:             arg.Request,
		RequestedAt:         now,
		CreatedAt:           now,
		UpdatedAt:           now,
		Inserted:            f.ensureWorkspaceMaterializationInserted,
	}, nil
}

func (f *fakeStore) SetQueuedRunWorkspaceMaterialization(_ context.Context, arg db.SetQueuedRunWorkspaceMaterializationParams) error {
	f.setQueuedRunWorkspaceMaterialization = arg
	f.setQueuedRunWorkspaceMaterializationCalls++
	f.run.WorkspaceMaterializationID = arg.WorkspaceMaterializationID
	return nil
}

func (f *fakeStore) SetTaskSessionCurrentRun(_ context.Context, arg db.SetTaskSessionCurrentRunParams) (db.TaskSession, error) {
	f.taskSession.CurrentRunID = arg.RunID
	f.taskSession.CurrentRunVersion++
	f.taskSession.UpdatedAt = testTime()
	return f.taskSession, nil
}

func (f *fakeStore) CreateTaskSessionRun(_ context.Context, arg db.CreateTaskSessionRunParams) (db.TaskSessionRun, error) {
	row := db.TaskSessionRun{
		ID:            arg.ID,
		OrgID:         arg.OrgID,
		ProjectID:     arg.ProjectID,
		EnvironmentID: arg.EnvironmentID,
		TaskSessionID: arg.TaskSessionID,
		RunID:         arg.RunID,
		DeploymentID:  arg.DeploymentID,
		PreviousRunID: arg.PreviousRunID,
		TurnIndex:     arg.TurnIndex,
		CreatedAt:     testTime(),
	}
	f.taskSessionRuns = append(f.taskSessionRuns, row)
	return row, nil
}

func (f *fakeStore) GetTaskStartIdempotency(_ context.Context, arg db.GetTaskStartIdempotencyParams) (db.GetTaskStartIdempotencyRow, error) {
	if f.startIdempotency.ID.Valid &&
		f.startIdempotency.OrgID == arg.OrgID &&
		f.startIdempotency.ProjectID == arg.ProjectID &&
		f.startIdempotency.EnvironmentID == arg.EnvironmentID &&
		f.startIdempotency.TaskID == arg.TaskID &&
		f.startIdempotency.IdempotencyKey == arg.IdempotencyKey &&
		(!f.startIdempotency.ExpiresAt.Valid || f.startIdempotency.ExpiresAt.Time.After(time.Now())) {
		return f.startIdempotency, nil
	}
	return db.GetTaskStartIdempotencyRow{}, pgx.ErrNoRows
}

func (f *fakeStore) CreateTaskStartIdempotency(_ context.Context, arg db.CreateTaskStartIdempotencyParams) (db.TaskStartIdempotency, error) {
	f.startIdempotency = db.GetTaskStartIdempotencyRow{
		ID:                         arg.ID,
		OrgID:                      arg.OrgID,
		ProjectID:                  arg.ProjectID,
		EnvironmentID:              arg.EnvironmentID,
		TaskID:                     arg.TaskID,
		IdempotencyKey:             arg.IdempotencyKey,
		RequestFingerprint:         arg.RequestFingerprint,
		TaskSessionID:              arg.TaskSessionID,
		FirstRunID:                 arg.FirstRunID,
		ExpiresAt:                  arg.ExpiresAt,
		CreatedAt:                  testTime(),
		LastUsedAt:                 testTime(),
		SessionID:                  f.taskSession.ID,
		SessionOrgID:               f.taskSession.OrgID,
		SessionProjectID:           f.taskSession.ProjectID,
		SessionEnvironmentID:       f.taskSession.EnvironmentID,
		SessionTaskID:              f.taskSession.TaskID,
		SessionInitialDeploymentID: f.taskSession.InitialDeploymentID,
		SessionActiveDeploymentID:  f.taskSession.ActiveDeploymentID,
		SessionExternalID:          f.taskSession.ExternalID,
		SessionStartFingerprint:    f.taskSession.StartFingerprint,
		SessionStatus:              f.taskSession.Status,
		SessionCurrentRunID:        f.taskSession.CurrentRunID,
		SessionCurrentRunVersion:   f.taskSession.CurrentRunVersion,
		SessionWorkspaceID:         f.taskSession.WorkspaceID,
		SessionMetadata:            f.taskSession.Metadata,
		SessionTags:                f.taskSession.Tags,
		SessionResult:              f.taskSession.Result,
		SessionTerminalReason:      f.taskSession.TerminalReason,
		SessionExpiresAt:           f.taskSession.ExpiresAt,
		SessionCompletedAt:         f.taskSession.CompletedAt,
		SessionFailedAt:            f.taskSession.FailedAt,
		SessionCancelledAt:         f.taskSession.CancelledAt,
		SessionCreatedAt:           f.taskSession.CreatedAt,
		SessionUpdatedAt:           f.taskSession.UpdatedAt,
		RunID:                      f.run.ID,
		RunOrgID:                   f.run.OrgID,
		RunProjectID:               f.run.ProjectID,
		RunEnvironmentID:           f.run.EnvironmentID,
		RunDeploymentID:            f.run.DeploymentID,
		RunDeploymentTaskID:        f.run.DeploymentTaskID,
		RunDeploymentVersion:       f.run.DeploymentVersion,
		RunApiVersion:              f.run.ApiVersion,
		RunSdkVersion:              f.run.SdkVersion,
		RunCliVersion:              f.run.CliVersion,
		RunTaskID:                  f.run.TaskID,
		RunAttemptNumber:           f.run.CurrentAttemptNumber,
		RunStatus:                  f.run.Status,
		RunExecutionStatus:         f.run.ExecutionStatus,
		RunTerminalOutcome:         f.run.TerminalOutcome,
		RunOutput:                  f.run.Output,
		RunMetadata:                f.run.Metadata,
		RunTags:                    f.run.Tags,
		RunExitCode:                f.run.ExitCode,
		RunCreatedAt:               f.run.CreatedAt,
		RunUpdatedAt:               f.run.UpdatedAt,
	}
	return db.TaskStartIdempotency{
		ID:                 arg.ID,
		OrgID:              arg.OrgID,
		ProjectID:          arg.ProjectID,
		EnvironmentID:      arg.EnvironmentID,
		TaskID:             arg.TaskID,
		IdempotencyKey:     arg.IdempotencyKey,
		RequestFingerprint: arg.RequestFingerprint,
		TaskSessionID:      arg.TaskSessionID,
		FirstRunID:         arg.FirstRunID,
		ExpiresAt:          arg.ExpiresAt,
		CreatedAt:          testTime(),
		LastUsedAt:         testTime(),
	}, nil
}

func (f *fakeStore) DeleteExpiredTaskStartIdempotency(_ context.Context, arg db.DeleteExpiredTaskStartIdempotencyParams) error {
	if f.startIdempotency.ID.Valid &&
		f.startIdempotency.OrgID == arg.OrgID &&
		f.startIdempotency.ProjectID == arg.ProjectID &&
		f.startIdempotency.EnvironmentID == arg.EnvironmentID &&
		f.startIdempotency.TaskID == arg.TaskID &&
		f.startIdempotency.IdempotencyKey == arg.IdempotencyKey &&
		f.startIdempotency.ExpiresAt.Valid &&
		!f.startIdempotency.ExpiresAt.Time.After(time.Now()) {
		f.startIdempotency = db.GetTaskStartIdempotencyRow{}
	}
	return nil
}

func (f *fakeStore) TouchTaskStartIdempotency(context.Context, db.TouchTaskStartIdempotencyParams) error {
	return nil
}

func (f *fakeStore) GetTaskSession(_ context.Context, arg db.GetTaskSessionParams) (db.TaskSession, error) {
	if f.taskSession.ID.Valid &&
		f.taskSession.OrgID == arg.OrgID &&
		f.taskSession.ProjectID == arg.ProjectID &&
		f.taskSession.EnvironmentID == arg.EnvironmentID &&
		f.taskSession.ID == arg.ID {
		return f.taskSession, nil
	}
	return db.TaskSession{}, pgx.ErrNoRows
}

func (f *fakeStore) LockTaskSession(_ context.Context, arg db.LockTaskSessionParams) (db.TaskSession, error) {
	session := f.taskSession
	if f.lockTaskSession.ID.Valid {
		session = f.lockTaskSession
	}
	if session.ID.Valid &&
		session.OrgID == arg.OrgID &&
		session.ProjectID == arg.ProjectID &&
		session.EnvironmentID == arg.EnvironmentID &&
		session.ID == arg.ID {
		return session, nil
	}
	return db.TaskSession{}, pgx.ErrNoRows
}

func (f *fakeStore) GetTaskSessionByOrgID(_ context.Context, arg db.GetTaskSessionByOrgIDParams) (db.TaskSession, error) {
	if f.taskSession.ID.Valid && f.taskSession.OrgID == arg.OrgID && f.taskSession.ID == arg.ID {
		return f.taskSession, nil
	}
	return db.TaskSession{}, pgx.ErrNoRows
}

func (f *fakeStore) PatchTaskSession(_ context.Context, arg db.PatchTaskSessionParams) (db.TaskSession, error) {
	if f.taskSession.ID.Valid &&
		f.taskSession.OrgID == arg.OrgID &&
		f.taskSession.ProjectID == arg.ProjectID &&
		f.taskSession.EnvironmentID == arg.EnvironmentID &&
		f.taskSession.ID == arg.ID &&
		f.taskSession.Status == db.TaskSessionStatusOpen {
		if arg.Metadata != nil {
			f.taskSession.Metadata = arg.Metadata
		}
		if arg.Tags != nil {
			f.taskSession.Tags = arg.Tags
		}
		if arg.ExpiresAt.Valid && f.taskSession.ExpiresAt.Valid && arg.ExpiresAt.Time.After(f.taskSession.ExpiresAt.Time) {
			f.taskSession.ExpiresAt = arg.ExpiresAt
		}
		f.taskSession.UpdatedAt = testTime()
		return f.taskSession, nil
	}
	return db.TaskSession{}, pgx.ErrNoRows
}

func (f *fakeStore) GetTaskSessionByExternalID(_ context.Context, arg db.GetTaskSessionByExternalIDParams) (db.TaskSession, error) {
	if f.getTaskSessionByExternalIDMisses > 0 {
		f.getTaskSessionByExternalIDMisses--
		return db.TaskSession{}, pgx.ErrNoRows
	}
	if f.taskSession.ID.Valid &&
		f.taskSession.OrgID == arg.OrgID &&
		f.taskSession.ProjectID == arg.ProjectID &&
		f.taskSession.EnvironmentID == arg.EnvironmentID &&
		f.taskSession.TaskID == arg.TaskID &&
		f.taskSession.ExternalID == arg.ExternalID {
		return f.taskSession, nil
	}
	return db.TaskSession{}, pgx.ErrNoRows
}

func (f *fakeStore) CancelTaskSession(_ context.Context, arg db.CancelTaskSessionParams) (db.TaskSession, error) {
	if f.taskSession.ID.Valid &&
		f.taskSession.OrgID == arg.OrgID &&
		f.taskSession.ProjectID == arg.ProjectID &&
		f.taskSession.EnvironmentID == arg.EnvironmentID &&
		f.taskSession.ID == arg.ID &&
		f.taskSession.Status == db.TaskSessionStatusOpen {
		f.taskSession.Status = db.TaskSessionStatusCancelled
		f.taskSession.CancelledAt = testTime()
		f.taskSession.TerminalReason = fmt.Appendf(nil, `{"origin":"api","reason":%q}`, arg.Reason)
		f.taskSession.Result = fmt.Appendf(nil, `{"ok":false,"error":{"name":"TaskCancelled","message":%q,"details":{"origin":"api"}}}`, arg.Reason)
		f.taskSession.CurrentRunID = pgtype.UUID{}
		f.taskSession.CurrentRunVersion++
		f.taskSession.UpdatedAt = testTime()
		return f.taskSession, nil
	}
	return db.TaskSession{}, pgx.ErrNoRows
}

func (f *fakeStore) CloseTaskSession(_ context.Context, arg db.CloseTaskSessionParams) (db.TaskSession, error) {
	if f.closeTaskSessionAttachesRun.Valid {
		f.taskSession.CurrentRunID = f.closeTaskSessionAttachesRun
		f.closeTaskSessionAttachesRun = pgtype.UUID{}
		return db.TaskSession{}, pgx.ErrNoRows
	}
	if f.taskSession.ID.Valid &&
		f.taskSession.OrgID == arg.OrgID &&
		f.taskSession.ProjectID == arg.ProjectID &&
		f.taskSession.EnvironmentID == arg.EnvironmentID &&
		f.taskSession.ID == arg.ID &&
		f.taskSession.Status == db.TaskSessionStatusOpen &&
		!f.taskSession.CurrentRunID.Valid {
		f.taskSession.Status = db.TaskSessionStatusClosed
		f.taskSession.ClosedAt = testTime()
		f.taskSession.ClosedReason = arg.Reason
		f.taskSession.TerminalReason = fmt.Appendf(nil, `{"origin":"api","reason":%q}`, arg.Reason)
		f.taskSession.UpdatedAt = testTime()
		return f.taskSession, nil
	}
	return db.TaskSession{}, pgx.ErrNoRows
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

func (f *fakeStore) GetRunLeaseRuntimeRelease(_ context.Context, arg db.GetRunLeaseRuntimeReleaseParams) (db.GetRunLeaseRuntimeReleaseRow, error) {
	if f.activeQueueLeaseMissing {
		return db.GetRunLeaseRuntimeReleaseRow{}, pgx.ErrNoRows
	}
	if f.run.ID != arg.RunID || f.sessionID != arg.RunLeaseID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.GetRunLeaseRuntimeReleaseRow{}, pgx.ErrNoRows
	}
	capabilities := testWorkerCapabilities()
	return db.GetRunLeaseRuntimeReleaseRow{
		RuntimeID:       capabilities.RuntimeID,
		RuntimeArch:     capabilities.RuntimeArch,
		RuntimeABI:      capabilities.RuntimeABI,
		KernelDigest:    capabilities.KernelDigest,
		InitramfsDigest: capabilities.InitramfsDigest,
		RootfsDigest:    capabilities.RootfsDigest,
		CniProfile:      capabilities.CNIProfile,
	}, nil
}

func (f *fakeStore) RunLeaseDispatchAttemptsExhausted(context.Context, db.RunLeaseDispatchAttemptsExhaustedParams) (bool, error) {
	return false, nil
}

func (f *fakeStore) FailExpiredRunningRunLeases(context.Context, pgtype.UUID) error {
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
			auth.PermissionSessionStreamsRead,
			auth.PermissionSessionInputSend,
			auth.PermissionSessionOutputAppend,
			auth.PermissionTokensCreate,
			auth.PermissionTokensRead,
			auth.PermissionTokensComplete,
			auth.PermissionTokensCancel,
			auth.PermissionWorkspaceLifecycleManage,
			auth.PermissionFilesRead,
			auth.PermissionFilesWrite,
			auth.PermissionVersionsRead,
			auth.PermissionVersionsCapture,
			auth.PermissionVersionsRestore,
			auth.PermissionVersionsDiff,
			auth.PermissionExecCreate,
			auth.PermissionExecRead,
			auth.PermissionExecManage,
			auth.PermissionPtyCreate,
			auth.PermissionPtyRead,
			auth.PermissionPtyManage,
			auth.PermissionPortsExpose,
			auth.PermissionPortsRead,
			auth.PermissionPortsClose,
			auth.PermissionSecretsWrite,
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

var _ auth.Authenticator = fakeAuth{}
