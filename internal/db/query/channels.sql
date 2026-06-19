-- name: GetChannelRecordByIdempotencyKey :one
SELECT channel_records.*
  FROM channel_records
 WHERE channel_records.org_id = sqlc.arg(org_id)
   AND channel_records.channel_id = sqlc.arg(channel_id)
   AND channel_records.idempotency_key = sqlc.arg(idempotency_key)
   AND sqlc.arg(idempotency_key)::text <> '';

-- name: GetChannelRecordByExternalEventID :one
SELECT channel_records.*
  FROM channel_records
 WHERE channel_records.org_id = sqlc.arg(org_id)
   AND channel_records.channel_id = sqlc.arg(channel_id)
   AND channel_records.external_event_id = sqlc.arg(external_event_id)
   AND sqlc.arg(external_event_id)::text <> '';

-- name: LockTaskSessionForChannelAppend :one
SELECT task_sessions.*
  FROM channels
  JOIN task_sessions ON task_sessions.org_id = channels.org_id
                    AND task_sessions.project_id = channels.project_id
                    AND task_sessions.environment_id = channels.environment_id
                    AND task_sessions.id = channels.task_session_id
 WHERE channels.org_id = sqlc.arg(org_id)
   AND channels.project_id = sqlc.arg(project_id)
   AND channels.environment_id = sqlc.arg(environment_id)
   AND channels.id = sqlc.arg(channel_id)
   AND channels.direction = 'input'
 FOR UPDATE OF task_sessions, channels;

-- name: AppendChannelRecord :one
WITH locked_channel AS (
    SELECT *
      FROM channels
     WHERE channels.org_id = sqlc.arg(org_id)
       AND channels.project_id = sqlc.arg(project_id)
       AND channels.environment_id = sqlc.arg(environment_id)
       AND channels.id = sqlc.arg(channel_id)
       AND channels.direction = sqlc.arg(direction)::channel_direction
     FOR UPDATE
),
allocated_channel AS (
    UPDATE channels
       SET next_sequence = channels.next_sequence + 1
      FROM locked_channel
     WHERE channels.org_id = locked_channel.org_id
       AND channels.id = locked_channel.id
    RETURNING channels.*, channels.next_sequence - 1 AS allocated_sequence
),
inserted_record AS (
    INSERT INTO channel_records (
        id,
        org_id,
        project_id,
        environment_id,
        channel_id,
        sequence,
        data,
        correlation_id,
        content_type,
        idempotency_key,
        idempotency_fingerprint,
        external_event_id,
        actor,
        source,
        auth_subject_type,
        auth_subject_id,
        public_access_token_id
    )
    SELECT sqlc.arg(id),
           allocated_channel.org_id,
           allocated_channel.project_id,
           allocated_channel.environment_id,
           allocated_channel.id,
           allocated_channel.allocated_sequence,
           COALESCE(sqlc.arg(data)::jsonb, 'null'::jsonb),
           COALESCE(sqlc.arg(correlation_id)::text, ''),
           COALESCE(NULLIF(sqlc.arg(content_type)::text, ''), 'application/json'),
           COALESCE(sqlc.arg(idempotency_key)::text, ''),
           COALESCE(sqlc.arg(idempotency_fingerprint)::text, ''),
           COALESCE(sqlc.arg(external_event_id)::text, ''),
           COALESCE(sqlc.arg(actor)::jsonb, '{}'::jsonb),
           COALESCE(sqlc.arg(source)::text, ''),
           sqlc.arg(auth_subject_type)::channel_record_auth_subject_type,
           COALESCE(sqlc.arg(auth_subject_id)::text, ''),
           sqlc.narg(public_access_token_id)::uuid
      FROM allocated_channel
    RETURNING channel_records.*
)
SELECT inserted_record.*
  FROM inserted_record;

-- name: ListChannelRecords :many
SELECT channel_records.*
  FROM channel_records
  JOIN channels ON channels.org_id = channel_records.org_id
               AND channels.id = channel_records.channel_id
 WHERE channel_records.org_id = sqlc.arg(org_id)
   AND channel_records.channel_id = sqlc.arg(channel_id)
   AND channels.project_id = sqlc.arg(project_id)
   AND channels.environment_id = sqlc.arg(environment_id)
   AND channels.direction = sqlc.arg(direction)::channel_direction
   AND channel_records.sequence > sqlc.arg(after_sequence)::bigint
   AND (
       sqlc.narg(correlation_id)::text IS NULL
       OR channel_records.correlation_id = sqlc.narg(correlation_id)::text
   )
 ORDER BY channel_records.sequence ASC, channel_records.id ASC
 LIMIT sqlc.arg(limit_count);

-- name: ResolveChannelWaitpointsForChannel :many
WITH candidate_raw AS (
    SELECT channel_waits.waitpoint_id,
           channel_waits.org_id,
           channel_waits.run_id,
           channel_waits.channel_id,
           channels.name AS channel,
	           channel_waits.created_at,
	           runs.task_session_id,
	           'runtime:input-wait'::text AS consumer_key,
	           next_record.id AS record_id,
	           next_record.sequence,
	           next_record.data,
	           channel_waits.correlation_id AS wait_correlation_id,
	           next_record.correlation_id AS record_correlation_id
      FROM channel_waits
      JOIN channels ON channels.org_id = channel_waits.org_id
                   AND channels.id = channel_waits.channel_id
      JOIN waitpoints ON waitpoints.org_id = channel_waits.org_id
                     AND waitpoints.id = channel_waits.waitpoint_id
      JOIN run_suspensions ON run_suspensions.org_id = channel_waits.org_id
                     AND run_suspensions.id = channel_waits.run_suspension_id
      JOIN runs ON runs.org_id = channel_waits.org_id
               AND runs.id = channel_waits.run_id
      JOIN LATERAL (
          SELECT channel_records.*
            FROM channel_records
           WHERE channel_records.org_id = channel_waits.org_id
             AND channel_records.channel_id = channel_waits.channel_id
             AND channel_records.sequence > channel_waits.after_sequence
             AND (
                 channel_waits.correlation_id = ''
                 OR channel_records.correlation_id = channel_waits.correlation_id
             )
             AND NOT EXISTS (
                 SELECT 1
                   FROM channel_waits matched_wait
                  WHERE matched_wait.org_id = channel_waits.org_id
                    AND matched_wait.channel_id = channel_waits.channel_id
                    AND matched_wait.matched_record_id = channel_records.id
             )
           ORDER BY channel_records.sequence ASC, channel_records.id ASC
           LIMIT 1
      ) next_record ON true
     WHERE channel_waits.org_id = sqlc.arg(org_id)
       AND channel_waits.channel_id = sqlc.arg(channel_id)
       AND channel_waits.matched_record_id IS NULL
       AND waitpoints.kind = 'channel'
       AND waitpoints.status = 'pending'
       AND run_suspensions.status = 'waiting'
       AND runs.status = 'waiting'
       AND runs.current_run_lease_id IS NULL
       AND NOT EXISTS (
           SELECT 1
             FROM channel_waits earlier_wait
             JOIN waitpoints earlier_waitpoint
               ON earlier_waitpoint.org_id = earlier_wait.org_id
              AND earlier_waitpoint.id = earlier_wait.waitpoint_id
             JOIN run_suspensions earlier_run_suspension
               ON earlier_run_suspension.org_id = earlier_wait.org_id
              AND earlier_run_suspension.id = earlier_wait.run_suspension_id
            WHERE earlier_wait.org_id = channel_waits.org_id
              AND earlier_wait.channel_id = channel_waits.channel_id
              AND earlier_wait.correlation_id = channel_waits.correlation_id
              AND earlier_wait.matched_record_id IS NULL
              AND earlier_waitpoint.kind = 'channel'
              AND earlier_waitpoint.status = 'pending'
              AND earlier_run_suspension.status = 'waiting'
              AND (earlier_wait.created_at, earlier_wait.waitpoint_id) < (channel_waits.created_at, channel_waits.waitpoint_id)
       )
       AND EXISTS (
           SELECT 1
             FROM run_queue_items
            WHERE run_queue_items.org_id = channel_waits.org_id
              AND run_queue_items.run_id = channel_waits.run_id
              AND run_queue_items.status = 'parked'
       )
     ORDER BY channel_waits.created_at ASC, channel_waits.waitpoint_id ASC
     FOR UPDATE OF channel_waits, waitpoints, run_suspensions
),
candidate AS (
    SELECT *
      FROM (
          SELECT candidate_raw.*,
                 row_number() OVER (
                     PARTITION BY candidate_raw.org_id, candidate_raw.channel_id, candidate_raw.record_id
                     ORDER BY candidate_raw.created_at ASC, candidate_raw.waitpoint_id ASC
                 ) AS record_match_rank
            FROM candidate_raw
      ) ranked_candidate
     WHERE record_match_rank = 1
),
matched_wait AS (
    UPDATE channel_waits
       SET matched_record_id = candidate.record_id,
           matched_at = now()
      FROM candidate
     WHERE channel_waits.org_id = candidate.org_id
       AND channel_waits.waitpoint_id = candidate.waitpoint_id
       AND channel_waits.matched_record_id IS NULL
    RETURNING channel_waits.waitpoint_id,
              channel_waits.org_id,
              channel_waits.run_id,
              channel_waits.channel_id,
              candidate.task_session_id,
              candidate.consumer_key,
              candidate.channel,
	              candidate.record_id,
	              candidate.sequence,
	              candidate.data,
	              candidate.wait_correlation_id,
	              candidate.record_correlation_id
),
completed_waitpoint AS (
    UPDATE waitpoints
       SET status = 'completed',
           data = jsonb_build_object(
	               'channel', matched_wait.channel,
	               'sequence', matched_wait.sequence,
	               'correlation_id', matched_wait.record_correlation_id,
	               'data', matched_wait.data
           ),
           error = NULL,
           resolved_at = now(),
           updated_at = now()
      FROM matched_wait
     WHERE waitpoints.org_id = matched_wait.org_id
       AND waitpoints.id = matched_wait.waitpoint_id
       AND waitpoints.status = 'pending'
    RETURNING waitpoints.id, waitpoints.org_id, waitpoints.run_id
)
SELECT *
  FROM completed_waitpoint;

-- name: AppendExecutionChannelRecord :one
WITH locked_task_session AS MATERIALIZED (
    SELECT task_sessions.id,
           task_sessions.org_id,
           task_sessions.project_id,
           task_sessions.environment_id,
           task_sessions.current_run_id
      FROM task_sessions
      JOIN runs ON runs.org_id = task_sessions.org_id
               AND runs.project_id = task_sessions.project_id
               AND runs.environment_id = task_sessions.environment_id
               AND runs.task_session_id = task_sessions.id
               AND runs.id = task_sessions.current_run_id
     WHERE task_sessions.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND task_sessions.status = 'open'
     FOR UPDATE OF task_sessions
),
current_run_lease AS (
	    SELECT runs.id,
	           runs.project_id,
	           runs.environment_id,
	           runs.deployment_id,
	           runs.task_id,
	           runs.task_session_id,
	           runs.trace_id,
	           runs.state_version,
           run_leases.id AS run_lease_id,
           run_leases.attempt_id,
           run_leases.span_id,
           run_leases.parent_span_id,
           run_leases.traceparent,
           run_attempts.attempt_number
      FROM locked_task_session
      JOIN runs ON runs.org_id = locked_task_session.org_id
               AND runs.project_id = locked_task_session.project_id
               AND runs.environment_id = locked_task_session.environment_id
               AND runs.id = locked_task_session.current_run_id
               AND runs.task_session_id = locked_task_session.id
      JOIN run_leases ON run_leases.id = runs.current_run_lease_id
                          AND run_leases.org_id = runs.org_id
                          AND run_leases.run_id = runs.id
      JOIN run_attempts ON run_attempts.org_id = run_leases.org_id
                       AND run_attempts.run_id = run_leases.run_id
                       AND run_attempts.id = run_leases.attempt_id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status = 'running'
       AND run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.status IN ('leased', 'running')
       AND run_leases.lease_expires_at > now()
     FOR UPDATE OF runs, run_leases
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
    SELECT sqlc.arg(org_id),
           current_run_lease.project_id,
           current_run_lease.environment_id,
           current_run_lease.deployment_id,
           current_run_lease.task_id,
           sqlc.arg(channel),
           'output'
      FROM current_run_lease
    ON CONFLICT (org_id, deployment_id, task_id, name, direction)
    DO UPDATE SET name = EXCLUDED.name
    RETURNING *
),
	allocated_channel AS (
	    INSERT INTO channels (
	        org_id,
	        project_id,
	        environment_id,
	        task_session_id,
	        definition_id,
	        name,
	        direction,
        next_sequence
    )
    SELECT
	        sqlc.arg(org_id),
	        current_run_lease.project_id,
	        current_run_lease.environment_id,
	        current_run_lease.task_session_id,
	        channel_definition.id,
	        sqlc.arg(channel),
	        'output',
	        2
	      FROM current_run_lease
	      JOIN channel_definition ON channel_definition.deployment_id = current_run_lease.deployment_id
	    ON CONFLICT (org_id, task_session_id, name, direction)
	    DO UPDATE SET next_sequence = channels.next_sequence + 1
	    RETURNING channels.*, channels.next_sequence - 1 AS allocated_sequence
	),
inserted_record AS (
    INSERT INTO channel_records (
        id,
        org_id,
        project_id,
        environment_id,
        channel_id,
        sequence,
        data,
        content_type,
        object_refs,
        actor,
        source,
        auth_subject_type,
        auth_subject_id
    )
    SELECT sqlc.arg(id),
           allocated_channel.org_id,
           allocated_channel.project_id,
           allocated_channel.environment_id,
           allocated_channel.id,
           allocated_channel.allocated_sequence,
           COALESCE(sqlc.arg(payload)::jsonb, 'null'::jsonb),
           COALESCE(NULLIF(sqlc.arg(content_type)::text, ''), 'application/json'),
           CASE
               WHEN sqlc.narg(object_ref)::jsonb IS NULL THEN '[]'::jsonb
               ELSE jsonb_build_array(sqlc.narg(object_ref)::jsonb)
           END,
           jsonb_build_object(
               'run_lease_id', current_run_lease.run_lease_id,
               'attempt_id', current_run_lease.attempt_id,
               'attempt_number', current_run_lease.attempt_number
           ),
           'worker',
	           'worker_lease'::channel_record_auth_subject_type,
	           current_run_lease.run_lease_id::text
	      FROM allocated_channel
	      JOIN current_run_lease ON true
	    RETURNING channel_records.*
		),
event_seq AS (
    INSERT INTO event_subject_cursors (org_id, subject_type, subject_id, last_seq)
    SELECT inserted_record.org_id,
           'run',
           current_run_lease.id,
           1
      FROM inserted_record
      JOIN current_run_lease ON true
    ON CONFLICT (org_id, subject_type, subject_id)
    DO UPDATE SET last_seq = event_subject_cursors.last_seq + 1,
                  updated_at = now()
    RETURNING org_id, subject_type, subject_id, last_seq
),
inserted_event AS (
    INSERT INTO events (org_id, project_id, environment_id, run_id, seq, attempt_id, run_lease_id, attempt_number, trace_id, span_id, parent_span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT inserted_record.org_id,
           inserted_record.project_id,
           inserted_record.environment_id,
           current_run_lease.id,
           event_seq.last_seq,
           current_run_lease.attempt_id,
           current_run_lease.run_lease_id,
           current_run_lease.attempt_number,
           current_run_lease.trace_id,
           current_run_lease.span_id,
           current_run_lease.parent_span_id,
           current_run_lease.traceparent,
           'guest',
           'info',
           'worker',
           'channel.appended',
           'channel.appended',
           jsonb_build_object(
               'channel', allocated_channel.name,
               'record_id', inserted_record.id,
               'sequence', inserted_record.sequence,
               'content_type', inserted_record.content_type,
               'object_refs', inserted_record.object_refs
           ),
	           'sensitive',
	           current_run_lease.state_version
	      FROM inserted_record
	      JOIN allocated_channel ON allocated_channel.id = inserted_record.channel_id
	      JOIN current_run_lease ON true
	      JOIN event_seq ON event_seq.org_id = inserted_record.org_id
	                    AND event_seq.subject_type = 'run'
	                    AND event_seq.subject_id = current_run_lease.id
	    RETURNING id
	),
event_outbox AS (
    INSERT INTO event_outbox (event_record_id, stream_key)
    SELECT inserted_event.id,
           'helmr:events:' || inserted_record.org_id::text || ':run:' || current_run_lease.id::text
      FROM inserted_event
      JOIN inserted_record ON true
      JOIN current_run_lease ON true
    RETURNING id
)
SELECT inserted_record.*
  FROM inserted_record
  JOIN event_outbox ON true;
-- name: AppendSessionChannelInput :one
WITH target_session AS (
    SELECT task_sessions.id,
           task_sessions.org_id,
           task_sessions.project_id,
           task_sessions.environment_id,
           task_sessions.current_run_id
      FROM task_sessions
     WHERE task_sessions.org_id = sqlc.arg(org_id)
       AND task_sessions.current_run_id = sqlc.arg(run_id)
       AND task_sessions.status = 'open'
     FOR UPDATE
),
target_run AS (
    SELECT runs.id,
           runs.project_id,
           runs.environment_id,
           runs.deployment_id,
           runs.task_id,
           runs.trace_id,
           runs.root_span_id,
           runs.current_attempt_id,
           runs.current_attempt_number,
           runs.state_version,
           runs.task_session_id
      FROM runs
      JOIN target_session ON target_session.org_id = runs.org_id
                         AND target_session.current_run_id = runs.id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status IN ('queued', 'running', 'waiting')
     FOR UPDATE OF runs
),
input_values AS (
    SELECT sqlc.arg(channel)::text AS channel,
           COALESCE(sqlc.arg(data)::jsonb, 'null'::jsonb) AS data,
           COALESCE(sqlc.arg(correlation_id)::text, '')::text AS correlation_id,
           COALESCE(NULLIF(sqlc.arg(content_type)::text, ''), 'application/json')::text AS content_type,
           COALESCE(sqlc.arg(idempotency_key)::text, '')::text AS idempotency_key,
           COALESCE(sqlc.arg(idempotency_fingerprint)::text, '')::text AS idempotency_fingerprint,
           COALESCE(sqlc.arg(external_event_id)::text, '')::text AS external_event_id
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
    SELECT sqlc.arg(org_id),
           target_run.project_id,
           target_run.environment_id,
           target_run.deployment_id,
           target_run.task_id,
           input_values.channel,
           'input'
      FROM target_run
      JOIN input_values ON true
    ON CONFLICT (org_id, deployment_id, task_id, name, direction)
    DO UPDATE SET name = EXCLUDED.name
    RETURNING *
),
	inserted_session_channel AS (
	    INSERT INTO channels (
	        org_id,
	        project_id,
	        environment_id,
	        task_session_id,
	        definition_id,
	        name,
	        direction,
	        next_sequence
	    )
	    SELECT sqlc.arg(org_id),
	           target_run.project_id,
	           target_run.environment_id,
	           target_run.task_session_id,
	           channel_definition.id,
	           input_values.channel,
	           'input',
	           2
	      FROM target_run
	      JOIN input_values ON true
	      JOIN channel_definition ON channel_definition.deployment_id = target_run.deployment_id
	    ON CONFLICT (org_id, task_session_id, name, direction)
	    DO NOTHING
	    RETURNING channels.*, 1::bigint AS allocated_sequence
	),
	existing_session_channel AS (
	    SELECT channels.*
	      FROM channels
	      JOIN target_run ON target_run.task_session_id = channels.task_session_id
	      JOIN input_values ON input_values.channel = channels.name
	     WHERE channels.org_id = sqlc.arg(org_id)
	       AND channels.direction = 'input'
	       AND NOT EXISTS (SELECT 1 FROM inserted_session_channel)
	),
	selected_channel AS (
	    SELECT id, org_id, project_id, environment_id, task_session_id, definition_id, name, direction, backend, next_sequence, created_at FROM inserted_session_channel
	    UNION ALL
	    SELECT id, org_id, project_id, environment_id, task_session_id, definition_id, name, direction, backend, next_sequence, created_at FROM existing_session_channel
	),
matching_identity_records AS (
    SELECT channel_records.*
      FROM channel_records
      JOIN selected_channel ON selected_channel.id = channel_records.channel_id
      JOIN input_values ON input_values.idempotency_key <> '' OR input_values.external_event_id <> ''
     WHERE channel_records.org_id = sqlc.arg(org_id)
       AND (
           (
               input_values.idempotency_key <> ''
               AND channel_records.idempotency_key = input_values.idempotency_key
           )
           OR (
               input_values.external_event_id <> ''
               AND channel_records.external_event_id = input_values.external_event_id
           )
       )
),
ambiguous_identity_record AS (
    SELECT 1
      FROM matching_identity_records
     GROUP BY matching_identity_records.channel_id
    HAVING count(DISTINCT matching_identity_records.id) > 1
),
existing_record AS (
    SELECT matching_identity_records.*, false AS inserted
      FROM matching_identity_records
      JOIN input_values ON true
     WHERE matching_identity_records.idempotency_fingerprint = input_values.idempotency_fingerprint
       AND NOT EXISTS (SELECT 1 FROM ambiguous_identity_record)
     ORDER BY matching_identity_records.created_at, matching_identity_records.id
     LIMIT 1
),
conflicting_idempotency_record AS (
    SELECT matching_identity_records.id
      FROM matching_identity_records
      JOIN input_values ON true
     WHERE matching_identity_records.idempotency_fingerprint <> input_values.idempotency_fingerprint
        OR EXISTS (SELECT 1 FROM ambiguous_identity_record)
     LIMIT 1
),
channel_record_count AS (
    SELECT count(*)::bigint AS record_count
      FROM selected_channel
      JOIN channel_records ON channel_records.org_id = sqlc.arg(org_id)
                          AND channel_records.channel_id = selected_channel.id
),
	allocated_existing_channel AS (
	    UPDATE channels
	       SET next_sequence = channels.next_sequence + 1
	      FROM selected_channel
	     WHERE channels.org_id = selected_channel.org_id
	       AND channels.id = selected_channel.id
	       AND NOT EXISTS (SELECT 1 FROM inserted_session_channel)
	       AND NOT EXISTS (SELECT 1 FROM existing_record)
	       AND NOT EXISTS (SELECT 1 FROM conflicting_idempotency_record)
	       AND COALESCE((SELECT record_count FROM channel_record_count), 0) < sqlc.arg(max_inputs_per_channel)::bigint
	    RETURNING channels.*, channels.next_sequence - 1 AS allocated_sequence
	),
	allocated_channel AS (
	    SELECT id, org_id, project_id, environment_id, task_session_id, definition_id, name, direction, backend, next_sequence, created_at, allocated_sequence
	      FROM inserted_session_channel
	     WHERE NOT EXISTS (SELECT 1 FROM existing_record)
	       AND NOT EXISTS (SELECT 1 FROM conflicting_idempotency_record)
	       AND COALESCE((SELECT record_count FROM channel_record_count), 0) < sqlc.arg(max_inputs_per_channel)::bigint
	    UNION ALL
	    SELECT id, org_id, project_id, environment_id, task_session_id, definition_id, name, direction, backend, next_sequence, created_at, allocated_sequence
	      FROM allocated_existing_channel
	),
inserted_record AS (
    INSERT INTO channel_records (
        id,
        org_id,
        project_id,
        environment_id,
        channel_id,
        sequence,
        data,
        correlation_id,
        content_type,
        idempotency_key,
        idempotency_fingerprint,
        external_event_id,
        source,
        auth_subject_type,
        auth_subject_id
    )
    SELECT sqlc.arg(id),
           allocated_channel.org_id,
           allocated_channel.project_id,
           allocated_channel.environment_id,
           allocated_channel.id,
           allocated_channel.allocated_sequence,
           input_values.data,
           input_values.correlation_id,
           input_values.content_type,
           input_values.idempotency_key,
           input_values.idempotency_fingerprint,
           input_values.external_event_id,
           'control',
           sqlc.arg(auth_subject_type)::channel_record_auth_subject_type,
           sqlc.arg(auth_subject_id)
      FROM allocated_channel
      JOIN input_values ON true
    RETURNING channel_records.*, true AS inserted
),
selected_record AS (
    SELECT * FROM existing_record
    UNION ALL
    SELECT * FROM inserted_record
),
event_seq AS (
    INSERT INTO event_subject_cursors (org_id, subject_type, subject_id, last_seq)
    SELECT selected_record.org_id,
           'run',
           target_run.id,
           1
      FROM selected_record
      JOIN target_run ON true
     WHERE selected_record.inserted
    ON CONFLICT (org_id, subject_type, subject_id)
    DO UPDATE SET last_seq = event_subject_cursors.last_seq + 1,
                  updated_at = now()
    RETURNING org_id, subject_type, subject_id, last_seq
),
inserted_event AS (
    INSERT INTO events (org_id, project_id, environment_id, run_id, seq, attempt_id, attempt_number, trace_id, span_id, traceparent, category, severity, source, kind, message, payload, redaction_class, snapshot_version)
    SELECT selected_record.org_id,
           selected_record.project_id,
           selected_record.environment_id,
           target_run.id,
           event_seq.last_seq,
           target_run.current_attempt_id,
           target_run.current_attempt_number,
           target_run.trace_id,
           target_run.root_span_id,
           '00-' || target_run.trace_id || '-' || target_run.root_span_id || '-01',
           'control',
           'info',
           'control',
           'channel.appended',
           'channel.appended',
           jsonb_build_object(
               'channel', selected_channel.name,
               'record_id', selected_record.id,
               'sequence', selected_record.sequence,
               'content_type', selected_record.content_type
           ),
           'sensitive',
           target_run.state_version
      FROM selected_record
      JOIN selected_channel ON selected_channel.id = selected_record.channel_id
      JOIN target_run ON true
      JOIN event_seq ON event_seq.org_id = selected_record.org_id
                    AND event_seq.subject_type = 'run'
                    AND event_seq.subject_id = target_run.id
     WHERE selected_record.inserted
    RETURNING id
),
event_outbox AS (
    INSERT INTO event_outbox (event_record_id, stream_key)
    SELECT inserted_event.id,
           'helmr:events:' || selected_record.org_id::text || ':run:' || target_run.id::text
      FROM inserted_event
      JOIN selected_record ON selected_record.inserted
      JOIN target_run ON true
    RETURNING id
)
SELECT selected_record.id,
       selected_record.org_id,
       selected_record.project_id,
       selected_record.environment_id,
       target_run.id AS run_id,
       selected_channel.id AS channel_id,
       selected_channel.name AS channel,
       selected_record.data,
       selected_record.correlation_id,
       selected_record.sequence,
       selected_record.content_type,
       selected_record.idempotency_key,
       selected_record.idempotency_fingerprint,
       selected_record.external_event_id,
       selected_record.auth_subject_type,
       selected_record.auth_subject_id,
       selected_record.inserted,
       selected_record.created_at
  FROM selected_record
  JOIN selected_channel ON selected_channel.id = selected_record.channel_id
  JOIN target_run ON true;

-- name: GetExistingSessionChannelInputRecord :one
WITH input_values AS (
    SELECT sqlc.arg(channel)::text AS channel,
           COALESCE(sqlc.arg(idempotency_key)::text, '')::text AS idempotency_key,
           COALESCE(sqlc.arg(idempotency_fingerprint)::text, '')::text AS idempotency_fingerprint,
           COALESCE(sqlc.arg(external_event_id)::text, '')::text AS external_event_id
),
selected_channel AS (
    SELECT channels.id,
           channels.name
      FROM channels
      JOIN input_values ON true
     WHERE channels.org_id = sqlc.arg(org_id)
       AND channels.task_session_id = sqlc.arg(task_session_id)
       AND channels.name = input_values.channel
       AND channels.direction = 'input'
     LIMIT 1
),
matching_identity_records AS (
    SELECT channel_records.*
      FROM selected_channel
      JOIN channel_records ON channel_records.org_id = sqlc.arg(org_id)
                          AND channel_records.channel_id = selected_channel.id
      JOIN input_values ON input_values.idempotency_key <> '' OR input_values.external_event_id <> ''
     WHERE (
           (
               input_values.idempotency_key <> ''
               AND channel_records.idempotency_key = input_values.idempotency_key
           )
           OR (
               input_values.external_event_id <> ''
               AND channel_records.external_event_id = input_values.external_event_id
           )
       )
),
ambiguous_identity_record AS (
    SELECT 1
      FROM matching_identity_records
     GROUP BY matching_identity_records.channel_id
    HAVING count(DISTINCT matching_identity_records.id) > 1
)
SELECT matching_identity_records.id,
       matching_identity_records.org_id,
       matching_identity_records.project_id,
       matching_identity_records.environment_id,
       matching_identity_records.channel_id,
       selected_channel.name AS channel,
       matching_identity_records.data,
       matching_identity_records.correlation_id,
       matching_identity_records.sequence,
       matching_identity_records.content_type,
       matching_identity_records.idempotency_key,
       matching_identity_records.idempotency_fingerprint,
       matching_identity_records.external_event_id,
       matching_identity_records.auth_subject_type,
       matching_identity_records.auth_subject_id,
       matching_identity_records.created_at
  FROM matching_identity_records
  JOIN selected_channel ON selected_channel.id = matching_identity_records.channel_id
  JOIN input_values ON true
 WHERE matching_identity_records.idempotency_fingerprint = input_values.idempotency_fingerprint
   AND NOT EXISTS (SELECT 1 FROM ambiguous_identity_record)
 ORDER BY matching_identity_records.created_at, matching_identity_records.id
 LIMIT 1;

-- name: GetSessionChannelInputAppendConflictReason :one
WITH target_session AS (
    SELECT task_sessions.id,
           task_sessions.org_id,
           task_sessions.current_run_id
      FROM task_sessions
     WHERE task_sessions.org_id = sqlc.arg(org_id)
       AND task_sessions.current_run_id = sqlc.arg(run_id)
       AND task_sessions.status = 'open'
),
target_run AS (
    SELECT runs.id,
           runs.task_session_id
      FROM runs
      JOIN target_session ON target_session.org_id = runs.org_id
                         AND target_session.current_run_id = runs.id
     WHERE runs.org_id = sqlc.arg(org_id)
       AND runs.id = sqlc.arg(run_id)
       AND runs.status IN ('queued', 'running', 'waiting')
),
input_values AS (
    SELECT sqlc.arg(channel)::text AS channel,
           COALESCE(sqlc.arg(idempotency_key)::text, '')::text AS idempotency_key,
           COALESCE(sqlc.arg(idempotency_fingerprint)::text, '')::text AS idempotency_fingerprint,
           COALESCE(sqlc.arg(external_event_id)::text, '')::text AS external_event_id
),
selected_channel AS (
    SELECT channels.id
      FROM target_run
      JOIN input_values ON true
      JOIN channels ON channels.org_id = sqlc.arg(org_id)
                   AND channels.task_session_id = target_run.task_session_id
                   AND channels.name = input_values.channel
                   AND channels.direction = 'input'
     LIMIT 1
),
matching_identity_records AS (
    SELECT channel_records.*
      FROM selected_channel
      JOIN channel_records ON channel_records.org_id = sqlc.arg(org_id)
                          AND channel_records.channel_id = selected_channel.id
      JOIN input_values ON input_values.idempotency_key <> '' OR input_values.external_event_id <> ''
     WHERE (
           (
               input_values.idempotency_key <> ''
               AND channel_records.idempotency_key = input_values.idempotency_key
           )
           OR (
               input_values.external_event_id <> ''
               AND channel_records.external_event_id = input_values.external_event_id
           )
       )
),
ambiguous_identity_record AS (
    SELECT 1
      FROM matching_identity_records
     GROUP BY matching_identity_records.channel_id
    HAVING count(DISTINCT matching_identity_records.id) > 1
),
existing_record AS (
    SELECT matching_identity_records.id
      FROM matching_identity_records
      JOIN input_values ON true
     WHERE matching_identity_records.idempotency_fingerprint = input_values.idempotency_fingerprint
       AND NOT EXISTS (SELECT 1 FROM ambiguous_identity_record)
     ORDER BY matching_identity_records.created_at, matching_identity_records.id
     LIMIT 1
),
conflicting_idempotency_record AS (
    SELECT matching_identity_records.id
      FROM matching_identity_records
      JOIN input_values ON true
     WHERE matching_identity_records.idempotency_fingerprint <> input_values.idempotency_fingerprint
        OR EXISTS (SELECT 1 FROM ambiguous_identity_record)
     LIMIT 1
),
channel_record_count AS (
    SELECT count(*)::bigint AS record_count
      FROM selected_channel
      JOIN channel_records ON channel_records.org_id = sqlc.arg(org_id)
                          AND channel_records.channel_id = selected_channel.id
)
SELECT CASE
           WHEN NOT EXISTS (SELECT 1 FROM target_run) THEN 'run_not_accepting'
           WHEN EXISTS (SELECT 1 FROM conflicting_idempotency_record) THEN 'idempotency_conflict'
           WHEN NOT EXISTS (SELECT 1 FROM existing_record)
                AND COALESCE((SELECT record_count FROM channel_record_count), 0) >= sqlc.arg(max_inputs_per_channel)::bigint
                THEN 'input_limit_exceeded'
           ELSE 'unknown'
       END::text AS reason;
-- name: ResolveRunChannelWaitpointsForRun :many
WITH candidate_raw AS (
    SELECT channel_waits.waitpoint_id,
           channel_waits.org_id,
           channel_waits.run_id,
           channel_waits.channel_id,
           channels.name AS channel,
	           channel_waits.created_at,
	           runs.task_session_id,
	           'runtime:input-wait'::text AS consumer_key,
	           next_record.id AS record_id,
	           next_record.sequence,
	           next_record.data,
	           channel_waits.correlation_id AS wait_correlation_id,
	           next_record.correlation_id AS record_correlation_id
      FROM channel_waits
      JOIN channels ON channels.org_id = channel_waits.org_id
                   AND channels.id = channel_waits.channel_id
      JOIN waitpoints ON waitpoints.org_id = channel_waits.org_id
                     AND waitpoints.id = channel_waits.waitpoint_id
      JOIN run_suspensions ON run_suspensions.org_id = channel_waits.org_id
                     AND run_suspensions.id = channel_waits.run_suspension_id
      JOIN runs ON runs.org_id = channel_waits.org_id
               AND runs.id = channel_waits.run_id
      JOIN LATERAL (
          SELECT channel_records.*
            FROM channel_records
           WHERE channel_records.org_id = channel_waits.org_id
             AND channel_records.channel_id = channel_waits.channel_id
             AND channel_records.sequence > channel_waits.after_sequence
             AND (
                 channel_waits.correlation_id = ''
                 OR channel_records.correlation_id = channel_waits.correlation_id
             )
             AND NOT EXISTS (
                 SELECT 1
                   FROM channel_waits matched_wait
                  WHERE matched_wait.org_id = channel_waits.org_id
                    AND matched_wait.channel_id = channel_waits.channel_id
                    AND matched_wait.matched_record_id = channel_records.id
             )
           ORDER BY channel_records.sequence ASC, channel_records.id ASC
           LIMIT 1
      ) next_record ON true
     WHERE channel_waits.org_id = sqlc.arg(org_id)
       AND channel_waits.run_id = sqlc.arg(run_id)
       AND channel_waits.matched_record_id IS NULL
       AND waitpoints.kind = 'channel'
       AND waitpoints.status = 'pending'
       AND run_suspensions.status = 'waiting'
       AND runs.status = 'waiting'
       AND runs.current_run_lease_id IS NULL
       AND NOT EXISTS (
           SELECT 1
             FROM channel_waits earlier_wait
             JOIN waitpoints earlier_waitpoint
               ON earlier_waitpoint.org_id = earlier_wait.org_id
              AND earlier_waitpoint.id = earlier_wait.waitpoint_id
             JOIN run_suspensions earlier_run_suspension
               ON earlier_run_suspension.org_id = earlier_wait.org_id
              AND earlier_run_suspension.id = earlier_wait.run_suspension_id
            WHERE earlier_wait.org_id = channel_waits.org_id
              AND earlier_wait.channel_id = channel_waits.channel_id
              AND earlier_wait.correlation_id = channel_waits.correlation_id
              AND earlier_wait.matched_record_id IS NULL
              AND earlier_waitpoint.kind = 'channel'
              AND earlier_waitpoint.status = 'pending'
              AND earlier_run_suspension.status = 'waiting'
              AND (earlier_wait.created_at, earlier_wait.waitpoint_id) < (channel_waits.created_at, channel_waits.waitpoint_id)
       )
       AND EXISTS (
           SELECT 1
             FROM run_queue_items
            WHERE run_queue_items.org_id = channel_waits.org_id
              AND run_queue_items.run_id = channel_waits.run_id
              AND run_queue_items.status = 'parked'
       )
     ORDER BY channel_waits.created_at ASC, channel_waits.waitpoint_id ASC
     FOR UPDATE OF channel_waits, waitpoints, run_suspensions
),
candidate AS (
    SELECT *
      FROM (
          SELECT candidate_raw.*,
                 row_number() OVER (
                     PARTITION BY candidate_raw.org_id, candidate_raw.channel_id, candidate_raw.record_id
                     ORDER BY candidate_raw.created_at ASC, candidate_raw.waitpoint_id ASC
                 ) AS record_match_rank
            FROM candidate_raw
      ) ranked_candidate
     WHERE record_match_rank = 1
),
matched_wait AS (
    UPDATE channel_waits
       SET matched_record_id = candidate.record_id,
           matched_at = now()
      FROM candidate
     WHERE channel_waits.org_id = candidate.org_id
       AND channel_waits.waitpoint_id = candidate.waitpoint_id
       AND channel_waits.matched_record_id IS NULL
    RETURNING channel_waits.waitpoint_id,
              channel_waits.org_id,
              channel_waits.run_id,
              channel_waits.channel_id,
              candidate.task_session_id,
              candidate.consumer_key,
              candidate.channel,
	              candidate.record_id,
	              candidate.sequence,
	              candidate.data,
	              candidate.wait_correlation_id,
	              candidate.record_correlation_id
),
completed_waitpoint AS (
    UPDATE waitpoints
       SET status = 'completed',
           data = jsonb_build_object(
	               'channel', matched_wait.channel,
	               'sequence', matched_wait.sequence,
	               'correlation_id', matched_wait.record_correlation_id,
	               'data', matched_wait.data
           ),
           error = NULL,
           resolved_at = now(),
           updated_at = now()
      FROM matched_wait
     WHERE waitpoints.org_id = matched_wait.org_id
       AND waitpoints.id = matched_wait.waitpoint_id
       AND waitpoints.status = 'pending'
    RETURNING waitpoints.id, waitpoints.org_id, waitpoints.run_id
)
SELECT *
  FROM completed_waitpoint;
