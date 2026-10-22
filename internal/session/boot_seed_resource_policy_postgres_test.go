package session_test

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

func TestBootSeedWorkerReclaimRequiresFreshPressureAndRetainsCustody(t *testing.T) {
	p, a, id := bootSeedFixture(t)
	f := p.f
	r, err := a.AllocateBootSeed(t.Context(), f.env, id, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := a.DeliverBootSeed(t.Context(), *f.host(), bootSeedReceipt(r))
	if err != nil {
		t.Fatal(err)
	}
	reclaim := func() error { return computer.ReclaimBootSeedBuild(t.Context(), f.pool, *f.host(), delivery.Executor) }
	if err := reclaim(); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("unobserved pressure authorized reclaim: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts h SET
 resource_observation_sequence=1,resource_policy_revision=p.resource_policy_revision,resource_observed_at=clock_timestamp()-interval '1 minute',
 local_optional_cpu_millis=0,local_optional_memory_bytes=0,optional_disk_bytes=0,local_optional_vm_slots=0,local_optional_tasks=0,
 optional_reclaim_pending=true,disk_slots_for_real_work=0,ready_leaf_keys='[]'::jsonb
 FROM worker_pools p WHERE h.id=$1 AND p.id=h.worker_pool_id`, f.worker)
	if err := reclaim(); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("stale pressure authorized reclaim: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET resource_observed_at=clock_timestamp() WHERE id=$1`, f.worker)
	if err := reclaim(); err != nil {
		t.Fatal(err)
	}
	if err := reclaim(); err != nil {
		t.Fatalf("lost-response replay: %v", err)
	}
	var retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='failed' AND error_code='resource_reclaimed' AND fenced_at IS NULL AND allocation_id=$3 FROM boot_seed_builds WHERE environment_id=$1 AND id=$2`, f.env, id, r.InstanceID).Scan(&retained); err != nil || !retained {
		t.Fatalf("reclaim prematurely released custody: %v %v", retained, err)
	}
	if _, err := computer.RenewBootSeedBuild(t.Context(), f.pool, *f.host(), delivery.Executor); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("reclaimed attempt renewed: %v", err)
	}
	if err := dispatch.ObserveBootSeedStopped(t.Context(), f.pool, *f.host(), bootSeedReceipt(r)); err != nil {
		t.Fatal(err)
	}
	if err := reclaim(); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("old reclaim reached reset attempt: %v", err)
	}
}

func TestBootSeedPolicyChangeStopsAuthorityAndFencesRetry(t *testing.T) {
	p, a, id := bootSeedFixture(t)
	f := p.f
	r, err := a.AllocateBootSeed(t.Context(), f.env, id, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := a.DeliverBootSeed(t.Context(), *f.host(), bootSeedReceipt(r))
	if err != nil {
		t.Fatal(err)
	}
	if err := computer.FailBootSeedBuild(t.Context(), f.pool, *f.host(), delivery.Executor, "resource_reclaimed"); !errors.Is(err, computer.ErrInvalidInput) {
		t.Fatalf("Worker synthesized retry: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_pools SET resource_policy_revision=resource_policy_revision+1,optional_cpu_millis=0 WHERE id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1)`, f.worker)
	if _, err := a.DeliverBootSeed(t.Context(), *f.host(), bootSeedReceipt(r)); !errors.Is(err, dispatch.ErrAllocationClosed) {
		t.Fatalf("policy allowed delivery: %v", err)
	}
	if _, err := computer.RenewBootSeedBuild(t.Context(), f.pool, *f.host(), delivery.Executor); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("policy allowed renewal: %v", err)
	}
	// Worker failure arrives before the asynchronous lifecycle reconciler.
	if err := computer.FailBootSeedBuild(t.Context(), f.pool, *f.host(), delivery.Executor, "boot_seed_build_failed"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dispatch.ReconcileImageBuildLifecycle(t.Context(), f.pool, dispatch.ImageBuildLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	var retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='failed' AND error_code='resource_reclaimed' AND fenced_at IS NULL AND allocation_id=$3 FROM boot_seed_builds WHERE environment_id=$1 AND id=$2`, f.env, id, r.InstanceID).Scan(&retained); err != nil || !retained {
		t.Fatalf("logical termination released custody: %v %v", retained, err)
	}
	if err := dispatch.ObserveBootSeedStopped(t.Context(), f.pool, *f.host(), bootSeedReceipt(r)); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT status='queued' AND worker_host_id IS NULL AND executor_epoch=$3 AND retry_after>clock_timestamp() FROM boot_seed_builds WHERE environment_id=$1 AND id=$2`, f.env, id, r.Epoch).Scan(&retained); err != nil || !retained {
		t.Fatalf("retry state: %v %v", retained, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE boot_seed_builds SET retry_after=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.env, id)
	if _, err := a.AllocateBootSeed(t.Context(), f.env, id, f.worker); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("zero policy allocated: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_pools SET optional_cpu_millis=capacity_cpu_millis WHERE id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1)`, f.worker)
	retry, err := a.AllocateBootSeed(t.Context(), f.env, id, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	// Delayed startup recovery holds the identity discovered before the reset.
	if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		return computer.RecordBootSeedStopped(t.Context(), tx, r, "delayed old-host recovery evidence")
	}); err != nil {
		t.Fatal(err)
	}
	var intact bool
	if err := f.pool.QueryRow(t.Context(), `SELECT allocation_id=$3 AND fenced_at IS NULL AND status='running' FROM boot_seed_builds WHERE environment_id=$1 AND id=$2`, f.env, id, retry.InstanceID).Scan(&intact); err != nil || !intact {
		t.Fatalf("old recovery fenced new attempt: %v %v", intact, err)
	}
	if retry.Epoch != r.Epoch+1 || retry.InstanceID == r.InstanceID {
		t.Fatal("retry reused executor identity")
	}
	if err := dispatch.ObserveBootSeedStopped(t.Context(), f.pool, *f.host(), bootSeedReceipt(r)); !errors.Is(err, dispatch.ErrAllocationClosed) {
		t.Fatalf("old stop reached new attempt: %v", err)
	}
	if _, err := computer.RenewBootSeedBuild(t.Context(), f.pool, *f.host(), delivery.Executor); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("old credential renewed: %v", err)
	}
}

func TestBootSeedRealGrantReclaimsWithoutConsumingEnvironmentQuota(t *testing.T) {
	p, a, id := bootSeedFixture(t)
	f := p.f
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET epoch_cpu_millis=2000,max_vm_slots=2 WHERE id=$1`, f.worker)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_resident_computers=2 WHERE id=$1`, f.env)
	r, err := a.AllocateBootSeed(t.Context(), f.env, id, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := session.Start(t.Context(), f.pool, nil, f.caller(), f.startRequest("real-work"))
	if err != nil {
		t.Fatal(err)
	}
	var real uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT computer_id FROM sessions WHERE environment_id=$1 AND id=$2`, f.env, admitted.SessionID).Scan(&real); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocateFreshLease(t.Context(), f.env, real, 1, f.worker); err != nil {
		t.Fatalf("optional work blocked real grant: %v", err)
	}
	var retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='failed' AND error_code='resource_reclaimed' AND fenced_at IS NULL AND allocation_id=$3 FROM boot_seed_builds WHERE environment_id=$1 AND id=$2`, f.env, id, r.InstanceID).Scan(&retained); err != nil || !retained {
		t.Fatalf("real grant did not retain seed custody: %v %v", retained, err)
	}
}

func TestBootSeedReclaimedObsoleteGenerationDoesNotRetry(t *testing.T) {
	p, a, id := bootSeedFixture(t)
	f := p.f
	r, err := a.AllocateBootSeed(t.Context(), f.env, id, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	next := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO deployments(environment_id,id,bundle_digest,created_at) VALUES($1,$2,$3,clock_timestamp()+interval '1 second')`, f.env, next, dbtest.Digest("new-deployment"))
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.env, next)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_pools SET resource_policy_revision=resource_policy_revision+1 WHERE id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1)`, f.worker)
	// Stop can arrive without a preceding failure report or lifecycle pass.
	if err := dispatch.ObserveBootSeedStopped(t.Context(), f.pool, *f.host(), bootSeedReceipt(r)); err != nil {
		t.Fatal(err)
	}
	var terminal bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='failed' AND fenced_at IS NOT NULL AND executor_epoch=$3 AND retry_after IS NULL FROM boot_seed_builds WHERE environment_id=$1 AND id=$2`, f.env, id, r.Epoch).Scan(&terminal); err != nil || !terminal {
		t.Fatalf("obsolete generation retried: %v %v", terminal, err)
	}
}

func TestBootSeedOrdinaryRecoveryDoesNotDependOnExpiryOrder(t *testing.T) {
	for _, expireFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovery_first", true: "expiry_first"}[expireFirst], func(t *testing.T) {
			p, a, id := bootSeedFixture(t)
			f := p.f
			if _, err := a.AllocateBootSeed(t.Context(), f.env, id, f.worker); err != nil {
				t.Fatal(err)
			}
			host := recoveringComputerHost(t, f.fixture)
			expire := func() {
				if _, _, err := dispatch.ReconcileImageBuildLifecycle(t.Context(), f.pool, dispatch.ImageBuildLifecyclePosition{}); err != nil {
					t.Fatal(err)
				}
			}
			if expireFirst {
				expire()
			}
			if err := dispatch.ObserveRecoveredHost(t.Context(), f.pool, host, []uuid.UUID{}); err != nil {
				t.Fatal(err)
			}
			if !expireFirst {
				expire()
			}
			var terminal bool
			if err := f.pool.QueryRow(t.Context(), `SELECT status='failed' AND fenced_at IS NOT NULL AND error_code<>'resource_reclaimed' AND retry_after IS NULL FROM boot_seed_builds WHERE environment_id=$1 AND id=$2`, f.env, id).Scan(&terminal); err != nil || !terminal {
				t.Fatalf("ordinary recovery became retryable: %v %v", terminal, err)
			}
		})
	}
}

func TestBootSeedAdmissionWaitsForWorkerLocalTaskOccupancy(t *testing.T) {
	p, a, id := bootSeedFixture(t)
	f := p.f
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts h SET
 resource_observation_sequence=1,resource_policy_revision=p.resource_policy_revision,resource_observed_at=clock_timestamp(),
 local_optional_cpu_millis=0,local_optional_memory_bytes=0,optional_disk_bytes=0,local_optional_vm_slots=0,local_optional_tasks=p.optional_max_tasks,
 optional_reclaim_pending=false,disk_slots_for_real_work=h.max_vm_slots,ready_leaf_keys='[]'::jsonb
 FROM worker_pools p WHERE h.id=$1 AND p.id=h.worker_pool_id`, f.worker)
	if _, err := a.AllocateBootSeed(t.Context(), f.env, id, f.worker); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("occupied local tasks admitted seed: %v", err)
	}
	var epoch int64
	if err := f.pool.QueryRow(t.Context(), `SELECT executor_epoch FROM boot_seed_builds WHERE environment_id=$1 AND id=$2`, f.env, id).Scan(&epoch); err != nil || epoch != 0 {
		t.Fatalf("rejected admission consumed attempt: epoch=%d err=%v", epoch, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET local_optional_tasks=0,resource_observed_at=clock_timestamp() WHERE id=$1`, f.worker)
	if _, err := a.AllocateBootSeed(t.Context(), f.env, id, f.worker); err != nil {
		t.Fatal(err)
	}
}
