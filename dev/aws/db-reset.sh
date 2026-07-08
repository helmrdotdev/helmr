#!/usr/bin/env bash
set -euo pipefail

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  cat <<'EOF'
usage: dev/aws/db-reset.sh

Drop and recreate the AWS dev database public schema from a one-off ECS/Fargate
task. Run the branch migration task afterwards.
EOF
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DEV_STACK="${DEV_STACK:-${ROOT}/infra/aws/stacks/dev}"
AWS_REGION="${AWS_REGION:-us-east-1}"
TF_BIN="${TF_BIN:-tofu}"
PSQL_IMAGE="${PSQL_IMAGE:-public.ecr.aws/docker/library/postgres:18-alpine}"
REDIS_IMAGE="${REDIS_IMAGE:-public.ecr.aws/docker/library/redis:7-alpine}"
LOG_GROUP="${LOG_GROUP:-/aws/ecs/helmr-smoke/control}"
TASK_FAMILY_PREFIX="${TASK_FAMILY_PREFIX:-helmr-smoke-db-reset}"
RESET_WORKER_INSTANCES="${RESET_WORKER_INSTANCES:-1}"
RESET_WORKER_TIMEOUT_SECONDS="${RESET_WORKER_TIMEOUT_SECONDS:-1800}"
RESET_WORKER_POLL_SECONDS="${RESET_WORKER_POLL_SECONDS:-30}"

cluster="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw control_cluster_name)"
migration_task_definition="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw migration_task_definition_arn)"
execution_role="$(
  aws ecs describe-task-definition \
    --region "${AWS_REGION}" \
    --task-definition "${migration_task_definition}" \
    --query "taskDefinition.executionRoleArn" \
    --output text
)"
subnets="$("${TF_BIN}" -chdir="${DEV_STACK}" output -json control_task_subnet_ids)"
security_group="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw control_security_group_id)"
assign_public_ip="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw control_assign_public_ip)"
database_secret="$("${TF_BIN}" -chdir="${DEV_STACK}" output -json secret_arns | jq -r .database_url)"
redis_url="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw redis_url 2>/dev/null || true)"

case "${assign_public_ip}" in
  true) assign_public_ip_value="ENABLED" ;;
  false) assign_public_ip_value="DISABLED" ;;
  *) echo "unexpected control_assign_public_ip=${assign_public_ip}" >&2; exit 1 ;;
esac

task_json="$(mktemp)"
trap 'rm -f "${task_json}"' EXIT

network_configuration="$(
  jq -cn \
    --argjson subnets "${subnets}" \
    --arg sg "${security_group}" \
    --arg assign_public_ip "${assign_public_ip_value}" \
    '{awsvpcConfiguration:{subnets:$subnets,securityGroups:[$sg],assignPublicIp:$assign_public_ip}}'
)"

run_reset_task() {
  local task_name="$1"
  local registered_task_definition
  local task
  local task_details
  local exit_code

  registered_task_definition="$(
    aws ecs register-task-definition \
      --region "${AWS_REGION}" \
      --cli-input-json "file://${task_json}" \
      --query taskDefinition.taskDefinitionArn \
      --output text
  )"
  task="$(
    aws ecs run-task \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --launch-type FARGATE \
      --task-definition "${registered_task_definition}" \
      --network-configuration "${network_configuration}" \
      --query "tasks[0].taskArn" \
      --output text
  )"
  if [ -z "${task}" ] || [ "${task}" = "None" ]; then
    echo "${task_name} task did not start" >&2
    exit 1
  fi

  echo "waiting for ${task_name} task: ${task}" >&2
  aws ecs wait tasks-stopped --region "${AWS_REGION}" --cluster "${cluster}" --tasks "${task}"
  task_details="$(
    aws ecs describe-tasks \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --tasks "${task}"
  )"
  exit_code="$(printf '%s\n' "${task_details}" | jq -r '.tasks[0].containers[0].exitCode // "missing"')"
  if [ "${exit_code}" != "0" ]; then
    printf '%s\n' "${task_details}" | jq -r '.tasks[0] | {stoppedReason, containers}' >&2
    exit 1
  fi
}

replace_worker_instances() {
  [ "${RESET_WORKER_INSTANCES}" = "1" ] || return 0

  local asg_name
  local asg_json
  local desired
  local instance_ids
  local instance_id
  local old_ids_json
  local deadline
  local healthy_replacements
  asg_name="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw worker_autoscaling_group_name 2>/dev/null || true)"
  [ -n "${asg_name}" ] && [ "${asg_name}" != "null" ] || return 0

  asg_json="$(
    aws autoscaling describe-auto-scaling-groups \
      --region "${AWS_REGION}" \
      --auto-scaling-group-names "${asg_name}" \
      --query 'AutoScalingGroups[0]' \
      --output json
  )"
  [ "${asg_json}" != "null" ] || return 0
  desired="$(printf '%s\n' "${asg_json}" | jq -r '.DesiredCapacity // 0')"
  instance_ids="$(printf '%s\n' "${asg_json}" | jq -r '.Instances[].InstanceId')"
  [ -n "${instance_ids}" ] || return 0
  old_ids_json="$(printf '%s\n' "${instance_ids}" | jq -R . | jq -s .)"

  for instance_id in ${instance_ids}; do
    echo "terminating worker instance after db reset: ${instance_id}" >&2
    aws autoscaling terminate-instance-in-auto-scaling-group \
      --region "${AWS_REGION}" \
      --instance-id "${instance_id}" \
      --no-should-decrement-desired-capacity >/dev/null
  done
  [ "${desired}" -gt 0 ] || return 0

  deadline=$((SECONDS + RESET_WORKER_TIMEOUT_SECONDS))
  while :; do
    asg_json="$(
      aws autoscaling describe-auto-scaling-groups \
        --region "${AWS_REGION}" \
        --auto-scaling-group-names "${asg_name}" \
        --query 'AutoScalingGroups[0]' \
        --output json
    )"
    healthy_replacements="$(
      printf '%s\n' "${asg_json}" |
        jq --argjson old_ids "${old_ids_json}" '
          [.Instances[]
            | select(.LifecycleState == "InService")
            | select(.HealthStatus == "Healthy")
            | select((.InstanceId as $id | $old_ids | index($id)) | not)]
          | length
        '
    )"
    if [ "${healthy_replacements}" -ge "${desired}" ]; then
      echo "worker instance replacement complete"
      return 0
    fi
    [ "${SECONDS}" -lt "${deadline}" ] || {
      printf '%s\n' "${asg_json}" | jq -r '{DesiredCapacity, Instances}' >&2
      echo "timed out waiting for worker instance replacement" >&2
      exit 1
    }
    sleep "${RESET_WORKER_POLL_SECONDS}"
  done
}

jq -n \
  --arg family "${TASK_FAMILY_PREFIX}-postgres-$(date +%s)" \
  --arg role "${execution_role}" \
  --arg image "${PSQL_IMAGE}" \
  --arg secret "${database_secret}" \
  --arg log_group "${LOG_GROUP}" \
  --arg region "${AWS_REGION}" \
  '{
    family:$family,
    networkMode:"awsvpc",
    requiresCompatibilities:["FARGATE"],
    cpu:"256",
    memory:"512",
    executionRoleArn:$role,
    containerDefinitions:[{
      name:"psql",
      image:$image,
      essential:true,
      secrets:[{name:"DATABASE_URL",valueFrom:$secret}],
      command:["sh","-lc","psql \"$DATABASE_URL\" -v ON_ERROR_STOP=1 -c \"DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;\""],
      logConfiguration:{
        logDriver:"awslogs",
        options:{
          "awslogs-group":$log_group,
          "awslogs-region":$region,
          "awslogs-stream-prefix":"db-reset"
        }
      }
    }]
  }' >"${task_json}"
run_reset_task "db reset"

if [ -n "${redis_url}" ]; then
  jq -n \
    --arg family "${TASK_FAMILY_PREFIX}-redis-$(date +%s)" \
    --arg role "${execution_role}" \
    --arg image "${REDIS_IMAGE}" \
    --arg redis_url "${redis_url}" \
    --arg log_group "${LOG_GROUP}" \
    --arg region "${AWS_REGION}" \
    '{
      family:$family,
      networkMode:"awsvpc",
      requiresCompatibilities:["FARGATE"],
      cpu:"256",
      memory:"512",
      executionRoleArn:$role,
      containerDefinitions:[{
        name:"redis",
        image:$image,
        essential:true,
        environment:[{name:"REDIS_URL",value:$redis_url}],
        command:["sh","-lc","redis-cli -u \"$REDIS_URL\" FLUSHDB >/dev/null"],
        logConfiguration:{
          logDriver:"awslogs",
          options:{
            "awslogs-group":$log_group,
            "awslogs-region":$region,
            "awslogs-stream-prefix":"db-reset-redis"
          }
        }
      }]
    }' >"${task_json}"
  run_reset_task "redis reset"
  echo "redis db flush complete"
fi

replace_worker_instances

echo "database schema reset complete"
