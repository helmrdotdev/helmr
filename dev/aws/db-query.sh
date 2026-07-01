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
# This command string runs inside the one-off ECS container. Keep the local
# shell from expanding its variables before the task definition is registered.
# shellcheck disable=SC2016
command='
is_hex_pair() {
  case "$1" in
    [0-9A-Fa-f][0-9A-Fa-f]) return 0 ;;
    *) return 1 ;;
  esac
}

urldecode() {
  input=$1
  output=""
  while [ -n "${input}" ]; do
    char="${input%"${input#?}"}"
    if [ "${char}" = "%" ] && [ "${#input}" -ge 3 ]; then
      hex="${input#?}"
      hex="${hex%"${hex#??}"}"
      if is_hex_pair "${hex}"; then
        code=$((0x${hex}))
        output="${output}$(printf "\\$(printf "%03o" "${code}")")"
        input="${input#???}"
        continue
      fi
    fi
    output="${output}${char}"
    input="${input#?}"
  done
  printf "%s" "${output}"
}

case "${DATABASE_URL}" in
  postgres://*) database_rest="${DATABASE_URL#postgres://}" ;;
  postgresql://*) database_rest="${DATABASE_URL#postgresql://}" ;;
  *) echo "DATABASE_URL must use postgres:// or postgresql://" >&2; exit 2 ;;
esac

database_query=""
case "${database_rest}" in
  *\?*)
    database_query="${database_rest#*\?}"
    database_main="${database_rest%%\?*}"
    ;;
  *)
    database_main="${database_rest}"
    ;;
esac

case "${database_main}" in
  */*)
    database_authority="${database_main%%/*}"
    database_name="${database_main#*/}"
    ;;
  *)
    database_authority="${database_main}"
    database_name=""
    ;;
esac

case "${database_authority}" in
  *@*)
    database_auth="${database_authority%@*}"
    database_hostport="${database_authority##*@}"
    ;;
  *)
    database_auth=""
    database_hostport="${database_authority}"
    ;;
esac
database_user=""
database_password=""
if [ -n "${database_auth}" ]; then
  database_user="${database_auth%%:*}"
  if [ "${database_auth}" != "${database_user}" ]; then
    database_password="${database_auth#*:}"
  fi
fi
case "${database_hostport}" in
  \[*\]*)
    database_host="${database_hostport#\[}"
    database_host="${database_host%%\]*}"
    database_remainder="${database_hostport#\["${database_host}"\]}"
    case "${database_remainder}" in
      "") database_port="" ;;
      :*) database_port="${database_remainder#:}" ;;
      *) echo "invalid bracketed DATABASE_URL host" >&2; exit 2 ;;
    esac
    ;;
  *)
    database_host="${database_hostport%%:*}"
    if [ "${database_hostport}" != "${database_host}" ]; then
      database_port="${database_hostport#*:}"
    else
      database_port=""
    fi
    ;;
esac
query_params_file="$(mktemp)"
pg_service_file="$(mktemp)"
trap "rm -f \"\${query_params_file}\" \"\${pg_service_file}\"" EXIT
query_has_host=0
query_has_port=0
query_has_user=0
query_has_password=0
query_has_dbname=0

write_pg_service_param() {
  key=$1
  value=$2
  case "${key}" in
    ""|[0-9]*|*[!A-Za-z0-9_]*)
      echo "invalid DATABASE_URL query parameter: ${key}" >&2
      exit 2
      ;;
  esac
  case "${value}" in
    *"
"*)
      echo "DATABASE_URL query parameter ${key} contains a newline" >&2
      exit 2
      ;;
  esac
  printf "%s=%s\n" "${key}" "${value}" >>"${pg_service_file}"
}

queue_query_param() {
  key="$(urldecode "$1")"
  value="$(urldecode "$2")"
  case "${key}" in
    host) query_has_host=1 ;;
    port) query_has_port=1 ;;
    user) query_has_user=1 ;;
    password) query_has_password=1 ;;
    dbname) query_has_dbname=1 ;;
  esac
  case "${key}" in
    ""|[0-9]*|*[!A-Za-z0-9_]*)
      echo "invalid DATABASE_URL query parameter: ${key}" >&2
      exit 2
      ;;
  esac
  case "${value}" in
    *"
"*)
      echo "DATABASE_URL query parameter ${key} contains a newline" >&2
      exit 2
      ;;
  esac
  printf "%s=%s\n" "${key}" "${value}" >>"${query_params_file}"
}

if [ -n "${database_query}" ]; then
  old_ifs="${IFS}"
  had_noglob=0
  case $- in
    *f*) had_noglob=1 ;;
  esac
  set -f
  IFS="&"
  set -- ${database_query}
  IFS="${old_ifs}"
  if [ "${had_noglob}" != "1" ]; then
    set +f
  fi
  for pair do
    case "${pair}" in
      *=*)
        key="${pair%%=*}"
        value="${pair#*=}"
        ;;
      *)
        key="${pair}"
        value=""
        ;;
    esac
    queue_query_param "${key}" "${value}"
  done
fi

case "${database_host}" in
  \[*\])
    database_host="${database_host#\[}"
    database_host="${database_host%\]}"
    ;;
esac

printf "[helmr_db_query]\n" >"${pg_service_file}"
if [ "${query_has_host}" != "1" ] && [ -n "${database_host}" ]; then
  write_pg_service_param host "$(urldecode "${database_host}")"
fi
if [ "${query_has_port}" != "1" ] && [ -n "${database_port}" ]; then
  write_pg_service_param port "$(urldecode "${database_port}")"
fi
if [ "${query_has_dbname}" != "1" ] && [ -n "${database_name}" ]; then
  write_pg_service_param dbname "$(urldecode "${database_name}")"
fi
if [ "${query_has_user}" != "1" ] && [ -n "${database_user}" ]; then
  write_pg_service_param user "$(urldecode "${database_user}")"
fi
if [ "${query_has_password}" != "1" ] && [ -n "${database_password}" ]; then
  write_pg_service_param password "$(urldecode "${database_password}")"
fi

cat "${query_params_file}" >>"${pg_service_file}"
if [ -s "${pg_service_file}" ]; then
  export PGSERVICEFILE="${pg_service_file}"
  export PGSERVICE=helmr_db_query
fi
printf "%s\n" "$SQL" | psql -v ON_ERROR_STOP=1 -P pager=off
'
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

wait_timeout_seconds="${DB_QUERY_WAIT_TIMEOUT_SECONDS:-300}"
wait_poll_seconds="${DB_QUERY_WAIT_POLL_SECONDS:-3}"
wait_started_at="$(date +%s)"
wait_deadline=$((wait_started_at + wait_timeout_seconds))
task_details=""
while true; do
  if ! task_details="$(
    aws ecs describe-tasks \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --tasks "${task}"
  )"; then
    now="$(date +%s)"
    if [ "${now}" -ge "${wait_deadline}" ]; then
      echo "db query task status polling failed before timeout (${wait_timeout_seconds}s): ${task}" >&2
      exit 124
    fi
    sleep "${wait_poll_seconds}"
    continue
  fi
  if ! task_status="$(printf '%s\n' "${task_details}" | jq -r '.tasks[0].lastStatus // "missing"')"; then
    now="$(date +%s)"
    if [ "${now}" -ge "${wait_deadline}" ]; then
      echo "db query task status response stayed unreadable before timeout (${wait_timeout_seconds}s): ${task}" >&2
      printf '%s\n' "${task_details}" >&2
      exit 124
    fi
    sleep "${wait_poll_seconds}"
    continue
  fi
  if [ "${task_status}" = "STOPPED" ]; then
    break
  fi
  now="$(date +%s)"
  if [ "${now}" -ge "${wait_deadline}" ]; then
    echo "db query task did not stop before timeout (${wait_timeout_seconds}s): ${task}" >&2
    printf '%s\n' "${task_details}" | jq -r '.tasks[0] // {error:"task missing from describe-tasks"}' >&2
    exit 124
  fi
  sleep "${wait_poll_seconds}"
done
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
      --output json |
      jq -r '.events[].message'; then
      break
    fi
    sleep 2
  done
else
  printf '%s\n' "${task_details}" | jq -r '.tasks[0] | {stoppedReason, containers}'
fi

echo "db_query_exit=${exit_code}"
test "${exit_code}" = "0"
