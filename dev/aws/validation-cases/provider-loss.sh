#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=case-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/case-lib.sh"

workspace_id=""
run_id=""
run_asg=""
original_instance=""
cleanup() {
  validation_probe_cleanup "${workspace_id}" "${run_id}" || true
  if [ -n "${run_asg}" ] && [ -n "${original_instance}" ]; then
    for _ in $(seq 1 180); do
      replacement="$(
        aws autoscaling describe-auto-scaling-groups \
          --auto-scaling-group-names "${run_asg}" 2>/dev/null |
          jq -r --arg original "${original_instance}" '
            [.AutoScalingGroups[0].Instances[]?
              | select(.InstanceId != $original and .LifecycleState == "InService")]
            | length
          ' 2>/dev/null
      )" || replacement=0
      [ "${replacement}" -ge 1 ] && break
      sleep 2
    done
  fi
  validation_cleanup_tmp
}
trap cleanup EXIT INT TERM

if validation_dry_run; then
  exit 0
fi

[ "${HELMR_ALLOW_PROVIDER_LOSS:-0}" = 1 ] || {
  validation_write_result failed explicit_provider_loss_approval_required
  exit 1
}

marker="provider-loss-${HELMR_VALIDATION_CASE_ATTEMPT:-1}"
payload="$(jq -cn --arg marker "${marker}" '{
  marker:$marker,mode:"hold",delaySeconds:0,holdSeconds:300,
  retryHoldSeconds:2,denyAttempts:1
}')"
IFS=$'\t' read -r workspace_id run_id < <(validation_probe_start "${marker}" "${payload}")
validation_wait_run_status "${run_id}" running 300 >/dev/null
run_asg="$(validation_tf_output worker_autoscaling_group_name)"
lease_fact="$(
  validation_db_query "
    COPY (
      SELECT 'provider-loss-worker|' || worker_instances.resource_id
        FROM runs
        JOIN run_leases
          ON run_leases.id = runs.current_run_lease_id
         AND run_leases.run_id = runs.id
        JOIN worker_instances
          ON worker_instances.id = run_leases.worker_instance_id
         AND worker_instances.current_epoch = run_leases.worker_epoch
       WHERE runs.public_id = '${run_id}'
         AND run_leases.state = 'running'
         AND worker_instances.state = 'active'
    ) TO STDOUT;
  " |
    grep -E '^provider-loss-worker[|]i-[0-9a-f]{8,17}$' |
    tail -1
)"
[ -n "${lease_fact}" ] || {
  validation_write_result failed exact_worker_not_found
  exit 1
}
original_instance="${lease_fact#*|}"
group_before="$(
  aws autoscaling describe-auto-scaling-groups \
    --auto-scaling-group-names "${run_asg}"
)"
asg_instance="$(
  jq -er '
    .AutoScalingGroups
    | select(length == 1)
    | .[0]
    | select(.MaxSize == 1)
    | [.Instances[] | select(.LifecycleState == "InService") | .InstanceId]
    | select(length == 1)
    | .[0]
  ' <<<"${group_before}"
)"
[ "${asg_instance}" = "${original_instance}" ] || {
  validation_write_result failed run_worker_asg_mismatch
  exit 1
}
original_desired="$(jq -er '.AutoScalingGroups[0].DesiredCapacity' <<<"${group_before}")"

aws ec2 terminate-instances --instance-ids "${original_instance}" >/dev/null

deficit=0
replacement_instance=""
for _ in $(seq 1 300); do
  group="$(
    aws autoscaling describe-auto-scaling-groups \
      --auto-scaling-group-names "${run_asg}"
  )"
  desired="$(jq -er '.AutoScalingGroups[0].DesiredCapacity' <<<"${group}")"
  [ "${desired}" = "${original_desired}" ] || {
    validation_write_result failed desired_capacity_drifted
    exit 1
  }
  in_service="$(jq '[.AutoScalingGroups[0].Instances[]? | select(.LifecycleState == "InService")] | length' <<<"${group}")"
  if [ "${in_service}" -lt "${desired}" ]; then
    deficit=1
  fi
  replacement_instance="$(
    jq -r --arg original "${original_instance}" '
      [.AutoScalingGroups[0].Instances[]?
        | select(.InstanceId != $original and .LifecycleState == "InService")]
      | .[0].InstanceId // ""
    ' <<<"${group}"
  )"
  [ "${deficit}" = 1 ] && [ -n "${replacement_instance}" ] && break
  sleep 2
done
[ "${deficit}" = 1 ] && [ -n "${replacement_instance}" ] || {
  validation_write_result failed replacement_not_ready
  exit 1
}

terminal="$(validation_wait_run_status "${run_id}" succeeded 900)"
attempt="$(jq -er '.current_attempt_number' <<<"${terminal}")"
[ "${attempt}" -ge 2 ] || {
  validation_write_result failed lost_run_not_retried
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
observations="$(jq -cn --argjson attempt "${attempt}" '{
  instance_loss_observed:true,
  capacity_deficit_visible:true,
  replacement_ready:true,
  run_attempts:$attempt,
  desired_capacity_unchanged:true,
  cleanup_verified:true
}')"
validation_write_result passed null "${objects}" "${observations}"
