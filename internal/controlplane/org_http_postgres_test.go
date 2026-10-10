package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// These tests cover the HTTP contract of the project handlers; the durable
// behavior behind them is tested in internal/org.

func TestProjectHTTPPostgresListAndDetailContract(t *testing.T) {
	var store *projectHTTPCountingStore
	fixture := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		store = &projectHTTPCountingStore{Querier: cfg.DB}
		cfg.DB = store
	})
	queries := fixture.queries
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
		environmentRows = append(environmentRows, []any{uuid.NewV7(), orgID, projectID, "production", "Production", "#315FCE", true, "until_environment_deletion"})
	}
	if _, err := fixture.pool.CopyFrom(t.Context(), pgx.Identifier{"projects"},
		[]string{"id", "org_id", "default_region_id", "slug", "name", "is_default"}, pgx.CopyFromRows(projectRows)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.CopyFrom(t.Context(), pgx.Identifier{"environments"},
		[]string{"id", "org_id", "project_id", "slug", "name", "color_hex", "is_default", "history_retention_mode"}, pgx.CopyFromRows(environmentRows)); err != nil {
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

	store.statements.Store(0)
	crossOrg := fixture.request(t, http.MethodGet, "/api/projects?limit=2&cursor="+page.NextCursor, otherOwnerToken, "")
	if crossOrg.Code != http.StatusBadRequest || store.statements.Load() != 0 {
		t.Fatalf("cross-org cursor = %d with %d project statements: %s", crossOrg.Code, store.statements.Load(), crossOrg.Body.String())
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
	fixture := newHTTPPostgresFixture(t)
	_, ownerToken := fixture.organizationOwner(t, "missing-region")
	recorder := fixture.request(t, http.MethodPost, "/api/projects", ownerToken, `{"slug":"project","name":"Project","history_retention_mode":"until_environment_deletion"}`)
	var body api.HTTPErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response = %d %s: %v", recorder.Code, recorder.Body.String(), err)
	}
	if recorder.Code != http.StatusBadRequest || body.Error.Code != "bad_request" || body.Error.Message != "no region configured" {
		t.Fatalf("response = %d %+v", recorder.Code, body.Error)
	}
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

func TestHistoryRetentionHTTPPolicy(t *testing.T) {
	f := newHTTPPostgresFixture(t)
	_, token := f.organizationOwner(t, "history-http")
	if _, err := f.queries.CreateRegion(t.Context(), db.CreateRegionParams{ID: "history-http", DisplayName: "History"}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"slug":"history","name":"History"}`, `{"slug":"history","name":"History","history_retention_mode":"duration"}`, `{"slug":"history","name":"History","history_retention_mode":"duration","history_retention_seconds":0}`, `{"slug":"history","name":"History","history_retention_mode":"until_environment_deletion","history_retention_seconds":1}`} {
		r := f.request(t, http.MethodPost, "/api/projects", token, body)
		if r.Code != http.StatusBadRequest {
			t.Fatalf("invalid policy accepted: %d %s", r.Code, r.Body.String())
		}
	}
	r := f.request(t, http.MethodPost, "/api/projects", token, `{"slug":"history","name":"History","history_retention_mode":"duration","history_retention_seconds":3600}`)
	var project api.ProjectSummary
	if r.Code != http.StatusCreated || json.Unmarshal(r.Body.Bytes(), &project) != nil || len(project.Environments) != 2 {
		t.Fatalf("create: %d %s", r.Code, r.Body.String())
	}
	for _, env := range project.Environments {
		if env.HistoryRetentionMode != "duration" || env.HistoryRetentionSeconds == nil || *env.HistoryRetentionSeconds != 3600 {
			t.Fatalf("policy absent: %+v", env)
		}
	}
	env := project.Environments[0]
	path := "/api/projects/" + project.ID + "/environments/" + env.ID
	r = f.request(t, http.MethodPatch, path, token, fmt.Sprintf(`{"slug":%q,"name":"Renamed","color_hex":%q}`, env.Slug, env.ColorHex))
	var updated api.EnvironmentSummary
	if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &updated) != nil || updated.HistoryRetentionSeconds == nil || *updated.HistoryRetentionSeconds != 3600 {
		t.Fatalf("rename changed policy: %d %s", r.Code, r.Body.String())
	}
	r = f.request(t, http.MethodPatch, path, token, fmt.Sprintf(`{"slug":%q,"name":"Renamed","color_hex":%q,"history_retention_mode":"until_environment_deletion"}`, env.Slug, env.ColorHex))
	updated = api.EnvironmentSummary{}
	if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &updated) != nil || updated.HistoryRetentionMode != "until_environment_deletion" || updated.HistoryRetentionSeconds != nil {
		t.Fatalf("policy update: %d %s", r.Code, r.Body.String())
	}
	r = f.request(t, http.MethodPost, "/api/projects/"+project.ID+"/environments", token, `{"slug":"preview","name":"Preview","color_hex":"#123456"}`)
	if r.Code != http.StatusBadRequest {
		t.Fatalf("new environment omitted policy: %d %s", r.Code, r.Body.String())
	}
}
