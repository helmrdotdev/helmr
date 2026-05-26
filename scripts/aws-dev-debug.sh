#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TF_BIN="${TF_BIN:-tofu}"
AWS_REGION="${AWS_REGION:-us-east-1}"
BOOTSTRAP_STACK="${BOOTSTRAP_STACK:-${ROOT}/infra/aws/modules/bootstrap}"
DEV_STACK="${DEV_STACK:-${ROOT}/infra/aws/stacks/dev}"
WORKER_IMAGE_NAME="${WORKER_IMAGE_NAME:-helmr-dev-image}"
STATE_DIR="${STATE_DIR:-${ROOT}/.helmr-aws-dev-smoke}"
DEBUG_ARTIFACT_PREFIX="${DEBUG_ARTIFACT_PREFIX:-helmr/debug}"
GUESTD_OUTPUT="${GUESTD_OUTPUT:-${STATE_DIR}/guestd-linux-amd64}"
GUEST_ADAPTER_BUNDLE="${GUEST_ADAPTER_BUNDLE:-${STATE_DIR}/guest-adapter-bundle.tar}"
SOURCE_BUNDLE_URI_FILE="${STATE_DIR}/source-bundle-s3-uri"

usage() {
  cat <<'EOF'
Usage: scripts/aws-dev-debug.sh <command>

Commands:
  check              Verify local tools, AWS credentials, and dev stack outputs.
  status             Print control URL, ECS services, and worker instance summary.
  control-url        Print the current dev control-plane URL.
  control-up [CONTROL_COUNT] [DISPATCHER_COUNT]
                     Temporarily scale control and dispatcher ECS services up. Defaults to 1 each.
  control-down       Temporarily scale control and dispatcher ECS services down to zero tasks.
  database-up        Start the dev RDS instance and wait until available.
  database-down      Stop the dev RDS instance and wait until stopped.
  dev-on [COUNT]     Start database and control service. Defaults to one control task.
  dev-off            Scale worker/control to zero and stop the database.
  worker-instance    Print the active worker EC2 instance ID.
  worker-up [COUNT]  Temporarily scale the worker Auto Scaling group up. Defaults to 1.
  worker-down        Temporarily scale the worker Auto Scaling group down to zero instances.
  worker-image-cleanup [KEEP]
                     Deregister old tagged worker AMIs and delete their snapshots.
  worker-journal     Print recent worker and BuildKit journal logs over SSM.
  restart-worker     Restart helmr-worker on the active worker over SSM.
  hotpatch-guestd    Build local guestd and replace it inside the worker guest rootfs.
  run-hello-world    Create a GitHub-backed hello-world run against the dev control plane.
  show-run RUN_ID    Print run details, events, and logs.

Required environment:
  AWS_PROFILE        AWS CLI profile name, unless credentials are otherwise configured.
  STATE_BUCKET       S3 backend bucket, only when Terraform outputs are not initialized locally.

Common optional environment:
  AWS_REGION         AWS region. Defaults to us-east-1.
  TF_BIN             Terraform-compatible binary. Defaults to tofu.
  DEV_STACK          Dev Terraform/OpenTofu stack path.
  WORKER_IMAGE_NAME  Worker image stack name used for AMI cleanup tags. Defaults to helmr-dev-image.
  STATE_DIR          Local scratch directory. Defaults to .helmr-aws-dev-smoke.
  DEBUG_ARTIFACT_BUCKET
                     S3 bucket for hotpatch artifacts. Defaults to SOURCE_BUNDLE_BUCKET,
                     bootstrap output, or the dev stack source artifact bucket when available.
  DEBUG_ARTIFACT_PREFIX
                     S3 key prefix for hotpatch artifacts. Defaults to helmr/debug.
  WORKER_INSTANCE_ID Override worker discovery.
  WORKER_UP_ALLOW_NO_NAT
                     Set to 1 to bypass the NAT check for emergency debugging.
  WORKER_SCALE_TIMEOUT_SECONDS
                     Seconds to wait for worker scaling. Defaults to 900.
  WORKER_SCALE_DOWN_TIMEOUT_SECONDS
                     Seconds to wait for worker scale-down. Defaults to 3900.
  WORKER_SCALE_POLL_SECONDS
                     Seconds between worker scaling polls. Defaults to 15.
  DATABASE_WAIT_TIMEOUT_SECONDS
                     Seconds to wait for RDS start/stop. Defaults to 1800.
  DATABASE_WAIT_POLL_SECONDS
                     Seconds between RDS status polls. Defaults to 15.
  JOURNAL_LINES      Lines per journal unit. Defaults to 200.
  WORKER_IMAGE_CLEANUP_KEEP
                     Worker AMI count to retain when no KEEP argument is passed. Defaults to 3.
  WORKER_IMAGE_CLEANUP_DRY_RUN
                     Set to 1 to print AMIs that would be deleted.
  WORKER_IMAGE_CLEANUP_FORCE
                     Set to 1 to allow deleting AMIs referenced by the current worker ASG.

Run commands from the Nix infra shell so AWS CLI, OpenTofu, and jq are supplied by Nix:

  AWS_PROFILE=<profile> nix develop .#infra -c scripts/aws-dev-debug.sh status
EOF
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

info() {
  printf '==> %s\n' "$*" >&2
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || die "missing command: $1"
}

check_tools() {
  need_command "${TF_BIN}"
  need_command aws
  need_command jq
}

tf_output_raw() {
  output=$1
  "${TF_BIN}" -chdir="${DEV_STACK}" output -raw "${output}"
}

try_tf_output_raw() {
  output=$1
  "${TF_BIN}" -chdir="${DEV_STACK}" output -raw "${output}" 2>/dev/null || true
}

bootstrap_output_raw() {
  output=$1
  "${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" output -raw "${output}" 2>/dev/null || true
}

control_url() {
  value="$(tf_output_raw control_url)"
  [ -n "${value}" ] || die "control_url output is unavailable; run dev-init/dev-apply first"
  printf '%s\n' "${value}"
}

control_cluster_name() {
  value="$(tf_output_raw control_cluster_name)"
  [ -n "${value}" ] || die "control_cluster_name output is unavailable"
  printf '%s\n' "${value}"
}

control_service_name() {
  value="$(tf_output_raw control_service_name)"
  [ -n "${value}" ] && [ "${value}" != "null" ] || die "control_service_name output is unavailable; is create_control_service=true?"
  printf '%s\n' "${value}"
}

try_control_service_name() {
  value="$(try_tf_output_raw control_service_name)"
  [ -n "${value}" ] && [ "${value}" != "null" ] || return 1
  printf '%s\n' "${value}"
}

try_dispatcher_service_name() {
  value="$(try_tf_output_raw dispatcher_service_name)"
  [ -n "${value}" ] && [ "${value}" != "null" ] || return 1
  printf '%s\n' "${value}"
}

database_identifier() {
  value="$(tf_output_raw postgres_identifier)"
  [ -n "${value}" ] || die "postgres_identifier output is unavailable"
  printf '%s\n' "${value}"
}

try_database_identifier() {
  value="$(try_tf_output_raw postgres_identifier)"
  [ -n "${value}" ] || return 1
  printf '%s\n' "${value}"
}

try_redis_endpoint() {
  value="$(try_tf_output_raw redis_endpoint)"
  [ -n "${value}" ] && [ "${value}" != "null" ] || return 1
  printf '%s\n' "${value}"
}

worker_asg_name() {
  value="$(tf_output_raw worker_autoscaling_group_name)"
  [ -n "${value}" ] && [ "${value}" != "null" ] || die "worker_autoscaling_group_name output is unavailable; is create_worker=true?"
  printf '%s\n' "${value}"
}

try_worker_asg_name() {
  value="$(tf_output_raw worker_autoscaling_group_name 2>/dev/null || true)"
  [ -n "${value}" ] && [ "${value}" != "null" ] || return 1
  printf '%s\n' "${value}"
}

artifact_bucket() {
  if [ -n "${DEBUG_ARTIFACT_BUCKET:-}" ]; then
    printf '%s\n' "${DEBUG_ARTIFACT_BUCKET}"
    return 0
  fi
  if [ -n "${SOURCE_BUNDLE_BUCKET:-}" ]; then
    printf '%s\n' "${SOURCE_BUNDLE_BUCKET}"
    return 0
  fi
  value="$(bootstrap_output_raw source_artifact_bucket_name)"
  if [ -n "${value}" ]; then
    printf '%s\n' "${value}"
    return 0
  fi
  if [ -f "${SOURCE_BUNDLE_URI_FILE}" ]; then
    uri="$(cat "${SOURCE_BUNDLE_URI_FILE}")"
    bucket="${uri#s3://}"
    bucket="${bucket%%/*}"
    if [ -n "${bucket}" ] && [ "${bucket}" != "${uri}" ]; then
      printf '%s\n' "${bucket}"
      return 0
    fi
  fi
  die "DEBUG_ARTIFACT_BUCKET or SOURCE_BUNDLE_BUCKET is required; bootstrap output was unavailable"
}

worker_instance_id() {
  if [ -n "${WORKER_INSTANCE_ID:-}" ]; then
    printf '%s\n' "${WORKER_INSTANCE_ID}"
    return 0
  fi
  asg="$(worker_asg_name)"
  # The backticks are JMESPath literals, not shell substitutions.
  # shellcheck disable=SC2016
  instance_id="$(
    aws autoscaling describe-auto-scaling-groups \
      --region "${AWS_REGION}" \
      --auto-scaling-group-names "${asg}" \
      --query 'AutoScalingGroups[0].Instances[?LifecycleState==`InService`].InstanceId | [0]' \
      --output text
  )"
  if [ -z "${instance_id}" ] || [ "${instance_id}" = "None" ]; then
    instance_id="$(
      aws autoscaling describe-auto-scaling-groups \
        --region "${AWS_REGION}" \
        --auto-scaling-group-names "${asg}" \
        --query 'AutoScalingGroups[0].Instances[0].InstanceId' \
        --output text
    )"
  fi
  [ -n "${instance_id}" ] && [ "${instance_id}" != "None" ] || die "no worker instance found in ${asg}"
  printf '%s\n' "${instance_id}"
}

check() {
  check_tools
  info "tool: $(${TF_BIN} version | head -n 1)"
  info "tool: $(aws --version)"
  info "region: ${AWS_REGION}"
  aws sts get-caller-identity --region "${AWS_REGION}" >/dev/null
  control_url >/dev/null
  info "control URL: $(control_url)"
}

status() {
  check_tools
  printf 'control_url=%s\n' "$(control_url)"

  cluster="$(control_cluster_name)"
  printf 'control_cluster=%s\n' "${cluster}"
  services="$(
    aws ecs list-services \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --query 'serviceArns' \
      --output json
  )"
  if [ "$(printf '%s\n' "${services}" | jq 'length')" -gt 0 ]; then
    mapfile -t service_arns < <(printf '%s\n' "${services}" | jq -r '.[]')
    aws ecs describe-services \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --services "${service_arns[@]}" \
      --query 'services[].{service:serviceName,status:status,desired:desiredCount,running:runningCount,pending:pendingCount,taskDefinition:taskDefinition}' \
      --output table
  fi

  if database_id="$(try_database_identifier)"; then
    printf 'database=%s\n' "${database_id}"
    aws rds describe-db-instances \
      --region "${AWS_REGION}" \
      --db-instance-identifier "${database_id}" \
      --query 'DBInstances[].{id:DBInstanceIdentifier,status:DBInstanceStatus,class:DBInstanceClass,engine:Engine,endpoint:Endpoint.Address}' \
      --output table
  else
    printf 'database=unavailable\n'
  fi

  if redis_endpoint="$(try_redis_endpoint)"; then
    printf 'redis=%s\n' "${redis_endpoint}"
  else
    printf 'redis=unavailable\n'
  fi

  if asg="$(try_worker_asg_name)"; then
    printf 'worker_asg=%s\n' "${asg}"
    aws autoscaling describe-auto-scaling-groups \
      --region "${AWS_REGION}" \
      --auto-scaling-group-names "${asg}" \
      --query 'AutoScalingGroups[0].{min:MinSize,max:MaxSize,desired:DesiredCapacity,instances:Instances[].{instance:InstanceId,lifecycle:LifecycleState,health:HealthStatus,launchTemplate:LaunchTemplate.Version}}' \
      --output table

    if instance_id="$(worker_instance_id 2>/dev/null)"; then
      aws ec2 describe-instances \
        --region "${AWS_REGION}" \
        --instance-ids "${instance_id}" \
        --query 'Reservations[].Instances[].{instance:InstanceId,state:State.Name,type:InstanceType,privateIp:PrivateIpAddress,launchTime:LaunchTime}' \
        --output table
    fi
  else
    printf 'worker_asg=unavailable\n'
  fi
}

control_scale() {
  check_tools
  desired=${1:-}
  dispatcher_desired=${2:-${DEV_DISPATCHER_DESIRED_COUNT:-${desired}}}
  case "${desired}" in
    ''|*[!0-9]*) die "control task count must be a non-negative integer" ;;
  esac
  case "${dispatcher_desired}" in
    ''|*[!0-9]*) die "dispatcher task count must be a non-negative integer" ;;
  esac

  cluster="$(control_cluster_name)"
  if ! service="$(try_control_service_name)"; then
    info "control service is not created; run dev-control-tfvars and dev-apply first"
    return 0
  fi

  aws ecs update-service \
    --region "${AWS_REGION}" \
    --cluster "${cluster}" \
    --service "${service}" \
    --desired-count "${desired}" >/dev/null

  services=("${service}")
  if dispatcher_service="$(try_dispatcher_service_name)"; then
    aws ecs update-service \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --service "${dispatcher_service}" \
      --desired-count "${dispatcher_desired}" >/dev/null
    services+=("${dispatcher_service}")
  else
    info "dispatcher service is not created; skipping dispatcher scale"
  fi

  aws ecs wait services-stable \
    --region "${AWS_REGION}" \
    --cluster "${cluster}" \
    --services "${services[@]}"
  info "control service desired count is ${desired}; dispatcher service desired count is ${dispatcher_desired}"
}

control_up() {
  control_scale "${1:-1}" "${2:-${DEV_DISPATCHER_DESIRED_COUNT:-1}}"
}

control_down() {
  control_scale 0 0
}

database_status() {
  database_id="$(database_identifier)"
  aws rds describe-db-instances \
    --region "${AWS_REGION}" \
    --db-instance-identifier "${database_id}" \
    --query 'DBInstances[0].DBInstanceStatus' \
    --output text
}

wait_database_status() {
  database_id=$1
  target_status=$2
  timeout="${DATABASE_WAIT_TIMEOUT_SECONDS:-1800}"
  interval="${DATABASE_WAIT_POLL_SECONDS:-15}"
  deadline=$((SECONDS + timeout))

  while :; do
    status="$(database_status)"
    info "database status: ${status}"
    [ "${status}" = "${target_status}" ] && return 0
    [ "${SECONDS}" -lt "${deadline}" ] || die "timed out waiting for database ${database_id} to become ${target_status}"
    sleep "${interval}"
  done
}

database_up() {
  check_tools
  database_id="$(database_identifier)"
  status="$(database_status)"
  case "${status}" in
    available)
      info "database already available: ${database_id}"
      ;;
    stopped)
      aws rds start-db-instance \
        --region "${AWS_REGION}" \
        --db-instance-identifier "${database_id}" >/dev/null
      wait_database_status "${database_id}" available
      ;;
    starting|backing-up|configuring-enhanced-monitoring|configuring-iam-database-auth|configuring-log-exports|maintenance|modifying|rebooting|renaming|resetting-master-credentials|storage-optimization|upgrading)
      aws rds wait db-instance-available \
        --region "${AWS_REGION}" \
        --db-instance-identifier "${database_id}"
      ;;
    *)
      die "database ${database_id} is in ${status}; cannot start safely"
      ;;
  esac
}

database_down() {
  check_tools
  database_id="$(database_identifier)"
  status="$(database_status)"
  case "${status}" in
    stopped)
      info "database already stopped: ${database_id}"
      ;;
    stopping)
      wait_database_status "${database_id}" stopped
      ;;
    available)
      aws rds stop-db-instance \
        --region "${AWS_REGION}" \
        --db-instance-identifier "${database_id}" >/dev/null
      wait_database_status "${database_id}" stopped
      ;;
    *)
      die "database ${database_id} is in ${status}; cannot stop safely"
      ;;
  esac
}

dev_on() {
  database_up
  control_up "${1:-1}"
}

dev_off() {
  if try_worker_asg_name >/dev/null; then
    worker_down
  else
    info "worker Auto Scaling group is unavailable; skipping worker scale-down"
  fi
  control_down
  database_down
}

wait_worker_capacity() {
  asg=$1
  desired=$2
  if [ "${desired}" -eq 0 ]; then
    timeout="${WORKER_SCALE_DOWN_TIMEOUT_SECONDS:-3900}"
  else
    timeout="${WORKER_SCALE_TIMEOUT_SECONDS:-900}"
  fi
  interval="${WORKER_SCALE_POLL_SECONDS:-15}"
  deadline=$((SECONDS + timeout))

  while :; do
    group="$(
      aws autoscaling describe-auto-scaling-groups \
        --region "${AWS_REGION}" \
        --auto-scaling-group-names "${asg}" \
        --query 'AutoScalingGroups[0]' \
        --output json
    )"
    current_desired="$(printf '%s\n' "${group}" | jq -r '.DesiredCapacity')"
    in_service="$(printf '%s\n' "${group}" | jq '[.Instances[]? | select(.LifecycleState == "InService")] | length')"
    total="$(printf '%s\n' "${group}" | jq '[.Instances[]?] | length')"
    info "worker scale: desired=${current_desired} in_service=${in_service} total=${total}"

    if [ "${desired}" -eq 0 ]; then
      [ "${current_desired}" -eq 0 ] && [ "${total}" -eq 0 ] && return 0
    elif [ "${current_desired}" -eq "${desired}" ] && [ "${in_service}" -ge "${desired}" ]; then
      return 0
    fi

    [ "${SECONDS}" -lt "${deadline}" ] || die "timed out waiting for worker capacity ${desired}"
    sleep "${interval}"
  done
}

worker_scale() {
  check_tools
  desired=${1:-}
  case "${desired}" in
    ''|*[!0-9]*) die "worker count must be a non-negative integer" ;;
  esac

  if [ "${desired}" -gt 0 ]; then
    require_worker_private_egress
  fi

  asg="$(worker_asg_name)"
  if [ "${desired}" -eq 0 ]; then
    aws autoscaling update-auto-scaling-group \
      --region "${AWS_REGION}" \
      --auto-scaling-group-name "${asg}" \
      --min-size 0 \
      --desired-capacity 0
  else
    aws autoscaling update-auto-scaling-group \
      --region "${AWS_REGION}" \
      --auto-scaling-group-name "${asg}" \
      --min-size "${desired}" \
      --max-size "${desired}" \
      --desired-capacity "${desired}"
  fi
  wait_worker_capacity "${asg}" "${desired}"
}

worker_up() {
  worker_scale "${1:-1}"
}

worker_down() {
  worker_scale 0
}

require_worker_private_egress() {
  [ "${WORKER_UP_ALLOW_NO_NAT:-0}" != "1" ] || return 0

  nat_gateway_id="$(try_tf_output_raw nat_gateway_id)"
  if [ -z "${nat_gateway_id}" ] || [ "${nat_gateway_id}" = "null" ]; then
    die "worker-up requires NAT Gateway because workers run in private subnets; run aws-dev-smoke.sh dev-worker-tfvars and dev-apply first, or set WORKER_UP_ALLOW_NO_NAT=1 for emergency debugging"
  fi
}

worker_image_cleanup() {
  check_tools
  keep="${1:-${WORKER_IMAGE_CLEANUP_KEEP:-3}}"
  case "${keep}" in
    ''|*[!0-9]*) die "keep count must be a non-negative integer" ;;
  esac

  images="$(
    aws ec2 describe-images \
      --region "${AWS_REGION}" \
      --owners self \
      --filters "Name=tag:HelmrWorkerImageName,Values=${WORKER_IMAGE_NAME}" \
      --query 'sort_by(Images, &CreationDate)' \
      --output json
  )"
  count="$(printf '%s\n' "${images}" | jq 'length')"
  if [ "${count}" -le "${keep}" ]; then
    info "worker AMI cleanup: ${count} image(s), keeping ${keep}; nothing to delete"
    return 0
  fi

  protected_images="$(protected_worker_image_ids)"
  delete_count=$((count - keep))
  printf '%s\n' "${images}" | jq -c ".[:${delete_count}][]" | while read -r image; do
    image_id="$(printf '%s\n' "${image}" | jq -r '.ImageId')"
    image_name="$(printf '%s\n' "${image}" | jq -r '.Name')"
    mapfile -t snapshot_ids < <(printf '%s\n' "${image}" | jq -r '.BlockDeviceMappings[]?.Ebs.SnapshotId // empty')
    if [ "${WORKER_IMAGE_CLEANUP_FORCE:-0}" != "1" ] && printf '%s\n' "${protected_images}" | grep -Fxq "${image_id}"; then
      info "skipped worker AMI still referenced by current worker resources: ${image_id} (${image_name})"
      continue
    fi
    if [ "${WORKER_IMAGE_CLEANUP_DRY_RUN:-0}" = "1" ]; then
      printf 'would_delete_image=%s name=%s snapshots=%s\n' "${image_id}" "${image_name}" "${snapshot_ids[*]}"
      continue
    fi
    aws ec2 deregister-image \
      --region "${AWS_REGION}" \
      --image-id "${image_id}" >/dev/null
    for snapshot_id in "${snapshot_ids[@]}"; do
      aws ec2 delete-snapshot \
        --region "${AWS_REGION}" \
        --snapshot-id "${snapshot_id}" >/dev/null || true
    done
    info "deleted worker AMI ${image_id} (${image_name})"
  done
}

protected_worker_image_ids() {
  asg="$(try_worker_asg_name 2>/dev/null || true)"
  [ -n "${asg}" ] || return 0

  group="$(
    aws autoscaling describe-auto-scaling-groups \
      --region "${AWS_REGION}" \
      --auto-scaling-group-names "${asg}" \
      --query 'AutoScalingGroups[0]' \
      --output json 2>/dev/null || true
  )"
  [ -n "${group}" ] && [ "${group}" != "null" ] || return 0

  mapfile -t instance_ids < <(printf '%s\n' "${group}" | jq -r '.Instances[]?.InstanceId // empty')
  if [ "${#instance_ids[@]}" -gt 0 ]; then
    aws ec2 describe-instances \
      --region "${AWS_REGION}" \
      --instance-ids "${instance_ids[@]}" |
      jq -r '.Reservations[].Instances[].ImageId // empty'
  fi

  launch_template_id="$(printf '%s\n' "${group}" | jq -r '.LaunchTemplate.LaunchTemplateId // empty')"
  launch_template_version="$(printf '%s\n' "${group}" | jq -r '.LaunchTemplate.Version // empty')"
  if [ -n "${launch_template_id}" ] && [ -n "${launch_template_version}" ]; then
    aws ec2 describe-launch-template-versions \
      --region "${AWS_REGION}" \
      --launch-template-id "${launch_template_id}" \
      --versions "${launch_template_version}" \
      --query 'LaunchTemplateVersions[].LaunchTemplateData.ImageId' \
      --output text 2>/dev/null | tr '\t' '\n'
  fi
}

ssm_send_commands() {
  instance_id=$1
  description=$2
  commands_json=$3
  parameters="$(jq -cn --argjson commands "${commands_json}" '{commands:$commands}')"
  command_id="$(
    aws ssm send-command \
      --region "${AWS_REGION}" \
      --instance-ids "${instance_id}" \
      --document-name AWS-RunShellScript \
      --comment "${description}" \
      --parameters "${parameters}" \
      --query 'Command.CommandId' \
      --output text
  )"
  [ -n "${command_id}" ] && [ "${command_id}" != "None" ] || die "SSM did not return a command ID"
  printf '%s\n' "${command_id}"
}

ssm_wait() {
  instance_id=$1
  command_id=$2
  while :; do
    invocation="$(
      aws ssm get-command-invocation \
        --region "${AWS_REGION}" \
        --command-id "${command_id}" \
        --instance-id "${instance_id}" \
        --output json 2>/dev/null || true
    )"
    if [ -z "${invocation}" ]; then
      sleep 2
      continue
    fi
    status="$(printf '%s\n' "${invocation}" | jq -r '.Status')"
    case "${status}" in
      Success)
        printf '%s\n' "${invocation}" | jq -r '.StandardOutputContent'
        stderr="$(printf '%s\n' "${invocation}" | jq -r '.StandardErrorContent')"
        [ -z "${stderr}" ] || printf '%s\n' "${stderr}" >&2
        return 0
        ;;
      Failed|Cancelled|TimedOut|Cancelling)
        printf '%s\n' "${invocation}" | jq -r '.StandardOutputContent'
        printf '%s\n' "${invocation}" | jq -r '.StandardErrorContent' >&2
        return 1
        ;;
      Pending|InProgress|Delayed)
        sleep 5
        ;;
      *)
        die "unexpected SSM status ${status}"
        ;;
    esac
  done
}

worker_journal() {
  check_tools
  instance_id="$(worker_instance_id)"
  lines="${JOURNAL_LINES:-200}"
  commands="$(
    jq -cn --arg lines "${lines}" '[
      "set -eu",
      "journalctl -u helmr-worker -n " + $lines + " --no-pager || true",
      "journalctl -u buildkit.service -n " + $lines + " --no-pager || true"
    ]'
  )"
  command_id="$(ssm_send_commands "${instance_id}" "helmr worker journal" "${commands}")"
  ssm_wait "${instance_id}" "${command_id}"
}

restart_worker() {
  check_tools
  instance_id="$(worker_instance_id)"
  commands='["set -eu","systemctl restart helmr-worker","systemctl is-active helmr-worker"]'
  command_id="$(ssm_send_commands "${instance_id}" "restart helmr worker" "${commands}")"
  ssm_wait "${instance_id}" "${command_id}"
}

hotpatch_guestd() {
  check_tools
  need_command go
  mkdir -p "${STATE_DIR}"
  bucket="$(artifact_bucket)"
  revision="$(git -C "${ROOT}" rev-parse --short=12 HEAD)"
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  guestd_key="${DEBUG_ARTIFACT_PREFIX%/}/guestd/${revision}-${stamp}/guestd-linux-amd64"
  init_key="${DEBUG_ARTIFACT_PREFIX%/}/guestd/${revision}-${stamp}/init.sh"
  adapter_key="${DEBUG_ARTIFACT_PREFIX%/}/guestd/${revision}-${stamp}/guest-adapter-bundle.tar"
  guestd_s3_uri="s3://${bucket}/${guestd_key}"
  init_s3_uri="s3://${bucket}/${init_key}"
  adapter_s3_uri="s3://${bucket}/${adapter_key}"

  info "building guestd linux/amd64"
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "${GUESTD_OUTPUT}" ./cmd/guestd
  info "building guest adapter bundle"
  adapter_bundle_dir="${STATE_DIR}/guest-adapter-bundle"
  rm -rf "${adapter_bundle_dir}"
  mkdir -p "${adapter_bundle_dir}/adapter"
  bun build "${ROOT}/runtime/typescript/src/main.ts" --target=node --format=esm --outfile="${adapter_bundle_dir}/adapter/main.js" >/dev/null
  install -m 0644 "${ROOT}/runtime/typescript/src/register.mjs" "${adapter_bundle_dir}/adapter/register.mjs"
  install -m 0644 "${ROOT}/runtime/typescript/src/loader.mjs" "${adapter_bundle_dir}/adapter/loader.mjs"
  adapter_hash="$(shasum -a 256 "${adapter_bundle_dir}/adapter/main.js" | awk '{print $1}')"
  proto_hash="$(shasum -a 256 "${ROOT}"/proto/*.proto | shasum -a 256 | awk '{print $1}')"
  node_version="$(node --version)"
  cat >"${adapter_bundle_dir}/adapter/manifest.json" <<EOF
{
  "runtime_contract_version": 1,
  "adapter_hash": "sha256:${adapter_hash}",
  "proto_schema_hash": "sha256:${proto_hash}",
  "node_version": "${node_version}",
  "guestd_version": "${revision}",
  "source_revision": "$(git -C "${ROOT}" rev-parse HEAD)",
  "dirty": $(git -C "${ROOT}" diff --quiet && echo false || echo true)
}
EOF
  tar -C "${adapter_bundle_dir}" -cf "${GUEST_ADAPTER_BUNDLE}" adapter
  info "uploading ${guestd_s3_uri}"
  aws s3 cp --region "${AWS_REGION}" "${GUESTD_OUTPUT}" "${guestd_s3_uri}" >/dev/null
  info "uploading ${init_s3_uri}"
  aws s3 cp --region "${AWS_REGION}" "${ROOT}/images/guest/init.sh" "${init_s3_uri}" >/dev/null
  info "uploading ${adapter_s3_uri}"
  aws s3 cp --region "${AWS_REGION}" "${GUEST_ADAPTER_BUNDLE}" "${adapter_s3_uri}" >/dev/null
  guestd_url="$(aws s3 presign --region "${AWS_REGION}" "${guestd_s3_uri}" --expires-in 900)"
  init_url="$(aws s3 presign --region "${AWS_REGION}" "${init_s3_uri}" --expires-in 900)"
  adapter_url="$(aws s3 presign --region "${AWS_REGION}" "${adapter_s3_uri}" --expires-in 900)"

  instance_id="$(worker_instance_id)"
  commands="$(
    jq -cn --arg guestd_url "${guestd_url}" --arg init_url "${init_url}" --arg adapter_url "${adapter_url}" '[
      "set -eu",
      "if [ -r /etc/helmr/worker.env ]; then set -a; . /etc/helmr/worker.env; set +a; fi",
      ": \"${GUEST_ROOTFS_PATH:=${HELMR_WORKER_IMAGES_DIR:-/var/lib/helmr/images}/guest/out/rootfs.ext4}\"",
      "[ -f \"$GUEST_ROOTFS_PATH\" ] || { echo \"guest rootfs not found: $GUEST_ROOTFS_PATH\" >&2; exit 1; }",
      "systemctl stop helmr-worker",
      "install -d /tmp/helmr-rootfs-mnt",
      "mountpoint -q /tmp/helmr-rootfs-mnt && umount /tmp/helmr-rootfs-mnt || true",
      "trap '\''mountpoint -q /tmp/helmr-rootfs-mnt && umount /tmp/helmr-rootfs-mnt || true; systemctl start helmr-worker || true'\'' EXIT",
      "curl -fL " + @sh "\($guestd_url)" + " -o /tmp/guestd-linux-amd64",
      "curl -fL " + @sh "\($init_url)" + " -o /tmp/helmr-init.sh",
      "curl -fL " + @sh "\($adapter_url)" + " -o /tmp/helmr-adapter-bundle.tar",
      "chmod 755 /tmp/guestd-linux-amd64",
      "chmod 755 /tmp/helmr-init.sh",
      "mount -o loop,rw \"$GUEST_ROOTFS_PATH\" /tmp/helmr-rootfs-mnt",
      "install -m 0755 /tmp/guestd-linux-amd64 /tmp/helmr-rootfs-mnt/usr/bin/guestd",
      "install -m 0755 /tmp/helmr-init.sh /tmp/helmr-rootfs-mnt/init",
      "rm -rf /tmp/helmr-rootfs-mnt/opt/helmr/adapter /tmp/helmr-rootfs-mnt/opt/helmr-adapter",
      "install -d /tmp/helmr-rootfs-mnt/opt/helmr /tmp/helmr-rootfs-mnt/opt/helmr-adapter",
      "tar -xf /tmp/helmr-adapter-bundle.tar -C /tmp/helmr-rootfs-mnt/opt/helmr",
      "tar -xf /tmp/helmr-adapter-bundle.tar -C /tmp/helmr-rootfs-mnt/opt/helmr-adapter",
      "sync",
      "umount /tmp/helmr-rootfs-mnt",
      "trap - EXIT",
      "systemctl start helmr-worker",
      "systemctl is-active helmr-worker"
    ]'
  )"
  command_id="$(ssm_send_commands "${instance_id}" "hotpatch guestd" "${commands}")"
  ssm_wait "${instance_id}" "${command_id}"
  info "guestd hotpatched on ${instance_id}"
  printf '%s\n' "${guestd_s3_uri}"
}

repo_slug() {
  if [ -n "${DEBUG_RUN_REPO:-}" ]; then
    printf '%s\n' "${DEBUG_RUN_REPO}"
    return 0
  fi
  remote="$(git -C "${ROOT}" config --get remote.origin.url)"
  case "${remote}" in
    git@github.com:*)
      slug="${remote#git@github.com:}"
      ;;
    https://github.com/*)
      slug="${remote#https://github.com/}"
      ;;
    *)
      die "cannot infer DEBUG_RUN_REPO from remote.origin.url=${remote}"
      ;;
  esac
  slug="${slug%.git}"
  printf '%s\n' "${slug}"
}

default_run_ref() {
  origin_head="$(git -C "${ROOT}" symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null || true)"
  if [ -n "${origin_head}" ]; then
    printf '%s\n' "${origin_head#origin/}"
    return 0
  fi
  git -C "${ROOT}" symbolic-ref --quiet --short HEAD || git -C "${ROOT}" rev-parse HEAD
}

run_hello_world() {
  check_tools
  task="${DEBUG_RUN_TASK:-hello-world}"
  repo="$(repo_slug)"
  ref="${DEBUG_RUN_REF:-$(default_run_ref)}"
  subpath="${DEBUG_RUN_SUBPATH:-examples/hello-world}"
  max_duration="${DEBUG_RUN_MAX_DURATION_SECONDS:-600}"
  HELMR_URL="$(control_url)" go run ./cmd/helmr run "${task}" \
    --repo "${repo}" \
    --ref "${ref}" \
    --subpath "${subpath}" \
    --max-duration-seconds "${max_duration}"
}

show_run() {
  run_id=${1:-}
  [ -n "${run_id}" ] || die "RUN_ID is required"
  base_url="$(control_url)"
  HELMR_URL="${base_url}" go run ./cmd/helmr show "${run_id}"
  HELMR_URL="${base_url}" go run ./cmd/helmr events "${run_id}"
  HELMR_URL="${base_url}" go run ./cmd/helmr logs "${run_id}"
}

command=${1:-}
case "${command}" in
  check) check ;;
  status) status ;;
  control-url) control_url ;;
  control-up) shift; control_up "$@" ;;
  control-down) control_down ;;
  database-up) database_up ;;
  database-down) database_down ;;
  dev-on) shift; dev_on "$@" ;;
  dev-off) dev_off ;;
  worker-instance) worker_instance_id ;;
  worker-up) shift; worker_up "$@" ;;
  worker-down) worker_down ;;
  worker-image-cleanup) shift; worker_image_cleanup "$@" ;;
  worker-journal) worker_journal ;;
  restart-worker) restart_worker ;;
  hotpatch-guestd) hotpatch_guestd ;;
  run-hello-world) run_hello_world ;;
  show-run) shift; show_run "$@" ;;
  -h|--help|help|"") usage ;;
  *) usage >&2; die "unknown command: ${command}" ;;
esac
