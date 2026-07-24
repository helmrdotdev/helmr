package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type runPlacementFixture struct {
	ctx           context.Context
	pool          *pgxpool.Pool
	authority     *Authority
	fencingKeys   workspace.FencingKeys
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
	freshAfter := pgvalue.Timestamptz(time.Now().Add(-time.Minute))

	reserved, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		candidate,
		freshAfter,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.LeaseCreated ||
		!reserved.RuntimeInstanceID.Valid ||
		reserved.WorkspaceMountID.Valid {
		t.Fatalf("reservation placement = %+v", reserved)
	}

	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
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
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_network_slots
   SET state = 'bound',
       host_interface_name = 'veth-test',
       guest_address = '10.0.0.2',
       gateway_address = '10.0.0.1',
       subnet = '10.0.0.0/24',
       tap_name = 'tap-test',
       netns_name = 'netns-test',
       guest_mac = '02:00:00:00:00:01'
 WHERE runtime_instance_id = $1`,
		reserved.RuntimeInstanceID,
	)

	mounting, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		candidate,
		freshAfter,
	)
	if err != nil {
		t.Fatal(err)
	}
	if mounting.LeaseCreated ||
		!mounting.WorkspaceMountID.Valid ||
		mounting.RuntimeInstanceID != reserved.RuntimeInstanceID {
		t.Fatalf("mounting placement = %+v", mounting)
	}
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts
   SET state = 'mounted',
       mounted_at = transaction_timestamp()
 WHERE id = $1`,
		mounting.WorkspaceMountID,
	)

	granted, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		candidate,
		freshAfter,
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
	var keyFingerprint []byte
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
       workspace_leases.fencing_key_fingerprint,
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
		&keyFingerprint,
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
	replayed, err := fixture.fencingKeys.DeriveActive(workspace.FenceInput{
		LeaseID:                leaseUUID,
		WorkspaceID:            fixture.workspaceID,
		OwnershipGeneration:    1,
		WriterGeneration:       writerGeneration,
		MountFencingGeneration: mountGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(keyFingerprint, replayed.KeyFingerprint.Bytes()) ||
		tokenHash != replayed.Hash ||
		tokenHash == replayed.Token {
		t.Fatal("Workspace Lease did not persist the replayable fingerprint and token hash")
	}
}

func TestPlaceReadyRunRecreatesExactSuspendedRuntimeAndBindsWait(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	candidate := fixture.candidate()
	freshAfter := pgvalue.Timestamptz(time.Now().Add(-time.Minute))

	reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate, freshAfter)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
	mounting, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate, freshAfter)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, mounting.WorkspaceMountID)
	granted, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate, freshAfter)
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
	mustRunPlacementExec(t, fixture.ctx, tx, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES ($1, $2, 1, $3)`, fixture.orgID, privateDigest, workspace.ArtifactMediaType)
	mustRunPlacementExec(t, fixture.ctx, tx, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
) VALUES ($1, $2, $3, $4, $5, 'workspace_version', 1, $6)`,
		privateArtifactID, fixture.orgID, fixture.projectID, fixture.environmentID,
		privateDigest, workspace.ArtifactMediaType,
	)
	mustRunPlacementExec(t, fixture.ctx, tx, `
INSERT INTO workspace_versions (
    id, public_id, org_id, project_id, environment_id, workspace_id,
    parent_version_id, kind, content_digest, state, source_workspace_lease_id,
    ownership_generation, writer_generation, artifact_id, artifact_kind,
    entry_count, size_bytes
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, 'user', $8, 'private', $9,
    1, 1, $10, 'workspace_version', 1, 1
)`,
		privateVersionID, dispatchPublicID(t, publicid.WorkspaceVersion), fixture.orgID,
		fixture.projectID, fixture.environmentID, fixture.workspaceID, baseVersionID,
		privateDigest, sourceWorkspaceLeaseID, privateArtifactID,
	)
	mustRunPlacementExec(t, fixture.ctx, tx, `
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
	mustRunPlacementExec(t, fixture.ctx, tx, `
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
	mustRunPlacementExec(t, fixture.ctx, tx, `
UPDATE run_waits
   SET suspension_state = 'resume_pending', suspend_checkpoint_id = $2,
       resume_request_version = 1
 WHERE id = $1`, runWaitID, checkpointID)
	mustRunPlacementExec(t, fixture.ctx, tx, `
UPDATE runs
   SET current_run_lease_id = NULL, state_version = 3, updated_at = now()
 WHERE id = $1 AND current_run_lease_id = $2`, fixture.runID, granted.Lease.ID)
	mustRunPlacementExec(t, fixture.ctx, tx, `
UPDATE run_leases
   SET state = 'checkpointed', claimed_at = assigned_at, started_at = assigned_at,
       checkpointed_at = now(), terminal_at = now(), terminal_reason_code = 'checkpointed'
 WHERE id = $1`, granted.Lease.ID)
	mustRunPlacementExec(t, fixture.ctx, tx, `
UPDATE workspace_leases
   SET state = 'released', released_at = now(), terminal_at = now()
 WHERE id = $1`, sourceWorkspaceLeaseID)
	mustRunPlacementExec(t, fixture.ctx, tx, `
UPDATE workspace_mounts
   SET state = 'unmounted', unmounted_at = now(), terminal_at = now(),
       terminal_reason_code = 'checkpointed'
 WHERE id = $1`, mounting.WorkspaceMountID)
	mustRunPlacementExec(t, fixture.ctx, tx, `
UPDATE runtime_instances
   SET desired_state = 'closed', desired_version = desired_version + 1,
       observed_state = 'closed', observed_desired_version = desired_version + 1,
       observed_version = observed_version + 1, closing_at = now(), closed_at = now(),
       terminal_at = now(), terminal_reason_code = 'checkpointed',
       reclaimed_at = now(), reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL
 WHERE id = $1`, reserved.RuntimeInstanceID)
	mustRunPlacementExec(t, fixture.ctx, tx, `
UPDATE worker_network_slots
   SET state = 'available', runtime_instance_id = NULL,
       host_interface_name = NULL, guest_address = NULL, gateway_address = NULL,
       subnet = NULL, tap_name = NULL, netns_name = NULL, guest_mac = NULL
 WHERE runtime_instance_id = $1`, reserved.RuntimeInstanceID)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}

	restoreCandidate := ReadyRunCandidate{
		OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID),
		ExpectedRunStateVersion: 3,
	}
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances SET substrate_layout_abi = 'incompatible-layout' WHERE id = $1`, fixture.workerID)
	if _, err := fixture.authority.PlaceReadyRun(fixture.ctx, restoreCandidate, freshAfter); !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("restore placement with incompatible substrate contract error = %v, want ErrCapacityUnavailable", err)
	}
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances SET substrate_layout_abi = 'layout-v0' WHERE id = $1`, fixture.workerID)
	restored, err := fixture.authority.PlaceReadyRun(fixture.ctx, restoreCandidate, freshAfter)
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
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE runtime_substrates
   SET retired_at = transaction_timestamp()
 WHERE id = (SELECT runtime_substrate_id FROM runtime_instances WHERE id = $1)`, reserved.RuntimeInstanceID)
	if err := markRunPlacementRuntimeReadyQuery(t, fixture, restored.RuntimeInstanceID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mark ready with retired substrate error = %v, want pgx.ErrNoRows", err)
	}
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE runtime_substrates
   SET retired_at = NULL
 WHERE id = (SELECT runtime_substrate_id FROM runtime_instances WHERE id = $1)`, reserved.RuntimeInstanceID)
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE run_checkpoints
   SET state = 'invalid', ready_at = NULL, invalidated_at = now(),
       invalidation_reason_code = 'test_invalidated'
 WHERE id = $1`, checkpointID)
	if err := markRunPlacementRuntimeReadyQuery(t, fixture, restored.RuntimeInstanceID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mark ready with invalidated Checkpoint error = %v, want pgx.ErrNoRows", err)
	}
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE run_checkpoints
   SET state = 'ready', ready_at = now(), invalidated_at = NULL,
       invalidation_reason_code = NULL
 WHERE id = $1`, checkpointID)
	if err := markRunPlacementRuntimeReadyQuery(t, fixture, restored.RuntimeInstanceID); err != nil {
		t.Fatal(err)
	}
	restoreMount, err := fixture.authority.PlaceReadyRun(fixture.ctx, restoreCandidate, freshAfter)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, restoreMount.WorkspaceMountID)
	restoreGrant, err := fixture.authority.PlaceReadyRun(fixture.ctx, restoreCandidate, freshAfter)
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
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'running',
       assigned_at = transaction_timestamp() - interval '20 seconds',
       start_deadline_at = transaction_timestamp() - interval '19 seconds',
       claimed_at = transaction_timestamp() - interval '18 seconds',
       started_at = transaction_timestamp() - interval '18 seconds'
 WHERE id = $1 AND state = 'assigned'`, restoreGrant.Lease.ID)
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
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
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
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
	recovered, err := db.New(fixture.pool).RecoverExpiredRecreatedRunResumes(fixture.ctx, 10)
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

	// Reclaim the first recreated runtime, grant once more, then prove a failed
	// runtime/Mount redrives the exact restore immediately even while its Run
	// Lease is still live. Physical cleanup must remain blocked until that Lease
	// is fenced; recovery must also tolerate the slot's historical generation
	// after the worker is subsequently lost.
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts
   SET state = 'unmounted', unmounted_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(), terminal_reason_code = 'test_reclaimed'
 WHERE id = $1 AND state = 'unmounting'`, restoreMount.WorkspaceMountID)
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE runtime_instances
   SET observed_state = 'closed', observed_version = observed_version + 1,
       observed_desired_version = desired_version, observed_at = transaction_timestamp(),
       closing_at = transaction_timestamp(), closed_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(), terminal_reason_code = 'test_reclaimed',
       reclaimed_at = transaction_timestamp()
 WHERE id = $1 AND desired_state = 'closed'`, restored.RuntimeInstanceID)
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_network_slots
   SET state = 'available', runtime_instance_id = NULL,
       host_interface_name = NULL, guest_address = NULL, gateway_address = NULL,
       subnet = NULL, tap_name = NULL, netns_name = NULL, guest_mac = NULL,
       reclaimed_at = transaction_timestamp(),
       reclaim_evidence = '{"reason":"test_reclaimed"}'::jsonb
 WHERE runtime_instance_id = $1`, restored.RuntimeInstanceID)

	secondRestoreCandidate := ReadyRunCandidate{
		OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID),
		ExpectedRunStateVersion: 6,
	}
	secondRestored, err := fixture.authority.PlaceReadyRun(fixture.ctx, secondRestoreCandidate, freshAfter)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, secondRestored.RuntimeInstanceID)
	secondRestoreMount, err := fixture.authority.PlaceReadyRun(fixture.ctx, secondRestoreCandidate, freshAfter)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, secondRestoreMount.WorkspaceMountID)
	secondRestoreGrant, err := fixture.authority.PlaceReadyRun(fixture.ctx, secondRestoreCandidate, freshAfter)
	if err != nil {
		t.Fatal(err)
	}
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
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
		rows []db.RecoverExpiredRecreatedRunResumesRow
		err  error
	}
	recoveryDone := make(chan recoveryResult, 1)
	go func() {
		rows, recoverErr := db.New(fixture.pool).RecoverExpiredRecreatedRunResumes(fixture.ctx, 10)
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
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'running',
       assigned_at = transaction_timestamp() - interval '20 seconds',
       start_deadline_at = transaction_timestamp() - interval '19 seconds',
       claimed_at = transaction_timestamp() - interval '18 seconds',
       started_at = transaction_timestamp() - interval '18 seconds'
 WHERE id = $1`, secondRestoreGrant.Lease.ID)
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
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
	if _, err := startupTx.Exec(fixture.ctx, `
UPDATE worker_instances
   SET state = 'registering', current_epoch = 2,
       epoch_started_at = transaction_timestamp(), updated_at = transaction_timestamp()
 WHERE id = $1`, fixture.workerID); err != nil {
		t.Fatal(err)
	}
	_, err = db.New(startupTx).RecordWorkerStartupRecovery(fixture.ctx, db.RecordWorkerStartupRecoveryParams{
		RecoveryEvidence: []byte(`{"quarantined":[]}`),
		WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerGroupID:    fixture.groupID,
		WorkerEpoch:      pgtype.Int8{Int64: 2, Valid: true},
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("startup recovery with active prior-epoch Run Lease error = %v, want pgx.ErrNoRows", err)
	}
	var startupReclaimedAt pgtype.Timestamptz
	var startupRuntimeID pgtype.UUID
	var startupSlotGeneration int64
	if err := startupTx.QueryRow(fixture.ctx, `
SELECT runtime_instances.reclaimed_at,
       worker_network_slots.runtime_instance_id,
       worker_network_slots.generation
  FROM runtime_instances
  JOIN worker_network_slots
    ON worker_network_slots.id = $2
 WHERE runtime_instances.id = $1`,
		secondRestored.RuntimeInstanceID,
		secondRestoreGrant.Lease.NetworkSlotID,
	).Scan(&startupReclaimedAt, &startupRuntimeID, &startupSlotGeneration); err != nil {
		t.Fatal(err)
	}
	if startupReclaimedAt.Valid || startupRuntimeID != secondRestored.RuntimeInstanceID ||
		startupSlotGeneration != secondRestoreGrant.Lease.NetworkSlotGeneration {
		t.Fatalf("startup cleanup erased active restore fence: reclaimed=%v runtime=%s generation=%d",
			startupReclaimedAt.Valid, pgvalue.UUIDString(startupRuntimeID), startupSlotGeneration)
	}
	if err := startupTx.Rollback(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts
   SET state = 'failed', failed_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(), terminal_reason_code = 'test_mount_failed'
 WHERE id = $1`, secondRestoreMount.WorkspaceMountID)
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE runtime_instances
   SET observed_state = 'failed', observed_version = observed_version + 1,
       observed_at = transaction_timestamp(), failed_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(), terminal_reason_code = 'test_runtime_failed',
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL
 WHERE id = $1`, secondRestored.RuntimeInstanceID)
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_network_slots
   SET state = 'quarantined', reclaiming_at = transaction_timestamp(),
       quarantined_at = transaction_timestamp(),
       state_reason_code = 'runtime_physical_cleanup_pending'
 WHERE runtime_instance_id = $1`, secondRestored.RuntimeInstanceID)
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
		NetworkSlotID:           secondRestoreGrant.Lease.NetworkSlotID,
		NetworkSlotGeneration:   secondRestoreGrant.Lease.NetworkSlotGeneration,
		CleanupProof:            []byte(`{"reason":"test_cleanup"}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reclaim failed restore runtime with active Lease error = %v, want pgx.ErrNoRows", err)
	}
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET state = 'lost', lost_at = transaction_timestamp()
 WHERE id = $1`, fixture.workerID)
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_network_slots
   SET state = 'lost', generation = generation + 1,
       lost_at = transaction_timestamp(), state_reason_code = 'test_worker_lost'
 WHERE runtime_instance_id = $1`, secondRestored.RuntimeInstanceID)
	recovered, err = db.New(fixture.pool).RecoverExpiredRecreatedRunResumes(fixture.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].ID != pgvalue.UUID(runWaitID) || recovered[0].RunID != pgvalue.UUID(fixture.runID) {
		t.Fatalf("worker-loss recovered resumes = %+v", recovered)
	}
	var lostRunState, lostWaitState, lostMountState string
	var lostRunLeaseState, lostRunLeaseReason, lostWorkspaceLeaseState, lostWorkspaceLeaseReason string
	var lostAttemptOutcome, lostRunReason pgtype.Text
	var lostAttemptTerminalAt, lostRunTerminalAt pgtype.Timestamptz
	var lostActiveElapsed, lostRunVersion, lostResumeRequestVersion, lostSlotGeneration int64
	var lostWaitLeaseID pgtype.UUID
	var ownerRunID pgtype.UUID
	err = fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.status, run_waits.suspension_state, run_waits.current_run_lease_id,
       runs.state_version, run_waits.resume_request_version,
       run_attempts.terminal_outcome, run_attempts.terminal_at,
       runs.terminal_reason_code, runs.terminal_at, runs.active_elapsed_ms,
       workspaces.owner_run_id, run_leases.state, run_leases.terminal_reason_code,
       workspace_leases.state, workspace_leases.terminal_reason_code,
       workspace_mounts.state, worker_network_slots.generation
  FROM runs
  JOIN run_waits ON run_waits.run_id = runs.id
  JOIN run_attempts
    ON run_attempts.run_id = runs.id
   AND run_attempts.number = runs.current_attempt_number
  JOIN workspaces ON workspaces.id = runs.workspace_id
  JOIN run_leases ON run_leases.id = $2
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
  JOIN workspace_mounts ON workspace_mounts.id = workspace_leases.workspace_mount_id
  JOIN worker_network_slots ON worker_network_slots.id = run_leases.network_slot_id
 WHERE runs.id = $1`, fixture.runID, secondRestoreGrant.Lease.ID).Scan(
		&lostRunState, &lostWaitState, &lostWaitLeaseID,
		&lostRunVersion, &lostResumeRequestVersion,
		&lostAttemptOutcome, &lostAttemptTerminalAt,
		&lostRunReason, &lostRunTerminalAt, &lostActiveElapsed,
		&ownerRunID, &lostRunLeaseState, &lostRunLeaseReason,
		&lostWorkspaceLeaseState, &lostWorkspaceLeaseReason,
		&lostMountState, &lostSlotGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}
	if lostRunState != "queued" || lostWaitState != "resume_pending" || lostWaitLeaseID.Valid ||
		lostRunVersion != 9 || lostResumeRequestVersion != 3 ||
		lostAttemptOutcome.Valid || lostAttemptTerminalAt.Valid ||
		lostRunReason.Valid || lostRunTerminalAt.Valid ||
		lostActiveElapsed < 9000 || lostActiveElapsed > 15000 ||
		ownerRunID != pgvalue.UUID(fixture.runID) ||
		lostRunLeaseState != "expired" || lostRunLeaseReason != "runtime_failed" ||
		lostWorkspaceLeaseState != "expired" || lostWorkspaceLeaseReason != "runtime_failed" ||
		lostMountState != "failed" ||
		lostSlotGeneration != secondRestoreGrant.Lease.NetworkSlotGeneration+1 {
		t.Fatalf("worker-loss recovery run=%s wait=%s wait_lease=%s run_version=%d request_version=%d attempt=%v attempt_at=%v run_reason=%v run_at=%v active=%d owner=%s run_lease=%s/%s workspace_lease=%s/%s mount=%s slot_generation=%d",
			lostRunState, lostWaitState, pgvalue.UUIDString(lostWaitLeaseID), lostRunVersion,
			lostResumeRequestVersion, lostAttemptOutcome, lostAttemptTerminalAt,
			lostRunReason, lostRunTerminalAt, lostActiveElapsed, pgvalue.UUIDString(ownerRunID),
			lostRunLeaseState, lostRunLeaseReason, lostWorkspaceLeaseState,
			lostWorkspaceLeaseReason, lostMountState, lostSlotGeneration)
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
		{name: "unavailable checkpoint", invalidate: true, wantActorState: "failed", wantActorReason: pgtype.Text{String: "platform-failure", Valid: true}, wantRunStatus: "system_failed"},
		{name: "maximum active duration", maxDuration: true, wantActorState: "failed", wantActorReason: pgtype.Text{String: "run-expired", Valid: true}, wantRunStatus: "expired"},
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
			queued, err := db.New(fixture.pool).ListQueuedRunDispatchCandidatesForScope(
				fixture.ctx,
				db.ListQueuedRunDispatchCandidatesForScopeParams{
					OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
					EnvironmentID: pgvalue.UUID(fixture.environmentID), RegionID: "us-east-1",
					QueueName: "default", RowLimit: 10,
				},
			)
			if err != nil || len(queued) != 1 || queued[0].RunID != pgvalue.UUID(fixture.runID) {
				t.Fatalf("Actor restore dispatch candidates = %+v, error = %v", queued, err)
			}
			scopes, err := db.New(fixture.pool).ListQueuedRunCandidateScopes(
				fixture.ctx,
				db.ListQueuedRunCandidateScopesParams{RowLimit: 10, ScanSeed: "actor-restore"},
			)
			if err != nil || len(scopes) != 1 || scopes[0].EnvironmentID != pgvalue.UUID(fixture.environmentID) {
				t.Fatalf("Actor restore candidate scopes = %+v, error = %v", scopes, err)
			}
			freshAfter := pgvalue.Timestamptz(time.Now().Add(-time.Minute))
			if tc.invalidCursor {
				mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE run_checkpoints SET actor_speculative_input_sequence = 2 WHERE id = $1`, checkpointID)
				if _, err := db.New(fixture.pool).GetQueuedRunReadyHint(fixture.ctx, db.GetQueuedRunReadyHintParams{
					OrgID: pgvalue.UUID(fixture.orgID), RunID: pgvalue.UUID(fixture.runID),
				}); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("out-of-bounds Actor restore ready hint error = %v, want pgx.ErrNoRows", err)
				}
				queued, err := db.New(fixture.pool).ListQueuedRunDispatchCandidatesForScope(
					fixture.ctx,
					db.ListQueuedRunDispatchCandidatesForScopeParams{
						OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
						EnvironmentID: pgvalue.UUID(fixture.environmentID), RegionID: "us-east-1",
						QueueName: "default", RowLimit: 10,
					},
				)
				if err != nil || len(queued) != 0 {
					t.Fatalf("out-of-bounds Actor dispatch candidates = %+v, error = %v", queued, err)
				}
				if _, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate, freshAfter); !errors.Is(err, ErrCandidateChanged) {
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
			reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate, freshAfter)
			if err != nil {
				t.Fatal(err)
			}
			markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
			mount, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate, freshAfter)
			if err != nil {
				t.Fatal(err)
			}
			markRunPlacementMountReady(t, fixture, mount.WorkspaceMountID)
			grant, err := fixture.authority.PlaceReadyRun(fixture.ctx, candidate, freshAfter)
			if err != nil {
				t.Fatal(err)
			}
			if tc.invalidate {
				mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE run_checkpoints
   SET state = 'invalid', ready_at = NULL, invalidated_at = transaction_timestamp(),
       invalidation_reason_code = 'test_unavailable'
 WHERE id = $1`, checkpointID)
			}
			if tc.maxDuration {
				mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'running', claimed_at = assigned_at, started_at = assigned_at
 WHERE id = $1`, grant.Lease.ID)
				mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET status = 'running', max_active_duration_ms = 5000,
       active_started_at = transaction_timestamp() - interval '10 seconds',
       started_at = coalesce(started_at, transaction_timestamp() - interval '10 seconds'),
       state_version = state_version + 1
 WHERE id = $1`, fixture.runID)
			}
			mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
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
			recovered, err := db.New(fixture.pool).RecoverExpiredRecreatedRunResumes(fixture.ctx, 10)
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
SELECT actors.state, actors.current_run_id, actors.run_generation, actors.state_version,
       actors.failure_code, workspaces.owner_actor_id, workspaces.ownership_generation,
       run_waits.suspension_state, runs.status, run_attempts.terminal_actor_input_sequence
  FROM actors
  JOIN workspaces ON workspaces.id = actors.workspace_id
  JOIN runs ON runs.id = $2
  JOIN run_waits ON run_waits.id = $3
  JOIN run_attempts ON run_attempts.run_id = runs.id AND run_attempts.number = runs.current_attempt_number
 WHERE actors.id = $1`, actorID, fixture.runID, waitID).Scan(
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
	freshAfter := pgvalue.Timestamptz(time.Now().Add(-time.Minute))
	reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate(), freshAfter)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
	mount, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate(), freshAfter)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, mount.WorkspaceMountID)
	grant, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate(), freshAfter)
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
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
ALTER TABLE run_attempts
ALTER CONSTRAINT run_attempts_run_id_entrypoint_kind_workspace_id_fkey
DEFERRABLE INITIALLY DEFERRED`)
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	mustRunPlacementExec(t, fixture.ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	mustRunPlacementExec(t, fixture.ctx, tx, `
INSERT INTO deployment_definitions (
    id, environment_id, deployment_id, kind, declared_id, manifest_version, manifest, manifest_digest
) VALUES ($1, $2, $3, 'actor', 'test-actor', 0, '{}'::jsonb, decode(repeat('06', 32), 'hex'))`,
		actorDefinitionID, fixture.environmentID, fixture.deploymentID)
	mustRunPlacementExec(t, fixture.ctx, tx, `
INSERT INTO actors (
    id, public_id, org_id, project_id, environment_id, actor_declared_id,
    deployment_definition_id, workspace_id, current_run_id,
    next_input_sequence, committed_input_sequence, managed_queue_name,
    managed_max_active_duration_ms
) VALUES ($1, $2, $3, $4, $5, 'test-actor', $6, $7, $8, 2, 1, 'default', 300000)`,
		actorID, "act_aaaaaaaaaaaaaaaaaaaaaaaaaa", fixture.orgID, fixture.projectID,
		fixture.environmentID, actorDefinitionID, fixture.workspaceID, fixture.runID)
	mustRunPlacementExec(t, fixture.ctx, tx, `
UPDATE runs
   SET deployment_definition_id = $2, entrypoint_kind = 'actor',
       entrypoint_declared_id = 'test-actor', actor_id = $3,
       cause_kind = 'actor_start', actor_start_input_sequence = 1,
       actor_start_input_high_watermark = 1, payload = NULL
 WHERE id = $1`, fixture.runID, actorDefinitionID, actorID)
	mustRunPlacementExec(t, fixture.ctx, tx, `
UPDATE run_attempts
   SET entrypoint_kind = 'actor', actor_start_input_sequence = 1,
       entrypoint_entered_at = transaction_timestamp()
 WHERE run_id = $1 AND number = 1`, fixture.runID)
	mustRunPlacementExec(t, fixture.ctx, tx, `
UPDATE workspaces SET owner_run_id = NULL, owner_actor_id = $2 WHERE id = $1`, fixture.workspaceID, actorID)
	mustRunPlacementExec(t, fixture.ctx, tx, `INSERT INTO cas_objects (org_id, digest, size_bytes, media_type) VALUES ($1, $2, 1, $3)`,
		fixture.orgID, privateDigest, workspace.ArtifactMediaType)
	mustRunPlacementExec(t, fixture.ctx, tx, `
INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
VALUES ($1, $2, $3, $4, $5, 'workspace_version', 1, $6)`, privateArtifactID, fixture.orgID,
		fixture.projectID, fixture.environmentID, privateDigest, workspace.ArtifactMediaType)
	mustRunPlacementExec(t, fixture.ctx, tx, `
INSERT INTO workspace_versions (
    id, public_id, org_id, project_id, environment_id, workspace_id, parent_version_id,
    kind, content_digest, state, source_workspace_lease_id, ownership_generation,
    writer_generation, artifact_id, artifact_kind, entry_count, size_bytes
) VALUES ($1, $2, $3, $4, $5, $6, $7, 'user', $8, 'private', $9, 1, 1, $10, 'workspace_version', 1, 1)`,
		privateVersionID, dispatchPublicID(t, publicid.WorkspaceVersion), fixture.orgID, fixture.projectID,
		fixture.environmentID, fixture.workspaceID, baseVersionID, privateDigest, sourceWorkspaceLeaseID, privateArtifactID)
	mustRunPlacementExec(t, fixture.ctx, tx, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind, due_at, condition_state,
    condition_result, condition_terminal_at, suspension_state, expected_run_state_version,
    attempt_number, prior_run_lease_id, resume_attach_id
) VALUES ($1, $2, $3, $4, 'timer', now() - interval '1 second', 'completed', '{}'::jsonb,
          now(), 'resume_pending', 3, 1, $5, $6)`, waitID, fixture.environmentID, fixture.runID,
		fixture.workspaceID, grant.Lease.ID, uuid.Must(uuid.NewV7()))
	mustRunPlacementExec(t, fixture.ctx, tx, `
INSERT INTO run_checkpoints (
    id, kind, run_id, attempt_number, run_wait_id, source_run_lease_id,
    source_workspace_lease_id, workspace_id, base_workspace_version_id,
    private_workspace_version_id, actor_speculative_input_sequence,
    state, restore_manifest, ready_request_fingerprint, ready_at
) VALUES ($1, 'suspend', $2, 1, $3, $4, $5, $6, $7, $8, 1,
          'ready', '{"kind":"suspend"}'::jsonb, 'test-ready', now())`,
		checkpointID, fixture.runID, waitID, grant.Lease.ID, sourceWorkspaceLeaseID,
		fixture.workspaceID, baseVersionID, privateVersionID)
	mustRunPlacementExec(t, fixture.ctx, tx, `UPDATE run_waits SET suspend_checkpoint_id = $2, resume_request_version = 1 WHERE id = $1`, waitID, checkpointID)
	mustRunPlacementExec(t, fixture.ctx, tx, `UPDATE runs SET current_run_lease_id = NULL, state_version = 3 WHERE id = $1`, fixture.runID)
	mustRunPlacementExec(t, fixture.ctx, tx, `
UPDATE run_leases SET state = 'checkpointed', claimed_at = assigned_at, started_at = assigned_at,
       checkpointed_at = now(), terminal_at = now(), terminal_reason_code = 'checkpointed' WHERE id = $1`, grant.Lease.ID)
	mustRunPlacementExec(t, fixture.ctx, tx, `UPDATE workspace_leases SET state = 'released', released_at = now(), terminal_at = now() WHERE id = $1`, sourceWorkspaceLeaseID)
	mustRunPlacementExec(t, fixture.ctx, tx, `UPDATE workspace_mounts SET state = 'unmounted', unmounted_at = now(), terminal_at = now(), terminal_reason_code = 'checkpointed' WHERE id = $1`, mount.WorkspaceMountID)
	mustRunPlacementExec(t, fixture.ctx, tx, `
UPDATE runtime_instances SET desired_state = 'closed', desired_version = desired_version + 1,
       observed_state = 'closed', observed_desired_version = desired_version + 1,
       observed_version = observed_version + 1, closing_at = now(), closed_at = now(),
       terminal_at = now(), terminal_reason_code = 'checkpointed', reclaimed_at = now(),
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL WHERE id = $1`, reserved.RuntimeInstanceID)
	mustRunPlacementExec(t, fixture.ctx, tx, `
UPDATE worker_network_slots SET state = 'available', runtime_instance_id = NULL,
       host_interface_name = NULL, guest_address = NULL, gateway_address = NULL,
       subnet = NULL, tap_name = NULL, netns_name = NULL, guest_mac = NULL
 WHERE runtime_instance_id = $1`, reserved.RuntimeInstanceID)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	return actorID, waitID, checkpointID
}

func markRunPlacementRuntimeReadyQuery(t *testing.T, fixture runPlacementFixture, runtimeID pgtype.UUID) error {
	t.Helper()
	var desiredVersion, observedVersion, workerEpoch, slotGeneration int64
	var workerID, slotID, runtimeSubstrateID pgtype.UUID
	err := fixture.pool.QueryRow(fixture.ctx, `
INSERT INTO runtime_substrates (
    org_id, project_id, environment_id, deployment_definition_id, artifact_id,
    substrate_digest, substrate_format, builder_abi, layout_abi,
    substrate_size_bytes, source, created_by_worker_instance_id
)
SELECT runtime_instances.org_id,
       runtime_instances.project_id,
       runtime_instances.environment_id,
       runtime_instances.deployment_definition_id,
       deployment_definitions.artifact_id,
       'sha256:test-runtime-substrate', 'squashfs', 'builder-v0', 'layout-v0',
       1, '{}'::jsonb, runtime_instances.worker_instance_id
  FROM runtime_instances
  JOIN deployment_definitions
    ON deployment_definitions.environment_id = runtime_instances.environment_id
   AND deployment_definitions.id = runtime_instances.deployment_definition_id
 WHERE runtime_instances.id = $1
ON CONFLICT (
    org_id, project_id, environment_id, deployment_definition_id,
    substrate_format, builder_abi, layout_abi
) DO UPDATE SET last_referenced_at = transaction_timestamp()
RETURNING id`, runtimeID).Scan(&runtimeSubstrateID)
	if err != nil {
		t.Fatal(err)
	}
	err = fixture.pool.QueryRow(fixture.ctx, `
SELECT runtime_instances.desired_version,
       runtime_instances.observed_version,
       runtime_instances.worker_instance_id,
       runtime_instances.worker_epoch,
       worker_network_slots.id,
       worker_network_slots.generation
  FROM runtime_instances
  JOIN worker_network_slots ON worker_network_slots.runtime_instance_id = runtime_instances.id
 WHERE runtime_instances.id = $1`, runtimeID).Scan(
		&desiredVersion, &observedVersion, &workerID, &workerEpoch, &slotID, &slotGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}
	guestAddress := netip.MustParseAddr("10.0.0.2")
	gatewayAddress := netip.MustParseAddr("10.0.0.1")
	subnet := netip.MustParsePrefix("10.0.0.0/24")
	_, err = db.New(fixture.pool).MarkRuntimeInstanceReady(fixture.ctx, db.MarkRuntimeInstanceReadyParams{
		RuntimeSubstrateID: runtimeSubstrateID,
		DesiredVersion:     desiredVersion, ID: runtimeID, WorkerInstanceID: workerID,
		WorkerEpoch: workerEpoch, ExpectedObservedVersion: observedVersion,
		HostInterfaceName: pgtype.Text{String: "veth-test", Valid: true},
		GuestAddress:      &guestAddress, GatewayAddress: &gatewayAddress, Subnet: &subnet,
		TapName:       pgtype.Text{String: "tap-test", Valid: true},
		NetnsName:     pgtype.Text{String: "netns-test", Valid: true},
		GuestMac:      net.HardwareAddr{0x02, 0, 0, 0, 0, 1},
		NetworkSlotID: slotID, NetworkSlotGeneration: slotGeneration,
	})
	return err
}

func markRunPlacementMountReady(t *testing.T, fixture runPlacementFixture, mountID pgtype.UUID) {
	t.Helper()
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts SET state = 'mounted', mounted_at = transaction_timestamp() WHERE id = $1`, mountID)
}

func TestPlaceReadyRunRejectsPerVMIncompatibleWorkspace(t *testing.T) {
	fixture := newRunPlacementFixture(t)
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET per_vm_memory_bytes = 536870912
 WHERE id = $1`,
		fixture.workerID,
	)

	_, err := fixture.authority.PlaceReadyRun(
		fixture.ctx,
		fixture.candidate(),
		pgvalue.Timestamptz(time.Now().Add(-time.Minute)),
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
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET certified_memory_bytes = 4294967296,
       certified_scratch_bytes = 68719476736,
       per_vm_scratch_bytes = 68719476736
 WHERE id = $1`,
		fixture.workerID,
	)
	mustRunPlacementExec(t, fixture.ctx, fixture.pool, `
INSERT INTO deployment_build_leases (
    id, org_id, project_id, environment_id, deployment_id, build_region_id,
    lease_sequence, worker_group_id, worker_instance_id, worker_epoch,
    worker_protocol_version, requested_cpu_millis, requested_memory_bytes,
    requested_workload_disk_bytes, requested_scratch_bytes,
    requested_build_executors, build_snapshot, start_deadline_at, expires_at
) VALUES (
    $1, $2, $3, $4, $5, 'us-east-1', 1, $6, $7, 1,
    'helmr.worker.v0', 3000, 4294967296, 0, 34359738368,
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
		pgvalue.Timestamptz(time.Now().Add(-time.Minute)),
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
	t.Helper()
	ctx := context.Background()
	pool := newDispatchIntegrationDB(t, ctx)
	fixture := runPlacementFixture{
		ctx:           ctx,
		pool:          pool,
		orgID:         uuid.Must(uuid.NewV7()),
		projectID:     uuid.Must(uuid.NewV7()),
		environmentID: uuid.Must(uuid.NewV7()),
		runID:         uuid.Must(uuid.NewV7()),
		workspaceID:   uuid.Must(uuid.NewV7()),
		workerID:      uuid.Must(uuid.NewV7()),
		groupID:       "run-placement-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
	}
	key := bytes.Repeat([]byte{7}, workspace.FencingKeySize)
	var fixedKey [workspace.FencingKeySize]byte
	copy(fixedKey[:], key)
	fingerprint := workspace.FencingKeyFingerprintForKey(fixedKey).String()
	var err error
	fixture.fencingKeys, err = workspace.NewFencingKeys(
		fingerprint,
		map[string][]byte{fingerprint: key},
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.authority, err = NewRunAuthority(
		pool,
		fixture.fencingKeys,
		RunPlacementPolicy{
			PreparationLimit: 8,
			ReservationTTL:   5 * time.Minute,
			StartDeadline:    time.Minute,
			LeaseTTL:         5 * time.Minute,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	deploymentID := uuid.Must(uuid.NewV7())
	fixture.deploymentID = deploymentID
	taskDefinitionID := uuid.Must(uuid.NewV7())
	workspaceDefinitionID := uuid.Must(uuid.NewV7())
	versionID := uuid.Must(uuid.NewV7())
	sourceID := uuid.Must(uuid.NewV7())
	programID := uuid.Must(uuid.NewV7())
	imageID := uuid.Must(uuid.NewV7())
	runtimeIdentityID := "run-runtime-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	sourceDigest := "sha256:" + strings.Repeat("1", 64)
	programDigest := "sha256:" + strings.Repeat("2", 64)
	imageDigest := "sha256:" + strings.Repeat("4", 64)
	programReceipt := dbtest.ProgramReceipt(dbtest.ProgramReceiptAuthority{
		Architecture:            "x86_64",
		ProgramArtifactID:       programID,
		ProgramDigest:           programDigest,
		ProgramSizeBytes:        1,
		RuntimeDigest:           "sha256:" + strings.Repeat("01", 32),
		SourceArtifactID:        sourceID,
		SourceDigest:            sourceDigest,
		SourceSizeBytes:         1,
		StandardToolchainDigest: "sha256:" + strings.Repeat("02", 32),
	})

	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO regions (id, provider, provider_region, display_name)
VALUES ('us-east-1', 'aws', 'us-east-1', 'US East')`)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO organizations (id, public_id, name, slug)
VALUES ($1, $2, 'Org', $3)`,
		fixture.orgID,
		dispatchPublicID(t, publicid.Organization),
		"org-"+fixture.orgID.String(),
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO projects (id, public_id, org_id, default_region_id, slug, name)
VALUES ($1, $2, $3, 'us-east-1', $4, 'Project')`,
		fixture.projectID,
		dispatchPublicID(t, publicid.Project),
		fixture.orgID,
		"project-"+fixture.projectID.String(),
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO environments (id, public_id, org_id, project_id, slug, name, color_hex)
VALUES ($1, $2, $3, $4, $5, 'Environment', '#3366ff')`,
		fixture.environmentID,
		dispatchPublicID(t, publicid.Environment),
		fixture.orgID,
		fixture.projectID,
		"environment-"+fixture.environmentID.String(),
	)
	mustRunPlacementExec(t, ctx, pool, `
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
	mustRunPlacementExec(t, ctx, pool, `
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
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO deployments (
    id, public_id, org_id, project_id, environment_id, build_region_id,
    build_architecture, build_runtime_digest, build_standard_toolchain_digest,
    build_manager_name, build_manager_version, build_manager_digest,
    build_contract_version, version, content_hash, deployment_source_artifact_id,
    program_artifact_id, program_runtime_digest, program_architecture,
    program_receipt, queue_config, status
) VALUES (
    $1, $2, $3, $4, $5, 'us-east-1', 'x86_64',
    decode(repeat('01', 32), 'hex'), decode(repeat('02', 32), 'hex'),
    'bun', '1.2.3', decode(repeat('22', 32), 'hex'),
    'helmr.program-build.v0', 'v1', $6, $7, $8,
    decode(repeat('01', 32), 'hex'), 'x86_64', $9::jsonb, '{}'::jsonb, 'deployed'
)`,
		deploymentID,
		dispatchPublicID(t, publicid.Deployment),
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		sourceDigest,
		sourceID,
		programID,
		programReceipt,
	)
	workspaceManifest := fmt.Sprintf(
		`{"image":{"artifactDigest":%q,"mediaType":"application/octet-stream"},"resources":{"milliCpu":1000,"memoryMiB":1024,"diskMiB":2048},"network":{"internet":true,"denyCidrs":[]},"architecture":"x86_64"}`,
		imageDigest,
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO deployment_definitions (
    id, environment_id, deployment_id, kind, declared_id, manifest_version,
    manifest, manifest_digest, workspace_architecture, artifact_id
) VALUES
    ($1, $3, $4, 'task', 'test-task', 0, '{}'::jsonb,
     decode(repeat('03', 32), 'hex'), NULL, NULL),
    ($2, $3, $4, 'workspace', 'test-workspace', 0, $5::jsonb,
     decode(repeat('04', 32), 'hex'), 'x86_64', $6)`,
		taskDefinitionID,
		workspaceDefinitionID,
		fixture.environmentID,
		deploymentID,
		workspaceManifest,
		imageID,
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO worker_groups (
    id, region_id, name, enrollment_policy_fingerprint,
    allowed_attestation_fingerprints, allows_run, allows_build
) VALUES ($1, 'us-east-1', $1, 'test-policy', ARRAY['test-attestation'], true, false)`,
		fixture.groupID,
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO runtime_identities (
    id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest,
    rootfs_digest, cni_profile
) VALUES ($1, 'x86_64', 'helmr.runtime.v0', 'kernel', 'initramfs', 'rootfs', 'default')`,
		runtimeIdentityID,
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO worker_instances (
    id, resource_id, worker_group_id, attestation_fingerprint, state,
    current_epoch, current_service_id, protocol_version, supervisor_version,
    supports_run, runtime_identity_id,
    substrate_format, substrate_builder_abi, substrate_layout_abi,
    certified_cpu_millis, certified_memory_bytes, certified_workload_disk_bytes,
    certified_scratch_bytes, per_vm_cpu_millis, per_vm_memory_bytes,
    per_vm_workload_disk_bytes, per_vm_scratch_bytes, max_vm_slots,
    max_run_consumers, max_runtime_starts, certification_profile,
    certification_fingerprint, epoch_started_at, certified_at, activated_at
) VALUES (
    $1, $2, $3, 'test-attestation', 'active', 1, $4, 'helmr.worker.v0',
    'test-worker', true, $5, 'squashfs', 'builder-v0', 'layout-v0',
    8000, 8589934592, 17179869184, 17179869184,
    1000, 1073741824, 2147483648, 2147483648,
    8, 8, 8, 'run-v0', 'test-cert', now(), now(), now()
)`,
		fixture.workerID,
		fixture.workerID.String(),
		fixture.groupID,
		uuid.Must(uuid.NewV7()),
		runtimeIdentityID,
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO worker_observations (
    worker_instance_id, worker_epoch, cpu_pressure_bps, memory_pressure_bps,
    workload_disk_pressure_bps, scratch_pressure_bps, build_cache_pressure_bps,
    artifact_cache_pressure_bps, checkpoint_pressure_bps, leaked_slot_count,
    run_queue_depth, build_queue_depth, runtime_start_queue_depth, observed_at
) VALUES ($1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, now())`,
		fixture.workerID,
	)
	mustRunPlacementExec(t, ctx, pool, `
INSERT INTO worker_network_slots (
    id, worker_group_id, worker_instance_id, worker_epoch, slot_name, generation
) VALUES ($1, $2, $3, 1, 'slot-1', 1)`,
		uuid.Must(uuid.NewV7()),
		fixture.groupID,
		fixture.workerID,
	)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	mustRunPlacementExec(t, ctx, tx, `
INSERT INTO workspaces (
    id, public_id, org_id, project_id, environment_id, region_id,
    declaration_kind, workspace_declared_id, deployment_definition_id,
    owner_run_id, ownership_generation, writer_generation, head_version_id
) VALUES (
    $1, $2, $3, $4, $5, 'us-east-1', 'workspace', 'test-workspace',
    $6, $7, 1, 0, $8
)`,
		fixture.workspaceID,
		dispatchPublicID(t, publicid.Workspace),
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		workspaceDefinitionID,
		fixture.runID,
		versionID,
	)
	mustRunPlacementExec(t, ctx, tx, `
INSERT INTO workspace_versions (
    id, public_id, org_id, project_id, environment_id, workspace_id,
    kind, content_digest, state, ownership_generation, writer_generation, published_at
) VALUES (
    $1, $2, $3, $4, $5, $6, 'system',
    'sha256:d2ce8eece19cb4f6db14e37f6d986da7eec7f654f3b91c5c706e9d74e7d2bc96',
    'committed', 0, 0, now()
)`,
		versionID,
		dispatchPublicID(t, publicid.WorkspaceVersion),
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		fixture.workspaceID,
	)
	mustRunPlacementExec(t, ctx, tx, `
INSERT INTO runs (
    id, public_id, org_id, project_id, environment_id, deployment_id,
    deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
    cause_kind, workspace_id, base_workspace_version_id, payload, queue_name,
    queue_origin_at, queue_score_at, max_active_duration_ms, retry_policy,
    trace_id, root_span_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, 'task', 'test-task', 'api', $8, $9,
    '{}'::jsonb, 'default', now(), now(), 300000, '{"enabled":false}'::jsonb,
    '11111111111111111111111111111111', '2222222222222222'
)`,
		fixture.runID,
		dispatchPublicID(t, publicid.Run),
		fixture.orgID,
		fixture.projectID,
		fixture.environmentID,
		deploymentID,
		taskDefinitionID,
		fixture.workspaceID,
		versionID,
	)
	mustRunPlacementExec(t, ctx, tx, `
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

func mustRunPlacementExec(
	t *testing.T,
	ctx context.Context,
	execer interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	},
	sql string,
	args ...any,
) {
	t.Helper()
	if _, err := execer.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}
