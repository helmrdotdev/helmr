package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
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

func TestAPIKeysRequireOwnerSession(t *testing.T) {
	store := &apiKeyStore{role: db.OrgMemberRoleOwner}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{}, AuthSecret: []byte("abcdefghijabcdefghijabcdefghij12"), PublicURL: mustParseTestURL("https://helmr.example.test")})
	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+testProjectIDString()+"/environments/"+testEnvironmentIDString()+"/api-keys", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListAPIKeysFiltersAndShapesResponse(t *testing.T) {
	activeID := uuid.Must(uuid.NewV7())
	revokedID := uuid.Must(uuid.NewV7())
	store := &apiKeyStore{
		role: db.OrgMemberRoleOwner,
		keys: []db.ListAPIKeysRow{
			{
				ID:            pgvalue.UUID(activeID),
				OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
				ProjectID:     testProjectID(),
				EnvironmentID: testEnvironmentID(),
				Name:          "active key",
				KeyPrefix:     "hlmr_sk_abcdef12",
				CreatedAt:     testTime(),
			},
			{
				ID:            pgvalue.UUID(revokedID),
				OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
				ProjectID:     testProjectID(),
				EnvironmentID: testEnvironmentID(),
				Name:          "revoked key",
				KeyPrefix:     "hlmr_sk_revoked",
				CreatedAt:     testTime(),
				RevokedAt:     testTime(),
			},
		},
	}
	server := testAPIKeyServer(store)
	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+testProjectIDString()+"/environments/"+testEnvironmentIDString()+"/api-keys?filter=revoked", nil)
	addSessionCookie(req)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.listParams.StatusFilter != "revoked" || store.listParams.RowLimit != apiKeyListLimit+1 {
		t.Fatalf("list params = %+v", store.listParams)
	}
	if store.listParams.ProjectID != testProjectID() || store.listParams.EnvironmentID != testEnvironmentID() {
		t.Fatalf("list scope = %+v", store.listParams)
	}
	var response api.ListAPIKeysResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.HasMore || len(response.Items) != 1 {
		t.Fatalf("response = %+v", response)
	}
	item := response.Items[0]
	if item.ID != revokedID.String() || item.Name != "revoked key" || item.Status != api.APIKeyStatusRevoked || item.RevokedAt == nil {
		t.Fatalf("item = %+v", item)
	}
}

func TestIssueAPIKeyReturnsRawKeyOnce(t *testing.T) {
	store := &apiKeyStore{role: db.OrgMemberRoleOwner}
	server := testAPIKeyServer(store)
	req := httptest.NewRequest(http.MethodPost, "/api/projects/"+testProjectIDString()+"/environments/"+testEnvironmentIDString()+"/api-keys", strings.NewReader(`{"name":"deploy","expires_in_days":30,"permissions":[{"scopes":["runs:create","runs:read"]}]}`))
	addSessionCookie(req)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.upsert.Name != "deploy" || !store.upsert.ExpiresAt.Valid || len(store.upsert.TokenHash) == 0 {
		t.Fatalf("upsert = %+v", store.upsert)
	}
	var issued api.APIKeyIssued
	if err := json.Unmarshal(rec.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.RawKey == "" || !strings.HasPrefix(issued.RawKey, auth.APIKeyPrefix) {
		t.Fatalf("raw key = %q", issued.RawKey)
	}
	if issued.KeyPrefix != store.upsert.KeyPrefix || issued.Name != "deploy" || issued.Status != api.APIKeyStatusActive {
		t.Fatalf("issued = %+v", issued)
	}
	if issued.ProjectID != testProjectIDString() || issued.EnvironmentID != testEnvironmentIDString() {
		t.Fatalf("issued scope = %+v", issued)
	}
	if store.upsert.ProjectID != testProjectID() || store.upsert.EnvironmentID != testEnvironmentID() {
		t.Fatalf("token scope = %+v", store.upsert)
	}
	if len(issued.Permissions) != 1 || len(issued.Permissions[0].Scopes) != 2 || issued.Permissions[0].Scopes[0] != api.APIKeyScopeRunsCreate {
		t.Fatalf("permissions = %+v", issued.Permissions)
	}
	if len(store.grants) != 2 || store.grants[0].Permission != string(auth.PermissionRunsCreate) {
		t.Fatalf("grants = %+v", store.grants)
	}
}

func TestIssueAPIKeySupportsSecretsWrite(t *testing.T) {
	store := &apiKeyStore{role: db.OrgMemberRoleOwner}
	server := testAPIKeyServer(store)
	req := httptest.NewRequest(http.MethodPost, "/api/projects/"+testProjectIDString()+"/environments/"+testEnvironmentIDString()+"/api-keys", strings.NewReader(`{"name":"secret-sync","expires_in_days":30,"permissions":[{"scopes":["secrets:write"]}]}`))
	addSessionCookie(req)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var issued api.APIKeyIssued
	if err := json.Unmarshal(rec.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if len(issued.Permissions) != 1 || len(issued.Permissions[0].Scopes) != 1 || issued.Permissions[0].Scopes[0] != api.APIKeyScopeSecretsWrite {
		t.Fatalf("permissions = %+v", issued.Permissions)
	}
	if len(store.grants) != 1 || store.grants[0].Permission != string(auth.PermissionSecretsWrite) {
		t.Fatalf("grants = %+v", store.grants)
	}
	if store.upsert.ProjectID != testProjectID() || store.upsert.EnvironmentID != testEnvironmentID() {
		t.Fatalf("token scope = %+v", store.upsert)
	}
}

func TestIssueAPIKeySupportsTasksDeploy(t *testing.T) {
	store := &apiKeyStore{role: db.OrgMemberRoleOwner}
	server := testAPIKeyServer(store)
	req := httptest.NewRequest(http.MethodPost, "/api/projects/"+testProjectIDString()+"/environments/"+testEnvironmentIDString()+"/api-keys", strings.NewReader(`{"name":"deploy","expires_in_days":30,"permissions":[{"scopes":["tasks:deploy"]}]}`))
	addSessionCookie(req)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var issued api.APIKeyIssued
	if err := json.Unmarshal(rec.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if len(issued.Permissions) != 1 || len(issued.Permissions[0].Scopes) != 1 || issued.Permissions[0].Scopes[0] != api.APIKeyScopeTasksDeploy {
		t.Fatalf("permissions = %+v", issued.Permissions)
	}
	if len(store.grants) != 1 || store.grants[0].Permission != string(auth.PermissionTasksDeploy) {
		t.Fatalf("grants = %+v", store.grants)
	}
	if store.upsert.ProjectID != testProjectID() || store.upsert.EnvironmentID != testEnvironmentID() {
		t.Fatalf("token scope = %+v", store.upsert)
	}
}

func TestIssueAPIKeySupportsChannelScopes(t *testing.T) {
	store := &apiKeyStore{role: db.OrgMemberRoleOwner}
	server := testAPIKeyServer(store)
	req := httptest.NewRequest(http.MethodPost, "/api/projects/"+testProjectIDString()+"/environments/"+testEnvironmentIDString()+"/api-keys", strings.NewReader(`{"name":"channels","expires_in_days":30,"permissions":[{"scopes":["run-waitpoints:read","channels:write","waitpoint-tokens:create"]}]}`))
	addSessionCookie(req)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	got := make([]string, 0, len(store.grants))
	for _, grant := range store.grants {
		got = append(got, grant.Permission)
	}
	want := []string{
		string(auth.PermissionRunWaitpointsRead),
		string(auth.PermissionChannelsWrite),
		string(auth.PermissionWaitpointTokensCreate),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("grants = %+v, want %+v", got, want)
	}
}

func TestRevokeAPIKeyReturnsNoContentAndNotFoundEnvelope(t *testing.T) {
	keyID := uuid.Must(uuid.NewV7())
	store := &apiKeyStore{role: db.OrgMemberRoleOwner, revokeRows: 1}
	server := testAPIKeyServer(store)
	req := httptest.NewRequest(http.MethodDelete, "/api/projects/"+testProjectIDString()+"/environments/"+testEnvironmentIDString()+"/api-keys/"+keyID.String(), nil)
	addSessionCookie(req)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.revokeParams.ID != pgvalue.UUID(keyID) || store.revokeParams.ProjectID != testProjectID() || store.revokeParams.EnvironmentID != testEnvironmentID() {
		t.Fatalf("revoke params = %+v", store.revokeParams)
	}

	store.revokeRows = 0
	req = httptest.NewRequest(http.MethodDelete, "/api/projects/"+testProjectIDString()+"/environments/"+testEnvironmentIDString()+"/api-keys/"+uuid.Must(uuid.NewV7()).String(), nil)
	addSessionCookie(req)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error == "" {
		t.Fatalf("envelope = %+v", envelope)
	}
}

type apiKeyStore struct {
	db.Querier
	role         db.OrgMemberRole
	keys         []db.ListAPIKeysRow
	listParams   db.ListAPIKeysParams
	upsert       db.IssueAPIKeyParams
	grants       []db.CreateAPIKeyGrantParams
	revokeParams db.RevokeAPIKeyParams
	revokeRows   int64
}

func (s *apiKeyStore) GetSessionByTokenHash(context.Context, []byte) (db.GetSessionByTokenHashRow, error) {
	return db.GetSessionByTokenHashRow{
		ID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
		UserID:    pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Role:      string(s.role),
		ExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	}, nil
}

func (s *apiKeyStore) RefreshSession(context.Context, db.RefreshSessionParams) error {
	return nil
}

func (s *apiKeyStore) ListAPIKeys(_ context.Context, arg db.ListAPIKeysParams) ([]db.ListAPIKeysRow, error) {
	s.listParams = arg
	rows := make([]db.ListAPIKeysRow, 0, len(s.keys))
	for _, key := range s.keys {
		if key.ProjectID == arg.ProjectID &&
			key.EnvironmentID == arg.EnvironmentID &&
			(arg.StatusFilter == "all" || string(apiKeyStatusForTest(key.ExpiresAt, key.RevokedAt)) == arg.StatusFilter) {
			rows = append(rows, key)
		}
	}
	if int32(len(rows)) > arg.RowLimit {
		rows = rows[:arg.RowLimit]
	}
	return rows, nil
}

func (s *apiKeyStore) IssueAPIKey(_ context.Context, arg db.IssueAPIKeyParams) (db.APIKey, error) {
	s.upsert = arg
	return db.APIKey{
		ID:              arg.ID,
		OrgID:           arg.OrgID,
		ProjectID:       arg.ProjectID,
		EnvironmentID:   arg.EnvironmentID,
		CreatedByUserID: arg.CreatedByUserID,
		Name:            arg.Name,
		KeyPrefix:       arg.KeyPrefix,
		TokenHash:       arg.TokenHash,
		CreatedAt:       testTime(),
		ExpiresAt:       arg.ExpiresAt,
	}, nil
}

func (s *apiKeyStore) CreateAPIKeyGrant(_ context.Context, arg db.CreateAPIKeyGrantParams) (db.ApiKeyGrant, error) {
	s.grants = append(s.grants, arg)
	return db.ApiKeyGrant{
		ID:              arg.ID,
		OrgID:           arg.OrgID,
		ApiKeyID:        arg.ApiKeyID,
		Permission:      arg.Permission,
		CreatedByUserID: arg.CreatedByUserID,
		CreatedAt:       testTime(),
	}, nil
}

func (s *apiKeyStore) GetDefaultProjectEnvironment(context.Context, pgtype.UUID) (db.GetDefaultProjectEnvironmentRow, error) {
	return db.GetDefaultProjectEnvironmentRow{
		ProjectID:     testProjectID(),
		EnvironmentID: testEnvironmentID(),
	}, nil
}

func (s *apiKeyStore) GetProject(_ context.Context, arg db.GetProjectParams) (db.Project, error) {
	if arg.ID != testProjectID() {
		return db.Project{}, pgx.ErrNoRows
	}
	return db.Project{
		ID:        testProjectID(),
		OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
		Slug:      "helmr",
		Name:      "Helmr",
		IsDefault: true,
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (s *apiKeyStore) GetDefaultEnvironment(_ context.Context, arg db.GetDefaultEnvironmentParams) (db.Environment, error) {
	if arg.ProjectID != testProjectID() {
		return db.Environment{}, pgx.ErrNoRows
	}
	return db.Environment{
		ID:        testEnvironmentID(),
		OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID: testProjectID(),
		Slug:      "production",
		Name:      "Production",
		IsDefault: true,
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (s *apiKeyStore) GetEnvironment(_ context.Context, arg db.GetEnvironmentParams) (db.Environment, error) {
	if arg.ID != testEnvironmentID() || arg.ProjectID != testProjectID() {
		return db.Environment{}, pgx.ErrNoRows
	}
	return db.Environment{
		ID:        testEnvironmentID(),
		OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID: testProjectID(),
		Slug:      "production",
		Name:      "Production",
		IsDefault: true,
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (s *apiKeyStore) ListAPIKeyGrants(_ context.Context, arg db.ListAPIKeyGrantsParams) ([]db.ApiKeyGrant, error) {
	rows := make([]db.ApiKeyGrant, 0, len(s.grants))
	for _, grant := range s.grants {
		if grant.OrgID == arg.OrgID && grant.ApiKeyID == arg.ApiKeyID {
			rows = append(rows, db.ApiKeyGrant{
				ID:              grant.ID,
				OrgID:           grant.OrgID,
				ApiKeyID:        grant.ApiKeyID,
				Permission:      grant.Permission,
				CreatedByUserID: grant.CreatedByUserID,
				CreatedAt:       testTime(),
			})
		}
	}
	return rows, nil
}

func (s *apiKeyStore) RevokeAPIKey(_ context.Context, arg db.RevokeAPIKeyParams) (int64, error) {
	s.revokeParams = arg
	return s.revokeRows, nil
}

func testAPIKeyServer(store *apiKeyStore) http.Handler {
	return newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, AuthSecret: []byte("abcdefghijabcdefghijabcdefghij12"), PublicURL: mustParseTestURL("https://helmr.example.test")})
}

func addSessionCookie(req *http.Request) {
	req.AddCookie(&http.Cookie{Name: sessionCookieName(req), Value: "raw-session"})
}

func apiKeyStatusForTest(expiresAt pgtype.Timestamptz, revokedAt pgtype.Timestamptz) api.APIKeyStatus {
	if revokedAt.Valid {
		return api.APIKeyStatusRevoked
	}
	if expiresAt.Valid && !expiresAt.Time.After(time.Now()) {
		return api.APIKeyStatusExpired
	}
	return api.APIKeyStatusActive
}
