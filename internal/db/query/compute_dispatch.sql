-- name: DrainWorkerInstance :one
WITH transitioned AS (
    UPDATE worker_instances
       SET state = 'draining',
           claim_version = worker_instances.claim_version + 1,
           draining_at = COALESCE(draining_at, now()), updated_at = now()
     WHERE worker_instances.id = sqlc.arg(id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(expected_epoch)
       AND worker_instances.claim_version = sqlc.arg(expected_claim_version)
       AND worker_instances.state = 'active'
    RETURNING *
), target AS (
    SELECT transitioned.* FROM transitioned
    UNION ALL
    SELECT worker_instances.*
      FROM worker_instances
     WHERE worker_instances.id = sqlc.arg(id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(expected_epoch)
       AND worker_instances.state = 'draining'
       AND worker_instances.claim_version = sqlc.arg(expected_claim_version) + 1
       AND NOT EXISTS (SELECT 1 FROM transitioned)
), idle_mounts AS (
    UPDATE workspace_mounts
       SET state = 'unmounting',
           finalization_kind = 'discard',
           finalization_reason_code = 'worker_draining',
           finalization_error = NULL,
           stopped_at = COALESCE(stopped_at, now()), updated_at = now()
      FROM target
     WHERE workspace_mounts.worker_instance_id = target.id
       AND workspace_mounts.worker_epoch = target.current_epoch
       AND (
           workspace_mounts.state IN ('mounting', 'mounted')
           OR (
               workspace_mounts.state = 'unmounting'
               AND workspace_mounts.finalization_kind IS NULL
           )
       )
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
), credential_fence AS (
    UPDATE worker_instance_credentials
       SET claim_version = target.claim_version
      FROM target
     WHERE worker_instance_credentials.worker_instance_id = target.id
       AND worker_instance_credentials.revoked_at IS NULL
       AND worker_instance_credentials.claim_version < target.claim_version
    RETURNING worker_instance_credentials.id
)
SELECT target.*
  FROM target
 WHERE (SELECT count(*) FROM idle_mounts) >= 0
   AND (SELECT count(*) FROM idle_runtimes) >= 0
   AND (SELECT count(*) FROM credential_fence) >= 0;

-- name: FenceWorkerInstance :one
WITH target AS (
    UPDATE worker_instances
       SET state = 'lost', claim_version = claim_version + 1,
           lost_at = COALESCE(lost_at, now()), updated_at = now()
     WHERE worker_instances.id = sqlc.arg(id)
       AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_instances.current_epoch = sqlc.arg(expected_epoch)
       AND worker_instances.claim_version = sqlc.arg(expected_claim_version)
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
)
SELECT target.*
  FROM target
 WHERE (SELECT count(*) FROM revoked_credentials) >= 0
   AND (SELECT count(*) FROM lost_mounts) >= 0
   AND (SELECT count(*) FROM lost_runtimes) >= 0
UNION ALL
SELECT worker_instances.*
  FROM worker_instances
 WHERE worker_instances.id = sqlc.arg(id)
   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_instances.current_epoch = sqlc.arg(expected_epoch)
   AND worker_instances.state = 'lost'
   AND worker_instances.claim_version = sqlc.arg(expected_claim_version) + 1
   AND NOT EXISTS (SELECT 1 FROM target)
LIMIT 1;

-- name: GetWorkerInstanceState :one
SELECT worker_instances.*,
       runtime_identities.rootfs_digest,
       runtime_identities.vm_runtime_contract,
       runtime_identities.runtime_arch,
	       COALESCE((
	           worker_instances.state = 'active'
	           AND worker_groups.state = 'active'
           AND worker_instances.observed_at >= transaction_timestamp()
               - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second'
           AND worker_instances.run_paused_reason IS NULL
       ), false)::boolean AS run_ready,
	       COALESCE((
	           worker_instances.state = 'active'
	           AND worker_groups.state = 'active'
           AND worker_instances.observed_at >= transaction_timestamp()
               - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second'
           AND worker_instances.runtime_paused_reason IS NULL
       ), false)::boolean AS runtime_ready,
       COALESCE((
           worker_instances.state = 'active'
           AND worker_groups.state = 'active'
           AND worker_instances.observed_at >= transaction_timestamp()
               - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second'
	           AND worker_instances.run_paused_reason IS NULL
	           AND worker_instances.runtime_paused_reason IS NULL
       ), false)::boolean AS all_configured_roles_ready,
       ((SELECT count(*) FROM run_leases
         WHERE run_leases.worker_instance_id = worker_instances.id
           AND run_leases.worker_epoch = worker_instances.current_epoch
           AND run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')) +
        (SELECT count(*) FROM workspace_mounts
         WHERE workspace_mounts.worker_instance_id = worker_instances.id
           AND workspace_mounts.worker_epoch = worker_instances.current_epoch
           AND workspace_mounts.state IN ('mounting', 'mounted', 'unmounting')) +
        (SELECT count(*) FROM runtime_instances
         WHERE runtime_instances.worker_instance_id = worker_instances.id
           AND runtime_instances.worker_epoch = worker_instances.current_epoch
           AND runtime_instances.observed_state IN ('allocated', 'preparing', 'ready', 'closing')))::int AS active_executions
  FROM worker_instances
  JOIN worker_groups ON worker_groups.id = worker_instances.worker_group_id
  LEFT JOIN runtime_identities ON runtime_identities.id = worker_instances.runtime_identity_id
 WHERE worker_instances.id = sqlc.arg(id)
   AND worker_instances.worker_group_id = sqlc.arg(worker_group_id);

-- name: ListQueuedRunEligibleScopes :many
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
       AND (sqlc.arg(region_filter)::text = '' OR workspaces.region_id = sqlc.arg(region_filter))
       AND runs.current_run_lease_id IS NULL
       AND (runs.next_runtime_preparation_at IS NULL
            OR runs.next_runtime_preparation_at <= transaction_timestamp())
       AND (
           (runs.entrypoint_kind = 'task'
            AND runs.session_id IS NULL
            AND runs.cause_kind IN ('api', 'manual', 'schedule', 'child')
            AND (
              (workspaces.owner_run_id = runs.id
               AND workspaces.owner_session_id IS NULL
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
                     AND (
                         handoff.child_writer_generation IS NULL
                         OR EXISTS (
                             SELECT 1
                               FROM run_leases AS prior_child_lease
                               JOIN workspace_leases AS prior_child_workspace_lease
                                 ON prior_child_workspace_lease.owner_run_lease_id = prior_child_lease.id
                                AND prior_child_workspace_lease.workspace_id = prior_child_lease.workspace_id
                                AND prior_child_workspace_lease.runtime_instance_id = handoff.handoff_runtime_instance_id
                                AND prior_child_workspace_lease.workspace_mount_id = handoff.handoff_workspace_mount_id
                                AND prior_child_workspace_lease.base_version_id = handoff.base_workspace_version_id
                                AND prior_child_workspace_lease.ownership_generation = handoff.ownership_generation
                                AND prior_child_workspace_lease.writer_generation = handoff.child_writer_generation
                                AND prior_child_workspace_lease.state IN ('released', 'fenced')
                              WHERE prior_child_lease.run_id = runs.id
                                AND prior_child_lease.workspace_id = runs.workspace_id
                                AND prior_child_lease.state IN ('failed', 'expired', 'lost', 'rejected')
                         )
                     )
              )
            ))
           OR
           (runs.entrypoint_kind = 'actor'
            AND runs.session_id IS NOT NULL
            AND runs.cause_kind IN ('actor_start', 'continuation')
            AND runs.parent_run_id IS NULL
            AND workspaces.owner_session_id = runs.session_id
            AND workspaces.owner_run_id IS NULL
            AND EXISTS (
                SELECT 1 FROM sessions
                 WHERE sessions.id = runs.session_id
                   AND sessions.workspace_id = runs.workspace_id
                   AND sessions.current_run_id = runs.id
                   AND sessions.state IN ('open', 'closing')
            )
            AND EXISTS (
                SELECT 1
                  FROM run_attempts
                 WHERE run_attempts.run_id = runs.id
                   AND run_attempts.number = runs.current_attempt_number
                   AND run_attempts.workspace_id = runs.workspace_id
                   AND run_attempts.entrypoint_kind = 'actor'
                   AND run_attempts.session_input_start_sequence = runs.session_input_start_sequence
                   AND run_attempts.terminal_at IS NULL
            )
            AND (
                NOT EXISTS (
                    SELECT 1
                      FROM run_waits
                     WHERE run_waits.run_id = runs.id
                       AND run_waits.suspension_state IN (
                           'hot', 'checkpointing', 'parked', 'resume_pending', 'resuming'
                       )
                )
                OR EXISTS (
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
                  JOIN sessions AS restore_actor
                    ON restore_actor.id = runs.session_id
                   AND restore_actor.workspace_id = runs.workspace_id
                   AND restore_actor.current_run_id = runs.id
                   AND restore_actor.state IN ('open', 'closing')
                  JOIN run_attempts AS restore_attempt
                    ON restore_attempt.run_id = runs.id
                   AND restore_attempt.number = runs.current_attempt_number
                   AND restore_attempt.workspace_id = runs.workspace_id
                   AND restore_attempt.entrypoint_kind = 'actor'
                   AND restore_attempt.session_input_start_sequence = runs.session_input_start_sequence
                   AND restore_attempt.terminal_at IS NULL
                 WHERE run_waits.run_id = runs.id
                   AND run_waits.suspension_state = 'resume_pending'
                   AND run_waits.handoff_runtime_instance_id IS NULL
                   AND run_waits.handoff_workspace_mount_id IS NULL
                   AND run_waits.handoff_resume_checkpoint_id IS NULL
                   AND runs.session_input_start_sequence <= runs.session_input_high_watermark
                   AND restore_actor.committed_input_sequence >= runs.session_input_start_sequence
                   AND restore_actor.committed_input_sequence < restore_actor.next_input_sequence
                   AND run_checkpoints.actor_speculative_input_sequence
                       BETWEEN restore_actor.committed_input_sequence
                           AND restore_actor.next_input_sequence - 1
            )))
       )
       AND (runs.first_lease_at IS NOT NULL OR runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
     GROUP BY runs.org_id, runs.project_id, runs.environment_id, workspaces.region_id,
              coalesce(runs.concurrency_key, ''), runs.queue_name
)
SELECT candidate_scopes.*
  FROM candidate_scopes
 WHERE sqlc.arg(after_sort_key)::text = ''
    OR (sort_key, org_id, project_id, environment_id, region_id, concurrency_key, queue_name)
       > (sqlc.arg(after_sort_key)::text, sqlc.arg(after_org_id)::uuid,
          sqlc.arg(after_project_id)::uuid, sqlc.arg(after_environment_id)::uuid,
          sqlc.arg(after_region_id)::text, sqlc.arg(after_concurrency_key)::text,
          sqlc.arg(after_queue_name)::text)
 ORDER BY sort_key, org_id, project_id, environment_id, region_id, concurrency_key, queue_name
 LIMIT sqlc.arg(row_limit);

-- name: ListQueuedRunPlanningUsage :many
WITH input_scopes AS (
    SELECT input_environments.position::bigint AS scope_ordinal,
           input_environments.environment_id,
           input_concurrency_keys.concurrency_key,
           input_queues.queue_name
      FROM unnest(sqlc.arg(environment_ids)::uuid[])
           WITH ORDINALITY AS input_environments(environment_id, position)
      JOIN unnest(sqlc.arg(concurrency_keys)::text[])
           WITH ORDINALITY AS input_concurrency_keys(concurrency_key, position)
        ON input_concurrency_keys.position = input_environments.position
      JOIN unnest(sqlc.arg(queue_names)::text[])
           WITH ORDINALITY AS input_queues(queue_name, position)
        ON input_queues.position = input_environments.position
     WHERE cardinality(sqlc.arg(environment_ids)::uuid[]) BETWEEN 1 AND 128
       AND cardinality(sqlc.arg(concurrency_keys)::text[]) = cardinality(sqlc.arg(environment_ids)::uuid[])
       AND cardinality(sqlc.arg(queue_names)::text[]) = cardinality(sqlc.arg(environment_ids)::uuid[])
), active_usage AS (
    SELECT input_scopes.scope_ordinal,
           count(*)::bigint AS active_runs,
           COALESCE(min(active_runs.queue_concurrency_limit), 0)::bigint AS active_limit
      FROM run_leases
      JOIN runs AS active_runs
        ON active_runs.id = run_leases.run_id
       AND active_runs.environment_id = run_leases.environment_id
      JOIN input_scopes
        ON input_scopes.environment_id = active_runs.environment_id
       AND input_scopes.queue_name = active_runs.queue_name
       AND active_runs.concurrency_key IS NOT DISTINCT FROM
           NULLIF(input_scopes.concurrency_key, '')::text
     WHERE run_leases.state IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
     GROUP BY input_scopes.scope_ordinal
), prepared_usage AS (
    SELECT input_scopes.scope_ordinal,
           count(*)::bigint AS prepared_runs,
           COALESCE(min(prepared_runs.queue_concurrency_limit), 0)::bigint AS prepared_limit
      FROM runtime_instances
      JOIN runs AS prepared_runs
        ON prepared_runs.environment_id = runtime_instances.environment_id
       AND prepared_runs.id = runtime_instances.reserved_run_id
      JOIN input_scopes
        ON input_scopes.environment_id = prepared_runs.environment_id
       AND input_scopes.queue_name = prepared_runs.queue_name
       AND prepared_runs.concurrency_key IS NOT DISTINCT FROM
           NULLIF(input_scopes.concurrency_key, '')::text
     WHERE runtime_instances.reserved_run_id IS NOT NULL
       AND runtime_instances.reclaimed_at IS NULL
     GROUP BY input_scopes.scope_ordinal
)
SELECT input_scopes.scope_ordinal,
       COALESCE(active_usage.active_runs, 0)::bigint AS active_runs,
       COALESCE(active_usage.active_limit, 0)::bigint AS active_limit,
       COALESCE(prepared_usage.prepared_runs, 0)::bigint AS prepared_runs,
       COALESCE(prepared_usage.prepared_limit, 0)::bigint AS prepared_limit
  FROM input_scopes
  LEFT JOIN active_usage USING (scope_ordinal)
  LEFT JOIN prepared_usage USING (scope_ordinal)
 ORDER BY input_scopes.scope_ordinal;

-- name: ListQueuedRunPlacementCandidates :many
WITH input_scopes AS (
    SELECT input_orgs.position::bigint AS scope_ordinal,
           input_orgs.org_id,
           input_environments.environment_id,
           input_concurrency_keys.concurrency_key,
           input_queues.queue_name,
           input_candidate_limits.candidate_limit,
           input_after_set.after_set,
           input_after_scores.queue_score_at AS after_queue_score_at,
           input_after_run_ids.run_id AS after_run_id
      FROM unnest(sqlc.arg(org_ids)::uuid[])
           WITH ORDINALITY AS input_orgs(org_id, position)
      JOIN unnest(sqlc.arg(environment_ids)::uuid[])
           WITH ORDINALITY AS input_environments(environment_id, position)
        ON input_environments.position = input_orgs.position
      JOIN unnest(sqlc.arg(concurrency_keys)::text[])
           WITH ORDINALITY AS input_concurrency_keys(concurrency_key, position)
        ON input_concurrency_keys.position = input_orgs.position
      JOIN unnest(sqlc.arg(queue_names)::text[])
           WITH ORDINALITY AS input_queues(queue_name, position)
        ON input_queues.position = input_orgs.position
      JOIN unnest(sqlc.arg(candidate_limits)::integer[])
           WITH ORDINALITY AS input_candidate_limits(candidate_limit, position)
        ON input_candidate_limits.position = input_orgs.position
      JOIN unnest(sqlc.arg(after_set)::boolean[])
           WITH ORDINALITY AS input_after_set(after_set, position)
        ON input_after_set.position = input_orgs.position
      JOIN unnest(sqlc.arg(after_queue_score_at)::timestamptz[])
           WITH ORDINALITY AS input_after_scores(queue_score_at, position)
        ON input_after_scores.position = input_orgs.position
      JOIN unnest(sqlc.arg(after_run_ids)::uuid[])
           WITH ORDINALITY AS input_after_run_ids(run_id, position)
        ON input_after_run_ids.position = input_orgs.position
     WHERE cardinality(sqlc.arg(org_ids)::uuid[]) > 0
       AND cardinality(sqlc.arg(environment_ids)::uuid[]) = cardinality(sqlc.arg(org_ids)::uuid[])
       AND cardinality(sqlc.arg(concurrency_keys)::text[]) = cardinality(sqlc.arg(org_ids)::uuid[])
       AND cardinality(sqlc.arg(queue_names)::text[]) = cardinality(sqlc.arg(org_ids)::uuid[])
       AND cardinality(sqlc.arg(candidate_limits)::integer[]) = cardinality(sqlc.arg(org_ids)::uuid[])
       AND NOT EXISTS (
           SELECT 1 FROM unnest(sqlc.arg(candidate_limits)::integer[]) AS candidate_limit
            WHERE candidate_limit <= 0
       )
       AND cardinality(sqlc.arg(after_set)::boolean[]) = cardinality(sqlc.arg(org_ids)::uuid[])
       AND cardinality(sqlc.arg(after_queue_score_at)::timestamptz[]) = cardinality(sqlc.arg(org_ids)::uuid[])
       AND cardinality(sqlc.arg(after_run_ids)::uuid[]) = cardinality(sqlc.arg(org_ids)::uuid[])
)
SELECT input_scopes.scope_ordinal,
       candidates.org_id,
       candidates.run_id,
       candidates.state_version,
       candidates.queue_score_at
  FROM input_scopes
 CROSS JOIN LATERAL (
      SELECT runs.org_id,
             runs.id AS run_id,
             runs.state_version,
             runs.queue_score_at
        FROM runs
       WHERE runs.org_id = input_scopes.org_id
         AND runs.environment_id = input_scopes.environment_id
         AND coalesce(runs.concurrency_key, '') = input_scopes.concurrency_key
         AND runs.queue_name = input_scopes.queue_name
         AND runs.status = 'queued'
         AND runs.current_run_lease_id IS NULL
         AND (runs.next_runtime_preparation_at IS NULL
              OR runs.next_runtime_preparation_at <= transaction_timestamp())
         AND (runs.first_lease_at IS NOT NULL OR runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
         AND (
             NOT input_scopes.after_set
             OR (runs.queue_score_at, runs.id)
                > (input_scopes.after_queue_score_at, input_scopes.after_run_id)
         )
       ORDER BY runs.queue_score_at, runs.id
       LIMIT input_scopes.candidate_limit
  ) AS candidates
 ORDER BY input_scopes.scope_ordinal, candidates.queue_score_at, candidates.run_id;

-- name: ListQueuedRunPlanningCandidatesForScopes :many
WITH input_scopes AS (
    SELECT input_orgs.position::bigint AS scope_ordinal,
           input_orgs.org_id,
           input_projects.project_id,
           input_environments.environment_id,
           input_regions.region_id,
           input_concurrency_keys.concurrency_key,
           input_queues.queue_name
      FROM unnest(sqlc.arg(org_ids)::uuid[])
           WITH ORDINALITY AS input_orgs(org_id, position)
      JOIN unnest(sqlc.arg(project_ids)::uuid[])
           WITH ORDINALITY AS input_projects(project_id, position)
        ON input_projects.position = input_orgs.position
      JOIN unnest(sqlc.arg(environment_ids)::uuid[])
           WITH ORDINALITY AS input_environments(environment_id, position)
        ON input_environments.position = input_orgs.position
      JOIN unnest(sqlc.arg(region_ids)::text[])
           WITH ORDINALITY AS input_regions(region_id, position)
        ON input_regions.position = input_orgs.position
      JOIN unnest(sqlc.arg(concurrency_keys)::text[])
           WITH ORDINALITY AS input_concurrency_keys(concurrency_key, position)
        ON input_concurrency_keys.position = input_orgs.position
      JOIN unnest(sqlc.arg(queue_names)::text[])
           WITH ORDINALITY AS input_queues(queue_name, position)
        ON input_queues.position = input_orgs.position
     WHERE cardinality(sqlc.arg(org_ids)::uuid[]) BETWEEN 1 AND 32
       AND cardinality(sqlc.arg(project_ids)::uuid[]) = cardinality(sqlc.arg(org_ids)::uuid[])
       AND cardinality(sqlc.arg(environment_ids)::uuid[]) = cardinality(sqlc.arg(org_ids)::uuid[])
       AND cardinality(sqlc.arg(region_ids)::text[]) = cardinality(sqlc.arg(org_ids)::uuid[])
       AND cardinality(sqlc.arg(concurrency_keys)::text[]) = cardinality(sqlc.arg(org_ids)::uuid[])
       AND cardinality(sqlc.arg(queue_names)::text[]) = cardinality(sqlc.arg(org_ids)::uuid[])
)
SELECT input_scopes.scope_ordinal,
       candidates.org_id,
       candidates.run_id,
       candidates.state_version,
       candidates.queue_concurrency_limit,
       candidates.workspace_manifest,
       candidates.requires_retained_runtime,
       candidates.retained_worker_pool_id,
       candidates.required_worker_group_id,
       candidates.required_runtime_identity_id,
       candidates.required_vm_vcpu_count,
       candidates.required_cpu_config_digest,
       candidates.required_cpu_millis,
       candidates.required_memory_bytes,
       candidates.required_guest_ephemeral_disk_bytes,
       candidates.required_substrate_format,
       candidates.required_substrate_contract
  FROM input_scopes
 CROSS JOIN LATERAL (
SELECT runs.org_id,
       runs.id AS run_id,
       runs.state_version,
       runs.queue_concurrency_limit,
       workspace_definitions.manifest AS workspace_manifest,
       (EXISTS (
           SELECT 1
             FROM run_waits
            WHERE run_waits.run_id = runs.id
              AND run_waits.attempt_number = runs.current_attempt_number
              AND run_waits.workspace_id = runs.workspace_id
              AND run_waits.suspension_state IN ('parked', 'resume_pending')
              AND run_waits.handoff_runtime_instance_id IS NOT NULL
       ) OR EXISTS (
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
              AND handoff.base_workspace_version_id = runs.base_workspace_version_id
              AND handoff.handoff_runtime_instance_id IS NOT NULL
              AND handoff.handoff_workspace_mount_id IS NOT NULL
              AND handoff.handoff_mount_generation IS NOT NULL
              AND handoff.ownership_generation IS NOT NULL
              AND handoff.parent_writer_generation IS NOT NULL
              AND (
                  handoff.child_writer_generation IS NULL
                  OR EXISTS (
                      SELECT 1
                        FROM run_leases AS prior_child_lease
                        JOIN workspace_leases AS prior_child_workspace_lease
                          ON prior_child_workspace_lease.owner_run_lease_id = prior_child_lease.id
                         AND prior_child_workspace_lease.workspace_id = prior_child_lease.workspace_id
                         AND prior_child_workspace_lease.runtime_instance_id = handoff.handoff_runtime_instance_id
                         AND prior_child_workspace_lease.workspace_mount_id = handoff.handoff_workspace_mount_id
                         AND prior_child_workspace_lease.base_version_id = handoff.base_workspace_version_id
                         AND prior_child_workspace_lease.ownership_generation = handoff.ownership_generation
                         AND prior_child_workspace_lease.writer_generation = handoff.child_writer_generation
                         AND prior_child_workspace_lease.state IN ('released', 'fenced')
                       WHERE prior_child_lease.run_id = runs.id
                         AND prior_child_lease.workspace_id = runs.workspace_id
                         AND prior_child_lease.state IN ('failed', 'expired', 'lost', 'rejected')
                  )
              )
       ))::boolean AS requires_retained_runtime,
       retained_capacity.worker_pool_id AS retained_worker_pool_id,
       COALESCE(capacity_restore.worker_group_id, '') AS required_worker_group_id,
       COALESCE(capacity_restore.runtime_identity_id, '') AS required_runtime_identity_id,
       COALESCE(capacity_restore.vm_vcpu_count, 0)::integer AS required_vm_vcpu_count,
       COALESCE(capacity_restore.cpu_config_digest, '') AS required_cpu_config_digest,
       COALESCE(capacity_restore.requested_cpu_millis, 0)::bigint AS required_cpu_millis,
       COALESCE(capacity_restore.requested_memory_bytes, 0)::bigint AS required_memory_bytes,
       COALESCE(capacity_restore.requested_guest_ephemeral_disk_bytes, 0)::bigint AS required_guest_ephemeral_disk_bytes,
       COALESCE(capacity_restore.substrate_format, '') AS required_substrate_format,
       COALESCE(capacity_restore.substrate_contract, '') AS required_substrate_contract,
       runs.queue_score_at AS candidate_score_at
  FROM runs
  JOIN workspaces ON workspaces.environment_id = runs.environment_id
                 AND workspaces.id = runs.workspace_id
  JOIN deployment_definitions AS workspace_definitions
    ON workspace_definitions.environment_id = workspaces.environment_id
   AND workspace_definitions.id = workspaces.deployment_definition_id
   AND workspace_definitions.kind = 'sandbox'
  LEFT JOIN LATERAL (
      SELECT worker_instances.worker_pool_id
        FROM runtime_instances
        JOIN worker_instances
          ON worker_instances.id = runtime_instances.worker_instance_id
         AND worker_instances.worker_group_id = runtime_instances.worker_group_id
       WHERE runtime_instances.id = (
           SELECT retained.runtime_instance_id
             FROM (
                 SELECT run_waits.handoff_runtime_instance_id AS runtime_instance_id,
                        0 AS precedence
                   FROM run_waits
                  WHERE run_waits.run_id = runs.id
                    AND run_waits.attempt_number = runs.current_attempt_number
                    AND run_waits.workspace_id = runs.workspace_id
                    AND run_waits.suspension_state IN ('parked', 'resume_pending')
                    AND run_waits.handoff_runtime_instance_id IS NOT NULL
                 UNION ALL
                 SELECT handoff.handoff_runtime_instance_id,
                        1 AS precedence
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
                    AND handoff.base_workspace_version_id = runs.base_workspace_version_id
                    AND handoff.handoff_runtime_instance_id IS NOT NULL
                    AND handoff.handoff_workspace_mount_id IS NOT NULL
                    AND handoff.handoff_mount_generation IS NOT NULL
                    AND handoff.ownership_generation IS NOT NULL
                    AND handoff.parent_writer_generation IS NOT NULL
                    AND (
                        handoff.child_writer_generation IS NULL
                        OR EXISTS (
                            SELECT 1
                              FROM run_leases AS prior_child_lease
                              JOIN workspace_leases AS prior_child_workspace_lease
                                ON prior_child_workspace_lease.owner_run_lease_id = prior_child_lease.id
                               AND prior_child_workspace_lease.workspace_id = prior_child_lease.workspace_id
                               AND prior_child_workspace_lease.runtime_instance_id = handoff.handoff_runtime_instance_id
                               AND prior_child_workspace_lease.workspace_mount_id = handoff.handoff_workspace_mount_id
                               AND prior_child_workspace_lease.base_version_id = handoff.base_workspace_version_id
                               AND prior_child_workspace_lease.ownership_generation = handoff.ownership_generation
                               AND prior_child_workspace_lease.writer_generation = handoff.child_writer_generation
                               AND prior_child_workspace_lease.state IN ('released', 'fenced')
                             WHERE prior_child_lease.run_id = runs.id
                               AND prior_child_lease.workspace_id = runs.workspace_id
                               AND prior_child_lease.state IN ('failed', 'expired', 'lost', 'rejected')
                        )
                    )
             ) AS retained
            ORDER BY retained.precedence, retained.runtime_instance_id
            LIMIT 1
       )
       LIMIT 1
  ) AS retained_capacity ON true
  LEFT JOIN LATERAL (
      SELECT source_lease.worker_group_id,
             source_lease.requested_cpu_millis,
             source_lease.requested_memory_bytes,
             source_lease.requested_guest_ephemeral_disk_bytes,
             source_runtime.runtime_identity_id,
             source_runtime.vm_vcpu_count,
             source_runtime.cpu_config_digest,
             runtime_substrates.substrate_format,
             runtime_substrates.substrate_contract
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
        JOIN run_leases AS source_lease
          ON source_lease.id = run_checkpoints.source_run_lease_id
         AND source_lease.run_id = run_checkpoints.run_id
         AND source_lease.attempt_number = run_checkpoints.attempt_number
         AND source_lease.workspace_id = run_checkpoints.workspace_id
         AND source_lease.state = 'checkpointed'
        JOIN runtime_instances AS source_runtime
          ON source_runtime.id = source_lease.runtime_instance_id
         AND source_runtime.workspace_id = run_checkpoints.workspace_id
         AND source_runtime.runtime_identity_id = source_lease.runtime_identity_id
        JOIN runtime_substrates
          ON runtime_substrates.id = source_runtime.runtime_substrate_id
         AND runtime_substrates.org_id = source_runtime.org_id
         AND runtime_substrates.project_id = source_runtime.project_id
         AND runtime_substrates.environment_id = source_runtime.environment_id
         AND runtime_substrates.deployment_definition_id = source_runtime.deployment_definition_id
       WHERE run_waits.run_id = runs.id
         AND run_waits.attempt_number = runs.current_attempt_number
         AND run_waits.workspace_id = runs.workspace_id
         AND run_waits.suspension_state = 'resume_pending'
         AND run_waits.handoff_runtime_instance_id IS NULL
         AND run_waits.handoff_workspace_mount_id IS NULL
         AND run_waits.handoff_resume_checkpoint_id IS NULL
       ORDER BY run_waits.id
       LIMIT 1
  ) AS capacity_restore ON true
 WHERE runs.org_id = input_scopes.org_id
   AND runs.project_id = input_scopes.project_id
   AND runs.environment_id = input_scopes.environment_id
   AND workspaces.region_id = input_scopes.region_id
   AND runs.concurrency_key IS NOT DISTINCT FROM NULLIF(input_scopes.concurrency_key, '')::text
   AND runs.queue_name = input_scopes.queue_name
   AND runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
   AND (runs.next_runtime_preparation_at IS NULL
        OR runs.next_runtime_preparation_at <= transaction_timestamp())
   AND (
       (runs.entrypoint_kind = 'task'
        AND runs.session_id IS NULL
        AND runs.cause_kind IN ('api', 'manual', 'schedule', 'child')
        AND (
          (workspaces.owner_run_id = runs.id
           AND workspaces.owner_session_id IS NULL
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
                 AND (
                     handoff.child_writer_generation IS NULL
                     OR EXISTS (
                         SELECT 1
                           FROM run_leases AS prior_child_lease
                           JOIN workspace_leases AS prior_child_workspace_lease
                             ON prior_child_workspace_lease.owner_run_lease_id = prior_child_lease.id
                            AND prior_child_workspace_lease.workspace_id = prior_child_lease.workspace_id
                            AND prior_child_workspace_lease.runtime_instance_id = handoff.handoff_runtime_instance_id
                            AND prior_child_workspace_lease.workspace_mount_id = handoff.handoff_workspace_mount_id
                            AND prior_child_workspace_lease.base_version_id = handoff.base_workspace_version_id
                            AND prior_child_workspace_lease.ownership_generation = handoff.ownership_generation
                            AND prior_child_workspace_lease.writer_generation = handoff.child_writer_generation
                            AND prior_child_workspace_lease.state IN ('released', 'fenced')
                          WHERE prior_child_lease.run_id = runs.id
                            AND prior_child_lease.workspace_id = runs.workspace_id
                            AND prior_child_lease.state IN ('failed', 'expired', 'lost', 'rejected')
                     )
                 )
          )
        ))
       OR
       (runs.entrypoint_kind = 'actor'
        AND runs.session_id IS NOT NULL
        AND runs.cause_kind IN ('actor_start', 'continuation')
        AND runs.parent_run_id IS NULL
        AND workspaces.owner_session_id = runs.session_id
        AND workspaces.owner_run_id IS NULL
        AND EXISTS (
            SELECT 1 FROM sessions
             WHERE sessions.id = runs.session_id
               AND sessions.workspace_id = runs.workspace_id
               AND sessions.current_run_id = runs.id
               AND sessions.state IN ('open', 'closing')
        )
        AND EXISTS (
            SELECT 1
              FROM run_attempts
             WHERE run_attempts.run_id = runs.id
               AND run_attempts.number = runs.current_attempt_number
               AND run_attempts.workspace_id = runs.workspace_id
               AND run_attempts.entrypoint_kind = 'actor'
               AND run_attempts.session_input_start_sequence = runs.session_input_start_sequence
               AND run_attempts.terminal_at IS NULL
        )
        AND (
            NOT EXISTS (
                SELECT 1
                  FROM run_waits
                 WHERE run_waits.run_id = runs.id
                   AND run_waits.suspension_state IN (
                       'hot', 'checkpointing', 'parked', 'resume_pending', 'resuming'
                   )
            )
            OR EXISTS (
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
              JOIN sessions AS restore_actor
                ON restore_actor.id = runs.session_id
               AND restore_actor.workspace_id = runs.workspace_id
               AND restore_actor.current_run_id = runs.id
               AND restore_actor.state IN ('open', 'closing')
              JOIN run_attempts AS restore_attempt
                ON restore_attempt.run_id = runs.id
               AND restore_attempt.number = runs.current_attempt_number
               AND restore_attempt.workspace_id = runs.workspace_id
               AND restore_attempt.entrypoint_kind = 'actor'
               AND restore_attempt.session_input_start_sequence = runs.session_input_start_sequence
               AND restore_attempt.terminal_at IS NULL
             WHERE run_waits.run_id = runs.id
               AND run_waits.suspension_state = 'resume_pending'
               AND run_waits.handoff_runtime_instance_id IS NULL
               AND run_waits.handoff_workspace_mount_id IS NULL
               AND run_waits.handoff_resume_checkpoint_id IS NULL
               AND runs.session_input_start_sequence <= runs.session_input_high_watermark
               AND restore_actor.committed_input_sequence >= runs.session_input_start_sequence
               AND restore_actor.committed_input_sequence < restore_actor.next_input_sequence
               AND run_checkpoints.actor_speculative_input_sequence
                   BETWEEN restore_actor.committed_input_sequence
                       AND restore_actor.next_input_sequence - 1
        )))
 )
   AND (runs.first_lease_at IS NOT NULL OR runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
 ORDER BY runs.queue_score_at, runs.id
 LIMIT sqlc.arg(per_scope_limit)
 ) AS candidates
 ORDER BY input_scopes.scope_ordinal, candidates.candidate_score_at, candidates.run_id;
