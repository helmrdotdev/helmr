#!/usr/bin/env bash
set -euo pipefail

PROJECT="${PROJECT:-helmr}"
STAGING_ENV="${STAGING_ENV:-staging}"
PRODUCTION_ENV="${PRODUCTION_ENV:-production}"
API_URL="${HELMR_API_URL:-https://dev.helmr.dev}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
TOKEN_CHECKPOINT_OUTPUT_TIMEOUT_SECONDS="${TOKEN_CHECKPOINT_OUTPUT_TIMEOUT_SECONDS:-420}"
ACTIVE_STREAM_ONCE_DELAY_SECONDS="${ACTIVE_STREAM_ONCE_DELAY_SECONDS:-0}"
ACTIVE_STREAM_ON_DELAY_SECONDS="${ACTIVE_STREAM_ON_DELAY_SECONDS:-0}"
STREAM_INPUT_APPROVAL_DELAY_SECONDS="${STREAM_INPUT_APPROVAL_DELAY_SECONDS:-5}"
STREAM_INPUT_MESSAGE_DELAY_SECONDS="${STREAM_INPUT_MESSAGE_DELAY_SECONDS:-2}"
TOKEN_CHECKPOINT_DECISION_DELAY_SECONDS="${TOKEN_CHECKPOINT_DECISION_DELAY_SECONDS:-0}"
TOKEN_CHECKPOINT_REPLY_DELAY_SECONDS="${TOKEN_CHECKPOINT_REPLY_DELAY_SECONDS:-0}"

session_ids=()
run_ids=()
stopped_workspace_ids=()
executed_smoke_cases=()
skipped_smoke_cases=()
helmr_cmd=()
staging_scope_args=()
production_scope_args=()
skip_production="${SKIP_PRODUCTION:-}"
phase9_http_smoke_enabled=0
selected_smoke_cases="${SMOKE_CASES:-}"
all_smoke_cases=(
  phase9-start-and-wait
  runtime
  session-continuation
  stream-input
  active-stream
  timer
  token-checkpoint
  edge-workspace
  missing-secrets
  invalid-payload
  expected-error
  production-secrets
)

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

sleep_seconds() {
  local seconds=$1
  if [[ ! "${seconds}" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
    printf 'invalid delay seconds: %s\n' "${seconds}" >&2
    return 2
  fi
  if [[ "${seconds}" =~ ^0+([.]0+)?$ ]]; then
    return 0
  fi
  sleep "${seconds}"
}

now_ms() {
  python3 -c 'import time; print(int(time.time() * 1000))'
}

ux_timing() {
  local case_name=$1
  local event=$2
  local session_id="${3:-}"
  local run_id="${4:-}"
  local detail="${5:-}"
  printf 'ux_timing case=%s event=%s at_ms=%s session_id=%s run_id=%s detail=%s\n' \
    "${case_name}" "${event}" "$(now_ms)" "${session_id}" "${run_id}" "${detail}"
}

mark_smoke_executed() {
  executed_smoke_cases+=("$1")
}

mark_smoke_skipped() {
  skipped_smoke_cases+=("$1")
}

smoke_case_enabled() {
  local name=$1
  if [ -z "${selected_smoke_cases}" ]; then
    return 0
  fi
  case ",${selected_smoke_cases}," in
    *",${name},"*) return 0 ;;
    *) return 1 ;;
  esac
}

validate_smoke_cases() {
  local candidate
  local known
  local matched
  local requested_smoke_cases
  if [ -z "${selected_smoke_cases}" ]; then
    return 0
  fi
  IFS=, read -r -a requested_smoke_cases <<<"${selected_smoke_cases}"
  for candidate in "${requested_smoke_cases[@]}"; do
    matched=0
    for known in "${all_smoke_cases[@]}"; do
      if [ "${candidate}" = "${known}" ]; then
        matched=1
        break
      fi
    done
    if [ "${matched}" != "1" ]; then
      printf 'unknown SMOKE_CASES entry: %s\n' "${candidate}" >&2
      printf 'known SMOKE_CASES entries: %s\n' "${all_smoke_cases[*]}" >&2
      return 2
    fi
  done
}

validate_selected_smoke_preconditions() {
  if [ -z "${selected_smoke_cases}" ]; then
    return 0
  fi
  if smoke_case_enabled phase9-start-and-wait && [ "${phase9_http_smoke_enabled}" != "1" ]; then
    printf 'SMOKE_CASES=phase9-start-and-wait requires HELMR_API_KEY for root API checks\n' >&2
    return 2
  fi
  if smoke_case_enabled production-secrets && [ "${skip_production}" = "1" ]; then
    printf 'SMOKE_CASES=production-secrets cannot run while SKIP_PRODUCTION=1; HELMR_API_KEY mode defaults SKIP_PRODUCTION to 1\n' >&2
    return 2
  fi
}

print_smoke_summary() {
  printf 'release smoke session ids: %s\n' "${session_ids[*]-}"
  printf 'release smoke run ids: %s\n' "${run_ids[*]-}"
  printf 'release smoke stopped workspace ids: %s\n' "${stopped_workspace_ids[*]-}"
  printf 'release smoke executed cases: %s\n' "${executed_smoke_cases[*]-}"
  printf 'release smoke skipped cases: %s\n' "${skipped_smoke_cases[*]-}"
}

production_smoke_enabled() {
  [ "${skip_production}" != "1" ] && smoke_case_enabled production-secrets
}

validate_selected_smoke_execution() {
  if [ -z "${selected_smoke_cases}" ]; then
    return 0
  fi
  if [ "${#skipped_smoke_cases[@]}" -ne 0 ]; then
    printf 'selected smoke cases were skipped: %s\n' "${skipped_smoke_cases[*]}" >&2
    return 2
  fi
  if [ "${#executed_smoke_cases[@]}" -eq 0 ]; then
    printf 'no selected smoke cases executed\n' >&2
    return 2
  fi
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

wait_for_continuation_run() {
  local session_id=$1
  local initial_run_id=$2
  shift 2
  local timeout_seconds="${SESSION_CONTINUATION_TIMEOUT_SECONDS:-420}"
  local output
  local run_id
  for _ in $(seq 1 "${timeout_seconds}"); do
    output="$(run_helmr run list --session "${session_id}" "$@" --json)"
    run_id="$(
      printf '%s\n' "${output}" |
        jq -er --arg initial "${initial_run_id}" '
          .runs[]
          | select(.id != $initial)
          | .id
        ' 2>/dev/null | head -n 1
    )" || run_id=""
    if [ -n "${run_id}" ]; then
      printf '%s\n' "${run_id}"
      return 0
    fi
    sleep 1
  done
  inspect_run "${initial_run_id}" >&2 || true
  printf 'FAIL session-continuation: timed out waiting for continuation run in session %s\n' "${session_id}" >&2
  return 1
}

expect_session_open_idle() {
  local name=$1
  local session_id=$2
  shift 2
  local session_json
  session_json="$(run_helmr session get "${session_id}" "$@" --json)"
  printf '%s\n' "${session_json}" >&2
  if [ "$(printf '%s\n' "${session_json}" | jq -er '.status')" != "open" ]; then
    printf 'FAIL %s: expected session to remain open after terminal run\n' "${name}" >&2
    return 1
  fi
  if [ "$(printf '%s\n' "${session_json}" | jq -er '.activity')" != "idle" ]; then
    printf 'FAIL %s: expected terminal current run to derive idle session activity\n' "${name}" >&2
    return 1
  fi
  if [ "$(printf '%s\n' "${session_json}" | jq -er '.can_close')" != "true" ]; then
    printf 'FAIL %s: expected idle open session to be closable\n' "${name}" >&2
    return 1
  fi
  if [ -z "$(printf '%s\n' "${session_json}" | jq -r '.current_run_id // ""')" ]; then
    printf 'FAIL %s: expected current_run_id to remain as last/current run pointer\n' "${name}" >&2
    return 1
  fi
}

expect_session_continuation_success() {
  local name=$1
  shift
  local marker
  local correlation_id
  local ids
  local session_id
  local initial_run_id
  local continuation_run_id
  local status
  marker="release-smoke-${name}-$(date -u +%Y%m%d%H%M%S)"
  correlation_id="${marker}-corr"
  ux_timing "${name}" "start_requested" "" "" "task=session-continuation-smoke"
  ids="$(start_capture_ids session-continuation-smoke "$@" --payload-json "$(jq -nc --arg marker "${marker}" --arg correlationId "${correlation_id}" '{marker:$marker,correlationId:$correlationId}')")"
  session_id="${ids%% *}"
  initial_run_id="${ids##* }"
  session_ids+=("${session_id}")
  run_ids+=("${initial_run_id}")
  ux_timing "${name}" "start_returned" "${session_id}" "${initial_run_id}" "task=session-continuation-smoke"

  status="$(wait_status "${initial_run_id}")"
  ux_timing "${name}" "initial_terminal_observed" "${session_id}" "${initial_run_id}" "status=${status}"
  if [ "${status}" != "succeeded" ]; then
    inspect_run "${initial_run_id}" >&2
    printf 'FAIL %s: expected initial run succeeded, got %s: %s\n' "${name}" "${status}" "${initial_run_id}" >&2
    return 1
  fi
  expect_session_open_idle "${name}" "${session_id}" "$@"
  ux_timing "${name}" "initial_idle_wait_requested" "${session_id}" "${initial_run_id}" "phase=initial-idle"
  wait_for_stream_phase "${session_id}" session-continuation-smoke.report "${marker}" initial-idle "$@"
  ux_timing "${name}" "initial_idle_visible" "${session_id}" "${initial_run_id}" "phase=initial-idle"

  ux_timing "${name}" "input_send_requested" "${session_id}" "${initial_run_id}" "step=continuation"
  run_helmr session stream input send "${session_id}" session-continuation-smoke.input "$@" \
    --correlation-id "${correlation_id}" \
    --idempotency-key "${marker}:continuation" \
    --data-json "$(jq -nc --arg message "continue ${marker}" '{message:$message}')"
  ux_timing "${name}" "input_send_accepted" "${session_id}" "${initial_run_id}" "step=continuation"

  continuation_run_id="$(wait_for_continuation_run "${session_id}" "${initial_run_id}" "$@")"
  run_ids+=("${continuation_run_id}")
  ux_timing "${name}" "continuation_run_visible" "${session_id}" "${continuation_run_id}" "initial_run_id=${initial_run_id}"
  status="$(wait_status "${continuation_run_id}")"
  ux_timing "${name}" "continuation_terminal_observed" "${session_id}" "${continuation_run_id}" "status=${status}"
  if [ "${status}" != "succeeded" ]; then
    inspect_run "${continuation_run_id}" >&2
    printf 'FAIL %s: expected continuation run succeeded, got %s: %s\n' "${name}" "${status}" "${continuation_run_id}" >&2
    return 1
  fi
  ux_timing "${name}" "continuation_wait_requested" "${session_id}" "${continuation_run_id}" "phase=continuation"
  wait_for_stream_phase "${session_id}" session-continuation-smoke.report "${marker}" continuation "$@"
  ux_timing "${name}" "continuation_visible" "${session_id}" "${continuation_run_id}" "phase=continuation"
  inspect_run "${initial_run_id}"
  inspect_run "${continuation_run_id}"
  run_helmr session stream output list "${session_id}" session-continuation-smoke.report "$@" --json
  stop_session_workspace "${session_id}" "$@"
  printf 'PASS %s session_id=%s initial_run_id=%s continuation_run_id=%s\n' "${name}" "${session_id}" "${initial_run_id}" "${continuation_run_id}"
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
  ux_timing "${name}" "start_requested" "" "" "task=active-stream-smoke"
  ids="$(start_capture_ids active-stream-smoke "$@" --payload-json "$(jq -nc --arg marker "${marker}" --arg correlationId "${correlation_id}" '{marker:$marker,correlationId:$correlationId,timeout:300}')")"
  session_id="${ids%% *}"
  run_id="${ids##* }"
  session_ids+=("${session_id}")
  run_ids+=("${run_id}")
  ux_timing "${name}" "start_returned" "${session_id}" "${run_id}" "task=active-stream-smoke"

  ux_timing "${name}" "phase_wait_requested" "${session_id}" "${run_id}" "phase=ready-for-empty-peek"
  wait_for_stream_phase "${session_id}" active-stream-smoke.report "${marker}" ready-for-empty-peek "$@"
  ux_timing "${name}" "phase_visible" "${session_id}" "${run_id}" "phase=ready-for-empty-peek"
  ux_timing "${name}" "phase_wait_requested" "${session_id}" "${run_id}" "phase=ready-for-once"
  wait_for_stream_phase "${session_id}" active-stream-smoke.report "${marker}" ready-for-once "$@"
  ux_timing "${name}" "phase_visible" "${session_id}" "${run_id}" "phase=ready-for-once"
  sleep_seconds "${ACTIVE_STREAM_ONCE_DELAY_SECONDS}"
  ux_timing "${name}" "input_send_requested" "${session_id}" "${run_id}" "step=once"
  run_helmr session stream input send "${session_id}" active-stream-smoke.input "$@" \
    --correlation-id "${correlation_id}" \
    --idempotency-key "${marker}:once" \
    --data-json "$(jq -nc --arg value "once" '{step:"once",value:$value}')"
  ux_timing "${name}" "input_send_accepted" "${session_id}" "${run_id}" "step=once"
  ux_timing "${name}" "phase_wait_requested" "${session_id}" "${run_id}" "phase=ready-for-on"
  wait_for_stream_phase "${session_id}" active-stream-smoke.report "${marker}" ready-for-on "$@"
  ux_timing "${name}" "phase_visible" "${session_id}" "${run_id}" "phase=ready-for-on"
  sleep_seconds "${ACTIVE_STREAM_ON_DELAY_SECONDS}"
  ux_timing "${name}" "input_send_requested" "${session_id}" "${run_id}" "step=on-one"
  run_helmr session stream input send "${session_id}" active-stream-smoke.input "$@" \
    --correlation-id "${correlation_id}" \
    --idempotency-key "${marker}:on-one" \
    --data-json "$(jq -nc --arg value "on-one" '{step:"on-one",value:$value}')"
  ux_timing "${name}" "input_send_accepted" "${session_id}" "${run_id}" "step=on-one"
  ux_timing "${name}" "input_send_requested" "${session_id}" "${run_id}" "step=on-two"
  run_helmr session stream input send "${session_id}" active-stream-smoke.input "$@" \
    --correlation-id "${correlation_id}" \
    --idempotency-key "${marker}:on-two" \
    --data-json "$(jq -nc --arg value "on-two" '{step:"on-two",value:$value}')"
  ux_timing "${name}" "input_send_accepted" "${session_id}" "${run_id}" "step=on-two"

  status="$(wait_status "${run_id}")"
  ux_timing "${name}" "terminal_observed" "${session_id}" "${run_id}" "status=${status}"
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
  ux_timing "${name}" "start_requested" "" "" "task=stream-input-smoke"
  ids="$(start_capture_ids stream-input-smoke "$@" --payload-json "$(jq -nc --arg marker "${marker}" --arg correlationId "${correlation_id}" '{marker:$marker,correlationId:$correlationId,firstTimeout:300,secondTimeout:300}')")"
  session_id="${ids%% *}"
  run_id="${ids##* }"
  session_ids+=("${session_id}")
  run_ids+=("${run_id}")
  ux_timing "${name}" "start_returned" "${session_id}" "${run_id}" "task=stream-input-smoke"
  sleep_seconds "${STREAM_INPUT_APPROVAL_DELAY_SECONDS}"
  ux_timing "${name}" "input_send_requested" "${session_id}" "${run_id}" "step=approval"
  run_helmr session stream input send "${session_id}" input-smoke "$@" \
    --correlation-id "${correlation_id}" \
    --idempotency-key "${marker}:approval" \
    --data-json '{"step":"approve","approved":true}'
  ux_timing "${name}" "input_send_accepted" "${session_id}" "${run_id}" "step=approval"
  sleep_seconds "${STREAM_INPUT_MESSAGE_DELAY_SECONDS}"
  ux_timing "${name}" "input_send_requested" "${session_id}" "${run_id}" "step=message"
  run_helmr session stream input send "${session_id}" input-smoke "$@" \
    --correlation-id "${correlation_id}" \
    --idempotency-key "${marker}:message" \
    --data-json "$(jq -nc --arg text "hello ${marker}" '{step:"message",text:$text}')"
  ux_timing "${name}" "input_send_accepted" "${session_id}" "${run_id}" "step=message"
  status="$(wait_status "${run_id}")"
  ux_timing "${name}" "terminal_observed" "${session_id}" "${run_id}" "status=${status}"
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
  ux_timing "${name}" "start_requested" "" "" "task=token-checkpoint-smoke"
  ids="$(start_capture_ids token-checkpoint-smoke "$@" --payload-json "$(jq -nc --arg marker "${marker}" '{marker:$marker,approvalTimeout:300,messageTimeout:300}')")"
  session_id="${ids%% *}"
  run_id="${ids##* }"
  session_ids+=("${session_id}")
  run_ids+=("${run_id}")
  ux_timing "${name}" "start_returned" "${session_id}" "${run_id}" "task=token-checkpoint-smoke"

  ux_timing "${name}" "token_wait_requested" "${session_id}" "${run_id}" "step=decision"
  token_id="$(wait_for_token_checkpoint_token "${session_id}" "${marker}" decision "$@")"
  ux_timing "${name}" "token_visible" "${session_id}" "${run_id}" "step=decision"
  sleep_seconds "${TOKEN_CHECKPOINT_DECISION_DELAY_SECONDS}"
  ux_timing "${name}" "token_complete_requested" "${session_id}" "${run_id}" "step=decision"
  run_helmr token complete "${token_id}" "$@" --data-json '{"approved":true}'
  ux_timing "${name}" "token_complete_accepted" "${session_id}" "${run_id}" "step=decision"
  ux_timing "${name}" "token_wait_requested" "${session_id}" "${run_id}" "step=reply"
  token_id="$(wait_for_token_checkpoint_token "${session_id}" "${marker}" reply "$@")"
  ux_timing "${name}" "token_visible" "${session_id}" "${run_id}" "step=reply"
  sleep_seconds "${TOKEN_CHECKPOINT_REPLY_DELAY_SECONDS}"
  ux_timing "${name}" "token_complete_requested" "${session_id}" "${run_id}" "step=reply"
  run_helmr token complete "${token_id}" "$@" --data-json "$(jq -nc --arg text "checkpoint ${marker}" '{text:$text}')"
  ux_timing "${name}" "token_complete_accepted" "${session_id}" "${run_id}" "step=reply"

  status="$(wait_status "${run_id}")"
  ux_timing "${name}" "terminal_observed" "${session_id}" "${run_id}" "status=${status}"
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
validate_smoke_cases
validate_selected_smoke_preconditions

if [ "${SKIP_DEPLOY:-0}" != "1" ]; then
  dev/workflows/scripts/sync-local-sdk.sh
  run_helmr deploy ./dev/workflows "${staging_scope_args[@]}" --timeout 20m
  if production_smoke_enabled; then
    run_helmr deploy ./dev/workflows "${production_scope_args[@]}" --timeout 20m
  fi
fi

if [ "${phase9_http_smoke_enabled}" = "1" ] && smoke_case_enabled phase9-start-and-wait; then
  mark_smoke_executed phase9-start-and-wait
  expect_start_and_wait_success phase9-start-and-wait
elif [ "${phase9_http_smoke_enabled}" != "1" ] && smoke_case_enabled phase9-start-and-wait; then
  printf 'SKIP phase9 HTTP smoke: HELMR_API_KEY is required for root API checks\n'
  mark_smoke_skipped phase9-start-and-wait
fi

if smoke_case_enabled runtime; then
  mark_smoke_executed runtime
  expect_run_success staging-runtime runtime-smoke \
    "${staging_scope_args[@]}" \
    --payload-json '{"scenario":"staging-runtime","expectedEnvironment":"staging"}'
fi

if smoke_case_enabled session-continuation; then
  mark_smoke_executed session-continuation
  expect_session_continuation_success staging-session-continuation "${staging_scope_args[@]}"
fi

if smoke_case_enabled stream-input; then
  mark_smoke_executed stream-input
  expect_stream_input_success staging-stream-input "${staging_scope_args[@]}"
fi

if smoke_case_enabled active-stream; then
  mark_smoke_executed active-stream
  expect_active_stream_success staging-active-stream "${staging_scope_args[@]}"
fi

if smoke_case_enabled timer; then
  mark_smoke_executed timer
  expect_run_success staging-timer timer-smoke \
    "${staging_scope_args[@]}" \
    --payload-json '{"waitFor":"5s"}'
fi

if smoke_case_enabled token-checkpoint; then
  mark_smoke_executed token-checkpoint
  expect_token_checkpoint_success staging-token-checkpoint "${staging_scope_args[@]}"
fi

if smoke_case_enabled edge-workspace; then
  mark_smoke_executed edge-workspace
  expect_run_success staging-edge-workspace edge-smoke \
    "${staging_scope_args[@]}" \
    --payload-json '{"mode":"workspace-overwrite"}'
fi

if smoke_case_enabled missing-secrets; then
  mark_smoke_executed missing-secrets
  expect_run_rejected staging-missing-secrets missing-secret-smoke \
    "${staging_scope_args[@]}" \
    --payload-json '{"scenario":"staging-missing-secrets","expectedEnvironment":"staging"}'
fi

if smoke_case_enabled invalid-payload; then
  mark_smoke_executed invalid-payload
  expect_run_failure staging-invalid-payload runtime-smoke \
    "${staging_scope_args[@]}" \
    --payload-json '{"scenario":"bad-payload","unknown":true}'
fi

if smoke_case_enabled expected-error; then
  mark_smoke_executed expected-error
  expect_run_failure staging-expected-error edge-smoke \
    "${staging_scope_args[@]}" \
    --payload-json '{"mode":"expected-error"}'
fi

if production_smoke_enabled; then
  mark_smoke_executed production-secrets
  expect_run_success production-secrets secret-smoke \
    "${production_scope_args[@]}" \
    --payload-json '{"scenario":"production-secrets","expectedEnvironment":"production"}'
elif smoke_case_enabled production-secrets; then
  mark_smoke_skipped production-secrets
fi

print_smoke_summary
validate_selected_smoke_execution
