#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=case-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/case-lib.sh"

first_workspace=""
first_run=""
second_workspace=""
second_run=""
cleanup() {
  validation_probe_cleanup "${second_workspace}" "${second_run}" || true
  validation_probe_cleanup "${first_workspace}" "${first_run}" || true
  validation_cleanup_tmp
}
trap cleanup EXIT INT TERM

if validation_dry_run; then
  exit 0
fi

first_marker="queue-first-${HELMR_VALIDATION_CASE_ATTEMPT:-1}"
second_marker="queue-second-${HELMR_VALIDATION_CASE_ATTEMPT:-1}"
first_payload="$(jq -cn --arg marker "${first_marker}" '{
  marker:$marker,mode:"hold",delaySeconds:0,holdSeconds:300,
  retryHoldSeconds:2,denyAttempts:1
}')"
second_payload="$(jq -cn --arg marker "${second_marker}" '{
  marker:$marker,mode:"hold",delaySeconds:0,holdSeconds:2,
  retryHoldSeconds:2,denyAttempts:1
}')"
IFS=$'\t' read -r first_workspace first_run < <(
  validation_probe_start "${first_marker}" "${first_payload}"
)
validation_wait_run_status "${first_run}" running 300 >/dev/null
IFS=$'\t' read -r second_workspace second_run < <(
  validation_probe_start "${second_marker}" "${second_payload}"
)
validation_wait_run_status "${second_run}" queued 300 >/dev/null

facts="$(
  validation_db_query "
    COPY (
      SELECT 'queue-facts|' || worker_instances.resource_id
        FROM runs
        JOIN run_leases
          ON run_leases.id = runs.current_run_lease_id
         AND run_leases.run_id = runs.id
        JOIN worker_instances
          ON worker_instances.id = run_leases.worker_instance_id
       WHERE runs.public_id = '${first_run}'
         AND run_leases.state = 'running'
    ) TO STDOUT;
  " | grep -E '^queue-facts[|]i-[0-9a-f]{8,17}$' | tail -1
)"
[ -n "${facts}" ] || {
  validation_write_result failed exact_worker_not_found
  exit 1
}
instance_id="${facts#*|}"
validation_ssm "${instance_id}" \
  "systemctl restart helmr-worker && systemctl is-active --quiet helmr-worker" 180 \
  >/dev/null

first_terminal="$(validation_wait_run_status "${first_run}" succeeded 600)"
second_terminal="$(validation_wait_run_status "${second_run}" succeeded 600)"
first_attempt="$(jq -er '.current_attempt_number' <<<"${first_terminal}")"
second_attempt="$(jq -er '.current_attempt_number' <<<"${second_terminal}")"
[ "${first_attempt}" -ge 2 ] || {
  validation_write_result failed active_run_not_retried
  exit 1
}
[ "${second_attempt}" -eq 1 ] || {
  validation_write_result failed queued_run_duplicated
  exit 1
}

objects="$(jq -cn \
  --arg first_run "${first_run}" --arg second_run "${second_run}" \
  --arg first_workspace "${first_workspace}" --arg second_workspace "${second_workspace}" '{
  run_ids:[$first_run,$second_run],
  workspace_ids:[$first_workspace,$second_workspace],
  deployment_ids:[],schedule_ids:[],token_ids:[],actor_ids:[]
}')"
observations="$(jq -cn \
  --argjson first_attempt "${first_attempt}" \
  --argjson second_attempt "${second_attempt}" '{
  queued_before_fault:true,
  service_restarted:true,
  active_attempts:$first_attempt,
  queued_attempts:$second_attempt,
  cleanup_verified:true
}')"
validation_probe_cleanup "${second_workspace}" "${second_run}" || {
  validation_write_result failed cleanup_failed "${objects}"
  exit 1
}
second_workspace=""
second_run=""
validation_probe_cleanup "${first_workspace}" "${first_run}" || {
  validation_write_result failed cleanup_failed "${objects}"
  exit 1
}
first_workspace=""
first_run=""
validation_write_result passed null "${objects}" "${observations}"
