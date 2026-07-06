package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestEnsureWorkspaceMountRequestedBumpsExistingPriority(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	first, err := queries.EnsureWorkspaceMountRequested(ctx, db.EnsureWorkspaceMountRequestedParams{
		ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:           pgvalue.UUID(ids.orgID),
		ProjectID:       pgvalue.UUID(ids.projectID),
		EnvironmentID:   pgvalue.UUID(ids.environmentID),
		WorkspaceID:     pgvalue.UUID(ids.workspaceID),
		RequestPriority: 3,
		Request:         []byte(`{"source":"priority-test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Priority != 3 {
		t.Fatalf("first priority = %d, want 3", first.Priority)
	}

	raised, err := queries.EnsureWorkspaceMountRequested(ctx, db.EnsureWorkspaceMountRequestedParams{
		ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:           pgvalue.UUID(ids.orgID),
		ProjectID:       pgvalue.UUID(ids.projectID),
		EnvironmentID:   pgvalue.UUID(ids.environmentID),
		WorkspaceID:     pgvalue.UUID(ids.workspaceID),
		RequestPriority: 9,
		Request:         []byte(`{"source":"priority-test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if raised.ID != first.ID || raised.Priority != 9 {
		t.Fatalf("raised mount = id %s priority %d, want id %s priority 9", pgvalue.UUIDString(raised.ID), raised.Priority, pgvalue.UUIDString(first.ID))
	}

	lowered, err := queries.EnsureWorkspaceMountRequested(ctx, db.EnsureWorkspaceMountRequestedParams{
		ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:           pgvalue.UUID(ids.orgID),
		ProjectID:       pgvalue.UUID(ids.projectID),
		EnvironmentID:   pgvalue.UUID(ids.environmentID),
		WorkspaceID:     pgvalue.UUID(ids.workspaceID),
		RequestPriority: 1,
		Request:         []byte(`{"source":"priority-test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if lowered.ID != first.ID || lowered.Priority != 9 {
		t.Fatalf("lowered mount = id %s priority %d, want id %s priority 9", pgvalue.UUIDString(lowered.ID), lowered.Priority, pgvalue.UUIDString(first.ID))
	}
}

func TestClaimWorkspaceMountAllowsColdClaimWithoutReadyRuntime(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, _, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_mounts
		   SET state = 'unmounted',
		       unmounted_at = now(),
		       updated_at = now()
		 WHERE org_id = $1
		   AND workspace_id = $2
	`, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET state = 'closed',
		       closed_at = now(),
		       updated_at = now()
		 WHERE org_id = $1
		   AND workspace_mount_id IN (
		       SELECT id
		         FROM workspace_mounts
		        WHERE org_id = $1
		          AND workspace_id = $2
		   )
	`, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	requested, err := queries.EnsureWorkspaceMountRequested(ctx, db.EnsureWorkspaceMountRequestedParams{
		ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:           pgvalue.UUID(ids.orgID),
		ProjectID:       pgvalue.UUID(ids.projectID),
		EnvironmentID:   pgvalue.UUID(ids.environmentID),
		WorkspaceID:     pgvalue.UUID(ids.workspaceID),
		RequestPriority: 1,
		Request:         []byte(`{"source":"cold-claim-test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var runtimeID string
	if err := pool.QueryRow(ctx, `
		SELECT runtime_id
		  FROM worker_instances
		 WHERE id = $1
	`, workerID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}

	claimed, err := queries.ClaimWorkspaceMount(ctx, db.ClaimWorkspaceMountParams{
		WorkerInstanceID:            pgvalue.UUID(workerID),
		WorkerGroupID:               dbtest.DefaultWorkerGroupID,
		RuntimeID:                   runtimeID,
		RootfsDigest:                "sha256:rootfs",
		RuntimeABI:                  "test",
		GuestdAbi:                   "guestd-test",
		AdapterAbi:                  "adapter-test",
		NetworkPolicy:               []byte(`{"internet":true}`),
		RuntimeInstanceID:           pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeInstanceToken:        "cold-runtime-instance-token",
		GuestdChannelTokenHash:      "guestd-token-hash",
		GuestdChannelTokenExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != requested.ID {
		t.Fatalf("claimed mount = %s, want requested %s", pgvalue.UUIDString(claimed.ID), pgvalue.UUIDString(requested.ID))
	}
	if !claimed.RuntimeInstanceID.Valid || claimed.RuntimeInstanceToken != "cold-runtime-instance-token" {
		t.Fatalf("cold claim runtime = %s token %q, want bound runtime with token", pgvalue.UUIDString(claimed.RuntimeInstanceID), claimed.RuntimeInstanceToken)
	}
	if claimed.RequestedCpuMillis != 1000 || claimed.RequestedMemoryMib != 1024 || claimed.RequestedDiskMib != 1024 || claimed.RequestedExecutionSlots != 1 {
		t.Fatalf("cold claim resources = cpu %d memory %d disk %d slots %d, want sandbox floor 1000/1024/1024/1", claimed.RequestedCpuMillis, claimed.RequestedMemoryMib, claimed.RequestedDiskMib, claimed.RequestedExecutionSlots)
	}
	var runtimeState db.RuntimeInstanceState
	if err := pool.QueryRow(ctx, `
		SELECT state
		  FROM runtime_instances
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(claimed.RuntimeInstanceID)).Scan(&runtimeState); err != nil {
		t.Fatal(err)
	}
	if runtimeState != db.RuntimeInstanceStateBinding {
		t.Fatalf("cold runtime state = %s, want binding", runtimeState)
	}
}

func TestClaimWorkspaceMountRejectsDrainingWorkerGroup(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, _, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_mounts
		   SET state = 'unmounted',
		       unmounted_at = now(),
		       updated_at = now()
		 WHERE org_id = $1
		   AND workspace_id = $2
	`, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET state = 'closed',
		       closed_at = now(),
		       updated_at = now()
		 WHERE org_id = $1
		   AND workspace_mount_id IN (
		       SELECT id
		         FROM workspace_mounts
		        WHERE org_id = $1
		          AND workspace_id = $2
		   )
	`, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.EnsureWorkspaceMountRequested(ctx, db.EnsureWorkspaceMountRequestedParams{
		ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:           pgvalue.UUID(ids.orgID),
		ProjectID:       pgvalue.UUID(ids.projectID),
		EnvironmentID:   pgvalue.UUID(ids.environmentID),
		WorkspaceID:     pgvalue.UUID(ids.workspaceID),
		RequestPriority: 1,
		Request:         []byte(`{"source":"draining-claim-test"}`),
	}); err != nil {
		t.Fatal(err)
	}
	var runtimeID string
	if err := pool.QueryRow(ctx, `
		SELECT runtime_id
		  FROM worker_instances
		 WHERE id = $1
	`, workerID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	markDefaultWorkerGroupDrainingWithStaleHealth(t, ctx, pool, ids)

	_, err := queries.ClaimWorkspaceMount(ctx, db.ClaimWorkspaceMountParams{
		WorkerInstanceID:            pgvalue.UUID(workerID),
		WorkerGroupID:               dbtest.DefaultWorkerGroupID,
		RuntimeID:                   runtimeID,
		RootfsDigest:                "sha256:rootfs",
		RuntimeABI:                  "test",
		GuestdAbi:                   "guestd-test",
		AdapterAbi:                  "adapter-test",
		NetworkPolicy:               []byte(`{"internet":true}`),
		RuntimeInstanceID:           pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeInstanceToken:        "draining-cold-runtime-instance-token",
		GuestdChannelTokenHash:      "draining-guestd-token-hash",
		GuestdChannelTokenExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim on draining worker group err = %v, want pgx.ErrNoRows", err)
	}
}

func TestStopWorkspaceMountReplaysAfterSuccessfulStop(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, _, workerID := seedRunningSessionLease(t, ctx, pool, ids)

	var mountID uuid.UUID
	var runtimeInstanceID uuid.UUID
	var runtimeInstanceToken string
	if err := pool.QueryRow(ctx, `
		SELECT workspace_mounts.id,
		       runtime_instances.id,
		       runtime_instances.instance_token
		  FROM runs
		  JOIN workspace_mounts
		    ON workspace_mounts.org_id = runs.org_id
		   AND workspace_mounts.id = runs.workspace_mount_id
		  JOIN runtime_instances
		    ON runtime_instances.org_id = workspace_mounts.org_id
		   AND runtime_instances.id = workspace_mounts.runtime_instance_id
		 WHERE runs.org_id = $1
		   AND runs.id = $2
	`, ids.orgID, ids.runID).Scan(&mountID, &runtimeInstanceID, &runtimeInstanceToken); err != nil {
		t.Fatal(err)
	}

	params := db.StopWorkspaceMountParams{
		OrgID:                pgvalue.UUID(ids.orgID),
		ID:                   pgvalue.UUID(mountID),
		WorkerInstanceID:     pgvalue.UUID(workerID),
		RuntimeInstanceToken: runtimeInstanceToken,
	}
	first, err := queries.StopWorkspaceMount(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := queries.StopWorkspaceMount(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != replayed.ID || replayed.State != db.WorkspaceMountStateUnmounted || replayed.RuntimeInstanceID.Valid {
		t.Fatalf("replayed stop = id %s state %s runtime %s, want same unmounted mount with detached runtime", pgvalue.UUIDString(replayed.ID), replayed.State, pgvalue.UUIDString(replayed.RuntimeInstanceID))
	}

	var runtimeState db.RuntimeInstanceState
	if err := pool.QueryRow(ctx, `
		SELECT state
		  FROM runtime_instances
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, runtimeInstanceID).Scan(&runtimeState); err != nil {
		t.Fatal(err)
	}
	if runtimeState != db.RuntimeInstanceStateClosed {
		t.Fatalf("runtime state = %s, want closed", runtimeState)
	}
}

func TestRequestWorkspaceMountStopUsesMountPlacementSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, _, _ = seedRunningSessionLease(t, ctx, pool, ids)
	alternateWorkerGroupID := "alternate-stop-route"
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_groups (id, region_id, name, description)
		VALUES ($1, $2, 'alternate-stop-route', 'alternate route for stop test')
	`, alternateWorkerGroupID, dbtest.DefaultRegionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspaces
		   SET worker_group_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, alternateWorkerGroupID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}

	stopped, err := queries.RequestWorkspaceMountStop(ctx, db.RequestWorkspaceMountStopParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.WorkerGroupID != dbtest.DefaultWorkerGroupID {
		t.Fatalf("stopped mount worker group = %q, want persisted mount worker group %q", stopped.WorkerGroupID, dbtest.DefaultWorkerGroupID)
	}
	if stopped.State != db.WorkspaceMountStateUnmounting {
		t.Fatalf("stopped mount state = %s, want unmounting", stopped.State)
	}
	var desiredState db.WorkspaceDesiredState
	if err := pool.QueryRow(ctx, `
		SELECT desired_state
		  FROM workspaces
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID).Scan(&desiredState); err != nil {
		t.Fatal(err)
	}
	if desiredState != db.WorkspaceDesiredStateStopped {
		t.Fatalf("workspace desired_state = %s, want stopped", desiredState)
	}
}

func TestMarkWorkspaceMountMountedRejectsWrongRuntimeTokenWithoutMountSideEffect(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, _, workerID := seedRunningSessionLease(t, ctx, pool, ids)

	var mountID uuid.UUID
	var runtimeInstanceID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT workspace_mounts.id,
		       runtime_instances.id
		  FROM runs
		  JOIN workspace_mounts
		    ON workspace_mounts.org_id = runs.org_id
		   AND workspace_mounts.id = runs.workspace_mount_id
		  JOIN runtime_instances
		    ON runtime_instances.org_id = workspace_mounts.org_id
		   AND runtime_instances.id = workspace_mounts.runtime_instance_id
		 WHERE runs.org_id = $1
		   AND runs.id = $2
	`, ids.orgID, ids.runID).Scan(&mountID, &runtimeInstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_mounts
		   SET state = 'mounting'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, mountID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET state = 'binding'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, runtimeInstanceID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.MarkWorkspaceMountMounted(ctx, db.MarkWorkspaceMountMountedParams{
		GuestdChannelTokenExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		OrgID:                       pgvalue.UUID(ids.orgID),
		ID:                          pgvalue.UUID(mountID),
		WorkerInstanceID:            pgvalue.UUID(workerID),
		RuntimeInstanceToken:        "wrong-runtime-token",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mark mounted with wrong token error = %v, want pgx.ErrNoRows", err)
	}
	var mountState db.WorkspaceMountState
	if err := pool.QueryRow(ctx, `
		SELECT state
		  FROM workspace_mounts
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, mountID).Scan(&mountState); err != nil {
		t.Fatal(err)
	}
	if mountState != db.WorkspaceMountStateMounting {
		t.Fatalf("mount state after wrong token = %s, want mounting", mountState)
	}
}

func TestMarkStaleWorkspaceMountsLostMarksReadyRuntimeLost(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, _ := seedRunningSessionLease(t, ctx, pool, ids)

	var mountID uuid.UUID
	var runtimeInstanceID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT workspace_mounts.id,
		       runtime_instances.id
		  FROM runs
		  JOIN workspace_mounts
		    ON workspace_mounts.org_id = runs.org_id
		   AND workspace_mounts.id = runs.workspace_mount_id
		  JOIN runtime_instances
		    ON runtime_instances.org_id = workspace_mounts.org_id
		   AND runtime_instances.id = workspace_mounts.runtime_instance_id
		 WHERE runs.org_id = $1
		   AND runs.id = $2
	`, ids.orgID, ids.runID).Scan(&mountID, &runtimeInstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_leases
		   SET status = 'released',
		       released_at = now(),
		       lease_expires_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, runLeaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET state = 'ready',
		       expires_at = now() + interval '1 hour',
		       updated_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, runtimeInstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_mounts
		   SET last_heartbeat_at = now() - interval '1 hour',
		       updated_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, mountID); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.MarkStaleWorkspaceMountsLost(ctx, pgvalue.Timestamptz(time.Now())); err != nil {
		t.Fatal(err)
	}
	var runtimeState db.RuntimeInstanceState
	var ownerRunID pgtype.UUID
	var ownerWorkspaceID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT state, owner_run_id, owner_workspace_id
		  FROM runtime_instances
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, runtimeInstanceID).Scan(&runtimeState, &ownerRunID, &ownerWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if runtimeState != db.RuntimeInstanceStateLost || ownerRunID.Valid || ownerWorkspaceID.Valid {
		t.Fatalf("runtime after stale mount = state %s owner_run %s owner_workspace %s, want lost with owners cleared", runtimeState, pgvalue.UUIDString(ownerRunID), pgvalue.UUIDString(ownerWorkspaceID))
	}
}
