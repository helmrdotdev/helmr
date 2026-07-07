-- name: CreateWorkerCommand :one
INSERT INTO worker_commands (
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    run_id,
    run_wait_id,
    run_lease_id,
    worker_instance_id,
    deployment_sandbox_id,
    runtime_instance_id,
    runtime_epoch,
    run_state_version,
    kind,
    payload
) VALUES (
    sqlc.arg(org_id),
    sqlc.arg(worker_group_id),
    sqlc.arg(project_id),
    sqlc.arg(environment_id),
    sqlc.arg(run_id),
    sqlc.arg(run_wait_id),
    sqlc.arg(run_lease_id),
    sqlc.arg(worker_instance_id),
    sqlc.narg(deployment_sandbox_id)::uuid,
    sqlc.narg(runtime_instance_id)::uuid,
    sqlc.narg(runtime_epoch)::bigint,
    sqlc.arg(run_state_version),
    sqlc.arg(kind),
    COALESCE(sqlc.arg(payload)::jsonb, '{}'::jsonb)
)
RETURNING *;

-- name: ClaimWorkerCommands :many
WITH claimable AS (
    SELECT worker_commands.id
      FROM worker_commands
      JOIN worker_groups
        ON worker_groups.id = worker_commands.worker_group_id
       AND worker_groups.state IN ('active', 'draining')
      JOIN worker_instances
        ON worker_instances.id = worker_commands.worker_instance_id
       AND worker_instances.worker_group_id = worker_commands.worker_group_id
     WHERE worker_commands.worker_group_id = sqlc.arg(worker_group_id)
       AND delivered_at IS NULL
       AND acknowledged_at IS NULL
       AND (delivery_locked_until IS NULL OR delivery_locked_until < now())
     ORDER BY worker_commands.id ASC
     LIMIT sqlc.arg(row_limit)
     FOR UPDATE SKIP LOCKED
),
updated AS (
    UPDATE worker_commands
       SET delivery_locked_until = now() + sqlc.arg(lease_duration)::interval,
           delivery_attempts = worker_commands.delivery_attempts + 1,
           updated_at = now()
      FROM claimable
     WHERE worker_commands.id = claimable.id
    RETURNING worker_commands.*
)
SELECT *
  FROM updated
 ORDER BY id ASC;

-- name: MarkWorkerCommandDelivered :one
UPDATE worker_commands
   SET delivered_at = COALESCE(delivered_at, now()),
       delivery_locked_until = NULL,
       updated_at = now()
 WHERE worker_commands.id = sqlc.arg(id)
   AND worker_commands.org_id = sqlc.arg(org_id)
   AND worker_commands.worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_commands.delivered_at IS NULL
RETURNING *;

-- name: MarkWorkerCommandDeliveryFailed :exec
UPDATE worker_commands
   SET delivery_locked_until = now() + sqlc.arg(retry_after)::interval,
       last_delivery_error = sqlc.arg(last_delivery_error),
       updated_at = now()
 WHERE worker_commands.id = sqlc.arg(id)
   AND worker_commands.org_id = sqlc.arg(org_id)
   AND worker_commands.worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_commands.delivered_at IS NULL
   AND worker_commands.acknowledged_at IS NULL;

-- name: AcceptWorkerCommand :one
UPDATE worker_commands
   SET accepted_at = COALESCE(accepted_at, now()),
       updated_at = now()
 WHERE worker_commands.worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_commands.id = sqlc.arg(id)
   AND worker_commands.worker_group_id = sqlc.arg(worker_group_id)
   AND EXISTS (
       SELECT 1
         FROM worker_instances
         JOIN worker_groups
           ON worker_groups.id = worker_commands.worker_group_id
          AND worker_groups.state IN ('active', 'draining')
        WHERE worker_instances.id = worker_commands.worker_instance_id
          AND worker_instances.worker_group_id = worker_commands.worker_group_id
          AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
   )
   AND worker_commands.acknowledged_at IS NULL
RETURNING *;

-- name: ListWorkerCommandsAfter :many
SELECT *
  FROM worker_commands
 WHERE worker_commands.worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_commands.worker_group_id = sqlc.arg(worker_group_id)
   AND EXISTS (
       SELECT 1
         FROM worker_instances
         JOIN worker_groups
           ON worker_groups.id = worker_commands.worker_group_id
          AND worker_groups.state IN ('active', 'draining')
        WHERE worker_instances.id = worker_commands.worker_instance_id
          AND worker_instances.worker_group_id = worker_commands.worker_group_id
          AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
   )
   AND worker_commands.id > sqlc.arg(after_id)
   AND worker_commands.acknowledged_at IS NULL
 ORDER BY worker_commands.id ASC
 LIMIT sqlc.arg(limit_count);

-- name: AcknowledgeWorkerCommand :one
WITH target AS MATERIALIZED (
    SELECT worker_commands.*
      FROM worker_commands
     WHERE worker_commands.worker_instance_id = sqlc.arg(worker_instance_id)
       AND worker_commands.id = sqlc.arg(id)
       AND worker_commands.worker_group_id = sqlc.arg(worker_group_id)
       AND EXISTS (
           SELECT 1
             FROM worker_instances
             JOIN worker_groups
               ON worker_groups.id = worker_commands.worker_group_id
              AND worker_groups.state IN ('active', 'draining')
            WHERE worker_instances.id = worker_commands.worker_instance_id
              AND worker_instances.worker_group_id = worker_commands.worker_group_id
              AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       )
     FOR UPDATE OF worker_commands
),
already_acknowledged AS MATERIALIZED (
    SELECT *
      FROM target
     WHERE target.acknowledged_at IS NOT NULL
),
eligible_resume AS MATERIALIZED (
    SELECT target.*
      FROM target
      JOIN run_waits
        ON run_waits.org_id = target.org_id
       AND run_waits.worker_group_id = target.worker_group_id
       AND run_waits.run_id = target.run_id
       AND run_waits.id = target.run_wait_id
       AND run_waits.owner_run_lease_id = target.run_lease_id
       AND run_waits.owner_worker_instance_id = target.worker_instance_id
       AND run_waits.owner_runtime_instance_id = target.runtime_instance_id
       AND run_waits.owner_runtime_epoch = target.runtime_epoch
       AND run_waits.owner_run_state_version = target.run_state_version
       AND run_waits.state = 'resuming'
     JOIN runtime_instances
        ON runtime_instances.org_id = target.org_id
       AND runtime_instances.worker_group_id = target.worker_group_id
       AND runtime_instances.id = target.runtime_instance_id
       AND runtime_instances.worker_instance_id = target.worker_instance_id
       AND runtime_instances.runtime_epoch = target.runtime_epoch
       AND runtime_instances.owner_run_id = target.run_id
       AND runtime_instances.owner_run_lease_id = target.run_lease_id
       AND runtime_instances.owner_run_wait_id = target.run_wait_id
       AND runtime_instances.owner_run_state_version = target.run_state_version
       AND runtime_instances.state = 'waiting_hot'
     WHERE target.kind = 'runtime_resume_wait'
       AND target.acknowledged_at IS NULL
     FOR UPDATE OF run_waits, runtime_instances
),
resumed_live_wait AS (
    UPDATE run_waits
       SET released_at = COALESCE(run_waits.released_at, now()),
           state = 'released',
           updated_at = now()
     FROM eligible_resume
     WHERE run_waits.org_id = eligible_resume.org_id
       AND run_waits.worker_group_id = eligible_resume.worker_group_id
       AND run_waits.run_id = eligible_resume.run_id
       AND run_waits.id = eligible_resume.run_wait_id
       AND run_waits.owner_run_lease_id = eligible_resume.run_lease_id
       AND run_waits.owner_worker_instance_id = eligible_resume.worker_instance_id
       AND run_waits.owner_runtime_instance_id = eligible_resume.runtime_instance_id
       AND run_waits.owner_runtime_epoch = eligible_resume.runtime_epoch
       AND run_waits.owner_run_state_version = eligible_resume.run_state_version
       AND run_waits.state = 'resuming'
    RETURNING run_waits.*
),
resumed_runtime_instance AS (
    UPDATE runtime_instances
       SET state = 'running',
           owner_run_id = eligible_resume.run_id,
           owner_run_lease_id = eligible_resume.run_lease_id,
           owner_run_wait_id = NULL,
           owner_run_state_version = eligible_resume.run_state_version,
           running_at = COALESCE(runtime_instances.running_at, now()),
           updated_at = now()
      FROM eligible_resume
      JOIN resumed_live_wait
        ON resumed_live_wait.org_id = eligible_resume.org_id
       AND resumed_live_wait.worker_group_id = eligible_resume.worker_group_id
       AND resumed_live_wait.id = eligible_resume.run_wait_id
     WHERE runtime_instances.org_id = eligible_resume.org_id
       AND runtime_instances.worker_group_id = eligible_resume.worker_group_id
       AND runtime_instances.id = eligible_resume.runtime_instance_id
       AND runtime_instances.worker_instance_id = eligible_resume.worker_instance_id
       AND runtime_instances.runtime_epoch = eligible_resume.runtime_epoch
       AND runtime_instances.owner_run_id = eligible_resume.run_id
       AND runtime_instances.owner_run_lease_id = eligible_resume.run_lease_id
       AND runtime_instances.owner_run_state_version = eligible_resume.run_state_version
       AND runtime_instances.state = 'waiting_hot'
    RETURNING runtime_instances.id
),
stale_resume AS MATERIALIZED (
    SELECT target.*
      FROM target
     WHERE target.kind = 'runtime_resume_wait'
       AND target.acknowledged_at IS NULL
       AND NOT EXISTS (SELECT 1 FROM eligible_resume)
),
active_checkpoint AS MATERIALIZED (
    SELECT target.*
      FROM target
      JOIN run_waits
        ON run_waits.org_id = target.org_id
       AND run_waits.worker_group_id = target.worker_group_id
       AND run_waits.run_id = target.run_id
       AND run_waits.id = target.run_wait_id
       AND run_waits.owner_run_lease_id = target.run_lease_id
       AND run_waits.owner_worker_instance_id = target.worker_instance_id
       AND run_waits.owner_runtime_instance_id = target.runtime_instance_id
       AND run_waits.owner_runtime_epoch = target.runtime_epoch
       AND run_waits.owner_run_state_version = target.run_state_version
       AND run_waits.state IN ('hot_waiting', 'checkpointing')
      JOIN runtime_instances
        ON runtime_instances.org_id = target.org_id
       AND runtime_instances.worker_group_id = target.worker_group_id
       AND runtime_instances.id = target.runtime_instance_id
       AND runtime_instances.worker_instance_id = target.worker_instance_id
       AND runtime_instances.runtime_epoch = target.runtime_epoch
       AND runtime_instances.owner_run_id = target.run_id
       AND runtime_instances.owner_run_lease_id = target.run_lease_id
       AND runtime_instances.owner_run_wait_id = target.run_wait_id
       AND runtime_instances.owner_run_state_version = target.run_state_version
       AND runtime_instances.state IN ('waiting_hot', 'checkpointing')
     WHERE target.kind = 'runtime_checkpoint_wait'
       AND target.acknowledged_at IS NULL
),
stale_checkpoint AS MATERIALIZED (
    SELECT target.*
      FROM target
     WHERE target.kind = 'runtime_checkpoint_wait'
       AND target.acknowledged_at IS NULL
       AND NOT EXISTS (SELECT 1 FROM active_checkpoint)
),
acknowledged AS (
    UPDATE worker_commands
       SET accepted_at = COALESCE(worker_commands.accepted_at, now()),
           completed_at = COALESCE(worker_commands.completed_at, now()),
           acknowledged_at = COALESCE(worker_commands.acknowledged_at, now()),
           delivery_locked_until = NULL,
           updated_at = now()
      FROM target
     WHERE worker_commands.id = target.id
       AND target.acknowledged_at IS NULL
       AND (
           target.kind NOT IN ('runtime_resume_wait', 'runtime_checkpoint_wait')
           OR (
               EXISTS (SELECT 1 FROM resumed_live_wait)
               AND EXISTS (SELECT 1 FROM resumed_runtime_instance)
           )
           OR EXISTS (SELECT 1 FROM stale_resume)
           OR EXISTS (SELECT 1 FROM stale_checkpoint)
       )
    RETURNING worker_commands.*
)
SELECT *
  FROM acknowledged
UNION ALL
SELECT *
  FROM already_acknowledged
LIMIT 1;

-- name: AcknowledgeWorkerCommandForRunWait :one
UPDATE worker_commands
   SET accepted_at = COALESCE(accepted_at, now()),
       completed_at = COALESCE(completed_at, now()),
       acknowledged_at = COALESCE(acknowledged_at, now()),
       delivery_locked_until = NULL,
       updated_at = now()
 WHERE worker_commands.worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_commands.id = sqlc.arg(id)
   AND worker_commands.org_id = sqlc.arg(org_id)
   AND worker_commands.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_commands.run_id = sqlc.arg(run_id)
   AND worker_commands.run_wait_id = sqlc.arg(run_wait_id)
   AND worker_commands.run_lease_id = sqlc.arg(run_lease_id)
   AND worker_commands.kind = sqlc.arg(kind)
   AND (
       worker_commands.acknowledged_at IS NOT NULL
       OR worker_commands.kind <> 'runtime_checkpoint_wait'
       OR EXISTS (
           SELECT 1
             FROM runtime_checkpoints
            WHERE runtime_checkpoints.org_id = worker_commands.org_id
              AND runtime_checkpoints.worker_group_id = worker_commands.worker_group_id
              AND runtime_checkpoints.project_id = worker_commands.project_id
              AND runtime_checkpoints.environment_id = worker_commands.environment_id
              AND runtime_checkpoints.run_id = worker_commands.run_id
              AND runtime_checkpoints.id = sqlc.arg(runtime_checkpoint_id)
              AND runtime_checkpoints.owner_run_wait_id = worker_commands.run_wait_id
              AND runtime_checkpoints.owner_run_lease_id = worker_commands.run_lease_id
              AND runtime_checkpoints.owner_worker_instance_id = worker_commands.worker_instance_id
              AND runtime_checkpoints.owner_runtime_instance_id = worker_commands.runtime_instance_id
              AND runtime_checkpoints.owner_runtime_epoch = worker_commands.runtime_epoch
              AND runtime_checkpoints.created_at >= worker_commands.accepted_at
              AND runtime_checkpoints.state IN ('ready', 'invalid')
       )
   )
   AND EXISTS (
       SELECT 1
         FROM worker_instances
         JOIN worker_groups
           ON worker_groups.id = worker_commands.worker_group_id
          AND worker_groups.state IN ('active', 'draining')
        WHERE worker_instances.id = worker_commands.worker_instance_id
          AND worker_instances.worker_group_id = worker_commands.worker_group_id
          AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
   )
RETURNING *;

-- name: CreateDueLiveRuntimeCheckpointWaitCommandsForOrg :many
WITH due AS (
    SELECT run_waits.*
      FROM run_waits
      JOIN worker_instances ON worker_instances.id = run_waits.owner_worker_instance_id
      JOIN run_leases ON run_leases.org_id = run_waits.org_id
                     AND run_leases.run_id = run_waits.run_id
                     AND run_leases.id = run_waits.owner_run_lease_id
                     AND run_leases.worker_instance_id = run_waits.owner_worker_instance_id
                     AND run_leases.status IN ('leased', 'running')
      JOIN runtime_instances
        ON runtime_instances.org_id = run_waits.org_id
       AND runtime_instances.id = run_waits.owner_runtime_instance_id
       AND runtime_instances.worker_instance_id = run_waits.owner_worker_instance_id
       AND runtime_instances.runtime_epoch = run_waits.owner_runtime_epoch
       AND runtime_instances.owner_run_id = run_waits.run_id
       AND runtime_instances.owner_run_lease_id = run_waits.owner_run_lease_id
       AND runtime_instances.owner_run_wait_id = run_waits.id
       AND runtime_instances.owner_run_state_version = run_waits.owner_run_state_version
       AND runtime_instances.state = 'waiting_hot'
       AND (
           runtime_instances.expires_at IS NULL
           OR runtime_instances.expires_at > now()
       )
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.worker_group_id = sqlc.arg(worker_group_id)
       AND run_waits.state = 'hot_waiting'
       AND run_waits.runtime_checkpoint_due_at IS NOT NULL
       AND (
           run_waits.runtime_checkpoint_due_at <= now()
           OR worker_instances.status = 'draining'
       )
       AND run_waits.owner_run_lease_id IS NOT NULL
       AND run_waits.owner_worker_instance_id IS NOT NULL
       AND run_waits.owner_runtime_instance_id IS NOT NULL
       AND run_waits.owner_runtime_epoch IS NOT NULL
       AND run_waits.owner_run_state_version IS NOT NULL
       AND NOT EXISTS (
           SELECT 1
             FROM waits
            WHERE waits.org_id = run_waits.org_id
              AND waits.id = run_waits.wait_id
              AND waits.state = 'pending'
              AND waits.expires_at IS NOT NULL
              AND waits.expires_at <= now()
       )
       AND NOT EXISTS (
           SELECT 1
             FROM waits
            WHERE waits.org_id = run_waits.org_id
              AND waits.id = run_waits.wait_id
              AND waits.kind = 'timer'
              AND waits.state = 'pending'
              AND waits.completed_after <= now()
       )
       AND NOT EXISTS (
           SELECT 1
             FROM worker_commands
            WHERE worker_commands.org_id = run_waits.org_id
              AND worker_commands.run_wait_id = run_waits.id
              AND worker_commands.kind = 'runtime_checkpoint_wait'
              AND worker_commands.acknowledged_at IS NULL
       )
     ORDER BY run_waits.runtime_checkpoint_due_at ASC, run_waits.id ASC
     LIMIT sqlc.arg(limit_count)
)
INSERT INTO worker_commands (
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    run_id,
    run_wait_id,
    run_lease_id,
    worker_instance_id,
    runtime_instance_id,
    runtime_epoch,
    run_state_version,
    kind,
    payload
)
SELECT due.org_id,
       due.worker_group_id,
       due.project_id,
       due.environment_id,
       due.run_id,
       due.id,
       due.owner_run_lease_id,
       due.owner_worker_instance_id,
       due.owner_runtime_instance_id,
       due.owner_runtime_epoch,
       due.owner_run_state_version,
       'runtime_checkpoint_wait',
       '{}'::jsonb
  FROM due
ON CONFLICT (org_id, run_wait_id, kind, run_lease_id, runtime_instance_id, runtime_epoch, run_state_version) WHERE kind = 'runtime_checkpoint_wait' AND acknowledged_at IS NULL DO NOTHING
RETURNING *;

-- name: CreateDueLiveRuntimeCheckpointWaitCommandsForWorker :many
WITH due AS (
    SELECT run_waits.*
      FROM run_waits
      JOIN worker_instances ON worker_instances.id = run_waits.owner_worker_instance_id
      JOIN run_leases ON run_leases.org_id = run_waits.org_id
                     AND run_leases.run_id = run_waits.run_id
                     AND run_leases.id = run_waits.owner_run_lease_id
                     AND run_leases.worker_instance_id = run_waits.owner_worker_instance_id
                     AND run_leases.status IN ('leased', 'running')
      JOIN runtime_instances
        ON runtime_instances.org_id = run_waits.org_id
       AND runtime_instances.id = run_waits.owner_runtime_instance_id
       AND runtime_instances.worker_instance_id = run_waits.owner_worker_instance_id
       AND runtime_instances.runtime_epoch = run_waits.owner_runtime_epoch
       AND runtime_instances.owner_run_id = run_waits.run_id
       AND runtime_instances.owner_run_lease_id = run_waits.owner_run_lease_id
       AND runtime_instances.owner_run_wait_id = run_waits.id
       AND runtime_instances.owner_run_state_version = run_waits.owner_run_state_version
       AND runtime_instances.state = 'waiting_hot'
       AND (
           runtime_instances.expires_at IS NULL
           OR runtime_instances.expires_at > now()
       )
     WHERE run_waits.owner_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_waits.state = 'hot_waiting'
       AND run_waits.runtime_checkpoint_due_at IS NOT NULL
       AND (
           run_waits.runtime_checkpoint_due_at <= now()
           OR worker_instances.status = 'draining'
       )
       AND run_waits.owner_run_lease_id IS NOT NULL
       AND run_waits.owner_worker_instance_id IS NOT NULL
       AND run_waits.owner_runtime_instance_id IS NOT NULL
       AND run_waits.owner_runtime_epoch IS NOT NULL
       AND run_waits.owner_run_state_version IS NOT NULL
       AND NOT EXISTS (
           SELECT 1
             FROM waits
            WHERE waits.org_id = run_waits.org_id
              AND waits.id = run_waits.wait_id
              AND waits.state = 'pending'
              AND waits.expires_at IS NOT NULL
              AND waits.expires_at <= now()
       )
       AND NOT EXISTS (
           SELECT 1
             FROM waits
            WHERE waits.org_id = run_waits.org_id
              AND waits.id = run_waits.wait_id
              AND waits.kind = 'timer'
              AND waits.state = 'pending'
              AND waits.completed_after <= now()
       )
       AND NOT EXISTS (
           SELECT 1
             FROM worker_commands
            WHERE worker_commands.org_id = run_waits.org_id
              AND worker_commands.run_wait_id = run_waits.id
              AND worker_commands.kind = 'runtime_checkpoint_wait'
              AND worker_commands.acknowledged_at IS NULL
       )
     ORDER BY run_waits.runtime_checkpoint_due_at ASC, run_waits.id ASC
     LIMIT sqlc.arg(limit_count)
)
INSERT INTO worker_commands (
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    run_id,
    run_wait_id,
    run_lease_id,
    worker_instance_id,
    runtime_instance_id,
    runtime_epoch,
    run_state_version,
    kind,
    payload
)
SELECT due.org_id,
       due.worker_group_id,
       due.project_id,
       due.environment_id,
       due.run_id,
       due.id,
       due.owner_run_lease_id,
       due.owner_worker_instance_id,
       due.owner_runtime_instance_id,
       due.owner_runtime_epoch,
       due.owner_run_state_version,
       'runtime_checkpoint_wait',
       '{}'::jsonb
  FROM due
ON CONFLICT (org_id, run_wait_id, kind, run_lease_id, runtime_instance_id, runtime_epoch, run_state_version) WHERE kind = 'runtime_checkpoint_wait' AND acknowledged_at IS NULL DO NOTHING
RETURNING *;

-- name: CreateCapacityPressureLiveRuntimeCheckpointWaitCommandsForWorker :many
WITH worker_scope AS MATERIALIZED (
    SELECT worker_instances.*
      FROM worker_instances
     WHERE worker_instances.id = sqlc.arg(worker_instance_id)
),
blocked_mount AS MATERIALIZED (
    SELECT workspace_mounts.id
      FROM workspace_mounts
      JOIN deployment_sandboxes
        ON deployment_sandboxes.org_id = workspace_mounts.org_id
       AND deployment_sandboxes.project_id = workspace_mounts.project_id
       AND deployment_sandboxes.environment_id = workspace_mounts.environment_id
       AND deployment_sandboxes.id = workspace_mounts.deployment_sandbox_id
       AND deployment_sandboxes.fingerprint = workspace_mounts.sandbox_fingerprint
      JOIN deployments
        ON deployments.org_id = deployment_sandboxes.org_id
       AND deployments.project_id = deployment_sandboxes.project_id
       AND deployments.environment_id = deployment_sandboxes.environment_id
       AND deployments.id = deployment_sandboxes.deployment_id
      JOIN worker_scope ON worker_scope.worker_group_id = workspace_mounts.worker_group_id
     WHERE workspace_mounts.state = 'mounting'
       AND NOT EXISTS (
           SELECT 1
             FROM workspace_leases
            WHERE workspace_leases.org_id = workspace_mounts.org_id
              AND workspace_leases.workspace_id = workspace_mounts.workspace_id
              AND workspace_leases.workspace_mount_id = workspace_mounts.id
              AND workspace_leases.lease_kind = 'write'
              AND workspace_leases.state IN ('active', 'releasing')
              AND workspace_leases.expires_at > now()
       )
       AND workspace_mounts.rootfs_digest = worker_scope.rootfs_digest
       AND workspace_mounts.runtime_abi = worker_scope.runtime_abi
       AND workspace_mounts.guestd_abi = sqlc.arg(guestd_abi)
       AND workspace_mounts.adapter_abi = sqlc.arg(adapter_abi)
     ORDER BY workspace_mounts.priority DESC,
              workspace_mounts.requested_at ASC,
              workspace_mounts.claim_attempt ASC
     LIMIT 1
),
victim AS (
    SELECT run_waits.*
      FROM run_waits
      JOIN run_leases ON run_leases.org_id = run_waits.org_id
                     AND run_leases.run_id = run_waits.run_id
                     AND run_leases.id = run_waits.owner_run_lease_id
                     AND run_leases.worker_instance_id = run_waits.owner_worker_instance_id
                     AND run_leases.status IN ('leased', 'running')
     WHERE run_waits.state = 'hot_waiting'
       AND run_waits.owner_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_waits.owner_run_lease_id IS NOT NULL
       AND run_waits.owner_runtime_instance_id IS NOT NULL
       AND run_waits.owner_runtime_epoch IS NOT NULL
       AND run_waits.owner_run_state_version IS NOT NULL
       AND NOT EXISTS (
           SELECT 1
             FROM waits
            WHERE waits.org_id = run_waits.org_id
              AND waits.id = run_waits.wait_id
              AND waits.state = 'pending'
              AND waits.expires_at IS NOT NULL
              AND waits.expires_at <= now()
       )
       AND EXISTS (SELECT 1 FROM blocked_mount)
       AND NOT EXISTS (
           SELECT 1
             FROM waits
            WHERE waits.org_id = run_waits.org_id
              AND waits.id = run_waits.wait_id
              AND waits.kind = 'timer'
              AND waits.state = 'pending'
              AND waits.completed_after <= now()
       )
       AND NOT EXISTS (
           SELECT 1
             FROM worker_commands
            WHERE worker_commands.org_id = run_waits.org_id
              AND worker_commands.run_wait_id = run_waits.id
              AND worker_commands.kind = 'runtime_checkpoint_wait'
              AND worker_commands.acknowledged_at IS NULL
       )
     ORDER BY run_waits.hot_wait_started_at ASC, run_waits.id ASC
     LIMIT sqlc.arg(limit_count)
     FOR UPDATE OF run_waits SKIP LOCKED
)
INSERT INTO worker_commands (
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    run_id,
    run_wait_id,
    run_lease_id,
    worker_instance_id,
    runtime_instance_id,
    runtime_epoch,
    run_state_version,
    kind,
    payload
)
SELECT victim.org_id,
       victim.worker_group_id,
       victim.project_id,
       victim.environment_id,
       victim.run_id,
       victim.id,
       victim.owner_run_lease_id,
       victim.owner_worker_instance_id,
       victim.owner_runtime_instance_id,
       victim.owner_runtime_epoch,
       victim.owner_run_state_version,
       'runtime_checkpoint_wait',
       '{}'::jsonb
  FROM victim
ON CONFLICT (org_id, run_wait_id, kind, run_lease_id, runtime_instance_id, runtime_epoch, run_state_version) WHERE kind = 'runtime_checkpoint_wait' AND acknowledged_at IS NULL DO NOTHING
RETURNING *;

-- name: CreateResolvedLiveRuntimeResumeWaitCommandsForOrg :many
WITH resolved AS (
    SELECT run_waits.*,
           CASE
             WHEN waits.state = 'cancelled' THEN 'cancelled'
             WHEN waits.state = 'expired' THEN 'timed_out'
             ELSE 'completed'
           END AS resume_kind,
           CASE
             WHEN waits.state = 'completed' THEN COALESCE(waits.result, 'null'::jsonb)
             ELSE 'null'::jsonb
           END AS resume_payload
      FROM run_waits
      JOIN waits ON waits.org_id = run_waits.org_id
                AND waits.id = run_waits.wait_id
	      JOIN run_leases ON run_leases.org_id = run_waits.org_id
	                     AND run_leases.run_id = run_waits.run_id
	                     AND run_leases.id = run_waits.owner_run_lease_id
	                     AND run_leases.worker_instance_id = run_waits.owner_worker_instance_id
	                     AND run_leases.status IN ('leased', 'running')
	      JOIN runtime_instances
	        ON runtime_instances.org_id = run_waits.org_id
	       AND runtime_instances.id = run_waits.owner_runtime_instance_id
	       AND runtime_instances.worker_instance_id = run_waits.owner_worker_instance_id
	       AND runtime_instances.runtime_epoch = run_waits.owner_runtime_epoch
	       AND runtime_instances.owner_run_id = run_waits.run_id
	       AND runtime_instances.owner_run_lease_id = run_waits.owner_run_lease_id
	       AND runtime_instances.owner_run_wait_id = run_waits.id
	       AND runtime_instances.owner_run_state_version = run_waits.owner_run_state_version
	       AND runtime_instances.state = 'waiting_hot'
	       AND (
	           runtime_instances.expires_at IS NULL
	           OR runtime_instances.expires_at > now()
	       )
	     WHERE run_waits.org_id = sqlc.arg(org_id)
	       AND run_waits.worker_group_id = sqlc.arg(worker_group_id)
	       AND run_waits.state = 'resuming'
       AND waits.state IN ('completed', 'expired', 'cancelled')
       AND run_waits.owner_run_lease_id IS NOT NULL
       AND run_waits.owner_worker_instance_id IS NOT NULL
       AND run_waits.owner_runtime_instance_id IS NOT NULL
       AND run_waits.owner_runtime_epoch IS NOT NULL
       AND run_waits.owner_run_state_version IS NOT NULL
       AND NOT EXISTS (
           SELECT 1
             FROM worker_commands
            WHERE worker_commands.org_id = run_waits.org_id
              AND worker_commands.run_wait_id = run_waits.id
              AND worker_commands.kind = 'runtime_resume_wait'
       )
     ORDER BY run_waits.resuming_at ASC, run_waits.id ASC
     LIMIT sqlc.arg(limit_count)
)
INSERT INTO worker_commands (
    org_id,
    worker_group_id,
    project_id,
    environment_id,
    run_id,
    run_wait_id,
    run_lease_id,
    worker_instance_id,
    runtime_instance_id,
    runtime_epoch,
    run_state_version,
    kind,
    payload
)
SELECT resolved.org_id,
       resolved.worker_group_id,
       resolved.project_id,
       resolved.environment_id,
       resolved.run_id,
       resolved.id,
       resolved.owner_run_lease_id,
       resolved.owner_worker_instance_id,
       resolved.owner_runtime_instance_id,
       resolved.owner_runtime_epoch,
       resolved.owner_run_state_version,
       'runtime_resume_wait',
       jsonb_build_object(
           'resume_kind', resolved.resume_kind,
           'resume_payload', resolved.resume_payload
       )
  FROM resolved
ON CONFLICT (org_id, run_wait_id, kind, run_lease_id, runtime_instance_id, runtime_epoch, run_state_version) WHERE kind = 'runtime_resume_wait' DO NOTHING
RETURNING *;
