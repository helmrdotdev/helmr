#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TF_BIN="${TF_BIN:-tofu}"
AWS_REGION="${AWS_REGION:-us-east-1}"
STATE_REGION="${STATE_REGION:-${AWS_REGION}}"
STATE_KEY="${STATE_KEY:-}"
WORKER_IMAGE_NAME="${WORKER_IMAGE_NAME:-helmr-dev-image}"
CURRENT_GIT_REF="$(git -C "${ROOT}" symbolic-ref --quiet --short HEAD || git -C "${ROOT}" rev-parse HEAD)"
WORKER_IMAGE_SOURCE_REPOSITORY_URL="${WORKER_IMAGE_SOURCE_REPOSITORY_URL:-https://github.com/helmrdotdev/helmr.git}"
WORKER_IMAGE_SOURCE_REF="${WORKER_IMAGE_SOURCE_REF:-${CURRENT_GIT_REF}}"
WORKER_IMAGE_VERSION="${WORKER_IMAGE_VERSION:-}"
WORKER_IMAGE_INSTANCE_PROFILE_NAME="${WORKER_IMAGE_INSTANCE_PROFILE_NAME:-}"
WORKER_IMAGE_DISTRIBUTION_REGIONS="${WORKER_IMAGE_DISTRIBUTION_REGIONS:-}"
WORKER_IMAGE_AMI_PUBLIC="${WORKER_IMAGE_AMI_PUBLIC:-}"
WORKER_IMAGE_ROOT_VOLUME_ENCRYPTED="${WORKER_IMAGE_ROOT_VOLUME_ENCRYPTED:-}"
BOOTSTRAP_NAME="${BOOTSTRAP_NAME:-helmr-dev}"
BOOTSTRAP_STACK="${BOOTSTRAP_STACK:-${ROOT}/infra/aws/modules/bootstrap}"
WORKER_IMAGE_STACK="${WORKER_IMAGE_STACK:-${ROOT}/infra/aws/stacks/worker-image}"
DEV_STACK="${DEV_STACK:-${ROOT}/infra/aws/stacks/dev}"
DEV_TFVARS_TEMPLATE="${DEV_TFVARS_TEMPLATE:-${DEV_STACK}/full-run-smoke.tfvars.example}"
DEV_TFVARS="${DEV_TFVARS:-${DEV_STACK}/full-run-smoke.tfvars}"
STATE_DIR="${STATE_DIR:-${ROOT}/.helmr-aws-dev-smoke}"
IMAGE_ARN_FILE="${STATE_DIR}/worker-image-build-version-arn"
AMI_ID_FILE="${STATE_DIR}/worker-ami-id"
AMI_IDS_FILE="${STATE_DIR}/worker-ami-ids.json"
SOURCE_BUNDLE_FILE="${STATE_DIR}/source.bundle"
SOURCE_BUNDLE_URI_FILE="${STATE_DIR}/source-bundle-s3-uri"
SOURCE_BUNDLE_REF_FILE="${STATE_DIR}/source-bundle-ref"
BUILD_POLICY_DIGEST_FILE="${STATE_DIR}/build-policy-digest"
WORKER_IMAGE_PROVENANCE_FILE="${STATE_DIR}/worker-image-provenance.json"
CONTROL_IMAGE_PROVENANCE_FILE="${STATE_DIR}/control-image-provenance.json"
CONTROL_IMAGE_URI_FILE="${STATE_DIR}/control-image-uri"
IMAGE_WAIT_INTERVAL_SECONDS="${IMAGE_WAIT_INTERVAL_SECONDS:-60}"
IMAGE_WAIT_TIMEOUT_SECONDS="${IMAGE_WAIT_TIMEOUT_SECONDS:-7200}"

usage() {
  cat <<'EOF'
Usage: scripts/aws-dev-smoke.sh <command>

Commands:
  check                 Verify local tools and AWS credentials.
  bootstrap-init        Initialize the local bootstrap module.
  bootstrap-apply       Create the S3 state bucket with the bootstrap module.
  bootstrap-output      Print shell exports for the created state bucket.
  bootstrap-destroy-prepare
                       Empty versioned bootstrap buckets before destroying them.
  platform-release-publish
                        Build and publish the pinned Platform release to the Platform store.
  source-bundle         Upload the current Git HEAD as an S3 git bundle.
  worker-image-source-check
                        Check that Image Builder can fetch the configured Git ref.
  worker-image-init     Initialize the worker-image stack backend.
  worker-image-apply    Apply the worker-image stack.
  worker-image-start    Start the EC2 Image Builder pipeline.
  worker-image-wait     Wait for the Image Builder run and record the AMI ID.
  worker-image-amis     Print the last worker-image-wait region-to-AMI JSON map.
  control-image-build   Build the helmr-control container image.
  control-image-push    Push the built helmr-control image to ECR.
  dev-tfvars            Copy the dev tfvars template and inject worker_ami_id.
  dev-base-tfvars       Write non-secret tfvars for a staged base dev apply.
  dev-init              Initialize the dev stack backend.
  dev-apply             Apply the dev stack with the generated tfvars file.
  dev-secrets           Print the Secrets Manager ARNs that need values.
  dev-database-url      Populate the database_url secret from the RDS master secret.
  dev-generated-secrets Populate generated secret values except values supplied by external providers.
  dev-github-oauth-secret
                       Populate the GitHub OAuth client secret.
  dev-control-tfvars   Update dev tfvars to start the control service.
  dev-worker-tfvars    Configure cost-bounded managed run/build fleets at zero workers.
  dev-migrate           Run the ECS migration task for the dev stack.
  dev-destroy-prepare   Prepare an ephemeral dev stack for destroy.
  dev-destroy           Prepare and destroy an ephemeral dev stack.

Required environment:
  STATE_BUCKET          S3 bucket for Terraform/OpenTofu state; not needed for check/bootstrap-*.
  STATE_KEY             Optional S3 backend state key override.

Common optional environment:
  AWS_PROFILE           AWS CLI profile name; credentials are never written by this script.
  AWS_REGION            AWS region. Defaults to us-east-1.
  STATE_REGION          State bucket region. Defaults to AWS_REGION.
  TF_BIN                Terraform-compatible binary. Defaults to tofu.
  TOFU_APPLY_ARGS       Extra args for apply, for example "-auto-approve".
  TOFU_DESTROY_ARGS     Extra args for destroy, for example "-auto-approve".
  SOURCE_BUNDLE_BUCKET  S3 artifact bucket for local source bundles. Defaults to bootstrap output.
  ALLOW_VALIDATION_EVIDENCE_DELETE
                        Set to 1 only when intentionally deleting retained validation evidence
                        during bootstrap destruction.
Worker image optional environment:
  BOOTSTRAP_NAME           Bootstrap resource name. Defaults to helmr-dev.
  WORKER_IMAGE_NAME        Stack name. Defaults to helmr-dev-image.
  WORKER_IMAGE_SOURCE_REPOSITORY_URL
                           Git repository cloned by Image Builder.
  WORKER_IMAGE_SOURCE_REF  Git ref checked out by Image Builder. Defaults to the current branch.
  WORKER_IMAGE_SOURCE_BUNDLE_S3_URI
                           S3 git bundle URI. Defaults to the last source-bundle result.
  WORKER_IMAGE_VERSION     Optional Image Builder component/recipe version for immutable updates.
  WORKER_IMAGE_INSTANCE_PROFILE_NAME
                           Existing EC2 instance profile for Image Builder. Defaults to module-managed.
  WORKER_IMAGE_DISTRIBUTION_REGIONS
                           Optional comma-separated AWS regions for Image Builder AMI distribution.
  WORKER_IMAGE_AMI_PUBLIC  Set to 1 or true to make distributed worker AMIs public.
  WORKER_IMAGE_ROOT_VOLUME_ENCRYPTED
                           Set to 0 or false for public official AMIs.
  SKIP_SOURCE_REF_CHECK    Set to 1 to skip the remote ref check.

Control image environment:
  CONTROL_IMAGE_TAG         Immutable image tag. Defaults to a unique source/time/process tag.
  CONTROL_IMAGE_PLATFORM    Docker platform. Defaults to linux/amd64.
  ROTATE_DEV_SECRETS        Set to 1 to replace generated dev secret values except immutable Workspace fencing authority.

Dev optional environment:
  DEV_TFVARS            Generated tfvars path. Defaults to infra/aws/stacks/dev/full-run-smoke.tfvars.
  DEV_NAME              Dev stack name. Defaults to helmr-smoke.
  DEV_PUBLIC_URL        External URL placeholder. Defaults to http://localhost.
  DEV_ENABLE_NAT_GATEWAY
                       Create a NAT Gateway for private egress. Defaults to false for control mode.
  DEV_CONTROL_IMAGE     Digest-pinned Control release image. Defaults to the last control-image-push result.
  DEV_CONTROL_ASSIGN_PUBLIC_IP
                       Run control tasks in public subnets. Defaults to 1 for control mode.
  DEV_GITHUB_OAUTH_CLIENT_ID
                       Initial GitHub OAuth client ID placeholder. Defaults to placeholder.
  DEV_CREATE_CLICKHOUSE_CLOUD
                       Set to true to create ClickHouse Cloud, AWS PrivateLink, DNS, and password secret with Terraform.
  DEV_CLICKHOUSE_ORGANIZATION_ID
                       ClickHouse Cloud organization ID for Terraform-managed ClickHouse.
  CLICKHOUSE_CLOUD_API_KEY
                       ClickHouse Cloud API key ID for the Terraform provider when DEV_CREATE_CLICKHOUSE_CLOUD=true.
  CLICKHOUSE_CLOUD_API_SECRET
                       ClickHouse Cloud API key secret for the Terraform provider when DEV_CREATE_CLICKHOUSE_CLOUD=true.
  DEV_CLICKHOUSE_CLOUD_SERVICE_NAME
                       Optional ClickHouse Cloud service name. Defaults to <DEV_NAME>-telemetry.
  DEV_CLICKHOUSE_CLOUD_REGION
                       Optional ClickHouse Cloud AWS region. Defaults to AWS_REGION.
  DEV_CLICKHOUSE_SECRET_KMS_KEY_ID
                       Optional KMS key ID or ARN for the Terraform-managed ClickHouse password secret.
  DEV_CLICKHOUSE_URL   ClickHouse Cloud HTTPS endpoint for dev telemetry.
  DEV_CLICKHOUSE_USER  ClickHouse username. Defaults to default.
  DEV_CLICKHOUSE_PASSWORD_SECRET_ARN
                       Secrets Manager ARN containing the ClickHouse password.
  DEV_CLICKHOUSE_PASSWORD_KMS_KEY_ARNS
                       Optional JSON array of KMS key ARNs for the ClickHouse password secret.
  DEV_ADDITIONAL_CONTROL_SECURITY_GROUP_IDS
                       Optional JSON array of security group IDs to attach to control tasks.
  DEV_CONTROL_DESIRED_COUNT
                       Control ECS desired task count. Defaults to 1 for dev cost control.
  DEV_CONTROL_KEEP_WORKER
                       Set to 1 to leave existing worker capacity settings untouched.
  DEV_WORKER_VM_SCRATCH_DISK_MIB
                       Writable disk in MiB for dev Firecracker task VMs. Defaults to 32768 in run mode.
  DEV_WORKER_MAX_SIZE  Run-worker ASG ceiling. Defaults to 1.
  DEV_WORKER_EXECUTION_SLOTS
                       Execution slots advertised by each run Worker. Defaults to 1.
  DEV_RUN_WARM_WORKERS Run Workers held ready by the fleet controller. Defaults to 0.
  DEV_RUN_MAX_WORKERS  Fleet-controller run-worker ceiling. Defaults to DEV_WORKER_MAX_SIZE.
  DEV_ALLOW_EXTENDED_WORKER_CAPACITY
                       Must be true when either Worker ASG ceiling exceeds 1.
  WORKER_AMI_ID         AMI ID to inject; defaults to the last worker-image-wait result.
  HELMR_GITHUB_OAUTH_CLIENT_SECRET
                        GitHub OAuth client secret for dev-github-oauth-secret. Use
                        scripts/dev-secrets.sh so 1Password injects it only for the command.
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

require_clean_product_checkout() {
  [ -z "$(git -C "${ROOT}" status --porcelain --untracked-files=all)" ] ||
    die "authenticated dev artifacts require a clean checkout at the exact source commit"
}

sha256_stdin() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 | awk '{print $1}'
  else
    die "sha256sum or shasum is required"
  fi
}

sha256_file() {
  path="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${path}" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${path}" | awk '{print $1}'
  else
    die "sha256sum or shasum is required"
  fi
}

need_state_bucket() {
  [ -n "${STATE_BUCKET:-}" ] || die "STATE_BUCKET is required"
}

sensitive_mktemp() {
  local name=$1
  local tmpdir="${TMPDIR:-/tmp}"
  local tmpfile
  tmpfile="$(mktemp "${tmpdir%/}/helmr-${name}.XXXXXX")"
  chmod 0600 "${tmpfile}"
  printf '%s\n' "${tmpfile}"
}

tf_init() {
  stack=$1
  need_state_bucket
  backend_args=(
    "-backend-config=bucket=${STATE_BUCKET}"
    "-backend-config=region=${STATE_REGION}"
  )
  if [ -n "${STATE_KEY}" ]; then
    backend_args+=("-backend-config=key=${STATE_KEY}")
  fi
  "${TF_BIN}" -chdir="${stack}" init \
    -reconfigure \
    "${backend_args[@]}"
}

tf_apply() {
  stack=$1
  shift
  if [ -n "${TOFU_APPLY_ARGS:-}" ]; then
    had_noglob=0
    case $- in
      *f*) had_noglob=1 ;;
    esac
    set -f
    # shellcheck disable=SC2206
    extra_args=(${TOFU_APPLY_ARGS})
    if [ "${had_noglob}" != "1" ]; then
      set +f
    fi
    "${TF_BIN}" -chdir="${stack}" apply "${extra_args[@]}" "$@"
  else
    "${TF_BIN}" -chdir="${stack}" apply "$@"
  fi
}

tf_destroy() {
  stack=$1
  shift
  if [ -n "${TOFU_DESTROY_ARGS:-}" ]; then
    had_noglob=0
    case $- in
      *f*) had_noglob=1 ;;
    esac
    set -f
    # shellcheck disable=SC2206
    extra_args=(${TOFU_DESTROY_ARGS})
    if [ "${had_noglob}" != "1" ]; then
      set +f
    fi
    "${TF_BIN}" -chdir="${stack}" destroy "${extra_args[@]}" "$@"
  else
    "${TF_BIN}" -chdir="${stack}" destroy "$@"
  fi
}

check() {
  need_command "${TF_BIN}"
  need_command aws
  need_command jq
  info "tool: $(${TF_BIN} version | head -n 1)"
  info "tool: $(aws --version)"
  info "region: ${AWS_REGION}"
  aws sts get-caller-identity --region "${AWS_REGION}"
}

bootstrap_init() {
  "${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" init -backend=false
}

bootstrap_principal_arn() {
  caller_arn="$(aws sts get-caller-identity --region "${AWS_REGION}" --query Arn --output text)"
  case "${caller_arn}" in
    arn:*:sts::*:assumed-role/*/*)
      partition="$(printf '%s\n' "${caller_arn}" | cut -d: -f2)"
      account_id="$(printf '%s\n' "${caller_arn}" | cut -d: -f5)"
      role_and_session="${caller_arn#*:assumed-role/}"
      printf 'arn:%s:iam::%s:role/%s\n' "${partition}" "${account_id}" "${role_and_session%/*}"
      ;;
    arn:*:iam::*:role/* | arn:*:iam::*:user/*)
      printf '%s\n' "${caller_arn}"
      ;;
    *)
      die "bootstrap requires an IAM role or user principal; set the Platform publisher principal JSON override when using ${caller_arn}"
      ;;
  esac
}

bootstrap_apply() {
  principal_arn="$(bootstrap_principal_arn)"
  publishers="${PLATFORM_PUBLISHER_PRINCIPAL_ARNS_JSON:-$(jq -cn --arg arn "${principal_arn}" '[$arn]')}"
  printf '%s\n' "${publishers}" | jq -e 'type == "array" and length > 0 and all(.[]; type == "string")' >/dev/null ||
    die "PLATFORM_PUBLISHER_PRINCIPAL_ARNS_JSON must be a non-empty JSON string array"
  tf_apply "${BOOTSTRAP_STACK}" \
    -var="name=${BOOTSTRAP_NAME}" \
    -var="platform_publisher_principal_arns=${publishers}"
}

bootstrap_output() {
  bucket="$("${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" output -raw bucket_name)"
  artifact_bucket="$("${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" output -raw source_artifact_bucket_name)"
  printf 'export STATE_BUCKET=%q\n' "${bucket}"
  printf 'export STATE_REGION=%q\n' "${STATE_REGION}"
  printf 'export SOURCE_BUNDLE_BUCKET=%q\n' "${artifact_bucket}"
  for output_name in \
    control_release_repository_url \
    control_release_repository_arn \
    platform_publisher_role_arn \
    platform_store_uri \
    platform_store_bucket_arn \
    platform_store_kms_key_arn \
    retained_cas_uri \
    retained_cas_bucket_arn \
    retained_cas_kms_key_arn; do
    value="$("${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" output -raw "${output_name}")"
    printf 'export %s=%q\n' "$(printf '%s' "${output_name}" | tr '[:lower:]' '[:upper:]')" "${value}"
  done
}

bootstrap_contract_value() {
  environment_name="$1"
  output_name="$2"
  value="${!environment_name:-}"
  if [ -z "${value}" ]; then
    value="$("${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" output -raw "${output_name}")"
  fi
  [ -n "${value}" ] || die "${environment_name} is required"
  printf '%s\n' "${value}"
}

with_platform_publisher() {
  local role_arn credentials access_key_id secret_access_key session_token
  role_arn="$(bootstrap_contract_value PLATFORM_PUBLISHER_ROLE_ARN platform_publisher_role_arn)"
  credentials="$(
    aws sts assume-role \
      --region "${AWS_REGION}" \
      --role-arn "${role_arn}" \
      --role-session-name "helmr-release-$(date -u +%s)-$$" \
      --output json
  )"
  printf '%s\n' "${credentials}" | jq -e '
    .Credentials |
    (.AccessKeyId | type == "string" and length > 0) and
    (.SecretAccessKey | type == "string" and length > 0) and
    (.SessionToken | type == "string" and length > 0)
  ' >/dev/null || die "platform publisher role did not return complete temporary credentials"

  access_key_id="$(printf '%s\n' "${credentials}" | jq -r '.Credentials.AccessKeyId')"
  secret_access_key="$(printf '%s\n' "${credentials}" | jq -r '.Credentials.SecretAccessKey')"
  session_token="$(printf '%s\n' "${credentials}" | jq -r '.Credentials.SessionToken')"
  env -u AWS_PROFILE -u AWS_DEFAULT_PROFILE \
    AWS_ACCESS_KEY_ID="${access_key_id}" \
    AWS_SECRET_ACCESS_KEY="${secret_access_key}" \
    AWS_SESSION_TOKEN="${session_token}" \
    "$@"
}

delete_all_ecr_images() {
  repository=$1
  while :; do
    image_ids_file="$(mktemp "${STATE_DIR}/ecr-image-ids.XXXXXX.json")"
    trap 'rm -f "${image_ids_file}"' RETURN
    aws ecr list-images \
      --region "${AWS_REGION}" \
      --repository-name "${repository}" \
      --filter tagStatus=ANY \
      --max-items 100 \
      --query 'imageIds' \
      --output json >"${image_ids_file}"
    if [ "$(jq 'length' <"${image_ids_file}")" -eq 0 ]; then
      rm -f "${image_ids_file}"
      trap - RETURN
      break
    fi
    delete_response="$(
      aws ecr batch-delete-image \
        --region "${AWS_REGION}" \
        --repository-name "${repository}" \
        --image-ids "file://${image_ids_file}" \
        --output json
    )"
    if ! jq -e '(.failures // []) | length == 0' <<<"${delete_response}" >/dev/null; then
      failure_summary="$(jq -c '[.failures[] | {failureCode, failureReason, imageId}]' <<<"${delete_response}")"
      die "ECR image deletion returned failures: ${failure_summary}"
    fi
    rm -f "${image_ids_file}"
    trap - RETURN
  done
  info "emptied ECR repository: ${repository}"
}

delete_all_s3_object_versions() {
  bucket=$1
  bucket_region=${2:-${STATE_REGION}}
  mkdir -p "${STATE_DIR}"
  while :; do
    delete_file="$(mktemp "${STATE_DIR}/s3-delete.XXXXXX.json")"
    trap 'rm -f "${delete_file}"' RETURN
    aws s3api list-object-versions \
      --region "${bucket_region}" \
      --bucket "${bucket}" \
      --output json |
      jq '{Objects: ((((.Versions // []) + (.DeleteMarkers // [])) | map({Key} + (if ((.VersionId // "") == "" or .VersionId == "null") then {} else {VersionId} end)))[:1000])}' >"${delete_file}"
    if [ "$(jq '.Objects | length' <"${delete_file}")" -eq 0 ]; then
      rm -f "${delete_file}"
      trap - RETURN
      break
    fi
    aws s3api delete-objects \
      --region "${bucket_region}" \
      --bucket "${bucket}" \
      --delete "file://${delete_file}" >/dev/null
    rm -f "${delete_file}"
    trap - RETURN
  done
  info "emptied versioned bucket: ${bucket}"
}

bootstrap_destroy_prepare() {
  state_bucket="$("${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" output -raw bucket_name)"
  artifact_bucket="$("${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" output -raw source_artifact_bucket_name)"
  runtime_bucket_arn="$("${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" output -raw platform_store_bucket_arn)"
  retained_bucket_arn="$("${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" output -raw retained_cas_bucket_arn)"
  control_release_repository="$("${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" output -raw control_release_repository_name)"
  if [ "${ALLOW_VALIDATION_EVIDENCE_DELETE:-0}" != "1" ]; then
    for protected_prefix in helmr/validation-evidence/ helmr/validation-claims/; do
      evidence_versions="$(
        aws s3api list-object-versions \
          --bucket "${artifact_bucket}" \
          --prefix "${protected_prefix}" \
          --max-items 1 \
          --output json
      )"
      if ! printf '%s\n' "${evidence_versions}" | jq -e \
        '((.Versions // []) + (.DeleteMarkers // [])) | length == 0' >/dev/null; then
        die "bootstrap artifact bucket contains retained validation claims or evidence; set ALLOW_VALIDATION_EVIDENCE_DELETE=1 only after explicit approval"
      fi
    done
  fi
  if [ "${ALLOW_RETAINED_STORE_DELETE:-0}" != "1" ]; then
    die "bootstrap teardown includes retained runtime and deployment artifacts; set ALLOW_RETAINED_STORE_DELETE=1 only for an explicitly approved teardown"
  fi
  runtime_bucket="${runtime_bucket_arn##*:}"
  retained_bucket="${retained_bucket_arn##*:}"
  aws s3api delete-bucket-policy --region "${STATE_REGION}" --bucket "${runtime_bucket}"
  aws s3api delete-bucket-policy --region "${STATE_REGION}" --bucket "${retained_bucket}"
  delete_all_s3_object_versions "${runtime_bucket}"
  delete_all_s3_object_versions "${retained_bucket}"
  delete_all_s3_object_versions "${artifact_bucket}"
  delete_all_s3_object_versions "${state_bucket}"
  delete_all_ecr_images "${control_release_repository}"
}

state_bucket_arn() {
  need_state_bucket
  printf 'arn:aws:s3:::%s\n' "${STATE_BUCKET}"
}

source_bundle_bucket() {
  if [ -n "${SOURCE_BUNDLE_BUCKET:-}" ]; then
    printf '%s\n' "${SOURCE_BUNDLE_BUCKET}"
    return 0
  fi
  if artifact_bucket="$("${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" output -raw source_artifact_bucket_name 2>/dev/null)"; then
    printf '%s\n' "${artifact_bucket}"
    return 0
  fi
  die "SOURCE_BUNDLE_BUCKET is required; run bootstrap-apply after this branch and export bootstrap-output"
}

tf_bool() {
  value="$(printf '%s\n' "$1" | tr '[:upper:]' '[:lower:]')"
  case "${value}" in
    1|true|yes|on) printf 'true\n' ;;
    0|false|no|off) printf 'false\n' ;;
    *) die "invalid boolean value: $1" ;;
  esac
}

dev_region_id() {
  printf '%s\n' "${DEV_REGION_ID:-${AWS_REGION}}"
}

dev_default_region_id() {
  printf '%s\n' "${DEV_DEFAULT_REGION_ID:-$(dev_region_id)}"
}

dev_worker_group_id() {
  printf '%s\n' "${DEV_WORKER_GROUP_ID:-$(dev_region_id)-worker-group-1}"
}

source_bundle_object_arn() {
  uri=$1
  case "${uri}" in
    s3://*/*)
      without_scheme=${uri#s3://}
      bucket=${without_scheme%%/*}
      key=${without_scheme#*/}
      printf 'arn:aws:s3:::%s/%s\n' "${bucket}" "${key}"
      ;;
    *)
      die "source bundle URI must be an s3:// URI: ${uri}"
      ;;
  esac
}

worker_image_version() {
  if [ -n "${WORKER_IMAGE_VERSION}" ]; then
    printf '%s\n' "${WORKER_IMAGE_VERSION}"
    return 0
  fi
  revision="$(git -C "${ROOT}" rev-parse --short=8 HEAD)"
  printf '0.1.%d\n' "$((16#${revision} % 1000000))"
}

bucket_kms_key_arn() {
  bucket=$1
  aws s3api get-bucket-encryption \
    --region "${STATE_REGION}" \
    --bucket "${bucket}" \
    --query 'ServerSideEncryptionConfiguration.Rules[0].ApplyServerSideEncryptionByDefault.KMSMasterKeyID' \
    --output text 2>/dev/null | awk '$0 != "None" { print }'
}

source_bundle_uri() {
  if [ -n "${WORKER_IMAGE_SOURCE_BUNDLE_S3_URI:-}" ]; then
    printf '%s\n' "${WORKER_IMAGE_SOURCE_BUNDLE_S3_URI}"
  elif [ -f "${SOURCE_BUNDLE_URI_FILE}" ]; then
    cat "${SOURCE_BUNDLE_URI_FILE}"
  fi
}

source_bundle_ref() {
  if [ -n "${WORKER_IMAGE_SOURCE_BUNDLE_S3_URI:-}" ]; then
    git -C "${ROOT}" rev-parse HEAD
  elif [ -f "${SOURCE_BUNDLE_REF_FILE}" ]; then
    cat "${SOURCE_BUNDLE_REF_FILE}"
  else
    printf '%s\n' "${WORKER_IMAGE_SOURCE_REF}"
  fi
}

resolve_remote_source_ref() {
  info "checking source ref ${WORKER_IMAGE_SOURCE_REF} in ${WORKER_IMAGE_SOURCE_REPOSITORY_URL}"
  refs="$(git ls-remote --exit-code --heads --tags "${WORKER_IMAGE_SOURCE_REPOSITORY_URL}" "${WORKER_IMAGE_SOURCE_REF}" 2>/dev/null || true)"
  if [ -n "${refs}" ]; then
    printf '%s\n' "${refs}" | awk '
      $2 ~ /\^\{\}$/ { print $1; found = 1; exit }
      first == "" { first = $1 }
      END { if (!found && first != "") print first }
    '
    return 0
  fi

  refs="$(git ls-remote "${WORKER_IMAGE_SOURCE_REPOSITORY_URL}" "${WORKER_IMAGE_SOURCE_REF}" 2>/dev/null || true)"
  if [ -n "${refs}" ]; then
    printf '%s\n' "${refs}" | awk 'NR == 1 { print $1 }'
    return 0
  fi

  if git ls-remote "${WORKER_IMAGE_SOURCE_REPOSITORY_URL}" 2>/dev/null | awk -v rev="${WORKER_IMAGE_SOURCE_REF}" '$1 == rev { found = 1 } END { exit found ? 0 : 1 }'; then
    printf '%s\n' "${WORKER_IMAGE_SOURCE_REF}"
    return 0
  fi

  die "source ref is not visible to Image Builder; push the branch/tag or set WORKER_IMAGE_SOURCE_REF"
}

source_bundle() {
  mkdir -p "${STATE_DIR}"
  source_ref="$(git -C "${ROOT}" rev-parse HEAD)"
  bucket="$(source_bundle_bucket)"
  s3_uri="s3://${bucket}/helmr/source-bundles/${source_ref}.bundle"
  git -C "${ROOT}" bundle create "${SOURCE_BUNDLE_FILE}" HEAD
  aws s3 cp --region "${AWS_REGION}" "${SOURCE_BUNDLE_FILE}" "${s3_uri}"
  printf '%s\n' "${s3_uri}" >"${SOURCE_BUNDLE_URI_FILE}"
  printf '%s\n' "${source_ref}" >"${SOURCE_BUNDLE_REF_FILE}"
  info "source bundle uploaded: ${s3_uri}"
  printf '%s\n' "${s3_uri}"
}

platform_release_publish() (
  local release publish_input object
  require_clean_product_checkout
  platform_store_uri="$(bootstrap_contract_value PLATFORM_STORE_URI platform_store_uri)"
  release="$(nix build -L --no-link --print-out-paths "${ROOT}#platformRelease")"
  mkdir -p "${STATE_DIR}"
  publish_input="$(mktemp -d "${STATE_DIR}/platform-release-publish.XXXXXX")"
  chmod 0700 "${publish_input}"
  trap 'rm -rf "${publish_input}"' EXIT
  install -d -m0700 "${publish_input}/objects/sha256"
  install -m0400 "${release}/platform-release.json" "${publish_input}/platform-release.json"
  while IFS= read -r -d '' object; do
    install -m0400 "${object}" "${publish_input}/objects/sha256/$(basename "${object}")"
  done < <(find "${release}/objects/sha256" -maxdepth 1 -type f -print0)
  with_platform_publisher nix develop "${ROOT}" -c go run ./cmd/helmr-control release publish \
    --store "${platform_store_uri}" \
    --input "${publish_input}"
  build_policy_digest="$(cat "${release}/build-policy.digest")"
  printf '%s\n' "${build_policy_digest}" >"${BUILD_POLICY_DIGEST_FILE}"
  info "Platform release published: ${build_policy_digest}"
  printf '%s\n' "${build_policy_digest}"
)

worker_image_source_check() {
  if [ -n "$(source_bundle_uri)" ]; then
    info "using source bundle: $(source_bundle_uri)"
    return 0
  fi
  [ "${SKIP_SOURCE_REF_CHECK:-}" != "1" ] || return 0
  resolve_remote_source_ref >/dev/null
}

worker_image_apply() {
  require_clean_product_checkout
  worker_image_source_check
  nix develop "${ROOT}#images" -c "${ROOT}/scripts/check-apko-lock.sh"
  bundle_uri="$(source_bundle_uri)"
  version_args=(-var="image_version=$(worker_image_version)")
  instance_profile_args=()
  if [ -n "${WORKER_IMAGE_INSTANCE_PROFILE_NAME}" ]; then
    instance_profile_args=(-var="instance_profile_name=${WORKER_IMAGE_INSTANCE_PROFILE_NAME}")
  fi
  distribution_args=()
  if [ -n "${WORKER_IMAGE_DISTRIBUTION_REGIONS}" ]; then
    distribution_regions_json="$(
      printf '%s\n' "${WORKER_IMAGE_DISTRIBUTION_REGIONS}" |
        jq -Rc 'split(",") | map(gsub("^\\s+|\\s+$"; "")) | map(select(length > 0))'
    )"
    distribution_args=(-var="distribution_regions=${distribution_regions_json}")
  fi
  public_args=()
  if [ -n "${WORKER_IMAGE_AMI_PUBLIC}" ]; then
    public_args=(-var="ami_public=$(tf_bool "${WORKER_IMAGE_AMI_PUBLIC}")")
  fi
  encryption_args=()
  if [ -n "${WORKER_IMAGE_ROOT_VOLUME_ENCRYPTED}" ]; then
    encryption_args=(-var="root_volume_encrypted=$(tf_bool "${WORKER_IMAGE_ROOT_VOLUME_ENCRYPTED}")")
  fi
  if [ -n "${bundle_uri}" ]; then
    source_ref="$(source_bundle_ref)"
    bundle_bucket="${bundle_uri#s3://}"
    bundle_bucket="${bundle_bucket%%/*}"
    kms_key_arn="$(bucket_kms_key_arn "${bundle_bucket}")"
    kms_args=()
    if [ -n "${kms_key_arn}" ]; then
      kms_args=(-var="source_bundle_kms_key_arn=${kms_key_arn}")
    fi
    apply_args=(
      -var="aws_region=${AWS_REGION}" \
      -var="name=${WORKER_IMAGE_NAME}" \
      -var="source_ref=${source_ref}" \
      -var="source_bundle_s3_uri=${bundle_uri}" \
      -var="source_bundle_object_arn=$(source_bundle_object_arn "${bundle_uri}")"
    )
    if ((${#distribution_args[@]})); then apply_args+=("${distribution_args[@]}"); fi
    if ((${#instance_profile_args[@]})); then apply_args+=("${instance_profile_args[@]}"); fi
    if ((${#public_args[@]})); then apply_args+=("${public_args[@]}"); fi
    if ((${#encryption_args[@]})); then apply_args+=("${encryption_args[@]}"); fi
    if ((${#kms_args[@]})); then apply_args+=("${kms_args[@]}"); fi
    if ((${#version_args[@]})); then apply_args+=("${version_args[@]}"); fi
    tf_apply "${WORKER_IMAGE_STACK}" "${apply_args[@]}"
  else
    source_ref="$(resolve_remote_source_ref)"
    apply_args=(
      -var="aws_region=${AWS_REGION}" \
      -var="name=${WORKER_IMAGE_NAME}" \
      -var="source_repository_url=${WORKER_IMAGE_SOURCE_REPOSITORY_URL}" \
      -var="source_ref=${source_ref}"
    )
    if ((${#distribution_args[@]})); then apply_args+=("${distribution_args[@]}"); fi
    if ((${#instance_profile_args[@]})); then apply_args+=("${instance_profile_args[@]}"); fi
    if ((${#public_args[@]})); then apply_args+=("${public_args[@]}"); fi
    if ((${#encryption_args[@]})); then apply_args+=("${encryption_args[@]}"); fi
    if ((${#version_args[@]})); then apply_args+=("${version_args[@]}"); fi
    tf_apply "${WORKER_IMAGE_STACK}" "${apply_args[@]}"
  fi
}

worker_image_start() {
  mkdir -p "${STATE_DIR}"
  pipeline_arn="$("${TF_BIN}" -chdir="${WORKER_IMAGE_STACK}" output -raw image_pipeline_arn)"
  token="helmr-$(date -u +%Y%m%d%H%M%S)-$$"
  info "starting Image Builder pipeline: ${pipeline_arn}"
  image_arn="$(
    aws imagebuilder start-image-pipeline-execution \
      --region "${AWS_REGION}" \
      --image-pipeline-arn "${pipeline_arn}" \
      --client-token "${token}" \
      --query imageBuildVersionArn \
      --output text
  )"
  [ -n "${image_arn}" ] && [ "${image_arn}" != "None" ] || die "Image Builder did not return an image build version ARN"
  printf '%s\n' "${image_arn}" >"${IMAGE_ARN_FILE}"
  info "image build version ARN recorded at ${IMAGE_ARN_FILE}"
  printf '%s\n' "${image_arn}"
}

worker_image_wait() {
  mkdir -p "${STATE_DIR}"
  image_arn="${1:-${WORKER_IMAGE_BUILD_VERSION_ARN:-}}"
  if [ -z "${image_arn}" ] && [ -f "${IMAGE_ARN_FILE}" ]; then
    image_arn="$(cat "${IMAGE_ARN_FILE}")"
  fi
  [ -n "${image_arn}" ] || die "image build version ARN is required; run worker-image-start first"

  deadline=$((SECONDS + IMAGE_WAIT_TIMEOUT_SECONDS))
  while :; do
    image_json="$(
      aws imagebuilder get-image \
        --region "${AWS_REGION}" \
        --image-build-version-arn "${image_arn}" \
        --output json
    )"
    status="$(printf '%s\n' "${image_json}" | jq -r '.image.state.status')"
    reason="$(printf '%s\n' "${image_json}" | jq -r '.image.state.reason // ""')"
    info "Image Builder status: ${status}${reason:+ (${reason})}"

    case "${status}" in
      AVAILABLE)
        ami_ids_json="$(
          printf '%s\n' "${image_json}" |
            jq -c '[.image.outputResources.amis[]? | select(.region != null and .image != null) | {key: .region, value: .image}] | from_entries'
        )"
        [ "$(printf '%s\n' "${ami_ids_json}" | jq 'length')" -gt 0 ] || die "image is AVAILABLE but no AMIs were returned"
        ami_id="$(printf '%s\n' "${ami_ids_json}" | jq -r --arg region "${AWS_REGION}" '.[$region] // empty')"
        [ -n "${ami_id}" ] || die "image is AVAILABLE but does not include an AMI for AWS_REGION=${AWS_REGION}"
        recipe_arn="$("${TF_BIN}" -chdir="${WORKER_IMAGE_STACK}" output -raw image_recipe_arn)"
        [ "$(printf '%s\n' "${image_json}" | jq -er '.image.imageRecipe.arn')" = "${recipe_arn}" ] ||
          die "available Worker image was not built from the applied image recipe"
        source_commit="$(git -C "${ROOT}" rev-parse HEAD)"
        ami_json="$(
          aws ec2 describe-images \
            --region "${AWS_REGION}" \
            --owners self \
            --image-ids "${ami_id}" \
            --output json
        )"
        jq -e \
          --arg ami "${ami_id}" \
          --arg commit "${source_commit}" '
          (.Images // []) as $images |
          (($images[0].Tags // []) | map({key: .Key, value: .Value}) | from_entries) as $tags |
          ($images | length) == 1 and
          $images[0].ImageId == $ami and
          $tags.HelmrSourceCommit == $commit
        ' <<<"${ami_json}" >/dev/null ||
          die "available Worker AMI is not bound to the exact source commit"
        jq -cn \
          --arg ami "${ami_id}" \
          --arg build_arn "${image_arn}" \
          --arg recipe_arn "${recipe_arn}" \
          --arg region "${AWS_REGION}" \
          --arg source_commit "${source_commit}" \
          '{
            ami: {id: $ami, region: $region},
            formatVersion: 0,
            imageBuildVersionARN: $build_arn,
            imageRecipeARN: $recipe_arn,
            sourceCommit: $source_commit
          }' >"${WORKER_IMAGE_PROVENANCE_FILE}"
        chmod 0600 "${WORKER_IMAGE_PROVENANCE_FILE}"
        printf '%s\n' "${ami_id}" >"${AMI_ID_FILE}"
        printf '%s\n' "${ami_ids_json}" >"${AMI_IDS_FILE}"
        info "worker AMI ID recorded at ${AMI_ID_FILE}"
        info "worker AMI region map recorded at ${AMI_IDS_FILE}"
        printf '%s\n' "${ami_id}"
        return 0
        ;;
      FAILED|CANCELLED)
        die "Image Builder finished with ${status}: ${reason}"
        ;;
    esac

    [ "${SECONDS}" -lt "${deadline}" ] || die "timed out waiting for Image Builder after ${IMAGE_WAIT_TIMEOUT_SECONDS}s"
    sleep "${IMAGE_WAIT_INTERVAL_SECONDS}"
  done
}

worker_image_amis() {
  [ -f "${AMI_IDS_FILE}" ] || die "worker AMI region map not found; run worker-image-wait first"
  jq -c . "${AMI_IDS_FILE}"
}

control_image_repository() {
  bootstrap_contract_value CONTROL_RELEASE_REPOSITORY_URL control_release_repository_url
}

control_image_uri() {
  repository="$(control_image_repository)"
  tag="${CONTROL_IMAGE_TAG:-$(git -C "${ROOT}" rev-parse --short=12 HEAD)-$(date -u +%Y%m%d%H%M%S)-$$}"
  printf '%s:%s\n' "${repository}" "${tag}"
}

control_image_digest_uri() {
  image_uri=$1
  [ "${image_uri#*@}" = "${image_uri}" ] || die "control-image-push requires a tag image URI, got digest-pinned image: ${image_uri}"

  repository="${image_uri%:*}"
  tag="${image_uri##*:}"
  repository_name="${repository#*/}"
  [ -n "${repository_name}" ] && [ "${repository_name}" != "${repository}" ] || die "control image URI must include an ECR registry: ${image_uri}"
  [ -n "${tag}" ] && [ "${tag}" != "${image_uri}" ] || die "control image URI must include a tag: ${image_uri}"

  digest="$(with_platform_publisher aws ecr describe-images \
    --region "${AWS_REGION}" \
    --repository-name "${repository_name}" \
    --image-ids "imageTag=${tag}" \
    --query 'imageDetails[0].imageDigest' \
    --output text)"
  case "${digest}" in
    sha256:*) printf '%s@%s\n' "${repository}" "${digest}" ;;
    *) die "could not resolve pushed digest for ${image_uri}" ;;
  esac
}

control_image_context() {
  printf '%s\n' "${STATE_DIR}/control-image"
}

control_image_build() {
  need_command docker
  require_clean_product_checkout
  image_uri="$(control_image_uri)"
  context="$(control_image_context)"

  # shellcheck disable=SC2016
  nix develop "${ROOT}#images" -c env \
    CONTROL_IMAGE_CONTEXT="${context}" \
    IMAGE_URI="${image_uri}" \
    bash -ceu '
      cd "$1"
      ./scripts/build-control-image.sh "$IMAGE_URI"
    ' bash "${ROOT}"

  printf '%s\n' "${image_uri}" >"${CONTROL_IMAGE_URI_FILE}"
  info "control image built: ${image_uri}"
  printf '%s\n' "${image_uri}"
}

control_image_push() {
  need_command aws
  need_command docker
  require_clean_product_checkout
  image_uri="${CONTROL_IMAGE_URI:-}"
  if [ -z "${image_uri}" ] && [ -f "${CONTROL_IMAGE_URI_FILE}" ]; then
    image_uri="$(cat "${CONTROL_IMAGE_URI_FILE}")"
  fi
  [ -n "${image_uri}" ] || die "CONTROL_IMAGE_URI is required, or run control-image-build first"
  build_inputs_file="$(control_image_context)/build-inputs.json"
  [ -f "${build_inputs_file}" ] || die "Control image build-input receipt is missing; run control-image-build first"
  expected_source_commit="$(git -C "${ROOT}" rev-parse HEAD)"
  "${ROOT}/scripts/verify-control-image-build.sh" "${build_inputs_file}" "${image_uri}" ||
    die "Control image build-input verification failed"
  registry="${image_uri%%/*}"
  (
    docker_config="$(mktemp -d "${STATE_DIR}/docker-config.XXXXXX")"
    trap 'docker --config "${docker_config}" logout "${registry}" >/dev/null 2>&1 || true; rm -rf "${docker_config}"' EXIT
    with_platform_publisher aws ecr get-login-password --region "${AWS_REGION}" |
      docker --config "${docker_config}" login --username AWS --password-stdin "${registry}"
    docker --config "${docker_config}" push "${image_uri}"
  )
  digest_image_uri="$(control_image_digest_uri "${image_uri}")"
  repository="${digest_image_uri%@*}"
  repository_name="${repository#*/}"
  digest="${digest_image_uri#*@}"
  jq -cn \
    --arg digest "${digest}" \
    --arg repository "${repository_name}" \
    --arg source_commit "${expected_source_commit}" \
    --slurpfile build_inputs "${build_inputs_file}" \
    '{
      buildInputs: $build_inputs[0],
      formatVersion: 1,
      image: {digest: $digest, repository: $repository},
      sourceCommit: $source_commit
    }' >"${CONTROL_IMAGE_PROVENANCE_FILE}"
  chmod 0600 "${CONTROL_IMAGE_PROVENANCE_FILE}"
  printf '%s\n' "${digest_image_uri}" >"${CONTROL_IMAGE_URI_FILE}"
  info "control image pushed: ${digest_image_uri}"
  printf '%s\n' "${digest_image_uri}"
}

dev_tfvars() {
  ami_id="${WORKER_AMI_ID:-}"
  if [ -z "${ami_id}" ] && [ -f "${AMI_ID_FILE}" ]; then
    ami_id="$(cat "${AMI_ID_FILE}")"
  fi
  [ -n "${ami_id}" ] || die "WORKER_AMI_ID is required, or run worker-image-wait first"

  if [ ! -f "${DEV_TFVARS}" ]; then
    cp "${DEV_TFVARS_TEMPLATE}" "${DEV_TFVARS}"
  fi

  tmp="${DEV_TFVARS}.tmp"
  awk -v ami="${ami_id}" '
    BEGIN { done = 0 }
    /^worker_ami_id[[:space:]]*=/ {
      print "worker_ami_id = \"" ami "\""
      done = 1
      next
    }
    { print }
    END {
      if (done == 0) {
        print "worker_ami_id = \"" ami "\""
      }
    }
  ' "${DEV_TFVARS}" >"${tmp}"
  mv "${tmp}" "${DEV_TFVARS}"
  info "updated ${DEV_TFVARS}"
}

apply_bootstrap_contract_tfvars() {
  tfvars_file="$1"
  platform_store_uri="$(bootstrap_contract_value PLATFORM_STORE_URI platform_store_uri)"
  platform_store_bucket_arn="$(bootstrap_contract_value PLATFORM_STORE_BUCKET_ARN platform_store_bucket_arn)"
  platform_store_kms_key_arn="$(bootstrap_contract_value PLATFORM_STORE_KMS_KEY_ARN platform_store_kms_key_arn)"
  build_policy_digest="${DEV_BUILD_POLICY_DIGEST:-}"
  if [ -z "${build_policy_digest}" ] && [ -f "${BUILD_POLICY_DIGEST_FILE}" ]; then
    build_policy_digest="$(cat "${BUILD_POLICY_DIGEST_FILE}")"
  fi
  printf '%s\n' "${build_policy_digest}" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
    die "publish the Platform release first or set DEV_BUILD_POLICY_DIGEST"

  set_tfvar "${tfvars_file}" "platform_store_uri" "$(tf_quote "${platform_store_uri}")"
  set_tfvar "${tfvars_file}" "platform_store_bucket_arn" "$(tf_quote "${platform_store_bucket_arn}")"
  set_tfvar "${tfvars_file}" "platform_store_kms_key_arn" "$(tf_quote "${platform_store_kms_key_arn}")"
  set_tfvar "${tfvars_file}" "build_policy_digest" "$(tf_quote "${build_policy_digest}")"
}

dev_base_tfvars() {
  mkdir -p "$(dirname "${DEV_TFVARS}")"
  control_image="${DEV_CONTROL_IMAGE:-}"
  if [ -z "${control_image}" ] && [ -f "${CONTROL_IMAGE_URI_FILE}" ]; then
    control_image="$(cat "${CONTROL_IMAGE_URI_FILE}")"
  fi
  [ -n "${control_image}" ] ||
    die "a digest-pinned Control release image is required; run control-image-build and control-image-push first"
  control_image_repository_arn="$(bootstrap_contract_value CONTROL_RELEASE_REPOSITORY_ARN control_release_repository_arn)"
  certificate_arn_value="null"
  if [ -n "${DEV_CERTIFICATE_ARN:-}" ]; then
    certificate_arn_value="$(tf_quote "${DEV_CERTIFICATE_ARN}")"
  fi
  cloudfront_origin_value="null"
  if [ "${DEV_ENABLE_CLOUDFRONT:-false}" = "true" ]; then
    [ -n "${DEV_CLOUDFRONT_ORIGIN_DOMAIN_NAME:-}" ] || die "DEV_CLOUDFRONT_ORIGIN_DOMAIN_NAME is required when DEV_ENABLE_CLOUDFRONT=true"
    cloudfront_origin_value="$(tf_quote "${DEV_CLOUDFRONT_ORIGIN_DOMAIN_NAME}")"
  fi
  cat >"${DEV_TFVARS}" <<EOF
aws_region = "${AWS_REGION}"
name       = "${DEV_NAME:-helmr-smoke}"

worker_group_id  = "$(dev_worker_group_id)"
region_id        = "$(dev_region_id)"
default_region_id = "$(dev_default_region_id)"

public_url                    = "${DEV_PUBLIC_URL:-http://localhost}"
enable_nat_gateway            = ${DEV_ENABLE_NAT_GATEWAY:-false}
control_image                 = "${control_image}"
control_image_repository_arn  = "${control_image_repository_arn}"
certificate_arn               = ${certificate_arn_value}
allow_insecure_http           = ${DEV_ALLOW_INSECURE_HTTP:-true}
enable_cloudfront             = ${DEV_ENABLE_CLOUDFRONT:-false}
cloudfront_origin_domain_name = ${cloudfront_origin_value}

github_oauth_client_id = "${DEV_GITHUB_OAUTH_CLIENT_ID:-placeholder}"

create_control_service  = false
control_desired_count   = ${DEV_CONTROL_DESIRED_COUNT:-1}
dispatcher_desired_count = ${DEV_DISPATCHER_DESIRED_COUNT:-1}
control_assign_public_ip = ${DEV_CONTROL_ASSIGN_PUBLIC_IP:-true}
create_worker           = false

database_backup_retention_days              = ${DEV_DATABASE_BACKUP_RETENTION_DAYS:-1}
redis_node_type                             = "${DEV_REDIS_NODE_TYPE:-cache.t4g.micro}"
redis_node_count                            = ${DEV_REDIS_NODE_COUNT:-1}
control_log_retention_days                  = ${DEV_CONTROL_LOG_RETENTION_DAYS:-7}
kms_deletion_window_in_days                 = ${DEV_KMS_DELETION_WINDOW_IN_DAYS:-7}
secret_recovery_window_in_days              = ${DEV_SECRET_RECOVERY_WINDOW_IN_DAYS:-0}
cas_object_expiration_days                  = ${DEV_CAS_OBJECT_EXPIRATION_DAYS:-7}
cas_noncurrent_version_expiration_days      = ${DEV_CAS_NONCURRENT_VERSION_EXPIRATION_DAYS:-1}
worker_instance_type                = "c8i.xlarge"
worker_enable_nested_virtualization = true
worker_min_size                     = 0
worker_max_size                     = 1
build_worker_min_size               = 0
build_worker_max_size               = 1
worker_root_volume_size_gb          = ${DEV_WORKER_ROOT_VOLUME_SIZE_GB:-120}
worker_root_volume_iops             = ${DEV_WORKER_ROOT_VOLUME_IOPS:-3000}
worker_root_volume_throughput       = ${DEV_WORKER_ROOT_VOLUME_THROUGHPUT:-125}
worker_disk_mib                     = ${DEV_WORKER_DISK_MIB:-null}
worker_vm_vcpus                     = ${DEV_WORKER_VM_VCPUS:-2}
worker_vm_memory_mib                = ${DEV_WORKER_VM_MEMORY_MIB:-4096}
worker_vm_scratch_disk_mib          = ${DEV_WORKER_VM_SCRATCH_DISK_MIB:-32768}
EOF
  apply_bootstrap_contract_tfvars "${DEV_TFVARS}"
  apply_dev_clickhouse_tfvars "${DEV_TFVARS}"
  info "wrote ${DEV_TFVARS}"
}

dev_apply() {
  [ -f "${DEV_TFVARS}" ] || die "${DEV_TFVARS} does not exist; run dev-tfvars and fill required values first"
  tf_apply "${DEV_STACK}" -var-file="${DEV_TFVARS}"
  # A fresh database cannot make the control service healthy until its schema
  # exists. Run the idempotent migration task before waiting for ECS stability.
  dev_migrate
}

tf_quote() {
  jq -Rn --arg value "$1" '$value'
}

set_tfvar() {
  file=$1
  key=$2
  value=$3
  tmp="${file}.tmp"
  awk -v key="${key}" -v value="${value}" '
    function is_tfvar_assignment(line, key) {
      return line ~ "^[[:space:]]*" key "[[:space:]]*="
    }
    BEGIN { done = 0 }
    is_tfvar_assignment($0, key) {
      print key " = " value
      done = 1
      next
    }
    { print }
    END {
      if (done == 0) {
        print key " = " value
      }
    }
  ' "${file}" >"${tmp}"
  mv "${tmp}" "${file}"
}

unset_tfvar() {
  file=$1
  key=$2
  tmp="${file}.tmp"
  awk -v key="${key}" '
    function is_tfvar_assignment(line, key) {
      return line ~ "^[[:space:]]*" key "[[:space:]]*="
    }
    !is_tfvar_assignment($0, key)
  ' "${file}" >"${tmp}"
  mv "${tmp}" "${file}"
}

tfvar_value() {
  file=$1
  key=$2
  awk -v key="${key}" '
    function is_tfvar_assignment(line, key) {
      return line ~ "^[[:space:]]*" key "[[:space:]]*="
    }
    is_tfvar_assignment($0, key) {
      value = $0
      sub("^[[:space:]]*" key "[[:space:]]*=[[:space:]]*", "", value)
      split(value, parts, /[[:space:]]+/)
      print parts[1]
      found = 1
    }
    END { exit found ? 0 : 1 }
  ' "${file}"
}

tfvar_string_value() {
  file=$1
  key=$2
  value="$(tfvar_value "${file}" "${key}" 2>/dev/null || true)"
  [ -n "${value}" ] && [ "${value}" != "null" ] || return 1
  case "${value}" in
    \"*) printf '%s\n' "${value}" | jq -er '.' ;;
    *) printf '%s\n' "${value}" ;;
  esac
}

env_is_set() {
  eval '[ "${'"$1"'+set}" = set ]'
}

tf_json_string_array_or_empty() {
  name=$1
  value="${!name:-}"
  if [ -z "${value}" ]; then
    printf '[]\n'
    return 0
  fi
  printf '%s\n' "${value}" |
    jq -e 'type == "array" and all(.[]; type == "string")' >/dev/null ||
    die "${name} must be a JSON array of strings"
  printf '%s\n' "${value}"
}

apply_dev_clickhouse_tfvars() {
  file=$1
  create_clickhouse="${DEV_CREATE_CLICKHOUSE_CLOUD:-false}"
  validate_tf_bool DEV_CREATE_CLICKHOUSE_CLOUD "${create_clickhouse}"

  set_tfvar "${file}" "create_clickhouse_cloud" "${create_clickhouse}"
  set_tfvar "${file}" "additional_control_security_group_ids" "$(tf_json_string_array_or_empty DEV_ADDITIONAL_CONTROL_SECURITY_GROUP_IDS)"

  if [ "${create_clickhouse}" = "true" ]; then
    [ -n "${DEV_CLICKHOUSE_ORGANIZATION_ID:-}" ] || die "DEV_CLICKHOUSE_ORGANIZATION_ID is required when DEV_CREATE_CLICKHOUSE_CLOUD=true"
    [ -n "${CLICKHOUSE_CLOUD_API_KEY:-}" ] || die "CLICKHOUSE_CLOUD_API_KEY is required when DEV_CREATE_CLICKHOUSE_CLOUD=true"
    [ -n "${CLICKHOUSE_CLOUD_API_SECRET:-}" ] || die "CLICKHOUSE_CLOUD_API_SECRET is required when DEV_CREATE_CLICKHOUSE_CLOUD=true"

    set_tfvar "${file}" "clickhouse_organization_id" "$(tf_quote "${DEV_CLICKHOUSE_ORGANIZATION_ID}")"
    unset_tfvar "${file}" "clickhouse_url"
    unset_tfvar "${file}" "clickhouse_user"
    unset_tfvar "${file}" "clickhouse_password_secret_arn"
    unset_tfvar "${file}" "clickhouse_password_kms_key_arns"

    if [ -n "${DEV_CLICKHOUSE_CLOUD_SERVICE_NAME:-}" ]; then
      set_tfvar "${file}" "clickhouse_cloud_service_name" "$(tf_quote "${DEV_CLICKHOUSE_CLOUD_SERVICE_NAME}")"
    else
      unset_tfvar "${file}" "clickhouse_cloud_service_name"
    fi
    if [ -n "${DEV_CLICKHOUSE_CLOUD_REGION:-}" ]; then
      set_tfvar "${file}" "clickhouse_cloud_region" "$(tf_quote "${DEV_CLICKHOUSE_CLOUD_REGION}")"
    else
      unset_tfvar "${file}" "clickhouse_cloud_region"
    fi
    if [ -n "${DEV_CLICKHOUSE_SECRET_KMS_KEY_ID:-}" ]; then
      set_tfvar "${file}" "clickhouse_secret_kms_key_id" "$(tf_quote "${DEV_CLICKHOUSE_SECRET_KMS_KEY_ID}")"
    else
      unset_tfvar "${file}" "clickhouse_secret_kms_key_id"
    fi
    if [ -n "${DEV_CLICKHOUSE_MIN_REPLICA_MEMORY_GB:-}" ]; then
      set_tfvar "${file}" "clickhouse_min_replica_memory_gb" "${DEV_CLICKHOUSE_MIN_REPLICA_MEMORY_GB}"
    fi
    if [ -n "${DEV_CLICKHOUSE_MAX_REPLICA_MEMORY_GB:-}" ]; then
      set_tfvar "${file}" "clickhouse_max_replica_memory_gb" "${DEV_CLICKHOUSE_MAX_REPLICA_MEMORY_GB}"
    fi
    if [ -n "${DEV_CLICKHOUSE_IDLE_SCALING:-}" ]; then
      validate_tf_bool DEV_CLICKHOUSE_IDLE_SCALING "${DEV_CLICKHOUSE_IDLE_SCALING}"
      set_tfvar "${file}" "clickhouse_idle_scaling" "${DEV_CLICKHOUSE_IDLE_SCALING}"
    fi
    if [ -n "${DEV_CLICKHOUSE_IDLE_TIMEOUT_MINUTES:-}" ]; then
      set_tfvar "${file}" "clickhouse_idle_timeout_minutes" "${DEV_CLICKHOUSE_IDLE_TIMEOUT_MINUTES}"
    fi
    if [ -n "${DEV_CLICKHOUSE_BACKUP_RETENTION_HOURS:-}" ]; then
      set_tfvar "${file}" "clickhouse_backup_retention_period_in_hours" "${DEV_CLICKHOUSE_BACKUP_RETENTION_HOURS}"
    fi
    return
  fi

  [ -n "${DEV_CLICKHOUSE_URL:-}" ] || die "DEV_CLICKHOUSE_URL is required when DEV_CREATE_CLICKHOUSE_CLOUD=false"
  [ -n "${DEV_CLICKHOUSE_PASSWORD_SECRET_ARN:-}" ] || die "DEV_CLICKHOUSE_PASSWORD_SECRET_ARN is required when DEV_CREATE_CLICKHOUSE_CLOUD=false"

  unset_tfvar "${file}" "clickhouse_organization_id"
  unset_tfvar "${file}" "clickhouse_cloud_service_name"
  unset_tfvar "${file}" "clickhouse_cloud_region"
  unset_tfvar "${file}" "clickhouse_secret_kms_key_id"
  set_tfvar "${file}" "clickhouse_url" "$(tf_quote "${DEV_CLICKHOUSE_URL}")"
  set_tfvar "${file}" "clickhouse_user" "$(tf_quote "${DEV_CLICKHOUSE_USER:-default}")"
  set_tfvar "${file}" "clickhouse_password_secret_arn" "$(tf_quote "${DEV_CLICKHOUSE_PASSWORD_SECRET_ARN}")"
  set_tfvar "${file}" "clickhouse_password_kms_key_arns" "$(tf_json_string_array_or_empty DEV_CLICKHOUSE_PASSWORD_KMS_KEY_ARNS)"
}

validate_tf_bool() {
  name=$1
  value=$2
  case "${value}" in
    true|false) ;;
    *) die "${name} must be true or false" ;;
  esac
}

url_host() {
  value=$1
  case "${value}" in
    *://*) value="${value#*://}" ;;
  esac
  value="${value%%/*}"
  case "${value}" in
    \[*\]*)
      value="${value#\[}"
      printf '%s\n' "${value%%\]*}"
      ;;
    *)
      printf '%s\n' "${value%%:*}"
      ;;
  esac
}

is_loopback_host() {
  host="$(printf '%s\n' "$1" | tr '[:upper:]' '[:lower:]')"
  host="${host%.}"
  case "${host}" in
    ""|localhost|*.localhost|127.*|0.0.0.0|::1|0:0:0:0:0:0:0:1) return 0 ;;
    *) return 1 ;;
  esac
}

require_non_loopback_control_host() {
  name=$1
  value=$2
  host="$(url_host "${value}")"
  if is_loopback_host "${host}"; then
    die "dev-worker-tfvars requires ${name} to use a non-loopback hostname before enabling workers; current ${name}=${value}. Otherwise workers can receive HELMR_CONTROL_URL=http://localhost or another local address."
  fi
}

require_cloudfront_origin_domain_name() {
  value=$1
  case "${value}" in
    *://*|*/*|*:*|*\?*|*\#*)
      die "dev-worker-tfvars requires cloudfront_origin_domain_name to be a DNS hostname without scheme, path, or port; current cloudfront_origin_domain_name=${value}."
      ;;
  esac
  require_non_loopback_control_host "cloudfront_origin_domain_name" "${value}"
}

ensure_public_worker_control_url_ready() {
  if [ -n "${DEV_CERTIFICATE_ARN:-}" ]; then
    set_tfvar "${DEV_TFVARS}" "certificate_arn" "$(tf_quote "${DEV_CERTIFICATE_ARN}")"
  fi
  if [ -n "${DEV_PUBLIC_URL:-}" ]; then
    set_tfvar "${DEV_TFVARS}" "public_url" "$(tf_quote "${DEV_PUBLIC_URL}")"
  fi
  if env_is_set DEV_ENABLE_CLOUDFRONT; then
    validate_tf_bool DEV_ENABLE_CLOUDFRONT "${DEV_ENABLE_CLOUDFRONT}"
    set_tfvar "${DEV_TFVARS}" "enable_cloudfront" "${DEV_ENABLE_CLOUDFRONT}"
  fi
  if [ -n "${DEV_CLOUDFRONT_ORIGIN_DOMAIN_NAME:-}" ]; then
    set_tfvar "${DEV_TFVARS}" "cloudfront_origin_domain_name" "$(tf_quote "${DEV_CLOUDFRONT_ORIGIN_DOMAIN_NAME}")"
  fi

  certificate_arn="$(tfvar_string_value "${DEV_TFVARS}" "certificate_arn" || true)"
  [ -n "${certificate_arn}" ] || die "dev-worker-tfvars requires DEV_CERTIFICATE_ARN or an existing certificate_arn tfvar before enabling workers; worker enrollment must use the public HTTPS control URL."

  enable_cloudfront="$(tfvar_value "${DEV_TFVARS}" "enable_cloudfront" 2>/dev/null || printf 'false')"
  validate_tf_bool enable_cloudfront "${enable_cloudfront}"
  if [ "${enable_cloudfront}" = "true" ]; then
    cloudfront_origin="$(tfvar_string_value "${DEV_TFVARS}" "cloudfront_origin_domain_name" || true)"
    [ -n "${cloudfront_origin}" ] || die "dev-worker-tfvars requires DEV_CLOUDFRONT_ORIGIN_DOMAIN_NAME or an existing cloudfront_origin_domain_name tfvar when enable_cloudfront=true."
    require_cloudfront_origin_domain_name "${cloudfront_origin}"
  else
    public_url="$(tfvar_string_value "${DEV_TFVARS}" "public_url" || true)"
    [ -n "${public_url}" ] || die "dev-worker-tfvars requires DEV_PUBLIC_URL or an existing public_url tfvar when enable_cloudfront=false; workers use this public control URL."
    require_non_loopback_control_host "public_url" "${public_url}"
  fi
}

apply_control_network_overrides() {
  if env_is_set DEV_ENABLE_NAT_GATEWAY; then
    validate_tf_bool DEV_ENABLE_NAT_GATEWAY "${DEV_ENABLE_NAT_GATEWAY}"
    set_tfvar "${DEV_TFVARS}" "enable_nat_gateway" "${DEV_ENABLE_NAT_GATEWAY}"
  fi
  if env_is_set DEV_CONTROL_ASSIGN_PUBLIC_IP; then
    validate_tf_bool DEV_CONTROL_ASSIGN_PUBLIC_IP "${DEV_CONTROL_ASSIGN_PUBLIC_IP}"
    set_tfvar "${DEV_TFVARS}" "control_assign_public_ip" "${DEV_CONTROL_ASSIGN_PUBLIC_IP}"
  fi

  nat_enabled="$(tfvar_value "${DEV_TFVARS}" "enable_nat_gateway" 2>/dev/null || printf 'false')"
  assign_public_ip="$(tfvar_value "${DEV_TFVARS}" "control_assign_public_ip" 2>/dev/null || printf 'true')"
  if [ "${assign_public_ip}" = "false" ] && [ "${nat_enabled}" != "true" ]; then
    die "enable_nat_gateway=true is required when control_assign_public_ip=false"
  fi
  create_worker="$(tfvar_value "${DEV_TFVARS}" "create_worker" 2>/dev/null || printf 'false')"
  if [ "${create_worker}" = "true" ] && [ "${nat_enabled}" != "true" ]; then
    die "enable_nat_gateway=false is not supported while application-owned worker resources exist; keep run mode or destroy the ephemeral stack"
  fi
}

dev_control_tfvars() {
  control_image="${DEV_CONTROL_IMAGE:-}"
  if [ -z "${control_image}" ] && [ -f "${CONTROL_IMAGE_URI_FILE}" ]; then
    control_image="$(cat "${CONTROL_IMAGE_URI_FILE}")"
  fi
  [ -n "${control_image}" ] || die "DEV_CONTROL_IMAGE is required, or run control-image-build first"
  [ -n "${DEV_GITHUB_OAUTH_CLIENT_ID:-}" ] || die "DEV_GITHUB_OAUTH_CLIENT_ID is required"

  if [ "${DEV_CONTROL_KEEP_WORKER:-0}" != "1" ] &&
    [ -f "${DEV_TFVARS}" ] &&
    [ "$(tfvar_value "${DEV_TFVARS}" "create_worker" 2>/dev/null || true)" = "true" ]; then
    die "dev-control-tfvars cannot remove active worker fleets; use DEV_CONTROL_KEEP_WORKER=1 or destroy the ephemeral stack"
  fi

  mkdir -p "$(dirname "${DEV_TFVARS}")"
  if [ ! -f "${DEV_TFVARS}" ]; then
    cat >"${DEV_TFVARS}" <<EOF
aws_region = "${AWS_REGION}"
name       = "${DEV_NAME:-helmr-smoke}"
public_url = "${DEV_PUBLIC_URL:-http://localhost}"
EOF
  fi

  set_tfvar "${DEV_TFVARS}" "aws_region" "$(tf_quote "${AWS_REGION}")"
  set_tfvar "${DEV_TFVARS}" "name" "$(tf_quote "${DEV_NAME:-helmr-smoke}")"
  set_tfvar "${DEV_TFVARS}" "worker_group_id" "$(tf_quote "$(dev_worker_group_id)")"
  set_tfvar "${DEV_TFVARS}" "region_id" "$(tf_quote "$(dev_region_id)")"
  set_tfvar "${DEV_TFVARS}" "default_region_id" "$(tf_quote "$(dev_default_region_id)")"
  set_tfvar "${DEV_TFVARS}" "public_url" "$(tf_quote "${DEV_PUBLIC_URL:-http://localhost}")"
  set_tfvar "${DEV_TFVARS}" "enable_nat_gateway" "${DEV_ENABLE_NAT_GATEWAY:-false}"
  set_tfvar "${DEV_TFVARS}" "control_image" "$(tf_quote "${control_image}")"
  set_tfvar "${DEV_TFVARS}" "control_image_repository_arn" \
    "$(tf_quote "$(bootstrap_contract_value CONTROL_RELEASE_REPOSITORY_ARN control_release_repository_arn)")"
  if env_is_set DEV_CERTIFICATE_ARN; then
    set_tfvar "${DEV_TFVARS}" "certificate_arn" "$(tf_quote "${DEV_CERTIFICATE_ARN}")"
  else
    set_tfvar "${DEV_TFVARS}" "certificate_arn" "null"
  fi
  allow_insecure_http="${DEV_ALLOW_INSECURE_HTTP:-}"
  if [ -z "${allow_insecure_http}" ]; then
    case "${DEV_PUBLIC_URL:-http://localhost}" in
      http://*) allow_insecure_http=true ;;
      *) allow_insecure_http=false ;;
    esac
  fi
  set_tfvar "${DEV_TFVARS}" "allow_insecure_http" "${allow_insecure_http}"
  set_tfvar "${DEV_TFVARS}" "enable_cloudfront" "${DEV_ENABLE_CLOUDFRONT:-false}"
  if [ "${DEV_ENABLE_CLOUDFRONT:-false}" = "true" ]; then
    [ -n "${DEV_CLOUDFRONT_ORIGIN_DOMAIN_NAME:-}" ] || die "DEV_CLOUDFRONT_ORIGIN_DOMAIN_NAME is required when DEV_ENABLE_CLOUDFRONT=true"
    set_tfvar "${DEV_TFVARS}" "cloudfront_origin_domain_name" "$(tf_quote "${DEV_CLOUDFRONT_ORIGIN_DOMAIN_NAME}")"
  else
    set_tfvar "${DEV_TFVARS}" "cloudfront_origin_domain_name" "null"
  fi
  set_tfvar "${DEV_TFVARS}" "github_oauth_client_id" "$(tf_quote "${DEV_GITHUB_OAUTH_CLIENT_ID}")"
  apply_bootstrap_contract_tfvars "${DEV_TFVARS}"
  apply_dev_clickhouse_tfvars "${DEV_TFVARS}"
  set_tfvar "${DEV_TFVARS}" "create_control_service" "true"
  set_tfvar "${DEV_TFVARS}" "control_desired_count" "${DEV_CONTROL_DESIRED_COUNT:-1}"
  set_tfvar "${DEV_TFVARS}" "dispatcher_desired_count" "${DEV_DISPATCHER_DESIRED_COUNT:-1}"
  set_tfvar "${DEV_TFVARS}" "control_assign_public_ip" "${DEV_CONTROL_ASSIGN_PUBLIC_IP:-true}"
  set_tfvar "${DEV_TFVARS}" "database_backup_retention_days" "${DEV_DATABASE_BACKUP_RETENTION_DAYS:-1}"
  set_tfvar "${DEV_TFVARS}" "redis_node_type" "$(tf_quote "${DEV_REDIS_NODE_TYPE:-cache.t4g.micro}")"
  set_tfvar "${DEV_TFVARS}" "redis_node_count" "${DEV_REDIS_NODE_COUNT:-1}"
  set_tfvar "${DEV_TFVARS}" "control_log_retention_days" "${DEV_CONTROL_LOG_RETENTION_DAYS:-7}"
  set_tfvar "${DEV_TFVARS}" "kms_deletion_window_in_days" "${DEV_KMS_DELETION_WINDOW_IN_DAYS:-7}"
  set_tfvar "${DEV_TFVARS}" "secret_recovery_window_in_days" "${DEV_SECRET_RECOVERY_WINDOW_IN_DAYS:-0}"
  set_tfvar "${DEV_TFVARS}" "cas_object_expiration_days" "${DEV_CAS_OBJECT_EXPIRATION_DAYS:-7}"
  set_tfvar "${DEV_TFVARS}" "cas_noncurrent_version_expiration_days" "${DEV_CAS_NONCURRENT_VERSION_EXPIRATION_DAYS:-1}"
  if [ "${DEV_CONTROL_KEEP_WORKER:-0}" != "1" ]; then
    set_tfvar "${DEV_TFVARS}" "create_worker" "false"
    set_tfvar "${DEV_TFVARS}" "enable_nat_gateway" "false"
    set_tfvar "${DEV_TFVARS}" "control_assign_public_ip" "true"
    set_tfvar "${DEV_TFVARS}" "worker_fleet_controller" '{}'
  else
    set_tfvar "${DEV_TFVARS}" "enable_nat_gateway" "true"
    set_tfvar "${DEV_TFVARS}" "control_assign_public_ip" "false"
  fi
  apply_control_network_overrides
  info "updated ${DEV_TFVARS} for control service"
}

dev_secrets() {
  "${TF_BIN}" -chdir="${DEV_STACK}" output -json secret_arns | jq .
}

dev_secret_arn() {
  key=$1
  "${TF_BIN}" -chdir="${DEV_STACK}" output -json secret_arns | jq -r --arg key "${key}" '.[$key]'
}

dev_secret_arn_optional() {
  key=$1
  "${TF_BIN}" -chdir="${DEV_STACK}" output -json secret_arns | jq -r --arg key "${key}" '.[$key] // empty'
}

put_secret_value() {
  secret_id=$1
  secret_value=$2
  input_file="$(sensitive_mktemp secret-put.json)"
  secret_file="$(sensitive_mktemp secret-value.txt)"
  trap 'rm -f "${input_file}" "${secret_file}"' RETURN
  printf '%s' "${secret_value}" >"${secret_file}"
  jq -n \
    --arg secret_id "${secret_id}" \
    --rawfile secret_value "${secret_file}" \
    '{SecretId:$secret_id, SecretString:$secret_value}' >"${input_file}"
  aws secretsmanager put-secret-value \
    --region "${AWS_REGION}" \
    --cli-input-json "file://${input_file}" >/dev/null
  rm -f "${input_file}" "${secret_file}"
  trap - RETURN
}

secret_value_status() {
  secret_id=$1
  error_file="$(sensitive_mktemp secret-get.err)"
  if aws secretsmanager get-secret-value \
    --region "${AWS_REGION}" \
    --secret-id "${secret_id}" >/dev/null 2>"${error_file}"; then
    rm -f "${error_file}"
    printf 'present\n'
    return 0
  fi
  if grep -q 'ResourceNotFoundException' "${error_file}"; then
    rm -f "${error_file}"
    printf 'missing\n'
    return 0
  fi
  cat "${error_file}" >&2
  rm -f "${error_file}"
  return 1
}

put_secret_value_if_missing() {
  secret_id=$1
  secret_value=$2
  if [ "${ROTATE_DEV_SECRETS:-0}" != "1" ]; then
    status="$(secret_value_status "${secret_id}")"
    case "${status}" in
      present)
        info "secret already populated: ${secret_id}"
        return 0
        ;;
      missing) ;;
      *) die "unexpected secret status for ${secret_id}: ${status}" ;;
    esac
  fi
  put_secret_value "${secret_id}" "${secret_value}"
}

random_base64() {
  dd if=/dev/urandom bs=32 count=1 2>/dev/null | base64 | tr -d '\n'
}

random_base64url() {
  dd if=/dev/urandom bs=32 count=1 2>/dev/null | base64 | tr '+/' '-_' | tr -d '=\n'
}

dev_root_key() {
  secret_id="$(dev_secret_arn "$1")"
  put_secret_value_if_missing "${secret_id}" "$(random_base64)"
}

dev_database_url() {
  database_secret_arn="$(dev_secret_arn database_url)"
  master_secret_arn="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw database_master_user_secret_arn)"
  endpoint="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw postgres_endpoint)"
  master_secret="$(
    aws secretsmanager get-secret-value \
      --region "${AWS_REGION}" \
      --secret-id "${master_secret_arn}" \
      --query SecretString \
      --output text
  )"
  username="$(printf '%s\n' "${master_secret}" | jq -r '.username')"
  password="$(printf '%s\n' "${master_secret}" | jq -r '.password')"
  encoded_password="$(printf '%s' "${password}" | jq -sRr @uri)"
  put_secret_value_if_missing "${database_secret_arn}" "postgres://${username}:${encoded_password}@${endpoint}/helmr?sslmode=require"
  info "database_url secret populated: ${database_secret_arn}"
}

dev_resend_api_key_secret() {
  secret_arn="$(dev_secret_arn_optional resend_api_key)"
  [ -n "${secret_arn}" ] && [ "${secret_arn}" != "null" ] || return 0

  api_key="${RESEND_API_KEY:-}"
  if [ -z "${api_key}" ] && [ -f "${STATE_DIR}/resend-api-key" ]; then
    api_key="$(cat "${STATE_DIR}/resend-api-key")"
  fi
  if [ -z "${api_key}" ]; then
    status="$(secret_value_status "${secret_arn}")"
    if [ "${status}" = "missing" ]; then
      info "resend_api_key secret is unpopulated; set RESEND_API_KEY or ${STATE_DIR}/resend-api-key before starting control service"
    fi
    return 0
  fi

  put_secret_value_if_missing "${secret_arn}" "${api_key}"
  info "resend_api_key secret populated: ${secret_arn}"
}

dev_generated_secrets() {
  dev_database_url
  put_secret_value_if_missing "$(dev_secret_arn worker_token_signing_key)" "$(random_base64)"
  put_secret_value_if_missing "$(dev_secret_arn auth_key)" "$(random_base64)"
  put_secret_value_if_missing "$(dev_secret_arn encryption_key)" "$(random_base64)"
  dev_root_key workspace_fencing_key
  dev_root_key token_credential_key
  put_secret_value_if_missing "$(dev_secret_arn checkpoint_encryption_key)" "$(random_base64)"
  put_secret_value_if_missing "$(dev_secret_arn setup_token)" "$(random_hex)"
  while IFS=$'\t' read -r group_id secret_arn; do
    [ -n "${group_id}" ] || continue
    put_secret_value_if_missing "${secret_arn}" "$(random_base64url)"
  done < <("${TF_BIN}" -chdir="${DEV_STACK}" output -json worker_enrollment_secret_arns | jq -r 'to_entries[] | [.key, .value] | @tsv')
  dev_resend_api_key_secret
  info "generated secrets populated"
}

random_hex() {
  dd if=/dev/urandom bs=32 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'
}

dev_github_oauth_secret() {
  client_secret="${HELMR_GITHUB_OAUTH_CLIENT_SECRET:-}"
  [ -n "${client_secret}" ] || die "HELMR_GITHUB_OAUTH_CLIENT_SECRET is required"
  put_secret_value_if_missing "$(dev_secret_arn github_oauth_client_secret)" "${client_secret}"
  info "GitHub OAuth client secret populated"
}

dev_worker_tfvars() {
  ami_id="${WORKER_AMI_ID:-}"
  worker_instance_type="${WORKER_INSTANCE_TYPE:-c8i.xlarge}"
  worker_root_volume_size_gb="${DEV_WORKER_ROOT_VOLUME_SIZE_GB:-120}"
  worker_root_volume_iops="${DEV_WORKER_ROOT_VOLUME_IOPS:-3000}"
  worker_root_volume_throughput="${DEV_WORKER_ROOT_VOLUME_THROUGHPUT:-125}"
  worker_max_size="${DEV_WORKER_MAX_SIZE:-1}"
  build_worker_max_size="${DEV_BUILD_WORKER_MAX_SIZE:-1}"
  build_worker_vm_vcpus="${DEV_BUILD_WORKER_VM_VCPUS:-3}"
  build_worker_vm_memory_mib="${DEV_BUILD_WORKER_VM_MEMORY_MIB:-4096}"
  build_worker_vm_scratch_disk_mib="${DEV_BUILD_WORKER_VM_SCRATCH_DISK_MIB:-32768}"
  worker_execution_slots="${DEV_WORKER_EXECUTION_SLOTS:-1}"
  run_warm_workers="${DEV_RUN_WARM_WORKERS:-0}"
  run_max_workers="${DEV_RUN_MAX_WORKERS:-${worker_max_size}}"
  build_max_workers="${DEV_BUILD_MAX_WORKERS:-${build_worker_max_size}}"
  max_scale_out_per_cycle="${DEV_MAX_SCALE_OUT_PER_CYCLE:-1}"
  max_pending_workers="${DEV_MAX_PENDING_WORKERS:-1}"
  allow_extended_worker_capacity="${DEV_ALLOW_EXTENDED_WORKER_CAPACITY:-false}"
  for value_name in worker_max_size build_worker_max_size build_worker_vm_vcpus build_worker_vm_memory_mib build_worker_vm_scratch_disk_mib worker_execution_slots run_warm_workers run_max_workers build_max_workers max_scale_out_per_cycle max_pending_workers; do
    value="${!value_name}"
    case "${value}" in
      ''|*[!0-9]*) die "${value_name} must be a non-negative integer" ;;
    esac
  done
  case "${allow_extended_worker_capacity}" in
    true|false) ;;
    *) die "DEV_ALLOW_EXTENDED_WORKER_CAPACITY must be true or false" ;;
  esac
  if { [ "${worker_max_size}" -gt 1 ] || [ "${build_worker_max_size}" -gt 1 ]; } &&
    [ "${allow_extended_worker_capacity}" != "true" ]; then
    die "DEV_ALLOW_EXTENDED_WORKER_CAPACITY=true is required when a Worker ASG ceiling exceeds 1"
  fi
  [ "${run_max_workers}" -le "${worker_max_size}" ] ||
    die "DEV_RUN_MAX_WORKERS cannot exceed DEV_WORKER_MAX_SIZE"
  [ "${build_max_workers}" -le "${build_worker_max_size}" ] ||
    die "DEV_BUILD_MAX_WORKERS cannot exceed DEV_BUILD_WORKER_MAX_SIZE"
  if [ -z "${ami_id}" ] && [ -f "${AMI_ID_FILE}" ]; then
    ami_id="$(cat "${AMI_ID_FILE}")"
  fi
  [ -n "${ami_id}" ] || die "WORKER_AMI_ID is required, or run worker-image-wait first"
  [ -f "${DEV_TFVARS}" ] || die "${DEV_TFVARS} does not exist; run dev-control-tfvars first"
  ensure_public_worker_control_url_ready
  set_tfvar "${DEV_TFVARS}" "create_worker" "true"
  set_tfvar "${DEV_TFVARS}" "enable_nat_gateway" "true"
  set_tfvar "${DEV_TFVARS}" "control_assign_public_ip" "false"
  set_tfvar "${DEV_TFVARS}" "worker_ami_id" "$(tf_quote "${ami_id}")"
  set_tfvar "${DEV_TFVARS}" "worker_instance_type" "$(tf_quote "${worker_instance_type}")"
  set_tfvar "${DEV_TFVARS}" "worker_enable_nested_virtualization" "true"
  set_tfvar "${DEV_TFVARS}" "allow_extended_worker_capacity" "${allow_extended_worker_capacity}"
  set_tfvar "${DEV_TFVARS}" "worker_min_size" "0"
  set_tfvar "${DEV_TFVARS}" "worker_max_size" "${worker_max_size}"
  set_tfvar "${DEV_TFVARS}" "build_worker_min_size" "0"
  set_tfvar "${DEV_TFVARS}" "build_worker_max_size" "${build_worker_max_size}"
  set_tfvar "${DEV_TFVARS}" "worker_root_volume_size_gb" "${worker_root_volume_size_gb}"
  set_tfvar "${DEV_TFVARS}" "worker_root_volume_iops" "${worker_root_volume_iops}"
  set_tfvar "${DEV_TFVARS}" "worker_root_volume_throughput" "${worker_root_volume_throughput}"
  set_tfvar "${DEV_TFVARS}" "worker_disk_mib" "${DEV_WORKER_DISK_MIB:-98304}"
  set_tfvar "${DEV_TFVARS}" "worker_vm_vcpus" "${DEV_WORKER_VM_VCPUS:-2}"
  set_tfvar "${DEV_TFVARS}" "worker_vm_memory_mib" "${DEV_WORKER_VM_MEMORY_MIB:-4096}"
  set_tfvar "${DEV_TFVARS}" "worker_vm_scratch_disk_mib" "${DEV_WORKER_VM_SCRATCH_DISK_MIB:-32768}"
  set_tfvar "${DEV_TFVARS}" "worker_substrate_cache_max_mib" "${DEV_WORKER_SUBSTRATE_CACHE_MAX_MIB:-4096}"
  set_tfvar "${DEV_TFVARS}" "worker_artifact_cache_max_mib" "${DEV_WORKER_ARTIFACT_CACHE_MAX_MIB:-2048}"
  set_tfvar "${DEV_TFVARS}" "worker_capacity_vcpus" "${DEV_WORKER_CAPACITY_VCPUS:-4}"
  set_tfvar "${DEV_TFVARS}" "worker_capacity_memory_mib" "${DEV_WORKER_CAPACITY_MEMORY_MIB:-8192}"
  set_tfvar "${DEV_TFVARS}" "worker_execution_slots" "${worker_execution_slots}"
  set_tfvar "${DEV_TFVARS}" "build_worker_instance_type" "null"
  set_tfvar "${DEV_TFVARS}" "build_worker_enable_nested_virtualization" "null"
  set_tfvar "${DEV_TFVARS}" "build_worker_root_volume_size_gb" "null"
  set_tfvar "${DEV_TFVARS}" "build_worker_root_volume_iops" "null"
  set_tfvar "${DEV_TFVARS}" "build_worker_root_volume_throughput" "null"
  set_tfvar "${DEV_TFVARS}" "build_worker_disk_mib" "null"
  set_tfvar "${DEV_TFVARS}" "build_worker_disk_reserve_mib" "null"
  set_tfvar "${DEV_TFVARS}" "build_worker_vm_vcpus" "${build_worker_vm_vcpus}"
  set_tfvar "${DEV_TFVARS}" "build_worker_vm_memory_mib" "${build_worker_vm_memory_mib}"
  set_tfvar "${DEV_TFVARS}" "build_worker_vm_scratch_disk_mib" "${build_worker_vm_scratch_disk_mib}"
  set_tfvar "${DEV_TFVARS}" "build_worker_capacity_vcpus" "null"
  set_tfvar "${DEV_TFVARS}" "build_worker_capacity_memory_mib" "null"
  set_tfvar "${DEV_TFVARS}" "build_worker_execution_slots" "null"
  set_tfvar "${DEV_TFVARS}" "build_worker_substrate_cache_max_mib" "null"
  set_tfvar "${DEV_TFVARS}" "build_worker_artifact_cache_max_mib" "null"
  set_tfvar "${DEV_TFVARS}" "worker_fleet_controller" "$(jq -cn \
    --argjson run_warm_workers "${run_warm_workers}" \
    --argjson run_max_workers "${run_max_workers}" \
    --argjson build_max_workers "${build_max_workers}" \
    --argjson max_scale_out_per_cycle "${max_scale_out_per_cycle}" \
    --argjson max_pending_workers "${max_pending_workers}" \
    '{
      run_warm_workers:$run_warm_workers,
      build_warm_workers:0,
      run_max_workers:$run_max_workers,
      build_max_workers:$build_max_workers,
      max_scale_out_per_cycle:$max_scale_out_per_cycle,
      max_pending_workers:$max_pending_workers,
      max_packing_items:10000,
      controller_interval_seconds:15,
      scale_out_cooldown_seconds:30,
      scale_in_cooldown_seconds:300,
      scale_in_hysteresis_seconds:300,
      stale_worker_timeout_seconds:120,
      readiness_timeout_seconds:900,
      drain_timeout_seconds:1800,
      emergency_stop:false,
      metric_interval_seconds:60
    }')"
  info "updated ${DEV_TFVARS} for cost-bounded run/build fleets"
}

dev_migrate() {
  cluster="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw control_cluster_name)"
  task_definition="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw migration_task_definition_arn)"
  security_groups="$("${TF_BIN}" -chdir="${DEV_STACK}" output -json control_task_security_group_ids)"
  subnets="$("${TF_BIN}" -chdir="${DEV_STACK}" output -json control_task_subnet_ids)"
  assign_public_ip="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw control_assign_public_ip)"
  if [ "${assign_public_ip}" = "true" ]; then
    assign_public_ip_value="ENABLED"
  else
    assign_public_ip_value="DISABLED"
  fi
  network_configuration="$(
    jq -cn \
      --argjson subnets "${subnets}" \
      --argjson security_groups "${security_groups}" \
      --arg assign_public_ip "${assign_public_ip_value}" \
      '{awsvpcConfiguration:{subnets:$subnets,securityGroups:$security_groups,assignPublicIp:$assign_public_ip}}'
  )"

  task_arn="$(
    aws ecs run-task \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --task-definition "${task_definition}" \
      --launch-type FARGATE \
      --network-configuration "${network_configuration}" \
      --query 'tasks[0].taskArn' \
      --output text
  )"
  [ -n "${task_arn}" ] && [ "${task_arn}" != "None" ] || die "migration task did not start"
  info "waiting for migration task: ${task_arn}"
  aws ecs wait tasks-stopped --region "${AWS_REGION}" --cluster "${cluster}" --tasks "${task_arn}"
  exit_code="$(
    aws ecs describe-tasks \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --tasks "${task_arn}" \
      --query 'tasks[0].containers[0].exitCode' \
      --output text
  )"
  [ "${exit_code}" = "0" ] || die "migration task exited with ${exit_code}"
}

worker_asg_instance_count() {
  local asg_name=$1
  local group_json

  if ! group_json="$(
    aws autoscaling describe-auto-scaling-groups \
      --region "${AWS_REGION}" \
      --auto-scaling-group-names "${asg_name}" \
      --output json
  )"; then
    return 1
  fi
  printf '%s\n' "${group_json}" | jq -er --arg asg_name "${asg_name}" '
    if (.AutoScalingGroups | length) == 0 then
      0
    elif (.AutoScalingGroups | length) == 1 and .AutoScalingGroups[0].AutoScalingGroupName == $asg_name then
      ([.AutoScalingGroups[0].DesiredCapacity, (.AutoScalingGroups[0].Instances | length)] | max)
    else
      error("unexpected Auto Scaling group lookup result")
    end
  '
}

require_fleet_controller_for_active_workers() {
  local cluster=$1
  local service_json

  service_json="$(
    aws ecs describe-services \
      --region "${AWS_REGION}" \
      --cluster "${cluster}" \
      --services dispatcher \
      --output json 2>/dev/null || true
  )"
  printf '%s\n' "${service_json}" | jq -e '
    (.failures | length) == 0 and
    (.services | length) == 1 and
    .services[0].status == "ACTIVE" and
    .services[0].desiredCount > 0 and
    .services[0].runningCount > 0
  ' >/dev/null || die "active workers require a running application fleet controller to drain before destroy"
}

wait_for_worker_asgs_empty() {
  local timeout=${DEV_DESTROY_WORKER_DRAIN_TIMEOUT_SECONDS:-2400}
  local poll=${DEV_DESTROY_WORKER_DRAIN_POLL_SECONDS:-15}
  local deadline=$((SECONDS + timeout))
  local asg_name
  local count
  local -a remaining=()

  while :; do
    remaining=()
    for asg_name in "$@"; do
      if ! count="$(worker_asg_instance_count "${asg_name}")"; then
        die "failed to inspect worker Auto Scaling group ${asg_name} before destroy"
      fi
      if [ "${count}" -gt 0 ]; then
        remaining+=("${asg_name}=${count}")
      fi
    done
    if [ "${#remaining[@]}" -eq 0 ]; then
      info "worker fleets reached zero through the application drain path"
      return 0
    fi
    if [ "${SECONDS}" -ge "${deadline}" ]; then
      die "worker fleets did not drain to zero before destroy: ${remaining[*]}"
    fi
    info "waiting for the application fleet controller to drain workers: ${remaining[*]}"
    sleep "${poll}"
  done
}

dev_destroy_prepare() {
  need_command aws
  need_command jq
  mkdir -p "${STATE_DIR}"
  name="$(printf '%s' "${DEV_NAME:-helmr-smoke}" | tr '[:upper:]' '[:lower:]')"
  db_identifier="${name}-postgres"
  account_id="$(aws sts get-caller-identity --region "${AWS_REGION}" --query Account --output text)"
  cas_bucket="${name}-${account_id}-${AWS_REGION}-cas"

  worker_asgs=()
  for output in worker_autoscaling_group_name build_worker_autoscaling_group_name; do
    asg_name="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw "${output}" 2>/dev/null || true)"
    [ -n "${asg_name}" ] && [ "${asg_name}" != "null" ] || continue
    worker_asgs+=("${asg_name}")
  done
  for asg_name in "${name}-run-worker" "${name}-build-worker"; do
    if ! printf '%s\n' "${worker_asgs[@]}" | grep -Fxq "${asg_name}"; then
      worker_asgs+=("${asg_name}")
    fi
  done

  active_workers=0
  for asg_name in "${worker_asgs[@]}"; do
    if ! asg_count="$(worker_asg_instance_count "${asg_name}")"; then
      die "failed to inspect worker Auto Scaling group ${asg_name} before destroy"
    fi
    if [ "${asg_count}" -gt 0 ]; then
      active_workers=1
    fi
  done
  if [ "${active_workers}" = "1" ]; then
    require_fleet_controller_for_active_workers "${name}-control"
  fi
  wait_for_worker_asgs_empty "${worker_asgs[@]}"

  dispatcher_stopped=0
  dispatcher_error="$(sensitive_mktemp dispatcher-stop.err)"
  trap 'rm -f "${dispatcher_error}"' RETURN
  if aws ecs update-service \
    --region "${AWS_REGION}" \
    --cluster "${name}-control" \
    --service dispatcher \
    --desired-count 0 >/dev/null 2>"${dispatcher_error}"; then
    aws ecs wait services-stable \
      --region "${AWS_REGION}" \
      --cluster "${name}-control" \
      --services dispatcher
    dispatcher_stopped=1
    info "stopped the application fleet controller after worker drain proof"
  elif grep -Eq 'ClusterNotFoundException|ServiceNotFoundException' "${dispatcher_error}"; then
    info "application fleet controller is already absent"
  else
    cat "${dispatcher_error}" >&2
    die "failed to stop the application fleet controller after worker drain proof"
  fi

  post_stop_active=0
  post_stop_unproven=0
  for asg_name in "${worker_asgs[@]}"; do
    if ! asg_count="$(worker_asg_instance_count "${asg_name}")"; then
      post_stop_unproven=1
      continue
    fi
    if [ "${asg_count}" -gt 0 ]; then
      post_stop_active=1
    fi
  done
  if [ "${post_stop_active}" = "1" ] || [ "${post_stop_unproven}" = "1" ]; then
    if [ "${dispatcher_stopped}" = "1" ]; then
      aws ecs update-service \
        --region "${AWS_REGION}" \
        --cluster "${name}-control" \
        --service dispatcher \
        --desired-count 1 >/dev/null
      aws ecs wait services-stable \
        --region "${AWS_REGION}" \
        --cluster "${name}-control" \
        --services dispatcher
      if [ "${post_stop_unproven}" = "1" ]; then
        die "worker zero could not be proved after stopping the fleet controller; dispatcher was restored so the normal drain path can finish"
      fi
      die "worker capacity reappeared while stopping the fleet controller; dispatcher was restored so the normal drain path can finish"
    fi
    if [ "${post_stop_unproven}" = "1" ]; then
      die "worker zero could not be proved after the fleet controller was absent"
    fi
    die "worker capacity reappeared after the fleet controller was absent"
  fi
  info "worker fleets remained at zero after the application fleet controller stopped"
  rm -f "${dispatcher_error}"
  trap - RETURN

  deletion_protection="$(
    aws rds describe-db-instances \
      --region "${AWS_REGION}" \
      --db-instance-identifier "${db_identifier}" \
      --query 'DBInstances[0].DeletionProtection' \
      --output text 2>/dev/null || true
  )"
  if [ "${deletion_protection}" = "True" ] || [ "${deletion_protection}" = "true" ]; then
    aws rds modify-db-instance \
      --region "${AWS_REGION}" \
      --db-instance-identifier "${db_identifier}" \
      --no-deletion-protection \
      --apply-immediately >/dev/null
    aws rds wait db-instance-available \
      --region "${AWS_REGION}" \
      --db-instance-identifier "${db_identifier}"
    info "disabled deletion protection for ${db_identifier}"
  fi

  if aws s3api head-bucket --bucket "${cas_bucket}" >/dev/null 2>&1; then
    delete_all_s3_object_versions "${cas_bucket}" "${AWS_REGION}"
  fi
}

dev_destroy() {
  [ -f "${DEV_TFVARS}" ] || die "${DEV_TFVARS} does not exist; run dev-base-tfvars/dev-control-tfvars first or set DEV_TFVARS"
  dev_destroy_prepare
  tf_destroy "${DEV_STACK}" -var-file="${DEV_TFVARS}"
}

command=${1:-}
case "${command}" in
  check) check ;;
  bootstrap-init) bootstrap_init ;;
  bootstrap-apply) bootstrap_apply ;;
  bootstrap-output) bootstrap_output ;;
  bootstrap-destroy-prepare) bootstrap_destroy_prepare ;;
  platform-release-publish) platform_release_publish ;;
  source-bundle) source_bundle ;;
  worker-image-source-check) worker_image_source_check ;;
  worker-image-init) tf_init "${WORKER_IMAGE_STACK}" ;;
  worker-image-apply) worker_image_apply ;;
  worker-image-start) worker_image_start ;;
  worker-image-wait) shift; worker_image_wait "$@" ;;
  worker-image-amis) worker_image_amis ;;
  control-image-build) control_image_build ;;
  control-image-push) control_image_push ;;
  dev-tfvars) dev_tfvars ;;
  dev-base-tfvars) dev_base_tfvars ;;
  dev-init) tf_init "${DEV_STACK}" ;;
  dev-apply) dev_apply ;;
  dev-secrets) dev_secrets ;;
  dev-database-url) dev_database_url ;;
  dev-generated-secrets) dev_generated_secrets ;;
  dev-github-oauth-secret) dev_github_oauth_secret ;;
  dev-control-tfvars) dev_control_tfvars ;;
  dev-worker-tfvars) dev_worker_tfvars ;;
  dev-migrate) dev_migrate ;;
  dev-destroy-prepare) dev_destroy_prepare ;;
  dev-destroy) dev_destroy ;;
  -h|--help|help|"") usage ;;
  *) usage >&2; die "unknown command: ${command}" ;;
esac
