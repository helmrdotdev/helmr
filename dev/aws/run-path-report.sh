#!/usr/bin/env bash
set -euo pipefail

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  cat <<'EOF'
usage: dev/aws/run-path-report.sh RUN_ID

Print AWS dev DB evidence for classifying a run's runtime path:
resident live wait, checkpoint park/restore, prepared runtime claim, or cold
workspace_mount.

By default this script requires HELMR_DATABASE_URL or DATABASE_URL and runs
psql locally. For AWS dev private-network queries, set
HELMR_PATH_REPORT_ALLOW_ECS_TASK=1 to opt into dev/aws/db-query.sh. That path
does not mutate Helmr product data, but it does create AWS ECS task/log records.
EOF
  exit 0
fi
if [ "$#" -ne 1 ]; then
  cat >&2 <<'EOF'
usage: dev/aws/run-path-report.sh RUN_ID
EOF
  exit 2
fi

run_id=$1
case "${run_id}" in
  ????????-????-????-????-????????????) ;;
  *) echo "RUN_ID must be a UUID" >&2; exit 2 ;;
esac
if ! printf '%s' "${run_id}" | grep -Eq '^[0-9a-fA-F-]{36}$'; then
  echo "RUN_ID must be a UUID" >&2
  exit 2
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

write_pg_service_file() {
  local service_file=$1
  HELMR_PATH_REPORT_DATABASE_URL="${database_url}" python3 - "${service_file}" <<'PY'
import os
import re
import sys
from urllib.parse import unquote, urlparse

service_file = sys.argv[1]
raw = os.environ["HELMR_PATH_REPORT_DATABASE_URL"]
try:
    parsed = urlparse(raw)
    hostname = parsed.hostname or ""
    port = parsed.port
except ValueError as exc:
    raise SystemExit(f"invalid database URL: {exc}") from exc
if parsed.scheme not in ("postgres", "postgresql"):
    raise SystemExit("database URL must use postgres:// or postgresql://")

valid_key_re = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
query_params = []
overrides = set()
for pair in parsed.query.split("&"):
    if pair == "":
        continue
    key, separator, value = pair.partition("=")
    if separator == "":
        value = ""
    decoded_key = unquote(key)
    decoded_value = unquote(value)
    if not valid_key_re.match(decoded_key):
        raise SystemExit(f"invalid DATABASE_URL query parameter: {decoded_key}")
    if "\n" in decoded_value:
        raise SystemExit(f"DATABASE_URL query parameter {decoded_key} contains a newline")
    if decoded_key in ("host", "port", "user", "password", "dbname"):
        overrides.add(decoded_key)
    query_params.append((decoded_key, decoded_value))

def base_param(name, value):
    if name in overrides or value == "":
        return None
    if "\n" in value:
        raise SystemExit(f"DATABASE_URL query parameter {name} contains a newline")
    return (name, value)

params = [
    base_param("host", hostname),
    base_param("port", "" if port is None else str(port)),
    base_param("dbname", unquote(parsed.path.lstrip("/"))),
    base_param("user", unquote(parsed.username or "")),
    base_param("password", unquote(parsed.password or "")),
]
params = [param for param in params if param is not None]

with open(service_file, "w", encoding="utf-8") as handle:
    handle.write("[helmr_path_report]\n")
    for key, value in params + query_params:
        handle.write(f"{key}={value}\n")
PY
}

sql="$(cat <<SQL
\\pset null '[null]'
\\timing off

DO \$\$
BEGIN
  IF NOT EXISTS (
      SELECT 1
        FROM runs
       WHERE id = '${run_id}'::uuid
  ) THEN
    RAISE EXCEPTION 'run ${run_id} not found';
  END IF;
END
\$\$;

WITH target AS MATERIALIZED (
    SELECT *
      FROM runs
     WHERE id = '${run_id}'::uuid
)
SELECT 'run' AS section,
       runs.id,
       runs.task_id,
       runs.status,
       runs.execution_status,
       runs.workspace_id,
       runs.workspace_mount_id,
       runs.latest_runtime_checkpoint_id,
       runs.created_at,
       runs.started_at,
       runs.finished_at,
       CASE
         WHEN runs.started_at IS NOT NULL
          AND runs.finished_at IS NOT NULL
         THEN round(extract(epoch FROM runs.finished_at - runs.started_at) * 1000)::bigint
       END AS started_to_finished_ms
  FROM target AS runs;

WITH target AS MATERIALIZED (
    SELECT *
      FROM runs
     WHERE id = '${run_id}'::uuid
)
SELECT 'wait' AS section,
       run_waits.id,
       run_waits.kind,
       run_waits.state,
       run_waits.owner_runtime_instance_id,
       run_waits.owner_worker_instance_id,
       run_waits.owner_runtime_instance_id IS NOT NULL AS had_runtime_owner,
       run_waits.runtime_checkpoint_id IS NOT NULL AS has_runtime_checkpoint,
       run_waits.live_wait_started_at,
       run_waits.runtime_checkpoint_due_at,
       run_waits.runtime_checkpoint_started_at,
       run_waits.resolved_at,
       run_waits.resumed_at,
       CASE
         WHEN run_waits.live_wait_started_at IS NOT NULL
          AND run_waits.runtime_checkpoint_started_at IS NOT NULL
         THEN round(extract(epoch FROM run_waits.runtime_checkpoint_started_at - run_waits.live_wait_started_at) * 1000)::bigint
       END AS live_to_checkpoint_start_ms,
       CASE
         WHEN run_waits.resolved_at IS NOT NULL
          AND run_waits.resumed_at IS NOT NULL
         THEN round(extract(epoch FROM run_waits.resumed_at - run_waits.resolved_at) * 1000)::bigint
       END AS resolved_to_resumed_ms
  FROM target
  JOIN run_waits ON run_waits.org_id = target.org_id
                AND run_waits.run_id = target.id
 ORDER BY run_waits.created_at, run_waits.id;

WITH target AS MATERIALIZED (
    SELECT *
      FROM runs
     WHERE id = '${run_id}'::uuid
)
SELECT 'checkpoint' AS section,
       runtime_checkpoints.id,
       runtime_checkpoints.state,
       runtime_checkpoints.workspace_mount_id,
       runtime_checkpoints.base_workspace_version_id,
       runtime_checkpoints.created_at,
       runtime_checkpoints.ready_at,
       runtime_checkpoints.invalidated_at,
       CASE
         WHEN runtime_checkpoints.created_at IS NOT NULL
          AND runtime_checkpoints.ready_at IS NOT NULL
         THEN round(extract(epoch FROM runtime_checkpoints.ready_at - runtime_checkpoints.created_at) * 1000)::bigint
       END AS checkpoint_create_to_ready_ms
  FROM target
  JOIN runtime_checkpoints ON runtime_checkpoints.org_id = target.org_id
                          AND runtime_checkpoints.run_id = target.id
 ORDER BY runtime_checkpoints.created_at, runtime_checkpoints.id;

WITH target AS MATERIALIZED (
    SELECT *
      FROM runs
     WHERE id = '${run_id}'::uuid
),
telemetry AS MATERIALIZED (
    SELECT substring(events.payload ->> 'message' FROM '^checkpoint_storage_telemetry (.*)$')::jsonb AS data
      FROM target
      JOIN events ON events.org_id = target.org_id
                 AND events.run_id = target.id
     WHERE events.kind = 'log'
       AND events.payload ->> 'message' LIKE 'checkpoint_storage_telemetry %'
)
SELECT 'checkpoint_storage_telemetry' AS section,
       telemetry.data ->> 'run_wait_id' AS run_wait_id,
       telemetry.data ->> 'checkpoint_id' AS checkpoint_id,
       (telemetry.data -> 'workspace' ->> 'apparent_bytes')::bigint AS workspace_apparent_bytes,
       (telemetry.data -> 'workspace' ->> 'allocated_bytes')::bigint AS workspace_allocated_bytes,
       (telemetry.data -> 'workspace' ->> 'entries')::bigint AS workspace_entries,
       (telemetry.data -> 'image_root' ->> 'apparent_bytes')::bigint AS image_root_apparent_bytes,
       (telemetry.data -> 'image_root' ->> 'allocated_bytes')::bigint AS image_root_allocated_bytes,
       (telemetry.data ->> 'image_root_within_guestd_temp')::boolean AS image_root_within_guestd_temp,
       (telemetry.data ->> 'workspace_within_guestd_temp')::boolean AS workspace_within_guestd_temp,
       (telemetry.data ->> 'image_root_excluding_workspace_apparent_bytes')::bigint AS image_root_excluding_workspace_apparent_bytes,
       (telemetry.data ->> 'image_root_excluding_workspace_allocated_bytes')::bigint AS image_root_excluding_workspace_allocated_bytes,
       (telemetry.data -> 'guestd_temp' ->> 'apparent_bytes')::bigint AS guestd_temp_apparent_bytes,
       (telemetry.data -> 'guestd_temp' ->> 'allocated_bytes')::bigint AS guestd_temp_allocated_bytes,
       (telemetry.data ->> 'guestd_temp_excluding_image_root_apparent_bytes')::bigint AS guestd_temp_excluding_image_root_apparent_bytes,
       (telemetry.data ->> 'guestd_temp_excluding_image_root_allocated_bytes')::bigint AS guestd_temp_excluding_image_root_allocated_bytes
  FROM telemetry;

WITH target AS MATERIALIZED (
    SELECT *
      FROM runs
     WHERE id = '${run_id}'::uuid
)
SELECT 'checkpoint_restore' AS section,
       runtime_checkpoint_restores.id,
       runtime_checkpoint_restores.runtime_checkpoint_id,
       runtime_checkpoint_restores.run_wait_id,
       runtime_checkpoint_restores.run_lease_id,
       runtime_checkpoint_restores.worker_instance_id,
       runtime_checkpoint_restores.status,
       runtime_checkpoint_restores.started_at,
       runtime_checkpoint_restores.acknowledged_at,
       runtime_checkpoint_restores.finished_at,
       CASE
         WHEN runtime_checkpoint_restores.started_at IS NOT NULL
          AND runtime_checkpoint_restores.acknowledged_at IS NOT NULL
         THEN round(extract(epoch FROM runtime_checkpoint_restores.acknowledged_at - runtime_checkpoint_restores.started_at) * 1000)::bigint
       END AS restore_start_to_ack_ms,
       CASE
         WHEN runtime_checkpoint_restores.started_at IS NOT NULL
          AND runtime_checkpoint_restores.finished_at IS NOT NULL
         THEN round(extract(epoch FROM runtime_checkpoint_restores.finished_at - runtime_checkpoint_restores.started_at) * 1000)::bigint
       END AS restore_start_to_finished_ms,
       left(coalesce(runtime_checkpoint_restores.error_message, ''), 200) AS error_message
  FROM target
  JOIN runtime_checkpoint_restores
    ON runtime_checkpoint_restores.org_id = target.org_id
   AND runtime_checkpoint_restores.run_id = target.id
 ORDER BY runtime_checkpoint_restores.started_at, runtime_checkpoint_restores.id;

WITH target AS MATERIALIZED (
    SELECT *
      FROM runs
     WHERE id = '${run_id}'::uuid
),
runtime_checkpoint_phases AS (
    SELECT runtime_checkpoints.id AS runtime_checkpoint_id,
           (phase.ordinality - 1)::int AS ordinal,
           phase.value->>'name' AS name,
           phase.value->>'role' AS role,
           phase.value->>'media_type' AS media_type,
           CASE WHEN NULLIF(phase.value->>'duration_ms', '') ~ '^-?[0-9]+$'
                THEN (phase.value->>'duration_ms')::bigint
           END AS duration_ms,
           phase.value->>'error_class' AS error_class,
           CASE WHEN NULLIF(phase.value->'filepack'->>'logical_bytes', '') ~ '^-?[0-9]+$'
                THEN (phase.value->'filepack'->>'logical_bytes')::bigint
           END AS filepack_logical_bytes,
           CASE WHEN NULLIF(phase.value->'filepack'->>'allocated_bytes', '') ~ '^-?[0-9]+$'
                THEN (phase.value->'filepack'->>'allocated_bytes')::bigint
           END AS filepack_allocated_bytes,
           CASE WHEN lower(NULLIF(phase.value->'filepack'->>'sparse_supported', '')) IN ('true', 'false')
                THEN (phase.value->'filepack'->>'sparse_supported')::boolean
           END AS filepack_sparse_supported,
           CASE WHEN NULLIF(phase.value->'filepack'->>'sparse_data_ranges', '') ~ '^-?[0-9]+$'
                THEN (phase.value->'filepack'->>'sparse_data_ranges')::bigint
           END AS filepack_sparse_data_ranges,
           CASE WHEN NULLIF(phase.value->'filepack'->>'sparse_data_bytes', '') ~ '^-?[0-9]+$'
                THEN (phase.value->'filepack'->>'sparse_data_bytes')::bigint
           END AS filepack_sparse_data_bytes,
           CASE WHEN NULLIF(phase.value->'filepack'->>'zero_chunks_skipped', '') ~ '^-?[0-9]+$'
                THEN (phase.value->'filepack'->>'zero_chunks_skipped')::bigint
           END AS filepack_zero_chunks_skipped,
           CASE WHEN NULLIF(phase.value->'filepack'->>'encoded_chunks', '') ~ '^-?[0-9]+$'
                THEN (phase.value->'filepack'->>'encoded_chunks')::bigint
           END AS filepack_encoded_chunks,
           CASE WHEN NULLIF(phase.value->'filepack'->>'compressed_bytes', '') ~ '^-?[0-9]+$'
                THEN (phase.value->'filepack'->>'compressed_bytes')::bigint
           END AS filepack_compressed_bytes,
           CASE WHEN NULLIF(phase.value->'filepack'->>'unpack_written_bytes', '') ~ '^-?[0-9]+$'
                THEN (phase.value->'filepack'->>'unpack_written_bytes')::bigint
           END AS filepack_unpack_written_bytes
      FROM target
      JOIN runtime_checkpoints
        ON runtime_checkpoints.org_id = target.org_id
       AND runtime_checkpoints.run_id = target.id
      CROSS JOIN LATERAL jsonb_array_elements(
          CASE
            WHEN jsonb_typeof(runtime_checkpoints.manifest->'phases') = 'array'
            THEN runtime_checkpoints.manifest->'phases'
            ELSE '[]'::jsonb
          END
      ) WITH ORDINALITY AS phase(value, ordinality)
)
SELECT 'checkpoint_phase' AS section,
       runtime_checkpoint_phases.runtime_checkpoint_id,
       runtime_checkpoint_phases.ordinal,
       runtime_checkpoint_phases.name,
       runtime_checkpoint_phases.role,
       runtime_checkpoint_phases.media_type,
       runtime_checkpoint_phases.duration_ms,
       runtime_checkpoint_phases.error_class,
       runtime_checkpoint_phases.filepack_logical_bytes,
       runtime_checkpoint_phases.filepack_allocated_bytes,
       runtime_checkpoint_phases.filepack_sparse_supported,
       runtime_checkpoint_phases.filepack_sparse_data_ranges,
       runtime_checkpoint_phases.filepack_sparse_data_bytes,
       runtime_checkpoint_phases.filepack_zero_chunks_skipped,
       runtime_checkpoint_phases.filepack_encoded_chunks,
       runtime_checkpoint_phases.filepack_compressed_bytes,
       runtime_checkpoint_phases.filepack_unpack_written_bytes
  FROM runtime_checkpoint_phases
 ORDER BY runtime_checkpoint_phases.runtime_checkpoint_id,
          runtime_checkpoint_phases.ordinal;

WITH target AS MATERIALIZED (
    SELECT *
      FROM runs
     WHERE id = '${run_id}'::uuid
),
runtime_checkpoint_restore_phases AS (
    SELECT runtime_checkpoint_restores.id AS runtime_checkpoint_restore_id,
           (phase.ordinality - 1)::int AS ordinal,
           phase.value->>'name' AS name,
           phase.value->>'role' AS role,
           phase.value->>'media_type' AS media_type,
           CASE WHEN NULLIF(phase.value->>'duration_ms', '') ~ '^-?[0-9]+$'
                THEN (phase.value->>'duration_ms')::bigint
           END AS duration_ms,
           phase.value->>'error_class' AS error_class,
           CASE WHEN NULLIF(phase.value->'filepack'->>'logical_bytes', '') ~ '^-?[0-9]+$'
                THEN (phase.value->'filepack'->>'logical_bytes')::bigint
           END AS filepack_logical_bytes,
           CASE WHEN NULLIF(phase.value->'filepack'->>'allocated_bytes', '') ~ '^-?[0-9]+$'
                THEN (phase.value->'filepack'->>'allocated_bytes')::bigint
           END AS filepack_allocated_bytes,
           CASE WHEN lower(NULLIF(phase.value->'filepack'->>'sparse_supported', '')) IN ('true', 'false')
                THEN (phase.value->'filepack'->>'sparse_supported')::boolean
           END AS filepack_sparse_supported,
           CASE WHEN NULLIF(phase.value->'filepack'->>'sparse_data_ranges', '') ~ '^-?[0-9]+$'
                THEN (phase.value->'filepack'->>'sparse_data_ranges')::bigint
           END AS filepack_sparse_data_ranges,
           CASE WHEN NULLIF(phase.value->'filepack'->>'sparse_data_bytes', '') ~ '^-?[0-9]+$'
                THEN (phase.value->'filepack'->>'sparse_data_bytes')::bigint
           END AS filepack_sparse_data_bytes,
           CASE WHEN NULLIF(phase.value->'filepack'->>'zero_chunks_skipped', '') ~ '^-?[0-9]+$'
                THEN (phase.value->'filepack'->>'zero_chunks_skipped')::bigint
           END AS filepack_zero_chunks_skipped,
           CASE WHEN NULLIF(phase.value->'filepack'->>'encoded_chunks', '') ~ '^-?[0-9]+$'
                THEN (phase.value->'filepack'->>'encoded_chunks')::bigint
           END AS filepack_encoded_chunks,
           CASE WHEN NULLIF(phase.value->'filepack'->>'compressed_bytes', '') ~ '^-?[0-9]+$'
                THEN (phase.value->'filepack'->>'compressed_bytes')::bigint
           END AS filepack_compressed_bytes,
           CASE WHEN NULLIF(phase.value->'filepack'->>'unpack_written_bytes', '') ~ '^-?[0-9]+$'
                THEN (phase.value->'filepack'->>'unpack_written_bytes')::bigint
           END AS filepack_unpack_written_bytes
      FROM target
      JOIN runtime_checkpoint_restores
        ON runtime_checkpoint_restores.org_id = target.org_id
       AND runtime_checkpoint_restores.run_id = target.id
      CROSS JOIN LATERAL jsonb_array_elements(
          CASE
            WHEN jsonb_typeof(runtime_checkpoint_restores.phases) = 'array'
            THEN runtime_checkpoint_restores.phases
            ELSE '[]'::jsonb
          END
      ) WITH ORDINALITY AS phase(value, ordinality)
)
SELECT 'checkpoint_restore_phase' AS section,
       runtime_checkpoint_restore_phases.runtime_checkpoint_restore_id,
       runtime_checkpoint_restore_phases.ordinal,
       runtime_checkpoint_restore_phases.name,
       runtime_checkpoint_restore_phases.role,
       runtime_checkpoint_restore_phases.media_type,
       runtime_checkpoint_restore_phases.duration_ms,
       runtime_checkpoint_restore_phases.error_class,
       runtime_checkpoint_restore_phases.filepack_logical_bytes,
       runtime_checkpoint_restore_phases.filepack_allocated_bytes,
       runtime_checkpoint_restore_phases.filepack_sparse_supported,
       runtime_checkpoint_restore_phases.filepack_sparse_data_ranges,
       runtime_checkpoint_restore_phases.filepack_sparse_data_bytes,
       runtime_checkpoint_restore_phases.filepack_zero_chunks_skipped,
       runtime_checkpoint_restore_phases.filepack_encoded_chunks,
       runtime_checkpoint_restore_phases.filepack_compressed_bytes,
       runtime_checkpoint_restore_phases.filepack_unpack_written_bytes
  FROM runtime_checkpoint_restore_phases
 ORDER BY runtime_checkpoint_restore_phases.runtime_checkpoint_restore_id,
          runtime_checkpoint_restore_phases.ordinal;

WITH target AS MATERIALIZED (
    SELECT *
      FROM runs
     WHERE id = '${run_id}'::uuid
)
SELECT 'checkpoint_artifact' AS section,
       runtime_checkpoint_artifacts.runtime_checkpoint_id,
       runtime_checkpoint_artifacts.role,
       runtime_checkpoint_artifacts.ordinal,
       runtime_checkpoint_artifacts.size_bytes,
       runtime_checkpoint_artifacts.media_type,
       runtime_checkpoint_artifacts.digest,
       runtime_checkpoint_artifacts.encrypt_duration_ms,
       runtime_checkpoint_artifacts.store_duration_ms,
       runtime_checkpoint_artifacts.created_at
  FROM target
  JOIN runtime_checkpoint_artifacts
    ON runtime_checkpoint_artifacts.org_id = target.org_id
   AND runtime_checkpoint_artifacts.run_id = target.id
 ORDER BY runtime_checkpoint_artifacts.runtime_checkpoint_id,
          runtime_checkpoint_artifacts.role,
          runtime_checkpoint_artifacts.ordinal;

WITH target AS MATERIALIZED (
    SELECT *
      FROM runs
     WHERE id = '${run_id}'::uuid
)
SELECT 'checkpoint_artifact_summary' AS section,
       runtime_checkpoint_artifacts.runtime_checkpoint_id,
       runtime_checkpoint_artifacts.role,
       count(*) AS artifact_count,
       sum(runtime_checkpoint_artifacts.size_bytes)::bigint AS total_size_bytes,
       sum(runtime_checkpoint_artifacts.encrypt_duration_ms)::bigint AS total_encrypt_duration_ms,
       sum(runtime_checkpoint_artifacts.store_duration_ms)::bigint AS total_store_duration_ms,
       max(runtime_checkpoint_artifacts.encrypt_duration_ms)::bigint AS max_encrypt_duration_ms,
       max(runtime_checkpoint_artifacts.store_duration_ms)::bigint AS max_store_duration_ms
  FROM target
  JOIN runtime_checkpoint_artifacts
    ON runtime_checkpoint_artifacts.org_id = target.org_id
   AND runtime_checkpoint_artifacts.run_id = target.id
 GROUP BY runtime_checkpoint_artifacts.runtime_checkpoint_id,
          runtime_checkpoint_artifacts.role
 ORDER BY runtime_checkpoint_artifacts.runtime_checkpoint_id,
          runtime_checkpoint_artifacts.role;

WITH target AS MATERIALIZED (
    SELECT *
      FROM runs
     WHERE id = '${run_id}'::uuid
)
SELECT 'lease' AS section,
       run_leases.id,
       run_leases.status,
       run_leases.worker_instance_id,
       run_leases.restore_runtime_checkpoint_id,
       run_leases.leased_at,
       run_leases.started_at,
       run_leases.released_at,
       run_leases.active_duration_ms,
       CASE
         WHEN run_leases.leased_at IS NOT NULL
          AND run_leases.started_at IS NOT NULL
         THEN round(extract(epoch FROM run_leases.started_at - run_leases.leased_at) * 1000)::bigint
       END AS lease_to_started_ms
  FROM target
  JOIN run_leases ON run_leases.org_id = target.org_id
                 AND run_leases.run_id = target.id
 ORDER BY run_leases.leased_at, run_leases.id;

WITH target AS MATERIALIZED (
    SELECT *
      FROM runs
     WHERE id = '${run_id}'::uuid
)
SELECT 'workspace_mount' AS section,
       workspace_mounts.id,
       workspace_mounts.state,
       CASE
         WHEN runtime_instances.id IS NULL THEN 'no_runtime_instance'
         WHEN runtime_instances.adopting_workspace_mount_id = workspace_mounts.id THEN 'awaiting_preparing'
         WHEN runtime_instances.prepared_at IS NOT NULL THEN 'ready_runtime'
         WHEN runtime_instances.bound_at IS NOT NULL THEN 'cold'
         ELSE 'unknown'
       END AS runtime_path_class,
       runtime_instances.worker_instance_id,
       workspace_mounts.runtime_instance_id,
       runtime_instances.runtime_substrate_artifact_id,
       runtime_instances.adopting_workspace_mount_id,
       runtime_instances.adoption_expires_at,
       runtime_instances.expires_at AS runtime_expires_at,
       runtime_instances.reserved_cpu_millis AS runtime_reserved_cpu_millis,
       runtime_instances.reserved_memory_mib AS runtime_reserved_memory_mib,
       runtime_instances.reserved_disk_mib AS runtime_reserved_disk_mib,
       runtime_instances.reserved_execution_slots AS runtime_reserved_execution_slots,
       workspace_mounts.requested_at,
       workspace_mounts.mounted_at,
       workspace_mounts.unmounted_at,
       workspace_mounts.stopped_at,
       workspace_mounts.failed_at,
       CASE
         WHEN workspace_mounts.requested_at IS NOT NULL
          AND workspace_mounts.mounted_at IS NOT NULL
         THEN round(extract(epoch FROM workspace_mounts.mounted_at - workspace_mounts.requested_at) * 1000)::bigint
       END AS requested_to_mounted_ms
  FROM target
  JOIN workspace_mounts
    ON workspace_mounts.org_id = target.org_id
   AND workspace_mounts.project_id = target.project_id
   AND workspace_mounts.environment_id = target.environment_id
   AND workspace_mounts.workspace_id = target.workspace_id
   AND workspace_mounts.id = target.workspace_mount_id
  LEFT JOIN runtime_instances
    ON runtime_instances.org_id = workspace_mounts.org_id
   AND runtime_instances.project_id = workspace_mounts.project_id
   AND runtime_instances.environment_id = workspace_mounts.environment_id
   AND runtime_instances.id = workspace_mounts.runtime_instance_id
 ORDER BY workspace_mounts.created_at, workspace_mounts.id;

WITH target AS MATERIALIZED (
    SELECT *
      FROM runs
     WHERE id = '${run_id}'::uuid
),
run_workspace_mounts AS MATERIALIZED (
    SELECT workspace_mounts.*
      FROM target
      JOIN workspace_mounts
        ON workspace_mounts.org_id = target.org_id
       AND workspace_mounts.project_id = target.project_id
       AND workspace_mounts.environment_id = target.environment_id
       AND workspace_mounts.workspace_id = target.workspace_id
       AND workspace_mounts.id = target.workspace_mount_id
)
SELECT 'runtime_instance' AS section,
       runtime_instances.id,
       runtime_instances.state,
       runtime_instances.worker_instance_id,
       runtime_instances.deployment_sandbox_id,
       runtime_instances.runtime_substrate_artifact_id,
       runtime_instances.adopting_workspace_mount_id,
       runtime_instances.adoption_expires_at,
       runtime_instances.workspace_mount_id,
       runtime_instances.owner_run_id,
       runtime_instances.owner_run_lease_id,
       runtime_instances.last_reclaim_reason,
       runtime_instances.created_at,
       runtime_instances.prepared_at,
       runtime_instances.bound_at,
       runtime_instances.running_at,
       runtime_instances.waiting_at,
       runtime_instances.closed_at,
       runtime_instances.lost_at,
       runtime_instances.failed_at,
       CASE
         WHEN runtime_instances.created_at IS NOT NULL
          AND runtime_instances.prepared_at IS NOT NULL
         THEN round(extract(epoch FROM runtime_instances.prepared_at - runtime_instances.created_at) * 1000)::bigint
       END AS instance_create_to_ready_ms,
       CASE
         WHEN runtime_instances.prepared_at IS NOT NULL
          AND runtime_instances.bound_at IS NOT NULL
         THEN round(extract(epoch FROM runtime_instances.bound_at - runtime_instances.prepared_at) * 1000)::bigint
       END AS instance_ready_to_bound_ms,
       CASE
         WHEN runtime_instances.bound_at IS NOT NULL
          AND runtime_instances.running_at IS NOT NULL
         THEN round(extract(epoch FROM runtime_instances.running_at - runtime_instances.bound_at) * 1000)::bigint
       END AS instance_bound_to_running_ms
  FROM run_workspace_mounts
  JOIN runtime_instances
    ON runtime_instances.org_id = run_workspace_mounts.org_id
   AND (
       runtime_instances.id = run_workspace_mounts.runtime_instance_id
       OR runtime_instances.workspace_mount_id = run_workspace_mounts.id
   )
 ORDER BY runtime_instances.created_at, runtime_instances.id;

WITH target AS MATERIALIZED (
    SELECT *
      FROM runs
     WHERE id = '${run_id}'::uuid
)
SELECT 'worker_command' AS section,
       worker_commands.id,
       worker_commands.kind,
       worker_commands.run_wait_id,
       worker_commands.run_lease_id,
       worker_commands.worker_instance_id,
       worker_commands.created_at,
       worker_commands.delivered_at,
       worker_commands.acknowledged_at,
       worker_commands.completed_at,
       worker_commands.delivery_attempts,
       CASE
         WHEN worker_commands.created_at IS NOT NULL
          AND worker_commands.acknowledged_at IS NOT NULL
         THEN round(extract(epoch FROM worker_commands.acknowledged_at - worker_commands.created_at) * 1000)::bigint
       END AS command_create_to_ack_ms,
       CASE
         WHEN worker_commands.created_at IS NOT NULL
          AND worker_commands.completed_at IS NOT NULL
         THEN round(extract(epoch FROM worker_commands.completed_at - worker_commands.created_at) * 1000)::bigint
       END AS command_create_to_completed_ms
  FROM target
  JOIN worker_commands ON worker_commands.org_id = target.org_id
                      AND worker_commands.run_id = target.id
 ORDER BY worker_commands.created_at, worker_commands.id;

WITH target AS MATERIALIZED (
    SELECT *
      FROM runs
     WHERE id = '${run_id}'::uuid
),
evidence AS (
    SELECT EXISTS (
               SELECT 1
                 FROM run_waits
                WHERE run_waits.org_id = target.org_id
                  AND run_waits.run_id = target.id
                  AND run_waits.live_wait_started_at IS NOT NULL
           ) AS has_live_wait,
           EXISTS (
               SELECT 1
                 FROM run_waits
                WHERE run_waits.org_id = target.org_id
                  AND run_waits.run_id = target.id
                  AND run_waits.state = 'resumed'
                  AND run_waits.live_wait_started_at IS NOT NULL
                  AND run_waits.runtime_checkpoint_id IS NULL
           ) AS has_resident_live_resume_evidence,
           EXISTS (
               SELECT 1
                 FROM run_waits
                WHERE run_waits.org_id = target.org_id
                  AND run_waits.run_id = target.id
                  AND run_waits.runtime_checkpoint_id IS NOT NULL
           ) AS has_checkpoint_wait_evidence,
           EXISTS (
               SELECT 1
                 FROM run_leases
                WHERE run_leases.org_id = target.org_id
                  AND run_leases.run_id = target.id
                  AND run_leases.restore_runtime_checkpoint_id IS NOT NULL
           ) AS has_checkpoint_restore_lease_evidence,
           EXISTS (
               SELECT 1
                 FROM workspace_mounts
                WHERE workspace_mounts.org_id = target.org_id
                  AND workspace_mounts.project_id = target.project_id
                  AND workspace_mounts.environment_id = target.environment_id
                  AND workspace_mounts.workspace_id = target.workspace_id
                  AND workspace_mounts.id = target.workspace_mount_id
                  AND EXISTS (
                      SELECT 1
                        FROM runtime_instances
                       WHERE runtime_instances.org_id = workspace_mounts.org_id
                         AND (
                             runtime_instances.id = workspace_mounts.runtime_instance_id
                             OR runtime_instances.workspace_mount_id = workspace_mounts.id
                         )
                         AND runtime_instances.bound_at IS NOT NULL
                  )
           ) AS has_runtime_instance_claim_evidence,
           EXISTS (
               SELECT 1
                 FROM workspace_mounts
                WHERE workspace_mounts.org_id = target.org_id
                  AND workspace_mounts.project_id = target.project_id
                  AND workspace_mounts.environment_id = target.environment_id
                  AND workspace_mounts.workspace_id = target.workspace_id
                  AND workspace_mounts.id = target.workspace_mount_id
                  AND EXISTS (
                      SELECT 1
                        FROM runtime_instances
                       WHERE runtime_instances.org_id = workspace_mounts.org_id
                         AND (
                             runtime_instances.id = workspace_mounts.runtime_instance_id
                             OR runtime_instances.workspace_mount_id = workspace_mounts.id
                         )
                         AND runtime_instances.prepared_at IS NOT NULL
                         AND runtime_instances.bound_at IS NOT NULL
                  )
           ) AS has_prepared_runtime_claim_evidence,
           EXISTS (
               SELECT 1
                 FROM workspace_mounts
                WHERE workspace_mounts.org_id = target.org_id
                  AND workspace_mounts.project_id = target.project_id
                  AND workspace_mounts.environment_id = target.environment_id
                  AND workspace_mounts.workspace_id = target.workspace_id
                  AND workspace_mounts.id = target.workspace_mount_id
           ) AS has_workspace_mount_evidence
      FROM target
)
SELECT 'path_hints' AS section,
       has_live_wait,
       has_resident_live_resume_evidence,
       has_checkpoint_wait_evidence,
       has_checkpoint_restore_lease_evidence,
       has_runtime_instance_claim_evidence,
       has_prepared_runtime_claim_evidence,
       has_workspace_mount_evidence
  FROM evidence;
SQL
)"

database_url="${HELMR_DATABASE_URL:-${DATABASE_URL:-}}"
if [ -n "${database_url}" ]; then
  service_file="$(mktemp)"
  cleanup_service_file() {
    rm -f "${service_file}"
  }
  trap cleanup_service_file EXIT
  chmod 600 "${service_file}"
  write_pg_service_file "${service_file}"
  PGSERVICEFILE="${service_file}" PGSERVICE=helmr_path_report psql -v ON_ERROR_STOP=1 -P pager=off <<<"${sql}"
elif [ "${HELMR_PATH_REPORT_ALLOW_ECS_TASK:-0}" = "1" ]; then
  AWS_PROFILE="${AWS_PROFILE:-helmr-dev}" "${ROOT}/dev/aws/db-query.sh" "${sql}"
else
  cat >&2 <<'EOF'
run-path-report requires HELMR_DATABASE_URL/DATABASE_URL for a local read-only
query path. For AWS dev private-network diagnostics, explicitly set
HELMR_PATH_REPORT_ALLOW_ECS_TASK=1 to use dev/aws/db-query.sh. That fallback
does not mutate Helmr product data, but it creates AWS ECS task/log records.
EOF
  exit 2
fi
