package dispatch

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRuntimeDeadlinePhasesAndCleanupBarrier(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprintf("ready=%t", ready), func(t *testing.T) {
			fixture := newRunPlacementFixture(t)
			candidate := ReadyRunCandidate{OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID), ExpectedRunRevision: 1}
			reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate)
			if err != nil {
				t.Fatal(err)
			}
			q := db.New(fixture.pool)
			initial, err := runtimeDeadlineState(fixture, reserved.RuntimeInstanceID)
			if err != nil {
				t.Fatal(err)
			}
			if initial.ReservationExpiresAt.Valid {
				t.Fatal("execution reservation began before preparation finished")
			}
			column := "preparation_expires_at"
			wantCount := 1
			if ready {
				markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
				column = "reservation_expires_at"
				wantCount = 0
			}
			dbtest.MustExec(t, fixture.ctx, fixture.pool, "UPDATE runtime_instances SET "+column+" = transaction_timestamp() - interval '1 second' WHERE id=$1", reserved.RuntimeInstanceID)
			for i, want := range []int{1, 0} {
				changed, err := fixture.authority.RecoverExpiredRuntimeReservations(fixture.ctx, 10)
				if err != nil || changed != want {
					t.Fatalf("sweep %d: changed=%d error=%v", i, changed, err)
				}
			}
			closed, err := runtimeDeadlineState(fixture, reserved.RuntimeInstanceID)
			if err != nil {
				t.Fatal(err)
			}
			if closed.DesiredVersion != initial.DesiredVersion+1 || closed.ReclaimedAt.Valid {
				t.Fatalf("close fence: %+v", closed)
			}
			var failures int
			if err := fixture.pool.QueryRow(fixture.ctx, "SELECT runtime_preparation_count FROM runs WHERE id=$1", fixture.runID).Scan(&failures); err != nil {
				t.Fatal(err)
			}
			if failures != wantCount {
				t.Fatalf("preparation failures=%d want=%d", failures, wantCount)
			}
			// Remove only backoff from the fixture; physical exclusion must still block a new VM.
			dbtest.MustExec(t, fixture.ctx, fixture.pool, "UPDATE runs SET next_runtime_preparation_at=NULL WHERE id=$1", fixture.runID)
			if placement, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate); err == nil && placement.RuntimeInstanceID != reserved.RuntimeInstanceID {
				t.Fatalf("replacement before cleanup: %+v", placement)
			}
			_, err = q.MarkRuntimeInstanceClosed(fixture.ctx, db.MarkRuntimeInstanceClosedParams{
				ID: closed.ID, WorkerInstanceID: closed.WorkerInstanceID, WorkerEpoch: closed.WorkerEpoch,
				DesiredVersion: closed.DesiredVersion, ExpectedObservedVersion: closed.ObservedVersion,
				ReasonCode: pgvalue.Text("deadline"), CleanupProof: []byte(`{"method":"host_reconciled"}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			placement, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate)
			if err != nil || !placement.RuntimeInstanceID.Valid || placement.RuntimeInstanceID == reserved.RuntimeInstanceID {
				t.Fatalf("replacement after simulated cleanup proof: %+v error=%v", placement, err)
			}
		})
	}
}

func TestRuntimeReadyDeadlineAndReplay(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	candidate := ReadyRunCandidate{OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID), ExpectedRunRevision: 1}
	reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	// A nearly exhausted preparation must still receive a full execution reservation.
	dbtest.MustExec(t, fixture.ctx, fixture.pool, "UPDATE runtime_instances SET preparation_expires_at=transaction_timestamp()+interval '10 seconds' WHERE id=$1", reserved.RuntimeInstanceID)
	markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
	q := db.New(fixture.pool)
	ready, err := runtimeDeadlineState(fixture, reserved.RuntimeInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.ReservationExpiresAt.Time.Sub(ready.ReadyAt.Time) != 5*time.Minute {
		t.Fatal("ready did not start full reservation")
	}
	_, err = q.MarkRuntimeInstanceReady(fixture.ctx, db.MarkRuntimeInstanceReadyParams{ReservationSeconds: 300,
		ID: ready.ID, WorkerInstanceID: ready.WorkerInstanceID, WorkerEpoch: ready.WorkerEpoch, DesiredVersion: ready.DesiredVersion,
		ExpectedObservedVersion: ready.ObservedVersion, VMVCPUCount: ready.VMVCPUCount, CPUConfigDigest: ready.CPUConfigDigest})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("duplicate ready=%v", err)
	}
	after, err := runtimeDeadlineState(fixture, ready.ID)
	if err != nil || after.ReservationExpiresAt != ready.ReservationExpiresAt {
		t.Fatalf("duplicate changed expiry: %v", err)
	}
}

func runtimeDeadlineState(fixture runPlacementFixture, id pgtype.UUID) (db.RuntimeInstance, error) {
	rows, err := fixture.pool.Query(fixture.ctx, "SELECT * FROM runtime_instances WHERE id=$1", id)
	if err != nil {
		return db.RuntimeInstance{}, err
	}
	return pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[db.RuntimeInstance])
}

func TestExpiredPreparationRejectsReadyAndExhaustsBudget(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	candidate := ReadyRunCandidate{OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID), ExpectedRunRevision: 1}
	reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, "UPDATE runs SET runtime_preparation_count=7 WHERE id=$1", fixture.runID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, "UPDATE runtime_instances SET preparation_expires_at=now()-interval '1 second' WHERE id=$1", reserved.RuntimeInstanceID)
	if err := markRunPlacementRuntimeReadyQuery(t, fixture, reserved.RuntimeInstanceID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("late ready: %v", err)
	}
	if n, err := fixture.authority.RecoverExpiredRuntimeReservations(fixture.ctx, 10); err != nil || n != 1 {
		t.Fatalf("expiry: count=%d error=%v", n, err)
	}
	var status string
	var failures int
	if err := fixture.pool.QueryRow(fixture.ctx, "SELECT status,runtime_preparation_count FROM runs WHERE id=$1", fixture.runID).Scan(&status, &failures); err != nil {
		t.Fatal(err)
	}
	if status != "system_failed" || failures != 8 {
		t.Fatalf("exhaustion: %s/%d", status, failures)
	}
}

func TestExpiredExecReservationClosesWithoutPendingCandidate(t *testing.T) {
	fixture, processID, mountID := prepareClaimableWorkspaceExecMount(t)
	q := db.New(fixture.pool)
	if _, err := q.FailPendingWorkspaceExecProcess(fixture.ctx, db.FailPendingWorkspaceExecProcessParams{
		OrgID: pgvalue.UUID(fixture.orgID), ProcessID: pgvalue.UUID(processID), ExpectedRevision: 1,
		ReasonCode: pgvalue.Text("test_expiry"), Error: []byte(`{"code":"test_expiry"}`),
	}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `UPDATE runtime_instances SET reservation_expires_at=now()-interval '1 second' WHERE id=(SELECT runtime_instance_id FROM workspace_mounts WHERE id=$1)`, mountID)
	if n, err := fixture.authority.RecoverExpiredRuntimeReservations(fixture.ctx, 10); err != nil || n != 1 {
		t.Fatalf("exec recovery: %d/%v", n, err)
	}
	var state, kind, reason string
	if err := fixture.pool.QueryRow(fixture.ctx, "SELECT status,finalization_action,finalization_reason_code FROM workspace_mounts WHERE id=$1", mountID).Scan(&state, &kind, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "unmounting" || kind != "discard" || reason != "runtime_reservation_expired" {
		t.Fatalf("exec close mount: %s/%s/%s", state, kind, reason)
	}
}

func TestExpiredRestorePreparationRetainsPendingWait(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	_, waitID, _ := prepareActorSuspendedRestore(t, fixture)
	reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, ReadyRunCandidate{OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID), ExpectedRunRevision: 3})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, "UPDATE runtime_instances SET preparation_expires_at=now()-interval '1 second' WHERE id=$1", reserved.RuntimeInstanceID)
	if n, err := fixture.authority.RecoverExpiredRuntimeReservations(fixture.ctx, 10); err != nil || n != 1 {
		t.Fatalf("restore recovery: %d/%v", n, err)
	}
	var state string
	if err := fixture.pool.QueryRow(fixture.ctx, "SELECT suspension_status FROM run_waits WHERE id=$1", waitID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "resume_pending" {
		t.Fatalf("wait=%s", state)
	}
}

func TestRuntimeExpirySerializesWithCancellationAndWorkerLoss(t *testing.T) {
	for _, action := range []string{"cancel", "worker loss"} {
		t.Run(action, func(t *testing.T) {
			fixture := newRunPlacementFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			reserved, err := fixture.authority.PlaceReadyRun(ctx, ReadyRunCandidate{OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID), ExpectedRunRevision: 1})
			if err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, ctx, fixture.pool, "UPDATE runtime_instances SET preparation_expires_at=now()-interval '1 second' WHERE id=$1", reserved.RuntimeInstanceID)
			before, err := runtimeDeadlineState(fixture, reserved.RuntimeInstanceID)
			if err != nil {
				t.Fatal(err)
			}
			var claimVersion int64
			if err := fixture.pool.QueryRow(ctx, "SELECT claim_version FROM worker_instances WHERE id=$1", fixture.workerID).Scan(&claimVersion); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			results := make(chan error, 3)
			for range 2 {
				go func() {
					<-start
					_, err := fixture.authority.RecoverExpiredRuntimeReservations(ctx, 10)
					results <- err
				}()
			}
			go func() {
				<-start
				if action == "cancel" {
					c, err := run.NewCanceler(fixture.pool)
					if err == nil {
						_, err = c.Cancel(ctx, run.CancellationRequest{OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID, RunID: fixture.runID})
					}
					results <- err
				} else {
					_, err := db.New(fixture.pool).FenceWorkerInstance(ctx, db.FenceWorkerInstanceParams{
						ID: pgvalue.UUID(fixture.workerID), WorkerGroupID: fixture.groupID, ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: claimVersion, ReasonCode: pgvalue.Text("test_loss"),
					})
					results <- err
				}
			}()
			close(start)
			for range 3 {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			after, err := runtimeDeadlineState(fixture, reserved.RuntimeInstanceID)
			if err != nil {
				t.Fatal(err)
			}
			if after.ReclaimedAt.Valid || after.DesiredVersion > before.DesiredVersion+1 {
				t.Fatalf("invalid revocation: %+v", after)
			}
			var failures int
			if err := fixture.pool.QueryRow(ctx, "SELECT runtime_preparation_count FROM runs WHERE id=$1", fixture.runID).Scan(&failures); err != nil {
				t.Fatal(err)
			}
			if failures > 1 {
				t.Fatalf("duplicate failure charges=%d", failures)
			}
			if action == "cancel" && after.DesiredState != db.RuntimeDesiredStateClosed {
				t.Fatal("cancel did not close")
			}
			if action == "worker loss" && after.ObservedState != db.RuntimeObservedStateLost {
				t.Fatal("worker loss did not fence runtime")
			}
		})
	}
}

func TestExpiredAllocatedExecClosesWithoutRunCharge(t *testing.T) {
	fixture, _, mountID := prepareClaimableWorkspaceExecMount(t)
	// Put this fixture back at the allocated preparation boundary before expiry.
	var runtimeID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, "SELECT runtime_instance_id FROM workspace_mounts WHERE id=$1", mountID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, "DELETE FROM workspace_mounts WHERE id=$1", mountID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `UPDATE runtime_instances SET observed_state='allocated',ready_at=NULL,observed_desired_version=0,reservation_expires_at=NULL,preparation_expires_at=now()-interval '1 second' WHERE id=$1`, runtimeID)
	if n, err := fixture.authority.RecoverExpiredRuntimeReservations(fixture.ctx, 10); err != nil || n != 1 {
		t.Fatalf("allocated exec: %d/%v", n, err)
	}
	state, err := runtimeDeadlineState(fixture, runtimeID)
	if err != nil {
		t.Fatal(err)
	}
	if state.DesiredState != db.RuntimeDesiredStateClosed || state.DesiredReason != "runtime_preparation_expired" || state.ReclaimedAt.Valid {
		t.Fatalf("allocated exec close: %+v", state)
	}
}

func TestReadyRuntimeExpiryConcurrentWithWorkerLoss(t *testing.T) {
	fixture, _, mountID := prepareClaimableWorkspaceExecMount(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var runtimeID pgtype.UUID
	if err := fixture.pool.QueryRow(ctx, "SELECT runtime_instance_id FROM workspace_mounts WHERE id=$1", mountID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, mountID)
	dbtest.MustExec(t, ctx, fixture.pool, "UPDATE runtime_instances SET reservation_expires_at=now()-interval '1 second' WHERE id=$1", runtimeID)
	var claimVersion int64
	if err := fixture.pool.QueryRow(ctx, "SELECT claim_version FROM worker_instances WHERE id=$1", fixture.workerID).Scan(&claimVersion); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := fixture.authority.RecoverExpiredRuntimeReservations(ctx, 10)
		results <- err
	}()
	go func() {
		<-start
		_, err := db.New(fixture.pool).FenceWorkerInstance(ctx, db.FenceWorkerInstanceParams{
			ID: pgvalue.UUID(fixture.workerID), WorkerGroupID: fixture.groupID, ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: claimVersion, ReasonCode: pgvalue.Text("test_loss"),
		})
		results <- err
	}()
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	state, err := runtimeDeadlineState(fixture, runtimeID)
	if err != nil {
		t.Fatal(err)
	}
	if state.ObservedState != db.RuntimeObservedStateLost || state.ReclaimedAt.Valid {
		t.Fatalf("lost runtime=%+v", state)
	}
}

func TestLockedRuntimeDeadlineUsesCurrentDatabaseTime(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprintf("ready=%t", ready), func(t *testing.T) {
			f := newRunPlacementFixture(t)
			placement, err := f.authority.PlaceReadyRun(f.ctx, ReadyRunCandidate{
				OrgID: pgvalue.UUID(f.orgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			if ready {
				markRunPlacementRuntimeReady(t, f, placement.RuntimeInstanceID)
			}
			tx, err := f.pool.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			var workspaceID pgtype.UUID
			if err := tx.QueryRow(f.ctx, `SELECT workspace_id FROM runtime_instances WHERE id=$1`, placement.RuntimeInstanceID).Scan(&workspaceID); err != nil {
				t.Fatal(err)
			}
			discovered, err := discoverRunRuntime(f.ctx, tx, workspaceID)
			if err != nil || !discovered.reservationActive {
				t.Fatalf("live discovery: %+v, %v", discovered, err)
			}
			// Commit a deadline after this transaction began but before its locked
			// recheck. This reproduces time spent between discovery and locking
			// without a sleep or reliance on the application's clock.
			column := "preparation_expires_at"
			if ready {
				column = "reservation_expires_at"
			}
			dbtest.MustExec(t, f.ctx, f.pool, "UPDATE runtime_instances SET "+column+"=clock_timestamp() WHERE id=$1", placement.RuntimeInstanceID)
			locked, err := lockRunRuntime(f.ctx, tx, discovered)
			if err != nil {
				t.Fatal(err)
			}
			if locked.reservationActive {
				t.Fatal("expired runtime remained authorized using transaction-start time")
			}
		})
	}
}

func TestLockedRuntimeDeadlineExpiresWhileWaitingForUnchangedRow(t *testing.T) {
	f := newRunPlacementFixture(t)
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	placement, err := f.authority.PlaceReadyRun(ctx, ReadyRunCandidate{
		OrgID: pgvalue.UUID(f.orgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE runtime_instances SET preparation_expires_at=clock_timestamp()+interval '5 seconds' WHERE id=$1`, placement.RuntimeInstanceID)
	blocker, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	var workspaceID pgtype.UUID
	if err := blocker.QueryRow(ctx, `SELECT workspace_id FROM runtime_instances WHERE id=$1 FOR UPDATE`, placement.RuntimeInstanceID).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	discovered, err := discoverRunRuntime(ctx, tx, workspaceID)
	if err != nil || !discovered.reservationActive {
		t.Fatalf("live discovery: %+v, %v", discovered, err)
	}
	type result struct {
		runtime runRuntime
		err     error
	}
	finished := make(chan result, 1)
	go func() { r, err := lockRunRuntime(ctx, tx, discovered); finished <- result{r, err} }()
	waitForBlockedQuery(t, f, "FOR UPDATE OF runtime_instances", 1)
	// Do not change the locked row: a row update could make PostgreSQL evaluate
	// an otherwise stale SELECT-list expression again after lock acquisition.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var expired bool
		if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp() >= preparation_expires_at FROM runtime_instances WHERE id=$1`, placement.RuntimeInstanceID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-finished:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.runtime.reservationActive {
			t.Fatal("deadline was evaluated before waiting for the runtime lock")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestRuntimeReadyDecisionUsesCurrentDatabaseTime(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%t", expired), func(t *testing.T) {
			f := newRunPlacementFixture(t)
			placement, err := f.authority.PlaceReadyRun(f.ctx, ReadyRunCandidate{OrgID: pgvalue.UUID(f.orgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: 1})
			if err != nil {
				t.Fatal(err)
			}
			params := runPlacementRuntimeReadyParams(t, f, placement.RuntimeInstanceID)
			tx, err := f.pool.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			var decisionLowerBound time.Time
			if err := f.pool.QueryRow(f.ctx, `SELECT clock_timestamp()`).Scan(&decisionLowerBound); err != nil {
				t.Fatal(err)
			}
			deadline := decisionLowerBound
			if !expired {
				deadline = deadline.Add(time.Minute)
			}
			dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runtime_instances SET preparation_expires_at=$2 WHERE id=$1`, placement.RuntimeInstanceID, deadline)
			ready, err := db.New(tx).MarkRuntimeInstanceReady(f.ctx, params)
			if expired {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("expired preparation accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ready.ReadyAt.Time.Before(decisionLowerBound) || !ready.ObservedAt.Time.Equal(ready.ReadyAt.Time) || ready.ReservationExpiresAt.Time.Sub(ready.ReadyAt.Time) != 5*time.Minute {
				t.Fatalf("ready decision backdated or shortened reservation: ready=%v observed=%v expiry=%v bound=%v", ready.ReadyAt, ready.ObservedAt, ready.ReservationExpiresAt, decisionLowerBound)
			}
		})
	}
}

func TestRuntimeReservationConsumptionRechecksTime(t *testing.T) {
	for _, process := range []bool{false, true} {
		t.Run(fmt.Sprintf("process=%t", process), func(t *testing.T) {
			var f runPlacementFixture
			var runtimeID pgtype.UUID
			if process {
				var mountID pgtype.UUID
				f, _, mountID = prepareClaimableWorkspaceExecMount(t)
				if err := f.pool.QueryRow(f.ctx, `SELECT runtime_instance_id FROM workspace_mounts WHERE id=$1`, mountID).Scan(&runtimeID); err != nil {
					t.Fatal(err)
				}
			} else {
				f = newRunPlacementFixture(t)
				placement, err := f.authority.PlaceReadyRun(f.ctx, ReadyRunCandidate{OrgID: pgvalue.UUID(f.orgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: 1})
				if err != nil {
					t.Fatal(err)
				}
				runtimeID = placement.RuntimeInstanceID
				markRunPlacementRuntimeReady(t, f, runtimeID)
			}
			runtime, err := runtimeDeadlineState(f, runtimeID)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := f.pool.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			// The caller already owns the Runtime lock. Set the deadline after
			// transaction start to reproduce time passing before final consumption.
			dbtest.MustExec(t, f.ctx, tx, `UPDATE runtime_instances SET reservation_expires_at=clock_timestamp() WHERE id=$1`, runtimeID)
			var n int64
			if process {
				n, err = db.New(tx).ConsumeWorkspaceExecRuntimeReservation(f.ctx, db.ConsumeWorkspaceExecRuntimeReservationParams{ID: runtimeID, WorkspaceID: runtime.WorkspaceID, ProcessID: runtime.ReservedProcessID, BaseWorkspaceVersionID: runtime.ReservedWorkspaceVersionID})
			} else {
				n, err = db.New(tx).ConsumeRunRuntimeReservation(f.ctx, db.ConsumeRunRuntimeReservationParams{ID: runtimeID, WorkspaceID: runtime.WorkspaceID, RunID: runtime.ReservedRunID, AttemptNumber: runtime.ReservedAttemptNumber, BaseWorkspaceVersionID: runtime.ReservedWorkspaceVersionID, RestoreCheckpointID: runtime.RestoreCheckpointID})
			}
			if err != nil || n != 0 {
				t.Fatalf("expired reservation consumed: rows=%d, %v", n, err)
			}
		})
	}
}

func TestRuntimeReadyRejectsExpiryDuringRuntimeLockWait(t *testing.T) {
	f := newRunPlacementFixture(t)
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	placement, err := f.authority.PlaceReadyRun(ctx, ReadyRunCandidate{OrgID: pgvalue.UUID(f.orgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	params := runPlacementRuntimeReadyParams(t, f, placement.RuntimeInstanceID)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE runtime_instances SET preparation_expires_at=clock_timestamp()+interval '5 seconds' WHERE id=$1`, placement.RuntimeInstanceID)
	blocker, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	var id pgtype.UUID
	if err := blocker.QueryRow(ctx, `SELECT id FROM runtime_instances WHERE id=$1 FOR UPDATE`, placement.RuntimeInstanceID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { _, err := db.New(f.pool).MarkRuntimeInstanceReady(ctx, params); finished <- err }()
	waitForBlockedQuery(t, f, "MarkRuntimeInstanceReady", 1)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var expired bool
		if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp() >= preparation_expires_at FROM runtime_instances WHERE id=$1`, placement.RuntimeInstanceID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("expired preparation acknowledged: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	state, err := runtimeDeadlineState(f, placement.RuntimeInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if state.ObservedState != "allocated" || state.ReadyAt.Valid || state.ReservationExpiresAt.Valid {
		t.Fatalf("expiry changed runtime readiness: %+v", state)
	}
}
