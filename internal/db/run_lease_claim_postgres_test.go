package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	runLeaseTestRegion      = runtest.Region
	runLeaseTestWorkerGroup = runtest.WorkerGroup
)

type runLeaseClaimFixture struct {
	pool                  *pgxpool.Pool
	queries               *Queries
	orgID                 uuid.UUID
	projectID             uuid.UUID
	environmentID         uuid.UUID
	deploymentID          uuid.UUID
	taskDefinitionID      uuid.UUID
	workspaceDefinitionID uuid.UUID
	workerID              uuid.UUID
	runtimeIdentityID     string
	base                  runtest.Fixture
}

type runLeaseWork struct {
	leaseID uuid.UUID
	runID   uuid.UUID
}

func TestRunLeaseClaimReadinessFailsClosedWithoutObservation(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	if _, err := fixture.pool.Exec(ctx,
		`UPDATE worker_instances SET observed_at = NULL WHERE id = $1`,
		fixture.workerID,
	); err != nil {
		t.Fatal(err)
	}

	worker, err := fixture.queries.LockRunLeaseClaimReadyWorker(ctx, LockRunLeaseClaimReadyWorkerParams{
		ID:                          pgvalue.UUID(fixture.workerID),
		WorkerGroupID:               runLeaseTestWorkerGroup,
		ObservationFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds,
	})
	if err != nil {
		t.Fatal(err)
	}
	if worker.RunReady {
		t.Fatal("unobserved worker is ready to claim a run lease")
	}
}

func TestRunLeaseDiscoveryAndClaimFoundation(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	assigned := fixture.addWork(t, ctx, "assigned", time.Now().Add(-2*time.Minute))
	starting := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))

	rows, err := fixture.queries.DiscoverWorkerRunLeaseWork(ctx, DiscoverWorkerRunLeaseWorkParams{
		WorkerGroupID: runLeaseTestWorkerGroup, RowLimit: 8, WorkerInstanceID: pgvalue.UUID(fixture.workerID), WorkerEpoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || pgvalue.MustUUIDValue(rows[0].ID) != starting.leaseID ||
		pgvalue.MustUUIDValue(rows[1].ID) != assigned.leaseID {
		t.Fatalf("discovery = %+v, want starting then assigned", rows)
	}
	var state RunLeaseState
	var claimedAt pgtype.Timestamptz
	if err := fixture.pool.QueryRow(ctx,
		`SELECT state, claimed_at FROM run_leases WHERE id = $1`, assigned.leaseID,
	).Scan(&state, &claimedAt); err != nil {
		t.Fatal(err)
	}
	if state != RunLeaseStateAssigned || claimedAt.Valid {
		t.Fatalf("discovery mutated assigned lease to state=%s claimed_at=%v", state, claimedAt)
	}
	if _, err := fixture.pool.Exec(ctx, `
UPDATE workspace_mounts
   SET state = 'failed', failed_at = now(), terminal_at = now(),
       terminal_reason_code = 'test_failure'
 WHERE id = (SELECT workspace_mount_id FROM workspace_leases
              WHERE owner_run_lease_id = $1)`, starting.leaseID); err != nil {
		t.Fatal(err)
	}
	rows, err = fixture.queries.DiscoverWorkerRunLeaseWork(ctx, DiscoverWorkerRunLeaseWorkParams{
		WorkerGroupID: runLeaseTestWorkerGroup, RowLimit: 8,
		WorkerInstanceID: pgvalue.UUID(fixture.workerID), WorkerEpoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || pgvalue.MustUUIDValue(rows[0].ID) != assigned.leaseID {
		t.Fatalf("discovery after Mount failure = %+v, want only healthy assigned lease", rows)
	}
	if _, err := fixture.pool.Exec(ctx, `
UPDATE workspace_mounts
   SET state = 'mounted', failed_at = NULL, terminal_at = NULL,
       terminal_reason_code = NULL
 WHERE id = (SELECT workspace_mount_id FROM workspace_leases
              WHERE owner_run_lease_id = $1)`, starting.leaseID); err != nil {
		t.Fatal(err)
	}

	secretLocators, err := fixture.queries.GetRunLeaseSecretDeliveryLocators(ctx, GetRunLeaseSecretDeliveryLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(secretLocators.RunID) != assigned.runID ||
		secretLocators.EnvironmentID != pgvalue.UUID(fixture.environmentID) ||
		secretLocators.AttemptNumber != 1 {
		t.Fatalf("Secret delivery locators = %+v", secretLocators)
	}

	locators, err := fixture.queries.GetRunLeaseClaimLocators(ctx, GetRunLeaseClaimLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(locators.RunID) != assigned.runID {
		t.Fatalf("locator run = %s, want %s", pgvalue.UUIDString(locators.RunID), assigned.runID)
	}
	if locators.RunWaitID.Valid ||
		locators.SuspendCheckpointID.Valid ||
		locators.CheckpointPrivateWorkspaceVersionID.Valid {
		t.Fatalf("fresh locator exposed restore authority: %+v", locators)
	}
	source, err := fixture.queries.GetRunCheckpointSource(ctx, GetRunCheckpointSourceParams{
		SourceWorkspaceLeaseID: locators.WorkspaceLeaseID,
		SourceRunLeaseID:       pgvalue.UUID(assigned.leaseID),
		RunID:                  locators.RunID,
		AttemptNumber:          locators.AttemptNumber,
		WorkspaceID:            locators.WorkspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if source.RunLease.ID != pgvalue.UUID(assigned.leaseID) ||
		source.WorkspaceLease.ID != locators.WorkspaceLeaseID ||
		source.WorkspaceLease.OwnerRunLeaseID != source.RunLease.ID ||
		source.RuntimeInstance.ID != locators.RuntimeInstanceID {
		t.Fatal("checkpoint source did not return one Run/Workspace Lease and Runtime receipt")
	}
	if _, err := fixture.queries.GetRunCheckpointSource(ctx, GetRunCheckpointSourceParams{
		SourceWorkspaceLeaseID: pgvalue.UUID(uuid.NewV7()),
		SourceRunLeaseID:       pgvalue.UUID(assigned.leaseID),
		RunID:                  locators.RunID,
		AttemptNumber:          locators.AttemptNumber,
		WorkspaceID:            locators.WorkspaceID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mismatched source Workspace Lease error = %v, want no rows", err)
	}
	if _, err := fixture.queries.LockRunLeaseClaimWait(ctx, LockRunLeaseClaimWaitParams{
		ID:                pgvalue.UUID(uuid.NewV7()),
		EnvironmentID:     locators.EnvironmentID,
		RunID:             locators.RunID,
		AttemptNumber:     locators.AttemptNumber,
		WorkspaceID:       locators.WorkspaceID,
		CurrentRunLeaseID: pgvalue.UUID(assigned.leaseID),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing restore Wait error = %v, want no rows", err)
	}
	if _, err := fixture.queries.LockRestorableRunCheckpoint(ctx, LockRestorableRunCheckpointParams{
		ID:            pgvalue.UUID(uuid.NewV7()),
		RunID:         locators.RunID,
		AttemptNumber: locators.AttemptNumber,
		RunWaitID:     pgvalue.UUID(uuid.NewV7()),
		WorkspaceID:   locators.WorkspaceID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing restore Checkpoint error = %v, want no rows", err)
	}
	if _, err := fixture.queries.LockReadyRunCheckpoint(ctx, LockReadyRunCheckpointParams{
		ID:            pgvalue.UUID(uuid.NewV7()),
		RunID:         locators.RunID,
		AttemptNumber: locators.AttemptNumber,
		RunWaitID:     pgvalue.UUID(uuid.NewV7()),
		WorkspaceID:   locators.WorkspaceID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing restore Checkpoint error = %v, want no rows", err)
	}
	if _, err := fixture.queries.GetRunCheckpointSource(ctx, GetRunCheckpointSourceParams{
		SourceWorkspaceLeaseID: pgvalue.UUID(uuid.NewV7()),
		SourceRunLeaseID:       pgvalue.UUID(uuid.NewV7()),
		RunID:                  locators.RunID,
		AttemptNumber:          locators.AttemptNumber,
		WorkspaceID:            locators.WorkspaceID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing source Runtime error = %v, want no rows", err)
	}
	if _, err := fixture.queries.GetRunLeaseClaimLocators(ctx, GetRunLeaseClaimLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 2,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale sequence locator error = %v, want no rows", err)
	}
	if _, err := fixture.queries.GetRunLeaseClaimLocators(ctx, GetRunLeaseClaimLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(uuid.NewV7()),
		WorkerEpoch: 1}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-worker locator error = %v, want no rows", err)
	}

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	locked := New(tx)
	run, err := locked.LockRunLeaseClaimRun(ctx, LockRunLeaseClaimRunParams{
		ID: locators.RunID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := locked.LockRunLeaseClaimWorkspace(ctx, LockRunLeaseClaimWorkspaceParams{
		ID: locators.WorkspaceID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locked.LockRunLeaseClaimAttempt(ctx, LockRunLeaseClaimAttemptParams{
		RunID: locators.RunID, Number: locators.AttemptNumber, WorkspaceID: locators.WorkspaceID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := locked.LockRunLeaseClaimWorkerGroup(ctx, LockRunLeaseClaimWorkerGroupParams{
		ID: runLeaseTestWorkerGroup, RegionID: locators.RegionID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := locked.LockRunLeaseClaimWorker(ctx, LockRunLeaseClaimWorkerParams{
		ID: pgvalue.UUID(fixture.workerID), WorkerGroupID: runLeaseTestWorkerGroup,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := locked.LockRunLeaseClaimRuntime(ctx, LockRunLeaseClaimRuntimeParams{
		ID: locators.RuntimeInstanceID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, WorkspaceID: locators.WorkspaceID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := locked.LockRunLeaseClaimLease(ctx, LockRunLeaseClaimLeaseParams{
		ID: pgvalue.UUID(assigned.leaseID), RunID: locators.RunID,
		WorkspaceID: locators.WorkspaceID, AttemptNumber: locators.AttemptNumber,
		LeaseSequence: 1,
	}); err != nil {
		t.Fatal(err)
	}
	mount, err := locked.LockRunLeaseClaimMount(ctx, LockRunLeaseClaimMountParams{
		ID: locators.WorkspaceMountID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, RuntimeInstanceID: locators.RuntimeInstanceID, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	workspaceLease, err := locked.LockRunLeaseClaimWorkspaceLease(ctx, LockRunLeaseClaimWorkspaceLeaseParams{
		ID: locators.WorkspaceLeaseID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, RuntimeInstanceID: locators.RuntimeInstanceID, WorkspaceID: locators.WorkspaceID,
		WorkspaceMountID: locators.WorkspaceMountID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.CurrentRunLeaseID != pgvalue.UUID(assigned.leaseID) ||
		workspaceLease.OwnerRunLeaseID != pgvalue.UUID(assigned.leaseID) ||
		workspaceLease.MountFencingGeneration != mount.FencingGeneration ||
		workspaceLease.OwnershipGeneration != workspace.OwnershipGeneration ||
		workspaceLease.WriterGeneration != workspace.WriterGeneration {
		t.Fatal("locked claim authority is not one exact attachment")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	claimed, err := fixture.queries.MarkRunLeaseStarting(ctx, MarkRunLeaseStartingParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != RunLeaseStateStarting || !claimed.ClaimedAt.Valid {
		t.Fatalf("claimed lease = state:%s claimed_at:%v", claimed.State, claimed.ClaimedAt)
	}
	firstClaimedAt := claimed.ClaimedAt.Time
	if _, err := fixture.queries.MarkRunLeaseStarting(ctx, MarkRunLeaseStartingParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second claim update error = %v, want no rows", err)
	}
	var replayedClaimedAt pgtype.Timestamptz
	if err := fixture.pool.QueryRow(ctx, `
		SELECT claimed_at
		  FROM run_leases
		 WHERE run_id = $1
		   AND attempt_number = 1
		   AND workspace_id = $2
		   AND id = $3
	`, pgvalue.UUID(assigned.runID), locators.WorkspaceID, pgvalue.UUID(assigned.leaseID)).Scan(&replayedClaimedAt); err != nil {
		t.Fatal(err)
	}
	if !replayedClaimedAt.Valid || !replayedClaimedAt.Time.Equal(firstClaimedAt) {
		t.Fatalf("claim replay timestamp = %v, want %s", replayedClaimedAt, firstClaimedAt)
	}
	unclaimed := fixture.addWork(t, ctx, "assigned", time.Now())

	if _, err := fixture.pool.Exec(ctx,
		`UPDATE worker_instances SET state = 'draining', draining_at = now() WHERE id = $1`,
		fixture.workerID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx,
		`UPDATE worker_groups SET state = 'draining' WHERE id = $1`,
		runLeaseTestWorkerGroup,
	); err != nil {
		t.Fatal(err)
	}
	drainingRows, err := fixture.queries.DiscoverWorkerRunLeaseWork(ctx, DiscoverWorkerRunLeaseWorkParams{
		WorkerGroupID: runLeaseTestWorkerGroup, RowLimit: 8, WorkerInstanceID: pgvalue.UUID(fixture.workerID), WorkerEpoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(drainingRows) != 3 {
		t.Fatalf("draining discovery returned %d rows, want assigned plus two starting leases", len(drainingRows))
	}
	foundUnclaimed := false
	for _, row := range drainingRows {
		if pgvalue.MustUUIDValue(row.ID) == unclaimed.leaseID {
			foundUnclaimed = true
		}
		if pgvalue.MustUUIDValue(row.ID) != assigned.leaseID &&
			pgvalue.MustUUIDValue(row.ID) != starting.leaseID &&
			pgvalue.MustUUIDValue(row.ID) != unclaimed.leaseID {
			t.Fatalf("draining discovery returned unrelated lease %s", pgvalue.UUIDString(row.ID))
		}
	}
	if !foundUnclaimed {
		t.Fatalf("draining discovery omitted assigned lease %s", unclaimed.leaseID)
	}
	if _, err := fixture.queries.GetRunLeaseSecretDeliveryLocators(ctx, GetRunLeaseSecretDeliveryLocatorsParams{
		ID: pgvalue.UUID(unclaimed.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1}); err != nil {
		t.Fatalf("draining assigned Secret locator: %v", err)
	}
	if _, err := fixture.queries.GetRunLeaseSecretDeliveryLocators(ctx, GetRunLeaseSecretDeliveryLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1}); err != nil {
		t.Fatalf("draining replay Secret locator: %v", err)
	}
	if _, err := fixture.queries.GetRunLeaseClaimLocators(ctx, GetRunLeaseClaimLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1}); err != nil {
		t.Fatalf("draining replay claim locator: %v", err)
	}
	if _, err := fixture.queries.GetRunLeaseStartLocators(ctx, GetRunLeaseStartLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1}); err != nil {
		t.Fatalf("draining starting lease start locator: %v", err)
	}
	if _, err := fixture.pool.Exec(ctx, `
UPDATE run_leases
   SET state = 'running', started_at = now(), updated_at = now()
 WHERE id = $1`, pgvalue.UUID(assigned.leaseID)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `
UPDATE runs
   SET status = 'running', started_at = now(), active_started_at = now(), updated_at = now()
 WHERE id = $1`, pgvalue.UUID(assigned.runID)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.GetRunEntrypointLocators(ctx, GetRunEntrypointLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1}); err != nil {
		t.Fatalf("draining running lease entrypoint locator: %v", err)
	}
	if _, err := fixture.queries.GetRunEntrypointLocators(ctx, GetRunEntrypointLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 2}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale draining entrypoint locator error = %v, want no rows", err)
	}
}

func newRunLeaseClaimFixture(t *testing.T, _ context.Context) runLeaseClaimFixture {
	t.Helper()
	base := runtest.New(t)
	return runLeaseClaimFixture{
		pool:                  base.Pool,
		queries:               New(base.Pool),
		orgID:                 base.OrgID,
		projectID:             base.ProjectID,
		environmentID:         base.EnvironmentID,
		deploymentID:          base.DeploymentID,
		taskDefinitionID:      base.TaskDefinitionID,
		workspaceDefinitionID: base.WorkspaceDefinitionID,
		workerID:              base.WorkerID,
		runtimeIdentityID:     base.RuntimeIdentityID,
		base:                  base,
	}
}

func (fixture runLeaseClaimFixture) addWork(
	t *testing.T,
	_ context.Context,
	state string,
	assignedAt time.Time,
) runLeaseWork {
	t.Helper()
	work := fixture.base.AddRunLease(t, state, assignedAt)
	return runLeaseWork{leaseID: work.LeaseID, runID: work.RunID}
}

func (fixture runLeaseClaimFixture) convertToActor(
	t *testing.T,
	ctx context.Context,
	work runLeaseWork,
	retryPolicy string,
) uuid.UUID {
	t.Helper()
	return fixture.base.ConvertToActor(
		t,
		ctx,
		runtest.RunLease{LeaseID: work.leaseID, RunID: work.runID},
		retryPolicy,
	)
}
