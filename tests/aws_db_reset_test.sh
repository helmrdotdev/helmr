#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
script="${repo_root}/dev/aws/db-reset.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

line_number() {
  grep -n -m1 -F "$1" "${MOCK_LOG}" | cut -d: -f1
}

assert_no_reset() {
  if grep -Fq 'ecs register-task-definition' "${MOCK_LOG}"; then
    fail "$1"
  fi
}

mkdir -p "${tmp}/bin" "${tmp}/state"
MOCK_LOG="${tmp}/aws.log"
MOCK_SERVICE_STATE_DIR="${tmp}/state"
export MOCK_LOG MOCK_SERVICE_STATE_DIR

cat >"${tmp}/bin/tofu" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
name="${4:-}"
case "${name}" in
  control_cluster_name) printf 'cluster\n' ;;
  migration_task_definition_arn) printf 'arn:taskdef:migration\n' ;;
  control_task_subnet_ids) printf '["subnet-1"]\n' ;;
  control_task_security_group_ids) printf '["sg-1","sg-clickhouse"]\n' ;;
  control_assign_public_ip) printf 'false\n' ;;
  secret_arns) printf '{"database_url":"arn:secret:database"}\n' ;;
  redis_url) ;;
  control_service_name) printf 'control\n' ;;
  dispatcher_service_name) printf 'dispatcher\n' ;;
  worker_autoscaling_group_name) printf 'run-asg\n' ;;
  build_worker_autoscaling_group_name) printf 'build-asg\n' ;;
  *) exit 1 ;;
esac
EOF
chmod +x "${tmp}/bin/tofu"

cat >"${tmp}/bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
service="${1:-}"
operation="${2:-}"
shift 2 || true

arg_value() {
  local target=$1
  shift
  while [ "$#" -gt 0 ]; do
    if [ "$1" = "${target}" ]; then
      printf '%s\n' "$2"
      return 0
    fi
    shift
  done
  return 1
}

printf '%s %s %s\n' "${service}" "${operation}" "$*" >>"${MOCK_LOG}"
case "${service}:${operation}" in
  ecs:describe-task-definition)
    printf 'arn:role:execution\n'
    ;;
  ecs:describe-services)
    service_name="$(arg_value --services "$@")"
    desired="$(cat "${MOCK_SERVICE_STATE_DIR}/${service_name}")"
    jq -cn \
      --arg service_name "${service_name}" \
      --argjson desired "${desired}" \
      '{failures:[],services:[{serviceName:$service_name,desiredCount:$desired,runningCount:$desired,pendingCount:0}]}'
    ;;
  ecs:list-tasks)
    [ "${MOCK_LIST_TASKS_FAIL:-0}" != "1" ] || exit 42
    service_name="$(arg_value --service-name "$@" || true)"
    desired_status="$(arg_value --desired-status "$@")"
    if [ -z "${service_name}" ]; then
      if [ "${MOCK_ORPHAN_TASK:-0}" = "1" ] && [ "${desired_status}" = "RUNNING" ]; then
        printf '["arn:task:orphan"]\n'
      else
        printf '[]\n'
      fi
      exit 0
    fi
    desired="$(cat "${MOCK_SERVICE_STATE_DIR}/${service_name}")"
    if [ "${desired}" -gt 0 ] && [ "${desired_status}" = "RUNNING" ]; then
      jq -cn --arg task_arn "arn:task:${service_name}" '[$task_arn]'
    else
      printf '[]\n'
    fi
    ;;
  ecs:update-service)
    service_name="$(arg_value --service "$@")"
    desired="$(arg_value --desired-count "$@")"
    printf '%s\n' "${desired}" >"${MOCK_SERVICE_STATE_DIR}/${service_name}"
    ;;
  autoscaling:describe-auto-scaling-groups)
    [ "${MOCK_ASG_DESCRIBE_FAIL:-0}" != "1" ] || exit 42
    asg="$(arg_value --auto-scaling-group-names "$@")"
    min=0
    desired=0
    instances='[]'
    post_stop=0
    if grep -Fq 'ecs update-service --region us-east-1 --cluster cluster --service dispatcher --desired-count 0' "${MOCK_LOG}"; then
      post_stop=1
    fi
    if [ "${MOCK_ACTIVE_ASG:-}" = "${asg}" ] ||
      { [ "${post_stop}" = "1" ] && [ "${MOCK_POST_STOP_ACTIVE_ASG:-}" = "${asg}" ]; }; then
      min=1
      desired=1
      instances='[{"InstanceId":"i-active","LifecycleState":"InService","HealthStatus":"Healthy"}]'
    fi
    jq -cn \
      --arg name "${asg}" \
      --argjson min "${min}" \
      --argjson desired "${desired}" \
      --argjson instances "${instances}" \
      '{AutoScalingGroups:[{AutoScalingGroupName:$name,MinSize:$min,DesiredCapacity:$desired,Instances:$instances}]}'
    ;;
  ecs:register-task-definition)
    printf 'arn:taskdef:reset\n'
    ;;
  ecs:run-task)
    task_definition="$(arg_value --task-definition "$@")"
    if [ "${task_definition}" = "arn:taskdef:migration" ]; then
      printf 'arn:task:migration\n'
    else
      printf 'arn:task:reset\n'
    fi
    ;;
  ecs:wait)
    if [ "${1:-}" = "tasks-stopped" ] && [ "${MOCK_TASK_WAIT_FAIL:-0}" = "1" ]; then
      exit 42
    fi
    ;;
  ecs:describe-tasks)
    task="$(arg_value --tasks "$@")"
    if [ "${task}" = "arn:task:migration" ]; then
      printf '{"tasks":[{"containers":[{"name":"migration","exitCode":%s}]}]}\n' "${MOCK_FAIL_MIGRATION:-0}"
    else
      printf '{"tasks":[{"containers":[{"name":"psql","exitCode":0}]}]}\n'
    fi
    ;;
  autoscaling:update-auto-scaling-group)
    printf 'manual ASG mutation is forbidden\n' >&2
    exit 99
    ;;
  *)
    printf 'unexpected aws call: %s %s %s\n' "${service}" "${operation}" "$*" >&2
    exit 1
    ;;
esac
EOF
chmod +x "${tmp}/bin/aws"

reset_mock_state() {
  printf '1\n' >"${MOCK_SERVICE_STATE_DIR}/control"
  printf '1\n' >"${MOCK_SERVICE_STATE_DIR}/dispatcher"
}

run_reset() {
  : >"${MOCK_LOG}"
  PATH="${tmp}/bin:${PATH}" \
    TF_BIN="${tmp}/bin/tofu" \
    "${script}" >"${tmp}/stdout" 2>"${tmp}/stderr"
}

reset_mock_state
run_reset
initial_run_check="$(line_number 'autoscaling describe-auto-scaling-groups --region us-east-1 --auto-scaling-group-names run-asg')"
control_stop="$(line_number 'ecs update-service --region us-east-1 --cluster cluster --service control --desired-count 0')"
dispatcher_stop="$(line_number 'ecs update-service --region us-east-1 --cluster cluster --service dispatcher --desired-count 0')"
tasks_stopped="$(line_number 'ecs wait tasks-stopped --region us-east-1 --cluster cluster --tasks arn:task:control arn:task:dispatcher')"
reset="$(line_number 'ecs register-task-definition')"
migration="$(line_number 'ecs run-task --region us-east-1 --cluster cluster --launch-type FARGATE --task-definition arn:taskdef:migration')"

[ "${initial_run_check}" -lt "${control_stop}" ] || fail "worker zero must be proved before service quiescence"
[ "${control_stop}" -lt "${dispatcher_stop}" ] || fail "control must stop before dispatcher"
[ "${dispatcher_stop}" -lt "${tasks_stopped}" ] || fail "service desired counts must reach zero before physical task wait"
[ "${tasks_stopped}" -lt "${reset}" ] || fail "captured ECS tasks must physically stop before schema reset"
[ "$(grep -Fc 'autoscaling describe-auto-scaling-groups --region us-east-1 --auto-scaling-group-names run-asg' "${MOCK_LOG}")" -eq 2 ] ||
  fail "run fleet zero must be re-proved after service quiescence"
[ "$(grep -Fc 'autoscaling describe-auto-scaling-groups --region us-east-1 --auto-scaling-group-names build-asg' "${MOCK_LOG}")" -eq 2 ] ||
  fail "build fleet zero must be re-proved after service quiescence"
[ "${reset}" -lt "${migration}" ] || fail "migration must follow schema reset"
[ "$(cat "${MOCK_SERVICE_STATE_DIR}/control")" = "0" ] || fail "control remains stopped after reset"
[ "$(cat "${MOCK_SERVICE_STATE_DIR}/dispatcher")" = "0" ] || fail "dispatcher remains stopped after reset"
if grep -Fq 'autoscaling update-auto-scaling-group' "${MOCK_LOG}"; then
  fail "database reset must never mutate application-owned ASG capacity"
fi

reset_mock_state
if MOCK_ACTIVE_ASG=run-asg run_reset; then
  fail "active run fleet must block database reset"
fi
assert_no_reset "active run fleet must block before schema mutation"
if grep -Fq 'ecs update-service' "${MOCK_LOG}"; then
  fail "services must remain running when initial worker zero proof fails"
fi

reset_mock_state
if MOCK_POST_STOP_ACTIVE_ASG=run-asg run_reset; then
  fail "capacity reappearing during service quiescence must block database reset"
fi
assert_no_reset "post-stop capacity race must block before schema mutation"
[ "$(cat "${MOCK_SERVICE_STATE_DIR}/control")" = "1" ] || fail "control desired count restored after race"
[ "$(cat "${MOCK_SERVICE_STATE_DIR}/dispatcher")" = "1" ] || fail "dispatcher desired count restored after race"
grep -Fq 'restoring control and dispatcher desired counts' "${tmp}/stderr" ||
  fail "post-stop race restoration reason"

reset_mock_state
if MOCK_TASK_WAIT_FAIL=1 run_reset; then
  fail "unproved ECS task stop must block database reset"
fi
assert_no_reset "ECS task stop proof failure must block before schema mutation"
[ "$(cat "${MOCK_SERVICE_STATE_DIR}/control")" = "1" ] || fail "control restored after task stop proof failure"
[ "$(cat "${MOCK_SERVICE_STATE_DIR}/dispatcher")" = "1" ] || fail "dispatcher restored after task stop proof failure"

reset_mock_state
if MOCK_ORPHAN_TASK=1 run_reset; then
  fail "orphaned cluster task must block database reset"
fi
assert_no_reset "orphaned one-off task must block before schema mutation"
[ "$(cat "${MOCK_SERVICE_STATE_DIR}/control")" = "1" ] || fail "control restored after orphan task proof"
[ "$(cat "${MOCK_SERVICE_STATE_DIR}/dispatcher")" = "1" ] || fail "dispatcher restored after orphan task proof"
grep -Fq 'requires no cluster-wide RUNNING ECS tasks' "${tmp}/stderr" ||
  fail "orphaned task refusal reason"

reset_mock_state
if MOCK_ASG_DESCRIBE_FAIL=1 run_reset; then
  fail "ASG proof failure must block database reset"
fi
assert_no_reset "ASG proof failure must fail closed before schema mutation"
[ "$(cat "${MOCK_SERVICE_STATE_DIR}/control")" = "1" ] || fail "control unchanged on initial proof failure"
[ "$(cat "${MOCK_SERVICE_STATE_DIR}/dispatcher")" = "1" ] || fail "dispatcher unchanged on initial proof failure"

reset_mock_state
if MOCK_LIST_TASKS_FAIL=1 run_reset; then
  fail "ECS task discovery failure must block database reset"
fi
assert_no_reset "ECS task discovery failure must fail closed before schema mutation"
if grep -Fq 'ecs update-service' "${MOCK_LOG}"; then
  fail "service desired counts must remain unchanged when task discovery fails"
fi

reset_mock_state
if MOCK_FAIL_MIGRATION=1 run_reset; then
  fail "migration failure should fail database reset"
fi
[ "$(cat "${MOCK_SERVICE_STATE_DIR}/control")" = "0" ] || fail "control stays stopped after schema mutation failure"
[ "$(cat "${MOCK_SERVICE_STATE_DIR}/dispatcher")" = "0" ] || fail "dispatcher stays stopped after schema mutation failure"
if grep -Fq 'autoscaling update-auto-scaling-group' "${MOCK_LOG}"; then
  fail "migration failure must not trigger ASG restoration"
fi

printf 'ok - aws db reset safely quiesces services without ASG ownership\n'
