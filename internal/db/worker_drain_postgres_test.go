package db_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkerDrainPublishesExactTerminalReceiptAndReplays(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := insertActiveWorkerWithObservation(t, ctx, pool, time.Now().UTC())
	hostSecretID := uuid.NewV7()
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO worker_host_secrets (
			id, worker_group_id, worker_host_id, key_prefix, claim_version,
			secret_hash
		) VALUES ($1, $2, $3, $4, 1, $5)
	`, hostSecretID, dbtest.DefaultWorkerGroupID, workerID, uuid.New().String(), []byte("drain-secret"))

	draining, err := q.DrainWorkerHost(ctx, db.DrainWorkerHostParams{
		ID:                   pgvalue.UUID(workerID),
		WorkerGroupID:        dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:        pgtype.Int8{Int64: 1, Valid: true},
		ExpectedClaimVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if draining.Status != db.WorkerHostStatusDraining || draining.ClaimVersion != 2 {
		t.Fatalf("draining row = %+v", draining)
	}

	params := db.CompleteWorkerDrainParams{
		WorkerHostID:         pgvalue.UUID(workerID),
		WorkerGroupID:        dbtest.DefaultWorkerGroupID,
		WorkerEpoch:          pgtype.Int8{Int64: 1, Valid: true},
		ExpectedClaimVersion: draining.ClaimVersion,
		ObservedAt:           pgvalue.Timestamptz(time.Now().UTC()),
	}
	completed, err := q.CompleteWorkerDrain(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != db.WorkerHostStatusTerminationReady || completed.ClaimVersion != 3 || !completed.TerminationReadyAt.Valid {
		t.Fatalf("terminal receipt = %+v", completed)
	}
	replayed, err := q.CompleteWorkerDrain(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Status != completed.Status || replayed.ClaimVersion != completed.ClaimVersion || replayed.TerminationReadyAt != completed.TerminationReadyAt {
		t.Fatalf("replayed receipt = %+v, want %+v", replayed, completed)
	}
	params.ExpectedClaimVersion = completed.ClaimVersion
	if _, err := q.CompleteWorkerDrain(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale/new completion error = %v, want pgx.ErrNoRows", err)
	}

	var revoked bool
	if err := pool.QueryRow(ctx, `
		SELECT revoked_at IS NOT NULL
		  FROM worker_host_secrets
		 WHERE id = $1
	`, hostSecretID).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("terminal receipt did not revoke the worker credential")
	}
}

func TestWorkerDrainCurrentClaimPreservesFences(t *testing.T) {
	ctx := t.Context()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := insertActiveWorkerWithObservation(t, ctx, pool, time.Now().UTC())
	params := db.DrainWorkerHostParams{
		ID: pgvalue.UUID(workerID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: 1,
	}
	first, err := q.DrainWorkerHost(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	for _, claim := range []int64{first.ClaimVersion, 1, first.ClaimVersion} {
		params.ExpectedClaimVersion = claim
		got, err := q.DrainWorkerHost(ctx, params)
		if err != nil {
			t.Fatalf("drain claim %d: %v", claim, err)
		}
		if got.ClaimVersion != first.ClaimVersion || got.DrainingAt != first.DrainingAt {
			t.Fatalf("reentry changed transition: %+v", got)
		}
	}
	for _, name := range []string{"worker", "group", "epoch", "older claim", "future claim"} {
		t.Run(name, func(t *testing.T) {
			bad := params
			switch name {
			case "worker":
				bad.ID = pgvalue.UUID(uuid.NewV7())
			case "group":
				bad.WorkerGroupID = pgvalue.UUID(uuid.NewV7())
			case "epoch":
				bad.ExpectedEpoch.Int64++
			case "older claim":
				bad.ExpectedClaimVersion = 0
			case "future claim":
				bad.ExpectedClaimVersion++
			}
			if _, err := q.DrainWorkerHost(ctx, bad); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("invalid fence error=%v, want no rows", err)
			}
		})
	}
	dbtest.MustExec(t, ctx, pool, `UPDATE worker_hosts
 SET status='termination_ready',claim_version=claim_version+1,termination_ready_at=now() WHERE id=$1`, workerID)
	for _, claim := range []int64{first.ClaimVersion, first.ClaimVersion + 1} {
		params.ExpectedClaimVersion = claim
		if _, err := q.DrainWorkerHost(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("terminal drain claim %d error=%v, want no rows", claim, err)
		}
	}
}

func TestWorkerDrainPreservesPhysicalStateAndClosesAdmission(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	var instanceID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&instanceID); err != nil {
		t.Fatal(err)
	}
	snapshot := func() []byte {
		var value []byte
		if err := f.Pool.QueryRow(t.Context(), `SELECT to_jsonb(i)-'admission_state'-'updated_at' FROM computer_instances i WHERE id=$1`, instanceID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot()
	params := db.DrainWorkerHostParams{ID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: 1}
	for range 2 {
		if _, err := db.New(f.Pool).DrainWorkerHost(t.Context(), params); err != nil {
			t.Fatal(err)
		}
		var state string
		if err := f.Pool.QueryRow(t.Context(), `SELECT admission_state FROM computer_instances WHERE id=$1`, instanceID).Scan(&state); err != nil || state != "draining" {
			t.Fatalf("admission=%s err=%v", state, err)
		}
		if after := snapshot(); !bytes.Equal(before, after) {
			t.Fatal("drain changed physical instance state")
		}
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='closed' WHERE id=$1`, instanceID)
	if _, err := db.New(f.Pool).DrainWorkerHost(t.Context(), params); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.Pool.QueryRow(t.Context(), `SELECT admission_state FROM computer_instances WHERE id=$1`, instanceID).Scan(&state); err != nil || state != "closed" {
		t.Fatalf("replayed drain reopened admission=%s err=%v", state, err)
	}
}

func TestWorkerStartupRecoveryReclaimsPriorInstanceAtomicallyAndReplays(t *testing.T) {
	prepared := prepareOldEpochStartupRecovery(t)
	f := prepared.fixture
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET finalization_action='discard',finalization_reason_code='worker_draining' WHERE id=$1`, prepared.instanceID)
	q := db.New(f.Pool)
	if _, err := q.CompleteWorkerStartupRecovery(t.Context(), prepared.params); err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT observed_state='lost' AND mount_state='lost' AND admission_state='closed' AND reclaimed_at IS NOT NULL AND terminal_at IS NOT NULL AND terminal_reason_code='worker_startup_reclaimed' AND finalization_action='discard' AND finalization_reason_code='worker_draining' AND reclaim_evidence->>'method'='host_reconciled' AND reclaim_evidence->>'completed_at'='2026-08-17T00:00:00Z' FROM computer_instances WHERE id=$1`, prepared.instanceID).Scan(&exact); err != nil || !exact {
		t.Fatalf("startup exclusion=%v err=%v", exact, err)
	}
	var reconciled bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT bool_and(l.process_reconciled_at=i.reclaimed_at) FROM run_leases l JOIN computer_instances i ON i.id=l.computer_instance_id WHERE i.id=$1`, prepared.instanceID).Scan(&reconciled); err != nil || !reconciled {
		t.Fatalf("startup left processes unreconciled: %v %v", reconciled, err)
	}
	before := instanceSnapshot(t, f, prepared.instanceID)
	if _, err := q.CompleteWorkerStartupRecovery(t.Context(), prepared.params); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, instanceSnapshot(t, f, prepared.instanceID)) {
		t.Fatal("replay changed reclaimed instance")
	}
}

func TestWorkerStartupRecoveryPreservesTerminalDiagnostics(t *testing.T) {
	for _, state := range []string{"failed", "lost"} {
		t.Run(state, func(t *testing.T) {
			p := prepareOldEpochStartupRecovery(t)
			f := p.fixture
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state=$2,terminal_at=now(),terminal_reason_code='keep_reason',terminal_error='{"code":"keep_error"}' WHERE id=$1`, p.instanceID, state)
			var terminalAt time.Time
			if err := f.Pool.QueryRow(t.Context(), `SELECT terminal_at FROM computer_instances WHERE id=$1`, p.instanceID).Scan(&terminalAt); err != nil {
				t.Fatal(err)
			}
			if _, err := db.New(f.Pool).CompleteWorkerStartupRecovery(t.Context(), p.params); err != nil {
				t.Fatal(err)
			}
			var exact bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT observed_state=$2 AND terminal_at=$3 AND terminal_reason_code='keep_reason' AND terminal_error->>'code'='keep_error' AND reclaimed_at IS NOT NULL AND mount_state='lost' AND admission_state='closed' FROM computer_instances WHERE id=$1`, p.instanceID, state, terminalAt).Scan(&exact); err != nil || !exact {
				t.Fatalf("preserved diagnostics=%v err=%v", exact, err)
			}
		})
	}
}

func TestProviderAbsenceReclaimsInstanceFromPriorWorkerEpoch(t *testing.T) {
	p := prepareOldEpochStartupRecovery(t)
	q := db.New(p.fixture.Pool)
	if _, err := q.ConfirmWorkerHostProviderAbsent(t.Context(), p.params.WorkerHostID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ReconcileProviderAbsentWorkerInstances(t.Context(), p.params.WorkerHostID); err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err := p.fixture.Pool.QueryRow(t.Context(), `SELECT reclaimed_at IS NOT NULL AND mount_state='lost' AND reclaim_evidence->>'method'='provider_absent' FROM computer_instances WHERE id=$1`, p.instanceID).Scan(&exact); err != nil || !exact {
		t.Fatalf("provider exclusion=%v err=%v", exact, err)
	}
}

func TestWorkerStartupRecoverySerializesInstanceDiagnostics(t *testing.T) {
	p := prepareOldEpochStartupRecovery(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tx, err := p.fixture.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, ctx, tx, `SELECT id FROM computer_instances WHERE id=$1 FOR UPDATE`, p.instanceID)
	done := make(chan error, 1)
	go func() { _, err := db.New(p.fixture.Pool).CompleteWorkerStartupRecovery(ctx, p.params); done <- err }()
	for {
		var blocked bool
		if err := p.fixture.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE query LIKE '%CompleteWorkerStartupRecovery%' AND wait_event_type='Lock')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("recovery completed before lock release: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	dbtest.MustExec(t, ctx, tx, `UPDATE computer_instances SET observed_state='failed',terminal_at=now(),terminal_reason_code='concurrent_failure',terminal_error='{"code":"concurrent"}' WHERE id=$1`, p.instanceID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err := p.fixture.Pool.QueryRow(ctx, `SELECT observed_state='failed' AND terminal_reason_code='concurrent_failure' AND terminal_error->>'code'='concurrent' AND reclaimed_at IS NOT NULL FROM computer_instances WHERE id=$1`, p.instanceID).Scan(&exact); err != nil || !exact {
		t.Fatalf("serialized diagnostics=%v err=%v", exact, err)
	}
}

func TestWorkerStartupRecoveryPreservesQuarantinedAndCurrentInstances(t *testing.T) {
	p := prepareOldEpochStartupRecovery(t)
	// Extra instances each retain their own Computer and source authority.
	peer := p.fixture.AddRunLease(t, "assigned", time.Now())
	current := p.fixture.AddRunLease(t, "assigned", time.Now())
	var peerID, currentID uuid.UUID
	if err := p.fixture.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, peer.LeaseID).Scan(&peerID); err != nil {
		t.Fatal(err)
	}
	if err := p.fixture.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, current.LeaseID).Scan(&currentID); err != nil {
		t.Fatal(err)
	}
	tx, err := p.fixture.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	// This current-epoch instance has no resident Run.
	dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET current_run_lease_id=NULL WHERE id=$1`, current.RunID)
	dbtest.MustExec(t, t.Context(), tx, `DELETE FROM run_leases WHERE id=$1`, current.LeaseID)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_instances SET worker_epoch=2 WHERE id=$1`, currentID)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, currentBefore := instanceSnapshot(t, p.fixture, p.instanceID), instanceSnapshot(t, p.fixture, currentID)
	p.params.RecoveryEvidence = []byte(fmt.Sprintf(`{"observed_at":"2026-08-17T00:00:00Z","quarantined":[%q]}`, p.instanceID.String()))
	if _, err := db.New(p.fixture.Pool).CompleteWorkerStartupRecovery(t.Context(), p.params); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, instanceSnapshot(t, p.fixture, p.instanceID)) || !bytes.Equal(currentBefore, instanceSnapshot(t, p.fixture, currentID)) {
		t.Fatal("excluded instance was mutated")
	}
	var reclaimed bool
	if err := p.fixture.Pool.QueryRow(t.Context(), `SELECT reclaimed_at IS NOT NULL FROM computer_instances WHERE id=$1`, peerID).Scan(&reclaimed); err != nil || !reclaimed {
		t.Fatalf("eligible peer reclaimed=%v err=%v", reclaimed, err)
	}
}

type oldEpochStartupRecovery struct {
	fixture    runtest.Fixture
	instanceID uuid.UUID
	params     db.CompleteWorkerStartupRecoveryParams
}

func prepareOldEpochStartupRecovery(t *testing.T) oldEpochStartupRecovery {
	t.Helper()
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	var instanceID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&instanceID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET status='registering',current_epoch=2,current_service_id=$2,epoch_started_at=now(),activated_at=NULL,vm_platform_id=NULL,epoch_cpu_millis=0,epoch_memory_bytes=0,epoch_guest_ephemeral_disk_bytes=0,per_vm_cpu_millis=0,per_vm_memory_bytes=0,per_vm_guest_ephemeral_disk_bytes=0,max_vm_slots=0,max_vm_starts=0,cpu_environment=NULL,cpu_environment_digest=NULL,observed_at=NULL,run_paused_reason=NULL,vm_paused_reason=NULL WHERE id=$1`, f.WorkerID, uuid.NewV7())
	return oldEpochStartupRecovery{fixture: f, instanceID: instanceID, params: db.CompleteWorkerStartupRecoveryParams{WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerEpoch: pgtype.Int8{Int64: 2, Valid: true}, RecoveryEvidence: []byte(`{"observed_at":"2026-08-17T00:00:00Z","quarantined":[]}`)}}
}

func instanceSnapshot(t *testing.T, f runtest.Fixture, id uuid.UUID) []byte {
	t.Helper()
	var result []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT to_jsonb(i) FROM computer_instances i WHERE id=$1`, id).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestWorkerFencePublishesExactLostReceiptAndReplays(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := insertActiveWorkerWithObservation(t, ctx, pool, time.Now().UTC())
	hostSecretID := uuid.NewV7()
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO worker_host_secrets (
			id, worker_group_id, worker_host_id, key_prefix, claim_version,
			secret_hash
		) VALUES ($1, $2, $3, $4, 1, $5)
	`, hostSecretID, dbtest.DefaultWorkerGroupID, workerID, uuid.New().String(), []byte("fence-secret"))

	params := db.FenceWorkerHostParams{
		ID:                   pgvalue.UUID(workerID),
		WorkerGroupID:        dbtest.DefaultWorkerGroupID,
		ExpectedEpoch:        pgtype.Int8{Int64: 1, Valid: true},
		ExpectedClaimVersion: 1,
		ReasonCode:           pgtype.Text{String: "provider_termination", Valid: true},
	}
	lost, err := q.FenceWorkerHost(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if lost.Status != db.WorkerHostStatusLost || lost.ClaimVersion != 2 || !lost.LostAt.Valid {
		t.Fatalf("lost receipt = %+v", lost)
	}
	replayed, err := q.FenceWorkerHost(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Status != lost.Status || replayed.ClaimVersion != lost.ClaimVersion || replayed.LostAt != lost.LostAt {
		t.Fatalf("replayed lost receipt = %+v, want %+v", replayed, lost)
	}
	params.ExpectedClaimVersion = lost.ClaimVersion
	if _, err := q.FenceWorkerHost(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale/new fence error = %v, want pgx.ErrNoRows", err)
	}

	if _, err := q.AuthorizeWorkerFenceReplay(ctx, db.AuthorizeWorkerFenceReplayParams{
		HostSecretID: pgvalue.UUID(hostSecretID), ClaimVersion: 1,
		WorkerEpoch: pgtype.Int8{Int64: 1, Valid: true},
	}); err != nil {
		t.Fatalf("authorize exact fence replay: %v", err)
	}
}

func TestWorkerDrainRequiresEveryPriorEpochPhysicalScopeReconciled(t *testing.T) {
	for _, blocker := range []string{"instance", "run", "command"} {
		t.Run(blocker, func(t *testing.T) {
			f := runtest.New(t)
			work := f.AddRunLease(t, "assigned", time.Now())
			var instanceID, computerID uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id,computer_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&instanceID, &computerID); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET current_epoch=2 WHERE id=$1`, f.WorkerID)
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_host_secrets(id,worker_group_id,worker_host_id,key_prefix,claim_version,secret_hash) VALUES($1,$2,$3,$4,1,$5)`, uuid.NewV7(), runtest.WorkerGroupID, f.WorkerID, uuid.NewV7().String(), []byte("drain-test"))
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled' WHERE id=$1`, work.LeaseID)
			if blocker != "run" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET process_reconciled_at=now() WHERE id=$1`, work.LeaseID)
			}
			if blocker != "instance" {
				reclaimCapacityInstance(t, f, instanceID)
			}
			if blocker == "command" {
				addCapacityCommand(t, f, computerID)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled',computer_instance_id=$2,writer_generation=2 WHERE computer_id=$1`, computerID, instanceID)
			}
			q := db.New(f.Pool)
			row, err := q.DrainWorkerHost(t.Context(), db.DrainWorkerHostParams{ID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), ExpectedEpoch: pgtype.Int8{Int64: 2, Valid: true}, ExpectedClaimVersion: 1})
			if err != nil {
				t.Fatal(err)
			}
			params := db.CompleteWorkerDrainParams{WorkerHostID: row.ID, WorkerGroupID: row.WorkerGroupID, WorkerEpoch: row.CurrentEpoch, ExpectedClaimVersion: row.ClaimVersion, ObservedAt: pgvalue.Timestamptz(time.Now())}
			if _, err = q.CompleteWorkerDrain(t.Context(), params); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("unreconciled %s allowed drain: %v", blocker, err)
			}
			switch blocker {
			case "instance":
				reclaimCapacityInstance(t, f, instanceID)
			case "run":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET process_reconciled_at=now() WHERE id=$1`, work.LeaseID)
			case "command":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET process_reconciled_at=now() WHERE computer_id=$1`, computerID)
			}
			if row, err := q.CompleteWorkerDrain(t.Context(), params); err != nil || row.Status != db.WorkerHostStatusTerminationReady {
				t.Fatalf("reconciled drain=%+v err=%v", row, err)
			}
		})
	}
}
