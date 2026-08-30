package db

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestFreshRunStartQueriesCommitAndReplay(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	locators := fixture.freshRunStartLocators(t, ctx, work)

	var originalWorkspaceActivity time.Time
	if err := fixture.pool.QueryRow(ctx,
		`SELECT last_activity_at FROM workspaces WHERE id = $1`,
		locators.WorkspaceID,
	).Scan(&originalWorkspaceActivity); err != nil {
		t.Fatal(err)
	}

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queries := New(tx)
	lease, err := queries.MarkRunLeaseRunning(ctx, fixture.freshRunLeaseRunningParams(work, locators))
	if err != nil {
		t.Fatal(err)
	}
	run, err := queries.MarkRunRunning(ctx, MarkRunRunningParams{
		ID: workUUID(work.runID), OrgID: workUUID(fixture.orgID),
		ProjectID: workUUID(fixture.projectID), EnvironmentID: workUUID(fixture.environmentID),
		WorkspaceID: locators.WorkspaceID, ExpectedStateVersion: 1,
		AttemptNumber: locators.AttemptNumber, RunLeaseID: workUUID(work.leaseID),
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := queries.TouchRunWorkspaceActivity(ctx, TouchRunWorkspaceActivityParams{
		ID: locators.WorkspaceID, OrgID: workUUID(fixture.orgID),
		ProjectID: workUUID(fixture.projectID), EnvironmentID: workUUID(fixture.environmentID),
		OwnershipGeneration: 1, WriterGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if lease.State != RunLeaseStateRunning || !lease.StartedAt.Valid {
		t.Fatalf("lease = state %s started_at %v", lease.State, lease.StartedAt)
	}
	if run.Status != RunStatusRunning || !run.StartedAt.Valid || !run.ActiveStartedAt.Valid {
		t.Fatalf("run = status %s started_at %v active_started_at %v", run.Status, run.StartedAt, run.ActiveStartedAt)
	}
	if !workspace.LastActivityAt.Time.After(originalWorkspaceActivity) {
		t.Fatalf("Workspace activity = %s, want after %s", workspace.LastActivityAt.Time, originalWorkspaceActivity)
	}

	if _, err := fixture.pool.Exec(ctx,
		`UPDATE run_leases SET start_deadline_at = now() - interval '1 second' WHERE id = $1`,
		work.leaseID,
	); err != nil {
		t.Fatal(err)
	}
	beforeReplay := fixture.freshRunStartState(t, ctx, work)
	replay := fixture.freshRunStartLocators(t, ctx, work)
	if replay.RunID != locators.RunID {
		t.Fatalf("replay Run = %s, want %s", pgvalue.UUIDString(replay.RunID), pgvalue.UUIDString(locators.RunID))
	}
	if _, err := fixture.queries.MarkRunLeaseRunning(
		ctx,
		fixture.freshRunLeaseRunningParams(work, replay),
	); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second transition error = %v, want no rows", err)
	}
	afterReplay := fixture.freshRunStartState(t, ctx, work)
	if afterReplay != beforeReplay {
		t.Fatalf("replay changed state: before=%+v after=%+v", beforeReplay, afterReplay)
	}
	if _, err := fixture.pool.Exec(ctx,
		`UPDATE run_leases
		    SET assigned_at = now() - interval '3 minutes',
		        start_deadline_at = now() - interval '2 minutes',
		        claimed_at = now() - interval '2 minutes',
		        started_at = now() - interval '2 minutes',
		        expires_at = now() - interval '1 second'
		  WHERE id = $1`,
		work.leaseID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.GetRunLeaseStartLocators(ctx, GetRunLeaseStartLocatorsParams{
		ID: workUUID(work.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: workUUID(fixture.workerID),
		WorkerEpoch: 1}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired replay error = %v, want no rows", err)
	}
}

func TestCloseRunActiveIntervalForCheckpointUsesDatabaseTimeAndExactFence(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	locators := fixture.freshRunStartLocators(t, ctx, work)
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'waiting',
		       current_run_lease_id = $1,
		       active_elapsed_ms = 250,
		       active_started_at = transaction_timestamp() - interval '2 seconds'
		 WHERE id = $2
	`, work.leaseID, work.runID); err != nil {
		t.Fatal(err)
	}
	params := CloseRunActiveIntervalForCheckpointParams{
		ID: workUUID(work.runID), OrgID: workUUID(fixture.orgID),
		ProjectID: workUUID(fixture.projectID), EnvironmentID: workUUID(fixture.environmentID),
		WorkspaceID: locators.WorkspaceID, AttemptNumber: locators.AttemptNumber,
		RunLeaseID: workUUID(work.leaseID),
	}
	elapsed, err := fixture.queries.CloseRunActiveIntervalForCheckpoint(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed < 2_000 || elapsed > 5_000 {
		t.Fatalf("active elapsed = %dms, want database-derived interval near 2250ms", elapsed)
	}
	var activeStartedAt pgtype.Timestamptz
	if err := fixture.pool.QueryRow(ctx,
		`SELECT active_started_at FROM runs WHERE id = $1`, work.runID,
	).Scan(&activeStartedAt); err != nil {
		t.Fatal(err)
	}
	if activeStartedAt.Valid {
		t.Fatalf("active_started_at remained open: %v", activeStartedAt)
	}
	if _, err := fixture.queries.CloseRunActiveIntervalForCheckpoint(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("replay error = %v, want no rows", err)
	}
}

func TestFreshRunStartQueriesRollbackTogether(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	locators := fixture.freshRunStartLocators(t, ctx, work)

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queries := New(tx)
	if _, err := queries.MarkRunLeaseRunning(ctx, fixture.freshRunLeaseRunningParams(work, locators)); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkRunRunning(ctx, MarkRunRunningParams{
		ID: workUUID(work.runID), OrgID: workUUID(fixture.orgID),
		ProjectID: workUUID(fixture.projectID), EnvironmentID: workUUID(fixture.environmentID),
		WorkspaceID: locators.WorkspaceID, ExpectedStateVersion: 1,
		AttemptNumber: locators.AttemptNumber, RunLeaseID: workUUID(work.leaseID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.TouchRunWorkspaceActivity(ctx, TouchRunWorkspaceActivityParams{
		ID: locators.WorkspaceID, OrgID: workUUID(fixture.orgID),
		ProjectID: workUUID(fixture.projectID), EnvironmentID: workUUID(fixture.environmentID),
		OwnershipGeneration: 1, WriterGeneration: 2,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mismatched Workspace fence error = %v, want no rows", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	var leaseState RunLeaseState
	var runStatus RunStatus
	var leaseStartedAt, runStartedAt, activeStartedAt *time.Time
	if err := fixture.pool.QueryRow(ctx, `
		SELECT run_leases.state, runs.status, run_leases.started_at,
		       runs.started_at, runs.active_started_at
		  FROM run_leases
		  JOIN runs ON runs.id = run_leases.run_id
		 WHERE run_leases.id = $1
	`, work.leaseID).Scan(
		&leaseState,
		&runStatus,
		&leaseStartedAt,
		&runStartedAt,
		&activeStartedAt,
	); err != nil {
		t.Fatal(err)
	}
	if leaseState != RunLeaseStateStarting || runStatus != RunStatusQueued ||
		leaseStartedAt != nil || runStartedAt != nil || activeStartedAt != nil {
		t.Fatalf(
			"rollback left lease=%s run=%s lease_started=%v run_started=%v active_started=%v",
			leaseState,
			runStatus,
			leaseStartedAt,
			runStartedAt,
			activeStartedAt,
		)
	}
}

func TestRunEntrypointQueriesCommitOnceAndRejectExpiredLease(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	start := fixture.freshRunStartLocators(t, ctx, work)
	if _, err := fixture.queries.GetRunEntrypointLocators(ctx, fixture.runEntrypointLocatorParams(work)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("pre-start entrypoint error = %v, want no rows", err)
	}

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queries := New(tx)
	if _, err := queries.MarkRunLeaseRunning(ctx, fixture.freshRunLeaseRunningParams(work, start)); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkRunRunning(ctx, MarkRunRunningParams{
		ID: workUUID(work.runID), OrgID: workUUID(fixture.orgID),
		ProjectID: workUUID(fixture.projectID), EnvironmentID: workUUID(fixture.environmentID),
		WorkspaceID: start.WorkspaceID, ExpectedStateVersion: 1,
		AttemptNumber: start.AttemptNumber, RunLeaseID: workUUID(work.leaseID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.TouchRunWorkspaceActivity(ctx, TouchRunWorkspaceActivityParams{
		ID: start.WorkspaceID, OrgID: workUUID(fixture.orgID),
		ProjectID: workUUID(fixture.projectID), EnvironmentID: workUUID(fixture.environmentID),
		OwnershipGeneration: 1, WriterGeneration: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	locators := fixture.runEntrypointLocators(t, ctx, work)
	attempt, err := fixture.queries.MarkRunEntrypointEntered(ctx, MarkRunEntrypointEnteredParams{
		RunID:       locators.RunID,
		Number:      locators.AttemptNumber,
		WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !attempt.EntrypointEnteredAt.Valid {
		t.Fatal("entrypoint_entered_at was not set")
	}
	enteredAt := attempt.EntrypointEnteredAt.Time

	if _, err := fixture.queries.MarkRunEntrypointEntered(ctx, MarkRunEntrypointEnteredParams{
		RunID:       locators.RunID,
		Number:      locators.AttemptNumber,
		WorkspaceID: locators.WorkspaceID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("replay mutation error = %v, want no rows", err)
	}
	state := fixture.freshRunStartState(t, ctx, work)
	if !state.EntrypointEnteredAt.Valid || !state.EntrypointEnteredAt.Time.Equal(enteredAt) {
		t.Fatalf("replay changed entrypoint timestamp: got %v want %v", state.EntrypointEnteredAt, enteredAt)
	}
	if replay := fixture.runEntrypointLocators(t, ctx, work); replay.RunID != locators.RunID {
		t.Fatalf("replay Run = %s, want %s", pgvalue.UUIDString(replay.RunID), pgvalue.UUIDString(locators.RunID))
	}

	waitID := pgvalue.UUID(uuid.NewV7())
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO run_waits (
			id, environment_id, run_id, workspace_id, kind, condition_state,
			due_at, suspension_state, expected_run_state_version, attempt_number,
			current_run_lease_id, resume_attach_id
		) VALUES (
			$1, $2, $3, $4, 'timer', 'pending',
			now() + interval '1 minute', 'hot', 2, $5, $6, $7
		)
	`, waitID, workUUID(fixture.environmentID), locators.RunID, locators.WorkspaceID,
		locators.AttemptNumber, workUUID(work.leaseID), pgvalue.UUID(uuid.NewV7()),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.GetRunEntrypointLocators(ctx, fixture.runEntrypointLocatorParams(work)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("post-entry Wait error = %v, want no rows", err)
	}
	if _, err := fixture.pool.Exec(ctx, `DELETE FROM run_waits WHERE id = $1`, waitID); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.pool.Exec(ctx,
		`UPDATE run_leases
		    SET assigned_at = now() - interval '3 minutes',
		        start_deadline_at = now() - interval '2 minutes',
		        claimed_at = now() - interval '2 minutes',
		        started_at = now() - interval '2 minutes',
		        expires_at = now() - interval '1 second'
		  WHERE id = $1`,
		work.leaseID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.GetRunEntrypointLocators(ctx, fixture.runEntrypointLocatorParams(work)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired entrypoint error = %v, want no rows", err)
	}
}

func (fixture runLeaseClaimFixture) freshRunStartLocators(
	t *testing.T,
	ctx context.Context,
	work runLeaseWork,
) GetRunLeaseStartLocatorsRow {
	t.Helper()
	locators, err := fixture.queries.GetRunLeaseStartLocators(ctx, GetRunLeaseStartLocatorsParams{
		ID: workUUID(work.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: workUUID(fixture.workerID),
		WorkerEpoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	return locators
}

func (fixture runLeaseClaimFixture) runEntrypointLocators(
	t *testing.T,
	ctx context.Context,
	work runLeaseWork,
) GetRunEntrypointLocatorsRow {
	t.Helper()
	locators, err := fixture.queries.GetRunEntrypointLocators(ctx, fixture.runEntrypointLocatorParams(work))
	if err != nil {
		t.Fatal(err)
	}
	return locators
}

func (fixture runLeaseClaimFixture) runEntrypointLocatorParams(
	work runLeaseWork,
) GetRunEntrypointLocatorsParams {
	return GetRunEntrypointLocatorsParams{
		ID: workUUID(work.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: workUUID(fixture.workerID),
		WorkerEpoch: 1}
}

func (fixture runLeaseClaimFixture) freshRunLeaseRunningParams(
	work runLeaseWork,
	locators GetRunLeaseStartLocatorsRow,
) MarkRunLeaseRunningParams {
	return MarkRunLeaseRunningParams{
		ID: workUUID(work.leaseID), RunID: workUUID(work.runID),
		WorkspaceID: locators.WorkspaceID, AttemptNumber: locators.AttemptNumber,
		LeaseSequence: 1, WorkerGroupID: runLeaseTestWorkerGroup,
		WorkerInstanceID: workUUID(fixture.workerID), WorkerEpoch: 1,
		RuntimeInstanceID: locators.RuntimeInstanceID,
		RuntimeIdentityID: fixture.runtimeIdentityID,
	}
}

func workUUID(value [16]byte) pgtype.UUID {
	return pgvalue.UUID(value)
}

type freshRunStartState struct {
	LeaseState            RunLeaseState
	RunStatus             RunStatus
	LeaseStartedAt        pgtype.Timestamptz
	RunStartedAt          pgtype.Timestamptz
	ActiveStartedAt       pgtype.Timestamptz
	StateVersion          int64
	ActiveElapsedMs       int64
	WorkspaceLastActivity pgtype.Timestamptz
	EntrypointEnteredAt   pgtype.Timestamptz
}

func (fixture runLeaseClaimFixture) freshRunStartState(
	t *testing.T,
	ctx context.Context,
	work runLeaseWork,
) freshRunStartState {
	t.Helper()
	var state freshRunStartState
	if err := fixture.pool.QueryRow(ctx, `
		SELECT run_leases.state,
		       runs.status,
		       run_leases.started_at,
		       runs.started_at,
		       runs.active_started_at,
		       runs.state_version,
		       runs.active_elapsed_ms,
		       workspaces.last_activity_at,
		       run_attempts.entrypoint_entered_at
		  FROM run_leases
		  JOIN runs ON runs.id = run_leases.run_id
		  JOIN workspaces ON workspaces.id = runs.workspace_id
		  JOIN run_attempts
		    ON run_attempts.run_id = runs.id
		   AND run_attempts.number = run_leases.attempt_number
		 WHERE run_leases.id = $1
	`, work.leaseID).Scan(
		&state.LeaseState,
		&state.RunStatus,
		&state.LeaseStartedAt,
		&state.RunStartedAt,
		&state.ActiveStartedAt,
		&state.StateVersion,
		&state.ActiveElapsedMs,
		&state.WorkspaceLastActivity,
		&state.EntrypointEnteredAt,
	); err != nil {
		t.Fatal(err)
	}
	return state
}
