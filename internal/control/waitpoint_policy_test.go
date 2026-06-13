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

func TestGetWaitpointPolicyRoute(t *testing.T) {
	store := &waitpointPolicyStore{
		policy: db.WaitpointPolicy{
			ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:     testProjectID(),
			EnvironmentID: testEnvironmentID(),
			Name:          "deploy-prod",
			Label:         "Production deploy",
			Config:        []byte(`{"deliveries":[{"type":"email","to":["sre@example.test"]}]}`),
			CreatedAt:     testTime(),
			UpdatedAt:     testTime(),
		},
	}
	server := testWaitpointPolicyServer(store)
	req := httptest.NewRequest(http.MethodGet, "/api/waitpoint-policies/deploy-prod", nil)
	req.Header.Set("authorization", "Bearer "+auth.APIKeyPrefix+"test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.get.Name != "deploy-prod" || store.get.OrgID != pgvalue.UUID(dbtest.DefaultOrgID) || store.get.ProjectID != testProjectID() || store.get.EnvironmentID != testEnvironmentID() {
		t.Fatalf("get = %+v", store.get)
	}
	var response api.WaitpointPolicyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Name != "deploy-prod" || response.Label != "Production deploy" {
		t.Fatalf("response = %+v", response)
	}
	if !strings.Contains(string(response.Config), "sre@example.test") {
		t.Fatalf("config = %s", response.Config)
	}
}

func TestGetWaitpointPolicyRouteReturnsNotFound(t *testing.T) {
	store := &waitpointPolicyStore{err: pgx.ErrNoRows}
	server := testWaitpointPolicyServer(store)
	req := httptest.NewRequest(http.MethodGet, "/api/waitpoint-policies/missing-policy", nil)
	req.Header.Set("authorization", "Bearer "+auth.APIKeyPrefix+"test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetWaitpointPolicyRouteValidatesName(t *testing.T) {
	store := &waitpointPolicyStore{}
	server := testWaitpointPolicyServer(store)
	req := httptest.NewRequest(http.MethodGet, "/api/waitpoint-policies/bad%20name", nil)
	req.Header.Set("authorization", "Bearer "+auth.APIKeyPrefix+"test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.called {
		t.Fatal("store was called")
	}
}

func TestWaitpointPolicyRoutesRequirePermission(t *testing.T) {
	for _, tt := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "list", method: http.MethodGet, path: "/api/waitpoint-policies"},
		{name: "get", method: http.MethodGet, path: "/api/waitpoint-policies/deploy-prod"},
		{name: "create", method: http.MethodPost, path: "/api/waitpoint-policies", body: `{"name":"deploy-prod","label":"Deploy","config":{}}`},
		{name: "update", method: http.MethodPatch, path: "/api/waitpoint-policies/deploy-prod", body: `{"label":"Deploy","config":{}}`},
		{name: "delete", method: http.MethodDelete, path: "/api/waitpoint-policies/deploy-prod"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &waitpointPolicyStore{}
			server := testWaitpointPolicyServerWithPermissions(store, nil)
			req := httptest.NewRequest(tt.method, tt.path, bytes.NewBufferString(tt.body))
			req.Header.Set("authorization", "Bearer "+auth.APIKeyPrefix+"test-key")
			if tt.body != "" {
				req.Header.Set("content-type", "application/json")
			}
			rec := httptest.NewRecorder()

			server.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if store.called {
				t.Fatal("store was called")
			}
		})
	}
}

func TestWaitpointPolicyRoutesAllowSessionOwner(t *testing.T) {
	basePath := "/api/projects/" + testProjectIDString() + "/environments/" + testEnvironmentIDString() + "/waitpoint-policies"
	for _, tt := range []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{name: "list", method: http.MethodGet, path: basePath, wantStatus: http.StatusOK},
		{name: "get", method: http.MethodGet, path: basePath + "/deploy-prod", wantStatus: http.StatusOK},
		{name: "create", method: http.MethodPost, path: basePath, body: `{"name":"deploy-prod","label":"Deploy","config":{}}`, wantStatus: http.StatusCreated},
		{name: "update", method: http.MethodPatch, path: basePath + "/deploy-prod", body: `{"label":"Deploy","config":{}}`, wantStatus: http.StatusOK},
		{name: "delete", method: http.MethodDelete, path: basePath + "/deploy-prod", wantStatus: http.StatusNoContent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &waitpointPolicyStore{policy: testWaitpointPolicy()}
			server := testWaitpointPolicySessionServer(store, auth.RoleOwner)
			req := httptest.NewRequest(tt.method, tt.path, bytes.NewBufferString(tt.body))
			addSessionCookie(req)
			if tt.body != "" {
				req.Header.Set("content-type", "application/json")
			}
			rec := httptest.NewRecorder()

			server.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if !store.called {
				t.Fatal("store was not called")
			}
		})
	}
}

func TestWaitpointPolicyRoutesRejectSessionWithoutPathScope(t *testing.T) {
	store := &waitpointPolicyStore{}
	server := testWaitpointPolicySessionServer(store, auth.RoleOwner)
	req := httptest.NewRequest(http.MethodGet, "/api/waitpoint-policies", nil)
	addSessionCookie(req)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.called {
		t.Fatal("store was called")
	}
}

func testWaitpointPolicyServer(store *waitpointPolicyStore) http.Handler {
	return testWaitpointPolicyServerWithPermissions(store, []auth.Permission{auth.PermissionWaitpointPolicies})
}

func testWaitpointPolicyServerWithPermissions(store *waitpointPolicyStore, permissions []auth.Permission) http.Handler {
	return newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{
		kind:          auth.ActorKindAPIKey,
		projectID:     testProjectIDString(),
		environmentID: testEnvironmentIDString(),
		permissions:   permissions,
	}},
	)
}

func testWaitpointPolicySessionServer(store *waitpointPolicyStore, role auth.Role) http.Handler {
	return newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{
		kind: auth.ActorKindSession,
		role: role,
	}, AuthSecret: []byte("abcdefghijabcdefghijabcdefghij12"), PublicURL: mustParseTestURL("https://helmr.example.test")},
	)
}

func testWaitpointPolicy() db.WaitpointPolicy {
	return db.WaitpointPolicy{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:     testProjectID(),
		EnvironmentID: testEnvironmentID(),
		Name:          "deploy-prod",
		Label:         "Production deploy",
		Config:        []byte(`{}`),
		CreatedAt:     testTime(),
		UpdatedAt:     testTime(),
	}
}

type waitpointPolicyStore struct {
	db.Querier
	policy db.WaitpointPolicy
	err    error
	get    db.GetWaitpointPolicyByNameParams
	called bool
}

func (s *waitpointPolicyStore) GetProject(_ context.Context, arg db.GetProjectParams) (db.Project, error) {
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

func (s *waitpointPolicyStore) GetSessionByTokenHash(context.Context, []byte) (db.GetSessionByTokenHashRow, error) {
	return db.GetSessionByTokenHashRow{
		ID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
		UserID:    pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Role:      string(db.OrgMemberRoleOwner),
		ExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	}, nil
}

func (s *waitpointPolicyStore) RefreshSession(context.Context, db.RefreshSessionParams) error {
	return nil
}

func (s *waitpointPolicyStore) GetEnvironment(_ context.Context, arg db.GetEnvironmentParams) (db.Environment, error) {
	if arg.ID != testEnvironmentID() || arg.ProjectID != testProjectID() {
		return db.Environment{}, pgx.ErrNoRows
	}
	return db.Environment{
		ID:        arg.ID,
		OrgID:     arg.OrgID,
		ProjectID: arg.ProjectID,
		Slug:      "production",
		Name:      "Production",
		ColorHex:  "#4F46E5",
		IsDefault: true,
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (s *waitpointPolicyStore) ListWaitpointPolicies(_ context.Context, _ db.ListWaitpointPoliciesParams) ([]db.WaitpointPolicy, error) {
	s.called = true
	if s.policy.ID == (pgtype.UUID{}) {
		return nil, nil
	}
	return []db.WaitpointPolicy{s.policy}, nil
}

func (s *waitpointPolicyStore) CreateWaitpointPolicy(_ context.Context, _ db.CreateWaitpointPolicyParams) (db.WaitpointPolicy, error) {
	s.called = true
	return s.policy, nil
}

func (s *waitpointPolicyStore) GetWaitpointPolicyByName(_ context.Context, arg db.GetWaitpointPolicyByNameParams) (db.WaitpointPolicy, error) {
	s.called = true
	s.get = arg
	if s.err != nil {
		return db.WaitpointPolicy{}, s.err
	}
	if s.policy.ID == (pgtype.UUID{}) {
		return db.WaitpointPolicy{}, pgx.ErrNoRows
	}
	return s.policy, nil
}

func (s *waitpointPolicyStore) UpdateWaitpointPolicy(_ context.Context, _ db.UpdateWaitpointPolicyParams) (db.WaitpointPolicy, error) {
	s.called = true
	return s.policy, nil
}

func (s *waitpointPolicyStore) DeleteWaitpointPolicy(_ context.Context, _ db.DeleteWaitpointPolicyParams) (int64, error) {
	s.called = true
	return 1, nil
}
