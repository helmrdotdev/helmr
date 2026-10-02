-- name: DrainWorkerHost :one
WITH transitioned AS (
    UPDATE worker_hosts
       SET status = 'draining',
           claim_version = worker_hosts.claim_version + 1,
           draining_at = COALESCE(draining_at, now()),
           drain_reason = sqlc.arg(drain_reason)::text, updated_at = now()
     WHERE worker_hosts.id = sqlc.arg(id)
       AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_hosts.current_epoch = sqlc.arg(expected_epoch)
       AND worker_hosts.claim_version = sqlc.arg(expected_claim_version)
       AND worker_hosts.status = 'active'
    RETURNING *
), target AS (
    SELECT transitioned.* FROM transitioned
    UNION ALL
    SELECT worker_hosts.*
      FROM worker_hosts
     WHERE worker_hosts.id = sqlc.arg(id)
       AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_hosts.current_epoch = sqlc.arg(expected_epoch)
       AND worker_hosts.status = 'draining'
       AND worker_hosts.claim_version IN (sqlc.arg(expected_claim_version), sqlc.arg(expected_claim_version) + 1)
       AND NOT EXISTS (SELECT 1 FROM transitioned)
), draining_instances AS (
    UPDATE computer_instances i SET admission_state='draining',updated_at=now()
    FROM target WHERE i.worker_host_id=target.id AND i.worker_epoch=target.current_epoch
      AND i.reclaimed_at IS NULL AND i.admission_state='open'
    RETURNING i.id
), host_secret_fence AS (
    UPDATE worker_host_secrets
       SET claim_version = target.claim_version
      FROM target
     WHERE worker_host_secrets.worker_host_id = target.id
       AND worker_host_secrets.revoked_at IS NULL
       AND worker_host_secrets.claim_version < target.claim_version
    RETURNING worker_host_secrets.id
)
SELECT target.*
  FROM target
 WHERE (SELECT count(*) FROM draining_instances) >= 0
   AND (SELECT count(*) FROM host_secret_fence) >= 0;

-- name: FenceWorkerHost :one
WITH target AS (
    UPDATE worker_hosts
       SET status = 'lost', claim_version = claim_version + 1,
           lost_at = COALESCE(lost_at, now()), updated_at = now()
     WHERE worker_hosts.id = sqlc.arg(id)
       AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
       AND worker_hosts.current_epoch = sqlc.arg(expected_epoch)
       AND worker_hosts.claim_version = sqlc.arg(expected_claim_version)
       AND worker_hosts.status IN ('active', 'draining')
    RETURNING *
), revoked_host_secrets AS (
    UPDATE worker_host_secrets
       SET revoked_at = COALESCE(revoked_at, now())
      FROM target
     WHERE worker_host_secrets.worker_host_id = target.id
       AND worker_host_secrets.revoked_at IS NULL
    RETURNING worker_host_secrets.id
), lost_instances AS (
    UPDATE computer_instances
       SET observed_state = 'lost', observed_version = observed_version + 1,
           observed_at = now(), terminal_at = now(),
           terminal_reason_code = sqlc.arg(reason_code),
           mount_state='lost', admission_state='closed', updated_at=now()
      FROM target
     WHERE computer_instances.worker_host_id = target.id
       AND computer_instances.worker_epoch = target.current_epoch
       AND computer_instances.reclaimed_at IS NULL
       AND computer_instances.observed_state IN ('allocated', 'ready')
    RETURNING computer_instances.id
)
SELECT target.*
  FROM target
 WHERE (SELECT count(*) FROM revoked_host_secrets) >= 0
   AND (SELECT count(*) FROM lost_instances) >= 0
UNION ALL
SELECT worker_hosts.*
  FROM worker_hosts
 WHERE worker_hosts.id = sqlc.arg(id)
   AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id)
   AND worker_hosts.current_epoch = sqlc.arg(expected_epoch)
   AND worker_hosts.status = 'lost'
   AND worker_hosts.claim_version = sqlc.arg(expected_claim_version) + 1
   AND NOT EXISTS (SELECT 1 FROM target)
LIMIT 1;

-- name: GetWorkerHostStatus :one
SELECT worker_hosts.*,
       vm_platforms.rootfs_digest,
       vm_platforms.contract,
       vm_platforms.arch,
	       COALESCE((
	           worker_hosts.status = 'active'
	           AND worker_groups.status = 'active'
           AND worker_hosts.observed_at >= transaction_timestamp()
               - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second'
           AND worker_hosts.run_paused_reason IS NULL
       ), false)::boolean AS run_ready,
	       COALESCE((
	           worker_hosts.status = 'active'
	           AND worker_groups.status = 'active'
           AND worker_hosts.observed_at >= transaction_timestamp()
               - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second'
           AND worker_hosts.vm_paused_reason IS NULL
       ), false)::boolean AS instance_ready,
       COALESCE((
           worker_hosts.status = 'active'
           AND worker_groups.status = 'active'
           AND worker_hosts.observed_at >= transaction_timestamp()
               - sqlc.arg(observation_freshness_seconds)::bigint * interval '1 second'
	           AND worker_hosts.run_paused_reason IS NULL
	           AND worker_hosts.vm_paused_reason IS NULL
       ), false)::boolean AS all_configured_roles_ready,
       (SELECT count(*)::int FROM computer_instances i
          WHERE i.worker_host_id=worker_hosts.id AND i.worker_epoch=worker_hosts.current_epoch
            AND i.reclaimed_at IS NULL) AS active_instances
  FROM worker_hosts
  JOIN worker_groups ON worker_groups.id = worker_hosts.worker_group_id
  LEFT JOIN vm_platforms ON vm_platforms.id = worker_hosts.vm_platform_id
 WHERE worker_hosts.id = sqlc.arg(id)
   AND worker_hosts.worker_group_id = sqlc.arg(worker_group_id);

-- name: ListQueuedRunEligibleScopes :many
WITH candidate_scopes AS (
    SELECT runs.org_id, runs.project_id, runs.environment_id, computers.region_id,
           coalesce(runs.concurrency_key, '') AS concurrency_key, runs.queue_name,
           md5(runs.org_id::text || ':' || runs.project_id::text || ':' ||
               runs.environment_id::text || ':' || computers.region_id || ':' ||
               coalesce(runs.concurrency_key, '') || ':' || runs.queue_name || ':' || sqlc.arg(scan_seed)::text) AS sort_key
      FROM runs
      JOIN computers ON computers.environment_id = runs.environment_id
                     AND computers.id = runs.computer_id
     WHERE runs.status = 'queued'
       AND (sqlc.arg(region_filter)::text = '' OR computers.region_id = sqlc.arg(region_filter))
       AND runs.current_run_lease_id IS NULL
       AND (runs.next_instance_preparation_at IS NULL
            OR runs.next_instance_preparation_at <= transaction_timestamp())

       AND computers.status='active' AND computers.desired_state='active'
       AND computers.deleted_at IS NULL AND computers.recovery_failure IS NULL AND computers.preparation_failure IS NULL
       AND computers.dirty_state NOT IN ('dirty_state_lost')
       AND runs.active_elapsed_ms < runs.max_active_duration_ms
       AND EXISTS(SELECT 1 FROM run_attempts a WHERE a.run_id=runs.id
         AND a.number=runs.current_attempt_number AND a.terminal_at IS NULL)
       AND (runs.entrypoint_kind='task' OR EXISTS(SELECT 1 FROM sessions a
         WHERE a.id=runs.session_id AND a.current_run_id=runs.id AND a.computer_id=runs.computer_id
           AND a.status IN ('open','closing') AND a.cancel_requested_at IS NULL
           AND a.dispatch_hold_id IS NULL))
       AND (runs.parent_owns_lifecycle IS NOT TRUE OR EXISTS(SELECT 1 FROM runs parent
         WHERE parent.id=runs.parent_run_id AND parent.status IN ('queued','running','waiting','retry_delayed')))
       AND (
         NOT EXISTS(SELECT 1 FROM run_waits w WHERE w.run_id=runs.id
           AND w.attempt_number=runs.current_attempt_number
           AND w.suspension_status IN ('hot','checkpointing','parked','resume_pending','resuming'))
         OR EXISTS(SELECT 1 FROM run_waits w
           JOIN computer_checkpoint_runs m ON m.checkpoint_id=w.suspend_checkpoint_id AND m.run_wait_id=w.id
             AND m.run_id=runs.id AND m.attempt_number=runs.current_attempt_number
           JOIN computer_checkpoints c ON c.id=m.checkpoint_id AND c.computer_id=runs.computer_id
           JOIN computer_disk_versions d ON d.id=c.private_computer_disk_version_id AND d.status='private'
           JOIN run_leases l ON l.id=m.source_run_lease_id AND l.status='checkpointed'
           WHERE w.run_id=runs.id AND w.suspension_status='resume_pending'
             AND c.status='ready' AND c.resume_committed_at IS NULL
             AND (c.expires_at IS NULL OR c.expires_at>clock_timestamp())
             AND (runs.session_id IS NULL OR EXISTS(SELECT 1 FROM sessions a WHERE a.id=runs.session_id
               AND m.actor_speculative_input_sequence BETWEEN a.committed_input_sequence AND a.next_input_sequence-1)))
       )
       AND (runs.first_lease_at IS NOT NULL OR runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
     GROUP BY runs.org_id, runs.project_id, runs.environment_id, computers.region_id,
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
     WHERE run_leases.status IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')
     GROUP BY input_scopes.scope_ordinal
)
SELECT input_scopes.scope_ordinal,
       COALESCE(active_usage.active_runs,0)::bigint AS active_runs,
       COALESCE(active_usage.active_limit,0)::bigint AS active_limit
FROM input_scopes LEFT JOIN active_usage USING(scope_ordinal)
ORDER BY input_scopes.scope_ordinal;

-- name: ListQueuedRunDispatchCandidates :many
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
       candidates.revision,
       candidates.queue_score_at
  FROM input_scopes
 CROSS JOIN LATERAL (
      SELECT runs.org_id,
             runs.id AS run_id,
             runs.revision,
             runs.queue_score_at
        FROM runs
        JOIN computers ON computers.id=runs.computer_id AND computers.environment_id=runs.environment_id
       WHERE runs.org_id = input_scopes.org_id
         AND runs.environment_id = input_scopes.environment_id
         AND coalesce(runs.concurrency_key, '') = input_scopes.concurrency_key
         AND runs.queue_name = input_scopes.queue_name
         AND runs.status = 'queued'
         AND runs.current_run_lease_id IS NULL

       AND computers.status='active' AND computers.desired_state='active'
       AND computers.deleted_at IS NULL AND computers.recovery_failure IS NULL AND computers.preparation_failure IS NULL
       AND computers.dirty_state NOT IN ('dirty_state_lost')
       AND runs.active_elapsed_ms < runs.max_active_duration_ms
       AND EXISTS(SELECT 1 FROM run_attempts a WHERE a.run_id=runs.id
         AND a.number=runs.current_attempt_number AND a.terminal_at IS NULL)
       AND (runs.entrypoint_kind='task' OR EXISTS(SELECT 1 FROM sessions a
         WHERE a.id=runs.session_id AND a.current_run_id=runs.id AND a.computer_id=runs.computer_id
           AND a.status IN ('open','closing') AND a.cancel_requested_at IS NULL
           AND a.dispatch_hold_id IS NULL))
       AND (runs.parent_owns_lifecycle IS NOT TRUE OR EXISTS(SELECT 1 FROM runs parent
         WHERE parent.id=runs.parent_run_id AND parent.status IN ('queued','running','waiting','retry_delayed')))
       AND (
         NOT EXISTS(SELECT 1 FROM run_waits w WHERE w.run_id=runs.id
           AND w.attempt_number=runs.current_attempt_number
           AND w.suspension_status IN ('hot','checkpointing','parked','resume_pending','resuming'))
         OR EXISTS(SELECT 1 FROM run_waits w
           JOIN computer_checkpoint_runs m ON m.checkpoint_id=w.suspend_checkpoint_id AND m.run_wait_id=w.id
             AND m.run_id=runs.id AND m.attempt_number=runs.current_attempt_number
           JOIN computer_checkpoints c ON c.id=m.checkpoint_id AND c.computer_id=runs.computer_id
           JOIN computer_disk_versions d ON d.id=c.private_computer_disk_version_id AND d.status='private'
           JOIN run_leases l ON l.id=m.source_run_lease_id AND l.status='checkpointed'
           WHERE w.run_id=runs.id AND w.suspension_status='resume_pending'
             AND c.status='ready' AND c.resume_committed_at IS NULL
             AND (c.expires_at IS NULL OR c.expires_at>clock_timestamp())
             AND (runs.session_id IS NULL OR EXISTS(SELECT 1 FROM sessions a WHERE a.id=runs.session_id
               AND m.actor_speculative_input_sequence BETWEEN a.committed_input_sequence AND a.next_input_sequence-1)))
       )
         AND (runs.next_instance_preparation_at IS NULL
              OR runs.next_instance_preparation_at <= transaction_timestamp())
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
     WHERE cardinality(sqlc.arg(org_ids)::uuid[]) BETWEEN 1 AND 128
       AND cardinality(sqlc.arg(project_ids)::uuid[]) = cardinality(sqlc.arg(org_ids)::uuid[])
       AND cardinality(sqlc.arg(environment_ids)::uuid[]) = cardinality(sqlc.arg(org_ids)::uuid[])
       AND cardinality(sqlc.arg(region_ids)::text[]) = cardinality(sqlc.arg(org_ids)::uuid[])
       AND cardinality(sqlc.arg(concurrency_keys)::text[]) = cardinality(sqlc.arg(org_ids)::uuid[])
       AND cardinality(sqlc.arg(queue_names)::text[]) = cardinality(sqlc.arg(org_ids)::uuid[])
)
SELECT input_scopes.scope_ordinal,
       candidates.org_id,
       candidates.run_id,
       candidates.computer_id,
       candidates.accounted_pool_ids,
       candidates.revision,
       candidates.queue_concurrency_limit,
       candidates.computer_config,
       candidates.required_worker_group_id,
       candidates.required_vm_platform_id,
       candidates.required_vm_vcpu_count,
       candidates.required_cpu_config_digest,
       candidates.required_cpu_millis,
       candidates.required_memory_bytes,
       candidates.required_guest_ephemeral_disk_bytes
  FROM input_scopes
 CROSS JOIN LATERAL (
SELECT runs.org_id,
       runs.id AS run_id,
       runs.computer_id,
       ARRAY(SELECT h.worker_pool_id FROM computer_instances live
         JOIN worker_hosts h ON h.id=live.worker_host_id
         WHERE live.computer_id=runs.computer_id AND live.reclaimed_at IS NULL)::uuid[] AS accounted_pool_ids,
       runs.revision,
       runs.queue_concurrency_limit,
       computer_specs.config AS computer_config,
       capacity_restore.worker_group_id AS required_worker_group_id,
       COALESCE(capacity_restore.vm_platform_id, '') AS required_vm_platform_id,
       COALESCE(capacity_restore.vm_vcpu_count, 0)::integer AS required_vm_vcpu_count,
       COALESCE(capacity_restore.cpu_config_digest, '') AS required_cpu_config_digest,
       COALESCE(capacity_restore.requested_cpu_millis, 0)::bigint AS required_cpu_millis,
       COALESCE(capacity_restore.requested_memory_bytes, 0)::bigint AS required_memory_bytes,
       COALESCE(capacity_restore.requested_guest_ephemeral_disk_bytes, 0)::bigint AS required_guest_ephemeral_disk_bytes,
       runs.queue_score_at AS candidate_score_at
  FROM runs
  JOIN computers ON computers.environment_id = runs.environment_id
                 AND computers.id = runs.computer_id
  JOIN computer_specs
    ON computer_specs.environment_id = computers.environment_id
   AND computer_specs.id = computers.computer_spec_id
  LEFT JOIN LATERAL (
      SELECT i.worker_group_id,i.reserved_cpu_millis AS requested_cpu_millis,
             i.reserved_memory_bytes AS requested_memory_bytes,
             i.reserved_guest_ephemeral_disk_bytes AS requested_guest_ephemeral_disk_bytes,
             i.vm_platform_id,i.vm_vcpu_count,i.cpu_config_digest
      FROM computer_checkpoints c
      JOIN computer_instances i ON i.id=c.source_computer_instance_id
      WHERE c.computer_id=runs.computer_id AND c.status='ready' AND c.resume_committed_at IS NULL
        AND (c.expires_at IS NULL OR c.expires_at>clock_timestamp())
      ORDER BY c.created_at DESC,c.id DESC LIMIT 1
  ) AS capacity_restore ON true
 WHERE runs.org_id = input_scopes.org_id
   AND (get_byte(uuid_send(runs.org_id), 15) & 63) =
       (get_byte(uuid_send(input_scopes.org_id), 15) & 63)
   AND runs.project_id = input_scopes.project_id
   AND runs.environment_id = input_scopes.environment_id
   AND computers.region_id = input_scopes.region_id
   AND coalesce(runs.concurrency_key, '') = input_scopes.concurrency_key
   AND runs.queue_name = input_scopes.queue_name
   AND runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
   AND (runs.next_instance_preparation_at IS NULL
        OR runs.next_instance_preparation_at <= transaction_timestamp())

       AND computers.status='active' AND computers.desired_state='active'
       AND computers.deleted_at IS NULL AND computers.recovery_failure IS NULL AND computers.preparation_failure IS NULL
       AND computers.dirty_state NOT IN ('dirty_state_lost')
       AND runs.active_elapsed_ms < runs.max_active_duration_ms
       AND EXISTS(SELECT 1 FROM run_attempts a WHERE a.run_id=runs.id
         AND a.number=runs.current_attempt_number AND a.terminal_at IS NULL)
       AND (runs.entrypoint_kind='task' OR EXISTS(SELECT 1 FROM sessions a
         WHERE a.id=runs.session_id AND a.current_run_id=runs.id AND a.computer_id=runs.computer_id
           AND a.status IN ('open','closing') AND a.cancel_requested_at IS NULL
           AND a.dispatch_hold_id IS NULL))
       AND (runs.parent_owns_lifecycle IS NOT TRUE OR EXISTS(SELECT 1 FROM runs parent
         WHERE parent.id=runs.parent_run_id AND parent.status IN ('queued','running','waiting','retry_delayed')))
       AND (
         NOT EXISTS(SELECT 1 FROM run_waits w WHERE w.run_id=runs.id
           AND w.attempt_number=runs.current_attempt_number
           AND w.suspension_status IN ('hot','checkpointing','parked','resume_pending','resuming'))
         OR EXISTS(SELECT 1 FROM run_waits w
           JOIN computer_checkpoint_runs m ON m.checkpoint_id=w.suspend_checkpoint_id AND m.run_wait_id=w.id
             AND m.run_id=runs.id AND m.attempt_number=runs.current_attempt_number
           JOIN computer_checkpoints c ON c.id=m.checkpoint_id AND c.computer_id=runs.computer_id
           JOIN computer_disk_versions d ON d.id=c.private_computer_disk_version_id AND d.status='private'
           JOIN run_leases l ON l.id=m.source_run_lease_id AND l.status='checkpointed'
           WHERE w.run_id=runs.id AND w.suspension_status='resume_pending'
             AND c.status='ready' AND c.resume_committed_at IS NULL
             AND (c.expires_at IS NULL OR c.expires_at>clock_timestamp())
             AND (runs.session_id IS NULL OR EXISTS(SELECT 1 FROM sessions a WHERE a.id=runs.session_id
               AND m.actor_speculative_input_sequence BETWEEN a.committed_input_sequence AND a.next_input_sequence-1)))
       )
   AND (runs.first_lease_at IS NOT NULL OR runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
 ORDER BY runs.queue_score_at, runs.id
 LIMIT sqlc.arg(row_limit)
 ) AS candidates
 ORDER BY input_scopes.scope_ordinal, candidates.candidate_score_at, candidates.run_id
 LIMIT sqlc.arg(row_limit);
