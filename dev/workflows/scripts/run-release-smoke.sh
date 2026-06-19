#!/usr/bin/env bash
set -euo pipefail

PROJECT="${PROJECT:-helmr}"
STAGING_ENV="${STAGING_ENV:-staging}"
PRODUCTION_ENV="${PRODUCTION_ENV:-production}"
API_URL="${HELMR_API_URL:-https://dev.helmr.dev}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

session_ids=()
run_ids=()
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
  if ! output="$(run_helmr run "${task}" "$@" --json)"; then
    return 1
  fi
  printf '%s\n' "${output}" >&2
  printf '%s %s\n' \
    "$(printf '%s\n' "${output}" | jq -er '.session.id')" \
    "$(printf '%s\n' "${output}" | jq -er '.run.id')"
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
  printf 'PASS %s session_id=%s run_id=%s\n' "${name}" "${session_id}" "${run_id}"
}

session_run_id() {
  local session_id=$1
  api_json GET "/sessions/${session_id}/runs" | jq -er '.runs[0].run_id'
}

wait_for_session_status() {
  local session_id=$1
  local timeout_seconds="${2:-900}"
  api_json POST "/sessions/${session_id}/wait" "$(jq -nc --argjson timeout "${timeout_seconds}" '{timeout_seconds:$timeout}')" |
    jq -er '.status'
}

expect_session_still_waiting() {
  local session_id=$1
  local timeout_seconds="${2:-5}"
  api_json POST "/sessions/${session_id}/wait" "$(jq -nc --argjson timeout "${timeout_seconds}" '{timeout_seconds:$timeout}')" |
    jq -e '.status == "open" and .timed_out == true' >/dev/null
}

wait_for_channel() {
  local session_id=$1
  local channel=$2
  local direction=$3
  for _ in $(seq 1 120); do
    if api_json GET "/sessions/${session_id}/channels" |
      jq -e --arg channel "${channel}" --arg direction "${direction}" '.channels[] | select(.name == $channel and .direction == $direction)' >/dev/null; then
      return 0
    fi
    sleep 2
  done
  printf 'channel not observed: session_id=%s channel=%s direction=%s\n' "${session_id}" "${channel}" "${direction}" >&2
  return 1
}

create_public_token() {
  local scope_type=$1
  local session_id=$2
  local channel=$3
  local max_uses="${4:-}"
  local correlation_id="${5:-}"
  local body
  body="$(jq -nc \
    --arg type "${scope_type}" \
    --arg session_id "${session_id}" \
    --arg channel "${channel}" \
    --arg correlation_id "${correlation_id}" \
    --arg max_uses "${max_uses}" \
    '{
      scope: {
        type: $type,
        session_id: $session_id,
        channel: $channel
      }
    }
    | if $correlation_id != "" then .scope.correlation_id = $correlation_id else . end
    | if $max_uses != "" then .max_uses = ($max_uses | tonumber) else . end')"
  api_json POST /public-access-tokens "${body}"
}

append_session_input() {
  local session_id=$1
  local channel=$2
  local data=$3
  local correlation_id=$4
  local external_event_id=$5
  local bearer="${6:-${HELMR_API_KEY:?HELMR_API_KEY is required}}"
  local body
  body="$(jq -nc \
    --argjson data "${data}" \
    --arg correlation_id "${correlation_id}" \
    --arg external_event_id "${external_event_id}" \
    '{
      data: $data,
      correlation_id: $correlation_id,
      external_event_id: $external_event_id
    }')"
  api_json POST "/sessions/${session_id}/channels/${channel}/inputs" "${body}" "${bearer}"
}

expect_append_created() {
  local name=$1
  local session_id=$2
  local run_id=$3
  local channel=$4
  local data=$5
  local correlation_id=$6
  local external_event_id=$7
  local bearer="${8:-${HELMR_API_KEY:?HELMR_API_KEY is required}}"
  local response
  local status
  response="$(append_session_input "${session_id}" "${channel}" "${data}" "${correlation_id}" "${external_event_id}" "${bearer}")"
  status="$(printf '%s\n' "${response}" | jq -er '.idempotency_status')"
  if [ "${status}" != "created" ]; then
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: expected created channel append for %s, got %s\n' "${name}" "${external_event_id}" "${status}" >&2
    return 1
  fi
}

expect_workspace_version() {
  local name=$1
  local session_id=$2
  local run_id=$3
  local workspace_json
  local workspace_version
  workspace_json="$(api_json GET "/sessions/${session_id}/workspace")"
  workspace_version="$(printf '%s\n' "${workspace_json}" | jq -er '.current_version_id')" || {
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: completed session did not publish current workspace version\n' "${name}" >&2
    printf '%s\n' "${workspace_json}" >&2
    return 1
  }
  printf '%s\n' "${workspace_version}"
}

expect_public_not_found_status() {
  local name=$1
  local status=$2
  local description=$3
  if [ "${status}" != "404" ]; then
    printf 'FAIL %s: expected 404 for %s, got %s\n' "${name}" "${description}" "${status}" >&2
    return 1
  fi
}

expect_channel_report() {
  local name=$1
  local session_id=$2
  local run_id=$3
  local marker=$4
  local correlation_id=$5
  local response
  response="$(api_json GET "/sessions/${session_id}/channels/channel-input-smoke.report/outputs")"
  if ! printf '%s\n' "${response}" |
    jq -e --arg marker "${marker}" --arg correlation_id "${correlation_id}" \
      '.records[-1].data.ok == true and .records[-1].data.marker == $marker and .records[-1].data.correlationId == $correlation_id and .records[-1].data.message.text == "correlated phase9 answer"' >/dev/null; then
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: channel report evidence did not match expected marker/correlation/message\n' "${name}" >&2
    printf '%s\n' "${response}" >&2
    return 1
  fi
}

expect_public_output_read() {
  local name=$1
  local session_id=$2
  local run_id=$3
  local output_public_token=$4
  local response
  response="$(api_json GET "/sessions/${session_id}/channels/channel-input-smoke.report/outputs" "" "${output_public_token}")"
  if ! printf '%s\n' "${response}" | jq -e '.records | length > 0' >/dev/null; then
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: public output token did not read channel report records\n' "${name}" >&2
    printf '%s\n' "${response}" >&2
    return 1
  fi
}

expect_start_and_wait_success() {
  local name=$1
  local marker
  local response
  local session_id
  local run_id
  local status
  marker="release-smoke-${name}-$(date -u +%Y%m%d%H%M%S)"
  response="$(api_json POST /tasks/runtime-smoke/start-and-wait "$(jq -nc --arg marker "${marker}" '{
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

expect_channel_session_success() {
  local name=$1
  local marker
  local correlation_id="${name}-correlation"
  local ids
  local session_id
  local run_id
  local scope_mismatch_ids
  local scope_mismatch_session_id
  local scope_mismatch_run_id
  local output_scope_mismatch_ids
  local output_scope_mismatch_session_id
  local output_scope_mismatch_run_id
  local token_json
  local input_public_token
  local output_public_token
  local status
  local workspace_version
  marker="release-smoke-${name}-$(date -u +%Y%m%d%H%M%S)"
  ids="$(start_capture_ids channel-input-smoke \
    --payload-json "$(jq -nc --arg marker "${marker}" --arg correlation_id "${correlation_id}" '{
      marker: $marker,
      correlationId: $correlation_id,
      firstTimeout: 300,
      secondTimeout: 300
    }')")"
  session_id="${ids%% *}"
  run_id="${ids##* }"
  session_ids+=("${session_id}")
  run_ids+=("${run_id}")

  wait_for_channel "${session_id}" input-smoke input
  token_json="$(create_public_token session.input.append "${session_id}" input-smoke 2 "${correlation_id}")"
  input_public_token="$(printf '%s\n' "${token_json}" | jq -er '.public_access_token')"
  scope_mismatch_ids="$(start_capture_ids channel-input-smoke \
    --payload-json "$(jq -nc --arg marker "${marker}-scope-mismatch" --arg correlation_id "${correlation_id}" '{
      marker: $marker,
      correlationId: $correlation_id,
      firstTimeout: 600,
      secondTimeout: 600
    }')")"
  scope_mismatch_session_id="${scope_mismatch_ids%% *}"
  scope_mismatch_run_id="${scope_mismatch_ids##* }"
  session_ids+=("${scope_mismatch_session_id}")
  run_ids+=("${scope_mismatch_run_id}")
  wait_for_channel "${scope_mismatch_session_id}" input-smoke input
  status="$(api_status POST "/sessions/${scope_mismatch_session_id}/channels/input-smoke/inputs" \
    "$(jq -nc --arg correlation_id "${correlation_id}" --arg event_id "${marker}-wrong-public-session" '{data:{step:"approve",approved:true}, correlation_id:$correlation_id, external_event_id:$event_id}')" \
    "${input_public_token}")"
  expect_public_not_found_status "${name}" "${status}" "wrong-session public append"
  status="$(api_json POST "/sessions/${scope_mismatch_session_id}/cancel" '{"reason":"phase9 smoke scope isolation probe complete"}' | jq -er '.status')"
  if [ "${status}" != "cancelled" ]; then
    inspect_run "${scope_mismatch_run_id}" >&2
    printf 'FAIL %s: expected cancelled scope-mismatch session, got %s\n' "${name}" "${status}" >&2
    return 1
  fi
  expect_append_created "${name}" "${session_id}" "${run_id}" input-smoke '{"step":"approve","approved":true}' "${correlation_id}-other" "${marker}-wrong-approval"
  expect_session_still_waiting "${session_id}" 5
  status="$(api_status POST "/sessions/${session_id}/channels/input-smoke/inputs" \
    "$(jq -nc --arg correlation_id "${correlation_id}-other" --arg event_id "${marker}-wrong-public-correlation" '{data:{step:"approve",approved:true}, correlation_id:$correlation_id, external_event_id:$event_id}')" \
    "${input_public_token}")"
  expect_public_not_found_status "${name}" "${status}" "wrong-correlation public append"
  expect_append_created "${name}" "${session_id}" "${run_id}" input-smoke '{"step":"approve","approved":true}' "${correlation_id}" "${marker}-approval" "${input_public_token}"
  status="$(api_status POST "/sessions/${session_id}/channels/wrong-channel/inputs" \
    "$(jq -nc --arg correlation_id "${correlation_id}" '{data:{step:"approve",approved:true}, correlation_id:$correlation_id}')" \
    "${input_public_token}")"
  expect_public_not_found_status "${name}" "${status}" "wrong-channel public append"

  expect_append_created "${name}" "${session_id}" "${run_id}" input-smoke '{"step":"message","text":"wrong correlated answer"}' "${correlation_id}-other" "${marker}-wrong-message"
  expect_session_still_waiting "${session_id}" 5
  expect_append_created "${name}" "${session_id}" "${run_id}" input-smoke '{"step":"message","text":"correlated phase9 answer"}' "${correlation_id}" "${marker}-message"
  status="$(wait_for_session_status "${session_id}" 900)"
  if [ "${status}" != "completed" ]; then
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: expected completed session, got %s\n' "${name}" "${status}" >&2
    return 1
  fi
  workspace_version="$(expect_workspace_version "${name}" "${session_id}" "${run_id}")"
  expect_channel_report "${name}" "${session_id}" "${run_id}" "${marker}" "${correlation_id}"

  output_scope_mismatch_ids="$(start_capture_ids channel-input-smoke \
    --payload-json "$(jq -nc --arg marker "${marker}-output-scope-mismatch" --arg correlation_id "${correlation_id}" '{
      marker: $marker,
      correlationId: $correlation_id,
      firstTimeout: 300,
      secondTimeout: 300
    }')")"
  output_scope_mismatch_session_id="${output_scope_mismatch_ids%% *}"
  output_scope_mismatch_run_id="${output_scope_mismatch_ids##* }"
  session_ids+=("${output_scope_mismatch_session_id}")
  run_ids+=("${output_scope_mismatch_run_id}")
  wait_for_channel "${output_scope_mismatch_session_id}" input-smoke input
  expect_append_created "${name}" "${output_scope_mismatch_session_id}" "${output_scope_mismatch_run_id}" input-smoke '{"step":"approve","approved":true}' "${correlation_id}" "${marker}-output-scope-approval"
  expect_append_created "${name}" "${output_scope_mismatch_session_id}" "${output_scope_mismatch_run_id}" input-smoke '{"step":"message","text":"correlated phase9 answer"}' "${correlation_id}" "${marker}-output-scope-message"
  status="$(wait_for_session_status "${output_scope_mismatch_session_id}" 900)"
  if [ "${status}" != "completed" ]; then
    inspect_run "${output_scope_mismatch_run_id}" >&2
    printf 'FAIL %s: expected completed output-scope-mismatch session, got %s\n' "${name}" "${status}" >&2
    return 1
  fi
  expect_channel_report "${name}" "${output_scope_mismatch_session_id}" "${output_scope_mismatch_run_id}" "${marker}-output-scope-mismatch" "${correlation_id}"

  token_json="$(create_public_token session.output.read "${session_id}" channel-input-smoke.report)"
  output_public_token="$(printf '%s\n' "${token_json}" | jq -er '.public_access_token')"
  status="$(api_status GET "/sessions/${output_scope_mismatch_session_id}/channels/channel-input-smoke.report/outputs" "" "${output_public_token}")"
  expect_public_not_found_status "${name}" "${status}" "wrong-session public read"
  expect_public_output_read "${name}" "${session_id}" "${run_id}" "${output_public_token}"
  status="$(api_status GET "/sessions/${session_id}/channels/wrong-channel/outputs" "" "${output_public_token}")"
  expect_public_not_found_status "${name}" "${status}" "wrong-channel public read"

  printf 'PASS %s session_id=%s run_id=%s workspace_version=%s correlation_id=%s\n' \
    "${name}" "${session_id}" "${run_id}" "${workspace_version}" "${correlation_id}"
}

expect_cancel_session() {
  local name=$1
  local marker
  local ids
  local session_id
  local run_id
  local status
  marker="release-smoke-${name}-$(date -u +%Y%m%d%H%M%S)"
  ids="$(start_capture_ids channel-input-smoke \
    --payload-json "$(jq -nc --arg marker "${marker}" '{marker: $marker, firstTimeout: 600, secondTimeout: 600}')")"
  session_id="${ids%% *}"
  run_id="${ids##* }"
  session_ids+=("${session_id}")
  run_ids+=("${run_id}")
  wait_for_channel "${session_id}" input-smoke input
  status="$(api_json POST "/sessions/${session_id}/cancel" '{"reason":"phase9 smoke cancel"}' | jq -er '.status')"
  if [ "${status}" != "cancelled" ]; then
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: expected cancelled session, got %s\n' "${name}" "${status}" >&2
    return 1
  fi
  printf 'PASS %s session_id=%s run_id=%s\n' "${name}" "${session_id}" "${run_id}"
}

expect_waitpoint_success() {
  local name=$1
  local response_payload=$2
  local task=$3
  local payload_json=$4
  shift 4
  local args=("$@")
  local token_project="${PROJECT}"
  local token_env="${STAGING_ENV}"
  local ids
  local session_id
  local run_id
  local marker="release-smoke-${name}"
  local token_json
  local token_id
  local token_public_access_token
  local token_scope_args=()
  local waitpoint_id=""
  local status
  for ((i = 0; i < ${#args[@]}; i++)); do
    case "${args[$i]}" in
      --project|-p)
        if [ $((i + 1)) -lt ${#args[@]} ]; then
          token_project="${args[$((i + 1))]}"
        fi
        ;;
      --env|-e)
        if [ $((i + 1)) -lt ${#args[@]} ]; then
          token_env="${args[$((i + 1))]}"
        fi
        ;;
    esac
  done
  if [ -z "${HELMR_API_KEY:-}" ]; then
    token_scope_args=(--project "${token_project}" --env "${token_env}")
  fi
  token_json="$(
    run_helmr waitpoint token create \
      "${token_scope_args[@]}" \
      --timeout-seconds 300 \
      --metadata "$(jq -nc --arg marker "${marker}" '{marker:$marker, source:"release-smoke"}')"
  )"
  printf '%s\n' "${token_json}" >&2
  token_id="$(printf '%s\n' "${token_json}" | jq -er '.id')"
  token_public_access_token="$(printf '%s\n' "${token_json}" | jq -er '.public_access_token')"
  payload_json="$(
    jq -cn \
      --argjson base "${payload_json}" \
      --arg marker "${marker}" \
      --arg token_id "${token_id}" \
      '$base + {exerciseWaitpoint:true, marker:$marker, waitpointTokenId:$token_id}'
  )"
  ids="$(start_capture_ids "${task}" "${args[@]}" --payload-json "${payload_json}")"
  session_id="${ids%% *}"
  run_id="${ids##* }"
  session_ids+=("${session_id}")
  run_ids+=("${run_id}")
  for _ in $(seq 1 120); do
    waitpoint_id="$(
      run_helmr waitpoint list "${token_scope_args[@]}" --json \
        | jq -r --arg run_id "${run_id}" --arg token_id "${token_id}" 'select(.run_id == $run_id and .params.token_id == $token_id) | .waitpoint_id' \
        | head -n 1
    )"
    if [ -n "${waitpoint_id}" ]; then
      break
    fi
    sleep 2
  done
  if [ -z "${waitpoint_id}" ]; then
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: waitpoint was not observed for run_id=%s\n' "${name}" "${run_id}" >&2
    return 1
  fi
  if [ "${phase9_http_smoke_enabled}" = "1" ]; then
    api_json POST "/waitpoints/tokens/${token_id}/complete" "$(jq -nc --argjson data "${response_payload}" '{data:$data}')" "${token_public_access_token}" >/dev/null
  else
    run_helmr waitpoint token complete "${token_id}" --data "${response_payload}"
  fi
  status="$(wait_status "${run_id}")"
  if [ "${status}" != "succeeded" ]; then
    inspect_run "${run_id}" >&2
    printf 'FAIL %s: expected succeeded, got %s: %s\n' "${name}" "${status}" "${run_id}" >&2
    return 1
  fi
  inspect_run "${run_id}"
  printf 'PASS %s session_id=%s run_id=%s waitpoint_id=%s token_id=%s\n' "${name}" "${session_id}" "${run_id}" "${waitpoint_id}" "${token_id}"
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
  printf 'PASS %s failed as expected run_id=%s\n' "${name}" "${run_id}"
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

  expect_channel_session_success phase9-channel-input

  expect_cancel_session phase9-cancel
else
  printf 'SKIP phase9 HTTP/public-token smoke: HELMR_API_KEY is required for root session-channel API checks\n'
fi

expect_run_success staging-runtime runtime-smoke \
  "${staging_scope_args[@]}" \
  --payload-json '{"scenario":"staging-runtime","expectedEnvironment":"staging"}'

expect_waitpoint_success staging-runtime-waitpoint '{"approved":true,"note":"release smoke"}' runtime-smoke \
  '{"scenario":"staging-runtime-waitpoint","expectedEnvironment":"staging","waitpointTimeout":300}' \
  "${staging_scope_args[@]}"

expect_run_success staging-edge-workspace edge-smoke \
  "${staging_scope_args[@]}" \
  --payload-json '{"mode":"workspace-overwrite"}'

expect_run_rejected staging-missing-secrets secret-smoke \
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
