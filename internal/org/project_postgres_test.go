package org

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestListProjectsPostgresPaginationAndDetail(t *testing.T) {
	fixture := newProjectFixture(t, 101, 2)

	t.Run("static traversal", func(t *testing.T) {
		var after *ProjectPosition
		seen := map[uuid.UUID]bool{}
		pages := 0
		for {
			fixture.store.statements.Store(0)
			projects, hasMore := fixture.list(t, after, 50)
			pages++
			if got := fixture.store.statements.Load(); got != 1 {
				t.Fatalf("page statements = %d, want 1", got)
			}
			if len(projects) > 50 {
				t.Fatalf("page projects = %d, want <= 50", len(projects))
			}
			for _, project := range projects {
				id := pgvalue.MustUUIDValue(project.ID)
				if seen[id] {
					t.Fatalf("duplicate project %s", id)
				}
				seen[id] = true
			}
			if pages == 1 && (len(projects) == 0 || !projects[0].IsDefault) {
				t.Fatal("default project is not first")
			}
			if !hasMore {
				break
			}
			last := projects[len(projects)-1]
			after = &ProjectPosition{IsDefault: last.IsDefault, Slug: last.Slug, ID: pgvalue.MustUUIDValue(last.ID)}
		}
		if pages != 3 || len(seen) != 101 {
			t.Fatalf("pages/projects = %d/%d, want 3/101", pages, len(seen))
		}
	})

	t.Run("slug and uuid detail", func(t *testing.T) {
		for _, ref := range []string{"PROJECT-0001", fixture.projectIDs[0].String()} {
			fixture.store.statements.Store(0)
			project, environments := fixture.detail(t, ref, fixture.orgID)
			if pgvalue.MustUUIDValue(project.ID) != fixture.projectIDs[0] || len(environments) != 2 {
				t.Fatalf("detail = %+v with %d environments", project, len(environments))
			}
			if got := fixture.store.statements.Load(); got != 2 {
				t.Fatalf("detail statements = %d, want 2", got)
			}
		}
	})

	t.Run("invalid reference reads nothing", func(t *testing.T) {
		fixture.store.statements.Store(0)
		_, err := GetProject(t.Context(), fixture.store, fixture.orgID, "not a slug")
		var input InputError
		if !errors.As(err, &input) || fixture.store.statements.Load() != 0 {
			t.Fatalf("error/statements = %v/%d, want input error without statements", err, fixture.store.statements.Load())
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
		_, err := GetProject(t.Context(), fixture.store, otherOrg, "project-0001")
		if !errors.Is(err, ErrProjectNotFound) || fixture.store.statements.Load() != 1 {
			t.Fatalf("cross-org error/statements = %v/%d", err, fixture.store.statements.Load())
		}
	})
}

func TestCreateProjectSelectsFirstRegionWhenDefaultIsOmitted(t *testing.T) {
	fixture := newProjectFixture(t, 0, 0)
	if _, err := fixture.queries.CreateRegion(t.Context(), db.CreateRegionParams{ID: "project-list-z", DisplayName: "Alpha"}); err != nil {
		t.Fatal(err)
	}
	project, environments, err := CreateProject(t.Context(), fixture.pool, fixture.orgID, ProjectInput{HistoryRetention: HistoryRetentionPolicy{Mode: "until_environment_deletion"},
		ExecutionLimits: testExecutionLimits(),
		ProjectDetails:  ProjectDetails{Slug: "project", Name: "Project"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if project.DefaultRegionID != "project-list-z" {
		t.Fatalf("default region = %q, want project-list-z", project.DefaultRegionID)
	}
	if len(environments) != 2 {
		t.Fatalf("environments = %d, want production and staging", len(environments))
	}
}

func TestCreateProjectRequiresARegionWhenDefaultIsOmitted(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	orgID := uuid.NewV7()
	if _, err := db.New(database.Pool).CreateOrganization(t.Context(), db.CreateOrganizationParams{
		ID: pgvalue.UUID(orgID), Name: "Test", Slug: "test",
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err := CreateProject(t.Context(), database.Pool, orgID, ProjectInput{HistoryRetention: HistoryRetentionPolicy{Mode: "until_environment_deletion"},
		ExecutionLimits: testExecutionLimits(),
		ProjectDetails:  ProjectDetails{Slug: "project", Name: "Project"},
	})
	if !errors.Is(err, ErrNoRegion) {
		t.Fatalf("error = %v, want %v", err, ErrNoRegion)
	}
}

func TestCreateProjectRejectsUnknownDefaultRegion(t *testing.T) {
	fixture := newProjectFixture(t, 0, 0)
	_, _, err := CreateProject(t.Context(), fixture.pool, fixture.orgID, ProjectInput{HistoryRetention: HistoryRetentionPolicy{Mode: "until_environment_deletion"},
		ExecutionLimits: testExecutionLimits(),
		ProjectDetails:  ProjectDetails{Slug: "project"},
		DefaultRegionID: "missing-region",
	})
	if !errors.Is(err, ErrDefaultRegionNotFound) {
		t.Fatalf("error = %v, want %v", err, ErrDefaultRegionNotFound)
	}
}

func TestCreateProjectsPostgresSerializesFirstDefaultSelection(t *testing.T) {
	fixture := newProjectFixture(t, 0, 0)
	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)

	for _, slug := range []string{"first", "second"} {
		go func() {
			ready.Done()
			<-start
			_, _, err := CreateProject(context.Background(), fixture.pool, fixture.orgID, ProjectInput{HistoryRetention: HistoryRetentionPolicy{Mode: "until_environment_deletion"},
				ExecutionLimits: testExecutionLimits(),
				ProjectDetails:  ProjectDetails{Slug: slug, Name: slug},
			})
			results <- err
		}()
	}

	ready.Wait()
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("create project: %v", err)
		}
	}
	var projectCount, defaultCount int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*), count(*) FILTER (WHERE is_default)
		  FROM projects
		 WHERE org_id = $1
	`, fixture.orgID).Scan(&projectCount, &defaultCount); err != nil {
		t.Fatal(err)
	}
	if projectCount != 2 || defaultCount != 1 {
		t.Fatalf("projects/defaults = %d/%d, want 2/1", projectCount, defaultCount)
	}
}

func TestUpdateEnvironmentKeepsProtectedSlugs(t *testing.T) {
	fixture := newProjectFixture(t, 0, 0)
	project, environments, err := CreateProject(t.Context(), fixture.pool, fixture.orgID, ProjectInput{HistoryRetention: HistoryRetentionPolicy{Mode: "until_environment_deletion"},
		ExecutionLimits: testExecutionLimits(),
		ProjectDetails:  ProjectDetails{Slug: "project"},
	})
	if err != nil {
		t.Fatal(err)
	}
	projectID := pgvalue.MustUUIDValue(project.ID)
	for _, environment := range environments {
		_, err := UpdateEnvironment(t.Context(), fixture.queries, fixture.orgID, projectID, pgvalue.MustUUIDValue(environment.ID), EnvironmentDetails{HistoryRetention: HistoryRetentionPolicy{Mode: "until_environment_deletion"},
			Slug: "renamed", ColorHex: environment.ColorHex,
		})
		var input InputError
		if !errors.As(err, &input) {
			t.Fatalf("rename %s error = %v, want input error", environment.Slug, err)
		}
	}
	preview, err := CreateEnvironment(t.Context(), fixture.pool, fixture.orgID, projectID, EnvironmentDetails{HistoryRetention: HistoryRetentionPolicy{Mode: "until_environment_deletion"}, Slug: "preview", ColorHex: "#315FCE"}, testExecutionLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateEnvironment(t.Context(), fixture.pool, fixture.orgID, projectID, EnvironmentDetails{HistoryRetention: HistoryRetentionPolicy{Mode: "until_environment_deletion"}, Slug: "preview", ColorHex: "#315FCE"}, testExecutionLimits()); !errors.Is(err, ErrEnvironmentSlugInUse) {
		t.Fatalf("duplicate environment error = %v, want %v", err, ErrEnvironmentSlugInUse)
	}
	if _, err := UpdateEnvironment(t.Context(), fixture.queries, fixture.orgID, projectID, pgvalue.MustUUIDValue(preview.ID), EnvironmentDetails{HistoryRetention: HistoryRetentionPolicy{Mode: "until_environment_deletion"},
		Slug: "production", ColorHex: preview.ColorHex,
	}); err == nil {
		t.Fatal("renaming an environment to production succeeded")
	}
	renamed, err := UpdateEnvironment(t.Context(), fixture.queries, fixture.orgID, projectID, pgvalue.MustUUIDValue(preview.ID), EnvironmentDetails{HistoryRetention: HistoryRetentionPolicy{Mode: "until_environment_deletion"},
		Slug: "Review", Name: "Review apps", ColorHex: preview.ColorHex,
	})
	if err != nil || renamed.Slug != "review" || renamed.Name != "Review apps" {
		t.Fatalf("rename = %+v, %v", renamed, err)
	}
}

func TestListProjectsPostgresCandidateScale(t *testing.T) {
	if os.Getenv("HELMR_TEST_PROJECT_LIST_SCALE") != "1" {
		t.Skip("HELMR_TEST_PROJECT_LIST_SCALE is not set")
	}
	for _, projectCount := range []int{1, 10, 50, 100, 1000} {
		t.Run(fmt.Sprintf("projects-%d", projectCount), func(t *testing.T) {
			fixture := newProjectFixture(t, projectCount, 2)
			measure := func(limit int32) (time.Duration, int64) {
				t.Helper()
				fixture.store.statements.Store(0)
				started := time.Now()
				var after *ProjectPosition
				for {
					projects, hasMore := fixture.list(t, after, limit)
					if !hasMore {
						break
					}
					last := projects[len(projects)-1]
					after = &ProjectPosition{IsDefault: last.IsDefault, Slug: last.Slug, ID: pgvalue.MustUUIDValue(last.ID)}
				}
				return time.Since(started), fixture.store.statements.Load()
			}

			measure(50)
			elapsed := make([]time.Duration, 0, 5)
			statements := make([]int64, 0, 5)
			for range 5 {
				duration, count := measure(50)
				elapsed = append(elapsed, duration)
				statements = append(statements, count)
			}
			sort.Slice(elapsed, func(i, j int) bool { return elapsed[i] < elapsed[j] })
			t.Logf(
				"project list candidate: projects=%d page_limit=50 statements=%v elapsed_min=%s elapsed_median=%s elapsed_max=%s",
				projectCount, statements, elapsed[0], elapsed[len(elapsed)/2], elapsed[len(elapsed)-1],
			)
			wantStatements := int64((projectCount + 49) / 50)
			for _, count := range statements {
				if count != wantStatements {
					t.Fatalf("statements = %d, want %d", count, wantStatements)
				}
			}
			if projectCount == 1000 {
				detailElapsed := make([]time.Duration, 0, 5)
				detailStatements := make([]int64, 0, 5)
				fixture.detail(t, fixture.projectIDs[0].String(), fixture.orgID)
				for range 5 {
					fixture.store.statements.Store(0)
					started := time.Now()
					fixture.detail(t, fixture.projectIDs[0].String(), fixture.orgID)
					detailElapsed = append(detailElapsed, time.Since(started))
					detailStatements = append(detailStatements, fixture.store.statements.Load())
				}
				sort.Slice(detailElapsed, func(i, j int) bool { return detailElapsed[i] < detailElapsed[j] })
				t.Logf(
					"project detail candidate: organization_projects=1000 statements=%v elapsed_min=%s elapsed_median=%s elapsed_max=%s",
					detailStatements, detailElapsed[0], detailElapsed[len(detailElapsed)/2], detailElapsed[len(detailElapsed)-1],
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
				fixture.list(t, nil, 100)
				if got := fixture.store.statements.Load(); got != 1 {
					t.Fatalf("maximum page statements = %d, want 1", got)
				}
				fixture.logPlans(t)
			}
		})
	}
}

type projectFixture struct {
	pool       *pgxpool.Pool
	queries    *db.Queries
	store      *countingQuerier
	orgID      uuid.UUID
	projectIDs []uuid.UUID
}

func newProjectFixture(t *testing.T, projectCount, environmentsEach int) projectFixture {
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
				"#315FCE", environmentIndex == 0, "until_environment_deletion",
			})
		}
	}
	if _, err := database.Pool.CopyFrom(t.Context(), pgx.Identifier{"projects"},
		[]string{"id", "org_id", "default_region_id", "slug", "name", "is_default"},
		pgx.CopyFromRows(projectRows)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.CopyFrom(t.Context(), pgx.Identifier{"environments"},
		[]string{"id", "org_id", "project_id", "slug", "name", "color_hex", "is_default", "history_retention_mode"},
		pgx.CopyFromRows(environmentRows)); err != nil {
		t.Fatal(err)
	}
	return projectFixture{
		pool: database.Pool, queries: queries, store: &countingQuerier{Querier: queries},
		orgID: orgID, projectIDs: projectIDs,
	}
}

func (f projectFixture) list(t *testing.T, after *ProjectPosition, limit int32) ([]db.Project, bool) {
	t.Helper()
	projects, hasMore, err := ListProjects(t.Context(), f.store, f.orgID, limit, after)
	if err != nil {
		t.Fatal(err)
	}
	return projects, hasMore
}

func (f projectFixture) detail(t *testing.T, ref string, orgID uuid.UUID) (db.Project, []db.Environment) {
	t.Helper()
	project, err := GetProject(t.Context(), f.store, orgID, ref)
	if err != nil {
		t.Fatal(err)
	}
	environments, err := ListEnvironments(t.Context(), f.store, project)
	if err != nil {
		t.Fatal(err)
	}
	return project, environments
}

func (f projectFixture) logPlans(t *testing.T) {
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

// countingQuerier counts the project and environment reads that listing and
// detail issue.
type countingQuerier struct {
	db.Querier
	statements atomic.Int64
}

func (s *countingQuerier) ListProjects(ctx context.Context, arg db.ListProjectsParams) ([]db.Project, error) {
	s.statements.Add(1)
	return s.Querier.ListProjects(ctx, arg)
}

func (s *countingQuerier) GetProject(ctx context.Context, arg db.GetProjectParams) (db.Project, error) {
	s.statements.Add(1)
	return s.Querier.GetProject(ctx, arg)
}

func (s *countingQuerier) GetProjectBySlug(ctx context.Context, arg db.GetProjectBySlugParams) (db.Project, error) {
	s.statements.Add(1)
	return s.Querier.GetProjectBySlug(ctx, arg)
}

func (s *countingQuerier) ListEnvironments(ctx context.Context, arg db.ListEnvironmentsParams) ([]db.Environment, error) {
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

var _ db.Querier = (*countingQuerier)(nil)

func testExecutionLimits() ExecutionLimits {
	return ExecutionLimits{MaxResidentComputers: 10, MaxCPUMillis: 16000, MaxMemoryBytes: 1 << 36, MaxReservedStorageBytes: 1 << 40, MaxOutstandingAdmissions: 100, MaxCausalDepth: 8, AdmissionRatePerSecond: 10, AdmissionBurst: 20, PreparationTimeoutMS: 600000}
}

func TestProjectCreationRequiresLimitsWithoutPartialEnvironment(t *testing.T) {
	f := newProjectFixture(t, 0, 0)
	input := ProjectInput{HistoryRetention: HistoryRetentionPolicy{Mode: "until_environment_deletion"}, ProjectDetails: ProjectDetails{Slug: "configured", Name: "Configured"}}
	if _, _, err := CreateProject(t.Context(), f.pool, f.orgID, input); err == nil {
		t.Fatal("missing operating limits accepted")
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM projects WHERE org_id=$1`, f.orgID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed configuration leaked Project: %d %v", count, err)
	}
	input.ExecutionLimits = testExecutionLimits()
	_, environments, err := CreateProject(t.Context(), f.pool, f.orgID, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range environments {
		var configured bool
		if err := f.pool.QueryRow(t.Context(), `SELECT admission_tokens=admission_burst AND admission_burst=$2 AND max_reserved_storage_bytes=$3 AND max_causal_depth=$4 AND preparation_timeout_ms=$5 FROM environments WHERE id=$1`, env.ID, input.ExecutionLimits.AdmissionBurst, input.ExecutionLimits.MaxReservedStorageBytes, input.ExecutionLimits.MaxCausalDepth, input.ExecutionLimits.PreparationTimeoutMS).Scan(&configured); err != nil || !configured {
			t.Fatalf("Environment limits not committed: %v %v", configured, err)
		}
	}
	if _, _, err := CreateProject(t.Context(), f.pool, f.orgID, input); !errors.Is(err, ErrProjectSlugInUse) {
		t.Fatalf("duplicate Project accepted: %v", err)
	}
}

func TestExecutionLimitsCannotReinitializeEnvironment(t *testing.T) {
	f := newProjectFixture(t, 0, 0)
	_, envs, err := CreateProject(t.Context(), f.pool, f.orgID, ProjectInput{HistoryRetention: HistoryRetentionPolicy{Mode: "until_environment_deletion"}, ProjectDetails: ProjectDetails{Slug: "once", Name: "Once"}, ExecutionLimits: testExecutionLimits()})
	if err != nil {
		t.Fatal(err)
	}
	env := pgvalue.MustUUIDValue(envs[0].ID)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET admission_tokens=0 WHERE id=$1`, env)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := testExecutionLimits().InitializeEnvironment(t.Context(), tx, env); err == nil {
		t.Fatal("existing Environment policy reset")
	}
	var zero bool
	if err := tx.QueryRow(t.Context(), `SELECT admission_tokens=0 FROM environments WHERE id=$1`, env).Scan(&zero); err != nil || !zero {
		t.Fatalf("tokens reset: %v %v", zero, err)
	}
}

func TestHistoryRetentionPolicyRequiredAndAtomic(t *testing.T) {
	f := newProjectFixture(t, 0, 0)
	zero, positive := int64(0), int64(3600)
	input := ProjectInput{ProjectDetails: ProjectDetails{Slug: "history", Name: "History"}, ExecutionLimits: testExecutionLimits()}
	for _, policy := range []HistoryRetentionPolicy{{}, {Mode: "duration"}, {Mode: "duration", Seconds: &zero}, {Mode: "until_environment_deletion", Seconds: &positive}, {Mode: "unknown"}} {
		input.HistoryRetention = policy
		if _, _, err := CreateProject(t.Context(), f.pool, f.orgID, input); err == nil {
			t.Fatalf("invalid policy accepted: %+v", policy)
		}
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM projects WHERE org_id=$1`, f.orgID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid policy leaked project: %d %v", count, err)
	}
	input.HistoryRetention = HistoryRetentionPolicy{Mode: "duration", Seconds: &positive}
	project, envs, err := CreateProject(t.Context(), f.pool, f.orgID, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 2 {
		t.Fatalf("initial Environments=%d", len(envs))
	}
	for _, env := range envs {
		if env.HistoryRetentionMode != "duration" || !env.HistoryRetentionSeconds.Valid || env.HistoryRetentionSeconds.Int64 != positive {
			t.Fatalf("policy not applied atomically: %+v", env)
		}
	}
	env := envs[0]
	updated, err := UpdateEnvironment(t.Context(), db.New(f.pool), f.orgID, pgvalue.MustUUIDValue(project.ID), pgvalue.MustUUIDValue(env.ID), EnvironmentDetails{Slug: env.Slug, Name: "Renamed", ColorHex: env.ColorHex})
	if err != nil || updated.HistoryRetentionMode != "duration" || updated.HistoryRetentionSeconds.Int64 != positive {
		t.Fatalf("rename changed policy: %+v %v", updated, err)
	}
	updated, err = UpdateEnvironment(t.Context(), db.New(f.pool), f.orgID, pgvalue.MustUUIDValue(project.ID), pgvalue.MustUUIDValue(env.ID), EnvironmentDetails{Slug: env.Slug, Name: "Renamed", ColorHex: env.ColorHex, HistoryRetention: HistoryRetentionPolicy{Mode: "until_environment_deletion"}})
	if err != nil || updated.HistoryRetentionMode != "until_environment_deletion" || updated.HistoryRetentionSeconds.Valid {
		t.Fatalf("explicit policy update failed: %+v %v", updated, err)
	}
	if _, err := CreateEnvironment(t.Context(), f.pool, f.orgID, pgvalue.MustUUIDValue(project.ID), EnvironmentDetails{Slug: "missing", ColorHex: "#123456"}, testExecutionLimits()); err == nil {
		t.Fatal("new Environment accepted no policy")
	}
}
