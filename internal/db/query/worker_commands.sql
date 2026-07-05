-- name: CreateWorkerCommand :one
INSERT INTO worker_commands (
    org_id,
    cell_id,
    route_generation,
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
    sqlc.arg(cell_id),
    sqlc.arg(route_generation),
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
    sqlc.arg(kind)::worker_command_kind,
    COALESCE(sqlc.arg(payload)::jsonb, '{}'::jsonb)
)
RETURNING *;

-- name: ClaimWorkerCommands :many
WITH claimable AS (
    SELECT worker_commands.id
      FROM worker_commands
      JOIN environment_cells
        ON environment_cells.org_id = worker_commands.org_id
       AND environment_cells.project_id = worker_commands.project_id
       AND environment_cells.environment_id = worker_commands.environment_id
       AND environment_cells.cell_id = worker_commands.cell_id
       AND environment_cells.route_generation = worker_commands.route_generation
       AND environment_cells.route_state IN ('active', 'draining')
      JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                    AND org_cells.cell_id = environment_cells.cell_id
                    AND org_cells.state = 'active'
      JOIN cells ON cells.id = environment_cells.cell_id
                AND cells.state IN ('active', 'draining')
      JOIN worker_instances
        ON worker_instances.id = worker_commands.worker_instance_id
       AND worker_instances.cell_id = worker_commands.cell_id
     WHERE worker_commands.cell_id = sqlc.arg(cell_id)
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
   AND worker_commands.cell_id = sqlc.arg(cell_id)
   AND EXISTS (
       SELECT 1
         FROM worker_instances
         JOIN environment_cells
           ON environment_cells.org_id = worker_commands.org_id
          AND environment_cells.project_id = worker_commands.project_id
          AND environment_cells.environment_id = worker_commands.environment_id
          AND environment_cells.cell_id = worker_commands.cell_id
          AND environment_cells.route_generation = worker_commands.route_generation
          AND environment_cells.route_state IN ('active', 'draining')
         JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                       AND org_cells.cell_id = environment_cells.cell_id
                       AND org_cells.state = 'active'
         JOIN cells ON cells.id = environment_cells.cell_id
                   AND cells.state IN ('active', 'draining')
        WHERE worker_instances.id = worker_commands.worker_instance_id
          AND worker_instances.cell_id = worker_commands.cell_id
          AND worker_instances.cell_id = sqlc.arg(cell_id)
   )
   AND worker_commands.acknowledged_at IS NULL
RETURNING *;

-- name: ListWorkerCommandsAfter :many
SELECT *
  FROM worker_commands
 WHERE worker_commands.worker_instance_id = sqlc.arg(worker_instance_id)
   AND worker_commands.cell_id = sqlc.arg(cell_id)
   AND EXISTS (
       SELECT 1
         FROM worker_instances
         JOIN environment_cells
           ON environment_cells.org_id = worker_commands.org_id
          AND environment_cells.project_id = worker_commands.project_id
          AND environment_cells.environment_id = worker_commands.environment_id
          AND environment_cells.cell_id = worker_commands.cell_id
          AND environment_cells.route_generation = worker_commands.route_generation
          AND environment_cells.route_state IN ('active', 'draining')
         JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                       AND org_cells.cell_id = environment_cells.cell_id
                       AND org_cells.state = 'active'
         JOIN cells ON cells.id = environment_cells.cell_id
                   AND cells.state IN ('active', 'draining')
        WHERE worker_instances.id = worker_commands.worker_instance_id
          AND worker_instances.cell_id = worker_commands.cell_id
          AND worker_instances.cell_id = sqlc.arg(cell_id)
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
       AND worker_commands.cell_id = sqlc.arg(cell_id)
       AND EXISTS (
           SELECT 1
             FROM worker_instances
             JOIN environment_cells
               ON environment_cells.org_id = worker_commands.org_id
              AND environment_cells.project_id = worker_commands.project_id
              AND environment_cells.environment_id = worker_commands.environment_id
              AND environment_cells.cell_id = worker_commands.cell_id
              AND environment_cells.route_generation = worker_commands.route_generation
              AND environment_cells.route_state IN ('active', 'draining')
             JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                           AND org_cells.cell_id = environment_cells.cell_id
                           AND org_cells.state = 'active'
             JOIN cells ON cells.id = environment_cells.cell_id
                       AND cells.state IN ('active', 'draining')
            WHERE worker_instances.id = worker_commands.worker_instance_id
              AND worker_instances.cell_id = worker_commands.cell_id
              AND worker_instances.cell_id = sqlc.arg(cell_id)
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
       AND run_waits.cell_id = target.cell_id
       AND run_waits.run_id = target.run_id
       AND run_waits.id = target.run_wait_id
       AND run_waits.owner_run_lease_id = target.run_lease_id
       AND run_waits.owner_worker_instance_id = target.worker_instance_id
       AND run_waits.owner_runtime_instance_id = target.runtime_instance_id
       AND run_waits.owner_runtime_epoch = target.runtime_epoch
       AND run_waits.owner_run_state_version = target.run_state_version
       AND run_waits.state = 'resolved_live'
     JOIN runtime_instances
        ON runtime_instances.org_id = target.org_id
       AND runtime_instances.cell_id = target.cell_id
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
       SET resumed_at = COALESCE(run_waits.resumed_at, now()),
           state = 'resumed',
           updated_at = now()
     FROM eligible_resume
     WHERE run_waits.org_id = eligible_resume.org_id
       AND run_waits.cell_id = eligible_resume.cell_id
       AND run_waits.run_id = eligible_resume.run_id
       AND run_waits.id = eligible_resume.run_wait_id
       AND run_waits.owner_run_lease_id = eligible_resume.run_lease_id
       AND run_waits.owner_worker_instance_id = eligible_resume.worker_instance_id
       AND run_waits.owner_runtime_instance_id = eligible_resume.runtime_instance_id
       AND run_waits.owner_runtime_epoch = eligible_resume.runtime_epoch
       AND run_waits.owner_run_state_version = eligible_resume.run_state_version
       AND run_waits.state = 'resolved_live'
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
       AND resumed_live_wait.cell_id = eligible_resume.cell_id
       AND resumed_live_wait.id = eligible_resume.run_wait_id
     WHERE runtime_instances.org_id = eligible_resume.org_id
       AND runtime_instances.cell_id = eligible_resume.cell_id
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
           target.kind <> 'runtime_resume_wait'
           OR (
               EXISTS (SELECT 1 FROM resumed_live_wait)
               AND EXISTS (SELECT 1 FROM resumed_runtime_instance)
           )
           OR EXISTS (SELECT 1 FROM stale_resume)
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
   AND worker_commands.cell_id = sqlc.arg(cell_id)
   AND worker_commands.run_id = sqlc.arg(run_id)
   AND worker_commands.run_wait_id = sqlc.arg(run_wait_id)
   AND worker_commands.run_lease_id = sqlc.arg(run_lease_id)
   AND worker_commands.kind = sqlc.arg(kind)::worker_command_kind
   AND EXISTS (
       SELECT 1
         FROM worker_instances
         JOIN environment_cells
           ON environment_cells.org_id = worker_commands.org_id
          AND environment_cells.project_id = worker_commands.project_id
          AND environment_cells.environment_id = worker_commands.environment_id
          AND environment_cells.cell_id = worker_commands.cell_id
          AND environment_cells.route_generation = worker_commands.route_generation
          AND environment_cells.route_state IN ('active', 'draining')
         JOIN org_cells ON org_cells.org_id = environment_cells.org_id
                       AND org_cells.cell_id = environment_cells.cell_id
                       AND org_cells.state = 'active'
         JOIN cells ON cells.id = environment_cells.cell_id
                   AND cells.state IN ('active', 'draining')
        WHERE worker_instances.id = worker_commands.worker_instance_id
          AND worker_instances.cell_id = worker_commands.cell_id
          AND worker_instances.cell_id = sqlc.arg(cell_id)
   )
RETURNING *;

-- name: CreateDueLiveRuntimeCheckpointWaitCommandsForOrg :many
WITH due AS (
    SELECT run_waits.*,
           run_leases.route_generation
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
       AND run_waits.cell_id = sqlc.arg(cell_id)
       AND run_waits.state = 'live_waiting'
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
       AND (run_waits.timeout_at IS NULL OR run_waits.timeout_at > now())
       AND NOT EXISTS (
           SELECT 1
             FROM timer_waits
            WHERE timer_waits.org_id = run_waits.org_id
              AND timer_waits.project_id = run_waits.project_id
              AND timer_waits.environment_id = run_waits.environment_id
              AND timer_waits.run_wait_id = run_waits.id
              AND timer_waits.fire_at <= now()
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
    cell_id,
    route_generation,
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
       due.cell_id,
       due.route_generation,
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
    SELECT run_waits.*,
           run_leases.route_generation
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
       AND run_waits.state = 'live_waiting'
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
       AND (run_waits.timeout_at IS NULL OR run_waits.timeout_at > now())
       AND NOT EXISTS (
           SELECT 1
             FROM timer_waits
            WHERE timer_waits.org_id = run_waits.org_id
              AND timer_waits.project_id = run_waits.project_id
              AND timer_waits.environment_id = run_waits.environment_id
              AND timer_waits.run_wait_id = run_waits.id
              AND timer_waits.fire_at <= now()
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
    cell_id,
    route_generation,
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
       due.cell_id,
       due.route_generation,
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
      JOIN worker_scope ON worker_scope.worker_group_id = deployments.worker_group_id
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
    SELECT run_waits.*,
           run_leases.route_generation
      FROM run_waits
      JOIN run_leases ON run_leases.org_id = run_waits.org_id
                     AND run_leases.run_id = run_waits.run_id
                     AND run_leases.id = run_waits.owner_run_lease_id
                     AND run_leases.worker_instance_id = run_waits.owner_worker_instance_id
                     AND run_leases.status IN ('leased', 'running')
     WHERE run_waits.state = 'live_waiting'
       AND run_waits.owner_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_waits.owner_run_lease_id IS NOT NULL
       AND run_waits.owner_runtime_instance_id IS NOT NULL
       AND run_waits.owner_runtime_epoch IS NOT NULL
       AND run_waits.owner_run_state_version IS NOT NULL
       AND (run_waits.timeout_at IS NULL OR run_waits.timeout_at > now())
       AND EXISTS (SELECT 1 FROM blocked_mount)
       AND NOT EXISTS (
           SELECT 1
             FROM timer_waits
            WHERE timer_waits.org_id = run_waits.org_id
              AND timer_waits.project_id = run_waits.project_id
              AND timer_waits.environment_id = run_waits.environment_id
              AND timer_waits.run_wait_id = run_waits.id
              AND timer_waits.fire_at <= now()
       )
       AND NOT EXISTS (
           SELECT 1
             FROM worker_commands
            WHERE worker_commands.org_id = run_waits.org_id
              AND worker_commands.run_wait_id = run_waits.id
              AND worker_commands.kind = 'runtime_checkpoint_wait'
              AND worker_commands.acknowledged_at IS NULL
       )
     ORDER BY run_waits.live_wait_started_at ASC, run_waits.id ASC
     LIMIT sqlc.arg(limit_count)
     FOR UPDATE OF run_waits SKIP LOCKED
)
INSERT INTO worker_commands (
    org_id,
    cell_id,
    route_generation,
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
       victim.cell_id,
       victim.route_generation,
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
           run_leases.route_generation,
           CASE
             WHEN run_waits.kind = 'timer' THEN 'completed'
             WHEN run_waits.timeout_at IS NOT NULL
              AND run_waits.resolved_at IS NOT NULL
              AND run_waits.timeout_at <= run_waits.resolved_at THEN 'timed_out'
             WHEN run_waits.kind = 'token' AND tokens.state = 'cancelled' THEN 'cancelled'
             WHEN run_waits.kind = 'token' AND tokens.state = 'expired' THEN 'timed_out'
             WHEN run_waits.kind = 'token' THEN 'completed'
             ELSE 'completed'
           END AS resume_kind,
           CASE
             WHEN run_waits.kind = 'timer' THEN 'null'::jsonb
             WHEN run_waits.timeout_at IS NOT NULL
              AND run_waits.resolved_at IS NOT NULL
              AND run_waits.timeout_at <= run_waits.resolved_at THEN 'null'::jsonb
             WHEN run_waits.kind = 'stream' THEN jsonb_build_object(
                 'stream', streams.name,
                 'sequence', stream_records.sequence,
                 'data', stream_records.data
             )
             WHEN run_waits.kind = 'token' AND tokens.state = 'completed' THEN COALESCE(tokens.completion_data, 'null'::jsonb)
             ELSE 'null'::jsonb
           END AS resume_payload
      FROM run_waits
      LEFT JOIN stream_waits ON stream_waits.org_id = run_waits.org_id
                            AND stream_waits.project_id = run_waits.project_id
                            AND stream_waits.environment_id = run_waits.environment_id
                            AND stream_waits.run_wait_id = run_waits.id
      LEFT JOIN streams ON streams.org_id = stream_waits.org_id
                       AND streams.project_id = stream_waits.project_id
                       AND streams.environment_id = stream_waits.environment_id
                       AND streams.id = stream_waits.stream_id
      LEFT JOIN stream_records ON stream_records.org_id = stream_waits.org_id
                              AND stream_records.stream_id = stream_waits.stream_id
                              AND stream_records.id = stream_waits.matched_record_id
      LEFT JOIN token_waits ON token_waits.org_id = run_waits.org_id
                           AND token_waits.project_id = run_waits.project_id
                           AND token_waits.environment_id = run_waits.environment_id
                           AND token_waits.run_wait_id = run_waits.id
      LEFT JOIN tokens ON tokens.org_id = token_waits.org_id
                      AND tokens.project_id = token_waits.project_id
                      AND tokens.environment_id = token_waits.environment_id
                      AND tokens.id = token_waits.token_id
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
	       AND run_waits.cell_id = sqlc.arg(cell_id)
	       AND run_waits.state = 'resolved_live'
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
     ORDER BY run_waits.resolved_at ASC, run_waits.id ASC
     LIMIT sqlc.arg(limit_count)
)
INSERT INTO worker_commands (
    org_id,
    cell_id,
    route_generation,
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
       resolved.cell_id,
       resolved.route_generation,
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
