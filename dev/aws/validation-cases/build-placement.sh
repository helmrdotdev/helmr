#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=case-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/case-lib.sh"
trap validation_cleanup_tmp EXIT INT TERM

if validation_dry_run; then
  exit 0
fi

HELMR_SMOKE_RESULT_FILE="${VALIDATION_TMP}/release.json" \
SMOKE_CASES=runtime \
  "${VALIDATION_ROOT}/dev/workflows/scripts/run-release-smoke.sh" \
  >"${VALIDATION_TMP}/release.stdout" 2>"${VALIDATION_TMP}/release.stderr"
jq -e '
  .schema == "helmrdotdev.release-smoke-result.v1" and
  .status == "passed" and .executed_cases == ["runtime"]
' "${VALIDATION_TMP}/release.json" >/dev/null

deployment="$(
  validation_run_helmr deployment list \
    --project "${VALIDATION_PROJECT}" --env "${VALIDATION_ENVIRONMENT}" --json |
    jq -er '.deployments[0] | select(.status == "deployed")'
)"
deployment_id="$(jq -er '.id' <<<"${deployment}")"
validation_require_public_id dep "${deployment_id}"
validation_db_marker "
  COPY (
    SELECT 'build-group-only'
      FROM deployments
      JOIN environments
        ON environments.org_id = deployments.org_id
       AND environments.project_id = deployments.project_id
       AND environments.id = deployments.environment_id
       AND environments.current_deployment_id = deployments.id
       AND environments.slug = '${VALIDATION_ENVIRONMENT}'
     WHERE deployments.public_id = '${deployment_id}'
       AND deployments.status = 'deployed'
       AND EXISTS (
           SELECT 1
             FROM deployment_build_leases lease
             JOIN worker_instances worker
               ON worker.id = lease.worker_instance_id
              AND worker.worker_group_id = lease.worker_group_id
            WHERE lease.deployment_id = deployments.id
              AND lease.state = 'succeeded'
              AND worker.supports_build
              AND NOT worker.supports_run
       )
       AND NOT EXISTS (
           SELECT 1
             FROM deployment_build_leases lease
             JOIN worker_instances worker
               ON worker.id = lease.worker_instance_id
              AND worker.worker_group_id = lease.worker_group_id
            WHERE lease.deployment_id = deployments.id
              AND (NOT worker.supports_build OR worker.supports_run)
       )
     LIMIT 1
  ) TO STDOUT;
" build-group-only || {
  validation_write_result failed build_placement_not_proved
  exit 1
}

run_ids="$(jq -c '[.run_ids[]?] | unique' "${VALIDATION_TMP}/release.json")"
while IFS= read -r run_id; do
  validation_require_public_id run "${run_id}" || {
    validation_write_result failed invalid_release_run_id
    exit 1
  }
done < <(jq -r '.[]' <<<"${run_ids}")
objects="$(jq -cn --arg deployment "${deployment_id}" --argjson runs "${run_ids}" '{
  run_ids:$runs,workspace_ids:[],deployment_ids:[$deployment],
  schedule_ids:[],token_ids:[],actor_ids:[]
}')"
validation_write_result passed null "${objects}" '{
  build_completed:true,
  build_group_only:true,
  promoted_deployment_exact:true,
  runtime_probe_succeeded:true
}'
