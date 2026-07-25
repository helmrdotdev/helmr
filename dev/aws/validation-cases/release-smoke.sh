#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CASE_JSON="${HELMR_VALIDATION_CASE:?HELMR_VALIDATION_CASE is required}"
RESULT_FILE="${HELMR_VALIDATION_CASE_RESULT_FILE:?HELMR_VALIDATION_CASE_RESULT_FILE is required}"
SMOKE_CASE="$(jq -er '.payload.smokeCase | select(type == "string" and length > 0)' <<<"${CASE_JSON}")"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

set +e
HELMR_SMOKE_RESULT_FILE="${tmp}/result.json" SMOKE_CASES="${SMOKE_CASE}" \
  "${ROOT}/dev/workflows/scripts/run-release-smoke.sh" >"${tmp}/stdout" 2>"${tmp}/stderr"
command_status=$?
set -e

status=failed
reason_json='"release_smoke_failed"'
checks='[]'
run_ids='[]'
if [ -f "${tmp}/result.json" ] && jq -e --arg smoke_case "${SMOKE_CASE}" '
  .schema == "helmrdotdev.release-smoke-result.v1" and .status == "passed" and .exit_code == 0 and
  .selected_cases == [$smoke_case] and .executed_cases == [$smoke_case] and .skipped_cases == []
' "${tmp}/result.json" >/dev/null && [ "${command_status}" = 0 ]; then
  status=passed
  reason_json=null
  checks="$(jq -c --argjson expected "$(jq -c '.producer.checks' <<<"${CASE_JSON}")" '
    [$expected[] | {id:.,status:"passed"}]
  ' <<<"{}")"
  run_ids="$(jq -c '[.run_ids[]?] | unique' "${tmp}/result.json")"
else
  checks="$(jq -c --argjson expected "$(jq -c '.producer.checks' <<<"${CASE_JSON}")" '
    [$expected[] | {id:.,status:"failed"}]
  ' <<<"{}")"
fi

jq -n --arg status "${status}" --argjson reason "${reason_json}" --argjson checks "${checks}" --argjson run_ids "${run_ids}" \
  '{schema:"helmrdotdev.validation-case-source-result.v2",status:$status,reason:$reason,checks:$checks,
    objects:{run_ids:$run_ids,workspace_ids:[],deployment_ids:[],schedule_ids:[],token_ids:[],actor_ids:[]},
    observations:{smoke_case_count:1}}' >"${RESULT_FILE}.tmp"
chmod 0600 "${RESULT_FILE}.tmp"
mv "${RESULT_FILE}.tmp" "${RESULT_FILE}"
[ "${status}" = passed ]
