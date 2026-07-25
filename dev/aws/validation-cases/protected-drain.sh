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

marker="protected-drain-${HELMR_VALIDATION_CASE_ATTEMPT:-1}"
payload="$(jq -cn --arg marker "${marker}" '{
  marker:$marker,mode:"hold",delaySeconds:0,holdSeconds:20,
  retryHoldSeconds:2,denyAttempts:1
}')"
IFS=$'\t' read -r workspace_id run_id < <(validation_probe_start "${marker}" "${payload}")
validation_wait_run_status "${run_id}" running 300 >/dev/null
run_asg="$(validation_tf_output worker_autoscaling_group_name)"
instance_id="$(validation_single_asg_instance "${run_asg}")"
aws autoscaling describe-auto-scaling-groups \
  --auto-scaling-group-names "${run_asg}" |
  jq -e --arg instance "${instance_id}" '
    any(.AutoScalingGroups[0].Instances[]?;
      .InstanceId == $instance and .ProtectedFromScaleIn == true)
  ' >/dev/null || {
    validation_write_result failed scale_in_protection_missing
    exit 1
  }

validation_wait_run_status "${run_id}" succeeded 300 >/dev/null

drained=0
for _ in $(seq 1 420); do
  if validation_db_marker "
    COPY (
      SELECT 'drain-complete'
        FROM worker_instances
       WHERE resource_id = '${instance_id}'
         AND state = 'disabled'
         AND drain_cleanup_fingerprint IS NOT NULL
         AND drain_cleanup_evidence IS NOT NULL
         AND NOT EXISTS (
           SELECT 1
             FROM run_leases
            WHERE run_leases.worker_instance_id = worker_instances.id
              AND run_leases.state IN ('assigned','starting','running')
         )
    ) TO STDOUT;
  " drain-complete; then
    live="$(
      aws autoscaling describe-auto-scaling-groups \
        --auto-scaling-group-names "${run_asg}" |
        jq -r --arg instance "${instance_id}" '
          [.AutoScalingGroups[0].Instances[]? | select(.InstanceId == $instance)]
          | length
        '
    )"
    if [ "${live}" = 0 ]; then
      drained=1
      break
    fi
  fi
  sleep 2
done
[ "${drained}" = 1 ] || {
  validation_write_result failed protected_drain_not_observed
  exit 1
}

objects="$(jq -cn --arg run "${run_id}" --arg workspace "${workspace_id}" '{
  run_ids:[$run],workspace_ids:[$workspace],deployment_ids:[],
  schedule_ids:[],token_ids:[],actor_ids:[]
}')"
validation_probe_cleanup "${workspace_id}" "${run_id}" || {
  validation_write_result failed cleanup_failed "${objects}"
  exit 1
}
workspace_id=""
run_id=""
validation_write_result passed null "${objects}" '{
  scale_in_protection_held:true,
  zero_authority_proved:true,
  drain_cleanup_recorded:true,
  termination_after_drain:true,
  direct_termination_used:false,
  cleanup_verified:true
}'
