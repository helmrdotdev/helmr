#!/usr/bin/env bash
set -euo pipefail

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  cat <<'EOF'
usage: dev/aws/run-path-report.sh RUN_ID

Print AWS dev Postgres evidence for classifying a run's placement path:
resident live wait, checkpoint resume, prepared runtime claim, or cold runtime
allocation.

The report treats runs, run leases, waits, checkpoints, runtime instances,
network slots, and workspace mounts as the final authority. By default it
requires HELMR_DATABASE_URL or DATABASE_URL and runs psql locally. For AWS dev
private-network queries, set HELMR_PATH_REPORT_ALLOW_ECS_TASK=1 to opt into
dev/aws/db-query.sh. That path does not mutate Helmr product data, but it does
create AWS ECS task/log records.
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
BEGIN READ ONLY;

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

SELECT 'run' AS section,
       runs.id,
       runs.task_id,
       runs.status,
       runs.execution_status,
       runs.terminal_outcome,
       runs.workspace_id,
       runs.current_run_lease_id,
       runs.latest_run_checkpoint_id,
       runs.state_version,
       runs.current_attempt_number,
       runs.active_elapsed_ms,
       runs.active_started_at,
       runs.created_at,
       runs.started_at,
       runs.finished_at
  FROM runs
 WHERE runs.id = '${run_id}'::uuid;

SELECT 'run_lease' AS section,
       run_leases.id,
       run_leases.lease_sequence,
       run_leases.task_attempt_number,
       run_leases.state,
       run_leases.worker_group_id,
       run_leases.worker_instance_id,
       run_leases.worker_epoch,
       run_leases.runtime_instance_id,
       run_leases.network_slot_id,
       run_leases.network_slot_generation,
       run_leases.assigned_at,
       run_leases.claimed_at,
       run_leases.started_at,
       run_leases.renewed_at,
       run_leases.expires_at,
       run_leases.checkpointed_at,
       run_leases.terminal_at,
       run_leases.terminal_reason_code,
       CASE
         WHEN run_leases.claimed_at IS NOT NULL
         THEN round(extract(epoch FROM run_leases.claimed_at - run_leases.assigned_at) * 1000)::bigint
       END AS assigned_to_claimed_ms,
       CASE
         WHEN run_leases.started_at IS NOT NULL
         THEN round(extract(epoch FROM run_leases.started_at - run_leases.assigned_at) * 1000)::bigint
       END AS assigned_to_started_ms
  FROM run_leases
 WHERE run_leases.run_id = '${run_id}'::uuid
 ORDER BY run_leases.lease_sequence;

SELECT 'run_wait' AS section,
       run_waits.id,
       run_waits.wait_id,
       waits.kind,
       waits.state AS wait_state,
       run_waits.state AS run_wait_state,
       run_waits.current_run_lease_id,
       run_waits.prior_run_lease_id,
       run_waits.run_checkpoint_id,
       run_waits.reserved_workspace_id,
       run_waits.reserved_workspace_version_id,
       run_waits.checkpoint_request_version,
       run_waits.checkpoint_ack_version,
       run_waits.checkpoint_attempt_id,
       run_waits.resume_request_version,
       run_waits.resume_ack_version,
       run_waits.hot_wait_started_at,
       run_waits.checkpoint_requested_at,
       run_waits.checkpoint_acknowledged_at,
       run_waits.resume_requested_at,
       run_waits.resume_acknowledged_at,
       run_waits.resuming_at,
       run_waits.released_at,
       run_waits.terminal_at,
       run_waits.terminal_reason_code
  FROM run_waits
  JOIN waits ON waits.org_id = run_waits.org_id
            AND waits.id = run_waits.wait_id
 WHERE run_waits.run_id = '${run_id}'::uuid
 ORDER BY run_waits.created_at, run_waits.id;

SELECT 'run_checkpoint' AS section,
       run_checkpoints.id,
       run_checkpoints.run_wait_id,
       run_checkpoints.state,
       run_checkpoints.source_run_lease_id,
       run_checkpoints.source_runtime_instance_id,
       run_checkpoints.source_worker_instance_id,
       run_checkpoints.source_worker_epoch,
       run_checkpoints.source_workspace_lease_id,
       run_checkpoints.workspace_mount_id,
       run_checkpoints.base_workspace_version_id,
       run_checkpoints.runtime_backend,
       run_checkpoints.runtime_identity_id,
       run_checkpoints.runtime_substrate_id,
       run_checkpoints.creation_started_at,
       run_checkpoints.creation_expires_at,
       run_checkpoints.ready_at,
       run_checkpoints.invalidated_at,
       run_checkpoints.expires_at,
       run_checkpoints.error
  FROM run_checkpoints
 WHERE run_checkpoints.run_id = '${run_id}'::uuid
 ORDER BY run_checkpoints.created_at, run_checkpoints.id;

SELECT 'run_checkpoint_artifact' AS section,
       run_checkpoint_artifacts.run_checkpoint_id,
       run_checkpoint_artifacts.role,
       run_checkpoint_artifacts.ordinal,
       run_checkpoint_artifacts.artifact_id,
       run_checkpoint_artifacts.size_bytes,
       run_checkpoint_artifacts.media_type,
       run_checkpoint_artifacts.digest,
       run_checkpoint_artifacts.encrypt_duration_ms,
       run_checkpoint_artifacts.store_duration_ms,
       run_checkpoint_artifacts.created_at
  FROM run_checkpoint_artifacts
 WHERE run_checkpoint_artifacts.run_id = '${run_id}'::uuid
 ORDER BY run_checkpoint_artifacts.run_checkpoint_id,
          run_checkpoint_artifacts.role,
          run_checkpoint_artifacts.ordinal;

SELECT 'runtime_instance' AS section,
       run_leases.id AS run_lease_id,
       runtime_instances.id AS runtime_instance_id,
       runtime_instances.worker_group_id,
       runtime_instances.worker_instance_id,
       runtime_instances.worker_epoch,
       runtime_instances.runtime_identity_id,
       runtime_instances.workspace_id,
       runtime_instances.workspace_version_id,
       runtime_instances.reserved_workspace_id,
       runtime_instances.reserved_workspace_version_id,
       runtime_instances.reservation_expires_at,
       runtime_instances.desired_state,
       runtime_instances.desired_version,
       runtime_instances.observed_state,
       runtime_instances.observed_version,
       runtime_instances.observed_desired_version,
       runtime_instances.allocated_at,
       runtime_instances.preparing_at,
       runtime_instances.ready_at,
       runtime_instances.closing_at,
       runtime_instances.closed_at,
       runtime_instances.failed_at,
       runtime_instances.lost_at,
       runtime_instances.reclaimed_at,
       runtime_instances.terminal_reason_code
  FROM run_leases
  JOIN runtime_instances ON runtime_instances.org_id = run_leases.org_id
                        AND runtime_instances.id = run_leases.runtime_instance_id
 WHERE run_leases.run_id = '${run_id}'::uuid
 ORDER BY run_leases.lease_sequence;

SELECT 'network_slot' AS section,
       run_leases.id AS run_lease_id,
       run_leases.network_slot_generation AS leased_generation,
       worker_network_slots.id AS network_slot_id,
       worker_network_slots.generation AS current_generation,
       worker_network_slots.state,
       worker_network_slots.runtime_instance_id,
       worker_network_slots.assigned_at,
       worker_network_slots.reclaiming_at,
       worker_network_slots.quarantined_at,
       worker_network_slots.lost_at,
       worker_network_slots.reclaimed_at,
       worker_network_slots.state_reason_code,
       worker_network_slots.generation = run_leases.network_slot_generation AS generation_matches_lease,
       worker_network_slots.runtime_instance_id = run_leases.runtime_instance_id AS runtime_matches_lease
  FROM run_leases
  JOIN worker_network_slots ON worker_network_slots.id = run_leases.network_slot_id
 WHERE run_leases.run_id = '${run_id}'::uuid
 ORDER BY run_leases.lease_sequence;

SELECT 'workspace_mount' AS section,
       run_leases.id AS run_lease_id,
       workspace_mounts.id AS workspace_mount_id,
       workspace_mounts.runtime_instance_id,
       workspace_mounts.worker_instance_id,
       workspace_mounts.worker_epoch,
       workspace_mounts.workspace_id,
       workspace_mounts.base_version_id,
       workspace_mounts.state,
       workspace_mounts.fencing_generation,
       workspace_mounts.requested_at,
       workspace_mounts.mounted_at,
       workspace_mounts.unmounted_at,
       workspace_mounts.terminal_at,
       workspace_mounts.terminal_reason_code
  FROM run_leases
  JOIN workspace_mounts ON workspace_mounts.org_id = run_leases.org_id
                       AND workspace_mounts.runtime_instance_id = run_leases.runtime_instance_id
 WHERE run_leases.run_id = '${run_id}'::uuid
 ORDER BY run_leases.lease_sequence, workspace_mounts.created_at;

WITH lease_evidence AS MATERIALIZED (
    SELECT run_leases.*,
           runtime_instances.allocated_at,
           runtime_instances.ready_at,
           runtime_instances.observed_state,
           EXISTS (
               SELECT 1
                 FROM workspace_mounts
                WHERE workspace_mounts.org_id = run_leases.org_id
                  AND workspace_mounts.runtime_instance_id = run_leases.runtime_instance_id
           ) AS has_workspace_mount
      FROM run_leases
      JOIN runtime_instances ON runtime_instances.org_id = run_leases.org_id
                            AND runtime_instances.id = run_leases.runtime_instance_id
     WHERE run_leases.run_id = '${run_id}'::uuid
),
wait_evidence AS MATERIALIZED (
    SELECT bool_or(run_waits.state = 'hot_waiting') AS has_live_wait,
           bool_or(
               run_waits.run_checkpoint_id IS NOT NULL
               AND run_waits.prior_run_lease_id IS NOT NULL
               AND run_waits.resume_request_version > 0
           ) AS has_checkpoint_resume
      FROM run_waits
     WHERE run_waits.run_id = '${run_id}'::uuid
)
SELECT 'path_hints' AS section,
       lease_evidence.id AS run_lease_id,
       lease_evidence.lease_sequence,
       COALESCE(wait_evidence.has_live_wait, false) AS has_live_wait,
       COALESCE(wait_evidence.has_checkpoint_resume, false) AS has_checkpoint_resume,
       lease_evidence.ready_at IS NOT NULL
         AND lease_evidence.ready_at <= lease_evidence.assigned_at AS was_ready_before_assignment,
       lease_evidence.allocated_at >= lease_evidence.assigned_at AS was_allocated_after_assignment,
       lease_evidence.has_workspace_mount,
       CASE
         WHEN COALESCE(wait_evidence.has_checkpoint_resume, false)
          AND EXISTS (
              SELECT 1
                FROM run_waits
               WHERE run_waits.run_id = lease_evidence.run_id
                 AND run_waits.current_run_lease_id = lease_evidence.id
                 AND run_waits.run_checkpoint_id IS NOT NULL
          )
         THEN 'checkpoint_resume'
         WHEN COALESCE(wait_evidence.has_live_wait, false)
          AND EXISTS (
              SELECT 1
                FROM run_waits
               WHERE run_waits.run_id = lease_evidence.run_id
                 AND run_waits.current_run_lease_id = lease_evidence.id
                 AND run_waits.state = 'hot_waiting'
          )
         THEN 'resident_live_wait'
         WHEN lease_evidence.ready_at IS NOT NULL
          AND lease_evidence.ready_at <= lease_evidence.assigned_at
         THEN 'prepared_runtime_claim'
         ELSE 'cold_runtime_allocation'
       END AS inferred_path
  FROM lease_evidence
 CROSS JOIN wait_evidence
 ORDER BY lease_evidence.lease_sequence;

COMMIT;
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
