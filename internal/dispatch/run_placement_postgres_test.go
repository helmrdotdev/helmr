package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/capacity"
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

func recoverExpiredRunResumesParams(limit int32) int32 {
	return limit
}

type planningScope struct {
	OrgID          pgtype.UUID
	ProjectID      pgtype.UUID
	EnvironmentID  pgtype.UUID
	RegionID       string
	ConcurrencyKey string
	QueueName      string
}

func mustRunCandidateParams(t *testing.T, scope planningScope, limit int32) db.ListQueuedRunPlanningCandidatesForScopesParams {
	t.Helper()
	params, err := planningCandidateParams([]planningScope{scope}, limit)
	if err != nil {
		t.Fatal(err)
	}
	return params
}

func planningCandidateParams(scopes []planningScope, limit int32) (db.ListQueuedRunPlanningCandidatesForScopesParams, error) {
	if len(scopes) == 0 || len(scopes) > 32 {
		return db.ListQueuedRunPlanningCandidatesForScopesParams{}, fmt.Errorf("planning candidate scope count must be between 1 and 32: %d", len(scopes))
	}
	params := db.ListQueuedRunPlanningCandidatesForScopesParams{
		PerScopeLimit: limit,
		OrgIds:        make([]pgtype.UUID, 0, len(scopes)), ProjectIds: make([]pgtype.UUID, 0, len(scopes)),
		EnvironmentIds: make([]pgtype.UUID, 0, len(scopes)), RegionIds: make([]string, 0, len(scopes)),
		ConcurrencyKeys: make([]string, 0, len(scopes)), QueueNames: make([]string, 0, len(scopes)),
	}
	for _, scope := range scopes {
		params.OrgIds = append(params.OrgIds, scope.OrgID)
		params.ProjectIds = append(params.ProjectIds, scope.ProjectID)
		params.EnvironmentIds = append(params.EnvironmentIds, scope.EnvironmentID)
		params.RegionIds = append(params.RegionIds, scope.RegionID)
		params.ConcurrencyKeys = append(params.ConcurrencyKeys, scope.ConcurrencyKey)
		params.QueueNames = append(params.QueueNames, scope.QueueName)
	}
	return params, nil
}

func listRunPlacementCandidates(
	t *testing.T,
	fixture runPlacementFixture,
	limit int32,
) []db.ListQueuedRunPlacementCandidatesRow {
	t.Helper()
	store, err := NewRunPlacementStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	organizations, err := store.ListOrganizations(
		fixture.ctx,
		runPlacementLaneBytes(fixture.orgID),
		pgtype.UUID{},
		limit,
	)
	if err != nil {
		t.Fatal(err)
	}
	after := make([]runPlacementScopeCursor, len(organizations))
	rows, err := store.ListScopes(fixture.ctx, runPlacementScopeParams{
		organizations: organizations, after: after, limit: limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	scopes := make([]runPlacementScope, 0, len(rows))
	for _, row := range rows {
		scopes = append(scopes, row.scope)
	}
	if len(scopes) == 0 {
		return nil
	}
	var cursor runPlacementCursor
	limits := make([]int32, len(scopes))
	for i := range limits {
		limits[i] = limit
	}
	candidates, err := store.ListCandidates(fixture.ctx, cursor.candidateParams(scopes, limits))
	if err != nil {
		t.Fatal(err)
	}
	return candidates
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
	var vmVCPUCount int32
	var cpuConfigDigest string
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT vm_vcpu_count, cpu_config_digest
  FROM runtime_instances
 WHERE id = $1`, reserved.RuntimeInstanceID).Scan(&vmVCPUCount, &cpuConfigDigest); err != nil {
		t.Fatal(err)
	}
	if vmVCPUCount != 1 || cpuConfigDigest != dbtest.DefaultCPUConfigID {
		t.Fatalf("runtime CPU shape = %d/%q, want 1/%q", vmVCPUCount, cpuConfigDigest, dbtest.DefaultCPUConfigID)
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
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET runtime_preparation_count = 3,
       next_runtime_preparation_at = transaction_timestamp()
 WHERE id = $1`, fixture.runID)

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
	var runtimePreparationCount int32
	var nextRuntimePreparationAt pgtype.Timestamptz
	var ownerRunLeaseID pgtype.UUID
	var tokenHash string
	err = fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.current_run_lease_id,
       runs.first_lease_at,
       runs.state_version,
       runs.runtime_preparation_count,
       runs.next_runtime_preparation_at,
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
		&runtimePreparationCount,
		&nextRuntimePreparationAt,
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
		runtimePreparationCount != 0 ||
		nextRuntimePreparationAt.Valid ||
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

func TestPlaceReadyRunEvictsOneIdleMountUnderCapacityPressure(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET max_vm_slots = 1,
       max_runtime_starts = 1
 WHERE id = $1`, fixture.workerID)

	reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
	mounted, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, mounted.WorkspaceMountID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runtime_instances
   SET reserved_run_id = NULL,
       reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL,
       reservation_expires_at = NULL
 WHERE id = $1`, reserved.RuntimeInstanceID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspaces SET owner_run_id = NULL WHERE id = $1`, fixture.workspaceID)

	secondRunID := uuid.New()
	secondWorkspaceID := uuid.New()
	secondVersionID := uuid.New()
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(fixture.ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspaces (
    id, environment_id, region_id, sandbox_declared_id,
    deployment_definition_id, owner_run_id, ownership_generation,
    writer_generation, head_version_id
)
SELECT $1, environment_id, region_id, sandbox_declared_id,
       deployment_definition_id, $2, 1, 0, $3
  FROM workspaces
 WHERE id = $4`, secondWorkspaceID, secondRunID, secondVersionID, fixture.workspaceID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id, kind, content_digest,
    size_bytes, entry_count, state,
    ownership_generation, writer_generation, published_at
)
SELECT $1, environment_id, $2, kind, content_digest,
       size_bytes, entry_count, state, 0, 0, transaction_timestamp()
  FROM workspace_versions
 WHERE id = (SELECT head_version_id FROM workspaces WHERE id = $3)`,
		secondVersionID, secondWorkspaceID, fixture.workspaceID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO runs (
    id, org_id, project_id, environment_id, deployment_id,
    deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
    cause_kind, workspace_id, base_workspace_version_id, payload, queue_name,
    queue_origin_at, queue_score_at, max_active_duration_ms, retry_policy,
    trace_id, root_span_id
)
SELECT $1, org_id, project_id, environment_id, deployment_id,
       deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
       'api', $2, $3, payload, queue_name,
       transaction_timestamp(), transaction_timestamp(), max_active_duration_ms,
       retry_policy, '33333333333333333333333333333333', '4444444444444444'
  FROM runs
 WHERE id = $4`, secondRunID, secondWorkspaceID, secondVersionID, fixture.runID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id, base_workspace_version_id
) VALUES ($1, 1, 'task', $2, $3)`, secondRunID, secondWorkspaceID, secondVersionID)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}

	secondCandidate := ReadyRunCandidate{
		OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(secondRunID),
		ExpectedRunStateVersion: 1,
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspaces SET dirty_state = 'dirty' WHERE id = $1`, fixture.workspaceID)
	if _, err := fixture.authority.PlaceReadyRun(fixture.ctx, secondCandidate); !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("dirty Workspace pressure error = %v, want ErrCapacityUnavailable", err)
	}
	var protectedState string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state FROM workspace_mounts WHERE id = $1`,
		mounted.WorkspaceMountID).Scan(&protectedState); err != nil {
		t.Fatal(err)
	}
	if protectedState != "mounted" {
		t.Fatalf("dirty Workspace Mount state = %s, want mounted", protectedState)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspaces SET dirty_state = 'clean' WHERE id = $1`, fixture.workspaceID)

	processClaimID := uuid.New()
	processID := uuid.New()
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash,
    request_fingerprint, accepted_at, expires_at
) VALUES (
    $1, $2, 'test.capacity-pressure', decode(repeat('51', 32), 'hex'),
    decode(repeat('52', 32), 'hex'), transaction_timestamp(),
    transaction_timestamp() + interval '30 days'
)`, processClaimID, fixture.environmentID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
INSERT INTO workspace_processes (
    id, org_id, project_id, environment_id, workspace_id, base_version_id,
    restore_desired_state, state, request, claim_id,
    created_by_subject_type, created_by_subject_id
) VALUES (
    $1, $2, $3, $4, $5,
    (SELECT head_version_id FROM workspaces WHERE id = $5),
    'active', 'pending', '{}'::jsonb, $6, 'test', 'capacity-pressure'
)`, processID, fixture.orgID, fixture.projectID, fixture.environmentID,
		fixture.workspaceID, processClaimID)
	if _, err := fixture.authority.PlaceReadyRun(fixture.ctx, secondCandidate); !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("live Process pressure error = %v, want ErrCapacityUnavailable", err)
	}
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT state FROM workspace_mounts WHERE id = $1`,
		mounted.WorkspaceMountID).Scan(&protectedState); err != nil {
		t.Fatal(err)
	}
	if protectedState != "mounted" {
		t.Fatalf("live Process Workspace Mount state = %s, want mounted", protectedState)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `DELETE FROM workspace_processes WHERE id = $1`, processID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `DELETE FROM idempotency_claims WHERE id = $1`, processClaimID)

	if _, err := fixture.authority.PlaceReadyRun(fixture.ctx, secondCandidate); !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("capacity pressure placement error = %v, want ErrCapacityUnavailable", err)
	}
	var state string
	var finalizationKind, finalizationReason pgtype.Text
	var mountWorkerID, runtimeID pgtype.UUID
	var workerEpoch, fencingGeneration int64
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT state, finalization_kind, finalization_reason_code,
       worker_instance_id, worker_epoch, runtime_instance_id, fencing_generation
  FROM workspace_mounts
 WHERE id = $1`, mounted.WorkspaceMountID).Scan(
		&state, &finalizationKind, &finalizationReason,
		&mountWorkerID, &workerEpoch, &runtimeID, &fencingGeneration,
	); err != nil {
		t.Fatal(err)
	}
	if state != "unmounting" || !finalizationKind.Valid || finalizationKind.String != "discard" ||
		!finalizationReason.Valid || finalizationReason.String != "capacity_pressure" {
		t.Fatalf("pressure Mount = state:%s finalization:%v/%v", state, finalizationKind, finalizationReason)
	}
	if _, err := db.New(fixture.pool).StopWorkspaceMount(fixture.ctx, db.StopWorkspaceMountParams{
		ReasonCode: pgvalue.Text("capacity_pressure"), OrgID: pgvalue.UUID(fixture.orgID),
		ID: mounted.WorkspaceMountID, WorkerInstanceID: mountWorkerID, WorkerEpoch: workerEpoch,
		RuntimeInstanceID: runtimeID, FencingGeneration: fencingGeneration,
		CleanupProof: []byte(`{"kind":"test_cleanup"}`),
	}); err != nil {
		t.Fatal(err)
	}

	next, err := fixture.authority.PlaceReadyRun(fixture.ctx, secondCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if next.LeaseCreated || !next.RuntimeInstanceID.Valid || next.RuntimeInstanceID == reserved.RuntimeInstanceID {
		t.Fatalf("post-pressure reservation = %+v", next)
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

	candidates, err := queries.ListQueuedRunPlanningCandidatesForScopes(
		fixture.ctx,
		mustRunCandidateParams(t, planningScope{
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
	scopes := []planningScope{
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
	params, err := planningCandidateParams(scopes, 3)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.New(fixture.pool).ListQueuedRunPlanningCandidatesForScopes(fixture.ctx, params)
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

	isolationParams, err := planningCandidateParams(scopes[:1], 3)
	if err != nil {
		t.Fatal(err)
	}
	isolationParams.ProjectIds[0] = pgtype.UUID{Bytes: [16]byte{15: 99}, Valid: true}
	isolated, err := db.New(fixture.pool).ListQueuedRunPlanningCandidatesForScopes(fixture.ctx, isolationParams)
	if err != nil {
		t.Fatal(err)
	}
	if len(isolated) != 0 {
		t.Fatalf("mismatched project returned %d candidates", len(isolated))
	}
}

func TestRunPlanningExcludesQueuedRunWithoutPlacementOwnership(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	queries := db.New(fixture.pool)
	scope := planningScope{
		OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), RegionID: "us-east-1",
		QueueName: "default",
	}
	eligible, err := queries.ListQueuedRunEligibleScopes(fixture.ctx, db.ListQueuedRunEligibleScopesParams{
		RowLimit: 10, ScanSeed: "ownership",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(eligible) != 1 {
		t.Fatalf("eligible scopes before ownership change = %d, want 1", len(eligible))
	}

	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspaces
   SET owner_run_id = NULL
 WHERE id = $1`, fixture.workspaceID)

	if candidates := listRunPlacementCandidates(t, fixture, 1); len(candidates) != 1 {
		t.Fatalf("raw placement candidates = %d, want 1", len(candidates))
	}
	eligible, err = queries.ListQueuedRunEligibleScopes(fixture.ctx, db.ListQueuedRunEligibleScopesParams{
		RowLimit: 10, ScanSeed: "ownership",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(eligible) != 0 {
		t.Fatalf("eligible planning scopes after ownership change = %d, want 0", len(eligible))
	}
	params := mustRunCandidateParams(t, scope, 1)
	planning, err := queries.ListQueuedRunPlanningCandidatesForScopes(fixture.ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if len(planning) != 0 {
		t.Fatalf("planning candidates after ownership change = %d, want 0", len(planning))
	}
}

func TestRunPlanningHonorsRuntimePreparationBackoff(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET runtime_preparation_count = 1,
       next_runtime_preparation_at = transaction_timestamp() + interval '1 minute'
 WHERE id = $1`, fixture.runID)
	params := mustRunCandidateParams(t, planningScope{
		OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), RegionID: "us-east-1",
		QueueName: "default",
	}, 10)
	candidates, err := db.New(fixture.pool).ListQueuedRunPlanningCandidatesForScopes(
		fixture.ctx,
		params,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("backed-off planning candidates = %d, want 0", len(candidates))
	}
	if _, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate()); !errors.Is(err, ErrCandidateChanged) {
		t.Fatalf("backed-off direct placement error = %v", err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET next_runtime_preparation_at = transaction_timestamp() - interval '1 second'
 WHERE id = $1`, fixture.runID)
	candidates, err = db.New(fixture.pool).ListQueuedRunPlanningCandidatesForScopes(
		fixture.ctx,
		params,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].RunID != pgvalue.UUID(fixture.runID) {
		t.Fatalf("eligible planning candidates = %+v", candidates)
	}
}

func TestRunPlanningExcludesActorWithInvalidRestore(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	_, _, checkpointID := prepareActorSuspendedRestore(t, fixture)
	queries := db.New(fixture.pool)
	scope := planningScope{
		OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), RegionID: "us-east-1",
		QueueName: "default",
	}
	params := mustRunCandidateParams(t, scope, 1)
	planning, err := queries.ListQueuedRunPlanningCandidatesForScopes(fixture.ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if len(planning) != 1 {
		t.Fatalf("planning candidates before restore invalidation = %d, want 1", len(planning))
	}

	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_checkpoints
   SET state = 'invalid',
       ready_at = NULL,
       invalidated_at = transaction_timestamp(),
       invalidation_reason_code = 'test_invalid_restore'
 WHERE id = $1`, checkpointID)

	if candidates := listRunPlacementCandidates(t, fixture, 1); len(candidates) != 1 {
		t.Fatalf("raw placement candidates = %d, want 1", len(candidates))
	}
	eligible, err := queries.ListQueuedRunEligibleScopes(fixture.ctx, db.ListQueuedRunEligibleScopesParams{
		RowLimit: 10, ScanSeed: "invalid-actor-restore",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(eligible) != 0 {
		t.Fatalf("eligible planning scopes with invalid Actor restore = %d, want 0", len(eligible))
	}
	planning, err = queries.ListQueuedRunPlanningCandidatesForScopes(fixture.ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if len(planning) != 0 {
		t.Fatalf("planning candidates with invalid Actor restore = %d, want 0", len(planning))
	}
}

func TestRunPlacementAdvancesPastUnplaceableRunsInOneScope(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	seedDispatchMeasurement(t, fixture, 40, 1, 0, false)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
INSERT INTO regions (id, display_name)
VALUES ('blocked-region', 'Blocked region')`)

	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 40; index++ {
		runID := measurementUUID("run", index)
		workspaceID := measurementUUID("workspace", index)
		if index == 0 {
			runID = fixture.runID
			workspaceID = fixture.workspaceID
		}
		dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET priority = 0,
       queue_score_at = $2
 WHERE id = $1`, runID, base.Add(time.Duration(index)*time.Millisecond))
		if index < 35 {
			dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspaces
   SET region_id = 'blocked-region'
 WHERE id = $1`, workspaceID)
		}
	}

	store, err := NewRunPlacementStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &PlacementReconciler{
		runDiscovery: store,
		runAuthority: fixture.authority,
		runPolicy:    testRunPlacementPolicy(32),
	}
	lane := runPlacementLaneBytes(fixture.orgID)
	if _, err := reconciler.reconcileRunLane(fixture.ctx, lane, store); err != nil {
		t.Fatal(err)
	}
	eligibleRunID := measurementUUID("run", 35)
	var reserved int
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT count(*)
  FROM runtime_instances
 WHERE reserved_run_id = $1
   AND reclaimed_at IS NULL`, eligibleRunID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != 0 {
		t.Fatal("later placeable Run was reached before the first bounded candidate window completed")
	}
	if _, err := reconciler.reconcileRunLane(fixture.ctx, lane, store); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT count(*)
  FROM runtime_instances
 WHERE reserved_run_id = $1
   AND reclaimed_at IS NULL`, eligibleRunID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != 1 {
		t.Fatalf("later placeable Run reservations = %d, want 1", reserved)
	}
}

func TestRunPlacementRetriesPreparedScopeHeadBeforeDeepBacklog(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	seedDispatchMeasurement(t, fixture, 40, 1, 0, false)
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 40; index++ {
		runID := measurementUUID("run", index)
		if index == 0 {
			runID = fixture.runID
		}
		dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET priority = 0,
       queue_score_at = $2
 WHERE id = $1`, runID, base.Add(time.Duration(index)*time.Millisecond))
	}
	store, err := NewRunPlacementStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	policy := testRunPlacementPolicy(32)
	policy.pendingInterval = 0
	reconciler := &PlacementReconciler{
		runDiscovery: store,
		runAuthority: fixture.authority,
		runPolicy:    policy,
	}
	lane := runPlacementLaneBytes(fixture.orgID)

	if _, err := reconciler.reconcileRunLane(fixture.ctx, lane, store); err != nil {
		t.Fatal(err)
	}
	var runtimeID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT id
  FROM runtime_instances
 WHERE reserved_run_id = $1
   AND reclaimed_at IS NULL`, fixture.runID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, runtimeID)

	if _, err := reconciler.reconcileRunLane(fixture.ctx, lane, store); err != nil {
		t.Fatal(err)
	}
	var mountID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT id
  FROM workspace_mounts
 WHERE workspace_id = $1
   AND runtime_instance_id = $2
   AND state = 'mounting'`, fixture.workspaceID, runtimeID).Scan(&mountID); err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, mountID)

	if _, err := reconciler.reconcileRunLane(fixture.ctx, lane, store); err != nil {
		t.Fatal(err)
	}
	var leaseID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT current_run_lease_id
  FROM runs
 WHERE id = $1`, fixture.runID).Scan(&leaseID); err != nil {
		t.Fatal(err)
	}
	if !leaseID.Valid {
		t.Fatal("prepared scope-head Run was not granted before scanning the deep backlog")
	}
}

func TestRunPlacementAdvancesPastUnplaceableScopes(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	seedDispatchMeasurement(t, fixture, 33, 33, 0, false)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
INSERT INTO regions (id, display_name)
VALUES ('blocked-region', 'Blocked region')`)
	for index := 0; index < 32; index++ {
		workspaceID := measurementUUID("workspace", index)
		if index == 0 {
			workspaceID = fixture.workspaceID
		}
		dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspaces
   SET region_id = 'blocked-region'
 WHERE id = $1`, workspaceID)
	}

	store, err := NewRunPlacementStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &PlacementReconciler{
		runDiscovery: store,
		runAuthority: fixture.authority,
		runPolicy:    testRunPlacementPolicy(32),
	}
	lane := runPlacementLaneBytes(fixture.orgID)
	if _, err := reconciler.reconcileRunLane(fixture.ctx, lane, store); err != nil {
		t.Fatal(err)
	}
	eligibleRunID := measurementUUID("run", 32)
	var reserved int
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT count(*)
  FROM runtime_instances
 WHERE reserved_run_id = $1
   AND reclaimed_at IS NULL`, eligibleRunID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != 0 {
		t.Fatal("later placeable scope was reached before the first bounded scope window completed")
	}
	if _, err := reconciler.reconcileRunLane(fixture.ctx, lane, store); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT count(*)
  FROM runtime_instances
 WHERE reserved_run_id = $1
   AND reclaimed_at IS NULL`, eligibleRunID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != 1 {
		t.Fatalf("later placeable scope reservations = %d, want 1", reserved)
	}
}

func optionalString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func convertRunToActor(
	t *testing.T,
	fixture runPlacementFixture,
	runID uuid.UUID,
) {
	t.Helper()
	actorID := uuid.NewV7()
	definitionID := uuid.NewV7()
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
)`, definitionID, fixture.environmentID, fixture.deploymentID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO sessions (
    id, environment_id,
    actor_declared_id, deployment_definition_id, workspace_id,
    current_run_id, next_input_sequence, committed_input_sequence,
    run_queue_name, run_max_active_duration_ms
) VALUES (
    $1, $2, 'test-actor', $3, $4, $5, 2, 1, 'default', 300000
)`, actorID, fixture.environmentID, definitionID, fixture.workspaceID, runID)
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
 WHERE id = $1`, runID, definitionID, actorID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_attempts
   SET entrypoint_kind = 'actor', session_input_start_sequence = 1
 WHERE run_id = $1 AND number = 1`, runID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspaces
   SET owner_run_id = NULL, owner_session_id = $2
 WHERE id = $1`, fixture.workspaceID, actorID)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPlaceReadyActorStartWithoutCheckpoint(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	convertRunToActor(t, fixture, fixture.runID)
	queries := db.New(fixture.pool)
	eligible, err := queries.ListQueuedRunEligibleScopes(fixture.ctx, db.ListQueuedRunEligibleScopesParams{
		RowLimit: 10, ScanSeed: "fresh-actor-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(eligible) != 1 {
		t.Fatalf("fresh Actor planning scopes = %d, want 1", len(eligible))
	}
	planning, err := queries.ListQueuedRunPlanningCandidatesForScopes(
		fixture.ctx,
		mustRunCandidateParams(t, planningScope{
			OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
			EnvironmentID: pgvalue.UUID(fixture.environmentID), RegionID: "us-east-1",
			QueueName: "default",
		}, 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(planning) != 1 || planning[0].RunID != pgvalue.UUID(fixture.runID) {
		t.Fatalf("fresh Actor planning candidates = %+v, want current Run", planning)
	}

	candidates := listRunPlacementCandidates(t, fixture, 10)
	if len(candidates) != 1 || candidates[0].RunID != pgvalue.UUID(fixture.runID) {
		t.Fatalf("Actor start candidates = %+v", candidates)
	}

	placement, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	if !placement.RuntimeInstanceID.Valid || placement.LeaseCreated {
		t.Fatalf("Actor start placement = %+v", placement)
	}
	var restoreCheckpointID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT restore_checkpoint_id FROM runtime_instances WHERE id = $1`, placement.RuntimeInstanceID).Scan(&restoreCheckpointID); err != nil {
		t.Fatal(err)
	}
	if restoreCheckpointID.Valid {
		t.Fatalf("Actor start restore checkpoint = %s", pgvalue.UUIDString(restoreCheckpointID))
	}
}

func TestPlaceReadyRunRestoresCompatibleCheckpointAndBindsWait(t *testing.T) {
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
	var sourceMountGeneration int64
	err = fixture.pool.QueryRow(fixture.ctx, `
SELECT workspace_leases.id, workspace_leases.base_version_id,
       workspace_leases.mount_fencing_generation
  FROM workspace_leases
 WHERE owner_run_lease_id = $1`, granted.Lease.ID).Scan(
		&sourceWorkspaceLeaseID,
		&baseVersionID,
		&sourceMountGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}

	runWaitID := uuid.NewV7()
	checkpointID := uuid.NewV7()
	privateVersionID := uuid.NewV7()
	privateArtifactID := uuid.NewV7()
	privateDigest := "sha256:" + strings.Repeat("5", 64)
	resumeAttachID := uuid.NewV7()
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
    id, run_id, attempt_number, run_wait_id, source_run_lease_id,
    source_workspace_lease_id, workspace_id, base_workspace_version_id,
    private_workspace_version_id, state, restore_manifest,
    ready_request_fingerprint, ready_at
) VALUES (
    $1, $2, 1, $3, $4, $5, $6, $7, $8,
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
UPDATE worker_instances SET substrate_contract = $2 WHERE id = $1`,
		fixture.workerID, capacity.SubstrateContractExt4)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_leases SET mount_fencing_generation = $2 WHERE id = $1`,
		sourceWorkspaceLeaseID, int64(math.MaxInt64-1))
	if _, err := fixture.authority.PlaceReadyRun(fixture.ctx, restoreCandidate); !errors.Is(err, ErrCandidateChanged) {
		t.Fatalf("restore placement with exhausted Mount generation error = %v, want ErrCandidateChanged", err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_leases SET mount_fencing_generation = $2 WHERE id = $1`,
		sourceWorkspaceLeaseID, sourceMountGeneration)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_leases SET base_version_id = $2 WHERE id = $1`,
		sourceWorkspaceLeaseID, privateVersionID)
	if _, err := fixture.authority.PlaceReadyRun(fixture.ctx, restoreCandidate); !errors.Is(err, ErrCandidateChanged) {
		t.Fatalf("restore placement with crossed source Lease base error = %v, want ErrCandidateChanged", err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_leases SET base_version_id = $2 WHERE id = $1`,
		sourceWorkspaceLeaseID, baseVersionID)
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
	var restoreMaterializationGeneration int64
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT fencing_generation
  FROM workspace_mounts
 WHERE id = $1`, restoreMount.WorkspaceMountID).Scan(&restoreMaterializationGeneration); err != nil {
		t.Fatal(err)
	}
	if restoreMaterializationGeneration != sourceMountGeneration+1 {
		t.Fatalf("restore materialization generation = %d, want source %d + 1",
			restoreMaterializationGeneration, sourceMountGeneration)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts SET fencing_generation = 1 WHERE id = $1`, restoreMount.WorkspaceMountID)
	if _, err := fixture.authority.PlaceReadyRun(fixture.ctx, restoreCandidate); !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("restore placement with stale active Mount generation error = %v, want ErrCapacityUnavailable", err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts SET fencing_generation = $2 WHERE id = $1`,
		restoreMount.WorkspaceMountID, restoreMaterializationGeneration)
	replayedRestoreMount, err := fixture.authority.PlaceReadyRun(fixture.ctx, restoreCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if replayedRestoreMount.WorkspaceMountID != restoreMount.WorkspaceMountID {
		t.Fatalf("replayed restore Mount = %s, want %s",
			pgvalue.UUIDString(replayedRestoreMount.WorkspaceMountID),
			pgvalue.UUIDString(restoreMount.WorkspaceMountID))
	}
	markRunPlacementMountReady(t, fixture, restoreMount.WorkspaceMountID)
	restoreGrant, err := fixture.authority.PlaceReadyRun(fixture.ctx, restoreCandidate)
	if err != nil {
		t.Fatal(err)
	}

	var waitState string
	var waitLeaseID, leaseBaseVersionID, restoredCheckpointID, clearedReservation pgtype.UUID
	var restoredSubstrateID, sourceSubstrateID pgtype.UUID
	var restoredLeaseMountGeneration, restoredMountGeneration int64
	err = fixture.pool.QueryRow(fixture.ctx, `
SELECT run_waits.suspension_state,
       run_waits.current_run_lease_id,
       workspace_leases.base_version_id,
       runtime_instances.restore_checkpoint_id,
       runtime_instances.reserved_run_id,
       runtime_instances.runtime_substrate_id,
       source_runtime.runtime_substrate_id,
       workspace_leases.mount_fencing_generation,
       workspace_mounts.fencing_generation
  FROM run_waits
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = $2
  JOIN runtime_instances ON runtime_instances.id = workspace_leases.runtime_instance_id
  JOIN runtime_instances AS source_runtime ON source_runtime.id = $3
  JOIN workspace_mounts ON workspace_mounts.id = workspace_leases.workspace_mount_id
 WHERE run_waits.id = $1`, runWaitID, restoreGrant.Lease.ID, reserved.RuntimeInstanceID).Scan(
		&waitState, &waitLeaseID, &leaseBaseVersionID, &restoredCheckpointID, &clearedReservation,
		&restoredSubstrateID, &sourceSubstrateID, &restoredLeaseMountGeneration,
		&restoredMountGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}
	if waitState != "resuming" || waitLeaseID != restoreGrant.Lease.ID ||
		leaseBaseVersionID != pgvalue.UUID(privateVersionID) ||
		restoredCheckpointID != pgvalue.UUID(checkpointID) || clearedReservation.Valid ||
		!restoredSubstrateID.Valid || restoredSubstrateID != sourceSubstrateID {
		t.Fatalf("restore grant wait=%s lease=%s base=%s checkpoint=%s reserved=%s",
			waitState, pgvalue.UUIDString(waitLeaseID), pgvalue.UUIDString(leaseBaseVersionID),
			pgvalue.UUIDString(restoredCheckpointID), pgvalue.UUIDString(clearedReservation))
	}
	if restoredLeaseMountGeneration != sourceMountGeneration+2 ||
		restoredMountGeneration != sourceMountGeneration+2 {
		t.Fatalf("restored grant generations lease=%d mount=%d, want source %d + 2",
			restoredLeaseMountGeneration, restoredMountGeneration, sourceMountGeneration)
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
	// Reclaim the first restored runtime and grant once more. Physical loss
	// discovered after the Lease deadline closes the runtime; cleanup remains
	// blocked until recovery fences the Lease.
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
	       runtime_identity_id = NULL,
	       substrate_format = '',
	       substrate_contract = '',
       epoch_cpu_millis = 0,
       epoch_memory_bytes = 0,
       epoch_guest_ephemeral_disk_bytes = 0,
       per_vm_cpu_millis = 0,
       per_vm_memory_bytes = 0,
       per_vm_guest_ephemeral_disk_bytes = 0,
       max_vm_slots = 0,
       max_runtime_starts = 0,
	   cpu_environment = NULL,
	   cpu_environment_digest = NULL,
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
	var terminalEventCount int
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

func TestActorCurrentRunCheckpointRestoreAndRecovery(t *testing.T) {
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
			queued := listRunPlacementCandidates(t, fixture, 10)
			if len(queued) != 1 || queued[0].RunID != pgvalue.UUID(fixture.runID) {
				t.Fatalf("Actor restore candidates = %+v", queued)
			}
			if tc.invalidCursor {
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_checkpoints SET actor_speculative_input_sequence = 2 WHERE id = $1`, checkpointID)
				queued = listRunPlacementCandidates(t, fixture, 10)
				if len(queued) != 1 || queued[0].RunID != pgvalue.UUID(fixture.runID) {
					t.Fatalf("out-of-bounds Actor candidates = %+v", queued)
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

func TestPlaceReadyActorContinuationReusesRestoredRuntimeAtCurrentFrontier(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	actorID, waitID, checkpointID := prepareActorSuspendedRestore(t, fixture)
	restoreCandidate := ReadyRunCandidate{
		OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID),
		ExpectedRunStateVersion: 3,
	}
	reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, restoreCandidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
	mounting, err := fixture.authority.PlaceReadyRun(fixture.ctx, restoreCandidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, mounting.WorkspaceMountID)
	granted, err := fixture.authority.PlaceReadyRun(fixture.ctx, restoreCandidate)
	if err != nil {
		t.Fatal(err)
	}

	var workspaceLeaseID, frontierID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT workspace_leases.id, workspace_mounts.materialized_version_id
  FROM workspace_leases
  JOIN workspace_mounts ON workspace_mounts.id = workspace_leases.workspace_mount_id
 WHERE workspace_leases.owner_run_lease_id = $1`, granted.Lease.ID).Scan(
		&workspaceLeaseID, &frontierID,
	); err != nil {
		t.Fatal(err)
	}
	continuationFrontierID := pgvalue.NewUUIDv7()
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_leases
   SET state = 'completed', claimed_at = assigned_at, started_at = assigned_at,
       terminal_at = transaction_timestamp(), terminal_reason_code = 'completed'
 WHERE id = $1`, granted.Lease.ID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspace_leases
   SET state = 'released', released_at = transaction_timestamp(),
       terminal_at = transaction_timestamp()
 WHERE id = $1`, workspaceLeaseID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_waits
   SET suspension_state = 'released', current_run_lease_id = NULL,
       resume_ack_version = resume_request_version,
       suspension_terminal_at = transaction_timestamp()
 WHERE id = $1`, waitID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_attempts
   SET terminal_outcome = 'succeeded', terminal_reason_code = 'completed',
       terminal_session_input_sequence = 1, terminal_at = transaction_timestamp()
 WHERE run_id = $1 AND number = 1`, fixture.runID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runs
   SET status = 'succeeded', current_run_lease_id = NULL,
       output = '{}'::jsonb, terminal_at = transaction_timestamp()
 WHERE id = $1`, fixture.runID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id, parent_version_id, kind,
    content_digest, size_bytes, entry_count, state,
    source_workspace_lease_id, ownership_generation, writer_generation,
    artifact_id, artifact_kind, published_at
)
SELECT $1, workspace_versions.environment_id, workspace_versions.workspace_id,
       workspace_versions.id, workspace_versions.kind,
       workspace_versions.content_digest, workspace_versions.size_bytes,
       workspace_versions.entry_count, 'committed', $2,
       workspaces.ownership_generation, workspaces.writer_generation,
       workspace_versions.artifact_id, workspace_versions.artifact_kind,
       transaction_timestamp()
  FROM workspace_versions
  JOIN workspaces ON workspaces.id = workspace_versions.workspace_id
 WHERE workspace_versions.id = $3`, continuationFrontierID, workspaceLeaseID, frontierID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspace_mounts
   SET materialized_version_id = $2, updated_at = transaction_timestamp()
 WHERE id = $1`, mounting.WorkspaceMountID, continuationFrontierID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspaces SET head_version_id = $2 WHERE id = $1`, fixture.workspaceID, continuationFrontierID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE sessions
   SET current_run_id = NULL, committed_input_sequence = 1,
       next_input_sequence = 3
 WHERE id = $1`, actorID)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}

	continuationID := pgvalue.NewUUIDv7()
	continuation, err := db.New(fixture.pool).CreateActorContinuationRun(
		fixture.ctx,
		db.CreateActorContinuationRunParams{
			RunID: continuationID, QueueOriginAt: pgvalue.Timestamptz(time.Now().UTC()),
			TraceID: pgvalue.Text("55555555555555555555555555555555"), RootSpanID: "6666666666666666",
			EnvironmentID: pgvalue.UUID(fixture.environmentID), SessionID: pgvalue.UUID(actorID),
			WorkspaceID: pgvalue.UUID(fixture.workspaceID), ExpectedRunGeneration: 1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	placement, err := fixture.authority.PlaceReadyRun(fixture.ctx, ReadyRunCandidate{
		OrgID: pgvalue.UUID(fixture.orgID), RunID: continuation.ID,
		ExpectedRunStateVersion: continuation.StateVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !placement.LeaseCreated || placement.RuntimeInstanceID != reserved.RuntimeInstanceID ||
		placement.WorkspaceMountID != mounting.WorkspaceMountID {
		t.Fatalf("continuation placement = %+v, want restored runtime %s mount %s",
			placement, pgvalue.UUIDString(reserved.RuntimeInstanceID), pgvalue.UUIDString(mounting.WorkspaceMountID))
	}
	var historicalCheckpointID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT restore_checkpoint_id FROM runtime_instances WHERE id = $1`,
		placement.RuntimeInstanceID).Scan(&historicalCheckpointID); err != nil {
		t.Fatal(err)
	}
	if historicalCheckpointID != pgvalue.UUID(checkpointID) {
		t.Fatalf("historical restore checkpoint = %s, want %s",
			pgvalue.UUIDString(historicalCheckpointID), checkpointID)
	}
	var continuationWorkspaceLeaseID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT id FROM workspace_leases WHERE owner_run_lease_id = $1`,
		placement.Lease.ID).Scan(&continuationWorkspaceLeaseID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'completed', claimed_at = assigned_at, started_at = assigned_at,
       terminal_at = transaction_timestamp(), terminal_reason_code = 'completed'
 WHERE id = $1`, placement.Lease.ID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_leases
   SET state = 'released', released_at = transaction_timestamp(),
       terminal_at = transaction_timestamp()
 WHERE id = $1`, continuationWorkspaceLeaseID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET status = 'succeeded', current_run_lease_id = NULL,
       output = '{}'::jsonb, terminal_at = transaction_timestamp()
 WHERE id = $1`, continuation.ID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE sessions
   SET state = 'closed', current_run_id = NULL, closed_at = transaction_timestamp()
 WHERE id = $1`, actorID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspaces SET owner_session_id = NULL WHERE id = $1`, fixture.workspaceID)
	processID, _ := placeWorkspaceExecForClaim(t, fixture)
	var processRuntimeID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runtime_instance_id
  FROM workspace_leases
 WHERE owner_process_id = $1`, processID).Scan(&processRuntimeID); err != nil {
		t.Fatal(err)
	}
	if processRuntimeID != reserved.RuntimeInstanceID {
		t.Fatalf("Workspace Exec runtime = %s, want restored runtime %s",
			pgvalue.UUIDString(processRuntimeID), pgvalue.UUIDString(reserved.RuntimeInstanceID))
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
	actorID := uuid.NewV7()
	actorDefinitionID := uuid.NewV7()
	waitID := uuid.NewV7()
	checkpointID := uuid.NewV7()
	privateVersionID := uuid.NewV7()
	privateArtifactID := uuid.NewV7()
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
		fixture.workspaceID, grant.Lease.ID, uuid.NewV7())
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_checkpoints (
    id, run_id, attempt_number, run_wait_id, source_run_lease_id,
    source_workspace_lease_id, workspace_id, base_workspace_version_id,
    private_workspace_version_id, actor_speculative_input_sequence,
    state, restore_manifest, ready_request_fingerprint, ready_at
) VALUES ($1, $2, 1, $3, $4, $5, $6, $7, $8, 1,
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
	var vmVCPUCount int32
	var cpuConfigDigest string
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
		   'sha256:test-runtime-substrate', $3, $4, 1
      FROM runtime
    ON CONFLICT ON CONSTRAINT runtime_substrates_input_key DO NOTHING
    RETURNING id
)
SELECT id FROM inserted
UNION ALL
SELECT runtime_substrates.id
  FROM runtime_substrates
  JOIN runtime USING (org_id, project_id, environment_id, deployment_definition_id)
 WHERE substrate_format = $3
   AND substrate_contract = $4
LIMIT 1`, runtimeID, pgvalue.NewUUIDv7(),
		capacity.SubstrateFormatExt4, capacity.SubstrateContractExt4).Scan(&runtimeSubstrateID)
	if err != nil {
		t.Fatal(err)
	}
	err = fixture.pool.QueryRow(fixture.ctx, `
SELECT runtime_instances.desired_version,
       runtime_instances.observed_version,
       runtime_instances.worker_instance_id,
       runtime_instances.worker_epoch,
       runtime_instances.vm_vcpu_count,
       runtime_instances.cpu_config_digest
  FROM runtime_instances
 WHERE runtime_instances.id = $1`, runtimeID).Scan(
		&desiredVersion, &observedVersion, &workerID, &workerEpoch,
		&vmVCPUCount, &cpuConfigDigest,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.New(fixture.pool).MarkRuntimeInstanceReady(fixture.ctx, db.MarkRuntimeInstanceReadyParams{
		RuntimeSubstrateID: runtimeSubstrateID,
		DesiredVersion:     desiredVersion, ID: runtimeID, WorkerInstanceID: workerID,
		WorkerEpoch: workerEpoch, ExpectedObservedVersion: observedVersion,
		VMVCPUCount: vmVCPUCount, CPUConfigDigest: cpuConfigDigest,
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

func (fixture runPlacementFixture) candidate() ReadyRunCandidate {
	return ReadyRunCandidate{
		OrgID:                   pgvalue.UUID(fixture.orgID),
		RunID:                   pgvalue.UUID(fixture.runID),
		ExpectedRunStateVersion: 1,
	}
}

func newRunPlacementFixture(t *testing.T) runPlacementFixture {
	return newRunPlacementFixtureWithSeed(t, uuid.New().String())
}

func newRunPlacementFixtureWithSeed(t *testing.T, seed string) runPlacementFixture {
	t.Helper()
	ctx := context.Background()
	pool := newDispatchIntegrationDB(t, ctx)
	id := func(kind string) uuid.UUID {
		return deterministicUUID(seed + ":" + kind)
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
	programID := id("program-artifact")
	imageID := id("image-artifact")
	bundleDigest := "sha256:" + strings.Repeat("1", 64)
	runtimeDigest := "sha256:" + strings.Repeat("3", 64)
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
    ($1, $2, 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
    ($1, $3, 1, 'application/octet-stream')`,
		fixture.orgID,
		programDigest,
		imageDigest,
	)
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
) VALUES
    ($1, $3, $4, $5, $6, 'deployment_program', 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
    ($2, $3, $4, $5, $7, 'workspace_image', 1, 'application/octet-stream')`,
		programID,
		imageID,
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		programDigest,
		imageDigest,
	)
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO deployments (
    id, org_id, project_id, environment_id, version, bundle_digest,
    runtime_artifact_digest, program_artifact_id, program_index_digest, queue_config
) VALUES (
    $1, $2, $3, $4, 'v1', $5, $6, $7,
    decode(repeat('03', 32), 'hex'), '{}'::jsonb
)`,
		deploymentID,
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		bundleDigest,
		runtimeDigest,
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
    id, token_id, region_id, name
)
SELECT $1, token.id, 'us-east-1', $1 FROM token`,
		fixture.groupID, id("worker-group-token"), dbtest.Hash("run-placement-worker-group"),
	)
	poolSpec := dispatchWorkerPoolFixture{
		substrateFormat:                 capacity.SubstrateFormatExt4,
		substrateContract:               capacity.SubstrateContractExt4,
		capacityCPUMillis:               8000,
		capacityMemoryBytes:             8589934592,
		capacityGuestEphemeralDiskBytes: 274877906944,
		perVMCPUMillis:                  1000,
		perVMMemoryBytes:                1073741824,
		perVMGuestEphemeralDiskBytes:    34359738368,
		maxVMSlots:                      8,
	}
	seedDispatchWorkerPool(t, ctx, pool, fixture.groupID, poolSpec)
	cpuEnvironment, cpuEnvironmentDigest := dispatchCPUEnvironment(t)
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO worker_instances (
	id, resource_id, worker_group_id, worker_pool_id, state,
	current_epoch, current_service_id,
	runtime_identity_id,
	substrate_format, substrate_contract,
    epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
    per_vm_cpu_millis, per_vm_memory_bytes,
    per_vm_guest_ephemeral_disk_bytes, max_vm_slots, max_runtime_starts,
    cpu_environment, cpu_environment_digest,
    observed_at, epoch_started_at, activated_at
) VALUES (
	$1, $2, $3, $4, 'active', 1, $5,
	$6, $16, $17,
	$7, $8, $9,
	$10, $11, $12,
	$13, $13,
	$14::jsonb, $15, now(), now(), now()
)`,
		fixture.workerID,
		fixture.workerID.String(),
		fixture.groupID,
		dbtest.DefaultWorkerPoolID,
		id("worker-service"),
		dbtest.DefaultRuntimeID,
		poolSpec.capacityCPUMillis,
		poolSpec.capacityMemoryBytes,
		poolSpec.capacityGuestEphemeralDiskBytes,
		poolSpec.perVMCPUMillis,
		poolSpec.perVMMemoryBytes,
		poolSpec.perVMGuestEphemeralDiskBytes,
		poolSpec.maxVMSlots,
		cpuEnvironment,
		cpuEnvironmentDigest,
		poolSpec.substrateFormat,
		poolSpec.substrateContract,
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
