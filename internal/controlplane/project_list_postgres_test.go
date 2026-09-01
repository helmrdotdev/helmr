package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProjectListPostgresPaginationAndDetail(t *testing.T) {
	fixture := newProjectListPostgresFixture(t, 101, 2)

	t.Run("static traversal", func(t *testing.T) {
		cursor := ""
		seen := map[string]bool{}
		pages := 0
		for {
			fixture.store.statements.Store(0)
			response := fixture.list(t, cursor, 50)
			pages++
			if got := fixture.store.statements.Load(); got != 1 {
				t.Fatalf("page statements = %d, want 1", got)
			}
			if len(response.Projects) > 50 {
				t.Fatalf("page projects = %d, want <= 50", len(response.Projects))
			}
			for _, project := range response.Projects {
				if project.Environments != nil {
					t.Fatalf("list project %s includes environments", project.ID)
				}
				if seen[project.ID] {
					t.Fatalf("duplicate project %s", project.ID)
				}
				seen[project.ID] = true
			}
			if pages == 1 && (len(response.Projects) == 0 || !response.Projects[0].IsDefault) {
				t.Fatal("default project is not first")
			}
			if response.NextCursor == "" {
				break
			}
			cursor = response.NextCursor
		}
		if pages != 3 || len(seen) != 101 {
			t.Fatalf("pages/projects = %d/%d, want 3/101", pages, len(seen))
		}
	})

	t.Run("slug and uuid detail", func(t *testing.T) {
		for _, ref := range []string{"PROJECT-0001", fixture.projectIDs[0].String()} {
			fixture.store.statements.Store(0)
			project := fixture.detail(t, ref, fixture.orgID)
			if project.ID != fixture.projectIDs[0].String() || len(project.Environments) != 2 {
				t.Fatalf("detail = %+v", project)
			}
			if got := fixture.store.statements.Load(); got != 2 {
				t.Fatalf("detail statements = %d, want 2", got)
			}
		}
	})

	t.Run("cursor errors are scoped", func(t *testing.T) {
		first := fixture.list(t, "", 1)
		otherOrg := uuid.NewV7()
		if _, err := fixture.queries.CreateOrganization(t.Context(), db.CreateOrganizationParams{
			ID: pgvalue.UUID(otherOrg), Name: "Other", Slug: "project-list-other",
		}); err != nil {
			t.Fatal(err)
		}
		fixture.store.statements.Store(0)
		response := fixture.listRecorder(t, first.NextCursor, 1, otherOrg)
		if response.Code != http.StatusBadRequest || fixture.store.statements.Load() != 0 {
			t.Fatalf("cross-scope response/statements = %d/%d", response.Code, fixture.store.statements.Load())
		}
		response = fixture.listRecorder(t, "not-base64", 1, fixture.orgID)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("malformed cursor status = %d", response.Code)
		}
	})

	t.Run("cross-organization detail is hidden", func(t *testing.T) {
		otherOrg := uuid.NewV7()
		if _, err := fixture.queries.CreateOrganization(t.Context(), db.CreateOrganizationParams{
			ID: pgvalue.UUID(otherOrg), Name: "Hidden", Slug: "project-list-hidden",
		}); err != nil {
			t.Fatal(err)
		}
		fixture.store.statements.Store(0)
		response := fixture.detailRecorder(t, "project-0001", otherOrg)
		if response.Code != http.StatusNotFound || fixture.store.statements.Load() != 1 {
			t.Fatalf("cross-org response/statements = %d/%d", response.Code, fixture.store.statements.Load())
		}
	})
}

func TestProjectListCursorValidatesRequiredFields(t *testing.T) {
	valid := projectListCursor{
		OrgID: uuid.NewV7().String(), IsDefault: true,
		Slug: "project", ID: uuid.NewV7().String(),
	}
	for name, cursor := range map[string]projectListCursor{
		"valid":          valid,
		"invalid org id": {OrgID: "invalid", Slug: valid.Slug, ID: valid.ID},
		"empty slug":     {OrgID: valid.OrgID, ID: valid.ID},
		"invalid id":     {OrgID: valid.OrgID, Slug: valid.Slug, ID: "invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := encodeProjectListCursor(cursor)
			if err != nil {
				t.Fatal(err)
			}
			_, err = decodeProjectListCursor(raw)
			if (err == nil) != (name == "valid") {
				t.Fatalf("decode error = %v", err)
			}
		})
	}
}

func TestProjectListQueryValidation(t *testing.T) {
	orgID := uuid.NewV7()
	for _, rawQuery := range []string{
		"limit=0", "limit=101", "limit=abc", "limit=1&limit=2",
		"cursor=", "unsupported=true",
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/projects?"+rawQuery, nil)
		if _, _, err := parseProjectListQuery(request, orgID); err == nil {
			t.Fatalf("query %q succeeded", rawQuery)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	limit, cursor, err := parseProjectListQuery(request, orgID)
	if err != nil || limit != 50 || cursor != nil {
		t.Fatalf("default query = %d/%+v/%v", limit, cursor, err)
	}
}

func TestNormalizeProjectInputRejectsUUIDSlug(t *testing.T) {
	if _, _, err := normalizeProjectInput(uuid.NewV7().String(), "Project"); err == nil {
		t.Fatal("UUID project slug succeeded")
	}
	if slug, _, err := normalizeProjectInput("PROJECT-SLUG", "Project"); err != nil || slug != "project-slug" {
		t.Fatalf("ordinary project slug = %q, %v", slug, err)
	}
}

func TestProjectListPostgresCandidateScale(t *testing.T) {
	if os.Getenv("HELMR_TEST_PROJECT_LIST_SCALE") != "1" {
		t.Skip("HELMR_TEST_PROJECT_LIST_SCALE is not set")
	}
	for _, projectCount := range []int{1, 10, 50, 100, 1000} {
		t.Run(fmt.Sprintf("projects-%d", projectCount), func(t *testing.T) {
			fixture := newProjectListPostgresFixture(t, projectCount, 2)
			measure := func(limit int) (time.Duration, int, int64) {
				t.Helper()
				fixture.store.statements.Store(0)
				started := time.Now()
				cursor := ""
				responseBytes := 0
				for {
					response := fixture.listRecorder(t, cursor, limit, fixture.orgID)
					if response.Code != http.StatusOK {
						t.Fatalf("status = %d: %s", response.Code, response.Body.String())
					}
					responseBytes += response.Body.Len()
					var page api.ListProjectsResponse
					if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
						t.Fatal(err)
					}
					if page.NextCursor == "" {
						break
					}
					cursor = page.NextCursor
				}
				return time.Since(started), responseBytes, fixture.store.statements.Load()
			}

			measure(50)
			elapsed := make([]time.Duration, 0, 5)
			responseBytes := make([]int, 0, 5)
			statements := make([]int64, 0, 5)
			for range 5 {
				duration, bytes, count := measure(50)
				elapsed = append(elapsed, duration)
				responseBytes = append(responseBytes, bytes)
				statements = append(statements, count)
			}
			sort.Slice(elapsed, func(i, j int) bool { return elapsed[i] < elapsed[j] })
			t.Logf(
				"project list candidate: projects=%d page_limit=50 statements=%v response_bytes=%v elapsed_min=%s elapsed_median=%s elapsed_max=%s",
				projectCount, statements, responseBytes, elapsed[0], elapsed[len(elapsed)/2], elapsed[len(elapsed)-1],
			)
			wantStatements := int64((projectCount + 49) / 50)
			for _, count := range statements {
				if count != wantStatements {
					t.Fatalf("statements = %d, want %d", count, wantStatements)
				}
			}
			if projectCount == 1000 {
				detailElapsed := make([]time.Duration, 0, 5)
				detailBytes := make([]int, 0, 5)
				detailStatements := make([]int64, 0, 5)
				fixture.detail(t, fixture.projectIDs[0].String(), fixture.orgID)
				for range 5 {
					fixture.store.statements.Store(0)
					started := time.Now()
					detail := fixture.detailRecorder(t, fixture.projectIDs[0].String(), fixture.orgID)
					if detail.Code != http.StatusOK {
						t.Fatalf("detail status = %d: %s", detail.Code, detail.Body.String())
					}
					detailElapsed = append(detailElapsed, time.Since(started))
					detailBytes = append(detailBytes, detail.Body.Len())
					detailStatements = append(detailStatements, fixture.store.statements.Load())
				}
				sort.Slice(detailElapsed, func(i, j int) bool { return detailElapsed[i] < detailElapsed[j] })
				t.Logf(
					"project detail candidate: organization_projects=1000 statements=%v response_bytes=%v elapsed_min=%s elapsed_median=%s elapsed_max=%s",
					detailStatements, detailBytes, detailElapsed[0], detailElapsed[len(detailElapsed)/2], detailElapsed[len(detailElapsed)-1],
				)
				for _, count := range detailStatements {
					if count != 2 {
						t.Fatalf("detail statements = %d, want 2", count)
					}
				}
				if _, err := fixture.pool.Exec(t.Context(), `
					WITH ordered AS (
						SELECT id, row_number() OVER (ORDER BY id) AS sequence
						  FROM projects
						 WHERE org_id = $1
					)
					UPDATE projects AS project
					   SET slug = lpad(ordered.sequence::text, 4, '0') || repeat('s', 59),
					name = repeat('N', 80)
					  FROM ordered
					 WHERE project.id = ordered.id
				`, fixture.orgID); err != nil {
					t.Fatal(err)
				}
				fixture.store.statements.Store(0)
				maximumPage := fixture.listRecorder(t, "", 100, fixture.orgID)
				if maximumPage.Code != http.StatusOK {
					t.Fatalf("maximum page status = %d: %s", maximumPage.Code, maximumPage.Body.String())
				}
				if got := fixture.store.statements.Load(); got != 1 {
					t.Fatalf("maximum page statements = %d, want 1", got)
				}
				t.Logf("project list candidate maximum page: projects=1000 limit=100 statements=%d response_bytes=%d", fixture.store.statements.Load(), maximumPage.Body.Len())
				fixture.logPlans(t)
			}
		})
	}
}

type projectListPostgresFixture struct {
	pool       *pgxpool.Pool
	queries    *db.Queries
	store      *projectListCountingStore
	server     *Server
	orgID      uuid.UUID
	projectIDs []uuid.UUID
}

func newProjectListPostgresFixture(t *testing.T, projectCount, environmentsEach int) projectListPostgresFixture {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	queries := db.New(database.Pool)
	regionID := "project-list-region"
	if _, err := queries.CreateRegion(t.Context(), db.CreateRegionParams{ID: regionID, DisplayName: "Project list"}); err != nil {
		t.Fatal(err)
	}
	orgID := uuid.NewV7()
	if _, err := queries.CreateOrganization(t.Context(), db.CreateOrganizationParams{
		ID: pgvalue.UUID(orgID), Name: "Project list", Slug: "project-list-" + orgID.String(),
	}); err != nil {
		t.Fatal(err)
	}
	projectIDs := make([]uuid.UUID, projectCount)
	projectRows := make([][]any, 0, projectCount)
	environmentRows := make([][]any, 0, projectCount*environmentsEach)
	for projectIndex := range projectCount {
		projectID := uuid.NewV7()
		projectIDs[projectIndex] = projectID
		projectRows = append(projectRows, []any{
			projectID, orgID, regionID,
			fmt.Sprintf("project-%04d", projectIndex+1),
			fmt.Sprintf("Project %04d", projectIndex+1),
			projectIndex == 0,
		})
		for environmentIndex := range environmentsEach {
			environmentRows = append(environmentRows, []any{
				uuid.NewV7(), orgID, projectID,
				fmt.Sprintf("environment-%02d", environmentIndex+1),
				fmt.Sprintf("Environment %02d", environmentIndex+1),
				"#315FCE", environmentIndex == 0,
			})
		}
	}
	if _, err := database.Pool.CopyFrom(t.Context(), pgx.Identifier{"projects"},
		[]string{"id", "org_id", "default_region_id", "slug", "name", "is_default"},
		pgx.CopyFromRows(projectRows)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.CopyFrom(t.Context(), pgx.Identifier{"environments"},
		[]string{"id", "org_id", "project_id", "slug", "name", "color_hex", "is_default"},
		pgx.CopyFromRows(environmentRows)); err != nil {
		t.Fatal(err)
	}
	store := &projectListCountingStore{Querier: queries}
	return projectListPostgresFixture{
		pool: database.Pool, queries: queries, store: store,
		server: &Server{db: store}, orgID: orgID, projectIDs: projectIDs,
	}
}

func (f projectListPostgresFixture) list(t *testing.T, cursor string, limit int) api.ListProjectsResponse {
	t.Helper()
	recorder := f.listRecorder(t, cursor, limit, f.orgID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response api.ListProjectsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func (f projectListPostgresFixture) listRecorder(t *testing.T, cursor string, limit int, orgID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	path := fmt.Sprintf("/api/projects?limit=%d", limit)
	if cursor != "" {
		path += "&cursor=" + cursor
	}
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request = request.WithContext(context.WithValue(request.Context(), actorContextKey{}, auth.Actor{
		OrgID: orgID, Kind: auth.ActorKindSession, Role: auth.RoleOwner,
	}))
	recorder := httptest.NewRecorder()
	f.server.listProjects(recorder, request)
	return recorder
}

func (f projectListPostgresFixture) detail(t *testing.T, ref string, orgID uuid.UUID) api.ProjectSummary {
	t.Helper()
	recorder := f.detailRecorder(t, ref, orgID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("detail status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response api.ProjectSummary
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func (f projectListPostgresFixture) detailRecorder(t *testing.T, ref string, orgID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Get("/api/projects/{projectRef}", f.server.getProject)
	request := httptest.NewRequest(http.MethodGet, "/api/projects/"+ref, nil)
	request = request.WithContext(context.WithValue(request.Context(), actorContextKey{}, auth.Actor{
		OrgID: orgID, Kind: auth.ActorKindSession, Role: auth.RoleOwner,
	}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func (f projectListPostgresFixture) logPlans(t *testing.T) {
	t.Helper()
	var afterIsDefault bool
	var afterSlug string
	var afterID uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `
		SELECT is_default, slug, id
		  FROM projects
		 WHERE org_id = $1
		 ORDER BY is_default DESC, slug, id
		 OFFSET 499 LIMIT 1
	`, f.orgID).Scan(&afterIsDefault, &afterSlug, &afterID); err != nil {
		t.Fatal(err)
	}
	for _, page := range []struct {
		name     string
		hasAfter bool
	}{{name: "first"}, {name: "deep", hasAfter: true}} {
		var plan []byte
		if err := f.pool.QueryRow(t.Context(), `
			EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
			SELECT *
			  FROM projects
			 WHERE org_id = $1
			   AND (NOT $2::boolean OR ((NOT is_default), slug, id) > ((NOT $3::boolean), $4::text, $5::uuid))
			 ORDER BY is_default DESC, slug, id
			 LIMIT 51
		`, f.orgID, page.hasAfter, afterIsDefault, afterSlug, afterID).Scan(&plan); err != nil {
			t.Fatal(err)
		}
		t.Logf("project list candidate plan: page=%s plan=%s", page.name, compactJSON(plan))
	}
}

type projectListCountingStore struct {
	db.Querier
	statements atomic.Int64
}

func (s *projectListCountingStore) ListProjects(ctx context.Context, arg db.ListProjectsParams) ([]db.Project, error) {
	s.statements.Add(1)
	return s.Querier.ListProjects(ctx, arg)
}

func (s *projectListCountingStore) GetProject(ctx context.Context, arg db.GetProjectParams) (db.Project, error) {
	s.statements.Add(1)
	return s.Querier.GetProject(ctx, arg)
}

func (s *projectListCountingStore) GetProjectBySlug(ctx context.Context, arg db.GetProjectBySlugParams) (db.Project, error) {
	s.statements.Add(1)
	return s.Querier.GetProjectBySlug(ctx, arg)
}

func (s *projectListCountingStore) ListEnvironments(ctx context.Context, arg db.ListEnvironmentsParams) ([]db.Environment, error) {
	s.statements.Add(1)
	return s.Querier.ListEnvironments(ctx, arg)
}

func compactJSON(value []byte) string {
	var decoded any
	if json.Unmarshal(value, &decoded) != nil {
		return string(value)
	}
	compact, err := json.Marshal(decoded)
	if err != nil {
		return string(value)
	}
	return string(compact)
}

var _ db.Querier = (*projectListCountingStore)(nil)
