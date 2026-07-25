-- name: AppendReceiptRunLogChunk :one
WITH event_args AS (
    SELECT sqlc.arg(kind)::text AS event_kind,
           sqlc.arg(payload)::jsonb AS event_payload,
           sqlc.arg(severity)::text AS event_severity
),
current_run_lease AS (
    SELECT runs.org_id,
           runs.project_id,
           runs.environment_id,
           runs.trace_id,
           runs.state_version,
           runs.id,
           run_leases.id AS run_lease_id,
           run_leases.span_id,
           run_leases.parent_span_id,
           run_leases.traceparent,
           run_leases.attempt_number AS attempt_number
      FROM run_leases
      JOIN runs ON runs.id = run_leases.run_id
               AND runs.org_id = run_leases.org_id
               AND runs.project_id = run_leases.project_id
               AND runs.environment_id = run_leases.environment_id
               AND runs.workspace_id = run_leases.workspace_id
      JOIN run_attempts ON run_attempts.run_id = run_leases.run_id
                       AND run_attempts.number = run_leases.attempt_number
                       AND run_attempts.workspace_id = run_leases.workspace_id
      JOIN worker_groups ON worker_groups.id = run_leases.worker_group_id
                        AND worker_groups.region_id = run_leases.region_id
      JOIN worker_instances ON worker_instances.id = run_leases.worker_instance_id
                           AND worker_instances.worker_group_id = run_leases.worker_group_id
      JOIN workspaces ON workspaces.id = run_leases.workspace_id
                     AND workspaces.org_id = run_leases.org_id
                     AND workspaces.project_id = run_leases.project_id
                     AND workspaces.environment_id = run_leases.environment_id
                     AND workspaces.region_id = run_leases.region_id
      JOIN runtime_instances ON runtime_instances.id = run_leases.runtime_instance_id
                            AND runtime_instances.org_id = run_leases.org_id
                            AND runtime_instances.project_id = run_leases.project_id
                            AND runtime_instances.environment_id = run_leases.environment_id
                            AND runtime_instances.region_id = run_leases.region_id
                            AND runtime_instances.worker_group_id = run_leases.worker_group_id
                            AND runtime_instances.worker_instance_id = run_leases.worker_instance_id
                            AND runtime_instances.worker_epoch = run_leases.worker_epoch
                            AND runtime_instances.workspace_id = run_leases.workspace_id
      JOIN worker_network_slots ON worker_network_slots.id = run_leases.network_slot_id
                               AND worker_network_slots.generation = run_leases.network_slot_generation
                               AND worker_network_slots.worker_group_id = run_leases.worker_group_id
                               AND worker_network_slots.worker_instance_id = run_leases.worker_instance_id
                               AND worker_network_slots.worker_epoch = run_leases.worker_epoch
                               AND worker_network_slots.runtime_instance_id = run_leases.runtime_instance_id
      JOIN workspace_mounts ON workspace_mounts.id = sqlc.arg(workspace_mount_id)
                           AND workspace_mounts.org_id = run_leases.org_id
                           AND workspace_mounts.project_id = run_leases.project_id
                           AND workspace_mounts.environment_id = run_leases.environment_id
                           AND workspace_mounts.region_id = run_leases.region_id
                           AND workspace_mounts.worker_group_id = run_leases.worker_group_id
                           AND workspace_mounts.worker_instance_id = run_leases.worker_instance_id
                           AND workspace_mounts.worker_epoch = run_leases.worker_epoch
                           AND workspace_mounts.runtime_instance_id = run_leases.runtime_instance_id
                           AND workspace_mounts.workspace_id = run_leases.workspace_id
      JOIN workspace_leases ON workspace_leases.id = sqlc.arg(workspace_lease_id)
                           AND workspace_leases.org_id = run_leases.org_id
                           AND workspace_leases.project_id = run_leases.project_id
                           AND workspace_leases.environment_id = run_leases.environment_id
                           AND workspace_leases.region_id = run_leases.region_id
                           AND workspace_leases.worker_group_id = run_leases.worker_group_id
                           AND workspace_leases.worker_instance_id = run_leases.worker_instance_id
                           AND workspace_leases.worker_epoch = run_leases.worker_epoch
                           AND workspace_leases.runtime_instance_id = run_leases.runtime_instance_id
                           AND workspace_leases.workspace_id = run_leases.workspace_id
                           AND workspace_leases.workspace_mount_id = workspace_mounts.id
                           AND workspace_leases.owner_run_lease_id = run_leases.id
     WHERE run_leases.id = sqlc.arg(run_lease_id)
       AND run_leases.run_id = sqlc.arg(run_id)
       AND run_leases.attempt_number = sqlc.arg(attempt_number)
       AND run_leases.lease_sequence = sqlc.arg(lease_sequence)
       AND run_leases.worker_group_id = sqlc.arg(worker_group_id)
       AND run_leases.worker_instance_id = sqlc.arg(worker_instance_id)
       AND run_leases.worker_epoch = sqlc.arg(worker_epoch)
       AND run_leases.worker_protocol_version = sqlc.arg(worker_protocol_version)
       AND run_leases.runtime_instance_id = sqlc.arg(runtime_instance_id)
       AND run_leases.runtime_identity_id = sqlc.arg(runtime_identity_id)
       AND run_leases.network_slot_id = sqlc.arg(network_slot_id)
       AND run_leases.network_slot_generation = sqlc.arg(network_slot_generation)
       AND run_leases.workspace_id = sqlc.arg(workspace_id)
       AND workspace_mounts.materialized_version_id = sqlc.arg(base_workspace_version_id)
       AND workspace_mounts.fencing_generation = sqlc.arg(mount_fencing_generation)
       AND workspace_leases.base_version_id = sqlc.arg(base_workspace_version_id)
       AND workspace_leases.ownership_generation = sqlc.arg(ownership_generation)
       AND workspace_leases.writer_generation = sqlc.arg(writer_generation)
       AND workspace_leases.mount_fencing_generation = sqlc.arg(mount_fencing_generation)
       AND run_leases.requested_cpu_millis = sqlc.arg(requested_cpu_millis)
       AND run_leases.requested_memory_bytes = sqlc.arg(requested_memory_bytes)
       AND run_leases.requested_workload_disk_bytes = sqlc.arg(requested_workload_disk_bytes)
       AND run_leases.requested_scratch_bytes = sqlc.arg(requested_scratch_bytes)
       AND run_leases.requested_execution_slots = sqlc.arg(requested_execution_slots)
       AND runs.max_active_duration_ms = sqlc.arg(max_active_duration_ms)
       AND runs.active_elapsed_ms = sqlc.arg(active_elapsed_ms)
       AND COALESCE(run_leases.trace_id, '') = sqlc.arg(trace_id)
       AND COALESCE(run_leases.span_id, '') = sqlc.arg(span_id)
       AND COALESCE(run_leases.traceparent, '') = sqlc.arg(traceparent)
       AND run_leases.start_deadline_at = sqlc.arg(start_deadline_at)
       AND run_leases.expires_at = sqlc.arg(expires_at)
       AND runs.current_run_lease_id = run_leases.id
       AND runs.current_attempt_number = run_leases.attempt_number
       AND runs.status = 'running'
       AND run_attempts.terminal_at IS NULL
       AND worker_groups.state IN ('active', 'draining')
       AND worker_groups.allows_run
       AND worker_groups.protocol_version = run_leases.worker_protocol_version
       AND worker_instances.current_epoch = run_leases.worker_epoch
       AND worker_instances.state IN ('active', 'draining')
       AND worker_instances.supports_run
       AND worker_instances.protocol_version = run_leases.worker_protocol_version
       AND worker_instances.runtime_identity_id = run_leases.runtime_identity_id
       AND worker_instances.per_vm_cpu_millis = run_leases.requested_cpu_millis
       AND worker_instances.per_vm_memory_bytes = run_leases.requested_memory_bytes
       AND worker_instances.per_vm_workload_disk_bytes = run_leases.requested_workload_disk_bytes
       AND worker_instances.per_vm_scratch_bytes = run_leases.requested_scratch_bytes
       AND workspaces.state = 'active'
       AND workspaces.desired_state = 'active'
       AND workspaces.ownership_generation = workspace_leases.ownership_generation
       AND workspaces.writer_generation = workspace_leases.writer_generation
       AND runtime_instances.runtime_identity_id = run_leases.runtime_identity_id
       AND runtime_instances.program_deployment_id = runs.deployment_id
       AND runtime_instances.deployment_definition_id = workspaces.deployment_definition_id
       AND runtime_instances.desired_state = 'ready'
       AND runtime_instances.observed_state = 'ready'
       AND runtime_instances.observed_desired_version = runtime_instances.desired_version
       AND runtime_instances.reclaimed_at IS NULL
       AND runtime_instances.reserved_run_id IS NULL
       AND runtime_instances.reserved_attempt_number IS NULL
       AND runtime_instances.reserved_process_id IS NULL
       AND runtime_instances.reserved_workspace_version_id IS NULL
       AND runtime_instances.reservation_expires_at IS NULL
       AND runtime_instances.reserved_cpu_millis = run_leases.requested_cpu_millis
       AND runtime_instances.reserved_memory_bytes = run_leases.requested_memory_bytes
       AND runtime_instances.reserved_workload_disk_bytes = run_leases.requested_workload_disk_bytes
       AND runtime_instances.reserved_scratch_bytes = run_leases.requested_scratch_bytes
       AND runtime_instances.reserved_execution_slots = run_leases.requested_execution_slots
       AND worker_network_slots.state = 'bound'
       AND workspace_mounts.state = 'mounted'
       AND workspace_leases.state = 'active'
       AND workspace_leases.expires_at > now()
       AND run_leases.state = 'running'
       AND run_leases.expires_at > now()
),
candidate AS (
    SELECT current_run_lease.*,
           sqlc.arg(stream)::text AS stream,
           sqlc.arg(observed_seq)::bigint AS observed_seq,
           sqlc.arg(content)::bytea AS content,
           octet_length(sqlc.arg(content)::bytea)::bigint AS size_bytes,
           event_args.event_kind,
           event_args.event_payload,
           'run_log:' || current_run_lease.id::text || ':' || current_run_lease.attempt_number::text || ':' || sqlc.arg(stream)::text || ':' || (sqlc.arg(observed_seq)::bigint)::text AS idempotency_key
      FROM current_run_lease
      CROSS JOIN event_args
),
inserted_chunk AS (
    INSERT INTO telemetry_outbox (
        org_id, stream_kind, source_kind, source_id, stream_name,
        idempotency_key, project_id, environment_id, run_id, run_lease_id, attempt_number,
        trace_id, span_id, parent_span_id, traceparent, source, kind, message, payload,
        content, size_bytes, observed_seq, redaction_class, retention_class, observed_at
    )
    SELECT candidate.org_id,
           'run_log',
           'run',
           candidate.id,
           candidate.stream::text,
           candidate.idempotency_key,
           candidate.project_id,
           candidate.environment_id,
           candidate.id,
           candidate.run_lease_id,
           candidate.attempt_number,
           candidate.trace_id,
           candidate.span_id,
           candidate.parent_span_id,
           candidate.traceparent,
           'worker',
           'run.log',
           'run.log',
           jsonb_build_object(
               'stream', candidate.stream,
               'observed_seq', candidate.observed_seq,
               'bytes', candidate.size_bytes,
               'event_kind', candidate.event_kind,
               'event_payload', candidate.event_payload
           ),
           candidate.content,
           candidate.size_bytes,
           candidate.observed_seq,
           'standard',
           'standard',
           now()
      FROM candidate
    ON CONFLICT (org_id, stream_kind, source_kind, source_id, stream_name, idempotency_key)
    DO UPDATE SET content = telemetry_outbox.content
     WHERE telemetry_outbox.content IS NOT DISTINCT FROM excluded.content
       AND telemetry_outbox.size_bytes = excluded.size_bytes
       AND telemetry_outbox.payload = excluded.payload
    RETURNING telemetry_outbox.org_id,
              telemetry_outbox.run_id,
              telemetry_outbox.run_lease_id,
              telemetry_outbox.attempt_number,
              telemetry_outbox.stream_name AS stream,
              telemetry_outbox.id AS seq,
              telemetry_outbox.observed_seq,
              telemetry_outbox.content,
              telemetry_outbox.size_bytes,
              telemetry_outbox.created_at,
              (xmax = 0) AS is_new,
              true AS replay_matches
),
selected_chunk AS (
    SELECT * FROM inserted_chunk
),
event_input AS (
    SELECT current_run_lease.org_id,
           current_run_lease.project_id,
           current_run_lease.environment_id,
           selected_chunk.run_id,
           selected_chunk.run_lease_id,
           selected_chunk.attempt_number,
           current_run_lease.trace_id,
           current_run_lease.span_id,
           current_run_lease.parent_span_id,
           current_run_lease.traceparent,
           'log' AS category,
           event_args.event_severity AS severity,
           'worker' AS source,
           event_args.event_kind AS kind,
           event_args.event_kind AS message,
           event_args.event_payload AS payload,
           'sensitive' AS redaction_class,
           current_run_lease.state_version AS snapshot_version
      FROM selected_chunk
      JOIN current_run_lease ON current_run_lease.org_id = selected_chunk.org_id
                            AND current_run_lease.id = selected_chunk.run_id
      CROSS JOIN event_args
     WHERE selected_chunk.is_new
),
event AS (
    INSERT INTO telemetry_outbox (
        org_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, run_lease_id, attempt_number, trace_id, span_id,
        parent_span_id, traceparent, category, severity, source, kind, message,
        payload, redaction_class, snapshot_version, observed_at
    )
    SELECT event_input.org_id,
           'event',
           'run',
           event_input.run_id,
           event_input.project_id,
           event_input.environment_id,
           event_input.run_id,
           event_input.run_lease_id,
           event_input.attempt_number,
           event_input.trace_id,
           event_input.span_id,
           event_input.parent_span_id,
           event_input.traceparent,
           event_input.category,
           event_input.severity,
           event_input.source,
           event_input.kind,
           event_input.message,
           event_input.payload,
           event_input.redaction_class,
           event_input.snapshot_version,
           now()
      FROM event_input
    RETURNING id
),
meter_event AS (
    INSERT INTO meter_events (
        org_id, project_id, environment_id, run_id, run_lease_id,
        attempt_number, trace_id, span_id, meter, quantity, unit, details,
        idempotency_key, idempotency_fingerprint
    )
    SELECT current_run_lease.org_id,
           current_run_lease.project_id,
           current_run_lease.environment_id,
           selected_chunk.run_id,
           selected_chunk.run_lease_id,
           current_run_lease.attempt_number,
           current_run_lease.trace_id,
           current_run_lease.span_id,
           'log_bytes',
           selected_chunk.size_bytes,
           'bytes',
           jsonb_build_object('stream', selected_chunk.stream, 'observed_seq', selected_chunk.observed_seq),
           'log:' || selected_chunk.run_id::text || ':' || selected_chunk.attempt_number::text || ':' || selected_chunk.stream::text || ':' || selected_chunk.observed_seq::text,
           jsonb_build_object(
               'quantity', selected_chunk.size_bytes,
               'unit', 'bytes',
               'details', jsonb_build_object('stream', selected_chunk.stream, 'observed_seq', selected_chunk.observed_seq)
           )::text
      FROM selected_chunk
      JOIN current_run_lease ON current_run_lease.org_id = selected_chunk.org_id
                            AND current_run_lease.id = selected_chunk.run_id
     WHERE selected_chunk.is_new
       AND selected_chunk.size_bytes > 0
    ON CONFLICT (org_id, source_type, source_id, meter, idempotency_key)
    DO UPDATE SET idempotency_fingerprint = meter_events.idempotency_fingerprint
     WHERE meter_events.idempotency_fingerprint = excluded.idempotency_fingerprint
    RETURNING *
),
meter_event_outbox AS (
    INSERT INTO telemetry_outbox (
        org_id, stream_kind, source_kind, source_id, project_id,
        environment_id, run_id, run_lease_id, meter_event_id, attempt_number,
        trace_id, span_id, kind, payload, idempotency_key, observed_at
    )
    SELECT meter_event.org_id,
           'meter_event',
           meter_event.source_type,
           meter_event.source_id,
           meter_event.project_id,
           meter_event.environment_id,
           meter_event.run_id,
           meter_event.run_lease_id,
           meter_event.id,
           meter_event.attempt_number,
           meter_event.trace_id,
           meter_event.span_id,
           meter_event.meter,
           meter_event.details,
           meter_event.idempotency_key,
           meter_event.occurred_at
      FROM meter_event
    ON CONFLICT DO NOTHING
    RETURNING id
)
SELECT selected_chunk.org_id,
       selected_chunk.run_id,
       selected_chunk.run_lease_id,
       selected_chunk.attempt_number,
       selected_chunk.stream,
       selected_chunk.seq,
       selected_chunk.observed_seq,
       selected_chunk.content,
       selected_chunk.size_bytes,
       selected_chunk.created_at,
       selected_chunk.replay_matches
 FROM selected_chunk
 WHERE (SELECT count(*) FROM event) >= 0
   AND (
       NOT selected_chunk.is_new
       OR selected_chunk.size_bytes = 0
       OR EXISTS (SELECT 1 FROM meter_event_outbox)
   );
