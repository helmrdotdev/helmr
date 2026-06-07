-- name: RequeueExpiredLeasedRunExecutionSessions :exec
WITH eligible AS (
    SELECT runs.id AS run_id,
           run_execution_sessions.id AS session_id,
           run_attempts.attempt_number,
           run_execution_sessions.restore_checkpoint_id
      FROM runs
      JOIN run_execution_sessions ON run_execution_sessions.id = runs.current_session_id
                          AND run_execution_sessions.org_id = runs.org_id
                          AND run_execution_sessions.run_id = runs.id
      JOIN run_attempts ON run_attempts.org_id = run_execution_sessions.org_id
                       AND run_attempts.run_id = run_execution_sessions.run_id
                       AND run_attempts.id = run_execution_sessions.attempt_id
     WHERE runs.org_id = $1
       AND runs.status = 'running'
       AND run_execution_sessions.status = 'leased'
       AND run_execution_sessions.lease_expires_at <= now()
     FOR UPDATE OF runs, run_execution_sessions
),
updated_runs AS (
    UPDATE runs
       SET status = 'queued',
           current_session_id = NULL,
           state_version = state_version + 1,
           updated_at = now()
      FROM eligible
     WHERE runs.id = eligible.run_id
       AND runs.status = 'running'
       AND runs.current_session_id = eligible.session_id
    RETURNING eligible.run_id, eligible.session_id, eligible.attempt_number, eligible.restore_checkpoint_id, runs.project_id, runs.environment_id, runs.current_attempt_id, runs.state_version, runs.queued_expires_at
),
requeued_attempts AS (
    UPDATE run_attempts
       SET status = 'queued',
           updated_at = now()
      FROM updated_runs
     WHERE run_attempts.org_id = $1
       AND run_attempts.run_id = updated_runs.run_id
       AND run_attempts.id = updated_runs.current_attempt_id
    RETURNING run_attempts.id, run_attempts.run_id
),
restored_checkpoint AS (
    UPDATE checkpoints
       SET status = 'ready',
           error_message = NULL,
           invalidated_at = NULL
      FROM updated_runs
     WHERE checkpoints.run_id = updated_runs.run_id
       AND checkpoints.id = updated_runs.restore_checkpoint_id
       AND checkpoints.status = 'restoring'
    RETURNING checkpoints.id
),
requeued_queue_entries AS (
    UPDATE run_queue_items
       SET status = 'queued',
           dispatch_message_id = NULL,
           reserved_by_worker_instance_id = NULL,
           reservation_expires_at = NULL,
           queued_expires_at = updated_runs.queued_expires_at,
           dispatch_generation = dispatch_generation + 1,
           last_error = 'worker lease expired before execution started',
           enqueued_at = now(),
           updated_at = now(),
           finished_at = NULL
      FROM updated_runs
      JOIN run_execution_sessions ON run_execution_sessions.org_id = $1
                         AND run_execution_sessions.run_id = updated_runs.run_id
                         AND run_execution_sessions.id = updated_runs.session_id
     WHERE run_queue_items.org_id = $1
       AND run_queue_items.run_id = updated_runs.run_id
       AND run_queue_items.reserved_by_worker_instance_id = run_execution_sessions.worker_instance_id
       AND run_queue_items.dispatch_message_id = run_execution_sessions.dispatch_message_id
       AND run_queue_items.status = 'reserved'
    RETURNING run_queue_items.run_id
),
released_concurrency_slots AS (
    UPDATE run_queue_concurrency_leases
       SET released_at = now()
      FROM updated_runs
     WHERE run_queue_concurrency_leases.org_id = $1
       AND run_queue_concurrency_leases.run_id = updated_runs.run_id
       AND run_queue_concurrency_leases.session_id = updated_runs.session_id
       AND run_queue_concurrency_leases.released_at IS NULL
    RETURNING run_queue_concurrency_leases.id
),
lost_events AS (
    INSERT INTO run_events (org_id, project_id, environment_id, run_id, attempt_id, session_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT $1,
           updated_runs.project_id,
           updated_runs.environment_id,
           updated_runs.run_id,
           run_execution_sessions.attempt_id,
           updated_runs.session_id,
           updated_runs.attempt_number,
           run_execution_sessions.trace_id,
           run_execution_sessions.span_id,
           run_execution_sessions.parent_span_id,
           run_execution_sessions.traceparent,
           'worker',
           'warn',
           'lease_sweeper',
           'run.execution_lost',
           'run.execution_lost',
           jsonb_build_object(
               'reason', 'worker lease expired before execution started',
               'source', 'lease_sweeper'
           ),
           'internal',
           updated_runs.state_version
      FROM updated_runs
      JOIN run_execution_sessions ON run_execution_sessions.org_id = $1
                                  AND run_execution_sessions.run_id = updated_runs.run_id
                                  AND run_execution_sessions.id = updated_runs.session_id
    RETURNING id
),
lost_snapshots AS (
    INSERT INTO run_snapshots (org_id, run_id, version, status, attempt_id, session_id, transition, reason)
    SELECT $1,
           updated_runs.run_id,
           updated_runs.state_version,
           'queued',
           updated_runs.current_attempt_id,
           updated_runs.session_id,
           'session.lost_requeued',
           jsonb_build_object('reason', 'worker lease expired before execution started', 'source', 'lease_sweeper')
      FROM updated_runs
      JOIN requeued_attempts ON requeued_attempts.run_id = updated_runs.run_id
    RETURNING id
),
cleanup AS (
    SELECT
        (SELECT count(*) FROM restored_checkpoint) AS restored_checkpoint_count,
        (SELECT count(*) FROM requeued_queue_entries) AS requeued_queue_entry_count,
        (SELECT count(*) FROM released_concurrency_slots) AS released_concurrency_slot_count,
        (SELECT count(*) FROM lost_events) AS lost_event_count,
        (SELECT count(*) FROM lost_snapshots) AS lost_snapshot_count
)
UPDATE run_execution_sessions
   SET lost_at = COALESCE(lost_at, now()),
       renewed_at = now(),
       status = 'lost'
  FROM updated_runs
 WHERE run_execution_sessions.id = updated_runs.session_id
   AND run_execution_sessions.run_id = updated_runs.run_id
   AND (SELECT restored_checkpoint_count + requeued_queue_entry_count + released_concurrency_slot_count + lost_event_count + lost_snapshot_count FROM cleanup) >= 0;

-- name: AbandonLeasedRunExecutionSession :exec
WITH abandoned AS (
    UPDATE runs
       SET status = 'queued',
           current_session_id = NULL,
           state_version = state_version + 1,
           updated_at = now()
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND runs.current_session_id = sqlc.arg(session_id)
       AND EXISTS (
           SELECT 1
             FROM run_execution_sessions
            WHERE run_execution_sessions.org_id = sqlc.arg(org_id)
              AND run_execution_sessions.run_id = sqlc.arg(run_id)
              AND run_execution_sessions.id = sqlc.arg(session_id)
              AND run_execution_sessions.worker_instance_id = sqlc.arg(worker_instance_id)
              AND run_execution_sessions.status = 'leased'
       )
    RETURNING runs.id, runs.org_id, runs.current_attempt_id, runs.state_version
),
requeued_attempt AS (
    UPDATE run_attempts
       SET status = 'queued',
           updated_at = now()
      FROM abandoned
     WHERE run_attempts.org_id = abandoned.org_id
       AND run_attempts.run_id = abandoned.id
       AND run_attempts.id = abandoned.current_attempt_id
    RETURNING run_attempts.id, run_attempts.run_id
),
restored_checkpoint AS (
    UPDATE checkpoints
       SET status = 'ready',
           error_message = NULL,
           invalidated_at = NULL
      FROM abandoned
      JOIN run_execution_sessions ON run_execution_sessions.org_id = sqlc.arg(org_id)
                         AND run_execution_sessions.run_id = abandoned.id
                         AND run_execution_sessions.id = sqlc.arg(session_id)
                         AND run_execution_sessions.worker_instance_id = sqlc.arg(worker_instance_id)
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = abandoned.id
       AND checkpoints.id = run_execution_sessions.restore_checkpoint_id
       AND checkpoints.status = 'restoring'
    RETURNING checkpoints.id
),
released_concurrency_slots AS (
    UPDATE run_queue_concurrency_leases
       SET released_at = now()
      FROM abandoned
     WHERE run_queue_concurrency_leases.org_id = sqlc.arg(org_id)
       AND run_queue_concurrency_leases.run_id = abandoned.id
       AND run_queue_concurrency_leases.session_id = sqlc.arg(session_id)
       AND run_queue_concurrency_leases.released_at IS NULL
    RETURNING run_queue_concurrency_leases.id
),
abandoned_snapshot AS (
    INSERT INTO run_snapshots (org_id, run_id, version, status, attempt_id, session_id, transition, reason)
    SELECT abandoned.org_id,
           abandoned.id,
           abandoned.state_version,
           'queued',
           abandoned.current_attempt_id,
           sqlc.arg(session_id),
           'session.abandoned_requeued',
           jsonb_build_object('reason', 'worker payload build abandoned')
      FROM abandoned
      JOIN requeued_attempt ON true
    RETURNING id
),
cleanup AS (
    SELECT
        (SELECT count(*) FROM restored_checkpoint) AS restored_checkpoint_count,
        (SELECT count(*) FROM released_concurrency_slots) AS released_concurrency_slot_count,
        (SELECT count(*) FROM abandoned_snapshot) AS abandoned_snapshot_count
)
UPDATE run_execution_sessions
   SET lost_at = COALESCE(lost_at, now()),
       renewed_at = now(),
       status = 'lost'
  FROM abandoned
 WHERE run_execution_sessions.org_id = sqlc.arg(org_id)
   AND run_execution_sessions.run_id = abandoned.id
   AND run_execution_sessions.id = sqlc.arg(session_id)
   AND run_execution_sessions.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_execution_sessions.status = 'leased'
   AND (SELECT restored_checkpoint_count + released_concurrency_slot_count + abandoned_snapshot_count FROM cleanup) >= 0;

-- name: FailExpiredRunningRunExecutionSessions :exec
WITH eligible AS (
    SELECT runs.id AS run_id,
           run_execution_sessions.id AS session_id,
           run_attempts.attempt_number,
           run_execution_sessions.restore_checkpoint_id
      FROM runs
      JOIN run_execution_sessions ON run_execution_sessions.id = runs.current_session_id
                          AND run_execution_sessions.org_id = runs.org_id
                          AND run_execution_sessions.run_id = runs.id
      JOIN run_attempts ON run_attempts.org_id = run_execution_sessions.org_id
                       AND run_attempts.run_id = run_execution_sessions.run_id
                       AND run_attempts.id = run_execution_sessions.attempt_id
     WHERE runs.org_id = $1
       AND runs.status = 'running'
       AND run_execution_sessions.status = 'running'
       AND run_execution_sessions.lease_expires_at <= now()
     FOR UPDATE OF runs, run_execution_sessions
),
updated_runs AS (
    UPDATE runs
       SET status = 'failed',
           current_session_id = NULL,
           error_message = 'worker lease expired',
           state_version = state_version + 1,
           finished_at = COALESCE(finished_at, now()),
           updated_at = now()
      FROM eligible
     WHERE runs.id = eligible.run_id
       AND runs.status = 'running'
       AND runs.current_session_id = eligible.session_id
    RETURNING eligible.run_id, eligible.session_id, eligible.attempt_number, eligible.restore_checkpoint_id, runs.project_id, runs.environment_id, runs.current_attempt_id, runs.state_version
),
failed_attempts AS (
    UPDATE run_attempts
       SET status = 'failed',
           error_message = 'worker lease expired',
           finished_at = COALESCE(run_attempts.finished_at, now()),
           updated_at = now()
      FROM updated_runs
     WHERE run_attempts.org_id = $1
       AND run_attempts.run_id = updated_runs.run_id
       AND run_attempts.id = updated_runs.current_attempt_id
    RETURNING run_attempts.id, run_attempts.run_id
),
cancelled_run_waits AS (
    UPDATE run_waits
       SET status = 'cancelled',
           failure = jsonb_build_object('reason', 'worker lease expired', 'source', 'lease_sweeper'),
           resolution_kind = 'cancelled',
           resolution = jsonb_build_object('reason', 'worker lease expired', 'source', 'lease_sweeper'),
           failed_at = now(),
           updated_at = now()
      FROM updated_runs
     WHERE run_waits.org_id = $1
       AND run_waits.run_id = updated_runs.run_id
       AND run_waits.session_id = updated_runs.session_id
       AND run_waits.status IN ('opening', 'waiting', 'resuming')
    RETURNING run_waits.id, run_waits.org_id
),
cancelled_waitpoints AS (
    UPDATE waitpoints
       SET status = 'cancelled',
           resolution_kind = 'cancelled',
           output = 'null'::jsonb,
           resolution = jsonb_build_object('reason', 'worker lease expired', 'source', 'lease_sweeper'),
           output_is_error = true,
           completed_at = now(),
           updated_at = now()
      FROM cancelled_run_waits
      JOIN run_wait_dependencies ON run_wait_dependencies.org_id = cancelled_run_waits.org_id
                                AND run_wait_dependencies.run_wait_id = cancelled_run_waits.id
     WHERE waitpoints.org_id = run_wait_dependencies.org_id
       AND waitpoints.id = run_wait_dependencies.waitpoint_id
       AND waitpoints.status = 'pending'
    RETURNING waitpoints.id
),
invalidated_checkpoints AS (
    UPDATE checkpoints
       SET status = 'invalid',
           error_message = 'worker lease expired',
           invalidated_at = now()
      FROM updated_runs
     WHERE checkpoints.run_id = updated_runs.run_id
       AND checkpoints.session_id = updated_runs.session_id
       AND checkpoints.status IN ('creating', 'restoring')
    RETURNING checkpoints.run_id, checkpoints.id
),
failed_restore_checkpoints AS (
    UPDATE checkpoints
       SET status = 'invalid',
           error_message = 'worker lease expired',
           invalidated_at = now()
      FROM updated_runs
     WHERE checkpoints.run_id = updated_runs.run_id
       AND checkpoints.id = updated_runs.restore_checkpoint_id
       AND checkpoints.status = 'restoring'
    RETURNING checkpoints.run_id, checkpoints.id
),
completed_queue_entries AS (
    UPDATE run_queue_items
       SET status = 'completed',
           dispatch_generation = dispatch_generation + 1,
           updated_at = now(),
           finished_at = now()
      FROM updated_runs
      JOIN run_execution_sessions ON run_execution_sessions.org_id = $1
                         AND run_execution_sessions.run_id = updated_runs.run_id
                         AND run_execution_sessions.id = updated_runs.session_id
     WHERE run_queue_items.org_id = $1
       AND run_queue_items.run_id = updated_runs.run_id
       AND run_queue_items.reserved_by_worker_instance_id = run_execution_sessions.worker_instance_id
       AND run_queue_items.dispatch_message_id = run_execution_sessions.dispatch_message_id
       AND run_queue_items.status = 'reserved'
    RETURNING run_queue_items.run_id
),
released_concurrency_slots AS (
    UPDATE run_queue_concurrency_leases
       SET released_at = now()
      FROM updated_runs
     WHERE run_queue_concurrency_leases.org_id = $1
       AND run_queue_concurrency_leases.run_id = updated_runs.run_id
       AND run_queue_concurrency_leases.session_id = updated_runs.session_id
       AND run_queue_concurrency_leases.released_at IS NULL
    RETURNING run_queue_concurrency_leases.id
),
terminal_events AS (
    INSERT INTO run_events (org_id, project_id, environment_id, run_id, attempt_id, session_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT $1,
           updated_runs.project_id,
           updated_runs.environment_id,
           updated_runs.run_id,
           run_execution_sessions.attempt_id,
           updated_runs.session_id,
           updated_runs.attempt_number,
           run_execution_sessions.trace_id,
           run_execution_sessions.span_id,
           run_execution_sessions.parent_span_id,
           run_execution_sessions.traceparent,
           'lifecycle',
           'error',
           'lease_sweeper',
           'run.failed',
           'run.failed',
           jsonb_build_object(
               'failure_kind', 'worker_lease_expired',
               'detail', jsonb_build_object('message', 'worker lease expired')
           ),
           'internal',
           updated_runs.state_version
      FROM updated_runs
      JOIN run_execution_sessions ON run_execution_sessions.org_id = $1
                                  AND run_execution_sessions.run_id = updated_runs.run_id
                                  AND run_execution_sessions.id = updated_runs.session_id
    RETURNING id
),
lost_events AS (
    INSERT INTO run_events (org_id, project_id, environment_id, run_id, attempt_id, session_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT $1,
           updated_runs.project_id,
           updated_runs.environment_id,
           updated_runs.run_id,
           run_execution_sessions.attempt_id,
           updated_runs.session_id,
           updated_runs.attempt_number,
           run_execution_sessions.trace_id,
           run_execution_sessions.span_id,
           run_execution_sessions.parent_span_id,
           run_execution_sessions.traceparent,
           'worker',
           'warn',
           'lease_sweeper',
           'run.execution_lost',
           'run.execution_lost',
           jsonb_build_object(
               'reason', 'worker lease expired',
               'source', 'lease_sweeper'
           ),
           'internal',
           updated_runs.state_version
      FROM updated_runs
      JOIN run_execution_sessions ON run_execution_sessions.org_id = $1
                                  AND run_execution_sessions.run_id = updated_runs.run_id
                                  AND run_execution_sessions.id = updated_runs.session_id
    RETURNING id
),
failed_snapshots AS (
    INSERT INTO run_snapshots (org_id, run_id, version, status, attempt_id, session_id, transition, reason)
    SELECT $1,
           updated_runs.run_id,
           updated_runs.state_version,
           'failed',
           updated_runs.current_attempt_id,
           updated_runs.session_id,
           'session.lost_failed',
           jsonb_build_object('reason', 'worker lease expired', 'source', 'lease_sweeper')
      FROM updated_runs
      JOIN failed_attempts ON failed_attempts.run_id = updated_runs.run_id
    RETURNING id
),
cleanup AS (
    SELECT
        (SELECT count(*) FROM cancelled_waitpoints) AS cancelled_waitpoints,
        (SELECT count(*) FROM invalidated_checkpoints) AS invalidated_checkpoints,
        (SELECT count(*) FROM failed_restore_checkpoints) AS failed_restore_checkpoints,
        (SELECT count(*) FROM completed_queue_entries) AS completed_queue_entries,
        (SELECT count(*) FROM released_concurrency_slots) AS released_concurrency_slots,
        (SELECT count(*) FROM terminal_events) AS terminal_events,
        (SELECT count(*) FROM lost_events) AS lost_events,
        (SELECT count(*) FROM failed_snapshots) AS failed_snapshots
)
UPDATE run_execution_sessions
   SET lost_at = COALESCE(lost_at, now()),
       renewed_at = now(),
       status = 'lost'
 FROM updated_runs
 WHERE run_execution_sessions.id = updated_runs.session_id
   AND run_execution_sessions.run_id = updated_runs.run_id
   AND (SELECT cancelled_waitpoints + invalidated_checkpoints + failed_restore_checkpoints + completed_queue_entries + released_concurrency_slots + terminal_events + lost_events + failed_snapshots FROM cleanup) >= 0;

-- name: LeaseRunExecutionSession :one
WITH
locked_dispatch AS MATERIALIZED (
    SELECT run_queue_items.run_id,
           run_queue_items.org_id,
           run_queue_items.reserved_by_worker_instance_id,
           run_queue_items.dispatch_message_id
      FROM run_queue_items
     WHERE run_queue_items.org_id = sqlc.arg(org_id)
       AND run_queue_items.run_id = sqlc.arg(run_id)
       AND run_queue_items.reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_queue_items.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_queue_items.status = 'reserved'
       AND run_queue_items.reservation_expires_at > now()
     FOR UPDATE OF run_queue_items
),
locked_worker_instance AS MATERIALIZED (
    SELECT worker_instances.*
      FROM worker_instances
      JOIN locked_dispatch ON locked_dispatch.reserved_by_worker_instance_id = worker_instances.id
     WHERE worker_instances.status = 'active'
     FOR UPDATE OF worker_instances
),
dispatch AS (
    SELECT locked_dispatch.run_id,
           locked_dispatch.reserved_by_worker_instance_id AS worker_instance_id,
           locked_dispatch.dispatch_message_id,
           worker_instances.available_milli_cpu,
           worker_instances.available_memory_mib,
           worker_instances.available_disk_mib,
           worker_instances.available_execution_slots,
           worker_instances.total_milli_cpu,
           worker_instances.total_memory_mib,
           worker_instances.total_disk_mib,
           worker_instances.region,
           worker_instances.labels,
           worker_instances.runtime_id,
           worker_instances.runtime_arch,
           worker_instances.runtime_abi,
           worker_instances.kernel_digest,
           worker_instances.initramfs_digest,
           worker_instances.rootfs_digest,
           worker_instances.cni_profile,
           worker_instances.worker_group_id,
           worker_instances.protocol_version,
           active.used_milli_cpu,
           active.used_memory_mib,
           active.used_disk_mib,
           active.used_slots
      FROM locked_dispatch
      JOIN locked_worker_instance AS worker_instances ON worker_instances.id = locked_dispatch.reserved_by_worker_instance_id
      LEFT JOIN LATERAL (
          SELECT COALESCE(sum(run_runtime_requirements.requested_milli_cpu), 0)::bigint AS used_milli_cpu,
                 COALESCE(sum(run_runtime_requirements.requested_memory_mib), 0)::bigint AS used_memory_mib,
                 COALESCE(sum(run_runtime_requirements.requested_disk_mib), 0)::bigint AS used_disk_mib,
                 COALESCE(sum(run_runtime_requirements.requested_execution_slots), 0)::int AS used_slots
            FROM run_execution_sessions
            JOIN run_runtime_requirements ON run_runtime_requirements.org_id = run_execution_sessions.org_id
                                 AND run_runtime_requirements.run_id = run_execution_sessions.run_id
           WHERE run_execution_sessions.worker_instance_id = worker_instances.id
             AND run_execution_sessions.status IN ('leased', 'running')
      ) active ON true
),
candidate AS (
    SELECT runs.id,
           runs.project_id,
           runs.environment_id,
           runs.trace_id,
           runs.root_span_id,
           runs.latest_checkpoint_id,
           runs.queue_name,
           runs.queue_concurrency_limit,
           runs.concurrency_key,
           runs.current_attempt_id,
           runs.current_attempt_number,
           run_runtime_requirements.runtime_id
      FROM runs
      JOIN dispatch ON dispatch.run_id = runs.id
      JOIN run_runtime_requirements ON run_runtime_requirements.org_id = runs.org_id
                                    AND run_runtime_requirements.run_id = runs.id
      JOIN LATERAL (
          SELECT COALESCE(NULLIF(run_runtime_requirements.placement->>'region', ''), NULLIF(run_runtime_requirements.placement->>'Region', ''), '') AS placement_region,
                 COALESCE(run_runtime_requirements.placement->'tags', run_runtime_requirements.placement->'Tags') AS placement_tags,
                 COALESCE(NULLIF(run_runtime_requirements.placement->>'dedicated_key', ''), NULLIF(run_runtime_requirements.placement->>'DedicatedKey', ''), '') AS dedicated_key,
                 COALESCE(NULLIF(run_runtime_requirements.placement->>'snapshot_key', ''), NULLIF(run_runtime_requirements.placement->>'SnapshotKey', ''), '') AS snapshot_key
      ) placement ON true
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'queued'
       AND runs.current_session_id IS NULL
       AND (runs.queued_expires_at IS NULL OR runs.queued_expires_at > now())
       AND run_runtime_requirements.requested_milli_cpu <= GREATEST(dispatch.available_milli_cpu - dispatch.used_milli_cpu, 0)
       AND run_runtime_requirements.requested_memory_mib <= GREATEST(dispatch.available_memory_mib - dispatch.used_memory_mib, 0)
       AND run_runtime_requirements.requested_disk_mib <= GREATEST(dispatch.available_disk_mib - dispatch.used_disk_mib, 0)
       AND run_runtime_requirements.requested_execution_slots <= GREATEST(dispatch.available_execution_slots - dispatch.used_slots, 0)
       AND run_runtime_requirements.worker_group_id = dispatch.worker_group_id
       AND run_runtime_requirements.runtime_id = dispatch.runtime_id
       AND run_runtime_requirements.runtime_arch = dispatch.runtime_arch
       AND run_runtime_requirements.runtime_abi = dispatch.runtime_abi
       AND run_runtime_requirements.kernel_digest = dispatch.kernel_digest
       AND run_runtime_requirements.initramfs_digest = dispatch.initramfs_digest
       AND run_runtime_requirements.rootfs_digest = dispatch.rootfs_digest
       AND run_runtime_requirements.cni_profile = dispatch.cni_profile
       AND (placement.placement_region = '' OR placement.placement_region = dispatch.region)
       AND (
           placement.placement_tags IS NULL
           OR placement.placement_tags = 'null'::jsonb
           OR (
               jsonb_typeof(placement.placement_tags) = 'object'
               AND dispatch.labels @> placement.placement_tags
           )
       )
       AND (placement.dedicated_key = '' OR dispatch.labels->>'dedicated_key' = placement.dedicated_key)
       AND (placement.snapshot_key = '' OR dispatch.labels->>'snapshot_key' = placement.snapshot_key)
       AND (
           runs.latest_checkpoint_id IS NULL
           OR EXISTS (
               SELECT 1
                 FROM checkpoints
                 JOIN checkpoint_runtime_snapshots
                   ON checkpoint_runtime_snapshots.org_id = checkpoints.org_id
                  AND checkpoint_runtime_snapshots.run_id = checkpoints.run_id
                  AND checkpoint_runtime_snapshots.checkpoint_id = checkpoints.id
                 JOIN run_waits ON run_waits.org_id = sqlc.arg(org_id)
                               AND run_waits.run_id = runs.id
                               AND run_waits.checkpoint_id = checkpoints.id
                 JOIN run_wait_dependencies ON run_wait_dependencies.org_id = run_waits.org_id
                                           AND run_wait_dependencies.run_wait_id = run_waits.id
                 JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                                AND waitpoints.id = run_wait_dependencies.waitpoint_id
                WHERE checkpoints.org_id = sqlc.arg(org_id)
                  AND checkpoints.run_id = runs.id
                  AND checkpoints.id = runs.latest_checkpoint_id
                  AND checkpoints.status = 'ready'
                  AND run_waits.status = 'resuming'
                  AND run_waits.resolution_kind IS NOT NULL
                  AND checkpoint_runtime_snapshots.runtime_id = dispatch.runtime_id
                  AND checkpoint_runtime_snapshots.runtime_arch = dispatch.runtime_arch
                  AND checkpoint_runtime_snapshots.runtime_abi = dispatch.runtime_abi
                  AND checkpoint_runtime_snapshots.kernel_digest = dispatch.kernel_digest
                  AND checkpoint_runtime_snapshots.initramfs_digest = dispatch.initramfs_digest
                  AND checkpoint_runtime_snapshots.rootfs_digest = dispatch.rootfs_digest
                  AND (checkpoint_runtime_snapshots.runtime_vcpus IS NULL OR checkpoint_runtime_snapshots.runtime_vcpus = ((dispatch.total_milli_cpu + 999) / 1000))
                  AND (checkpoint_runtime_snapshots.runtime_memory_mib IS NULL OR checkpoint_runtime_snapshots.runtime_memory_mib = dispatch.total_memory_mib)
                  AND (checkpoint_runtime_snapshots.runtime_scratch_disk_mib IS NULL OR checkpoint_runtime_snapshots.runtime_scratch_disk_mib = dispatch.total_disk_mib)
                  AND checkpoint_runtime_snapshots.cni_profile = dispatch.cni_profile
           )
       )
     FOR UPDATE OF runs
),
concurrency_scope_lock AS MATERIALIZED (
    SELECT candidate.id AS run_id,
           true AS locked
      FROM candidate
      CROSS JOIN LATERAL (
          SELECT pg_advisory_xact_lock(
                     hashtext(sqlc.arg(org_id)::text || ':' || candidate.environment_id::text),
                     hashtext(candidate.queue_name || ':' || COALESCE(candidate.concurrency_key, ''))
                 )
      ) lock
     WHERE candidate.queue_concurrency_limit IS NOT NULL
),
concurrency_capacity AS (
    SELECT candidate.*
      FROM candidate
      LEFT JOIN concurrency_scope_lock ON concurrency_scope_lock.run_id = candidate.id
     WHERE candidate.queue_concurrency_limit IS NULL
        OR (
            concurrency_scope_lock.locked
            AND (
                SELECT count(*)::int
                  FROM run_queue_concurrency_leases
                 WHERE run_queue_concurrency_leases.org_id = sqlc.arg(org_id)
                   AND run_queue_concurrency_leases.environment_id = candidate.environment_id
                   AND run_queue_concurrency_leases.queue_name = candidate.queue_name
                   AND COALESCE(run_queue_concurrency_leases.concurrency_key, '') = COALESCE(candidate.concurrency_key, '')
                   AND run_queue_concurrency_leases.released_at IS NULL
            ) < candidate.queue_concurrency_limit
        )
),
concurrency_slot_candidate AS (
    SELECT concurrency_capacity.*,
           slots.slot_ordinal
      FROM concurrency_capacity
      CROSS JOIN LATERAL generate_series(1, concurrency_capacity.queue_concurrency_limit) AS slots(slot_ordinal)
     WHERE concurrency_capacity.queue_concurrency_limit IS NOT NULL
       AND NOT EXISTS (
            SELECT 1
              FROM run_queue_concurrency_leases
             WHERE run_queue_concurrency_leases.org_id = sqlc.arg(org_id)
               AND run_queue_concurrency_leases.environment_id = concurrency_capacity.environment_id
               AND run_queue_concurrency_leases.queue_name = concurrency_capacity.queue_name
               AND COALESCE(run_queue_concurrency_leases.concurrency_key, '') = COALESCE(concurrency_capacity.concurrency_key, '')
               AND run_queue_concurrency_leases.slot_ordinal = slots.slot_ordinal
               AND run_queue_concurrency_leases.released_at IS NULL
       )
     ORDER BY slots.slot_ordinal
     LIMIT 1
),
concurrency_slot AS (
    INSERT INTO run_queue_concurrency_leases (
        org_id,
        project_id,
        environment_id,
        run_id,
        session_id,
        queue_name,
        concurrency_key,
        slot_ordinal
    )
    SELECT sqlc.arg(org_id),
           concurrency_slot_candidate.project_id,
           concurrency_slot_candidate.environment_id,
           concurrency_slot_candidate.id,
           sqlc.arg(session_id),
           concurrency_slot_candidate.queue_name,
           concurrency_slot_candidate.concurrency_key,
           concurrency_slot_candidate.slot_ordinal
      FROM concurrency_slot_candidate
    ON CONFLICT DO NOTHING
    RETURNING id
),
leaseable_capacity AS (
    SELECT concurrency_capacity.*
      FROM concurrency_capacity
     WHERE concurrency_capacity.queue_concurrency_limit IS NULL
    UNION ALL
    SELECT concurrency_capacity.*
      FROM concurrency_capacity
     WHERE concurrency_capacity.queue_concurrency_limit IS NOT NULL
       AND EXISTS (SELECT 1 FROM concurrency_slot)
),
restore_checkpoint AS (
    SELECT checkpoints.id
      FROM leaseable_capacity AS concurrency_capacity
      JOIN checkpoints ON checkpoints.org_id = sqlc.arg(org_id)
                      AND checkpoints.run_id = concurrency_capacity.id
                      AND checkpoints.id = concurrency_capacity.latest_checkpoint_id
      JOIN run_waits ON run_waits.org_id = sqlc.arg(org_id)
                    AND run_waits.run_id = concurrency_capacity.id
                    AND run_waits.checkpoint_id = checkpoints.id
      JOIN run_wait_dependencies ON run_wait_dependencies.org_id = run_waits.org_id
                                AND run_wait_dependencies.run_wait_id = run_waits.id
      JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                     AND waitpoints.id = run_wait_dependencies.waitpoint_id
     WHERE checkpoints.status = 'ready'
       AND run_waits.status = 'resuming'
       AND run_waits.resolution_kind IS NOT NULL
     ORDER BY run_waits.resolved_at DESC
     LIMIT 1
),
leased_session AS (
    INSERT INTO run_execution_sessions (
        id,
        org_id,
        run_id,
        attempt_id,
        worker_instance_id,
        worker_group_id,
        dispatch_message_id,
        dispatch_lease_id,
        dispatch_attempt,
        status,
        lease_expires_at,
        runtime_id,
        worker_protocol_version,
        trace_id,
        span_id,
        parent_span_id,
        traceparent,
        restore_checkpoint_id
    )
    SELECT sqlc.arg(session_id),
           sqlc.arg(org_id),
           candidate.id,
           candidate.current_attempt_id,
           sqlc.arg(worker_instance_id),
           dispatch.worker_group_id,
           sqlc.arg(dispatch_message_id),
           sqlc.arg(dispatch_lease_id),
           sqlc.arg(dispatch_attempt),
           'leased',
           sqlc.arg(lease_expires_at),
           candidate.runtime_id,
           dispatch.protocol_version,
           candidate.trace_id,
           sqlc.arg(session_span_id),
           candidate.root_span_id,
           '00-' || candidate.trace_id || '-' || sqlc.arg(session_span_id)::text || '-01',
           (SELECT id FROM restore_checkpoint)
      FROM leaseable_capacity AS candidate
      JOIN dispatch ON dispatch.run_id = candidate.id
    RETURNING id, worker_instance_id, dispatch_message_id, dispatch_lease_id, dispatch_attempt, attempt_id, lease_expires_at, worker_protocol_version, trace_id, span_id, traceparent, restore_checkpoint_id
),
active_time AS (
    -- active_duration_ms is stored as run-cumulative elapsed worker time on each terminal/detached session.
    SELECT COALESCE(MAX(run_execution_sessions.active_duration_ms), 0)::bigint AS active_duration_ms
      FROM leaseable_capacity AS concurrency_capacity
      LEFT JOIN run_execution_sessions ON run_execution_sessions.org_id = sqlc.arg(org_id)
                              AND run_execution_sessions.run_id = concurrency_capacity.id
                              AND run_execution_sessions.status IN ('detached', 'released')
),
marked_restore_checkpoint AS (
    UPDATE checkpoints
       SET status = 'restoring',
           error_message = NULL,
           invalidated_at = NULL
      FROM restore_checkpoint
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.id = restore_checkpoint.id
       AND checkpoints.status = 'ready'
    RETURNING checkpoints.id
),
updated AS (
    UPDATE runs
       SET status = 'running',
           current_session_id = (SELECT id FROM leased_session),
           state_version = state_version + 1,
           updated_at = now()
     WHERE id = (SELECT id FROM concurrency_capacity)
      AND EXISTS (SELECT 1 FROM leased_session)
    RETURNING *
),
updated_attempt AS (
    UPDATE run_attempts
       SET status = 'running',
           updated_at = now()
      FROM updated
     WHERE run_attempts.org_id = updated.org_id
       AND run_attempts.run_id = updated.id
       AND run_attempts.id = updated.current_attempt_id
    RETURNING run_attempts.id
),
leased_snapshot AS (
    INSERT INTO run_snapshots (org_id, run_id, version, status, attempt_id, session_id, transition, reason)
    SELECT updated.org_id,
           updated.id,
           updated.state_version,
           updated.status,
           updated.current_attempt_id,
           leased_session.id,
           'session.leased',
           jsonb_build_object(
               'worker_instance_id', leased_session.worker_instance_id,
               'dispatch_message_id', leased_session.dispatch_message_id,
               'dispatch_attempt', leased_session.dispatch_attempt
           )
      FROM updated
      JOIN leased_session ON true
      JOIN updated_attempt ON true
    RETURNING id
)
SELECT
    updated.id,
    updated.org_id,
    updated.project_id,
    updated.environment_id,
    updated.task_id,
    updated.deployment_version AS run_deployment_version,
    updated.api_version AS run_api_version,
    updated.sdk_version AS run_sdk_version,
    updated.cli_version AS run_cli_version,
    updated.status,
    updated.payload,
    deployment_tasks.id AS deployment_task_id,
    deployment_tasks.file_path AS deployment_task_file_path,
    deployment_tasks.export_name AS deployment_task_export_name,
    deployment_tasks.handler_entrypoint AS deployment_task_handler_entrypoint,
    task_bundle_artifacts.digest AS deployment_task_bundle_digest,
    deployment_tasks.bundle_format_version AS deployment_task_bundle_format_version,
    deployment_tasks.secret_declarations AS deployment_task_secret_declarations,
    deployments.version AS deployment_version,
    deployments.api_version AS deployment_api_version,
    deployments.sdk_version AS deployment_sdk_version,
    deployments.cli_version AS deployment_cli_version,
    deployments.worker_protocol_version AS deployment_worker_protocol_version,
    source_artifacts.digest AS deployment_source_digest,
    updated.max_duration_seconds,
    updated.exit_code,
    updated.error_message,
    updated.created_at,
    updated.updated_at,
    updated.started_at,
    updated.finished_at,
    run_runtime_requirements.requested_milli_cpu,
    run_runtime_requirements.requested_memory_mib,
    run_runtime_requirements.requested_disk_mib,
    run_runtime_requirements.requested_execution_slots,
    run_runtime_requirements.runtime_id AS requirements_runtime_id,
    run_runtime_requirements.runtime_arch AS requirements_runtime_arch,
    run_runtime_requirements.runtime_abi AS requirements_runtime_abi,
    run_runtime_requirements.kernel_digest AS requirements_kernel_digest,
    run_runtime_requirements.initramfs_digest AS requirements_initramfs_digest,
    run_runtime_requirements.rootfs_digest AS requirements_rootfs_digest,
    run_runtime_requirements.cni_profile AS requirements_cni_profile,
    run_runtime_requirements.network_policy AS requirements_network_policy,
    run_runtime_requirements.placement AS requirements_placement,
    leased_session.id AS session_id,
    leased_session.worker_instance_id AS session_worker_instance_id,
    leased_session.dispatch_message_id AS session_dispatch_message_id,
    leased_session.dispatch_lease_id AS session_dispatch_lease_id,
    leased_session.dispatch_attempt AS session_dispatch_attempt,
    run_attempts.attempt_number AS session_attempt_number,
    leased_session.lease_expires_at AS session_lease_expires_at,
    leased_session.worker_protocol_version AS session_worker_protocol_version,
    leased_session.trace_id AS session_trace_id,
    leased_session.span_id AS session_span_id,
    leased_session.traceparent AS session_traceparent,
    leased_session.restore_checkpoint_id AS session_restore_checkpoint_id,
    active_time.active_duration_ms AS active_duration_ms
FROM updated
JOIN leased_session ON true
JOIN leased_snapshot ON true
JOIN active_time ON true
JOIN run_attempts ON run_attempts.org_id = updated.org_id
                 AND run_attempts.run_id = updated.id
                 AND run_attempts.id = leased_session.attempt_id
JOIN deployments ON deployments.org_id = updated.org_id
                AND deployments.id = updated.deployment_id
JOIN deployment_tasks ON deployment_tasks.org_id = updated.org_id
                     AND deployment_tasks.deployment_id = updated.deployment_id
                     AND deployment_tasks.id = updated.deployment_task_id
JOIN artifacts AS task_bundle_artifacts
  ON task_bundle_artifacts.org_id = deployment_tasks.org_id
 AND task_bundle_artifacts.project_id = deployment_tasks.project_id
 AND task_bundle_artifacts.environment_id = deployment_tasks.environment_id
 AND task_bundle_artifacts.id = deployment_tasks.bundle_artifact_id
JOIN artifacts AS source_artifacts
  ON source_artifacts.org_id = deployments.org_id
 AND source_artifacts.project_id = deployments.project_id
 AND source_artifacts.environment_id = deployments.environment_id
 AND source_artifacts.id = deployments.deployment_source_artifact_id
JOIN run_runtime_requirements ON run_runtime_requirements.org_id = updated.org_id
                             AND run_runtime_requirements.run_id = updated.id
LEFT JOIN marked_restore_checkpoint ON true;

-- name: StartRunExecutionSession :one
WITH current_session AS MATERIALIZED (
    SELECT runs.id AS run_id,
           runs.org_id,
           runs.current_attempt_id,
           runs.current_session_id,
           run_execution_sessions.status AS session_status
      FROM runs
      JOIN run_execution_sessions ON run_execution_sessions.id = runs.current_session_id
                          AND run_execution_sessions.org_id = runs.org_id
                          AND run_execution_sessions.run_id = runs.id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND runs.current_session_id = sqlc.arg(session_id)
       AND run_execution_sessions.id = sqlc.arg(session_id)
       AND run_execution_sessions.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_execution_sessions.status IN ('leased', 'running')
       AND run_execution_sessions.lease_expires_at > now()
     FOR UPDATE OF runs, run_execution_sessions
),
started_run AS (
    UPDATE runs
       SET status = 'running',
           started_at = COALESCE(runs.started_at, now()),
           queued_expires_at = NULL,
           state_version = state_version + CASE WHEN current_session.session_status = 'leased' THEN 1 ELSE 0 END,
           updated_at = now()
      FROM current_session
     WHERE runs.org_id = current_session.org_id
       AND runs.id = current_session.run_id
    RETURNING status, id, runs.org_id, runs.current_attempt_id, runs.current_session_id, runs.state_version, current_session.session_status
),
started_session AS (
    UPDATE run_execution_sessions
       SET status = 'running',
           started_at = COALESCE(run_execution_sessions.started_at, now()),
           renewed_at = now()
      FROM started_run
     WHERE run_execution_sessions.id = started_run.current_session_id
       AND run_execution_sessions.run_id = started_run.id
       AND run_execution_sessions.worker_instance_id = sqlc.arg(worker_instance_id)
     RETURNING run_execution_sessions.id, run_execution_sessions.restore_checkpoint_id, started_run.session_status
),
started_attempt AS (
    UPDATE run_attempts
       SET status = 'running',
           started_at = COALESCE(run_attempts.started_at, now()),
           updated_at = now()
      FROM started_run
     WHERE run_attempts.org_id = started_run.org_id
       AND run_attempts.run_id = started_run.id
       AND run_attempts.id = started_run.current_attempt_id
    RETURNING run_attempts.id
),
started_snapshot AS (
    INSERT INTO run_snapshots (org_id, run_id, version, status, attempt_id, session_id, transition, reason)
    SELECT started_run.org_id,
           started_run.id,
           started_run.state_version,
           started_run.status,
           started_run.current_attempt_id,
           started_session.id,
           'session.started',
           jsonb_build_object('worker_instance_id', sqlc.arg(worker_instance_id))
      FROM started_run
      JOIN started_session ON true
      JOIN started_attempt ON true
     WHERE started_session.session_status = 'leased'
    RETURNING id
)
SELECT started_run.status
  FROM started_run
  JOIN started_session ON true
  LEFT JOIN started_snapshot ON true;

-- name: RenewRunExecutionSessionLease :one
WITH renewed_session AS (
    UPDATE run_execution_sessions
       SET lease_expires_at = sqlc.arg(lease_expires_at),
           renewed_at = now()
      FROM runs
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND runs.current_session_id = run_execution_sessions.id
       AND run_execution_sessions.org_id = sqlc.arg(org_id)
       AND run_execution_sessions.run_id = sqlc.arg(run_id)
       AND run_execution_sessions.id = sqlc.arg(session_id)
       AND run_execution_sessions.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_execution_sessions.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_execution_sessions.dispatch_lease_id = sqlc.arg(dispatch_lease_id)
       AND run_execution_sessions.status IN ('leased', 'running')
       AND run_execution_sessions.lease_expires_at > now()
    RETURNING run_execution_sessions.id, run_execution_sessions.org_id, run_execution_sessions.run_id, run_execution_sessions.worker_instance_id, run_execution_sessions.dispatch_message_id, run_execution_sessions.dispatch_lease_id, run_execution_sessions.dispatch_attempt, run_execution_sessions.attempt_id, run_execution_sessions.lease_expires_at, run_execution_sessions.trace_id, run_execution_sessions.span_id, run_execution_sessions.traceparent
)
SELECT renewed_session.id,
       renewed_session.worker_instance_id,
       renewed_session.dispatch_message_id,
       renewed_session.dispatch_lease_id,
       renewed_session.dispatch_attempt,
       run_attempts.attempt_number,
       renewed_session.lease_expires_at,
       renewed_session.trace_id,
       renewed_session.span_id,
       renewed_session.traceparent
  FROM renewed_session
  JOIN run_attempts ON run_attempts.org_id = renewed_session.org_id
                   AND run_attempts.run_id = renewed_session.run_id
                   AND run_attempts.id = renewed_session.attempt_id;

-- name: GetRunExecutionSessionQueueLease :one
SELECT run_execution_sessions.id,
       run_execution_sessions.run_id,
       run_execution_sessions.worker_instance_id,
       run_execution_sessions.dispatch_message_id,
       run_execution_sessions.dispatch_lease_id,
       run_execution_sessions.dispatch_attempt,
       run_attempts.attempt_number,
       run_execution_sessions.lease_expires_at,
       run_queue_items.queue_name
  FROM run_execution_sessions
  JOIN run_attempts ON run_attempts.org_id = run_execution_sessions.org_id
                   AND run_attempts.run_id = run_execution_sessions.run_id
                   AND run_attempts.id = run_execution_sessions.attempt_id
  JOIN run_queue_items ON run_queue_items.org_id = run_execution_sessions.org_id
                     AND run_queue_items.run_id = run_execution_sessions.run_id
                     AND run_queue_items.dispatch_message_id = run_execution_sessions.dispatch_message_id
                     AND run_queue_items.reserved_by_worker_instance_id = run_execution_sessions.worker_instance_id
 WHERE run_execution_sessions.org_id = sqlc.arg(org_id)
   AND run_execution_sessions.run_id = sqlc.arg(run_id)
   AND run_execution_sessions.id = sqlc.arg(session_id)
   AND run_execution_sessions.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_execution_sessions.status IN ('leased', 'running')
   AND run_execution_sessions.lease_expires_at > now()
   AND run_queue_items.status = 'reserved'
   AND run_queue_items.reservation_expires_at > now();

-- name: GetRunExecutionSessionRuntimeRelease :one
SELECT run_execution_sessions.runtime_id,
       runtime_releases.runtime_arch,
       runtime_releases.runtime_abi,
       runtime_releases.kernel_digest,
       runtime_releases.initramfs_digest,
       runtime_releases.rootfs_digest,
       runtime_releases.cni_profile
  FROM run_execution_sessions
  JOIN runtime_releases ON runtime_releases.runtime_id = run_execution_sessions.runtime_id
 WHERE run_execution_sessions.org_id = sqlc.arg(org_id)
   AND run_execution_sessions.run_id = sqlc.arg(run_id)
   AND run_execution_sessions.id = sqlc.arg(session_id)
   AND run_execution_sessions.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_execution_sessions.status IN ('leased', 'running')
   AND run_execution_sessions.lease_expires_at > now();

-- name: ReleaseRunExecutionSession :one
WITH eligible AS (
    SELECT runs.org_id, runs.id AS run_id
      FROM runs
      JOIN run_execution_sessions
        ON run_execution_sessions.org_id = runs.org_id
       AND run_execution_sessions.run_id = runs.id
       AND run_execution_sessions.id = sqlc.arg(session_id)
       AND run_execution_sessions.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_execution_sessions.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_execution_sessions.dispatch_lease_id = sqlc.arg(dispatch_lease_id)
       AND run_execution_sessions.status IN ('leased', 'running')
       AND run_execution_sessions.lease_expires_at > now()
      JOIN run_queue_items
        ON run_queue_items.org_id = runs.org_id
       AND run_queue_items.run_id = runs.id
       AND run_queue_items.reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_queue_items.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_queue_items.status = 'reserved'
       AND run_queue_items.reservation_expires_at > now()
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND runs.current_session_id = sqlc.arg(session_id)
     FOR UPDATE OF runs, run_execution_sessions, run_queue_items
),
completed_queue_entry AS (
    UPDATE run_queue_items
       SET status = 'completed',
           dispatch_generation = dispatch_generation + 1,
           updated_at = now(),
           finished_at = now()
      FROM eligible
     WHERE run_queue_items.org_id = eligible.org_id
       AND run_queue_items.run_id = eligible.run_id
       AND run_queue_items.reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_queue_items.dispatch_message_id = sqlc.arg(dispatch_message_id)
    RETURNING run_queue_items.run_id
),
released AS (
    UPDATE runs
       SET status = sqlc.arg(status),
           current_session_id = NULL,
           state_version = state_version + 1,
           exit_code = sqlc.arg(exit_code),
           output = sqlc.arg(output),
           error_message = sqlc.arg(error_message),
           finished_at = now(),
           updated_at = now()
      FROM eligible
      JOIN completed_queue_entry ON completed_queue_entry.run_id = eligible.run_id
     WHERE runs.org_id = eligible.org_id
       AND runs.id = eligible.run_id
    RETURNING runs.*
),
released_session AS (
    UPDATE run_execution_sessions
       SET released_at = now(),
           renewed_at = now(),
           status = 'released',
           -- Store cumulative active time so a restored run can resume from prior usage.
           active_duration_ms = LEAST(
               GREATEST(
                   run_execution_sessions.active_duration_ms,
                   sqlc.arg(release_active_duration_ms),
                   COALESCE((
                       SELECT SUM(run_usage_events.quantity)::bigint
                         FROM run_usage_events
                        WHERE run_usage_events.org_id = released.org_id
                          AND run_usage_events.run_id = released.id
                          AND run_usage_events.kind = 'active_time'
                   ), 0)
                   +
                   (EXTRACT(EPOCH FROM (now() - COALESCE(run_execution_sessions.started_at, run_execution_sessions.leased_at))) * 1000)::bigint
               ),
               released.max_duration_seconds::bigint * 1000
           )::bigint
      FROM released
     WHERE run_execution_sessions.id = sqlc.arg(session_id)
       AND run_execution_sessions.run_id = released.id
       AND run_execution_sessions.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_execution_sessions.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_execution_sessions.dispatch_lease_id = sqlc.arg(dispatch_lease_id)
    RETURNING run_execution_sessions.id,
              run_execution_sessions.attempt_id,
              run_execution_sessions.trace_id,
              run_execution_sessions.span_id,
              run_execution_sessions.parent_span_id,
              run_execution_sessions.traceparent,
              run_execution_sessions.active_duration_ms,
              run_execution_sessions.restore_checkpoint_id
),
released_concurrency_slot AS (
    UPDATE run_queue_concurrency_leases
       SET released_at = now()
      FROM released
     WHERE run_queue_concurrency_leases.org_id = sqlc.arg(org_id)
       AND run_queue_concurrency_leases.run_id = released.id
       AND run_queue_concurrency_leases.session_id = sqlc.arg(session_id)
       AND run_queue_concurrency_leases.released_at IS NULL
    RETURNING run_queue_concurrency_leases.id
),
cancelled_run_waits AS (
    UPDATE run_waits
       SET status = 'cancelled',
           failure = jsonb_build_object('reason', COALESCE(sqlc.arg(error_message)::text, 'session released'), 'source', 'release'),
           resolution_kind = 'cancelled',
           resolution = jsonb_build_object('reason', COALESCE(sqlc.arg(error_message)::text, 'session released'), 'source', 'release'),
           failed_at = now(),
           updated_at = now()
      FROM released
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.run_id = released.id
       AND run_waits.session_id = sqlc.arg(session_id)
       AND (
           run_waits.status IN ('opening', 'waiting')
           OR (run_waits.status = 'resuming' AND sqlc.arg(error_message)::text IS NOT NULL)
       )
    RETURNING run_waits.id, run_waits.org_id
),
cancelled_waitpoints AS (
    UPDATE waitpoints
       SET status = 'cancelled',
           resolution_kind = 'cancelled',
           output = 'null'::jsonb,
           resolution = jsonb_build_object('reason', COALESCE(sqlc.arg(error_message)::text, 'session released'), 'source', 'release'),
           output_is_error = true,
           completed_at = now(),
           updated_at = now()
      FROM cancelled_run_waits
      JOIN run_wait_dependencies ON run_wait_dependencies.org_id = cancelled_run_waits.org_id
                                AND run_wait_dependencies.run_wait_id = cancelled_run_waits.id
     WHERE waitpoints.org_id = run_wait_dependencies.org_id
       AND waitpoints.id = run_wait_dependencies.waitpoint_id
       AND waitpoints.status = 'pending'
    RETURNING waitpoints.id
),
invalidated_checkpoints AS (
    UPDATE checkpoints
       SET status = 'invalid',
           error_message = COALESCE(sqlc.arg(error_message)::text, 'session released'),
           invalidated_at = now()
      FROM released
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = released.id
       AND checkpoints.session_id = sqlc.arg(session_id)
       AND checkpoints.status IN ('creating', 'restoring')
    RETURNING checkpoints.run_id, checkpoints.id
),
completed_restore_checkpoint AS (
    UPDATE checkpoints
       SET status = 'ready',
           error_message = NULL,
           invalidated_at = NULL
      FROM released
      JOIN released_session ON true
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = released.id
       AND checkpoints.id = released_session.restore_checkpoint_id
       AND checkpoints.status = 'restoring'
       AND sqlc.arg(error_message)::text IS NULL
    RETURNING checkpoints.id
),
failed_restore_checkpoint AS (
    UPDATE checkpoints
       SET status = 'invalid',
           error_message = sqlc.arg(error_message)::text,
           invalidated_at = now()
      FROM released
      JOIN released_session ON true
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = released.id
       AND checkpoints.id = released_session.restore_checkpoint_id
       AND checkpoints.status = 'restoring'
       AND sqlc.arg(error_message)::text IS NOT NULL
    RETURNING checkpoints.run_id, checkpoints.id
),
resolved_restore_waitpoint AS (
    UPDATE run_waits
       SET status = 'restored',
           restored_at = now(),
           updated_at = now()
      FROM released
      JOIN released_session ON true
      JOIN completed_restore_checkpoint ON completed_restore_checkpoint.id = released_session.restore_checkpoint_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.run_id = released.id
       AND run_waits.checkpoint_id = released_session.restore_checkpoint_id
       AND run_waits.status = 'resuming'
       AND sqlc.arg(error_message)::text IS NULL
    RETURNING run_waits.id
),
released_attempt AS (
    UPDATE run_attempts
       SET status = sqlc.arg(status)::text::run_attempt_status,
           output = sqlc.arg(output),
           error_message = sqlc.arg(error_message),
           finished_at = now(),
           updated_at = now()
      FROM released
     WHERE run_attempts.org_id = released.org_id
       AND run_attempts.run_id = released.id
       AND run_attempts.id = released.current_attempt_id
    RETURNING run_attempts.id, run_attempts.attempt_number
),
active_time_delta AS (
    SELECT GREATEST(
               released_session.active_duration_ms
               - COALESCE((
                   SELECT SUM(run_usage_events.quantity)::bigint
                     FROM run_usage_events
                    WHERE run_usage_events.org_id = released.org_id
                      AND run_usage_events.run_id = released.id
                      AND run_usage_events.kind = 'active_time'
               ), 0),
               0
           )::bigint AS quantity
      FROM released
      JOIN released_session ON true
),
active_time_usage_event AS (
    INSERT INTO run_usage_events (org_id, project_id, environment_id, run_id, attempt_id, session_id, trace_id, span_id, source, kind, quantity, unit, billable, measured_to, attributes, idempotency_key)
    SELECT released.org_id,
           released.project_id,
           released.environment_id,
           released.id,
           released_session.attempt_id,
           released_session.id,
           released_session.trace_id,
           released_session.span_id,
           'worker',
           'active_time',
           active_time_delta.quantity,
           'ms',
           false,
           now(),
           jsonb_build_object('phase', 'final'),
           'active_time:' || released_session.id::text || ':final'
      FROM released
      JOIN released_session ON true
      JOIN active_time_delta ON true
     WHERE active_time_delta.quantity > 0
    ON CONFLICT DO NOTHING
    RETURNING id
),
output_usage_event AS (
    INSERT INTO run_usage_events (org_id, project_id, environment_id, run_id, attempt_id, session_id, trace_id, span_id, source, kind, quantity, unit, billable, measured_to, attributes, idempotency_key)
    SELECT released.org_id,
           released.project_id,
           released.environment_id,
           released.id,
           released_session.attempt_id,
           released_session.id,
           released_session.trace_id,
           released_session.span_id,
           'worker',
           'output_bytes',
           octet_length(sqlc.arg(output)::text)::bigint,
           'bytes',
           false,
           now(),
           jsonb_build_object('terminal_event_kind', sqlc.arg(terminal_event_kind)::text),
           'output:' || released_session.id::text || ':final'
      FROM released
      JOIN released_session ON true
     WHERE sqlc.arg(output)::jsonb IS NOT NULL
       AND octet_length(sqlc.arg(output)::text) > 0
    ON CONFLICT DO NOTHING
    RETURNING id
),
released_snapshot AS (
    INSERT INTO run_snapshots (org_id, run_id, version, status, attempt_id, session_id, transition, reason)
    SELECT released.org_id,
           released.id,
           released.state_version,
           released.status,
           released.current_attempt_id,
           released_session.id,
           CASE
             WHEN sqlc.arg(error_message)::text IS NULL THEN 'run.completed'
             ELSE 'run.failed'
           END,
           sqlc.arg(terminal_event_payload)
      FROM released
      JOIN released_session ON true
      JOIN released_attempt ON true
    RETURNING id
),
terminal_event AS (
    INSERT INTO run_events (org_id, project_id, environment_id, run_id, attempt_id, session_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT released.org_id,
           released.project_id,
           released.environment_id,
           released.id,
           released_session.attempt_id,
           released_session.id,
           released_attempt.attempt_number,
           released_session.trace_id,
           released_session.span_id,
           released_session.parent_span_id,
           released_session.traceparent,
           'lifecycle',
           CASE WHEN sqlc.arg(error_message)::text IS NULL THEN 'info' ELSE 'error' END,
           'control',
           sqlc.arg(terminal_event_kind)::text,
           sqlc.arg(terminal_event_kind)::text,
           sqlc.arg(terminal_event_payload),
           'internal',
           released.state_version
      FROM released
      JOIN released_session ON true
      JOIN released_attempt ON true
      JOIN released_snapshot ON true
    RETURNING id
),
cleanup AS (
    SELECT
        (SELECT count(*) FROM cancelled_waitpoints) AS cancelled_waitpoints,
        (SELECT count(*) FROM invalidated_checkpoints) AS invalidated_checkpoints,
        (SELECT count(*) FROM released_concurrency_slot) AS released_concurrency_slots,
        (SELECT count(*) FROM completed_restore_checkpoint) AS completed_restore_checkpoints,
        (SELECT count(*) FROM resolved_restore_waitpoint) AS resolved_restore_waitpoints,
        (SELECT count(*) FROM terminal_event) AS terminal_events,
        (SELECT count(*) FROM active_time_usage_event) AS active_time_usage_events,
        (SELECT count(*) FROM output_usage_event) AS output_usage_events
),
idempotent_released AS (
    SELECT runs.*
      FROM runs
      JOIN run_execution_sessions
        ON run_execution_sessions.org_id = runs.org_id
       AND run_execution_sessions.run_id = runs.id
       AND run_execution_sessions.id = sqlc.arg(session_id)
       AND run_execution_sessions.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_execution_sessions.dispatch_message_id = sqlc.arg(dispatch_message_id)
       AND run_execution_sessions.dispatch_lease_id = sqlc.arg(dispatch_lease_id)
       AND run_execution_sessions.status = 'released'
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = sqlc.arg(status)
       AND runs.current_session_id IS NULL
       AND runs.exit_code IS NOT DISTINCT FROM sqlc.arg(exit_code)
       AND runs.error_message IS NOT DISTINCT FROM sqlc.arg(error_message)
       AND runs.output IS NOT DISTINCT FROM sqlc.arg(output)::jsonb
       AND NOT EXISTS (SELECT 1 FROM released)
)
SELECT released.*
  FROM released
  JOIN released_session ON true
  JOIN completed_queue_entry ON true
  JOIN released_snapshot ON true
  JOIN cleanup ON true
UNION ALL
SELECT *
  FROM idempotent_released;
