#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=case-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/case-lib.sh"
trap validation_cleanup_tmp EXIT INT TERM

if validation_dry_run; then
  exit 0
fi

HELMR_SMOKE_RESULT_FILE="${VALIDATION_TMP}/release.json" \
SMOKE_CASES=child-tasks SKIP_DEPLOY=1 \
  "${VALIDATION_ROOT}/dev/workflows/scripts/run-release-smoke.sh" \
  >"${VALIDATION_TMP}/release.stdout" 2>"${VALIDATION_TMP}/release.stderr"
jq -e '
  .schema == "helmrdotdev.release-smoke-result.v1" and
  .status == "passed" and .executed_cases == ["child-tasks"]
' "${VALIDATION_TMP}/release.json" >/dev/null || {
  validation_write_result failed same_workspace_call_failed
  exit 1
}

SKIP_DEPLOY=1 HELMR_CLIENT_SMOKE_RESULT_FILE="${VALIDATION_TMP}/client.json" \
  "${VALIDATION_ROOT}/dev/client/scripts/workspace-lifecycle-smoke.sh" \
  >"${VALIDATION_TMP}/client.stdout" 2>"${VALIDATION_TMP}/client.stderr"
jq -e '
  .schema == "helmrdotdev.client-smoke-result.v1" and
  .status == "passed" and
  ([.checks[].id] | index("external-token-fanout") != null) and
  all(.checks[]; .status == "passed")
' "${VALIDATION_TMP}/client.json" >/dev/null

client_run_ids="$(jq -c '[.objects.run_ids[]?] | unique' "${VALIDATION_TMP}/client.json")"
release_run_ids="$(jq -c '[.run_ids[]?] | unique' "${VALIDATION_TMP}/release.json")"
run_ids="$(jq -cn --argjson client "${client_run_ids}" --argjson release "${release_run_ids}" '$client + $release | unique')"
run_count="$(jq 'length' <<<"${run_ids}")"
[ "${run_count}" -gt 0 ]
while IFS= read -r run_id; do
  validation_require_public_id run "${run_id}" || {
    validation_write_result failed invalid_client_run_id
    exit 1
  }
done < <(jq -r '.[]' <<<"${run_ids}")
quoted_ids="$(
  jq -r 'map("'"'"'" + . + "'"'"'") | join(",")' <<<"${run_ids}"
)"
validation_db_marker "
  COPY (
    SELECT 'run-group-only'
      FROM runs
     WHERE runs.public_id IN (${quoted_ids})
       AND (
         EXISTS (
           SELECT 1
             FROM run_leases
             JOIN worker_instances
               ON worker_instances.id = run_leases.worker_instance_id
            WHERE run_leases.run_id = runs.id
              AND worker_instances.supports_run
              AND NOT worker_instances.supports_build
         )
         AND NOT EXISTS (
           SELECT 1
             FROM run_leases
             JOIN worker_instances
               ON worker_instances.id = run_leases.worker_instance_id
            WHERE run_leases.run_id = runs.id
              AND NOT (
                worker_instances.supports_run
                AND NOT worker_instances.supports_build
              )
         )
       )
     GROUP BY 1
    HAVING count(*) = ${run_count}
  ) TO STDOUT;
" run-group-only || {
  validation_write_result failed run_placement_not_proved
  exit 1
}

objects="$(jq -c --argjson runs "${run_ids}" '.objects.run_ids = $runs | .objects' "${VALIDATION_TMP}/client.json")"
observations="$(jq -cn --argjson count "${run_count}" '{
  run_completed:true,
  run_group_only:true,
  same_workspace_call_completed:true,
  external_token_wait_completed:true,
  actor_output_observed:true,
  run_count:$count
}')"
validation_write_result passed null "${objects}" "${observations}"
