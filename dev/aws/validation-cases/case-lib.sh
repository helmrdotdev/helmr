#!/usr/bin/env bash
set -euo pipefail

VALIDATION_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
VALIDATION_CASE_JSON="${HELMR_VALIDATION_CASE:?HELMR_VALIDATION_CASE is required}"
VALIDATION_RESULT_FILE="${HELMR_VALIDATION_CASE_RESULT_FILE:?HELMR_VALIDATION_CASE_RESULT_FILE is required}"
VALIDATION_CASE_ID="$(jq -er '.id' <<<"${VALIDATION_CASE_JSON}")"
VALIDATION_PROJECT="$(jq -er '.payload.project // "helmr"' <<<"${VALIDATION_CASE_JSON}")"
VALIDATION_ENVIRONMENT="$(jq -er '.payload.environment // "staging"' <<<"${VALIDATION_CASE_JSON}")"
VALIDATION_TMP="$(mktemp -d)"
VALIDATION_HELMR=()

if [ -n "${HELMR_BIN:-}" ]; then
  VALIDATION_HELMR=("${HELMR_BIN}")
else
  VALIDATION_HELMR=(go run ./cmd/helmr)
fi

validation_cleanup_tmp() {
  rm -rf "${VALIDATION_TMP}"
}

validation_run_helmr() {
  (
    cd "${VALIDATION_ROOT}"
    HELMR_API_URL="${HELMR_API_URL:-https://dev.helmr.dev}" \
      "${VALIDATION_HELMR[@]}" "$@"
  )
}

validation_checks() {
  local status=$1
  jq -c --arg status "${status}" \
    '[.producer.checks[] | {id:.,status:$status}]' <<<"${VALIDATION_CASE_JSON}"
}

validation_empty_objects() {
  printf '%s\n' \
    '{"run_ids":[],"workspace_ids":[],"deployment_ids":[],"schedule_ids":[],"token_ids":[],"actor_ids":[]}'
}

validation_write_result() {
  local status=$1 reason=$2 objects=${3:-"$(validation_empty_objects)"} observations=${4:-'{}'}
  local reason_json
  if [ "${reason}" = "null" ]; then
    reason_json=null
  else
    reason_json="$(jq -Rn --arg value "${reason}" '$value')"
  fi
  jq -n \
    --arg status "${status}" \
    --argjson reason "${reason_json}" \
    --argjson checks "$(validation_checks "${status}")" \
    --argjson objects "${objects}" \
    --argjson observations "${observations}" \
    '{
      schema:"helmrdotdev.validation-case-source-result.v2",
      status:$status,
      reason:$reason,
      checks:$checks,
      objects:$objects,
      observations:$observations
    }' >"${VALIDATION_RESULT_FILE}.tmp"
  chmod 0600 "${VALIDATION_RESULT_FILE}.tmp"
  mv "${VALIDATION_RESULT_FILE}.tmp" "${VALIDATION_RESULT_FILE}"
}

validation_dry_run() {
  [ "${HELMR_VALIDATION_DRY_RUN:-0}" = "1" ] || return 1
  validation_write_result passed null "$(validation_empty_objects)" \
    '{"dry_run":true,"cleanup_verified":true}'
}

validation_require_public_id() {
  local prefix=$1 value=$2
  [[ "${value}" =~ ^${prefix}_[a-z2-7]{26}$ ]]
}

validation_wait_run_status() {
  local run_id=$1 accepted=$2 timeout=${3:-900}
  local deadline=$((SECONDS + timeout)) snapshot status
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    snapshot="$(validation_run_helmr run get "${run_id}" \
      --project "${VALIDATION_PROJECT}" --env "${VALIDATION_ENVIRONMENT}" --json 2>/dev/null)" || {
      sleep 2
      continue
    }
    status="$(jq -er '.status' <<<"${snapshot}")"
    case ",${accepted}," in
      *",${status},"*) printf '%s\n' "${snapshot}"; return 0 ;;
    esac
    case "${status}" in
      succeeded|failed|cancelled|expired|system-failed) return 1 ;;
    esac
    sleep 2
  done
  return 124
}

validation_db_query() {
  "${VALIDATION_ROOT}/dev/aws/db-query.sh" "$1"
}

validation_db_marker() {
  local sql=$1 marker=$2
  validation_db_query "${sql}" | grep -Fqx "${marker}"
}

validation_tf_output() {
  local name=$1
  "${TF_BIN:-tofu}" -chdir="${DEV_STACK:-${VALIDATION_ROOT}/infra/aws/stacks/dev}" \
    output -raw "${name}"
}

validation_single_asg_instance() {
  local asg=$1
  aws autoscaling describe-auto-scaling-groups \
    --auto-scaling-group-names "${asg}" |
    jq -er '
      .AutoScalingGroups
      | select(length == 1)
      | .[0]
      | select(.MaxSize == 1 and .DesiredCapacity <= 1)
      | [.Instances[] | select(.LifecycleState == "InService") | .InstanceId]
      | select(length == 1)
      | .[0]
    '
}

validation_ssm() {
  local instance_id=$1 command=$2 timeout=${3:-120}
  [[ "${instance_id}" =~ ^i-[0-9a-f]{8,17}$ ]] || return 2
  local command_id deadline status output
  command_id="$(
    aws ssm send-command \
      --instance-ids "${instance_id}" \
      --document-name AWS-RunShellScript \
      --parameters "$(jq -cn --arg command "${command}" '{commands:[$command]}')" \
      --query Command.CommandId \
      --output text
  )"
  deadline=$((SECONDS + timeout))
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    output="$(
      aws ssm get-command-invocation \
        --command-id "${command_id}" \
        --instance-id "${instance_id}" 2>/dev/null
    )" || {
      sleep 2
      continue
    }
    status="$(jq -er '.Status' <<<"${output}")"
    case "${status}" in
      Success) jq -r '.StandardOutputContent' <<<"${output}"; return 0 ;;
      Failed|Cancelled|TimedOut) return 1 ;;
    esac
    sleep 2
  done
  return 124
}

validation_probe_start() {
  local marker=$1 payload=$2
  local workspace_response workspace_id run_response run_id
  workspace_response="$(
    validation_run_helmr workspace create helmr-fault-probe \
      --project "${VALIDATION_PROJECT}" --env "${VALIDATION_ENVIRONMENT}" \
      --key "${marker}" --idempotency-key "${marker}:workspace" --json
  )"
  workspace_id="$(jq -er '.workspace_id' <<<"${workspace_response}")"
  validation_require_public_id wsp "${workspace_id}"
  run_response="$(
    validation_run_helmr task start fault-probe \
      --project "${VALIDATION_PROJECT}" --env "${VALIDATION_ENVIRONMENT}" \
      --workspace "${workspace_id}" \
      --idempotency-key "${marker}:run" \
      --payload-json "${payload}" --json
  )"
  run_id="$(jq -er '.run_id' <<<"${run_response}")"
  validation_require_public_id run "${run_id}"
  printf '%s\t%s\n' "${workspace_id}" "${run_id}"
}

validation_probe_cleanup() {
  local workspace_id=${1:-} run_id=${2:-}
  if validation_require_public_id run "${run_id}"; then
    validation_run_helmr run cancel "${run_id}" \
      --project "${VALIDATION_PROJECT}" --env "${VALIDATION_ENVIRONMENT}" \
      --idempotency-key "${VALIDATION_CASE_ID}:cancel" --json >/dev/null 2>&1 || true
  fi
  if validation_require_public_id wsp "${workspace_id}"; then
    validation_run_helmr workspace delete --id "${workspace_id}" \
      --project "${VALIDATION_PROJECT}" --env "${VALIDATION_ENVIRONMENT}" \
      --idempotency-key "${VALIDATION_CASE_ID}:delete" --json >/dev/null
  fi
}
