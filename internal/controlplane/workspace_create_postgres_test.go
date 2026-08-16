package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestRunPinnedWorkspaceCreateUsesSourceDeploymentAndFencesBeforeClaim(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	sourceWorkspaceID := fixture.workspaceIDs[0]
	source, err := fixture.server.startTask(t.Context(), taskStartRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		TaskDeclaredID: "resize-image",
		PayloadPresent: true,
		Payload:        []byte(`{"source":"workspace-create"}`),
		WorkspaceID:    sourceWorkspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE environments SET current_deployment_id = NULL WHERE id = $1
	`, fixture.environmentID); err != nil {
		t.Fatal(err)
	}

	stale := errors.New("stale source run")
	var claimsBefore int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM idempotency_claims
	`).Scan(&claimsBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.server.createWorkspace(t.Context(), workspaceCreateRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		Declaration: workspaceDeclarationSelector{
			Kind: workspaceDeclarationRunPinned, RunID: source.RunID,
		},
		DeclaredID: "workspace.v1", IdempotencyKey: "blocked",
		Authorize: func(context.Context, db.Querier) error {
			return stale
		},
	}); !errors.Is(err, stale) {
		t.Fatalf("stale source error = %v", err)
	}
	var claimsAfter int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM idempotency_claims
	`).Scan(&claimsAfter); err != nil {
		t.Fatal(err)
	}
	if claimsAfter != claimsBefore {
		t.Fatalf("stale source changed claim count from %d to %d", claimsBefore, claimsAfter)
	}

	key := "run-pinned"
	request := workspaceCreateRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		Declaration: workspaceDeclarationSelector{
			Kind: workspaceDeclarationRunPinned, RunID: source.RunID,
		},
		DeclaredID: "workspace.v1", Key: &key,
		Secrets: []api.WorkspaceSecret{
			{Name: "API_TOKEN", Env: "API_TOKEN"},
		},
		IdempotencyKey: "create",
		Authorize: func(context.Context, db.Querier) error {
			return nil
		},
	}
	created, err := fixture.server.createWorkspace(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if created.Snapshot.ID != created.WorkspaceID.String() ||
		created.Snapshot.Status != api.WorkspaceStatusAvailable ||
		len(created.Snapshot.Secrets) != 1 ||
		created.Snapshot.Secrets[0] != (api.WorkspaceSecret{Name: "API_TOKEN", Env: "API_TOKEN"}) {
		t.Fatalf("creation snapshot = %+v", created.Snapshot)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE workspaces
		   SET state = 'deleting', desired_state = 'deleted', updated_at = now() + interval '1 minute'
		 WHERE id = $1
	`, created.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	replayed, err := fixture.server.createWorkspace(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	createdSnapshot, err := json.Marshal(created.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	replayedSnapshot, err := json.Marshal(replayed.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.WorkspaceID != created.WorkspaceID ||
		!bytes.Equal(replayedSnapshot, createdSnapshot) {
		t.Fatalf("replayed = %+v, created = %+v", replayed, created)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE environments SET current_deployment_id = $1 WHERE id = $2
	`, fixture.deploymentID, fixture.environmentID); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.server.createWorkspace(t.Context(), workspaceCreateRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		Declaration: workspaceDeclarationSelector{Kind: workspaceDeclarationPromoted},
		DeclaredID:  request.DeclaredID, Key: request.Key, Secrets: request.Secrets,
		IdempotencyKey: request.IdempotencyKey,
	})
	var keyConflict WorkspaceKeyConflictError
	if !errors.As(err, &keyConflict) {
		t.Fatalf("cross-authority create error = %v, want WorkspaceKeyConflictError", err)
	}

	var deploymentID uuid.UUID
	var versionCount, secretPlacementCount int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT deployment_definitions.deployment_id,
		       (SELECT count(*) FROM workspace_versions WHERE workspace_id = workspaces.id),
		       (SELECT count(*) FROM workspace_secrets WHERE workspace_id = workspaces.id)
		  FROM workspaces
		  JOIN deployment_definitions
		    ON deployment_definitions.id = workspaces.deployment_definition_id
		 WHERE workspaces.id = $1
	`, created.WorkspaceID).Scan(&deploymentID, &versionCount, &secretPlacementCount); err != nil {
		t.Fatal(err)
	}
	if deploymentID != fixture.deploymentID || versionCount != 1 || secretPlacementCount != 1 {
		t.Fatalf(
			"deployment=%s versions=%d secret placements=%d",
			deploymentID,
			versionCount,
			secretPlacementCount,
		)
	}
}

func TestRunSourcedWorkspaceSelfExecAndDeleteAreBusyWithoutSideEffects(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	sourceWorkspaceID := fixture.workspaceIDs[0]
	source, err := fixture.server.startTask(t.Context(), taskStartRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		TaskDeclaredID: "resize-image", PayloadPresent: true,
		Payload:     []byte(`{"source":"self-workspace"}`),
		WorkspaceID: sourceWorkspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := fixture.server.db.GetWorkspace(
		t.Context(),
		db.GetWorkspaceParams{
			OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
			EnvironmentID: pgvalue.UUID(fixture.environmentID), ID: pgvalue.UUID(sourceWorkspaceID),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var claimsBefore int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM idempotency_claims
	`).Scan(&claimsBefore); err != nil {
		t.Fatal(err)
	}
	authorize := func(context.Context, db.Querier) error { return nil }
	stale := errors.New("stale source authority")
	_, err = fixture.server.admitWorkspaceExec(t.Context(), workspaceExecRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		Workspace: record,
		Creator: workspaceExecCreator{
			SubjectType: "run", SubjectID: source.RunID.String(),
		},
		Command: []string{"true"}, IdempotencyKey: "stale-exec",
		Authorize: func(context.Context, db.Querier) error {
			return stale
		},
	})
	if !errors.Is(err, stale) {
		t.Fatalf("stale exec error = %v", err)
	}
	_, err = fixture.server.deleteWorkspace(t.Context(), workspaceDeleteRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		WorkspaceID: sourceWorkspaceID, IdempotencyKey: "stale-delete",
		Authorize: func(context.Context, db.Querier) error {
			return stale
		},
	})
	if !errors.Is(err, stale) {
		t.Fatalf("stale delete error = %v", err)
	}
	_, err = fixture.server.admitWorkspaceExec(t.Context(), workspaceExecRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		Workspace: record,
		Creator: workspaceExecCreator{
			SubjectType: "run", SubjectID: source.RunID.String(),
		},
		Command: []string{"true"}, IdempotencyKey: "self-exec", Authorize: authorize,
	})
	if !errors.Is(err, errWorkspaceBusy) {
		t.Fatalf("self exec error = %v", err)
	}
	_, err = fixture.server.deleteWorkspace(t.Context(), workspaceDeleteRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		WorkspaceID: sourceWorkspaceID, IdempotencyKey: "self-delete", Authorize: authorize,
	})
	if !errors.Is(err, errWorkspaceBusy) {
		t.Fatalf("self delete error = %v", err)
	}
	var processCount, claimCount int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM workspace_processes WHERE workspace_id = $1
	`, pgvalue.MustUUIDValue(record.ID)).Scan(&processCount); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM idempotency_claims
	`).Scan(&claimCount); err != nil {
		t.Fatal(err)
	}
	var state db.WorkspaceState
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT state FROM workspaces WHERE id = $1
	`, pgvalue.MustUUIDValue(record.ID)).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if processCount != 0 || claimCount != claimsBefore || state != db.WorkspaceStateActive {
		t.Fatalf("processes=%d claims=%d before=%d state=%s", processCount, claimCount, claimsBefore, state)
	}
}

func TestWorkspaceDeletePublishesOwnerlessMountCleanupOnce(t *testing.T) {
	product := newActorStartPostgresFixture(t, 1)
	poolFixture := newAdminPoolPostgresFixture(t, product.pool, "us-east-1")
	workerPool := poolFixture.addActivePool(t, "workspace-delete-cleanup")
	workspaceID := product.workspaceIDs[0]

	var headVersionID, sandboxDefinitionID uuid.UUID
	if err := product.pool.QueryRow(t.Context(), `
SELECT head_version_id, deployment_definition_id
  FROM workspaces WHERE id = $1`, workspaceID).Scan(&headVersionID, &sandboxDefinitionID); err != nil {
		t.Fatal(err)
	}
	workerID := uuid.Must(uuid.NewV7())
	runtimeID := uuid.Must(uuid.NewV7())
	substrateID := uuid.Must(uuid.NewV7())
	if _, err := product.pool.Exec(t.Context(), `
INSERT INTO runtime_substrates (
    id, org_id, project_id, environment_id, deployment_definition_id,
    substrate_digest, substrate_format, substrate_contract, substrate_size_bytes
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1)`,
		substrateID, product.orgID, product.projectID, product.environmentID,
		sandboxDefinitionID, dbtest.Digest("workspace-delete-substrate"),
		poolFixture.substrateFormat, poolFixture.substrateContract,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := product.pool.Exec(t.Context(), `
INSERT INTO worker_instances (
    id, resource_id, worker_group_id, worker_pool_id, state,
    current_epoch, current_service_id, runtime_identity_id,
    substrate_format, substrate_contract,
    epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
    per_vm_cpu_millis, per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes,
    max_vm_slots, max_runtime_starts,
    cpu_environment, cpu_environment_digest, observed_at,
    epoch_started_at, activated_at
) VALUES (
    $1, 'workspace-delete-worker', $2, $3, 'active',
    1, $4, $5, $6, $7,
    4000, 8589934592, 34359738368,
    1000, 1073741824, 4294967296,
    4, 4, '{"vendor":"test"}'::jsonb, $8, now(), now(), now()
)`, workerID, poolFixture.group.ID, workerPool.ID, uuid.Must(uuid.NewV7()),
		poolFixture.runtimeIdentityID, poolFixture.substrateFormat,
		poolFixture.substrateContract, dbtest.Digest("workspace-delete-cpu-environment"),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := product.pool.Exec(t.Context(), `
INSERT INTO runtime_instances (
    id, org_id, worker_group_id, project_id, environment_id, region_id,
    worker_instance_id, runtime_identity_id, deployment_definition_id,
    runtime_substrate_id, worker_epoch, vm_vcpu_count, cpu_config_digest,
    reserved_cpu_millis, reserved_memory_bytes,
    reserved_guest_ephemeral_disk_bytes, reserved_execution_slots,
    workspace_id, desired_reason,
    observed_state, observed_version, observed_desired_version,
    preparing_at, ready_at
) VALUES (
    $1, $2, $3, $4, $5, 'us-east-1', $6, $7, $8, $9,
    1, 1, $10, 1000, 1073741824, 4294967296, 1,
    $11, 'placed', 'ready', 1, 1, now(), now()
)`, runtimeID, product.orgID, poolFixture.group.ID, product.projectID,
		product.environmentID, workerID, poolFixture.runtimeIdentityID,
		sandboxDefinitionID, substrateID, poolFixture.cpuConfigDigest, workspaceID,
	); err != nil {
		t.Fatal(err)
	}
	mountID := uuid.Must(uuid.NewV7())
	if _, err := product.pool.Exec(t.Context(), `
INSERT INTO workspace_mounts (
    id, org_id, worker_group_id, project_id, environment_id, region_id,
    worker_instance_id, worker_epoch, workspace_id, materialized_version_id,
    runtime_instance_id, state, mounted_at
	) VALUES ($1, $2, $3, $4, $5, 'us-east-1', $6, 1, $7, $8, $9, 'mounted', now())`,
		mountID, product.orgID, poolFixture.group.ID, product.projectID,
		product.environmentID, workerID, workspaceID, headVersionID, runtimeID,
	); err != nil {
		t.Fatal(err)
	}

	request := workspaceDeleteRequest{
		OrgID: product.orgID, ProjectID: product.projectID,
		EnvironmentID: product.environmentID, WorkspaceID: workspaceID,
		IdempotencyKey: "workspace-delete-cleanup",
	}
	deleted, err := product.server.deleteWorkspace(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Replayed || deleted.WorkspaceID != workspaceID {
		t.Fatalf("delete result = %+v", deleted)
	}

	assertCleanup := func() {
		t.Helper()
		var workspaceState db.WorkspaceState
		var mountState, desiredState, desiredReason string
		var desiredVersion int64
		var stoppedAtValid bool
		if err := product.pool.QueryRow(t.Context(), `
SELECT workspaces.state, workspace_mounts.state,
       workspace_mounts.stopped_at IS NOT NULL,
       runtime_instances.desired_state, runtime_instances.desired_version,
       runtime_instances.desired_reason
  FROM workspaces
  JOIN workspace_mounts ON workspace_mounts.workspace_id = workspaces.id
  JOIN runtime_instances ON runtime_instances.id = workspace_mounts.runtime_instance_id
 WHERE workspaces.id = $1 AND workspace_mounts.id = $2`,
			workspaceID, mountID,
		).Scan(
			&workspaceState, &mountState, &stoppedAtValid,
			&desiredState, &desiredVersion, &desiredReason,
		); err != nil {
			t.Fatal(err)
		}
		if workspaceState != db.WorkspaceStateDeleting || mountState != "unmounting" ||
			!stoppedAtValid || desiredState != "closed" || desiredVersion != 2 ||
			desiredReason != "workspace_deleted" {
			t.Fatalf(
				"workspace=%s mount=%s stopped=%t runtime=%s version=%d reason=%s",
				workspaceState, mountState, stoppedAtValid,
				desiredState, desiredVersion, desiredReason,
			)
		}
	}
	assertCleanup()

	replayed, err := product.server.deleteWorkspace(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.WorkspaceID != workspaceID {
		t.Fatalf("replayed delete result = %+v", replayed)
	}
	assertCleanup()
}
