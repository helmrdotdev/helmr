package agent

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func recoveringComputerHost(t *testing.T, f fixture) workergroup.HostPrincipal {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET current_epoch=current_epoch+1,status='registering' WHERE id=$1`, f.host().HostID)
	h := *f.host()
	h.Epoch++
	return h
}

func TestComputerHostRecoveryRequiresPhysicalReportAndKeepsQuarantine(t *testing.T) {
	f := newFixture(t)
	instance := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET computer_instance_id=$1`, instance)
	running := f.enqueue(t, "running")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	queued := f.enqueue(t, "queued")
	h := recoveringComputerHost(t, f)
	if err := expireComputerLease(t.Context(), f.pool, f.env, f.computer, 1); err != nil {
		t.Fatal(err)
	}
	assertFenced := func(want bool) {
		t.Helper()
		var fenced bool
		if err := f.pool.QueryRow(t.Context(), `SELECT fenced_at IS NOT NULL FROM computer_leases`).Scan(&fenced); err != nil || fenced != want {
			t.Fatalf("fenced=%v want %v: %v", fenced, want, err)
		}
	}
	assertFenced(false)
	if err := ObserveRecoveredHostComputers(t.Context(), f.pool, h, []uuid.UUID{instance}); err != nil {
		t.Fatal(err)
	}
	assertFenced(false)
	if err := ObserveRecoveredHostComputers(t.Context(), f.pool, *f.host(), []uuid.UUID{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("prior epoch reported recovery: %v", err)
	}
	if err := ObserveRecoveredHostComputers(t.Context(), f.pool, h, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing inventory accepted: %v", err)
	}
	for range 2 {
		if err := ObserveRecoveredHostComputers(t.Context(), f.pool, h, []uuid.UUID{}); err != nil {
			t.Fatal(err)
		}
	}
	assertFenced(true)
	for id, want := range map[uuid.UUID]string{running.TurnID: "interrupted", queued.TurnID: "queued"} {
		var actual string
		if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, id).Scan(&actual); err != nil || actual != want {
			t.Fatalf("turn %s got %s want %s: %v", id, actual, want, err)
		}
	}
	var holds int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds`).Scan(&holds); err != nil || holds != 1 {
		t.Fatalf("duplicate failure %d %v", holds, err)
	}
}

func TestComputerHostRecoveryPreservesEligibleLogicalProcess(t *testing.T) {
	r := newComputerRestoreFixture(t)
	dbtest.MustExec(t, t.Context(), r.f.pool, `DELETE FROM computer_leases WHERE epoch=2;
 UPDATE computer_leases SET status='active',fenced_at=NULL,fence_evidence=NULL WHERE epoch=1`, pgx.QueryExecModeSimpleProtocol)
	h := recoveringComputerHost(t, r.f)
	if err := ObserveRecoveredHostComputers(t.Context(), r.f.pool, h, []uuid.UUID{}); err != nil {
		t.Fatal(err)
	}
	var preserved bool
	if err := r.f.pool.QueryRow(t.Context(), `SELECT status='ready' AND fenced_at IS NULL AND failure_recorded_at IS NULL FROM session_processes WHERE session_id=$1`, r.f.session).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("logical continuation lost: %v %v", preserved, err)
	}
	r.newTarget(t)
	if p := r.prepare(t); len(p.StoppedSessions) != 0 {
		t.Fatal("recovery discarded healthy setup")
	}
}

func TestComputerHostRecoveryResumesAfterPartialProgress(t *testing.T) {
	f := newFixture(t)
	next := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest) SELECT environment_id,$2,initial_root_id,initial_root_digest FROM computers WHERE id=$1;
 INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,computer_instance_id,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at)
 SELECT environment_id,$2,epoch,worker_host_id,worker_epoch,expires_at,$2,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE computer_id=$1`, pgx.QueryExecModeSimpleProtocol, f.computer, next)
	h := recoveringComputerHost(t, f)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `SELECT id FROM computers WHERE id=$1 FOR NO KEY UPDATE`, next); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ObserveRecoveredHostComputers(ctx, f.pool, h, []uuid.UUID{}) }()
	waitSessionLifecycleLock(t, f)
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_leases WHERE fenced_at IS NOT NULL`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("partial progress not durable: %d %v", count, err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = ObserveRecoveredHostComputers(t.Context(), f.pool, h, []uuid.UUID{}); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_leases WHERE fenced_at IS NOT NULL`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("retry did not finish: %d %v", count, err)
	}
}

func TestComputerHostRecoveryExcludesCurrentAndForeignAllocations(t *testing.T) {
	f := newFixture(t)
	h := recoveringComputerHost(t, f)
	otherHost := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$2::uuid,'resource_id','other-recovering-host','current_service_id',$2::uuid))).* FROM worker_hosts h WHERE id=$1`, h.HostID, otherHost)
	for _, owner := range []struct {
		host  uuid.UUID
		epoch int64
	}{{h.HostID, h.Epoch}, {otherHost, 1}} {
		computer := uuid.NewV7()
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest) SELECT environment_id,$2,initial_root_id,initial_root_digest FROM computers WHERE id=$1;
 INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,computer_instance_id,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at)
 SELECT environment_id,$2,epoch,$3,$4,expires_at,$2,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE computer_id=$1`, pgx.QueryExecModeSimpleProtocol, f.computer, computer, owner.host, owner.epoch)
	}
	if err := ObserveRecoveredHostComputers(t.Context(), f.pool, h, []uuid.UUID{}); err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err := f.pool.QueryRow(t.Context(), `SELECT bool_and((fenced_at IS NOT NULL)=(computer_id=$1)) FROM computer_leases`, f.computer).Scan(&exact); err != nil || !exact {
		t.Fatalf("recovery crossed allocation identity: %v %v", exact, err)
	}
}

func TestComputerHostRecoverySettlesUnpublishedCaptureAfterFencing(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(map[bool]string{false: "requested", true: "captured"}[recorded], func(t *testing.T) {
			f := newFixture(t)
			req := f.captureRequest()
			result, _ := beginCapture(t, f, req)
			if recorded {
				if err := RecordCapture(t.Context(), f.pool, CaptureEvidence{EnvironmentID: f.env, SaveID: result.SaveID, LeaseEpoch: 1, DiskRoot: "sha256:1111111111111111111111111111111111111111111111111111111111111111", Evidence: "frozen cut"}); err != nil {
					t.Fatal(err)
				}
			}
			h := recoveringComputerHost(t, f)
			for range 2 {
				if err := ObserveRecoveredHostComputers(t.Context(), f.pool, h, []uuid.UUID{}); err != nil {
					t.Fatal(err)
				}
			}
			var status, failure string
			var lost bool
			if err := f.pool.QueryRow(t.Context(), `SELECT s.status,COALESCE(s.failure_evidence,''),c.status='lost' AND c.capture_request IS NULL AND l.fenced_at IS NOT NULL FROM computer_saves s JOIN computer_checkpoints c ON c.disk_save_id=s.id JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(s.environment_id,s.computer_id,s.computer_lease_epoch) WHERE s.id=$1`, result.SaveID).Scan(&status, &failure, &lost); err != nil {
				t.Fatal(err)
			}
			if !lost || status != "failed" || failure != "publication absent after writer authority ended" {
				t.Fatalf("capture lost and fenced=%v save=%s failure=%q", lost, status, failure)
			}
		})
	}
}

func TestComputerHostRecoveryContinuesPastBlockedComputer(t *testing.T) {
	f := newFixture(t)
	next := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest) SELECT environment_id,$2,initial_root_id,initial_root_digest FROM computers WHERE id=$1;
 INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,computer_instance_id,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at)
 SELECT environment_id,$2,epoch,worker_host_id,worker_epoch,expires_at,$3,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE computer_id=$1`, pgx.QueryExecModeSimpleProtocol, f.computer, next, uuid.NewV7())
	h := recoveringComputerHost(t, f)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `SELECT id FROM computers WHERE id=$1 FOR NO KEY UPDATE`, f.computer); err != nil {
		t.Fatal(err)
	}
	if err = ObserveRecoveredHostComputers(t.Context(), f.pool, h, []uuid.UUID{}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("blocked recovery: %v", err)
	}
	var progressed bool
	if err = f.pool.QueryRow(t.Context(), `SELECT bool_and((fenced_at IS NOT NULL)=(computer_id=$1)) FROM computer_leases`, next).Scan(&progressed); err != nil || !progressed {
		t.Fatalf("healthy peer blocked: %v %v", progressed, err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = ObserveRecoveredHostComputers(t.Context(), f.pool, h, []uuid.UUID{}); err != nil {
		t.Fatal(err)
	}
}

func TestComputerHostRecoveryPreservesStoppedMember(t *testing.T) {
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
			_, _ = sourceAbort(t, f)
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
			host := recoveringComputerHost(t, f)
			for range 2 {
				if err := ObserveRecoveredHostComputers(t.Context(), f.pool, host, []uuid.UUID{}); err != nil {
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
