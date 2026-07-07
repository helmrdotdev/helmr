package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/substrate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCreateRuntimeInstanceForDeploymentSandboxFitsExactWorkerCapacity(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, runtimeIdentityID := seedExactCapacityRuntimeWorker(t, ctx, pool)
	setCurrentDeploymentForRuntimeInstanceTest(t, ctx, pool, ids)

	instance, err := queries.CreateRuntimeInstanceForDeploymentSandbox(ctx, db.CreateRuntimeInstanceForDeploymentSandboxParams{
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RootfsDigest:        "sha256:rootfs",
		RuntimeABI:          "test",
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeIdentityID:   runtimeIdentityID,
		RuntimeKeyHash:      "runtime-key-hash",
		RuntimeKey:          []byte(`{"test":"deployment-sandbox"}`),
		InstanceToken:       "runtime-instance-token",
		ExpiresAt:           pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if instance.ReservedCpuMillis != 1000 || instance.ReservedMemoryMib != 1024 || instance.ReservedExecutionSlots != 1 {
		t.Fatalf("reserved resources = cpu:%d mem:%d slots:%d, want exact sandbox floor", instance.ReservedCpuMillis, instance.ReservedMemoryMib, instance.ReservedExecutionSlots)
	}
}

func TestCreateRuntimeInstanceForDeploymentSandboxRejectsWhenWorkerCapacityIsReserved(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, runtimeIdentityID := seedExactCapacityRuntimeWorker(t, ctx, pool)
	setCurrentDeploymentForRuntimeInstanceTest(t, ctx, pool, ids)

	base := db.CreateRuntimeInstanceForDeploymentSandboxParams{
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RootfsDigest:        "sha256:rootfs",
		RuntimeABI:          "test",
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		RuntimeIdentityID:   runtimeIdentityID,
		RuntimeKeyHash:      "runtime-key-hash",
		RuntimeKey:          []byte(`{"test":"deployment-sandbox"}`),
		InstanceToken:       "runtime-instance-token",
		ExpiresAt:           pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	}
	first := base
	first.ID = pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.CreateRuntimeInstanceForDeploymentSandbox(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := base
	second.ID = pgvalue.UUID(uuid.Must(uuid.NewV7()))
	second.InstanceToken = "runtime-instance-token-2"
	if _, err := queries.CreateRuntimeInstanceForDeploymentSandbox(ctx, second); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second create error = %v, want pgx.ErrNoRows", err)
	}
}

func TestCreateExpiredRuntimeStopCommandsCarriesWorkerGroupID(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, runtimeIdentityID := seedExactCapacityRuntimeWorker(t, ctx, pool)
	setCurrentDeploymentForRuntimeInstanceTest(t, ctx, pool, ids)

	instance, err := queries.CreateRuntimeInstanceForDeploymentSandbox(ctx, db.CreateRuntimeInstanceForDeploymentSandboxParams{
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RootfsDigest:        "sha256:rootfs",
		RuntimeABI:          "test",
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeIdentityID:   runtimeIdentityID,
		RuntimeKeyHash:      "runtime-key-hash-route-generation",
		RuntimeKey:          []byte(`{"test":"expired-runtime-route-generation"}`),
		InstanceToken:       "runtime-instance-token-route-generation",
		ExpiresAt:           pgvalue.Timestamptz(time.Now().Add(-time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}

	commands, err := queries.CreateExpiredRuntimeStopCommands(ctx, db.CreateExpiredRuntimeStopCommandsParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ExpiredBefore: pgvalue.Timestamptz(time.Now()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].RuntimeInstanceID != instance.ID {
		t.Fatalf("expired runtime stop commands = %+v, want one for runtime %s", commands, pgvalue.UUIDString(instance.ID))
	}
	if _, err := pool.Exec(ctx, `
		UPDATE worker_groups
		   SET state = 'draining'
		 WHERE id = $1
	`, dbtest.DefaultWorkerGroupID); err != nil {
		t.Fatal(err)
	}
	claimed, err := queries.ClaimWorkerCommands(ctx, db.ClaimWorkerCommandsParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		RowLimit:      10,
		LeaseDuration: pgvalue.Interval(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ID != commands[0].ID {
		t.Fatalf("claimed stop commands = %+v, want command %d", claimed, commands[0].ID)
	}
}

func TestMarkRuntimeInstanceClosedAcceptsLostRuntimeInstance(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, runtimeIdentityID := seedExactCapacityRuntimeWorker(t, ctx, pool)
	setCurrentDeploymentForRuntimeInstanceTest(t, ctx, pool, ids)

	instanceToken := "runtime-instance-token"
	instance, err := queries.CreateRuntimeInstanceForDeploymentSandbox(ctx, db.CreateRuntimeInstanceForDeploymentSandboxParams{
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RootfsDigest:        "sha256:rootfs",
		RuntimeABI:          "test",
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeIdentityID:   runtimeIdentityID,
		RuntimeKeyHash:      "runtime-key-hash",
		RuntimeKey:          []byte(`{"test":"deployment-sandbox"}`),
		InstanceToken:       instanceToken,
		ExpiresAt:           pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	var workspaceVersionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT current_version_id
		  FROM workspaces
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID).Scan(&workspaceVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET state = 'lost',
		       lost_at = now(),
		       expires_at = NULL,
		       owner_workspace_id = $3,
		       owner_workspace_version_id = $4
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(instance.ID), ids.workspaceID, workspaceVersionID); err != nil {
		t.Fatal(err)
	}
	closed, err := queries.MarkRuntimeInstanceClosed(ctx, db.MarkRuntimeInstanceClosedParams{
		ID:               instance.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		InstanceToken:    instanceToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if closed.State != db.RuntimeInstanceStateLost || !closed.LostAt.Valid || closed.ClosedAt.Valid || closed.OwnerWorkspaceID.Valid || closed.OwnerWorkspaceVersionID.Valid {
		t.Fatalf("closed lost runtime = state %s lost_at %+v closed_at %+v owner_workspace %s/%s, want lost state preserved with owner cleared", closed.State, closed.LostAt, closed.ClosedAt, pgvalue.UUIDString(closed.OwnerWorkspaceID), pgvalue.UUIDString(closed.OwnerWorkspaceVersionID))
	}
}

func TestRuntimeInstanceWorkerMutationsContinueOnDrainingWorkerGroup(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, runtimeIdentityID := seedExactCapacityRuntimeWorker(t, ctx, pool)
	setCurrentDeploymentForRuntimeInstanceTest(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		UPDATE worker_instances
		   SET total_milli_cpu = 2000,
		       available_milli_cpu = 2000,
		       total_memory_mib = 2048,
		       available_memory_mib = 2048,
		       total_disk_mib = 2048,
		       available_disk_mib = 2048,
		       total_execution_slots = 2,
		       available_execution_slots = 2
		 WHERE id = $1
	`, workerID); err != nil {
		t.Fatal(err)
	}

	instanceToken := "draining-runtime-instance-token"
	instance, err := queries.CreateRuntimeInstanceForDeploymentSandbox(ctx, db.CreateRuntimeInstanceForDeploymentSandboxParams{
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RootfsDigest:        "sha256:rootfs",
		RuntimeABI:          "test",
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeIdentityID:   runtimeIdentityID,
		RuntimeKeyHash:      "runtime-key-hash",
		RuntimeKey:          []byte(`{"test":"draining-route-runtime"}`),
		InstanceToken:       instanceToken,
		ExpiresAt:           pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	failedToken := "failed-draining-runtime-instance-token"
	failedInstance, err := queries.CreateRuntimeInstanceForDeploymentSandbox(ctx, db.CreateRuntimeInstanceForDeploymentSandboxParams{
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RootfsDigest:        "sha256:rootfs",
		RuntimeABI:          "test",
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeIdentityID:   runtimeIdentityID,
		RuntimeKeyHash:      "runtime-key-hash",
		RuntimeKey:          []byte(`{"test":"failed-draining-route-runtime"}`),
		InstanceToken:       failedToken,
		ExpiresAt:           pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	markDefaultWorkerGroupDrainingWithStaleHealth(t, ctx, pool, ids)

	if _, err := queries.RenewRuntimeInstance(ctx, db.RenewRuntimeInstanceParams{
		ID:               instance.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		InstanceToken:    instanceToken,
		ExpiresAt:        pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatalf("RenewRuntimeInstance on draining route: %v", err)
	}
	if _, err := queries.MarkRuntimeInstanceReady(ctx, db.MarkRuntimeInstanceReadyParams{
		ID:                 instance.ID,
		WorkerInstanceID:   pgvalue.UUID(workerID),
		InstanceToken:      instanceToken,
		ExpiresAt:          pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		RuntimeSubstrateID: pgtype.UUID{},
	}); err != nil {
		t.Fatalf("MarkRuntimeInstanceReady on draining route: %v", err)
	}
	if _, err := queries.MarkRuntimeInstanceClosed(ctx, db.MarkRuntimeInstanceClosedParams{
		ID:               instance.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		InstanceToken:    instanceToken,
	}); err != nil {
		t.Fatalf("MarkRuntimeInstanceClosed on draining route: %v", err)
	}
	if _, err := queries.MarkRuntimeInstanceFailed(ctx, db.MarkRuntimeInstanceFailedParams{
		ID:               failedInstance.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		InstanceToken:    failedToken,
		Error:            []byte(`{"message":"test failure"}`),
	}); err != nil {
		t.Fatalf("MarkRuntimeInstanceFailed on draining route: %v", err)
	}
}

func TestClaimWorkspaceMountAdvancesPreparedRuntimeEpoch(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, runtimeIdentityID := seedExactCapacityRuntimeWorker(t, ctx, pool)
	setCurrentDeploymentForRuntimeInstanceTest(t, ctx, pool, ids)

	instanceToken := "prepared-runtime-instance-token"
	instance, err := queries.CreateRuntimeInstanceForDeploymentSandbox(ctx, db.CreateRuntimeInstanceForDeploymentSandboxParams{
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RootfsDigest:        "sha256:rootfs",
		RuntimeABI:          "test",
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeIdentityID:   runtimeIdentityID,
		RuntimeKeyHash:      "runtime-key-hash",
		RuntimeKey:          []byte(`{"test":"deployment-sandbox"}`),
		InstanceToken:       instanceToken,
		ExpiresAt:           pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkRuntimeInstanceReady(ctx, db.MarkRuntimeInstanceReadyParams{
		ID:                 instance.ID,
		WorkerInstanceID:   pgvalue.UUID(workerID),
		InstanceToken:      instanceToken,
		ExpiresAt:          pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		RuntimeSubstrateID: pgtype.UUID{},
	}); err != nil {
		t.Fatal(err)
	}
	requestedMount, err := requestWorkspaceMountForTest(ctx, queries, db.EnsureWorkspaceMountRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Request:       []byte(`{"source":"prepared-runtime-epoch-test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := queries.ClaimWorkspaceMount(ctx, db.ClaimWorkspaceMountParams{
		RootfsDigest:                "sha256:rootfs",
		RuntimeABI:                  "test",
		GuestdAbi:                   "guestd-test",
		AdapterAbi:                  "adapter-test",
		NetworkPolicy:               []byte(`{"internet":true}`),
		RuntimeInstanceID:           pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeInstanceToken:        "unused-cold-runtime-token",
		WorkerInstanceID:            pgvalue.UUID(workerID),
		WorkerGroupID:               dbtest.DefaultWorkerGroupID,
		GuestdChannelTokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		GuestdChannelTokenHash:      "workspace-mount-channel-token-hash",
		RuntimeID:                   runtimeIdentityID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != requestedMount.ID {
		t.Fatalf("claimed workspace mount id = %v, want %v", claimed.ID, requestedMount.ID)
	}
	var claimedRuntimeEpoch int64
	var ownerWorkspaceID pgtype.UUID
	var ownerWorkspaceVersionID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT runtime_epoch, owner_workspace_id, owner_workspace_version_id
		  FROM runtime_instances
		 WHERE org_id = $1
		   AND id = $2
		   AND workspace_mount_id = $3
	`, ids.orgID, pgvalue.MustUUIDValue(instance.ID), pgvalue.MustUUIDValue(claimed.ID)).Scan(&claimedRuntimeEpoch, &ownerWorkspaceID, &ownerWorkspaceVersionID); err != nil {
		t.Fatal(err)
	}
	if claimedRuntimeEpoch != instance.RuntimeEpoch+1 {
		t.Fatalf("claimed runtime epoch = %d, want %d", claimedRuntimeEpoch, instance.RuntimeEpoch+1)
	}
	if ownerWorkspaceID != requestedMount.WorkspaceID || ownerWorkspaceVersionID != requestedMount.BaseVersionID {
		t.Fatalf("runtime owner workspace = %s/%s, want %s/%s", pgvalue.UUIDString(ownerWorkspaceID), pgvalue.UUIDString(ownerWorkspaceVersionID), pgvalue.UUIDString(requestedMount.WorkspaceID), pgvalue.UUIDString(requestedMount.BaseVersionID))
	}
}

func TestClaimWorkspaceMountDefersColdClaimWhenPreparingRuntimeExists(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, runtimeIdentityID := seedExactCapacityRuntimeWorker(t, ctx, pool)
	setCurrentDeploymentForRuntimeInstanceTest(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		UPDATE worker_instances
		   SET total_milli_cpu = 2000,
		       total_memory_mib = 2048,
		       total_disk_mib = 2048,
		       total_execution_slots = 2,
		       available_milli_cpu = 2000,
		       available_memory_mib = 2048,
		       available_disk_mib = 2048,
		       available_execution_slots = 2
		 WHERE id = $1
	`, workerID); err != nil {
		t.Fatal(err)
	}
	otherWorkerID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instances (
				id, org_id, resource_id, worker_group_id, status, protocol_version,
			total_milli_cpu, total_memory_mib, total_disk_mib, total_execution_slots,
			available_milli_cpu, available_memory_mib, available_disk_mib, available_execution_slots,
			runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		)
			SELECT $1, org_id, $2, worker_group_id, 'active', protocol_version,
		       2000, 2048, 2048, 2, 2000, 2048, 2048, 2,
		       runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		  FROM worker_instances
		 WHERE id = $3
	`, otherWorkerID, "worker-"+shortUUID(otherWorkerID), workerID); err != nil {
		t.Fatal(err)
	}
	otherWorkspaceID := uuid.Must(uuid.NewV7())
	otherWorkspaceVersionID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
			INSERT INTO workspaces (
				id, public_id, org_id, worker_group_id, project_id, environment_id, deployment_sandbox_id, sandbox_id, sandbox_fingerprint
			)
			VALUES ($1, $7, $2, $3, $4, $5, $6, 'default', 'sandbox-fingerprint')
		`, otherWorkspaceID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID, ids.deploymentSandboxID, testWorkspacePublicID(t)); err != nil {
		t.Fatal(err)
	}
	var currentVersionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT current_version_id
		  FROM workspaces
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID).Scan(&currentVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
			INSERT INTO workspace_versions (
				id, public_id, org_id, project_id, environment_id, workspace_id, kind, state,
				artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at
			)
			SELECT $1, $5, org_id, project_id, environment_id, $2, kind, state,
			       artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, now()
			  FROM workspace_versions
			 WHERE org_id = $3
			   AND id = $4
		`, otherWorkspaceVersionID, otherWorkspaceID, ids.orgID, currentVersionID, testWorkspaceVersionPublicID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspaces
		   SET current_version_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, otherWorkspaceVersionID, ids.orgID, otherWorkspaceID); err != nil {
		t.Fatal(err)
	}
	instanceToken := "preparing-runtime-instance-token"
	instance, err := queries.CreateRuntimeInstanceForDeploymentSandbox(ctx, db.CreateRuntimeInstanceForDeploymentSandboxParams{
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RootfsDigest:        "sha256:rootfs",
		RuntimeABI:          "test",
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeIdentityID:   runtimeIdentityID,
		RuntimeKeyHash:      "runtime-key-hash",
		RuntimeKey:          []byte(`{"test":"preparing-runtime-adoption"}`),
		InstanceToken:       instanceToken,
		ExpiresAt:           pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	requestedMount, err := requestWorkspaceMountForTest(ctx, queries, db.EnsureWorkspaceMountRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Request:       []byte(`{"source":"preparing-runtime-adoption-test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = queries.ClaimWorkspaceMount(ctx, db.ClaimWorkspaceMountParams{
		RootfsDigest:                "sha256:rootfs",
		RuntimeABI:                  "test",
		GuestdAbi:                   "guestd-test",
		AdapterAbi:                  "adapter-test",
		NetworkPolicy:               []byte(`{"internet":true}`),
		RuntimeInstanceID:           pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeInstanceToken:        "must-not-create-cross-worker-cold-runtime",
		WorkerInstanceID:            pgvalue.UUID(otherWorkerID),
		WorkerGroupID:               dbtest.DefaultWorkerGroupID,
		GuestdChannelTokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		GuestdChannelTokenHash:      "other-worker-workspace-mount-channel-token-hash",
		RuntimeID:                   runtimeIdentityID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-worker claim with preparing runtime err = %v, want pgx.ErrNoRows", err)
	}
	_, err = queries.ClaimWorkspaceMount(ctx, db.ClaimWorkspaceMountParams{
		RootfsDigest:                "sha256:rootfs",
		RuntimeABI:                  "test",
		GuestdAbi:                   "guestd-test",
		AdapterAbi:                  "adapter-test",
		NetworkPolicy:               []byte(`{"internet":true}`),
		RuntimeInstanceID:           pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeInstanceToken:        "must-not-create-cold-runtime",
		WorkerInstanceID:            pgvalue.UUID(workerID),
		WorkerGroupID:               dbtest.DefaultWorkerGroupID,
		GuestdChannelTokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		GuestdChannelTokenHash:      "workspace-mount-channel-token-hash",
		RuntimeID:                   runtimeIdentityID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim with preparing runtime err = %v, want pgx.ErrNoRows", err)
	}
	reserved, err := queries.ReserveWorkspaceMountPreparingRuntime(ctx, db.ReserveWorkspaceMountPreparingRuntimeParams{
		RootfsDigest:                "sha256:rootfs",
		RuntimeABI:                  "test",
		GuestdAbi:                   "guestd-test",
		AdapterAbi:                  "adapter-test",
		WorkerInstanceID:            pgvalue.UUID(workerID),
		WorkerGroupID:               dbtest.DefaultWorkerGroupID,
		RuntimeID:                   runtimeIdentityID,
		GuestdChannelTokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reserved.ID != requestedMount.ID || reserved.PreparingRuntimeInstanceID != instance.ID {
		t.Fatalf("reserved mount/runtime = %s/%s, want %s/%s", pgvalue.UUIDString(reserved.ID), pgvalue.UUIDString(reserved.PreparingRuntimeInstanceID), pgvalue.UUIDString(requestedMount.ID), pgvalue.UUIDString(instance.ID))
	}
	otherRequestedMount, err := requestWorkspaceMountForTest(ctx, queries, db.EnsureWorkspaceMountRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(otherWorkspaceID),
		Request:       []byte(`{"source":"second-workspace-preparing-runtime-block-test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if otherRequestedMount.ID == requestedMount.ID {
		t.Fatal("second workspace reused first mount, want distinct mount")
	}
	_, err = queries.ClaimWorkspaceMount(ctx, db.ClaimWorkspaceMountParams{
		RootfsDigest:                "sha256:rootfs",
		RuntimeABI:                  "test",
		GuestdAbi:                   "guestd-test",
		AdapterAbi:                  "adapter-test",
		NetworkPolicy:               []byte(`{"internet":true}`),
		RuntimeInstanceID:           pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeInstanceToken:        "must-not-create-cold-runtime-after-reserve",
		WorkerInstanceID:            pgvalue.UUID(otherWorkerID),
		WorkerGroupID:               dbtest.DefaultWorkerGroupID,
		GuestdChannelTokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		GuestdChannelTokenHash:      "reserved-other-worker-workspace-mount-channel-token-hash",
		RuntimeID:                   runtimeIdentityID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim with reserved preparing runtime err = %v, want pgx.ErrNoRows", err)
	}
	_, err = queries.ClaimWorkspaceMount(ctx, db.ClaimWorkspaceMountParams{
		RootfsDigest:                "sha256:rootfs",
		RuntimeABI:                  "test",
		GuestdAbi:                   "guestd-test",
		AdapterAbi:                  "adapter-test",
		NetworkPolicy:               []byte(`{"internet":true}`),
		RuntimeInstanceID:           pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeInstanceToken:        "must-not-create-cold-runtime-for-second-workspace",
		WorkerInstanceID:            pgvalue.UUID(otherWorkerID),
		WorkerGroupID:               dbtest.DefaultWorkerGroupID,
		GuestdChannelTokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		GuestdChannelTokenHash:      "second-workspace-workspace-mount-channel-token-hash",
		RuntimeID:                   runtimeIdentityID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second workspace claim with reserved preparing runtime err = %v, want pgx.ErrNoRows", err)
	}
	var coldDuplicates int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM runtime_instances
		 WHERE org_id = $1
		   AND deployment_sandbox_id = $2
		   AND instance_token IN ('must-not-create-cold-runtime', 'must-not-create-cross-worker-cold-runtime', 'must-not-create-cold-runtime-after-reserve', 'must-not-create-cold-runtime-for-second-workspace')
	`, ids.orgID, ids.deploymentSandboxID).Scan(&coldDuplicates); err != nil {
		t.Fatal(err)
	}
	if coldDuplicates != 0 {
		t.Fatalf("cold duplicate runtimes = %d, want 0", coldDuplicates)
	}
}

func TestExpiredPreparingRuntimeAdoptionDoesNotBlockColdClaim(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, runtimeIdentityID := seedExactCapacityRuntimeWorker(t, ctx, pool)
	setCurrentDeploymentForRuntimeInstanceTest(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		UPDATE worker_instances
		   SET total_milli_cpu = 2000,
		       total_memory_mib = 2048,
		       total_disk_mib = 2048,
		       total_execution_slots = 2,
		       available_milli_cpu = 2000,
		       available_memory_mib = 2048,
		       available_disk_mib = 2048,
		       available_execution_slots = 2
		 WHERE id = $1
	`, workerID); err != nil {
		t.Fatal(err)
	}
	instance, err := queries.CreateRuntimeInstanceForDeploymentSandbox(ctx, db.CreateRuntimeInstanceForDeploymentSandboxParams{
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RootfsDigest:        "sha256:rootfs",
		RuntimeABI:          "test",
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeIdentityID:   runtimeIdentityID,
		RuntimeKeyHash:      "runtime-key-hash",
		RuntimeKey:          []byte(`{"test":"expired-preparing-runtime-adoption"}`),
		InstanceToken:       "expired-preparing-runtime-instance-token",
		ExpiresAt:           pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	requestedMount, err := requestWorkspaceMountForTest(ctx, queries, db.EnsureWorkspaceMountRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Request:       []byte(`{"source":"expired-preparing-runtime-adoption-test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = queries.ReserveWorkspaceMountPreparingRuntime(ctx, db.ReserveWorkspaceMountPreparingRuntimeParams{
		RootfsDigest:                "sha256:rootfs",
		RuntimeABI:                  "test",
		GuestdAbi:                   "guestd-test",
		AdapterAbi:                  "adapter-test",
		WorkerInstanceID:            pgvalue.UUID(workerID),
		WorkerGroupID:               dbtest.DefaultWorkerGroupID,
		RuntimeID:                   runtimeIdentityID,
		GuestdChannelTokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Second), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	released, err := queries.ReleaseExpiredPreparedRuntimeReservations(ctx, pgvalue.Timestamptz(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0].ID != requestedMount.ID {
		t.Fatalf("released mounts = %+v, want requested mount %s", released, pgvalue.UUIDString(requestedMount.ID))
	}
	claimed, err := queries.ClaimWorkspaceMount(ctx, db.ClaimWorkspaceMountParams{
		RootfsDigest:                "sha256:rootfs",
		RuntimeABI:                  "test",
		GuestdAbi:                   "guestd-test",
		AdapterAbi:                  "adapter-test",
		NetworkPolicy:               []byte(`{"internet":true}`),
		RuntimeInstanceID:           pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeInstanceToken:        "cold-runtime-after-expired-adoption",
		WorkerInstanceID:            pgvalue.UUID(workerID),
		WorkerGroupID:               dbtest.DefaultWorkerGroupID,
		GuestdChannelTokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		GuestdChannelTokenHash:      "workspace-mount-channel-token-hash",
		RuntimeID:                   runtimeIdentityID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != requestedMount.ID || claimed.RuntimeInstanceToken != "cold-runtime-after-expired-adoption" {
		t.Fatalf("claim after expired adoption = mount %s token %q, want cold fallback for %s", pgvalue.UUIDString(claimed.ID), claimed.RuntimeInstanceToken, pgvalue.UUIDString(requestedMount.ID))
	}
	var expiresAt pgtype.Timestamptz
	var reclaimReason string
	if err := pool.QueryRow(ctx, `
		SELECT expires_at, last_reclaim_reason
		  FROM runtime_instances
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(instance.ID)).Scan(&expiresAt, &reclaimReason); err != nil {
		t.Fatal(err)
	}
	if !expiresAt.Valid || expiresAt.Time.After(time.Now()) || reclaimReason != "adoption_expired" {
		t.Fatalf("expired preparing runtime expires_at=%+v reclaim=%q, want expired adoption marker", expiresAt, reclaimReason)
	}
}

func TestExpiredReadyRuntimeAdoptionReturnsRuntimeToReadyPool(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, runtimeIdentityID := seedExactCapacityRuntimeWorker(t, ctx, pool)
	setCurrentDeploymentForRuntimeInstanceTest(t, ctx, pool, ids)
	instanceToken := "expired-ready-adoption-runtime-token"
	instance, err := queries.CreateRuntimeInstanceForDeploymentSandbox(ctx, db.CreateRuntimeInstanceForDeploymentSandboxParams{
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RootfsDigest:        "sha256:rootfs",
		RuntimeABI:          "test",
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeIdentityID:   runtimeIdentityID,
		RuntimeKeyHash:      "runtime-key-hash",
		RuntimeKey:          []byte(`{"test":"expired-ready-runtime-adoption"}`),
		InstanceToken:       instanceToken,
		ExpiresAt:           pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	requestedMount, err := requestWorkspaceMountForTest(ctx, queries, db.EnsureWorkspaceMountRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Request:       []byte(`{"source":"expired-ready-runtime-adoption-test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = queries.ReserveWorkspaceMountPreparingRuntime(ctx, db.ReserveWorkspaceMountPreparingRuntimeParams{
		RootfsDigest:                "sha256:rootfs",
		RuntimeABI:                  "test",
		GuestdAbi:                   "guestd-test",
		AdapterAbi:                  "adapter-test",
		WorkerInstanceID:            pgvalue.UUID(workerID),
		WorkerGroupID:               dbtest.DefaultWorkerGroupID,
		RuntimeID:                   runtimeIdentityID,
		GuestdChannelTokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Second), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkRuntimeInstanceReady(ctx, db.MarkRuntimeInstanceReadyParams{
		ID:                 instance.ID,
		WorkerInstanceID:   pgvalue.UUID(workerID),
		InstanceToken:      instanceToken,
		ExpiresAt:          pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		RuntimeSubstrateID: pgtype.UUID{},
	}); err != nil {
		t.Fatal(err)
	}
	released, err := queries.ReleaseExpiredPreparedRuntimeReservations(ctx, pgvalue.Timestamptz(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0].ID != requestedMount.ID {
		t.Fatalf("released mounts = %+v, want requested mount %s", released, pgvalue.UUIDString(requestedMount.ID))
	}
	claimed, err := queries.ClaimWorkspaceMount(ctx, db.ClaimWorkspaceMountParams{
		RootfsDigest:                "sha256:rootfs",
		RuntimeABI:                  "test",
		GuestdAbi:                   "guestd-test",
		AdapterAbi:                  "adapter-test",
		NetworkPolicy:               []byte(`{"internet":true}`),
		RuntimeInstanceID:           pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeInstanceToken:        "must-not-create-cold-runtime-after-ready-adoption-expiry",
		WorkerInstanceID:            pgvalue.UUID(workerID),
		WorkerGroupID:               dbtest.DefaultWorkerGroupID,
		GuestdChannelTokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		GuestdChannelTokenHash:      "workspace-mount-channel-token-hash",
		RuntimeID:                   runtimeIdentityID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != requestedMount.ID || claimed.RuntimeInstanceToken != instanceToken {
		t.Fatalf("claim after expired ready adoption = mount %s token %q, want ready runtime token %q for %s", pgvalue.UUIDString(claimed.ID), claimed.RuntimeInstanceToken, instanceToken, pgvalue.UUIDString(requestedMount.ID))
	}
	var reclaimReason string
	if err := pool.QueryRow(ctx, `
		SELECT last_reclaim_reason
		  FROM runtime_instances
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(instance.ID)).Scan(&reclaimReason); err != nil {
		t.Fatal(err)
	}
	if reclaimReason != "" {
		t.Fatalf("ready runtime reclaim reason = %q, want unchanged", reclaimReason)
	}
}

func TestCreatePreparedRuntimeInstanceForWorkspaceMountSourceFitsExactWorkerCapacity(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, runtimeIdentityID := seedExactCapacityRuntimeWorker(t, ctx, pool)
	mountTokenHash := "workspace-mount-channel-token-hash"
	requestedMount, err := requestWorkspaceMountForTest(ctx, queries, db.EnsureWorkspaceMountRequestedParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Request:       []byte(`{"test":"prepared-source"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_mounts
		   SET guestd_channel_token_hash = $1,
		       guestd_channel_token_expires_at = now() + interval '1 hour'
		 WHERE org_id = $2
		   AND id = $3
	`, mountTokenHash, ids.orgID, pgvalue.MustUUIDValue(requestedMount.ID)); err != nil {
		t.Fatal(err)
	}

	instance, err := queries.CreatePreparedRuntimeInstanceForWorkspaceMountSource(ctx, db.CreatePreparedRuntimeInstanceForWorkspaceMountSourceParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		WorkerInstanceID:       pgvalue.UUID(workerID),
		RuntimeIdentityID:      runtimeIdentityID,
		RuntimeKeyHash:         "runtime-key-hash",
		RuntimeKey:             []byte(`{"test":"workspace-mount-source"}`),
		NetworkPolicy:          []byte(`{}`),
		InstanceToken:          "prepared-runtime-instance-token",
		ExpiresAt:              pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		WorkspaceMountID:       requestedMount.ID,
		GuestdChannelTokenHash: mountTokenHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if instance.ReservedCpuMillis != 1000 ||
		instance.ReservedMemoryMib != 1024 ||
		instance.ReservedExecutionSlots != 1 {
		t.Fatalf("reserved resources = cpu:%d mem:%d slots:%d, want sandbox floor resources", instance.ReservedCpuMillis, instance.ReservedMemoryMib, instance.ReservedExecutionSlots)
	}
}

func TestCreatePreparedRuntimeInstanceForWorkspaceMountSourceContinuesOnDrainingWorkerGroup(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, runtimeIdentityID := seedExactCapacityRuntimeWorker(t, ctx, pool)
	mountTokenHash := "draining-workspace-mount-channel-token-hash"
	requestedMount, err := requestWorkspaceMountForTest(ctx, queries, db.EnsureWorkspaceMountRequestedParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Request:       []byte(`{"test":"prepared-source-draining"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_mounts
		   SET guestd_channel_token_hash = $1,
		       guestd_channel_token_expires_at = now() + interval '1 hour'
		 WHERE org_id = $2
		   AND id = $3
	`, mountTokenHash, ids.orgID, pgvalue.MustUUIDValue(requestedMount.ID)); err != nil {
		t.Fatal(err)
	}
	markDefaultWorkerGroupDrainingWithStaleHealth(t, ctx, pool, ids)

	instance, err := queries.CreatePreparedRuntimeInstanceForWorkspaceMountSource(ctx, db.CreatePreparedRuntimeInstanceForWorkspaceMountSourceParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		WorkerInstanceID:       pgvalue.UUID(workerID),
		RuntimeIdentityID:      runtimeIdentityID,
		RuntimeKeyHash:         "runtime-key-hash",
		RuntimeKey:             []byte(`{"test":"workspace-mount-source-draining"}`),
		NetworkPolicy:          []byte(`{}`),
		InstanceToken:          "prepared-runtime-instance-draining-token",
		ExpiresAt:              pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		WorkspaceMountID:       requestedMount.ID,
		GuestdChannelTokenHash: mountTokenHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if instance.WorkerGroupID != dbtest.DefaultWorkerGroupID {
		t.Fatalf("instance worker group = %q, want %q", instance.WorkerGroupID, dbtest.DefaultWorkerGroupID)
	}
}

func TestPreparedRuntimeMountAdoptionRejectsDrainingWorkerGroup(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, runtimeIdentityID := seedExactCapacityRuntimeWorker(t, ctx, pool)
	setCurrentDeploymentForRuntimeInstanceTest(t, ctx, pool, ids)
	instance, err := queries.CreateRuntimeInstanceForDeploymentSandbox(ctx, db.CreateRuntimeInstanceForDeploymentSandboxParams{
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RootfsDigest:        "sha256:rootfs",
		RuntimeABI:          "test",
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeIdentityID:   runtimeIdentityID,
		RuntimeKeyHash:      "runtime-key-hash",
		RuntimeKey:          []byte(`{"test":"draining-prepared-adoption"}`),
		InstanceToken:       "draining-prepared-runtime-token",
		ExpiresAt:           pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	requestedMount, err := requestWorkspaceMountForTest(ctx, queries, db.EnsureWorkspaceMountRequestedParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Request:       []byte(`{"test":"draining-prepared-adoption"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	markDefaultWorkerGroupDrainingWithStaleHealth(t, ctx, pool, ids)

	_, err = queries.ReserveWorkspaceMountPreparingRuntime(ctx, db.ReserveWorkspaceMountPreparingRuntimeParams{
		RootfsDigest:                "sha256:rootfs",
		RuntimeABI:                  "test",
		GuestdAbi:                   "guestd-test",
		AdapterAbi:                  "adapter-test",
		WorkerInstanceID:            pgvalue.UUID(workerID),
		WorkerGroupID:               dbtest.DefaultWorkerGroupID,
		RuntimeID:                   runtimeIdentityID,
		GuestdChannelTokenExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reserve preparing runtime on draining worker group err = %v, want pgx.ErrNoRows", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET adopting_workspace_mount_id = $1,
		       adoption_expires_at = now() + interval '1 hour',
		       updated_at = now()
		 WHERE org_id = $2
		   AND id = $3
	`, pgvalue.MustUUIDValue(requestedMount.ID), ids.orgID, pgvalue.MustUUIDValue(instance.ID)); err != nil {
		t.Fatal(err)
	}
	_, err = queries.GetAwaitingPreparedRuntimeMountForWorker(ctx, db.GetAwaitingPreparedRuntimeMountForWorkerParams{
		WorkerInstanceID: pgvalue.UUID(workerID),
		WorkerGroupID:    dbtest.DefaultWorkerGroupID,
		RuntimeID:        runtimeIdentityID,
		RootfsDigest:     "sha256:rootfs",
		RuntimeABI:       "test",
		GuestdAbi:        "guestd-test",
		AdapterAbi:       "adapter-test",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("get awaiting prepared runtime mount on draining worker group err = %v, want pgx.ErrNoRows", err)
	}
}

func TestCreatePreparedRuntimeInstanceForWorkspaceMountSourceRejectsOwnedMount(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, runtimeIdentityID := seedExactCapacityRuntimeWorker(t, ctx, pool)
	mountTokenHash := "workspace-mount-channel-token-hash"
	requestedMount, err := requestWorkspaceMountForTest(ctx, queries, db.EnsureWorkspaceMountRequestedParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Request:       []byte(`{"test":"prepared-source-owned-mount"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_mounts
		   SET guestd_channel_token_hash = $1,
		       guestd_channel_token_expires_at = now() + interval '1 hour'
		 WHERE org_id = $2
		   AND id = $3
	`, mountTokenHash, ids.orgID, pgvalue.MustUUIDValue(requestedMount.ID)); err != nil {
		t.Fatal(err)
	}
	first, err := queries.CreatePreparedRuntimeInstanceForWorkspaceMountSource(ctx, db.CreatePreparedRuntimeInstanceForWorkspaceMountSourceParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		WorkerInstanceID:       pgvalue.UUID(workerID),
		RuntimeIdentityID:      runtimeIdentityID,
		RuntimeKeyHash:         "runtime-key-hash",
		RuntimeKey:             []byte(`{"test":"workspace-mount-source"}`),
		NetworkPolicy:          []byte(`{}`),
		InstanceToken:          "prepared-runtime-instance-token",
		ExpiresAt:              pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		WorkspaceMountID:       requestedMount.ID,
		GuestdChannelTokenHash: mountTokenHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runtime_instances
		   SET state = 'closed',
		       closed_at = now(),
		       expires_at = NULL,
		       updated_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(first.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_mounts
		   SET runtime_instance_id = $2,
		       updated_at = now()
		 WHERE org_id = $1
		   AND id = $3
	`, ids.orgID, pgvalue.MustUUIDValue(first.ID), pgvalue.MustUUIDValue(requestedMount.ID)); err != nil {
		t.Fatal(err)
	}
	_, err = queries.CreatePreparedRuntimeInstanceForWorkspaceMountSource(ctx, db.CreatePreparedRuntimeInstanceForWorkspaceMountSourceParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		WorkerInstanceID:       pgvalue.UUID(workerID),
		RuntimeIdentityID:      runtimeIdentityID,
		RuntimeKeyHash:         "runtime-key-hash-2",
		RuntimeKey:             []byte(`{"test":"workspace-mount-source-2"}`),
		NetworkPolicy:          []byte(`{}`),
		InstanceToken:          "prepared-runtime-instance-token-2",
		ExpiresAt:              pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		WorkspaceMountID:       requestedMount.ID,
		GuestdChannelTokenHash: mountTokenHash,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second prepared runtime create error = %v, want pgx.ErrNoRows", err)
	}
}

func TestGetWorkspaceMountForWorkerPrimitiveScopeRequiresRuntimeOwnerAndToken(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, _, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	var mountID uuid.UUID
	var runtimeInstanceToken string
	if err := pool.QueryRow(ctx, `
		SELECT workspace_mounts.id, runtime_instances.instance_token
		  FROM runs
		  JOIN workspace_mounts
		    ON workspace_mounts.org_id = runs.org_id
		   AND workspace_mounts.id = runs.workspace_mount_id
		  JOIN runtime_instances
		    ON runtime_instances.org_id = workspace_mounts.org_id
		   AND runtime_instances.id = workspace_mounts.runtime_instance_id
		 WHERE runs.org_id = $1
		   AND runs.id = $2
	`, ids.orgID, ids.runID).Scan(&mountID, &runtimeInstanceToken); err != nil {
		t.Fatal(err)
	}
	params := db.GetWorkspaceMountForWorkerPrimitiveScopeParams{
		OrgID:                pgvalue.UUID(ids.orgID),
		WorkerGroupID:        testWorkerGroupID,
		ProjectID:            pgvalue.UUID(ids.projectID),
		EnvironmentID:        pgvalue.UUID(ids.environmentID),
		WorkspaceID:          pgvalue.UUID(ids.workspaceID),
		ID:                   pgvalue.UUID(mountID),
		WorkerInstanceID:     pgvalue.UUID(workerID),
		RuntimeInstanceToken: runtimeInstanceToken,
	}
	row, err := queries.GetWorkspaceMountForWorkerPrimitiveScope(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(row.ID) != mountID {
		t.Fatalf("mount id = %s, want %s", pgvalue.MustUUIDValue(row.ID), mountID)
	}
	wrongWorker := params
	wrongWorker.WorkerInstanceID = pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.GetWorkspaceMountForWorkerPrimitiveScope(ctx, wrongWorker); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong worker error = %v, want pgx.ErrNoRows", err)
	}
	wrongToken := params
	wrongToken.RuntimeInstanceToken = runtimeInstanceToken + "-wrong"
	if _, err := queries.GetWorkspaceMountForWorkerPrimitiveScope(ctx, wrongToken); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong runtime token error = %v, want pgx.ErrNoRows", err)
	}
	wrongWorkerGroup := params
	wrongWorkerGroup.WorkerGroupID = "us-east-1-worker-group-2"
	if _, err := queries.GetWorkspaceMountForWorkerPrimitiveScope(ctx, wrongWorkerGroup); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong worker group error = %v, want pgx.ErrNoRows", err)
	}
}

func TestUpsertRuntimeSubstrateBlobReusesDigestRow(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	digest := testDigest("runtime-substrate-blob")
	if _, err := queries.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		OrgID:     pgvalue.UUID(ids.orgID),
		Digest:    digest,
		SizeBytes: 10,
		MediaType: "application/vnd.helmr.runtime-substrate.v0.tar",
	}); err != nil {
		t.Fatal(err)
	}
	first, err := queries.UpsertRuntimeSubstrateBlob(ctx, db.UpsertRuntimeSubstrateBlobParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		Digest:        digest,
		SizeBytes:     10,
		MediaType:     "application/vnd.helmr.runtime-substrate.v0.tar",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := queries.UpsertRuntimeSubstrateBlob(ctx, db.UpsertRuntimeSubstrateBlobParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		Digest:        digest,
		SizeBytes:     10,
		MediaType:     "application/vnd.helmr.runtime-substrate.v0.tar",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(second.ID) != pgvalue.MustUUIDValue(first.ID) {
		t.Fatalf("second artifact id = %s, want first id %s", pgvalue.MustUUIDValue(second.ID), pgvalue.MustUUIDValue(first.ID))
	}
}

func TestUpsertRuntimeSubstrateIsAtomicForConcurrentIdenticalReports(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	artifact := seedRuntimeSubstrateBlob(t, ctx, queries, ids, "runtime-substrate-concurrent")
	const workers = 8
	var wg sync.WaitGroup
	results := make(chan db.RuntimeSubstrate, workers)
	errs := make(chan error, workers)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			row, err := queries.UpsertRuntimeSubstrate(ctx, db.UpsertRuntimeSubstrateParams{
				ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:                     pgvalue.UUID(ids.orgID),
				WorkerGroupID:             dbtest.DefaultWorkerGroupID,
				ProjectID:                 pgvalue.UUID(ids.projectID),
				EnvironmentID:             pgvalue.UUID(ids.environmentID),
				DeploymentSandboxID:       pgvalue.UUID(ids.deploymentSandboxID),
				ArtifactID:                artifact.ID,
				SubstrateDigest:           "sha256:runtime-substrate-raw",
				SubstrateFormat:           "ext4",
				BuilderAbi:                "builder-v0",
				LayoutAbi:                 "layout-v0",
				SubstrateSizeBytes:        1024,
				Source:                    []byte(`{"test":"concurrent"}`),
				CreatedByWorkerInstanceID: pgtype.UUID{},
			})
			if err != nil {
				errs <- err
				return
			}
			results <- row
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var first uuid.UUID
	count := 0
	for row := range results {
		id := pgvalue.MustUUIDValue(row.ID)
		if first == uuid.Nil {
			first = id
		}
		if id != first {
			t.Fatalf("runtime substrate id = %s, want %s", id, first)
		}
		count++
	}
	if count != workers {
		t.Fatalf("upsert results = %d, want %d", count, workers)
	}
}

func TestMarkRuntimeInstanceReadyRejectsRuntimeSubstrateFromDifferentSandbox(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, runtimeIdentityID := seedExactCapacityRuntimeWorker(t, ctx, pool)
	setCurrentDeploymentForRuntimeInstanceTest(t, ctx, pool, ids)
	instanceToken := "runtime-instance-token"
	instance, err := queries.CreateRuntimeInstanceForDeploymentSandbox(ctx, db.CreateRuntimeInstanceForDeploymentSandboxParams{
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RootfsDigest:        "sha256:rootfs",
		RuntimeABI:          "test",
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeIdentityID:   runtimeIdentityID,
		RuntimeKeyHash:      "runtime-key-hash",
		RuntimeKey:          []byte(`{"test":"deployment-sandbox"}`),
		InstanceToken:       instanceToken,
		ExpiresAt:           pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	otherSandboxID := seedSiblingDeploymentSandbox(t, ctx, pool, ids)
	artifact := seedRuntimeSubstrateBlob(t, ctx, queries, ids, "runtime-substrate-other-sandbox")
	substrate, err := queries.UpsertRuntimeSubstrate(ctx, db.UpsertRuntimeSubstrateParams{
		ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                     pgvalue.UUID(ids.orgID),
		WorkerGroupID:             dbtest.DefaultWorkerGroupID,
		ProjectID:                 pgvalue.UUID(ids.projectID),
		EnvironmentID:             pgvalue.UUID(ids.environmentID),
		DeploymentSandboxID:       pgvalue.UUID(otherSandboxID),
		ArtifactID:                artifact.ID,
		SubstrateDigest:           "sha256:other-sandbox-raw",
		SubstrateFormat:           "ext4",
		BuilderAbi:                "builder-v0",
		LayoutAbi:                 "layout-v0",
		SubstrateSizeBytes:        1024,
		Source:                    []byte(`{"test":"other-sandbox"}`),
		CreatedByWorkerInstanceID: pgtype.UUID{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = queries.MarkRuntimeInstanceReady(ctx, db.MarkRuntimeInstanceReadyParams{
		ID:                 instance.ID,
		WorkerInstanceID:   pgvalue.UUID(workerID),
		InstanceToken:      instanceToken,
		RuntimeSubstrateID: substrate.ID,
		ExpiresAt:          pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("MarkRuntimeInstanceReady error = %v, want pgx.ErrNoRows", err)
	}
}

func TestRuntimeInstanceRejectsRuntimeSubstrateFromDifferentWorkerGroup(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID, runtimeIdentityID := seedExactCapacityRuntimeWorker(t, ctx, pool)
	setCurrentDeploymentForRuntimeInstanceTest(t, ctx, pool, ids)
	instanceToken := "runtime-instance-token"
	instance, err := queries.CreateRuntimeInstanceForDeploymentSandbox(ctx, db.CreateRuntimeInstanceForDeploymentSandboxParams{
		WorkerInstanceID:    pgvalue.UUID(workerID),
		RootfsDigest:        "sha256:rootfs",
		RuntimeABI:          "test",
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeIdentityID:   runtimeIdentityID,
		RuntimeKeyHash:      "runtime-key-hash",
		RuntimeKey:          []byte(`{"test":"deployment-sandbox"}`),
		InstanceToken:       instanceToken,
		ExpiresAt:           pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	otherWorkerGroupID, otherSandboxID := seedRuntimeSubstrateSourceInOtherWorkerGroup(t, ctx, pool, ids, "runtime-substrate-wrong-worker-group")
	digest := testDigest("runtime-substrate-wrong-worker-group")
	if _, err := queries.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		OrgID:     pgvalue.UUID(ids.orgID),
		Digest:    digest,
		SizeBytes: 1024,
		MediaType: "application/vnd.helmr.runtime-substrate.v0.ext4",
	}); err != nil {
		t.Fatal(err)
	}
	artifact, err := queries.UpsertRuntimeSubstrateBlob(ctx, db.UpsertRuntimeSubstrateBlobParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		Digest:        digest,
		SizeBytes:     1024,
		MediaType:     "application/vnd.helmr.runtime-substrate.v0.ext4",
	})
	if err != nil {
		t.Fatal(err)
	}
	substrate, err := queries.UpsertRuntimeSubstrate(ctx, db.UpsertRuntimeSubstrateParams{
		ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                     pgvalue.UUID(ids.orgID),
		WorkerGroupID:             otherWorkerGroupID,
		ProjectID:                 pgvalue.UUID(ids.projectID),
		EnvironmentID:             pgvalue.UUID(ids.environmentID),
		DeploymentSandboxID:       pgvalue.UUID(otherSandboxID),
		ArtifactID:                artifact.ID,
		SubstrateDigest:           "sha256:wrong-worker-group-runtime-substrate",
		SubstrateFormat:           "ext4",
		BuilderAbi:                "builder-v0",
		LayoutAbi:                 "layout-v0",
		SubstrateSizeBytes:        1024,
		Source:                    []byte(`{"test":"wrong-worker-group"}`),
		CreatedByWorkerInstanceID: pgtype.UUID{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = queries.MarkRuntimeInstanceReady(ctx, db.MarkRuntimeInstanceReadyParams{
		ID:                 instance.ID,
		WorkerInstanceID:   pgvalue.UUID(workerID),
		InstanceToken:      instanceToken,
		RuntimeSubstrateID: substrate.ID,
		ExpiresAt:          pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("MarkRuntimeInstanceReady wrong-worker-group substrate error = %v, want pgx.ErrNoRows", err)
	}
	_, err = queries.RenewRuntimeInstance(ctx, db.RenewRuntimeInstanceParams{
		ID:                 instance.ID,
		WorkerInstanceID:   pgvalue.UUID(workerID),
		InstanceToken:      instanceToken,
		RuntimeSubstrateID: substrate.ID,
		ExpiresAt:          pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("RenewRuntimeInstance wrong-worker-group substrate error = %v, want pgx.ErrNoRows", err)
	}
}

func TestGetDeploymentSandboxForWorkerGroupRejectsDisabledWorkerGroup(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	if _, err := queries.GetDeploymentSandboxForWorkerGroup(ctx, db.GetDeploymentSandboxForWorkerGroupParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ID:            pgvalue.UUID(ids.deploymentSandboxID),
	}); err != nil {
		t.Fatal(err)
	}
	disableDefaultWorkerGroupPlacement(t, ctx, pool, ids)
	if _, err := queries.GetDeploymentSandboxForWorkerGroup(ctx, db.GetDeploymentSandboxForWorkerGroupParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ID:            pgvalue.UUID(ids.deploymentSandboxID),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("disabled worker group lookup error = %v, want pgx.ErrNoRows", err)
	}
}

func TestGetRuntimeSubstrateForSandboxRejectsWrongWorkerGroupScope(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	artifact := seedRuntimeSubstrateBlob(t, ctx, queries, ids, "runtime-substrate-worker-group-mismatch")
	substrate, err := queries.UpsertRuntimeSubstrate(ctx, db.UpsertRuntimeSubstrateParams{
		ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                     pgvalue.UUID(ids.orgID),
		WorkerGroupID:             dbtest.DefaultWorkerGroupID,
		ProjectID:                 pgvalue.UUID(ids.projectID),
		EnvironmentID:             pgvalue.UUID(ids.environmentID),
		DeploymentSandboxID:       pgvalue.UUID(ids.deploymentSandboxID),
		ArtifactID:                artifact.ID,
		SubstrateDigest:           "sha256:runtime-substrate-worker-group-mismatch",
		SubstrateFormat:           "ext4",
		BuilderAbi:                "builder/v1",
		LayoutAbi:                 "layout/v1",
		SubstrateSizeBytes:        1024,
		Source:                    []byte(`{"test":"worker-group-mismatch"}`),
		CreatedByWorkerInstanceID: pgtype.UUID{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.GetRuntimeSubstrateForSandbox(ctx, db.GetRuntimeSubstrateForSandboxParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		WorkerGroupID:       dbtest.DefaultWorkerGroupID,
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		SubstrateDigest:     substrate.SubstrateDigest,
		SubstrateFormat:     substrate.SubstrateFormat,
		BuilderAbi:          substrate.BuilderAbi,
		LayoutAbi:           substrate.LayoutAbi,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = queries.GetRuntimeSubstrateForSandbox(ctx, db.GetRuntimeSubstrateForSandboxParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		WorkerGroupID:       dbtest.DefaultWorkerGroupID + "-other",
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		SubstrateDigest:     substrate.SubstrateDigest,
		SubstrateFormat:     substrate.SubstrateFormat,
		BuilderAbi:          substrate.BuilderAbi,
		LayoutAbi:           substrate.LayoutAbi,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong worker group lookup error = %v, want pgx.ErrNoRows", err)
	}
}

func TestGetRuntimeSubstrateForSandboxRejectsDisabledWorkerGroup(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	artifact := seedRuntimeSubstrateBlob(t, ctx, queries, ids, "runtime-substrate-disabled-worker-group")
	substrate, err := queries.UpsertRuntimeSubstrate(ctx, db.UpsertRuntimeSubstrateParams{
		ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                     pgvalue.UUID(ids.orgID),
		WorkerGroupID:             dbtest.DefaultWorkerGroupID,
		ProjectID:                 pgvalue.UUID(ids.projectID),
		EnvironmentID:             pgvalue.UUID(ids.environmentID),
		DeploymentSandboxID:       pgvalue.UUID(ids.deploymentSandboxID),
		ArtifactID:                artifact.ID,
		SubstrateDigest:           "sha256:runtime-substrate-disabled-worker-group",
		SubstrateFormat:           "ext4",
		BuilderAbi:                "builder/v1",
		LayoutAbi:                 "layout/v1",
		SubstrateSizeBytes:        1024,
		Source:                    []byte(`{"test":"disabled-worker-group"}`),
		CreatedByWorkerInstanceID: pgtype.UUID{},
	})
	if err != nil {
		t.Fatal(err)
	}
	disableDefaultWorkerGroupPlacement(t, ctx, pool, ids)

	_, err = queries.GetRuntimeSubstrateForSandbox(ctx, db.GetRuntimeSubstrateForSandboxParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		WorkerGroupID:       dbtest.DefaultWorkerGroupID,
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		DeploymentSandboxID: pgvalue.UUID(ids.deploymentSandboxID),
		SubstrateDigest:     substrate.SubstrateDigest,
		SubstrateFormat:     substrate.SubstrateFormat,
		BuilderAbi:          substrate.BuilderAbi,
		LayoutAbi:           substrate.LayoutAbi,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("disabled worker group lookup error = %v, want pgx.ErrNoRows", err)
	}
}

func seedExactCapacityRuntimeWorker(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (uuid.UUID, string) {
	t.Helper()
	workerID := uuid.Must(uuid.NewV7())
	runtimeIdentityID := "runtime-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	workerResourceID := "worker-" + shortUUID(workerID)
	var workerGroupID string
	if err := pool.QueryRow(ctx, `SELECT id FROM worker_groups WHERE id = $1 AND name = 'default'`, dbtest.DefaultWorkerGroupID).Scan(&workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_identities (id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile)
		VALUES ($1, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, runtimeIdentityID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instances (
				id, org_id, resource_id, worker_group_id, status, protocol_version,
			total_milli_cpu, total_memory_mib, total_disk_mib, total_execution_slots,
			available_milli_cpu, available_memory_mib, available_disk_mib, available_execution_slots,
			runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		)
			VALUES ($1, $2, $3, $4, 'active', $5,
			1000, 1024, 1024, 1, 1000, 1024, 1024, 1,
			$6, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
		`, workerID, dbtest.DefaultOrgID, workerResourceID, workerGroupID, api.CurrentWorkerProtocolVersion, runtimeIdentityID); err != nil {
		t.Fatal(err)
	}
	return workerID, runtimeIdentityID
}

func setCurrentDeploymentForRuntimeInstanceTest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		UPDATE environments
		   SET current_deployment_id = $1
		 WHERE org_id = $2
		   AND project_id = $3
		   AND id = $4
	`, ids.deploymentID, ids.orgID, ids.projectID, ids.environmentID); err != nil {
		t.Fatal(err)
	}
}

func seedRuntimeSubstrateBlob(t *testing.T, ctx context.Context, queries *db.Queries, ids integrationIDs, label string) db.Artifact {
	t.Helper()
	digest := testDigest(label)
	if _, err := queries.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		OrgID:     pgvalue.UUID(ids.orgID),
		Digest:    digest,
		SizeBytes: 1024,
		MediaType: "application/vnd.helmr.runtime-substrate.v0.ext4",
	}); err != nil {
		t.Fatal(err)
	}
	artifact, err := queries.UpsertRuntimeSubstrateBlob(ctx, db.UpsertRuntimeSubstrateBlobParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		Digest:        digest,
		SizeBytes:     1024,
		MediaType:     "application/vnd.helmr.runtime-substrate.v0.ext4",
	})
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func runtimeSubstrateSourceForTarget(t *testing.T, target db.ListRuntimeSubstratePrepareTargetsRow, overrides map[string]string) []byte {
	t.Helper()
	source := map[string]string{
		"sandbox_artifact_digest": target.SandboxImageArtifactDigest,
		"sandbox_artifact_format": target.SandboxImageArtifactFormat,
		"image_digest":            target.ImageDigest,
		"rootfs_digest":           target.RootfsDigest,
		"runtime_abi":             target.RuntimeABI,
		"guestd_abi":              target.GuestdAbi,
		"adapter_abi":             target.AdapterAbi,
		"workspace_mount_path":    target.WorkspaceMountPath,
	}
	maps.Copy(source, overrides)
	body, err := json.Marshal(map[string]any{
		"producer":         "test",
		"substrate_source": source,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func runtimeSubstratePreparePayloadForTarget(t *testing.T, target db.ListRuntimeSubstratePrepareTargetsRow, overrides map[string]string) []byte {
	t.Helper()
	source := api.WorkerPreparedRuntimeSource{
		DeploymentSandboxID:        pgvalue.UUIDString(target.DeploymentSandboxID),
		RuntimeID:                  target.RuntimeIdentityID,
		SandboxImageArtifact:       api.CASObject{Digest: target.SandboxImageArtifactDigest, MediaType: target.SandboxImageArtifactMediaType, SizeBytes: target.SandboxImageArtifactSizeBytes},
		SandboxImageArtifactFormat: target.SandboxImageArtifactFormat,
		RootfsDigest:               target.RootfsDigest,
		ImageDigest:                target.ImageDigest,
		ImageFormat:                target.ImageFormat,
		WorkspaceMountPath:         target.WorkspaceMountPath,
		RuntimeABI:                 target.RuntimeABI,
		GuestdABI:                  target.GuestdAbi,
		AdapterABI:                 target.AdapterAbi,
	}
	for key, value := range overrides {
		switch key {
		case "sandbox_artifact_digest":
			source.SandboxImageArtifact.Digest = value
		case "sandbox_artifact_format":
			source.SandboxImageArtifactFormat = value
		case "image_digest":
			source.ImageDigest = value
		case "rootfs_digest":
			source.RootfsDigest = value
		case "runtime_abi":
			source.RuntimeABI = value
		case "guestd_abi":
			source.GuestdABI = value
		case "adapter_abi":
			source.AdapterABI = value
		case "workspace_mount_path":
			source.WorkspaceMountPath = value
		}
	}
	body, err := json.Marshal(api.WorkerRuntimeSubstratePrepareCommand{
		DeploymentSandboxID: pgvalue.UUIDString(target.DeploymentSandboxID),
		Source:              source,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestListRuntimeSubstratePrepareTargetsSuppressesExistingArtifact(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	seedExactCapacityRuntimeWorker(t, ctx, pool)
	setCurrentDeploymentForRuntimeInstanceTest(t, ctx, pool, ids)
	if _, err := requestWorkspaceMountForTest(ctx, queries, db.EnsureWorkspaceMountRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Request:       []byte(`{"source":"substrate-demand-test"}`),
	}); err != nil {
		t.Fatal(err)
	}
	targets, err := queries.ListRuntimeSubstratePrepareTargets(ctx, db.ListRuntimeSubstratePrepareTargetsParams{
		SubstrateFormat:     substrate.Format,
		SubstrateBuilderAbi: substrate.BuilderABI,
		SubstrateLayoutAbi:  substrate.LayoutABI,
		RowLimit:            10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].DeploymentSandboxID != pgvalue.UUID(ids.deploymentSandboxID) {
		t.Fatalf("targets before substrate = %+v, want one current sandbox target", targets)
	}
	target := targets[0]
	artifact := seedRuntimeSubstrateBlob(t, ctx, queries, ids, "runtime-substrate-existing")
	if _, err := queries.UpsertRuntimeSubstrate(ctx, db.UpsertRuntimeSubstrateParams{
		ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                     pgvalue.UUID(ids.orgID),
		WorkerGroupID:             dbtest.DefaultWorkerGroupID,
		ProjectID:                 pgvalue.UUID(ids.projectID),
		EnvironmentID:             pgvalue.UUID(ids.environmentID),
		DeploymentSandboxID:       pgvalue.UUID(ids.deploymentSandboxID),
		ArtifactID:                artifact.ID,
		SubstrateDigest:           "sha256:prepared-substrate",
		SubstrateFormat:           substrate.Format,
		BuilderAbi:                substrate.BuilderABI,
		LayoutAbi:                 "old-layout-abi",
		SubstrateSizeBytes:        1024,
		Source:                    runtimeSubstrateSourceForTarget(t, target, nil),
		CreatedByWorkerInstanceID: pgtype.UUID{},
	}); err != nil {
		t.Fatal(err)
	}
	targets, err = queries.ListRuntimeSubstratePrepareTargets(ctx, db.ListRuntimeSubstratePrepareTargetsParams{
		SubstrateFormat:     substrate.Format,
		SubstrateBuilderAbi: substrate.BuilderABI,
		SubstrateLayoutAbi:  substrate.LayoutABI,
		RowLimit:            10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].DeploymentSandboxID != pgvalue.UUID(ids.deploymentSandboxID) {
		t.Fatalf("targets after stale substrate = %+v, want current sandbox target", targets)
	}
	if _, err := queries.UpsertRuntimeSubstrate(ctx, db.UpsertRuntimeSubstrateParams{
		ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                     pgvalue.UUID(ids.orgID),
		WorkerGroupID:             dbtest.DefaultWorkerGroupID,
		ProjectID:                 pgvalue.UUID(ids.projectID),
		EnvironmentID:             pgvalue.UUID(ids.environmentID),
		DeploymentSandboxID:       pgvalue.UUID(ids.deploymentSandboxID),
		ArtifactID:                artifact.ID,
		SubstrateDigest:           "sha256:prepared-substrate-stale-source",
		SubstrateFormat:           substrate.Format,
		BuilderAbi:                substrate.BuilderABI,
		LayoutAbi:                 substrate.LayoutABI,
		SubstrateSizeBytes:        1024,
		Source:                    runtimeSubstrateSourceForTarget(t, target, map[string]string{"rootfs_digest": "sha256:old-rootfs"}),
		CreatedByWorkerInstanceID: pgtype.UUID{},
	}); err != nil {
		t.Fatal(err)
	}
	targets, err = queries.ListRuntimeSubstratePrepareTargets(ctx, db.ListRuntimeSubstratePrepareTargetsParams{
		SubstrateFormat:     substrate.Format,
		SubstrateBuilderAbi: substrate.BuilderABI,
		SubstrateLayoutAbi:  substrate.LayoutABI,
		RowLimit:            10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].DeploymentSandboxID != pgvalue.UUID(ids.deploymentSandboxID) {
		t.Fatalf("targets after stale substrate source = %+v, want current sandbox target", targets)
	}
	if _, err := queries.CreateWorkerCommand(ctx, db.CreateWorkerCommandParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		WorkerGroupID:       target.WorkerGroupID,
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		WorkerInstanceID:    target.WorkerInstanceID,
		DeploymentSandboxID: target.DeploymentSandboxID,
		RunStateVersion:     pgtype.Int8{},
		Kind:                db.WorkerCommandKindRuntimeSubstratePrepare,
		Payload:             runtimeSubstratePreparePayloadForTarget(t, target, map[string]string{"rootfs_digest": "sha256:old-rootfs"}),
	}); err != nil {
		t.Fatal(err)
	}
	targets, err = queries.ListRuntimeSubstratePrepareTargets(ctx, db.ListRuntimeSubstratePrepareTargetsParams{
		SubstrateFormat:     substrate.Format,
		SubstrateBuilderAbi: substrate.BuilderABI,
		SubstrateLayoutAbi:  substrate.LayoutABI,
		RowLimit:            10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].DeploymentSandboxID != pgvalue.UUID(ids.deploymentSandboxID) {
		t.Fatalf("targets after stale substrate command = %+v, want current sandbox target", targets)
	}
	otherWorkerGroupID := dbtest.DefaultWorkerGroupID + "-cache-only"
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_groups (id, region_id, name, state, health_state, routing_fresh_until)
		VALUES ($1, $2, $1, 'active', 'healthy', now() + interval '5 minutes')
	`, otherWorkerGroupID, dbtest.DefaultRegionID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertRuntimeSubstrate(ctx, db.UpsertRuntimeSubstrateParams{
		ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                     pgvalue.UUID(ids.orgID),
		WorkerGroupID:             otherWorkerGroupID,
		ProjectID:                 pgvalue.UUID(ids.projectID),
		EnvironmentID:             pgvalue.UUID(ids.environmentID),
		DeploymentSandboxID:       pgvalue.UUID(ids.deploymentSandboxID),
		ArtifactID:                artifact.ID,
		SubstrateDigest:           "sha256:prepared-substrate-other-worker-group",
		SubstrateFormat:           substrate.Format,
		BuilderAbi:                substrate.BuilderABI,
		LayoutAbi:                 substrate.LayoutABI,
		SubstrateSizeBytes:        1024,
		Source:                    runtimeSubstrateSourceForTarget(t, target, nil),
		CreatedByWorkerInstanceID: pgtype.UUID{},
	}); err != nil {
		t.Fatal(err)
	}
	targets, err = queries.ListRuntimeSubstratePrepareTargets(ctx, db.ListRuntimeSubstratePrepareTargetsParams{
		SubstrateFormat:     substrate.Format,
		SubstrateBuilderAbi: substrate.BuilderABI,
		SubstrateLayoutAbi:  substrate.LayoutABI,
		RowLimit:            10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].DeploymentSandboxID != pgvalue.UUID(ids.deploymentSandboxID) {
		t.Fatalf("targets after other worker group substrate = %+v, want current worker group target", targets)
	}
	exactCommand, err := queries.CreateWorkerCommand(ctx, db.CreateWorkerCommandParams{
		OrgID:               pgvalue.UUID(ids.orgID),
		WorkerGroupID:       target.WorkerGroupID,
		ProjectID:           pgvalue.UUID(ids.projectID),
		EnvironmentID:       pgvalue.UUID(ids.environmentID),
		WorkerInstanceID:    target.WorkerInstanceID,
		DeploymentSandboxID: target.DeploymentSandboxID,
		RunStateVersion:     pgtype.Int8{},
		Kind:                db.WorkerCommandKindRuntimeSubstratePrepare,
		Payload:             runtimeSubstratePreparePayloadForTarget(t, target, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	targets, err = queries.ListRuntimeSubstratePrepareTargets(ctx, db.ListRuntimeSubstratePrepareTargetsParams{
		SubstrateFormat:     substrate.Format,
		SubstrateBuilderAbi: substrate.BuilderABI,
		SubstrateLayoutAbi:  substrate.LayoutABI,
		RowLimit:            10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Fatalf("targets after exact substrate command = %+v, want none", targets)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE worker_commands
		   SET acknowledged_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, exactCommand.ID); err != nil {
		t.Fatal(err)
	}
	targets, err = queries.ListRuntimeSubstratePrepareTargets(ctx, db.ListRuntimeSubstratePrepareTargetsParams{
		SubstrateFormat:     substrate.Format,
		SubstrateBuilderAbi: substrate.BuilderABI,
		SubstrateLayoutAbi:  substrate.LayoutABI,
		RowLimit:            10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].DeploymentSandboxID != pgvalue.UUID(ids.deploymentSandboxID) {
		t.Fatalf("targets after acknowledged exact substrate command = %+v, want current sandbox target", targets)
	}
	if _, err := queries.UpsertRuntimeSubstrate(ctx, db.UpsertRuntimeSubstrateParams{
		ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                     pgvalue.UUID(ids.orgID),
		WorkerGroupID:             dbtest.DefaultWorkerGroupID,
		ProjectID:                 pgvalue.UUID(ids.projectID),
		EnvironmentID:             pgvalue.UUID(ids.environmentID),
		DeploymentSandboxID:       pgvalue.UUID(ids.deploymentSandboxID),
		ArtifactID:                artifact.ID,
		SubstrateDigest:           "sha256:prepared-substrate-current-source",
		SubstrateFormat:           substrate.Format,
		BuilderAbi:                substrate.BuilderABI,
		LayoutAbi:                 substrate.LayoutABI,
		SubstrateSizeBytes:        1024,
		Source:                    runtimeSubstrateSourceForTarget(t, target, nil),
		CreatedByWorkerInstanceID: pgtype.UUID{},
	}); err != nil {
		t.Fatal(err)
	}
	targets, err = queries.ListRuntimeSubstratePrepareTargets(ctx, db.ListRuntimeSubstratePrepareTargetsParams{
		SubstrateFormat:     substrate.Format,
		SubstrateBuilderAbi: substrate.BuilderABI,
		SubstrateLayoutAbi:  substrate.LayoutABI,
		RowLimit:            10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Fatalf("targets after substrate = %+v, want none", targets)
	}
}

func seedSiblingDeploymentSandbox(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) uuid.UUID {
	t.Helper()
	otherSandboxID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployment_sandboxes (
			id,
			public_id,
			org_id,
			project_id,
			environment_id,
			deployment_id,
			sandbox_id,
			image_artifact_id,
			image_artifact_format,
			rootfs_digest,
			image_digest,
			image_format,
			workspace_mount_path,
			runtime_abi,
			guestd_abi,
			adapter_abi,
			filesystem_format,
			contract_version,
			fingerprint
		)
		SELECT $1,
		       $6,
		       org_id,
		       project_id,
		       environment_id,
		       deployment_id,
		       'other',
		       image_artifact_id,
		       image_artifact_format,
		       rootfs_digest,
		       image_digest,
		       image_format,
		       workspace_mount_path,
		       runtime_abi,
		       guestd_abi,
		       adapter_abi,
		       filesystem_format,
		       contract_version,
		       'other-sandbox-fingerprint'
		  FROM deployment_sandboxes
		 WHERE org_id = $2
		   AND project_id = $3
		   AND environment_id = $4
		   AND id = $5
	`, otherSandboxID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentSandboxID, testSandboxPublicID(t)); err != nil {
		t.Fatal(err)
	}
	return otherSandboxID
}
