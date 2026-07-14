-- name: ListFleetRunDemand :many
WITH target_group AS (
    SELECT worker_groups.id, worker_groups.region_id FROM worker_groups
     WHERE worker_groups.id = sqlc.arg(worker_group_id) AND worker_groups.allows_run
), demand AS (
    SELECT 'queued'::text AS demand_state,
           target_group.id AS compatibility_key,
           runs.requested_milli_cpu AS milli_cpu,
           CAST(runs.requested_memory_mib * 1048576 AS bigint) AS memory_bytes,
           CAST(runs.requested_disk_mib * 1048576 AS bigint) AS workload_disk_bytes,
           0::bigint AS scratch_bytes,
           runs.requested_execution_slots::bigint AS vm_slots,
           count(*)::bigint AS demand_count
      FROM runs
      JOIN workspaces ON workspaces.org_id = runs.org_id
                     AND workspaces.project_id = runs.project_id
                     AND workspaces.environment_id = runs.environment_id
                     AND workspaces.id = runs.workspace_id
      JOIN target_group ON target_group.region_id = workspaces.region_id
     WHERE runs.status = 'queued' AND runs.current_run_lease_id IS NULL
       AND runs.queue_timestamp <= now()
       AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
     GROUP BY target_group.id, runs.requested_milli_cpu,
              runs.requested_memory_mib, runs.requested_disk_mib,
              runs.requested_execution_slots
    UNION ALL
    SELECT 'active'::text,
           target_group.id,
           run_leases.requested_cpu_millis,
           run_leases.requested_memory_bytes,
           run_leases.requested_workload_disk_bytes,
           run_leases.requested_scratch_bytes,
           run_leases.requested_execution_slots::bigint,
           count(*)::bigint
      FROM run_leases
      JOIN target_group ON target_group.id = run_leases.worker_group_id
     WHERE run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing')
     GROUP BY target_group.id, run_leases.requested_cpu_millis,
              run_leases.requested_memory_bytes, run_leases.requested_workload_disk_bytes,
              run_leases.requested_scratch_bytes, run_leases.requested_execution_slots
)
SELECT * FROM demand ORDER BY demand_state, compatibility_key, milli_cpu, memory_bytes,
                              workload_disk_bytes, scratch_bytes, vm_slots;

-- name: CountUncertifiedRunLaunchAttestations :one
SELECT count(*)::bigint
  FROM worker_groups
 WHERE worker_groups.id = sqlc.arg(worker_group_id)
   AND worker_groups.state = 'active'
   AND worker_groups.allows_run
   AND worker_groups.launch_attestation_fingerprint IS NOT NULL
   AND NOT EXISTS (
       SELECT 1
         FROM worker_instances
        WHERE worker_instances.worker_group_id = worker_groups.id
          AND worker_instances.attestation_fingerprint = worker_groups.launch_attestation_fingerprint
          AND worker_instances.supports_run
          AND worker_instances.runtime_identity_id IS NOT NULL
          AND worker_instances.certified_at IS NOT NULL
   );

-- name: GetFleetCooldown :one
SELECT last_scale_out_at, last_scale_in_at
  FROM worker_groups
 WHERE id = sqlc.arg(worker_group_id);

-- name: RecordFleetScaleOut :one
UPDATE worker_groups
   SET last_scale_out_at = GREATEST(last_scale_out_at, sqlc.arg(action_at))
 WHERE id = sqlc.arg(worker_group_id)
RETURNING id;

-- name: RecordFleetScaleIn :one
UPDATE worker_groups
   SET last_scale_in_at = GREATEST(last_scale_in_at, sqlc.arg(action_at))
 WHERE id = sqlc.arg(worker_group_id)
RETURNING id;

-- name: GetFleetOldestRunQueueTime :one
SELECT min(runs.queue_timestamp)::timestamptz
  FROM runs
  JOIN workspaces ON workspaces.org_id = runs.org_id
                 AND workspaces.project_id = runs.project_id
                 AND workspaces.environment_id = runs.environment_id
                 AND workspaces.id = runs.workspace_id
  JOIN worker_groups ON worker_groups.id = sqlc.arg(worker_group_id)
                    AND worker_groups.region_id = workspaces.region_id
                    AND worker_groups.allows_run
 WHERE runs.status = 'queued' AND runs.current_run_lease_id IS NULL
   AND runs.queue_timestamp <= now()
   AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now());

-- name: ListFleetBuildDemand :many
WITH target_group AS (
    SELECT worker_groups.id, worker_groups.region_id FROM worker_groups
     WHERE worker_groups.id = sqlc.arg(worker_group_id) AND worker_groups.allows_build
), demand AS (
    SELECT 'queued'::text AS demand_state,
           deployments.build_requested_cpu_millis AS milli_cpu,
           deployments.build_requested_memory_bytes AS memory_bytes,
           deployments.build_requested_workload_disk_bytes AS workload_disk_bytes,
           deployments.build_requested_scratch_bytes AS scratch_bytes,
           deployments.build_requested_build_cache_bytes AS build_cache_bytes,
           deployments.build_requested_artifact_cache_bytes AS artifact_cache_bytes,
           deployments.build_requested_executors::bigint AS build_executors,
           count(*)::bigint AS demand_count
      FROM deployments
      JOIN target_group ON target_group.region_id = deployments.build_region_id
     WHERE deployments.status IN ('queued', 'building')
       AND NOT EXISTS (
           SELECT 1 FROM deployment_build_leases active_lease
            WHERE active_lease.deployment_id = deployments.id
              AND active_lease.state IN ('assigned', 'starting', 'running')
       )
     GROUP BY deployments.build_requested_cpu_millis, deployments.build_requested_memory_bytes,
              deployments.build_requested_workload_disk_bytes, deployments.build_requested_scratch_bytes,
              deployments.build_requested_build_cache_bytes, deployments.build_requested_artifact_cache_bytes,
              deployments.build_requested_executors
    UNION ALL
    SELECT 'active'::text,
           deployment_build_leases.requested_cpu_millis,
           deployment_build_leases.requested_memory_bytes,
           deployment_build_leases.requested_workload_disk_bytes,
           deployment_build_leases.requested_scratch_bytes,
           deployment_build_leases.requested_build_cache_bytes,
           deployment_build_leases.requested_artifact_cache_bytes,
           deployment_build_leases.requested_build_executors::bigint,
           count(*)::bigint
      FROM deployment_build_leases
      JOIN target_group ON target_group.id = deployment_build_leases.worker_group_id
     WHERE deployment_build_leases.state IN ('assigned', 'starting', 'running')
     GROUP BY deployment_build_leases.requested_cpu_millis, deployment_build_leases.requested_memory_bytes,
              deployment_build_leases.requested_workload_disk_bytes, deployment_build_leases.requested_scratch_bytes,
              deployment_build_leases.requested_build_cache_bytes, deployment_build_leases.requested_artifact_cache_bytes,
              deployment_build_leases.requested_build_executors
)
SELECT * FROM demand ORDER BY demand_state, milli_cpu, memory_bytes, workload_disk_bytes,
                              scratch_bytes, build_cache_bytes, artifact_cache_bytes, build_executors;

-- name: GetFleetOldestBuildQueueTime :one
SELECT min(deployments.created_at)::timestamptz
  FROM deployments
  JOIN worker_groups ON worker_groups.id = sqlc.arg(worker_group_id)
                    AND worker_groups.region_id = deployments.build_region_id
                    AND worker_groups.allows_build
 WHERE deployments.status IN ('queued', 'building')
   AND NOT EXISTS (
       SELECT 1 FROM deployment_build_leases active_lease
        WHERE active_lease.deployment_id = deployments.id
          AND active_lease.state IN ('assigned', 'starting', 'running')
   );

-- name: ListFleetWorkers :many
SELECT id, resource_id, state, current_epoch, activated_at, draining_at,
       certified_cpu_millis, certified_memory_bytes, certified_workload_disk_bytes,
       certified_scratch_bytes, certified_build_cache_bytes, certified_artifact_cache_bytes,
       max_vm_slots, max_build_executors
 FROM worker_instances
 WHERE worker_group_id = sqlc.arg(worker_group_id)
   AND state IN ('registering', 'active', 'draining', 'disabled', 'lost')
   AND provider_terminated_at IS NULL
 ORDER BY state, activated_at NULLS FIRST, id;

-- name: MarkFleetWorkerDraining :one
WITH target AS (
    UPDATE worker_instances
       SET state = 'draining', draining_at = COALESCE(draining_at, now()), updated_at = now()
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch IS NOT NULL
       AND worker_instances.state IN ('active', 'draining')
       AND ((sqlc.arg(worker_role)::text = 'run' AND worker_instances.supports_run)
            OR (sqlc.arg(worker_role)::text = 'build' AND worker_instances.supports_build))
    RETURNING *
), idle_mounts AS (
    UPDATE workspace_mounts
       SET state = 'unmounting', stopped_at = COALESCE(stopped_at, now()), updated_at = now()
      FROM target
     WHERE workspace_mounts.worker_instance_id = target.id
       AND workspace_mounts.worker_epoch = target.current_epoch
       AND workspace_mounts.state IN ('mounting', 'mounted')
       AND NOT EXISTS (
           SELECT 1 FROM workspace_leases
            WHERE workspace_leases.workspace_mount_id = workspace_mounts.id
              AND workspace_leases.state IN ('active', 'releasing')
       )
    RETURNING workspace_mounts.id
), idle_runtimes AS (
    UPDATE runtime_instances
       SET desired_state = 'closed', desired_version = desired_version + 1,
           desired_at = now(), desired_reason = 'worker_draining', updated_at = now()
      FROM target
     WHERE runtime_instances.worker_instance_id = target.id
       AND runtime_instances.worker_epoch = target.current_epoch
       AND runtime_instances.reclaimed_at IS NULL
       AND runtime_instances.desired_state <> 'closed'
       AND runtime_instances.observed_state IN ('allocated', 'preparing', 'ready')
       AND NOT EXISTS (
           SELECT 1 FROM run_leases
            WHERE run_leases.runtime_instance_id = runtime_instances.id
              AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing')
       )
       AND NOT EXISTS (
           SELECT 1 FROM workspace_mounts
            WHERE workspace_mounts.runtime_instance_id = runtime_instances.id
              AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
       )
    RETURNING runtime_instances.id
)
SELECT target.id FROM target
 WHERE (SELECT count(*) FROM idle_mounts) >= 0
   AND (SELECT count(*) FROM idle_runtimes) >= 0;

-- name: GetFleetTerminationProof :one
SELECT worker_instances.id, worker_instances.resource_id, worker_instances.state,
       worker_instances.current_epoch,
       (
           (SELECT count(*) FROM run_leases
             WHERE worker_instance_id = worker_instances.id
               AND worker_epoch = worker_instances.current_epoch
               AND state IN ('assigned', 'starting', 'running', 'checkpointing'))
         + (SELECT count(*) FROM deployment_build_leases
             WHERE worker_instance_id = worker_instances.id
               AND worker_epoch = worker_instances.current_epoch
               AND state IN ('assigned', 'starting', 'running'))
         + (SELECT count(*) FROM runtime_instances
             WHERE worker_instance_id = worker_instances.id
               AND worker_epoch = worker_instances.current_epoch
               AND reclaimed_at IS NULL)
         + (SELECT count(*) FROM workspace_mounts
             WHERE worker_instance_id = worker_instances.id
               AND worker_epoch = worker_instances.current_epoch
               AND state IN ('mounting', 'mounted', 'unmounting'))
         + (SELECT count(*) FROM workspace_leases
             WHERE worker_instance_id = worker_instances.id
               AND worker_epoch = worker_instances.current_epoch
               AND state IN ('active', 'releasing'))
         + (SELECT count(*) FROM workspace_processes
             WHERE worker_instance_id = worker_instances.id
               AND worker_epoch = worker_instances.current_epoch
               AND state IN ('starting', 'running', 'closing'))
         + (SELECT count(*) FROM workspace_process_operations
             WHERE claimed_by_worker_instance_id = worker_instances.id
               AND claimed_worker_epoch = worker_instances.current_epoch
               AND state IN ('claimed', 'running'))
         + (SELECT count(*) FROM worker_network_slots
             WHERE worker_instance_id = worker_instances.id
               AND worker_epoch = worker_instances.current_epoch
               AND state IN ('assigned', 'bound', 'reclaiming', 'quarantined'))
       )::bigint AS authority_count,
       (worker_instances.state = 'disabled'
        AND worker_instances.drain_cleanup_fingerprint IS NOT NULL
        AND worker_instances.drain_cleanup_evidence IS NOT NULL
        AND jsonb_typeof(worker_instances.drain_cleanup_evidence) = 'object')::boolean AS local_cleanup_complete,
       (((worker_instances.state = 'lost'
          AND worker_instances.current_epoch IS NOT NULL
          AND worker_instances.lost_at IS NOT NULL)
         OR (worker_instances.state = 'disabled'
             AND worker_instances.current_epoch IS NULL))
        AND NOT EXISTS (
            SELECT 1 FROM worker_instance_credentials
             WHERE worker_instance_credentials.worker_instance_id = worker_instances.id
               AND worker_instance_credentials.revoked_at IS NULL
        ))::boolean AS fenced_for_termination
  FROM worker_instances
 WHERE worker_instances.id = sqlc.arg(worker_instance_id)
   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id);

-- name: ClaimFleetWorkerTermination :one
WITH target AS MATERIALIZED (
    SELECT worker_instances.*
      FROM worker_instances
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.provider_terminated_at IS NULL
       AND worker_instances.state IN ('disabled', 'lost')
     FOR UPDATE OF worker_instances
), proof AS MATERIALIZED (
    SELECT target.*,
           (
               (SELECT count(*) FROM run_leases
                 WHERE worker_instance_id = target.id
                   AND worker_epoch = target.current_epoch
                   AND state IN ('assigned', 'starting', 'running', 'checkpointing'))
             + (SELECT count(*) FROM deployment_build_leases
                 WHERE worker_instance_id = target.id
                   AND worker_epoch = target.current_epoch
                   AND state IN ('assigned', 'starting', 'running'))
             + (SELECT count(*) FROM runtime_instances
                 WHERE worker_instance_id = target.id
                   AND worker_epoch = target.current_epoch
                   AND reclaimed_at IS NULL)
             + (SELECT count(*) FROM workspace_mounts
                 WHERE worker_instance_id = target.id
                   AND worker_epoch = target.current_epoch
                   AND state IN ('mounting', 'mounted', 'unmounting'))
             + (SELECT count(*) FROM workspace_leases
                 WHERE worker_instance_id = target.id
                   AND worker_epoch = target.current_epoch
                   AND state IN ('active', 'releasing'))
             + (SELECT count(*) FROM workspace_processes
                 WHERE worker_instance_id = target.id
                   AND worker_epoch = target.current_epoch
                   AND state IN ('starting', 'running', 'closing'))
             + (SELECT count(*) FROM workspace_process_operations
                 WHERE claimed_by_worker_instance_id = target.id
                   AND claimed_worker_epoch = target.current_epoch
                   AND state IN ('claimed', 'running'))
             + (SELECT count(*) FROM worker_network_slots
                 WHERE worker_instance_id = target.id
                   AND worker_epoch = target.current_epoch
                   AND state IN ('assigned', 'bound', 'reclaiming', 'quarantined'))
           )::bigint AS authority_count,
           (target.state = 'disabled'
            AND target.drain_cleanup_fingerprint IS NOT NULL
            AND target.drain_cleanup_evidence IS NOT NULL
            AND jsonb_typeof(target.drain_cleanup_evidence) = 'object')::boolean AS local_cleanup_complete,
           (((target.state = 'lost'
              AND target.current_epoch IS NOT NULL
              AND target.lost_at IS NOT NULL)
             OR (target.state = 'disabled'
                 AND target.current_epoch IS NULL))
            AND NOT EXISTS (
                SELECT 1 FROM worker_instance_credentials
                 WHERE worker_instance_credentials.worker_instance_id = target.id
                   AND worker_instance_credentials.revoked_at IS NULL
            ))::boolean AS fenced_for_termination
      FROM target
), revoked AS (
    UPDATE worker_instance_credentials
       SET revoked_at = now()
      FROM proof
     WHERE worker_instance_credentials.worker_instance_id = proof.id
       AND worker_instance_credentials.revoked_at IS NULL
       AND proof.authority_count = 0
       AND (proof.local_cleanup_complete OR proof.fenced_for_termination)
    RETURNING worker_instance_credentials.id
), claimed AS (
    UPDATE worker_instances
       SET termination_claimed_at = COALESCE(worker_instances.termination_claimed_at, now()),
           updated_at = now()
      FROM proof
     WHERE worker_instances.id = proof.id
       AND proof.authority_count = 0
       AND (proof.local_cleanup_complete OR proof.fenced_for_termination)
       AND (SELECT count(*) FROM revoked) >= 0
    RETURNING worker_instances.id, worker_instances.resource_id, worker_instances.state,
              worker_instances.current_epoch, proof.authority_count,
              proof.local_cleanup_complete, proof.fenced_for_termination
)
SELECT * FROM claimed;

-- name: ConfirmFleetWorkerProviderTermination :one
UPDATE worker_instances
   SET provider_terminated_at = COALESCE(provider_terminated_at, now()),
       updated_at = now()
 WHERE id = sqlc.arg(worker_instance_id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND resource_id = sqlc.arg(resource_id)
   AND termination_claimed_at IS NOT NULL
   AND state IN ('disabled', 'lost')
RETURNING id;
