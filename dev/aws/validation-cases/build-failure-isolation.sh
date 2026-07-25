#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=case-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/case-lib.sh"
trap validation_cleanup_tmp EXIT INT TERM

if validation_dry_run; then
  exit 0
fi

(
  cd "${VALIDATION_ROOT}"
  dev/workflows/scripts/sync-local-sdk.sh
)

set +e
validation_run_helmr deploy "${VALIDATION_ROOT}/dev/workflows-failing-build" \
  --project "${VALIDATION_PROJECT}" --env "${VALIDATION_ENVIRONMENT}" \
  --skip-promotion --timeout 20m --json \
  >"${VALIDATION_TMP}/deploy.jsonl" 2>"${VALIDATION_TMP}/deploy.stderr"
deploy_status=$?
set -e
[ "${deploy_status}" -ne 0 ] || {
  validation_write_result failed invalid_build_succeeded
  exit 1
}
deployment_id="$(
  jq -Rr 'fromjson? | select(.type == "deployment_created") | .deployment.id' \
    "${VALIDATION_TMP}/deploy.jsonl" |
    tail -1
)"
validation_require_public_id dep "${deployment_id}" || {
  validation_write_result failed failed_deployment_id_missing
  exit 1
}

validation_db_marker "
  COPY (
    SELECT 'failed-on-build-worker'
      FROM deployments
     WHERE deployments.public_id = '${deployment_id}'
       AND deployments.status = 'failed'
       AND deployments.failure->>'message' LIKE '%intentional build failure%'
       AND EXISTS (
           SELECT 1
             FROM deployment_build_leases lease
             JOIN worker_instances worker
               ON worker.id = lease.worker_instance_id
              AND worker.worker_group_id = lease.worker_group_id
            WHERE lease.deployment_id = deployments.id
              AND lease.state = 'failed'
              AND lease.terminal_reason_code = 'workspace_image_failed'
              AND lease.terminal_error->>'message' LIKE '%intentional build failure%'
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
" failed-on-build-worker || {
  validation_write_result failed build_failure_not_attributed
  exit 1
}

HELMR_SMOKE_RESULT_FILE="${VALIDATION_TMP}/release.json" \
SMOKE_CASES=runtime SKIP_DEPLOY=1 \
  "${VALIDATION_ROOT}/dev/workflows/scripts/run-release-smoke.sh" \
  >"${VALIDATION_TMP}/release.stdout" 2>"${VALIDATION_TMP}/release.stderr"
jq -e '
  .schema == "helmrdotdev.release-smoke-result.v1" and
  .status == "passed" and .executed_cases == ["runtime"]
' "${VALIDATION_TMP}/release.json" >/dev/null || {
  validation_write_result failed promoted_run_failed
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
  build_failure_observed:true,
  build_group_only:true,
  promoted_deployment_unchanged:true,
  run_unaffected:true
}'
