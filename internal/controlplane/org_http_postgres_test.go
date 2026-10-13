package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
		environmentRows = append(environmentRows, []any{uuid.NewV7(), orgID, projectID, "production", "Production", "#315FCE", true,
			int64(1024), int32(32), int64(1024), int64(1024000), int64(1 << 40), int64(1 << 40), int64(600000), int64(1000000), int64(1000000), pgtype.Numeric{Int: big.NewInt(1000000), Valid: true}, time.Now()})
	}
	if _, err := fixture.pool.CopyFrom(t.Context(), pgx.Identifier{"projects"},
		[]string{"id", "org_id", "default_region_id", "slug", "name", "is_default"}, pgx.CopyFromRows(projectRows)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.CopyFrom(t.Context(), pgx.Identifier{"environments"},
		[]string{"id", "org_id", "project_id", "slug", "name", "color_hex", "is_default",
			"max_outstanding_admissions", "max_causal_depth", "max_resident_computers", "max_cpu_millis", "max_memory_bytes", "max_reserved_storage_bytes",
			"preparation_timeout_ms", "admission_rate_per_second", "admission_burst", "admission_tokens", "admission_refilled_at"}, pgx.CopyFromRows(environmentRows)); err != nil {
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
			t.Fatalf("list project %s includes environments", project.Project)
		}
		seen[project.Project] = true
	}
	second := fixture.request(t, http.MethodGet, "/api/projects?limit=2&cursor="+page.NextCursor, ownerToken, "")
	var secondPage api.ListProjectsResponse
	if second.Code != http.StatusOK || json.Unmarshal(second.Body.Bytes(), &secondPage) != nil {
		t.Fatalf("second page = %d %s", second.Code, second.Body.String())
	}
	if len(secondPage.Projects) != 1 || secondPage.NextCursor != "" || seen[secondPage.Projects[0].Project] {
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
	recorder := fixture.request(t, http.MethodPost, "/api/projects", ownerToken, `{"project":"project","name":"Project"}`)
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
	orgID, token := f.organizationOwner(t, "history-http")
	if _, err := f.queries.CreateRegion(t.Context(), db.CreateRegionParams{ID: "history-http", DisplayName: "History"}); err != nil {
		t.Fatal(err)
	}
	r := f.request(t, http.MethodPost, "/api/projects", token, `{"project":"history","name":"History"}`)
	if r.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.Code, r.Body.String())
	}
	path := "/api/projects/history/settings"
	r = f.request(t, http.MethodGet, path, token, "")
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"history_retention":"30d"`) {
		t.Fatalf("default: %d %s", r.Code, r.Body.String())
	}
	for _, role := range []db.OrgMemberRole{db.OrgMemberRoleAdmin, db.OrgMemberRoleDeveloper, db.OrgMemberRoleViewer} {
		user := f.user(t, string(role))
		f.member(t, orgID, user, role)
		login := f.session(t, user, orgID)
		read := f.request(t, http.MethodGet, path, login, "")
		if read.Code != http.StatusOK {
			t.Fatalf("%s read: %d", role, read.Code)
		}
		write := f.request(t, http.MethodPatch, path, login, `{"history_retention":"30d"}`)
		want := http.StatusForbidden
		if role == db.OrgMemberRoleAdmin {
			want = http.StatusOK
		}
		if write.Code != want {
			t.Fatalf("%s write: %d %s", role, write.Code, write.Body.String())
		}
	}
	_, other := f.organizationOwner(t, "other-history")
	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		response := f.request(t, method, path, other, `{"history_retention":"never"}`)
		if response.Code != http.StatusNotFound {
			t.Fatalf("cross-org %s: %d", method, response.Code)
		}
	}
	for _, body := range []string{`{}`, `null`, `{"history_retention":null}`, `{"history_retention":""}`, `{"history_retention":30}`, `{"history_retention":"0d"}`, `{"history_retention":"1.5d"}`, `{"history_retention":"1ms"}`, `{"history_retention":"30d","unknown":1}`} {
		r = f.request(t, http.MethodPatch, path, token, body)
		if r.Code != http.StatusBadRequest {
			t.Fatalf("invalid %s: %d %s", body, r.Code, r.Body.String())
		}
	}
	for _, value := range []string{"48h", "never", "1s"} {
		r = f.request(t, http.MethodPatch, path, token, fmt.Sprintf(`{"history_retention":%q}`, value))
		want := value
		if value == "48h" {
			want = "2d"
		}
		if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), fmt.Sprintf(`"history_retention":%q`, want)) {
			t.Fatalf("update: %d %s", r.Code, r.Body.String())
		}
		r = f.request(t, http.MethodGet, path, token, "")
		if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), fmt.Sprintf(`"history_retention":%q`, want)) {
			t.Fatalf("persisted: %d %s", r.Code, r.Body.String())
		}
	}
	r = f.request(t, http.MethodPatch, "/api/projects/history/environments/production", token, `{"name":"Production","color_hex":"#315FCE","history_retention":"never"}`)
	if r.Code != http.StatusBadRequest {
		t.Fatalf("environment override accepted: %d", r.Code)
	}
}
