package dispatch

import (
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkspaceMountClaimIsExclusive(t *testing.T) {
	t.Run("sequential delivery", func(t *testing.T) {
		fixture, mountID := prepareClaimableRunMount(t)
		queries := db.New(fixture.pool)
		first, err := queries.ClaimWorkspaceMount(
			fixture.ctx,
			claimWorkspaceMountParams(fixture, "first"),
		)
		if err != nil {
			t.Fatal(err)
		}
		if first.ID != mountID {
			t.Fatalf("claimed mount = %s, want %s", pgvalue.UUIDString(first.ID), pgvalue.UUIDString(mountID))
		}
		if _, err := queries.ClaimWorkspaceMount(
			fixture.ctx,
			claimWorkspaceMountParams(fixture, "second"),
		); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("second claim error = %v, want pgx.ErrNoRows", err)
		}
	})

	t.Run("concurrent delivery", func(t *testing.T) {
		fixture, mountID := prepareClaimableRunMount(t)
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for _, token := range []string{"first", "second"} {
			token := token
			wg.Go(func() {
				<-start
				row, err := db.New(fixture.pool).ClaimWorkspaceMount(
					fixture.ctx,
					claimWorkspaceMountParams(fixture, token),
				)
				if err == nil && row.ID != mountID {
					err = errors.New("claimed an unexpected mount")
				}
				results <- err
			})
		}
		close(start)
		wg.Wait()
		close(results)
		claimed := 0
		rejected := 0
		for err := range results {
			switch {
			case err == nil:
				claimed++
			case errors.Is(err, pgx.ErrNoRows):
				rejected++
			default:
				t.Fatal(err)
			}
		}
		if claimed != 1 || rejected != 1 {
			t.Fatalf("claim results = %d claimed, %d rejected; want 1 and 1", claimed, rejected)
		}
	})
}

func TestWorkspaceMountClaimSkipsSpentMountForOtherWorkspace(t *testing.T) {
	fixture, firstMountID := prepareClaimableRunMount(t)
	secondMountID := cloneClaimableRunMount(t, fixture, firstMountID)
	queries := db.New(fixture.pool)
	first, err := queries.ClaimWorkspaceMount(
		fixture.ctx,
		claimWorkspaceMountParams(fixture, "first-workspace"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != firstMountID {
		t.Fatalf("first claim = %s, want %s", pgvalue.UUIDString(first.ID), pgvalue.UUIDString(firstMountID))
	}
	second, err := queries.ClaimWorkspaceMount(
		fixture.ctx,
		claimWorkspaceMountParams(fixture, "second-workspace"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != secondMountID {
		t.Fatalf("second claim = %s, want %s", pgvalue.UUIDString(second.ID), pgvalue.UUIDString(secondMountID))
	}
	var firstHash string
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT guest_channel_token_hash FROM workspace_mounts WHERE id = $1`, firstMountID).Scan(&firstHash); err != nil {
		t.Fatal(err)
	}
	if firstHash != first.GuestChannelTokenHash {
		t.Fatalf("first claim hash changed from %q to %q", first.GuestChannelTokenHash, firstHash)
	}
}

func TestWorkspaceMountClaimCredentialPairIsConsistent(t *testing.T) {
	fixture, mountID := prepareClaimableRunMount(t)
	_, err := fixture.pool.Exec(fixture.ctx, `
UPDATE workspace_mounts
   SET guest_channel_token_expires_at = transaction_timestamp() + interval '5 minutes'
 WHERE id = $1`, mountID)
	if err == nil {
		t.Fatal("expiry without a token hash satisfied the workspace mount constraint")
	}
}

func TestExpiredWorkspaceMountClaimRecoversRunWithFreshAuthority(t *testing.T) {
	fixture, mountID := prepareClaimableRunMount(t)
	queries := db.New(fixture.pool)
	claimed, err := queries.ClaimWorkspaceMount(
		fixture.ctx,
		claimWorkspaceMountParams(fixture, "run"),
	)
	if err != nil {
		t.Fatal(err)
	}
	future := pgvalue.TimestamptzUTCZeroInvalid(time.Now().UTC().Add(10 * time.Minute))
	if _, err := queries.RenewWorkspaceMount(fixture.ctx, db.RenewWorkspaceMountParams{
		GuestChannelTokenExpiresAt: future,
		OrgID:                      claimed.OrgID,
		ID:                         claimed.ID,
		WorkerInstanceID:           claimed.WorkerInstanceID,
		WorkerEpoch:                claimed.WorkerEpoch,
		RuntimeInstanceID:          claimed.RuntimeInstanceID,
	}); err != nil {
		t.Fatal(err)
	}
	if rows, err := queries.LoseExpiredWorkspaceMountClaims(fixture.ctx, 8); err != nil {
		t.Fatal(err)
	} else if len(rows) != 0 {
		t.Fatalf("renewing claim was lost: %+v", rows)
	}

	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts
   SET guest_channel_token_expires_at = transaction_timestamp() - interval '1 second'
 WHERE id = $1`, mountID)
	lost, err := queries.LoseExpiredWorkspaceMountClaims(fixture.ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(lost) != 1 || lost[0].ID != mountID || lost[0].State != "lost" {
		t.Fatalf("lost claims = %+v, want mount %s", lost, pgvalue.UUIDString(mountID))
	}
	var mountState, mountReason, desiredState, desiredReason string
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT workspace_mounts.state, workspace_mounts.terminal_reason_code,
       runtime_instances.desired_state, runtime_instances.desired_reason
  FROM workspace_mounts
  JOIN runtime_instances ON runtime_instances.id = workspace_mounts.runtime_instance_id
 WHERE workspace_mounts.id = $1`, mountID).Scan(
		&mountState,
		&mountReason,
		&desiredState,
		&desiredReason,
	); err != nil {
		t.Fatal(err)
	}
	if mountState != "lost" || mountReason != "workspace_mount_claim_expired" ||
		desiredState != "closed" || desiredReason != "workspace_mount_claim_expired" {
		t.Fatalf("recovery state = %s/%s runtime=%s/%s", mountState, mountReason, desiredState, desiredReason)
	}

	markExpiredClaimRuntimeReclaimed(t, fixture, claimed.RuntimeInstanceID)
	fresh, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.RuntimeInstanceID.Valid || fresh.RuntimeInstanceID == claimed.RuntimeInstanceID {
		t.Fatalf("fresh placement = %+v, expired runtime = %s", fresh, pgvalue.UUIDString(claimed.RuntimeInstanceID))
	}
}

func TestExpiredWorkspaceMountClaimRecoversProcessWithFreshAuthority(t *testing.T) {
	fixture, processID, mountID := prepareClaimableWorkspaceExecMount(t)
	queries := db.New(fixture.pool)
	claimed, err := queries.ClaimWorkspaceMount(
		fixture.ctx,
		claimWorkspaceMountParams(fixture, "process"),
	)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspace_mounts
   SET guest_channel_token_expires_at = transaction_timestamp() - interval '1 second'
 WHERE id = $1`, mountID)
	lost, err := queries.LoseExpiredWorkspaceMountClaims(fixture.ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(lost) != 1 || lost[0].ID != mountID {
		t.Fatalf("lost claims = %+v, want mount %s", lost, pgvalue.UUIDString(mountID))
	}
	markExpiredClaimRuntimeReclaimed(t, fixture, claimed.RuntimeInstanceID)
	fresh, err := fixture.authority.PlaceWorkspaceExec(
		fixture.ctx,
		ReadyWorkspaceExecCandidate{
			OrgID:                pgvalue.UUID(fixture.orgID),
			ProcessID:            pgvalue.UUID(processID),
			ExpectedStateVersion: 1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.RuntimeInstanceID.Valid || fresh.RuntimeInstanceID == claimed.RuntimeInstanceID {
		t.Fatalf("fresh placement = %+v, expired runtime = %s", fresh, pgvalue.UUIDString(claimed.RuntimeInstanceID))
	}
}

func prepareClaimableRunMount(t *testing.T) (runPlacementFixture, pgtype.UUID) {
	t.Helper()
	fixture := newRunPlacementFixture(t)
	reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
	mounting, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	if !mounting.WorkspaceMountID.Valid {
		t.Fatalf("mount placement = %+v", mounting)
	}
	return fixture, mounting.WorkspaceMountID
}

func prepareClaimableWorkspaceExecMount(t *testing.T) (runPlacementFixture, uuid.UUID, pgtype.UUID) {
	t.Helper()
	fixture := newRunPlacementFixture(t)
	claimID := uuid.NewV7()
	processID := uuid.NewV7()
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspaces SET owner_run_id = NULL WHERE id = $1`, fixture.workspaceID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash, request_fingerprint, accepted_at
) VALUES (
    $1, $2, 'task.child.invoke', decode(repeat('61', 32), 'hex'),
    decode(repeat('62', 32), 'hex'), transaction_timestamp()
)`, claimID, fixture.environmentID)
	if _, err := db.New(fixture.pool).CreateWorkspaceExec(
		fixture.ctx,
		db.CreateWorkspaceExecParams{
			ID:                   pgvalue.UUID(processID),
			OrgID:                pgvalue.UUID(fixture.orgID),
			ProjectID:            pgvalue.UUID(fixture.projectID),
			EnvironmentID:        pgvalue.UUID(fixture.environmentID),
			WorkspaceID:          pgvalue.UUID(fixture.workspaceID),
			BaseVersionID:        workspaceHeadVersion(t, fixture),
			RestoreDesiredState:  "active",
			Request:              []byte(`{"command":["echo","ready"]}`),
			Stdin:                []byte{},
			ClaimID:              pgvalue.UUID(claimID),
			CreatedBySubjectType: "user",
			CreatedBySubjectID:   "test-user",
		},
	); err != nil {
		t.Fatal(err)
	}
	candidate := ReadyWorkspaceExecCandidate{
		OrgID:                pgvalue.UUID(fixture.orgID),
		ProcessID:            pgvalue.UUID(processID),
		ExpectedStateVersion: 1,
	}
	reserved, err := fixture.authority.PlaceWorkspaceExec(fixture.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
	mounting, err := fixture.authority.PlaceWorkspaceExec(fixture.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !mounting.WorkspaceMountID.Valid {
		t.Fatalf("mount placement = %+v", mounting)
	}
	return fixture, processID, mounting.WorkspaceMountID
}

func cloneClaimableRunMount(t *testing.T, fixture runPlacementFixture, sourceMountID pgtype.UUID) pgtype.UUID {
	t.Helper()
	workspaceID := uuid.NewV7()
	versionID := uuid.NewV7()
	runID := uuid.NewV7()
	runtimeID := uuid.NewV7()
	mountID := uuid.NewV7()
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(fixture.ctx) }()
	dbtest.MustExec(t, fixture.ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspaces
SELECT (jsonb_populate_record(
    NULL::workspaces,
    to_jsonb(source_workspace) || jsonb_build_object(
        'id', $2::text,
        'owner_run_id', $3::text,
        'head_version_id', $4::text,
        'created_at', transaction_timestamp(),
        'updated_at', transaction_timestamp()
    )
)).*
  FROM workspace_mounts source_mount
  JOIN workspaces source_workspace ON source_workspace.id = source_mount.workspace_id
 WHERE source_mount.id = $1`, sourceMountID, workspaceID, runID, versionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_versions
SELECT (jsonb_populate_record(
    NULL::workspace_versions,
    to_jsonb(source_version) || jsonb_build_object(
        'id', $2::text,
        'workspace_id', $3::text,
        'created_at', transaction_timestamp(),
        'published_at', transaction_timestamp()
    )
)).*
  FROM workspace_mounts source_mount
  JOIN workspace_versions source_version
    ON source_version.id = source_mount.materialized_version_id
 WHERE source_mount.id = $1`, sourceMountID, versionID, workspaceID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO runs
SELECT (jsonb_populate_record(
    NULL::runs,
    to_jsonb(source_run) || jsonb_build_object(
        'id', $2::text,
        'workspace_id', $3::text,
        'base_workspace_version_id', $4::text,
        'current_run_lease_id', NULL,
        'created_at', transaction_timestamp(),
        'updated_at', transaction_timestamp()
    )
)).*
  FROM runtime_instances source_runtime
  JOIN runs source_run ON source_run.id = source_runtime.reserved_run_id
 WHERE source_runtime.id = (
    SELECT runtime_instance_id FROM workspace_mounts WHERE id = $1
 )`, sourceMountID, runID, workspaceID, versionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO run_attempts
SELECT (jsonb_populate_record(
    NULL::run_attempts,
    to_jsonb(source_attempt) || jsonb_build_object(
        'run_id', $2::text,
        'workspace_id', $3::text,
        'base_workspace_version_id', $4::text,
        'created_at', transaction_timestamp()
    )
)).*
  FROM runtime_instances source_runtime
  JOIN run_attempts source_attempt
    ON source_attempt.run_id = source_runtime.reserved_run_id
   AND source_attempt.number = source_runtime.reserved_attempt_number
 WHERE source_runtime.id = (
    SELECT runtime_instance_id FROM workspace_mounts WHERE id = $1
 )`, sourceMountID, runID, workspaceID, versionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO runtime_instances
SELECT (jsonb_populate_record(
    NULL::runtime_instances,
    to_jsonb(source_runtime) || jsonb_build_object(
        'id', $2::text,
        'workspace_id', $3::text,
        'reserved_run_id', $4::text,
        'reserved_workspace_version_id', $5::text,
        'reservation_expires_at', transaction_timestamp() + interval '10 minutes',
        'updated_at', transaction_timestamp()
    )
)).*
  FROM workspace_mounts source_mount
  JOIN runtime_instances source_runtime ON source_runtime.id = source_mount.runtime_instance_id
 WHERE source_mount.id = $1`, sourceMountID, runtimeID, workspaceID, runID, versionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_mounts
SELECT (jsonb_populate_record(
    NULL::workspace_mounts,
    to_jsonb(source_mount) || jsonb_build_object(
        'id', $2::text,
        'workspace_id', $3::text,
        'materialized_version_id', $4::text,
        'runtime_instance_id', $5::text,
        'guest_channel_token_hash', '',
        'guest_channel_token_expires_at', NULL,
        'requested_at', source_mount.requested_at + interval '1 minute',
        'created_at', transaction_timestamp(),
        'updated_at', transaction_timestamp()
    )
)).*
  FROM workspace_mounts source_mount
 WHERE source_mount.id = $1`, sourceMountID, mountID, workspaceID, versionID, runtimeID)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	return pgvalue.UUID(mountID)
}

func markExpiredClaimRuntimeReclaimed(t *testing.T, fixture runPlacementFixture, runtimeID pgtype.UUID) {
	t.Helper()
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runtime_instances
   SET observed_state = 'closed', observed_version = observed_version + 1,
       observed_desired_version = desired_version, observed_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(), terminal_reason_code = desired_reason,
       reclaimed_at = transaction_timestamp(), reclaim_evidence = '{}'::jsonb,
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_process_id = NULL, reserved_workspace_version_id = NULL,
       reservation_expires_at = NULL, updated_at = transaction_timestamp()
 WHERE id = $1`, runtimeID)
}

func claimWorkspaceMountParams(fixture runPlacementFixture, token string) db.ClaimWorkspaceMountParams {
	return db.ClaimWorkspaceMountParams{
		WorkerInstanceID:           pgvalue.UUID(fixture.workerID),
		WorkerEpoch:                1,
		GuestChannelTokenHash:      hex.EncodeToString(dbtest.Hash("workspace-mount-" + token)),
		GuestChannelTokenExpiresAt: pgvalue.TimestamptzUTCZeroInvalid(time.Now().UTC().Add(5 * time.Minute)),
	}
}
