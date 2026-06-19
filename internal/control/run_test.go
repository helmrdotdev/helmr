package control

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/helmrdotdev/helmr/internal/publicaccess"
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
	}},
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

func TestSessionChannelInputFingerprintIncludesCorrelationAndAuthSubject(t *testing.T) {
	session := db.TaskSession{ID: pgvalue.UUID(uuid.Must(uuid.NewV7()))}
	base, err := channelInputFingerprint(session, "approval", []byte(`{"approved":true}`), "thread-1", "application/json", "api_key", "key-1", "event-1")
	if err != nil {
		t.Fatal(err)
	}
	changedCorrelation, err := channelInputFingerprint(session, "approval", []byte(`{"approved":true}`), "thread-2", "application/json", "api_key", "key-1", "event-1")
	if err != nil {
		t.Fatal(err)
	}
	changedAuth, err := channelInputFingerprint(session, "approval", []byte(`{"approved":true}`), "thread-1", "application/json", "api_key", "key-2", "event-1")
	if err != nil {
		t.Fatal(err)
	}
	changedExternalEvent, err := channelInputFingerprint(session, "approval", []byte(`{"approved":true}`), "thread-1", "application/json", "api_key", "key-1", "event-2")
	if err != nil {
		t.Fatal(err)
	}
	if base == changedCorrelation {
		t.Fatal("fingerprint did not change when correlation changed")
	}
	if base == changedAuth {
		t.Fatal("fingerprint did not change when auth subject changed")
	}
	if base == changedExternalEvent {
		t.Fatal("fingerprint did not change when external event changed")
	}
}

func TestAppendTaskSessionChannelInputReturnsDuplicateIdempotencyStatus(t *testing.T) {
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	channelID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	recordID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
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
		appendSessionChannelInput: db.AppendSessionChannelInputRow{
			ID:                     recordID,
			OrgID:                  pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:              testProjectID(),
			EnvironmentID:          testEnvironmentID(),
			RunID:                  runID,
			ChannelID:              channelID,
			Channel:                "approval",
			Data:                   []byte(`{"approved":true}`),
			CorrelationID:          "thread-1",
			Sequence:               1,
			ContentType:            "application/json",
			IdempotencyKey:         "slack-action-1",
			IdempotencyFingerprint: "fingerprint-1",
			ExternalEventID:        "event-1",
			AuthSubjectType:        "api_key",
			AuthSubjectID:          "key-1",
			Inserted:               false,
			CreatedAt:              testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+pgvalue.MustUUIDValue(sessionID).String()+"/channels/approval/inputs", strings.NewReader(`{"data":{"approved":true},"correlation_id":"thread-1","external_event_id":"event-1"}`))
	req.Header.Set("authorization", "Bearer test-key")
	req.Header.Set("idempotency-key", "slack-action-1")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.AppendChannelRecordResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.IdempotencyStatus != "duplicate" || response.Record.ID != pgvalue.MustUUIDValue(recordID).String() {
		t.Fatalf("response = %+v", response)
	}
	if store.appendSessionChannelInput.ExternalEventID != "event-1" {
		t.Fatalf("external event id = %q", store.appendSessionChannelInput.ExternalEventID)
	}
}

func TestAppendTaskSessionChannelInputReturnsDuplicateAfterTerminalSession(t *testing.T) {
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	channelID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	recordID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	session := db.TaskSession{
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
		TerminalReason:      []byte(`{}`),
		CreatedAt:           testTime(),
		UpdatedAt:           testTime(),
	}
	authSubjectID := "00000000-0000-0000-0000-000000000002"
	fingerprint, err := channelInputFingerprint(session, "approval", []byte(`{"approved":true}`), "thread-1", "application/json", "api_key", authSubjectID, "event-1")
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{
		taskSession: session,
		appendSessionChannelInput: db.AppendSessionChannelInputRow{
			ID:                     recordID,
			OrgID:                  pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:              testProjectID(),
			EnvironmentID:          testEnvironmentID(),
			ChannelID:              channelID,
			Channel:                "approval",
			Data:                   []byte(`{"approved":true}`),
			CorrelationID:          "thread-1",
			Sequence:               1,
			ContentType:            "application/json",
			IdempotencyKey:         "slack-action-1",
			IdempotencyFingerprint: fingerprint,
			ExternalEventID:        "event-1",
			AuthSubjectType:        "api_key",
			AuthSubjectID:          authSubjectID,
			Inserted:               false,
			CreatedAt:              testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+pgvalue.MustUUIDValue(sessionID).String()+"/channels/approval/inputs", strings.NewReader(`{"data":{"approved":true},"correlation_id":"thread-1","external_event_id":"event-1"}`))
	req.Header.Set("authorization", "Bearer test-key")
	req.Header.Set("idempotency-key", "slack-action-1")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.AppendChannelRecordResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.IdempotencyStatus != "duplicate" || response.Record.ID != pgvalue.MustUUIDValue(recordID).String() {
		t.Fatalf("response = %+v", response)
	}
	if store.appendSessionChannelInputCalls != 0 {
		t.Fatalf("append calls = %d, want 0", store.appendSessionChannelInputCalls)
	}
}

func TestListTaskSessionChannelsReturnsChannelSummary(t *testing.T) {
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	channelID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
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
		taskSessionChannels: []db.Channel{{
			ID:            channelID,
			OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:     testProjectID(),
			EnvironmentID: testEnvironmentID(),
			TaskSessionID: sessionID,
			Name:          "approval",
			Direction:     db.ChannelDirectionInput,
			Backend:       "inline",
			NextSequence:  2,
			CreatedAt:     testTime(),
		}},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+pgvalue.MustUUIDValue(sessionID).String()+"/channels", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.ListTaskSessionChannelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Channels) != 1 {
		t.Fatalf("channels len = %d, want 1", len(response.Channels))
	}
	channel := response.Channels[0]
	if channel.ID != pgvalue.MustUUIDValue(channelID).String() ||
		channel.Name != "approval" ||
		channel.Direction != "input" ||
		channel.NextSequence != 2 {
		t.Fatalf("channel = %+v", channel)
	}
}

func TestAppendTaskSessionChannelInputRejectsClosedInputWithStableCode(t *testing.T) {
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
			Status:              db.TaskSessionStatusClosed,
			Metadata:            []byte(`{}`),
			Tags:                []string{},
			TerminalReason:      []byte(`{}`),
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+pgvalue.MustUUIDValue(sessionID).String()+"/channels/approval/inputs", strings.NewReader(`{"data":{"approved":true}}`))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "session_input_closed")
}

func TestTaskStartRejectsDeploymentSelection(t *testing.T) {
	store := &fakeStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})

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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})

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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}, EventStream: eventStream})
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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}, EventStream: newTestEventStream(t)})
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

func TestStreamTaskSessionChannelOutputsWaitsForMissingChannel(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+pgvalue.MustUUIDValue(sessionID).String()+"/channels/release/outputs/stream", nil).WithContext(ctx)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("content-type"); got != "text/event-stream" {
		t.Fatalf("content-type = %q", got)
	}
	if !strings.Contains(rec.Body.String(), ": keep-alive") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestListTaskSessionChannelOutputsPassesCursorAndCorrelationFilter(t *testing.T) {
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	channelID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	recordID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
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
		taskSessionChannel: db.Channel{
			ID:            channelID,
			OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:     testProjectID(),
			EnvironmentID: testEnvironmentID(),
			TaskSessionID: sessionID,
			Name:          "agent.report",
			Direction:     db.ChannelDirectionOutput,
		},
		channelRecords: []db.ChannelRecord{{
			ID:            recordID,
			OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:     testProjectID(),
			EnvironmentID: testEnvironmentID(),
			ChannelID:     channelID,
			Sequence:      5,
			Data:          []byte(`{"text":"ready"}`),
			CorrelationID: "thread-1",
			ContentType:   "application/json",
			CreatedAt:     testTime(),
		}},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+pgvalue.MustUUIDValue(sessionID).String()+"/channels/agent.report/outputs?after_sequence=4&limit=3&correlation_id=thread-1", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.listChannelRecords.AfterSequence != 4 || store.listChannelRecords.LimitCount != 3 || store.listChannelRecords.CorrelationID.String != "thread-1" {
		t.Fatalf("list params = %+v", store.listChannelRecords)
	}
}

func TestCreatePublicAccessTokenStoresSessionChannelScope(t *testing.T) {
	sessionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	channelID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
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
		taskSessionChannel: db.Channel{
			ID:            channelID,
			OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:     testProjectID(),
			EnvironmentID: testEnvironmentID(),
			TaskSessionID: sessionID,
			Name:          "approval",
			Direction:     db.ChannelDirectionInput,
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, AuthSecret: []byte(waitpointTokenTestAuthSecret)})
	body := `{"scope":{"type":"session.input.append","session_id":"` + pgvalue.MustUUIDValue(sessionID).String() + `","channel":"approval","correlation_id":"thread-1"},"max_uses":1}`
	req := httptest.NewRequest(http.MethodPost, "/api/public-access-tokens", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.PublicAccessTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !publicaccess.IsToken(response.PublicAccessToken) || response.Scope.Type != "session.input.append" || response.Scope.Channel != "approval" || response.Scope.CorrelationID != "thread-1" {
		t.Fatalf("response = %+v", response)
	}
	if bytes.Contains(store.createPublicAccessToken.TokenHash, []byte(response.PublicAccessToken)) {
		t.Fatal("stored token hash contains raw token")
	}
	if !store.createPublicAccessToken.MaxUses.Valid || store.createPublicAccessToken.MaxUses.Int32 != 1 {
		t.Fatalf("max uses = %+v", store.createPublicAccessToken.MaxUses)
	}
	if !publicAccessTokenAllowsChannel(store.createPublicAccessToken.AllowedScopes, "session.input.append", store.taskSessionChannel, "thread-1") {
		t.Fatal("created token does not authorize the requested session channel")
	}
}

func TestCreatePublicAccessTokenAllowsFutureOutputChannel(t *testing.T) {
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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, AuthSecret: []byte(waitpointTokenTestAuthSecret)})
	body := `{"scope":{"type":"session.output.read","session_id":"` + pgvalue.MustUUIDValue(sessionID).String() + `","channel":"agent.report"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/public-access-tokens", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !publicAccessTokenAllowsSessionChannel(store.createPublicAccessToken.AllowedScopes, "session.output.read", pgvalue.MustUUIDValue(sessionID).String(), "agent.report", "") {
		t.Fatal("created token does not authorize the future output channel")
	}
	var metadata struct {
		ChannelID string `json:"channelId,omitempty"`
		Channel   string `json:"channel"`
		Direction string `json:"direction"`
	}
	if err := json.Unmarshal(store.createPublicAccessToken.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.ChannelID != "" || metadata.Channel != "agent.report" || metadata.Direction != "output" {
		t.Fatalf("metadata = %+v", metadata)
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

type fakeWaitpoint struct {
	ID              pgtype.UUID
	RunSuspensionID pgtype.UUID
	OrgID           pgtype.UUID
	ProjectID       pgtype.UUID
	EnvironmentID   pgtype.UUID
	RunID           pgtype.UUID
	RunLeaseID      pgtype.UUID
	CheckpointID    pgtype.UUID
	CorrelationID   string
	Kind            db.WaitpointKind
	Request         []byte
	TimeoutSeconds  pgtype.Int4
	Status          db.RunSuspensionStatus
	ResolutionKind  pgtype.Text
	Output          []byte
	Resolution      []byte
	CreatedAt       pgtype.Timestamptz
	WaitingAt       pgtype.Timestamptz
	ResolvedAt      pgtype.Timestamptz
}

func runSuspensionID(waitpoint fakeWaitpoint) pgtype.UUID {
	if waitpoint.RunSuspensionID.Valid {
		return waitpoint.RunSuspensionID
	}
	return waitpoint.ID
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
	secret                                      db.GetScopedSecretMetadataByNameRow
	secrets                                     []db.ListScopedSecretsRow
	deleteSecret                                db.DeleteScopedSecretParams
	deleteSecretRows                            int64
	defaultProjectID                            pgtype.UUID
	defaultEnvironmentID                        pgtype.UUID
	logCursor                                   int64
	casObjects                                  []db.UpsertCasObjectParams
	getCasObjectErr                             error
	sessionID                                   pgtype.UUID
	executionWorkerInstanceID                   pgtype.UUID
	executionLeaseExpiresAt                     pgtype.Timestamptz
	waitpoint                                   fakeWaitpoint
	checkpoint                                  db.Checkpoint
	abandonedClaim                              bool
	workerBootstrapTokenHash                    []byte
	workerCredentialID                          pgtype.UUID
	workerCredentialSecretHash                  []byte
	dequeueRequest                              dispatch.DequeueRequest
	ackedLeases                                 []dispatch.Lease
	activeQueueLeaseMissing                     bool
	renewErr                                    error
	listQueueScopes                             db.ListQueueScopesParams
	waitpointToken                              db.WaitpointToken
	publicAccessToken                           db.PublicAccessToken
	createPublicAccessToken                     db.CreatePublicAccessTokenParams
	taskSession                                 db.TaskSession
	lockTaskSession                             db.TaskSession
	createTaskSessionErr                        error
	getTaskSessionByExternalIDMisses            int
	workspace                                   db.Workspace
	startIdempotency                            db.GetTaskStartIdempotencyRow
	appendSessionChannelInput                   db.AppendSessionChannelInputRow
	appendSessionChannelInputCalls              int
	taskSessionChannel                          db.Channel
	taskSessionChannels                         []db.Channel
	listChannelRecords                          db.ListChannelRecordsParams
	channelRecords                              []db.ChannelRecord
	taskSessionRuns                             []db.TaskSessionRun
	getWaitpointToken                           db.GetWaitpointTokenParams
	getWaitpointTokenForAuthenticatedCompletion db.GetWaitpointTokenForAuthenticatedCompletionParams
	getWaitpointTokenForPublicCompletion        db.GetWaitpointTokenForPublicCompletionParams
	lockPublicAccessTokenByHash                 []byte
	consumePublicAccessToken                    db.ConsumePublicAccessTokenParams
	getWaitpointTokenForCallbackCompletion      db.GetWaitpointTokenForCallbackCompletionParams
	completeWaitpointToken                      db.CompleteWaitpointTokenParams
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
		MaxDurationSeconds:    arg.MaxDurationSeconds,
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
		Metadata:            arg.Metadata,
		Tags:                arg.Tags,
		TerminalReason:      []byte(`{}`),
		ExpiresAt:           arg.ExpiresAt,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	return f.taskSession, nil
}

func (f *fakeStore) CreateTaskSessionWorkspace(_ context.Context, arg db.CreateTaskSessionWorkspaceParams) (db.CreateTaskSessionWorkspaceRow, error) {
	now := testTime()
	f.workspace = db.Workspace{
		ID:              arg.ID,
		OrgID:           arg.OrgID,
		ProjectID:       arg.ProjectID,
		EnvironmentID:   arg.EnvironmentID,
		TaskSessionID:   arg.TaskSessionID,
		State:           db.WorkspaceStateActive,
		RetentionPolicy: arg.RetentionPolicy,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	f.taskSession.WorkspaceID = arg.ID
	return db.CreateTaskSessionWorkspaceRow{
		ID:              f.workspace.ID,
		OrgID:           f.workspace.OrgID,
		ProjectID:       f.workspace.ProjectID,
		EnvironmentID:   f.workspace.EnvironmentID,
		TaskSessionID:   f.workspace.TaskSessionID,
		State:           f.workspace.State,
		RetentionPolicy: f.workspace.RetentionPolicy,
		CreatedAt:       f.workspace.CreatedAt,
		UpdatedAt:       f.workspace.UpdatedAt,
	}, nil
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

func (f *fakeStore) AppendSessionChannelInput(_ context.Context, arg db.AppendSessionChannelInputParams) (db.AppendSessionChannelInputRow, error) {
	f.appendSessionChannelInputCalls++
	row := f.appendSessionChannelInput
	if !row.ID.Valid {
		row = db.AppendSessionChannelInputRow{
			ID:                     arg.ID,
			OrgID:                  arg.OrgID,
			ProjectID:              f.taskSession.ProjectID,
			EnvironmentID:          f.taskSession.EnvironmentID,
			RunID:                  arg.RunID,
			ChannelID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Channel:                arg.Channel,
			Data:                   arg.Data,
			CorrelationID:          arg.CorrelationID,
			Sequence:               1,
			ContentType:            arg.ContentType,
			IdempotencyKey:         arg.IdempotencyKey,
			IdempotencyFingerprint: arg.IdempotencyFingerprint,
			ExternalEventID:        arg.ExternalEventID,
			AuthSubjectType:        arg.AuthSubjectType,
			AuthSubjectID:          arg.AuthSubjectID,
			Inserted:               true,
			CreatedAt:              testTime(),
		}
	} else {
		row.ExternalEventID = arg.ExternalEventID
		row.IdempotencyKey = arg.IdempotencyKey
		row.CorrelationID = arg.CorrelationID
		row.AuthSubjectType = arg.AuthSubjectType
		row.AuthSubjectID = arg.AuthSubjectID
	}
	f.appendSessionChannelInput = row
	return row, nil
}

func (f *fakeStore) GetExistingSessionChannelInputRecord(_ context.Context, arg db.GetExistingSessionChannelInputRecordParams) (db.GetExistingSessionChannelInputRecordRow, error) {
	row := f.appendSessionChannelInput
	if !row.ID.Valid ||
		row.OrgID != arg.OrgID ||
		f.taskSession.ID != arg.TaskSessionID ||
		row.Channel != arg.Channel ||
		row.IdempotencyFingerprint != arg.IdempotencyFingerprint ||
		(arg.IdempotencyKey == "" && arg.ExternalEventID == "") {
		return db.GetExistingSessionChannelInputRecordRow{}, pgx.ErrNoRows
	}
	if arg.IdempotencyKey != "" && row.IdempotencyKey != arg.IdempotencyKey {
		return db.GetExistingSessionChannelInputRecordRow{}, pgx.ErrNoRows
	}
	if arg.ExternalEventID != "" && row.ExternalEventID != arg.ExternalEventID {
		return db.GetExistingSessionChannelInputRecordRow{}, pgx.ErrNoRows
	}
	return db.GetExistingSessionChannelInputRecordRow{
		ID:                     row.ID,
		OrgID:                  row.OrgID,
		ProjectID:              row.ProjectID,
		EnvironmentID:          row.EnvironmentID,
		ChannelID:              row.ChannelID,
		Channel:                row.Channel,
		Data:                   row.Data,
		CorrelationID:          row.CorrelationID,
		Sequence:               row.Sequence,
		ContentType:            row.ContentType,
		IdempotencyKey:         row.IdempotencyKey,
		IdempotencyFingerprint: row.IdempotencyFingerprint,
		ExternalEventID:        row.ExternalEventID,
		AuthSubjectType:        row.AuthSubjectType,
		AuthSubjectID:          row.AuthSubjectID,
		CreatedAt:              row.CreatedAt,
	}, nil
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

func (f *fakeStore) GetTaskSessionChannelByName(_ context.Context, arg db.GetTaskSessionChannelByNameParams) (db.Channel, error) {
	if f.taskSessionChannel.ID.Valid &&
		f.taskSessionChannel.OrgID == arg.OrgID &&
		f.taskSessionChannel.ProjectID == arg.ProjectID &&
		f.taskSessionChannel.EnvironmentID == arg.EnvironmentID &&
		f.taskSessionChannel.TaskSessionID == arg.TaskSessionID &&
		f.taskSessionChannel.Name == arg.Name &&
		f.taskSessionChannel.Direction == db.ChannelDirection(arg.Direction) {
		return f.taskSessionChannel, nil
	}
	return db.Channel{}, pgx.ErrNoRows
}

func (f *fakeStore) ListTaskSessionChannels(_ context.Context, arg db.ListTaskSessionChannelsParams) ([]db.Channel, error) {
	channels := make([]db.Channel, 0, len(f.taskSessionChannels))
	for _, channel := range f.taskSessionChannels {
		if channel.OrgID == arg.OrgID &&
			channel.ProjectID == arg.ProjectID &&
			channel.EnvironmentID == arg.EnvironmentID &&
			channel.TaskSessionID == arg.TaskSessionID {
			channels = append(channels, channel)
		}
	}
	return channels, nil
}

func (f *fakeStore) ListChannelRecords(_ context.Context, arg db.ListChannelRecordsParams) ([]db.ChannelRecord, error) {
	f.listChannelRecords = arg
	return f.channelRecords, nil
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
			auth.PermissionRunWaitpointsRead,
			auth.PermissionChannelsWrite,
			auth.PermissionSecretsWrite,
			auth.PermissionWaitpointTokensCreate,
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
