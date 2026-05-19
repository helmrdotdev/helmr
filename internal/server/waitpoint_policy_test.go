package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestGetWaitpointPolicyRoute(t *testing.T) {
	store := &waitpointPolicyStore{
		policy: db.WaitpointPolicy{
			ID:        ids.ToPG(ids.New()),
			OrgID:     ids.ToPG(ids.DefaultOrgID),
			Name:      "deploy-prod",
			Label:     "Production deploy",
			Config:    []byte(`{"deliveries":[{"type":"email","to":["sre@example.test"]}],"resolution":{"type":"any","count":1}}`),
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
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
	if store.get.Name != "deploy-prod" || store.get.OrgID != ids.ToPG(ids.DefaultOrgID) {
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

func testWaitpointPolicyServer(store *waitpointPolicyStore) http.Handler {
	return New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{
			kind: auth.ActorKindAPIKey,
			permissions: []auth.PermissionGrant{{
				ProjectID:     auth.DefaultProjectID,
				EnvironmentID: auth.DefaultEnvironmentID,
				Permissions:   []auth.Permission{auth.PermissionWaitpointPolicies},
			}},
		}),
	)
}

type waitpointPolicyStore struct {
	db.Querier
	policy db.WaitpointPolicy
	err    error
	get    db.GetWaitpointPolicyByNameParams
	called bool
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
