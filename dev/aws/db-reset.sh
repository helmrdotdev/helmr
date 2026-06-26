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
security_group="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw control_security_group_id)"
assign_public_ip="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw control_assign_public_ip)"
database_secret="$("${TF_BIN}" -chdir="${DEV_STACK}" output -json secret_arns | jq -r .database_url)"

case "${assign_public_ip}" in
  true) assign_public_ip_value="ENABLED" ;;
  false) assign_public_ip_value="DISABLED" ;;
  *) echo "unexpected control_assign_public_ip=${assign_public_ip}" >&2; exit 1 ;;
esac

task_json="$(mktemp)"
trap 'rm -f "${task_json}"' EXIT

jq -n \
  --arg family "${TASK_FAMILY_PREFIX}-$(date +%s)" \
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

registered_task_definition="$(
  aws ecs register-task-definition \
    --region "${AWS_REGION}" \
    --cli-input-json "file://${task_json}" \
    --query taskDefinition.taskDefinitionArn \
    --output text
)"
network_configuration="$(
  jq -cn \
    --argjson subnets "${subnets}" \
    --arg sg "${security_group}" \
    --arg assign_public_ip "${assign_public_ip_value}" \
    '{awsvpcConfiguration:{subnets:$subnets,securityGroups:[$sg],assignPublicIp:$assign_public_ip}}'
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
  echo "db reset task did not start" >&2
  exit 1
fi

echo "waiting for db reset task: ${task}" >&2
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

echo "database schema reset complete"
