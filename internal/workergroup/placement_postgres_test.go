package workergroup

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const otherRegionID = "us-west-2"

// placementFixture adds two organizations to a supply fixture, each with an
// environment in the fixture region, and the first with another environment
// in a second region.
type placementFixture struct {
	supplyFixture
	org, otherOrg          uuid.UUID
	env, westEnv, otherEnv uuid.UUID
}

func newPlacementFixture(t *testing.T) placementFixture {
	t.Helper()
	f := placementFixture{supplyFixture: newSupplyFixture(t), org: uuid.NewV7(), otherOrg: uuid.NewV7()}
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO regions (id, display_name) VALUES ($1, 'West')`, otherRegionID)
	f.env = f.environment(t, f.org, fixtureRegionID)
	f.westEnv = f.environment(t, f.org, otherRegionID)
	f.otherEnv = f.environment(t, f.otherOrg, fixtureRegionID)
	return f
}

func (f placementFixture) environment(t *testing.T, org uuid.UUID, regionID string) uuid.UUID {
	t.Helper()
	project, env := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO organizations (id, name, slug) VALUES ($1, 'Org', $2) ON CONFLICT (id) DO NOTHING`, org, org.String())
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO projects (id, org_id, default_region_id, slug, name) VALUES ($1, $2, $3, $4, 'Project')`, project, org, regionID, project.String())
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO environments (id, org_id, project_id, slug, name, color_hex, admission_tokens, admission_refilled_at) VALUES ($1, $2, $3, 'production', 'Production', '#315FCE', 1000000, clock_timestamp())`, env, org, project)
	return env
}

// createGroup creates an active group owned by owner, or a deployment group
// when owner is nil, and returns it with its raw enrollment token.
func (f placementFixture) createGroup(t *testing.T, owner *uuid.UUID, regionID, name string) (db.WorkerGroup, string, error) {
	t.Helper()
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	params := db.CreateWorkerGroupParams{
		ID: pgvalue.NewUUIDv7(), TokenID: pgvalue.NewUUIDv7(), TokenHash: token.Hash, RegionID: regionID, Name: name,
	}
	if owner != nil {
		params.OwnerOrgID = pgvalue.UUID(*owner)
	}
	group, err := f.q.CreateWorkerGroup(t.Context(), params)
	return group, token.Raw, err
}

func (f placementFixture) organizationGroup(t *testing.T, owner uuid.UUID, regionID, name string) uuid.UUID {
	t.Helper()
	group, _, err := f.createGroup(t, &owner, regionID, name)
	if err != nil {
		t.Fatal(err)
	}
	return pgvalue.MustUUIDValue(group.ID)
}

// runtimeRolePool connects as a runtime role granted the application
// privileges, not as the migration owner.
func runtimeRolePool(t *testing.T, owner *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	suffix := uuid.NewV7().String()
	runtimeRole, maintenanceRole := "runtime_"+suffix, "maintenance_"+suffix
	for _, role := range []string{runtimeRole, maintenanceRole} {
		dbtest.MustExec(t, t.Context(), owner, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN")
	}
	t.Cleanup(func() {
		for _, role := range []string{runtimeRole, maintenanceRole} {
			_, _ = owner.Exec(t.Context(), "DROP OWNED BY "+pgx.Identifier{role}.Sanitize())
			_, _ = owner.Exec(t.Context(), "DROP ROLE "+pgx.Identifier{role}.Sanitize())
		}
	})
	dbtest.MustExec(t, t.Context(), owner, "GRANT USAGE ON SCHEMA public TO "+pgx.Identifier{runtimeRole}.Sanitize())
	dsn := owner.Config().ConnString()
	if err := schema.ConfigurePrivileges(t.Context(), dsn, runtimeRole, maintenanceRole); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = runtimeRole
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func pgErrorCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestWorkerGroupOwnershipUniquenessPostgres(t *testing.T) {
	f := newPlacementFixture(t)
	// Organization groups coexist with the region's deployment group and with
	// each other, and may reuse a name another owner uses.
	f.organizationGroup(t, f.org, fixtureRegionID, "supply-test")
	f.organizationGroup(t, f.org, fixtureRegionID, "lab")
	f.organizationGroup(t, f.otherOrg, fixtureRegionID, "supply-test")
	f.organizationGroup(t, f.org, otherRegionID, "lab")
	if _, _, err := f.createGroup(t, &f.org, fixtureRegionID, "lab"); pgErrorCode(err) != "23505" {
		t.Fatalf("duplicate organization group name error = %v, want unique violation", err)
	}
	if _, _, err := f.createGroup(t, nil, fixtureRegionID, "second"); pgErrorCode(err) != "23505" {
		t.Fatalf("second active deployment group error = %v, want unique violation", err)
	}
	missing := uuid.NewV7()
	if _, _, err := f.createGroup(t, &missing, fixtureRegionID, "orphan"); pgErrorCode(err) != "23503" {
		t.Fatalf("group of a missing organization error = %v, want foreign key violation", err)
	}
	// A disabled deployment group keeps its name and frees the region's single
	// active deployment group.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET status='disabled' WHERE id=$1`, f.group.ID)
	if _, _, err := f.createGroup(t, nil, fixtureRegionID, "supply-test"); pgErrorCode(err) != "23505" {
		t.Fatalf("reused deployment group name error = %v, want unique violation", err)
	}
	if _, _, err := f.createGroup(t, nil, fixtureRegionID, "replacement"); err != nil {
		t.Fatal(err)
	}
	group, err := f.q.GetDeploymentWorkerGroupByRegionName(t.Context(), db.GetDeploymentWorkerGroupByRegionNameParams{RegionID: fixtureRegionID, Name: "supply-test"})
	if err != nil || group.ID != f.group.ID || group.OwnerOrgID.Valid {
		t.Fatalf("deployment group by name = %+v, %v", group, err)
	}
}

func TestWorkerGroupOwnerIsImmutableForRuntimePostgres(t *testing.T) {
	f := newPlacementFixture(t)
	owned := f.organizationGroup(t, f.org, fixtureRegionID, "lab")
	runtime := runtimeRolePool(t, f.pool)
	if _, err := runtime.Exec(t.Context(), `UPDATE worker_groups SET owner_org_id=NULL WHERE id=$1`, owned); pgErrorCode(err) != "42501" {
		t.Fatalf("clearing a group owner error = %v, want insufficient privilege", err)
	}
	if _, err := runtime.Exec(t.Context(), `UPDATE worker_groups SET owner_org_id=$2 WHERE id=$1`, owned, f.otherOrg); pgErrorCode(err) != "42501" {
		t.Fatalf("re-owning a group error = %v, want insufficient privilege", err)
	}
	if _, err := runtime.Exec(t.Context(), `UPDATE worker_groups SET owner_org_id=$2 WHERE id=$1`, f.group.ID, f.org); pgErrorCode(err) != "42501" {
		t.Fatalf("assigning a deployment group an owner error = %v, want insufficient privilege", err)
	}
	if _, err := runtime.Exec(t.Context(), `UPDATE worker_groups SET description='runs', status='paused' WHERE id=$1`, owned); err != nil {
		t.Fatalf("runtime group update: %v", err)
	}
	if _, err := UpdateGroupDescription(t.Context(), db.New(runtime), owned, "labs"); err != nil {
		t.Fatal(err)
	}
	var owner uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT owner_org_id FROM worker_groups WHERE id=$1`, owned).Scan(&owner); err != nil || owner != f.org {
		t.Fatalf("owner = %s, %v", owner, err)
	}
	// A Computer's pin is set when it is created and never moves.
	var pinUpdatable, computerUpdatable bool
	if err := runtime.QueryRow(t.Context(), `SELECT
		bool_or(has_column_privilege('computers', c, 'UPDATE')),
		has_column_privilege('computers', 'unused_since', 'UPDATE')
		FROM unnest(ARRAY['worker_group_org_id','worker_group_project_id','worker_group_region_id','worker_group_id']) AS c`).Scan(&pinUpdatable, &computerUpdatable); err != nil {
		t.Fatal(err)
	}
	if pinUpdatable || !computerUpdatable {
		t.Fatalf("runtime computer update privileges: pin %t, other columns %t", pinUpdatable, computerUpdatable)
	}
}

func TestPlacementsPostgres(t *testing.T) {
	f := newPlacementFixture(t)
	secure := f.organizationGroup(t, f.org, fixtureRegionID, "secure")
	lab := f.organizationGroup(t, f.org, fixtureRegionID, "lab")
	west := f.organizationGroup(t, f.org, otherRegionID, "west")
	foreign := f.organizationGroup(t, f.otherOrg, fixtureRegionID, "foreign")
	deployment := pgvalue.MustUUIDValue(f.group.ID)

	placements, err := ListPlacements(t.Context(), f.q, f.env)
	if err != nil || !slices.Equal(placements, []Placement{{Name: DefaultPlacement}}) {
		t.Fatalf("initial placements = %+v, %v", placements, err)
	}
	for _, placement := range []Placement{
		{Name: "secure", WorkerGroupID: &secure},
		{Name: "edge"},
		{Name: DefaultPlacement, WorkerGroupID: &lab},
		{Name: "Zeta", WorkerGroupID: &lab},
	} {
		if err := SetPlacement(t.Context(), f.q, f.env, placement); err != nil {
			t.Fatalf("SetPlacement(%s): %v", placement.Name, err)
		}
	}
	// Reassigning keeps one row per name.
	if err := SetPlacement(t.Context(), f.q, f.env, Placement{Name: "secure", WorkerGroupID: &lab}); err != nil {
		t.Fatal(err)
	}
	assertPlacements(t, f, f.env, []Placement{{Name: "Zeta", WorkerGroupID: &lab}, {Name: DefaultPlacement, WorkerGroupID: &lab}, {Name: "edge"}, {Name: "secure", WorkerGroupID: &lab}})

	// A group of another organization, another region or the deployment is
	// never assignable.
	for _, group := range []uuid.UUID{foreign, west, deployment, uuid.NewV7()} {
		if err := SetPlacement(t.Context(), f.q, f.env, Placement{Name: "secure", WorkerGroupID: &group}); !errors.Is(err, ErrPlacementGroup) {
			t.Fatalf("SetPlacement(group %s) error = %v, want ErrPlacementGroup", group, err)
		}
	}
	if err := SetPlacement(t.Context(), f.q, f.westEnv, Placement{Name: "secure", WorkerGroupID: &west}); err != nil {
		t.Fatalf("same-region organization group: %v", err)
	}
	if err := SetPlacement(t.Context(), f.q, f.otherEnv, Placement{Name: "secure", WorkerGroupID: &secure}); !errors.Is(err, ErrPlacementGroup) {
		t.Fatalf("other organization's environment error = %v, want ErrPlacementGroup", err)
	}
	if err := SetPlacement(t.Context(), f.q, uuid.NewV7(), Placement{Name: "secure"}); !errors.Is(err, ErrEnvironmentNotFound) {
		t.Fatalf("missing environment error = %v", err)
	}
	var input InputError
	for _, name := range []string{"", "-edge", "with space", strings.Repeat("a", 129)} {
		if err := SetPlacement(t.Context(), f.q, f.env, Placement{Name: name}); !errors.As(err, &input) {
			t.Fatalf("SetPlacement(%q) error = %v, want InputError", name, err)
		}
	}

	// Default maps to deployment capacity only by having no row.
	if _, err := f.pool.Exec(t.Context(), `UPDATE environment_placements SET worker_group_id=NULL WHERE environment_id=$1 AND name='default'`, f.env); pgErrorCode(err) != "23514" {
		t.Fatalf("deployment default row error = %v, want check violation", err)
	}
	if err := SetPlacement(t.Context(), f.q, f.env, Placement{Name: DefaultPlacement}); err != nil {
		t.Fatal(err)
	}
	removed, err := RemovePlacement(t.Context(), f.q, f.env, "edge")
	if err != nil || !removed {
		t.Fatalf("RemovePlacement(edge) = %t, %v", removed, err)
	}
	if removed, err := RemovePlacement(t.Context(), f.q, f.env, "edge"); err != nil || removed {
		t.Fatalf("second RemovePlacement(edge) = %t, %v", removed, err)
	}
	assertPlacements(t, f, f.env, []Placement{{Name: "Zeta", WorkerGroupID: &lab}, {Name: DefaultPlacement}, {Name: "secure", WorkerGroupID: &lab}})

	// An assigned group's owner and region stay consistent with its placements.
	if _, err := f.pool.Exec(t.Context(), `UPDATE worker_groups SET owner_org_id=$2 WHERE id=$1`, lab, f.otherOrg); pgErrorCode(err) != "23503" {
		t.Fatalf("re-owning an assigned group error = %v, want foreign key violation", err)
	}
}

func assertPlacements(t *testing.T, f placementFixture, env uuid.UUID, want []Placement) {
	t.Helper()
	got, err := ListPlacements(t.Context(), f.q, env)
	if err != nil {
		t.Fatal(err)
	}
	equal := slices.EqualFunc(got, want, func(a, b Placement) bool {
		return a.Name == b.Name && (a.WorkerGroupID == nil) == (b.WorkerGroupID == nil) &&
			(a.WorkerGroupID == nil || *a.WorkerGroupID == *b.WorkerGroupID)
	})
	if !equal {
		t.Fatalf("placements = %+v, want %+v", got, want)
	}
}

func TestHostAuthenticationReadsGroupOwnerPostgres(t *testing.T) {
	f := newPlacementFixture(t)
	cfg := testHostAuthConfig(t)
	deploymentHost := f.enroll(t, cfg, "default", "i-deployment")
	if principal := f.authenticate(t, AuthenticateRecoveringHost, cfg, f.exchange(t, cfg, deploymentHost, uuid.NewV7().String())); principal.OwnerOrgID != nil {
		t.Fatalf("deployment host owner = %s", principal.OwnerOrgID)
	}
	group, token, err := f.createGroup(t, &f.org, fixtureRegionID, "lab")
	if err != nil {
		t.Fatal(err)
	}
	owned := f.supplyFixture
	owned.group, owned.enrollmentToken = group, token
	host := owned.enroll(t, cfg, "default", "i-organization")
	principal := owned.authenticate(t, AuthenticateRecoveringHost, cfg, owned.exchange(t, cfg, host, uuid.NewV7().String()))
	if principal.GroupID != pgvalue.MustUUIDValue(group.ID) || principal.OwnerOrgID == nil || *principal.OwnerOrgID != f.org {
		t.Fatalf("organization host principal = %+v", principal)
	}
}
