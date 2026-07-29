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
if [[ ! "${run_id}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]]; then
  echo "RUN_ID must be a canonical UUIDv7" >&2
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
       WHERE id = '${run_id}'
  ) THEN
    RAISE EXCEPTION 'run ${run_id} not found';
  END IF;
END
\$\$;

SELECT 'run' AS section,
       runs.id,
       runs.entrypoint_kind,
       runs.entrypoint_declared_id,
       runs.status,
       runs.workspace_id,
       runs.base_workspace_version_id,
       runs.current_run_lease_id,
       runs.state_version,
       runs.current_attempt_number,
       runs.active_elapsed_ms,
       runs.active_started_at,
       runs.created_at,
       runs.started_at,
       runs.terminal_at,
       runs.terminal_reason_code
  FROM runs
 WHERE runs.id = '${run_id}';

SELECT 'run_lease' AS section,
       run_leases.id,
       run_leases.lease_sequence,
       run_leases.attempt_number,
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
 WHERE run_leases.run_id = (SELECT id FROM runs WHERE id = '${run_id}')
 ORDER BY run_leases.lease_sequence;

SELECT 'run_wait' AS section,
       run_waits.id,
       run_waits.kind,
       run_waits.condition_state,
       run_waits.suspension_state,
       run_waits.token_id,
       run_waits.child_run_id,
       run_waits.actor_id,
       run_waits.current_run_lease_id,
       run_waits.prior_run_lease_id,
       run_waits.suspend_checkpoint_id,
       run_waits.handoff_resume_checkpoint_id,
       run_waits.resume_attach_id,
       run_waits.base_workspace_version_id,
       run_waits.resume_workspace_version_id,
       run_waits.checkpoint_request_version,
       run_waits.checkpoint_ack_version,
       run_waits.resume_request_version,
       run_waits.resume_ack_version,
       run_waits.condition_terminal_at,
       run_waits.condition_reason_code,
       run_waits.suspension_terminal_at,
       run_waits.suspension_reason_code
  FROM run_waits
 WHERE run_waits.run_id = (SELECT id FROM runs WHERE id = '${run_id}')
 ORDER BY run_waits.created_at, run_waits.id;

SELECT 'run_checkpoint' AS section,
       run_checkpoints.id,
       run_checkpoints.kind,
       run_checkpoints.attempt_number,
       run_checkpoints.run_wait_id,
       run_checkpoints.state,
       run_checkpoints.source_run_lease_id,
       run_checkpoints.source_workspace_lease_id,
       run_checkpoints.workspace_id,
       run_checkpoints.base_workspace_version_id,
       run_checkpoints.private_workspace_version_id,
       run_checkpoints.created_at,
       run_checkpoints.ready_at,
       run_checkpoints.invalidated_at,
       run_checkpoints.expires_at,
       run_checkpoints.invalidation_reason_code
  FROM run_checkpoints
 WHERE run_checkpoints.run_id = (SELECT id FROM runs WHERE id = '${run_id}')
 ORDER BY run_checkpoints.created_at, run_checkpoints.id;

SELECT 'run_checkpoint_artifact' AS section,
       run_checkpoint_artifacts.run_checkpoint_id,
       run_checkpoint_artifacts.role,
       run_checkpoint_artifacts.ordinal,
       run_checkpoint_artifacts.artifact_id,
       artifacts.size_bytes,
       artifacts.media_type,
       artifacts.digest,
       run_checkpoint_artifacts.created_at
  FROM run_checkpoint_artifacts
  JOIN run_checkpoints ON run_checkpoints.id = run_checkpoint_artifacts.run_checkpoint_id
  JOIN artifacts ON artifacts.id = run_checkpoint_artifacts.artifact_id
 WHERE run_checkpoints.run_id = (SELECT id FROM runs WHERE id = '${run_id}')
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
       runtime_instances.reserved_run_id,
       runtime_instances.reserved_attempt_number,
       runtime_instances.reserved_process_id,
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
       runtime_instances.lost_at,
       runtime_instances.reclaimed_at,
       runtime_instances.terminal_reason_code
  FROM run_leases
  JOIN runtime_instances ON runtime_instances.org_id = run_leases.org_id
                        AND runtime_instances.id = run_leases.runtime_instance_id
 WHERE run_leases.run_id = (SELECT id FROM runs WHERE id = '${run_id}')
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
 WHERE run_leases.run_id = (SELECT id FROM runs WHERE id = '${run_id}')
 ORDER BY run_leases.lease_sequence;

SELECT 'workspace_mount' AS section,
       run_leases.id AS run_lease_id,
       workspace_mounts.id AS workspace_mount_id,
       workspace_mounts.runtime_instance_id,
       workspace_mounts.worker_instance_id,
       workspace_mounts.worker_epoch,
       workspace_mounts.workspace_id,
       workspace_mounts.materialized_version_id,
       workspace_mounts.state,
       workspace_mounts.dirty_generation,
       workspace_mounts.fencing_generation,
       workspace_mounts.finalization_kind,
       workspace_mounts.staged_version_id,
       workspace_mounts.requested_at,
       workspace_mounts.mounted_at,
       workspace_mounts.unmounted_at,
       workspace_mounts.stopped_at,
       workspace_mounts.lost_at,
       workspace_mounts.failed_at,
       workspace_mounts.terminal_at,
       workspace_mounts.terminal_reason_code
  FROM run_leases
  JOIN workspace_mounts ON workspace_mounts.org_id = run_leases.org_id
                       AND workspace_mounts.runtime_instance_id = run_leases.runtime_instance_id
 WHERE run_leases.run_id = (SELECT id FROM runs WHERE id = '${run_id}')
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
     WHERE run_leases.run_id = (SELECT id FROM runs WHERE id = '${run_id}')
),
wait_evidence AS MATERIALIZED (
    SELECT bool_or(
               run_waits.condition_state = 'pending'
               AND run_waits.suspension_state = 'hot'
           ) AS has_live_wait,
           bool_or(
               run_waits.suspend_checkpoint_id IS NOT NULL
               AND run_waits.prior_run_lease_id IS NOT NULL
               AND run_waits.resume_request_version > 0
           ) AS has_checkpoint_resume
      FROM run_waits
     WHERE run_waits.run_id = (SELECT id FROM runs WHERE id = '${run_id}')
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
                 AND (
                     run_waits.suspend_checkpoint_id IS NOT NULL
                     OR run_waits.handoff_resume_checkpoint_id IS NOT NULL
                 )
          )
         THEN 'checkpoint_resume'
         WHEN COALESCE(wait_evidence.has_live_wait, false)
          AND EXISTS (
              SELECT 1
               FROM run_waits
               WHERE run_waits.run_id = lease_evidence.run_id
                 AND run_waits.current_run_lease_id = lease_evidence.id
                 AND run_waits.condition_state = 'pending'
                 AND run_waits.suspension_state = 'hot'
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
