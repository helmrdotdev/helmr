#!/usr/bin/env bash
set -euo pipefail

PROJECT="${PROJECT:-helmr}"
STAGING_ENV="${STAGING_ENV:-staging}"
PRODUCTION_ENV="${PRODUCTION_ENV:-production}"
API_URL="${HELMR_API_URL:-https://dev.helmr.dev}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
TOKEN_WAIT_TIMEOUT_SECONDS="${TOKEN_WAIT_TIMEOUT_SECONDS:-420}"
TOKEN_DECISION_DELAY_SECONDS="${TOKEN_DECISION_DELAY_SECONDS:-0}"

workspace_ids=()
run_ids=()
deleted_workspace_ids=()
executed_smoke_cases=()
skipped_smoke_cases=()
helmr_cmd=()
staging_scope_args=()
production_scope_args=()
skip_production="${SKIP_PRODUCTION:-}"
selected_smoke_cases="${SMOKE_CASES:-}"
all_smoke_cases=(
  runtime
  token
  timer
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
  local workspace_id="${3:-}"
  local run_id="${4:-}"
  local detail="${5:-}"
  printf 'ux_timing case=%s event=%s at_ms=%s workspace_id=%s run_id=%s detail=%s\n' \
    "${case_name}" "${event}" "$(now_ms)" "${workspace_id}" "${run_id}" "${detail}"
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
  if smoke_case_enabled production-secrets && [ "${skip_production}" = "1" ]; then
    printf 'SMOKE_CASES=production-secrets cannot run while SKIP_PRODUCTION=1; HELMR_API_KEY mode defaults SKIP_PRODUCTION to 1\n' >&2
    return 2
  fi
}

print_smoke_summary() {
  printf 'release smoke workspace ids: %s\n' "${workspace_ids[*]-}"
  printf 'release smoke run ids: %s\n' "${run_ids[*]-}"
  printf 'release smoke deleted workspace ids: %s\n' "${deleted_workspace_ids[*]-}"
  printf 'release smoke executed cases: %s\n' "${executed_smoke_cases[*]-}"
  printf 'release smoke skipped cases: %s\n' "${skipped_smoke_cases[*]-}"
}

write_smoke_result() {
  local command_status=$1
  local result_file="${HELMR_SMOKE_RESULT_FILE:-}"
  local terminal_status="failed"
  [ -n "${result_file}" ] || return 0
  if [ "${command_status}" = "0" ]; then
    terminal_status="passed"
  fi
  mkdir -p "$(dirname "${result_file}")"
  umask 077
  jq -n \
    --arg schema "helmrdotdev.release-smoke-result.v1" \
    --arg status "${terminal_status}" \
    --argjson exit_code "${command_status}" \
    --arg selected "${selected_smoke_cases}" \
    --argjson executed "$(printf '%s\n' "${executed_smoke_cases[@]-}" | jq -Rsc 'split("\n") | map(select(length > 0))')" \
    --argjson skipped "$(printf '%s\n' "${skipped_smoke_cases[@]-}" | jq -Rsc 'split("\n") | map(select(length > 0))')" \
    --argjson workspaces "$(printf '%s\n' "${workspace_ids[@]-}" | jq -Rsc 'split("\n") | map(select(length > 0))')" \
    --argjson runs "$(printf '%s\n' "${run_ids[@]-}" | jq -Rsc 'split("\n") | map(select(length > 0))')" \
    '{schema:$schema,status:$status,exit_code:$exit_code,selected_cases:($selected | split(",") | map(select(length > 0))),executed_cases:$executed,skipped_cases:$skipped,workspace_ids:$workspaces,run_ids:$runs}' \
    >"${result_file}.tmp"
  chmod 0600 "${result_file}.tmp"
  mv "${result_file}.tmp" "${result_file}"
}

smoke_exit() {
  local command_status=$?
  trap - EXIT
  write_smoke_result "${command_status}" || true
  exit "${command_status}"
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

start_capture_ids() {
  local task=$1
  shift
  local declared_id
  local key
  local workspace
  local output
  local scope_args=()
  local secret_args=()
  case "${task}" in
    runtime-smoke) declared_id=helmr-runtime-smoke ;;
    timer-smoke) declared_id=helmr-timer-smoke ;;
    edge-smoke) declared_id=helmr-edge-smoke ;;
    agent-toolchain-smoke) declared_id=helmr-agent-toolchain-smoke ;;
    secret-smoke)
      declared_id=helmr-secret-smoke
      secret_args=(
        --secret-env ANTHROPIC_API_KEY=ANTHROPIC_API_KEY
        --secret-env CURSOR_API_KEY=CURSOR_API_KEY
        --secret-env GITHUB_TOKEN=GITHUB_TOKEN
        --secret-env OPENAI_API_KEY=OPENAI_API_KEY
      )
      ;;
    missing-secret-smoke)
      declared_id=helmr-secret-smoke
      secret_args=(
        --secret-env HELMR_RELEASE_SMOKE_MISSING=HELMR_RELEASE_SMOKE_MISSING
      )
      ;;
    *) printf 'no smoke Workspace declaration for Task %s\n' "${task}" >&2; return 2 ;;
  esac
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --project|-p|--env|-e)
        scope_args+=("$1" "$2")
        shift 2
        ;;
      *)
        break
        ;;
    esac
  done
  key="release-smoke:${task}:$(date -u +%Y%m%d%H%M%S):${RANDOM}"
  workspace="$(run_helmr workspace create "${declared_id}" "${scope_args[@]}" \
    --key "${key}" "${secret_args[@]}" \
    --idempotency-key "${key}:create" --json)"
  printf '%s\n' "${workspace}" >&2
  workspace="$(printf '%s\n' "${workspace}" | jq -er '.workspace_id')"
  if ! output="$(run_helmr run start "${task}" "${scope_args[@]}" "$@" \
    --workspace "${workspace}" --idempotency-key "${key}:run" --json)"; then
    run_helmr workspace delete --id "${workspace}" "${scope_args[@]}" \
      --idempotency-key "${key}:rollback" --json >&2 || true
    return 1
  fi
  printf '%s\n' "${output}" >&2
  printf '%s %s\n' \
    "${workspace}" \
    "$(printf '%s\n' "${output}" | jq -er '.run_id')"
}

inspect_run() {
  local run_id=$1
  shift
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
  run_helmr run get "${run_id}" "${scope_args[@]}"
  run_helmr run events "${run_id}" "${scope_args[@]}"
  run_helmr run logs "${run_id}" "${scope_args[@]}"
}

delete_smoke_workspace() {
  local workspace_id=$1
  shift
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
  run_helmr workspace delete --id "${workspace_id}" "${scope_args[@]}" \
    --idempotency-key "release-smoke:${workspace_id}:delete" \
    --json
  deleted_workspace_ids+=("${workspace_id}")
}

wait_status() {
  local run_id=$1
  shift
  local scope_args=()
  local output
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
  output="$(run_helmr run wait "${run_id}" "${scope_args[@]}" --json)"
  printf '%s\n' "${output}" >&2
  printf '%s\n' "${output}" | jq -er '.status'
}

expect_run_success() {
  local name=$1
  shift
  local ids
  local workspace_id
  local run_id
  local status
  ids="$(start_capture_ids "$@")"
  workspace_id="${ids%% *}"
  run_id="${ids##* }"
  workspace_ids+=("${workspace_id}")
  run_ids+=("${run_id}")
  status="$(wait_status "${run_id}" "$@")"
  if [ "${status}" != "succeeded" ]; then
    inspect_run "${run_id}" "$@" >&2
    printf 'FAIL %s: expected succeeded, got %s: %s\n' "${name}" "${status}" "${run_id}" >&2
    return 1
  fi
  inspect_run "${run_id}" "$@"
  delete_smoke_workspace "${workspace_id}" "$@"
  printf 'PASS %s workspace_id=%s run_id=%s\n' "${name}" "${workspace_id}" "${run_id}"
}

wait_for_pending_token() {
  local run_id=$1
  shift
  local scope_args=()
  local output
  local token_id
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
  for _ in $(seq 1 "${TOKEN_WAIT_TIMEOUT_SECONDS}"); do
    if output="$(run_helmr run get "${run_id}" "${scope_args[@]}" --json 2>/dev/null)"; then
      token_id="$(
        printf '%s\n' "${output}" |
          jq -er '
            .pending_wait
            | select(.kind == "token" and .status == "pending")
            | .params.token_id
          ' 2>/dev/null
      )" || token_id=""
      if [ -n "${token_id}" ]; then
        printf '%s\n' "${token_id}"
        return 0
      fi
    fi
    sleep 1
  done
  inspect_run "${run_id}" "${scope_args[@]}" >&2 || true
  printf 'FAIL token: timed out waiting for a pending Token on Run %s\n' "${run_id}" >&2
  return 1
}

expect_token_success() {
  local name=$1
  shift
  local marker
  local ids
  local workspace_id
  local run_id
  local token_id
  local status
  marker="release-smoke-${name}-$(date -u +%Y%m%d%H%M%S)"
  ids="$(start_capture_ids runtime-smoke "$@" --payload-json "$(jq -nc --arg marker "${marker}" '{
    scenario: "token",
    marker: $marker,
    expectedEnvironment: "staging",
    exerciseToken: true,
    tokenTimeout: 900
  }')")"
  workspace_id="${ids%% *}"
  run_id="${ids##* }"
  workspace_ids+=("${workspace_id}")
  run_ids+=("${run_id}")
  ux_timing "${name}" "start_returned" "${workspace_id}" "${run_id}" "task=runtime-smoke"
  token_id="$(wait_for_pending_token "${run_id}" "$@")"
  ux_timing "${name}" "token_visible" "${workspace_id}" "${run_id}"
  sleep_seconds "${TOKEN_DECISION_DELAY_SECONDS}"
  run_helmr token complete "${token_id}" "$@" --data-json '{"approved":true,"note":"release smoke"}'
  ux_timing "${name}" "token_complete_accepted" "${workspace_id}" "${run_id}"
  status="$(wait_status "${run_id}" "$@")"
  ux_timing "${name}" "terminal_observed" "${workspace_id}" "${run_id}" "status=${status}"
  if [ "${status}" != "succeeded" ]; then
    inspect_run "${run_id}" "$@" >&2
    printf 'FAIL %s: expected succeeded, got %s: %s\n' "${name}" "${status}" "${run_id}" >&2
    return 1
  fi
  inspect_run "${run_id}" "$@"
  delete_smoke_workspace "${workspace_id}" "$@"
  printf 'PASS %s workspace_id=%s run_id=%s token_id=%s\n' "${name}" "${workspace_id}" "${run_id}" "${token_id}"
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
  local workspace_id
  local run_id
  local status
  ids="$(start_capture_ids "$@")"
  workspace_id="${ids%% *}"
  run_id="${ids##* }"
  workspace_ids+=("${workspace_id}")
  run_ids+=("${run_id}")
  status="$(wait_status "${run_id}" "$@")"
  if [ "${status}" = "succeeded" ]; then
    inspect_run "${run_id}" "$@" >&2
    printf 'FAIL %s: run unexpectedly succeeded: %s\n' "${name}" "${run_id}" >&2
    return 1
  fi
  if [ "${status}" != "failed" ]; then
    inspect_run "${run_id}" "$@" >&2
    printf 'FAIL %s: expected failed, got %s: %s\n' "${name}" "${status}" "${run_id}" >&2
    return 1
  fi
  inspect_run "${run_id}" "$@"
  delete_smoke_workspace "${workspace_id}" "$@"
  printf 'PASS %s failed as expected run_id=%s\n' "${name}" "${run_id}"
}

cd "${ROOT}"
trap smoke_exit EXIT
validate_smoke_cases
validate_selected_smoke_preconditions

if [ "${SKIP_DEPLOY:-0}" != "1" ]; then
  dev/workflows/scripts/sync-local-sdk.sh
  run_helmr deploy ./dev/workflows "${staging_scope_args[@]}" --timeout 20m
  if production_smoke_enabled; then
    run_helmr deploy ./dev/workflows "${production_scope_args[@]}" --timeout 20m
  fi
fi

if smoke_case_enabled runtime; then
  mark_smoke_executed runtime
  expect_run_success staging-runtime runtime-smoke \
    "${staging_scope_args[@]}" \
    --payload-json '{"scenario":"staging-runtime","expectedEnvironment":"staging"}'
fi

if smoke_case_enabled token; then
  mark_smoke_executed token
  expect_token_success staging-token "${staging_scope_args[@]}"
fi

if smoke_case_enabled timer; then
  mark_smoke_executed timer
  expect_run_success staging-timer timer-smoke \
    "${staging_scope_args[@]}" \
    --payload-json '{"waitFor":"5s"}'
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
