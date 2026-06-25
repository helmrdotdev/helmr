#!/usr/bin/env bash
set -euo pipefail

PROJECT="${PROJECT:-helmr}"
STAGING_ENV="${STAGING_ENV:-staging}"
PRODUCTION_ENV="${PRODUCTION_ENV:-production}"
API_URL="${HELMR_API_URL:-https://dev.helmr.dev}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
TOKEN_CHECKPOINT_OUTPUT_TIMEOUT_SECONDS="${TOKEN_CHECKPOINT_OUTPUT_TIMEOUT_SECONDS:-420}"

session_ids=()
run_ids=()
stopped_workspace_ids=()
helmr_cmd=()
staging_scope_args=()
production_scope_args=()
skip_production="${SKIP_PRODUCTION:-}"
phase9_http_smoke_enabled=0

if [ -n "${HELMR_BIN:-}" ]; then
  helmr_cmd=("${HELMR_BIN}")
else
  helmr_cmd=(go run ./cmd/helmr)
fi

if [ -z "${HELMR_API_KEY:-}" ]; then
  staging_scope_args=(--project "${PROJECT}" --env "${STAGING_ENV}")
  production_scope_args=(--project "${PROJECT}" --env "${PRODUCTION_ENV}")
else
  skip_production="${skip_production:-1}"
  phase9_http_smoke_enabled=1
fi

run_helmr() {
  HELMR_API_URL="${API_URL}" "${helmr_cmd[@]}" "$@"
}

api_url() {
  printf '%s/api%s' "${API_URL%/}" "$1"
}

api_json() {
  local method=$1
  local path=$2
  local body="${3:-}"
  local bearer="${4:-${HELMR_API_KEY:?HELMR_API_KEY is required}}"
  if [ "${method}" = "GET" ]; then
    curl -fsS \
      -H "authorization: Bearer ${bearer}" \
      -H "accept: application/json" \
      "$(api_url "${path}")"
  else
    curl -fsS \
      -X "${method}" \
      -H "authorization: Bearer ${bearer}" \
      -H "accept: application/json" \
      -H "content-type: application/json" \
      --data "${body}" \
      "$(api_url "${path}")"
  fi
}

api_status() {
  local method=$1
  local path=$2
  local body="${3:-}"
  local bearer="${4:-${HELMR_API_KEY:?HELMR_API_KEY is required}}"
  local log_file
  local status
  log_file="$(mktemp)"
  if [ "${method}" = "GET" ]; then
    status="$(curl -sS -o "${log_file}" -w '%{http_code}' \
      -H "authorization: Bearer ${bearer}" \
      -H "accept: application/json" \
      "$(api_url "${path}")")"
  else
    status="$(curl -sS -o "${log_file}" -w '%{http_code}' \
      -X "${method}" \
      -H "authorization: Bearer ${bearer}" \
      -H "accept: application/json" \
      -H "content-type: application/json" \
      --data "${body}" \
      "$(api_url "${path}")")"
  fi
  cat "${log_file}" >&2
  rm -f "${log_file}"
  printf '%s\n' "${status}"
}

start_capture_ids() {
  local task=$1
  shift
  local output
  if ! output="$(run_helmr session start "${task}" "$@" --json)"; then
    return 1
  fi
  printf '%s\n' "${output}" >&2
  printf '%s %s\n' \
    "$(printf '%s\n' "${output}" | jq -er '.session.id')" \
    "$(printf '%s\n' "${output}" | jq -er '.run.id')"
}

inspect_run() {
  local run_id=$1
  run_helmr run get "${run_id}"
  run_helmr run events "${run_id}"
  run_helmr run logs "${run_id}"
}

stop_session_workspace() {
  local session_id=$1
  shift
  local session_json
  local workspace_id
  local scope_args=()
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --project|-p|--env|-e)
        scope_args+=("$1" "$2")
        shift 2
        ;;
      *)
        shift
        ;;
    esac
  done
  session_json="$(run_helmr session get "${session_id}" "${scope_args[@]}" --json)"
  workspace_id="$(printf '%s\n' "${session_json}" | jq -er '.workspace_id')"
  run_helmr workspace stop "${workspace_id}" "${scope_args[@]}" \
    --idempotency-key "release-smoke:${session_id}:workspace-stop" \
    --json
  stopped_workspace_ids+=("${workspace_id}")
}

wait_status() {
  local run_id=$1
  local output
  output="$(run_helmr run wait "${run_id}" --json)"
  printf '%s\n' "${output}" >&2
  printf '%s\n' "${output}" | jq -er '.status'
}

expect_run_success() {
  local name=$1
  shift
  local ids
  local session_id
  local run_id
  local status
  ids="$(start_capture_ids "$@")"
  session_id="${ids%% *}"
  run_id="${ids##* }"
  session_ids+=("${session_id}")
  run_ids+=("${run_id}")
  status="$(wait_status "${run_id}")"
  if [ "${status}" != "succeeded" ]; then
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: expected succeeded, got %s: %s\n' "${name}" "${status}" "${run_id}" >&2
    return 1
  fi
  inspect_run "${run_id}"
  stop_session_workspace "${session_id}" "$@"
  printf 'PASS %s session_id=%s run_id=%s\n' "${name}" "${session_id}" "${run_id}"
}

session_run_id() {
  local session_id=$1
  shift
  run_helmr run list --session "${session_id}" "$@" --json | jq -er '.runs[0].id'
}

expect_workspace_version() {
  local name=$1
  local session_id=$2
  local run_id=$3
  local session_json
  local workspace_json
  local workspace_id
  local workspace_version
  session_json="$(api_json GET "/sessions/${session_id}")"
  workspace_id="$(printf '%s\n' "${session_json}" | jq -er '.workspace_id')" || {
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: completed session did not expose workspace_id\n' "${name}" >&2
    printf '%s\n' "${session_json}" >&2
    return 1
  }
  workspace_json="$(api_json GET "/workspaces/${workspace_id}")"
  workspace_version="$(printf '%s\n' "${workspace_json}" | jq -er '.workspace.current_version_id')" || {
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: completed session workspace did not publish current workspace version\n' "${name}" >&2
    printf '%s\n' "${workspace_json}" >&2
    return 1
  }
  printf '%s\n' "${workspace_version}"
}

expect_start_and_wait_success() {
  local name=$1
  local marker
  local response
  local session_id
  local run_id
  local status
  marker="release-smoke-${name}-$(date -u +%Y%m%d%H%M%S)"
  response="$(api_json POST /sessions/start-and-wait "$(jq -nc --arg marker "${marker}" '{
    task_id: "runtime-smoke",
    payload: {
      scenario: "start-and-wait",
      marker: $marker,
      expectedEnvironment: "staging"
    },
    timeout_seconds: 900
  }')")"
  printf '%s\n' "${response}" >&2
  status="$(printf '%s\n' "${response}" | jq -er '.status')"
  if [ "${status}" != "completed" ]; then
    printf 'FAIL %s: expected completed, got %s\n' "${name}" "${status}" >&2
    return 1
  fi
  session_id="$(printf '%s\n' "${response}" | jq -er '.id')"
  run_id="$(session_run_id "${session_id}")"
  session_ids+=("${session_id}")
  run_ids+=("${run_id}")
  printf 'PASS %s session_id=%s run_id=%s\n' "${name}" "${session_id}" "${run_id}"
}

expect_run_rejected() {
  local name=$1
  shift
  local log_file
  log_file="$(mktemp)"
  if start_capture_ids "$@" >"${log_file}" 2>&1; then
    cat "${log_file}" >&2
    rm -f "${log_file}"
    printf 'FAIL %s: command unexpectedly succeeded\n' "${name}" >&2
    return 1
  fi
  cat "${log_file}"
  rm -f "${log_file}"
  printf 'PASS %s rejected before run creation\n' "${name}"
}

expect_run_failure() {
  local name=$1
  shift
  local ids
  local session_id
  local run_id
  local status
  ids="$(start_capture_ids "$@")"
  session_id="${ids%% *}"
  run_id="${ids##* }"
  session_ids+=("${session_id}")
  run_ids+=("${run_id}")
  status="$(wait_status "${run_id}")"
  if [ "${status}" = "succeeded" ]; then
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: run unexpectedly succeeded: %s\n' "${name}" "${run_id}" >&2
    return 1
  fi
  if [ "${status}" != "failed" ]; then
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: expected failed, got %s: %s\n' "${name}" "${status}" "${run_id}" >&2
    return 1
  fi
  inspect_run "${run_id}"
  stop_session_workspace "${session_id}" "$@"
  printf 'PASS %s failed as expected run_id=%s\n' "${name}" "${run_id}"
}

wait_for_token_checkpoint_token() {
  local session_id=$1
  local marker=$2
  local step=$3
  shift 3
  local output
  local token_id
  for _ in $(seq 1 "${TOKEN_CHECKPOINT_OUTPUT_TIMEOUT_SECONDS}"); do
    if output="$(run_helmr session stream output list "${session_id}" token-checkpoint-smoke.tokens "$@" --json 2>/dev/null)"; then
      token_id="$(
        printf '%s\n' "${output}" |
          jq -er --arg marker "${marker}" --arg step "${step}" '
            .records[]
            | select(.data.marker == $marker and .data.step == $step)
            | .data.tokenId
          ' 2>/dev/null | tail -n 1
      )" || token_id=""
      if [ -n "${token_id}" ]; then
        printf '%s\n' "${token_id}"
        return 0
      fi
    fi
    sleep 1
  done
  inspect_run "$(session_run_id "${session_id}" "$@")" >&2 || true
  printf 'FAIL token-checkpoint: timed out waiting for %s token output in session %s\n' "${step}" "${session_id}" >&2
  return 1
}

wait_for_stream_phase() {
  local session_id=$1
  local stream=$2
  local marker=$3
  local phase=$4
  shift 4
  local timeout_seconds="${STREAM_PHASE_TIMEOUT_SECONDS:-420}"
  local output
  for _ in $(seq 1 "${timeout_seconds}"); do
    if output="$(run_helmr session stream output list "${session_id}" "${stream}" "$@" --json 2>/dev/null)"; then
      if printf '%s\n' "${output}" |
        jq -e --arg marker "${marker}" --arg phase "${phase}" '
          .records[]
          | select(.data.marker == $marker and .data.phase == $phase)
        ' >/dev/null 2>&1; then
        return 0
      fi
    fi
    sleep 1
  done
  inspect_run "$(session_run_id "${session_id}" "$@")" >&2 || true
  printf 'FAIL stream phase: timed out waiting for %s on %s in session %s\n' "${phase}" "${stream}" "${session_id}" >&2
  return 1
}

expect_active_stream_success() {
  local name=$1
  shift
  local marker
  local correlation_id
  local ids
  local session_id
  local run_id
  local status
  marker="release-smoke-${name}-$(date -u +%Y%m%d%H%M%S)"
  correlation_id="${marker}-corr"
  ids="$(start_capture_ids active-stream-smoke "$@" --payload-json "$(jq -nc --arg marker "${marker}" --arg correlationId "${correlation_id}" '{marker:$marker,correlationId:$correlationId,timeout:300}')")"
  session_id="${ids%% *}"
  run_id="${ids##* }"
  session_ids+=("${session_id}")
  run_ids+=("${run_id}")

  wait_for_stream_phase "${session_id}" active-stream-smoke.report "${marker}" ready-for-empty-peek "$@"
  wait_for_stream_phase "${session_id}" active-stream-smoke.report "${marker}" ready-for-once "$@"
  run_helmr session stream input send "${session_id}" active-stream-smoke.input "$@" \
    --correlation-id "${correlation_id}" \
    --idempotency-key "${marker}:once" \
    --data-json "$(jq -nc --arg value "once" '{step:"once",value:$value}')"
  wait_for_stream_phase "${session_id}" active-stream-smoke.report "${marker}" ready-for-on "$@"
  run_helmr session stream input send "${session_id}" active-stream-smoke.input "$@" \
    --correlation-id "${correlation_id}" \
    --idempotency-key "${marker}:on-one" \
    --data-json "$(jq -nc --arg value "on-one" '{step:"on-one",value:$value}')"
  run_helmr session stream input send "${session_id}" active-stream-smoke.input "$@" \
    --correlation-id "${correlation_id}" \
    --idempotency-key "${marker}:on-two" \
    --data-json "$(jq -nc --arg value "on-two" '{step:"on-two",value:$value}')"

  status="$(wait_status "${run_id}")"
  if [ "${status}" != "succeeded" ]; then
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: expected succeeded, got %s: %s\n' "${name}" "${status}" "${run_id}" >&2
    return 1
  fi
  inspect_run "${run_id}"
  run_helmr session stream output list "${session_id}" active-stream-smoke.report "$@" --json
  stop_session_workspace "${session_id}" "$@"
  printf 'PASS %s session_id=%s run_id=%s\n' "${name}" "${session_id}" "${run_id}"
}

expect_stream_input_success() {
  local name=$1
  shift
  local marker
  local correlation_id
  local ids
  local session_id
  local run_id
  local status
  marker="release-smoke-${name}-$(date -u +%Y%m%d%H%M%S)"
  correlation_id="${marker}-corr"
  ids="$(start_capture_ids stream-input-smoke "$@" --payload-json "$(jq -nc --arg marker "${marker}" --arg correlationId "${correlation_id}" '{marker:$marker,correlationId:$correlationId,firstTimeout:300,secondTimeout:300}')")"
  session_id="${ids%% *}"
  run_id="${ids##* }"
  session_ids+=("${session_id}")
  run_ids+=("${run_id}")
  sleep 5
  run_helmr session stream input send "${session_id}" input-smoke "$@" \
    --correlation-id "${correlation_id}" \
    --idempotency-key "${marker}:approval" \
    --data-json '{"step":"approve","approved":true}'
  sleep 2
  run_helmr session stream input send "${session_id}" input-smoke "$@" \
    --correlation-id "${correlation_id}" \
    --idempotency-key "${marker}:message" \
    --data-json "$(jq -nc --arg text "hello ${marker}" '{step:"message",text:$text}')"
  status="$(wait_status "${run_id}")"
  if [ "${status}" != "succeeded" ]; then
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: expected succeeded, got %s: %s\n' "${name}" "${status}" "${run_id}" >&2
    return 1
  fi
  inspect_run "${run_id}"
  run_helmr session stream output list "${session_id}" stream-input-smoke.report "$@" --json
  stop_session_workspace "${session_id}" "$@"
  printf 'PASS %s session_id=%s run_id=%s\n' "${name}" "${session_id}" "${run_id}"
}

expect_token_checkpoint_success() {
  local name=$1
  shift
  local marker
  local ids
  local session_id
  local run_id
  local token_id
  local status
  marker="release-smoke-${name}-$(date -u +%Y%m%d%H%M%S)"
  ids="$(start_capture_ids token-checkpoint-smoke "$@" --payload-json "$(jq -nc --arg marker "${marker}" '{marker:$marker,approvalTimeout:300,messageTimeout:300}')")"
  session_id="${ids%% *}"
  run_id="${ids##* }"
  session_ids+=("${session_id}")
  run_ids+=("${run_id}")

  token_id="$(wait_for_token_checkpoint_token "${session_id}" "${marker}" decision "$@")"
  run_helmr token complete "${token_id}" "$@" --data-json '{"approved":true}'
  token_id="$(wait_for_token_checkpoint_token "${session_id}" "${marker}" reply "$@")"
  run_helmr token complete "${token_id}" "$@" --data-json "$(jq -nc --arg text "checkpoint ${marker}" '{text:$text}')"

  status="$(wait_status "${run_id}")"
  if [ "${status}" != "succeeded" ]; then
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: expected succeeded, got %s: %s\n' "${name}" "${status}" "${run_id}" >&2
    return 1
  fi
  inspect_run "${run_id}"
  stop_session_workspace "${session_id}" "$@"
  printf 'PASS %s session_id=%s run_id=%s\n' "${name}" "${session_id}" "${run_id}"
}

cd "${ROOT}"

if [ "${SKIP_DEPLOY:-0}" != "1" ]; then
  dev/workflows/scripts/sync-local-sdk.sh
  run_helmr deploy ./dev/workflows "${staging_scope_args[@]}" --timeout 20m
  if [ "${skip_production}" != "1" ]; then
    run_helmr deploy ./dev/workflows "${production_scope_args[@]}" --timeout 20m
  fi
fi

if [ "${phase9_http_smoke_enabled}" = "1" ]; then
  expect_start_and_wait_success phase9-start-and-wait
else
  printf 'SKIP phase9 HTTP smoke: HELMR_API_KEY is required for root API checks\n'
fi

if [ "${SMOKE_ONLY_ACTIVE_STREAM:-0}" = "1" ]; then
  expect_active_stream_success staging-active-stream "${staging_scope_args[@]}"
  printf 'release smoke session ids: %s\n' "${session_ids[*]}"
  printf 'release smoke run ids: %s\n' "${run_ids[*]}"
  printf 'release smoke stopped workspace ids: %s\n' "${stopped_workspace_ids[*]}"
  exit 0
fi

expect_run_success staging-runtime runtime-smoke \
  "${staging_scope_args[@]}" \
  --payload-json '{"scenario":"staging-runtime","expectedEnvironment":"staging"}'

expect_stream_input_success staging-stream-input "${staging_scope_args[@]}"

expect_active_stream_success staging-active-stream "${staging_scope_args[@]}"

expect_run_success staging-timer timer-smoke \
  "${staging_scope_args[@]}" \
  --payload-json '{"waitFor":"5s"}'

expect_token_checkpoint_success staging-token-checkpoint "${staging_scope_args[@]}"

expect_run_success staging-edge-workspace edge-smoke \
  "${staging_scope_args[@]}" \
  --payload-json '{"mode":"workspace-overwrite"}'

expect_run_rejected staging-missing-secrets missing-secret-smoke \
  "${staging_scope_args[@]}" \
  --payload-json '{"scenario":"staging-missing-secrets","expectedEnvironment":"staging"}'

expect_run_failure staging-invalid-payload runtime-smoke \
  "${staging_scope_args[@]}" \
  --payload-json '{"scenario":"bad-payload","unknown":true}'

expect_run_failure staging-expected-error edge-smoke \
  "${staging_scope_args[@]}" \
  --payload-json '{"mode":"expected-error"}'

if [ "${skip_production}" != "1" ]; then
  expect_run_success production-secrets secret-smoke \
    "${production_scope_args[@]}" \
    --payload-json '{"scenario":"production-secrets","expectedEnvironment":"production"}'
fi

printf 'release smoke session ids: %s\n' "${session_ids[*]}"
printf 'release smoke run ids: %s\n' "${run_ids[*]}"
printf 'release smoke stopped workspace ids: %s\n' "${stopped_workspace_ids[*]}"
