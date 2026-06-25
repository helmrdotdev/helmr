#!/usr/bin/env bash
set -euo pipefail

if [ $# -ne 1 ]; then
  echo "usage: $0 SQL" >&2
  exit 2
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DEV_STACK="${DEV_STACK:-${ROOT}/infra/aws/stacks/dev}"
AWS_REGION="${AWS_REGION:-us-east-1}"
TF_BIN="${TF_BIN:-tofu}"
PSQL_IMAGE="${PSQL_IMAGE:-public.ecr.aws/docker/library/postgres:16-alpine}"
LOG_GROUP="${LOG_GROUP:-/aws/ecs/helmr-smoke/control}"
TASK_FAMILY_PREFIX="${TASK_FAMILY_PREFIX:-helmr-smoke-db-query}"

cluster="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw control_cluster_name)"
service="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw control_service_name)"
task_definition="$(
  aws ecs describe-services \
    --region "${AWS_REGION}" \
    --cluster "${cluster}" \
    --services "${service}" \
    --query "services[0].taskDefinition" \
    --output text
)"
execution_role="$(
  aws ecs describe-task-definition \
    --region "${AWS_REGION}" \
    --task-definition "${task_definition}" \
    --query "taskDefinition.executionRoleArn" \
    --output text
)"
subnets="$("${TF_BIN}" -chdir="${DEV_STACK}" output -json private_subnet_ids 2>/dev/null || "${TF_BIN}" -chdir="${DEV_STACK}" output -json public_subnet_ids)"
security_group="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw control_security_group_id)"
database_secret="$("${TF_BIN}" -chdir="${DEV_STACK}" output -json secret_arns | jq -r .database_url)"

sql=$1
command='printf "%s\n" "$SQL" | psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -P pager=off'
task_json="$(mktemp)"
trap 'rm -f "${task_json}"' EXIT

jq -n \
  --arg family "${TASK_FAMILY_PREFIX}-$(date +%s)" \
  --arg role "${execution_role}" \
  --arg image "${PSQL_IMAGE}" \
  --arg secret "${database_secret}" \
  --arg log_group "${LOG_GROUP}" \
  --arg region "${AWS_REGION}" \
  --arg sql "${sql}" \
  --arg command "${command}" \
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
      environment:[{name:"SQL",value:$sql}],
      secrets:[{name:"DATABASE_URL",valueFrom:$secret}],
      command:["sh","-lc",$command],
      logConfiguration:{
        logDriver:"awslogs",
        options:{
          "awslogs-group":$log_group,
          "awslogs-region":$region,
          "awslogs-stream-prefix":"db-query"
        }
      }
    }]
  }' >"${task_json}"

registered_task_definition="$(
  aws ecs register-task-definition \
    --region "${AWS_REGION}" \
    --cli-input-json "file://${task_json}" \
    --query taskDefinition.taskDefinitionArn \
    --output text
)"
subnet_csv="$(printf '%s\n' "${subnets}" | jq -r 'join(",")')"
task="$(
  aws ecs run-task \
    --region "${AWS_REGION}" \
    --cluster "${cluster}" \
    --launch-type FARGATE \
    --task-definition "${registered_task_definition}" \
    --network-configuration "awsvpcConfiguration={subnets=[${subnet_csv}],securityGroups=[${security_group}],assignPublicIp=DISABLED}" \
    --query "tasks[0].taskArn" \
    --output text
)"
if [ -z "${task}" ] || [ "${task}" = "None" ]; then
  echo "db query task did not start" >&2
  exit 1
fi

aws ecs wait tasks-stopped --region "${AWS_REGION}" --cluster "${cluster}" --tasks "${task}"
task_details="$(
  aws ecs describe-tasks \
    --region "${AWS_REGION}" \
    --cluster "${cluster}" \
    --tasks "${task}"
)"
exit_code="$(printf '%s\n' "${task_details}" | jq -r '.tasks[0].containers[0].exitCode // "missing"')"
log_stream="$(printf '%s\n' "${task_details}" | jq -r '.tasks[0].containers[0].logStreamName // ""')"
if [ -z "${log_stream}" ] || [ "${log_stream}" = "null" ]; then
  task_id="${task##*/}"
  log_stream="db-query/psql/${task_id}"
fi

if [ -n "${log_stream}" ] && [ "${log_stream}" != "null" ]; then
  for _ in $(seq 1 20); do
    if aws logs get-log-events \
      --region "${AWS_REGION}" \
      --log-group-name "${LOG_GROUP}" \
      --log-stream-name "${log_stream}" \
      --query 'events[].message' \
      --output text; then
      break
    fi
    sleep 2
  done
else
  printf '%s\n' "${task_details}" | jq -r '.tasks[0] | {stoppedReason, containers}'
fi

echo "db_query_exit=${exit_code}"
test "${exit_code}" = "0"
