package workergroup_test

import (
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5"
)

func TestAllocationPlanningCountsPhysicalOwnersAndRetainsCharges(t *testing.T) {
	f := sessiontest.New(t)
	var org, project, pool uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT e.org_id,e.project_id,h.worker_pool_id FROM environments e,worker_hosts h WHERE e.id=$1 AND h.id=$2`, f.Environment, f.Worker).Scan(&org, &project, &pool); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_groups SET primary_pool_id=$2 WHERE id=$1`, f.Group, pool)
	for _, key := range []string{"first", "second"} {
		if _, err := command.Create(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, command.CreateRequest{OrgID: org, ProjectID: project, EnvironmentID: f.Environment, ComputerID: f.Computer, Creator: command.Creator{Kind: string(auth.PrincipalKindSession), ID: f.User, UserID: f.User}, Argv: []string{"true"}, IdempotencyKey: key}); err != nil {
			t.Fatal(err)
		}
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE template_definitions SET cpu_millis=1000,memory_bytes=536870912 WHERE environment_id=$1`, f.Environment)
	// This incompatible preparation sorts before the eligible candidate. It must
	// not consume the one-row budget, even on repeated planning passes.
	incompatibleSpec, incompatible := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO image_specs(environment_id,id,spec_digest,spec,seed) VALUES($1,$2,'sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee','{}','{}')`, f.Environment, incompatibleSpec)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO templates(environment_id,id,key) VALUES($1,$3,'incompatible');
 INSERT INTO template_definitions(environment_id,deployment_id,template_id,image_spec_id,cpu_millis,memory_bytes) VALUES($1,$2,$3,$3,2000,536870912)`, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Deployment, incompatibleSpec)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO image_builds(environment_id,id,image_spec_id,idempotency_key,status,deadline_at) VALUES($1,$2,$3,'incompatible','queued',clock_timestamp()+interval '10 minutes')`, f.Environment, incompatible, incompatibleSpec)
	preparation := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO image_builds(environment_id,id,image_spec_id,idempotency_key,status,deadline_at) VALUES($1,$2,$3,'queued','queued',clock_timestamp()+interval '10 minutes')`, f.Environment, preparation, f.Deployment)
	q := db.New(f.Pool)
	rows, err := q.ListAllocationPlanningDemand(t.Context(), db.ListAllocationPlanningDemandParams{RegionID: "test", WorkerGroupID: pgvalue.UUID(f.Group), RowLimit: 1})
	if err != nil || len(rows) != 1 || pgvalue.MustUUIDValue(rows[0].OwnerID) != preparation {
		t.Fatalf("queued demand=%+v err=%v", rows, err)
	}
	var group uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT worker_group_id FROM worker_hosts WHERE id=$1`, f.Worker).Scan(&group); err != nil {
		t.Fatal(err)
	}
	assertCharge := func() {
		t.Helper()
		charged, err := q.ListAllocationPlanningChargedPools(t.Context(), pgvalue.UUID(group))
		if err != nil || len(charged) != 1 || charged[0] != pgvalue.UUID(pool) {
			t.Fatalf("physical charges=%+v err=%v", charged, err)
		}
	}
	assertCharge()
	// Logical expiry retains physical reservations until stop evidence.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET status='lost',expires_at=clock_timestamp()-interval '1 hour' WHERE computer_id=$1`, f.Computer)
	assertCharge()
	// A resident Computer and its Commands never occupy the bounded queued page.
	for range 2 {
		rows, err = q.ListAllocationPlanningDemand(t.Context(), db.ListAllocationPlanningDemandParams{RegionID: "test", WorkerGroupID: pgvalue.UUID(f.Group), RowLimit: 1})
		if err != nil || len(rows) != 1 || pgvalue.MustUUIDValue(rows[0].OwnerID) != preparation {
			t.Fatalf("repeated queued page=%+v %v", rows, err)
		}
	}
	// Even expired, unfenced allocations consume the environment quota.
	// Keep blocked demand visible so admission can detect incomplete placement.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET max_cpu_millis=(SELECT sum(reserved_cpu_millis) FROM computer_leases WHERE environment_id=$1 AND fenced_at IS NULL) WHERE id=$1`, f.Environment)
	rows, err = q.ListAllocationPlanningDemand(t.Context(), db.ListAllocationPlanningDemandParams{RegionID: "test", WorkerGroupID: pgvalue.UUID(f.Group), RowLimit: 10})
	if err != nil || len(rows) != 1 || rows[0].RemainingCpuMillis != 0 {
		t.Fatalf("quota-blocked demand=%+v %v", rows, err)
	}
	assertCharge()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET max_cpu_millis=1024000 WHERE id=$1`, f.Environment)
	secret := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO secrets(id,environment_id,name,status,revoked_at) VALUES($1,$2,'TOKEN','revoked',clock_timestamp())`, secret, f.Environment)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO secret_bindings(environment_id,image_spec_id,secret_id,placement_kind,placement_target,mode) VALUES($1,$2,$3,'env','TOKEN','raw')`, f.Environment, f.Deployment, secret)
	rows, err = q.ListAllocationPlanningDemand(t.Context(), db.ListAllocationPlanningDemandParams{RegionID: "test", WorkerGroupID: pgvalue.UUID(f.Group), RowLimit: 10})
	if err != nil || len(rows) != 0 {
		t.Fatalf("revoked preparation demand=%+v %v", rows, err)
	}
	other, err := q.ListAllocationPlanningDemand(t.Context(), db.ListAllocationPlanningDemandParams{RegionID: "other", WorkerGroupID: pgvalue.UUID(f.Group), RowLimit: 10})
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-region demand=%+v %v", other, err)
	}
}
