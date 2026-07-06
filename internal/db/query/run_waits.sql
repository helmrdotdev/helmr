-- name: CreateHotRunWait :one
WITH scope AS MATERIALIZED (
    SELECT runs.org_id,
           runs.worker_group_id,
           runs.project_id,
           runs.environment_id,
           runs.id AS run_id,
           runs.state_version,
           run_leases.id AS run_lease_id,
           run_leases.worker_instance_id,
           workspace_mounts.runtime_instance_id,
           runtime_instances.runtime_epoch
      FROM runs
      JOIN run_leases ON run_leases.org_id = runs.org_id
                     AND run_leases.run_id = runs.id
                     AND run_leases.id = runs.current_run_lease_id
      JOIN workspace_mounts
        ON workspace_mounts.org_id = runs.org_id
       AND workspace_mounts.project_id = runs.project_id
       AND workspace_mounts.environment_id = runs.environment_id
       AND workspace_mounts.id = runs.workspace_mount_id
      JOIN runtime_instances
        ON runtime_instances.org_id = workspace_mounts.org_id
       AND runtime_instances.id = workspace_mounts.runtime_instance_id
       AND runtime_instances.worker_instance_id = run_leases.worker_instance_id
       AND runtime_instances.workspace_mount_id = workspace_mounts.id
       AND runtime_instances.state IN ('running', 'waiting_hot')
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.project_id = sqlc.arg(project_id)
       AND runs.environment_id = sqlc.arg(environment_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND runs.status = 'running'
       AND runs.execution_status = 'executing'
       AND run_leases.status IN ('leased', 'running')
       AND run_leases.lease_expires_at > now()
     FOR UPDATE OF runtime_instances
),
inserted_wait AS (
    INSERT INTO waits (
        id,
        public_id,
        org_id,
        project_id,
        environment_id,
        kind,
        state,
        correlation_key,
        stream_id,
        stream_sequence,
        token_id,
        completed_after,
        expires_at
    )
    SELECT sqlc.arg(wait_id),
           sqlc.arg(public_id),
           scope.org_id,
           scope.project_id,
           scope.environment_id,
           sqlc.arg(kind)::wait_kind,
           'pending'::wait_state,
           COALESCE(sqlc.arg(correlation_key)::text, ''),
           sqlc.narg(stream_id)::uuid,
           sqlc.narg(stream_sequence)::bigint,
           sqlc.narg(token_id)::uuid,
           sqlc.narg(completed_after)::timestamptz,
           sqlc.narg(expires_at)::timestamptz
      FROM scope
    RETURNING *
),
inserted_run_wait AS (
    INSERT INTO run_waits (
        id,
        org_id,
        worker_group_id,
        project_id,
        environment_id,
        run_id,
        wait_id,
        state,
        runtime_checkpoint_due_at,
        hot_wait_started_at,
        owner_runtime_instance_id,
        owner_runtime_epoch,
        owner_run_id,
        owner_run_lease_id,
        owner_worker_instance_id,
        owner_run_state_version
    )
    SELECT sqlc.arg(run_wait_id),
           scope.org_id,
           scope.worker_group_id,
           scope.project_id,
           scope.environment_id,
           scope.run_id,
           inserted_wait.id,
           'hot_waiting'::run_wait_state,
           CASE
             WHEN sqlc.arg(checkpoint_delay)::interval <= interval '0 seconds' THEN now()
             ELSE now() + sqlc.arg(checkpoint_delay)::interval
           END,
           now(),
           scope.runtime_instance_id,
           scope.runtime_epoch,
           scope.run_id,
           scope.run_lease_id,
           scope.worker_instance_id,
           scope.state_version
      FROM scope
      JOIN inserted_wait ON true
    RETURNING *
),
waiting_runtime_instance AS (
    UPDATE runtime_instances
       SET state = 'waiting_hot',
           owner_run_id = inserted_wait.run_id,
           owner_run_lease_id = inserted_wait.owner_run_lease_id,
           owner_run_wait_id = inserted_wait.id,
           owner_run_state_version = inserted_wait.owner_run_state_version,
           waiting_at = now(),
           updated_at = now()
      FROM scope, inserted_run_wait AS inserted_wait
     WHERE runtime_instances.org_id = scope.org_id
       AND runtime_instances.id = scope.runtime_instance_id
       AND runtime_instances.state IN ('running', 'waiting_hot')
    RETURNING runtime_instances.id
)
SELECT inserted_run_wait.*
  FROM inserted_run_wait
 WHERE EXISTS (SELECT 1 FROM waiting_runtime_instance);

-- name: GetRunWait :one
SELECT *
     FROM run_waits
 WHERE org_id = sqlc.arg(org_id)
   AND project_id = sqlc.arg(project_id)
   AND environment_id = sqlc.arg(environment_id)
   AND id = sqlc.arg(id);

-- name: GetRunWaitByRun :one
SELECT *
  FROM run_waits
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND id = sqlc.arg(id);

-- name: ListRunWaits :many
WITH cursor_wait AS (
    SELECT created_at, id
      FROM run_waits
     WHERE org_id = sqlc.arg(org_id)
       AND run_id = sqlc.arg(run_id)
       AND id = sqlc.narg(after_id)::uuid
)
SELECT *
  FROM run_waits
 WHERE run_waits.org_id = sqlc.arg(org_id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND (
       sqlc.narg(state)::text IS NULL
       OR run_waits.state = sqlc.narg(state)::run_wait_state
   )
   AND (
       sqlc.narg(after_id)::uuid IS NULL
       OR (run_waits.created_at, run_waits.id) > (SELECT cursor_wait.created_at, cursor_wait.id FROM cursor_wait)
   )
 ORDER BY run_waits.created_at ASC, run_waits.id ASC
 LIMIT sqlc.arg(limit_count);

-- name: ResolveRunWait :one
WITH target AS MATERIALIZED (
    SELECT run_waits.*
      FROM run_waits
      JOIN waits ON waits.org_id = run_waits.org_id
                AND waits.id = run_waits.wait_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.id = sqlc.arg(id)
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
       AND waits.state = 'pending'
     FOR UPDATE OF run_waits, waits
),
completed_wait AS (
    UPDATE waits
       SET state = 'completed',
           result = COALESCE(sqlc.narg(result)::jsonb, waits.result, 'null'::jsonb),
           completed_at = COALESCE(waits.completed_at, now()),
           updated_at = now()
      FROM target
     WHERE waits.org_id = target.org_id
       AND waits.id = target.wait_id
    RETURNING waits.id
),
updated_run_wait AS (
    UPDATE run_waits
       SET state = 'resuming',
           resuming_at = COALESCE(run_waits.resuming_at, now()),
           updated_at = now()
      FROM target
      JOIN completed_wait ON completed_wait.id = target.wait_id
     WHERE run_waits.org_id = target.org_id
       AND run_waits.id = target.id
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
    RETURNING run_waits.*
)
SELECT *
  FROM updated_run_wait;

-- name: ClaimRuntimeCheckpointWait :one
WITH scope AS (
    SELECT run_waits.*,
           runs.workspace_id,
           workspace_leases.id AS workspace_lease_id,
           workspace_leases.workspace_mount_id,
           workspace_mounts.runtime_instance_id,
           workspace_mounts.dirty_generation,
           workspaces.current_version_id AS current_workspace_version_id,
           worker_instances.runtime_id,
           worker_instances.runtime_arch,
           worker_instances.runtime_abi,
           worker_instances.kernel_digest,
           worker_instances.initramfs_digest,
           worker_instances.rootfs_digest,
           worker_instances.cni_profile,
           runtime_instances.runtime_key_hash
      FROM run_waits
      JOIN runs ON runs.org_id = run_waits.org_id
               AND runs.project_id = run_waits.project_id
               AND runs.environment_id = run_waits.environment_id
               AND runs.id = run_waits.run_id
      JOIN workspaces ON workspaces.org_id = runs.org_id
                     AND workspaces.project_id = runs.project_id
                     AND workspaces.environment_id = runs.environment_id
                     AND workspaces.id = runs.workspace_id
      JOIN run_leases ON run_leases.org_id = run_waits.org_id
                     AND run_leases.run_id = run_waits.run_id
                     AND run_leases.id = run_waits.owner_run_lease_id
      JOIN worker_instances ON worker_instances.id = run_leases.worker_instance_id
                           AND worker_instances.runtime_id <> ''
                           AND worker_instances.runtime_arch <> ''
                           AND worker_instances.runtime_abi <> ''
                           AND worker_instances.kernel_digest <> ''
                           AND worker_instances.initramfs_digest <> ''
                           AND worker_instances.rootfs_digest <> ''
                           AND worker_instances.cni_profile <> ''
      JOIN workspace_leases ON workspace_leases.org_id = runs.org_id
                           AND workspace_leases.project_id = runs.project_id
                           AND workspace_leases.environment_id = runs.environment_id
                           AND workspace_leases.workspace_id = runs.workspace_id
                           AND workspace_leases.owner_run_id = runs.id
                           AND workspace_leases.lease_kind = 'write'
                           AND workspace_leases.state = 'active'
                           AND workspace_leases.released_at IS NULL
                           AND workspace_leases.expires_at > now()
      JOIN workspace_mounts ON workspace_mounts.org_id = workspace_leases.org_id
                                     AND workspace_mounts.project_id = workspace_leases.project_id
                                     AND workspace_mounts.environment_id = workspace_leases.environment_id
                                     AND workspace_mounts.workspace_id = workspace_leases.workspace_id
                                     AND workspace_mounts.id = workspace_leases.workspace_mount_id
      JOIN runtime_instances
        ON runtime_instances.org_id = workspace_mounts.org_id
       AND runtime_instances.id = workspace_mounts.runtime_instance_id
       AND runtime_instances.worker_instance_id = run_waits.owner_worker_instance_id
       AND runtime_instances.id = run_waits.owner_runtime_instance_id
       AND runtime_instances.runtime_epoch = run_waits.owner_runtime_epoch
       AND runtime_instances.owner_run_id = run_waits.run_id
       AND runtime_instances.owner_run_lease_id = run_waits.owner_run_lease_id
       AND runtime_instances.owner_run_wait_id = run_waits.id
       AND runtime_instances.owner_run_state_version = run_waits.owner_run_state_version
       AND runtime_instances.workspace_mount_id = workspace_mounts.id
       AND runtime_instances.state IN ('waiting_hot', 'checkpointing')
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.project_id = sqlc.arg(project_id)
       AND run_waits.environment_id = sqlc.arg(environment_id)
       AND run_waits.run_id = sqlc.arg(run_id)
       AND run_waits.id = sqlc.arg(run_wait_id)
       AND run_waits.owner_run_lease_id = sqlc.arg(run_lease_id)
       AND run_waits.owner_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_waits.state IN ('hot_waiting', 'checkpointing')
       AND runs.status = 'running'
       AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.status IN ('leased', 'running')
       AND run_leases.lease_expires_at > now()
     FOR UPDATE OF run_waits, runs, workspace_leases, workspace_mounts, runtime_instances
),
claimed_checkpoint AS (
    INSERT INTO runtime_checkpoints (
        id,
        org_id,
        worker_group_id,
        project_id,
        environment_id,
        workspace_id,
        run_id,
        source_workspace_lease_id,
        workspace_mount_id,
        base_workspace_version_id,
        state,
        runtime_backend,
        runtime_id,
        runtime_arch,
        runtime_abi,
        kernel_digest,
        initramfs_digest,
        rootfs_digest,
        runtime_config_digest,
        owner_runtime_instance_id,
        owner_runtime_epoch,
        owner_run_id,
        owner_run_wait_id,
        owner_run_lease_id,
        owner_worker_instance_id,
        source_worker_instance_id,
        cni_profile,
        manifest,
        creation_expires_at
    )
    SELECT sqlc.arg(runtime_checkpoint_id),
           scope.org_id,
           scope.worker_group_id,
           scope.project_id,
           scope.environment_id,
           scope.workspace_id,
           scope.run_id,
           scope.workspace_lease_id,
           scope.workspace_mount_id,
           COALESCE(scope.workspace_version_id, scope.current_workspace_version_id),
           'creating',
           'firecracker',
           scope.runtime_id,
           scope.runtime_arch,
           scope.runtime_abi,
           scope.kernel_digest,
           scope.initramfs_digest,
           scope.rootfs_digest,
           scope.runtime_key_hash,
           scope.owner_runtime_instance_id,
           scope.owner_runtime_epoch,
           scope.run_id,
           scope.id,
           scope.owner_run_lease_id,
           scope.owner_worker_instance_id,
           scope.owner_worker_instance_id,
           scope.cni_profile,
           '{}'::jsonb,
           scope.runtime_checkpoint_due_at + interval '5 minutes'
      FROM scope
     WHERE scope.state = 'hot_waiting'
       AND COALESCE(scope.workspace_version_id, scope.current_workspace_version_id) IS NOT NULL
    ON CONFLICT (id) DO NOTHING
    RETURNING *
),
claimed_wait AS (
    UPDATE run_waits
       SET state = 'checkpointing',
           runtime_checkpoint_started_at = COALESCE(run_waits.runtime_checkpoint_started_at, now()),
           runtime_checkpoint_id = sqlc.arg(runtime_checkpoint_id),
           updated_at = now()
      FROM scope
     WHERE run_waits.org_id = scope.org_id
       AND run_waits.id = scope.id
       AND scope.state = 'hot_waiting'
       AND EXISTS (SELECT 1 FROM claimed_checkpoint)
    RETURNING run_waits.*
),
selected_wait AS (
    SELECT claimed_wait.*
      FROM claimed_wait
    UNION ALL
    SELECT run_waits.*
      FROM run_waits
      JOIN scope ON scope.org_id = run_waits.org_id
                AND scope.id = run_waits.id
     WHERE scope.state = 'checkpointing'
       AND run_waits.runtime_checkpoint_id IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM claimed_wait)
),
selected_checkpoint AS (
    SELECT claimed_checkpoint.id
      FROM claimed_checkpoint
    UNION ALL
    SELECT runtime_checkpoints.id
      FROM runtime_checkpoints
      JOIN scope ON scope.org_id = runtime_checkpoints.org_id
                AND scope.project_id = runtime_checkpoints.project_id
                AND scope.environment_id = runtime_checkpoints.environment_id
                AND scope.run_id = runtime_checkpoints.run_id
      JOIN selected_wait ON selected_wait.org_id = scope.org_id
                        AND selected_wait.id = scope.id
                        AND selected_wait.runtime_checkpoint_id = runtime_checkpoints.id
     WHERE scope.state = 'checkpointing'
       AND runtime_checkpoints.state = 'creating'
       AND runtime_checkpoints.owner_runtime_instance_id = scope.owner_runtime_instance_id
       AND runtime_checkpoints.owner_runtime_epoch = scope.owner_runtime_epoch
       AND runtime_checkpoints.owner_run_id = scope.run_id
       AND runtime_checkpoints.owner_run_wait_id = scope.id
       AND runtime_checkpoints.owner_run_lease_id = scope.owner_run_lease_id
       AND runtime_checkpoints.owner_worker_instance_id = scope.owner_worker_instance_id
       AND runtime_checkpoints.source_worker_instance_id = scope.owner_worker_instance_id
       AND NOT EXISTS (SELECT 1 FROM claimed_checkpoint)
),
checkpointing_runtime_instance AS (
    UPDATE runtime_instances
       SET state = 'checkpointing',
           owner_run_id = selected_wait.run_id,
           owner_run_lease_id = selected_wait.owner_run_lease_id,
           owner_run_wait_id = selected_wait.id,
           owner_run_state_version = selected_wait.owner_run_state_version,
           checkpointing_at = COALESCE(runtime_instances.checkpointing_at, now()),
           updated_at = now()
      FROM scope, selected_wait
     WHERE runtime_instances.org_id = scope.org_id
       AND runtime_instances.id = scope.runtime_instance_id
       AND runtime_instances.id = scope.owner_runtime_instance_id
       AND runtime_instances.runtime_epoch = scope.owner_runtime_epoch
       AND runtime_instances.owner_run_id = scope.run_id
       AND runtime_instances.owner_run_lease_id = scope.owner_run_lease_id
       AND runtime_instances.owner_run_wait_id = scope.id
       AND runtime_instances.owner_run_state_version = scope.owner_run_state_version
       AND runtime_instances.state IN ('waiting_hot', 'checkpointing')
    RETURNING runtime_instances.id
)
SELECT selected_checkpoint.id AS runtime_checkpoint_id,
       scope.org_id,
       scope.project_id,
       scope.environment_id,
       scope.run_id,
       scope.id AS run_wait_id,
       scope.owner_run_lease_id AS run_lease_id,
       scope.owner_worker_instance_id AS worker_instance_id,
       scope.owner_run_state_version AS run_state_version,
       scope.owner_runtime_instance_id AS runtime_instance_id,
       scope.owner_runtime_epoch AS runtime_epoch,
       scope.workspace_version_id,
       scope.dirty_generation
  FROM scope
  JOIN selected_wait ON selected_wait.org_id = scope.org_id
                    AND selected_wait.id = scope.id
  JOIN selected_checkpoint ON true
  JOIN checkpointing_runtime_instance ON true
 LIMIT 1;

-- name: MarkRuntimeResumeWaitResumed :one
WITH current_wait AS (
    SELECT run_waits.*
      FROM run_waits
      JOIN runs ON runs.org_id = run_waits.org_id
               AND runs.project_id = run_waits.project_id
               AND runs.environment_id = run_waits.environment_id
               AND runs.id = run_waits.run_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.id = sqlc.arg(id)
       AND run_waits.run_id = sqlc.arg(run_id)
       AND run_waits.runtime_checkpoint_id = sqlc.arg(runtime_checkpoint_id)
       AND run_waits.state = 'resuming'
       AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
       AND runs.status = 'running'
       AND runs.execution_status = 'executing'
     FOR UPDATE OF run_waits
),
restore_phase_payload AS (
    SELECT CASE
             WHEN jsonb_typeof(COALESCE(sqlc.arg(restore_phases)::jsonb, '[]'::jsonb)) = 'array'
             THEN COALESCE(sqlc.arg(restore_phases)::jsonb, '[]'::jsonb)
             ELSE '[]'::jsonb
           END AS phases
),
updated_restore AS (
    UPDATE runtime_checkpoint_restores
       SET status = 'restored',
           phases = restore_phase_payload.phases,
           error_message = NULL,
           acknowledged_at = COALESCE(runtime_checkpoint_restores.acknowledged_at, now()),
           finished_at = COALESCE(runtime_checkpoint_restores.finished_at, now()),
           updated_at = now()
      FROM current_wait
      JOIN restore_phase_payload ON true
     WHERE runtime_checkpoint_restores.org_id = sqlc.arg(org_id)
       AND runtime_checkpoint_restores.project_id = current_wait.project_id
       AND runtime_checkpoint_restores.environment_id = current_wait.environment_id
       AND runtime_checkpoint_restores.run_id = sqlc.arg(run_id)
       AND runtime_checkpoint_restores.runtime_checkpoint_id = sqlc.arg(runtime_checkpoint_id)
       AND runtime_checkpoint_restores.run_wait_id = current_wait.id
       AND runtime_checkpoint_restores.run_lease_id = sqlc.arg(run_lease_id)
       AND runtime_checkpoint_restores.status = 'restoring'
    RETURNING runtime_checkpoint_restores.id,
              runtime_checkpoint_restores.org_id,
              runtime_checkpoint_restores.project_id,
              runtime_checkpoint_restores.environment_id,
              runtime_checkpoint_restores.run_id
),
updated_wait AS (
    UPDATE run_waits
       SET released_at = COALESCE(run_waits.released_at, now()),
           state = 'released',
           updated_at = now()
      FROM current_wait
      JOIN updated_restore ON true
     WHERE run_waits.org_id = current_wait.org_id
       AND run_waits.id = current_wait.id
    RETURNING run_waits.*
)
SELECT updated_wait.*
  FROM updated_wait;

-- name: RequeueResolvedRunWaits :many
WITH eligible_waits AS (
    SELECT run_waits.*,
           runs.queued_expires_at,
           runs.workspace_id,
           runs.priority
      FROM run_waits
      JOIN runs ON runs.org_id = run_waits.org_id
               AND runs.project_id = run_waits.project_id
               AND runs.environment_id = run_waits.environment_id
               AND runs.id = run_waits.run_id
      JOIN runtime_checkpoints ON runtime_checkpoints.org_id = run_waits.org_id
                              AND runtime_checkpoints.project_id = run_waits.project_id
                              AND runtime_checkpoints.environment_id = run_waits.environment_id
                              AND runtime_checkpoints.run_id = run_waits.run_id
                              AND runtime_checkpoints.id = run_waits.runtime_checkpoint_id
                              AND runtime_checkpoints.id = runs.latest_runtime_checkpoint_id
      JOIN workspaces ON workspaces.org_id = runs.org_id
                     AND workspaces.project_id = runs.project_id
                     AND workspaces.environment_id = runs.environment_id
                     AND workspaces.id = runs.workspace_id
                     AND workspaces.current_version_id = runtime_checkpoints.base_workspace_version_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.worker_group_id = sqlc.arg(worker_group_id)
       AND run_waits.state = 'resuming'
       AND run_waits.runtime_checkpoint_id IS NOT NULL
       AND runs.status = 'waiting'
       AND runs.current_run_lease_id IS NULL
       AND runtime_checkpoints.state = 'ready'
       AND (runtime_checkpoints.expires_at IS NULL OR runtime_checkpoints.expires_at > now())
     ORDER BY COALESCE(run_waits.resuming_at, run_waits.updated_at), run_waits.id
     LIMIT sqlc.arg(limit_count)
     FOR UPDATE OF run_waits, runs
),
updated_waits AS (
    UPDATE run_waits
       SET state = 'resuming',
           resuming_at = COALESCE(run_waits.resuming_at, now()),
           updated_at = now()
      FROM eligible_waits
     WHERE run_waits.org_id = eligible_waits.org_id
       AND run_waits.id = eligible_waits.id
       AND run_waits.state = 'resuming'
    RETURNING run_waits.*
),
updated_runs AS (
    UPDATE runs
	       SET status = 'queued',
	           execution_status = 'queued',
	           dispatch_generation = runs.dispatch_generation + 1,
	           last_enqueue_error = '',
	           last_enqueued_at = NULL,
	           state_version = runs.state_version + 1,
	           updated_at = now()
      FROM eligible_waits
      JOIN updated_waits ON updated_waits.org_id = eligible_waits.org_id
                        AND updated_waits.id = eligible_waits.id
     WHERE runs.org_id = eligible_waits.org_id
       AND runs.id = eligible_waits.run_id
       AND runs.status = 'waiting'
       AND runs.current_run_lease_id IS NULL
	    RETURNING runs.*,
	              eligible_waits.id AS source_run_wait_id,
	              eligible_waits.runtime_checkpoint_id AS source_runtime_checkpoint_id
),
resumed_snapshots AS (
    INSERT INTO run_state_snapshots (org_id, worker_group_id, run_id, version, status, execution_status, attempt_number, runtime_checkpoint_id, previous_version, transition, reason)
    SELECT updated_runs.org_id,
           updated_runs.worker_group_id,
           updated_runs.id,
           updated_runs.state_version,
           updated_runs.status,
           updated_runs.execution_status,
           updated_runs.current_attempt_number,
           updated_runs.source_runtime_checkpoint_id,
           updated_runs.state_version - 1,
           'run.resumed',
           jsonb_build_object(
               'run_wait_id', updated_runs.source_run_wait_id,
               'runtime_checkpoint_id', updated_runs.source_runtime_checkpoint_id
           )
      FROM updated_runs
    RETURNING run_state_snapshots.run_id, run_state_snapshots.version
),
resumed_event_seq AS (
    INSERT INTO event_cursors (org_id, worker_group_id, subject_kind, subject_id, seq)
    SELECT updated_runs.org_id, updated_runs.worker_group_id, 'run', updated_runs.id, 1
      FROM updated_runs
      JOIN resumed_snapshots ON resumed_snapshots.run_id = updated_runs.id
    ON CONFLICT (org_id, worker_group_id, subject_kind, subject_id)
    DO UPDATE SET seq = event_cursors.seq + 1,
                  observed_at = now()
    RETURNING org_id, subject_kind, subject_id, seq
),
resumed_events AS (
    INSERT INTO event_hot_payloads (org_id, worker_group_id, project_id, environment_id, run_id, seq, attempt_number, trace_id, span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT updated_runs.org_id,
           updated_runs.worker_group_id,
           updated_runs.project_id,
           updated_runs.environment_id,
           updated_runs.id,
           resumed_event_seq.seq,
           updated_runs.current_attempt_number,
           updated_runs.trace_id,
           updated_runs.root_span_id,
           '00-' || updated_runs.trace_id || '-' || updated_runs.root_span_id || '-01',
           'lifecycle',
           'info',
           'control',
           'run.resumed',
           'run.resumed',
           jsonb_build_object(
               'run_wait_id', updated_runs.source_run_wait_id,
               'runtime_checkpoint_id', updated_runs.source_runtime_checkpoint_id
           ),
           'internal',
           updated_runs.state_version
      FROM updated_runs
      JOIN resumed_event_seq ON resumed_event_seq.org_id = updated_runs.org_id
                            AND resumed_event_seq.subject_kind = 'run'
                            AND resumed_event_seq.subject_id = updated_runs.id
    RETURNING *
),
resumed_telemetry_outboxes AS (
    INSERT INTO telemetry_outbox (org_id, worker_group_id, stream_kind, source_kind, source_id, seq, idempotency_key)
    SELECT resumed_events.org_id,
           resumed_events.worker_group_id,
           'event',
           resumed_events.subject_type,
           resumed_events.subject_id,
           resumed_events.seq,
           'event:' || resumed_events.subject_type::text || ':' || resumed_events.subject_id::text || ':' || resumed_events.seq::text
      FROM resumed_events
    RETURNING id
),
resumed_cleanup AS (
    SELECT count(*) AS telemetry_outbox_count
      FROM resumed_telemetry_outboxes
)
SELECT updated_waits.*,
       eligible_waits.workspace_id,
       eligible_waits.priority
  FROM updated_waits
	  JOIN eligible_waits ON eligible_waits.org_id = updated_waits.org_id
	                     AND eligible_waits.id = updated_waits.id
	  JOIN updated_runs ON updated_runs.org_id = updated_waits.org_id
	                   AND updated_runs.id = updated_waits.run_id
	  JOIN resumed_cleanup ON resumed_cleanup.telemetry_outbox_count >= 0;

-- name: FailStaleResolvedRunWaits :many
WITH stale_waits AS MATERIALIZED (
    SELECT run_waits.id AS run_wait_id,
           run_waits.org_id,
           run_waits.worker_group_id,
           run_waits.project_id,
           run_waits.environment_id,
           run_waits.run_id,
           runs.session_id,
	           runs.current_attempt_number,
           runs.trace_id,
           runs.root_span_id,
           runs.state_version + 1 AS next_state_version,
           runtime_checkpoints.id AS runtime_checkpoint_id,
           runtime_checkpoints.base_workspace_version_id,
           runtime_checkpoints.expires_at AS runtime_checkpoint_expires_at,
           workspaces.current_version_id,
           run_waits.state AS run_wait_state,
           runs.status AS run_status,
           CASE
             WHEN runs.latest_runtime_checkpoint_id IS DISTINCT FROM runtime_checkpoints.id
             THEN 'non_latest_runtime_checkpoint'
             WHEN runtime_checkpoints.expires_at <= now()
             THEN 'runtime_checkpoint_expired'
             ELSE 'workspace_version_mismatch'
           END AS failure_reason,
           CASE
             WHEN runs.latest_runtime_checkpoint_id IS DISTINCT FROM runtime_checkpoints.id
             THEN 'resolved wait is not attached to the latest runtime checkpoint'
             WHEN runtime_checkpoints.expires_at <= now()
             THEN 'runtime checkpoint expired while run was parked'
             ELSE 'workspace advanced while run was parked'
           END AS failure_message
      FROM run_waits
      JOIN runs ON runs.org_id = run_waits.org_id
               AND runs.project_id = run_waits.project_id
               AND runs.environment_id = run_waits.environment_id
               AND runs.id = run_waits.run_id
      JOIN sessions ON sessions.org_id = runs.org_id
                        AND sessions.project_id = runs.project_id
                        AND sessions.environment_id = runs.environment_id
                        AND sessions.id = runs.session_id
      JOIN runtime_checkpoints ON runtime_checkpoints.org_id = run_waits.org_id
                              AND runtime_checkpoints.project_id = run_waits.project_id
                              AND runtime_checkpoints.environment_id = run_waits.environment_id
                              AND runtime_checkpoints.run_id = run_waits.run_id
                              AND runtime_checkpoints.id = run_waits.runtime_checkpoint_id
      JOIN workspaces ON workspaces.org_id = runs.org_id
                     AND workspaces.project_id = runs.project_id
                     AND workspaces.environment_id = runs.environment_id
                     AND workspaces.id = runs.workspace_id
     WHERE run_waits.org_id = sqlc.arg(org_id)
       AND run_waits.worker_group_id = sqlc.arg(worker_group_id)
       AND (
           (run_waits.state = 'resuming' AND runs.status = 'waiting')
           OR (run_waits.state = 'resuming' AND runs.status = 'queued')
       )
       AND run_waits.runtime_checkpoint_id IS NOT NULL
       AND runs.current_run_lease_id IS NULL
       AND runtime_checkpoints.state = 'ready'
       AND (
           runs.latest_runtime_checkpoint_id IS DISTINCT FROM runtime_checkpoints.id
           OR workspaces.current_version_id IS DISTINCT FROM runtime_checkpoints.base_workspace_version_id
           OR runtime_checkpoints.expires_at <= now()
       )
     ORDER BY COALESCE(run_waits.resuming_at, run_waits.updated_at), run_waits.id
     LIMIT sqlc.arg(limit_count)
     FOR UPDATE OF run_waits, runs, sessions
),
failed_waits AS (
    UPDATE run_waits
       SET state = 'failed',
           updated_at = now()
      FROM stale_waits
     WHERE run_waits.org_id = stale_waits.org_id
       AND run_waits.id = stale_waits.run_wait_id
       AND run_waits.state = stale_waits.run_wait_state
    RETURNING run_waits.*
),
failed_runs AS (
    UPDATE runs
       SET status = 'failed',
           execution_status = 'finished',
           terminal_outcome = 'failed',
	           error_message = stale_waits.failure_message,
	           dispatch_generation = runs.dispatch_generation + 1,
	           state_version = stale_waits.next_state_version,
           finished_at = now(),
           updated_at = now()
      FROM stale_waits
      JOIN failed_waits ON failed_waits.org_id = stale_waits.org_id
                       AND failed_waits.id = stale_waits.run_wait_id
     WHERE runs.org_id = stale_waits.org_id
       AND runs.id = stale_waits.run_id
       AND runs.status = stale_waits.run_status
       AND runs.current_run_lease_id IS NULL
	    RETURNING runs.id, runs.org_id, runs.worker_group_id, runs.project_id, runs.environment_id, runs.session_id,
	              runs.current_attempt_number, runs.trace_id, runs.root_span_id,
              runs.state_version, runs.error_message, stale_waits.runtime_checkpoint_id,
              stale_waits.base_workspace_version_id, stale_waits.current_version_id,
              stale_waits.runtime_checkpoint_expires_at, stale_waits.failure_reason
),
invalidated_checkpoints AS (
    UPDATE runtime_checkpoints
       SET state = 'invalid',
           error_message = failed_runs.error_message,
           invalidated_at = now()
      FROM failed_runs
     WHERE runtime_checkpoints.org_id = failed_runs.org_id
       AND runtime_checkpoints.project_id = failed_runs.project_id
       AND runtime_checkpoints.environment_id = failed_runs.environment_id
       AND runtime_checkpoints.run_id = failed_runs.id
       AND runtime_checkpoints.id = failed_runs.runtime_checkpoint_id
       AND runtime_checkpoints.state = 'ready'
    RETURNING runtime_checkpoints.id
),
ended_session_runs AS (
    UPDATE session_runs
       SET ended_at = now()
      FROM failed_runs
     WHERE session_runs.org_id = failed_runs.org_id
       AND session_runs.project_id = failed_runs.project_id
       AND session_runs.environment_id = failed_runs.environment_id
       AND session_runs.session_id = failed_runs.session_id
       AND session_runs.run_id = failed_runs.id
    RETURNING session_runs.id
),
failed_sessions AS (
    SELECT failed_runs.session_id AS id
      FROM failed_runs
),
failed_snapshots AS (
    INSERT INTO run_state_snapshots (
        org_id,
        worker_group_id,
        run_id,
        version,
        status,
        execution_status,
        terminal_outcome,
        attempt_number,
        transition,
        runtime_checkpoint_id,
        reason,
        error
    )
    SELECT failed_runs.org_id,
           failed_runs.worker_group_id,
           failed_runs.id,
           failed_runs.state_version,
           'failed',
           'finished',
           'failed',
           failed_runs.current_attempt_number,
           'run.failed',
           failed_runs.runtime_checkpoint_id,
           jsonb_build_object(
               'origin', 'runtime_resume_wait',
               'reason', failed_runs.failure_reason,
               'message', failed_runs.error_message,
               'base_workspace_version_id', failed_runs.base_workspace_version_id,
               'current_workspace_version_id', failed_runs.current_version_id,
               'runtime_checkpoint_expires_at', failed_runs.runtime_checkpoint_expires_at
           ),
           jsonb_build_object(
               'origin', 'runtime_resume_wait',
               'reason', failed_runs.failure_reason,
               'message', failed_runs.error_message
           )
      FROM failed_runs
    RETURNING run_state_snapshots.run_id
),
failed_event_seq AS (
    INSERT INTO event_cursors (org_id, worker_group_id, subject_kind, subject_id, seq)
    SELECT failed_runs.org_id, failed_runs.worker_group_id, 'run', failed_runs.id, 1
      FROM failed_runs
      JOIN failed_snapshots ON failed_snapshots.run_id = failed_runs.id
    ON CONFLICT (org_id, worker_group_id, subject_kind, subject_id)
    DO UPDATE SET seq = event_cursors.seq + 1,
                  observed_at = now()
    RETURNING org_id, subject_kind, subject_id, seq
),
	failed_events AS (
	    INSERT INTO event_hot_payloads (org_id, worker_group_id, project_id, environment_id, run_id, seq, attempt_number, trace_id, span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT failed_runs.org_id,
           failed_runs.worker_group_id,
           failed_runs.project_id,
           failed_runs.environment_id,
           failed_runs.id,
           failed_event_seq.seq,
	           failed_runs.current_attempt_number,
           failed_runs.trace_id,
           failed_runs.root_span_id,
           '00-' || failed_runs.trace_id || '-' || failed_runs.root_span_id || '-01',
           'lifecycle',
           'error',
           'control',
           'run.failed',
           'run.failed',
           jsonb_build_object(
               'origin', 'runtime_resume_wait',
               'reason', failed_runs.failure_reason,
               'message', failed_runs.error_message,
               'runtime_checkpoint_id', failed_runs.runtime_checkpoint_id,
               'base_workspace_version_id', failed_runs.base_workspace_version_id,
               'current_workspace_version_id', failed_runs.current_version_id,
               'runtime_checkpoint_expires_at', failed_runs.runtime_checkpoint_expires_at
           ),
           'internal',
           failed_runs.state_version
      FROM failed_runs
      JOIN failed_snapshots ON failed_snapshots.run_id = failed_runs.id
      JOIN failed_event_seq ON failed_event_seq.org_id = failed_runs.org_id
                           AND failed_event_seq.subject_kind = 'run'
                           AND failed_event_seq.subject_id = failed_runs.id
    RETURNING *
),
failed_telemetry_outbox AS (
    INSERT INTO telemetry_outbox (org_id, worker_group_id, stream_kind, source_kind, source_id, seq, idempotency_key)
    SELECT failed_events.org_id,
                  failed_events.worker_group_id,
                  'event',
                  failed_events.subject_type,
                  failed_events.subject_id,
                  failed_events.seq,
                  'event:' || failed_events.subject_type::text || ':' || failed_events.subject_id::text || ':' || failed_events.seq::text
      FROM failed_events
    RETURNING id
),
cleanup AS (
    SELECT
        (SELECT count(*) FROM invalidated_checkpoints) AS invalidated_checkpoints,
        (SELECT count(*) FROM failed_telemetry_outbox) AS failed_telemetry_outboxes
)
SELECT failed_waits.*
  FROM failed_waits
  JOIN failed_runs ON failed_runs.org_id = failed_waits.org_id
                  AND failed_runs.id = failed_waits.run_id
 WHERE (SELECT invalidated_checkpoints + failed_telemetry_outboxes FROM cleanup) >= 0;

-- name: SetRunWaitWorkspaceVersion :one
UPDATE run_waits
   SET workspace_version_id = workspace_versions.id,
       updated_at = now()
  FROM runs
  JOIN workspace_versions
    ON workspace_versions.org_id = runs.org_id
   AND workspace_versions.project_id = runs.project_id
   AND workspace_versions.environment_id = runs.environment_id
   AND workspace_versions.workspace_id = runs.workspace_id
   AND workspace_versions.id = sqlc.arg(workspace_version_id)
   AND workspace_versions.state = 'ready'
 WHERE run_waits.org_id = sqlc.arg(org_id)
   AND run_waits.project_id = sqlc.arg(project_id)
   AND run_waits.environment_id = sqlc.arg(environment_id)
   AND run_waits.id = sqlc.arg(id)
   AND run_waits.run_id = sqlc.arg(run_id)
   AND run_waits.state IN ('hot_waiting', 'checkpointing')
   AND run_waits.workspace_version_id IS NULL
   AND runs.org_id = run_waits.org_id
   AND runs.project_id = run_waits.project_id
   AND runs.environment_id = run_waits.environment_id
   AND runs.id = run_waits.run_id
RETURNING run_waits.*;

-- name: CancelRunWait :one
UPDATE run_waits
   SET state = 'cancelled',
       cancelled_at = COALESCE(run_waits.cancelled_at, now()),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND id = sqlc.arg(id)
   AND state = 'checkpointed_waiting'
RETURNING *;

-- name: CancelRunWaitsForRun :many
UPDATE run_waits
   SET state = 'cancelled',
       cancelled_at = COALESCE(run_waits.cancelled_at, now()),
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)
   AND run_id = sqlc.arg(run_id)
   AND state = 'checkpointed_waiting'
RETURNING *;

-- name: ExpireDueRunWaits :many
WITH candidate_waits AS MATERIALIZED (
    SELECT waits.id,
           waits.org_id
      FROM waits
      JOIN run_waits ON run_waits.org_id = waits.org_id
                    AND run_waits.wait_id = waits.id
     WHERE waits.org_id = sqlc.arg(org_id)
       AND run_waits.worker_group_id = sqlc.arg(worker_group_id)
       AND waits.state = 'pending'
       AND waits.expires_at IS NOT NULL
       AND waits.expires_at <= now()
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
     FOR UPDATE OF waits, run_waits
),
expired_waits AS (
    UPDATE waits
       SET state = 'expired',
           completed_at = COALESCE(waits.completed_at, now()),
           updated_at = now()
      FROM candidate_waits
     WHERE waits.org_id = candidate_waits.org_id
       AND waits.id = candidate_waits.id
       AND waits.state = 'pending'
    RETURNING waits.*
),
expired_run_waits AS (
    UPDATE run_waits
       SET state = 'resuming',
           resuming_at = COALESCE(run_waits.resuming_at, now()),
           updated_at = now()
      FROM expired_waits
     WHERE run_waits.org_id = expired_waits.org_id
       AND run_waits.wait_id = expired_waits.id
       AND run_waits.worker_group_id = sqlc.arg(worker_group_id)
       AND run_waits.state IN ('hot_waiting', 'checkpointed_waiting')
    RETURNING run_waits.*
)
SELECT *
  FROM expired_run_waits;

-- name: GetWorkerRunWaitScope :one
SELECT runs.org_id,
       runs.worker_group_id,
       runs.project_id,
       runs.environment_id,
       runs.deployment_id,
       runs.task_id,
       runs.id AS run_id,
       runs.session_id,
       runs.workspace_id,
       runs.current_run_lease_id,
       run_leases.worker_instance_id,
       workspace_leases.id AS workspace_lease_id,
       workspace_leases.fencing_token AS workspace_fencing_token,
       workspace_leases.workspace_mount_id,
       workspace_leases.base_version_id AS workspace_base_version_id,
       workspaces.current_version_id AS workspace_current_version_id,
       workspace_mounts.dirty_generation,
       worker_instances.cni_profile AS worker_cni_profile
  FROM runs
  JOIN run_leases ON run_leases.org_id = runs.org_id
                 AND run_leases.worker_group_id = runs.worker_group_id
                 AND run_leases.run_id = runs.id
                 AND run_leases.id = runs.current_run_lease_id
  JOIN worker_groups
    ON worker_groups.id = runs.worker_group_id
   AND worker_groups.state IN ('active', 'draining')
  JOIN worker_instances ON worker_instances.id = run_leases.worker_instance_id
                       AND worker_instances.worker_group_id = run_leases.worker_group_id
                       AND worker_instances.worker_group_id = runs.worker_group_id
  JOIN workspaces ON workspaces.org_id = runs.org_id
                 AND workspaces.worker_group_id = runs.worker_group_id
                 AND workspaces.project_id = runs.project_id
                 AND workspaces.environment_id = runs.environment_id
                 AND workspaces.id = runs.workspace_id
  JOIN workspace_leases ON workspace_leases.org_id = runs.org_id
                       AND workspace_leases.worker_group_id = runs.worker_group_id
                       AND workspace_leases.project_id = runs.project_id
                       AND workspace_leases.environment_id = runs.environment_id
                       AND workspace_leases.workspace_id = runs.workspace_id
                       AND workspace_leases.owner_run_id = runs.id
                       AND workspace_leases.lease_kind = 'write'
                       AND workspace_leases.state = 'active'
                       AND workspace_leases.released_at IS NULL
                       AND workspace_leases.expires_at > now()
  JOIN workspace_mounts ON workspace_mounts.org_id = workspace_leases.org_id
                       AND workspace_mounts.worker_group_id = workspace_leases.worker_group_id
                       AND workspace_mounts.project_id = workspace_leases.project_id
                       AND workspace_mounts.environment_id = workspace_leases.environment_id
                       AND workspace_mounts.workspace_id = workspace_leases.workspace_id
                       AND workspace_mounts.id = workspace_leases.workspace_mount_id
 WHERE runs.org_id = sqlc.arg(org_id)
   AND runs.id = sqlc.arg(run_id)
   AND runs.current_run_lease_id = sqlc.arg(run_lease_id)
   AND runs.worker_group_id = worker_instances.worker_group_id
   AND runs.status = 'running'
   AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
   AND run_leases.status IN ('leased', 'running')
   AND run_leases.lease_expires_at > now();
