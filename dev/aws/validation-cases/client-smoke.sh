#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CASE_JSON="${HELMR_VALIDATION_CASE:?HELMR_VALIDATION_CASE is required}"
RESULT_FILE="${HELMR_VALIDATION_CASE_RESULT_FILE:?HELMR_VALIDATION_CASE_RESULT_FILE is required}"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

set +e
SKIP_DEPLOY=1 HELMR_CLIENT_SMOKE_RESULT_FILE="${tmp}/result.json" \
  "${ROOT}/dev/client/scripts/workspace-lifecycle-smoke.sh" \
  >"${tmp}/stdout" 2>"${tmp}/stderr"
command_status=$?
set -e

expected_checks="$(jq -c '.producer.checks | sort' <<<"${CASE_JSON}")"
status=failed
reason_json='"client_smoke_failed"'
checks="$(jq -cn --argjson expected "${expected_checks}" '[$expected[] | {id:.,status:"failed"}]')"
objects='{"run_ids":[],"workspace_ids":[],"deployment_ids":[],"schedule_ids":[],"token_ids":[],"actor_ids":[]}'
observations='{}'
if [ -f "${tmp}/result.json" ] && [ "${command_status}" = 0 ] &&
  jq -e --argjson expected "${expected_checks}" '
    .schema == "helmrdotdev.client-smoke-result.v1" and
    .status == "passed" and .reason == null and
    ([.checks[].id] | sort) as $actual |
    all($expected[]; . as $id | $actual | index($id) != null) and
    all(.checks[]; .status == "passed")
  ' "${tmp}/result.json" >/dev/null; then
  status=passed
  reason_json=null
  checks="$(jq -c --argjson expected "${expected_checks}" '
    [.checks[] | select(.id as $id | $expected | index($id) != null)]
  ' "${tmp}/result.json")"
  objects="$(jq -c '.objects' "${tmp}/result.json")"
  observations="$(jq -c '.observations' "${tmp}/result.json")"
fi

jq -n \
  --arg status "${status}" \
  --argjson reason "${reason_json}" \
  --argjson checks "${checks}" \
  --argjson objects "${objects}" \
  --argjson observations "${observations}" \
  '{schema:"helmrdotdev.validation-case-source-result.v2",status:$status,reason:$reason,
    checks:$checks,objects:$objects,observations:$observations}' >"${RESULT_FILE}.tmp"
chmod 0600 "${RESULT_FILE}.tmp"
mv "${RESULT_FILE}.tmp" "${RESULT_FILE}"
[ "${status}" = passed ]
