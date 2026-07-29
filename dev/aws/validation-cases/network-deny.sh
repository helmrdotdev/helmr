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

grep -Eq '169[.]254[.]0[.]0/16' \
  "${DEV_TFVARS:-${VALIDATION_ROOT}/infra/aws/stacks/dev/full-run-smoke.tfvars}" ||
  {
    validation_write_result failed metadata_cidr_not_blocked
    exit 1
  }

marker="network-deny-${HELMR_VALIDATION_CASE_ATTEMPT:-1}"
payload="$(
  jq -cn --arg marker "${marker}" '{
    marker:$marker,
    mode:"network-deny",
    delaySeconds:60,
    holdSeconds:120,
    retryHoldSeconds:2,
    denyAttempts:5
  }'
)"
IFS=$'\t' read -r workspace_id run_id < <(validation_probe_start "${marker}" "${payload}")

facts=""
for _ in $(seq 1 90); do
  facts="$(
    validation_db_query "
      COPY (
        SELECT 'network-facts|' || worker_instances.resource_id || '|' ||
               worker_network_slots.netns_name
          FROM runs
          JOIN run_leases
            ON run_leases.id = runs.current_run_lease_id
           AND run_leases.run_id = runs.id
          JOIN worker_instances
            ON worker_instances.id = run_leases.worker_instance_id
          JOIN worker_network_slots
            ON worker_network_slots.id = run_leases.network_slot_id
           AND worker_network_slots.generation = run_leases.network_slot_generation
           AND worker_network_slots.runtime_instance_id = run_leases.runtime_instance_id
         WHERE runs.id = '${run_id}'
           AND run_leases.state = 'running'
           AND worker_network_slots.state = 'bound'
      ) TO STDOUT;
    " 2>/dev/null |
      grep -E '^network-facts[|]i-[0-9a-f]{8,17}[|][0-9a-f-]{36}$' |
      tail -1
  )" || facts=""
  [ -n "${facts}" ] && break
  sleep 2
done
[ -n "${facts}" ] || {
  validation_write_result failed exact_runtime_not_found
  exit 1
}
instance_id="$(cut -d'|' -f2 <<<"${facts}")"
netns_name="$(cut -d'|' -f3 <<<"${facts}")"
[[ "${netns_name}" =~ ^[0-9a-f-]{36}$ ]] || {
  validation_write_result failed invalid_runtime_identity
  exit 1
}

counter_command="ip netns exec ${netns_name} nft list counter inet helmr_network_policy run_denied | awk '/counter run_denied/ {for (i=1;i<=NF;i++) if (\$i==\"packets\") {print \$(i+1); exit}}'"
baseline="$(validation_ssm "${instance_id}" "${counter_command}" 120)"
[[ "${baseline}" =~ ^[0-9]+$ ]] || {
  validation_write_result failed counter_baseline_unavailable
  exit 1
}
sleep 65
after="$(validation_ssm "${instance_id}" "${counter_command}" 120)"
[[ "${after}" =~ ^[0-9]+$ ]] || {
  validation_write_result failed counter_result_unavailable
  exit 1
}
delta=$((after - baseline))
[ "${delta}" -gt 0 ] || {
  validation_write_result failed deny_counter_unchanged
  exit 1
}

objects="$(jq -cn --arg run "${run_id}" --arg workspace "${workspace_id}" '{
  run_ids:[$run],workspace_ids:[$workspace],deployment_ids:[],
  schedule_ids:[],token_ids:[],actor_ids:[]
}')"
observations="$(jq -cn --argjson delta "${delta}" '{
  denied_packet_delta:$delta,
  exact_runtime_attribution:true,
  metadata_cidr_blocked:true,
  cleanup_verified:true
}')"
validation_probe_cleanup "${workspace_id}" "${run_id}" || {
  validation_write_result failed cleanup_failed "${objects}"
  exit 1
}
workspace_id=""
run_id=""
validation_write_result passed null "${objects}" "${observations}"
