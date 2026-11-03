package deployment

import (
	"errors"
	"slices"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPlacementBindingInvariantPostgres(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	first, err := register(t.Context(), policy, f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	env := first.EnvironmentID
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE template_definitions SET placement='secure' WHERE environment_id=$1 AND deployment_id=$2`, env, first.ID)
	promote := func() error {
		_, err := Promote(t.Context(), f.pool, policy, f.principal(), f.scope(), first.ID, nil)
		return err
	}
	assertUnbound := func(err error, unassigned, retired []string) {
		t.Helper()
		var unbound workergroup.UnboundPlacementsError
		if !errors.As(err, &unbound) || !slices.Equal(unbound.Unassigned, unassigned) || !slices.Equal(unbound.Retired, retired) {
			t.Fatalf("error = %v, want unassigned %v and retired %v", err, unassigned, retired)
		}
	}
	assertUnbound(promote(), []string{"secure"}, nil)

	group, token := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO worker_group_tokens(id,token_hash) VALUES($2,decode(repeat('44',32),'hex'));
 INSERT INTO worker_groups(id,token_id,region_id,name,status,owner_org_id) SELECT $1,$2,p.default_region_id,'secure','disabled',e.org_id FROM environments e JOIN projects p ON p.id=e.project_id WHERE e.id=$3`,
		pgx.QueryExecModeSimpleProtocol, group, token, env)
	if err := workergroup.SetPlacement(t.Context(), f.pool, testPlacementEditor, env, workergroup.Placement{Name: "secure", WorkerGroupID: &group}); err != nil {
		t.Fatalf("assign before the deployment is current: %v", err)
	}
	assertUnbound(promote(), nil, []string{"secure"})
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET status='draining' WHERE id=$1`, group)
	assertUnbound(promote(), nil, []string{"secure"})
	var current *uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT current_deployment_id FROM environments WHERE id=$1`, env).Scan(&current); err != nil || current != nil {
		t.Fatalf("failed promotion changed the current deployment: %v %v", current, err)
	}

	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET status='active' WHERE id=$1`, group)
	// A passing check holds the group, so it cannot retire before cutover.
	check, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := workergroup.RequireDeploymentPlacements(t.Context(), check, env, first.ID); err != nil {
		t.Fatal(err)
	}
	retire, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = retire.Exec(t.Context(), `SET LOCAL lock_timeout='100ms'; UPDATE worker_groups SET status='draining' WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, group)
	if pgErr := (*pgconn.PgError)(nil); !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("group retired under a passing check: %v", err)
	}
	if err := retire.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := check.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := promote(); err != nil {
		t.Fatalf("promotion with bound placements: %v", err)
	}

	// The placement writer keeps the current deployment bound.
	if _, err := workergroup.RemovePlacement(t.Context(), f.pool, testPlacementEditor, env, "secure"); err == nil {
		t.Fatal("removed a placement the current deployment uses")
	} else {
		assertUnbound(err, []string{"secure"}, nil)
	}
	other, otherToken := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO worker_group_tokens(id,token_hash) VALUES($2,decode(repeat('55',32),'hex'));
 INSERT INTO worker_groups(id,token_id,region_id,name,status,owner_org_id) SELECT $1,$2,region_id,'retired','disabled',owner_org_id FROM worker_groups WHERE id=$3`,
		pgx.QueryExecModeSimpleProtocol, other, otherToken, group)
	assertUnbound(workergroup.SetPlacement(t.Context(), f.pool, testPlacementEditor, env, workergroup.Placement{Name: "secure", WorkerGroupID: &other}), nil, []string{"secure"})
	if err := workergroup.SetPlacement(t.Context(), f.pool, testPlacementEditor, env, workergroup.Placement{Name: "secure"}); err != nil {
		t.Fatalf("rebinding a used placement to deployment capacity: %v", err)
	}
	if err := workergroup.SetPlacement(t.Context(), f.pool, testPlacementEditor, env, workergroup.Placement{Name: "unused", WorkerGroupID: &other}); err != nil {
		t.Fatalf("assigning an unused placement: %v", err)
	}
	if removed, err := workergroup.RemovePlacement(t.Context(), f.pool, testPlacementEditor, env, "unused"); err != nil || !removed {
		t.Fatalf("removing an unused placement = %v %v", removed, err)
	}
	// A used placement whose group is draining blocks only edits to itself.
	if err := workergroup.SetPlacement(t.Context(), f.pool, testPlacementEditor, env, workergroup.Placement{Name: "secure", WorkerGroupID: &group}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET status='draining' WHERE id=$1`, group)
	if err := workergroup.SetPlacement(t.Context(), f.pool, testPlacementEditor, env, workergroup.Placement{Name: "unused"}); err != nil {
		t.Fatalf("an unrelated edit blocked by a draining group: %v", err)
	}
	assertUnbound(workergroup.SetPlacement(t.Context(), f.pool, testPlacementEditor, env, workergroup.Placement{Name: "secure", WorkerGroupID: &other}), nil, []string{"secure"})
	if err := workergroup.SetPlacement(t.Context(), f.pool, testPlacementEditor, env, workergroup.Placement{Name: "secure"}); err != nil {
		t.Fatalf("rebinding away from a draining group: %v", err)
	}
}
