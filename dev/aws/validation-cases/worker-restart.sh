#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=case-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/case-lib.sh"

workspace_id=""
run_id=""
cleanup() {
  validation_probe_cleanup "${workspace_id}" "${run_id}" || true
  validation_cleanup_tmp
}
trap cleanup EXIT INT TERM

if validation_dry_run; then
  exit 0
fi

marker="worker-restart-${HELMR_VALIDATION_CASE_ATTEMPT:-1}"
payload="$(jq -cn --arg marker "${marker}" '{
  marker:$marker,mode:"hold",delaySeconds:0,holdSeconds:300,
  retryHoldSeconds:2,denyAttempts:1
}')"
IFS=$'\t' read -r workspace_id run_id < <(validation_probe_start "${marker}" "${payload}")
validation_wait_run_status "${run_id}" running 300 >/dev/null

facts="$(
  validation_db_query "
    COPY (
      SELECT 'restart-facts|' || worker_instances.resource_id || '|' ||
             worker_instances.current_epoch::text
        FROM runs
        JOIN run_leases
          ON run_leases.id = runs.current_run_lease_id
         AND run_leases.run_id = runs.id
        JOIN worker_instances
          ON worker_instances.id = run_leases.worker_instance_id
       WHERE runs.public_id = '${run_id}'
         AND run_leases.state = 'running'
    ) TO STDOUT;
  " |
    grep -E '^restart-facts[|]i-[0-9a-f]{8,17}[|][1-9][0-9]*$' |
    tail -1
)"
[ -n "${facts}" ] || {
  validation_write_result failed exact_worker_not_found
  exit 1
}
instance_id="$(cut -d'|' -f2 <<<"${facts}")"
old_epoch="$(cut -d'|' -f3 <<<"${facts}")"

validation_ssm "${instance_id}" \
  "systemctl restart helmr-worker && systemctl is-active --quiet helmr-worker" 180 \
  >/dev/null

advanced=0
for _ in $(seq 1 120); do
  if validation_db_marker "
    COPY (
      SELECT 'epoch-advanced'
        FROM worker_instances
       WHERE resource_id = '${instance_id}'
         AND current_epoch > ${old_epoch}
         AND startup_inventory_epoch = current_epoch
         AND startup_inventory_evidence IS NOT NULL
         AND state = 'active'
    ) TO STDOUT;
  " epoch-advanced; then
    advanced=1
    break
  fi
  sleep 2
done
[ "${advanced}" = 1 ] || {
  validation_write_result failed worker_epoch_not_recovered
  exit 1
}

terminal="$(validation_wait_run_status "${run_id}" succeeded 600)"
attempt="$(jq -er '.current_attempt_number' <<<"${terminal}")"
[ "${attempt}" -ge 2 ] || {
  validation_write_result failed active_run_not_retried
  exit 1
}

objects="$(jq -cn --arg run "${run_id}" --arg workspace "${workspace_id}" '{
  run_ids:[$run],workspace_ids:[$workspace],deployment_ids:[],
  schedule_ids:[],token_ids:[],actor_ids:[]
}')"
observations="$(jq -cn --argjson attempt "${attempt}" '{
  service_restarted:true,
  epoch_advanced:true,
  startup_recovery_recorded:true,
  terminal_attempt:$attempt,
  cleanup_verified:true
}')"
validation_probe_cleanup "${workspace_id}" "${run_id}" || {
  validation_write_result failed cleanup_failed "${objects}"
  exit 1
}
workspace_id=""
run_id=""
validation_write_result passed null "${objects}" "${observations}"
