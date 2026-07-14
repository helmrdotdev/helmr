#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
usage: dev/aws/run-measurement-preflight.sh [--setup-only] [--require-deployments]

Validate AWS dev state before interpreting runtime latency measurements.

Checks:
  - browser setup created an organization
  - project and staging/production environments exist
  - unless --setup-only is used, at least one active worker has a recent heartbeat after DB reset
  - worker capacity is visible to the scheduler
  - optionally, staging/production have current deployments

Default required resource vector:
  HELMR_MEASUREMENT_REQUIRED_MILLI_CPU=2000
  HELMR_MEASUREMENT_REQUIRED_MEMORY_MIB=2048
  HELMR_MEASUREMENT_REQUIRED_DISK_MIB=16384
  HELMR_MEASUREMENT_REQUIRED_EXECUTION_SLOTS=1

This script is read-only for Helmr product data. For AWS dev it uses
dev/aws/db-query.sh, which creates one-off ECS task/log records. Set
HELMR_MEASUREMENT_PREFLIGHT_ALLOW_ECS_TASK=1 to acknowledge that observer side
effect.
EOF
}

require_deployments=0
setup_only=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --setup-only)
      setup_only=1
      shift
      ;;
    --require-deployments)
      require_deployments=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
done

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PROJECT="${PROJECT:-helmr}"
STAGING_ENV="${STAGING_ENV:-staging}"
PRODUCTION_ENV="${PRODUCTION_ENV:-production}"
WORKER_HEARTBEAT_MAX_AGE_SECONDS="${WORKER_HEARTBEAT_MAX_AGE_SECONDS:-300}"
REQUIRED_MILLI_CPU="${HELMR_MEASUREMENT_REQUIRED_MILLI_CPU:-2000}"
REQUIRED_MEMORY_MIB="${HELMR_MEASUREMENT_REQUIRED_MEMORY_MIB:-2048}"
REQUIRED_DISK_MIB="${HELMR_MEASUREMENT_REQUIRED_DISK_MIB:-16384}"
REQUIRED_EXECUTION_SLOTS="${HELMR_MEASUREMENT_REQUIRED_EXECUTION_SLOTS:-1}"

validate_slug() {
  local name=$1
  local value=$2
  case "${value}" in
    *[!A-Za-z0-9._-]*|"")
      printf '%s must contain only letters, numbers, dot, underscore, or dash\n' "${name}" >&2
      exit 2
      ;;
  esac
}

sql_literal() {
  printf "'%s'" "$(printf '%s' "$1" | sed "s/'/''/g")"
}

validate_slug PROJECT "${PROJECT}"
validate_slug STAGING_ENV "${STAGING_ENV}"
validate_slug PRODUCTION_ENV "${PRODUCTION_ENV}"
case "${WORKER_HEARTBEAT_MAX_AGE_SECONDS}" in
  ''|*[!0-9]*)
    printf 'WORKER_HEARTBEAT_MAX_AGE_SECONDS must be a non-negative integer\n' >&2
    exit 2
    ;;
esac
for numeric_setting in REQUIRED_MILLI_CPU REQUIRED_MEMORY_MIB REQUIRED_DISK_MIB REQUIRED_EXECUTION_SLOTS; do
  numeric_value="${!numeric_setting}"
  case "${numeric_value}" in
    ''|*[!0-9]*)
      printf '%s must be a non-negative integer\n' "${numeric_setting}" >&2
      exit 2
      ;;
  esac
done
if [ "${REQUIRED_MILLI_CPU}" -lt 1 ] || [ "${REQUIRED_MEMORY_MIB}" -lt 1 ] || [ "${REQUIRED_EXECUTION_SLOTS}" -lt 1 ]; then
  printf 'required CPU, memory, and execution slots must be positive\n' >&2
  exit 2
fi

project_lit="$(sql_literal "${PROJECT}")"
staging_lit="$(sql_literal "${STAGING_ENV}")"
production_lit="$(sql_literal "${PRODUCTION_ENV}")"
heartbeat_lit="$(sql_literal "${WORKER_HEARTBEAT_MAX_AGE_SECONDS} seconds")"
required_milli_cpu="${REQUIRED_MILLI_CPU}"
required_memory_mib="${REQUIRED_MEMORY_MIB}"
required_disk_mib="${REQUIRED_DISK_MIB}"
required_execution_slots="${REQUIRED_EXECUTION_SLOTS}"

sql="$(cat <<SQL
CREATE TEMP TABLE preflight_target_project AS
    SELECT projects.id, projects.org_id, projects.slug
      FROM projects
     WHERE projects.slug = ${project_lit};

CREATE TEMP TABLE preflight_target_environments AS
    SELECT environments.slug,
           environments.current_deployment_id,
           deployments.status AS current_deployment_status
      FROM preflight_target_project
      JOIN environments
        ON environments.org_id = preflight_target_project.org_id
       AND environments.project_id = preflight_target_project.id
       AND environments.slug IN (${staging_lit}, ${production_lit})
      LEFT JOIN deployments
        ON deployments.org_id = environments.org_id
       AND deployments.project_id = environments.project_id
       AND deployments.environment_id = environments.id
       AND deployments.id = environments.current_deployment_id;

CREATE TEMP TABLE preflight_worker_summary AS
WITH active_workers AS (
    SELECT worker_instances.*
      FROM worker_instances
     WHERE state = 'active'
),
recent_workers AS (
    SELECT *
      FROM active_workers
     WHERE updated_at >= now() - ${heartbeat_lit}::interval
)
SELECT (SELECT count(*) FROM active_workers) AS active_workers,
       (SELECT count(*) FROM recent_workers) AS recent_active_workers,
       max(updated_at) AS latest_worker_heartbeat_at,
       count(*) FILTER (
           WHERE max_vm_slots >= ${required_execution_slots}
             AND certified_cpu_millis >= ${required_milli_cpu}
             AND certified_memory_bytes >= ${required_memory_mib} * 1048576::bigint
             AND certified_workload_disk_bytes >= ${required_disk_mib} * 1048576::bigint
       ) AS recent_schedulable_workers,
       COALESCE(sum(max_vm_slots), 0)::bigint AS recent_raw_available_slots,
       COALESCE(sum(max_vm_slots), 0)::bigint AS recent_effective_available_slots,
       COALESCE(sum(certified_cpu_millis), 0)::bigint AS recent_effective_available_milli_cpu,
       COALESCE(sum(certified_memory_bytes / 1048576), 0)::bigint AS recent_effective_available_memory_mib,
       COALESCE(sum(certified_workload_disk_bytes / 1048576), 0)::bigint AS recent_effective_available_disk_mib
  FROM recent_workers;

SELECT 'setup' AS section,
       (SELECT count(*) FROM organizations) AS organizations,
       (SELECT count(*) FROM preflight_target_project) AS target_projects,
       (SELECT count(*) FROM preflight_target_environments) AS target_environments,
       (SELECT string_agg(slug, ',' ORDER BY slug) FROM preflight_target_environments) AS environment_slugs
UNION ALL
SELECT 'workers' AS section,
       active_workers AS organizations,
       recent_active_workers AS target_projects,
       recent_effective_available_slots AS target_environments,
       concat(
         'latest_heartbeat=', coalesce(latest_worker_heartbeat_at::text, 'none'),
         ',schedulable_workers=', recent_schedulable_workers,
         ',required=', ${required_milli_cpu}, 'm/', ${required_memory_mib}, 'MiB/', ${required_disk_mib}, 'MiB-disk/', ${required_execution_slots}, 'slots',
         ',raw_available_slots=', recent_raw_available_slots,
         ',effective_milli_cpu=', recent_effective_available_milli_cpu,
         ',effective_memory_mib=', recent_effective_available_memory_mib,
         ',effective_disk_mib=', recent_effective_available_disk_mib
       ) AS environment_slugs
  FROM preflight_worker_summary
UNION ALL
SELECT 'deployments' AS section,
       count(*) FILTER (WHERE current_deployment_id IS NOT NULL) AS organizations,
       count(*) FILTER (WHERE current_deployment_status = 'deployed') AS target_projects,
       count(*) AS target_environments,
       coalesce(string_agg(slug || ':' || coalesce(current_deployment_status::text, 'missing'), ',' ORDER BY slug), '') AS environment_slugs
  FROM preflight_target_environments;

DO \$\$
DECLARE
    organization_count bigint;
    project_count bigint;
    environment_count bigint;
    recent_worker_count bigint;
    recent_slots bigint;
    recent_milli_cpu bigint;
    recent_memory_mib bigint;
    recent_disk_mib bigint;
    recent_schedulable_workers bigint;
    missing_deployments bigint;
BEGIN
    SELECT count(*) INTO organization_count FROM organizations;
    IF organization_count < 1 THEN
        RAISE EXCEPTION 'measurement preflight failed: setup incomplete (no organizations)';
    END IF;

    SELECT count(*) INTO project_count FROM projects WHERE slug = ${project_lit};
    IF project_count <> 1 THEN
        RAISE EXCEPTION 'measurement preflight failed: expected exactly one project with slug %, got %', ${project_lit}, project_count;
    END IF;

    SELECT count(*) INTO environment_count
      FROM preflight_target_environments;
    IF environment_count <> 2 THEN
        RAISE EXCEPTION 'measurement preflight failed: expected environments %, %, got %', ${staging_lit}, ${production_lit}, environment_count;
    END IF;

    IF ${setup_only} = 0 THEN
    SELECT preflight_worker_summary.recent_active_workers,
           preflight_worker_summary.recent_effective_available_slots,
           preflight_worker_summary.recent_effective_available_milli_cpu,
           preflight_worker_summary.recent_effective_available_memory_mib,
           preflight_worker_summary.recent_effective_available_disk_mib,
           preflight_worker_summary.recent_schedulable_workers
      INTO recent_worker_count,
           recent_slots,
           recent_milli_cpu,
           recent_memory_mib,
           recent_disk_mib,
           recent_schedulable_workers
      FROM preflight_worker_summary;
    IF recent_worker_count < 1 THEN
        RAISE EXCEPTION 'measurement preflight failed: no active worker heartbeat within %', ${heartbeat_lit};
    END IF;
    IF recent_slots < ${required_execution_slots} THEN
        RAISE EXCEPTION 'measurement preflight failed: recent active workers report only % effective slots, require %', recent_slots, ${required_execution_slots};
    END IF;
    IF recent_milli_cpu < ${required_milli_cpu} THEN
        RAISE EXCEPTION 'measurement preflight failed: recent active workers report only % effective milli CPU, require %', recent_milli_cpu, ${required_milli_cpu};
    END IF;
    IF recent_memory_mib < ${required_memory_mib} THEN
        RAISE EXCEPTION 'measurement preflight failed: recent active workers report only % effective memory MiB, require %', recent_memory_mib, ${required_memory_mib};
    END IF;
    IF recent_disk_mib < ${required_disk_mib} THEN
        RAISE EXCEPTION 'measurement preflight failed: recent active workers report only % effective disk MiB, require %', recent_disk_mib, ${required_disk_mib};
    END IF;
    IF recent_schedulable_workers < 1 THEN
        RAISE EXCEPTION 'measurement preflight failed: no single recent active worker fits required vector % milli CPU, % memory MiB, % disk MiB, % slot(s)', ${required_milli_cpu}, ${required_memory_mib}, ${required_disk_mib}, ${required_execution_slots};
    END IF;
    END IF;

    IF ${require_deployments} = 1 THEN
        SELECT count(*) INTO missing_deployments
          FROM preflight_target_environments
         WHERE current_deployment_id IS NULL
            OR current_deployment_status <> 'deployed';
        IF missing_deployments <> 0 THEN
            RAISE EXCEPTION 'measurement preflight failed: % target environments do not have deployed current deployments', missing_deployments;
        END IF;
    END IF;
END
\$\$;
SQL
)"

if [ "${HELMR_MEASUREMENT_PREFLIGHT_ALLOW_ECS_TASK:-0}" != "1" ]; then
  cat >&2 <<'EOF'
measurement preflight requires HELMR_MEASUREMENT_PREFLIGHT_ALLOW_ECS_TASK=1 for
AWS dev because it uses dev/aws/db-query.sh. That query is read-only for Helmr
product data, but it creates AWS ECS task/log records.
EOF
  exit 2
fi

AWS_PROFILE="${AWS_PROFILE:-helmr-dev}" "${ROOT}/dev/aws/db-query.sh" "${sql}"
