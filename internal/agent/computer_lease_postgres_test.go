package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func leaseIdentity(f fixture) ComputerLeaseIdentity {
	return ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, InstanceID: f.computer, Epoch: 1}
}

func TestComputerLeaseExpiryKeepsUnknownWriterAndPreparedResult(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	active := f.enqueue(t, "running")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	queued := f.enqueue(t, "queued")
	result, save := peer.finalize(t, "prepared")
	storage := newSaveStorageFixture(t, peer)
	cut, root := storage.cut(t, 2)
	peer.capture(t, save, root)
	if err := storage.publish(t, save.ID, cut); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`)
	for range 2 {
		if err := expireComputerLease(t.Context(), f.pool, f.env, f.computer, 1); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[uuid.UUID]string{active.TurnID: "interrupted", queued.TurnID: "queued", result.TurnID: "finalizing"} {
		var state string
		if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, id).Scan(&state); err != nil || state != want {
			t.Fatalf("turn %s: %s %v", id, state, err)
		}
	}
	var blocked bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='lost' AND fenced_at IS NULL AND fence_evidence IS NULL FROM computer_leases`).Scan(&blocked); err != nil || !blocked {
		t.Fatalf("expiry claimed physical absence %v %v", blocked, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT bool_and(status='lost' AND fenced_at IS NULL AND failure_recorded_at IS NOT NULL) FROM session_processes`).Scan(&blocked); err != nil || !blocked {
		t.Fatalf("process loss %v %v", blocked, err)
	}
	// The actual uniqueness constraint still excludes a second physical writer.
	_, err := f.pool.Exec(t.Context(), `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,computer_instance_id,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at)
 SELECT environment_id,computer_id,2,worker_host_id,worker_epoch,clock_timestamp()+interval '1 hour',$1,channel_credential_digest,'acquiring',reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,NULL FROM computer_leases`, uuid.NewV7())
	var unique *pgconn.PgError
	if !errors.As(err, &unique) || unique.Code != "23505" || unique.ConstraintName != "computer_leases_one_writer" {
		t.Fatalf("expected exact live-writer exclusion, got %v", err)
	}
	if err := Complete(t.Context(), f.pool, f.env, peer.session, result.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, s := range []fixture{f, peer} {
		var hold uuid.UUID
		if err := f.pool.QueryRow(t.Context(), `SELECT id FROM session_holds WHERE session_id=$1 AND scope='local' AND issuer_kind='system'`, s.session).Scan(&hold); err != nil {
			t.Fatal(err)
		}
		req := controlRequest(s, "resume", "release-loss")
		req.HoldID = hold
		if _, err := ControlSession(t.Context(), f.pool, s.caller(), req); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := ObserveComputerStopped(t.Context(), f.pool, *f.host(), leaseIdentity(f), uuid.Nil()); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT bool_and(status='lost' AND fenced_at IS NOT NULL) FROM session_processes`).Scan(&blocked); err != nil || !blocked {
		t.Fatalf("physical stop did not fence processes %v %v", blocked, err)
	}
	var holds int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE released_at IS NULL`).Scan(&holds); err != nil || holds != 0 {
		t.Fatalf("stop recreated hold %d %v", holds, err)
	}
}

func TestComputerLeaseRenewalRequiresLiveExactOwner(t *testing.T) {
	f := newFixture(t)
	identity := leaseIdentity(f)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()+interval '10 seconds'`)
	first, err := RenewComputerLease(t.Context(), f.pool, *f.host(), identity)
	if err != nil || time.Until(first) < 50*time.Second {
		t.Fatalf("renewal %v %v", first, err)
	}
	wrong := identity
	wrong.InstanceID = uuid.NewV7()
	if _, err = RenewComputerLease(t.Context(), f.pool, *f.host(), wrong); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong instance renewed: %v", err)
	}
	if err = ObserveComputerStopped(t.Context(), f.pool, *f.host(), wrong, uuid.Nil()); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong instance fenced: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`)
	if _, err = RenewComputerLease(t.Context(), f.pool, *f.host(), identity); !errors.Is(err, ErrDenied) {
		t.Fatalf("expired lease resurrected: %v", err)
	}
	if err = ObserveComputerStopped(t.Context(), f.pool, *f.host(), identity, uuid.Nil()); err != nil {
		t.Fatalf("expired owner could not record physical closure: %v", err)
	}
}

func TestComputerLeaseRenewalRechecksExpiryAfterLock(t *testing.T) {
	f := newFixture(t)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `SELECT id FROM computers WHERE id=$1 FOR NO KEY UPDATE`, f.computer); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := RenewComputerLease(t.Context(), f.pool, *f.host(), leaseIdentity(f)); done <- err }()
	waitSessionLifecycleLock(t, f)
	if _, err = tx.Exec(t.Context(), `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrDenied) {
		t.Fatalf("expired while waiting: %v", err)
	}
}

func TestComputerLeaseLossPreservesEligibleContinuation(t *testing.T) {
	for _, target := range []bool{false, true} {
		name := "source"
		if target {
			name = "target"
		}
		t.Run(name, func(t *testing.T) {
			r := newComputerRestoreFixture(t)
			identity := leaseIdentity(r.f)
			host := *r.f.host()
			if target {
				p := r.prepare(t)
				identity = ComputerLeaseIdentity{EnvironmentID: r.f.env, ComputerID: r.f.computer, InstanceID: uuid.MustParse(p.Envelope.ComputerInstanceId), Epoch: r.epoch}
				host = r.host
			} else {
				// This fixture had fenced the source to admit its target. Restore a
				// source-only ready-checkpoint state before exercising real expiry/closure.
				dbtest.MustExec(t, t.Context(), r.f.pool, `DELETE FROM computer_leases WHERE epoch=2;
 UPDATE computer_leases SET status='active',fenced_at=NULL,fence_evidence=NULL WHERE epoch=1`, pgx.QueryExecModeSimpleProtocol)
			}
			dbtest.MustExec(t, t.Context(), r.f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE epoch=$1`, identity.Epoch)
			if err := expireComputerLease(t.Context(), r.f.pool, r.f.env, r.f.computer, identity.Epoch); err != nil {
				t.Fatal(err)
			}
			if err := ObserveComputerStopped(t.Context(), r.f.pool, host, identity, uuid.Nil()); err != nil {
				t.Fatal(err)
			}
			var preserved bool
			if err := r.f.pool.QueryRow(t.Context(), `SELECT p.status='ready' AND p.fenced_at IS NULL AND p.failure_recorded_at IS NULL AND NOT EXISTS(SELECT 1 FROM session_holds) FROM session_processes p WHERE p.session_id=$1`, r.f.session).Scan(&preserved); err != nil || !preserved {
				t.Fatalf("healthy logical process lost %v %v", preserved, err)
			}
			// An eligible checkpoint can adopt the same logical process on a fresh lease.
			r.newTarget(t)
			p := r.prepare(t)
			if len(p.StoppedSessions) != 0 || p.Grants[0].Identity.ProcessEpoch != 1 {
				t.Fatal("valid continuation forced reconstruction")
			}
		})
	}
}

func TestComputerLeaseLossAfterConsumptionDoesNotReplayCheckpoint(t *testing.T) {
	r := newComputerRestoreFixture(t)
	p := r.prepare(t)
	if err := ValidateComputerRestore(t.Context(), r.f.pool, r.host, r.f.env, p, restoreReceipt(p, false, false)); err != nil {
		t.Fatal(err)
	}
	if err := CommitComputerRestore(t.Context(), r.f.pool, r.host, r.f.env, p, restoreReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), r.f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE epoch=$1`, r.epoch)
	if err := expireComputerLease(t.Context(), r.f.pool, r.f.env, r.f.computer, r.epoch); err != nil {
		t.Fatal(err)
	}
	var lost bool
	if err := r.f.pool.QueryRow(t.Context(), `SELECT status='lost' AND fenced_at IS NULL AND failure_recorded_at IS NOT NULL FROM session_processes WHERE session_id=$1`, r.f.session).Scan(&lost); err != nil || !lost {
		t.Fatalf("consumed continuation survived unknown execution %v %v", lost, err)
	}
	// Epoch replacement allows loss reconciliation, but is not physical fencing.
	dbtest.MustExec(t, t.Context(), r.f.pool, `UPDATE worker_hosts SET current_epoch=current_epoch+1 WHERE id=$1`, r.host.HostID)
	if err := LoseUnreadyComputerCapture(t.Context(), r.f.pool, r.f.env, r.manifest.CheckpointID); err != nil {
		t.Fatal(err)
	}
	var holds int
	if err := r.f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE session_id=$1`, r.f.session).Scan(&holds); err != nil || holds != 1 {
		t.Fatalf("checkpoint loss duplicated hold %d %v", holds, err)
	}
	if err := r.f.pool.QueryRow(t.Context(), `SELECT fenced_at IS NULL FROM session_processes WHERE session_id=$1`, r.f.session).Scan(&lost); err != nil || !lost {
		t.Fatalf("worker epoch treated as physical fence %v %v", lost, err)
	}
}

func TestComputerLifecycleWorkerExpiresWithoutWorkerConnection(t *testing.T) {
	f := newFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunComputerLifecycle(ctx, f.pool, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	until := time.Now().Add(5 * time.Second)
	for {
		var lost bool
		if err := f.pool.QueryRow(t.Context(), `SELECT status='lost' AND fenced_at IS NULL FROM computer_leases`).Scan(&lost); err != nil {
			t.Fatal(err)
		}
		if lost {
			break
		}
		if time.Now().After(until) {
			t.Fatal("lifecycle did not expire lease")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lifecycle did not join")
	}
}

func TestComputerLifecycleScanProgressesPastBlockedAllocation(t *testing.T) {
	f := newFixture(t)
	for range 100 {
		computer := uuid.NewV7()
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest) SELECT environment_id,$2,initial_root_id,initial_root_digest FROM computers WHERE id=$1;
 INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,computer_instance_id,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at) SELECT environment_id,$2,epoch,worker_host_id,worker_epoch,expires_at,$2,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE computer_id=$1`, pgx.QueryExecModeSimpleProtocol, f.computer, computer)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `SELECT id FROM computers WHERE id=$1 FOR NO KEY UPDATE`, f.computer); err != nil {
		t.Fatal(err)
	}
	cursor, more, err := reconcileComputerLifecycle(t.Context(), f.pool, computerLifecyclePosition{})
	if err == nil || !more {
		t.Fatalf("blocked first batch %+v %v %v", cursor, more, err)
	}
	cursor, more, err = reconcileComputerLifecycle(t.Context(), f.pool, cursor)
	if err != nil || more {
		t.Fatalf("later allocations starved %+v %v %v", cursor, more, err)
	}
	var lost int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_leases WHERE status='lost'`).Scan(&lost); err != nil || lost != 100 {
		t.Fatalf("later allocations %d %v", lost, err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err = reconcileComputerLifecycle(t.Context(), f.pool, cursor); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_leases WHERE status='lost'`).Scan(&lost); err != nil || lost != 101 {
		t.Fatalf("blocked allocation not revisited %d %v", lost, err)
	}
}

func TestComputerLeaseExpiryKeepsSeparatelyPlacedHelperRunning(t *testing.T) {
	f := newFixture(t)
	child := ownedFixture(t, f)
	child.computer = uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest) SELECT environment_id,$2,initial_root_id,initial_root_digest FROM computers WHERE id=$1;
 INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,computer_instance_id,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at) SELECT environment_id,$2,epoch,worker_host_id,worker_epoch,expires_at,$2,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE computer_id=$1;
 DELETE FROM session_processes WHERE session_id=$3;
 UPDATE sessions SET computer_id=$2 WHERE id=$3;
 INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status) VALUES($4,$3,1,$2,1,'ready')`, pgx.QueryExecModeSimpleProtocol, f.computer, child.computer, child.session, f.env)
	active := child.enqueue(t, "helper")
	if _, err := Dispatch(t.Context(), f.pool, child.execution()); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE computer_id=$1`, f.computer)
	if err := expireComputerLease(t.Context(), f.pool, f.env, f.computer, 1); err != nil {
		t.Fatal(err)
	}
	var unaffected bool
	if err := f.pool.QueryRow(t.Context(), `SELECT t.status='running' AND p.status='ready' AND p.failure_recorded_at IS NULL AND NOT EXISTS(SELECT 1 FROM session_holds WHERE session_id=$1) FROM turns t JOIN session_processes p ON p.session_id=t.session_id WHERE t.session_id=$1 AND t.id=$2`, child.session, active.TurnID).Scan(&unaffected); err != nil || !unaffected {
		t.Fatalf("separately placed helper interrupted %v %v", unaffected, err)
	}
}

func TestComputerCaptureLossPreservesStoppedMember(t *testing.T) {
	for _, graceful := range []bool{false, true} {
		name := "unexpected"
		if graceful {
			name = "requested"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
			if err != nil {
				t.Fatal(err)
			}
			p, _ := sourceAbort(t, f)
			if graceful {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopping' WHERE session_id=$1`, f.session)
			}
			if err := ObserveSessionStopped(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence); err != nil {
				t.Fatal(err)
			}
			if !graceful {
				var hold uuid.UUID
				if err := f.pool.QueryRow(t.Context(), `SELECT id FROM session_holds WHERE session_id=$1`, f.session).Scan(&hold); err != nil {
					t.Fatal(err)
				}
				req := controlRequest(f, "resume", "release-stop-hold")
				req.HoldID = hold
				if _, err := ControlSession(t.Context(), f.pool, f.caller(), req); err != nil {
					t.Fatal(err)
				}
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='physical owner destroyed' WHERE computer_id=$1`, f.computer)
			for range 2 {
				if err := LoseUnreadyComputerCapture(t.Context(), f.pool, f.env, uuid.MustParse(p.Capture.CheckpointId)); err != nil {
					t.Fatal(err)
				}
			}
			var stopped bool
			if err := f.pool.QueryRow(t.Context(), `SELECT status='stopped' AND fenced_at IS NOT NULL FROM session_processes WHERE session_id=$1`, f.session).Scan(&stopped); err != nil || !stopped {
				t.Fatalf("stop overwritten: %v %v", stopped, err)
			}
			var holds int
			if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE session_id=$1 AND released_at IS NULL`, f.session).Scan(&holds); err != nil || holds != 0 {
				t.Fatalf("new failure holds: %d %v", holds, err)
			}
		})
	}
}

func TestComputerLifecycleDetectsHostAuthorityLoss(t *testing.T) {
	for _, change := range []string{
		"UPDATE worker_hosts SET current_epoch=current_epoch+1",
		"UPDATE worker_hosts SET status='lost',lost_at=clock_timestamp()",
		"UPDATE worker_hosts SET status='registering',current_epoch=NULL,current_service_id=NULL,epoch_started_at=NULL",
	} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			dbtest.MustExec(t, t.Context(), f.pool, change)
			if _, _, err := reconcileComputerLifecycle(t.Context(), f.pool, computerLifecyclePosition{}); err != nil {
				t.Fatal(err)
			}
			var lost bool
			if err := f.pool.QueryRow(t.Context(), `SELECT status='lost' AND fenced_at IS NULL FROM computer_leases`).Scan(&lost); err != nil || !lost {
				t.Fatalf("host loss not reconciled: %v %v", lost, err)
			}
		})
	}
}

func TestComputerLeaseRenewalRetainsLifecyclePhase(t *testing.T) {
	for _, phase := range []string{"acquiring", "releasing"} {
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t)
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status=$1,expires_at=clock_timestamp()+interval '10 seconds'`, phase)
			if _, err := RenewComputerLease(t.Context(), f.pool, *f.host(), leaseIdentity(f)); err != nil {
				t.Fatal(err)
			}
			var actual string
			if err := f.pool.QueryRow(t.Context(), `SELECT status FROM computer_leases`).Scan(&actual); err != nil || actual != phase {
				t.Fatalf("renewal changed phase: %s %v", actual, err)
			}
		})
	}
}

func TestComputerLeaseLossSettlesUnpublishedCapturePreservingIdentity(t *testing.T) {
	f := newFixture(t)
	_, save := f.finalize(t, "prepared")
	storage := newSaveStorageFixture(t, f)
	_, root := storage.cut(t, 2)
	f.capture(t, save, root)
	if err := ObserveComputerStopped(t.Context(), f.pool, *f.host(), leaseIdentity(f), uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	var retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='failed' AND failure_evidence IS NOT NULL AND 'sha256:'||encode(captured_root_digest,'hex')=$2 FROM computer_saves WHERE id=$1`, save.ID, root).Scan(&retained); err != nil || !retained {
		t.Fatalf("unpublished cut identity or definitive disposition missing: %v %v", retained, err)
	}
}

func TestPhysicalStopReleasesOnlyDeletedComputerKey(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprint(deleted), func(t *testing.T) {
			f := newFixture(t)
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET key='retained-key',deleted_at=CASE WHEN $2 THEN clock_timestamp() END WHERE environment_id=$1; UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, deleted)
			if err := expireComputerLease(t.Context(), f.pool, f.env, f.computer, 1); err != nil {
				t.Fatal(err)
			}
			var key *string
			if err := f.pool.QueryRow(t.Context(), `SELECT key FROM computers WHERE environment_id=$1 AND id=$2`, f.env, f.computer).Scan(&key); err != nil || key == nil {
				t.Fatalf("expiry released physical key: %v %v", key, err)
			}
			if err := ObserveComputerStopped(t.Context(), f.pool, *f.host(), ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, InstanceID: f.computer, Epoch: 1}, uuid.Nil()); err != nil {
				t.Fatal(err)
			}
			if err := f.pool.QueryRow(t.Context(), `SELECT key FROM computers WHERE environment_id=$1 AND id=$2`, f.env, f.computer).Scan(&key); err != nil || (key == nil) != deleted {
				t.Fatalf("physical release key: %v %v", key, err)
			}
		})
	}
}

func TestComputerRestoreFailureWithdrawsOnlyExactRetainedCheckpoint(t *testing.T) {
	for _, phase := range []string{"before-installation", "after-installation-preparation", "later-target-before-preparation"} {
		t.Run(phase, func(t *testing.T) {
			r := newComputerRestoreFixture(t)
			if phase != "before-installation" {
				r.prepare(t)
			}
			identity := func() ComputerLeaseIdentity {
				id := ComputerLeaseIdentity{EnvironmentID: r.f.env, ComputerID: r.f.computer, Epoch: r.epoch}
				if err := r.f.pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, r.f.env, r.f.computer, r.epoch).Scan(&id.InstanceID); err != nil {
					t.Fatal(err)
				}
				return id
			}
			if phase == "later-target-before-preparation" {
				if err := ObserveComputerStopped(t.Context(), r.f.pool, r.host, identity(), uuid.Nil()); err != nil {
					t.Fatal(err)
				}
				r.newTarget(t)
			}
			current := identity()
			wrongInstance := current
			wrongInstance.InstanceID = uuid.NewV7()
			if err := ObserveComputerStopped(t.Context(), r.f.pool, r.host, wrongInstance, r.manifest.CheckpointID); !errors.Is(err, ErrDenied) {
				t.Fatalf("wrong instance report: %v", err)
			}
			queued := r.f.enqueue(t, "queued-through-invalid-restore")
			if err := ObserveComputerStopped(t.Context(), r.f.pool, r.host, current, uuid.NewV7()); !errors.Is(err, ErrDenied) {
				t.Fatalf("unbound checkpoint report: %v", err)
			}
			var intact bool
			if err := r.f.pool.QueryRow(t.Context(), `SELECT cp.status IN ('ready','restoring') AND l.fenced_at IS NULL FROM computer_checkpoints cp JOIN computer_leases l ON l.environment_id=cp.environment_id AND l.computer_id=cp.computer_id WHERE cp.id=$1 AND l.epoch=$2`, r.manifest.CheckpointID, r.epoch).Scan(&intact); err != nil || !intact {
				t.Fatalf("unbound report mutated state: %v %v", intact, err)
			}
			for range 2 {
				if err := ObserveComputerStopped(t.Context(), r.f.pool, r.host, current, r.manifest.CheckpointID); err != nil {
					t.Fatal(err)
				}
			}
			var lost bool
			if err := r.f.pool.QueryRow(t.Context(), `SELECT status='lost' AND capture_request IS NULL AND manifest IS NOT NULL FROM computer_checkpoints WHERE id=$1`, r.manifest.CheckpointID).Scan(&lost); err != nil || !lost {
				t.Fatalf("checkpoint remained eligible: %v %v", lost, err)
			}
			if err := r.f.pool.QueryRow(t.Context(), `SELECT bool_and(status='lost' AND failure_recorded_at IS NOT NULL AND fenced_at IS NOT NULL) FROM session_processes WHERE computer_id=$1`, r.f.computer).Scan(&lost); err != nil || !lost {
				t.Fatalf("captured generations not held/fenced: %v %v", lost, err)
			}
			var holds int
			if err := r.f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE released_at IS NULL`).Scan(&holds); err != nil || holds != 2 {
				t.Fatalf("holds=%d error=%v", holds, err)
			}
			resume := controlRequest(r.f, "resume", "after-checkpoint-loss")
			if err := r.f.pool.QueryRow(t.Context(), `SELECT id FROM session_holds WHERE session_id=$1 AND released_at IS NULL`, r.f.session).Scan(&resume.HoldID); err != nil {
				t.Fatal(err)
			}
			if _, err := ControlSession(t.Context(), r.f.pool, r.f.caller(), resume); err != nil {
				t.Fatal(err)
			}
			if err := ObserveComputerStopped(t.Context(), r.f.pool, r.host, current, r.manifest.CheckpointID); err != nil {
				t.Fatal(err)
			}
			if err := r.f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE session_id=$1 AND released_at IS NULL`, r.f.session).Scan(&holds); err != nil || holds != 0 {
				t.Fatalf("replayed failure recreated released hold: %d %v", holds, err)
			}
			var queuedIntact bool
			if err := r.f.pool.QueryRow(t.Context(), `SELECT status='queued' FROM turns WHERE id=$1`, queued.TurnID).Scan(&queuedIntact); err != nil || !queuedIntact {
				t.Fatalf("accepted input lost: %v %v", queuedIntact, err)
			}
		})
	}
}

func TestComputerRestoreInvalidCheckpointPreservesCompletedOutcomeAndRejectsStaleOwner(t *testing.T) {
	f := newFixture(t)
	storage := newSaveStorageFixture(t, f)
	completed, save := f.finalize(t, "completed-before-capture")
	cut, root := storage.cut(t, 41)
	f.capture(t, save, root)
	if err := storage.publish(t, save.ID, cut); err != nil {
		t.Fatal(err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, completed.TurnID); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := f.pool.QueryRow(t.Context(), `SELECT to_jsonb(t)::text FROM turns t WHERE id=$1`, completed.TurnID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	checkpoint := checkpointStorageForSaveFixture(t, storage)
	if err := checkpoint.publisher.RegisterCheckpoint(t.Context(), checkpoint.ref, checkpoint.manifest); err != nil {
		t.Fatal(err)
	}
	checkpoint.upload(t)
	if err := checkpoint.publish(t, checkpoint.save, checkpoint.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.publisher.CompleteCheckpoint(t.Context(), checkpoint.ref, checkpoint.manifest); err != nil {
		t.Fatal(err)
	}
	r := restoreCheckpointFixture(t, checkpoint)
	old := ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, Epoch: r.epoch}
	if err := f.pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM computer_leases WHERE computer_id=$1 AND epoch=$2`, f.computer, r.epoch).Scan(&old.InstanceID); err != nil {
		t.Fatal(err)
	}
	if err := ObserveComputerStopped(t.Context(), f.pool, r.host, old, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	r.newTarget(t)
	current := old
	current.Epoch = r.epoch
	if err := f.pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM computer_leases WHERE computer_id=$1 AND epoch=$2`, f.computer, r.epoch).Scan(&current.InstanceID); err != nil {
		t.Fatal(err)
	}
	if err := ObserveComputerStopped(t.Context(), f.pool, r.host, old, r.manifest.CheckpointID); err != nil {
		t.Fatal(err)
	}
	var intact bool
	if err := f.pool.QueryRow(t.Context(), `SELECT cp.status='ready' AND l.fenced_at IS NULL FROM computer_checkpoints cp JOIN computer_leases l ON l.computer_id=cp.computer_id WHERE cp.id=$1 AND l.epoch=$2`, r.manifest.CheckpointID, r.epoch).Scan(&intact); err != nil || !intact {
		t.Fatalf("stale report changed current restore: %v %v", intact, err)
	}
	if err := ObserveComputerStopped(t.Context(), f.pool, r.host, current, r.manifest.CheckpointID); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := f.pool.QueryRow(t.Context(), `SELECT to_jsonb(t)::text FROM turns t WHERE id=$1`, completed.TurnID).Scan(&after); err != nil || before != after {
		t.Fatalf("completed outcome changed: %v", err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT status='published' FROM computer_saves WHERE id=$1`, save.ID).Scan(&intact); err != nil || !intact {
		t.Fatalf("completed save changed: %v %v", intact, err)
	}
}
