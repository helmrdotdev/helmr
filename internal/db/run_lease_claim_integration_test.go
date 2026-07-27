package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	runLeaseTestRegion      = runtest.Region
	runLeaseTestWorkerGroup = runtest.WorkerGroup
	runLeaseTestProtocol    = runtest.WorkerProtocol
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

type nestedHandoffChain struct {
	outerRunID          uuid.UUID
	parentRunID         uuid.UUID
	outerWaitID         uuid.UUID
	outerCheckpoint     uuid.UUID
	outerResumeID       uuid.UUID
	enclosingWaitID     uuid.UUID
	enclosingCheckpoint uuid.UUID
	enclosingResumeID   uuid.UUID
	runtimeID           uuid.UUID
	mountID             uuid.UUID
	versionID           uuid.UUID
}

func TestRunLeaseDiscoveryAndClaimFoundation(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	assigned := fixture.addWork(t, ctx, "assigned", time.Now().Add(-2*time.Minute))
	starting := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))

	rows, err := fixture.queries.DiscoverWorkerRunLeaseWork(ctx, DiscoverWorkerRunLeaseWorkParams{
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerProtocolVersion: runLeaseTestProtocol,
		RowLimit: 8, WorkerInstanceID: pgvalue.UUID(fixture.workerID), WorkerEpoch: 1,
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

	secretLocators, err := fixture.queries.GetRunLeaseSecretDeliveryLocators(ctx, GetRunLeaseSecretDeliveryLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	})
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
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	})
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
		SourceWorkspaceLeaseID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		SourceRunLeaseID:       pgvalue.UUID(assigned.leaseID),
		RunID:                  locators.RunID,
		AttemptNumber:          locators.AttemptNumber,
		WorkspaceID:            locators.WorkspaceID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mismatched source Workspace Lease error = %v, want no rows", err)
	}
	if _, err := fixture.queries.LockRunLeaseClaimWait(ctx, LockRunLeaseClaimWaitParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID:     locators.EnvironmentID,
		RunID:             locators.RunID,
		AttemptNumber:     locators.AttemptNumber,
		WorkspaceID:       locators.WorkspaceID,
		CurrentRunLeaseID: pgvalue.UUID(assigned.leaseID),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing restore Wait error = %v, want no rows", err)
	}
	if _, err := fixture.queries.LockSameWorkspaceHandoffWait(ctx, LockSameWorkspaceHandoffWaitParams{
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID:       locators.EnvironmentID,
		ParentRunID:         pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ParentAttemptNumber: 1,
		WorkspaceID:         locators.WorkspaceID,
		ChildRunID:          locators.RunID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing handoff Wait error = %v, want no rows", err)
	}
	if _, err := fixture.queries.LockRestorableRunCheckpoint(ctx, LockRestorableRunCheckpointParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RunID:         locators.RunID,
		AttemptNumber: locators.AttemptNumber,
		RunWaitID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		WorkspaceID:   locators.WorkspaceID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing restore Checkpoint error = %v, want no rows", err)
	}
	if _, err := fixture.queries.LockReadyRunCheckpoint(ctx, LockReadyRunCheckpointParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:          RunCheckpointKindHandoffResume,
		RunID:         locators.RunID,
		AttemptNumber: locators.AttemptNumber,
		RunWaitID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		WorkspaceID:   locators.WorkspaceID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing handoff Checkpoint error = %v, want no rows", err)
	}
	if _, err := fixture.queries.GetRunCheckpointSource(ctx, GetRunCheckpointSourceParams{
		SourceWorkspaceLeaseID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		SourceRunLeaseID:       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RunID:                  locators.RunID,
		AttemptNumber:          locators.AttemptNumber,
		WorkspaceID:            locators.WorkspaceID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing source Runtime error = %v, want no rows", err)
	}
	if _, err := fixture.queries.GetRunLeaseClaimLocators(ctx, GetRunLeaseClaimLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 2,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale sequence locator error = %v, want no rows", err)
	}
	if _, err := fixture.queries.GetRunLeaseClaimLocators(ctx, GetRunLeaseClaimLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	}); !errors.Is(err, pgx.ErrNoRows) {
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
	if _, err := locked.LockRunLeaseClaimNetworkSlot(ctx, LockRunLeaseClaimNetworkSlotParams{
		ID: locators.NetworkSlotID, WorkerGroupID: runLeaseTestWorkerGroup,
		WorkerInstanceID: pgvalue.UUID(fixture.workerID), WorkerEpoch: 1,
		Generation: locators.NetworkSlotGeneration, RuntimeInstanceID: locators.RuntimeInstanceID,
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
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	})
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
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second claim update error = %v, want no rows", err)
	}
	replayed, err := fixture.queries.GetRunLease(ctx, GetRunLeaseParams{
		RunID: pgvalue.UUID(assigned.runID), AttemptNumber: 1,
		WorkspaceID: locators.WorkspaceID, ID: pgvalue.UUID(assigned.leaseID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.ClaimedAt.Valid || !replayed.ClaimedAt.Time.Equal(firstClaimedAt) {
		t.Fatalf("claim replay timestamp = %v, want %s", replayed.ClaimedAt, firstClaimedAt)
	}
	unclaimed := fixture.addWork(t, ctx, "assigned", time.Now())

	if _, err := fixture.pool.Exec(ctx,
		`UPDATE worker_instances SET state = 'draining', draining_at = now() WHERE id = $1`,
		fixture.workerID,
	); err != nil {
		t.Fatal(err)
	}
	drainingRows, err := fixture.queries.DiscoverWorkerRunLeaseWork(ctx, DiscoverWorkerRunLeaseWorkParams{
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerProtocolVersion: runLeaseTestProtocol,
		RowLimit: 8, WorkerInstanceID: pgvalue.UUID(fixture.workerID), WorkerEpoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(drainingRows) != 2 {
		t.Fatalf("draining discovery returned %d rows, want two replayable starting leases", len(drainingRows))
	}
	for _, row := range drainingRows {
		if pgvalue.MustUUIDValue(row.ID) == unclaimed.leaseID {
			t.Fatalf("draining discovery returned unclaimed assigned lease %s", unclaimed.leaseID)
		}
		if pgvalue.MustUUIDValue(row.ID) != assigned.leaseID &&
			pgvalue.MustUUIDValue(row.ID) != starting.leaseID {
			t.Fatalf("draining discovery returned unrelated lease %s", pgvalue.UUIDString(row.ID))
		}
	}
	if _, err := fixture.queries.GetRunLeaseSecretDeliveryLocators(ctx, GetRunLeaseSecretDeliveryLocatorsParams{
		ID: pgvalue.UUID(unclaimed.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("draining assigned Secret locator error = %v, want no rows", err)
	}
	if _, err := fixture.queries.GetRunLeaseSecretDeliveryLocators(ctx, GetRunLeaseSecretDeliveryLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	}); err != nil {
		t.Fatalf("draining replay Secret locator: %v", err)
	}
}

func TestRunLeaseClaimLocatesNestedHandoffAuthority(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "assigned", time.Now().Add(-time.Minute))
	chain := fixture.addNestedHandoffChain(t, ctx, work)
	locatorArgs := []any{
		pgvalue.UUID(work.leaseID),
		int64(1),
		runLeaseTestWorkerGroup,
		pgvalue.UUID(fixture.workerID),
		int64(1),
		runLeaseTestProtocol,
	}
	var locatorCount int
	if err := fixture.pool.QueryRow(
		ctx,
		"SELECT count(*) FROM ("+getRunLeaseClaimLocators+") AS claim_locators",
		locatorArgs...,
	).Scan(&locatorCount); err != nil {
		t.Fatal(err)
	}
	if locatorCount != 1 {
		t.Fatalf("nested claim locator rows = %d, want exactly one", locatorCount)
	}

	locators, err := fixture.queries.GetRunLeaseClaimLocators(ctx, GetRunLeaseClaimLocatorsParams{
		ID: pgvalue.UUID(work.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(locators.ParentRunID) != chain.parentRunID ||
		!locators.ParentOwnsLifecycle.Valid ||
		!locators.ParentOwnsLifecycle.Bool ||
		locators.ParentAttemptNumber != 1 {
		t.Fatalf("parent locator = %+v, want parent %s attempt 1", locators, chain.parentRunID)
	}
	if pgvalue.MustUUIDValue(locators.EnclosingWaitID) != chain.enclosingWaitID ||
		pgvalue.MustUUIDValue(locators.EnclosingSuspendCheckpointID) != chain.enclosingCheckpoint ||
		pgvalue.MustUUIDValue(locators.EnclosingResumeAttachID) != chain.enclosingResumeID ||
		pgvalue.MustUUIDValue(locators.EnclosingRuntimeInstanceID) != chain.runtimeID ||
		pgvalue.MustUUIDValue(locators.EnclosingWorkspaceMountID) != chain.mountID ||
		pgvalue.MustUUIDValue(locators.EnclosingBaseWorkspaceVersionID) != chain.versionID ||
		locators.EnclosingMountGeneration.Int64 != 2 ||
		locators.EnclosingOwnershipGeneration.Int64 != 1 ||
		locators.EnclosingParentWriterGeneration.Int64 != 2 ||
		locators.EnclosingChildWriterGeneration.Int64 != 3 ||
		locators.EnclosingResumeWriterGeneration.Valid {
		t.Fatalf("enclosing locator = %+v, want B→C writer receipt 2→3", locators)
	}
	if pgvalue.MustUUIDValue(locators.ParentEnclosingWaitID) != chain.outerWaitID ||
		pgvalue.MustUUIDValue(locators.ParentEnclosingRunID) != chain.outerRunID ||
		locators.ParentEnclosingAttemptNumber != 1 {
		t.Fatalf("parent enclosing locator = %+v, want A→B Wait %s", locators, chain.outerWaitID)
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

func (fixture runLeaseClaimFixture) addNestedHandoffChain(
	t *testing.T,
	ctx context.Context,
	work runLeaseWork,
) nestedHandoffChain {
	t.Helper()
	chain := nestedHandoffChain{
		outerRunID:          uuid.Must(uuid.NewV7()),
		parentRunID:         uuid.Must(uuid.NewV7()),
		outerWaitID:         uuid.Must(uuid.NewV7()),
		outerCheckpoint:     uuid.Must(uuid.NewV7()),
		outerResumeID:       uuid.Must(uuid.NewV7()),
		enclosingWaitID:     uuid.Must(uuid.NewV7()),
		enclosingCheckpoint: uuid.Must(uuid.NewV7()),
		enclosingResumeID:   uuid.Must(uuid.NewV7()),
	}
	outerClaimID, enclosingClaimID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	outerLeaseID, parentLeaseID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	outerWorkspaceLeaseID, parentWorkspaceLeaseID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	historicalWaitID := uuid.Must(uuid.NewV7())

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT run_leases.runtime_instance_id,
		       workspace_leases.workspace_mount_id,
		       workspace_leases.base_version_id
		  FROM run_leases
		  JOIN workspace_leases
		    ON workspace_leases.owner_run_lease_id = run_leases.id
		 WHERE run_leases.id = $1
	`, work.leaseID).Scan(&chain.runtimeID, &chain.mountID, &chain.versionID); err != nil {
		t.Fatal(err)
	}
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE workspace_leases
		   SET state = 'released',
		       writer_generation = 3,
		       released_at = now(),
		       terminal_at = now()
		 WHERE owner_run_lease_id = $1
	`, work.leaseID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE run_leases
		   SET state = 'checkpointed',
		       claimed_at = assigned_at,
		       started_at = assigned_at,
		       checkpointed_at = now(),
		       terminal_at = now(),
		       terminal_reason_code = 'test_handoff'
		 WHERE id = $1
	`, work.leaseID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO idempotency_claims (
			id, environment_id, operation, scope_hash, key_hash,
			hash_key_version, generation, request_fingerprint, accepted_at
		) VALUES
			($1, $3, 'task.child.invoke', $4, $5, 1, 1, $6, now()),
			($2, $3, 'task.child.invoke', $7, $8, 1, 1, $9, now())
	`, outerClaimID, enclosingClaimID, fixture.environmentID,
		runLeaseTestHash("outer-scope"), runLeaseTestHash("outer-key"),
		runLeaseTestHash("outer-request"), runLeaseTestHash("inner-scope"),
		runLeaseTestHash("inner-key"), runLeaseTestHash("inner-request"))
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO runs (
			id, public_id, org_id, project_id, environment_id, deployment_id,
			deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
			cause_kind, parent_run_id, parent_owns_lifecycle, workspace_id,
			base_workspace_version_id, payload, queue_name, queue_origin_at,
			queue_score_at, max_active_duration_ms, retry_policy, trace_id,
			root_span_id, claim_id
		) VALUES (
			$1, $2, $5, $6, $7, $8, $9, 'task', 'test-task', 'api',
			NULL, NULL, $10, $11, '{}'::jsonb, 'default', now(), now(),
			300000, '{"enabled":false}'::jsonb,
			'33333333333333333333333333333333', '4444444444444444', NULL
		), (
			$3, $4, $5, $6, $7, $8, $9, 'task', 'test-task', 'child',
			$1, true, $10, $11, '{}'::jsonb, 'default', now(), now(),
			300000, '{"enabled":false}'::jsonb,
			'55555555555555555555555555555555', '6666666666666666', $12
		)
	`, chain.outerRunID, runLeasePublicID(t, publicid.Run),
		chain.parentRunID, runLeasePublicID(t, publicid.Run),
		fixture.orgID, fixture.projectID, fixture.environmentID,
		fixture.deploymentID, fixture.taskDefinitionID, fixture.workspaceID(t, ctx, tx, work.runID),
		chain.versionID, outerClaimID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE runs
		   SET cause_kind = 'child',
		       parent_run_id = $1,
		       parent_owns_lifecycle = true,
		       claim_id = $2
		 WHERE id = $3
	`, chain.parentRunID, enclosingClaimID, work.runID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO run_attempts (
			run_id, number, entrypoint_kind, workspace_id,
			entrypoint_entered_at, base_workspace_version_id
		) VALUES
			($1, 1, 'task', $3, now(), $4),
			($2, 1, 'task', $3, now(), $4)
	`, chain.outerRunID, chain.parentRunID, fixture.workspaceID(t, ctx, tx, work.runID), chain.versionID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE workspaces
		   SET owner_run_id = $1,
		       writer_generation = 3
		 WHERE id = (SELECT workspace_id FROM runs WHERE id = $2)
	`, chain.outerRunID, work.runID)

	fixture.parkNestedRun(t, ctx, tx, nestedRunPark{
		runID: chain.outerRunID, childRunID: chain.parentRunID,
		claimID: outerClaimID, leaseID: outerLeaseID,
		workspaceLeaseID: outerWorkspaceLeaseID, waitID: chain.outerWaitID,
		checkpointID: chain.outerCheckpoint, writerGeneration: 1,
		childWriterGeneration: 2, runtimeID: chain.runtimeID,
		mountID: chain.mountID, versionID: chain.versionID,
		resumeAttachID: chain.outerResumeID,
	})
	fixture.parkNestedRun(t, ctx, tx, nestedRunPark{
		runID: chain.parentRunID, childRunID: work.runID,
		claimID: enclosingClaimID, leaseID: parentLeaseID,
		workspaceLeaseID: parentWorkspaceLeaseID, waitID: chain.enclosingWaitID,
		checkpointID: chain.enclosingCheckpoint, writerGeneration: 2,
		childWriterGeneration: 3, runtimeID: chain.runtimeID,
		mountID: chain.mountID, versionID: chain.versionID,
		resumeAttachID: chain.enclosingResumeID,
	})
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO run_waits (
			id, environment_id, run_id, workspace_id, kind, condition_state,
			child_run_id, child_parent_owned, child_target_declared_id,
			child_claim_id, child_request, suspension_state,
			expected_run_state_version, attempt_number, resume_attach_id,
			condition_error, condition_terminal_at, condition_reason_code,
			suspension_terminal_at, suspension_reason_code, suspension_error
		) VALUES (
			$1, $2, $3, $4, 'child', 'failed',
			$5, true, 'test-task', $6, '{}'::jsonb, 'failed',
			1, 1, $7, '{}'::jsonb, now(), 'test_history',
			now(), 'test_history', '{}'::jsonb
		)
	`, historicalWaitID, fixture.environmentID, chain.outerRunID,
		fixture.workspaceID(t, ctx, tx, work.runID), chain.parentRunID,
		outerClaimID, uuid.Must(uuid.NewV7()))
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE run_leases
		   SET state = 'assigned',
		       claimed_at = NULL,
		       started_at = NULL,
		       checkpointed_at = NULL,
		       terminal_at = NULL,
		       terminal_reason_code = NULL
		 WHERE id = $1
	`, work.leaseID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE workspace_leases
		   SET state = 'active',
		       released_at = NULL,
		       terminal_at = NULL
		 WHERE owner_run_lease_id = $1
	`, work.leaseID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return chain
}

type nestedRunPark struct {
	runID                 uuid.UUID
	childRunID            uuid.UUID
	claimID               uuid.UUID
	leaseID               uuid.UUID
	workspaceLeaseID      uuid.UUID
	waitID                uuid.UUID
	checkpointID          uuid.UUID
	writerGeneration      int64
	childWriterGeneration int64
	runtimeID             uuid.UUID
	mountID               uuid.UUID
	versionID             uuid.UUID
	resumeAttachID        uuid.UUID
}

func (fixture runLeaseClaimFixture) parkNestedRun(
	t *testing.T,
	ctx context.Context,
	tx pgx.Tx,
	park nestedRunPark,
) {
	t.Helper()
	workspaceID := fixture.workspaceID(t, ctx, tx, park.runID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO run_leases (
			id, org_id, project_id, environment_id, run_id, workspace_id, region_id,
			lease_sequence, attempt_number, worker_group_id, worker_instance_id,
			worker_epoch, runtime_instance_id, network_slot_id, network_slot_generation,
			runtime_identity_id, worker_protocol_version, requested_cpu_millis,
			requested_memory_bytes, requested_workload_disk_bytes, requested_scratch_bytes,
			requested_execution_slots, state, assigned_at, start_deadline_at,
			claimed_at, started_at, expires_at
		)
		SELECT $1, org_id, project_id, environment_id, $2, workspace_id, region_id,
		       1, 1, worker_group_id, worker_instance_id, worker_epoch,
		       runtime_instance_id, network_slot_id, network_slot_generation,
		       runtime_identity_id, worker_protocol_version, requested_cpu_millis,
		       requested_memory_bytes, requested_workload_disk_bytes,
		       requested_scratch_bytes, requested_execution_slots, 'running',
		       now() - interval '1 minute', now() + interval '5 minutes',
		       now() - interval '1 minute', now() - interval '1 minute',
		       now() + interval '10 minutes'
		  FROM run_leases
		 WHERE runtime_instance_id = $3
		 ORDER BY created_at
		 LIMIT 1
	`, park.leaseID, park.runID, park.runtimeID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO workspace_leases (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
			workspace_mount_id, owner_run_lease_id, base_version_id,
			ownership_generation, writer_generation, mount_fencing_generation,
			fencing_key_fingerprint, fencing_token_hash, expires_at
		)
		SELECT $1, org_id, worker_group_id, project_id, environment_id, region_id,
		       worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
		       workspace_mount_id, $2, base_version_id, ownership_generation, $3,
		       mount_fencing_generation, fencing_key_fingerprint, fencing_token_hash,
		       now() + interval '10 minutes'
		  FROM workspace_leases
		 WHERE workspace_id = $4
		 ORDER BY acquired_at
		 LIMIT 1
	`, park.workspaceLeaseID, park.leaseID, park.writerGeneration, workspaceID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE runs
		   SET current_run_lease_id = $1,
		       status = 'running',
		       first_lease_at = now() - interval '1 minute',
		       started_at = now() - interval '1 minute'
		 WHERE id = $2
	`, park.leaseID, park.runID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO run_waits (
			id, environment_id, run_id, workspace_id, kind, condition_state,
			child_run_id, child_parent_owned, child_target_declared_id,
			child_claim_id, child_request, suspension_state,
			expected_run_state_version, attempt_number, current_run_lease_id,
			checkpoint_request_version, checkpoint_ack_version, resume_attach_id
		) VALUES (
			$1, $2, $3, $4, 'child', 'pending',
			$5, true, 'test-task', $6, '{}'::jsonb, 'hot',
			1, 1, $7, 1, 0, $8
		)
	`, park.waitID, fixture.environmentID, park.runID, workspaceID,
		park.childRunID, park.claimID, park.leaseID, park.resumeAttachID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE run_leases
		   SET state = 'checkpointed',
		       checkpointed_at = now(),
		       terminal_at = now(),
		       terminal_reason_code = 'test_handoff'
		 WHERE id = $1
	`, park.leaseID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE workspace_leases
		   SET state = 'released',
		       released_at = now(),
		       terminal_at = now()
		 WHERE id = $1
	`, park.workspaceLeaseID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO run_checkpoints (
			id, kind, run_id, attempt_number, run_wait_id,
			source_run_lease_id, source_workspace_lease_id, workspace_id,
			base_workspace_version_id, private_workspace_version_id,
			state, restore_manifest, ready_request_fingerprint, ready_at
		) VALUES (
			$1, 'suspend', $2, 1, $3, $4, $5, $6,
			$7, $7, 'ready', '{"test":true}'::jsonb, 'test-ready', now()
		)
	`, park.checkpointID, park.runID, park.waitID, park.leaseID,
		park.workspaceLeaseID, workspaceID, park.versionID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE run_waits
		   SET suspension_state = 'parked',
		       current_run_lease_id = NULL,
		       prior_run_lease_id = $1,
		       checkpoint_ack_version = 1,
		       suspend_checkpoint_id = $2,
		       base_workspace_version_id = $3,
		       base_workspace_content_digest = (
		           SELECT content_digest
		             FROM workspace_versions
		            WHERE id = $3
		       ),
		       handoff_runtime_instance_id = $4,
		       handoff_workspace_mount_id = $5,
		       handoff_mount_generation = 2,
		       ownership_generation = 1,
		       parent_writer_generation = $6,
		       child_writer_generation = $7
		 WHERE id = $8
	`, park.leaseID, park.checkpointID, park.versionID, park.runtimeID,
		park.mountID, park.writerGeneration, park.childWriterGeneration, park.waitID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE runs
		   SET status = 'waiting',
		       current_run_lease_id = NULL
		 WHERE id = $1
	`, park.runID)
}

func (fixture runLeaseClaimFixture) workspaceID(
	t *testing.T,
	ctx context.Context,
	tx pgx.Tx,
	runID uuid.UUID,
) uuid.UUID {
	t.Helper()
	var workspaceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT workspace_id FROM runs WHERE id = $1`, runID).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	return workspaceID
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

func mustRunLeaseExec(t *testing.T, ctx context.Context, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, query string, args ...any) {
	t.Helper()
	runtest.MustExec(t, ctx, db, query, args...)
}

func runLeasePublicID(t *testing.T, prefix publicid.Prefix) string {
	t.Helper()
	return runtest.PublicID(t, prefix)
}

func runLeaseTestDigest(seed string) string {
	return runtest.Digest(seed)
}

func runLeaseTestHash(seed string) []byte {
	return runtest.Hash(seed)
}

func shortRunLeaseID(id uuid.UUID) string {
	return runtest.ShortID(id)
}
