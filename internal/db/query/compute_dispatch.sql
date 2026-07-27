-- name: SetWorkerInstanceState :one
UPDATE worker_instances
   SET state = sqlc.arg(state)::text,
       draining_at = CASE WHEN sqlc.arg(state)::text = 'draining'
                          THEN COALESCE(draining_at, now()) ELSE draining_at END,
       disabled_at = CASE WHEN sqlc.arg(state)::text = 'disabled'
                          THEN COALESCE(disabled_at, now()) ELSE disabled_at END,
       lost_at = CASE WHEN sqlc.arg(state)::text = 'lost'
                      THEN COALESCE(lost_at, now()) ELSE lost_at END,
       updated_at = now()
 WHERE id = sqlc.arg(id)
   AND worker_group_id = sqlc.arg(worker_group_id)
   AND (sqlc.narg(expected_epoch)::bigint IS NULL OR current_epoch = sqlc.narg(expected_epoch)::bigint)
RETURNING *;

-- name: DrainWorkerInstance :one
WITH target AS (
    UPDATE worker_instances
       SET state = 'draining', draining_at = COALESCE(draining_at, now()), updated_at = now()
     WHERE worker_instances.id = sqlc.arg(id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(expected_epoch)
       AND worker_instances.state IN ('active', 'draining')
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
              AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
       )
       AND NOT EXISTS (
           SELECT 1 FROM workspace_mounts
            WHERE workspace_mounts.runtime_instance_id = runtime_instances.id
              AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
       )
    RETURNING runtime_instances.id
)
SELECT target.*
  FROM target
 WHERE (SELECT count(*) FROM idle_mounts) >= 0
   AND (SELECT count(*) FROM idle_runtimes) >= 0;

-- name: FenceWorkerInstance :one
WITH target AS (
    UPDATE worker_instances
       SET state = 'lost', claim_version = claim_version + 1,
           lost_at = COALESCE(lost_at, now()), updated_at = now()
     WHERE worker_instances.id = sqlc.arg(id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(expected_epoch)
       AND worker_instances.state IN ('active', 'draining')
    RETURNING *
), revoked_credentials AS (
    UPDATE worker_instance_credentials
       SET revoked_at = COALESCE(revoked_at, now())
      FROM target
     WHERE worker_instance_credentials.worker_instance_id = target.id
       AND worker_instance_credentials.revoked_at IS NULL
    RETURNING worker_instance_credentials.id
), lost_mounts AS (
    UPDATE workspace_mounts
       SET state = 'lost', lost_at = now(), terminal_at = now(),
           terminal_reason_code = sqlc.arg(reason_code), updated_at = now()
      FROM target
     WHERE workspace_mounts.worker_instance_id = target.id
       AND workspace_mounts.worker_epoch = target.current_epoch
       AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')
    RETURNING workspace_mounts.id
), lost_runtimes AS (
    UPDATE runtime_instances
       SET observed_state = 'lost', observed_version = observed_version + 1,
           observed_at = now(), lost_at = now(), terminal_at = now(),
           terminal_reason_code = sqlc.arg(reason_code),
           reserved_run_id = NULL, reserved_attempt_number = NULL,
           reserved_process_id = NULL, reserved_workspace_version_id = NULL,
           reservation_expires_at = NULL, updated_at = now()
      FROM target
     WHERE runtime_instances.worker_instance_id = target.id
       AND runtime_instances.worker_epoch = target.current_epoch
       AND runtime_instances.reclaimed_at IS NULL
       AND runtime_instances.observed_state IN ('allocated', 'preparing', 'ready', 'closing')
    RETURNING runtime_instances.id
), lost_slots AS (
    UPDATE worker_network_slots
       SET state = 'lost', generation = generation + 1,
           lost_at = now(), state_reason_code = sqlc.arg(reason_code), updated_at = now()
      FROM target
     WHERE worker_network_slots.worker_instance_id = target.id
       AND worker_network_slots.worker_epoch = target.current_epoch
       AND worker_network_slots.state IN ('assigned', 'bound', 'reclaiming', 'quarantined')
    RETURNING worker_network_slots.id
)
SELECT target.*
  FROM target
 WHERE (SELECT count(*) FROM revoked_credentials) >= 0
   AND (SELECT count(*) FROM lost_mounts) >= 0
   AND (SELECT count(*) FROM lost_runtimes) >= 0
   AND (SELECT count(*) FROM lost_slots) >= 0;

-- name: ListWorkerInstances :many
SELECT * FROM worker_instances
 WHERE sqlc.arg(state_filter)::text = 'all' OR state::text = sqlc.arg(state_filter)::text
 ORDER BY updated_at DESC, created_at ASC
 LIMIT sqlc.arg(row_limit);

-- name: GetWorkerInstanceState :one
SELECT worker_instances.*,
       runtime_identities.rootfs_digest,
       runtime_identities.runtime_abi,
       runtime_identities.runtime_arch,
       ((SELECT count(*) FROM run_leases
         WHERE run_leases.worker_instance_id = worker_instances.id
           AND run_leases.worker_epoch = worker_instances.current_epoch
           AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')) +
        (SELECT count(*) FROM deployment_build_leases
         WHERE deployment_build_leases.worker_instance_id = worker_instances.id
           AND deployment_build_leases.worker_epoch = worker_instances.current_epoch
           AND deployment_build_leases.state IN ('assigned', 'starting', 'running')) +
        (SELECT count(*) FROM workspace_mounts
         WHERE workspace_mounts.worker_instance_id = worker_instances.id
           AND workspace_mounts.worker_epoch = worker_instances.current_epoch
           AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')) +
        (SELECT count(*) FROM runtime_instances
         WHERE runtime_instances.worker_instance_id = worker_instances.id
           AND runtime_instances.worker_epoch = worker_instances.current_epoch
           AND runtime_instances.observed_state IN ('allocated', 'preparing', 'ready', 'closing')))::int AS active_executions
  FROM worker_instances
  LEFT JOIN runtime_identities ON runtime_identities.id = worker_instances.runtime_identity_id
 WHERE worker_instances.id = sqlc.arg(id)
   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id);

-- name: GetWorkerInstanceRunDispatchCapacity :one
SELECT GREATEST(worker_instances.certified_cpu_millis - usage.cpu_millis, 0)::bigint AS available_cpu_millis,
       GREATEST(worker_instances.certified_memory_bytes - usage.memory_bytes, 0)::bigint AS available_memory_bytes,
       GREATEST(worker_instances.certified_workload_disk_bytes - usage.workload_disk_bytes, 0)::bigint AS available_workload_disk_bytes,
       GREATEST(worker_instances.certified_scratch_bytes - usage.scratch_bytes, 0)::bigint AS available_scratch_bytes,
       GREATEST(worker_instances.max_vm_slots - usage.vm_slots, 0)::int AS available_vm_slots,
       GREATEST(worker_instances.max_run_consumers - usage.run_consumers, 0)::int AS available_run_consumers,
       GREATEST(worker_instances.max_build_executors - usage.build_executors, 0)::int AS available_build_executors
  FROM worker_instances
  CROSS JOIN LATERAL (
      SELECT
        COALESCE((SELECT sum(reserved_cpu_millis) FROM runtime_instances
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND (observed_state IN ('allocated','preparing','ready','closing')
                           OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)
        + COALESCE((SELECT sum(requested_cpu_millis) FROM deployment_build_leases
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND state IN ('assigned','starting','running')), 0) AS cpu_millis,
        COALESCE((SELECT sum(reserved_memory_bytes) FROM runtime_instances
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND (observed_state IN ('allocated','preparing','ready','closing')
                           OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)
        + COALESCE((SELECT sum(requested_memory_bytes) FROM deployment_build_leases
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND state IN ('assigned','starting','running')), 0) AS memory_bytes,
        COALESCE((SELECT sum(reserved_workload_disk_bytes) FROM runtime_instances
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND (observed_state IN ('allocated','preparing','ready','closing')
                           OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)
        + COALESCE((SELECT sum(requested_workload_disk_bytes) FROM deployment_build_leases
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND state IN ('assigned','starting','running')), 0) AS workload_disk_bytes,
        COALESCE((SELECT sum(reserved_scratch_bytes) FROM runtime_instances
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND (observed_state IN ('allocated','preparing','ready','closing')
                           OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)
        + COALESCE((SELECT sum(requested_scratch_bytes) FROM deployment_build_leases
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND state IN ('assigned','starting','running')), 0) AS scratch_bytes,
        COALESCE((SELECT count(*) FROM runtime_instances
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND (observed_state IN ('allocated','preparing','ready','closing')
                          OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)::int AS vm_slots,
        COALESCE((SELECT sum(requested_execution_slots) FROM run_leases
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND state IN ('assigned','starting','running','checkpointing','finalizing')), 0)::int AS run_consumers,
        COALESCE((SELECT sum(requested_build_executors) FROM deployment_build_leases
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND state IN ('assigned','starting','running')), 0)::int AS build_executors
  ) usage
 WHERE worker_instances.id = sqlc.arg(id)
   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instances.state = 'active'
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch);

-- name: GetWorkerInstanceQueueCapacity :one
SELECT GREATEST(worker_instances.certified_cpu_millis - usage.cpu_millis, 0)::bigint AS available_cpu_millis,
       GREATEST(worker_instances.certified_memory_bytes - usage.memory_bytes, 0)::bigint AS available_memory_bytes,
       GREATEST(worker_instances.certified_workload_disk_bytes - usage.workload_disk_bytes, 0)::bigint AS available_workload_disk_bytes,
       GREATEST(worker_instances.certified_scratch_bytes - usage.scratch_bytes, 0)::bigint AS available_scratch_bytes,
       GREATEST(worker_instances.max_run_consumers - usage.run_consumers, 0)::int AS available_run_consumers,
       GREATEST(worker_instances.max_build_executors - usage.build_executors, 0)::int AS available_build_executors
  FROM worker_instances
  CROSS JOIN LATERAL (
      SELECT
        COALESCE((SELECT sum(reserved_cpu_millis) FROM runtime_instances
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND (observed_state IN ('allocated','preparing','ready','closing')
                          OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)
        + COALESCE((SELECT sum(requested_cpu_millis) FROM deployment_build_leases
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND state IN ('assigned','starting','running')), 0) AS cpu_millis,
        COALESCE((SELECT sum(reserved_memory_bytes) FROM runtime_instances
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND (observed_state IN ('allocated','preparing','ready','closing')
                          OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)
        + COALESCE((SELECT sum(requested_memory_bytes) FROM deployment_build_leases
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND state IN ('assigned','starting','running')), 0) AS memory_bytes,
        COALESCE((SELECT sum(reserved_workload_disk_bytes) FROM runtime_instances
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND (observed_state IN ('allocated','preparing','ready','closing')
                          OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)
        + COALESCE((SELECT sum(requested_workload_disk_bytes) FROM deployment_build_leases
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND state IN ('assigned','starting','running')), 0) AS workload_disk_bytes,
        COALESCE((SELECT sum(reserved_scratch_bytes) FROM runtime_instances
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND (observed_state IN ('allocated','preparing','ready','closing')
                          OR (observed_state IN ('failed','lost') AND reclaimed_at IS NULL))), 0)
        + COALESCE((SELECT sum(requested_scratch_bytes) FROM deployment_build_leases
                    WHERE worker_instance_id = worker_instances.id
                      AND worker_epoch = worker_instances.current_epoch
                      AND state IN ('assigned','starting','running')), 0) AS scratch_bytes,
        COALESCE((SELECT sum(requested_execution_slots) FROM run_leases
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND state IN ('assigned','starting','running','checkpointing','finalizing')), 0)::int AS run_consumers,
        COALESCE((SELECT sum(requested_build_executors) FROM deployment_build_leases
                   WHERE worker_instance_id = worker_instances.id
                     AND worker_epoch = worker_instances.current_epoch
                     AND state IN ('assigned','starting','running')), 0)::int AS build_executors
  ) usage
 WHERE worker_instances.id = sqlc.arg(id)
   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instances.current_epoch = sqlc.arg(worker_epoch)
   AND worker_instances.state = 'active';

-- name: GetQueuedRunReadyHint :one
SELECT runs.id, runs.org_id, runs.project_id, runs.environment_id,
       runs.queue_name, runs.concurrency_key, runs.state_version, runs.priority,
       runs.queue_origin_at, runs.queue_score_at, workspaces.region_id
  FROM runs
  JOIN workspaces
    ON workspaces.environment_id = runs.environment_id
   AND workspaces.id = runs.workspace_id
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
   AND (
        (runs.entrypoint_kind = 'task'
         AND runs.actor_id IS NULL
         AND runs.cause_kind IN ('api', 'manual', 'schedule', 'child')
         AND (
             (workspaces.owner_run_id = runs.id
              AND workspaces.owner_actor_id IS NULL
              AND (NOT EXISTS (
                 SELECT 1
                   FROM run_waits
                  WHERE run_waits.run_id = runs.id
                    AND run_waits.suspension_state IN (
                        'hot', 'checkpointing', 'parked',
                        'resume_pending', 'resuming'
                    )
              ) OR EXISTS (
                 SELECT 1
                   FROM run_waits
                   JOIN run_checkpoints
                     ON run_checkpoints.id =
                        run_waits.suspend_checkpoint_id
                    AND run_checkpoints.kind = 'suspend'
                    AND run_checkpoints.run_id = run_waits.run_id
                    AND run_checkpoints.attempt_number =
                        run_waits.attempt_number
                    AND run_checkpoints.run_wait_id = run_waits.id
                    AND run_checkpoints.workspace_id =
                        run_waits.workspace_id
                    AND run_checkpoints.state = 'ready'
                    AND (run_checkpoints.expires_at IS NULL
                         OR run_checkpoints.expires_at > now())
                   JOIN workspace_versions
                     ON workspace_versions.workspace_id =
                        run_checkpoints.workspace_id
                    AND workspace_versions.id =
                        run_checkpoints.private_workspace_version_id
                    AND workspace_versions.state = 'private'
                  WHERE run_waits.run_id = runs.id
                    AND run_waits.suspension_state = 'resume_pending'
                    AND run_waits.handoff_runtime_instance_id IS NULL
                    AND run_waits.handoff_workspace_mount_id IS NULL
                    AND run_waits.handoff_resume_checkpoint_id IS NULL
              )))
             OR EXISTS (
                 SELECT 1
                   FROM run_waits AS handoff
                   JOIN runs AS parent
                     ON parent.environment_id = handoff.environment_id
                    AND parent.id = handoff.run_id
                    AND parent.workspace_id = handoff.workspace_id
                    AND parent.status = 'waiting'
                    AND parent.current_run_lease_id IS NULL
                   JOIN run_checkpoints AS checkpoint
                     ON checkpoint.id = handoff.suspend_checkpoint_id
                    AND checkpoint.kind = 'suspend'
                    AND checkpoint.run_id = handoff.run_id
                    AND checkpoint.attempt_number =
                        handoff.attempt_number
                    AND checkpoint.run_wait_id = handoff.id
                    AND checkpoint.workspace_id = handoff.workspace_id
                    AND checkpoint.state = 'ready'
                   JOIN workspace_versions AS base
                     ON base.workspace_id = handoff.workspace_id
                    AND base.id = handoff.base_workspace_version_id
                    AND base.state = 'private'
                  WHERE handoff.child_run_id = runs.id
                    AND handoff.child_parent_owned IS TRUE
                    AND handoff.workspace_id = runs.workspace_id
                    AND handoff.condition_state = 'pending'
                    AND handoff.suspension_state = 'parked'
                    AND handoff.base_workspace_version_id =
                        runs.base_workspace_version_id
                    AND handoff.handoff_runtime_instance_id IS NOT NULL
                    AND handoff.handoff_workspace_mount_id IS NOT NULL
                    AND handoff.handoff_mount_generation IS NOT NULL
                    AND handoff.ownership_generation IS NOT NULL
                    AND handoff.parent_writer_generation IS NOT NULL
                    AND handoff.child_writer_generation IS NULL
             )
         ))
       OR
       (runs.entrypoint_kind = 'actor'
        AND runs.actor_id IS NOT NULL
        AND runs.cause_kind IN ('actor_start', 'continuation')
        AND runs.parent_run_id IS NULL
        AND workspaces.owner_actor_id = runs.actor_id
        AND workspaces.owner_run_id IS NULL
        AND EXISTS (
            SELECT 1
              FROM actors
             WHERE actors.id = runs.actor_id
               AND actors.workspace_id = runs.workspace_id
               AND actors.current_run_id = runs.id
               AND actors.state IN ('open', 'closing')
        )
        AND EXISTS (
            SELECT 1
              FROM run_waits
              JOIN run_checkpoints
                ON run_checkpoints.id = run_waits.suspend_checkpoint_id
               AND run_checkpoints.kind = 'suspend'
               AND run_checkpoints.run_id = run_waits.run_id
               AND run_checkpoints.attempt_number = run_waits.attempt_number
               AND run_checkpoints.run_wait_id = run_waits.id
               AND run_checkpoints.workspace_id = run_waits.workspace_id
               AND run_checkpoints.actor_speculative_input_sequence IS NOT NULL
               AND run_checkpoints.state = 'ready'
               AND (run_checkpoints.expires_at IS NULL OR run_checkpoints.expires_at > now())
              JOIN workspace_versions
                ON workspace_versions.workspace_id = run_checkpoints.workspace_id
               AND workspace_versions.id = run_checkpoints.private_workspace_version_id
               AND workspace_versions.state = 'private'
              JOIN actors AS restore_actor
                ON restore_actor.id = runs.actor_id
               AND restore_actor.workspace_id = runs.workspace_id
               AND restore_actor.current_run_id = runs.id
               AND restore_actor.state IN ('open', 'closing')
              JOIN run_attempts AS restore_attempt
                ON restore_attempt.run_id = runs.id
               AND restore_attempt.number = runs.current_attempt_number
               AND restore_attempt.workspace_id = runs.workspace_id
               AND restore_attempt.entrypoint_kind = 'actor'
               AND restore_attempt.actor_start_input_sequence = runs.actor_start_input_sequence
               AND restore_attempt.terminal_at IS NULL
             WHERE run_waits.run_id = runs.id
               AND run_waits.suspension_state = 'resume_pending'
               AND run_waits.handoff_runtime_instance_id IS NULL
               AND run_waits.handoff_workspace_mount_id IS NULL
               AND run_waits.handoff_resume_checkpoint_id IS NULL
               AND runs.actor_start_input_sequence <= runs.actor_start_input_high_watermark
               AND restore_actor.committed_input_sequence >= runs.actor_start_input_sequence
               AND restore_actor.committed_input_sequence < restore_actor.next_input_sequence
               AND run_checkpoints.actor_speculative_input_sequence
                   BETWEEN restore_actor.committed_input_sequence
                       AND restore_actor.next_input_sequence - 1
        ))
   )
   AND (runs.first_lease_at IS NOT NULL OR runs.queued_expires_at IS NULL OR runs.queued_expires_at > now());

-- name: GetQueuedRunResumeHint :one
SELECT runs.id, runs.org_id, runs.project_id, runs.environment_id,
       runs.queue_name, runs.concurrency_key, runs.state_version, runs.priority,
       runs.queue_origin_at, runs.queue_score_at, workspaces.region_id
  FROM runs
  JOIN workspaces
    ON workspaces.environment_id = runs.environment_id
   AND workspaces.id = runs.workspace_id
  JOIN run_waits
    ON run_waits.environment_id = runs.environment_id
   AND run_waits.run_id = runs.id
   AND run_waits.workspace_id = runs.workspace_id
   AND run_waits.attempt_number = runs.current_attempt_number
  JOIN run_checkpoints
    ON run_checkpoints.id = run_waits.suspend_checkpoint_id
   AND run_checkpoints.kind = 'suspend'
   AND run_checkpoints.run_id = run_waits.run_id
   AND run_checkpoints.attempt_number = run_waits.attempt_number
   AND run_checkpoints.run_wait_id = run_waits.id
   AND run_checkpoints.workspace_id = run_waits.workspace_id
   AND run_checkpoints.state = 'ready'
   AND (run_checkpoints.expires_at IS NULL OR run_checkpoints.expires_at > now())
  JOIN workspace_versions
    ON workspace_versions.workspace_id = run_checkpoints.workspace_id
   AND workspace_versions.id = run_checkpoints.private_workspace_version_id
   AND workspace_versions.state = 'private'
 WHERE runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
   AND run_waits.id = sqlc.arg(run_wait_id)
   AND run_waits.condition_state <> 'pending'
   AND run_waits.suspension_state = 'resume_pending'
   AND run_waits.expected_run_state_version = runs.state_version
   AND run_waits.resume_request_version = sqlc.arg(resume_request_version)
   AND run_waits.resume_ack_version < run_waits.resume_request_version
   AND (runs.first_lease_at IS NOT NULL OR runs.queued_expires_at IS NULL OR runs.queued_expires_at > now());

-- name: GetRunResumeHintAuthority :one
SELECT runs.status,
       runs.state_version,
       runs.current_run_lease_id,
       run_waits.condition_state,
       run_waits.suspension_state,
       run_waits.expected_run_state_version,
       run_waits.resume_request_version
  FROM runs
  JOIN run_waits
    ON run_waits.environment_id = runs.environment_id
   AND run_waits.run_id = runs.id
 WHERE runs.environment_id = sqlc.arg(environment_id)
   AND runs.id = sqlc.arg(run_id)
   AND run_waits.id = sqlc.arg(run_wait_id);

-- name: ListQueuedRunCandidateScopes :many
WITH candidate_scopes AS (
    SELECT runs.org_id, runs.project_id, runs.environment_id, workspaces.region_id,
           coalesce(runs.concurrency_key, '') AS concurrency_key, runs.queue_name,
           md5(runs.org_id::text || ':' || runs.project_id::text || ':' ||
               runs.environment_id::text || ':' || workspaces.region_id || ':' ||
               coalesce(runs.concurrency_key, '') || ':' || runs.queue_name || ':' || sqlc.arg(scan_seed)::text) AS sort_key
      FROM runs
      JOIN workspaces ON workspaces.environment_id = runs.environment_id
                     AND workspaces.id = runs.workspace_id
     WHERE runs.status = 'queued'
       AND runs.current_run_lease_id IS NULL
       AND (
           (runs.entrypoint_kind = 'task'
            AND runs.actor_id IS NULL
            AND runs.cause_kind IN ('api', 'manual', 'schedule', 'child')
            AND (
              (workspaces.owner_run_id = runs.id
               AND workspaces.owner_actor_id IS NULL
               AND (NOT EXISTS (
               SELECT 1
                 FROM run_waits
                WHERE run_waits.run_id = runs.id
                  AND run_waits.suspension_state IN (
                      'hot', 'checkpointing', 'parked', 'resume_pending', 'resuming'
                  )
            ) OR EXISTS (
               SELECT 1
                 FROM run_waits
                 JOIN run_checkpoints
                   ON run_checkpoints.id = run_waits.suspend_checkpoint_id
                  AND run_checkpoints.kind = 'suspend'
                  AND run_checkpoints.run_id = run_waits.run_id
                  AND run_checkpoints.attempt_number = run_waits.attempt_number
                  AND run_checkpoints.run_wait_id = run_waits.id
                  AND run_checkpoints.workspace_id = run_waits.workspace_id
                  AND run_checkpoints.state = 'ready'
                  AND (run_checkpoints.expires_at IS NULL OR run_checkpoints.expires_at > now())
                 JOIN workspace_versions
                   ON workspace_versions.workspace_id = run_checkpoints.workspace_id
                  AND workspace_versions.id = run_checkpoints.private_workspace_version_id
                  AND workspace_versions.state = 'private'
                WHERE run_waits.run_id = runs.id
                  AND run_waits.suspension_state = 'resume_pending'
                  AND run_waits.handoff_runtime_instance_id IS NULL
                  AND run_waits.handoff_workspace_mount_id IS NULL
                  AND run_waits.handoff_resume_checkpoint_id IS NULL
              )))
              OR EXISTS (
                  SELECT 1
                    FROM run_waits AS handoff
                    JOIN runs AS parent
                      ON parent.environment_id = handoff.environment_id
                     AND parent.id = handoff.run_id
                     AND parent.workspace_id = handoff.workspace_id
                     AND parent.status = 'waiting'
                     AND parent.current_run_lease_id IS NULL
                    JOIN run_checkpoints AS checkpoint
                      ON checkpoint.id = handoff.suspend_checkpoint_id
                     AND checkpoint.kind = 'suspend'
                     AND checkpoint.run_id = handoff.run_id
                     AND checkpoint.attempt_number =
                         handoff.attempt_number
                     AND checkpoint.run_wait_id = handoff.id
                     AND checkpoint.workspace_id = handoff.workspace_id
                     AND checkpoint.state = 'ready'
                    JOIN workspace_versions AS base
                      ON base.workspace_id = handoff.workspace_id
                     AND base.id = handoff.base_workspace_version_id
                     AND base.state = 'private'
                   WHERE handoff.child_run_id = runs.id
                     AND handoff.child_parent_owned IS TRUE
                     AND handoff.workspace_id = runs.workspace_id
                     AND handoff.condition_state = 'pending'
                     AND handoff.suspension_state = 'parked'
                     AND handoff.base_workspace_version_id =
                         runs.base_workspace_version_id
                     AND handoff.handoff_runtime_instance_id IS NOT NULL
                     AND handoff.handoff_workspace_mount_id IS NOT NULL
                     AND handoff.handoff_mount_generation IS NOT NULL
                     AND handoff.ownership_generation IS NOT NULL
                     AND handoff.parent_writer_generation IS NOT NULL
                     AND handoff.child_writer_generation IS NULL
              )
            ))
           OR
           (runs.entrypoint_kind = 'actor'
            AND runs.actor_id IS NOT NULL
            AND runs.cause_kind IN ('actor_start', 'continuation')
            AND runs.parent_run_id IS NULL
            AND workspaces.owner_actor_id = runs.actor_id
            AND workspaces.owner_run_id IS NULL
            AND EXISTS (
                SELECT 1 FROM actors
                 WHERE actors.id = runs.actor_id
                   AND actors.workspace_id = runs.workspace_id
                   AND actors.current_run_id = runs.id
                   AND actors.state IN ('open', 'closing')
            )
            AND EXISTS (
                SELECT 1
                  FROM run_waits
                  JOIN run_checkpoints
                    ON run_checkpoints.id = run_waits.suspend_checkpoint_id
                   AND run_checkpoints.kind = 'suspend'
                   AND run_checkpoints.run_id = run_waits.run_id
                   AND run_checkpoints.attempt_number = run_waits.attempt_number
                   AND run_checkpoints.run_wait_id = run_waits.id
                   AND run_checkpoints.workspace_id = run_waits.workspace_id
                   AND run_checkpoints.actor_speculative_input_sequence IS NOT NULL
                   AND run_checkpoints.state = 'ready'
                   AND (run_checkpoints.expires_at IS NULL OR run_checkpoints.expires_at > now())
                  JOIN workspace_versions
                    ON workspace_versions.workspace_id = run_checkpoints.workspace_id
                   AND workspace_versions.id = run_checkpoints.private_workspace_version_id
                   AND workspace_versions.state = 'private'
                  JOIN actors AS restore_actor
                    ON restore_actor.id = runs.actor_id
                   AND restore_actor.workspace_id = runs.workspace_id
                   AND restore_actor.current_run_id = runs.id
                   AND restore_actor.state IN ('open', 'closing')
                  JOIN run_attempts AS restore_attempt
                    ON restore_attempt.run_id = runs.id
                   AND restore_attempt.number = runs.current_attempt_number
                   AND restore_attempt.workspace_id = runs.workspace_id
                   AND restore_attempt.entrypoint_kind = 'actor'
                   AND restore_attempt.actor_start_input_sequence = runs.actor_start_input_sequence
                   AND restore_attempt.terminal_at IS NULL
                 WHERE run_waits.run_id = runs.id
                   AND run_waits.suspension_state = 'resume_pending'
                   AND run_waits.handoff_runtime_instance_id IS NULL
                   AND run_waits.handoff_workspace_mount_id IS NULL
                   AND run_waits.handoff_resume_checkpoint_id IS NULL
                   AND runs.actor_start_input_sequence <= runs.actor_start_input_high_watermark
                   AND restore_actor.committed_input_sequence >= runs.actor_start_input_sequence
                   AND restore_actor.committed_input_sequence < restore_actor.next_input_sequence
                   AND run_checkpoints.actor_speculative_input_sequence
                       BETWEEN restore_actor.committed_input_sequence
                           AND restore_actor.next_input_sequence - 1
            ))
       )
       AND (runs.first_lease_at IS NOT NULL OR runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
     GROUP BY runs.org_id, runs.project_id, runs.environment_id, workspaces.region_id,
              coalesce(runs.concurrency_key, ''), runs.queue_name
)
SELECT * FROM candidate_scopes
 WHERE sqlc.arg(after_sort_key)::text = ''
    OR (sort_key, org_id, project_id, environment_id, region_id, concurrency_key, queue_name)
       > (sqlc.arg(after_sort_key)::text, sqlc.arg(after_org_id)::uuid,
          sqlc.arg(after_project_id)::uuid, sqlc.arg(after_environment_id)::uuid,
          sqlc.arg(after_region_id)::text, sqlc.arg(after_concurrency_key)::text,
          sqlc.arg(after_queue_name)::text)
 ORDER BY sort_key, org_id, project_id, environment_id, region_id, concurrency_key, queue_name
 LIMIT sqlc.arg(row_limit);

-- name: ListQueuedRunDispatchCandidatesForScope :many
SELECT runs.org_id, runs.id AS run_id, runs.state_version
  FROM runs
  JOIN workspaces ON workspaces.environment_id = runs.environment_id
                 AND workspaces.id = runs.workspace_id
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.project_id = sqlc.arg(project_id)
   AND runs.environment_id = sqlc.arg(environment_id)
   AND workspaces.region_id = sqlc.arg(region_id)
   AND runs.concurrency_key IS NOT DISTINCT FROM sqlc.narg(concurrency_key)::text
   AND runs.queue_name = sqlc.arg(queue_name)
   AND runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
   AND (
       (runs.entrypoint_kind = 'task'
        AND runs.actor_id IS NULL
        AND runs.cause_kind IN ('api', 'manual', 'schedule', 'child')
        AND (
          (workspaces.owner_run_id = runs.id
           AND workspaces.owner_actor_id IS NULL
           AND (NOT EXISTS (
           SELECT 1
             FROM run_waits
            WHERE run_waits.run_id = runs.id
              AND run_waits.suspension_state IN (
                  'hot', 'checkpointing', 'parked', 'resume_pending', 'resuming'
              )
        ) OR EXISTS (
           SELECT 1
             FROM run_waits
             JOIN run_checkpoints
               ON run_checkpoints.id = run_waits.suspend_checkpoint_id
              AND run_checkpoints.kind = 'suspend'
              AND run_checkpoints.run_id = run_waits.run_id
              AND run_checkpoints.attempt_number = run_waits.attempt_number
              AND run_checkpoints.run_wait_id = run_waits.id
              AND run_checkpoints.workspace_id = run_waits.workspace_id
              AND run_checkpoints.state = 'ready'
              AND (run_checkpoints.expires_at IS NULL OR run_checkpoints.expires_at > now())
             JOIN workspace_versions
               ON workspace_versions.workspace_id = run_checkpoints.workspace_id
              AND workspace_versions.id = run_checkpoints.private_workspace_version_id
              AND workspace_versions.state = 'private'
            WHERE run_waits.run_id = runs.id
              AND run_waits.suspension_state = 'resume_pending'
              AND run_waits.handoff_runtime_instance_id IS NULL
              AND run_waits.handoff_workspace_mount_id IS NULL
              AND run_waits.handoff_resume_checkpoint_id IS NULL
          )))
          OR EXISTS (
              SELECT 1
                FROM run_waits AS handoff
                JOIN runs AS parent
                  ON parent.environment_id = handoff.environment_id
                 AND parent.id = handoff.run_id
                 AND parent.workspace_id = handoff.workspace_id
                 AND parent.status = 'waiting'
                 AND parent.current_run_lease_id IS NULL
                JOIN run_checkpoints AS checkpoint
                  ON checkpoint.id = handoff.suspend_checkpoint_id
                 AND checkpoint.kind = 'suspend'
                 AND checkpoint.run_id = handoff.run_id
                 AND checkpoint.attempt_number = handoff.attempt_number
                 AND checkpoint.run_wait_id = handoff.id
                 AND checkpoint.workspace_id = handoff.workspace_id
                 AND checkpoint.state = 'ready'
                JOIN workspace_versions AS base
                  ON base.workspace_id = handoff.workspace_id
                 AND base.id = handoff.base_workspace_version_id
                 AND base.state = 'private'
               WHERE handoff.child_run_id = runs.id
                 AND handoff.child_parent_owned IS TRUE
                 AND handoff.workspace_id = runs.workspace_id
                 AND handoff.condition_state = 'pending'
                 AND handoff.suspension_state = 'parked'
                 AND handoff.base_workspace_version_id =
                     runs.base_workspace_version_id
                 AND handoff.handoff_runtime_instance_id IS NOT NULL
                 AND handoff.handoff_workspace_mount_id IS NOT NULL
                 AND handoff.handoff_mount_generation IS NOT NULL
                 AND handoff.ownership_generation IS NOT NULL
                 AND handoff.parent_writer_generation IS NOT NULL
                 AND handoff.child_writer_generation IS NULL
          )
        ))
       OR
       (runs.entrypoint_kind = 'actor'
        AND runs.actor_id IS NOT NULL
        AND runs.cause_kind IN ('actor_start', 'continuation')
        AND runs.parent_run_id IS NULL
        AND workspaces.owner_actor_id = runs.actor_id
        AND workspaces.owner_run_id IS NULL
        AND EXISTS (
            SELECT 1 FROM actors
             WHERE actors.id = runs.actor_id
               AND actors.workspace_id = runs.workspace_id
               AND actors.current_run_id = runs.id
               AND actors.state IN ('open', 'closing')
        )
        AND EXISTS (
            SELECT 1
              FROM run_waits
              JOIN run_checkpoints
                ON run_checkpoints.id = run_waits.suspend_checkpoint_id
               AND run_checkpoints.kind = 'suspend'
               AND run_checkpoints.run_id = run_waits.run_id
               AND run_checkpoints.attempt_number = run_waits.attempt_number
               AND run_checkpoints.run_wait_id = run_waits.id
               AND run_checkpoints.workspace_id = run_waits.workspace_id
               AND run_checkpoints.actor_speculative_input_sequence IS NOT NULL
               AND run_checkpoints.state = 'ready'
               AND (run_checkpoints.expires_at IS NULL OR run_checkpoints.expires_at > now())
              JOIN workspace_versions
                ON workspace_versions.workspace_id = run_checkpoints.workspace_id
               AND workspace_versions.id = run_checkpoints.private_workspace_version_id
               AND workspace_versions.state = 'private'
              JOIN actors AS restore_actor
                ON restore_actor.id = runs.actor_id
               AND restore_actor.workspace_id = runs.workspace_id
               AND restore_actor.current_run_id = runs.id
               AND restore_actor.state IN ('open', 'closing')
              JOIN run_attempts AS restore_attempt
                ON restore_attempt.run_id = runs.id
               AND restore_attempt.number = runs.current_attempt_number
               AND restore_attempt.workspace_id = runs.workspace_id
               AND restore_attempt.entrypoint_kind = 'actor'
               AND restore_attempt.actor_start_input_sequence = runs.actor_start_input_sequence
               AND restore_attempt.terminal_at IS NULL
             WHERE run_waits.run_id = runs.id
               AND run_waits.suspension_state = 'resume_pending'
               AND run_waits.handoff_runtime_instance_id IS NULL
               AND run_waits.handoff_workspace_mount_id IS NULL
               AND run_waits.handoff_resume_checkpoint_id IS NULL
               AND runs.actor_start_input_sequence <= runs.actor_start_input_high_watermark
               AND restore_actor.committed_input_sequence >= runs.actor_start_input_sequence
               AND restore_actor.committed_input_sequence < restore_actor.next_input_sequence
               AND run_checkpoints.actor_speculative_input_sequence
                   BETWEEN restore_actor.committed_input_sequence
                       AND restore_actor.next_input_sequence - 1
        ))
   )
   AND (runs.first_lease_at IS NOT NULL OR runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
 ORDER BY runs.queue_score_at, runs.id
 LIMIT sqlc.arg(row_limit);
