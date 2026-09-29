package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests cover the HTTP contract of the project handlers; the durable
// behavior behind them is tested in internal/org.

func TestProjectHTTPPostgresListAndDetailContract(t *testing.T) {
	fixture := newOrgHTTPFixture(t)
	queries := db.New(fixture.pool)
	if _, err := queries.CreateRegion(t.Context(), db.CreateRegionParams{ID: "project-http", DisplayName: "Project HTTP"}); err != nil {
		t.Fatal(err)
	}
	orgID, ownerToken := fixture.organizationOwner(t, "project-http")
	_, otherOwnerToken := fixture.organizationOwner(t, "project-http-other")
	projectRows := make([][]any, 0, 3)
	environmentRows := make([][]any, 0, 3)
	for index := range 3 {
		projectID := uuid.NewV7()
		projectRows = append(projectRows, []any{projectID, orgID, "project-http", fmt.Sprintf("project-%d", index), fmt.Sprintf("Project %d", index), index == 0})
		environmentRows = append(environmentRows, []any{uuid.NewV7(), orgID, projectID, "production", "Production", "#315FCE", true})
	}
	if _, err := fixture.pool.CopyFrom(t.Context(), pgx.Identifier{"projects"},
		[]string{"id", "org_id", "default_region_id", "slug", "name", "is_default"}, pgx.CopyFromRows(projectRows)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.CopyFrom(t.Context(), pgx.Identifier{"environments"},
		[]string{"id", "org_id", "project_id", "slug", "name", "color_hex", "is_default"}, pgx.CopyFromRows(environmentRows)); err != nil {
		t.Fatal(err)
	}

	first := fixture.request(t, http.MethodGet, "/api/projects?limit=2", ownerToken, "")
	var page api.ListProjectsResponse
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &page) != nil {
		t.Fatalf("first page = %d %s", first.Code, first.Body.String())
	}
	if len(page.Projects) != 2 || page.NextCursor == "" || !page.Projects[0].IsDefault {
		t.Fatalf("first page = %+v", page)
	}
	seen := map[string]bool{}
	for _, project := range page.Projects {
		if project.Environments != nil {
			t.Fatalf("list project %s includes environments", project.ID)
		}
		seen[project.ID] = true
	}
	second := fixture.request(t, http.MethodGet, "/api/projects?limit=2&cursor="+page.NextCursor, ownerToken, "")
	var secondPage api.ListProjectsResponse
	if second.Code != http.StatusOK || json.Unmarshal(second.Body.Bytes(), &secondPage) != nil {
		t.Fatalf("second page = %d %s", second.Code, second.Body.String())
	}
	if len(secondPage.Projects) != 1 || secondPage.NextCursor != "" || seen[secondPage.Projects[0].ID] {
		t.Fatalf("second page = %+v", secondPage)
	}

	fixture.store.statements.Store(0)
	crossOrg := fixture.request(t, http.MethodGet, "/api/projects?limit=2&cursor="+page.NextCursor, otherOwnerToken, "")
	if crossOrg.Code != http.StatusBadRequest || fixture.store.statements.Load() != 0 {
		t.Fatalf("cross-org cursor = %d with %d project statements: %s", crossOrg.Code, fixture.store.statements.Load(), crossOrg.Body.String())
	}

	detail := fixture.request(t, http.MethodGet, "/api/projects/project-0", ownerToken, "")
	var project api.ProjectSummary
	if detail.Code != http.StatusOK || json.Unmarshal(detail.Body.Bytes(), &project) != nil || len(project.Environments) != 1 {
		t.Fatalf("detail = %d %s", detail.Code, detail.Body.String())
	}
	hidden := fixture.request(t, http.MethodGet, "/api/projects/project-0", otherOwnerToken, "")
	if hidden.Code != http.StatusNotFound || !strings.Contains(hidden.Body.String(), `"code":"not_found"`) {
		t.Fatalf("cross-org detail = %d %s", hidden.Code, hidden.Body.String())
	}
}

func TestCreateProjectHTTPPostgresReportsMissingRegion(t *testing.T) {
	fixture := newOrgHTTPFixture(t)
	_, ownerToken := fixture.organizationOwner(t, "missing-region")
	recorder := fixture.request(t, http.MethodPost, "/api/projects", ownerToken, `{"slug":"project","name":"Project"}`)
	var body api.HTTPErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response = %d %s: %v", recorder.Code, recorder.Body.String(), err)
	}
	if recorder.Code != http.StatusBadRequest || body.Error.Code != "bad_request" || body.Error.Message != "no region configured" {
		t.Fatalf("response = %d %+v", recorder.Code, body.Error)
	}
}

// orgHTTPFixture serves the control plane built by NewServer over a test
// database, authenticating requests with real organization sessions.
type orgHTTPFixture struct {
	pool       *pgxpool.Pool
	store      *projectHTTPCountingStore
	handler    http.Handler
	sessionKey []byte
}

func newOrgHTTPFixture(t *testing.T) orgHTTPFixture {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	queries := db.New(database.Pool)
	store := &projectHTTPCountingStore{Querier: queries}
	cfg := completeServerConfig(t)
	cfg.DB = store
	cfg.TX = database.Pool
	cfg.Auth = NewDBAuthenticator(queries)
	cfg.PublicURL = &url.URL{Scheme: "https", Host: "console.example.test"}
	keys, err := auth.NewKeys(cfg.AuthKey)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return orgHTTPFixture{pool: database.Pool, store: store, handler: handler, sessionKey: keys.Session}
}

// organizationOwner creates an organization with an owner and returns the
// owner's session token for it.
func (f orgHTTPFixture) organizationOwner(t *testing.T, slug string) (uuid.UUID, string) {
	t.Helper()
	orgID, userID := uuid.NewV7(), uuid.NewV7()
	queries := db.New(f.pool)
	if _, err := queries.CreateOrganization(t.Context(), db.CreateOrganizationParams{
		ID: pgvalue.UUID(orgID), Name: slug, Slug: slug,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO users (id, display_name) VALUES ($1, 'Owner')`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.EnsureOrgMember(t.Context(), db.EnsureOrgMemberParams{
		OrgID: pgvalue.UUID(orgID), UserID: pgvalue.UUID(userID), Role: db.OrgMemberRoleOwner,
	}); err != nil {
		t.Fatal(err)
	}
	token, err := auth.GenerateOpaque(32)
	if err != nil {
		t.Fatal(err)
	}
	tokenHash, err := auth.HashToken(f.sessionKey, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateAuthSession(t.Context(), db.CreateAuthSessionParams{
		ID: pgvalue.UUID(uuid.NewV7()), OrgID: pgvalue.UUID(orgID), UserID: pgvalue.UUID(userID),
		TokenHash: tokenHash, ExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
	return orgID, token
}

func (f orgHTTPFixture) request(t *testing.T, method string, path string, token string, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

type projectHTTPCountingStore struct {
	db.Querier
	statements atomic.Int64
}

func (s *projectHTTPCountingStore) ListProjects(ctx context.Context, arg db.ListProjectsParams) ([]db.Project, error) {
	s.statements.Add(1)
	return s.Querier.ListProjects(ctx, arg)
}

func (s *projectHTTPCountingStore) GetProjectBySlug(ctx context.Context, arg db.GetProjectBySlugParams) (db.Project, error) {
	s.statements.Add(1)
	return s.Querier.GetProjectBySlug(ctx, arg)
}
