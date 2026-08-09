package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRunRuntimeReadyRequiresConvergedReadyIntent(t *testing.T) {
	ready := runRuntime{
		desiredState: db.RuntimeDesiredStateReady, desiredVersion: 3,
		observedState: db.RuntimeObservedStateReady, observedDesiredVersion: 3,
	}
	if !runRuntimeReady(ready) {
		t.Fatal("converged ready runtime was rejected")
	}
	for name, mutate := range map[string]func(*runRuntime){
		"close requested":      func(runtime *runRuntime) { runtime.desiredState = db.RuntimeDesiredStateClosed },
		"not physically ready": func(runtime *runRuntime) { runtime.observedState = db.RuntimeObservedStatePreparing },
		"stale observation":    func(runtime *runRuntime) { runtime.observedDesiredVersion-- },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := ready
			mutate(&candidate)
			if runRuntimeReady(candidate) {
				t.Fatal("non-converged runtime was accepted")
			}
		})
	}
}

func recoverExpiredRunResumesParams(limit int32) db.RecoverExpiredRunResumesParams {
	return db.RecoverExpiredRunResumesParams{
		OutboxMessageIds: pgvalue.NewUUIDv7Batch(limit),
		LimitCount:       limit,
	}
}

func mustRunCandidateParams(t *testing.T, scope QueueScope, limit int32) db.ListQueuedRunDispatchCandidatesForScopesParams {
	t.Helper()
	params, err := runCandidateParams([]QueueScope{scope}, limit)
	if err != nil {
		t.Fatal(err)
	}
	return params
}

type runPlacementFixture struct {
	ctx           context.Context
	pool          *pgxpool.Pool
	authority     *Authority
	fencingKey    workspace.FencingKey
	orgID         uuid.UUID
	projectID     uuid.UUID
	environmentID uuid.UUID
	deploymentID  uuid.UUID
	runID         uuid.UUID
	workspaceID   uuid.UUID
	workerID      uuid.UUID
	groupID       string
}

func TestPlaceReadyRunPreparesMountAndGrantsFencedLeases(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	candidate := fixture.candidate()

	reserved, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		candidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.LeaseCreated ||
		!reserved.RuntimeInstanceID.Valid ||
		reserved.WorkspaceMountID.Valid {
		t.Fatalf("reservation placement = %+v", reserved)
	}

	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runtime_instances
   SET observed_state = 'ready',
       observed_version = 1,
       observed_desired_version = desired_version,
       preparing_at = transaction_timestamp(),
       ready_at = transaction_timestamp(),
       observed_at = transaction_timestamp()
 WHERE id = $1`,
		reserved.RuntimeInstanceID,
	)
	mounting, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		candidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	if mounting.LeaseCreated ||
		!mounting.WorkspaceMountID.Valid ||
		mounting.RuntimeInstanceID != reserved.RuntimeInstanceID {
		t.Fatalf("mounting placement = %+v", mounting)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts
   SET state = 'mounted',
       mounted_at = transaction_timestamp()
 WHERE id = $1`,
		mounting.WorkspaceMountID,
	)

	granted, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		candidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !granted.LeaseCreated ||
		!granted.Lease.ID.Valid ||
		granted.Lease.RuntimeInstanceID != reserved.RuntimeInstanceID {
		t.Fatalf("granted placement = %+v", granted)
	}

	var currentLeaseID, reservedRunID, workspaceLeaseID pgtype.UUID
	var firstLeaseAt pgtype.Timestamptz
	var stateVersion, writerGeneration, mountGeneration int64
	var ownerRunLeaseID pgtype.UUID
	var tokenHash string
	err = fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.current_run_lease_id,
       runs.first_lease_at,
       runs.state_version,
       runtime_instances.reserved_run_id,
       workspaces.writer_generation,
       workspace_mounts.fencing_generation,
       workspace_leases.id,
       workspace_leases.owner_run_lease_id,
       workspace_leases.fencing_token_hash
  FROM runs
  JOIN workspaces ON workspaces.id = runs.workspace_id
  JOIN run_leases ON run_leases.id = runs.current_run_lease_id
  JOIN runtime_instances ON runtime_instances.id = run_leases.runtime_instance_id
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
  JOIN workspace_mounts ON workspace_mounts.id = workspace_leases.workspace_mount_id
 WHERE runs.id = $1`,
		fixture.runID,
	).Scan(
		&currentLeaseID,
		&firstLeaseAt,
		&stateVersion,
		&reservedRunID,
		&writerGeneration,
		&mountGeneration,
		&workspaceLeaseID,
		&ownerRunLeaseID,
		&tokenHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	if currentLeaseID != granted.Lease.ID ||
		ownerRunLeaseID != granted.Lease.ID ||
		!firstLeaseAt.Valid ||
		stateVersion != 2 ||
		reservedRunID.Valid ||
		writerGeneration != 1 ||
		mountGeneration != 2 {
		t.Fatalf(
			"grant receipt lease=%s owner=%s first=%v state=%d reserved=%s writer=%d mount=%d",
			pgvalue.UUIDString(currentLeaseID),
			pgvalue.UUIDString(ownerRunLeaseID),
			firstLeaseAt.Valid,
			stateVersion,
			pgvalue.UUIDString(reservedRunID),
			writerGeneration,
			mountGeneration,
		)
	}
	leaseUUID, err := pgvalue.UUIDValue(workspaceLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := fixture.fencingKey.Derive(workspace.FenceInput{
		LeaseID:                leaseUUID,
		WorkspaceID:            fixture.workspaceID,
		OwnershipGeneration:    1,
		WriterGeneration:       writerGeneration,
		MountFencingGeneration: mountGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tokenHash != replayed.Hash || tokenHash == replayed.Token {
		t.Fatal("Workspace Lease did not persist the replayable token hash")
	}
}

func TestRunPreparationConcurrencyUsesOnlyFiniteQueueLimits(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET queue_concurrency_limit = 1
 WHERE id = $1`,
		fixture.runID,
	)

	reserved, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		fixture.candidate(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.LeaseCreated || !reserved.RuntimeInstanceID.Valid {
		t.Fatalf("reservation placement = %+v", reserved)
	}

	queries := db.New(fixture.pool)
	scopes, err := queries.ListQueuedRunEligibleScopes(
		fixture.ctx,
		db.ListQueuedRunEligibleScopesParams{
			RowLimit: 10,
			ScanSeed: "preparation-concurrency",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 {
		t.Fatalf("candidate scopes = %d, want 1", len(scopes))
	}
	usage, err := queries.ListQueuedRunPlanningUsage(
		fixture.ctx,
		db.ListQueuedRunPlanningUsageParams{
			EnvironmentIds:  []pgtype.UUID{scopes[0].EnvironmentID},
			ConcurrencyKeys: []string{scopes[0].ConcurrencyKey},
			QueueNames:      []string{scopes[0].QueueName},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].ActiveRuns != 0 || usage[0].ActiveLimit != 0 ||
		usage[0].PreparedRuns != 1 || usage[0].PreparedLimit != 1 {
		t.Fatalf("queue usage = %+v", usage)
	}

	candidates, err := queries.ListQueuedRunDispatchCandidatesForScopes(
		fixture.ctx,
		mustRunCandidateParams(t, QueueScope{
			OrgID:          pgvalue.UUID(fixture.orgID),
			ProjectID:      pgvalue.UUID(fixture.projectID),
			EnvironmentID:  pgvalue.UUID(fixture.environmentID),
			RegionID:       "us-east-1",
			ConcurrencyKey: "",
			QueueName:      "default",
		}, 10),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 ||
		!candidates[0].QueueConcurrencyLimit.Valid ||
		candidates[0].QueueConcurrencyLimit.Int64 != 1 {
		t.Fatalf("dispatch candidates = %+v", candidates)
	}

	assertPreparationBlocked := func(authority runPlacementAuthority) {
		t.Helper()
		tx, err := fixture.pool.Begin(fixture.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if err := fixture.authority.checkRunQueuePreparationConcurrency(
			fixture.ctx,
			tx,
			authority,
		); !errors.Is(err, ErrCapacityUnavailable) {
			t.Fatalf("preparation concurrency error = %v, want ErrCapacityUnavailable", err)
		}
	}
	authority := runPlacementAuthority{
		environmentID: pgvalue.UUID(fixture.environmentID),
		queueName:     "default",
		queueLimit:    pgtype.Int8{Int64: 1, Valid: true},
	}
	assertPreparationBlocked(authority)
	authority.queueLimit = pgtype.Int8{}
	assertPreparationBlocked(authority)

	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET queue_concurrency_limit = NULL
 WHERE id = $1`,
		fixture.runID,
	)
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := fixture.authority.checkRunQueuePreparationConcurrency(
		fixture.ctx,
		tx,
		authority,
	); err != nil {
		t.Fatalf("unbounded queue preparation concurrency: %v", err)
	}
}

func TestRunPlanningUsageCountsActiveQueueState(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	seedDispatchMeasurement(t, fixture, 2, 1, 0, false)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs SET queue_concurrency_limit = 4 WHERE id = $1`, fixture.runID)

	reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runtime_instances
   SET observed_state = 'ready', observed_version = 1,
       observed_desired_version = desired_version,
       preparing_at = transaction_timestamp(), ready_at = transaction_timestamp(),
       observed_at = transaction_timestamp()
 WHERE id = $1`, reserved.RuntimeInstanceID)
	mounting, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts SET state = 'mounted', mounted_at = transaction_timestamp() WHERE id = $1`, mounting.WorkspaceMountID)
	granted, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	if !granted.LeaseCreated {
		t.Fatalf("run lease was not created: %+v", granted)
	}

	queries := db.New(fixture.pool)
	scopes, err := queries.ListQueuedRunEligibleScopes(fixture.ctx, db.ListQueuedRunEligibleScopesParams{
		RowLimit: 10, ScanSeed: "active-queue-usage",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 {
		t.Fatalf("eligible scopes = %d, want 1", len(scopes))
	}
	usage, err := queries.ListQueuedRunPlanningUsage(fixture.ctx, db.ListQueuedRunPlanningUsageParams{
		EnvironmentIds:  []pgtype.UUID{scopes[0].EnvironmentID},
		ConcurrencyKeys: []string{scopes[0].ConcurrencyKey},
		QueueNames:      []string{scopes[0].QueueName},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].ScopeOrdinal != 1 || usage[0].ActiveRuns != 1 || usage[0].ActiveLimit != 4 ||
		usage[0].PreparedRuns != 0 || usage[0].PreparedLimit != 0 {
		t.Fatalf("queue usage = %+v", usage)
	}
}

func TestQueuedRunCandidateBatchPreservesScopeAndQueueOrder(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	seedDispatchMeasurement(t, fixture, 7, 2, 0, false)
	scopes := []QueueScope{
		{
			OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
			EnvironmentID: pgvalue.UUID(fixture.environmentID), RegionID: "us-east-1",
			ConcurrencyKey: "key-0001", QueueName: "measure-0001",
		},
		{
			OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
			EnvironmentID: pgvalue.UUID(fixture.environmentID), RegionID: "us-east-1",
			QueueName: "measure-0000",
		},
	}
	params, err := runCandidateParams(scopes, 3)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.New(fixture.pool).ListQueuedRunDispatchCandidatesForScopes(fixture.ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 {
		t.Fatalf("candidate rows = %d, want 6", len(rows))
	}
	for index, row := range rows {
		wantOrdinal := int64(index/3 + 1)
		if row.ScopeOrdinal != wantOrdinal {
			t.Fatalf("row %d scope ordinal = %d, want %d", index, row.ScopeOrdinal, wantOrdinal)
		}
	}
	for scopeIndex, scope := range scopes {
		queryRows, err := fixture.pool.Query(fixture.ctx, `
SELECT id
  FROM runs
 WHERE environment_id = $1
   AND queue_name = $2
   AND concurrency_key IS NOT DISTINCT FROM $3::text
   AND status = 'queued'
 ORDER BY queue_score_at, id
 LIMIT 3`, scope.EnvironmentID, scope.QueueName, optionalString(scope.ConcurrencyKey))
		if err != nil {
			t.Fatal(err)
		}
		var expected []pgtype.UUID
		for queryRows.Next() {
			var id pgtype.UUID
			if err := queryRows.Scan(&id); err != nil {
				queryRows.Close()
				t.Fatal(err)
			}
			expected = append(expected, id)
		}
		queryRows.Close()
		if err := queryRows.Err(); err != nil {
			t.Fatal(err)
		}
		for index, id := range expected {
			if rows[scopeIndex*3+index].RunID != id {
				t.Fatalf("scope %d candidate %d = %s, want %s", scopeIndex, index,
					pgvalue.UUIDString(rows[scopeIndex*3+index].RunID), pgvalue.UUIDString(id))
			}
		}
	}

	isolationParams, err := runCandidateParams(scopes[:1], 3)
	if err != nil {
		t.Fatal(err)
	}
	isolationParams.ProjectIds[0] = pgtype.UUID{Bytes: [16]byte{15: 99}, Valid: true}
	isolated, err := db.New(fixture.pool).ListQueuedRunDispatchCandidatesForScopes(fixture.ctx, isolationParams)
	if err != nil {
		t.Fatal(err)
	}
	if len(isolated) != 0 {
		t.Fatalf("mismatched project returned %d candidates", len(isolated))
	}
}

func optionalString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func TestPlaceReadyRunGrantsSameWorkspaceChildOnRetainedRuntime(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	parentCandidate := fixture.candidate()
	reserved, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		parentCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
	mounting, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		parentCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, mounting.WorkspaceMountID)
	parent, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		parentCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}

	var parentWorkspaceLeaseID, originalVersionID, taskDefinitionID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT workspace_leases.id,
       workspace_leases.base_version_id,
       runs.deployment_definition_id
  FROM workspace_leases
  JOIN runs ON runs.id = $2
 WHERE workspace_leases.owner_run_lease_id = $1`,
		parent.Lease.ID,
		fixture.runID,
	).Scan(
		&parentWorkspaceLeaseID,
		&originalVersionID,
		&taskDefinitionID,
	); err != nil {
		t.Fatal(err)
	}

	claimID := uuid.Must(uuid.NewV7())
	waitID := uuid.Must(uuid.NewV7())
	checkpointID := uuid.Must(uuid.NewV7())
	resumeAttachID := uuid.Must(uuid.NewV7())
	childID := uuid.Must(uuid.NewV7())
	privateVersionID := uuid.Must(uuid.NewV7())
	privateArtifactID := uuid.Must(uuid.NewV7())
	privateDigest := "sha256:" + strings.Repeat("9", 64)
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(
		fixture.ctx,
		`SET CONSTRAINTS ALL DEFERRED`,
	); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash,
    request_fingerprint, accepted_at
) VALUES (
    $1, $2, 'task.child.invoke', decode(repeat('12', 32), 'hex'),
    decode(repeat('14', 32), 'hex'), now()
)`,
		claimID,
		fixture.environmentID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES ($1, $2, 1, $3)`,
		fixture.orgID,
		privateDigest,
		workspace.ArtifactMediaType,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind,
    size_bytes, media_type
) VALUES (
    $1, $2, $3, $4, $5, 'workspace_version', 1, $6
)`,
		privateArtifactID,
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		privateDigest,
		workspace.ArtifactMediaType,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id,
    parent_version_id, artifact_id, artifact_kind, kind, content_digest,
    size_bytes, entry_count, state, source_workspace_lease_id,
    ownership_generation, writer_generation
) VALUES (
    $1, $2, $3, $4, $5, 'workspace_version',
    'user', $6, 1, 1, 'private', $7, 1, 1
)`,
		privateVersionID,
		fixture.environmentID,
		fixture.workspaceID,
		originalVersionID,
		privateArtifactID,
		privateDigest,
		parentWorkspaceLeaseID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO runs (
    id, org_id, project_id, environment_id, deployment_id,
    deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
    cause_kind, parent_run_id, parent_owns_lifecycle, workspace_id,
    base_workspace_version_id, payload, queue_name, queue_origin_at,
    queue_score_at, max_active_duration_ms, retry_policy, trace_id,
    root_span_id, claim_id
) VALUES (
    $1, $2, $3, $4, $5, $6, 'task', 'test-task', 'child',
    $7, true, $8, $9, '{}'::jsonb, 'default', now(), now(),
    300000, '{"enabled":false}'::jsonb,
    '33333333333333333333333333333333', '4444444444444444', $10
)`,
		childID,
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		fixture.deploymentID,
		taskDefinitionID,
		fixture.runID,
		fixture.workspaceID,
		privateVersionID,
		claimID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id,
    base_workspace_version_id
) VALUES ($1, 1, 'task', $2, $3)`,
		childID,
		fixture.workspaceID,
		privateVersionID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind, child_run_id,
    child_parent_owned, child_target_declared_id, child_claim_id,
    child_request, expected_run_state_version, attempt_number,
    prior_run_lease_id, resume_attach_id, suspension_state
) VALUES (
    $1, $2, $3, $4, 'child', $5, true, 'test-task', $6,
    '{"Method":"call"}'::jsonb, 3, 1, $7, $8, 'parked'
)`,
		waitID,
		fixture.environmentID,
		fixture.runID,
		fixture.workspaceID,
		childID,
		claimID,
		parent.Lease.ID,
		resumeAttachID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_checkpoints (
    id, kind, run_id, attempt_number, run_wait_id,
    source_run_lease_id, source_workspace_lease_id, workspace_id,
    base_workspace_version_id, private_workspace_version_id,
    state, restore_manifest, ready_request_fingerprint, ready_at
) VALUES (
    $1, 'suspend', $2, 1, $3, $4, $5, $6, $7, $8,
    'ready', '{"kind":"suspend"}'::jsonb, 'test-ready', now()
)`,
		checkpointID,
		fixture.runID,
		waitID,
		parent.Lease.ID,
		parentWorkspaceLeaseID,
		fixture.workspaceID,
		originalVersionID,
		privateVersionID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_waits
   SET suspend_checkpoint_id = $2,
       base_workspace_version_id = $3,
       base_workspace_content_digest = $4,
       handoff_runtime_instance_id = $5,
       handoff_workspace_mount_id = $6,
       handoff_mount_generation = 2,
       ownership_generation = 1,
       parent_writer_generation = 1
 WHERE id = $1`,
		waitID,
		checkpointID,
		privateVersionID,
		privateDigest,
		reserved.RuntimeInstanceID,
		mounting.WorkspaceMountID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runs
   SET status = 'waiting', state_version = 3,
       current_run_lease_id = NULL, active_started_at = NULL
 WHERE id = $1`,
		fixture.runID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_leases
   SET state = 'checkpointed', claimed_at = assigned_at,
       started_at = assigned_at, checkpointed_at = now(),
       terminal_at = now(), terminal_reason_code = 'checkpointed'
 WHERE id = $1`,
		parent.Lease.ID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspace_leases
   SET state = 'released', released_at = now(), terminal_at = now()
 WHERE id = $1`,
		parentWorkspaceLeaseID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspace_mounts
   SET materialized_version_id = $2, dirty_generation = 1
 WHERE id = $1`,
		mounting.WorkspaceMountID,
		privateVersionID,
	)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}

	childCandidate := ReadyRunCandidate{
		OrgID:                   pgvalue.UUID(fixture.orgID),
		RunID:                   pgvalue.UUID(childID),
		ExpectedRunStateVersion: 1,
	}
	if _, err := db.New(fixture.pool).GetQueuedRunReadyHint(
		fixture.ctx,
		db.GetQueuedRunReadyHintParams{
			OrgID: pgvalue.UUID(fixture.orgID),
			RunID: pgvalue.UUID(childID),
		},
	); err != nil {
		t.Fatalf("same-Workspace child ready hint: %v", err)
	}
	dispatchCandidates, err := db.New(fixture.pool).ListQueuedRunDispatchCandidatesForScopes(
		fixture.ctx,
		mustRunCandidateParams(t, QueueScope{
			OrgID:          pgvalue.UUID(fixture.orgID),
			ProjectID:      pgvalue.UUID(fixture.projectID),
			EnvironmentID:  pgvalue.UUID(fixture.environmentID),
			RegionID:       "us-east-1",
			ConcurrencyKey: "",
			QueueName:      "default",
		}, 10),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(dispatchCandidates) != 1 ||
		dispatchCandidates[0].RunID != pgvalue.UUID(childID) ||
		!dispatchCandidates[0].RequiresRetainedRuntime {
		t.Fatalf("same-Workspace dispatch candidates = %+v", dispatchCandidates)
	}
	granted, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		childCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !granted.LeaseCreated ||
		granted.RuntimeInstanceID != reserved.RuntimeInstanceID ||
		granted.WorkspaceMountID != mounting.WorkspaceMountID {
		t.Fatalf("same-Workspace child placement = %+v", granted)
	}
	var ownerRunID, currentLeaseID pgtype.UUID
	var writerGeneration, mountGeneration int64
	var waitChildWriter, waitMountGeneration pgtype.Int8
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT workspaces.owner_run_id,
       workspaces.writer_generation,
       workspace_mounts.fencing_generation,
       child.current_run_lease_id,
       handoff.child_writer_generation,
       handoff.handoff_mount_generation
  FROM workspaces
  JOIN workspace_mounts
    ON workspace_mounts.id = $2
  JOIN runs AS child
    ON child.id = $3
  JOIN run_waits AS handoff
    ON handoff.id = $4
 WHERE workspaces.id = $1`,
		fixture.workspaceID,
		mounting.WorkspaceMountID,
		childID,
		waitID,
	).Scan(
		&ownerRunID,
		&writerGeneration,
		&mountGeneration,
		&currentLeaseID,
		&waitChildWriter,
		&waitMountGeneration,
	); err != nil {
		t.Fatal(err)
	}
	if ownerRunID != pgvalue.UUID(fixture.runID) ||
		currentLeaseID != granted.Lease.ID ||
		writerGeneration != 2 ||
		mountGeneration != 3 ||
		!waitChildWriter.Valid ||
		waitChildWriter.Int64 != 2 ||
		!waitMountGeneration.Valid ||
		waitMountGeneration.Int64 != 2 {
		t.Fatalf(
			"owner=%s lease=%s writer=%d mount=%d waitWriter=%v waitMount=%v",
			pgvalue.UUIDString(ownerRunID),
			pgvalue.UUIDString(currentLeaseID),
			writerGeneration,
			mountGeneration,
			waitChildWriter,
			waitMountGeneration,
		)
	}
	var priorLeaseMountGeneration, childLeaseMountGeneration int64
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT parent_lease.mount_fencing_generation,
       child_lease.mount_fencing_generation
  FROM workspace_leases AS parent_lease
  JOIN workspace_leases AS child_lease
    ON child_lease.owner_run_lease_id = $2
 WHERE parent_lease.id = $1`,
		parentWorkspaceLeaseID,
		granted.Lease.ID,
	).Scan(&priorLeaseMountGeneration, &childLeaseMountGeneration); err != nil {
		t.Fatal(err)
	}
	if priorLeaseMountGeneration != 2 ||
		childLeaseMountGeneration != mountGeneration {
		t.Fatalf(
			"Workspace Lease mount receipts = parent:%d child:%d current:%d",
			priorLeaseMountGeneration,
			childLeaseMountGeneration,
			mountGeneration,
		)
	}

	locators, err := db.New(fixture.pool).GetRunLeaseClaimLocators(
		fixture.ctx,
		db.GetRunLeaseClaimLocatorsParams{
			ID:               granted.Lease.ID,
			LeaseSequence:    granted.Lease.LeaseSequence,
			WorkerGroupID:    fixture.groupID,
			WorkerInstanceID: granted.Lease.WorkerInstanceID,
			WorkerEpoch:      granted.Lease.WorkerEpoch,
		},
	)
	if err != nil {
		t.Fatalf("same-Workspace child claim locators: %v", err)
	}
	if locators.RunWaitID.Valid ||
		locators.EnclosingWaitID != pgvalue.UUID(waitID) ||
		locators.EnclosingSuspendCheckpointID != pgvalue.UUID(checkpointID) ||
		locators.EnclosingResumeAttachID != pgvalue.UUID(resumeAttachID) ||
		locators.EnclosingRuntimeInstanceID != granted.RuntimeInstanceID ||
		locators.EnclosingWorkspaceMountID != granted.WorkspaceMountID ||
		!locators.EnclosingMountGeneration.Valid ||
		locators.EnclosingMountGeneration.Int64 != priorLeaseMountGeneration ||
		!locators.EnclosingChildWriterGeneration.Valid ||
		locators.EnclosingChildWriterGeneration.Int64 != writerGeneration {
		t.Fatalf("same-Workspace child claim locators = %+v", locators)
	}

	var childWorkspaceLeaseID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT id
  FROM workspace_leases
 WHERE owner_run_lease_id = $1`, granted.Lease.ID).Scan(&childWorkspaceLeaseID); err != nil {
		t.Fatal(err)
	}
	resultVersionID := uuid.Must(uuid.NewV7())
	resultArtifactID := uuid.Must(uuid.NewV7())
	handoffCheckpointID := uuid.Must(uuid.NewV7())
	resultDigest := "sha256:" + strings.Repeat("8", 64)
	tx, err = fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(fixture.ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES ($1, $2, 2, $3)`,
		fixture.orgID, resultDigest, workspace.ArtifactMediaType,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind,
    size_bytes, media_type
) VALUES ($1, $2, $3, $4, $5, 'workspace_version', 2, $6)`,
		resultArtifactID, fixture.orgID, fixture.projectID, fixture.environmentID,
		resultDigest, workspace.ArtifactMediaType,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id,
    parent_version_id, artifact_id, artifact_kind, kind, content_digest,
    size_bytes, entry_count, state, source_workspace_lease_id,
    ownership_generation, writer_generation
) VALUES (
    $1, $2, $3, $4, $5, 'workspace_version',
    'user', $6, 2, 2, 'private', $7, 1, 2
)`,
		resultVersionID, fixture.environmentID,
		fixture.workspaceID, privateVersionID, resultArtifactID, resultDigest,
		childWorkspaceLeaseID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_checkpoints (
    id, kind, run_id, attempt_number, run_wait_id,
    source_run_lease_id, source_workspace_lease_id, workspace_id,
    base_workspace_version_id, private_workspace_version_id,
    state, restore_manifest, ready_request_fingerprint, ready_at
) VALUES (
    $1, 'handoff_resume', $2, 1, $3, $4, $5, $6, $7, $8,
    'ready', '{"kind":"handoff_resume"}'::jsonb, 'test-handoff-ready', now()
)`,
		handoffCheckpointID, fixture.runID, waitID, parent.Lease.ID,
		parentWorkspaceLeaseID, fixture.workspaceID, originalVersionID,
		resultVersionID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_attempts
   SET entrypoint_entered_at = now(), terminal_outcome = 'succeeded',
       terminal_reason_code = 'completed', terminal_at = now()
 WHERE run_id = $1 AND number = 1`, childID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runs
   SET status = 'succeeded', output = '{}'::jsonb,
       current_run_lease_id = NULL, terminal_at = now(),
       state_version = state_version + 1
 WHERE id = $1 AND current_run_lease_id = $2`, childID, granted.Lease.ID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_leases
   SET state = 'completed', claimed_at = assigned_at, started_at = assigned_at,
       terminal_at = now(), terminal_reason_code = 'completed'
 WHERE id = $1`, granted.Lease.ID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspace_leases
   SET state = 'released', released_at = now(), terminal_at = now()
 WHERE id = $1`, childWorkspaceLeaseID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspace_mounts
   SET materialized_version_id = $2, dirty_generation = dirty_generation + 1
 WHERE id = $1`, mounting.WorkspaceMountID, resultVersionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runs
   SET status = 'queued', state_version = 4, updated_at = now()
 WHERE id = $1 AND status = 'waiting'`, fixture.runID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_waits
   SET condition_state = 'completed', condition_result = '{}'::jsonb,
       condition_terminal_at = now(), suspension_state = 'resume_pending',
       resume_request_version = 1, expected_run_state_version = 4,
       child_result_version_id = $2, resume_workspace_version_id = $2,
       handoff_resume_checkpoint_id = $3
 WHERE id = $1`, waitID, resultVersionID, handoffCheckpointID)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}

	parentResume, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		ReadyRunCandidate{
			OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID),
			ExpectedRunStateVersion: 4,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !parentResume.LeaseCreated ||
		parentResume.RuntimeInstanceID != reserved.RuntimeInstanceID ||
		parentResume.WorkspaceMountID != mounting.WorkspaceMountID {
		t.Fatalf("same-Workspace parent retained resume = %+v", parentResume)
	}
	var resumeState string
	var resumeWriter pgtype.Int8
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT suspension_state, resume_writer_generation
  FROM run_waits
 WHERE id = $1`, waitID).Scan(&resumeState, &resumeWriter); err != nil {
		t.Fatal(err)
	}
	if resumeState != "resuming" || !resumeWriter.Valid || resumeWriter.Int64 != 3 {
		t.Fatalf("parent resume wait state=%s writer=%v", resumeState, resumeWriter)
	}

	// A retained parent attach that loses its Lease before Run start is safe to
	// regrant on the still-frozen runtime. Recovery clears the old writer
	// generation so the next ordinary admission can bind a fresh one.
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
WITH expired AS (
    UPDATE run_leases
       SET assigned_at = transaction_timestamp() - interval '3 seconds',
           start_deadline_at = transaction_timestamp() - interval '2 seconds',
           expires_at = transaction_timestamp() - interval '1 second'
     WHERE id = $1
    RETURNING id, expires_at
)
UPDATE workspace_leases
   SET expires_at = expired.expires_at
  FROM expired
 WHERE workspace_leases.owner_run_lease_id = expired.id`, parentResume.Lease.ID)
	recovered, err := db.New(fixture.pool).RecoverExpiredRunResumes(fixture.ctx, recoverExpiredRunResumesParams(10))
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].ID != pgvalue.UUID(waitID) {
		var runStatus, waitSuspension, leaseState, leaseReason string
		if scanErr := fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.status, run_waits.suspension_state, run_leases.state,
       COALESCE(run_leases.terminal_reason_code, '')
  FROM runs
  JOIN run_waits ON run_waits.run_id = runs.id
  JOIN run_leases ON run_leases.id = $2
 WHERE run_waits.id = $1`, waitID, parentResume.Lease.ID).Scan(
			&runStatus, &waitSuspension, &leaseState, &leaseReason,
		); scanErr != nil {
			t.Fatal(scanErr)
		}
		var authorityJSON []byte
		if scanErr := fixture.pool.QueryRow(fixture.ctx, `
SELECT jsonb_build_object(
    'runActor', runs.session_id,
    'runAttempt', runs.current_attempt_number,
    'runLease', runs.current_run_lease_id,
    'runActive', runs.active_started_at,
    'workspaceOwnerRun', workspaces.owner_run_id,
    'workspaceOwnerActor', workspaces.owner_session_id,
    'workspaceState', workspaces.state,
    'workspaceDesired', workspaces.desired_state,
    'workspaceDirty', workspaces.dirty_state,
    'workspaceWriter', workspaces.writer_generation,
    'workspaceLeaseState', workspace_leases.state,
    'workspaceLeaseWriter', workspace_leases.writer_generation,
    'workspaceLeaseBase', workspace_leases.base_version_id,
    'mountState', workspace_mounts.state,
    'mountGeneration', workspace_mounts.fencing_generation,
    'runtimeRestore', runtime_instances.restore_checkpoint_id,
    'runtimeReclaimed', runtime_instances.reclaimed_at,
    'waitCondition', run_waits.condition_state,
    'waitHandoffRuntime', run_waits.handoff_runtime_instance_id,
    'waitHandoffMount', run_waits.handoff_workspace_mount_id,
    'waitHandoffGeneration', run_waits.handoff_mount_generation,
    'waitResumeWriter', run_waits.resume_writer_generation,
    'waitResumeVersion', run_waits.resume_workspace_version_id,
    'waitCheckpoint', run_waits.handoff_resume_checkpoint_id,
    'leaseExpired', run_leases.expires_at <= transaction_timestamp(),
    'startExpired', run_leases.start_deadline_at <= transaction_timestamp()
)
  FROM runs
  JOIN workspaces ON workspaces.id = runs.workspace_id
  JOIN run_waits ON run_waits.run_id = runs.id
  JOIN run_leases ON run_leases.id = $2
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
  JOIN workspace_mounts ON workspace_mounts.id = workspace_leases.workspace_mount_id
  JOIN runtime_instances ON runtime_instances.id = run_leases.runtime_instance_id
 WHERE run_waits.id = $1`, waitID, parentResume.Lease.ID).Scan(&authorityJSON); scanErr != nil {
			t.Fatal(scanErr)
		}
		t.Fatalf("retained pre-start recovery = %+v; run=%s wait=%s lease=%s reason=%s authority=%s",
			recovered, runStatus, waitSuspension, leaseState, leaseReason, authorityJSON)
	}
	var recoveredRunVersion int64
	var recoveredWriter pgtype.Int8
	var retainedDesiredState string
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.state_version, run_waits.resume_writer_generation,
       runtime_instances.desired_state
  FROM runs
  JOIN run_waits ON run_waits.run_id = runs.id
  JOIN runtime_instances ON runtime_instances.id = $2
 WHERE run_waits.id = $1`, waitID, parentResume.RuntimeInstanceID).Scan(
		&recoveredRunVersion, &recoveredWriter, &retainedDesiredState,
	); err != nil {
		t.Fatal(err)
	}
	if recoveredWriter.Valid || retainedDesiredState != "ready" {
		t.Fatalf("retained pre-start writer=%v runtime=%s", recoveredWriter, retainedDesiredState)
	}

	regranted, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		ReadyRunCandidate{
			OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID),
			ExpectedRunStateVersion: recoveredRunVersion,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !regranted.LeaseCreated ||
		regranted.RuntimeInstanceID != parentResume.RuntimeInstanceID {
		t.Fatalf("retained parent regrant = %+v", regranted)
	}

	// Once Run start commits, guest release may have been applied. The same
	// retained VM is no longer replay-safe; recovery closes it and requeues from
	// the authoritative handoff-resume checkpoint.
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'running', claimed_at = transaction_timestamp(),
       started_at = transaction_timestamp()
 WHERE id = $1 AND state = 'assigned'`, regranted.Lease.ID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET status = 'running', started_at = COALESCE(started_at, transaction_timestamp()),
       active_started_at = transaction_timestamp(), state_version = state_version + 1
 WHERE id = $1 AND status = 'queued'`, fixture.runID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
WITH expired AS (
    UPDATE run_leases
       SET assigned_at = transaction_timestamp() - interval '3 seconds',
           start_deadline_at = transaction_timestamp() - interval '2 seconds',
           expires_at = transaction_timestamp() - interval '1 second'
     WHERE id = $1
    RETURNING id, expires_at
)
UPDATE workspace_leases
   SET expires_at = expired.expires_at
  FROM expired
 WHERE workspace_leases.owner_run_lease_id = expired.id`, regranted.Lease.ID)
	recovered, err = db.New(fixture.pool).RecoverExpiredRunResumes(fixture.ctx, recoverExpiredRunResumesParams(10))
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].ID != pgvalue.UUID(waitID) {
		t.Fatalf("retained post-start recovery = %+v", recovered)
	}
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT run_waits.resume_writer_generation, runtime_instances.desired_state
  FROM run_waits
  JOIN runtime_instances ON runtime_instances.id = $2
 WHERE run_waits.id = $1`, waitID, regranted.RuntimeInstanceID).Scan(
		&recoveredWriter, &retainedDesiredState,
	); err != nil {
		t.Fatal(err)
	}
	if recoveredWriter.Valid || retainedDesiredState != "closed" {
		t.Fatalf("retained post-start writer=%v runtime=%s", recoveredWriter, retainedDesiredState)
	}

	reclaimRuntime := func(runtimeID, mountID pgtype.UUID) {
		dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts
   SET state = 'unmounted', unmounted_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(), terminal_reason_code = 'test_reclaimed'
 WHERE id = $1 AND state = 'unmounting'`, mountID)
		dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runtime_instances
   SET observed_state = 'closed', observed_version = observed_version + 1,
       observed_desired_version = desired_version, observed_at = transaction_timestamp(),
       closing_at = COALESCE(closing_at, transaction_timestamp()),
       closed_at = transaction_timestamp(), terminal_at = transaction_timestamp(),
       terminal_reason_code = 'test_reclaimed', reclaimed_at = transaction_timestamp(),
       reclaim_evidence = jsonb_build_object('method', 'session_closed', 'completed_at', to_char(transaction_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')),
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL
 WHERE id = $1 AND desired_state = 'closed'`, runtimeID)
	}
	expireResumeLease := func(leaseID pgtype.UUID) {
		dbtest.MustExec(t, fixture.ctx, fixture.pool, `
WITH expired AS (
    UPDATE run_leases
       SET assigned_at = transaction_timestamp() - interval '3 seconds',
           start_deadline_at = transaction_timestamp() - interval '2 seconds',
           expires_at = transaction_timestamp() - interval '1 second'
     WHERE id = $1
    RETURNING id, expires_at
)
UPDATE workspace_leases
   SET expires_at = expired.expires_at
  FROM expired
 WHERE workspace_leases.owner_run_lease_id = expired.id`, leaseID)
	}
	currentRunVersion := func() int64 {
		var version int64
		if err := fixture.pool.QueryRow(
			fixture.ctx,
			`SELECT state_version FROM runs WHERE id = $1`,
			fixture.runID,
		).Scan(&version); err != nil {
			t.Fatal(err)
		}
		return version
	}
	grantRecreated := func() ReadyRunPlacement {
		candidate := ReadyRunCandidate{
			OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID),
			ExpectedRunStateVersion: currentRunVersion(),
		}
		reservedRestore, err := fixture.authority.PlaceReadyRun(
			fixture.ctx, candidate,
		)
		if err != nil {
			t.Fatal(err)
		}
		markRunPlacementRuntimeReady(t, fixture, reservedRestore.RuntimeInstanceID)
		mountingRestore, err := fixture.authority.PlaceReadyRun(
			fixture.ctx, candidate,
		)
		if err != nil {
			t.Fatal(err)
		}
		markRunPlacementMountReady(t, fixture, mountingRestore.WorkspaceMountID)
		grantedRestore, err := fixture.authority.PlaceReadyRun(
			fixture.ctx, candidate,
		)
		if err != nil {
			t.Fatal(err)
		}
		return grantedRestore
	}

	// After the retained runtime is fenced, successful parent recovery creates
	// a new VM from the handoff-resume checkpoint. Lease loss before start is
	// redriven from that same checkpoint and never reuses the dirty VM.
	reclaimRuntime(regranted.RuntimeInstanceID, regranted.WorkspaceMountID)
	successRestore := grantRecreated()
	var successRestoreCheckpoint pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT restore_checkpoint_id
  FROM runtime_instances
 WHERE id = $1`, successRestore.RuntimeInstanceID).Scan(&successRestoreCheckpoint); err != nil {
		t.Fatal(err)
	}
	if successRestoreCheckpoint != pgvalue.UUID(handoffCheckpointID) {
		t.Fatalf("successful recreated checkpoint = %s, want %s",
			pgvalue.UUIDString(successRestoreCheckpoint), handoffCheckpointID)
	}
	expireResumeLease(successRestore.Lease.ID)
	recovered, err = db.New(fixture.pool).RecoverExpiredRunResumes(fixture.ctx, recoverExpiredRunResumesParams(10))
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].ID != pgvalue.UUID(waitID) {
		t.Fatalf("successful recreated recovery = %+v", recovered)
	}

	// The failure arm uses the same recovery transaction but selects the
	// original suspend checkpoint and base, never the successful handoff
	// checkpoint. This models the terminal child decision after its dirty VM
	// has been fenced.
	reclaimRuntime(successRestore.RuntimeInstanceID, successRestore.WorkspaceMountID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_waits
   SET condition_state = 'failed', condition_result = NULL,
       condition_error = '{"code":"task_failed"}'::jsonb,
       condition_reason_code = 'task_failed',
       child_result_version_id = NULL,
       handoff_resume_checkpoint_id = NULL,
       resume_workspace_version_id = $2,
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND suspension_state = 'resume_pending'
   AND resume_writer_generation IS NULL`, waitID, privateVersionID)
	failureRestore := grantRecreated()
	var failureRestoreCheckpoint pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT restore_checkpoint_id
  FROM runtime_instances
 WHERE id = $1`, failureRestore.RuntimeInstanceID).Scan(&failureRestoreCheckpoint); err != nil {
		t.Fatal(err)
	}
	if failureRestoreCheckpoint != pgvalue.UUID(checkpointID) {
		t.Fatalf("failed recreated checkpoint = %s, want %s",
			pgvalue.UUIDString(failureRestoreCheckpoint), checkpointID)
	}
	expireResumeLease(failureRestore.Lease.ID)
	recovered, err = db.New(fixture.pool).RecoverExpiredRunResumes(fixture.ctx, recoverExpiredRunResumesParams(10))
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].ID != pgvalue.UUID(waitID) {
		t.Fatalf("failed recreated recovery = %+v", recovered)
	}
}

func TestRecoverExpiredNestedRunResume(t *testing.T) {
	for _, tc := range []struct {
		name         string
		physicalLoss bool
		actorRoot    bool
	}{
		{name: "retains frozen handoff before start"},
		{name: "retains Actor-owned handoff before start", actorRoot: true},
		{name: "unwinds lineage after physical loss", physicalLoss: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRunPlacementFixture(t)
			state := prepareNestedResumeRecovery(t, fixture)
			if tc.actorRoot {
				convertNestedResumeRootToActor(t, fixture, state.parentRunID)
			}
			if tc.physicalLoss {
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runtime_instances
   SET observed_state = 'failed', failed_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(),
       terminal_reason_code = 'test_runtime_failed'
 WHERE id = $1`, state.runtimeID)
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts
   SET state = 'failed', failed_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(),
       terminal_reason_code = 'test_mount_failed'
 WHERE id = $1`, state.mountID)
			}

			candidates, err := fixture.authority.listExpiredNestedRunResumes(fixture.ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(candidates) != 1 {
				t.Fatalf("nested resume candidates = %+v, want one", candidates)
			}
			recovered, err := fixture.authority.RecoverExpiredRunResumes(
				fixture.ctx,
				10,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(recovered) != 1 ||
				recovered[0].ID != pgvalue.UUID(state.resumeWaitID) ||
				recovered[0].RunID != pgvalue.UUID(fixture.runID) {
				t.Fatalf("nested resume recovery = %+v", recovered)
			}

			var runStatus, resumeState, enclosingState, runtimeDesired string
			var runLeaseID pgtype.UUID
			var resumeWriter, enclosingWriter pgtype.Int8
			if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.status, runs.current_run_lease_id,
       resume_wait.suspension_state, resume_wait.resume_writer_generation,
       enclosing_wait.suspension_state, enclosing_wait.child_writer_generation,
       runtime_instances.desired_state
  FROM runs
  JOIN run_waits AS resume_wait ON resume_wait.id = $2
  JOIN run_waits AS enclosing_wait ON enclosing_wait.id = $3
  JOIN runtime_instances ON runtime_instances.id = $4
 WHERE runs.id = $1`,
				fixture.runID,
				state.resumeWaitID,
				state.enclosingWaitID,
				state.runtimeID,
			).Scan(
				&runStatus,
				&runLeaseID,
				&resumeState,
				&resumeWriter,
				&enclosingState,
				&enclosingWriter,
				&runtimeDesired,
			); err != nil {
				t.Fatal(err)
			}
			if tc.physicalLoss {
				if runStatus != "system_failed" ||
					runLeaseID.Valid ||
					resumeState != "failed" ||
					enclosingState != "resume_pending" ||
					!enclosingWriter.Valid ||
					runtimeDesired != "closed" {
					t.Fatalf(
						"lost nested recovery run=%s lease=%s resume=%s/%v enclosing=%s/%v runtime=%s",
						runStatus,
						pgvalue.UUIDString(runLeaseID),
						resumeState,
						resumeWriter,
						enclosingState,
						enclosingWriter,
						runtimeDesired,
					)
				}
				var parentStatus string
				if err := fixture.pool.QueryRow(
					fixture.ctx,
					`SELECT status FROM runs WHERE id = $1`,
					state.parentRunID,
				).Scan(&parentStatus); err != nil {
					t.Fatal(err)
				}
				if parentStatus != "queued" {
					t.Fatalf("outer parent status = %s, want queued", parentStatus)
				}
			} else if runStatus != "queued" ||
				runLeaseID.Valid ||
				resumeState != "resume_pending" ||
				resumeWriter.Valid ||
				enclosingState != "parked" ||
				enclosingWriter.Valid ||
				runtimeDesired != "ready" {
				t.Fatalf(
					"retained nested recovery run=%s lease=%s resume=%s/%v enclosing=%s/%v runtime=%s",
					runStatus,
					pgvalue.UUIDString(runLeaseID),
					resumeState,
					resumeWriter,
					enclosingState,
					enclosingWriter,
					runtimeDesired,
				)
			}
		})
	}
}

func TestRecoverExpiredNestedRunResumeRejectsCheckpointProvenanceDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(t *testing.T, fixture runPlacementFixture, state nestedResumeRecoveryState)
	}{
		{
			name: "handoff resume kind",
			tamper: func(t *testing.T, fixture runPlacementFixture, state nestedResumeRecoveryState) {
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_checkpoints
   SET kind = 'suspend'
 WHERE id = $1`, state.resumeCheckpointID)
			},
		},
		{
			name: "current source Workspace Lease writer",
			tamper: func(t *testing.T, fixture runPlacementFixture, state nestedResumeRecoveryState) {
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_leases
   SET writer_generation = 4
 WHERE id = $1`, state.resumeSourceWorkspaceLeaseID)
			},
		},
		{
			name: "current resume Workspace Lease ownership",
			tamper: func(t *testing.T, fixture runPlacementFixture, state nestedResumeRecoveryState) {
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_leases
   SET ownership_generation = ownership_generation + 1
 WHERE id = $1`, state.currentWorkspaceLeaseID)
			},
		},
		{
			name: "current resume Workspace Lease base",
			tamper: func(t *testing.T, fixture runPlacementFixture, state nestedResumeRecoveryState) {
				versionID := uuid.Must(uuid.NewV7())
				artifactID := uuid.Must(uuid.NewV7())
				digest := "sha256:" + strings.Repeat("7", 64)
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES ($1, $2, 1, $3)`,
					fixture.orgID,
					digest,
					workspace.ArtifactMediaType,
				)
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind,
    size_bytes, media_type
) VALUES ($1, $2, $3, $4, $5, 'workspace_version', 1, $6)`,
					artifactID,
					fixture.orgID,
					fixture.projectID,
					fixture.environmentID,
					digest,
					workspace.ArtifactMediaType,
				)
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id,
    parent_version_id, artifact_id, artifact_kind, kind,
    content_digest, size_bytes, entry_count, state,
    source_workspace_lease_id, ownership_generation, writer_generation
)
SELECT $1, source.environment_id, source.workspace_id,
       source.base_version_id, $2, 'workspace_version',
       'user', $3, 1, 1, 'private', source.id,
       source.ownership_generation, source.writer_generation
  FROM workspace_leases AS source
 WHERE source.id = $4`,
					versionID,
					artifactID,
					digest,
					state.currentWorkspaceLeaseID,
				)
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_leases
   SET base_version_id = $2
 WHERE id = $1`, state.currentWorkspaceLeaseID, versionID)
			},
		},
		{
			name: "outer source Workspace Lease writer",
			tamper: func(t *testing.T, fixture runPlacementFixture, state nestedResumeRecoveryState) {
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_leases
   SET writer_generation = 4
 WHERE id = $1`, state.parentSourceWorkspaceLeaseID)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRunPlacementFixture(t)
			state := prepareNestedResumeRecovery(t, fixture)
			tc.tamper(t, fixture, state)

			recovered, err := fixture.authority.RecoverExpiredRunResumes(fixture.ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(recovered) != 0 {
				t.Fatalf("recovered tampered nested resume = %+v", recovered)
			}

			var status, suspension string
			var leaseID pgtype.UUID
			if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.status, runs.current_run_lease_id, run_waits.suspension_state
  FROM runs
  JOIN run_waits ON run_waits.id = $2
 WHERE runs.id = $1`,
				fixture.runID,
				state.resumeWaitID,
			).Scan(&status, &leaseID, &suspension); err != nil {
				t.Fatal(err)
			}
			if status != "queued" || !leaseID.Valid || suspension != "resuming" {
				t.Fatalf(
					"tampered resume changed state: run=%s lease=%s wait=%s",
					status,
					pgvalue.UUIDString(leaseID),
					suspension,
				)
			}
		})
	}
}

func convertNestedResumeRootToActor(
	t *testing.T,
	fixture runPlacementFixture,
	runID uuid.UUID,
) {
	t.Helper()
	actorID := uuid.Must(uuid.NewV7())
	definitionID := uuid.Must(uuid.NewV7())
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
ALTER TABLE run_attempts
ALTER CONSTRAINT run_attempts_run_id_entrypoint_kind_workspace_id_fkey
DEFERRABLE INITIALLY DEFERRED`)
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, fixture.ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO deployment_definitions (
    id, environment_id, deployment_id, kind, declared_id,
    manifest_version, manifest, manifest_digest
) VALUES (
    $1, $2, $3, 'actor', 'test-actor', 0, '{}'::jsonb,
    decode(repeat('61', 32), 'hex')
)`,
		definitionID,
		fixture.environmentID,
		fixture.deploymentID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO sessions (
    id, environment_id,
    actor_declared_id, deployment_definition_id, workspace_id,
    current_run_id, next_input_sequence, committed_input_sequence,
    run_queue_name, run_max_active_duration_ms
) VALUES (
    $1, $2, 'test-actor', $3, $4, $5, 2, 1, 'default', 300000
)`,
		actorID,
		fixture.environmentID,
		definitionID,
		fixture.workspaceID,
		runID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runs
   SET deployment_definition_id = $2,
       entrypoint_kind = 'actor',
       entrypoint_declared_id = 'test-actor',
       session_id = $3,
       cause_kind = 'actor_start',
       session_input_start_sequence = 1,
       session_input_high_watermark = 1,
       payload = NULL
 WHERE id = $1`,
		runID,
		definitionID,
		actorID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_attempts
   SET entrypoint_kind = 'actor', session_input_start_sequence = 1
 WHERE run_id = $1 AND number = 1`,
		runID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspaces
   SET owner_run_id = NULL, owner_session_id = $2
 WHERE id = $1`,
		fixture.workspaceID,
		actorID,
	)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
}

type nestedResumeRecoveryState struct {
	parentRunID                  uuid.UUID
	enclosingWaitID              uuid.UUID
	resumeWaitID                 uuid.UUID
	resumeCheckpointID           uuid.UUID
	parentSourceWorkspaceLeaseID uuid.UUID
	resumeSourceWorkspaceLeaseID uuid.UUID
	currentWorkspaceLeaseID      pgtype.UUID
	runtimeID                    pgtype.UUID
	mountID                      pgtype.UUID
}

func prepareNestedResumeRecovery(
	t *testing.T,
	fixture runPlacementFixture,
) nestedResumeRecoveryState {
	t.Helper()
	candidate := fixture.candidate()
	reserved, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		candidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
	mounting, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		candidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, mounting.WorkspaceMountID)
	granted, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		candidate,
	)
	if err != nil {
		t.Fatal(err)
	}

	var workspaceLeaseID, baseVersionID, definitionID pgtype.UUID
	var baseDigest string
	var writerGeneration, mountGeneration int64
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT workspace_leases.id, workspace_leases.base_version_id,
       runs.deployment_definition_id, workspaces.writer_generation,
       workspace_mounts.fencing_generation, workspace_versions.content_digest
  FROM workspace_leases
  JOIN runs ON runs.id = $2
  JOIN workspaces ON workspaces.id = runs.workspace_id
  JOIN workspace_mounts ON workspace_mounts.id = workspace_leases.workspace_mount_id
  JOIN workspace_versions
    ON workspace_versions.id = workspace_leases.base_version_id
   AND workspace_versions.workspace_id = workspace_leases.workspace_id
 WHERE workspace_leases.owner_run_lease_id = $1`,
		granted.Lease.ID,
		fixture.runID,
	).Scan(
		&workspaceLeaseID,
		&baseVersionID,
		&definitionID,
		&writerGeneration,
		&mountGeneration,
		&baseDigest,
	); err != nil {
		t.Fatal(err)
	}

	parentRunID := uuid.Must(uuid.NewV7())
	childRunID := uuid.Must(uuid.NewV7())
	enclosingWaitID := uuid.Must(uuid.NewV7())
	resumeWaitID := uuid.Must(uuid.NewV7())
	enclosingCheckpointID := uuid.Must(uuid.NewV7())
	resumeSuspendID := uuid.Must(uuid.NewV7())
	resumeCheckpointID := uuid.Must(uuid.NewV7())
	enclosingAttachID := uuid.Must(uuid.NewV7())
	resumeAttachID := uuid.Must(uuid.NewV7())
	currentClaimID := uuid.Must(uuid.NewV7())
	childClaimID := uuid.Must(uuid.NewV7())
	parentLeaseID := uuid.Must(uuid.NewV7())
	parentWorkspaceLeaseID := uuid.Must(uuid.NewV7())
	resumeSourceLeaseID := uuid.Must(uuid.NewV7())
	resumeSourceWorkspaceLeaseID := uuid.Must(uuid.NewV7())

	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(fixture.ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	for index, claimID := range []uuid.UUID{currentClaimID, childClaimID} {
		dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash,
    request_fingerprint, accepted_at
) VALUES (
    $1, $2, 'task.child.invoke',
    decode(repeat($3::text, 32), 'hex'),
    decode(repeat($4::text, 32), 'hex'), now()
)`,
			claimID,
			fixture.environmentID,
			fmt.Sprintf("%02x", 31+index),
			fmt.Sprintf("%02x", 51+index),
		)
	}
	for _, run := range []struct {
		id       uuid.UUID
		parentID any
		claimID  any
		status   string
	}{
		{id: parentRunID, status: "waiting"},
		{
			id: childRunID, parentID: fixture.runID,
			claimID: childClaimID, status: "succeeded",
		},
	} {
		dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO runs (
    id, org_id, project_id, environment_id, deployment_id,
    deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
    cause_kind, parent_run_id, parent_owns_lifecycle, workspace_id,
    base_workspace_version_id, payload, queue_name, queue_origin_at,
    queue_score_at, max_active_duration_ms, retry_policy, trace_id,
    root_span_id, claim_id, status, state_version,
    terminal_at
) VALUES (
    $1, $2, $3, $4, $5, $6, 'task', 'test-task',
    CASE WHEN $7::uuid IS NULL THEN 'api' ELSE 'child' END,
    $7, CASE WHEN $7::uuid IS NULL THEN NULL ELSE true END,
    $8, $9, '{}'::jsonb, 'default',
    now(), now(), 300000, '{"enabled":false}'::jsonb,
    '55555555555555555555555555555555', '6666666666666666',
    $10, $11::text, 2,
    CASE WHEN $11::text = 'succeeded' THEN now() ELSE NULL END
)`,
			run.id,
			fixture.orgID,
			fixture.projectID,
			fixture.environmentID,
			fixture.deploymentID,
			definitionID,
			run.parentID,
			fixture.workspaceID,
			baseVersionID,
			run.claimID,
			run.status,
		)
		dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id,
    base_workspace_version_id, terminal_outcome, terminal_reason_code,
    terminal_at
) VALUES (
    $1, 1, 'task', $2, $3,
    CASE WHEN $4 = 'succeeded' THEN 'succeeded' END,
    CASE WHEN $4 = 'succeeded' THEN 'completed' END,
    CASE WHEN $4 = 'succeeded' THEN now() END
)`,
			run.id,
			fixture.workspaceID,
			baseVersionID,
			run.status,
		)
	}
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspaces
   SET writer_generation = 3
 WHERE id = $1 AND writer_generation = 1`, fixture.workspaceID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspace_leases
   SET writer_generation = 3
 WHERE id = $1 AND writer_generation = 1`, workspaceLeaseID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_leases
   SET lease_sequence = 2
 WHERE id = $1 AND lease_sequence = 1`, granted.Lease.ID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_leases (
    id, org_id, project_id, environment_id, run_id, workspace_id,
    region_id, lease_sequence, attempt_number, worker_group_id,
    worker_instance_id, worker_epoch, runtime_instance_id, runtime_identity_id,
    requested_cpu_millis, requested_memory_bytes,
    requested_guest_ephemeral_disk_bytes,
    requested_execution_slots, trace_id, span_id, state, assigned_at,
    start_deadline_at, claimed_at, started_at, expires_at,
    checkpointed_at, terminal_at, terminal_reason_code
)
SELECT $1, org_id, project_id, environment_id, $2, workspace_id,
       region_id, 1, 1, worker_group_id, worker_instance_id, worker_epoch,
       runtime_instance_id,
       runtime_identity_id, requested_cpu_millis,
       requested_memory_bytes, requested_guest_ephemeral_disk_bytes,
       requested_execution_slots, trace_id, span_id,
       'checkpointed', assigned_at, start_deadline_at, assigned_at, assigned_at,
       expires_at, transaction_timestamp(), transaction_timestamp(),
       'checkpointed'
  FROM run_leases
 WHERE id = $3`,
		parentLeaseID,
		parentRunID,
		granted.Lease.ID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_leases (
    id, org_id, worker_group_id, project_id, environment_id, region_id,
    worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
    workspace_mount_id, state, owner_run_lease_id, base_version_id,
    ownership_generation, writer_generation, mount_fencing_generation,
    fencing_token_hash, acquired_at, renewed_at,
    expires_at, released_at, terminal_at
)
SELECT $1, org_id, worker_group_id, project_id, environment_id, region_id,
       worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
       workspace_mount_id, 'released', $2, base_version_id,
       ownership_generation, 1, $4, fencing_token_hash,
       acquired_at, renewed_at, expires_at,
       transaction_timestamp(), transaction_timestamp()
  FROM workspace_leases
 WHERE id = $3`,
		parentWorkspaceLeaseID,
		parentLeaseID,
		workspaceLeaseID,
		mountGeneration-1,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_leases (
    id, org_id, project_id, environment_id, run_id, workspace_id,
    region_id, lease_sequence, attempt_number, worker_group_id,
    worker_instance_id, worker_epoch, runtime_instance_id, runtime_identity_id,
    requested_cpu_millis, requested_memory_bytes,
    requested_guest_ephemeral_disk_bytes,
    requested_execution_slots, trace_id, span_id, state, assigned_at,
    start_deadline_at, claimed_at, started_at, expires_at,
    checkpointed_at, terminal_at, terminal_reason_code
)
SELECT $1, org_id, project_id, environment_id, $2, workspace_id,
       region_id, lease_sequence - 1, 1, worker_group_id, worker_instance_id,
       worker_epoch, runtime_instance_id, runtime_identity_id,
       requested_cpu_millis, requested_memory_bytes,
       requested_guest_ephemeral_disk_bytes,
       requested_execution_slots, trace_id, span_id, 'checkpointed',
       assigned_at, start_deadline_at, assigned_at, assigned_at, expires_at,
       transaction_timestamp(), transaction_timestamp(), 'checkpointed'
  FROM run_leases
 WHERE id = $3`,
		resumeSourceLeaseID,
		fixture.runID,
		granted.Lease.ID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_leases (
    id, org_id, worker_group_id, project_id, environment_id, region_id,
    worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
    workspace_mount_id, state, owner_run_lease_id, base_version_id,
    ownership_generation, writer_generation, mount_fencing_generation,
    fencing_token_hash, acquired_at, renewed_at,
    expires_at, released_at, terminal_at
)
SELECT $1, org_id, worker_group_id, project_id, environment_id, region_id,
       worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
       workspace_mount_id, 'released', $2, base_version_id,
       ownership_generation, 2, $4, fencing_token_hash,
       acquired_at, renewed_at, expires_at,
       transaction_timestamp(), transaction_timestamp()
  FROM workspace_leases
 WHERE id = $3`,
		resumeSourceWorkspaceLeaseID,
		resumeSourceLeaseID,
		workspaceLeaseID,
		mountGeneration-1,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runs
   SET cause_kind = 'child', parent_run_id = $2,
       parent_owns_lifecycle = true, claim_id = $3,
       state_version = 2
 WHERE id = $1`, fixture.runID, parentRunID, currentClaimID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspaces
   SET owner_run_id = $2
 WHERE id = $1`, fixture.workspaceID, parentRunID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind, child_run_id,
    child_parent_owned, child_target_declared_id, child_claim_id, child_request,
    condition_state, suspension_state, expected_run_state_version,
    attempt_number, prior_run_lease_id, resume_attach_id
) VALUES (
    $1, $2, $3, $4, 'child', $5, true, 'test-task', $6,
    '{"Method":"call"}'::jsonb, 'pending', 'parked', 2, 1, $7, $8
)`,
		enclosingWaitID,
		fixture.environmentID,
		parentRunID,
		fixture.workspaceID,
		fixture.runID,
		currentClaimID,
		parentLeaseID,
		enclosingAttachID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind, child_run_id,
    child_parent_owned, child_target_declared_id, child_claim_id, child_request,
    condition_state, suspension_state, expected_run_state_version,
    attempt_number, prior_run_lease_id, resume_attach_id
) VALUES (
    $1, $2, $3, $4, 'child', $5, true, 'test-task', $6,
    '{"Method":"call"}'::jsonb, 'pending', 'parked', 2, 1, $7, $8
)`,
		resumeWaitID,
		fixture.environmentID,
		fixture.runID,
		fixture.workspaceID,
		childRunID,
		childClaimID,
		resumeSourceLeaseID,
		resumeAttachID,
	)
	for _, checkpoint := range []struct {
		id                uuid.UUID
		kind              string
		runID             any
		waitID            any
		sourceLeaseID     any
		sourceWorkspaceID any
	}{
		{
			id: enclosingCheckpointID, kind: "suspend",
			runID: parentRunID, waitID: enclosingWaitID,
			sourceLeaseID: parentLeaseID, sourceWorkspaceID: parentWorkspaceLeaseID,
		},
		{
			id: resumeSuspendID, kind: "suspend",
			runID: fixture.runID, waitID: resumeWaitID,
			sourceLeaseID: resumeSourceLeaseID, sourceWorkspaceID: resumeSourceWorkspaceLeaseID,
		},
		{
			id: resumeCheckpointID, kind: "handoff_resume",
			runID: fixture.runID, waitID: resumeWaitID,
			sourceLeaseID: resumeSourceLeaseID, sourceWorkspaceID: resumeSourceWorkspaceLeaseID,
		},
	} {
		dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_checkpoints (
    id, kind, run_id, attempt_number, run_wait_id,
    source_run_lease_id, source_workspace_lease_id, workspace_id,
    base_workspace_version_id, private_workspace_version_id,
    state, restore_manifest, ready_request_fingerprint, ready_at
) VALUES (
    $1, $2, $3, 1, $4, $5, $6, $7, $8, $8,
    'ready', '{"kind":"test"}'::jsonb, 'test-ready', now()
)`,
			checkpoint.id,
			checkpoint.kind,
			checkpoint.runID,
			checkpoint.waitID,
			checkpoint.sourceLeaseID,
			checkpoint.sourceWorkspaceID,
			fixture.workspaceID,
			baseVersionID,
		)
	}
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_waits
   SET suspend_checkpoint_id = $2,
       base_workspace_version_id = $3,
       base_workspace_content_digest = $4,
       handoff_runtime_instance_id = $5,
       handoff_workspace_mount_id = $6,
       handoff_mount_generation = $7,
       ownership_generation = 1,
       parent_writer_generation = 1,
       child_writer_generation = 3
 WHERE id = $1`,
		enclosingWaitID,
		enclosingCheckpointID,
		baseVersionID,
		baseDigest,
		granted.RuntimeInstanceID,
		granted.WorkspaceMountID,
		mountGeneration-1,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_waits
   SET condition_state = 'completed',
       condition_result = '{}'::jsonb,
       condition_terminal_at = transaction_timestamp(),
       suspension_state = 'resuming',
       current_run_lease_id = $2,
       suspend_checkpoint_id = $3,
       handoff_resume_checkpoint_id = $4,
       resume_request_version = 1,
       base_workspace_version_id = $5,
       base_workspace_content_digest = $6,
       child_result_version_id = $5,
       resume_workspace_version_id = $5,
       handoff_runtime_instance_id = $7,
       handoff_workspace_mount_id = $8,
       handoff_mount_generation = $9,
       ownership_generation = 1,
       parent_writer_generation = 2,
       child_writer_generation = 3,
       resume_writer_generation = 3
 WHERE id = $1`,
		resumeWaitID,
		granted.Lease.ID,
		resumeSuspendID,
		resumeCheckpointID,
		baseVersionID,
		baseDigest,
		granted.RuntimeInstanceID,
		granted.WorkspaceMountID,
		mountGeneration-1,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
WITH expired AS (
    UPDATE run_leases
       SET assigned_at = transaction_timestamp() - interval '3 seconds',
           start_deadline_at = transaction_timestamp() - interval '2 seconds',
           expires_at = transaction_timestamp() - interval '1 second'
     WHERE id = $1
    RETURNING id, expires_at
)
UPDATE workspace_leases
   SET expires_at = expired.expires_at
  FROM expired
 WHERE workspace_leases.owner_run_lease_id = expired.id`, granted.Lease.ID)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	return nestedResumeRecoveryState{
		parentRunID:                  parentRunID,
		enclosingWaitID:              enclosingWaitID,
		resumeWaitID:                 resumeWaitID,
		resumeCheckpointID:           resumeCheckpointID,
		parentSourceWorkspaceLeaseID: parentWorkspaceLeaseID,
		resumeSourceWorkspaceLeaseID: resumeSourceWorkspaceLeaseID,
		currentWorkspaceLeaseID:      workspaceLeaseID,
		runtimeID:                    granted.RuntimeInstanceID,
		mountID:                      granted.WorkspaceMountID,
	}
}

func TestPlaceReadyRunRecreatesExactSuspendedRuntimeAndBindsWait(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	candidate := fixture.candidate()
	reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
	mounting, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, mounting.WorkspaceMountID)
	granted, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}

	var sourceWorkspaceLeaseID, baseVersionID pgtype.UUID
	err = fixture.pool.QueryRow(fixture.ctx, `
SELECT workspace_leases.id, workspace_leases.base_version_id
  FROM workspace_leases
 WHERE owner_run_lease_id = $1`, granted.Lease.ID).Scan(
		&sourceWorkspaceLeaseID,
		&baseVersionID,
	)
	if err != nil {
		t.Fatal(err)
	}

	runWaitID := uuid.Must(uuid.NewV7())
	checkpointID := uuid.Must(uuid.NewV7())
	privateVersionID := uuid.Must(uuid.NewV7())
	privateArtifactID := uuid.Must(uuid.NewV7())
	privateDigest := "sha256:" + strings.Repeat("5", 64)
	resumeAttachID := uuid.Must(uuid.NewV7())
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(fixture.ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES ($1, $2, 1, $3)`, fixture.orgID, privateDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
) VALUES ($1, $2, $3, $4, $5, 'workspace_version', 1, $6)`,
		privateArtifactID, fixture.orgID, fixture.projectID, fixture.environmentID,
		privateDigest, workspace.ArtifactMediaType,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id,
    parent_version_id, kind, content_digest, state, source_workspace_lease_id,
    ownership_generation, writer_generation, artifact_id, artifact_kind,
    entry_count, size_bytes
) VALUES (
    $1, $2, $3, $4, 'user', $5, 'private', $6,
    1, 1, $7, 'workspace_version', 1, 1
)`,
		privateVersionID, fixture.environmentID, fixture.workspaceID, baseVersionID,
		privateDigest, sourceWorkspaceLeaseID, privateArtifactID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind, due_at, condition_state,
    condition_result, condition_terminal_at, suspension_state,
    expected_run_state_version, attempt_number, prior_run_lease_id,
    resume_attach_id
) VALUES (
    $1, $2, $3, $4, 'timer', now() - interval '1 second', 'completed',
    '{}'::jsonb, now(), 'parked', 3, 1, $5, $6
)`,
		runWaitID, fixture.environmentID, fixture.runID, fixture.workspaceID,
		granted.Lease.ID, resumeAttachID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_checkpoints (
    id, kind, run_id, attempt_number, run_wait_id, source_run_lease_id,
    source_workspace_lease_id, workspace_id, base_workspace_version_id,
    private_workspace_version_id, state, restore_manifest,
    ready_request_fingerprint, ready_at
) VALUES (
    $1, 'suspend', $2, 1, $3, $4, $5, $6, $7, $8,
    'ready', '{"kind":"suspend"}'::jsonb, 'test-ready', now()
)`,
		checkpointID, fixture.runID, runWaitID, granted.Lease.ID,
		sourceWorkspaceLeaseID, fixture.workspaceID, baseVersionID, privateVersionID,
	)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_waits
   SET suspension_state = 'resume_pending', suspend_checkpoint_id = $2,
       resume_request_version = 1
 WHERE id = $1`, runWaitID, checkpointID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runs
   SET current_run_lease_id = NULL, state_version = 3, updated_at = now()
 WHERE id = $1 AND current_run_lease_id = $2`, fixture.runID, granted.Lease.ID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_leases
   SET state = 'checkpointed', claimed_at = assigned_at, started_at = assigned_at,
       checkpointed_at = now(), terminal_at = now(), terminal_reason_code = 'checkpointed'
 WHERE id = $1`, granted.Lease.ID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspace_leases
   SET state = 'released', released_at = now(), terminal_at = now()
 WHERE id = $1`, sourceWorkspaceLeaseID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspace_mounts
   SET state = 'unmounted', unmounted_at = now(), terminal_at = now(),
       terminal_reason_code = 'checkpointed'
 WHERE id = $1`, mounting.WorkspaceMountID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runtime_instances
   SET desired_state = 'closed', desired_version = desired_version + 1,
       observed_state = 'closed', observed_desired_version = desired_version + 1,
       observed_version = observed_version + 1, closing_at = now(), closed_at = now(),
       terminal_at = now(), terminal_reason_code = 'checkpointed',
       reclaimed_at = now(),
       reclaim_evidence = jsonb_build_object('method', 'session_closed', 'completed_at', to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')),
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL
 WHERE id = $1`, reserved.RuntimeInstanceID)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}

	restoreCandidate := ReadyRunCandidate{
		OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID),
		ExpectedRunStateVersion: 3,
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances SET substrate_contract = 'incompatible-contract' WHERE id = $1`, fixture.workerID)
	if _, err := fixture.authority.PlaceReadyRun(fixture.ctx, restoreCandidate); !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("restore placement with incompatible substrate contract error = %v, want ErrCapacityUnavailable", err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances SET substrate_contract = 'builder-v0' WHERE id = $1`, fixture.workerID)
	restored, err := fixture.authority.PlaceReadyRun(fixture.ctx, restoreCandidate)
	if err != nil {
		t.Fatal(err)
	}
	var restoreCheckpointID, reservedVersionID pgtype.UUID
	err = fixture.pool.QueryRow(fixture.ctx, `
SELECT restore_checkpoint_id, reserved_workspace_version_id
  FROM runtime_instances
 WHERE id = $1`, restored.RuntimeInstanceID).Scan(&restoreCheckpointID, &reservedVersionID)
	if err != nil {
		t.Fatal(err)
	}
	if restoreCheckpointID != pgvalue.UUID(checkpointID) || reservedVersionID != pgvalue.UUID(privateVersionID) {
		t.Fatalf("restore reservation checkpoint=%s version=%s", pgvalue.UUIDString(restoreCheckpointID), pgvalue.UUIDString(reservedVersionID))
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_checkpoints
   SET state = 'invalid', ready_at = NULL, invalidated_at = now(),
       invalidation_reason_code = 'test_invalidated'
 WHERE id = $1`, checkpointID)
	if err := markRunPlacementRuntimeReadyQuery(t, fixture, restored.RuntimeInstanceID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mark ready with invalidated Checkpoint error = %v, want pgx.ErrNoRows", err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_checkpoints
   SET state = 'ready', ready_at = now(), invalidated_at = NULL,
       invalidation_reason_code = NULL
 WHERE id = $1`, checkpointID)
	if err := markRunPlacementRuntimeReadyQuery(t, fixture, restored.RuntimeInstanceID); err != nil {
		t.Fatal(err)
	}
	restoreMount, err := fixture.authority.PlaceReadyRun(fixture.ctx, restoreCandidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, restoreMount.WorkspaceMountID)
	restoreGrant, err := fixture.authority.PlaceReadyRun(fixture.ctx, restoreCandidate)
	if err != nil {
		t.Fatal(err)
	}

	var waitState string
	var waitLeaseID, leaseBaseVersionID, retainedCheckpointID, clearedReservation pgtype.UUID
	var restoredSubstrateID, sourceSubstrateID pgtype.UUID
	err = fixture.pool.QueryRow(fixture.ctx, `
SELECT run_waits.suspension_state,
       run_waits.current_run_lease_id,
       workspace_leases.base_version_id,
       runtime_instances.restore_checkpoint_id,
       runtime_instances.reserved_run_id,
       runtime_instances.runtime_substrate_id,
       source_runtime.runtime_substrate_id
  FROM run_waits
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = $2
  JOIN runtime_instances ON runtime_instances.id = workspace_leases.runtime_instance_id
  JOIN runtime_instances AS source_runtime ON source_runtime.id = $3
 WHERE run_waits.id = $1`, runWaitID, restoreGrant.Lease.ID, reserved.RuntimeInstanceID).Scan(
		&waitState, &waitLeaseID, &leaseBaseVersionID, &retainedCheckpointID, &clearedReservation,
		&restoredSubstrateID, &sourceSubstrateID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if waitState != "resuming" || waitLeaseID != restoreGrant.Lease.ID ||
		leaseBaseVersionID != pgvalue.UUID(privateVersionID) ||
		retainedCheckpointID != pgvalue.UUID(checkpointID) || clearedReservation.Valid ||
		!restoredSubstrateID.Valid || restoredSubstrateID != sourceSubstrateID {
		t.Fatalf("restore grant wait=%s lease=%s base=%s checkpoint=%s reserved=%s",
			waitState, pgvalue.UUIDString(waitLeaseID), pgvalue.UUIDString(leaseBaseVersionID),
			pgvalue.UUIDString(retainedCheckpointID), pgvalue.UUIDString(clearedReservation))
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'running',
       assigned_at = transaction_timestamp() - interval '20 seconds',
       start_deadline_at = transaction_timestamp() - interval '19 seconds',
       claimed_at = transaction_timestamp() - interval '18 seconds',
       started_at = transaction_timestamp() - interval '18 seconds'
 WHERE id = $1 AND state = 'assigned'`, restoreGrant.Lease.ID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET status = 'running', started_at = transaction_timestamp(),
       max_active_duration_ms = 5000,
       active_started_at = transaction_timestamp() - interval '10 seconds',
       state_version = state_version + 1
 WHERE id = $1 AND status = 'queued'`, fixture.runID)

	var originalQueueScore time.Time
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT queue_score_at FROM runs WHERE id = $1`, fixture.runID).Scan(&originalQueueScore); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
WITH expired AS (
    UPDATE run_leases
       SET expires_at = transaction_timestamp() - interval '8 seconds'
     WHERE id = $1
    RETURNING id, expires_at
)
UPDATE workspace_leases
   SET expires_at = expired.expires_at
  FROM expired
 WHERE workspace_leases.owner_run_lease_id = expired.id`, restoreGrant.Lease.ID)
	recovered, err := db.New(fixture.pool).RecoverExpiredRunResumes(fixture.ctx, recoverExpiredRunResumesParams(10))
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].ID != pgvalue.UUID(runWaitID) || recovered[0].RunID != pgvalue.UUID(fixture.runID) {
		t.Fatalf("recovered resumes = %+v", recovered)
	}
	var recoveredState string
	var recoveredLeaseID pgtype.UUID
	var recoveredVersion, recoveredRequestVersion, recoveredActiveElapsed int64
	var recoveredLeaseState, recoveredWorkspaceLeaseState, desiredState string
	err = fixture.pool.QueryRow(fixture.ctx, `
SELECT run_waits.suspension_state,
       run_waits.current_run_lease_id,
       runs.state_version,
       run_waits.resume_request_version,
       runs.active_elapsed_ms,
       run_leases.state,
       workspace_leases.state,
       runtime_instances.desired_state
  FROM run_waits
  JOIN runs ON runs.id = run_waits.run_id
  JOIN run_leases ON run_leases.id = $2
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
  JOIN runtime_instances ON runtime_instances.id = run_leases.runtime_instance_id
 WHERE run_waits.id = $1`, runWaitID, restoreGrant.Lease.ID).Scan(
		&recoveredState, &recoveredLeaseID, &recoveredVersion, &recoveredRequestVersion,
		&recoveredActiveElapsed, &recoveredLeaseState, &recoveredWorkspaceLeaseState, &desiredState,
	)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredState != "resume_pending" || recoveredLeaseID.Valid || recoveredVersion != 6 ||
		recoveredRequestVersion != 2 || recoveredActiveElapsed < 1900 || recoveredActiveElapsed > 3000 ||
		recoveredLeaseState != "expired" ||
		recoveredWorkspaceLeaseState != "expired" || desiredState != "closed" {
		t.Fatalf("recovery wait=%s lease=%s run_version=%d request_version=%d run_lease=%s workspace_lease=%s runtime=%s",
			recoveredState, pgvalue.UUIDString(recoveredLeaseID), recoveredVersion,
			recoveredRequestVersion, recoveredLeaseState, recoveredWorkspaceLeaseState, desiredState)
	}
	var recoveredQueueScore time.Time
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT queue_score_at FROM runs WHERE id = $1`, fixture.runID).Scan(&recoveredQueueScore); err != nil {
		t.Fatal(err)
	}
	if !recoveredQueueScore.Equal(originalQueueScore) {
		t.Fatalf("recovery changed immutable queue score from %s to %s", originalQueueScore, recoveredQueueScore)
	}
	var resumeAdmissionCount int
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT count(*)
  FROM outbox_messages
 WHERE topic = 'run.admit'
   AND payload ->> 'runId' = $1`, fixture.runID.String()).Scan(&resumeAdmissionCount); err != nil {
		t.Fatal(err)
	}
	if resumeAdmissionCount != 1 {
		t.Fatalf("resume admission outbox count = %d, want 1", resumeAdmissionCount)
	}

	// Reclaim the first recreated runtime and grant once more. The final branch
	// proves that physical loss discovered after the Lease deadline still
	// closes the retained runtime; cleanup remains blocked until recovery fences
	// the Lease.
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts
   SET state = 'unmounted', unmounted_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(), terminal_reason_code = 'test_reclaimed'
 WHERE id = $1 AND state = 'unmounting'`, restoreMount.WorkspaceMountID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runtime_instances
   SET observed_state = 'closed', observed_version = observed_version + 1,
       observed_desired_version = desired_version, observed_at = transaction_timestamp(),
       closing_at = transaction_timestamp(), closed_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(), terminal_reason_code = 'test_reclaimed',
       reclaimed_at = transaction_timestamp(),
       reclaim_evidence = jsonb_build_object('method', 'session_closed', 'completed_at', to_char(transaction_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
 WHERE id = $1 AND desired_state = 'closed'`, restored.RuntimeInstanceID)
	secondRestoreCandidate := ReadyRunCandidate{
		OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID),
		ExpectedRunStateVersion: 6,
	}
	secondRestored, err := fixture.authority.PlaceReadyRun(fixture.ctx, secondRestoreCandidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, secondRestored.RuntimeInstanceID)
	secondRestoreMount, err := fixture.authority.PlaceReadyRun(fixture.ctx, secondRestoreCandidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, secondRestoreMount.WorkspaceMountID)
	secondRestoreGrant, err := fixture.authority.PlaceReadyRun(fixture.ctx, secondRestoreCandidate)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET start_deadline_at = transaction_timestamp() - interval '1 millisecond'
 WHERE id = $1`, secondRestoreGrant.Lease.ID)
	lockTx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(fixture.ctx, `SELECT id FROM runs WHERE id = $1 FOR UPDATE`, fixture.runID); err != nil {
		t.Fatal(err)
	}
	type recoveryResult struct {
		rows []db.RecoverExpiredRunResumesRow
		err  error
	}
	recoveryDone := make(chan recoveryResult, 1)
	go func() {
		rows, recoverErr := db.New(fixture.pool).RecoverExpiredRunResumes(fixture.ctx, recoverExpiredRunResumesParams(10))
		recoveryDone <- recoveryResult{rows: rows, err: recoverErr}
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := lockTx.Exec(fixture.ctx, `
UPDATE run_leases
   SET start_deadline_at = transaction_timestamp() + interval '1 minute',
       expires_at = transaction_timestamp() + interval '5 minutes'
 WHERE id = $1`, secondRestoreGrant.Lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(fixture.ctx, `
UPDATE workspace_leases
   SET expires_at = transaction_timestamp() + interval '5 minutes'
 WHERE owner_run_lease_id = $1`, secondRestoreGrant.Lease.ID); err != nil {
		t.Fatal(err)
	}
	if err := lockTx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	renewedRecovery := <-recoveryDone
	if renewedRecovery.err != nil {
		t.Fatal(renewedRecovery.err)
	}
	if len(renewedRecovery.rows) != 0 {
		t.Fatalf("stale expiry selector recovered %d renewed Leases, want 0", len(renewedRecovery.rows))
	}
	var renewedLeaseState string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state FROM run_leases WHERE id = $1`, secondRestoreGrant.Lease.ID).Scan(&renewedLeaseState); err != nil {
		t.Fatal(err)
	}
	if renewedLeaseState != "assigned" {
		t.Fatalf("renewed Run Lease state = %s, want assigned", renewedLeaseState)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'running',
       assigned_at = transaction_timestamp() - interval '20 seconds',
       start_deadline_at = transaction_timestamp() - interval '19 seconds',
       claimed_at = transaction_timestamp() - interval '18 seconds',
       started_at = transaction_timestamp() - interval '18 seconds'
 WHERE id = $1`, secondRestoreGrant.Lease.ID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET status = 'running', max_active_duration_ms = 300000,
       started_at = COALESCE(started_at, transaction_timestamp() - interval '10 seconds'),
       active_started_at = transaction_timestamp() - interval '10 seconds',
       state_version = state_version + 1
 WHERE id = $1 AND status = 'queued'`, fixture.runID)
	startupTx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = startupTx.Rollback(fixture.ctx) }()
	if _, err := startupTx.Exec(fixture.ctx, `
UPDATE worker_instances
	   SET state = 'registering', current_epoch = 2,
       supervisor_version = '',
       supports_run = false,
       supports_build = false,
	       runtime_identity_id = NULL,
	       substrate_format = '',
	       substrate_contract = '',
       epoch_cpu_millis = 0,
       epoch_memory_bytes = 0,
       epoch_guest_ephemeral_disk_bytes = 0,
       epoch_build_cache_bytes = 0,
       epoch_artifact_cache_bytes = 0,
       per_vm_cpu_millis = 0,
       per_vm_memory_bytes = 0,
       per_vm_guest_ephemeral_disk_bytes = 0,
       max_vm_slots = 0,
       max_build_executors = 0,
       max_runtime_starts = 0,
       activated_at = NULL,
       epoch_started_at = transaction_timestamp(), updated_at = transaction_timestamp()
 WHERE id = $1`, fixture.workerID); err != nil {
		t.Fatal(err)
	}
	_, err = db.New(startupTx).CompleteWorkerStartupRecovery(fixture.ctx, db.CompleteWorkerStartupRecoveryParams{
		RecoveryEvidence: []byte(`{"quarantined":[]}`),
		WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerGroupID:    fixture.groupID,
		WorkerEpoch:      pgtype.Int8{Int64: 2, Valid: true},
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("startup recovery with active prior-epoch Run Lease error = %v, want pgx.ErrNoRows", err)
	}
	var startupReclaimedAt pgtype.Timestamptz
	if err := startupTx.QueryRow(fixture.ctx, `
SELECT runtime_instances.reclaimed_at
  FROM runtime_instances
 WHERE runtime_instances.id = $1`,
		secondRestored.RuntimeInstanceID,
	).Scan(&startupReclaimedAt); err != nil {
		t.Fatal(err)
	}
	if startupReclaimedAt.Valid {
		t.Fatalf("startup cleanup reclaimed active restore runtime")
	}
	if err := startupTx.Rollback(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
WITH expired AS (
    UPDATE run_leases
       SET expires_at = transaction_timestamp() - interval '1 second'
     WHERE id = $1
    RETURNING id, expires_at
)
UPDATE workspace_leases
   SET expires_at = expired.expires_at
  FROM expired
 WHERE workspace_leases.owner_run_lease_id = expired.id`,
		secondRestoreGrant.Lease.ID,
	)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts
   SET state = 'failed', failed_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(), terminal_reason_code = 'test_mount_failed'
 WHERE id = $1`, secondRestoreMount.WorkspaceMountID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runtime_instances
   SET observed_state = 'failed', observed_version = observed_version + 1,
       observed_at = transaction_timestamp(), failed_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(), terminal_reason_code = 'test_runtime_failed',
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL
 WHERE id = $1`, secondRestored.RuntimeInstanceID)
	var failedDesiredVersion, failedObservedVersion int64
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT desired_version, observed_version
  FROM runtime_instances
 WHERE id = $1`, secondRestored.RuntimeInstanceID).Scan(&failedDesiredVersion, &failedObservedVersion); err != nil {
		t.Fatal(err)
	}
	_, err = db.New(fixture.pool).ReclaimFailedRuntimeInstance(fixture.ctx, db.ReclaimFailedRuntimeInstanceParams{
		ID:                      secondRestored.RuntimeInstanceID,
		WorkerInstanceID:        secondRestoreGrant.Lease.WorkerInstanceID,
		WorkerEpoch:             secondRestoreGrant.Lease.WorkerEpoch,
		DesiredVersion:          failedDesiredVersion,
		ExpectedObservedVersion: failedObservedVersion,
		CleanupProof:            []byte(`{"method":"host_reconciled","completed_at":"2026-07-31T00:00:00Z"}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reclaim failed restore runtime with active Lease error = %v, want pgx.ErrNoRows", err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET state = 'lost', lost_at = transaction_timestamp()
 WHERE id = $1`, fixture.workerID)
	recovered, err = db.New(fixture.pool).RecoverExpiredRunResumes(fixture.ctx, recoverExpiredRunResumesParams(10))
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].ID != pgvalue.UUID(runWaitID) || recovered[0].RunID != pgvalue.UUID(fixture.runID) {
		t.Fatalf("worker-loss recovered resumes = %+v", recovered)
	}
	var lostRunState, lostWaitState, lostMountState string
	var lostRunLeaseState, lostRunLeaseReason, lostWorkspaceLeaseState, lostWorkspaceLeaseReason string
	var lostAttemptOutcome pgtype.Text
	var lostAttemptTerminalAt, lostRunTerminalAt pgtype.Timestamptz
	var lostActiveElapsed, lostRunVersion, lostResumeRequestVersion int64
	var lostWaitLeaseID pgtype.UUID
	var ownerRunID pgtype.UUID
	err = fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.status, run_waits.suspension_state, run_waits.current_run_lease_id,
	       runs.state_version, run_waits.resume_request_version,
	       run_attempts.terminal_outcome, run_attempts.terminal_at,
	       runs.terminal_at, runs.active_elapsed_ms,
       workspaces.owner_run_id, run_leases.state, run_leases.terminal_reason_code,
       workspace_leases.state, workspace_leases.terminal_reason_code,
       workspace_mounts.state
  FROM runs
  JOIN run_waits ON run_waits.run_id = runs.id
  JOIN run_attempts
    ON run_attempts.run_id = runs.id
   AND run_attempts.number = runs.current_attempt_number
  JOIN workspaces ON workspaces.id = runs.workspace_id
  JOIN run_leases ON run_leases.id = $2
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
  JOIN workspace_mounts ON workspace_mounts.id = workspace_leases.workspace_mount_id
 WHERE runs.id = $1`, fixture.runID, secondRestoreGrant.Lease.ID).Scan(
		&lostRunState, &lostWaitState, &lostWaitLeaseID,
		&lostRunVersion, &lostResumeRequestVersion,
		&lostAttemptOutcome, &lostAttemptTerminalAt,
		&lostRunTerminalAt, &lostActiveElapsed,
		&ownerRunID, &lostRunLeaseState, &lostRunLeaseReason,
		&lostWorkspaceLeaseState, &lostWorkspaceLeaseReason,
		&lostMountState,
	)
	if err != nil {
		t.Fatal(err)
	}
	if lostRunState != "queued" || lostWaitState != "resume_pending" || lostWaitLeaseID.Valid ||
		lostRunVersion != 9 || lostResumeRequestVersion != 3 ||
		lostAttemptOutcome.Valid || lostAttemptTerminalAt.Valid ||
		lostRunTerminalAt.Valid ||
		lostActiveElapsed < 9000 || lostActiveElapsed > 15000 ||
		ownerRunID != pgvalue.UUID(fixture.runID) ||
		lostRunLeaseState != "expired" || lostRunLeaseReason != "lease_expired" ||
		lostWorkspaceLeaseState != "expired" || lostWorkspaceLeaseReason != "lease_expired" ||
		lostMountState != "failed" {
		t.Fatalf("worker-loss recovery run=%s wait=%s wait_lease=%s run_version=%d request_version=%d attempt=%v attempt_at=%v run_at=%v active=%d owner=%s run_lease=%s/%s workspace_lease=%s/%s mount=%s",
			lostRunState, lostWaitState, pgvalue.UUIDString(lostWaitLeaseID), lostRunVersion,
			lostResumeRequestVersion, lostAttemptOutcome, lostAttemptTerminalAt,
			lostRunTerminalAt, lostActiveElapsed, pgvalue.UUIDString(ownerRunID),
			lostRunLeaseState, lostRunLeaseReason, lostWorkspaceLeaseState,
			lostWorkspaceLeaseReason, lostMountState)
	}
	var secondResumeAdmissionCount, terminalEventCount int
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT count(*)
  FROM outbox_messages
 WHERE topic = 'run.admit'
   AND payload ->> 'runId' = $1`, fixture.runID.String()).Scan(&secondResumeAdmissionCount); err != nil {
		t.Fatal(err)
	}
	if secondResumeAdmissionCount != 2 {
		t.Fatalf("worker-loss resume admission outbox count = %d, want 2", secondResumeAdmissionCount)
	}
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT count(*)
  FROM telemetry_outbox
 WHERE run_id = $1
	   AND kind = 'run.expired'`, fixture.runID).Scan(&terminalEventCount); err != nil {
		t.Fatal(err)
	}
	if terminalEventCount != 0 {
		t.Fatalf("worker-loss terminal event count = %d, want 0", terminalEventCount)
	}
}

func markRunPlacementRuntimeReady(t *testing.T, fixture runPlacementFixture, runtimeID pgtype.UUID) {
	t.Helper()
	if err := markRunPlacementRuntimeReadyQuery(t, fixture, runtimeID); err != nil {
		t.Fatal(err)
	}
}

func TestActorCurrentRunRecreatedRestoreAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		name            string
		invalidate      bool
		invalidCursor   bool
		maxDuration     bool
		wantRecovered   int
		wantActorState  string
		wantActorReason pgtype.Text
		wantRunStatus   string
	}{
		{name: "recoverable checkpoint", wantRecovered: 1, wantActorState: "open", wantRunStatus: "queued"},
		{name: "unavailable checkpoint", invalidate: true, wantActorState: "failed", wantActorReason: pgtype.Text{String: "platform_failure", Valid: true}, wantRunStatus: "system_failed"},
		{name: "maximum active duration", maxDuration: true, wantActorState: "failed", wantActorReason: pgtype.Text{String: "run_expired", Valid: true}, wantRunStatus: "expired"},
		{name: "speculative cursor outside Actor bounds", invalidCursor: true, wantActorState: "open", wantRunStatus: "queued"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRunPlacementFixture(t)
			actorID, waitID, checkpointID := prepareActorSuspendedRestore(t, fixture)
			candidate := ReadyRunCandidate{
				OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID),
				ExpectedRunStateVersion: 3,
			}
			if _, err := db.New(fixture.pool).GetQueuedRunReadyHint(fixture.ctx, db.GetQueuedRunReadyHintParams{
				OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID),
			}); err != nil {
				t.Fatalf("Actor restore ready hint: %v", err)
			}
			queued, err := db.New(fixture.pool).ListQueuedRunDispatchCandidatesForScopes(
				fixture.ctx,
				mustRunCandidateParams(t, QueueScope{
					OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
					EnvironmentID: pgvalue.UUID(fixture.environmentID), RegionID: "us-east-1",
					QueueName: "default",
				}, 10),
			)
			if err != nil || len(queued) != 1 || queued[0].RunID != pgvalue.UUID(fixture.runID) {
				t.Fatalf("Actor restore dispatch candidates = %+v, error = %v", queued, err)
			}
			scopes, err := db.New(fixture.pool).ListQueuedRunEligibleScopes(
				fixture.ctx,
				db.ListQueuedRunEligibleScopesParams{RowLimit: 10, ScanSeed: "actor-restore"},
			)
			if err != nil || len(scopes) != 1 || scopes[0].EnvironmentID != pgvalue.UUID(fixture.environmentID) {
				t.Fatalf("Actor restore candidate scopes = %+v, error = %v", scopes, err)
			}
			if tc.invalidCursor {
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_checkpoints SET actor_speculative_input_sequence = 2 WHERE id = $1`, checkpointID)
				if _, err := db.New(fixture.pool).GetQueuedRunReadyHint(fixture.ctx, db.GetQueuedRunReadyHintParams{
					OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID),
				}); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("out-of-bounds Actor restore ready hint error = %v, want pgx.ErrNoRows", err)
				}
				queued, err := db.New(fixture.pool).ListQueuedRunDispatchCandidatesForScopes(
					fixture.ctx,
					mustRunCandidateParams(t, QueueScope{
						OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
						EnvironmentID: pgvalue.UUID(fixture.environmentID), RegionID: "us-east-1",
						QueueName: "default",
					}, 10),
				)
				if err != nil || len(queued) != 0 {
					t.Fatalf("out-of-bounds Actor dispatch candidates = %+v, error = %v", queued, err)
				}
				if _, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate); !errors.Is(err, ErrCandidateChanged) {
					t.Fatalf("Actor restore with out-of-bounds cursor error = %v, want ErrCandidateChanged", err)
				}
				if _, err := fixture.pool.Exec(fixture.ctx, `
UPDATE run_attempts
   SET terminal_outcome = 'succeeded', terminal_reason_code = 'completed',
       terminal_at = transaction_timestamp()
 WHERE run_id = $1 AND number = 1`, fixture.runID); err == nil {
					t.Fatal("successful Actor Attempt accepted a NULL terminal cursor")
				}
				return
			}
			reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate)
			if err != nil {
				t.Fatal(err)
			}
			markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
			mount, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate)
			if err != nil {
				t.Fatal(err)
			}
			markRunPlacementMountReady(t, fixture, mount.WorkspaceMountID)
			grant, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate)
			if err != nil {
				t.Fatal(err)
			}
			if tc.invalidate {
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_checkpoints
   SET state = 'invalid', ready_at = NULL, invalidated_at = transaction_timestamp(),
       invalidation_reason_code = 'test_unavailable'
 WHERE id = $1`, checkpointID)
			}
			if tc.maxDuration {
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'running', claimed_at = assigned_at, started_at = assigned_at
 WHERE id = $1`, grant.Lease.ID)
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET status = 'running', max_active_duration_ms = 5000,
       active_started_at = transaction_timestamp() - interval '10 seconds',
       started_at = coalesce(started_at, transaction_timestamp() - interval '10 seconds'),
       state_version = state_version + 1
 WHERE id = $1`, fixture.runID)
			}
			dbtest.MustExec(t, fixture.ctx, fixture.pool, `
WITH expired AS (
    UPDATE run_leases
       SET start_deadline_at = assigned_at + interval '1 millisecond',
           expires_at = assigned_at + interval '2 milliseconds'
     WHERE id = $1
    RETURNING id, expires_at
)
UPDATE workspace_leases
   SET expires_at = expired.expires_at
  FROM expired
 WHERE owner_run_lease_id = expired.id`, grant.Lease.ID)
			recovered, err := db.New(fixture.pool).RecoverExpiredRunResumes(fixture.ctx, recoverExpiredRunResumesParams(10))
			if err != nil {
				t.Fatal(err)
			}
			if len(recovered) != tc.wantRecovered {
				t.Fatalf("recovered %d Actor resumes, want %d", len(recovered), tc.wantRecovered)
			}
			var actorState string
			var currentRunID, ownerActorID pgtype.UUID
			var runGeneration, actorStateVersion, ownershipGeneration int64
			var actorReason pgtype.Text
			var waitState, runStatus string
			var terminalCursor pgtype.Int8
			err = fixture.pool.QueryRow(fixture.ctx, `
SELECT sessions.state, sessions.current_run_id, sessions.run_generation, sessions.state_version,
	   sessions.failure->>'code', workspaces.owner_session_id, workspaces.ownership_generation,
       run_waits.suspension_state, runs.status, run_attempts.terminal_session_input_sequence
  FROM sessions
  JOIN workspaces ON workspaces.id = sessions.workspace_id
  JOIN runs ON runs.id = $2
  JOIN run_waits ON run_waits.id = $3
  JOIN run_attempts ON run_attempts.run_id = runs.id AND run_attempts.number = runs.current_attempt_number
 WHERE sessions.id = $1`, actorID, fixture.runID, waitID).Scan(
				&actorState, &currentRunID, &runGeneration, &actorStateVersion,
				&actorReason, &ownerActorID, &ownershipGeneration, &waitState, &runStatus, &terminalCursor,
			)
			if err != nil {
				t.Fatal(err)
			}
			if actorState != tc.wantActorState || actorReason != tc.wantActorReason {
				t.Fatalf("Actor state/reason = %s/%v, want %s/%v", actorState, actorReason, tc.wantActorState, tc.wantActorReason)
			}
			if tc.invalidate || tc.maxDuration {
				if currentRunID.Valid || ownerActorID.Valid || runGeneration != 2 || actorStateVersion != 2 ||
					ownershipGeneration != 2 || waitState != "failed" || runStatus != tc.wantRunStatus || terminalCursor.Valid {
					t.Fatalf("terminal Actor composition run=%s owner=%s generations=%d/%d workspace=%d wait=%s run=%s cursor=%v",
						pgvalue.UUIDString(currentRunID), pgvalue.UUIDString(ownerActorID), runGeneration,
						actorStateVersion, ownershipGeneration, waitState, runStatus, terminalCursor)
				}
			} else if currentRunID != pgvalue.UUID(fixture.runID) || ownerActorID != pgvalue.UUID(actorID) ||
				runGeneration != 1 || actorStateVersion != 1 || ownershipGeneration != 1 ||
				waitState != "resume_pending" || runStatus != "queued" {
				t.Fatalf("recoverable Actor changed durable identity/state")
			}
		})
	}
}

func prepareActorSuspendedRestore(t *testing.T, fixture runPlacementFixture) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
	mount, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, mount.WorkspaceMountID)
	grant, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	var sourceWorkspaceLeaseID, baseVersionID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT id, base_version_id FROM workspace_leases WHERE owner_run_lease_id = $1`, grant.Lease.ID).Scan(
		&sourceWorkspaceLeaseID, &baseVersionID,
	); err != nil {
		t.Fatal(err)
	}
	actorID := uuid.Must(uuid.NewV7())
	actorDefinitionID := uuid.Must(uuid.NewV7())
	waitID := uuid.Must(uuid.NewV7())
	checkpointID := uuid.Must(uuid.NewV7())
	privateVersionID := uuid.Must(uuid.NewV7())
	privateArtifactID := uuid.Must(uuid.NewV7())
	privateDigest := "sha256:" + strings.Repeat("6", 64)
	// This fixture converts the already-granted Task source into an Actor source.
	// Production Actor creation does not perform that conversion, but deferring
	// this FK lets the fixture establish the same valid final graph atomically.
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
ALTER TABLE run_attempts
ALTER CONSTRAINT run_attempts_run_id_entrypoint_kind_workspace_id_fkey
DEFERRABLE INITIALLY DEFERRED`)
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, fixture.ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO deployment_definitions (
    id, environment_id, deployment_id, kind, declared_id, manifest_version, manifest, manifest_digest
) VALUES ($1, $2, $3, 'actor', 'test-actor', 0, '{}'::jsonb, decode(repeat('06', 32), 'hex'))`,
		actorDefinitionID, fixture.environmentID, fixture.deploymentID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO sessions (
    id, environment_id, actor_declared_id,
    deployment_definition_id, workspace_id, current_run_id,
    next_input_sequence, committed_input_sequence, run_queue_name,
    run_max_active_duration_ms
) VALUES ($1, $2, 'test-actor', $3, $4, $5, 2, 1, 'default', 300000)`,
		actorID, fixture.environmentID, actorDefinitionID, fixture.workspaceID, fixture.runID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runs
   SET deployment_definition_id = $2, entrypoint_kind = 'actor',
       entrypoint_declared_id = 'test-actor', session_id = $3,
       cause_kind = 'actor_start', session_input_start_sequence = 1,
       session_input_high_watermark = 1, payload = NULL
 WHERE id = $1`, fixture.runID, actorDefinitionID, actorID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_attempts
   SET entrypoint_kind = 'actor', session_input_start_sequence = 1,
       entrypoint_entered_at = transaction_timestamp()
 WHERE run_id = $1 AND number = 1`, fixture.runID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspaces SET owner_run_id = NULL, owner_session_id = $2 WHERE id = $1`, fixture.workspaceID, actorID)
	dbtest.MustExec(t, fixture.ctx, tx, `INSERT INTO cas_objects (org_id, digest, size_bytes, media_type) VALUES ($1, $2, 1, $3)`,
		fixture.orgID, privateDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
VALUES ($1, $2, $3, $4, $5, 'workspace_version', 1, $6)`, privateArtifactID, fixture.orgID,
		fixture.projectID, fixture.environmentID, privateDigest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id, parent_version_id,
    kind, content_digest, state, source_workspace_lease_id, ownership_generation,
    writer_generation, artifact_id, artifact_kind, entry_count, size_bytes
) VALUES ($1, $2, $3, $4, 'user', $5, 'private', $6, 1, 1, $7, 'workspace_version', 1, 1)`,
		privateVersionID, fixture.environmentID, fixture.workspaceID, baseVersionID, privateDigest,
		sourceWorkspaceLeaseID, privateArtifactID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind, due_at, condition_state,
    condition_result, condition_terminal_at, suspension_state, expected_run_state_version,
    attempt_number, prior_run_lease_id, resume_attach_id
) VALUES ($1, $2, $3, $4, 'timer', now() - interval '1 second', 'completed', '{}'::jsonb,
          now(), 'resume_pending', 3, 1, $5, $6)`, waitID, fixture.environmentID, fixture.runID,
		fixture.workspaceID, grant.Lease.ID, uuid.Must(uuid.NewV7()))
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_checkpoints (
    id, kind, run_id, attempt_number, run_wait_id, source_run_lease_id,
    source_workspace_lease_id, workspace_id, base_workspace_version_id,
    private_workspace_version_id, actor_speculative_input_sequence,
    state, restore_manifest, ready_request_fingerprint, ready_at
) VALUES ($1, 'suspend', $2, 1, $3, $4, $5, $6, $7, $8, 1,
          'ready', '{"kind":"suspend"}'::jsonb, 'test-ready', now())`,
		checkpointID, fixture.runID, waitID, grant.Lease.ID, sourceWorkspaceLeaseID,
		fixture.workspaceID, baseVersionID, privateVersionID)
	dbtest.MustExec(t, fixture.ctx, tx, `UPDATE run_waits SET suspend_checkpoint_id = $2, resume_request_version = 1 WHERE id = $1`, waitID, checkpointID)
	dbtest.MustExec(t, fixture.ctx, tx, `UPDATE runs SET current_run_lease_id = NULL, state_version = 3 WHERE id = $1`, fixture.runID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_leases SET state = 'checkpointed', claimed_at = assigned_at, started_at = assigned_at,
       checkpointed_at = now(), terminal_at = now(), terminal_reason_code = 'checkpointed' WHERE id = $1`, grant.Lease.ID)
	dbtest.MustExec(t, fixture.ctx, tx, `UPDATE workspace_leases SET state = 'released', released_at = now(), terminal_at = now() WHERE id = $1`, sourceWorkspaceLeaseID)
	dbtest.MustExec(t, fixture.ctx, tx, `UPDATE workspace_mounts SET state = 'unmounted', unmounted_at = now(), terminal_at = now(), terminal_reason_code = 'checkpointed' WHERE id = $1`, mount.WorkspaceMountID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runtime_instances SET desired_state = 'closed', desired_version = desired_version + 1,
       observed_state = 'closed', observed_desired_version = desired_version + 1,
       observed_version = observed_version + 1, closing_at = now(), closed_at = now(),
       terminal_at = now(), terminal_reason_code = 'checkpointed', reclaimed_at = now(),
       reclaim_evidence = jsonb_build_object('method', 'session_closed', 'completed_at', to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')),
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL WHERE id = $1`, reserved.RuntimeInstanceID)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	return actorID, waitID, checkpointID
}

func markRunPlacementRuntimeReadyQuery(t *testing.T, fixture runPlacementFixture, runtimeID pgtype.UUID) error {
	t.Helper()
	var desiredVersion, observedVersion, workerEpoch int64
	var workerID, runtimeSubstrateID pgtype.UUID
	err := fixture.pool.QueryRow(fixture.ctx, `
WITH runtime AS (
    SELECT runtime_instances.org_id,
           runtime_instances.project_id,
           runtime_instances.environment_id,
           runtime_instances.deployment_definition_id
      FROM runtime_instances
     WHERE runtime_instances.id = $1
),
inserted AS (
	INSERT INTO runtime_substrates (
		id, org_id, project_id, environment_id, deployment_definition_id,
		substrate_digest, substrate_format, substrate_contract,
		substrate_size_bytes
	)
	SELECT $2, org_id, project_id, environment_id, deployment_definition_id,
		   'sha256:test-runtime-substrate', 'squashfs', 'builder-v0', 1
      FROM runtime
    ON CONFLICT ON CONSTRAINT runtime_substrates_input_key DO NOTHING
    RETURNING id
)
SELECT id FROM inserted
UNION ALL
SELECT runtime_substrates.id
  FROM runtime_substrates
  JOIN runtime USING (org_id, project_id, environment_id, deployment_definition_id)
 WHERE substrate_format = 'squashfs'
   AND substrate_contract = 'builder-v0'
LIMIT 1`, runtimeID, pgvalue.NewUUIDv7()).Scan(&runtimeSubstrateID)
	if err != nil {
		t.Fatal(err)
	}
	err = fixture.pool.QueryRow(fixture.ctx, `
SELECT runtime_instances.desired_version,
       runtime_instances.observed_version,
       runtime_instances.worker_instance_id,
       runtime_instances.worker_epoch
  FROM runtime_instances
 WHERE runtime_instances.id = $1`, runtimeID).Scan(
		&desiredVersion, &observedVersion, &workerID, &workerEpoch,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.New(fixture.pool).MarkRuntimeInstanceReady(fixture.ctx, db.MarkRuntimeInstanceReadyParams{
		RuntimeSubstrateID: runtimeSubstrateID,
		DesiredVersion:     desiredVersion, ID: runtimeID, WorkerInstanceID: workerID,
		WorkerEpoch: workerEpoch, ExpectedObservedVersion: observedVersion,
	})
	return err
}

func markRunPlacementMountReady(t *testing.T, fixture runPlacementFixture, mountID pgtype.UUID) {
	t.Helper()
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts SET state = 'mounted', mounted_at = transaction_timestamp() WHERE id = $1`, mountID)
}

func TestPlaceReadyRunRejectsPerVMIncompatibleWorkspace(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET per_vm_memory_bytes = 536870912
 WHERE id = $1`,
		fixture.workerID,
	)

	_, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		fixture.candidate(),
	)
	if err != ErrCapacityUnavailable {
		t.Fatalf("PlaceReadyRun() error = %v, want ErrCapacityUnavailable", err)
	}
	var runtimes int
	if err := fixture.pool.QueryRow(
		fixture.ctx,
		`SELECT count(*) FROM runtime_instances WHERE workspace_id = $1`,
		fixture.workspaceID,
	).Scan(&runtimes); err != nil {
		t.Fatal(err)
	}
	if runtimes != 0 {
		t.Fatalf("created %d runtimes for an incompatible per-VM profile", runtimes)
	}
}

func TestPlaceReadyRunAccountsForActiveBuildResources(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET epoch_memory_bytes = 4294967296,
       epoch_guest_ephemeral_disk_bytes = 68719476736,
       per_vm_guest_ephemeral_disk_bytes = 68719476736
 WHERE id = $1`,
		fixture.workerID,
	)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
INSERT INTO deployment_build_leases (
    id, org_id, project_id, environment_id, deployment_id, build_region_id,
    lease_sequence, worker_group_id, worker_instance_id, worker_epoch,
    requested_cpu_millis, requested_memory_bytes,
    requested_guest_ephemeral_disk_bytes, requested_build_executors,
    build_snapshot, start_deadline_at, expires_at
) VALUES (
    $1, $2, $3, $4, $5, 'us-east-1', 1, $6, $7, 1,
    3000, 4294967296, 34359738368,
    1, '{}'::jsonb, now() + interval '1 minute', now() + interval '5 minutes'
)`,
		uuid.Must(uuid.NewV7()),
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		fixture.deploymentID,
		fixture.groupID,
		fixture.workerID,
	)

	_, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		fixture.candidate(),
	)
	if err != ErrCapacityUnavailable {
		t.Fatalf("PlaceReadyRun() error = %v, want ErrCapacityUnavailable", err)
	}
}

func (fixture runPlacementFixture) candidate() ReadyRunCandidate {
	return ReadyRunCandidate{
		OrgID:                   pgvalue.UUID(fixture.orgID),
		RunID:                   pgvalue.UUID(fixture.runID),
		ExpectedRunStateVersion: 1,
	}
}

func newRunPlacementFixture(t *testing.T) runPlacementFixture {
	return newRunPlacementFixtureWithSeed(t, uuid.NewString())
}

func newRunPlacementFixtureWithSeed(t *testing.T, seed string) runPlacementFixture {
	t.Helper()
	ctx := context.Background()
	pool := newDispatchIntegrationDB(t, ctx)
	id := func(kind string) uuid.UUID {
		return uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed+":"+kind))
	}
	fixture := runPlacementFixture{
		ctx:           ctx,
		pool:          pool,
		orgID:         id("organization"),
		projectID:     id("project"),
		environmentID: id("environment"),
		runID:         id("run"),
		workspaceID:   id("workspace"),
		workerID:      id("worker"),
		groupID:       "run-placement-" + strings.ReplaceAll(id("group").String(), "-", ""),
	}
	key := bytes.Repeat([]byte{7}, workspace.FencingKeySize)
	var err error
	fixture.fencingKey, err = workspace.NewFencingKey(key)
	if err != nil {
		t.Fatal(err)
	}
	fixture.authority, err = NewRunAuthority(
		pool,
		fixture.fencingKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	deploymentID := id("deployment")
	fixture.deploymentID = deploymentID
	taskDefinitionID := id("task-definition")
	workspaceDefinitionID := id("workspace-definition")
	versionID := id("workspace-version")
	sourceID := id("source-artifact")
	programID := id("program-artifact")
	imageID := id("image-artifact")
	runtimeIdentityID := "run-runtime-" + strings.ReplaceAll(id("runtime-identity").String(), "-", "")
	sourceDigest := "sha256:" + strings.Repeat("1", 64)
	programDigest := "sha256:" + strings.Repeat("2", 64)
	imageDigest := "sha256:" + strings.Repeat("4", 64)

	dbtest.MustExec(t, ctx, pool, `
INSERT INTO regions (id, display_name)
VALUES ('us-east-1', 'US East')`)
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO organizations (id, name, slug)
VALUES ($1, 'Org', $2)`,
		fixture.orgID,
		"org-"+fixture.orgID.String(),
	)
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO projects (id, org_id, default_region_id, slug, name)
VALUES ($1, $2, 'us-east-1', $3, 'Project')`,
		fixture.projectID,
		fixture.orgID,
		"project-"+fixture.projectID.String(),
	)
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
VALUES ($1, $2, $3, $4, 'Environment', '#3366ff')`,
		fixture.environmentID,
		fixture.orgID,
		fixture.projectID,
		"environment-"+fixture.environmentID.String(),
	)
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES
    ($1, $2, 1, 'application/vnd.helmr.deployment-source.v0+tar'),
    ($1, $3, 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
    ($1, $4, 1, 'application/octet-stream')`,
		fixture.orgID,
		sourceDigest,
		programDigest,
		imageDigest,
	)
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
) VALUES
    ($1, $4, $5, $6, $7, 'deployment_source', 1, 'application/vnd.helmr.deployment-source.v0+tar'),
    ($2, $4, $5, $6, $8, 'deployment_program', 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
    ($3, $4, $5, $6, $9, 'workspace_image', 1, 'application/octet-stream')`,
		sourceID,
		programID,
		imageID,
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		sourceDigest,
		programDigest,
		imageDigest,
	)
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO deployments (
    id, org_id, project_id, environment_id, build_region_id,
    build_node_version, build_runtime_digest, build_toolchain_digest,
    build_manager_name, build_manager_version, build_manager_digest,
    build_contract, image_cache_mode, version, content_hash, deployment_source_artifact_id,
    program_artifact_id, program_index_digest, queue_config, status
) VALUES (
    $1, $2, $3, $4, 'us-east-1', '24.16.0',
    decode(repeat('01', 32), 'hex'), decode(repeat('02', 32), 'hex'),
    'npm', '11.5.0', decode(repeat('22', 32), 'hex'),
    'helmr.program-build.v0', 'prefer', 'v1', $5, $6, $7,
    decode(repeat('03', 32), 'hex'), '{}'::jsonb, 'deployed'
)`,
		deploymentID,
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		sourceDigest,
		sourceID,
		programID,
	)
	workspaceManifest := fmt.Sprintf(
		`{"image":{"artifactDigest":%q,"mediaType":"application/octet-stream"},"resources":{"milliCpu":1000,"memoryMiB":1024}}`,
		imageDigest,
	)
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO deployment_definitions (
    id, environment_id, deployment_id, kind, declared_id, manifest_version,
    manifest, manifest_digest, artifact_id
) VALUES
    ($1, $3, $4, 'task', 'test-task', 0, '{}'::jsonb,
     decode(repeat('03', 32), 'hex'), NULL),
    ($2, $3, $4, 'sandbox', 'test-workspace', 0, $5::jsonb,
     decode(repeat('04', 32), 'hex'), $6)`,
		taskDefinitionID,
		workspaceDefinitionID,
		fixture.environmentID,
		deploymentID,
		workspaceManifest,
		imageID,
	)
	dbtest.MustExec(t, ctx, pool, `
WITH token AS (
    INSERT INTO worker_group_tokens (id, token_hash)
    VALUES ($2, $3)
    RETURNING id
)
INSERT INTO worker_groups (
    id, token_id, region_id, name, allows_run, allows_build
)
SELECT $1, token.id, 'us-east-1', $1, true, false FROM token`,
		fixture.groupID, id("worker-group-token"), dbtest.Hash("run-placement-worker-group"),
	)
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO runtime_identities (
    id, runtime_arch, vm_runtime_contract, kernel_digest, initramfs_digest,
    rootfs_digest
) VALUES ($1, 'x86_64', 'helmr.vm-runtime.v0', 'kernel', 'initramfs', 'rootfs')`,
		runtimeIdentityID,
	)
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO worker_instances (
    id, resource_id, worker_group_id, state,
	current_epoch, current_service_id, supervisor_version,
    supports_run, runtime_identity_id,
	substrate_format, substrate_contract,
    epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
    per_vm_cpu_millis, per_vm_memory_bytes,
    per_vm_guest_ephemeral_disk_bytes, max_vm_slots,
    max_runtime_starts, observed_at, epoch_started_at, activated_at
) VALUES (
	$1, $2, $3, 'active', 1, $4,
	'test-worker', true, $5, 'squashfs', 'builder-v0',
    8000, 8589934592, 274877906944,
    1000, 1073741824, 34359738368,
    8, 8, now(), now(), now()
)`,
		fixture.workerID,
		fixture.workerID.String(),
		fixture.groupID,
		id("worker-service"),
		runtimeIdentityID,
	)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO workspaces (
    id, environment_id, region_id,
    sandbox_declared_id, deployment_definition_id,
    owner_run_id, ownership_generation, writer_generation, head_version_id
) VALUES (
    $1, $2, 'us-east-1', 'test-workspace',
    $3, $4, 1, 0, $5
)`,
		fixture.workspaceID,
		fixture.environmentID,
		workspaceDefinitionID,
		fixture.runID,
		versionID,
	)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id,
    kind, content_digest, state, ownership_generation, writer_generation, published_at
) VALUES (
    $1, $2, $3, 'system',
    'sha256:d2ce8eece19cb4f6db14e37f6d986da7eec7f654f3b91c5c706e9d74e7d2bc96',
    'committed', 0, 0, now()
)`,
		versionID,
		fixture.environmentID,
		fixture.workspaceID,
	)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO runs (
    id, org_id, project_id, environment_id, deployment_id,
    deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
    cause_kind, workspace_id, base_workspace_version_id, payload, queue_name,
    queue_origin_at, queue_score_at, max_active_duration_ms, retry_policy,
    trace_id, root_span_id
) VALUES (
    $1, $2, $3, $4, $5, $6, 'task', 'test-task', 'api', $7, $8,
    '{}'::jsonb, 'default', now(), now(), 300000, '{"enabled":false}'::jsonb,
    '11111111111111111111111111111111', '2222222222222222'
)`,
		fixture.runID,
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		deploymentID,
		taskDefinitionID,
		fixture.workspaceID,
		versionID,
	)
	dbtest.MustExec(t, ctx, tx, `
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id, base_workspace_version_id
) VALUES ($1, 1, 'task', $2, $3)`,
		fixture.runID,
		fixture.workspaceID,
		versionID,
	)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return fixture
}
