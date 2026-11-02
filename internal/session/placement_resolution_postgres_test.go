package session_test

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// organizationCapacity copies the fixture's group, pool and host into an
// active group owned by the environment's organization in the same region.
func organizationCapacity(t *testing.T, f fixture) (group, host uuid.UUID) {
	t.Helper()
	group, pool, token := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	host = uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO worker_group_tokens(id,token_hash) VALUES($4,decode(repeat('33',32),'hex'));
 INSERT INTO worker_groups(id,token_id,region_id,name,status,owner_org_id) SELECT $2,$4,g.region_id,'organization-group','active',e.org_id FROM worker_groups g JOIN environments e ON e.id=$7 WHERE g.id=$1;
 INSERT INTO worker_pools SELECT (jsonb_populate_record(NULL::worker_pools,to_jsonb(p)||jsonb_build_object('id',$3::uuid,'worker_group_id',$2::uuid))).* FROM worker_pools p JOIN worker_hosts h ON h.worker_pool_id=p.id WHERE h.id=$6;
 UPDATE worker_groups SET primary_pool_id=$3 WHERE id=$2;
 INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$5::uuid,'worker_group_id',$2::uuid,'worker_pool_id',$3::uuid,'resource_id','organization-host','current_service_id',$5::uuid))).* FROM worker_hosts h WHERE id=$6;
 INSERT INTO worker_pool_cpu_shapes SELECT (jsonb_populate_record(NULL::worker_pool_cpu_shapes,to_jsonb(s)||jsonb_build_object('worker_pool_id',$3::uuid))).* FROM worker_pool_cpu_shapes s JOIN worker_hosts h ON h.worker_pool_id=s.worker_pool_id WHERE h.id=$6`,
		pgx.QueryExecModeSimpleProtocol, f.group, group, pool, token, host, f.worker, f.env)
	return group, host
}

// placedComputerFixture admits a fresh Computer after assign runs, with one
// deployment host and one organization host able to run it.
func placedComputerFixture(t *testing.T, assign func(f fixture, group uuid.UUID)) (fixture, *dispatch.Allocator, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	pf, _, _ := preparationAllocationFixture(t)
	p, key := preparationPublicationTestFor(t, pf)
	root := p.captureCapacity(t, key, disk.SeedCapacity)
	if err := p.publisher.Publish(t.Context(), *pf.host(), p.ref, root, "certified"); err != nil {
		t.Fatal(err)
	}
	f := pf.fixture
	f.limits.MaxCPUMillis = 8000
	f.limits.MaxMemoryBytes = 8589934592
	f.limits.MaxResidentComputers = 3
	group, host := organizationCapacity(t, f)
	assign(f, group)
	a := testAllocator(t, f.pool, f.limits, nil)
	admitted, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.limits, f.pool, nil, f.caller(), f.startRequest("placed-allocation"))
	if err != nil {
		t.Fatal(err)
	}
	var computerID uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT computer_id FROM sessions WHERE environment_id=$1 AND id=$2`, f.env, admitted.SessionID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	return f, a, computerID, group, host
}

func assignDefault(t *testing.T) func(f fixture, group uuid.UUID) {
	return func(f fixture, group uuid.UUID) {
		if err := workergroup.SetPlacement(t.Context(), f.pool, f.env, workergroup.Placement{Name: definition.DefaultPlacement, WorkerGroupID: &group}); err != nil {
			t.Fatal(err)
		}
	}
}

func computerPin(t *testing.T, f fixture, computerID uuid.UUID) *uuid.UUID {
	t.Helper()
	var pin *uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT worker_group_id FROM computers WHERE environment_id=$1 AND id=$2`, f.env, computerID).Scan(&pin); err != nil {
		t.Fatal(err)
	}
	return pin
}

func TestPlacementPinsComputerToOrganizationGroupPostgres(t *testing.T) {
	f, a, computerID, group, host := placedComputerFixture(t, assignDefault(t))
	if pin := computerPin(t, f, computerID); pin == nil || *pin != group {
		t.Fatalf("pin = %v, want %s", pin, group)
	}
	if _, err := a.AllocateFreshLease(t.Context(), f.env, computerID, 1, f.worker); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("pinned Computer allocated on deployment capacity: %v", err)
	}
	if _, more, err := a.Reconcile(t.Context(), dispatch.AllocationPosition{}); err != nil || more {
		t.Fatalf("reconcile: %v %v", more, err)
	}
	r, err := dispatch.ReadLeaseAllocation(t.Context(), f.pool, f.env, computerID, 1)
	if err != nil || r.HostID != host {
		t.Fatalf("allocation = %+v %v, want organization host %s", r, err, host)
	}
}

func TestPlacementKeepsDeploymentComputerOffOrganizationGroupPostgres(t *testing.T) {
	f, a, computerID, _, host := placedComputerFixture(t, func(fixture, uuid.UUID) {})
	if pin := computerPin(t, f, computerID); pin != nil {
		t.Fatalf("default placement pinned to %s", *pin)
	}
	if _, err := a.AllocateFreshLease(t.Context(), f.env, computerID, 1, host); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("deployment Computer allocated on organization capacity: %v", err)
	}
	if _, more, err := a.Reconcile(t.Context(), dispatch.AllocationPosition{}); err != nil || more {
		t.Fatalf("reconcile: %v %v", more, err)
	}
	r, err := dispatch.ReadLeaseAllocation(t.Context(), f.pool, f.env, computerID, 1)
	if err != nil || r.HostID != f.worker {
		t.Fatalf("allocation = %+v %v, want deployment host %s", r, err, f.worker)
	}
}

func TestPlacementRebindingMovesNoComputerPostgres(t *testing.T) {
	f, _, computerID, _, _ := placedComputerFixture(t, assignDefault(t))
	before := computerPin(t, f, computerID)
	if err := workergroup.SetPlacement(t.Context(), f.pool, f.env, workergroup.Placement{Name: definition.DefaultPlacement}); err != nil {
		t.Fatal(err)
	}
	if after := computerPin(t, f, computerID); after == nil || *after != *before {
		t.Fatalf("pin moved from %v to %v", before, after)
	}
}

func TestPlacementUnassignedRejectsNewComputerPostgres(t *testing.T) {
	f := newAdmissionFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE template_definitions SET placement='secure' WHERE environment_id=$1`, f.env)
	_, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.limits, f.pool, nil, f.caller(), f.startRequest("unassigned"))
	var unassigned workergroup.PlacementUnassignedError
	if !errors.As(err, &unassigned) || unassigned.Placement != "secure" || !errors.Is(err, workergroup.ErrCapacityIncompatible) {
		t.Fatalf("start with unassigned placement: %v", err)
	}
}

func TestPlacementExistingComputerMustMatchAgentPlacementPostgres(t *testing.T) {
	f := newAdmissionFixture(t)
	group, _ := organizationCapacity(t, f)
	explicit := func(key string) error {
		req := f.startRequest(key)
		req.ComputerID = f.computer
		_, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.limits, f.pool, nil, f.caller(), req)
		return err
	}
	assignDefault(t)(f, group)
	if err := explicit("other-capacity"); !errors.Is(err, workergroup.ErrComputerPlacement) {
		t.Fatalf("deployment Computer reused under organization placement: %v", err)
	}
	if err := workergroup.SetPlacement(t.Context(), f.pool, f.env, workergroup.Placement{Name: definition.DefaultPlacement}); err != nil {
		t.Fatal(err)
	}
	if err := explicit("same-capacity"); err != nil {
		t.Fatalf("matching Computer rejected: %v", err)
	}
}

func TestPlacementDrainWaitsForPinnedComputersPostgres(t *testing.T) {
	f, _, computerID, group, _ := placedComputerFixture(t, assignDefault(t))
	var version int64
	if err := f.pool.QueryRow(t.Context(), `SELECT claim_version FROM worker_groups WHERE id=$1`, group).Scan(&version); err != nil {
		t.Fatal(err)
	}
	drain := func() error {
		_, err := workergroup.BeginGroupDrain(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Internal(audit.ControlPlane), group, version)
		return err
	}
	if err := drain(); err == nil {
		t.Fatal("drained a group with a pinned Computer")
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET deleted_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, f.env, computerID)
	if err := drain(); err != nil {
		t.Fatalf("drain after deletion: %v", err)
	}
}

func TestPlacementDemandCountsOnlyTheServingGroupPostgres(t *testing.T) {
	for _, tc := range []struct {
		name   string
		assign func(t *testing.T) func(fixture, uuid.UUID)
		pinned bool
	}{
		{"organization", assignDefault, true},
		{"deployment", func(*testing.T) func(fixture, uuid.UUID) { return func(fixture, uuid.UUID) {} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, computerID, group, _ := placedComputerFixture(t, tc.assign(t))
			planned := func(serving uuid.UUID) bool {
				rows, err := db.New(f.pool).ListAllocationPlanningDemand(t.Context(), db.ListAllocationPlanningDemandParams{
					MaxCpuMillis: f.limits.MaxCPUMillis, MaxMemoryBytes: f.limits.MaxMemoryBytes, MaxResidentComputers: f.limits.MaxResidentComputers,
					RegionID: "test", WorkerGroupID: pgvalue.UUID(serving), RowLimit: 100,
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					if row.Kind == "computer" && pgvalue.MustUUIDValue(row.OwnerID) == computerID {
						return true
					}
				}
				return false
			}
			if planned(group) != tc.pinned || planned(f.group) == tc.pinned {
				t.Fatalf("planning demand: organization=%v deployment=%v", planned(group), planned(f.group))
			}
			queued, err := workergroup.HasQueuedDemand(t.Context(), f.pool, group)
			if err != nil || queued != tc.pinned {
				t.Fatalf("organization group queued demand = %v %v", queued, err)
			}
		})
	}
}

func TestPlacementRetiredGroupRejectsNewWorkAtOncePostgres(t *testing.T) {
	f := newAdmissionFixture(t)
	group, _ := organizationCapacity(t, f)
	assignDefault(t)(f, group)
	// Retiring a group the current deployment's placement uses stays possible
	// for platform safety; only placement edits and promotion keep the binding.
	if _, err := workergroup.BeginGroupDrain(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Internal(audit.ControlPlane), group, 1); err != nil {
		t.Fatalf("drain a group in use: %v", err)
	}
	count := func(table string) int {
		var n int
		if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table+` WHERE environment_id=$1`, f.env).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sessions, computers := count("sessions"), count("computers")
	for _, status := range []string{"draining", "disabled"} {
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET status=$2 WHERE id=$1`, group, status)
		_, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.limits, f.pool, nil, f.caller(), f.startRequest(status))
		var retired workergroup.GroupRetiredError
		if !errors.As(err, &retired) || retired.Placement != definition.DefaultPlacement || retired.Status != status || !errors.Is(err, workergroup.ErrCapacityIncompatible) {
			t.Fatalf("start under a %s group: %v", status, err)
		}
	}
	if count("sessions") != sessions || count("computers") != computers {
		t.Fatal("rejected work was queued")
	}
}
