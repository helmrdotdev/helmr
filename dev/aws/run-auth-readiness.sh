#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PROJECT="${PROJECT:-helmr}"
STAGING_ENV="${STAGING_ENV:-staging}"
PRODUCTION_ENV="${PRODUCTION_ENV:-production}"
RESULT_FILE="${HELMR_VALIDATION_RESULT_FILE:?HELMR_VALIDATION_RESULT_FILE is required}"
status=failed
reason=auth_readiness_failed
tmp=""

write_result() {
  local command_status=$?
  trap - EXIT
  umask 077
  mkdir -p "$(dirname "${RESULT_FILE}")"
  jq -n \
    --arg status "${status}" \
    --arg reason "${reason}" \
    --arg project "${PROJECT}" \
    --arg staging "${STAGING_ENV}" \
    --arg production "${PRODUCTION_ENV}" \
    --argjson exit_code "${command_status}" '
    {
      schema:"helmrdotdev.validation-stage-result.v1",
      stage:"auth_ready",
      status:$status,
      reason:(if $status == "passed" then null else $reason end),
      observations:(if $status == "passed" then {project_slug:$project,environment_slugs:[$staging,$production],authenticated_cli_probe:true,exit_code:$exit_code} else {} end),
      cases:[]
    }' >"${RESULT_FILE}.tmp"
  chmod 0600 "${RESULT_FILE}.tmp"
  mv "${RESULT_FILE}.tmp" "${RESULT_FILE}"
  [ -z "${tmp}" ] || rm -rf "${tmp}"
  exit "${command_status}"
}
trap write_result EXIT

tmp="$(mktemp -d)"
chmod 0700 "${tmp}"
if [ -n "${HELMR_BIN:-}" ]; then
  helmr_cmd=("${HELMR_BIN}")
else
  helmr_cmd=(go run ./cmd/helmr)
fi

(cd "${ROOT}" && HELMR_API_URL="${HELMR_API_URL:?HELMR_API_URL is required}" \
  "${helmr_cmd[@]}" project list --json) >"${tmp}/projects.json"
(cd "${ROOT}" && HELMR_API_URL="${HELMR_API_URL}" \
  "${helmr_cmd[@]}" env list --project "${PROJECT}" --json) >"${tmp}/environments.json"

jq -e --arg project "${PROJECT}" '
  [.projects[] | select(.slug == $project)] | length == 1
' "${tmp}/projects.json" >/dev/null || { reason=authenticated_project_missing; exit 1; }
jq -e --arg staging "${STAGING_ENV}" --arg production "${PRODUCTION_ENV}" '
  ([.[].slug] | sort) == ([$staging,$production] | sort)
' "${tmp}/environments.json" >/dev/null || { reason=authenticated_environments_missing; exit 1; }

status=passed
reason=""
