package session_test

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func pendingCapacityComputer(t *testing.T, f fixture, env uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computers(environment_id,id,image_spec_id,preparation_deadline_at,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,storage_reservation_bytes) VALUES($1,$2,$3,clock_timestamp()+interval '10 minutes',$3,$3,1000,536870912,34359738368)`, env, id, f.deployment)
	return id
}
func admitCapacityComputer(ctx context.Context, f fixture, env, id uuid.UUID) error {
	return db.RunTx(ctx, f.pool, func(tx pgx.Tx) error {
		if err := workergroup.LockExecutionSupply(ctx, tx, env); err != nil {
			return err
		}
		if _, err := org.LockAdmission(ctx, tx, env); err != nil {
			return err
		}
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, id).Scan(&locked); err != nil {
			return err
		}
		return computer.RequireExecutionCapacity(ctx, tx, env, id)
	})
}
func TestExecutionAdmissionReservesPendingEnvironmentQuotaOnce(t *testing.T) {
	f := newAdmissionFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_cpu_millis=2000,max_resident_computers=2 WHERE id=$1`, f.env)
	first, second := pendingCapacityComputer(t, f, f.env), pendingCapacityComputer(t, f, f.env)
	if err := admitCapacityComputer(t.Context(), f, f.env, first); err != nil {
		t.Fatal(err)
	}
	if demand, err := workergroup.HasQueuedDemand(t.Context(), f.pool, f.group); err != nil || !demand {
		t.Fatalf("Computer claim without Sessions was invisible to scale-in: %v %v", demand, err)
	}
	var before, after time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT execution_deadline_at FROM computers WHERE id=$1`, first).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// Joining an accepted episode keeps its bound even if the default changes.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET preparation_timeout_ms=preparation_timeout_ms+1000 WHERE id=$1`, f.env)
	if err := admitCapacityComputer(t.Context(), f, f.env, first); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT execution_deadline_at FROM computers WHERE id=$1`, first).Scan(&after); err != nil || !before.Equal(after) {
		t.Fatalf("join extended episode: %v %v %v", before, after, err)
	}
	if err := admitCapacityComputer(t.Context(), f, f.env, second); !errors.Is(err, computer.ErrExecutionLimit) {
		t.Fatalf("second claim: %v", err)
	}
	budget, err := db.ReadExecutionBudget(t.Context(), f.pool, f.env)
	if err != nil || budget.CPU != 0 || budget.Residents != 0 {
		t.Fatalf("budget=%+v %v", budget, err)
	}
}
func TestExecutionAdmissionSerializesLastHostSlotAcrossEnvironments(t *testing.T) {
	f := newAdmissionFixture(t)
	other := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO environments SELECT (jsonb_populate_record(NULL::environments,to_jsonb(e)||jsonb_build_object('id',$2::uuid,'slug','other','current_deployment_id',NULL))).* FROM environments e WHERE id=$1;
 INSERT INTO deployments SELECT (jsonb_populate_record(NULL::deployments,to_jsonb(d)||jsonb_build_object('environment_id',$2::uuid))).* FROM deployments d WHERE environment_id=$1;
 INSERT INTO image_specs SELECT (jsonb_populate_record(NULL::image_specs,to_jsonb(s)||jsonb_build_object('environment_id',$2::uuid))).* FROM image_specs s WHERE environment_id=$1;
 INSERT INTO templates SELECT (jsonb_populate_record(NULL::templates,to_jsonb(d)||jsonb_build_object('environment_id',$2::uuid))).* FROM templates d WHERE environment_id=$1;
 INSERT INTO template_definitions SELECT (jsonb_populate_record(NULL::template_definitions,to_jsonb(d)||jsonb_build_object('environment_id',$2::uuid))).* FROM template_definitions d WHERE environment_id=$1;
 UPDATE worker_groups SET capacity_observed_at=NULL,capacity_supply_fingerprint=NULL,capacity_pools=NULL;
 UPDATE worker_hosts SET epoch_cpu_millis=2000,epoch_memory_bytes=2147483648,epoch_guest_ephemeral_disk_bytes=2147483648,max_vm_slots=2,max_vm_starts=2,disk_slots_for_real_work=2 WHERE id=$3`, pgx.QueryExecModeSimpleProtocol, f.env, other, f.worker)
	first, second := pendingCapacityComputer(t, f, f.env), pendingCapacityComputer(t, f, other)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	start := make(chan struct{})
	for _, target := range []struct{ env, id uuid.UUID }{{f.env, first}, {other, second}} {
		wg.Go(func() { <-start; errs <- admitCapacityComputer(t.Context(), f, target.env, target.id) })
	}
	close(start)
	wg.Wait()
	close(errs)
	accepted, rejected := 0, 0
	for err := range errs {
		if err == nil {
			accepted++
		} else if errors.Is(err, workergroup.ErrCapacityUnavailable) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d", accepted, rejected)
	}
	var claims int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computers WHERE execution_deadline_at IS NOT NULL`).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("claims=%d %v", claims, err)
	}
}
func TestExecutionAdmissionDeadlineFailsQueuedWorkWithoutRetry(t *testing.T) {
	f := newAdmissionFixture(t)
	first, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), f.startRequest("bounded"))
	if err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	if err = f.pool.QueryRow(t.Context(), `SELECT computer_id FROM sessions WHERE id=$1`, first.SessionID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET execution_deadline_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, id)
	if err = dispatch.ReconcileExecutionAdmissions(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool); err != nil {
		t.Fatal(err)
	}
	var state, code string
	if err = f.pool.QueryRow(t.Context(), `SELECT status,error_code FROM turns WHERE id=$1`, first.TurnID).Scan(&state, &code); err != nil || state != "failed" || code != "execution_deadline_exceeded" {
		t.Fatalf("turn=%s %s %v", state, code, err)
	}
	if err = dispatch.ReconcileExecutionAdmissions(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool); err != nil {
		t.Fatal(err)
	}
	var events int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_events WHERE turn_id=$1 AND kind='turn.failed'`, first.TurnID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("events=%d %v", events, err)
	}
	// Only new explicit input can start a new episode after settlement.
	_, err = session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: first.SessionID, IdempotencyKey: "new-episode", Input: f.startRequest("").Input})
	if err != nil {
		t.Fatal(err)
	}
	var current bool
	if err = f.pool.QueryRow(t.Context(), `SELECT execution_deadline_at>clock_timestamp() AND execution_deadline_at<=clock_timestamp()+(SELECT preparation_timeout_ms FROM environments WHERE id=computers.environment_id)*interval '1 millisecond' FROM computers WHERE id=$1`, id).Scan(&current); err != nil || !current {
		t.Fatalf("new episode=%v %v", current, err)
	}
}

func TestExecutionAllocationPreservesEarlierPendingShapePlacement(t *testing.T) {
	f, allocator, a := freshComputerAllocationFixture(t)
	b, secondHost := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_cpu_millis=20000,max_memory_bytes=34359738368,max_resident_computers=10 WHERE id=$1;
 UPDATE worker_pools SET per_vm_cpu_millis=8000,per_vm_memory_bytes=8589934592,capacity_cpu_millis=16000,capacity_memory_bytes=17179869184,capacity_guest_ephemeral_disk_bytes=17179869184,max_vm_slots=16 WHERE worker_group_id=$2;
 INSERT INTO worker_pool_cpu_shapes(worker_pool_id,vcpu_count,cpu_config_digest) SELECT worker_pool_id,4,cpu_config_digest FROM worker_pool_cpu_shapes WHERE vcpu_count=1;
 INSERT INTO worker_pool_cpu_shapes(worker_pool_id,vcpu_count,cpu_config_digest) SELECT worker_pool_id,8,cpu_config_digest FROM worker_pool_cpu_shapes WHERE vcpu_count=1;
 UPDATE worker_hosts SET epoch_cpu_millis=10000,epoch_memory_bytes=5368709120,epoch_guest_ephemeral_disk_bytes=17179869184,per_vm_cpu_millis=8000,per_vm_memory_bytes=8589934592,max_vm_slots=16,max_vm_starts=16,disk_slots_for_real_work=16 WHERE id=$3;
 INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$4::uuid,'resource_id','second','current_service_id',$4::uuid,'epoch_cpu_millis',4000,'epoch_memory_bytes',8589934592))).* FROM worker_hosts h WHERE id=$3;
 UPDATE computers SET cpu_millis=4000,memory_bytes=4294967296,execution_deadline_at=NULL WHERE environment_id=$1 AND id=$5;
 INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest,image_spec_id,image_id,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,storage_reservation_bytes)
 SELECT environment_id,$6,initial_root_id,initial_root_digest,image_spec_id,image_id,origin_template_id,origin_deployment_id,8000,4294967296,storage_reservation_bytes FROM computers WHERE environment_id=$1 AND id=$5`, pgx.QueryExecModeSimpleProtocol, f.env, f.group, f.worker, secondHost, a, b)
	if err := admitCapacityComputer(t.Context(), f, f.env, a); err != nil {
		t.Fatal(err)
	}
	if err := admitCapacityComputer(t.Context(), f, f.env, b); err != nil {
		t.Fatal(err)
	}
	if _, err := allocator.AllocateFreshLease(t.Context(), f.env, a, 1, f.worker); err == nil {
		t.Fatal("small claim stranded larger accepted claim")
	}
	var leaked bool
	if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM computer_leases WHERE computer_id=$1)`, a).Scan(&leaked); err != nil || leaked {
		t.Fatalf("rejected placement leaked: %v %v", leaked, err)
	}
	if _, err := allocator.AllocateFreshLease(t.Context(), f.env, a, 1, secondHost); err != nil {
		t.Fatalf("compatible small placement: %v", err)
	}
	if _, err := allocator.AllocateFreshLease(t.Context(), f.env, b, 1, f.worker); err != nil {
		t.Fatalf("preserved large placement: %v", err)
	}
}

func TestExecutionHeldQueueReleasesClaimAndResumeReadmits(t *testing.T) {
	f := newAdmissionFixture(t)
	started, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), f.startRequest("held-claim"))
	if err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	if err = f.pool.QueryRow(t.Context(), `SELECT computer_id FROM sessions WHERE id=$1`, started.SessionID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	hold, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: started.SessionID, Kind: "interrupt", IdempotencyKey: "hold"})
	if err != nil {
		t.Fatal(err)
	}
	var pending bool
	if err = f.pool.QueryRow(t.Context(), `SELECT execution_deadline_at IS NOT NULL FROM computers WHERE id=$1`, id).Scan(&pending); err != nil || pending {
		t.Fatalf("held claim retained: %v %v", pending, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_cpu_millis=1000 WHERE id=$1`, f.env)
	resume := session.SessionControlRequest{EnvironmentID: f.env, SessionID: started.SessionID, Kind: "resume", HoldID: hold.HoldID, IdempotencyKey: "resume"}
	if _, err = session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), resume); !errors.Is(err, computer.ErrExecutionLimit) {
		t.Fatalf("resume without quota: %v", err)
	}
	var held bool
	if err = f.pool.QueryRow(t.Context(), `SELECT released_at IS NULL FROM session_holds WHERE id=$1`, hold.HoldID).Scan(&held); err != nil || !held {
		t.Fatalf("failed resume released hold: %v %v", held, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_cpu_millis=2000 WHERE id=$1`, f.env)
	if _, err = session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), resume); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT execution_deadline_at IS NOT NULL FROM computers WHERE id=$1`, id).Scan(&pending); err != nil || !pending {
		t.Fatalf("resume missing claim: %v %v", pending, err)
	}
}

func TestExecutionFailedBootPreservesOriginalDeadline(t *testing.T) {
	f, a, id := freshComputerAllocationFixture(t)
	var before, after time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT execution_deadline_at FROM computers WHERE id=$1`, id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocateFreshLease(t.Context(), f.env, id, 1, f.worker); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT execution_deadline_at FROM computers WHERE id=$1`, id).Scan(&after); err != nil || !before.Equal(after) {
		t.Fatalf("allocation discarded deadline: %v %v", after, err)
	}
	if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error { return computer.FenceLease(t.Context(), tx, f.env, id, 1, "test physical stop") }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocateFreshLease(t.Context(), f.env, id, 2, f.worker); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT execution_deadline_at FROM computers WHERE id=$1`, id).Scan(&after); err != nil || !before.Equal(after) {
		t.Fatalf("retry extended deadline: %v %v", after, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET execution_deadline_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, id)
	if err := admitCapacityComputer(t.Context(), f, f.env, id); !errors.Is(err, computer.ErrExecutionSettling) {
		t.Fatalf("expired acquiring lease admitted more input: %v", err)
	}
}

func TestExecutionImageBuildSharesOnlyOnePendingClaim(t *testing.T) {
	f, p, a := preparationAllocationFixture(t)
	var id uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM computers WHERE image_build_id=$1`, p.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_resident_computers=2 WHERE id=$1`, f.env)
	if err := admitCapacityComputer(t.Context(), f.fixture, f.env, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker); err != nil {
		t.Fatalf("build deadlocked on its Computer claim: %v", err)
	}
	budget, err := db.ReadExecutionBudget(t.Context(), f.pool, f.env)
	if err != nil || budget.CPU != 0 || budget.Residents != 0 {
		t.Fatalf("shared budget: %+v %v", budget, err)
	}
	second := pendingCapacityComputer(t, f.fixture, f.env)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET image_build_id=$2 WHERE id=$1`, second, p.ID)
	if err := admitCapacityComputer(t.Context(), f.fixture, f.env, second); !errors.Is(err, computer.ErrExecutionLimit) {
		t.Fatalf("second claimant borrowed same build: %v", err)
	}
}

func TestExecutionExpiredBootCannotRenewOrReportReady(t *testing.T) {
	f, a, id := freshComputerAllocationFixture(t)
	allocation, err := a.AllocateFreshLease(t.Context(), f.env, id, 1, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: id, InstanceID: allocation.InstanceID, Epoch: 1}
	if _, err = a.DeliverLease(t.Context(), *f.host(), identity); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET execution_deadline_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, id)
	if _, err = computer.RenewLease(t.Context(), f.pool, *f.host(), identity); err == nil {
		t.Fatal("expired provisioning lease renewed")
	}
	if err = dispatch.ObserveFreshLeaseReady(t.Context(), f.pool, *f.host(), identity, allocation.Shape, allocation.BaseVersion); !errors.Is(err, dispatch.ErrAllocationClosed) {
		t.Fatalf("late ready accepted: %v", err)
	}
	var charged bool
	if err = f.pool.QueryRow(t.Context(), `SELECT initialized_at IS NULL AND fenced_at IS NULL AND reserved_cpu_millis>0 FROM computer_leases WHERE computer_id=$1`, id).Scan(&charged); err != nil || !charged {
		t.Fatalf("deadline released physical custody: %v %v", charged, err)
	}
}

func TestExecutionInputWhileHeldDoesNotReserveCapacity(t *testing.T) {
	f := newAdmissionFixture(t)
	started, err := session.Start(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), f.startRequest("held-input"))
	if err != nil {
		t.Fatal(err)
	}
	hold, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: started.SessionID, Kind: "interrupt", IdempotencyKey: "hold"})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_cpu_millis=1000 WHERE id=$1`, f.env)
	if _, err = session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: started.SessionID, IdempotencyKey: "held-followup", Input: f.startRequest("").Input}); err != nil {
		t.Fatal(err)
	}
	var reserved bool
	if err = f.pool.QueryRow(t.Context(), `SELECT c.execution_deadline_at IS NOT NULL FROM computers c JOIN sessions s ON s.environment_id=c.environment_id AND s.computer_id=c.id WHERE s.id=$1`, started.SessionID).Scan(&reserved); err != nil || reserved {
		t.Fatalf("held input reserved execution: %v %v", reserved, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_cpu_millis=2000 WHERE id=$1`, f.env)
	if _, err = session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: started.SessionID, Kind: "resume", HoldID: hold.HoldID, IdempotencyKey: "resume"}); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredInitialExecutionCannotAllocateWithoutNewInput(t *testing.T) {
	f, allocator, id := freshComputerAllocationFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET execution_deadline_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, id)
	if err := dispatch.ReconcileExecutionAdmissions(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool); err != nil {
		t.Fatal(err)
	}
	if demand, err := workergroup.HasQueuedDemand(t.Context(), f.pool, f.group); err != nil || demand {
		t.Fatalf("expired request retained scale-out demand: %v %v", demand, err)
	}
	if _, _, err := allocator.Reconcile(t.Context(), dispatch.AllocationPosition{}); err != nil {
		t.Fatal(err)
	}
	if _, err := allocator.AllocateFreshLease(t.Context(), f.env, id, 1, f.worker); err == nil {
		t.Fatal("expired initial request allocated without readmission")
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_leases WHERE computer_id=$1`, id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("silent retry created %d leases: %v", count, err)
	}
	var target uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM sessions WHERE computer_id=$1`, id).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: target, IdempotencyKey: "explicit-retry", Input: f.startRequest("").Input}); err != nil {
		t.Fatal(err)
	}
	if _, err := allocator.AllocateFreshLease(t.Context(), f.env, id, 1, f.worker); err != nil {
		t.Fatalf("new input failed to readmit execution: %v", err)
	}
}

func TestStaleProviderEvidenceDoesNotBlockUnrelatedExistingCapacity(t *testing.T) {
	f, allocator, large := freshComputerAllocationFixture(t)
	small := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_pools SET per_vm_cpu_millis=4000,capacity_cpu_millis=8000,capacity_memory_bytes=8589934592,capacity_guest_ephemeral_disk_bytes=8589934592,max_vm_slots=8 WHERE worker_group_id=$1;
 INSERT INTO worker_pool_cpu_shapes(worker_pool_id,vcpu_count,cpu_config_digest) SELECT worker_pool_id,4,cpu_config_digest FROM worker_pool_cpu_shapes WHERE vcpu_count=1;
 UPDATE worker_hosts SET epoch_cpu_millis=3000,epoch_memory_bytes=3221225472,epoch_guest_ephemeral_disk_bytes=3221225472,max_vm_slots=3,max_vm_starts=3,disk_slots_for_real_work=3,per_vm_cpu_millis=4000 WHERE id=$2;
 UPDATE computers SET cpu_millis=4000,execution_deadline_at=NULL WHERE id=$3;
 INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest,image_spec_id,image_id,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,storage_reservation_bytes)
 SELECT environment_id,$4,initial_root_id,initial_root_digest,image_spec_id,image_id,origin_template_id,origin_deployment_id,1000,memory_bytes,storage_reservation_bytes FROM computers WHERE id=$3`, pgx.QueryExecModeSimpleProtocol, f.group, f.worker, large, small)
	sessiontest.ObserveProvisioning(t, sessiontest.Fixture{Pool: f.pool, Worker: f.worker, Group: f.group})
	if err := admitCapacityComputer(t.Context(), f, f.env, large); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET capacity_observed_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, f.group)
	if err := admitCapacityComputer(t.Context(), f, f.env, small); err != nil {
		t.Fatalf("unrelated current capacity rejected: %v", err)
	}
	if _, err := allocator.AllocateFreshLease(t.Context(), f.env, small, 1, f.worker); err != nil {
		t.Fatalf("unrelated allocation blocked by prior unplaced claim: %v", err)
	}
	var retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT execution_deadline_at>clock_timestamp() FROM computers WHERE id=$1`, large).Scan(&retained); err != nil || !retained {
		t.Fatalf("earlier claim lost: %v %v", retained, err)
	}
}

func TestRevokedPendingClaimDoesNotBlockOtherAdmissions(t *testing.T) {
	f, _, revoked := freshComputerAllocationFixture(t)
	secretID, version := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO secrets(id,environment_id,name,status,revoked_at) VALUES($1,$2,'revoked-planning-fixture','revoked',clock_timestamp());
 INSERT INTO secret_versions(id,secret_id,version,nonce,ciphertext) VALUES($3,$1,1,decode(repeat('00',12),'hex'),decode(repeat('00',16),'hex'));
 INSERT INTO secret_exposures(id,environment_id,image_build_id,secret_id,version_id,revocation_generation) SELECT $4,c.environment_id,i.image_build_id,$1,$3,0 FROM computers c JOIN images i ON (i.environment_id,i.id)=(c.environment_id,c.image_id) WHERE c.id=$5`, pgx.QueryExecModeSimpleProtocol, secretID, f.env, version, uuid.NewV7(), revoked)
	claims, err := db.ReadPendingExecutionClaims(t.Context(), f.pool, f.group)
	if err != nil {
		t.Fatal(err)
	}
	for _, claim := range claims {
		if claim.ComputerID == revoked {
			t.Fatal("revoked demand still reserves pending execution")
		}
	}
	other := pendingCapacityComputer(t, f, f.env)
	if err := admitCapacityComputer(t.Context(), f, f.env, other); err != nil {
		t.Fatalf("revoked claim blocked unrelated admission: %v", err)
	}
}

func TestSessionCloseDoesNotWaitForRegionalPlacement(t *testing.T) {
	f := newFixture(t)
	gate, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(t.Context())
	if err = workergroup.LockExecutionSupply(t.Context(), gate, f.env); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err = session.ControlSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, f.caller(), session.SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "close", IdempotencyKey: "independent-close"}); err != nil {
		t.Fatal(err)
	}
}
