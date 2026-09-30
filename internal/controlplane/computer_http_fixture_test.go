package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgxpool"
)

// workerHTTPClient calls /worker/v1 routes of a NewServer handler with an
// epoch token exchanged for a seeded worker host's secret.
type workerHTTPClient struct {
	handler http.Handler
	token   string
}

func newWorkerHTTPClient(t *testing.T, handler http.Handler, pool *pgxpool.Pool, hostID uuid.UUID) workerHTTPClient {
	t.Helper()
	credential := seedHostCredential(t, pool, hostID)
	return workerHTTPClient{handler: handler, token: credential.token(t, handler)}
}

// post sends body as JSON, requires the status and decodes a 200 response
// into out when out is non-nil.
func (c workerHTTPClient) post(t *testing.T, path string, body any, want int, out any) *httptest.ResponseRecorder {
	t.Helper()
	response := c.send(t, path, body)
	if response.Code != want {
		t.Fatalf("POST %s status = %d, want %d: %s", path, response.Code, want, response.Body.String())
	}
	if out != nil && response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	return response
}

// send sends body as JSON and returns the response whatever its status.
func (c workerHTTPClient) send(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	c.handler.ServeHTTP(response, request)
	return response
}

// issueEnvironmentAPIKey issues an API key bound to an environment through an
// owner seeded into an organization that a fixture inserted directly.
func issueEnvironmentAPIKey(t *testing.T, pool *pgxpool.Pool, orgID, projectID, environmentID uuid.UUID, permissions ...auth.Permission) string {
	t.Helper()
	queries := db.New(pool)
	userID := uuid.NewV7()
	if _, err := pool.Exec(t.Context(), `INSERT INTO users (id, display_name) VALUES ($1, 'Owner')`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.EnsureOrgMember(t.Context(), db.EnsureOrgMemberParams{
		OrgID: pgvalue.UUID(orgID), UserID: pgvalue.UUID(userID), Role: db.OrgMemberRoleOwner,
	}); err != nil {
		t.Fatal(err)
	}
	owner := auth.Principal{OrgID: orgID, UserID: userID, Kind: auth.PrincipalKindSession, Role: auth.RoleOwner}
	scope := auth.Scope{OrgID: orgID, ProjectID: projectID.String(), EnvironmentID: environmentID.String()}
	issued, err := identity.IssueAPIKey(t.Context(), queries, owner, scope, identity.APIKeyInput{
		Name: "computers-" + userID.String(), Permissions: permissions,
	})
	if err != nil {
		t.Fatal(err)
	}
	return issued.Raw
}

// serveAPIKey serves a public request authenticated by an API key.
func serveAPIKey(handler http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
