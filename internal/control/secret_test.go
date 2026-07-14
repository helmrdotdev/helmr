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
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{role: auth.RoleDeveloper}, CAS: &fakeCAS{}, Secrets: fakeSecrets{}})
	bodyBytes, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(bodyBytes))
	req.Header.Set("authorization", "Bearer developer-key")
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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{
		kind:          auth.ActorKindAPIKey,
		projectID:     testProjectIDString(),
		environmentID: testEnvironmentIDString(),
		permissions:   []auth.Permission{auth.PermissionRunsCreate},
	}, CAS: &fakeCAS{}, Secrets: fakeSecrets{values: api.ResolvedSecrets{"API_KEY": []byte("secret-value")}}},
	)
	bodyBytes, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(bodyBytes))
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

func TestCreateScheduleRunUsesDeclaredTaskSecrets(t *testing.T) {
	scheduleID := uuid.Must(uuid.NewV7())
	instanceID := uuid.Must(uuid.NewV7())
	scheduledAt := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{
		currentDeploymentTaskSecretDeclarations: []byte(`[{"name":"API_KEY","env":"API_KEY"}]`),
	}
	runEnqueuer := &fakeRunEnqueuer{}
	server := &Server{
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:              store,
		cas:             &fakeCAS{},
		secrets:         fakeSecrets{values: api.ResolvedSecrets{"API_KEY": []byte("secret-value")}},
		runEnqueuer:     runEnqueuer,
		eventStream:     newTestEventStream(t),
		workerGroupID:   "us-east-1-worker-group-1",
		defaultRegionID: "us-east-1",
	}
	runID, err := server.CreateScheduleRun(context.Background(), db.GetScheduleTriggerCandidateRow{
		OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:     testProjectID(),
		EnvironmentID: testEnvironmentID(),
		ScheduleID:    pgvalue.UUID(scheduleID),
		InstanceID:    pgvalue.UUID(instanceID),
		ScheduleType:  db.TaskScheduleTypeImperative,
		TaskID:        "deploy",
		Cron:          "0 9 * * *",
		Timezone:      "UTC",
		RunOptions:    []byte(`{}`),
		Generation:    1,
		NextFireAt:    pgtype.Timestamptz{Time: scheduledAt, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if runID != store.run.ID || runEnqueuer.count != 1 {
		t.Fatalf("runID=%+v stored=%+v enqueues=%d", runID, store.run.ID, runEnqueuer.count)
	}
	if store.createRun.ScheduleID != pgvalue.UUID(scheduleID) || store.createRun.ScheduleInstanceID != pgvalue.UUID(instanceID) {
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

func TestCreateRunRejectsUnavailableDeclaredSecret(t *testing.T) {
	store := &fakeStore{
		currentDeploymentTaskSecretDeclarations: []byte(`[{"name":"API_KEY","env":"API_KEY"}]`),
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{values: api.ResolvedSecrets{"other": []byte("secret")}}})
	bodyBytes, err := json.Marshal(api.SessionStartRequest{TaskID: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(bodyBytes))
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

func TestSetSecret(t *testing.T) {
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: &fakeStore{}, Auth: fakeAuth{}, Secrets: fakeSecrets{}})

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
			ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:     testProjectID(),
			EnvironmentID: testEnvironmentID(),
			Name:          "github-token",
			CreatedAt:     testTime(),
			UpdatedAt:     testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}})

	req := httptest.NewRequest(http.MethodGet, "/api/secrets/github-token", nil)
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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: &fakeStore{}, Auth: fakeAuth{role: auth.RoleDeveloper}, Secrets: fakeSecrets{}})

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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, Secrets: fakeSecrets{}})

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/github-token", nil)
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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: &fakeStore{}, Auth: fakeAuth{}, Secrets: fakeSecrets{}})

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
			ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{
		kind:          auth.ActorKindAPIKey,
		role:          auth.RoleOwner,
		projectID:     testProjectIDString(),
		environmentID: testEnvironmentIDString(),
		permissions:   []auth.Permission{auth.PermissionSecretsWrite},
	}, Secrets: fakeSecrets{}},
	)

	for _, tt := range []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{
			name:       "list",
			method:     http.MethodGet,
			path:       "/api/secrets",
			wantStatus: http.StatusOK,
		},
		{
			name:       "get",
			method:     http.MethodGet,
			path:       "/api/secrets/github-token",
			wantStatus: http.StatusOK,
		},
		{
			name:       "set",
			method:     http.MethodPut,
			path:       "/api/secrets/github-token",
			body:       `{"value":"secret-value"}`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "delete",
			method:     http.MethodDelete,
			path:       "/api/secrets/github-token",
			wantStatus: http.StatusNoContent,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set("authorization", "Bearer test-key")
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestWorkerTokenRejectsWrongSecret(t *testing.T) {
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: &fakeStore{}, Auth: fakeAuth{}, WorkerTokenSecret: []byte(testWorkerTokenSecret), WorkerTokenTTL: time.Hour, AuthSecret: []byte(testWorkerTokenSecret), PublicURL: mustParseTestURL("http://127.0.0.1:8080")})

	req := httptest.NewRequest(http.MethodPost, "/api/worker/auth/token", bytes.NewBufferString(`{"worker_instance_id":"00000000-0000-0000-0000-000000000401","worker_instance_secret":"wrong","service_id":"00000000-0000-0000-0000-000000000402","protocol_version":"helmr.worker.v0","supports_run":true,"supports_build":false}`))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

type workerTokenAuthorityStore struct {
	*fakeStore
	row db.AuthenticateWorkerInstanceCredentialRow
}

func (s *workerTokenAuthorityStore) AuthenticateWorkerInstanceCredential(context.Context, db.AuthenticateWorkerInstanceCredentialParams) (db.AuthenticateWorkerInstanceCredentialRow, error) {
	return s.row, nil
}

func TestWorkerTokenExchangeUsesSharedRoleAuthority(t *testing.T) {
	workerID := uuid.MustParse("00000000-0000-0000-0000-000000000401")
	credentialID := uuid.MustParse("00000000-0000-0000-0000-000000000403")
	store := &workerTokenAuthorityStore{fakeStore: &fakeStore{}, row: db.AuthenticateWorkerInstanceCredentialRow{
		ID: pgvalue.UUID(credentialID), WorkerInstanceID: pgvalue.UUID(workerID), WorkerGroupID: "run-workers",
		ClaimVersion: 2, GroupClaimVersion: 4, ProtocolVersion: auth.WorkerProtocolVersion,
		CredentialAllowsRun: true, CredentialAllowsBuild: true,
		GroupAllowsRun: true, GroupAllowsBuild: false,
		CurrentEpoch: pgtype.Int8{Int64: 7, Valid: true},
	}}
	secret := []byte(testWorkerTokenSecret)
	server := newTestServer(testServerConfig{DB: store, Auth: fakeAuth{}, WorkerTokenSecret: secret,
		WorkerTokenTTL: time.Hour, AuthSecret: secret, PublicURL: mustParseTestURL("http://127.0.0.1:8080")})
	req := httptest.NewRequest(http.MethodPost, "/api/worker/auth/token", bytes.NewBufferString(`{"worker_instance_id":"00000000-0000-0000-0000-000000000401","worker_instance_secret":"secret","service_id":"00000000-0000-0000-0000-000000000402","protocol_version":"helmr.worker.v0","supports_run":true,"supports_build":true}`))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.WorkerTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	claims, err := auth.VerifyWorkerToken(secret, response.Token, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(claims.Roles) != 1 || claims.Roles[0] != auth.WorkerRoleRun || claims.WorkerEpoch != 7 || claims.ClaimVersion != 2 || claims.GroupClaimVersion != 4 {
		t.Fatalf("claims = %#v", claims)
	}
}

type workerLivenessStore struct {
	*fakeStore
	authenticated bool
}

func (s *workerLivenessStore) AuthenticateWorkerInstanceCredential(context.Context, db.AuthenticateWorkerInstanceCredentialParams) (db.AuthenticateWorkerInstanceCredentialRow, error) {
	s.authenticated = true
	return db.AuthenticateWorkerInstanceCredentialRow{}, pgx.ErrNoRows
}

func TestWorkerTokenExchangeUsesCredentialAuthorityWithoutProviderLivenessCall(t *testing.T) {
	store := &workerLivenessStore{fakeStore: &fakeStore{}}
	server := newTestServer(testServerConfig{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{},
		WorkerTokenSecret: []byte(testWorkerTokenSecret), WorkerTokenTTL: time.Hour,
		AuthSecret: []byte(testWorkerTokenSecret), PublicURL: mustParseTestURL("http://127.0.0.1:8080"),
		WorkerEnrollment: fixedWorkerEnrollmentVerifier{},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/worker/auth/token", bytes.NewBufferString(`{"worker_instance_id":"00000000-0000-0000-0000-000000000401","worker_instance_secret":"secret","service_id":"00000000-0000-0000-0000-000000000402","protocol_version":"helmr.worker.v0","supports_run":true,"supports_build":false}`))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !store.authenticated {
		t.Fatal("worker credential authority was bypassed")
	}
}

func (f *fakeStore) AuthenticateWorkerInstanceCredential(context.Context, db.AuthenticateWorkerInstanceCredentialParams) (db.AuthenticateWorkerInstanceCredentialRow, error) {
	return db.AuthenticateWorkerInstanceCredentialRow{}, pgx.ErrNoRows
}

func (f fakeSecrets) PutScoped(_ context.Context, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, name string, value []byte) (db.Secret, error) {
	return db.Secret{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     pgvalue.UUID(projectID),
		EnvironmentID: pgvalue.UUID(environmentID),
		Name:          name,
		Ciphertext:    append([]byte(nil), value...),
		CreatedAt:     testTime(),
		UpdatedAt:     testTime(),
	}, nil
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
