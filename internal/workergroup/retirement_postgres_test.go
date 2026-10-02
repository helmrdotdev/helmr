package workergroup_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func cloneRestorePool(t *testing.T, f runtest.Fixture) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_pools
 (id,worker_group_id,name,status,vm_platform_id,capacity_cpu_millis,capacity_memory_bytes,capacity_guest_ephemeral_disk_bytes,per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,max_vm_slots,sealed_at)
 SELECT $1,worker_group_id,$2,'active',vm_platform_id,capacity_cpu_millis,capacity_memory_bytes,capacity_guest_ephemeral_disk_bytes,per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,max_vm_slots,sealed_at
 FROM worker_pools WHERE id=$3`, id, id.String(), f.WorkerPoolID)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_pool_cpu_shapes(worker_pool_id,vcpu_count,cpu_config_digest) SELECT $1,vcpu_count,cpu_config_digest FROM worker_pool_cpu_shapes WHERE worker_pool_id=$2`, id, f.WorkerPoolID)
	return id
}

func retireFixtureGroup(t *testing.T, f runtest.Fixture) uuid.UUID {
	t.Helper()
	var group uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `UPDATE worker_groups SET primary_pool_id=NULL WHERE id=(SELECT worker_group_id FROM worker_pools WHERE id=$1) RETURNING id`, f.WorkerPoolID).Scan(&group); err != nil {
		t.Fatal(err)
	}
	return group
}

func TestPoolRetirementPostgresConcurrentLastRestorePath(t *testing.T) {
	f, _, _, _ := computertest.Capture(t)
	group := retireFixtureGroup(t, f)
	alternative := cloneRestorePool(t, f)
	start := make(chan struct{})
	results := make([]error, 2)
	var wait sync.WaitGroup
	for index, id := range []uuid.UUID{f.WorkerPoolID, alternative} {
		wait.Go(func() { <-start; _, _, results[index] = workergroup.DrainPool(t.Context(), f.Pool, group, id, 1) })
	}
	close(start)
	wait.Wait()
	successes := 0
	for _, err := range results {
		if err == nil {
			successes++
			continue
		}
		var conflict workergroup.ConflictError
		if !errors.As(err, &conflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("retirements=%v, want exactly one", results)
	}
	var active int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM worker_pools WHERE worker_group_id=$1 AND status='active'`, group).Scan(&active); err != nil || active != 1 {
		t.Fatalf("remaining pools=%d: %v", active, err)
	}
}

func TestPoolRetirementPostgresWaitsForCaptureFence(t *testing.T) {
	f, _, _, capture := computertest.Capture(t)
	group := retireFixtureGroup(t, f)
	cloneRestorePool(t, f)
	hold, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(t.Context())
	if _, err = computer.BeginCapture(t.Context(), hold, capture); err != nil {
		t.Fatal(err)
	}
	var blocker int32
	if err = hold.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := workergroup.DrainPool(ctx, f.Pool, group, f.WorkerPoolID, 1); done <- err }()
	for {
		var blocked bool
		if err = f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("retirement did not wait: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err = hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	var name string
	if err = f.Pool.QueryRow(ctx, `SELECT name FROM worker_pools WHERE id=$1`, f.WorkerPoolID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	profiles, err := workergroup.ResolvePool(ctx, db.New(f.Pool), group, name)
	if err != nil || !profiles.RetainedProfiles.Complete || len(profiles.RetainedProfiles.Profiles) != 1 {
		t.Fatalf("profiles=%+v: %v", profiles, err)
	}
	profile := profiles.RetainedProfiles.Profiles[0]
	if profile.CapturingCheckpoints != 1 || profile.EligiblePools != 1 {
		t.Fatalf("capture dependency=%+v", profile)
	}
}

func TestPoolRetirementPostgresParkedQueuedWorkRetainsProfile(t *testing.T) {
	f, ref, manifest, objects := computertest.ReadyCapture(t, false)
	cp := computertest.Complete(t, f, ref, manifest, objects)
	group := retireFixtureGroup(t, f)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='closed',observed_desired_version=desired_version,mount_state='unmounted',unmounted_at=now(),terminal_at=now(),reclaimed_at=now(),reclaim_evidence='{"method":"machine_closed"}',terminal_reason_code='checkpointed' WHERE id=$1`, ref.InstanceID)
	// Resolved waits remain parked until resume commits; they cannot release the profile.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET condition_status='completed',condition_terminal_at=now() WHERE suspend_checkpoint_id=$1`, cp.ID)
	claimID, commandID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at,receipt_expires_at) VALUES($1,$2,'computer.command.create',$3,$4,now(),now()+interval '30 days')`, claimID, f.EnvironmentID, dbtest.Hash(commandID.String()), dbtest.Hash("retained-command"))
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,argv,cwd,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id) VALUES($1,$2,$3,$4,ARRAY['true'],'/workspace','{}','',300000,'api_key','fixture')`, commandID, f.EnvironmentID, cp.ComputerID, claimID)
	var conflict workergroup.ConflictError
	if _, _, err := workergroup.DrainPool(t.Context(), f.Pool, group, f.WorkerPoolID, 1); !errors.As(err, &conflict) {
		t.Fatalf("last parked profile retired: %v", err)
	}
	alternative := cloneRestorePool(t, f)
	if _, _, err := workergroup.DrainPool(t.Context(), f.Pool, group, f.WorkerPoolID, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workergroup.DrainPool(t.Context(), f.Pool, group, alternative, 1); !errors.As(err, &conflict) {
		t.Fatalf("last alternative retired: %v", err)
	}
}

func TestPoolRetirementPostgresCaptureRequiresRemainingSupplier(t *testing.T) {
	f, _, _, capture := computertest.Capture(t)
	// Model a withdrawn source whose supplier has become unavailable. Capture
	// must leave the live source unchanged instead of parking without a path.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_pools SET status='draining',claim_version=claim_version+1 WHERE id=$1`, f.WorkerPoolID)
	err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error { _, err := computer.BeginCapture(t.Context(), tx, capture); return err })
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("capture without supplier=%v", err)
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoints WHERE source_computer_instance_id=$1`, pgvalue.UUID(capture.InstanceID)).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected capture committed %d rows: %v", count, err)
	}
}

func TestPoolRetirementPostgresDisableWaitsForTerminalProcesses(t *testing.T) {
	f, _, _, capture := computertest.Capture(t)
	group := retireFixtureGroup(t, f)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1,observed_state='closed',observed_desired_version=desired_version+1,admission_state='closed',mount_state='unmounted',unmounted_at=now(),terminal_at=now(),reclaimed_at=now(),reclaim_evidence='{"method":"machine_closed"}',terminal_reason_code='cancelled' WHERE id=$1`, capture.InstanceID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled' WHERE computer_instance_id=$1`, capture.InstanceID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET status='lost',lost_at=now() WHERE id=$1`, f.WorkerID)
	_, drained, err := workergroup.DrainPool(t.Context(), f.Pool, group, f.WorkerPoolID, 1)
	if err != nil {
		t.Fatal(err)
	}
	var conflict workergroup.ConflictError
	if _, _, err = workergroup.DisablePool(t.Context(), f.Pool, group, f.WorkerPoolID, drained.ClaimVersion); !errors.As(err, &conflict) {
		t.Fatalf("disabled with unreconciled terminal process: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET process_reconciled_at=now() WHERE computer_instance_id=$1`, capture.InstanceID)
	_, receipt, err := workergroup.DisablePool(t.Context(), f.Pool, group, f.WorkerPoolID, drained.ClaimVersion)
	if err != nil || receipt.Status != "disabled" {
		t.Fatalf("disable receipt=%+v: %v", receipt, err)
	}
	_, replay, err := workergroup.DisablePool(t.Context(), f.Pool, group, f.WorkerPoolID, drained.ClaimVersion)
	if err != nil || replay.ClaimVersion != receipt.ClaimVersion {
		t.Fatalf("disable replay=%+v: %v", replay, err)
	}
	if _, _, err = workergroup.DrainPool(t.Context(), f.Pool, group, f.WorkerPoolID, receipt.ClaimVersion); !errors.As(err, &conflict) {
		t.Fatalf("disabled pool revived: %v", err)
	}
}
