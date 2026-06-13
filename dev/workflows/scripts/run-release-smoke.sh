#!/usr/bin/env bash
set -euo pipefail

PROJECT="${PROJECT:-helmr}"
STAGING_ENV="${STAGING_ENV:-staging}"
PRODUCTION_ENV="${PRODUCTION_ENV:-production}"
API_URL="${HELMR_API_URL:-https://dev.helmr.dev}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

run_ids=()
helmr_cmd=()

if [ -n "${HELMR_BIN:-}" ]; then
  helmr_cmd=("${HELMR_BIN}")
else
  helmr_cmd=(go run ./cmd/helmr)
fi

run_helmr() {
  HELMR_API_URL="${API_URL}" "${helmr_cmd[@]}" "$@"
}

run_capture_id() {
  local task=$1
  shift
  local output
  if ! output="$(run_helmr run "${task}" "$@" --json)"; then
    return 1
  fi
  printf '%s\n' "${output}" >&2
  printf '%s\n' "${output}" | jq -er '.id // .run.id'
}

inspect_run() {
  local run_id=$1
  run_helmr show "${run_id}"
  run_helmr events "${run_id}"
  run_helmr logs "${run_id}"
}

wait_status() {
  local run_id=$1
  local output
  output="$(run_helmr wait "${run_id}" --json)"
  printf '%s\n' "${output}" >&2
  printf '%s\n' "${output}" | jq -er '.status'
}

expect_run_success() {
  local name=$1
  shift
  local run_id
  local status
  run_id="$(run_capture_id "$@")"
  run_ids+=("${run_id}")
  status="$(wait_status "${run_id}")"
  if [ "${status}" != "succeeded" ]; then
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: expected succeeded, got %s: %s\n' "${name}" "${status}" "${run_id}" >&2
    return 1
  fi
  inspect_run "${run_id}"
  printf 'PASS %s run_id=%s\n' "${name}" "${run_id}"
}

expect_run_rejected() {
  local name=$1
  shift
  local log_file
  log_file="$(mktemp)"
  if run_capture_id "$@" >"${log_file}" 2>&1; then
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
  local run_id
  local status
  run_id="$(run_capture_id "$@")"
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
  printf 'PASS %s failed as expected run_id=%s\n' "${name}" "${run_id}"
}

cd "${ROOT}"

if [ "${SKIP_DEPLOY:-0}" != "1" ]; then
  run_helmr deploy ./dev/workflows --project "${PROJECT}" --env "${STAGING_ENV}" --timeout 20m
  run_helmr deploy ./dev/workflows --project "${PROJECT}" --env "${PRODUCTION_ENV}" --timeout 20m
fi

expect_run_success staging-runtime runtime-smoke \
  --project "${PROJECT}" \
  --env "${STAGING_ENV}" \
  --payload-json '{"scenario":"staging-runtime","expectedEnvironment":"staging"}'

expect_run_success staging-edge-workspace edge-smoke \
  --project "${PROJECT}" \
  --env "${STAGING_ENV}" \
  --payload-json '{"mode":"workspace-overwrite"}'

expect_run_rejected staging-missing-secrets secret-smoke \
  --project "${PROJECT}" \
  --env "${STAGING_ENV}" \
  --payload-json '{"scenario":"staging-missing-secrets","expectedEnvironment":"staging"}'

expect_run_failure staging-invalid-payload runtime-smoke \
  --project "${PROJECT}" \
  --env "${STAGING_ENV}" \
  --payload-json '{"scenario":"bad-payload","unknown":true}'

expect_run_failure staging-expected-error edge-smoke \
  --project "${PROJECT}" \
  --env "${STAGING_ENV}" \
  --payload-json '{"mode":"expected-error"}'

expect_run_success production-secrets secret-smoke \
  --project "${PROJECT}" \
  --env "${PRODUCTION_ENV}" \
  --payload-json '{"scenario":"production-secrets","expectedEnvironment":"production"}'

printf 'release smoke run ids: %s\n' "${run_ids[*]}"
