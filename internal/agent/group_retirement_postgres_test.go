package agent

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func assertGroupDrainBlocked(t *testing.T, f fixture) {
	t.Helper()
	var conflict workergroup.ConflictError
	if _, err := workergroup.BeginGroupDrain(t.Context(), f.pool, f.group, 1); !errors.As(err, &conflict) {
		t.Fatalf("drain accepted retained state: %v", err)
	}
	var unchanged bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='active' AND claim_version=1 AND primary_pool_id IS NOT NULL FROM worker_groups WHERE id=$1`, f.group).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("rejected drain changed group: %v %v", unchanged, err)
	}
}

func setGroupPrimary(t *testing.T, f fixture) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET primary_pool_id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1) WHERE id=$2`, f.worker, f.group)
}

func TestGroupDrainRetainsPhysicalCustodyAcrossEpochs(t *testing.T) {
	f, p, a := preparationAllocationFixture(t)
	stopGroupFixtureComputer(t, f.fixture)
	if _, err := a.AllocatePreparation(t.Context(), f.env, p.ID, f.worker); err != nil {
		t.Fatal(err)
	}
	// An expired allocation owned by an earlier service epoch remains physical custody.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET current_epoch=2 WHERE id=$1;
 UPDATE computer_preparations SET status='failed',error_code='executor_lost' WHERE environment_id=$2 AND id=$3`, pgx.QueryExecModeSimpleProtocol, f.worker, f.env, p.ID)
	assertGroupDrainBlocked(t, f.fixture)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_preparations SET fenced_at=clock_timestamp(),fence_evidence='fixture physical closure' WHERE environment_id=$1`, f.env)
	result, err := workergroup.BeginGroupDrain(t.Context(), f.pool, f.group, 1)
	if err != nil || result.Status != "draining" {
		t.Fatalf("resolved preparation: %+v %v", result, err)
	}
	replay, err := workergroup.BeginGroupDrain(t.Context(), f.pool, f.group, 1)
	if err != nil || replay.TransitionApplied || replay.ClaimVersion != result.ClaimVersion {
		t.Fatalf("drain replay: %+v %v", replay, err)
	}
}

func TestGroupDrainRetainsEveryPendingCheckpoint(t *testing.T) {
	for _, state := range []string{"capturing", "sealed", "ready", "restoring", "aborting"} {
		t.Run(state, func(t *testing.T) {
			cp := readyCheckpointFixture(t)
			f := cp.f
			setGroupPrimary(t, f)
			source := ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, Epoch: 1, InstanceID: cp.manifest.InstanceID}
			if err := ObserveComputerStopped(t.Context(), f.pool, *f.host(), source, uuid.Nil()); err != nil {
				t.Fatal(err)
			}
			if state == "restoring" {
				// Target allocation is fenced too: the source-owned checkpoint must protect the Group on its own.
				dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,delivered_at,status,computer_instance_id,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,fenced_at,fence_evidence)
 SELECT environment_id,computer_id,2,worker_host_id,worker_epoch,expires_at,delivered_at,status,$3,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,fenced_at,fence_evidence FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1;
 UPDATE computer_checkpoints SET target_lease_epoch=2,restore_control_version=2 WHERE environment_id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, f.computer, uuid.NewV7())
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_checkpoints SET status=$2 WHERE environment_id=$1`, f.env, state)
			assertGroupDrainBlocked(t, f)
			// Terminal disposition is fixture setup here; this test does not prove its operation.
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_checkpoints SET status='lost',capture_request=NULL,terminal_evidence='fixture terminal disposition' WHERE environment_id=$1`, f.env)
			if _, err := workergroup.BeginGroupDrain(t.Context(), f.pool, f.group, 1); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGroupDrainRechecksDependenciesAfterAllocationLock(t *testing.T) {
	f, p, a := preparationAllocationFixture(t)
	stopGroupFixtureComputer(t, f.fixture)
	hold, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(t.Context())
	if _, err = hold.Exec(t.Context(), `SELECT id FROM worker_groups WHERE id=$1 FOR SHARE`, f.group); err != nil {
		t.Fatal(err)
	}
	var blocker int32
	if err = hold.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := workergroup.BeginGroupDrain(ctx, f.pool, f.group, 1); done <- err }()
	for {
		var blocked bool
		if err = f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("drain did not wait: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	// Use the allocator inside the transaction holding the admission lock.
	owned := *a
	owned.database = hold
	if _, err = owned.AllocatePreparation(ctx, f.env, p.ID, f.worker); err != nil {
		t.Fatal(err)
	}
	if err = hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var conflict workergroup.ConflictError
	if err = <-done; !errors.As(err, &conflict) {
		t.Fatalf("drain missed committed allocation: %v", err)
	}
	assertGroupDrainBlocked(t, f.fixture)
}

func TestAllocationRestoreRejectsSuccessorGroup(t *testing.T) {
	cp := readyCheckpointFixture(t)
	f := cp.f
	a, err := NewAllocator(f.pool, bytes.Repeat([]byte{19}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	source := ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, Epoch: 1, InstanceID: cp.manifest.InstanceID}
	if err = ObserveComputerStopped(t.Context(), f.pool, *f.host(), source, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	group, pool, host, token := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	// Model historical inconsistent retirement without asking the protected API to discard it.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET status='draining',primary_pool_id=NULL WHERE id=$1;
 INSERT INTO worker_group_tokens(id,token_hash) VALUES($5,decode(repeat('22',32),'hex'));
 INSERT INTO worker_groups(id,token_id,region_id,name) SELECT $2,$5,region_id,'successor' FROM worker_groups WHERE id=$1;
 INSERT INTO worker_pools SELECT (jsonb_populate_record(NULL::worker_pools,to_jsonb(p)||jsonb_build_object('id',$3::text,'worker_group_id',$2::text))).* FROM worker_pools p WHERE p.id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$6);
 INSERT INTO worker_pool_cpu_shapes SELECT $3,vcpu_count,cpu_config_digest FROM worker_pool_cpu_shapes WHERE worker_pool_id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$6);
 INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$4::text,'worker_group_id',$2::text,'worker_pool_id',$3::text,'resource_id','successor','observed_at',clock_timestamp(),'epoch_cpu_millis',8000,'epoch_memory_bytes',8589934592,'epoch_guest_ephemeral_disk_bytes',8589934592,'max_vm_slots',8,'max_vm_starts',8))).* FROM worker_hosts h WHERE id=$6`, pgx.QueryExecModeSimpleProtocol, f.group, group, pool, host, token, f.worker)
	if _, err = a.AllocateRestoredComputer(t.Context(), f.env, cp.manifest.CheckpointID, 2, host); !errors.Is(err, ErrNotReady) {
		t.Fatalf("cross-group restore: %v", err)
	}
	if _, err = Enqueue(t.Context(), f.pool, f.caller(), EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, RetryKey: "restore", Input: []byte(`[{"type":"text","text":"{}"}]`)}); err != nil {
		t.Fatal(err)
	}
	candidates, err := a.discoverAllocations(t.Context(), allocationPosition{})
	if err != nil || len(candidates) != 0 {
		t.Fatalf("discovered foreign Group restore: %+v %v", candidates, err)
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_leases WHERE environment_id=$1 AND epoch>1`, f.env).Scan(&count); err != nil || count != 0 {
		t.Fatalf("foreign target allocated: %d %v", count, err)
	}
	// A positive fixture control changes only source ownership. The same target
	// must now be discoverable and allocatable; other eligibility cannot hide a
	// missing Group predicate. Production never rewrites this historical owner.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET worker_host_id=$3 WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`, f.env, f.computer, host)
	candidates, err = a.discoverAllocations(t.Context(), allocationPosition{})
	if err != nil || len(candidates) != 1 || candidates[0].host != host || candidates[0].kind != "restore" {
		t.Fatalf("same Group target not discovered: %+v %v", candidates, err)
	}
	allocation, err := a.AllocateRestoredComputer(t.Context(), f.env, cp.manifest.CheckpointID, 2, host)
	if err != nil || allocation.HostID != host {
		t.Fatalf("same Group target not allocated: %+v %v", allocation, err)
	}
}

func TestGroupDrainWaitsForComputerPhysicalStop(t *testing.T) {
	f := newFixture(t)
	setGroupPrimary(t, f)
	// Closing logical work is not evidence that its Computer has stopped.
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: f.session, Kind: "close", RetryKey: "close"}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 hour' WHERE environment_id=$1`, f.env)
	assertGroupDrainBlocked(t, f)
	var instance uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM computer_leases WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer).Scan(&instance); err != nil {
		t.Fatal(err)
	}
	if err := ObserveComputerStopped(t.Context(), f.pool, *f.host(), ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, Epoch: 1, InstanceID: instance}, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	if _, err := workergroup.BeginGroupDrain(t.Context(), f.pool, f.group, 1); err != nil {
		t.Fatal(err)
	}
}

func stopGroupFixtureComputer(t *testing.T, f fixture) {
	t.Helper()
	var instance uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`, f.env, f.computer).Scan(&instance); err != nil {
		t.Fatal(err)
	}
	if err := ObserveComputerStopped(t.Context(), f.pool, *f.host(), ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, Epoch: 1, InstanceID: instance}, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
}
