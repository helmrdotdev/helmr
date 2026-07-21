package db

import (
	"context"
	"errors"
	"testing"
	"time"

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
	lease, err := queries.MarkFreshRunLeaseRunning(ctx, fixture.freshRunLeaseRunningParams(work, locators))
	if err != nil {
		t.Fatal(err)
	}
	run, err := queries.MarkFreshRunRunning(ctx, MarkFreshRunRunningParams{
		ID: workUUID(work.runID), OrgID: workUUID(fixture.orgID),
		ProjectID: workUUID(fixture.projectID), EnvironmentID: workUUID(fixture.environmentID),
		WorkspaceID: locators.WorkspaceID, ExpectedStateVersion: 1,
		AttemptNumber: locators.AttemptNumber, RunLeaseID: workUUID(work.leaseID),
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := queries.TouchFreshRunWorkspace(ctx, TouchFreshRunWorkspaceParams{
		ID: locators.WorkspaceID, OrgID: workUUID(fixture.orgID),
		ProjectID: workUUID(fixture.projectID), EnvironmentID: workUUID(fixture.environmentID),
		OwnershipGeneration: 1, WriterGeneration: 1, RunID: workUUID(work.runID),
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
	if _, err := fixture.queries.MarkFreshRunLeaseRunning(
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
		`UPDATE run_leases SET expires_at = now() - interval '1 second' WHERE id = $1`,
		work.leaseID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.GetFreshRunLeaseStartLocators(ctx, GetFreshRunLeaseStartLocatorsParams{
		ID: workUUID(work.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: workUUID(fixture.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired replay error = %v, want no rows", err)
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
	if _, err := queries.MarkFreshRunLeaseRunning(ctx, fixture.freshRunLeaseRunningParams(work, locators)); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkFreshRunRunning(ctx, MarkFreshRunRunningParams{
		ID: workUUID(work.runID), OrgID: workUUID(fixture.orgID),
		ProjectID: workUUID(fixture.projectID), EnvironmentID: workUUID(fixture.environmentID),
		WorkspaceID: locators.WorkspaceID, ExpectedStateVersion: 1,
		AttemptNumber: locators.AttemptNumber, RunLeaseID: workUUID(work.leaseID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.TouchFreshRunWorkspace(ctx, TouchFreshRunWorkspaceParams{
		ID: locators.WorkspaceID, OrgID: workUUID(fixture.orgID),
		ProjectID: workUUID(fixture.projectID), EnvironmentID: workUUID(fixture.environmentID),
		OwnershipGeneration: 1, WriterGeneration: 2, RunID: workUUID(work.runID),
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

func (fixture runLeaseClaimFixture) freshRunStartLocators(
	t *testing.T,
	ctx context.Context,
	work runLeaseWork,
) GetFreshRunLeaseStartLocatorsRow {
	t.Helper()
	locators, err := fixture.queries.GetFreshRunLeaseStartLocators(ctx, GetFreshRunLeaseStartLocatorsParams{
		ID: workUUID(work.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: workUUID(fixture.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	})
	if err != nil {
		t.Fatal(err)
	}
	return locators
}

func (fixture runLeaseClaimFixture) freshRunLeaseRunningParams(
	work runLeaseWork,
	locators GetFreshRunLeaseStartLocatorsRow,
) MarkFreshRunLeaseRunningParams {
	return MarkFreshRunLeaseRunningParams{
		ID: workUUID(work.leaseID), RunID: workUUID(work.runID),
		WorkspaceID: locators.WorkspaceID, AttemptNumber: locators.AttemptNumber,
		LeaseSequence: 1, WorkerGroupID: runLeaseTestWorkerGroup,
		WorkerInstanceID: workUUID(fixture.workerID), WorkerEpoch: 1,
		WorkerProtocolVersion: runLeaseTestProtocol, RuntimeInstanceID: locators.RuntimeInstanceID,
		NetworkSlotID: locators.NetworkSlotID, NetworkSlotGeneration: locators.NetworkSlotGeneration,
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
