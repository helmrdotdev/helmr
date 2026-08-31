package db_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

type workspaceExecInterleavedPlanStore struct {
	*db.Queries
	afterExec func()
}

func (s workspaceExecInterleavedPlanStore) ListPendingWorkspaceExecCapacityCandidates(
	ctx context.Context,
	params db.ListPendingWorkspaceExecCapacityCandidatesParams,
) ([]db.ListPendingWorkspaceExecCapacityCandidatesRow, error) {
	rows, err := s.Queries.ListPendingWorkspaceExecCapacityCandidates(ctx, params)
	if err == nil && s.afterExec != nil {
		s.afterExec()
	}
	return rows, err
}

func TestPendingWorkspaceExecCapacityCandidatesExcludeDiscoverableRuntime(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	ids := seedPostgres(t, ctx, pool)
	queries := db.New(pool)

	definitionID := uuid.NewV7()
	workspaceID := uuid.NewV7()
	versionID := uuid.NewV7()
	claimID := uuid.NewV7()
	processID := uuid.NewV7()
	manifest, err := json.Marshal(deployment.SandboxManifest{Resources: deployment.ResourcesManifest{
		MilliCPU:  1000,
		MemoryMiB: 1024,
	}})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO deployment_definitions (
			id, environment_id, deployment_id, kind, declared_id,
			manifest_version, manifest, manifest_digest, artifact_id
		) VALUES (
			$1, $2, $3, 'sandbox', 'capacity-workspace',
			0, $4::jsonb, decode(repeat('41', 32), 'hex'), $5
		)
	`, definitionID, ids.environmentID, ids.deploymentID, manifest, ids.workspaceImageArtifactID)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO workspaces (
			id, environment_id, region_id, sandbox_declared_id,
			deployment_definition_id, head_version_id
		) VALUES ($1, $2, $3, 'capacity-workspace', $4, $5)
	`, workspaceID, ids.environmentID, dbtest.DefaultRegionID, definitionID, versionID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO workspace_versions (
			id, environment_id, workspace_id, kind, content_digest, state,
			ownership_generation, writer_generation, published_at
		) VALUES (
			$1, $2, $3, 'system',
			'sha256:d2ce8eece19cb4f6db14e37f6d986da7eec7f654f3b91c5c706e9d74e7d2bc96',
			'committed', 0, 0, now()
		)
	`, versionID, ids.environmentID, workspaceID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO idempotency_claims (
			id, environment_id, operation, slot_hash,
			request_fingerprint, accepted_at, expires_at
		) VALUES (
			$1, $2, 'workspace.exec', decode(repeat('42', 32), 'hex'),
			decode(repeat('43', 32), 'hex'), now(), now() + interval '30 days'
		)
	`, claimID, ids.environmentID)
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO workspace_processes (
			id, org_id, project_id, environment_id, workspace_id,
			base_version_id, restore_desired_state, request, claim_id,
			created_by_subject_type, created_by_subject_id
		) VALUES (
			$1, $2, $3, $4, $5, $6, 'active', '{}'::jsonb, $7, 'test', 'capacity'
		)
	`, processID, ids.orgID, ids.projectID, ids.environmentID, workspaceID, versionID, claimID)

	list := func(q *db.Queries, regionID string) []db.ListPendingWorkspaceExecCapacityCandidatesRow {
		t.Helper()
		rows, err := q.ListPendingWorkspaceExecCapacityCandidates(ctx, db.ListPendingWorkspaceExecCapacityCandidatesParams{
			RegionID: regionID,
			RowLimit: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	requireVisible := func(q *db.Queries, visible bool, label string) {
		t.Helper()
		rows := list(q, dbtest.DefaultRegionID)
		if visible && (len(rows) != 1 || rows[0].ProcessID.Bytes != processID) {
			t.Fatalf("%s rows = %+v, want process %s", label, rows, processID)
		}
		if visible && rows[0].WorkspaceManifestVersion != deployment.DeploymentPlanFormatVersion {
			t.Fatalf("%s manifest version = %d, want %d", label, rows[0].WorkspaceManifestVersion, deployment.DeploymentPlanFormatVersion)
		}
		if !visible && len(rows) != 0 {
			t.Fatalf("%s rows = %+v, want none", label, rows)
		}
		if visible && len(rows[0].AccountedPoolIds) != 0 {
			t.Fatalf("%s accounted pools = %+v, want none", label, rows[0].AccountedPoolIds)
		}
	}
	requireAccounted := func(q *db.Queries, label string, poolIDs ...uuid.UUID) {
		t.Helper()
		rows := list(q, dbtest.DefaultRegionID)
		if len(rows) != 1 || rows[0].ProcessID.Bytes != processID {
			t.Fatalf("%s rows = %+v, want process %s", label, rows, processID)
		}
		if len(rows[0].AccountedPoolIds) != len(poolIDs) {
			t.Fatalf("%s accounted pools = %+v, want %v", label, rows[0].AccountedPoolIds, poolIDs)
		}
		for index, poolID := range poolIDs {
			if !rows[0].AccountedPoolIds[index].Valid || rows[0].AccountedPoolIds[index].Bytes != poolID {
				t.Fatalf("%s accounted pools = %+v, want %v", label, rows[0].AccountedPoolIds, poolIDs)
			}
		}
	}
	requireVisible(queries, true, "eligible")
	if rows := list(queries, "other-region"); len(rows) != 0 {
		t.Fatalf("wrong Region rows = %+v, want none", rows)
	}

	dbtest.MustExec(t, ctx, pool, `
		UPDATE workspace_processes
		   SET state = 'failed', terminal_at = now(), terminal_reason_code = 'test'
		 WHERE id = $1
	`, processID)
	requireVisible(queries, false, "non-pending process")
	dbtest.MustExec(t, ctx, pool, `
		UPDATE workspace_processes
		   SET state = 'pending', terminal_at = NULL, terminal_reason_code = NULL
		 WHERE id = $1
	`, processID)

	dbtest.MustExec(t, ctx, pool, `UPDATE workspaces SET desired_state = 'stopped' WHERE id = $1`, workspaceID)
	requireVisible(queries, true, "stopped Workspace")
	dbtest.MustExec(t, ctx, pool, `UPDATE workspaces SET desired_state = 'deleted' WHERE id = $1`, workspaceID)
	requireVisible(queries, false, "deleted desired state")
	dbtest.MustExec(t, ctx, pool, `UPDATE workspaces SET desired_state = 'active' WHERE id = $1`, workspaceID)
	dbtest.MustExec(t, ctx, pool, `UPDATE workspaces SET dirty_state = 'dirty' WHERE id = $1`, workspaceID)
	requireVisible(queries, false, "dirty Workspace")
	dbtest.MustExec(t, ctx, pool, `UPDATE workspaces SET dirty_state = 'clean' WHERE id = $1`, workspaceID)
	dbtest.MustExec(t, ctx, pool, `
		UPDATE workspaces SET state = 'deleting', desired_state = 'deleted' WHERE id = $1
	`, workspaceID)
	requireVisible(queries, false, "non-active Workspace")
	dbtest.MustExec(t, ctx, pool, `
		UPDATE workspaces SET state = 'active', desired_state = 'active' WHERE id = $1
	`, workspaceID)

	for _, authority := range []struct {
		name   string
		column string
	}{
		{name: "Run owner", column: "owner_run_id"},
		{name: "Session owner", column: "owner_session_id"},
	} {
		t.Run(authority.name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			dbtest.MustExec(t, ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
			dbtest.MustExec(t, ctx, tx, `UPDATE workspaces SET `+authority.column+` = $2 WHERE id = $1`, workspaceID, uuid.NewV7())
			requireVisible(db.New(tx), false, authority.name)
		})
	}

	t.Run("head differs from process base", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		dbtest.MustExec(t, ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
		dbtest.MustExec(t, ctx, tx, `UPDATE workspaces SET head_version_id = $2 WHERE id = $1`, workspaceID, uuid.NewV7())
		requireVisible(db.New(tx), false, "mismatched head")
	})

	if rows := list(queries, dbtest.DefaultRegionID); len(rows) != 1 || rows[0].ProcessID.Bytes != processID {
		t.Fatalf("eligible rows = %+v, want process %s", rows, processID)
	}

	workerID := uuid.NewV7()
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, worker_pool_id, state,
			current_epoch, current_service_id, runtime_identity_id,
			substrate_format, substrate_contract,
			epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
			per_vm_cpu_millis, per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes,
			max_vm_slots, max_runtime_starts, cpu_environment,
			cpu_environment_digest, observed_at, epoch_started_at, activated_at
		) VALUES (
			$1, $2, $3, $4, 'active', 1, $5, $6,
			'ext4', 'helmr.substrate.ext4.v0',
			8000, 17179869184, 274877906944,
			4000, 8589934592, 34359738368,
			8, 1, '{}'::jsonb, $7, now(), now(), now()
		)
	`, workerID, "capacity-"+workerID.String(), dbtest.DefaultWorkerGroupID,
		dbtest.DefaultWorkerPoolID, uuid.NewV7(), dbtest.DefaultRuntimeID,
		dbtest.DefaultCPUConfigID)
	runtimeID := uuid.NewV7()
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO runtime_instances (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, runtime_identity_id, deployment_definition_id,
			worker_epoch, vm_vcpu_count, cpu_config_digest,
			reserved_cpu_millis, reserved_memory_bytes,
			reserved_guest_ephemeral_disk_bytes, reserved_execution_slots,
			workspace_id, desired_reason
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9,
			1, 1, $10, 1000, 1073741824, 34359738368, 1,
			$11, 'workspace-exec-capacity-test'
		)
	`, runtimeID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID,
		ids.environmentID, dbtest.DefaultRegionID, workerID, dbtest.DefaultRuntimeID,
		definitionID, dbtest.DefaultCPUConfigID, workspaceID)
	dbtest.MustExec(t, ctx, pool, `
		UPDATE runtime_instances
		   SET reserved_process_id = $2,
		       reserved_workspace_version_id = $3,
		       reservation_expires_at = now() + interval '5 minutes'
		 WHERE id = $1
	`, runtimeID, processID, versionID)
	requireAccounted(queries, "same-process live Runtime", uuid.MustParse(dbtest.DefaultWorkerPoolID))

	leaseProcessClaimID := uuid.NewV7()
	leaseProcessID := uuid.NewV7()
	mountID := uuid.NewV7()
	leaseID := uuid.NewV7()
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO workspace_mounts (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, worker_epoch, workspace_id, materialized_version_id,
			runtime_instance_id, state, mounted_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, 1, $8, $9, $10, 'mounted', now()
		)
	`, mountID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID,
		ids.environmentID, dbtest.DefaultRegionID, workerID, workspaceID, versionID, runtimeID)
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO idempotency_claims (
			id, environment_id, operation, slot_hash,
			request_fingerprint, accepted_at, expires_at
		) VALUES (
			$1, $2, 'workspace.exec', decode(repeat('44', 32), 'hex'),
			decode(repeat('45', 32), 'hex'), now(), now() + interval '30 days'
		)
	`, leaseProcessClaimID, ids.environmentID)
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO workspace_processes (
			id, org_id, project_id, environment_id, workspace_id, base_version_id,
			restore_desired_state, region_id, worker_group_id, worker_instance_id,
			worker_epoch, runtime_instance_id, workspace_mount_id, state, request,
			claim_id, created_by_subject_type, created_by_subject_id,
			terminal_at, terminal_reason_code
		) VALUES (
			$1, $2, $3, $4, $5, $6, 'active', $7, $8, $9,
			1, $10, $11, 'failed', '{}'::jsonb, $12, 'test', 'lease-owner',
			now(), 'test'
		)
	`, leaseProcessID, ids.orgID, ids.projectID, ids.environmentID, workspaceID,
		versionID, dbtest.DefaultRegionID, dbtest.DefaultWorkerGroupID, workerID,
		runtimeID, mountID, leaseProcessClaimID)
	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO workspace_leases (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
			workspace_mount_id, state, owner_process_id, base_version_id,
			ownership_generation, writer_generation, mount_fencing_generation,
			fencing_token_hash, expires_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, 1, $8, $9, $10, 'releasing', $11, $12,
			1, 1, 1, 'capacity-test', now() + interval '5 minutes'
		)
	`, leaseID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID,
		ids.environmentID, dbtest.DefaultRegionID, workerID, runtimeID, workspaceID,
		mountID, leaseProcessID, versionID)

	dbtest.MustExec(t, ctx, pool, `
		UPDATE runtime_instances
		   SET desired_state = 'closed', desired_version = 2,
		       observed_state = 'closed', observed_version = 1,
		       observed_desired_version = 2,
		       reserved_process_id = NULL, reserved_workspace_version_id = NULL,
		       reservation_expires_at = NULL,
		       reclaimed_at = now(), reclaim_evidence = '{}'::jsonb,
		       terminal_at = now(), terminal_reason_code = 'closed'
		 WHERE id = $1
	`, runtimeID)
	requireAccounted(queries, "releasing lease after Runtime reclamation", uuid.MustParse(dbtest.DefaultWorkerPoolID))
	dbtest.MustExec(t, ctx, pool, `UPDATE workspace_leases SET state = 'active' WHERE id = $1`, leaseID)
	requireAccounted(queries, "active lease after Runtime reclamation", uuid.MustParse(dbtest.DefaultWorkerPoolID))
	dbtest.MustExec(t, ctx, pool, `
		UPDATE workspace_leases
		   SET state = 'released', released_at = now(), terminal_at = now()
		 WHERE id = $1
	`, leaseID)
	if rows := list(queries, dbtest.DefaultRegionID); len(rows) != 1 || rows[0].ProcessID.Bytes != processID {
		t.Fatalf("rows after Runtime reclamation = %+v, want process %s", rows, processID)
	}

	t.Run("full Plan remains blocked when Runtime is reclaimed after Exec discovery", func(t *testing.T) {
		interleavedRuntimeID := uuid.NewV7()
		dbtest.MustExec(t, ctx, pool, `
			INSERT INTO runtime_instances (
				id, org_id, worker_group_id, project_id, environment_id, region_id,
				worker_instance_id, runtime_identity_id, deployment_definition_id,
				worker_epoch, vm_vcpu_count, cpu_config_digest,
				reserved_cpu_millis, reserved_memory_bytes,
				reserved_guest_ephemeral_disk_bytes, reserved_execution_slots,
				workspace_id, reserved_process_id, reserved_workspace_version_id,
				reservation_expires_at, desired_reason
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9,
				1, 1, $10, 1000, 1073741824, 34359738368, 1,
				$11, $12, $13, now() + interval '5 minutes',
				'workspace-exec-capacity-interleaving-test'
			)
		`, interleavedRuntimeID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID,
			ids.environmentID, dbtest.DefaultRegionID, workerID, dbtest.DefaultRuntimeID,
			definitionID, dbtest.DefaultCPUConfigID, workspaceID, processID, versionID)

		store := workspaceExecInterleavedPlanStore{
			Queries: queries,
			afterExec: func() {
				dbtest.MustExec(t, ctx, pool, `
					UPDATE runtime_instances
					   SET desired_state = 'closed', desired_version = 2,
					       observed_state = 'closed', observed_version = 1,
					       observed_desired_version = 2,
					       reserved_process_id = NULL, reserved_workspace_version_id = NULL,
					       reservation_expires_at = NULL,
					       reclaimed_at = now(), reclaim_evidence = '{}'::jsonb,
					       terminal_at = now(), terminal_reason_code = 'closed'
					 WHERE id = $1
				`, interleavedRuntimeID)
			},
		}
		plan, err := capacity.Plan(ctx, store, dbtest.DefaultWorkerGroupID, capacity.PlanRequest{
			Pools: []capacity.PoolRequest{{
				PoolID:               dbtest.DefaultWorkerPoolID,
				MaxAdditionalWorkers: 1,
			}},
		}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Pools) != 1 {
			t.Fatalf("pools = %+v, want one", plan.Pools)
		}
		poolPlan := plan.Pools[0]
		if !poolPlan.ScaleInBlocked || poolPlan.CompatibleQueuedItems != 0 || poolPlan.RecommendedAdditionalWorkers != 0 {
			t.Fatalf("pool plan after interleaved reclaim = %+v", poolPlan)
		}
		if !plan.Complete || poolPlan.Saturated {
			t.Fatalf("plan after interleaved reclaim = %+v", plan)
		}
	})

	t.Run("full Plan keeps candidate demand when Runtime is reserved after Exec discovery", func(t *testing.T) {
		interleavedRuntimeID := uuid.NewV7()
		store := workspaceExecInterleavedPlanStore{
			Queries: queries,
			afterExec: func() {
				dbtest.MustExec(t, ctx, pool, `
					INSERT INTO runtime_instances (
						id, org_id, worker_group_id, project_id, environment_id, region_id,
						worker_instance_id, runtime_identity_id, deployment_definition_id,
						worker_epoch, vm_vcpu_count, cpu_config_digest,
						reserved_cpu_millis, reserved_memory_bytes,
						reserved_guest_ephemeral_disk_bytes, reserved_execution_slots,
						workspace_id, reserved_process_id, reserved_workspace_version_id,
						reservation_expires_at, desired_reason
					) VALUES (
						$1, $2, $3, $4, $5, $6, $7, $8, $9,
						1, 1, $10, 1000, 1073741824, 34359738368, 1,
						$11, $12, $13, now() + interval '5 minutes',
						'workspace-exec-capacity-reverse-interleaving-test'
					)
				`, interleavedRuntimeID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID,
					ids.environmentID, dbtest.DefaultRegionID, workerID, dbtest.DefaultRuntimeID,
					definitionID, dbtest.DefaultCPUConfigID, workspaceID, processID, versionID)
			},
		}
		plan, err := capacity.Plan(ctx, store, dbtest.DefaultWorkerGroupID, capacity.PlanRequest{
			Pools: []capacity.PoolRequest{{
				PoolID:               dbtest.DefaultWorkerPoolID,
				MaxAdditionalWorkers: 1,
			}},
		}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Pools) != 1 {
			t.Fatalf("pools = %+v, want one", plan.Pools)
		}
		poolPlan := plan.Pools[0]
		if poolPlan.CompatibleQueuedItems != 1 || poolPlan.ScaleInBlocked {
			t.Fatalf("pool plan after interleaved reservation = %+v", poolPlan)
		}
	})
}
