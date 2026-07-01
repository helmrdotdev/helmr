#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
usage: dev/aws/run-surface-attestation.sh [LABEL]

Print sanitized AWS dev and DB evidence describing the surface being measured:
control/dispatcher ECS revisions, current deployments, deployment sandbox
runtime ABI/digests, selected runtime release, and active worker heartbeat.

This script is read-only for Helmr product data. For AWS dev it uses
dev/aws/db-query.sh, which creates one-off ECS task/log records. Set
HELMR_SURFACE_ATTESTATION_ALLOW_ECS_TASK=1 or HELMR_PATH_REPORT_ALLOW_ECS_TASK=1
to acknowledge that observer side effect.
EOF
}

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  usage
  exit 0
fi
if [ "$#" -gt 1 ]; then
  usage >&2
  exit 2
fi

label="${1:-surface}"
case "${label}" in
  *[!A-Za-z0-9._-]*|"")
    echo "LABEL must contain only letters, numbers, '.', '_', or '-'" >&2
    exit 2
    ;;
esac

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TF_BIN="${TF_BIN:-tofu}"
DEV_STACK="${DEV_STACK:-${ROOT}/infra/aws/stacks/dev}"
AWS_REGION="${AWS_REGION:-us-east-1}"
PROJECT="${PROJECT:-helmr}"
STAGING_ENV="${STAGING_ENV:-staging}"
PRODUCTION_ENV="${PRODUCTION_ENV:-production}"

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

tf_output_raw() {
  "${TF_BIN}" -chdir="${DEV_STACK}" output -raw "$1" 2>/dev/null || true
}

task_definition_label() {
  local task_definition=$1
  task_definition="${task_definition##*/}"
  printf '%s\n' "${task_definition}"
}

image_digest_from_ref() {
  local image_ref=$1
  case "${image_ref}" in
    *@sha256:*)
      printf 'sha256:%s\n' "${image_ref##*@sha256:}"
      ;;
    *)
      printf '[not-digest-pinned]\n'
      ;;
  esac
}

print_local_attestation() {
  local git_head
  local git_dirty
  git_head="$(git -C "${ROOT}" rev-parse --short=12 HEAD 2>/dev/null || printf 'unknown')"
  if git -C "${ROOT}" diff --quiet --ignore-submodules -- 2>/dev/null &&
     git -C "${ROOT}" diff --cached --quiet --ignore-submodules -- 2>/dev/null; then
    git_dirty=0
  else
    git_dirty=1
  fi
  printf 'section\tlabel\tobserved_at\tgit_head\tgit_dirty\n'
  printf 'local_checkout\t%s\t%s\t%s\t%s\n' "${label}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${git_head}" "${git_dirty}"
}

print_ecs_service_attestation() {
  local cluster=$1
  local kind=$2
  local service=$3
  local service_json
  local task_definition
  local task_definition_label_value
  local image_ref
  local image_digest
  [ -n "${cluster}" ] && [ -n "${service}" ] && [ "${service}" != "null" ] || return 0

  service_json="$(
    aws ecs describe-services \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --services "${service}" \
      --output json
  )"
  task_definition="$(jq -r '.services[0].taskDefinition // ""' <<<"${service_json}")"
  [ -n "${task_definition}" ] && [ "${task_definition}" != "null" ] || return 0
  task_definition_label_value="$(task_definition_label "${task_definition}")"
  image_ref="$(
    aws ecs describe-task-definition \
      --region "${AWS_REGION}" \
      --task-definition "${task_definition}" \
      --query 'taskDefinition.containerDefinitions[0].image' \
      --output text
  )"
  image_digest="$(image_digest_from_ref "${image_ref}")"

  jq -r \
    --arg kind "${kind}" \
    --arg task_definition_label "${task_definition_label_value}" \
    --arg image_digest "${image_digest}" \
    '["aws_ecs_service",
      $kind,
      (.services[0].serviceName // ""),
      ((.services[0].desiredCount // 0) | tostring),
      ((.services[0].runningCount // 0) | tostring),
      ((.services[0].pendingCount // 0) | tostring),
      $task_definition_label,
      $image_digest,
      (([.services[0].deployments[]? | select(.status == "PRIMARY") | .rolloutState] | first) // "")
    ] | @tsv' <<<"${service_json}"
}

validate_slug PROJECT "${PROJECT}"
validate_slug STAGING_ENV "${STAGING_ENV}"
validate_slug PRODUCTION_ENV "${PRODUCTION_ENV}"

if [ "${HELMR_SURFACE_ATTESTATION_ALLOW_ECS_TASK:-0}" != "1" ] &&
   [ "${HELMR_PATH_REPORT_ALLOW_ECS_TASK:-0}" != "1" ]; then
  cat >&2 <<'EOF'
surface attestation requires HELMR_SURFACE_ATTESTATION_ALLOW_ECS_TASK=1 or
HELMR_PATH_REPORT_ALLOW_ECS_TASK=1 because it uses dev/aws/db-query.sh for AWS
dev DB evidence. That query is read-only for Helmr product data, but it creates
AWS ECS task/log records.
EOF
  exit 2
fi

project_lit="$(sql_literal "${PROJECT}")"
staging_lit="$(sql_literal "${STAGING_ENV}")"
production_lit="$(sql_literal "${PRODUCTION_ENV}")"

print_local_attestation

cluster="$(tf_output_raw control_cluster_name)"
control_service="$(tf_output_raw control_service_name)"
dispatcher_service="$(tf_output_raw dispatcher_service_name)"
printf 'section\tkind\tservice\tdesired_count\trunning_count\tpending_count\ttask_definition\timage_digest\tprimary_rollout_state\n'
print_ecs_service_attestation "${cluster}" "control" "${control_service}"
print_ecs_service_attestation "${cluster}" "dispatcher" "${dispatcher_service}"

sql="$(cat <<SQL
\\pset null '[null]'
\\timing off

WITH target_project AS MATERIALIZED (
    SELECT projects.id, projects.org_id, projects.slug
      FROM projects
     WHERE projects.slug = ${project_lit}
),
target_environments AS MATERIALIZED (
    SELECT environments.id,
           environments.org_id,
           environments.project_id,
           environments.slug,
           environments.current_deployment_id
      FROM target_project
      JOIN environments
        ON environments.org_id = target_project.org_id
       AND environments.project_id = target_project.id
       AND environments.slug IN (${staging_lit}, ${production_lit})
)
SELECT 'surface_setup' AS section,
       (SELECT count(*) FROM organizations) AS organizations,
       (SELECT count(*) FROM target_project) AS target_projects,
       (SELECT count(*) FROM target_environments) AS target_environments,
       COALESCE(string_agg(target_environments.slug, ',' ORDER BY target_environments.slug), '') AS environment_slugs
  FROM target_environments;

WITH target_project AS MATERIALIZED (
    SELECT projects.id, projects.org_id, projects.slug
      FROM projects
     WHERE projects.slug = ${project_lit}
),
target_environments AS MATERIALIZED (
    SELECT environments.id,
           environments.org_id,
           environments.project_id,
           environments.slug,
           environments.current_deployment_id
      FROM target_project
      JOIN environments
        ON environments.org_id = target_project.org_id
       AND environments.project_id = target_project.id
       AND environments.slug IN (${staging_lit}, ${production_lit})
)
SELECT 'current_deployment' AS section,
       target_environments.slug AS environment_slug,
       deployments.id AS deployment_id,
       deployments.version,
       deployments.status,
       deployments.content_hash,
       deployments.api_version,
       deployments.sdk_version,
       deployments.cli_version,
       deployments.bundle_format_version,
       deployments.worker_protocol_version,
       source_artifacts.digest AS source_digest,
       manifest_artifacts.digest AS manifest_digest,
       deployments.created_at,
       deployments.built_at,
       deployments.deployed_at
  FROM target_environments
  LEFT JOIN deployments
    ON deployments.org_id = target_environments.org_id
   AND deployments.project_id = target_environments.project_id
   AND deployments.environment_id = target_environments.id
   AND deployments.id = target_environments.current_deployment_id
  LEFT JOIN artifacts AS source_artifacts
    ON source_artifacts.org_id = deployments.org_id
   AND source_artifacts.project_id = deployments.project_id
   AND source_artifacts.environment_id = deployments.environment_id
   AND source_artifacts.id = deployments.deployment_source_artifact_id
  LEFT JOIN artifacts AS manifest_artifacts
    ON manifest_artifacts.org_id = deployments.org_id
   AND manifest_artifacts.project_id = deployments.project_id
   AND manifest_artifacts.environment_id = deployments.environment_id
   AND manifest_artifacts.id = deployments.deployment_manifest_artifact_id
 ORDER BY target_environments.slug;

WITH target_project AS MATERIALIZED (
    SELECT projects.id, projects.org_id, projects.slug
      FROM projects
     WHERE projects.slug = ${project_lit}
),
target_environments AS MATERIALIZED (
    SELECT environments.id,
           environments.org_id,
           environments.project_id,
           environments.slug,
           environments.current_deployment_id
      FROM target_project
      JOIN environments
        ON environments.org_id = target_project.org_id
       AND environments.project_id = target_project.id
       AND environments.slug IN (${staging_lit}, ${production_lit})
)
SELECT 'deployment_sandbox' AS section,
       target_environments.slug AS environment_slug,
       deployment_sandboxes.sandbox_id,
       deployment_sandboxes.fingerprint,
       deployment_sandboxes.rootfs_digest,
       deployment_sandboxes.image_digest,
       image_artifacts.digest AS image_artifact_digest,
       deployment_sandboxes.image_format,
       deployment_sandboxes.runtime_abi,
       deployment_sandboxes.guestd_abi,
       deployment_sandboxes.adapter_abi,
       deployment_sandboxes.filesystem_format,
       deployment_sandboxes.contract_version,
       deployment_sandboxes.disk_floor_mib,
       deployment_sandboxes.created_at
  FROM target_environments
  JOIN deployment_sandboxes
    ON deployment_sandboxes.org_id = target_environments.org_id
   AND deployment_sandboxes.project_id = target_environments.project_id
   AND deployment_sandboxes.environment_id = target_environments.id
   AND deployment_sandboxes.deployment_id = target_environments.current_deployment_id
  LEFT JOIN artifacts AS image_artifacts
    ON image_artifacts.org_id = deployment_sandboxes.org_id
   AND image_artifacts.project_id = deployment_sandboxes.project_id
   AND image_artifacts.environment_id = deployment_sandboxes.environment_id
   AND image_artifacts.id = deployment_sandboxes.image_artifact_id
 ORDER BY target_environments.slug, deployment_sandboxes.sandbox_id;

WITH target_project AS MATERIALIZED (
    SELECT projects.id, projects.org_id, projects.slug
      FROM projects
     WHERE projects.slug = ${project_lit}
),
target_environments AS MATERIALIZED (
    SELECT environments.id,
           environments.org_id,
           environments.project_id,
           environments.slug,
           environments.current_deployment_id
      FROM target_project
      JOIN environments
        ON environments.org_id = target_project.org_id
       AND environments.project_id = target_project.id
       AND environments.slug IN (${staging_lit}, ${production_lit})
)
SELECT 'deployment_task' AS section,
       target_environments.slug AS environment_slug,
       deployment_tasks.task_id,
       deployment_tasks.requested_milli_cpu,
       deployment_tasks.requested_memory_mib,
       deployment_tasks.requested_disk_mib,
       deployment_tasks.max_active_duration_ms,
       deployment_tasks.bundle_format_version,
       bundle_artifacts.digest AS bundle_digest,
       deployment_tasks.created_at
  FROM target_environments
  JOIN deployment_tasks
    ON deployment_tasks.org_id = target_environments.org_id
   AND deployment_tasks.project_id = target_environments.project_id
   AND deployment_tasks.environment_id = target_environments.id
   AND deployment_tasks.deployment_id = target_environments.current_deployment_id
  LEFT JOIN artifacts AS bundle_artifacts
    ON bundle_artifacts.org_id = deployment_tasks.org_id
   AND bundle_artifacts.project_id = deployment_tasks.project_id
   AND bundle_artifacts.environment_id = deployment_tasks.environment_id
   AND bundle_artifacts.id = deployment_tasks.bundle_artifact_id
 ORDER BY target_environments.slug, deployment_tasks.task_id;

SELECT 'selected_runtime_release' AS section,
       runtime_releases.runtime_id,
       runtime_releases.runtime_arch,
       runtime_releases.runtime_abi,
       runtime_releases.kernel_digest,
       runtime_releases.initramfs_digest,
       runtime_releases.rootfs_digest,
       runtime_releases.cni_profile,
       runtime_release_selections.selected_at,
       runtime_releases.last_seen_at
  FROM runtime_release_selections
  JOIN runtime_releases
    ON runtime_releases.runtime_id = runtime_release_selections.runtime_id
 ORDER BY runtime_release_selections.selected_at DESC;

SELECT 'worker_instance' AS section,
       worker_instances.id AS worker_instance_id,
       worker_groups.name AS worker_group,
       worker_instances.status,
       worker_instances.region,
       worker_instances.worker_version,
       worker_instances.protocol_version,
       worker_instances.runtime_id,
       worker_instances.runtime_arch,
       worker_instances.runtime_abi,
       worker_instances.kernel_digest,
       worker_instances.initramfs_digest,
       worker_instances.rootfs_digest,
       worker_instances.cni_profile,
       worker_instances.total_milli_cpu,
       worker_instances.total_memory_mib,
       worker_instances.total_disk_mib,
       worker_instances.total_execution_slots,
       worker_instances.available_milli_cpu,
       worker_instances.available_memory_mib,
       worker_instances.available_disk_mib,
       worker_instances.available_execution_slots,
       round(extract(epoch FROM now() - worker_instances.last_seen_at))::bigint AS last_seen_age_seconds,
       worker_instances.first_seen_at,
       worker_instances.last_seen_at
  FROM worker_instances
  JOIN worker_groups ON worker_groups.id = worker_instances.worker_group_id
 ORDER BY worker_instances.status, worker_instances.last_seen_at DESC;
SQL
)"

AWS_PROFILE="${AWS_PROFILE:-helmr-dev}" "${ROOT}/dev/aws/db-query.sh" "${sql}"
