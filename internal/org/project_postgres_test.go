package org

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"math/big"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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

	t.Run("public identifier detail", func(t *testing.T) {
		for _, ref := range []string{"project-0001"} {
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
	project, environments, err := CreateProject(t.Context(), fixture.pool, fixture.manager, ProjectInput{
		ExecutionLimits: testExecutionLimits(),
		Slug:            "project", Name: "Project",
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
	_, _, err := CreateProject(t.Context(), database.Pool, projectManager(t, database.Pool, orgID), ProjectInput{
		ExecutionLimits: testExecutionLimits(),
		Slug:            "project", Name: "Project",
	})
	if !errors.Is(err, ErrNoRegion) {
		t.Fatalf("error = %v, want %v", err, ErrNoRegion)
	}
}

func TestCreateProjectRejectsUnknownDefaultRegion(t *testing.T) {
	fixture := newProjectFixture(t, 0, 0)
	_, _, err := CreateProject(t.Context(), fixture.pool, fixture.manager, ProjectInput{
		ExecutionLimits: testExecutionLimits(),
		Slug:            "project",
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
			_, _, err := CreateProject(context.Background(), fixture.pool, fixture.manager, ProjectInput{
				ExecutionLimits: testExecutionLimits(),
				Slug:            slug, Name: slug,
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

func TestUpdateProjectAndEnvironmentPreserveIdentifiers(t *testing.T) {
	f := newProjectFixture(t, 0, 0)
	project, environments, err := CreateProject(t.Context(), f.pool, f.manager, ProjectInput{ExecutionLimits: testExecutionLimits(), Slug: "project"})
	if err != nil {
		t.Fatal(err)
	}
	projectID := pgvalue.MustUUIDValue(project.ID)
	updated, err := UpdateProject(t.Context(), f.pool, f.manager, projectID, ProjectDetails{Slug: "attempted-rename", Name: "New display name"})
	if err != nil || updated.Slug != "project" || updated.Name != "New display name" {
		t.Fatalf("project identity changed: %+v %v", updated, err)
	}
	preview, err := CreateEnvironment(t.Context(), f.pool, f.manager, projectID, EnvironmentDetails{Slug: "preview", ColorHex: "#315FCE"}, testExecutionLimits())
	if err != nil {
		t.Fatal(err)
	}
	environments = append(environments, preview)
	for _, environment := range environments {
		updated, err := UpdateEnvironment(t.Context(), f.pool, f.manager, projectID, pgvalue.MustUUIDValue(environment.ID), EnvironmentDetails{Name: "New display name", Slug: "attempted-rename", ColorHex: environment.ColorHex})
		if err != nil || updated.Slug != environment.Slug || updated.Name != "New display name" {
			t.Fatalf("environment identity changed: %+v %v", updated, err)
		}
	}
	if _, err := CreateEnvironment(t.Context(), f.pool, f.manager, projectID, EnvironmentDetails{Slug: "preview", ColorHex: "#315FCE"}, testExecutionLimits()); !errors.Is(err, ErrEnvironmentSlugInUse) {
		t.Fatalf("duplicate ID: %v", err)
	}
	if _, err := GetProject(t.Context(), f.queries, f.orgID, projectID.String()); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("internal UUID accepted as public ID: %v", err)
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
			slices.Sort(elapsed)
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
				fixture.detail(t, "project-0001", fixture.orgID)
				for range 5 {
					fixture.store.statements.Store(0)
					started := time.Now()
					fixture.detail(t, "project-0001", fixture.orgID)
					detailElapsed = append(detailElapsed, time.Since(started))
					detailStatements = append(detailStatements, fixture.store.statements.Load())
				}
				slices.Sort(detailElapsed)
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
	manager    ManagingMember
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
	limits := testExecutionLimits()
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
				limits.MaxOutstandingAdmissions, int32(limits.MaxCausalDepth), limits.MaxResidentComputers, limits.MaxCPUMillis, limits.MaxMemoryBytes, limits.MaxReservedStorageBytes,
				limits.PreparationTimeoutMS, limits.AdmissionRatePerSecond, limits.AdmissionBurst, pgtype.Numeric{Int: big.NewInt(limits.AdmissionBurst), Valid: true}, time.Now(),
			})
		}
	}
	if _, err := database.Pool.CopyFrom(t.Context(), pgx.Identifier{"projects"},
		[]string{"id", "org_id", "default_region_id", "slug", "name", "is_default"},
		pgx.CopyFromRows(projectRows)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.CopyFrom(t.Context(), pgx.Identifier{"environments"},
		[]string{"id", "org_id", "project_id", "slug", "name", "color_hex", "is_default",
			"max_outstanding_admissions", "max_causal_depth", "max_resident_computers", "max_cpu_millis", "max_memory_bytes", "max_reserved_storage_bytes",
			"preparation_timeout_ms", "admission_rate_per_second", "admission_burst", "admission_tokens", "admission_refilled_at"},
		pgx.CopyFromRows(environmentRows)); err != nil {
		t.Fatal(err)
	}
	return projectFixture{
		pool: database.Pool, queries: queries, store: &countingQuerier{Querier: queries},
		orgID: orgID, projectIDs: projectIDs, manager: projectManager(t, database.Pool, orgID),
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
	input := ProjectInput{Slug: "configured", Name: "Configured"}
	if _, _, err := CreateProject(t.Context(), f.pool, f.manager, input); err == nil {
		t.Fatal("missing operating limits accepted")
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM projects WHERE org_id=$1`, f.orgID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed configuration leaked Project: %d %v", count, err)
	}
	input.ExecutionLimits = testExecutionLimits()
	_, environments, err := CreateProject(t.Context(), f.pool, f.manager, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range environments {
		var configured bool
		if err := f.pool.QueryRow(t.Context(), `SELECT admission_tokens=admission_burst AND admission_burst=$2 AND max_reserved_storage_bytes=$3 AND max_causal_depth=$4 AND preparation_timeout_ms=$5 FROM environments WHERE id=$1`, env.ID, input.ExecutionLimits.AdmissionBurst, input.ExecutionLimits.MaxReservedStorageBytes, input.ExecutionLimits.MaxCausalDepth, input.ExecutionLimits.PreparationTimeoutMS).Scan(&configured); err != nil || !configured {
			t.Fatalf("Environment limits not committed: %v %v", configured, err)
		}
	}
	if _, _, err := CreateProject(t.Context(), f.pool, f.manager, input); !errors.Is(err, ErrProjectSlugInUse) {
		t.Fatalf("duplicate Project accepted: %v", err)
	}
}

func TestEnvironmentCreationRequiresExecutionLimits(t *testing.T) {
	f := newProjectFixture(t, 0, 0)
	project, envs, err := CreateProject(t.Context(), f.pool, f.manager, ProjectInput{Slug: "limits", Name: "Limits", ExecutionLimits: testExecutionLimits()})
	if err != nil || len(envs) != 2 {
		t.Fatalf("create project: %d %v", len(envs), err)
	}
	partial := testExecutionLimits()
	partial.AdmissionBurst = 0
	if _, err := CreateEnvironment(t.Context(), f.pool, f.manager, pgvalue.MustUUIDValue(project.ID), EnvironmentDetails{Slug: "partial", Name: "Partial", ColorHex: "#123456"}, partial); err == nil {
		t.Fatal("Environment created without a complete limit set")
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM environments WHERE project_id=$1`, project.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("environments after rejected creation: %d %v", count, err)
	}
}

func TestHistoryRetentionPolicyDefaultsAndAtomic(t *testing.T) {
	f := newProjectFixture(t, 0, 0)
	project, envs, err := CreateProject(t.Context(), f.pool, f.manager, ProjectInput{Slug: "history", Name: "History", ExecutionLimits: testExecutionLimits()})
	if err != nil || len(envs) != 2 || !project.HistoryRetentionSeconds.Valid || project.HistoryRetentionSeconds.Int64 != 2592000 {
		t.Fatalf("default: %+v %v", project, err)
	}
	id := pgvalue.MustUUIDValue(project.ID)
	zero, positive := int64(0), int64(3600)
	if err := UpdateProjectHistoryRetention(t.Context(), f.pool, f.manager, id, &zero); err == nil {
		t.Fatal("accepted zero")
	}
	if err := UpdateProjectHistoryRetention(t.Context(), f.pool, ManagingMember{OrgID: uuid.NewV7(), UserID: f.manager.UserID, MemberID: f.manager.MemberID, AuditPolicy: auditpolicy.Policy{RetentionSeconds: 3600}}, id, nil); !errors.Is(err, ErrMemberManagementRequired) {
		t.Fatalf("cross-organization: %v", err)
	}
	for range 2 {
		if err := UpdateProjectHistoryRetention(t.Context(), f.pool, f.manager, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='project.retention_changed' AND target_id=$1 AND project_id=$1 AND environment_id IS NULL AND details->>'old_retention_seconds'='2592000' AND details->'new_retention_seconds'='null'::jsonb`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit/no-op: %d %v", count, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_retention_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected audit failure'; END $$`)
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE TRIGGER reject_retention_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_retention_audit()`)
	if err := UpdateProjectHistoryRetention(t.Context(), f.pool, f.manager, id, &positive); err == nil {
		t.Fatal("audit failure committed policy")
	}
	var kept bool
	if err := f.pool.QueryRow(t.Context(), `SELECT history_retention_seconds IS NULL FROM projects WHERE id=$1`, id).Scan(&kept); err != nil || !kept {
		t.Fatalf("rollback: %v %v", kept, err)
	}
}

func projectManager(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID) ManagingMember {
	t.Helper()
	userID, memberID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), pool, `INSERT INTO users(id,display_name) VALUES($1,'Project manager'); INSERT INTO org_members(id,org_id,user_id) VALUES($2,$3,$1)`, pgx.QueryExecModeSimpleProtocol, userID, memberID, orgID)
	for _, role := range auth.StandardRoles() {
		if role.Key == "organization_structure_administrator" {
			permissions := []string{}
			for _, p := range role.Permissions {
				permissions = append(permissions, string(p))
			}
			dbtest.GrantMember(t, pool, orgID, userID, nil, permissions...)
		}
	}
	return ManagingMember{Actor: audit.VerifiedLogin(userID, "fixture"), OrgID: orgID, UserID: userID, MemberID: memberID, AuditPolicy: auditpolicy.Policy{RetentionSeconds: 3600}}
}

func TestCreateProjectWaitsBeforeLockingMemberAndRechecksRemoval(t *testing.T) {
	f := newProjectFixture(t, 0, 0)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := db.New(tx).LockOrganizationForProjectDefaults(t.Context(), pgvalue.UUID(f.orgID)); err != nil {
		t.Fatal(err)
	}
	var blocker int
	if err := tx.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := CreateProject(ctx, f.pool, f.manager, ProjectInput{ExecutionLimits: testExecutionLimits(), Slug: "revoked", Name: "Revoked"})
		done <- err
	}()
	for {
		var waiting bool
		if err := f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("creation did not wait: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	// Member removal owns the organization before the member. A waiting creator
	// must not hold the member lock in the reverse order.
	if _, err := tx.Exec(ctx, `SELECT id FROM org_members WHERE id=$1 FOR UPDATE NOWAIT`, f.manager.MemberID); err != nil {
		t.Fatalf("creator inverted organization/member locks: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE org_members SET removed_at=clock_timestamp() WHERE id=$1`, f.manager.MemberID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrMemberManagementRequired) {
		t.Fatalf("removed member result after waiting: %v", err)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM projects WHERE org_id=$1`, f.orgID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("project persisted: %d %v", count, err)
	}
}

func TestProjectEnvironmentAuditAtomicity(t *testing.T) {
	f := newProjectFixture(t, 0, 0)
	input := ProjectInput{Slug: "audit", Name: "Audit", ExecutionLimits: testExecutionLimits()}
	broken := f.manager
	broken.AuditPolicy = auditpolicy.Policy{}
	if _, _, err := CreateProject(t.Context(), f.pool, broken, input); err == nil {
		t.Fatal("missing audit policy accepted")
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM projects WHERE org_id=$1`, f.orgID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial project: %d %v", count, err)
	}
	project, environments, err := CreateProject(t.Context(), f.pool, f.manager, input)
	if err != nil {
		t.Fatal(err)
	}
	projectID := pgvalue.MustUUIDValue(project.ID)
	for _, action := range []string{"project.created", "environment.created"} {
		want := 1
		if action == "environment.created" {
			want = len(environments)
		}
		if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE org_id=$1 AND action=$2 AND actor_user_id=$3 AND authorization_evidence->>'decision'='allowed'`, f.orgID, action, f.manager.UserID).Scan(&count); err != nil || count != want {
			t.Fatalf("%s receipts: %d want %d: %v", action, count, want, err)
		}
	}
	if _, err := UpdateProject(t.Context(), f.pool, broken, projectID, ProjectDetails{Name: "Changed"}); err == nil {
		t.Fatal("project update escaped audit failure")
	}
	if _, err := UpdateProject(t.Context(), f.pool, f.manager, projectID, ProjectDetails{Name: "Audit"}); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateProject(t.Context(), f.pool, f.manager, projectID, ProjectDetails{Name: "Changed"}); err != nil {
		t.Fatal(err)
	}
	details := EnvironmentDetails{Slug: "preview", Name: "Preview", ColorHex: "#123456"}
	if _, err := CreateEnvironment(t.Context(), f.pool, broken, projectID, details, testExecutionLimits()); err == nil {
		t.Fatal("environment create escaped audit failure")
	}
	env, err := CreateEnvironment(t.Context(), f.pool, f.manager, projectID, details, testExecutionLimits())
	if err != nil {
		t.Fatal(err)
	}
	envID := pgvalue.MustUUIDValue(env.ID)
	changed := details
	changed.Name = "Changed"
	if _, err := UpdateEnvironment(t.Context(), f.pool, broken, projectID, envID, changed); err == nil {
		t.Fatal("environment update escaped audit failure")
	}
	if _, err := UpdateEnvironment(t.Context(), f.pool, f.manager, projectID, envID, details); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateEnvironment(t.Context(), f.pool, f.manager, projectID, envID, changed); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"project.updated", "environment.updated"} {
		if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE org_id=$1 AND action=$2`, f.orgID, action).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s no-op/rollback receipts: %d %v", action, count, err)
		}
	}
}
