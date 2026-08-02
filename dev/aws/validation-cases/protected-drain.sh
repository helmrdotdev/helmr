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

control_url="$(validation_tf_output control_url)"
worker_group_id="$(validation_tf_output worker_group_id)"
operator_token_arn="$(
  "${TF_BIN:-tofu}" -chdir="${DEV_STACK:-${VALIDATION_ROOT}/infra/aws/stacks/dev}" \
    output -json secret_arns | jq -er '.operator_token'
)"
operator_token="$(
  aws secretsmanager get-secret-value \
    --secret-id "${operator_token_arn}" \
    --query SecretString \
    --output text
)"
operator_header="${VALIDATION_TMP}/operator-header"
printf 'Authorization: Bearer %s\n' "${operator_token}" >"${operator_header}"
chmod 0600 "${operator_header}"
unset operator_token

instance_json="$(
  curl --fail-with-body --silent --show-error \
    --header "@${operator_header}" \
    --header 'Accept: application/json' \
    "${control_url%/}/api/operator/worker-instances?worker_group_id=$(jq -rn --arg value "${worker_group_id}" '$value|@uri')&limit=500"
)"
instance_json="$(
  jq -cer --arg instance_id "${instance_id}" '
    [.worker_instances[] |
      select(.resource_id == $instance_id and .state == "active")]
    | select(length == 1)
    | .[0]
  ' <<<"${instance_json}"
)" || {
  validation_write_result failed logical_worker_not_resolved
  exit 1
}
worker_instance_id="$(jq -er '.id' <<<"${instance_json}")"
drain_request="${VALIDATION_TMP}/operator-drain-request.json"
jq -n \
  --argjson expected_epoch "$(jq -er '.current_epoch | select(. > 0)' <<<"${instance_json}")" \
  --argjson expected_claim_version "$(jq -er '.claim_version | select(. > 0)' <<<"${instance_json}")" \
  '{expected_epoch:$expected_epoch,expected_claim_version:$expected_claim_version}' >"${drain_request}"
curl --fail-with-body --silent --show-error \
  --request POST \
  --header "@${operator_header}" \
  --header 'Accept: application/json' \
  --header 'Content-Type: application/json' \
  --data-binary "@${drain_request}" \
  "${control_url%/}/api/operator/worker-instances/${worker_instance_id}/drain" >/dev/null || {
  validation_write_result failed exact_drain_request_failed
  exit 1
}

termination_ready=0
for _ in $(seq 1 330); do
  worker_state="$(
    curl --fail-with-body --silent --show-error \
      --header "@${operator_header}" \
      --header 'Accept: application/json' \
      "${control_url%/}/api/operator/worker-instances/${worker_instance_id}" |
      jq -er '.state'
  )" || true
  if [ "${worker_state}" = "termination_ready" ]; then
    termination_ready=1
    break
  fi
  [ "${worker_state}" = "draining" ] || break
  sleep 2
done
[ "${termination_ready}" = 1 ] || {
  validation_write_result failed termination_ready_not_observed
  exit 1
}

validation_db_marker "
  COPY (
    SELECT 'termination-ready'
      FROM worker_instances
     WHERE resource_id = '${instance_id}'
       AND state = 'termination_ready'
       AND termination_ready_at IS NOT NULL
       AND NOT EXISTS (
         SELECT 1
           FROM run_leases
          WHERE run_leases.worker_instance_id = worker_instances.id
            AND run_leases.state IN ('assigned','starting','running','checkpointing','finalizing')
       )
  ) TO STDOUT;
" termination-ready || {
  validation_write_result failed termination_ready_not_observed
  exit 1
}

aws autoscaling terminate-instance-in-auto-scaling-group \
  --instance-id "${instance_id}" \
  --should-decrement-desired-capacity >/dev/null

drained=0
for _ in $(seq 1 420); do
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
  termination_ready_recorded:true,
  termination_after_drain:true,
  exact_instance_termination_used:true,
  cleanup_verified:true
}'
