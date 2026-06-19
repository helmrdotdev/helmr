-- name: CreateRunSuspensionForWaitpoint :one
WITH current_run_lease AS (
    SELECT runs.id AS run_id,
           runs.project_id,
           runs.environment_id,
           runs.deployment_id,
           runs.task_id,
           runs.task_session_id,
           run_leases.dispatch_message_id,
           run_leases.dispatch_lease_id
      FROM runs
      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
                          AND run_leases.org_id = runs.org_id
                          AND run_leases.run_id = runs.id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.status = 'running'
       AND run_leases.lease_expires_at > now()
     FOR UPDATE OF runs, run_leases
),
existing_run_suspension AS (
    SELECT run_suspensions.*
      FROM run_suspensions
      JOIN current_run_lease ON current_run_lease.run_id = run_suspensions.run_id
     WHERE run_suspensions.org_id = sqlc.arg(org_id)
       AND run_suspensions.correlation_id = sqlc.arg(correlation_id)
       AND run_suspensions.status = 'opening'
),
checkpoint AS (
    INSERT INTO checkpoints (
        id,
        org_id,
        run_id,
        project_id,
        environment_id,
        run_lease_id,
        reason
    )
    SELECT
        sqlc.arg(checkpoint_id),
        sqlc.arg(org_id),
        current_run_lease.run_id,
        current_run_lease.project_id,
        current_run_lease.environment_id,
        sqlc.arg(run_lease_id),
        sqlc.arg(checkpoint_reason)
      FROM current_run_lease
     WHERE NOT EXISTS (SELECT 1 FROM existing_run_suspension)
    ON CONFLICT (id) DO UPDATE SET
        id = EXCLUDED.id
     WHERE checkpoints.status = 'creating'
    RETURNING *
),
created_waitpoint AS (
    INSERT INTO waitpoints (
        id,
        org_id,
        project_id,
        environment_id,
        run_id,
        kind,
        params,
        metadata,
        tags
    )
    SELECT
        sqlc.arg(id),
        sqlc.arg(org_id),
        current_run_lease.project_id,
        current_run_lease.environment_id,
        current_run_lease.run_id,
        sqlc.arg(kind),
        sqlc.arg(params),
        COALESCE(sqlc.arg(metadata)::jsonb, '{}'::jsonb),
        COALESCE(sqlc.arg(tags)::text[], '{}'::text[])
      FROM current_run_lease
     WHERE EXISTS (SELECT 1 FROM checkpoint)
        OR EXISTS (SELECT 1 FROM existing_run_suspension)
    ON CONFLICT (id) DO UPDATE SET
        kind = waitpoints.kind
	     WHERE waitpoints.status IN ('pending', 'completed')
	       AND waitpoints.org_id = sqlc.arg(org_id)
	       AND waitpoints.project_id = EXCLUDED.project_id
	       AND waitpoints.environment_id = EXCLUDED.environment_id
           AND waitpoints.run_id = EXCLUDED.run_id
	       AND waitpoints.kind = sqlc.arg(kind)
    RETURNING *
),
created_run_suspension AS (
    INSERT INTO run_suspensions (
        id,
        org_id,
        run_id,
        project_id,
        environment_id,
        run_lease_id,
        checkpoint_id,
        correlation_id
    )
    SELECT
        sqlc.arg(run_suspension_id),
        sqlc.arg(org_id),
        current_run_lease.run_id,
        current_run_lease.project_id,
        current_run_lease.environment_id,
        sqlc.arg(run_lease_id),
        checkpoint.id,
        sqlc.arg(correlation_id)
      FROM current_run_lease
      JOIN checkpoint ON checkpoint.run_id = current_run_lease.run_id
      JOIN created_waitpoint ON true
    ON CONFLICT (run_id, correlation_id) WHERE status IN ('opening', 'waiting') DO UPDATE SET
        checkpoint_id = run_suspensions.checkpoint_id
     WHERE run_suspensions.status = 'opening'
    RETURNING *
),
selected_run_suspension AS (
    SELECT * FROM created_run_suspension
    UNION ALL
    SELECT * FROM existing_run_suspension
     WHERE NOT EXISTS (SELECT 1 FROM created_run_suspension)
),
created_dependency AS (
    INSERT INTO run_suspension_waitpoints (
        org_id,
        run_id,
        project_id,
        environment_id,
        run_suspension_id,
        waitpoint_id,
        ordinal,
        timeout_seconds
    )
    SELECT
        sqlc.arg(org_id),
        selected_run_suspension.run_id,
        current_run_lease.project_id,
        current_run_lease.environment_id,
        selected_run_suspension.id,
        created_waitpoint.id,
        sqlc.arg(ordinal)::integer,
        sqlc.narg(timeout_seconds)
      FROM selected_run_suspension
      JOIN current_run_lease ON current_run_lease.run_id = selected_run_suspension.run_id
      JOIN created_waitpoint ON true
    ON CONFLICT (org_id, run_suspension_id, waitpoint_id) DO NOTHING
    RETURNING *
),
channel_definition AS (
    INSERT INTO channel_definitions (
        org_id,
        project_id,
        environment_id,
        deployment_id,
        task_id,
        name,
        direction
    )
    SELECT created_waitpoint.org_id,
           created_waitpoint.project_id,
           created_waitpoint.environment_id,
           current_run_lease.deployment_id,
           current_run_lease.task_id,
           created_waitpoint.params->>'channel',
           'input'
      FROM created_waitpoint
      JOIN created_dependency ON created_dependency.org_id = created_waitpoint.org_id
                             AND created_dependency.waitpoint_id = created_waitpoint.id
      JOIN current_run_lease ON current_run_lease.run_id = created_waitpoint.run_id
    WHERE created_waitpoint.kind = 'channel'
    ON CONFLICT (org_id, deployment_id, task_id, name, direction)
    DO UPDATE SET name = EXCLUDED.name
    RETURNING *
),
	created_channel AS (
	    INSERT INTO channels (
	        org_id,
	        project_id,
	        environment_id,
	        task_session_id,
	        definition_id,
	        name,
	        direction
	    )
	    SELECT created_waitpoint.org_id,
	           created_waitpoint.project_id,
	           created_waitpoint.environment_id,
	           current_run_lease.task_session_id,
	           channel_definition.id,
	           created_waitpoint.params->>'channel',
	           'input'
	      FROM created_waitpoint
	      JOIN created_dependency ON created_dependency.org_id = created_waitpoint.org_id
	                             AND created_dependency.waitpoint_id = created_waitpoint.id
	      JOIN current_run_lease ON current_run_lease.run_id = created_waitpoint.run_id
	      JOIN channel_definition ON channel_definition.deployment_id = current_run_lease.deployment_id
	                             AND channel_definition.task_id = current_run_lease.task_id
	                             AND channel_definition.name = created_waitpoint.params->>'channel'
	                             AND channel_definition.direction = 'input'
     WHERE created_waitpoint.kind = 'channel'
	    ON CONFLICT (org_id, task_session_id, name, direction)
	    DO UPDATE SET next_sequence = channels.next_sequence
	    RETURNING *
	),
	selected_channel AS (
	    SELECT * FROM created_channel
	),
	created_channel_cursor AS (
    INSERT INTO channel_wait_cursors (
        org_id,
        project_id,
        environment_id,
        task_session_id,
        channel_id,
        consumer_key,
        correlation_id
    )
    SELECT created_waitpoint.org_id,
	           created_waitpoint.project_id,
	           created_waitpoint.environment_id,
	           current_run_lease.task_session_id,
	           selected_channel.id,
	           'runtime:input-wait',
	           NULLIF(COALESCE(created_waitpoint.params->>'correlation_id', ''), '')
	      FROM created_waitpoint
	      JOIN selected_channel ON selected_channel.name = created_waitpoint.params->>'channel'
	                           AND selected_channel.direction = 'input'
	      JOIN current_run_lease ON current_run_lease.run_id = created_waitpoint.run_id
	     WHERE created_waitpoint.kind = 'channel'
    ON CONFLICT (environment_id, channel_id, consumer_key, COALESCE(correlation_id, ''))
    DO UPDATE SET updated_at = now()
    RETURNING *
),
created_channel_wait AS (
    INSERT INTO channel_waits (
        waitpoint_id,
        org_id,
        project_id,
        environment_id,
        run_id,
        run_suspension_id,
        channel_id,
        after_sequence,
        correlation_id
    )
    SELECT created_waitpoint.id,
           created_waitpoint.org_id,
           created_waitpoint.project_id,
	           created_waitpoint.environment_id,
	           created_waitpoint.run_id,
	           selected_run_suspension.id,
	           selected_channel.id,
	           GREATEST(
	               COALESCE(created_channel_cursor.last_delivered_sequence, 0),
	               COALESCE((created_waitpoint.params->>'after_sequence')::bigint, 0)
	           ),
	           COALESCE(created_waitpoint.params->>'correlation_id', '')
	      FROM created_waitpoint
	      JOIN selected_run_suspension ON selected_run_suspension.run_id = created_waitpoint.run_id
	      JOIN selected_channel ON selected_channel.name = created_waitpoint.params->>'channel'
	                           AND selected_channel.direction = 'input'
	      LEFT JOIN created_channel_cursor ON created_channel_cursor.channel_id = selected_channel.id
	                                      AND created_channel_cursor.consumer_key = 'runtime:input-wait'
      JOIN created_dependency ON created_dependency.org_id = created_waitpoint.org_id
                             AND created_dependency.run_suspension_id = selected_run_suspension.id
                             AND created_dependency.waitpoint_id = created_waitpoint.id
     WHERE created_waitpoint.kind = 'channel'
    ON CONFLICT (waitpoint_id) DO NOTHING
    RETURNING *
),
selected AS (
    SELECT created_waitpoint.id,
           selected_run_suspension.id AS run_suspension_id,
           created_waitpoint.org_id,
           created_waitpoint.project_id,
           created_waitpoint.environment_id,
           selected_run_suspension.run_id,
           selected_run_suspension.run_lease_id,
           selected_run_suspension.checkpoint_id,
           selected_run_suspension.correlation_id,
           created_waitpoint.kind,
           COALESCE(created_waitpoint.params, '{}'::jsonb) AS params,
           COALESCE(created_waitpoint.metadata, '{}'::jsonb) AS metadata,
           COALESCE(created_waitpoint.tags, '{}'::text[]) AS tags,
           created_dependency.timeout_seconds,
           selected_run_suspension.status,
           selected_run_suspension.resolution_kind,
           selected_run_suspension.resolution,
           created_waitpoint.created_at,
           selected_run_suspension.waiting_at AS waiting_at,
           selected_run_suspension.resolved_at
	      FROM selected_run_suspension
	      JOIN created_dependency ON created_dependency.org_id = selected_run_suspension.org_id
	                             AND created_dependency.run_suspension_id = selected_run_suspension.id
	      JOIN created_waitpoint ON created_waitpoint.org_id = created_dependency.org_id
	                            AND created_waitpoint.id = created_dependency.waitpoint_id
),
checkpoint_started_event_seq AS (
    INSERT INTO event_subject_cursors (org_id, subject_type, subject_id, last_seq)
    SELECT sqlc.arg(org_id), 'run', selected.run_id, 1
      FROM selected
      JOIN runs ON runs.org_id = selected.org_id
               AND runs.id = selected.run_id
     WHERE NOT EXISTS (SELECT 1 FROM existing_run_suspension)
    ON CONFLICT (org_id, subject_type, subject_id)
    DO UPDATE SET last_seq = event_subject_cursors.last_seq + 1,
                  updated_at = now()
    RETURNING org_id, subject_type, subject_id, last_seq
),
checkpoint_started_event AS (
    INSERT INTO events (org_id, project_id, environment_id, run_id, seq, attempt_id, run_lease_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT sqlc.arg(org_id),
           runs.project_id,
           runs.environment_id,
           selected.run_id,
           checkpoint_started_event_seq.last_seq,
           COALESCE(run_leases.attempt_id, runs.current_attempt_id),
           run_leases.id,
           COALESCE(run_attempts.attempt_number, runs.current_attempt_number),
           runs.trace_id,
           COALESCE(run_leases.span_id, runs.root_span_id),
           run_leases.parent_span_id,
           '00-' || runs.trace_id || '-' || COALESCE(run_leases.span_id, runs.root_span_id) || '-01',
           'checkpoint',
           'info',
           'control',
           'checkpoint.started',
           'checkpoint.started',
           jsonb_build_object(
               'run_id', selected.run_id,
               'waitpoint_id', selected.id,
               'checkpoint_id', selected.checkpoint_id,
               'kind', selected.kind
           ),
           'internal',
           runs.state_version
      FROM selected
      JOIN runs ON runs.org_id = selected.org_id
               AND runs.id = selected.run_id
      LEFT JOIN run_leases ON run_leases.org_id = selected.org_id
                              AND run_leases.run_id = selected.run_id
                              AND run_leases.id = selected.run_lease_id
      LEFT JOIN run_attempts ON run_attempts.org_id = run_leases.org_id
                            AND run_attempts.run_id = run_leases.run_id
                            AND run_attempts.id = run_leases.attempt_id
      JOIN checkpoint_started_event_seq ON checkpoint_started_event_seq.org_id = sqlc.arg(org_id)
                                       AND checkpoint_started_event_seq.subject_type = 'run'
                                       AND checkpoint_started_event_seq.subject_id = selected.run_id
     WHERE NOT EXISTS (SELECT 1 FROM existing_run_suspension)
    RETURNING *
),
checkpoint_started_event_outbox AS (
    INSERT INTO event_outbox (event_record_id, stream_key)
    SELECT checkpoint_started_event.id,
           'helmr:events:' || checkpoint_started_event.org_id::text || ':' || checkpoint_started_event.subject_type::text || ':' || checkpoint_started_event.subject_id::text
      FROM checkpoint_started_event
    RETURNING id
),
checkpoint_started AS (
    SELECT count(*) AS event_count FROM checkpoint_started_event_outbox
)
SELECT selected.*
	 FROM selected
	 JOIN checkpoint_started ON true
	LIMIT 1;

-- name: CancelOpeningRunSuspension :exec
WITH failed_run_suspension AS (
    UPDATE run_suspensions
       SET status = 'failed',
           failure = jsonb_build_object('reason', sqlc.arg(error_message), 'origin', 'waitpoint_create'),
           failed_at = now(),
           updated_at = now()
     WHERE run_suspensions.org_id = sqlc.arg(org_id)
       AND run_suspensions.id = sqlc.arg(run_suspension_id)
       AND run_suspensions.status = 'opening'
    RETURNING run_suspensions.*
),
cancelled_waitpoints AS (
    UPDATE waitpoints
       SET status = 'cancelled',
           data = NULL,
           error = jsonb_build_object('reason', sqlc.arg(error_message), 'origin', 'waitpoint_create'),
           resolved_at = now(),
           updated_at = now()
      FROM failed_run_suspension
      JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = failed_run_suspension.org_id
                                AND run_suspension_waitpoints.run_suspension_id = failed_run_suspension.id
     WHERE waitpoints.org_id = run_suspension_waitpoints.org_id
       AND waitpoints.id = run_suspension_waitpoints.waitpoint_id
       AND waitpoints.status = 'pending'
    RETURNING waitpoints.id
),
invalid_checkpoint AS (
    UPDATE checkpoints
       SET status = 'invalid',
           error_message = sqlc.arg(error_message),
           invalidated_at = now()
      FROM failed_run_suspension
     WHERE checkpoints.org_id = failed_run_suspension.org_id
       AND checkpoints.run_id = failed_run_suspension.run_id
       AND checkpoints.id = failed_run_suspension.checkpoint_id
       AND checkpoints.status = 'creating'
    RETURNING checkpoints.id
)
SELECT count(*) FROM failed_run_suspension
UNION ALL
SELECT count(*) FROM cancelled_waitpoints
UNION ALL
SELECT count(*) FROM invalid_checkpoint;

-- name: AcknowledgeRestore :one
WITH current_run_lease AS (
    SELECT runs.id AS run_id,
           run_leases.restore_checkpoint_id
      FROM runs
      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
                          AND run_leases.org_id = runs.org_id
                          AND run_leases.run_id = runs.id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.status = 'running'
       AND run_leases.restore_checkpoint_id = sqlc.arg(checkpoint_id)
       AND run_leases.lease_expires_at > now()
     FOR UPDATE OF runs, run_leases
),
checkpoint AS (
    SELECT checkpoints.id,
           checkpoints.status
      FROM checkpoints
      JOIN current_run_lease ON current_run_lease.run_id = checkpoints.run_id
                           AND current_run_lease.restore_checkpoint_id = checkpoints.id
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.status IN ('restoring', 'ready')
     FOR UPDATE OF checkpoints
),
acknowledged_checkpoint AS (
    UPDATE checkpoints
       SET status = 'ready',
           error_message = NULL,
           invalidated_at = NULL
      FROM checkpoint
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.id = checkpoint.id
       AND checkpoint.status = 'restoring'
    RETURNING checkpoints.id
),
checkpoint_ready AS (
    SELECT id FROM acknowledged_checkpoint
    UNION ALL
    SELECT id FROM checkpoint WHERE status = 'ready'
),
restored_run_suspension AS (
    UPDATE run_suspensions
       SET status = 'restored',
           restored_at = now(),
           updated_at = now()
      FROM current_run_lease
      JOIN checkpoint_ready ON checkpoint_ready.id = current_run_lease.restore_checkpoint_id
     WHERE run_suspensions.org_id = sqlc.arg(org_id)
       AND run_suspensions.run_id = current_run_lease.run_id
       AND run_suspensions.id = sqlc.arg(run_suspension_id)
       AND run_suspensions.checkpoint_id = current_run_lease.restore_checkpoint_id
       AND run_suspensions.status = 'resuming'
    RETURNING run_suspensions.*
),
current_run_suspension AS (
    SELECT run_suspensions.*
      FROM run_suspensions
      JOIN current_run_lease ON current_run_lease.run_id = run_suspensions.run_id
      JOIN checkpoint_ready ON checkpoint_ready.id = current_run_lease.restore_checkpoint_id
     WHERE run_suspensions.org_id = sqlc.arg(org_id)
       AND run_suspensions.id = sqlc.arg(run_suspension_id)
       AND run_suspensions.checkpoint_id = current_run_lease.restore_checkpoint_id
       AND run_suspensions.status = 'restored'
),
selected_run_suspension AS (
    SELECT * FROM restored_run_suspension
    UNION ALL
    SELECT * FROM current_run_suspension
    WHERE NOT EXISTS (SELECT 1 FROM restored_run_suspension)
),
	released_channel_wait_matches AS (
	    UPDATE channel_waits
	       SET matched_record_id = NULL,
           matched_at = NULL
      FROM selected_run_suspension
      JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = selected_run_suspension.org_id
                                AND run_suspension_waitpoints.run_suspension_id = selected_run_suspension.id
     WHERE channel_waits.org_id = run_suspension_waitpoints.org_id
       AND channel_waits.waitpoint_id = run_suspension_waitpoints.waitpoint_id
       AND channel_waits.run_suspension_id = selected_run_suspension.id
       AND channel_waits.matched_record_id IS NOT NULL
	       AND selected_run_suspension.resolution_kind NOT IN ('completed', 'waitpoints')
	    RETURNING channel_waits.waitpoint_id
	),
	channel_wait_cursor_commits AS (
	    SELECT DISTINCT ON (
	           matched_channel_waits.org_id,
	           matched_channel_waits.task_session_id,
	           matched_channel_waits.channel_id,
	           matched_channel_waits.consumer_key,
	           matched_channel_waits.correlation_id
	       )
	           matched_channel_waits.org_id,
	           matched_channel_waits.task_session_id,
	           matched_channel_waits.channel_id,
	           matched_channel_waits.consumer_key,
	           matched_channel_waits.correlation_id,
	           matched_channel_waits.record_id,
	           matched_channel_waits.sequence
	      FROM (
	          SELECT channel_waits.org_id,
	                 channels.task_session_id,
	                 channel_waits.channel_id,
	                 'runtime:input-wait'::text AS consumer_key,
	                 NULLIF(channel_waits.correlation_id, '') AS correlation_id,
	                 channel_records.id AS record_id,
	                 channel_records.sequence
	            FROM selected_run_suspension
	      JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = selected_run_suspension.org_id
	                                AND run_suspension_waitpoints.run_suspension_id = selected_run_suspension.id
	      JOIN waitpoints ON waitpoints.org_id = run_suspension_waitpoints.org_id
	                     AND waitpoints.id = run_suspension_waitpoints.waitpoint_id
	                     AND waitpoints.status = 'completed'
	                     AND NULLIF(waitpoints.error, '{}'::jsonb) IS NULL
      JOIN channel_waits ON channel_waits.org_id = run_suspension_waitpoints.org_id
                        AND channel_waits.waitpoint_id = run_suspension_waitpoints.waitpoint_id
                        AND channel_waits.run_suspension_id = selected_run_suspension.id
                        AND channel_waits.matched_record_id IS NOT NULL
      JOIN channels ON channels.org_id = channel_waits.org_id
                   AND channels.id = channel_waits.channel_id
	      JOIN channel_records ON channel_records.org_id = channel_waits.org_id
	                          AND channel_records.channel_id = channel_waits.channel_id
	                          AND channel_records.id = channel_waits.matched_record_id
	           WHERE selected_run_suspension.resolution_kind IN ('completed', 'waitpoints')
	      ) matched_channel_waits
	     ORDER BY matched_channel_waits.org_id,
	              matched_channel_waits.task_session_id,
	              matched_channel_waits.channel_id,
	              matched_channel_waits.consumer_key,
	              matched_channel_waits.correlation_id,
	              matched_channel_waits.sequence DESC,
	              matched_channel_waits.record_id DESC
	),
	committed_channel_wait_cursors AS (
	    UPDATE channel_wait_cursors
	       SET last_delivered_sequence = GREATEST(channel_wait_cursors.last_delivered_sequence, channel_wait_cursor_commits.sequence),
	           last_delivered_record_id = channel_wait_cursor_commits.record_id,
	           updated_at = now()
	      FROM channel_wait_cursor_commits
	     WHERE channel_wait_cursors.org_id = channel_wait_cursor_commits.org_id
	       AND channel_wait_cursors.task_session_id = channel_wait_cursor_commits.task_session_id
	       AND channel_wait_cursors.channel_id = channel_wait_cursor_commits.channel_id
	       AND channel_wait_cursors.consumer_key = channel_wait_cursor_commits.consumer_key
	       AND (
	           (channel_wait_cursors.correlation_id IS NULL AND channel_wait_cursor_commits.correlation_id IS NULL)
	           OR channel_wait_cursors.correlation_id = channel_wait_cursor_commits.correlation_id
	       )
	       AND channel_wait_cursors.last_delivered_sequence < channel_wait_cursor_commits.sequence
	    RETURNING channel_wait_cursors.id
	),
committed_channel_cursor_count AS (
    SELECT count(*) AS cursor_count FROM committed_channel_wait_cursors
),
released_channel_match_count AS (
    SELECT count(*) AS match_count FROM released_channel_wait_matches
)
SELECT waitpoints.id,
       selected_run_suspension.id AS run_suspension_id,
       waitpoints.org_id,
       selected_run_suspension.run_id,
       selected_run_suspension.run_lease_id,
       selected_run_suspension.checkpoint_id,
       selected_run_suspension.correlation_id,
       waitpoints.kind,
       COALESCE(waitpoints.params, '{}'::jsonb) AS params,
       COALESCE(waitpoints.metadata, '{}'::jsonb) AS metadata,
       COALESCE(waitpoints.tags, '{}'::text[]) AS tags,
       run_suspension_waitpoints.timeout_seconds,
       selected_run_suspension.status,
       selected_run_suspension.resolution_kind,
       selected_run_suspension.resolution,
       waitpoints.created_at,
       selected_run_suspension.waiting_at AS waiting_at,
       selected_run_suspension.resolved_at
  FROM selected_run_suspension
  JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = selected_run_suspension.org_id
                            AND run_suspension_waitpoints.run_suspension_id = selected_run_suspension.id
  JOIN waitpoints ON waitpoints.org_id = run_suspension_waitpoints.org_id
                 AND waitpoints.id = run_suspension_waitpoints.waitpoint_id
                 AND waitpoints.id = sqlc.arg(waitpoint_id)
  JOIN committed_channel_cursor_count ON true
  JOIN released_channel_match_count ON true
 LIMIT 1;

-- name: MarkRunSuspensionCheckpointReady :one
WITH current_run_lease AS (
    SELECT runs.id AS run_id,
           run_leases.dispatch_message_id,
           run_leases.dispatch_lease_id,
           run_leases.runtime_id
      FROM runs
      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
                          AND run_leases.org_id = runs.org_id
                          AND run_leases.run_id = runs.id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.status = 'running'
       AND run_leases.lease_expires_at > now()
     FOR UPDATE OF runs, run_leases
),
target_run_suspension AS (
    SELECT run_suspensions.*
      FROM run_suspensions
      JOIN current_run_lease ON current_run_lease.run_id = run_suspensions.run_id
     WHERE run_suspensions.org_id = sqlc.arg(org_id)
       AND run_suspensions.id = sqlc.arg(run_suspension_id)
       AND run_suspensions.checkpoint_id = sqlc.arg(checkpoint_id)
       AND run_suspensions.run_lease_id = sqlc.arg(run_lease_id)
       AND run_suspensions.status = 'opening'
     FOR UPDATE OF run_suspensions
),
target_waitpoint AS (
    SELECT waitpoints.*
      FROM waitpoints
      JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = waitpoints.org_id
                                AND run_suspension_waitpoints.waitpoint_id = waitpoints.id
      JOIN target_run_suspension ON target_run_suspension.org_id = run_suspension_waitpoints.org_id
                          AND target_run_suspension.id = run_suspension_waitpoints.run_suspension_id
	     WHERE waitpoints.id = sqlc.arg(waitpoint_id)
	       AND waitpoints.status IN ('pending', 'completed', 'timed_out')
	     FOR UPDATE OF waitpoints
),
locked_queue_entry AS (
    SELECT run_queue_items.run_id,
           run_queue_items.reserved_by_worker_instance_id,
           run_queue_items.dispatch_message_id
      FROM run_queue_items
      JOIN current_run_lease ON current_run_lease.run_id = run_queue_items.run_id
                            AND current_run_lease.dispatch_message_id = run_queue_items.dispatch_message_id
     WHERE run_queue_items.org_id = sqlc.arg(org_id)
       AND run_queue_items.run_id = sqlc.arg(run_id)
       AND run_queue_items.reserved_by_worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_queue_items.status = 'reserved'
     FOR UPDATE OF run_queue_items
),
expected_runtime AS (
    -- Checkpoint resume is currently Firecracker-only. Add a backend-specific
    -- identity branch here before accepting other runtime_backends.
    SELECT runtime_releases.runtime_id,
           runtime_releases.runtime_arch,
           runtime_releases.runtime_abi,
           runtime_releases.kernel_digest,
           runtime_releases.initramfs_digest,
           runtime_releases.rootfs_digest,
           runtime_releases.cni_profile
      FROM current_run_lease
      JOIN runtime_releases ON runtime_releases.runtime_id = current_run_lease.runtime_id
     WHERE sqlc.arg(runtime_backend)::text = 'firecracker'
       AND runtime_releases.runtime_id = sqlc.arg(runtime_id)
       AND runtime_releases.runtime_arch = sqlc.arg(runtime_arch)
       AND runtime_releases.runtime_abi = sqlc.arg(runtime_abi)
       AND runtime_releases.kernel_digest = sqlc.arg(kernel_digest)
       AND runtime_releases.initramfs_digest = sqlc.arg(initramfs_digest)
       AND runtime_releases.rootfs_digest = sqlc.arg(rootfs_digest)
       AND runtime_releases.cni_profile = sqlc.arg(cni_profile)
),
checkpoint_artifact_input AS (
    SELECT uuidv7() AS artifact_id,
           (artifact.value->>'role')::checkpoint_artifact_role AS role,
           (artifact.value->>'ordinal')::int AS ordinal,
           artifact.value->>'digest' AS digest,
           (artifact.value->>'size_bytes')::bigint AS size_bytes,
           artifact.value->>'media_type' AS media_type,
           CASE (artifact.value->>'role')::checkpoint_artifact_role
             WHEN 'runtime_config' THEN 'checkpoint_runtime_config'::artifact_kind
             WHEN 'runtime_vmstate' THEN 'checkpoint_vmstate'::artifact_kind
             WHEN 'runtime_memory' THEN 'checkpoint_memory'::artifact_kind
             WHEN 'runtime_scratch_disk' THEN 'checkpoint_scratch_disk'::artifact_kind
           END AS kind,
           COALESCE((artifact.value->>'encrypt_duration_ms')::bigint, 0) AS encrypt_duration_ms,
           COALESCE((artifact.value->>'store_duration_ms')::bigint, 0) AS store_duration_ms
      FROM jsonb_array_elements(sqlc.arg(checkpoint_artifacts)::jsonb) AS artifact(value)
),
workspace_artifact_input AS (
    SELECT uuidv7() AS artifact_id,
           sqlc.narg(workspace_artifact_digest)::text AS digest,
           sqlc.narg(workspace_artifact_size_bytes)::bigint AS size_bytes,
           sqlc.narg(workspace_artifact_media_type)::text AS media_type,
           'checkpoint_workspace'::artifact_kind AS kind
     WHERE sqlc.narg(workspace_artifact_digest)::text IS NOT NULL
),
artifact_input AS (
    SELECT artifact_id,
           role,
           ordinal,
           digest,
           size_bytes,
           media_type,
           kind,
           encrypt_duration_ms,
           store_duration_ms
      FROM checkpoint_artifact_input
    UNION ALL
    SELECT artifact_id,
           NULL::checkpoint_artifact_role AS role,
           NULL::int AS ordinal,
           digest,
           size_bytes,
           media_type,
           kind,
           0::bigint AS encrypt_duration_ms,
           0::bigint AS store_duration_ms
      FROM workspace_artifact_input
),
cas_object_input AS (
    SELECT DISTINCT
           digest,
           size_bytes,
           media_type
      FROM artifact_input
),
published_cas_objects AS (
    INSERT INTO cas_objects (digest, size_bytes, media_type)
    SELECT digest, size_bytes, media_type
      FROM cas_object_input
      JOIN target_run_suspension ON true
      JOIN locked_queue_entry ON locked_queue_entry.run_id = target_run_suspension.run_id
      JOIN expected_runtime ON true
    ON CONFLICT (digest) DO UPDATE
       SET size_bytes = cas_objects.size_bytes
     WHERE cas_objects.size_bytes = EXCLUDED.size_bytes
       AND cas_objects.media_type = EXCLUDED.media_type
    RETURNING digest
),
cas_objects_ready AS (
    SELECT count(*) = (SELECT count(*) FROM cas_object_input) AS ok
      FROM published_cas_objects
),
ready_checkpoint AS (
    UPDATE checkpoints
       SET status = 'ready',
           manifest = sqlc.arg(manifest),
           ready_at = now()
      FROM target_run_suspension
      JOIN cas_objects_ready ON cas_objects_ready.ok
      JOIN locked_queue_entry ON locked_queue_entry.run_id = target_run_suspension.run_id
      JOIN expected_runtime ON true
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = target_run_suspension.run_id
       AND checkpoints.id = target_run_suspension.checkpoint_id
       AND checkpoints.run_lease_id = sqlc.arg(run_lease_id)
       AND checkpoints.status = 'creating'
    RETURNING checkpoints.*
),
inserted_artifacts AS (
    INSERT INTO artifacts (
        id,
        org_id,
        project_id,
        environment_id,
        digest,
        kind,
        size_bytes,
        media_type,
        created_by_worker_instance_id
    )
    SELECT artifact_input.artifact_id,
           ready_checkpoint.org_id,
           ready_checkpoint.project_id,
           ready_checkpoint.environment_id,
           artifact_input.digest,
           artifact_input.kind,
           artifact_input.size_bytes,
           artifact_input.media_type,
           sqlc.arg(worker_instance_id)
      FROM ready_checkpoint
      JOIN artifact_input ON true
    RETURNING id
),
ready_runtime_snapshot AS (
    INSERT INTO checkpoint_runtime_snapshots (
        org_id,
        project_id,
        environment_id,
        run_id,
        checkpoint_id,
        runtime_backend,
        runtime_id,
        runtime_arch,
        runtime_abi,
        kernel_digest,
        initramfs_digest,
        rootfs_digest,
        runtime_vcpus,
        runtime_memory_mib,
        runtime_scratch_disk_mib,
        cni_profile,
        image_key,
        runtime_config_artifact_id
    )
    SELECT ready_checkpoint.org_id,
           ready_checkpoint.project_id,
           ready_checkpoint.environment_id,
           ready_checkpoint.run_id,
           ready_checkpoint.id,
           sqlc.arg(runtime_backend),
           expected_runtime.runtime_id,
           expected_runtime.runtime_arch,
           expected_runtime.runtime_abi,
           expected_runtime.kernel_digest,
           expected_runtime.initramfs_digest,
           expected_runtime.rootfs_digest,
           sqlc.narg(runtime_vcpus),
           sqlc.narg(runtime_memory_mib),
           sqlc.narg(runtime_scratch_disk_mib),
           expected_runtime.cni_profile,
           sqlc.narg(image_key),
           runtime_config_artifact.artifact_id
      FROM ready_checkpoint
      JOIN expected_runtime ON true
      LEFT JOIN artifact_input AS runtime_config_artifact
        ON runtime_config_artifact.role = 'runtime_config'
       AND runtime_config_artifact.ordinal = 0
      LEFT JOIN inserted_artifacts AS inserted_runtime_config_artifact
        ON inserted_runtime_config_artifact.id = runtime_config_artifact.artifact_id
    ON CONFLICT (org_id, run_id, checkpoint_id) DO UPDATE
       SET runtime_backend = EXCLUDED.runtime_backend,
           project_id = EXCLUDED.project_id,
           environment_id = EXCLUDED.environment_id,
           runtime_id = EXCLUDED.runtime_id,
           runtime_arch = EXCLUDED.runtime_arch,
           runtime_abi = EXCLUDED.runtime_abi,
           kernel_digest = EXCLUDED.kernel_digest,
           initramfs_digest = EXCLUDED.initramfs_digest,
           rootfs_digest = EXCLUDED.rootfs_digest,
           runtime_vcpus = EXCLUDED.runtime_vcpus,
           runtime_memory_mib = EXCLUDED.runtime_memory_mib,
           runtime_scratch_disk_mib = EXCLUDED.runtime_scratch_disk_mib,
           cni_profile = EXCLUDED.cni_profile,
           image_key = EXCLUDED.image_key,
           runtime_config_artifact_id = EXCLUDED.runtime_config_artifact_id
    RETURNING *
),
ready_workspace_snapshot AS (
    INSERT INTO checkpoint_workspace_snapshots (
        org_id,
        project_id,
        environment_id,
        run_id,
        checkpoint_id,
        workspace_artifact_id,
        workspace_artifact_encoding,
        workspace_mount_path,
        workspace_volume_kind
    )
    SELECT ready_checkpoint.org_id,
           ready_checkpoint.project_id,
           ready_checkpoint.environment_id,
           ready_checkpoint.run_id,
           ready_checkpoint.id,
           workspace_artifact_input.artifact_id,
           sqlc.narg(workspace_artifact_encoding),
           sqlc.narg(workspace_mount_path),
           sqlc.narg(workspace_volume_kind)
      FROM ready_checkpoint
      LEFT JOIN workspace_artifact_input ON true
      LEFT JOIN inserted_artifacts AS inserted_workspace_artifact
        ON inserted_workspace_artifact.id = workspace_artifact_input.artifact_id
    ON CONFLICT (org_id, run_id, checkpoint_id) DO UPDATE
       SET project_id = EXCLUDED.project_id,
           environment_id = EXCLUDED.environment_id,
           workspace_artifact_id = EXCLUDED.workspace_artifact_id,
           workspace_artifact_encoding = EXCLUDED.workspace_artifact_encoding,
           workspace_mount_path = EXCLUDED.workspace_mount_path,
           workspace_volume_kind = EXCLUDED.workspace_volume_kind
    RETURNING *
),
ready_requirements AS (
    UPDATE run_runtime_requirements
       SET requested_milli_cpu = COALESCE(ready_runtime_snapshot.runtime_vcpus::bigint * 1000, run_runtime_requirements.requested_milli_cpu),
           requested_memory_mib = COALESCE(ready_runtime_snapshot.runtime_memory_mib::bigint, run_runtime_requirements.requested_memory_mib),
           requested_disk_mib = COALESCE(ready_runtime_snapshot.runtime_scratch_disk_mib::bigint, run_runtime_requirements.requested_disk_mib),
           runtime_id = ready_runtime_snapshot.runtime_id,
           runtime_arch = ready_runtime_snapshot.runtime_arch,
           runtime_abi = ready_runtime_snapshot.runtime_abi,
           kernel_digest = ready_runtime_snapshot.kernel_digest,
           initramfs_digest = ready_runtime_snapshot.initramfs_digest,
           rootfs_digest = ready_runtime_snapshot.rootfs_digest,
           cni_profile = ready_runtime_snapshot.cni_profile,
           updated_at = now()
      FROM ready_checkpoint
      JOIN ready_runtime_snapshot ON ready_runtime_snapshot.org_id = ready_checkpoint.org_id
                                 AND ready_runtime_snapshot.run_id = ready_checkpoint.run_id
                                 AND ready_runtime_snapshot.checkpoint_id = ready_checkpoint.id
     WHERE run_runtime_requirements.org_id = ready_checkpoint.org_id
       AND run_runtime_requirements.run_id = ready_checkpoint.run_id
    RETURNING run_runtime_requirements.run_id
),
inserted_checkpoint_artifacts AS (
    INSERT INTO checkpoint_artifacts (
        org_id,
        project_id,
        environment_id,
        run_id,
        checkpoint_id,
        role,
        ordinal,
        artifact_id,
        encrypt_duration_ms,
        store_duration_ms
    )
    SELECT sqlc.arg(org_id),
           ready_checkpoint.project_id,
           ready_checkpoint.environment_id,
           ready_checkpoint.run_id,
           ready_checkpoint.id,
           checkpoint_artifact_input.role,
           checkpoint_artifact_input.ordinal,
           checkpoint_artifact_input.artifact_id,
           checkpoint_artifact_input.encrypt_duration_ms,
           checkpoint_artifact_input.store_duration_ms
      FROM ready_checkpoint
      JOIN checkpoint_artifact_input ON true
      JOIN inserted_artifacts ON inserted_artifacts.id = checkpoint_artifact_input.artifact_id
    ON CONFLICT (org_id, run_id, checkpoint_id, role, ordinal) DO UPDATE
       SET project_id = EXCLUDED.project_id,
           environment_id = EXCLUDED.environment_id,
           artifact_id = EXCLUDED.artifact_id,
           encrypt_duration_ms = EXCLUDED.encrypt_duration_ms,
           store_duration_ms = EXCLUDED.store_duration_ms
    RETURNING artifact_id
),
checkpoint_artifacts_ready AS (
    SELECT count(*) AS artifact_count FROM inserted_checkpoint_artifacts
),
parked_queue_entry AS (
    UPDATE run_queue_items
       SET status = 'parked',
           dispatch_generation = dispatch_generation + 1,
           updated_at = now(),
           finished_at = now()
      FROM ready_checkpoint
      JOIN locked_queue_entry ON locked_queue_entry.run_id = ready_checkpoint.run_id
     WHERE run_queue_items.org_id = sqlc.arg(org_id)
       AND run_queue_items.run_id = ready_checkpoint.run_id
       AND run_queue_items.reserved_by_worker_instance_id = locked_queue_entry.reserved_by_worker_instance_id
       AND run_queue_items.dispatch_message_id = locked_queue_entry.dispatch_message_id
       AND run_queue_items.status = 'reserved'
    RETURNING run_queue_items.run_id
),
waiting_run_suspension AS (
    UPDATE run_suspensions
       SET status = 'waiting',
           waiting_at = now(),
           active_duration_ms = sqlc.arg(active_duration_ms),
           updated_at = now()
      FROM ready_checkpoint
      JOIN target_run_suspension ON target_run_suspension.checkpoint_id = ready_checkpoint.id
     WHERE run_suspensions.org_id = sqlc.arg(org_id)
       AND run_suspensions.run_id = ready_checkpoint.run_id
       AND run_suspensions.id = target_run_suspension.id
       AND run_suspensions.checkpoint_id = ready_checkpoint.id
       AND run_suspensions.run_lease_id = sqlc.arg(run_lease_id)
       AND run_suspensions.status = 'opening'
    RETURNING run_suspensions.*
),
updated AS (
    UPDATE runs
       SET status = 'waiting',
           latest_checkpoint_id = waiting_run_suspension.checkpoint_id,
           current_run_lease_id = NULL,
           state_version = state_version + 1,
           usage_duration_ms = LEAST(
               GREATEST(
                   runs.usage_duration_ms,
                   sqlc.arg(active_duration_ms),
                   COALESCE((
                       SELECT SUM(run_usage_events.quantity)::bigint
                         FROM run_usage_events
                        WHERE run_usage_events.org_id = runs.org_id
                          AND run_usage_events.run_id = runs.id
                          AND run_usage_events.kind = 'active_time'
                   ), 0)
                   +
                   (EXTRACT(EPOCH FROM (now() - COALESCE(current_run_lease.started_at, current_run_lease.leased_at))) * 1000)::bigint
               ),
               runs.max_duration_seconds::bigint * 1000
           ),
           updated_at = now()
      FROM waiting_run_suspension
      JOIN run_leases current_run_lease
        ON current_run_lease.org_id = sqlc.arg(org_id)
       AND current_run_lease.run_id = waiting_run_suspension.run_id
       AND current_run_lease.id = sqlc.arg(run_lease_id)
       AND current_run_lease.worker_instance_id = sqlc.arg(worker_instance_id)
       AND current_run_lease.status = 'running'
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = waiting_run_suspension.run_id
       AND runs.current_run_lease_id = current_run_lease.id
    RETURNING runs.id, runs.org_id, runs.project_id, runs.environment_id, runs.trace_id, runs.current_attempt_id, runs.max_duration_seconds, runs.state_version, runs.usage_duration_ms
),
detached_run_lease AS (
    UPDATE run_leases
       SET status = 'detached',
           -- Store cumulative active time so a restored run can resume from prior usage.
           active_duration_ms = updated.usage_duration_ms,
           released_at = now(),
           renewed_at = now()
      FROM waiting_run_suspension
      JOIN updated ON updated.id = waiting_run_suspension.run_id
     WHERE run_leases.org_id = sqlc.arg(org_id)
       AND run_leases.run_id = waiting_run_suspension.run_id
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.status = 'running'
    RETURNING run_leases.id, run_leases.attempt_id, run_leases.trace_id, run_leases.span_id, run_leases.parent_span_id, run_leases.traceparent, run_leases.active_duration_ms, run_leases.restore_checkpoint_id
),
released_workspace_lease AS (
    UPDATE workspace_leases
       SET released_at = now(),
           renewed_at = now()
      FROM waiting_run_suspension
      JOIN detached_run_lease ON true
     WHERE workspace_leases.org_id = sqlc.arg(org_id)
       AND workspace_leases.run_id = waiting_run_suspension.run_id
       AND workspace_leases.mode = 'write'
       AND workspace_leases.released_at IS NULL
    RETURNING workspace_leases.id
),
waiting_attempt AS (
    UPDATE run_attempts
       SET status = 'waiting',
           updated_at = now()
      FROM updated
     WHERE run_attempts.org_id = updated.org_id
       AND run_attempts.run_id = updated.id
       AND run_attempts.id = updated.current_attempt_id
    RETURNING run_attempts.id, run_attempts.attempt_number
),
active_time_delta AS (
    SELECT GREATEST(
               detached_run_lease.active_duration_ms
               - COALESCE((
                   SELECT SUM(run_usage_events.quantity)::bigint
                     FROM run_usage_events
                    WHERE run_usage_events.org_id = updated.org_id
                      AND run_usage_events.run_id = updated.id
                      AND run_usage_events.kind = 'active_time'
               ), 0),
               0
           )::bigint AS quantity
      FROM updated
      JOIN detached_run_lease ON true
),
waiting_checkpoint AS (
    SELECT DISTINCT waiting_run_suspension.run_id, waiting_run_suspension.checkpoint_id
      FROM waiting_run_suspension
),
active_time_usage_event AS (
    INSERT INTO run_usage_events (org_id, project_id, environment_id, run_id, attempt_id, run_lease_id, checkpoint_id, trace_id, span_id, snapshot_version, kind, quantity, unit, measured_to, attributes, idempotency_key)
    SELECT updated.org_id,
           updated.project_id,
           updated.environment_id,
           updated.id,
           detached_run_lease.attempt_id,
           detached_run_lease.id,
           waiting_checkpoint.checkpoint_id,
           detached_run_lease.trace_id,
           detached_run_lease.span_id,
           updated.state_version,
           'active_time',
           active_time_delta.quantity,
           'ms',
           now(),
           jsonb_build_object('phase', 'checkpoint_ready'),
           'active_time:' || detached_run_lease.id::text || ':checkpoint_ready'
      FROM updated
      JOIN detached_run_lease ON true
      JOIN waiting_checkpoint ON waiting_checkpoint.run_id = updated.id
      JOIN active_time_delta ON true
     WHERE active_time_delta.quantity > 0
    ON CONFLICT DO NOTHING
    RETURNING id
),
checkpoint_artifact_totals AS (
    SELECT COALESCE(SUM(artifact_input.size_bytes), 0)::bigint AS size_bytes,
           COUNT(artifact_input.artifact_id)::bigint AS artifact_count
      FROM inserted_checkpoint_artifacts
      JOIN artifact_input ON artifact_input.artifact_id = inserted_checkpoint_artifacts.artifact_id
),
checkpoint_bytes_usage_event AS (
    INSERT INTO run_usage_events (org_id, project_id, environment_id, run_id, attempt_id, run_lease_id, checkpoint_id, trace_id, span_id, snapshot_version, kind, quantity, unit, measured_to, attributes, idempotency_key)
    SELECT updated.org_id,
           updated.project_id,
           updated.environment_id,
           updated.id,
           detached_run_lease.attempt_id,
           detached_run_lease.id,
           waiting_checkpoint.checkpoint_id,
           detached_run_lease.trace_id,
           detached_run_lease.span_id,
           updated.state_version,
           'checkpoint_bytes',
           checkpoint_artifact_totals.size_bytes,
           'bytes',
           now(),
           jsonb_build_object('artifact_count', checkpoint_artifact_totals.artifact_count),
           'checkpoint_bytes:' || waiting_checkpoint.checkpoint_id::text
      FROM updated
      JOIN detached_run_lease ON true
      JOIN waiting_checkpoint ON waiting_checkpoint.run_id = updated.id
      JOIN checkpoint_artifact_totals ON true
     WHERE checkpoint_artifact_totals.size_bytes > 0
    ON CONFLICT DO NOTHING
    RETURNING id
),
released_concurrency_slot AS (
    UPDATE run_queue_concurrency_leases
       SET released_at = now()
      FROM waiting_run_suspension
     WHERE run_queue_concurrency_leases.org_id = sqlc.arg(org_id)
       AND run_queue_concurrency_leases.run_id = waiting_run_suspension.run_id
       AND run_queue_concurrency_leases.run_lease_id = sqlc.arg(run_lease_id)
       AND run_queue_concurrency_leases.released_at IS NULL
    RETURNING run_queue_concurrency_leases.id
),
completed_restore_checkpoint AS (
    UPDATE checkpoints
       SET status = 'ready',
           error_message = NULL,
           invalidated_at = NULL
      FROM waiting_run_suspension
      JOIN detached_run_lease ON true
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = waiting_run_suspension.run_id
       AND checkpoints.id = detached_run_lease.restore_checkpoint_id
       AND checkpoints.status = 'restoring'
    RETURNING checkpoints.id
),
restored_previous_run_suspension AS (
    UPDATE run_suspensions
       SET status = 'restored',
           restored_at = now(),
           updated_at = now()
      FROM completed_restore_checkpoint
      JOIN detached_run_lease ON detached_run_lease.restore_checkpoint_id = completed_restore_checkpoint.id
     WHERE run_suspensions.org_id = sqlc.arg(org_id)
       AND run_suspensions.run_id = sqlc.arg(run_id)
       AND run_suspensions.checkpoint_id = completed_restore_checkpoint.id
       AND run_suspensions.status = 'resuming'
    RETURNING run_suspensions.id
),
resolved_restore AS (
    SELECT
        (SELECT count(*) FROM restored_previous_run_suspension) AS waitpoint_count,
        (SELECT count(*) FROM released_concurrency_slot) AS concurrency_slot_count,
        (SELECT count(*) FROM active_time_usage_event) AS active_time_usage_events,
        (SELECT count(*) FROM checkpoint_bytes_usage_event) AS checkpoint_bytes_usage_events
),
waiting_snapshot AS (
    INSERT INTO run_snapshots (org_id, run_id, version, status, attempt_id, run_lease_id, transition, reason)
    SELECT updated.org_id,
           updated.id,
           updated.state_version,
           'waiting',
           updated.current_attempt_id,
           detached_run_lease.id,
           'checkpoint.ready',
           sqlc.arg(checkpoint_payload)
      FROM updated
      JOIN detached_run_lease ON true
      JOIN waiting_attempt ON true
    RETURNING run_snapshots.run_id
),
event_inputs AS (
    SELECT 1 AS event_ordinal,
           sqlc.arg(org_id) AS org_id,
           updated.project_id,
           updated.environment_id,
           waiting_run_suspension.run_id,
           detached_run_lease.attempt_id,
           detached_run_lease.id AS run_lease_id,
           waiting_attempt.attempt_number,
           detached_run_lease.trace_id,
           detached_run_lease.span_id,
           detached_run_lease.parent_span_id,
           detached_run_lease.traceparent,
           'checkpoint' AS category,
           'info' AS severity,
           'control' AS source,
           'checkpoint.ready' AS kind,
           'checkpoint.ready' AS message,
           sqlc.arg(checkpoint_payload)::jsonb AS payload,
           'internal' AS redaction_class,
           updated.state_version AS snapshot_version
      FROM waiting_run_suspension
      JOIN updated ON updated.id = waiting_run_suspension.run_id
      JOIN detached_run_lease ON true
      JOIN waiting_attempt ON true
      JOIN waiting_snapshot ON true
    UNION ALL
    SELECT 2 AS event_ordinal,
           sqlc.arg(org_id) AS org_id,
           updated.project_id,
           updated.environment_id,
           waiting_run_suspension.run_id,
           detached_run_lease.attempt_id,
           detached_run_lease.id,
           waiting_attempt.attempt_number,
           detached_run_lease.trace_id,
           detached_run_lease.span_id,
           detached_run_lease.parent_span_id,
           detached_run_lease.traceparent,
           'waitpoint',
           'info',
           'control',
           'waitpoint.created',
           'waitpoint.created',
           jsonb_build_object(
               'run_id', waiting_run_suspension.run_id,
               'waitpoint_id', waitpoints.id,
               'checkpoint_id', waiting_run_suspension.checkpoint_id,
               'kind', waitpoints.kind,
               'params', waitpoints.params,
               'metadata', waitpoints.metadata,
               'tags', waitpoints.tags,
               'timeout', run_suspension_waitpoints.timeout_seconds
           ),
           'internal',
           updated.state_version
      FROM waiting_run_suspension
      JOIN updated ON updated.id = waiting_run_suspension.run_id
      JOIN detached_run_lease ON true
      JOIN waiting_attempt ON true
      JOIN waiting_snapshot ON true
      JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = waiting_run_suspension.org_id
                                AND run_suspension_waitpoints.run_suspension_id = waiting_run_suspension.id
      JOIN waitpoints ON waitpoints.org_id = run_suspension_waitpoints.org_id
                     AND waitpoints.id = run_suspension_waitpoints.waitpoint_id
),
event_subject_counts AS (
    SELECT org_id, run_id, count(*)::bigint AS event_count
      FROM event_inputs
     GROUP BY org_id, run_id
),
event_seq AS (
    INSERT INTO event_subject_cursors (org_id, subject_type, subject_id, last_seq)
    SELECT org_id, 'run', run_id, event_count
      FROM event_subject_counts
    ON CONFLICT (org_id, subject_type, subject_id)
    DO UPDATE SET last_seq = event_subject_cursors.last_seq + EXCLUDED.last_seq,
                  updated_at = now()
    RETURNING org_id, subject_type, subject_id, last_seq
),
events AS (
    INSERT INTO events (org_id, project_id, environment_id, run_id, seq, attempt_id, run_lease_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT event_inputs.org_id,
           event_inputs.project_id,
           event_inputs.environment_id,
           event_inputs.run_id,
           event_seq.last_seq - event_subject_counts.event_count + row_number() OVER (PARTITION BY event_inputs.org_id, event_inputs.run_id ORDER BY event_inputs.event_ordinal),
           event_inputs.attempt_id,
           event_inputs.run_lease_id,
           event_inputs.attempt_number,
           event_inputs.trace_id,
           event_inputs.span_id,
           event_inputs.parent_span_id,
           event_inputs.traceparent,
           event_inputs.category,
           event_inputs.severity,
           event_inputs.source,
           event_inputs.kind,
           event_inputs.message,
           event_inputs.payload,
           event_inputs.redaction_class,
           event_inputs.snapshot_version
      FROM event_inputs
      JOIN event_subject_counts ON event_subject_counts.org_id = event_inputs.org_id
                               AND event_subject_counts.run_id = event_inputs.run_id
      JOIN event_seq ON event_seq.org_id = event_inputs.org_id
                    AND event_seq.subject_type = 'run'
                    AND event_seq.subject_id = event_inputs.run_id
    RETURNING *
),
event_outbox AS (
    INSERT INTO event_outbox (event_record_id, stream_key)
    SELECT events.id,
           'helmr:events:' || events.org_id::text || ':' || events.subject_type::text || ':' || events.subject_id::text
      FROM events
    RETURNING id
)
SELECT waitpoints.id,
       waiting_run_suspension.id AS run_suspension_id,
       waitpoints.org_id,
       waiting_run_suspension.run_id,
       waiting_run_suspension.run_lease_id,
       waiting_run_suspension.checkpoint_id,
       waiting_run_suspension.correlation_id,
       waitpoints.kind,
       COALESCE(waitpoints.params, '{}'::jsonb) AS params,
       COALESCE(waitpoints.metadata, '{}'::jsonb) AS metadata,
       COALESCE(waitpoints.tags, '{}'::text[]) AS tags,
       run_suspension_waitpoints.timeout_seconds,
       waiting_run_suspension.status,
       waiting_run_suspension.resolution_kind,
       waiting_run_suspension.resolution,
       waitpoints.created_at,
       waiting_run_suspension.waiting_at AS waiting_at,
       waiting_run_suspension.resolved_at
  FROM waiting_run_suspension
  JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = waiting_run_suspension.org_id
                            AND run_suspension_waitpoints.run_suspension_id = waiting_run_suspension.id
  JOIN waitpoints ON waitpoints.org_id = run_suspension_waitpoints.org_id
                 AND waitpoints.id = run_suspension_waitpoints.waitpoint_id
  JOIN updated ON true
  JOIN detached_run_lease ON true
  LEFT JOIN released_workspace_lease ON true
  LEFT JOIN completed_restore_checkpoint ON true
  JOIN resolved_restore ON true
  JOIN ready_runtime_snapshot ON true
  JOIN ready_workspace_snapshot ON true
  JOIN parked_queue_entry ON true
  JOIN ready_requirements ON true
  JOIN checkpoint_artifacts_ready ON true
 WHERE (SELECT count(*) FROM events WHERE kind = 'checkpoint.ready') > 0
   AND (SELECT count(*) FROM event_outbox) >= 0;

-- name: MarkRunSuspensionCheckpointFailed :one
WITH current_run_lease AS (
    SELECT runs.id AS run_id
      FROM runs
      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
                          AND run_leases.org_id = runs.org_id
                          AND run_leases.run_id = runs.id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.status = 'running'
       AND run_leases.lease_expires_at > now()
),
target_run_suspension AS (
    SELECT run_suspensions.*
      FROM run_suspensions
      JOIN current_run_lease ON current_run_lease.run_id = run_suspensions.run_id
     WHERE run_suspensions.org_id = sqlc.arg(org_id)
       AND run_suspensions.id = sqlc.arg(run_suspension_id)
       AND run_suspensions.checkpoint_id = sqlc.arg(checkpoint_id)
       AND run_suspensions.run_lease_id = sqlc.arg(run_lease_id)
       AND run_suspensions.status = 'opening'
     FOR UPDATE OF run_suspensions
),
failed_checkpoint AS (
    UPDATE checkpoints
       SET status = 'invalid',
           error_message = sqlc.arg(error_message),
           invalidated_at = now()
      FROM target_run_suspension
     WHERE checkpoints.org_id = sqlc.arg(org_id)
       AND checkpoints.run_id = target_run_suspension.run_id
       AND checkpoints.id = target_run_suspension.checkpoint_id
       AND checkpoints.run_lease_id = sqlc.arg(run_lease_id)
       AND checkpoints.status = 'creating'
    RETURNING checkpoints.*
),
failed_run_suspension AS (
    UPDATE run_suspensions
       SET status = 'failed',
           failure = jsonb_build_object('reason', sqlc.arg(error_message), 'origin', 'checkpoint'),
           resolution_kind = 'cancelled',
           resolution = jsonb_build_object('reason', sqlc.arg(error_message), 'origin', 'checkpoint'),
           failed_at = now(),
           updated_at = now()
      FROM failed_checkpoint
     WHERE run_suspensions.org_id = sqlc.arg(org_id)
       AND run_suspensions.run_id = failed_checkpoint.run_id
       AND run_suspensions.id = sqlc.arg(run_suspension_id)
       AND run_suspensions.checkpoint_id = failed_checkpoint.id
       AND run_suspensions.run_lease_id = sqlc.arg(run_lease_id)
       AND run_suspensions.status = 'opening'
    RETURNING run_suspensions.*
),
cancelled_waitpoint AS (
    UPDATE waitpoints
       SET status = 'cancelled',
           data = NULL,
           error = jsonb_build_object('reason', sqlc.arg(error_message), 'origin', 'checkpoint'),
           resolved_at = now(),
           updated_at = now()
      FROM failed_run_suspension
      JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = failed_run_suspension.org_id
                                AND run_suspension_waitpoints.run_suspension_id = failed_run_suspension.id
     WHERE waitpoints.org_id = run_suspension_waitpoints.org_id
       AND waitpoints.id = run_suspension_waitpoints.waitpoint_id
       AND waitpoints.status = 'pending'
    RETURNING waitpoints.*
),
resolved_cancelled_waitpoint AS (
    UPDATE waitpoints
       SET resolved_at = now(),
           updated_at = now()
      FROM cancelled_waitpoint
     WHERE waitpoints.org_id = cancelled_waitpoint.org_id
       AND waitpoints.id = cancelled_waitpoint.id
       AND waitpoints.resolved_at IS NULL
    RETURNING waitpoints.*
),
selected_waitpoint AS (
    SELECT waitpoints.*,
           run_suspension_waitpoints.timeout_seconds
      FROM failed_run_suspension
      JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = failed_run_suspension.org_id
                                AND run_suspension_waitpoints.run_suspension_id = failed_run_suspension.id
      JOIN waitpoints ON waitpoints.org_id = run_suspension_waitpoints.org_id
                     AND waitpoints.id = run_suspension_waitpoints.waitpoint_id
                     AND waitpoints.id = sqlc.arg(waitpoint_id)
)
SELECT selected_waitpoint.id,
       failed_run_suspension.id AS run_suspension_id,
       selected_waitpoint.org_id,
       failed_run_suspension.run_id,
       failed_run_suspension.run_lease_id,
       failed_run_suspension.checkpoint_id,
       failed_run_suspension.correlation_id,
       selected_waitpoint.kind,
       COALESCE(waitpoints.params, '{}'::jsonb) AS params,
       COALESCE(waitpoints.metadata, '{}'::jsonb) AS metadata,
       COALESCE(waitpoints.tags, '{}'::text[]) AS tags,
       selected_waitpoint.timeout_seconds,
       failed_run_suspension.status,
       failed_run_suspension.resolution_kind,
       failed_run_suspension.resolution,
       selected_waitpoint.created_at,
       failed_run_suspension.waiting_at AS waiting_at,
       failed_run_suspension.resolved_at
  FROM failed_run_suspension
  JOIN selected_waitpoint ON true
  LEFT JOIN waitpoints ON waitpoints.org_id = selected_waitpoint.org_id
                              AND waitpoints.id = selected_waitpoint.id
  LEFT JOIN resolved_cancelled_waitpoint ON resolved_cancelled_waitpoint.org_id = selected_waitpoint.org_id
                                   AND resolved_cancelled_waitpoint.id = selected_waitpoint.id;

-- name: GetPendingWaitpointForRun :one
SELECT waitpoints.id,
       run_suspensions.id AS run_suspension_id,
       waitpoints.org_id,
       run_suspensions.run_id,
       run_suspensions.run_lease_id,
       run_suspensions.checkpoint_id,
       run_suspensions.correlation_id,
       waitpoints.kind,
       'pending'::text AS waitpoint_status,
       waitpoints.params,
       waitpoints.metadata,
       waitpoints.tags,
       run_suspension_waitpoints.timeout_seconds,
       run_suspensions.status,
       run_suspensions.resolution_kind,
       run_suspensions.resolution,
       waitpoints.created_at,
       run_suspensions.waiting_at AS waiting_at,
       run_suspensions.resolved_at
  FROM run_suspensions
  JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = run_suspensions.org_id
                            AND run_suspension_waitpoints.run_suspension_id = run_suspensions.id
  JOIN waitpoints ON waitpoints.org_id = run_suspension_waitpoints.org_id
                 AND waitpoints.id = run_suspension_waitpoints.waitpoint_id
 WHERE run_suspensions.org_id = sqlc.arg(org_id)
   AND run_suspensions.run_id = sqlc.arg(run_id)
   AND run_suspensions.status = 'waiting'
   AND waitpoints.status = 'pending'
 ORDER BY run_suspensions.waiting_at DESC, run_suspension_waitpoints.ordinal ASC
 LIMIT 1;

-- name: ListPendingWaitpointsForRuns :many
WITH ranked_waitpoints AS (
    SELECT waitpoints.id,
           run_suspensions.id AS run_suspension_id,
           waitpoints.org_id,
           run_suspensions.run_id,
           run_suspensions.run_lease_id,
           run_suspensions.checkpoint_id,
           run_suspensions.correlation_id,
           waitpoints.kind,
           'pending'::text AS waitpoint_status,
           waitpoints.params,
           waitpoints.metadata,
           waitpoints.tags,
           run_suspension_waitpoints.timeout_seconds,
           run_suspensions.status,
           run_suspensions.resolution_kind,
           run_suspensions.resolution,
           waitpoints.created_at,
           run_suspensions.waiting_at AS waiting_at,
           run_suspensions.resolved_at,
           row_number() OVER (
               PARTITION BY run_suspensions.run_id
               ORDER BY run_suspensions.waiting_at DESC, run_suspension_waitpoints.ordinal ASC
           ) AS waitpoint_rank
      FROM run_suspensions
      JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = run_suspensions.org_id
                                AND run_suspension_waitpoints.run_suspension_id = run_suspensions.id
      JOIN waitpoints ON waitpoints.org_id = run_suspension_waitpoints.org_id
                     AND waitpoints.id = run_suspension_waitpoints.waitpoint_id
     WHERE run_suspensions.org_id = sqlc.arg(org_id)
       AND run_suspensions.run_id = ANY(sqlc.arg(run_ids)::uuid[])
       AND run_suspensions.status = 'waiting'
       AND waitpoints.status = 'pending'
)
SELECT id,
       run_suspension_id,
       org_id,
       run_id,
       run_lease_id,
       checkpoint_id,
       correlation_id,
       kind,
       waitpoint_status,
       params,
       metadata,
       tags,
       timeout_seconds,
       status,
       resolution_kind,
       resolution,
       created_at,
       waiting_at,
       resolved_at
  FROM ranked_waitpoints
 WHERE waitpoint_rank = 1;

-- name: UnblockRunWaitpointsForWaitpoint :many
WITH target_waitpoint AS (
    SELECT *
      FROM waitpoints
     WHERE waitpoints.org_id = sqlc.arg(org_id)
       AND waitpoints.id = sqlc.arg(waitpoint_id)
       AND waitpoints.status IN ('completed', 'timed_out')
     FOR UPDATE
),
	eligible_run_suspensions AS (
	    SELECT run_suspensions.*,
	           CASE
	               WHEN dependency_state.timeout_failure_count > 0 THEN 'timed_out'
	               WHEN dependency_state.dependency_count = 1 THEN dependency_state.first_resume_kind
	               ELSE 'waitpoints'
	           END AS resume_kind,
	           CASE
	               WHEN dependency_state.timeout_failure_count > 0 THEN 'null'::jsonb
	               WHEN dependency_state.dependency_count = 1 THEN dependency_state.first_resume_data
	               ELSE jsonb_build_object('waitpoints', COALESCE(dependency_state.resume_data, '[]'::jsonb))
	           END AS resume_output
      FROM target_waitpoint
      JOIN run_suspension_waitpoints target_dependency
        ON target_dependency.org_id = target_waitpoint.org_id
       AND target_dependency.waitpoint_id = target_waitpoint.id
      JOIN run_suspensions ON run_suspensions.org_id = target_dependency.org_id
                    AND run_suspensions.id = target_dependency.run_suspension_id
      JOIN runs ON runs.org_id = run_suspensions.org_id
               AND runs.id = run_suspensions.run_id
	      JOIN LATERAL (
	          SELECT count(*)::int AS dependency_count,
	                 count(*) FILTER (
	                    WHERE dependency_waitpoints.status IN ('completed', 'timed_out')
	                       OR run_suspension_waitpoints.waitpoint_id = target_waitpoint.id
	                 )::int AS resolved_count,
		                 count(*) FILTER (
		                    WHERE (
		                        dependency_waitpoints.status = 'completed'
		                        AND NULLIF(dependency_waitpoints.error, '{}'::jsonb) IS NULL
	                    )
	                    OR (
	                        dependency_waitpoints.kind = 'timer'
	                        AND dependency_waitpoints.status = 'timed_out'
		                    )
		                 )::int AS successful_count,
		                 count(*) FILTER (
		                    WHERE dependency_waitpoints.kind <> 'timer'
		                      AND dependency_waitpoints.status = 'timed_out'
		                 )::int AS timeout_failure_count,
		                 (array_agg(
		                    CASE
		                        WHEN dependency_waitpoints.status <> 'completed' THEN dependency_waitpoints.status::text
                        WHEN NULLIF(dependency_waitpoints.error, '{}'::jsonb) IS NOT NULL THEN dependency_waitpoints.status::text
                        ELSE 'completed'
                    END
                    ORDER BY run_suspension_waitpoints.ordinal
                  ) FILTER (
                    WHERE dependency_waitpoints.status IN ('completed', 'timed_out')
                       OR run_suspension_waitpoints.waitpoint_id = target_waitpoint.id
                 ))[1] AS first_resume_kind,
                 (array_agg(
                    COALESCE(dependency_waitpoints.data, 'null'::jsonb)
                    ORDER BY run_suspension_waitpoints.ordinal
                  ) FILTER (
                    WHERE dependency_waitpoints.status IN ('completed', 'timed_out')
                       OR run_suspension_waitpoints.waitpoint_id = target_waitpoint.id
                  ))[1] AS first_resume_data,
                 jsonb_agg(
                    COALESCE(dependency_waitpoints.data, 'null'::jsonb)
                    ORDER BY run_suspension_waitpoints.ordinal
                 ) FILTER (
                    WHERE dependency_waitpoints.status IN ('completed', 'timed_out')
                       OR run_suspension_waitpoints.waitpoint_id = target_waitpoint.id
                 ) AS resume_data
            FROM run_suspension_waitpoints
            JOIN waitpoints dependency_waitpoints
              ON dependency_waitpoints.org_id = run_suspension_waitpoints.org_id
             AND dependency_waitpoints.id = run_suspension_waitpoints.waitpoint_id
           WHERE run_suspension_waitpoints.org_id = run_suspensions.org_id
             AND run_suspension_waitpoints.run_suspension_id = run_suspensions.id
	      ) dependency_state ON true
	     WHERE run_suspensions.status = 'waiting'
	       AND runs.status = 'waiting'
	       AND runs.current_run_lease_id IS NULL
       AND EXISTS (
           SELECT 1
             FROM run_queue_items
            WHERE run_queue_items.org_id = run_suspensions.org_id
              AND run_queue_items.run_id = run_suspensions.run_id
              AND run_queue_items.status = 'parked'
		       )
		       AND (
		           dependency_state.timeout_failure_count > 0
		           OR dependency_state.dependency_count = 1
		           OR (
		               dependency_state.dependency_count > 1
	               AND dependency_state.successful_count = dependency_state.dependency_count
	               AND dependency_state.resolved_count = dependency_state.dependency_count
	           )
	       )
),
resuming_run_suspensions AS (
    UPDATE run_suspensions
       SET status = 'resuming',
           resolution_kind = eligible_run_suspensions.resume_kind,
           resolution = eligible_run_suspensions.resume_output,
           resolved_at = now(),
           updated_at = now()
      FROM eligible_run_suspensions
     WHERE run_suspensions.org_id = eligible_run_suspensions.org_id
       AND run_suspensions.id = eligible_run_suspensions.id
       AND run_suspensions.status = 'waiting'
    RETURNING run_suspensions.*
),
updated_runs AS (
    UPDATE runs
       SET status = 'queued',
           state_version = state_version + 1,
           updated_at = now()
      FROM resuming_run_suspensions
     WHERE runs.org_id = resuming_run_suspensions.org_id
       AND runs.id = resuming_run_suspensions.run_id
       AND runs.status = 'waiting'
       AND runs.current_run_lease_id IS NULL
    RETURNING runs.id, runs.org_id, runs.project_id, runs.environment_id, runs.current_attempt_id, runs.current_attempt_number, runs.trace_id, runs.root_span_id, runs.state_version
),
queued_attempts AS (
    UPDATE run_attempts
       SET status = 'queued',
           updated_at = now()
      FROM updated_runs
     WHERE run_attempts.org_id = updated_runs.org_id
       AND run_attempts.run_id = updated_runs.id
       AND run_attempts.id = updated_runs.current_attempt_id
    RETURNING run_attempts.id, run_attempts.run_id
),
continuation_queue_entries AS (
    UPDATE run_queue_items
       SET status = 'queued',
           dispatch_message_id = NULL,
           reserved_by_worker_instance_id = NULL,
           reservation_expires_at = NULL,
           dispatch_generation = dispatch_generation + 1,
           last_error = '',
           enqueued_at = now(),
           updated_at = now(),
           finished_at = NULL
      FROM updated_runs
     WHERE run_queue_items.org_id = updated_runs.org_id
       AND run_queue_items.run_id = updated_runs.id
       AND run_queue_items.status = 'parked'
    RETURNING run_queue_items.org_id, run_queue_items.run_id
),
resume_snapshot AS (
    INSERT INTO run_snapshots (org_id, run_id, version, status, attempt_id, run_lease_id, transition, reason)
    SELECT updated_runs.org_id,
           updated_runs.id,
           updated_runs.state_version,
           'queued',
           updated_runs.current_attempt_id,
           resuming_run_suspensions.run_lease_id,
           'waitpoint.completed',
           jsonb_build_object(
               'waitpoint_id', sqlc.arg(waitpoint_id),
               'resolution_kind', resuming_run_suspensions.resolution_kind
           )
      FROM updated_runs
      JOIN resuming_run_suspensions ON resuming_run_suspensions.org_id = updated_runs.org_id
                             AND resuming_run_suspensions.run_id = updated_runs.id
      JOIN queued_attempts ON queued_attempts.run_id = updated_runs.id
      JOIN continuation_queue_entries ON continuation_queue_entries.org_id = updated_runs.org_id
                                     AND continuation_queue_entries.run_id = updated_runs.id
    RETURNING run_snapshots.run_id
),
event_seq AS (
    INSERT INTO event_subject_cursors (org_id, subject_type, subject_id, last_seq)
    SELECT resuming_run_suspensions.org_id, 'run', resuming_run_suspensions.run_id, 1
      FROM resuming_run_suspensions
      JOIN resume_snapshot ON resume_snapshot.run_id = resuming_run_suspensions.run_id
    ON CONFLICT (org_id, subject_type, subject_id)
    DO UPDATE SET last_seq = event_subject_cursors.last_seq + 1,
                  updated_at = now()
    RETURNING org_id, subject_type, subject_id, last_seq
),
event AS (
    INSERT INTO events (org_id, project_id, environment_id, run_id, seq, attempt_id, run_lease_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT resuming_run_suspensions.org_id,
           updated_runs.project_id,
           updated_runs.environment_id,
           resuming_run_suspensions.run_id,
           event_seq.last_seq,
           COALESCE(run_leases.attempt_id, updated_runs.current_attempt_id),
           run_leases.id,
           COALESCE(run_attempts.attempt_number, updated_runs.current_attempt_number),
           COALESCE(run_leases.trace_id, updated_runs.trace_id),
           COALESCE(run_leases.span_id, updated_runs.root_span_id),
           run_leases.parent_span_id,
           COALESCE(run_leases.traceparent, '00-' || updated_runs.trace_id || '-' || updated_runs.root_span_id || '-01'),
           'waitpoint',
           'info',
           'control',
           'waitpoint.completed',
           'waitpoint.completed',
           jsonb_build_object(
               'run_id', resuming_run_suspensions.run_id,
               'waitpoint_id', sqlc.arg(waitpoint_id),
               'kind', target_waitpoint.kind,
               'resolution_kind', resuming_run_suspensions.resolution_kind,
               'payload', resuming_run_suspensions.resolution
           ),
           'internal',
           updated_runs.state_version
      FROM resuming_run_suspensions
      JOIN target_waitpoint ON true
      JOIN updated_runs ON updated_runs.org_id = resuming_run_suspensions.org_id
                       AND updated_runs.id = resuming_run_suspensions.run_id
      LEFT JOIN run_leases ON run_leases.org_id = resuming_run_suspensions.org_id
                              AND run_leases.run_id = resuming_run_suspensions.run_id
                              AND run_leases.id = resuming_run_suspensions.run_lease_id
      LEFT JOIN run_attempts ON run_attempts.org_id = run_leases.org_id
                            AND run_attempts.run_id = run_leases.run_id
                            AND run_attempts.id = run_leases.attempt_id
      JOIN continuation_queue_entries ON continuation_queue_entries.org_id = resuming_run_suspensions.org_id
                                     AND continuation_queue_entries.run_id = resuming_run_suspensions.run_id
      JOIN resume_snapshot ON resume_snapshot.run_id = resuming_run_suspensions.run_id
      JOIN event_seq ON event_seq.org_id = resuming_run_suspensions.org_id
                    AND event_seq.subject_type = 'run'
                    AND event_seq.subject_id = resuming_run_suspensions.run_id
    RETURNING *
),
event_outbox AS (
    INSERT INTO event_outbox (event_record_id, stream_key)
    SELECT event.id,
           'helmr:events:' || event.org_id::text || ':' || event.subject_type::text || ':' || event.subject_id::text
      FROM event
    RETURNING id
)
SELECT waitpoints.id,
       resuming_run_suspensions.id AS run_suspension_id,
       waitpoints.org_id,
       resuming_run_suspensions.run_id,
       resuming_run_suspensions.run_lease_id,
       resuming_run_suspensions.checkpoint_id,
       resuming_run_suspensions.correlation_id,
       waitpoints.kind,
       COALESCE(waitpoints.params, '{}'::jsonb) AS params,
       COALESCE(waitpoints.metadata, '{}'::jsonb) AS metadata,
       COALESCE(waitpoints.tags, '{}'::text[]) AS tags,
       run_suspension_waitpoints.timeout_seconds,
       resuming_run_suspensions.status,
       resuming_run_suspensions.resolution_kind,
       resuming_run_suspensions.resolution,
       waitpoints.created_at,
       resuming_run_suspensions.waiting_at AS waiting_at,
       resuming_run_suspensions.resolved_at
  FROM resuming_run_suspensions
  JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = resuming_run_suspensions.org_id
                            AND run_suspension_waitpoints.run_suspension_id = resuming_run_suspensions.id
  JOIN waitpoints ON waitpoints.org_id = run_suspension_waitpoints.org_id
                 AND waitpoints.id = run_suspension_waitpoints.waitpoint_id
  JOIN event_outbox ON true
 WHERE waitpoints.id = sqlc.arg(waitpoint_id);

-- name: ExpireDuePendingWaitpoints :exec
WITH expired_waitpoint_tokens AS (
    UPDATE waitpoint_tokens
       SET status = 'timed_out',
           error = COALESCE(waitpoint_tokens.error, jsonb_build_object('code', 'timed_out', 'at', to_jsonb(now()))),
           updated_at = now()
     WHERE waitpoint_tokens.org_id = sqlc.arg(org_id)
       AND waitpoint_tokens.status = 'waiting'
       AND waitpoint_tokens.timeout_at <= now()
    RETURNING waitpoint_tokens.*
),
due_waitpoints AS (
    SELECT run_suspensions.*,
           waitpoints.id AS waitpoint_id,
           waitpoints.kind AS waitpoint_kind
      FROM run_suspensions
      JOIN runs ON runs.org_id = run_suspensions.org_id
               AND runs.id = run_suspensions.run_id
      JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = run_suspensions.org_id
                                AND run_suspension_waitpoints.run_suspension_id = run_suspensions.id
      JOIN waitpoints ON waitpoints.org_id = run_suspension_waitpoints.org_id
                     AND waitpoints.id = run_suspension_waitpoints.waitpoint_id
      LEFT JOIN waitpoint_tokens ON waitpoint_tokens.org_id = waitpoints.org_id
                                AND waitpoint_tokens.id = waitpoints.waitpoint_token_id
     WHERE run_suspensions.org_id = sqlc.arg(org_id)
       AND run_suspensions.status = 'waiting'
       AND waitpoints.status = 'pending'
       AND (
           (
               run_suspension_waitpoints.timeout_seconds IS NOT NULL
               AND run_suspensions.waiting_at + (run_suspension_waitpoints.timeout_seconds * interval '1 second') <= now()
           )
           OR waitpoint_tokens.status = 'timed_out'
       )
       AND runs.status = 'waiting'
       AND runs.current_run_lease_id IS NULL
       AND EXISTS (
           SELECT 1
             FROM run_queue_items
            WHERE run_queue_items.org_id = run_suspensions.org_id
              AND run_queue_items.run_id = run_suspensions.run_id
              AND run_queue_items.status = 'parked'
       )
     FOR UPDATE OF run_suspensions
),
expired_waitpoints AS (
    UPDATE waitpoints
       SET status = 'timed_out',
           data = NULL,
           error = jsonb_build_object('code', 'timed_out', 'at', to_jsonb(now())),
           resolved_at = now(),
           updated_at = now()
      FROM due_waitpoints
     WHERE waitpoints.org_id = due_waitpoints.org_id
       AND waitpoints.id = due_waitpoints.waitpoint_id
       AND waitpoints.status = 'pending'
    RETURNING waitpoints.*
),
candidate_run_suspensions AS (
    SELECT DISTINCT run_suspensions.*
      FROM expired_waitpoints
      JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = expired_waitpoints.org_id
                                AND run_suspension_waitpoints.waitpoint_id = expired_waitpoints.id
      JOIN run_suspensions ON run_suspensions.org_id = run_suspension_waitpoints.org_id
                    AND run_suspensions.id = run_suspension_waitpoints.run_suspension_id
     WHERE run_suspensions.status = 'waiting'
),
eligible_run_suspensions AS (
    SELECT candidate_run_suspensions.*,
           CASE
               WHEN dependency_state.expired_failure_count > 0 THEN 'timed_out'
               WHEN dependency_state.dependency_count = 1 THEN dependency_state.first_resume_kind
               ELSE 'waitpoints'
           END AS resume_kind,
           CASE
               WHEN dependency_state.expired_failure_count > 0 THEN 'null'::jsonb
               WHEN dependency_state.dependency_count = 1 THEN dependency_state.first_resume_data
               ELSE jsonb_build_object('waitpoints', COALESCE(dependency_state.resume_data, '[]'::jsonb))
           END AS resume_output
      FROM candidate_run_suspensions
      JOIN LATERAL (
          SELECT count(*)::int AS dependency_count,
                 count(*) FILTER (
                    WHERE dependency_waitpoints.status IN ('completed', 'timed_out')
                 )::int AS resolved_count,
	                 count(*) FILTER (
	                    WHERE (
	                        dependency_waitpoints.status = 'completed'
	                        AND NULLIF(dependency_waitpoints.error, '{}'::jsonb) IS NULL
                    )
                    OR (
                        dependency_waitpoints.kind = 'timer'
                        AND dependency_waitpoints.status = 'timed_out'
                    )
                 )::int AS successful_count,
                 count(*) FILTER (
                    WHERE expired_waitpoints.id IS NOT NULL
                      AND expired_waitpoints.kind <> 'timer'
                 )::int AS expired_failure_count,
                 (array_agg(
                    CASE
                        WHEN dependency_waitpoints.kind = 'timer' AND dependency_waitpoints.status = 'timed_out' THEN 'timed_out'
                        WHEN dependency_waitpoints.status <> 'completed' THEN dependency_waitpoints.status::text
                        WHEN NULLIF(dependency_waitpoints.error, '{}'::jsonb) IS NOT NULL THEN dependency_waitpoints.status::text
                        ELSE 'completed'
                    END
                    ORDER BY run_suspension_waitpoints.ordinal
                  ) FILTER (
                    WHERE dependency_waitpoints.status IN ('completed', 'timed_out')
                  ))[1] AS first_resume_kind,
                 (array_agg(
                    COALESCE(dependency_waitpoints.data, 'null'::jsonb)
                    ORDER BY run_suspension_waitpoints.ordinal
                  ) FILTER (
                    WHERE dependency_waitpoints.status IN ('completed', 'timed_out')
                  ))[1] AS first_resume_data,
                 jsonb_agg(
                    COALESCE(dependency_waitpoints.data, 'null'::jsonb)
                    ORDER BY run_suspension_waitpoints.ordinal
                 ) FILTER (
                    WHERE dependency_waitpoints.status IN ('completed', 'timed_out')
                 ) AS resume_data
            FROM run_suspension_waitpoints
            JOIN waitpoints dependency_waitpoints
              ON dependency_waitpoints.org_id = run_suspension_waitpoints.org_id
             AND dependency_waitpoints.id = run_suspension_waitpoints.waitpoint_id
            LEFT JOIN expired_waitpoints ON expired_waitpoints.org_id = dependency_waitpoints.org_id
                                        AND expired_waitpoints.id = dependency_waitpoints.id
           WHERE run_suspension_waitpoints.org_id = candidate_run_suspensions.org_id
             AND run_suspension_waitpoints.run_suspension_id = candidate_run_suspensions.id
      ) dependency_state ON true
     WHERE dependency_state.expired_failure_count > 0
        OR (
            dependency_state.dependency_count > 0
            AND dependency_state.successful_count = dependency_state.dependency_count
            AND dependency_state.resolved_count = dependency_state.dependency_count
        )
),
expired_run_suspensions AS (
    UPDATE run_suspensions
       SET status = 'resuming',
           resolution_kind = eligible_run_suspensions.resume_kind,
           resolution = eligible_run_suspensions.resume_output,
           resolved_at = now(),
           updated_at = now()
      FROM eligible_run_suspensions
     WHERE run_suspensions.org_id = eligible_run_suspensions.org_id
       AND run_suspensions.id = eligible_run_suspensions.id
       AND run_suspensions.status = 'waiting'
    RETURNING run_suspensions.*
),
released_channel_wait_matches AS (
    UPDATE channel_waits
       SET matched_record_id = NULL,
           matched_at = NULL
      FROM expired_run_suspensions
      JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = expired_run_suspensions.org_id
                                AND run_suspension_waitpoints.run_suspension_id = expired_run_suspensions.id
     WHERE channel_waits.org_id = run_suspension_waitpoints.org_id
       AND channel_waits.waitpoint_id = run_suspension_waitpoints.waitpoint_id
       AND channel_waits.run_suspension_id = expired_run_suspensions.id
       AND channel_waits.matched_record_id IS NOT NULL
       AND expired_run_suspensions.resolution_kind NOT IN ('completed', 'waitpoints')
    RETURNING channel_waits.waitpoint_id
),
released_channel_match_count AS (
    SELECT count(*) AS match_count FROM released_channel_wait_matches
),
updated_runs AS (
    UPDATE runs
       SET status = 'queued',
           state_version = state_version + 1,
           updated_at = now()
      FROM expired_run_suspensions
     WHERE runs.org_id = expired_run_suspensions.org_id
       AND runs.id = expired_run_suspensions.run_id
       AND runs.status = 'waiting'
       AND runs.current_run_lease_id IS NULL
    RETURNING runs.id, runs.org_id, runs.project_id, runs.environment_id, runs.current_attempt_id, runs.current_attempt_number, runs.trace_id, runs.root_span_id, runs.state_version
),
queued_attempts AS (
    UPDATE run_attempts
       SET status = 'queued',
           updated_at = now()
      FROM updated_runs
     WHERE run_attempts.org_id = updated_runs.org_id
       AND run_attempts.run_id = updated_runs.id
       AND run_attempts.id = updated_runs.current_attempt_id
    RETURNING run_attempts.id, run_attempts.run_id
),
continuation_queue_entries AS (
    UPDATE run_queue_items
       SET status = 'queued',
           dispatch_message_id = NULL,
           reserved_by_worker_instance_id = NULL,
           reservation_expires_at = NULL,
           dispatch_generation = dispatch_generation + 1,
           last_error = '',
           enqueued_at = now(),
           updated_at = now(),
           finished_at = NULL
      FROM updated_runs
    WHERE run_queue_items.org_id = updated_runs.org_id
       AND run_queue_items.run_id = updated_runs.id
       AND run_queue_items.status = 'parked'
    RETURNING run_queue_items.org_id, run_queue_items.run_id
),
timeout_snapshots AS (
    INSERT INTO run_snapshots (org_id, run_id, version, status, attempt_id, run_lease_id, transition, reason)
    SELECT updated_runs.org_id,
           updated_runs.id,
           updated_runs.state_version,
           'queued',
           updated_runs.current_attempt_id,
           expired_run_suspensions.run_lease_id,
           'waitpoint.timed_out',
           jsonb_build_object('resolution_kind', 'timed_out')
      FROM updated_runs
      JOIN expired_run_suspensions ON expired_run_suspensions.org_id = updated_runs.org_id
                            AND expired_run_suspensions.run_id = updated_runs.id
      JOIN queued_attempts ON queued_attempts.run_id = updated_runs.id
      JOIN continuation_queue_entries ON continuation_queue_entries.org_id = updated_runs.org_id
                                     AND continuation_queue_entries.run_id = updated_runs.id
    RETURNING run_snapshots.run_id
),
event_inputs AS (
    SELECT expired_run_suspensions.org_id,
           updated_runs.project_id,
           updated_runs.environment_id,
           expired_run_suspensions.run_id,
           COALESCE(run_leases.attempt_id, updated_runs.current_attempt_id) AS attempt_id,
           run_leases.id AS run_lease_id,
           COALESCE(run_attempts.attempt_number, updated_runs.current_attempt_number) AS attempt_number,
           COALESCE(run_leases.trace_id, updated_runs.trace_id) AS trace_id,
           COALESCE(run_leases.span_id, updated_runs.root_span_id) AS span_id,
           run_leases.parent_span_id,
           COALESCE(run_leases.traceparent, '00-' || updated_runs.trace_id || '-' || updated_runs.root_span_id || '-01') AS traceparent,
           'waitpoint' AS category,
           'warn' AS severity,
           'control' AS source,
           'waitpoint.timed_out' AS kind,
           'waitpoint.timed_out' AS message,
           jsonb_build_object(
               'run_id', expired_run_suspensions.run_id,
               'waitpoint_id', expired_waitpoints.id,
               'kind', expired_waitpoints.kind,
               'resolution_kind', 'timed_out'
           ) AS payload,
           'internal' AS redaction_class,
           updated_runs.state_version AS snapshot_version,
           row_number() OVER (PARTITION BY expired_run_suspensions.org_id, expired_run_suspensions.run_id ORDER BY expired_waitpoints.id) AS event_ordinal
  FROM expired_run_suspensions
  JOIN updated_runs ON updated_runs.org_id = expired_run_suspensions.org_id
                   AND updated_runs.id = expired_run_suspensions.run_id
  LEFT JOIN run_leases ON run_leases.org_id = expired_run_suspensions.org_id
                          AND run_leases.run_id = expired_run_suspensions.run_id
                          AND run_leases.id = expired_run_suspensions.run_lease_id
  LEFT JOIN run_attempts ON run_attempts.org_id = run_leases.org_id
                        AND run_attempts.run_id = run_leases.run_id
                        AND run_attempts.id = run_leases.attempt_id
  JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = expired_run_suspensions.org_id
                            AND run_suspension_waitpoints.run_suspension_id = expired_run_suspensions.id
  JOIN expired_waitpoints ON expired_waitpoints.org_id = run_suspension_waitpoints.org_id
                         AND expired_waitpoints.id = run_suspension_waitpoints.waitpoint_id
  JOIN continuation_queue_entries ON continuation_queue_entries.org_id = expired_run_suspensions.org_id
                                 AND continuation_queue_entries.run_id = expired_run_suspensions.run_id
  JOIN timeout_snapshots ON timeout_snapshots.run_id = expired_run_suspensions.run_id
),
event_subject_counts AS (
    SELECT event_inputs.org_id,
           event_inputs.run_id,
           count(*)::bigint AS event_count
      FROM event_inputs
     GROUP BY event_inputs.org_id, event_inputs.run_id
),
event_seq AS (
    INSERT INTO event_subject_cursors (org_id, subject_type, subject_id, last_seq)
    SELECT event_subject_counts.org_id,
           'run',
           event_subject_counts.run_id,
           event_subject_counts.event_count
      FROM event_subject_counts
    ON CONFLICT (org_id, subject_type, subject_id)
    DO UPDATE SET last_seq = event_subject_cursors.last_seq + EXCLUDED.last_seq,
                  updated_at = now()
    RETURNING org_id, subject_type, subject_id, last_seq
),
event AS (
    INSERT INTO events (org_id, project_id, environment_id, run_id, seq, attempt_id, run_lease_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT event_inputs.org_id,
           event_inputs.project_id,
           event_inputs.environment_id,
           event_inputs.run_id,
           event_seq.last_seq - event_subject_counts.event_count + event_inputs.event_ordinal,
           event_inputs.attempt_id,
           event_inputs.run_lease_id,
           event_inputs.attempt_number,
           event_inputs.trace_id,
           event_inputs.span_id,
           event_inputs.parent_span_id,
           event_inputs.traceparent,
           event_inputs.category,
           event_inputs.severity,
           event_inputs.source,
           event_inputs.kind,
           event_inputs.message,
           event_inputs.payload,
           event_inputs.redaction_class,
           event_inputs.snapshot_version
      FROM event_inputs
      JOIN event_subject_counts ON event_subject_counts.org_id = event_inputs.org_id
                               AND event_subject_counts.run_id = event_inputs.run_id
      JOIN event_seq ON event_seq.org_id = event_inputs.org_id
                    AND event_seq.subject_type = 'run'
                    AND event_seq.subject_id = event_inputs.run_id
    RETURNING *
),
event_outbox AS (
    INSERT INTO event_outbox (event_record_id, stream_key)
    SELECT event.id,
           'helmr:events:' || event.org_id::text || ':' || event.subject_type::text || ':' || event.subject_id::text
      FROM event
    RETURNING id
)
SELECT event.*
  FROM event
  JOIN event_outbox ON true
  JOIN released_channel_match_count ON true;
