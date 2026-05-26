package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ghapi "github.com/google/go-github/v75/github"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/ghapp"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const testGitSHA = "0123456789abcdef0123456789abcdef01234567"
const testWorkerTokenSecret = "01234567890123456789012345678901"
const testWorkerInstanceCredentialID = "00000000-0000-0000-0000-00000000c001"

func testProjectID() pgtype.UUID {
	return ids.ToPG(uuid.MustParse("00000000-0000-0000-0000-000000000301"))
}

func testEnvironmentID() pgtype.UUID {
	return ids.ToPG(uuid.MustParse("00000000-0000-0000-0000-000000000302"))
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

func testWorkerRunLeaseRequestBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(api.WorkerRunLeaseRequest{Capabilities: testWorkerCapabilities()})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func testWorkerCapabilities() api.WorkerCapabilities {
	return api.WorkerCapabilities{
		RuntimeArch:             "arm64",
		RuntimeABI:              "helmr.firecracker.snapshot.v0",
		KernelDigest:            "sha256:kernel",
		RootfsDigest:            "sha256:rootfs",
		CNIProfile:              "helmr/v1",
		MaxVCPUs:                2,
		MaxMemoryMiB:            2048,
		MaxDiskMiB:              20480,
		ExecutionSlotsAvailable: 1,
	}
}

func TestCreateGetAndListRun(t *testing.T) {
	store := &fakeStore{}
	runEnqueuer := &fakeRunEnqueuer{}
	resolver := fakeGitHubResolver{refs: map[string]string{"main": testGitSHA}}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithGitHubResolver(resolver), WithSecrets(fakeSecrets{}), WithRunEnqueuer(runEnqueuer))

	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:    "deploy",
		Payload:   json.RawMessage(`{"env":"prod"}`),
		Secrets:   api.SecretBindings{"API_KEY": "vault:api-key"},
		Workspace: api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: "main"},
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
	if store.createRun.WorkspaceRepository != "helmrdotdev/helmr" || store.createRun.WorkspaceRef != "main" || store.createRun.WorkspaceSha != testGitSHA {
		t.Fatalf("stored workspace = %+v", store.createRun)
	}
	if store.createRun.WorkspaceInstallationID != 123 {
		t.Fatalf("stored installation = %d", store.createRun.WorkspaceInstallationID)
	}
	if string(store.createRun.Payload) != `{"env":"prod"}` {
		t.Fatalf("payload = %s", store.createRun.Payload)
	}
	if string(store.createRun.SecretBindings) != `{"API_KEY":"vault:api-key"}` {
		t.Fatalf("secrets = %s", store.createRun.SecretBindings)
	}
	if store.createRun.MaxDurationSeconds != 300 {
		t.Fatalf("max duration = %d", store.createRun.MaxDurationSeconds)
	}
	if store.runEvent.Kind != "run.created" {
		t.Fatalf("run event kind = %s", store.runEvent.Kind)
	}
	var eventPayload struct {
		TaskID             string           `json:"task_id"`
		Payload            json.RawMessage  `json:"payload"`
		Workspace          api.GitHubSource `json:"workspace"`
		MaxDurationSeconds int32            `json:"max_duration_seconds"`
		SecretNames        []string         `json:"secret_names"`
	}
	if err := json.Unmarshal(store.runEvent.Payload, &eventPayload); err != nil {
		t.Fatalf("run event payload decode: %v", err)
	}
	if eventPayload.TaskID != "deploy" || string(eventPayload.Payload) != `{"env":"prod"}` || eventPayload.Workspace.SHA != testGitSHA || eventPayload.MaxDurationSeconds != 300 {
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

func TestCreateRunWithSecretsRequiresOwner(t *testing.T) {
	store := &fakeStore{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{role: auth.RoleDeveloper}),
		WithGitHubResolver(fakeGitHubResolver{refs: map[string]string{"main": testGitSHA}}),
		WithSecrets(fakeSecrets{}),
	)
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:    "deploy",
		Secrets:   api.SecretBindings{"API_KEY": "vault:api-key"},
		Workspace: api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer developer-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.createRun.ID.Valid {
		t.Fatalf("run was created: %+v", store.createRun)
	}
}

func TestCreateRunWithoutSecretsAllowsDeveloper(t *testing.T) {
	store := &fakeStore{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{role: auth.RoleDeveloper}),
		WithGitHubResolver(fakeGitHubResolver{refs: map[string]string{"main": testGitSHA}}),
		WithSecrets(fakeSecrets{}),
	)
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:    "deploy",
		Workspace: api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: "main"},
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
		WithGitHubResolver(fakeGitHubResolver{refs: map[string]string{"main": testGitSHA}}),
	)
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:    "deploy",
		Workspace: api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: "main"},
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
		WithGitHubResolver(fakeGitHubResolver{refs: map[string]string{"main": testGitSHA}}),
	)
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:    "deploy",
		Workspace: api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: "main"},
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
		WithGitHubResolver(fakeGitHubResolver{refs: map[string]string{"main": testGitSHA}}),
	)
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:    "deploy",
		Workspace: api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: "main"},
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
		WithGitHubResolver(fakeGitHubResolver{refs: map[string]string{"main": testGitSHA}}),
	)
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:    "deploy",
		Workspace: api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: "main"},
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
		WithGitHubResolver(fakeGitHubResolver{refs: map[string]string{"main": testGitSHA}}),
	)
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:        "deploy",
		ProjectID:     auth.DefaultProjectID,
		EnvironmentID: auth.DefaultEnvironmentID,
		Workspace:     api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: "main"},
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

func TestAPIKeyRunCreateWithSecretsRequiresSeparatePermission(t *testing.T) {
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
		WithGitHubResolver(fakeGitHubResolver{refs: map[string]string{"main": testGitSHA}}),
		WithSecrets(fakeSecrets{}),
	)
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:    "deploy",
		Secrets:   api.SecretBindings{"API_KEY": "vault:api-key"},
		Workspace: api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer machine-key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.createRun.ID.Valid {
		t.Fatalf("run was created: %+v", store.createRun)
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

func TestCreateRunRejectsLocalSecretBindingSchemes(t *testing.T) {
	for _, binding := range []string{"env:API_KEY", "file:/tmp/api-key"} {
		store := &fakeStore{}
		resolver := fakeGitHubResolver{refs: map[string]string{"main": testGitSHA}}
		server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithGitHubResolver(resolver), WithSecrets(fakeSecrets{}))

		bodyBytes, err := json.Marshal(api.CreateRunRequest{
			TaskID:             "deploy",
			Payload:            json.RawMessage(`{}`),
			Secrets:            api.SecretBindings{"API_KEY": binding},
			Workspace:          api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: "main"},
			MaxDurationSeconds: 300,
		})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
		req.Header.Set("authorization", "Bearer test-key")
		rec := httptest.NewRecorder()

		server.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("binding %q status = %d body=%s", binding, rec.Code, rec.Body.String())
		}
		if store.createRun.ID.Valid {
			t.Fatalf("binding %q created run", binding)
		}
	}
}

func TestCreateRunRejectsInvalidTaskID(t *testing.T) {
	store := &fakeStore{}
	resolver := fakeGitHubResolver{refs: map[string]string{testGitSHA: testGitSHA}}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithGitHubResolver(resolver))

	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:    "bad task",
		Workspace: api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: testGitSHA},
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
	resolver := fakeGitHubResolver{refs: map[string]string{"main": testGitSHA}}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithGitHubResolver(resolver), WithSecrets(fakeSecrets{}))
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
			server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithGitHubResolver(fakeGitHubResolver{}))

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
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithGitHubResolver(fakeGitHubResolver{}))

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
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithGitHubResolver(fakeGitHubResolver{}))

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
			Manifest: api.WorkerCheckpointArtifact{Digest: manifestDigest, SizeBytes: 64, MediaType: cas.CheckpointManifestMediaType},
			VMState:  api.WorkerCheckpointArtifact{Digest: stateDigest, SizeBytes: 128, MediaType: cas.CheckpointVMStateMediaType},
			Memory:   []api.WorkerCheckpointArtifact{{Digest: memoryDigest, SizeBytes: 256, MediaType: cas.CheckpointMemoryMediaType}},
		},
		Workspace: api.WorkerCheckpointWorkspace{
			Scratch: &api.WorkerCheckpointArtifact{Digest: scratchDigest, SizeBytes: 512, MediaType: cas.CheckpointScratchDiskMediaType},
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
				m.RuntimeState.VMState = api.WorkerCheckpointArtifact{}
			}),
			want: "manifest.runtime_state.vm_state.digest",
		},
		{
			name: "wrong memory media type",
			manifest: withCheckpointManifest(valid, func(m *api.WorkerCheckpointManifest) {
				m.RuntimeState.Memory[0].MediaType = cas.CheckpointVMStateMediaType
			}),
			want: "expected",
		},
		{
			name: "wrong manifest media type",
			manifest: withCheckpointManifest(valid, func(m *api.WorkerCheckpointManifest) {
				m.RuntimeState.Manifest.MediaType = cas.CheckpointMemoryMediaType
			}),
			want: "expected",
		},
		{
			name: "conflicting duplicate metadata",
			manifest: withCheckpointManifest(valid, func(m *api.WorkerCheckpointManifest) {
				m.RuntimeState.Memory[0].MediaType = cas.CheckpointMemoryMediaType
				m.RuntimeState.Memory = append(m.RuntimeState.Memory, api.WorkerCheckpointArtifact{Digest: memoryDigest, SizeBytes: 257, MediaType: cas.CheckpointMemoryMediaType})
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

func withCheckpointManifest(manifest api.WorkerCheckpointManifest, edit func(*api.WorkerCheckpointManifest)) api.WorkerCheckpointManifest {
	manifest.RuntimeState.Memory = append([]api.WorkerCheckpointArtifact(nil), manifest.RuntimeState.Memory...)
	if manifest.Workspace.Scratch != nil {
		scratch := *manifest.Workspace.Scratch
		manifest.Workspace.Scratch = &scratch
	}
	edit(&manifest)
	return manifest
}

func TestCreateRunRejectsInvalidGitHubSource(t *testing.T) {
	store := &fakeStore{}
	resolver := fakeGitHubResolver{refs: map[string]string{testGitSHA: testGitSHA}}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithGitHubResolver(resolver))

	tests := map[string]api.CreateRunRequest{
		"missing source": {
			TaskID: "deploy",
		},
		"client sha": {
			TaskID:    "deploy",
			Workspace: api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: testGitSHA, SHA: testGitSHA},
		},
		"absolute subpath": {
			TaskID:    "deploy",
			Workspace: api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: testGitSHA, Subpath: "/app"},
		},
		"escaping subpath": {
			TaskID:    "deploy",
			Workspace: api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: testGitSHA, Subpath: "../app"},
		},
		"bad duration": {
			TaskID:             "deploy",
			Workspace:          api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: testGitSHA},
			MaxDurationSeconds: 1,
		},
		"too long duration": {
			TaskID:             "deploy",
			Workspace:          api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: testGitSHA},
			MaxDurationSeconds: 86401,
		},
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			bodyBytes, err := json.Marshal(body)
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
		})
	}
}

func TestCreateRunRejectsClientSuppliedBundle(t *testing.T) {
	store := &fakeStore{}
	resolver := fakeGitHubResolver{refs: map[string]string{testGitSHA: testGitSHA}}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithGitHubResolver(resolver))

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

func TestCreateRunRejectsUnavailableSecretBinding(t *testing.T) {
	store := &fakeStore{}
	resolver := fakeGitHubResolver{refs: map[string]string{testGitSHA: testGitSHA}}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithGitHubResolver(resolver),
		WithSecrets(fakeSecrets{values: api.ResolvedSecrets{"other": []byte("secret")}}),
	)
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:    "deploy",
		Secrets:   api.SecretBindings{"API_KEY": "vault:missing"},
		Workspace: api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: testGitSHA},
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

func TestCreateRunReturnsBadGatewayWhenGitHubResolutionFails(t *testing.T) {
	store := &fakeStore{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithGitHubResolver(fakeGitHubResolver{err: errors.New("github unavailable")}),
	)
	bodyBytes, err := json.Marshal(api.CreateRunRequest{
		TaskID:    "deploy",
		Workspace: api.RunWorkspace{Repository: "helmrdotdev/helmr", Ref: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunRoutesRequireBearerAuth(t *testing.T) {
	store := &fakeStore{}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithAuthenticator(fakeAuth{}), WithGitHubResolver(fakeGitHubResolver{}))

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

type fakeGitHubResolver struct {
	refs              map[string]string
	err               error
	checkoutToken     string
	checkoutExpiresAt time.Time
}

func (f fakeGitHubResolver) ResolveCommit(_ context.Context, installationID int64, githubRepositoryID int64, source api.GitHubSource) (ghapp.ResolvedSource, error) {
	if f.err != nil {
		return ghapp.ResolvedSource{}, f.err
	}
	if installationID != 123 || githubRepositoryID != 456 {
		return ghapp.ResolvedSource{}, errors.New("unexpected github repository source")
	}
	normalized, err := ghapp.NormalizeSource(source)
	if err != nil {
		return ghapp.ResolvedSource{}, err
	}
	sha, ok := f.refs[normalized.Ref]
	if !ok {
		return ghapp.ResolvedSource{}, ghapp.InvalidSourceError{Err: errors.New("source.ref does not resolve to a commit")}
	}
	normalized.SHA = sha
	normalized.RefKind = api.GitHubRefKindBranch
	normalized.RefName = normalized.Ref
	normalized.FullRef = "refs/heads/" + normalized.Ref
	normalized.DefaultBranch = "main"
	return ghapp.ResolvedSource{Source: normalized, InstallationID: 123, GitHubRepositoryID: 456}, nil
}

func (f fakeGitHubResolver) CreateRepositoryToken(_ context.Context, installationID int64, githubRepositoryID int64) (ghapp.InstallationToken, error) {
	if f.err != nil {
		return ghapp.InstallationToken{}, f.err
	}
	if installationID != 123 || githubRepositoryID != 456 {
		return ghapp.InstallationToken{}, errors.New("unexpected workspace checkout token request")
	}
	token := f.checkoutToken
	if token == "" {
		token = "checkout-token"
	}
	expiresAt := f.checkoutExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Date(2026, 5, 8, 12, 30, 0, 0, time.UTC)
	}
	return ghapp.InstallationToken{Token: token, ExpiresAt: expiresAt}, nil
}

func TestWorkerRunLeaseStartAndRelease(t *testing.T) {
	store := &fakeStore{
		run: db.Run{
			ID:                          ids.ToPG(ids.New()),
			OrgID:                       ids.ToPG(ids.DefaultOrgID),
			ProjectID:                   testProjectID(),
			EnvironmentID:               testEnvironmentID(),
			DeploymentID:                testDeploymentID(),
			DeploymentTaskID:            testDeploymentTaskID(),
			TaskID:                      "deploy",
			Status:                      db.RunStatusQueued,
			Payload:                     []byte(`{"env":"prod"}`),
			SecretBindings:              []byte(`{"API_KEY":"vault:api-key"}`),
			WorkspaceRepository:         "helmrdotdev/helmr",
			WorkspaceInstallationID:     123,
			WorkspaceGithubRepositoryID: 456,
			WorkspaceRef:                testGitSHA,
			WorkspaceSha:                testGitSHA,
			WorkspaceSubpath:            "packages/console",
			MaxDurationSeconds:          3600,
			CreatedAt:                   testTime(),
			UpdatedAt:                   testTime(),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithGitHubResolver(fakeGitHubResolver{}),
		WithSecrets(fakeSecrets{values: api.ResolvedSecrets{"api-key": []byte("secret-value")}}),
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
	if store.dequeueRequest.Runtime.Arch != capabilities.RuntimeArch ||
		store.dequeueRequest.Runtime.ABI != capabilities.RuntimeABI ||
		store.dequeueRequest.Runtime.KernelDigest != capabilities.KernelDigest ||
		store.dequeueRequest.Runtime.RootfsDigest != capabilities.RootfsDigest ||
		store.dequeueRequest.Runtime.CNIProfile != capabilities.CNIProfile ||
		store.dequeueRequest.Region != capabilities.Region ||
		store.dequeueRequest.Labels["pool"] != "snapshot" ||
		store.dequeueRequest.Labels["dedicated_key"] != "tenant-a" {
		t.Fatalf("dequeue request = %+v", store.dequeueRequest)
	}
	if store.dequeueRequest.QueueName != dispatch.QueueNameForRuntime("queue-a", compute.RuntimeSelector{
		Arch:         capabilities.RuntimeArch,
		ABI:          capabilities.RuntimeABI,
		KernelDigest: capabilities.KernelDigest,
		RootfsDigest: capabilities.RootfsDigest,
		CNIProfile:   capabilities.CNIProfile,
	}) {
		t.Fatalf("dequeue queue name = %q", store.dequeueRequest.QueueName)
	}
	if claimResponse.Run.Workspace.Repository != "helmrdotdev/helmr" || claimResponse.Run.Workspace.SHA != testGitSHA {
		t.Fatalf("worker workspace = %+v", claimResponse.Run.Workspace)
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
			ID:                          ids.ToPG(ids.New()),
			OrgID:                       ids.ToPG(ids.DefaultOrgID),
			TaskID:                      "deploy",
			Status:                      db.RunStatusQueued,
			Payload:                     []byte(`{}`),
			SecretBindings:              []byte(`{}`),
			WorkspaceRepository:         "helmrdotdev/helmr",
			WorkspaceInstallationID:     123,
			WorkspaceGithubRepositoryID: 456,
			WorkspaceRef:                testGitSHA,
			WorkspaceSha:                testGitSHA,
			MaxDurationSeconds:          3600,
			CreatedAt:                   testTime(),
			UpdatedAt:                   testTime(),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithGitHubResolver(fakeGitHubResolver{}),
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
			SecretBindings:   []byte(`{}`),
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

func TestWorkerRunLeaseAbandonsClaimWhenRunPayloadFails(t *testing.T) {
	store := &fakeStore{
		run: db.Run{
			ID:                          ids.ToPG(ids.New()),
			OrgID:                       ids.ToPG(ids.DefaultOrgID),
			TaskID:                      "deploy",
			Status:                      db.RunStatusQueued,
			Payload:                     []byte(`{}`),
			SecretBindings:              []byte(`{}`),
			WorkspaceRepository:         "helmrdotdev/helmr",
			WorkspaceInstallationID:     123,
			WorkspaceGithubRepositoryID: 456,
			WorkspaceRef:                testGitSHA,
			WorkspaceSha:                testGitSHA,
			MaxDurationSeconds:          3600,
			CreatedAt:                   testTime(),
			UpdatedAt:                   testTime(),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithGitHubResolver(fakeGitHubResolver{err: errors.New("github unavailable")}),
		WithWorkerAuth("01234567890123456789012345678901", time.Hour),
	)
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000401")

	req := httptest.NewRequest(http.MethodPost, "/api/worker/executions/lease", bytes.NewReader(testWorkerRunLeaseRequestBody(t)))
	req.Header.Set("authorization", "Bearer "+workerBearer)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("claim status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !store.abandonedClaim || store.run.Status != db.RunStatusQueued || store.run.CurrentExecutionID.Valid {
		t.Fatalf("abandoned=%v run=%+v", store.abandonedClaim, store.run)
	}
}

func TestWorkerRunLeaseFailsRunWhenCheckoutSourceUnavailable(t *testing.T) {
	store := &fakeStore{
		run: db.Run{
			ID:                          ids.ToPG(ids.New()),
			OrgID:                       ids.ToPG(ids.DefaultOrgID),
			TaskID:                      "deploy",
			Status:                      db.RunStatusQueued,
			Payload:                     []byte(`{}`),
			SecretBindings:              []byte(`{}`),
			WorkspaceRepository:         "helmrdotdev/helmr",
			WorkspaceInstallationID:     123,
			WorkspaceGithubRepositoryID: 456,
			WorkspaceRef:                testGitSHA,
			WorkspaceSha:                testGitSHA,
			MaxDurationSeconds:          3600,
			CreatedAt:                   testTime(),
			UpdatedAt:                   testTime(),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithGitHubResolver(fakeGitHubResolver{err: &ghapi.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}}),
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
	if claimResponse.Lease != nil || claimResponse.Run != nil {
		t.Fatalf("claim response = %+v", claimResponse)
	}
	assertTerminalPayloadFailure(t, store, "workspace_unavailable")
}

func TestWorkerRunLeaseFailsRunWhenWorkspaceDisconnected(t *testing.T) {
	store := &fakeStore{
		githubSourceUnavailable: true,
		run: db.Run{
			ID:                          ids.ToPG(ids.New()),
			OrgID:                       ids.ToPG(ids.DefaultOrgID),
			TaskID:                      "deploy",
			Status:                      db.RunStatusQueued,
			Payload:                     []byte(`{}`),
			SecretBindings:              []byte(`{}`),
			WorkspaceRepository:         "helmrdotdev/helmr",
			WorkspaceInstallationID:     123,
			WorkspaceGithubRepositoryID: 456,
			WorkspaceRef:                testGitSHA,
			WorkspaceSha:                testGitSHA,
			MaxDurationSeconds:          3600,
			CreatedAt:                   testTime(),
			UpdatedAt:                   testTime(),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithGitHubResolver(fakeGitHubResolver{}),
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
	if claimResponse.Lease != nil || claimResponse.Run != nil {
		t.Fatalf("claim response = %+v", claimResponse)
	}
	assertTerminalPayloadFailure(t, store, "workspace_unavailable")
}

func TestWorkerRestoreClaimFailsRunWhenWorkspaceDisconnected(t *testing.T) {
	runID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	store := &fakeStore{
		githubSourceUnavailable: true,
		run: db.Run{
			ID:                          runID,
			OrgID:                       ids.ToPG(ids.DefaultOrgID),
			TaskID:                      "deploy",
			Status:                      db.RunStatusQueued,
			Payload:                     []byte(`{}`),
			SecretBindings:              []byte(`{}`),
			WorkspaceRepository:         "helmrdotdev/helmr",
			WorkspaceInstallationID:     123,
			WorkspaceGithubRepositoryID: 456,
			WorkspaceRef:                testGitSHA,
			WorkspaceSha:                testGitSHA,
			MaxDurationSeconds:          3600,
			LatestCheckpointID:          checkpointID,
			CreatedAt:                   testTime(),
			UpdatedAt:                   testTime(),
		},
		checkpoint: db.Checkpoint{
			ID:             checkpointID,
			OrgID:          ids.ToPG(ids.DefaultOrgID),
			RunID:          runID,
			Status:         db.CheckpointStatusReady,
			RuntimeBackend: pgtype.Text{String: "firecracker", Valid: true},
			RuntimeArch:    pgtype.Text{String: "arm64", Valid: true},
			RuntimeABI:     pgtype.Text{String: "helmr.firecracker.snapshot.v0", Valid: true},
			Manifest:       []byte(`{}`),
		},
		waitpoint: db.Waitpoint{
			ID:             waitpointID,
			OrgID:          ids.ToPG(ids.DefaultOrgID),
			RunID:          runID,
			CheckpointID:   checkpointID,
			Kind:           db.WaitpointKindApproval,
			Status:         db.WaitpointStatusResolved,
			ResolutionKind: pgtype.Text{String: "approved", Valid: true},
			Resolution:     []byte(`{"approved":true}`),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithGitHubResolver(fakeGitHubResolver{}),
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
	if claimResponse.Lease != nil || claimResponse.Run != nil {
		t.Fatalf("claim response = %+v", claimResponse)
	}
	assertTerminalPayloadFailure(t, store, "workspace_unavailable")
}

func TestWorkerRunLeaseFailsRunWhenSecretUnavailable(t *testing.T) {
	store := &fakeStore{
		run: db.Run{
			ID:                          ids.ToPG(ids.New()),
			OrgID:                       ids.ToPG(ids.DefaultOrgID),
			TaskID:                      "deploy",
			Status:                      db.RunStatusQueued,
			Payload:                     []byte(`{}`),
			SecretBindings:              []byte(`{"API_KEY":"vault:missing"}`),
			WorkspaceRepository:         "helmrdotdev/helmr",
			WorkspaceInstallationID:     123,
			WorkspaceGithubRepositoryID: 456,
			WorkspaceRef:                testGitSHA,
			WorkspaceSha:                testGitSHA,
			MaxDurationSeconds:          3600,
			CreatedAt:                   testTime(),
			UpdatedAt:                   testTime(),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithGitHubResolver(fakeGitHubResolver{}),
		WithSecrets(fakeSecrets{}),
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
		WithGitHubResolver(fakeGitHubResolver{}),
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

	tokenBody, err := json.Marshal(api.WorkerTokenRequest(registered))
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
		WithGitHubResolver(fakeGitHubResolver{}),
		WithWorkerAuth("01234567890123456789012345678901", time.Hour),
	)
	workerBearer := mintTestWorkerToken(t, server, "00000000-0000-0000-0000-000000000402")
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
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerLogsAndEvents(t *testing.T) {
	store := &fakeStore{
		run: db.Run{
			ID:                          ids.ToPG(ids.New()),
			OrgID:                       ids.ToPG(ids.DefaultOrgID),
			TaskID:                      "deploy",
			Status:                      db.RunStatusQueued,
			Payload:                     []byte(`{}`),
			SecretBindings:              []byte(`{}`),
			WorkspaceRepository:         "helmrdotdev/helmr",
			WorkspaceInstallationID:     123,
			WorkspaceGithubRepositoryID: 456,
			WorkspaceRef:                testGitSHA,
			WorkspaceSha:                testGitSHA,
			MaxDurationSeconds:          3600,
			CreatedAt:                   testTime(),
			UpdatedAt:                   testTime(),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithGitHubResolver(fakeGitHubResolver{}),
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

func TestWorkerWaitpointLifecycle(t *testing.T) {
	store := &fakeStore{
		run: db.Run{
			ID:                          ids.ToPG(ids.New()),
			OrgID:                       ids.ToPG(ids.DefaultOrgID),
			TaskID:                      "deploy",
			Status:                      db.RunStatusQueued,
			Payload:                     []byte(`{}`),
			SecretBindings:              []byte(`{}`),
			WorkspaceRepository:         "helmrdotdev/helmr",
			WorkspaceInstallationID:     123,
			WorkspaceGithubRepositoryID: 456,
			WorkspaceRef:                testGitSHA,
			WorkspaceSha:                testGitSHA,
			MaxDurationSeconds:          3600,
			CreatedAt:                   testTime(),
			UpdatedAt:                   testTime(),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{}),
		WithGitHubResolver(fakeGitHubResolver{}),
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
		Kind:           api.WorkerWaitpointKindApproval,
		Request:        json.RawMessage(`{"message":"ship it"}`),
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
	if run.PendingWait != nil {
		t.Fatalf("pending wait before checkpoint ready = %+v", run.PendingWait)
	}

	readyBody, err := json.Marshal(api.WorkerCheckpointReadyRequest{
		Lease:        *claimResponse.Lease,
		WaitpointID:  created.WaitpointID,
		CheckpointID: created.CheckpointID,
		Manifest: api.WorkerCheckpointManifest{
			Runtime: api.WorkerCheckpointRuntime{
				Backend:      "firecracker",
				Arch:         "amd64",
				ABI:          "helmr.test.v0",
				KernelDigest: "sha256:" + strings.Repeat("3", 64),
				RootfsDigest: "sha256:" + strings.Repeat("4", 64),
				ConfigDigest: "sha256:" + strings.Repeat("5", 64),
			},
			RuntimeState: api.WorkerCheckpointRuntimeState{
				Manifest: api.WorkerCheckpointArtifact{Digest: "sha256:" + strings.Repeat("7", 64), SizeBytes: 64, MediaType: cas.CheckpointManifestMediaType},
				VMState:  api.WorkerCheckpointArtifact{Digest: "sha256:" + strings.Repeat("1", 64), SizeBytes: 128, MediaType: cas.CheckpointVMStateMediaType},
				Memory:   []api.WorkerCheckpointArtifact{{Digest: "sha256:" + strings.Repeat("2", 64), SizeBytes: 256, MediaType: cas.CheckpointMemoryMediaType}},
			},
			Workspace: api.WorkerCheckpointWorkspace{
				Base: api.WorkerCheckpointWorkspaceBase{
					Kind:           "github",
					ArtifactDigest: "sha256:" + strings.Repeat("8", 64),
					MountPath:      "/workspace",
					VolumeKind:     "copy-on-write",
				},
				Scratch: &api.WorkerCheckpointArtifact{Digest: "sha256:" + strings.Repeat("6", 64), SizeBytes: 512, MediaType: cas.CheckpointScratchDiskMediaType},
			},
			RuntimeManifest: json.RawMessage(`{"mode":"test"}`),
		},
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
	if run.PendingWait == nil || run.PendingWait.Kind != "approval" || run.PendingWait.WaitpointID != created.WaitpointID || run.PendingWait.Message == nil || *run.PendingWait.Message != "ship it" {
		t.Fatalf("pending wait = %+v", run.PendingWait)
	}
	if store.run.Status != db.RunStatusWaiting || store.run.CurrentExecutionID.Valid {
		t.Fatalf("run after checkpoint ready = %+v", store.run)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/runs/"+claimResponse.Lease.RunID+"/waitpoints/"+created.WaitpointID+"/message", bytes.NewBufferString(`{"text":"wrong route"}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("wrong-kind resolve status = %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/runs/"+claimResponse.Lease.RunID+"/waitpoints/"+created.WaitpointID+"/approve", bytes.NewBufferString(`{"reason":"ok"}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("approve status = %d body=%s", rec.Code, rec.Body.String())
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
	if restoreClaim.Run.Restore == nil || restoreClaim.Run.Restore.CheckpointID != created.CheckpointID || restoreClaim.Run.Restore.Waitpoint.ID != created.WaitpointID || restoreClaim.Run.Restore.Waitpoint.ResolutionKind != "approved" {
		t.Fatalf("restore payload = %+v", restoreClaim.Run.Restore)
	}
	restoreResolution := decodeObject(t, restoreClaim.Run.Restore.Waitpoint.ResolutionPayloadJSON)
	if restoreResolution["approved"] != true || restoreResolution["principal"] != "operator" || restoreResolution["reason"] != "ok" {
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
	}{
		{
			name:               "approval approved",
			waitpointKind:      db.WaitpointKindApproval,
			action:             "approve",
			body:               `{"reason":"looks good"}`,
			wantResolutionKind: "approved",
			assertResolution: func(t *testing.T, payload map[string]any) {
				t.Helper()
				if payload["approved"] != true || payload["principal"] != "operator" || payload["reason"] != "looks good" {
					t.Fatalf("resolution payload = %+v", payload)
				}
				assertRFC3339NanoField(t, payload, "at")
			},
			assertEvent: func(t *testing.T, payload map[string]any) {
				t.Helper()
				if payload["kind"] != "approval" || payload["resolution_kind"] != "approved" || payload["reason"] != "looks good" {
					t.Fatalf("event payload = %+v", payload)
				}
			},
		},
		{
			name:               "approval denied",
			waitpointKind:      db.WaitpointKindApproval,
			action:             "deny",
			body:               `{"reason":"too risky"}`,
			wantResolutionKind: "denied",
			assertResolution: func(t *testing.T, payload map[string]any) {
				t.Helper()
				if payload["approved"] != false || payload["principal"] != "operator" || payload["reason"] != "too risky" {
					t.Fatalf("resolution payload = %+v", payload)
				}
				assertRFC3339NanoField(t, payload, "at")
			},
			assertEvent: func(t *testing.T, payload map[string]any) {
				t.Helper()
				if payload["kind"] != "approval" || payload["resolution_kind"] != "denied" || payload["reason"] != "too risky" {
					t.Fatalf("event payload = %+v", payload)
				}
			},
		},
		{
			name:               "message replied",
			waitpointKind:      db.WaitpointKindMessage,
			action:             "message",
			body:               `{"text":"continue","attachments":[{"name":"notes.txt","url":"https://example.test/notes.txt"}]}`,
			wantResolutionKind: "replied",
			assertResolution: func(t *testing.T, payload map[string]any) {
				t.Helper()
				if payload["text"] != "continue" || payload["principal"] != "operator" {
					t.Fatalf("resolution payload = %+v", payload)
				}
				attachments, ok := payload["attachments"].([]any)
				if !ok || len(attachments) != 1 {
					t.Fatalf("attachments = %+v", payload["attachments"])
				}
				attachment, ok := attachments[0].(map[string]any)
				if !ok || attachment["name"] != "notes.txt" || attachment["url"] != "https://example.test/notes.txt" {
					t.Fatalf("attachment = %+v", attachments[0])
				}
				assertRFC3339NanoField(t, payload, "at")
			},
			assertEvent: func(t *testing.T, payload map[string]any) {
				t.Helper()
				result, ok := payload["result"].(map[string]any)
				if !ok || payload["kind"] != "message" || payload["resolution_kind"] != "replied" || result["text"] != "continue" {
					t.Fatalf("event payload = %+v", payload)
				}
			},
		},
	}

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
				waitpoint: db.Waitpoint{
					ID:          ids.ToPG(waitpointID),
					OrgID:       ids.ToPG(ids.DefaultOrgID),
					RunID:       ids.ToPG(runID),
					Kind:        tt.waitpointKind,
					Status:      db.WaitpointStatusPending,
					RequestedAt: testTime(),
				},
			}
			server := New(
				slog.New(slog.NewTextHandler(io.Discard, nil)),
				WithDB(store),
				WithAuthenticator(fakeAuth{}),
			)
			req := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID.String()+"/waitpoints/"+waitpointID.String()+"/"+tt.action, strings.NewReader(tt.body))
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

type fakeStore struct {
	db.Querier
	createRun                     db.CreateRunParams
	listRuns                      db.ListRunSummariesParams
	countRunsOrgID                pgtype.UUID
	countScopedRuns               db.CountScopedRunsByStatusParams
	run                           db.Run
	deployment                    db.Deployment
	deploymentLabels              []db.AssignDeploymentLabelParams
	createDeploymentResult        *db.Deployment
	createDeploymentErr           error
	deploymentTasks               []db.DeploymentTask
	runEvent                      db.AppendRunEventParams
	events                        []db.RunEvent
	stdout                        []byte
	stderr                        []byte
	runLogSnapshot                db.GetRunLogSnapshotParams
	logTruncated                  bool
	stdoutCursor                  int64
	stderrCursor                  int64
	casObjects                    []db.UpsertCasObjectParams
	getCasObjectErr               error
	executionID                   pgtype.UUID
	executionWorkerInstanceID     pgtype.UUID
	executionLeaseExpiresAt       pgtype.Timestamptz
	githubUpsert                  *db.UpsertGitHubInstallationParams
	githubSuspend                 *db.SuspendGitHubInstallationParams
	githubDelete                  *db.DeleteGitHubInstallationParams
	githubInstallation            db.GitHubAppInstallation
	githubSourceUnavailable       bool
	githubSuspendByInstallationID *int64
	githubDeleteByInstallationID  *int64
	waitpoint                     db.Waitpoint
	checkpoint                    db.Checkpoint
	checkpointArtifacts           []byte
	abandonedClaim                bool
	workerBootstrapTokenHash      []byte
	workerCredentialID            pgtype.UUID
	workerCredentialSecretHash    []byte
	dequeueRequest                dispatch.DequeueRequest
	ackedLeases                   []dispatch.Lease
	activeQueueLeaseMissing       bool
	renewErr                      error
}

type fakeRunEnqueuer struct {
	orgID pgtype.UUID
	runID pgtype.UUID
	err   error
}

func (f *fakeRunEnqueuer) EnqueueRun(_ context.Context, orgID pgtype.UUID, runID pgtype.UUID) (dispatch.EnqueueResult, error) {
	f.orgID = orgID
	f.runID = runID
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

func (f fakeSecrets) Check(_ context.Context, _ uuid.UUID, bindings api.SecretBindings) error {
	return f.CheckScoped(context.Background(), uuid.Nil, uuid.Nil, uuid.Nil, bindings)
}

func (f fakeSecrets) CheckScoped(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ uuid.UUID, bindings api.SecretBindings) error {
	for _, stored := range bindings {
		_, stored, _ = strings.Cut(stored, ":")
		if len(f.values) == 0 {
			continue
		}
		if _, ok := f.values[stored]; !ok {
			return pgx.ErrNoRows
		}
	}
	return nil
}

func (f fakeSecrets) Resolve(_ context.Context, _ uuid.UUID, bindings api.SecretBindings) (api.ResolvedSecrets, error) {
	return f.ResolveScoped(context.Background(), uuid.Nil, uuid.Nil, uuid.Nil, bindings)
}

func (f fakeSecrets) ResolveScoped(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ uuid.UUID, bindings api.SecretBindings) (api.ResolvedSecrets, error) {
	resolved := api.ResolvedSecrets{}
	for declared, stored := range bindings {
		_, stored, _ = strings.Cut(stored, ":")
		value, ok := f.values[stored]
		if !ok {
			return nil, pgx.ErrNoRows
		}
		resolved[declared] = append([]byte(nil), value...)
	}
	return resolved, nil
}

func (f *fakeStore) GetCurrentDeploymentTask(_ context.Context, arg db.GetCurrentDeploymentTaskParams) (db.GetCurrentDeploymentTaskRow, error) {
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
		MaxDurationSeconds:     300,
		CreatedAt:              testTime(),
		DeploymentSourceDigest: "sha256:" + strings.Repeat("a", 64),
	}, nil
}

func (f *fakeStore) GetActiveProjectGitHubRepositoryByFullName(_ context.Context, arg db.GetActiveProjectGitHubRepositoryByFullNameParams) (db.GetActiveProjectGitHubRepositoryByFullNameRow, error) {
	if f.githubSourceUnavailable {
		return db.GetActiveProjectGitHubRepositoryByFullNameRow{}, pgx.ErrNoRows
	}
	if arg.ProjectID != testProjectID() || arg.FullName != "helmrdotdev/helmr" {
		return db.GetActiveProjectGitHubRepositoryByFullNameRow{}, pgx.ErrNoRows
	}
	return db.GetActiveProjectGitHubRepositoryByFullNameRow{
		ProjectGithubRepositoryID: ids.ToPG(ids.New()),
		InstallationID:            123,
		GithubRepositoryID:        456,
		FullName:                  "helmrdotdev/helmr",
		RepositoryName:            "helmr",
	}, nil
}

func (f *fakeStore) GetActiveProjectGitHubRepository(_ context.Context, arg db.GetActiveProjectGitHubRepositoryParams) (db.GetActiveProjectGitHubRepositoryRow, error) {
	if f.githubSourceUnavailable {
		return db.GetActiveProjectGitHubRepositoryRow{}, pgx.ErrNoRows
	}
	if arg.ProjectID != testProjectID() || arg.GithubRepositoryID != 456 {
		return db.GetActiveProjectGitHubRepositoryRow{}, pgx.ErrNoRows
	}
	return db.GetActiveProjectGitHubRepositoryRow{
		ProjectGithubRepositoryID: ids.ToPG(ids.New()),
		InstallationID:            123,
		GithubRepositoryID:        456,
		FullName:                  "helmrdotdev/helmr",
		RepositoryName:            "helmr",
	}, nil
}

func (f *fakeStore) GetCurrentDeployment(_ context.Context, arg db.GetCurrentDeploymentParams) (db.GetCurrentDeploymentRow, error) {
	if f.deployment.ID == (pgtype.UUID{}) || f.deployment.Status != db.DeploymentStatusDeployed {
		return db.GetCurrentDeploymentRow{}, pgx.ErrNoRows
	}
	if f.deployment.OrgID != arg.OrgID || f.deployment.ProjectID != arg.ProjectID || f.deployment.EnvironmentID != arg.EnvironmentID {
		return db.GetCurrentDeploymentRow{}, pgx.ErrNoRows
	}
	return db.GetCurrentDeploymentRow{
		ID:                       f.deployment.ID,
		OrgID:                    f.deployment.OrgID,
		ProjectID:                f.deployment.ProjectID,
		EnvironmentID:            f.deployment.EnvironmentID,
		DeploymentSourceDigest:   f.deployment.DeploymentSourceDigest,
		BuildManifestDigest:      f.deployment.BuildManifestDigest,
		DeploymentManifestDigest: f.deployment.DeploymentManifestDigest,
		Status:                   f.deployment.Status,
		ErrorJson:                f.deployment.ErrorJson,
		CreatedAt:                f.deployment.CreatedAt,
		BuildingAt:               f.deployment.BuildingAt,
		BuiltAt:                  f.deployment.BuiltAt,
		DeployedAt:               f.deployment.DeployedAt,
		FailedAt:                 f.deployment.FailedAt,
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
		return f.deployment, nil
	}
	f.deployment = db.Deployment{
		ID:                     arg.ID,
		OrgID:                  arg.OrgID,
		ProjectID:              arg.ProjectID,
		EnvironmentID:          arg.EnvironmentID,
		ContentHash:            arg.ContentHash,
		DeploymentSourceDigest: arg.DeploymentSourceDigest,
		Status:                 arg.Status,
		CreatedAt:              testTime(),
		DeployedAt:             testTime(),
	}
	return f.deployment, nil
}

func (f *fakeStore) AssignDeploymentLabel(_ context.Context, arg db.AssignDeploymentLabelParams) (db.DeploymentLabel, error) {
	f.deploymentLabels = append(f.deploymentLabels, arg)
	return db.DeploymentLabel{
		OrgID:         arg.OrgID,
		ProjectID:     arg.ProjectID,
		EnvironmentID: arg.EnvironmentID,
		Label:         arg.Label,
		DeploymentID:  arg.DeploymentID,
		AssignedAt:    testTime(),
	}, nil
}

func (f *fakeStore) CreateDeploymentTask(_ context.Context, arg db.CreateDeploymentTaskParams) (db.DeploymentTask, error) {
	task := db.DeploymentTask{
		ID:                 arg.ID,
		OrgID:              arg.OrgID,
		ProjectID:          arg.ProjectID,
		EnvironmentID:      arg.EnvironmentID,
		DeploymentID:       arg.DeploymentID,
		TaskID:             arg.TaskID,
		FilePath:           arg.FilePath,
		ExportName:         arg.ExportName,
		HandlerEntrypoint:  arg.HandlerEntrypoint,
		BundleDigest:       arg.BundleDigest,
		RequestedMilliCpu:  arg.RequestedMilliCpu,
		RequestedMemoryMib: arg.RequestedMemoryMib,
		SecretsJson:        arg.SecretsJson,
		ResourcesJson:      arg.ResourcesJson,
		MaxDurationSeconds: arg.MaxDurationSeconds,
		CreatedAt:          testTime(),
	}
	f.deploymentTasks = append(f.deploymentTasks, task)
	return task, nil
}

func (f *fakeStore) CreateRun(_ context.Context, arg db.CreateRunParams) (db.CreateRunRow, error) {
	f.createRun = arg
	now := testTime()
	f.run = db.Run{
		ID:                          arg.ID,
		OrgID:                       arg.OrgID,
		ProjectID:                   testProjectID(),
		EnvironmentID:               testEnvironmentID(),
		DeploymentID:                arg.DeploymentID,
		DeploymentTaskID:            arg.DeploymentTaskID,
		TaskID:                      arg.TaskID,
		Status:                      db.RunStatusQueued,
		Payload:                     arg.Payload,
		SecretBindings:              arg.SecretBindings,
		WorkspaceRepository:         arg.WorkspaceRepository,
		WorkspaceInstallationID:     arg.WorkspaceInstallationID,
		WorkspaceGithubRepositoryID: arg.WorkspaceGithubRepositoryID,
		WorkspaceRef:                arg.WorkspaceRef,
		WorkspaceSha:                arg.WorkspaceSha,
		WorkspaceSubpath:            arg.WorkspaceSubpath,
		MaxDurationSeconds:          arg.MaxDurationSeconds,
		CreatedAt:                   now,
		UpdatedAt:                   now,
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
	return db.CreateRunRow{
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

func (f *fakeStore) CreateScopedRun(_ context.Context, arg db.CreateScopedRunParams) (db.CreateScopedRunRow, error) {
	f.createRun = db.CreateRunParams{
		ID:                          arg.ID,
		OrgID:                       arg.OrgID,
		DeploymentID:                arg.DeploymentID,
		DeploymentTaskID:            arg.DeploymentTaskID,
		TaskID:                      arg.TaskID,
		Payload:                     arg.Payload,
		SecretBindings:              arg.SecretBindings,
		WorkspaceRepository:         arg.WorkspaceRepository,
		WorkspaceInstallationID:     arg.WorkspaceInstallationID,
		WorkspaceGithubRepositoryID: arg.WorkspaceGithubRepositoryID,
		WorkspaceRef:                arg.WorkspaceRef,
		WorkspaceSha:                arg.WorkspaceSha,
		WorkspaceSubpath:            arg.WorkspaceSubpath,
		WorkspaceRefKind:            arg.WorkspaceRefKind,
		WorkspaceRefName:            arg.WorkspaceRefName,
		WorkspaceFullRef:            arg.WorkspaceFullRef,
		WorkspaceDefaultBranch:      arg.WorkspaceDefaultBranch,
		WorkspacePrNumber:           arg.WorkspacePrNumber,
		WorkspacePrBaseRef:          arg.WorkspacePrBaseRef,
		WorkspacePrBaseSha:          arg.WorkspacePrBaseSha,
		WorkspacePrHeadRef:          arg.WorkspacePrHeadRef,
		WorkspacePrHeadSha:          arg.WorkspacePrHeadSha,
		MaxDurationSeconds:          arg.MaxDurationSeconds,
		EventPayload:                arg.EventPayload,
	}
	now := testTime()
	f.run = db.Run{
		ID:                          arg.ID,
		OrgID:                       arg.OrgID,
		ProjectID:                   arg.ProjectID,
		EnvironmentID:               arg.EnvironmentID,
		DeploymentID:                arg.DeploymentID,
		DeploymentTaskID:            arg.DeploymentTaskID,
		TaskID:                      arg.TaskID,
		Status:                      db.RunStatusQueued,
		Payload:                     arg.Payload,
		SecretBindings:              arg.SecretBindings,
		WorkspaceRepository:         arg.WorkspaceRepository,
		WorkspaceInstallationID:     arg.WorkspaceInstallationID,
		WorkspaceGithubRepositoryID: arg.WorkspaceGithubRepositoryID,
		WorkspaceRef:                arg.WorkspaceRef,
		WorkspaceSha:                arg.WorkspaceSha,
		WorkspaceSubpath:            arg.WorkspaceSubpath,
		WorkspaceRefKind:            arg.WorkspaceRefKind,
		WorkspaceRefName:            arg.WorkspaceRefName,
		WorkspaceFullRef:            arg.WorkspaceFullRef,
		WorkspaceDefaultBranch:      arg.WorkspaceDefaultBranch,
		WorkspacePrNumber:           arg.WorkspacePrNumber,
		WorkspacePrBaseRef:          arg.WorkspacePrBaseRef,
		WorkspacePrBaseSha:          arg.WorkspacePrBaseSha,
		WorkspacePrHeadRef:          arg.WorkspacePrHeadRef,
		WorkspacePrHeadSha:          arg.WorkspacePrHeadSha,
		MaxDurationSeconds:          arg.MaxDurationSeconds,
		CreatedAt:                   now,
		UpdatedAt:                   now,
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
	return []db.ListQueueScopesRow{{
		OrgID:     ids.ToPG(ids.DefaultOrgID),
		QueueName: "queue-a",
	}}, nil
}

func (f *fakeStore) UpsertWorkerInstanceHeartbeat(_ context.Context, arg db.UpsertWorkerInstanceHeartbeatParams) (db.WorkerInstance, error) {
	return db.WorkerInstance{
		ID:                      arg.ID,
		ResourceID:              arg.ResourceID,
		Status:                  db.WorkerInstanceStatusActive,
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
		FirstSeenAt:             testTime(),
		LastSeenAt:              testTime(),
	}, nil
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
		LeaseExpiresAt:    f.executionLeaseExpiresAt,
		QueueName:         "queue-a",
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
	f.run.CurrentExecutionID = f.executionID
	if f.run.LatestCheckpointID.Valid && f.run.LatestCheckpointID == f.checkpoint.ID && f.checkpoint.Status == db.CheckpointStatusReady && f.waitpoint.Status == db.WaitpointStatusResolved {
		f.checkpoint.Status = db.CheckpointStatusRestoring
	}
	projectID := f.run.ProjectID
	if !projectID.Valid {
		projectID = testProjectID()
	}
	environmentID := f.run.EnvironmentID
	if !environmentID.Valid {
		environmentID = testEnvironmentID()
	}
	return db.LeaseRunExecutionRow{
		ID:                          f.run.ID,
		OrgID:                       f.run.OrgID,
		ProjectID:                   projectID,
		EnvironmentID:               environmentID,
		TaskID:                      f.run.TaskID,
		Status:                      f.run.Status,
		Payload:                     f.run.Payload,
		SecretBindings:              f.run.SecretBindings,
		DeploymentTaskID:            testDeploymentTaskID(),
		DeploymentTaskFilePath:      "src/task.ts",
		DeploymentTaskExportName:    "deploy",
		DeploymentSourceDigest:      "sha256:" + strings.Repeat("a", 64),
		WorkspaceRepository:         f.run.WorkspaceRepository,
		WorkspaceInstallationID:     f.run.WorkspaceInstallationID,
		WorkspaceGithubRepositoryID: f.run.WorkspaceGithubRepositoryID,
		WorkspaceRef:                f.run.WorkspaceRef,
		WorkspaceSha:                f.run.WorkspaceSha,
		WorkspaceSubpath:            f.run.WorkspaceSubpath,
		WorkspaceRefKind:            f.run.WorkspaceRefKind,
		WorkspaceRefName:            f.run.WorkspaceRefName,
		WorkspaceFullRef:            f.run.WorkspaceFullRef,
		WorkspaceDefaultBranch:      f.run.WorkspaceDefaultBranch,
		WorkspacePrNumber:           f.run.WorkspacePrNumber,
		WorkspacePrBaseRef:          f.run.WorkspacePrBaseRef,
		WorkspacePrBaseSha:          f.run.WorkspacePrBaseSha,
		WorkspacePrHeadRef:          f.run.WorkspacePrHeadRef,
		WorkspacePrHeadSha:          f.run.WorkspacePrHeadSha,
		MaxDurationSeconds:          f.run.MaxDurationSeconds,
		ExitCode:                    f.run.ExitCode,
		ErrorMessage:                f.run.ErrorMessage,
		CreatedAt:                   f.run.CreatedAt,
		UpdatedAt:                   f.run.UpdatedAt,
		StartedAt:                   f.run.StartedAt,
		FinishedAt:                  f.run.FinishedAt,
		ExecutionID:                 f.executionID,
		ExecutionWorkerInstanceID:   f.executionWorkerInstanceID,
		ExecutionDispatchMessageID:  arg.DispatchMessageID.String,
		ExecutionDispatchLeaseID:    arg.DispatchLeaseID,
		ExecutionDispatchAttempt:    arg.DispatchAttempt,
		ExecutionLeaseExpiresAt:     f.executionLeaseExpiresAt,
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

func (f *fakeStore) StartRunExecution(_ context.Context, arg db.StartRunExecutionParams) (db.RunStatus, error) {
	if f.run.Status != db.RunStatusRunning || f.executionID != arg.ExecutionID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return "", pgx.ErrNoRows
	}
	f.run.Status = db.RunStatusRunning
	f.run.StartedAt = testTime()
	f.run.UpdatedAt = testTime()
	return f.run.Status, nil
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
			ID:                          f.run.ID,
			OrgID:                       f.run.OrgID,
			TaskID:                      f.run.TaskID,
			Status:                      f.run.Status,
			Payload:                     f.run.Payload,
			Output:                      f.run.Output,
			SecretBindings:              f.run.SecretBindings,
			WorkspaceRepository:         f.run.WorkspaceRepository,
			WorkspaceInstallationID:     f.run.WorkspaceInstallationID,
			WorkspaceGithubRepositoryID: f.run.WorkspaceGithubRepositoryID,
			WorkspaceRef:                f.run.WorkspaceRef,
			WorkspaceSha:                f.run.WorkspaceSha,
			WorkspaceSubpath:            f.run.WorkspaceSubpath,
			MaxDurationSeconds:          f.run.MaxDurationSeconds,
			ExitCode:                    f.run.ExitCode,
			ErrorMessage:                f.run.ErrorMessage,
			CreatedAt:                   f.run.CreatedAt,
			UpdatedAt:                   f.run.UpdatedAt,
			StartedAt:                   f.run.StartedAt,
			FinishedAt:                  f.run.FinishedAt,
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
		ID:        int64(len(f.events) + 1),
		OrgID:     arg.OrgID,
		RunID:     arg.RunID,
		Kind:      arg.Kind,
		Payload:   arg.Payload,
		CreatedAt: testTime(),
	}
	f.events = append(f.events, event)
	return db.AppendRunLogChunkRow{
		RunID:       arg.RunID,
		Stream:      arg.Stream,
		Seq:         int64(len(f.events)),
		ObservedSeq: arg.ObservedSeq,
		Content:     arg.Content,
		CreatedAt:   testTime(),
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
	f.waitpoint = db.Waitpoint{
		ID:             arg.ID,
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
		Status:         db.WaitpointStatusCreating,
		RequestedAt:    testTime(),
	}
	return db.CreateWaitpointForExecutionRow{
		ID:             f.waitpoint.ID,
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

func (f *fakeStore) MarkWaitpointCheckpointReady(_ context.Context, arg db.MarkWaitpointCheckpointReadyParams) (db.MarkWaitpointCheckpointReadyRow, error) {
	if f.executionID != arg.ExecutionID || f.executionWorkerInstanceID != arg.WorkerInstanceID {
		return db.MarkWaitpointCheckpointReadyRow{}, pgx.ErrNoRows
	}
	if !f.waitpoint.ID.Valid || f.waitpoint.ID != arg.WaitpointID || f.waitpoint.CheckpointID != arg.CheckpointID || f.waitpoint.Status != db.WaitpointStatusCreating {
		return db.MarkWaitpointCheckpointReadyRow{}, pgx.ErrNoRows
	}
	f.waitpoint.Status = db.WaitpointStatusPending
	f.waitpoint.RequestedAt = testTime()
	f.checkpoint = db.Checkpoint{
		ID:                         arg.CheckpointID,
		OrgID:                      arg.OrgID,
		RunID:                      arg.RunID,
		ExecutionID:                arg.ExecutionID,
		Status:                     db.CheckpointStatusReady,
		RuntimeBackend:             arg.RuntimeBackend,
		RuntimeArch:                arg.RuntimeArch,
		RuntimeABI:                 arg.RuntimeABI,
		KernelDigest:               arg.KernelDigest,
		RootfsDigest:               arg.RootfsDigest,
		ImageKey:                   arg.ImageKey,
		RuntimeConfigDigest:        arg.RuntimeConfigDigest,
		WorkspaceBaseKind:          arg.WorkspaceBaseKind,
		WorkspaceRepository:        arg.WorkspaceRepository,
		WorkspaceRef:               arg.WorkspaceRef,
		WorkspaceSha:               arg.WorkspaceSha,
		WorkspaceSubpath:           arg.WorkspaceSubpath,
		WorkspaceRefKind:           arg.WorkspaceRefKind,
		WorkspaceRefName:           arg.WorkspaceRefName,
		WorkspaceFullRef:           arg.WorkspaceFullRef,
		WorkspaceDefaultBranch:     arg.WorkspaceDefaultBranch,
		WorkspaceArtifactDigest:    arg.WorkspaceArtifactDigest,
		WorkspaceArtifactMediaType: arg.WorkspaceArtifactMediaType,
		WorkspaceArtifactEncoding:  arg.WorkspaceArtifactEncoding,
		WorkspaceMountPath:         arg.WorkspaceMountPath,
		WorkspaceProjectSubpath:    arg.WorkspaceProjectSubpath,
		WorkspaceVolumeKind:        arg.WorkspaceVolumeKind,
		Manifest:                   arg.Manifest,
		ReadyAt:                    testTime(),
	}
	f.checkpointArtifacts = arg.CheckpointArtifacts
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
		Payload:   []byte(`{"kind":"approval"}`),
		CreatedAt: testTime(),
	})
	return db.MarkWaitpointCheckpointReadyRow{
		ID:             f.waitpoint.ID,
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
	if f.executionID != arg.ExecutionID || f.executionWorkerInstanceID != arg.WorkerInstanceID || !f.waitpoint.ID.Valid || f.waitpoint.CheckpointID != arg.CheckpointID || f.waitpoint.Status != db.WaitpointStatusCreating {
		return db.MarkWaitpointCheckpointFailedRow{}, pgx.ErrNoRows
	}
	f.waitpoint.Status = db.WaitpointStatusCancelled
	f.waitpoint.ResolutionKind = pgtype.Text{String: "cancelled", Valid: true}
	f.waitpoint.Resolution = []byte(`{"source":"checkpoint"}`)
	f.waitpoint.ResolvedAt = testTime()
	return db.MarkWaitpointCheckpointFailedRow{
		ID:             f.waitpoint.ID,
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

func (f *fakeStore) GetPendingWaitpointForRun(_ context.Context, arg db.GetPendingWaitpointForRunParams) (db.Waitpoint, error) {
	if f.waitpoint.ID.Valid && f.waitpoint.OrgID == arg.OrgID && f.waitpoint.RunID == arg.RunID && f.waitpoint.Status == db.WaitpointStatusPending {
		return f.waitpoint, nil
	}
	return db.Waitpoint{}, pgx.ErrNoRows
}

func (f *fakeStore) ListWaitpointDeliveries(context.Context, db.ListWaitpointDeliveriesParams) ([]db.WaitpointDelivery, error) {
	return nil, nil
}

func (f *fakeStore) ResolveWaitpoint(_ context.Context, arg db.ResolveWaitpointParams) (db.ResolveWaitpointRow, error) {
	if !f.waitpoint.ID.Valid || f.waitpoint.OrgID != arg.OrgID || f.waitpoint.RunID != arg.RunID || f.waitpoint.ID != arg.ID || f.waitpoint.Kind != arg.Kind || f.waitpoint.Status != db.WaitpointStatusPending {
		return db.ResolveWaitpointRow{}, pgx.ErrNoRows
	}
	f.waitpoint.Status = db.WaitpointStatusResolved
	f.waitpoint.ResolutionKind = arg.ResolutionKind
	f.waitpoint.Resolution = arg.Resolution
	f.waitpoint.ResolvedAt = testTime()
	f.run.Status = db.RunStatusQueued
	f.run.CurrentExecutionID = pgtype.UUID{}
	f.run.UpdatedAt = testTime()
	event := db.RunEvent{
		ID:        int64(len(f.events) + 1),
		OrgID:     arg.OrgID,
		RunID:     arg.RunID,
		Kind:      "waitpoint.resolved",
		Payload:   arg.Payload,
		CreatedAt: testTime(),
	}
	f.events = append(f.events, event)
	return db.ResolveWaitpointRow{
		ID:             f.waitpoint.ID,
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

func (f *fakeStore) ExpireDuePendingWaitpoints(context.Context, pgtype.UUID) error {
	if f.waitpoint.ID.Valid && f.waitpoint.Status == db.WaitpointStatusPending && f.waitpoint.TimeoutSeconds.Valid && f.run.Status == db.RunStatusWaiting && !f.run.CurrentExecutionID.Valid {
		if !testTime().Time.Before(f.waitpoint.RequestedAt.Time.Add(time.Duration(f.waitpoint.TimeoutSeconds.Int32) * time.Second)) {
			f.waitpoint.Status = db.WaitpointStatusResolved
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
	if !f.waitpoint.ID.Valid || f.waitpoint.Status != db.WaitpointStatusResolved || !f.waitpoint.ResolutionKind.Valid || f.waitpoint.CheckpointID != f.checkpoint.ID {
		return db.GetRunRestorePayloadRow{}, pgx.ErrNoRows
	}
	return db.GetRunRestorePayloadRow{
		CheckpointID:               f.checkpoint.ID,
		RuntimeBackend:             f.checkpoint.RuntimeBackend,
		RuntimeArch:                f.checkpoint.RuntimeArch,
		RuntimeABI:                 f.checkpoint.RuntimeABI,
		KernelDigest:               f.checkpoint.KernelDigest,
		RootfsDigest:               f.checkpoint.RootfsDigest,
		ImageKey:                   f.checkpoint.ImageKey,
		RuntimeConfigDigest:        f.checkpoint.RuntimeConfigDigest,
		WorkspaceBaseKind:          f.checkpoint.WorkspaceBaseKind,
		WorkspaceRepository:        f.checkpoint.WorkspaceRepository,
		WorkspaceRef:               f.checkpoint.WorkspaceRef,
		WorkspaceSha:               f.checkpoint.WorkspaceSha,
		WorkspaceSubpath:           f.checkpoint.WorkspaceSubpath,
		WorkspaceRefKind:           f.checkpoint.WorkspaceRefKind,
		WorkspaceRefName:           f.checkpoint.WorkspaceRefName,
		WorkspaceFullRef:           f.checkpoint.WorkspaceFullRef,
		WorkspaceDefaultBranch:     f.checkpoint.WorkspaceDefaultBranch,
		WorkspaceArtifactDigest:    f.checkpoint.WorkspaceArtifactDigest,
		WorkspaceArtifactMediaType: f.checkpoint.WorkspaceArtifactMediaType,
		WorkspaceArtifactEncoding:  f.checkpoint.WorkspaceArtifactEncoding,
		WorkspaceMountPath:         f.checkpoint.WorkspaceMountPath,
		WorkspaceProjectSubpath:    f.checkpoint.WorkspaceProjectSubpath,
		WorkspaceVolumeKind:        f.checkpoint.WorkspaceVolumeKind,
		CheckpointArtifacts:        f.checkpointArtifacts,
		Manifest:                   f.checkpoint.Manifest,
		WaitpointID:                f.waitpoint.ID,
		WaitpointKind:              f.waitpoint.Kind,
		ResolutionKind:             f.waitpoint.ResolutionKind,
		Resolution:                 f.waitpoint.Resolution,
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
	permissions []auth.PermissionGrant
}

func (f *fakeStore) GetDefaultProjectEnvironment(context.Context, pgtype.UUID) (db.GetDefaultProjectEnvironmentRow, error) {
	return db.GetDefaultProjectEnvironmentRow{
		ProjectID:     testProjectID(),
		EnvironmentID: testEnvironmentID(),
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
	return auth.Actor{OrgID: ids.DefaultOrgID, Role: role, Kind: kind, Permissions: f.permissions}, nil
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
