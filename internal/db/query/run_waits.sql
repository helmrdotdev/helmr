-- name: CreateHotRunWait :one
WITH target AS (
    SELECT runs.*, run_leases.id AS lease_id
      FROM runs
      JOIN run_leases ON run_leases.org_id = runs.org_id
                     AND run_leases.run_id = runs.id
                     AND run_leases.id = runs.current_run_lease_id
     WHERE runs.org_id = sqlc.arg(org_id) AND runs.id = sqlc.arg(run_id)
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runs.state_version = sqlc.arg(expected_run_state_version)
       AND runs.status = 'running' AND run_leases.state = 'running'
     FOR UPDATE OF runs, run_leases
), inserted_wait AS (
    INSERT INTO waits (
        id, public_id, org_id, project_id, environment_id, kind,
        idempotency_key, correlation_key, stream_id, stream_sequence,
        token_id, completed_after, metadata, tags, expires_at
    )
    SELECT sqlc.arg(wait_id), sqlc.arg(public_id), target.org_id,
           target.project_id, target.environment_id, sqlc.arg(kind),
           sqlc.arg(correlation_key), sqlc.arg(correlation_key),
           sqlc.narg(stream_id), sqlc.narg(stream_sequence), sqlc.narg(token_id),
           sqlc.narg(completed_after), sqlc.arg(metadata), sqlc.arg(tags),
           sqlc.narg(expires_at)
      FROM target
    RETURNING *
), inserted AS (
    INSERT INTO run_waits (
        id, org_id, project_id, environment_id, run_id, wait_id, state,
        expected_run_state_version, current_run_lease_id, run_checkpoint_due_at,
        hot_wait_started_at
    )
    SELECT sqlc.arg(run_wait_id), target.org_id, target.project_id, target.environment_id,
           target.id, inserted_wait.id, 'hot_waiting', target.state_version + 1,
           target.lease_id, now() + sqlc.arg(checkpoint_delay)::interval, now()
      FROM target JOIN inserted_wait ON inserted_wait.org_id = target.org_id
    RETURNING *
), transitioned AS (
    UPDATE runs SET status = 'waiting', execution_status = 'waiting',
                    state_version = state_version + 1, updated_at = now()
      FROM inserted WHERE runs.id = inserted.run_id
    RETURNING runs.*
), snapshot AS (
    INSERT INTO run_state_snapshots
        (org_id, run_id, version, status, execution_status, terminal_outcome,
         attempt_number, run_lease_id, worker_instance_id, worker_epoch,
         runtime_instance_id, previous_version, transition, reason)
    SELECT transitioned.org_id, transitioned.id, transitioned.state_version,
           transitioned.status, transitioned.execution_status,
           transitioned.terminal_outcome, transitioned.current_attempt_number,
           target.lease_id, run_leases.worker_instance_id, run_leases.worker_epoch,
           run_leases.runtime_instance_id, transitioned.state_version - 1,
           'run.wait_entered', jsonb_build_object('wait_id', inserted.wait_id)
      FROM transitioned
      JOIN inserted ON inserted.run_id = transitioned.id
      JOIN target ON target.id = transitioned.id
      JOIN run_leases ON run_leases.id = target.lease_id
    RETURNING run_id
)
SELECT inserted.* FROM inserted JOIN snapshot ON snapshot.run_id = inserted.run_id;

-- name: GetRunWait :one
SELECT * FROM run_waits
 WHERE org_id = sqlc.arg(org_id) AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id) AND id = sqlc.arg(id);

-- name: GetWorkerRunWaitCreateScope :one
SELECT runs.org_id, runs.project_id, runs.environment_id, runs.id AS run_id,
       runs.state_version AS expected_run_state_version,
       run_leases.id AS run_lease_id, run_leases.worker_group_id,
       run_leases.worker_instance_id, run_leases.worker_epoch,
       run_leases.runtime_instance_id, run_leases.network_slot_id,
       run_leases.network_slot_generation
  FROM runs
  JOIN run_leases ON run_leases.org_id = runs.org_id
                 AND run_leases.run_id = runs.id
                 AND run_leases.id = runs.current_run_lease_id
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.id = sqlc.arg(run_id)
   AND run_leases.id = sqlc.arg(run_lease_id)
   AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
   AND run_leases.runtime_instance_id = sqlc.arg(runtime_instance_id)
   AND run_leases.network_slot_id = sqlc.arg(network_slot_id)
   AND run_leases.network_slot_generation = sqlc.arg(network_slot_generation)
   AND run_leases.state = 'running'
   AND run_leases.expires_at > now()
   AND runs.status = 'running'
   AND runs.execution_status = 'executing';

-- name: GetRunWaitByRun :one
SELECT * FROM run_waits
 WHERE org_id = sqlc.arg(org_id) AND run_id = sqlc.arg(run_id)
 ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: GetRunWaitByID :one
SELECT * FROM run_waits
 WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(run_wait_id);

-- name: ListRunWaits :many
SELECT * FROM run_waits
 WHERE org_id = sqlc.arg(org_id) AND run_id = sqlc.arg(run_id)
 ORDER BY created_at DESC, id DESC LIMIT sqlc.arg(limit_count);

-- name: ClaimRunCheckpointWait :one
WITH candidate AS (
    SELECT run_waits.id
      FROM run_waits
      JOIN waits ON waits.org_id = run_waits.org_id
                AND waits.id = run_waits.wait_id
      JOIN run_leases ON run_leases.org_id = run_waits.org_id
                     AND run_leases.run_id = run_waits.run_id
                     AND run_leases.id = run_waits.current_run_lease_id
     WHERE run_waits.org_id = sqlc.arg(org_id) AND run_waits.run_id = sqlc.arg(run_id)
       AND run_waits.id = sqlc.arg(run_wait_id) AND run_waits.current_run_lease_id = sqlc.arg(run_lease_id)
       AND run_waits.expected_run_state_version = sqlc.arg(expected_run_state_version)
       AND run_waits.state = 'hot_waiting' AND run_waits.run_checkpoint_due_at <= now()
       AND run_waits.resume_ack_version = run_waits.resume_request_version
       AND waits.state = 'pending'
       AND run_leases.state = 'running'
     FOR UPDATE OF waits, run_waits, run_leases
), claimed AS (
    UPDATE run_waits
       SET state = 'checkpointing', run_checkpoint_started_at = now(),
           checkpoint_request_version = checkpoint_request_version + 1,
           checkpoint_attempt_id = uuidv7(),
           checkpoint_requested_at = now(), updated_at = now()
      FROM candidate
     WHERE run_waits.id = candidate.id
    RETURNING run_waits.*
), fenced_lease AS (
    UPDATE run_leases
       SET state = 'checkpointing', updated_at = now()
      FROM claimed
     WHERE run_leases.org_id = claimed.org_id
       AND run_leases.run_id = claimed.run_id
       AND run_leases.id = claimed.current_run_lease_id
       AND run_leases.state = 'running'
    RETURNING run_leases.id
)
SELECT claimed.*, run_leases.worker_instance_id, run_leases.worker_epoch,
       run_leases.runtime_instance_id
  FROM claimed
  JOIN fenced_lease ON fenced_lease.id = claimed.current_run_lease_id
  JOIN run_leases ON run_leases.org_id = claimed.org_id
                 AND run_leases.run_id = claimed.run_id
                 AND run_leases.id = claimed.current_run_lease_id;

-- name: ListDueRunCheckpointWaits :many
SELECT run_waits.org_id, run_waits.run_id, run_waits.id AS run_wait_id,
       run_waits.current_run_lease_id AS run_lease_id,
       run_waits.expected_run_state_version
  FROM run_waits
  JOIN waits ON waits.org_id = run_waits.org_id
            AND waits.id = run_waits.wait_id
 WHERE run_waits.state = 'hot_waiting' AND run_waits.run_checkpoint_due_at <= now()
   AND run_waits.resume_ack_version = run_waits.resume_request_version
   AND waits.state = 'pending'
 ORDER BY run_waits.run_checkpoint_due_at, run_waits.id
 LIMIT sqlc.arg(limit_count);

-- name: MarkRunResumeWaitResumed :one
WITH target AS MATERIALIZED (
    SELECT run_waits.*, run_leases.worker_instance_id,
           run_leases.worker_epoch, run_leases.runtime_instance_id
      FROM run_waits
      JOIN runs ON runs.org_id = run_waits.org_id
               AND runs.id = run_waits.run_id
               AND runs.current_run_lease_id = run_waits.current_run_lease_id
               AND runs.state_version = run_waits.expected_run_state_version
      JOIN run_leases ON run_leases.org_id = run_waits.org_id
                     AND run_leases.run_id = run_waits.run_id
                     AND run_leases.id = run_waits.current_run_lease_id
                     AND run_leases.state = 'running'
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.run_id = sqlc.arg(run_id)
       AND run_waits.id = sqlc.arg(run_wait_id)
       AND run_waits.current_run_lease_id = sqlc.arg(run_lease_id)
       AND run_waits.run_checkpoint_id IS NOT DISTINCT FROM sqlc.narg(run_checkpoint_id)::uuid
       AND run_waits.resume_request_version = sqlc.arg(resume_request_version)
       AND run_waits.resume_ack_version < run_waits.resume_request_version
       AND run_waits.state IN ('hot_waiting', 'resuming')
     FOR UPDATE OF run_leases, runs, run_waits
), transitioned AS (
    UPDATE runs
       SET status = 'running', execution_status = 'executing',
           state_version = runs.state_version + 1,
           active_started_at = now(), updated_at = now()
      FROM target
     WHERE runs.org_id = target.org_id
       AND runs.id = target.run_id
       AND runs.current_run_lease_id = target.current_run_lease_id
       AND runs.state_version = target.expected_run_state_version
    RETURNING runs.*, target.id AS target_wait_id,
              target.current_run_lease_id AS target_run_lease_id,
              target.run_checkpoint_id AS target_checkpoint_id,
              target.worker_instance_id AS target_worker_instance_id,
              target.worker_epoch AS target_worker_epoch,
              target.runtime_instance_id AS target_runtime_instance_id
), acknowledged AS (
    UPDATE run_waits
       SET resume_ack_version = sqlc.arg(resume_request_version),
           resume_acknowledged_at = now(), state = 'released', released_at = now(),
           terminal_at = now(), terminal_reason_code = 'resumed',
           expected_run_state_version = transitioned.state_version,
           updated_at = now()
      FROM transitioned
     WHERE run_waits.org_id = transitioned.org_id
       AND run_waits.run_id = transitioned.id
       AND run_waits.id = transitioned.target_wait_id
       AND run_waits.current_run_lease_id = transitioned.target_run_lease_id
       AND run_waits.resume_ack_version < run_waits.resume_request_version
       AND run_waits.state IN ('hot_waiting', 'resuming')
    RETURNING run_waits.*
), provenance AS (
    SELECT transitioned.* FROM transitioned
      JOIN acknowledged ON acknowledged.org_id = transitioned.org_id
                       AND acknowledged.id = transitioned.target_wait_id
), snapshot AS (
    INSERT INTO run_state_snapshots
        (org_id, run_id, version, status, execution_status, terminal_outcome,
         attempt_number, run_lease_id, worker_instance_id, worker_epoch,
         runtime_instance_id, run_checkpoint_id, previous_version, transition, reason)
    SELECT org_id, id, state_version, status, execution_status, terminal_outcome,
           current_attempt_number, target_run_lease_id, target_worker_instance_id,
           target_worker_epoch, target_runtime_instance_id, target_checkpoint_id,
           state_version - 1, 'run.wait_resumed',
           jsonb_build_object('run_wait_id', target_wait_id)
      FROM provenance RETURNING run_id
)
SELECT acknowledged.* FROM acknowledged JOIN snapshot ON snapshot.run_id = acknowledged.run_id;

-- name: RequeueResolvedRunWaits :many
WITH candidates AS MATERIALIZED (
    SELECT run_waits.id, run_waits.state
      FROM run_waits
      JOIN waits ON waits.org_id = run_waits.org_id
                AND waits.id = run_waits.wait_id
      JOIN runs ON runs.org_id = run_waits.org_id
               AND runs.id = run_waits.run_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
       AND run_waits.resume_ack_version = run_waits.resume_request_version
       AND waits.state IN ('completed', 'cancelled', 'expired')
       AND runs.status = 'waiting'
       AND runs.state_version = run_waits.expected_run_state_version
       AND (
           run_waits.state = 'hot_waiting'
           OR (
               run_waits.current_run_lease_id IS NULL
               AND runs.current_run_lease_id IS NULL
           )
       )
     ORDER BY run_waits.created_at, run_waits.id
     LIMIT sqlc.arg(limit_count)
     FOR UPDATE OF run_waits, runs SKIP LOCKED
), queued_runs AS (
    UPDATE runs
       SET status = 'queued', execution_status = 'queued',
           state_version = runs.state_version + 1,
           queue_timestamp = now(), queued_expires_at = NULL, updated_at = now()
      FROM candidates, run_waits
     WHERE candidates.state = 'checkpointed_waiting'
       AND run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.id = candidates.id
       AND runs.org_id = run_waits.org_id
       AND runs.id = run_waits.run_id
       AND runs.status = 'waiting'
       AND runs.current_run_lease_id IS NULL
       AND runs.state_version = run_waits.expected_run_state_version
    RETURNING runs.*, run_waits.id AS requested_wait_id
), requested AS (
    UPDATE run_waits
       SET expected_run_state_version = COALESCE(
               (SELECT queued_runs.state_version
                  FROM queued_runs
                 WHERE queued_runs.org_id = run_waits.org_id
                   AND queued_runs.requested_wait_id = run_waits.id),
               run_waits.expected_run_state_version
           ),
           resume_request_version = run_waits.resume_request_version + 1,
           resume_requested_at = now(), updated_at = now()
      FROM candidates
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.id = candidates.id
       AND (
           candidates.state = 'hot_waiting'
           OR EXISTS (
               SELECT 1 FROM queued_runs
                WHERE queued_runs.org_id = run_waits.org_id
                  AND queued_runs.requested_wait_id = run_waits.id
           )
       )
    RETURNING run_waits.*
), snapshots AS (
    INSERT INTO run_state_snapshots
        (org_id, run_id, version, status, execution_status, terminal_outcome,
         attempt_number, run_checkpoint_id, previous_version, transition, reason)
    SELECT queued_runs.org_id, queued_runs.id, queued_runs.state_version,
           queued_runs.status, queued_runs.execution_status,
           queued_runs.terminal_outcome, queued_runs.current_attempt_number,
           requested.run_checkpoint_id, queued_runs.state_version - 1,
           'run.wait_resume_queued',
           jsonb_build_object('run_wait_id', requested.id)
      FROM queued_runs
      JOIN requested ON requested.org_id = queued_runs.org_id
                    AND requested.id = queued_runs.requested_wait_id
    RETURNING org_id, run_id
)
SELECT requested.*
  FROM requested
 WHERE requested.state = 'hot_waiting'
UNION ALL
SELECT requested.*
  FROM requested
  JOIN snapshots ON snapshots.org_id = requested.org_id
                AND snapshots.run_id = requested.run_id
 WHERE requested.state = 'checkpointed_waiting';

-- name: RequeueStaleResumingRunWaits :many
WITH candidates AS MATERIALIZED (
    SELECT run_waits.id AS run_wait_id, run_waits.org_id, run_waits.run_id,
           run_waits.current_run_lease_id
      FROM run_waits
      JOIN runs ON runs.org_id = run_waits.org_id
               AND runs.id = run_waits.run_id
               AND runs.current_run_lease_id = run_waits.current_run_lease_id
               AND runs.state_version = run_waits.expected_run_state_version
      JOIN run_leases ON run_leases.org_id = run_waits.org_id
                     AND run_leases.run_id = run_waits.run_id
                     AND run_leases.id = run_waits.current_run_lease_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.state = 'resuming'
       AND run_waits.resume_ack_version < run_waits.resume_request_version
       AND run_waits.resuming_at <= now() - sqlc.arg(stale_after)::interval
       AND run_leases.state IN ('assigned', 'starting', 'running')
     ORDER BY run_waits.resuming_at, run_waits.id
     LIMIT sqlc.arg(limit_count)
     FOR UPDATE OF run_leases, runs, run_waits SKIP LOCKED
), stale_leases AS (
    UPDATE run_leases
       SET state = 'lost', terminal_at = now(),
           terminal_reason_code = 'resume_ack_timeout',
           terminal_error = jsonb_build_object('message', 'resume acknowledgement timed out'),
           updated_at = now()
      FROM candidates
     WHERE run_leases.org_id = candidates.org_id
       AND run_leases.run_id = candidates.run_id
       AND run_leases.id = candidates.current_run_lease_id
       AND run_leases.state IN ('assigned', 'starting', 'running')
    RETURNING run_leases.*
), released_workspace_leases AS (
    UPDATE workspace_leases
       SET state = 'released', released_at = now(), terminal_at = now(),
           terminal_reason_code = 'resume_ack_timeout', updated_at = now()
      FROM stale_leases
     WHERE workspace_leases.org_id = stale_leases.org_id
       AND workspace_leases.owner_run_id = stale_leases.run_id
       AND workspace_leases.lease_kind = 'write'
       AND workspace_leases.state IN ('active', 'releasing')
    RETURNING workspace_leases.id
), requested_mount_stop AS (
    UPDATE workspace_mounts
       SET state = 'unmounting', stopped_at = COALESCE(stopped_at, now()),
           updated_at = now()
      FROM stale_leases
     WHERE workspace_mounts.org_id = stale_leases.org_id
       AND workspace_mounts.workspace_id = stale_leases.workspace_id
       AND workspace_mounts.runtime_instance_id = stale_leases.runtime_instance_id
       AND workspace_mounts.state IN ('mounting', 'mounted')
    RETURNING workspace_mounts.id
), requested_runtime_close AS (
    UPDATE runtime_instances
       SET desired_state = 'closed', desired_version = desired_version + 1,
           desired_at = now(), desired_reason = 'resume_ack_timeout', updated_at = now()
      FROM stale_leases
     WHERE runtime_instances.org_id = stale_leases.org_id
       AND runtime_instances.id = stale_leases.runtime_instance_id
       AND runtime_instances.worker_group_id = stale_leases.worker_group_id
       AND runtime_instances.worker_instance_id = stale_leases.worker_instance_id
       AND runtime_instances.worker_epoch = stale_leases.worker_epoch
       AND runtime_instances.desired_state <> 'closed'
       AND runtime_instances.observed_state IN ('allocated', 'preparing', 'ready')
    RETURNING runtime_instances.id
), transitioned AS (
    UPDATE runs
       SET status = 'queued', execution_status = 'queued', terminal_outcome = NULL,
           current_run_lease_id = NULL, state_version = runs.state_version + 1,
           active_elapsed_ms = runs.active_elapsed_ms + CASE
               WHEN stale_leases.started_at IS NULL THEN 0
               ELSE GREATEST((extract(epoch FROM (now() - stale_leases.started_at)) * 1000)::bigint, 0)
           END,
           active_started_at = NULL, error_message = NULL,
           queue_timestamp = now(), queued_expires_at = NULL, updated_at = now()
      FROM stale_leases
     WHERE runs.org_id = stale_leases.org_id
       AND runs.id = stale_leases.run_id
       AND runs.current_run_lease_id = stale_leases.id
    RETURNING runs.*, stale_leases.id AS stale_run_lease_id,
              stale_leases.started_at AS stale_started_at,
              stale_leases.task_attempt_number AS stale_attempt_number,
              stale_leases.trace_id AS stale_trace_id,
              stale_leases.span_id AS stale_span_id,
              stale_leases.requested_cpu_millis AS stale_cpu_millis,
              stale_leases.requested_memory_bytes AS stale_memory_bytes,
              stale_leases.requested_workload_disk_bytes AS stale_workload_disk_bytes,
              stale_leases.requested_scratch_bytes AS stale_scratch_bytes,
              stale_leases.requested_execution_slots AS stale_execution_slots
), requeued_waits AS (
    UPDATE run_waits
       SET state = 'checkpointed_waiting', current_run_lease_id = NULL,
           resuming_at = NULL,
           expected_run_state_version = transitioned.state_version,
           updated_at = now()
      FROM transitioned
     WHERE run_waits.org_id = transitioned.org_id
       AND run_waits.run_id = transitioned.id
       AND run_waits.current_run_lease_id = transitioned.stale_run_lease_id
       AND run_waits.state = 'resuming'
       AND run_waits.resume_ack_version < run_waits.resume_request_version
    RETURNING run_waits.*
), meter_event AS (
    INSERT INTO meter_events (
        org_id, project_id, environment_id, run_id, run_lease_id, attempt_number,
        trace_id, span_id, meter, quantity, unit, measured_from, measured_to,
        details, idempotency_key, idempotency_fingerprint
    )
    SELECT transitioned.org_id, transitioned.project_id, transitioned.environment_id,
           transitioned.id, transitioned.stale_run_lease_id,
           transitioned.stale_attempt_number, transitioned.stale_trace_id,
           transitioned.stale_span_id, 'active_time',
           GREATEST((extract(epoch FROM (now() - transitioned.stale_started_at)) * 1000)::bigint, 0),
           'milliseconds', transitioned.stale_started_at, now(),
           jsonb_build_object(
               'transition', 'resume_ack_timeout_requeued',
               'cpu_millis', transitioned.stale_cpu_millis,
               'memory_bytes', transitioned.stale_memory_bytes,
               'workload_disk_bytes', transitioned.stale_workload_disk_bytes,
               'scratch_bytes', transitioned.stale_scratch_bytes,
               'execution_slots', transitioned.stale_execution_slots
           ),
           'resume-ack-timeout:' || transitioned.stale_run_lease_id::text,
           jsonb_build_object(
               'quantity', GREATEST((extract(epoch FROM (now() - transitioned.stale_started_at)) * 1000)::bigint, 0),
               'unit', 'milliseconds', 'measured_from', transitioned.stale_started_at,
               'measured_to', now(), 'transition', 'resume_ack_timeout_requeued',
               'cpu_millis', transitioned.stale_cpu_millis,
               'memory_bytes', transitioned.stale_memory_bytes,
               'workload_disk_bytes', transitioned.stale_workload_disk_bytes,
               'scratch_bytes', transitioned.stale_scratch_bytes,
               'execution_slots', transitioned.stale_execution_slots
           )::text
      FROM transitioned
     WHERE transitioned.stale_started_at IS NOT NULL
       AND transitioned.stale_started_at < now()
    ON CONFLICT (org_id, source_type, source_id, meter, idempotency_key)
    DO UPDATE SET idempotency_fingerprint = meter_events.idempotency_fingerprint
     WHERE meter_events.idempotency_fingerprint = excluded.idempotency_fingerprint
    RETURNING *
), meter_outbox AS (
    INSERT INTO telemetry_outbox (
        org_id, stream_kind, source_kind, source_id, project_id, environment_id,
        run_id, run_lease_id, meter_event_id, attempt_number, trace_id, span_id,
        kind, payload, idempotency_key, observed_at
    )
    SELECT org_id, 'meter_event', source_type, source_id, project_id, environment_id,
           run_id, run_lease_id, id, attempt_number, trace_id, span_id,
           meter, details, idempotency_key, occurred_at
      FROM meter_event
    ON CONFLICT DO NOTHING
    RETURNING meter_event_id
), snapshots AS (
    INSERT INTO run_state_snapshots
        (org_id, run_id, version, status, execution_status, terminal_outcome,
         attempt_number, run_lease_id, previous_version, transition, reason, error)
    SELECT transitioned.org_id, transitioned.id, transitioned.state_version,
           transitioned.status, transitioned.execution_status,
           transitioned.terminal_outcome, transitioned.current_attempt_number,
           transitioned.stale_run_lease_id, transitioned.state_version - 1,
           'run.wait_resume_timeout_requeued',
           jsonb_build_object('reason_code', 'resume_ack_timeout'),
           jsonb_build_object('message', 'resume acknowledgement timed out and was redriven')
      FROM transitioned
      JOIN requeued_waits ON requeued_waits.org_id = transitioned.org_id
                         AND requeued_waits.run_id = transitioned.id
     WHERE NOT EXISTS (SELECT 1 FROM meter_event)
        OR EXISTS (SELECT 1 FROM meter_outbox)
    RETURNING org_id, run_id
)
SELECT requeued_waits.*
  FROM requeued_waits
  JOIN snapshots ON snapshots.org_id = requeued_waits.org_id
                AND snapshots.run_id = requeued_waits.run_id;

-- name: SetRunWaitWorkspaceVersion :one
WITH target AS (
    SELECT run_waits.id AS run_wait_id, run_waits.org_id, run_waits.run_id,
           run_waits.expected_run_state_version, run_waits.current_run_lease_id,
           runs.active_elapsed_ms AS prior_active_elapsed_ms,
           run_leases.worker_instance_id, run_leases.worker_epoch,
           run_leases.runtime_instance_id
      FROM run_waits
      JOIN runs ON runs.org_id = run_waits.org_id AND runs.id = run_waits.run_id
      JOIN run_leases ON run_leases.org_id = run_waits.org_id
                     AND run_leases.run_id = run_waits.run_id
                     AND run_leases.id = run_waits.current_run_lease_id
     WHERE run_waits.org_id = sqlc.arg(org_id) AND run_waits.run_id = sqlc.arg(run_id)
       AND run_waits.id = sqlc.arg(run_wait_id)
       AND run_waits.current_run_lease_id = sqlc.arg(run_lease_id)
       AND run_waits.checkpoint_request_version = sqlc.arg(checkpoint_request_version)
       AND run_waits.checkpoint_attempt_id = sqlc.arg(run_checkpoint_id)
       AND run_waits.reserved_workspace_id = sqlc.arg(reserved_workspace_id)
       AND run_waits.reserved_workspace_version_id = sqlc.arg(reserved_workspace_version_id)
       AND run_waits.checkpoint_ack_version < run_waits.checkpoint_request_version
       AND run_waits.state = 'checkpointing'
       AND runs.state_version = run_waits.expected_run_state_version
       AND runs.current_run_lease_id = run_waits.current_run_lease_id
       AND run_leases.state = 'checkpointing'
     FOR UPDATE OF runs, run_waits, run_leases
), checkpoint AS (
    UPDATE run_checkpoints
       SET state = 'ready', ready_at = now()
      FROM target
     WHERE run_checkpoints.org_id = target.org_id
       AND run_checkpoints.run_id = target.run_id
       AND run_checkpoints.id = sqlc.arg(run_checkpoint_id)
       AND run_checkpoints.run_wait_id = target.run_wait_id
       AND run_checkpoints.source_run_lease_id = target.current_run_lease_id
       AND run_checkpoints.state = 'creating'
       AND run_checkpoints.creation_expires_at > now()
       AND run_checkpoints.manifest <> '{}'::jsonb
       AND EXISTS (
           SELECT 1 FROM run_checkpoint_artifacts
            WHERE run_checkpoint_artifacts.org_id = run_checkpoints.org_id
              AND run_checkpoint_artifacts.run_id = run_checkpoints.run_id
              AND run_checkpoint_artifacts.run_checkpoint_id = run_checkpoints.id
       )
    RETURNING run_checkpoints.*
), released_lease AS (
    UPDATE run_leases
       SET state = 'checkpointed', checkpointed_at = now(), terminal_at = now(),
           terminal_reason_code = 'checkpoint_committed', updated_at = now()
      FROM target, checkpoint
     WHERE run_leases.org_id = target.org_id AND run_leases.run_id = target.run_id
       AND run_leases.id = target.current_run_lease_id
    RETURNING run_leases.*
), released_workspace_lease AS (
    UPDATE workspace_leases
       SET state = 'released', released_at = now(), terminal_at = now(),
           terminal_reason_code = 'checkpoint_committed', updated_at = now()
      FROM checkpoint, target
     WHERE workspace_leases.org_id = target.org_id
       AND workspace_leases.workspace_id = checkpoint.workspace_id
       AND workspace_leases.id = checkpoint.source_workspace_lease_id
       AND workspace_leases.owner_run_id = target.run_id
       AND workspace_leases.state = 'active'
    RETURNING workspace_leases.id
), requested_runtime_close AS (
    UPDATE runtime_instances
       SET desired_state = 'closed', desired_version = desired_version + 1,
           desired_at = now(), desired_reason = 'run_wait_checkpointed', updated_at = now()
      FROM released_lease
     WHERE runtime_instances.org_id = released_lease.org_id
       AND runtime_instances.id = released_lease.runtime_instance_id
       AND runtime_instances.worker_instance_id = released_lease.worker_instance_id
       AND runtime_instances.worker_epoch = released_lease.worker_epoch
       AND runtime_instances.desired_state <> 'closed'
       AND runtime_instances.observed_state IN ('allocated','preparing','ready')
    RETURNING runtime_instances.id
), updated_wait AS (
UPDATE run_waits
   SET state = 'checkpointed_waiting', prior_run_lease_id = current_run_lease_id,
       current_run_lease_id = NULL, run_checkpoint_id = sqlc.arg(run_checkpoint_id),
       reserved_workspace_id = sqlc.arg(reserved_workspace_id),
       reserved_workspace_version_id = sqlc.arg(reserved_workspace_version_id),
       active_elapsed_ms_at_park = sqlc.arg(active_elapsed_ms_at_park),
       checkpoint_ack_version = sqlc.arg(checkpoint_request_version),
       checkpoint_acknowledged_at = now(), updated_at = now()
  FROM target, checkpoint, released_lease, released_workspace_lease, requested_runtime_close
 WHERE run_waits.id = target.run_wait_id
RETURNING run_waits.*
), transitioned AS (
    UPDATE runs
       SET current_run_lease_id = NULL, state_version = state_version + 1,
           status = 'waiting', execution_status = 'waiting',
           active_elapsed_ms = GREATEST(active_elapsed_ms, sqlc.arg(active_elapsed_ms_at_park)::bigint),
           active_started_at = NULL, updated_at = now()
      FROM target, updated_wait
     WHERE runs.org_id = target.org_id AND runs.id = target.run_id
       AND runs.state_version = target.expected_run_state_version
    RETURNING runs.*
), aligned_wait AS (
    UPDATE run_waits
       SET expected_run_state_version = transitioned.state_version, updated_at = now()
      FROM transitioned, updated_wait
     WHERE run_waits.id = updated_wait.id
    RETURNING run_waits.*
), meter_event AS (
    INSERT INTO meter_events (
        org_id, project_id, environment_id, run_id, run_lease_id, attempt_number,
        trace_id, span_id, meter, quantity, unit, measured_from, measured_to,
        details, idempotency_key, idempotency_fingerprint
    )
    SELECT transitioned.org_id, transitioned.project_id, transitioned.environment_id,
           transitioned.id, released_lease.id, released_lease.task_attempt_number,
           released_lease.trace_id, released_lease.span_id, 'active_time',
           GREATEST(sqlc.arg(active_elapsed_ms_at_park)::bigint - target.prior_active_elapsed_ms, 0),
           'milliseconds', released_lease.started_at, now(),
           jsonb_build_object(
               'transition','checkpointed',
               'cpu_millis',released_lease.requested_cpu_millis,
               'memory_bytes',released_lease.requested_memory_bytes,
               'workload_disk_bytes',released_lease.requested_workload_disk_bytes,
               'scratch_bytes',released_lease.requested_scratch_bytes,
               'execution_slots',released_lease.requested_execution_slots
           ),
           'checkpoint:' || released_lease.id::text,
           jsonb_build_object(
               'quantity', GREATEST(sqlc.arg(active_elapsed_ms_at_park)::bigint - target.prior_active_elapsed_ms, 0),
               'unit','milliseconds','measured_from',released_lease.started_at,'measured_to',now(),
               'transition','checkpointed','cpu_millis',released_lease.requested_cpu_millis,
               'memory_bytes',released_lease.requested_memory_bytes,
               'workload_disk_bytes',released_lease.requested_workload_disk_bytes,
               'scratch_bytes',released_lease.requested_scratch_bytes,
               'execution_slots',released_lease.requested_execution_slots
           )::text
      FROM transitioned, released_lease, target
     WHERE sqlc.arg(active_elapsed_ms_at_park)::bigint > target.prior_active_elapsed_ms
       AND released_lease.started_at < now()
    ON CONFLICT (org_id, source_type, source_id, meter, idempotency_key)
    DO UPDATE SET idempotency_fingerprint = meter_events.idempotency_fingerprint
     WHERE meter_events.idempotency_fingerprint = excluded.idempotency_fingerprint
    RETURNING *
), meter_outbox AS (
    INSERT INTO telemetry_outbox (
        org_id, stream_kind, source_kind, source_id, project_id, environment_id,
        run_id, run_lease_id, meter_event_id, attempt_number, trace_id, span_id,
        kind, payload, idempotency_key, observed_at
    )
    SELECT meter_event.org_id, 'meter_event', meter_event.source_type, meter_event.source_id,
           meter_event.project_id, meter_event.environment_id, meter_event.run_id,
           meter_event.run_lease_id, meter_event.id, meter_event.attempt_number,
           meter_event.trace_id, meter_event.span_id, meter_event.meter,
           meter_event.details, meter_event.idempotency_key, meter_event.occurred_at
      FROM meter_event
    ON CONFLICT DO NOTHING
    RETURNING meter_event_id
), snapshot AS (
    INSERT INTO run_state_snapshots
        (org_id, run_id, version, status, execution_status, terminal_outcome,
         attempt_number, run_lease_id, worker_instance_id, worker_epoch,
         runtime_instance_id, run_checkpoint_id, previous_version, transition, reason)
    SELECT transitioned.org_id, transitioned.id, transitioned.state_version,
           transitioned.status, transitioned.execution_status,
           transitioned.terminal_outcome, transitioned.current_attempt_number,
           released_lease.id, released_lease.worker_instance_id,
           released_lease.worker_epoch, released_lease.runtime_instance_id,
           checkpoint.id, transitioned.state_version - 1,
           'run.wait_checkpointed', jsonb_build_object('run_wait_id', aligned_wait.id)
      FROM transitioned, aligned_wait, released_lease, checkpoint, target
     WHERE sqlc.arg(active_elapsed_ms_at_park)::bigint <= target.prior_active_elapsed_ms
        OR EXISTS (SELECT 1 FROM meter_outbox)
    RETURNING run_id
)
SELECT aligned_wait.* FROM aligned_wait JOIN snapshot ON snapshot.run_id = aligned_wait.run_id;

-- name: CancelRunWait :one
UPDATE run_waits
   SET state = 'cancelled', cancelled_at = now(), terminal_at = now(),
       terminal_reason_code = sqlc.arg(reason_code), terminal_error = sqlc.narg(error),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(run_wait_id)
   AND state IN ('hot_waiting', 'checkpointing', 'checkpointed_waiting', 'resuming')
RETURNING *;

-- name: CancelRunWaitsForRun :many
UPDATE run_waits
   SET state = 'cancelled', cancelled_at = now(), terminal_at = now(),
       terminal_reason_code = sqlc.arg(reason_code), updated_at = now()
 WHERE org_id = sqlc.arg(org_id) AND run_id = sqlc.arg(run_id)
   AND state IN ('hot_waiting', 'checkpointing', 'checkpointed_waiting', 'resuming')
RETURNING *;

-- name: ExpireDueRunWaits :many
UPDATE run_waits
   SET state = 'failed', failed_at = now(), terminal_at = now(),
       terminal_reason_code = 'wait_expired', updated_at = now()
 WHERE id IN (
     SELECT run_waits.id FROM run_waits JOIN waits ON waits.id = run_waits.wait_id
      WHERE waits.expires_at <= now()
        AND run_waits.state IN ('hot_waiting','checkpointing','checkpointed_waiting','resuming')
      ORDER BY waits.expires_at, run_waits.id LIMIT sqlc.arg(limit_count)
      FOR UPDATE OF run_waits SKIP LOCKED
 )
RETURNING *;

-- name: GetWorkerRunWaitScope :one
SELECT run_waits.*, run_leases.worker_group_id, run_leases.worker_instance_id,
       run_leases.worker_epoch, run_leases.runtime_instance_id,
       run_leases.network_slot_id, run_leases.network_slot_generation,
       runs.runtime_identity_id, runs.cni_profile, runs.requested_milli_cpu,
       runs.requested_memory_mib, runs.requested_disk_mib,
       runs.session_id, runs.deployment_id, runs.task_id
  FROM run_waits
  JOIN run_leases ON run_leases.org_id = run_waits.org_id
                 AND run_leases.run_id = run_waits.run_id
                 AND run_leases.id = run_waits.current_run_lease_id
  JOIN runs ON runs.org_id = run_waits.org_id AND runs.id = run_waits.run_id
 WHERE run_waits.org_id = sqlc.arg(org_id) AND run_waits.id = sqlc.arg(run_wait_id)
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.worker_epoch = sqlc.arg(worker_epoch);
