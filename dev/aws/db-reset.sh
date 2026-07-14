#!/usr/bin/env bash
set -euo pipefail

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  cat <<'EOF'
usage: dev/aws/db-reset.sh

Require both worker fleets to be drained to zero, safely quiesce control and
dispatcher, drop and recreate the AWS dev database public schema from a one-off
ECS/Fargate task, flush Redis, and run the branch migration task. Worker
capacity is owned by the application controller and is never changed by this
command. Services remain stopped after reset.
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
security_groups="$("${TF_BIN}" -chdir="${DEV_STACK}" output -json control_task_security_group_ids)"
assign_public_ip="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw control_assign_public_ip)"
database_secret="$("${TF_BIN}" -chdir="${DEV_STACK}" output -json secret_arns | jq -r .database_url)"
redis_url="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw redis_url 2>/dev/null || true)"

case "${assign_public_ip}" in
  true) assign_public_ip_value="ENABLED" ;;
  false) assign_public_ip_value="DISABLED" ;;
  *) echo "unexpected control_assign_public_ip=${assign_public_ip}" >&2; exit 1 ;;
esac

task_json="$(mktemp)"
reset_started=0
services_quiesced=0
service_names=()
service_desired_counts=()

restore_control_services_best_effort() {
  local index
  local status=0
  local -a wait_services=()

  for index in "${!service_names[@]}"; do
    aws ecs update-service \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --service "${service_names[$index]}" \
      --desired-count "${service_desired_counts[$index]}" >/dev/null || status=1
    if [ "${service_desired_counts[$index]}" -gt 0 ]; then
      wait_services+=("${service_names[$index]}")
    fi
  done
  if [ "${#wait_services[@]}" -gt 0 ]; then
    aws ecs wait services-stable \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --services "${wait_services[@]}" || status=1
  fi
  return "${status}"
}

cleanup() {
  status=$?
  if [ "${status}" -ne 0 ] && [ "${services_quiesced}" = "1" ] && [ "${reset_started}" = "0" ]; then
    echo "database reset did not start; restoring control and dispatcher desired counts" >&2
    set +e
    restore_control_services_best_effort
    restore_status=$?
    set -e
    if [ "${restore_status}" -ne 0 ]; then
      echo "automatic service restoration failed; restore control and dispatcher before accepting work" >&2
    fi
  fi
  rm -f "${task_json}"
  exit "${status}"
}
trap cleanup EXIT

network_configuration="$(
  jq -cn \
    --argjson subnets "${subnets}" \
    --argjson security_groups "${security_groups}" \
    --arg assign_public_ip "${assign_public_ip_value}" \
    '{awsvpcConfiguration:{subnets:$subnets,securityGroups:$security_groups,assignPublicIp:$assign_public_ip}}'
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

run_migration_task() {
  local task
  local task_details
  local exit_code

  task="$(
    aws ecs run-task \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --launch-type FARGATE \
      --task-definition "${migration_task_definition}" \
      --network-configuration "${network_configuration}" \
      --query "tasks[0].taskArn" \
      --output text
  )"
  if [ -z "${task}" ] || [ "${task}" = "None" ]; then
    echo "migration task did not start" >&2
    exit 1
  fi

  echo "waiting for migration task: ${task}" >&2
  aws ecs wait tasks-stopped --region "${AWS_REGION}" --cluster "${cluster}" --tasks "${task}"
  task_details="$(
    aws ecs describe-tasks \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --tasks "${task}"
  )"
  exit_code="$(printf '%s\n' "${task_details}" | jq -r '.tasks[0].containers[] | select(.name == "migration") | .exitCode // "missing"')"
  if [ "${exit_code}" != "0" ]; then
    printf '%s\n' "${task_details}" | jq -r '.tasks[0] | {stoppedReason, containers}' >&2
    exit 1
  fi
  echo "database migration complete"
}

quiesce_control_services() {
  local output_name
  local service_name
  local service_json
  local desired_count
  local desired_status
  local remaining_tasks
  local task_arns_json
  local -a stop_services=()
  local -a tasks_to_stop=()
  local task_arn

  service_names=()
  service_desired_counts=()
  for output_name in control_service_name dispatcher_service_name; do
    service_name="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw "${output_name}")"
    [ -n "${service_name}" ] && [ "${service_name}" != "null" ] || continue
    service_json="$(
      aws ecs describe-services \
        --region "${AWS_REGION}" \
        --cluster "${cluster}" \
        --services "${service_name}" \
        --output json
    )"
    printf '%s\n' "${service_json}" | jq -e --arg service_name "${service_name}" '
      (.failures | length) == 0 and
      (.services | length) == 1 and
      .services[0].serviceName == $service_name
    ' >/dev/null || {
      echo "database reset could not resolve ECS service ${service_name}" >&2
      exit 1
    }
    desired_count="$(printf '%s\n' "${service_json}" | jq -er '.services[0].desiredCount')"
    service_names+=("${service_name}")
    service_desired_counts+=("${desired_count}")
    for desired_status in RUNNING PENDING; do
      task_arns_json="$(
        aws ecs list-tasks \
          --region "${AWS_REGION}" \
          --cluster "${cluster}" \
          --service-name "${service_name}" \
          --desired-status "${desired_status}" \
          --query taskArns \
          --output json
      )"
      printf '%s\n' "${task_arns_json}" | jq -e 'type == "array"' >/dev/null
      while IFS= read -r task_arn; do
        [ -n "${task_arn}" ] && tasks_to_stop+=("${task_arn}")
      done < <(printf '%s\n' "${task_arns_json}" | jq -r '.[]')
    done
  done

  services_quiesced=1
  for service_name in "${service_names[@]}"; do
    aws ecs update-service \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --service "${service_name}" \
      --desired-count 0 >/dev/null
    stop_services+=("${service_name}")
  done
  if [ "${#stop_services[@]}" -gt 0 ]; then
    aws ecs wait services-stable \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --services "${stop_services[@]}"
  fi
  if [ "${#tasks_to_stop[@]}" -gt 0 ]; then
    aws ecs wait tasks-stopped \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --tasks "${tasks_to_stop[@]}"
  fi
  for service_name in "${service_names[@]}"; do
    for desired_status in RUNNING PENDING; do
      remaining_tasks="$(
        aws ecs list-tasks \
          --region "${AWS_REGION}" \
          --cluster "${cluster}" \
          --service-name "${service_name}" \
          --desired-status "${desired_status}" \
          --query taskArns \
          --output json
      )"
      printf '%s\n' "${remaining_tasks}" | jq -e 'type == "array" and length == 0' >/dev/null || {
        echo "database reset requires every ${service_name} task to reach STOPPED" >&2
        exit 1
      }
    done
  done
}

require_worker_fleets_stopped() {
  local output_name
  local asg_name
  local asg_json

  for output_name in worker_autoscaling_group_name build_worker_autoscaling_group_name; do
    asg_name="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw "${output_name}")"
    [ -n "${asg_name}" ] && [ "${asg_name}" != "null" ] || continue
    asg_json="$(
      aws autoscaling describe-auto-scaling-groups \
        --region "${AWS_REGION}" \
        --auto-scaling-group-names "${asg_name}" \
        --output json
    )"
    printf '%s\n' "${asg_json}" | jq -e --arg asg_name "${asg_name}" '
      (.AutoScalingGroups | length) == 1 and
      .AutoScalingGroups[0].AutoScalingGroupName == $asg_name and
      .AutoScalingGroups[0].MinSize == 0 and
      .AutoScalingGroups[0].DesiredCapacity == 0 and
      (.AutoScalingGroups[0].Instances | length) == 0
    ' >/dev/null || {
      echo "database reset requires ${asg_name} min and desired capacity zero with no instances" >&2
      exit 1
    }
  done
}

require_cluster_tasks_absent() {
  local desired_status
  local task_arns_json

  for desired_status in RUNNING PENDING; do
    task_arns_json="$(
      aws ecs list-tasks \
        --region "${AWS_REGION}" \
        --cluster "${cluster}" \
        --desired-status "${desired_status}" \
        --query taskArns \
        --output json
    )"
    printf '%s\n' "${task_arns_json}" | jq -e 'type == "array" and length == 0' >/dev/null || {
      echo "database reset requires no cluster-wide ${desired_status} ECS tasks after service quiescence" >&2
      exit 1
    }
  done
}

require_reset_quiescence() {
  require_worker_fleets_stopped
  quiesce_control_services
  require_cluster_tasks_absent
  require_worker_fleets_stopped
  echo "control and dispatcher are stopped and worker fleets remained at zero for database reset"
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
require_reset_quiescence
reset_started=1
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

run_migration_task

echo "database reset and migration complete; services and worker fleets remain stopped"
