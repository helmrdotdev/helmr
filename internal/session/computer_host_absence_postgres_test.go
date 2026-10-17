package session_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/dispatch"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func confirmComputerHostAbsent(ctx context.Context, f fixture, host uuid.UUID) error {
	return db.RunTx(ctx, f.pool, func(tx pgx.Tx) error {
		absence, err := workergroup.ConfirmHostProviderAbsent(ctx, tx, audit.System("test-fixture"), host)
		if err != nil {
			return err
		}
		return dispatch.ObserveProviderAbsentHost(ctx, absence)
	})
}

func TestComputerHostAbsencePreservesAcceptedWorkAndFencesAllEpochs(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	active := f.enqueue(t, "running")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	queued := f.enqueue(t, "queued")
	result, save := peer.finalize(t, "prepared")
	storage := newSaveStorageFixture(t, peer)
	checkpointImageExposure(t, f)
	cut, root := storage.cut(t, 2)
	peer.capture(t, save, root)
	if err := storage.publish(t, save.ID, cut); err != nil {
		t.Fatal(err)
	}
	second := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET current_epoch=2 WHERE id=$1;
 INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes) SELECT environment_id,$3,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes FROM computers WHERE id=$2;
 INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,allocation_id,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at)
 SELECT environment_id,$3,epoch,worker_host_id,2,expires_at,$4,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE computer_id=$2`, pgx.QueryExecModeSimpleProtocol, f.worker, f.computer, second, uuid.NewV7())
	for range 2 {
		if err := confirmComputerHostAbsent(t.Context(), f, f.worker); err != nil {
			t.Fatal(err)
		}
	}
	var fenced bool
	if err := f.pool.QueryRow(t.Context(), `SELECT bool_and(fenced_at IS NOT NULL AND fence_evidence='provider confirmed physical host absence') FROM computer_leases`).Scan(&fenced); err != nil || !fenced {
		t.Fatalf("physical evidence: %v %v", fenced, err)
	}
	for id, want := range map[uuid.UUID]string{active.TurnID: "interrupted", queued.TurnID: "queued", result.TurnID: "completed"} {
		var status string
		if err := f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, id).Scan(&status); err != nil || status != want {
			t.Fatalf("turn status %s want %s: %v", status, want, err)
		}
	}
	if err := completedTurnForTest(t.Context(), f.pool, f.env, peer.session, result.TurnID); err != nil {
		t.Fatal(err)
	}
	host, err := workergroup.GetHost(t.Context(), db.New(f.pool), f.worker)
	if err != nil || host.Status != workergroup.WorkerHostStatusLost || host.DrainBlockers.UnreclaimedInstances != 0 || host.DrainBlockers.UnreconciledSessionProcesses != 0 {
		t.Fatalf("host %+v: %v", host, err)
	}
	var holds int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds`).Scan(&holds); err != nil || holds != 0 {
		t.Fatalf("duplicate or missing holds: %d %v", holds, err)
	}
}

func TestComputerHostAbsenceRollsBackConfirmationOnInterruptedReconciliation(t *testing.T) {
	f := newFixture(t)
	lock, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(t.Context())
	if _, err = lock.Exec(t.Context(), `SELECT id FROM computers WHERE id=$1 FOR NO KEY UPDATE`, f.computer); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- confirmComputerHostAbsent(ctx, f, f.worker) }()
	waitSessionLifecycleLock(t, f)
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reconciliation: %v", err)
	}
	var unchanged bool
	if err = f.pool.QueryRow(t.Context(), `SELECT h.status='active' AND h.lost_at IS NULL AND h.claim_version=1 AND l.fenced_at IS NULL FROM worker_hosts h JOIN computer_leases l ON l.worker_host_id=h.id WHERE h.id=$1`, f.worker).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("partial confirmation committed: %v %v", unchanged, err)
	}
	if err = lock.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = confirmComputerHostAbsent(t.Context(), f, f.worker); err != nil {
		t.Fatal(err)
	}
}

func TestComputerHostAbsenceRetainsEligibleSetupOnAnotherHost(t *testing.T) {
	r := newComputerRestoreFixture(t)
	dbtest.MustExec(t, t.Context(), r.f.pool, `DELETE FROM computer_leases WHERE epoch=2;
 UPDATE computer_leases SET status='active',fenced_at=NULL,fence_evidence=NULL WHERE epoch=1`, pgx.QueryExecModeSimpleProtocol)
	if err := confirmComputerHostAbsent(t.Context(), r.f, r.f.worker); err != nil {
		t.Fatal(err)
	}
	var retained bool
	if err := r.f.pool.QueryRow(t.Context(), `SELECT status='ready' AND fenced_at IS NULL AND failure_recorded_at IS NULL FROM session_processes`).Scan(&retained); err != nil || !retained {
		t.Fatalf("setup lost: %v %v", retained, err)
	}
	host, err := workergroup.GetHost(t.Context(), db.New(r.f.pool), r.f.worker)
	if err != nil || host.DrainBlockers.UnreconciledSessionProcesses != 0 {
		t.Fatalf("logical continuation counted as physical blocker: %+v %v", host.DrainBlockers, err)
	}
	r.newTarget(t)
	if p := r.prepare(t); len(p.StoppedSessions) != 0 {
		t.Fatal("healthy continuation discarded")
	}
}

func TestComputerHostAbsenceSeesAllocationCommittedBeforeSupplyLock(t *testing.T) {
	f := newFixture(t)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), tx, `SELECT id FROM worker_groups WHERE id=$1 FOR SHARE`, f.host().GroupID)
	next := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes) SELECT environment_id,$2,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes FROM computers WHERE id=$1;
 INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,allocation_id,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at)
 SELECT environment_id,$2,epoch,worker_host_id,worker_epoch,expires_at,$3,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE computer_id=$1`, pgx.QueryExecModeSimpleProtocol, f.computer, next, uuid.NewV7())
	done := make(chan error, 1)
	go func() { done <- confirmComputerHostAbsent(t.Context(), f, f.worker) }()
	waitSessionLifecycleLock(t, f)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	var fenced bool
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*)=2 AND bool_and(fenced_at IS NOT NULL) FROM computer_leases`).Scan(&fenced); err != nil || !fenced {
		t.Fatalf("missed late allocation: %v %v", fenced, err)
	}
}

func TestComputerHostAbsencePreservesStoppedMember(t *testing.T) {
	for _, graceful := range []bool{false, true} {
		name := "unexpected"
		if graceful {
			name = "requested"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			a, err := session.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
			if err != nil {
				t.Fatal(err)
			}
			_, _ = sourceAbort(t, f)
			if graceful {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopping' WHERE session_id=$1`, f.session)
			}
			if err := session.ObserveSessionStopped(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence); err != nil {
				t.Fatal(err)
			}
			if !graceful {
				var hold uuid.UUID
				if err := f.pool.QueryRow(t.Context(), `SELECT id FROM session_holds WHERE session_id=$1`, f.session).Scan(&hold); err != nil {
					t.Fatal(err)
				}
				req := controlRequest(f, "resume", "release-stop-hold")
				req.HoldID = hold
				if _, err := session.ControlSession(t.Context(), f.pool, f.caller(), req); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := confirmComputerHostAbsent(t.Context(), f, f.worker); err != nil {
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

func TestComputerHostAbsenceOrdersRootsAcrossWorkerGroups(t *testing.T) {
	f := newFixture(t)
	peer := f.peer(t)
	child, peerChild := ownedFixture(t, f), ownedFixture(t, peer)
	group, pool, host, token := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO worker_group_tokens(id,token_hash) VALUES($4,decode(repeat('22',32),'hex'));
 INSERT INTO worker_groups(id,token_id,region_id,name,status) SELECT $2,$4,region_id,'other-group','draining' FROM worker_groups WHERE id=$1;
 INSERT INTO worker_pools SELECT (jsonb_populate_record(NULL::worker_pools,to_jsonb(p)||jsonb_build_object('id',$3::uuid,'worker_group_id',$2::uuid))).* FROM worker_pools p WHERE worker_group_id=$1;
 INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$5::uuid,'worker_group_id',$2::uuid,'worker_pool_id',$3::uuid,'resource_id','other-host','current_service_id',$5::uuid))).* FROM worker_hosts h WHERE id=$6`, pgx.QueryExecModeSimpleProtocol, f.group, group, pool, token, host, f.worker)
	computers := []uuid.UUID{uuid.NewV7(), uuid.NewV7(), uuid.NewV7()}
	slices.SortFunc(computers, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	for i, computer := range computers {
		owner := host
		if i == 2 {
			owner = f.worker
		}
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes) SELECT environment_id,$2,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes FROM computers WHERE id=$1;
 INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,allocation_id,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at)
 SELECT environment_id,$2,epoch,$3,worker_epoch,expires_at,$4,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE computer_id=$1`, pgx.QueryExecModeSimpleProtocol, f.computer, computer, owner, uuid.NewV7())
	}
	// Host 1 visits peer-root then root; host 2 visits root then peer-root.
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM session_processes WHERE environment_id=$1`, f.env)
	for sessionID, computer := range map[uuid.UUID]uuid.UUID{peer.session: f.computer, child.session: computers[0], peerChild.session: computers[1], f.session: computers[2]} {
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET computer_id=$3 WHERE environment_id=$1 AND id=$2;
 INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status) VALUES($1,$2,1,$3,1,'ready')`, pgx.QueryExecModeSimpleProtocol, f.env, sessionID, computer)
	}
	barrier, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), barrier, `SELECT id FROM computers WHERE id=$1 FOR NO KEY UPDATE`, f.computer)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- confirmComputerHostAbsent(ctx, f, f.worker) }()
	waitSessionLifecycleLock(t, f)
	go func() { second <- confirmComputerHostAbsent(ctx, f, host) }()
	for {
		var waits int
		if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND pid<>pg_backend_pid()`).Scan(&waits); err != nil {
			t.Fatal(err)
		}
		if waits >= 2 {
			break
		}
		select {
		case err := <-second:
			t.Fatalf("second host did not synchronize: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if err = barrier.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-first; err != nil {
		t.Fatalf("first host: %v", err)
	}
	if err = <-second; err != nil {
		t.Fatalf("second host: %v", err)
	}
	var fenced bool
	if err = f.pool.QueryRow(ctx, `SELECT bool_and(fenced_at IS NOT NULL) FROM computer_leases`).Scan(&fenced); err != nil || !fenced {
		t.Fatalf("physical closure incomplete: %v %v", fenced, err)
	}
}

func TestComputerHostAbsenceRequiresTransactionBoundConfirmation(t *testing.T) {
	if err := dispatch.ObserveProviderAbsentHost(t.Context(), workergroup.ConfirmedProviderAbsence{}); !errors.Is(err, dispatch.ErrInvalidInput) {
		t.Fatalf("missing confirmation: %v", err)
	}
	f := newFixture(t)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	absence, err := workergroup.ConfirmHostProviderAbsent(t.Context(), tx, audit.System("test-fixture"), f.worker)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = dispatch.ObserveProviderAbsentHost(t.Context(), absence); !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("confirmation escaped its transaction: %v", err)
	}
}

func TestProviderAbsenceAuditIncludesAlreadyLostHostWithPhysicalAllocations(t *testing.T) {
	f := newFixture(t)
	if _, err := workergroup.MarkHostLost(t.Context(), f.pool, audit.System("operator"), f.group, "host", 1); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
			absence, err := workergroup.ConfirmHostProviderAbsent(t.Context(), tx, audit.CapacityAuthority(), f.worker)
			if err != nil {
				return err
			}
			return dispatch.ObserveProviderAbsentHost(t.Context(), absence)
		}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='worker_host.provider_absence_confirmed' AND actor_kind='capacity' AND actor_user_id IS NULL AND actor_credential_id IS NULL AND target_id=$1 AND org_id IS NULL`, f.worker).Scan(&count); err != nil || count != 1 {
		t.Fatalf("physical confirmation count=%d err=%v", count, err)
	}
}
