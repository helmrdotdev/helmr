-- name: FindCancellationTarget :one
SELECT id
  FROM runs
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND public_id = sqlc.arg(public_id);

-- name: ListCancellationLineage :many
WITH RECURSIVE lineage AS (
    SELECT runs.id,
           runs.parent_run_id,
           runs.parent_owns_lifecycle,
           0 AS depth,
           ARRAY[runs.id] AS path,
           false AS cycle,
           sqlc.arg(max_depth)::integer AS max_depth
      FROM runs
     WHERE runs.id = sqlc.arg(target_id)
    UNION ALL
    SELECT parent.id,
           parent.parent_run_id,
           parent.parent_owns_lifecycle,
           lineage.depth + 1,
           lineage.path || parent.id,
           parent.id = ANY(lineage.path),
           lineage.max_depth
      FROM lineage
      JOIN runs AS parent
        ON parent.id = lineage.parent_run_id
     WHERE lineage.parent_owns_lifecycle IS TRUE
       AND NOT lineage.cycle
       AND lineage.depth < lineage.max_depth
)
SELECT id, depth, cycle
  FROM lineage
 ORDER BY depth DESC;

-- name: ListOwnedCancellationRuns :many
WITH RECURSIVE owned AS (
    SELECT runs.id,
           0 AS depth,
           ARRAY[runs.id] AS path,
           false AS cycle,
           sqlc.arg(max_depth)::integer AS max_depth
      FROM runs
     WHERE runs.id = sqlc.arg(target_id)
       AND runs.org_id = sqlc.arg(org_id)
       AND runs.project_id = sqlc.arg(project_id)
       AND runs.environment_id = sqlc.arg(environment_id)
    UNION ALL
    SELECT child.id,
           owned.depth + 1,
           owned.path || child.id,
           child.id = ANY(owned.path),
           owned.max_depth
      FROM owned
      JOIN runs AS child
        ON child.parent_run_id = owned.id
       AND child.parent_owns_lifecycle IS TRUE
       AND child.org_id = sqlc.arg(org_id)
       AND child.project_id = sqlc.arg(project_id)
       AND child.environment_id = sqlc.arg(environment_id)
       AND child.status IN ('queued', 'running', 'waiting', 'retry_delayed', 'cancel_requested')
     WHERE NOT owned.cycle
       AND owned.depth < owned.max_depth
)
SELECT id, depth, cycle
  FROM owned
 ORDER BY depth, id
 LIMIT sqlc.arg(limit_count);

-- name: LockCancellationActors :many
SELECT actors.id
  FROM runs
  JOIN actors
    ON actors.id = runs.actor_id
   AND actors.environment_id = runs.environment_id
 WHERE runs.id = ANY(sqlc.arg(run_ids)::uuid[])
   AND runs.org_id = sqlc.arg(org_id)
   AND runs.project_id = sqlc.arg(project_id)
   AND runs.environment_id = sqlc.arg(environment_id)
 ORDER BY actors.id
 FOR UPDATE OF actors;

-- name: LockCancellationRun :one
SELECT id,
       public_id,
       parent_run_id,
       parent_owns_lifecycle,
       environment_id,
       workspace_id,
       actor_id,
       status,
       current_attempt_number,
       current_run_lease_id,
       state_version
  FROM runs
 WHERE id = sqlc.arg(id)
   AND org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
 FOR UPDATE;

-- name: LockCancellationWorkspaces :many
SELECT id
  FROM workspaces
 WHERE id IN (
       SELECT workspace_id
         FROM runs
        WHERE id = ANY(sqlc.arg(run_ids)::uuid[])
 )
 ORDER BY id
 FOR UPDATE;

-- name: LockCancellationAttempts :many
SELECT run_attempts.run_id
  FROM run_attempts
  JOIN runs
    ON runs.id = run_attempts.run_id
   AND runs.current_attempt_number = run_attempts.number
   AND runs.workspace_id = run_attempts.workspace_id
 WHERE runs.id = ANY(sqlc.arg(run_ids)::uuid[])
 ORDER BY array_position(sqlc.arg(run_ids)::uuid[], run_attempts.run_id),
          run_attempts.number
 FOR UPDATE OF run_attempts;

-- name: LockCancellationRuntimes :many
WITH target_runtimes AS (
    SELECT run_leases.runtime_instance_id AS id
      FROM runs
      JOIN run_leases
        ON run_leases.id = runs.current_run_lease_id
       AND run_leases.run_id = runs.id
     WHERE runs.id = ANY(sqlc.arg(cancel_ids)::uuid[])
    UNION
    SELECT runtime_instances.id
      FROM runtime_instances
     WHERE runtime_instances.reserved_run_id = ANY(sqlc.arg(cancel_ids)::uuid[])
    UNION
    SELECT run_waits.handoff_runtime_instance_id
      FROM run_waits
     WHERE run_waits.run_id = ANY(sqlc.arg(run_ids)::uuid[])
       AND run_waits.handoff_runtime_instance_id IS NOT NULL
       AND run_waits.suspension_state IN (
           'hot', 'checkpointing', 'parked', 'resume_pending', 'resuming'
       )
)
SELECT runtime_instances.id
  FROM runtime_instances
  JOIN target_runtimes ON target_runtimes.id = runtime_instances.id
 ORDER BY runtime_instances.id
 FOR UPDATE OF runtime_instances;

-- name: LockCancellationRunLeases :many
SELECT run_leases.id
  FROM runs
  JOIN run_leases
    ON run_leases.id = runs.current_run_lease_id
   AND run_leases.run_id = runs.id
 WHERE runs.id = ANY(sqlc.arg(run_ids)::uuid[])
 ORDER BY run_leases.id
 FOR UPDATE OF run_leases;

-- name: LockCancellationMounts :many
SELECT id
  FROM workspace_mounts
 WHERE runtime_instance_id = ANY(sqlc.arg(runtime_ids)::uuid[])
   AND state IN ('mounting', 'mounted', 'unmounting')
 ORDER BY id
 FOR UPDATE;

-- name: LockCancellationWorkspaceLeases :many
SELECT id
  FROM workspace_leases
 WHERE owner_run_lease_id = ANY(sqlc.arg(run_lease_ids)::uuid[])
   AND state IN ('active', 'releasing')
 ORDER BY id
 FOR UPDATE;

-- name: LockCancellationWaits :many
SELECT id,
       run_id,
       workspace_id,
       child_run_id,
       condition_state,
       suspension_state,
       expected_run_state_version,
       attempt_number,
       current_run_lease_id,
       prior_run_lease_id,
       suspend_checkpoint_id,
       resume_request_version,
       handoff_runtime_instance_id,
       handoff_workspace_mount_id,
       base_workspace_version_id
  FROM run_waits
 WHERE (
       run_id = ANY(sqlc.arg(run_ids)::uuid[])
       OR child_run_id = ANY(sqlc.arg(cancel_ids)::uuid[])
 )
   AND suspension_state IN (
       'hot', 'checkpointing', 'parked', 'resume_pending', 'resuming'
   )
 ORDER BY array_position(sqlc.arg(run_ids)::uuid[], run_id), id
 FOR UPDATE;

-- name: LockCancellationCheckpoints :many
SELECT id
  FROM run_checkpoints
 WHERE run_id = ANY(sqlc.arg(run_ids)::uuid[])
   AND state IN ('creating', 'ready')
 ORDER BY array_position(sqlc.arg(run_ids)::uuid[], run_id), id
 FOR UPDATE;
