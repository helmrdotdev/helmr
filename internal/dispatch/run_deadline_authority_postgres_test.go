package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func deadlineRunFixture(t *testing.T, kind string) (runPlacementFixture, ReadyRunCandidate, pgtype.UUID) {
	t.Helper()
	f := newRunPlacementFixture(t)
	candidate := ReadyRunCandidate{OrgID: pgvalue.UUID(f.orgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: 1}
	var checkpoint pgtype.UUID
	if kind != "queue" {
		_, _, id := prepareSuspendedRestore(t, f, kind == "actor")
		checkpoint = pgvalue.UUID(id)
		candidate.ExpectedRunRevision = 3
	}
	return f, candidate, checkpoint
}

func armRunDeadline(t *testing.T, f runPlacementFixture, checkpoint pgtype.UUID) time.Time {
	t.Helper()
	var deadline time.Time
	query := `UPDATE runs SET queued_expires_at=clock_timestamp()+interval '5 seconds' WHERE id=$1 RETURNING queued_expires_at`
	id := pgvalue.UUID(f.runID)
	if checkpoint.Valid {
		query = `UPDATE run_checkpoints SET expires_at=clock_timestamp()+interval '5 seconds' WHERE id=$1 RETURNING expires_at`
		id = checkpoint
	}
	if err := f.pool.QueryRow(f.ctx, query, id).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	return deadline
}

func waitDatabaseDeadline(t *testing.T, ctx context.Context, f runPlacementFixture, deadline time.Time) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var expired bool
		if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp() >= $1::timestamptz`, deadline).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestRunGrantRejectsDeadlineAfterMountLockWait(t *testing.T) {
	for _, kind := range []string{"queue", "task", "actor"} {
		t.Run(kind, func(t *testing.T) {
			f, candidate, checkpoint := deadlineRunFixture(t, kind)
			ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
			defer cancel()
			reserved, err := f.authority.prepareRunWorkspace(ctx, candidate)
			if err != nil {
				t.Fatal(err)
			}
			markRunPlacementRuntimeReady(t, f, reserved.runtimeID)
			mount, err := f.authority.prepareRunWorkspace(ctx, candidate)
			if err != nil {
				t.Fatal(err)
			}
			markRunPlacementMountReady(t, f, mount.id)
			var beforeLeases, beforeWriter int64
			if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM run_leases WHERE run_id=r.id),w.writer_generation FROM runs r JOIN workspaces w ON w.id=r.workspace_id WHERE r.id=$1`, f.runID).Scan(&beforeLeases, &beforeWriter); err != nil {
				t.Fatal(err)
			}
			deadline := armRunDeadline(t, f, checkpoint)
			blocker, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			var id pgtype.UUID
			if err := blocker.QueryRow(ctx, `SELECT id FROM workspace_mounts WHERE id=$1 FOR UPDATE`, mount.id).Scan(&id); err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() { _, err := f.authority.grantFreshRun(ctx, candidate, mount); finished <- err }()
			waitForBlockedQuery(t, f, "FROM workspace_mounts", 1)
			waitDatabaseDeadline(t, ctx, f, deadline)
			if err := blocker.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-finished:
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("expired grant: %v", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var afterLeases, afterWriter int64
			var lease pgtype.UUID
			if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM run_leases WHERE run_id=r.id),w.writer_generation,r.current_run_lease_id FROM runs r JOIN workspaces w ON w.id=r.workspace_id WHERE r.id=$1`, f.runID).Scan(&afterLeases, &afterWriter, &lease); err != nil {
				t.Fatal(err)
			}
			if afterLeases != beforeLeases || afterWriter != beforeWriter || lease.Valid {
				t.Fatalf("rejected grant left writes: leases %d->%d writer %d->%d current=%v", beforeLeases, afterLeases, beforeWriter, afterWriter, lease)
			}
			runtime, err := runtimeDeadlineState(f, reserved.runtimeID)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.ReservedRunID != candidate.RunID || !runtime.ReservationExpiresAt.Valid {
				t.Fatal("rejected grant consumed its reservation")
			}
		})
	}
}

func TestRunPreparationRejectsDeadlineAfterWorkerLockWait(t *testing.T) {
	for _, kind := range []string{"queue", "task"} {
		t.Run(kind, func(t *testing.T) {
			f, candidate, checkpoint := deadlineRunFixture(t, kind)
			ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
			defer cancel()
			var before int
			if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime_instances`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			deadline := armRunDeadline(t, f, checkpoint)
			blocker, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			var id pgtype.UUID
			if err := blocker.QueryRow(ctx, `SELECT id FROM worker_instances WHERE id=$1 FOR UPDATE`, f.workerID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() { _, err := f.authority.prepareRunWorkspace(ctx, candidate); finished <- err }()
			waitForBlockedQuery(t, f, "SELECT worker_instances.id", 1)
			waitDatabaseDeadline(t, ctx, f, deadline)
			if err := blocker.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-finished:
				if !errors.Is(err, ErrCandidateChanged) {
					t.Fatalf("expired preparation: %v", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var after int
			if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime_instances`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("rejected preparation allocated Runtime: %d -> %d", before, after)
			}
		})
	}
}

func TestRuntimeReadyRejectsCheckpointExpiryDuringRestoreLockWait(t *testing.T) {
	for _, kind := range []string{"task", "actor"} {
		t.Run(kind, func(t *testing.T) {
			f, candidate, checkpoint := deadlineRunFixture(t, kind)
			ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
			defer cancel()
			reserved, err := f.authority.prepareRunWorkspace(ctx, candidate)
			if err != nil {
				t.Fatal(err)
			}
			params := runPlacementRuntimeReadyParams(t, f, reserved.runtimeID)
			deadline := armRunDeadline(t, f, checkpoint)
			blocker, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			var id pgtype.UUID
			if err := blocker.QueryRow(ctx, `SELECT id FROM run_checkpoints WHERE id=$1 FOR UPDATE`, checkpoint).Scan(&id); err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() { _, err := db.New(f.pool).MarkRuntimeInstanceReady(ctx, params); finished <- err }()
			waitForBlockedQuery(t, f, "MarkRuntimeInstanceReady", 1)
			waitDatabaseDeadline(t, ctx, f, deadline)
			if err := blocker.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-finished:
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("expired checkpoint acknowledged: %v", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			state, err := runtimeDeadlineState(f, reserved.runtimeID)
			if err != nil {
				t.Fatal(err)
			}
			if state.ObservedState != "allocated" || state.ReadyAt.Valid || state.ReservationExpiresAt.Valid {
				t.Fatalf("rejected readiness left state: %+v", state)
			}
		})
	}
}

func TestPreparationDeadlineKeepsAlreadyStartedRunEligible(t *testing.T) {
	f, candidate, _ := deadlineRunFixture(t, "queue")
	// Queue expiry limits waiting for the first lease, not a previously started
	// Run's continuation. Check this existing distinction at the new final gate.
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runs SET first_lease_at=clock_timestamp(),queued_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, f.runID)
	if _, err := f.authority.prepareRunWorkspace(f.ctx, candidate); err != nil {
		t.Fatal(err)
	}
}

// Extend the existing same-Computer lifecycle fixture: after this rejected grant,
// that test restores the parent's no-expiry policy and completes child execution.
func assertExpiredParentCheckpointRejectsChild(t *testing.T, f runPlacementFixture, candidate ReadyRunCandidate, placement ReadyRunPlacement, checkpoint pgtype.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	deadline := armRunDeadline(t, f, checkpoint)
	blocker, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	var id pgtype.UUID
	if err := blocker.QueryRow(ctx, `SELECT id FROM workspace_mounts WHERE id=$1 FOR UPDATE`, placement.WorkspaceMountID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := f.authority.grantFreshRun(ctx, candidate, runWorkspaceMount{id: placement.WorkspaceMountID, workerID: placement.WorkerInstanceID, epoch: placement.WorkerEpoch, runtimeID: placement.RuntimeInstanceID})
		finished <- err
	}()
	waitForBlockedQuery(t, f, "FROM workspace_mounts", 1)
	waitDatabaseDeadline(t, ctx, f, deadline)
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("child grant after parent checkpoint expiry: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var leases int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM run_leases WHERE run_id=$1`, candidate.RunID).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatal("expired parent checkpoint left a child lease")
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE run_checkpoints SET expires_at=NULL WHERE id=$1`, checkpoint)
}
