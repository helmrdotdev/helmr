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
CONTROL_IMAGE_URI_FILE="${STATE_DIR}/control-image-uri"
GITHUB_WEBHOOK_SECRET_FILE="${STATE_DIR}/github-webhook-secret"
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
  dev-generated-secrets Populate generated non-GitHub secret values.
  dev-github-webhook-secret
                       Generate and store the GitHub App webhook secret.
  dev-github-secrets   Populate GitHub App private key, client secret, and webhook secret.
  dev-control-tfvars   Update dev tfvars to start the control service.
  dev-worker-tfvars    Update dev tfvars to start one nested-virtualization worker.
  dev-worker-down-tfvars
                       Update dev tfvars to keep worker resources but stop worker instances.
  dev-migrate           Run the ECS migration task for the dev stack.
  dev-destroy-prepare   Prepare an ephemeral dev stack for destroy.

Required environment:
  STATE_BUCKET          S3 bucket for Terraform/OpenTofu state; not needed for check/bootstrap-*.
  STATE_KEY             Optional S3 backend state key override.

Common optional environment:
  AWS_PROFILE           AWS CLI profile name; credentials are never written by this script.
  AWS_REGION            AWS region. Defaults to us-east-1.
  STATE_REGION          State bucket region. Defaults to AWS_REGION.
  TF_BIN                Terraform-compatible binary. Defaults to tofu.
  TOFU_APPLY_ARGS       Extra args for apply, for example "-auto-approve".
  SOURCE_BUNDLE_BUCKET  S3 artifact bucket for local source bundles. Defaults to bootstrap output.

Worker image optional environment:
  BOOTSTRAP_NAME           Bootstrap resource name. Defaults to helmr-dev.
  WORKER_IMAGE_NAME        Stack name. Defaults to helmr-dev-image.
  WORKER_IMAGE_SOURCE_REPOSITORY_URL
                           Git repository cloned by Image Builder.
  WORKER_IMAGE_SOURCE_REF  Git ref checked out by Image Builder. Defaults to the current branch.
  WORKER_IMAGE_SOURCE_BUNDLE_S3_URI
                           S3 git bundle URI. Defaults to the last source-bundle result.
  WORKER_IMAGE_VERSION     Optional Image Builder component/recipe version for immutable updates.
  WORKER_IMAGE_DISTRIBUTION_REGIONS
                           Optional comma-separated AWS regions for Image Builder AMI distribution.
  WORKER_IMAGE_AMI_PUBLIC  Set to 1 or true to make distributed worker AMIs public.
  WORKER_IMAGE_ROOT_VOLUME_ENCRYPTED
                           Set to 0 or false for public official AMIs.
  SKIP_SOURCE_REF_CHECK    Set to 1 to skip the remote ref check.

Control image optional environment:
  CONTROL_IMAGE_REPOSITORY  ECR repository URI. Defaults to the dev stack output.
  CONTROL_IMAGE_TAG         Image tag. Defaults to the current short Git revision.
  CONTROL_IMAGE_PLATFORM    Docker platform. Defaults to linux/amd64.
  ROTATE_DEV_SECRETS        Set to 1 to replace generated dev secret values.

Dev optional environment:
  DEV_TFVARS            Generated tfvars path. Defaults to infra/aws/stacks/dev/full-run-smoke.tfvars.
  DEV_NAME              Dev stack name. Defaults to helmr-smoke.
  DEV_PUBLIC_URL        External URL placeholder. Defaults to http://localhost.
  DEV_ENABLE_NAT_GATEWAY
                       Create a NAT Gateway for private egress. Defaults to false for control mode.
  DEV_CONTROL_IMAGE     Initial task definition image. Defaults to public.ecr.aws/docker/library/busybox:latest.
  DEV_CONTROL_ASSIGN_PUBLIC_IP
                       Run control tasks in public subnets. Defaults to 1 for control mode.
  DEV_GITHUB_APP_ID     Initial GitHub App ID placeholder. Defaults to 0.
  DEV_GITHUB_APP_SLUG   Initial GitHub App slug placeholder. Defaults to helmr-dev.
  DEV_GITHUB_APP_CLIENT_ID
                       Initial GitHub App client ID placeholder. Defaults to placeholder.
  DEV_CONTROL_DESIRED_COUNT
                       Control ECS desired task count. Defaults to 1 for dev cost control.
  DEV_CONTROL_KEEP_WORKER
                       Set to 1 to leave existing worker capacity settings untouched.
  WORKER_AMI_ID         AMI ID to inject; defaults to the last worker-image-wait result.
  GITHUB_APP_PRIVATE_KEY_FILE
                        File containing the GitHub App private key PEM for dev-github-secrets.
  GITHUB_APP_CLIENT_SECRET_FILE
                        File containing the GitHub App client secret for dev-github-secrets.
  GITHUB_APP_WEBHOOK_SECRET_FILE
                        Optional file containing the GitHub App webhook secret.
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

need_state_bucket() {
  [ -n "${STATE_BUCKET:-}" ] || die "STATE_BUCKET is required"
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
    # shellcheck disable=SC2206
    extra_args=(${TOFU_APPLY_ARGS})
    "${TF_BIN}" -chdir="${stack}" apply "${extra_args[@]}" "$@"
  else
    "${TF_BIN}" -chdir="${stack}" apply "$@"
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

bootstrap_apply() {
  tf_apply "${BOOTSTRAP_STACK}" -var="name=${BOOTSTRAP_NAME}"
}

bootstrap_output() {
  bucket="$("${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" output -raw bucket_name)"
  artifact_bucket="$("${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" output -raw source_artifact_bucket_name)"
  printf 'export STATE_BUCKET=%q\n' "${bucket}"
  printf 'export STATE_REGION=%q\n' "${STATE_REGION}"
  printf 'export SOURCE_BUNDLE_BUCKET=%q\n' "${artifact_bucket}"
}

delete_all_s3_object_versions() {
  bucket=$1
  mkdir -p "${STATE_DIR}"
  while :; do
    delete_file="$(mktemp "${STATE_DIR}/s3-delete.XXXXXX.json")"
    trap 'rm -f "${delete_file}"' RETURN
    aws s3api list-object-versions \
      --region "${STATE_REGION}" \
      --bucket "${bucket}" \
      --output json |
      jq '{Objects: (((.Versions // []) + (.DeleteMarkers // [])) | map({Key, VersionId}))}' >"${delete_file}"
    if [ "$(jq '.Objects | length' <"${delete_file}")" -eq 0 ]; then
      rm -f "${delete_file}"
      trap - RETURN
      break
    fi
    aws s3api delete-objects \
      --region "${STATE_REGION}" \
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
  delete_all_s3_object_versions "${artifact_bucket}"
  delete_all_s3_object_versions "${state_bucket}"
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

worker_image_source_check() {
  if [ -n "$(source_bundle_uri)" ]; then
    info "using source bundle: $(source_bundle_uri)"
    return 0
  fi
  [ "${SKIP_SOURCE_REF_CHECK:-}" != "1" ] || return 0
  resolve_remote_source_ref >/dev/null
}

worker_image_apply() {
  worker_image_source_check
  bundle_uri="$(source_bundle_uri)"
  version_args=(-var="image_version=$(worker_image_version)")
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
    tf_apply "${WORKER_IMAGE_STACK}" \
      -var="aws_region=${AWS_REGION}" \
      -var="name=${WORKER_IMAGE_NAME}" \
      -var="source_ref=${source_ref}" \
      -var="source_bundle_s3_uri=${bundle_uri}" \
      -var="source_bundle_object_arn=$(source_bundle_object_arn "${bundle_uri}")" \
      "${distribution_args[@]}" \
      "${public_args[@]}" \
      "${encryption_args[@]}" \
      "${kms_args[@]}" \
      "${version_args[@]}"
  else
    source_ref="$(resolve_remote_source_ref)"
    tf_apply "${WORKER_IMAGE_STACK}" \
      -var="aws_region=${AWS_REGION}" \
      -var="name=${WORKER_IMAGE_NAME}" \
      -var="source_repository_url=${WORKER_IMAGE_SOURCE_REPOSITORY_URL}" \
      -var="source_ref=${source_ref}" \
      "${distribution_args[@]}" \
      "${public_args[@]}" \
      "${encryption_args[@]}" \
      "${version_args[@]}"
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
  if [ -n "${CONTROL_IMAGE_REPOSITORY:-}" ]; then
    printf '%s\n' "${CONTROL_IMAGE_REPOSITORY}"
  else
    "${TF_BIN}" -chdir="${DEV_STACK}" output -raw control_ecr_repository_url
  fi
}

control_image_uri() {
  repository="$(control_image_repository)"
  tag="${CONTROL_IMAGE_TAG:-$(git -C "${ROOT}" rev-parse --short=12 HEAD)}"
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

  digest="$(aws ecr describe-images \
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
  image_uri="${CONTROL_IMAGE_URI:-}"
  if [ -z "${image_uri}" ] && [ -f "${CONTROL_IMAGE_URI_FILE}" ]; then
    image_uri="$(cat "${CONTROL_IMAGE_URI_FILE}")"
  fi
  [ -n "${image_uri}" ] || die "CONTROL_IMAGE_URI is required, or run control-image-build first"
  registry="${image_uri%%/*}"
  aws ecr get-login-password --region "${AWS_REGION}" | docker login --username AWS --password-stdin "${registry}"
  docker push "${image_uri}"
  digest_image_uri="$(control_image_digest_uri "${image_uri}")"
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

dev_base_tfvars() {
  mkdir -p "$(dirname "${DEV_TFVARS}")"
  control_image="${DEV_CONTROL_IMAGE:-}"
  if [ -z "${control_image}" ] && [ -f "${CONTROL_IMAGE_URI_FILE}" ]; then
    control_image="$(cat "${CONTROL_IMAGE_URI_FILE}")"
  fi
  if [ -z "${control_image}" ]; then
    control_image="public.ecr.aws/docker/library/busybox:latest"
  fi
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

public_url                    = "${DEV_PUBLIC_URL:-http://localhost}"
enable_nat_gateway            = ${DEV_ENABLE_NAT_GATEWAY:-false}
control_image                 = "${control_image}"
certificate_arn               = ${certificate_arn_value}
allow_insecure_http           = ${DEV_ALLOW_INSECURE_HTTP:-true}
enable_cloudfront             = ${DEV_ENABLE_CLOUDFRONT:-false}
cloudfront_origin_domain_name = ${cloudfront_origin_value}

github_app_id        = "${DEV_GITHUB_APP_ID:-0}"
github_app_slug      = "${DEV_GITHUB_APP_SLUG:-helmr-dev}"
github_app_client_id = "${DEV_GITHUB_APP_CLIENT_ID:-placeholder}"

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
control_ecr_max_images                      = ${DEV_CONTROL_ECR_MAX_IMAGES:-10}
control_ecr_untagged_image_expiration_days  = ${DEV_CONTROL_ECR_UNTAGGED_IMAGE_EXPIRATION_DAYS:-1}

worker_instance_type                = "c8i.xlarge"
worker_enable_nested_virtualization = true
worker_desired_capacity             = 0
worker_min_size                     = 0
worker_max_size                     = 1
worker_root_volume_size_gb          = ${DEV_WORKER_ROOT_VOLUME_SIZE_GB:-120}
worker_root_volume_iops             = ${DEV_WORKER_ROOT_VOLUME_IOPS:-3000}
worker_root_volume_throughput       = ${DEV_WORKER_ROOT_VOLUME_THROUGHPUT:-125}
worker_disk_mib                     = ${DEV_WORKER_DISK_MIB:-null}
EOF
  info "wrote ${DEV_TFVARS}"
}

dev_apply() {
  [ -f "${DEV_TFVARS}" ] || die "${DEV_TFVARS} does not exist; run dev-tfvars and fill required values first"
  tf_apply "${DEV_STACK}" -var-file="${DEV_TFVARS}"
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

ensure_worker_control_url_ready() {
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
  [ -n "${certificate_arn}" ] || die "dev-worker-tfvars requires DEV_CERTIFICATE_ARN or an existing certificate_arn tfvar before enabling workers; the dev stack only derives a private worker control URL when create_worker=true and certificate_arn is set."

  enable_cloudfront="$(tfvar_value "${DEV_TFVARS}" "enable_cloudfront" 2>/dev/null || printf 'false')"
  validate_tf_bool enable_cloudfront "${enable_cloudfront}"
  if [ "${enable_cloudfront}" = "true" ]; then
    cloudfront_origin="$(tfvar_string_value "${DEV_TFVARS}" "cloudfront_origin_domain_name" || true)"
    [ -n "${cloudfront_origin}" ] || die "dev-worker-tfvars requires DEV_CLOUDFRONT_ORIGIN_DOMAIN_NAME or an existing cloudfront_origin_domain_name tfvar when enable_cloudfront=true; workers use that origin hostname for their private control URL."
    require_cloudfront_origin_domain_name "${cloudfront_origin}"
  else
    public_url="$(tfvar_string_value "${DEV_TFVARS}" "public_url" || true)"
    [ -n "${public_url}" ] || die "dev-worker-tfvars requires DEV_PUBLIC_URL or an existing public_url tfvar when enable_cloudfront=false; workers use that hostname for their private control URL."
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
    if ! worker_asg_empty; then
      die "enable_nat_gateway=false is unsafe while worker ASG instances may still exist; run aws-dev-debug.sh worker-down and wait for completion first"
    fi
  fi
}

worker_asg_empty() {
  asg="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw worker_autoscaling_group_name 2>/dev/null || true)"
  [ -n "${asg}" ] && [ "${asg}" != "null" ] || return 2
  command -v aws >/dev/null 2>&1 || return 2

  total="$(
    aws autoscaling describe-auto-scaling-groups \
      --region "${AWS_REGION}" \
      --auto-scaling-group-names "${asg}" \
      --query 'length(AutoScalingGroups[0].Instances)' \
      --output text 2>/dev/null || true
  )"
  case "${total}" in
    0) return 0 ;;
    ''|None|null) return 2 ;;
    *) return 1 ;;
  esac
}

set_control_network_after_worker_down() {
  if worker_asg_empty; then
    set_tfvar "${DEV_TFVARS}" "enable_nat_gateway" "false"
    set_tfvar "${DEV_TFVARS}" "control_assign_public_ip" "true"
  else
    status=$?
    set_tfvar "${DEV_TFVARS}" "enable_nat_gateway" "true"
    if [ "${status}" -eq 1 ]; then
      info "kept NAT Gateway enabled because worker ASG still has instances"
    else
      info "kept NAT Gateway enabled because worker ASG state could not be verified"
    fi
  fi
}

dev_control_tfvars() {
  control_image="${DEV_CONTROL_IMAGE:-}"
  if [ -z "${control_image}" ] && [ -f "${CONTROL_IMAGE_URI_FILE}" ]; then
    control_image="$(cat "${CONTROL_IMAGE_URI_FILE}")"
  fi
  [ -n "${control_image}" ] || die "DEV_CONTROL_IMAGE is required, or run control-image-build first"
  [ -n "${DEV_GITHUB_APP_ID:-}" ] || die "DEV_GITHUB_APP_ID is required"
  [ -n "${DEV_GITHUB_APP_SLUG:-}" ] || die "DEV_GITHUB_APP_SLUG is required"
  [ -n "${DEV_GITHUB_APP_CLIENT_ID:-}" ] || die "DEV_GITHUB_APP_CLIENT_ID is required"

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
  set_tfvar "${DEV_TFVARS}" "public_url" "$(tf_quote "${DEV_PUBLIC_URL:-http://localhost}")"
  unset_tfvar "${DEV_TFVARS}" "control_url"
  unset_tfvar "${DEV_TFVARS}" "worker_control_url"
  unset_tfvar "${DEV_TFVARS}" "enable_private_control_dns"
  set_tfvar "${DEV_TFVARS}" "enable_nat_gateway" "${DEV_ENABLE_NAT_GATEWAY:-false}"
  set_tfvar "${DEV_TFVARS}" "control_image" "$(tf_quote "${control_image}")"
  if env_is_set DEV_CERTIFICATE_ARN; then
    set_tfvar "${DEV_TFVARS}" "certificate_arn" "$(tf_quote "${DEV_CERTIFICATE_ARN}")"
  else
    set_tfvar "${DEV_TFVARS}" "certificate_arn" "null"
  fi
  set_tfvar "${DEV_TFVARS}" "allow_insecure_http" "${DEV_ALLOW_INSECURE_HTTP:-true}"
  set_tfvar "${DEV_TFVARS}" "enable_cloudfront" "${DEV_ENABLE_CLOUDFRONT:-false}"
  if [ "${DEV_ENABLE_CLOUDFRONT:-false}" = "true" ]; then
    [ -n "${DEV_CLOUDFRONT_ORIGIN_DOMAIN_NAME:-}" ] || die "DEV_CLOUDFRONT_ORIGIN_DOMAIN_NAME is required when DEV_ENABLE_CLOUDFRONT=true"
    set_tfvar "${DEV_TFVARS}" "cloudfront_origin_domain_name" "$(tf_quote "${DEV_CLOUDFRONT_ORIGIN_DOMAIN_NAME}")"
  else
    set_tfvar "${DEV_TFVARS}" "cloudfront_origin_domain_name" "null"
  fi
  set_tfvar "${DEV_TFVARS}" "github_app_id" "$(tf_quote "${DEV_GITHUB_APP_ID}")"
  set_tfvar "${DEV_TFVARS}" "github_app_slug" "$(tf_quote "${DEV_GITHUB_APP_SLUG}")"
  set_tfvar "${DEV_TFVARS}" "github_app_client_id" "$(tf_quote "${DEV_GITHUB_APP_CLIENT_ID}")"
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
  set_tfvar "${DEV_TFVARS}" "control_ecr_max_images" "${DEV_CONTROL_ECR_MAX_IMAGES:-10}"
  set_tfvar "${DEV_TFVARS}" "control_ecr_untagged_image_expiration_days" "${DEV_CONTROL_ECR_UNTAGGED_IMAGE_EXPIRATION_DAYS:-1}"
  if [ "${DEV_CONTROL_KEEP_WORKER:-0}" != "1" ]; then
    if [ "$(tfvar_value "${DEV_TFVARS}" "create_worker" 2>/dev/null || true)" = "true" ]; then
      previous_worker_desired="$(tfvar_value "${DEV_TFVARS}" "worker_desired_capacity" 2>/dev/null || printf '0')"
      set_tfvar "${DEV_TFVARS}" "worker_desired_capacity" "0"
      set_tfvar "${DEV_TFVARS}" "worker_min_size" "0"
      if [ "${previous_worker_desired}" = "0" ]; then
        set_control_network_after_worker_down
      else
        set_tfvar "${DEV_TFVARS}" "enable_nat_gateway" "true"
      fi
    else
      set_tfvar "${DEV_TFVARS}" "create_worker" "false"
      set_tfvar "${DEV_TFVARS}" "enable_nat_gateway" "false"
      set_tfvar "${DEV_TFVARS}" "control_assign_public_ip" "true"
    fi
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

put_secret_value() {
  secret_id=$1
  secret_value=$2
  mkdir -p "${STATE_DIR}"
  input_file="$(mktemp "${STATE_DIR}/secret-put.XXXXXX.json")"
  secret_file="$(mktemp "${STATE_DIR}/secret-value.XXXXXX.txt")"
  chmod 0600 "${input_file}"
  chmod 0600 "${secret_file}"
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
  mkdir -p "${STATE_DIR}"
  error_file="$(mktemp "${STATE_DIR}/secret-get.XXXXXX.err")"
  chmod 0600 "${error_file}"
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
  password_file="$(mktemp "${STATE_DIR}/database-password.XXXXXX.txt")"
  chmod 0600 "${password_file}"
  trap 'rm -f "${password_file}"' RETURN
  printf '%s' "${password}" >"${password_file}"
  encoded_password="$(jq -sRr @uri <"${password_file}")"
  rm -f "${password_file}"
  trap - RETURN
  put_secret_value_if_missing "${database_secret_arn}" "postgres://${username}:${encoded_password}@${endpoint}/helmr?sslmode=require"
  info "database_url secret populated: ${database_secret_arn}"
}

dev_generated_secrets() {
  dev_database_url
  put_secret_value_if_missing "$(dev_secret_arn worker_token_signing_key)" "$(random_base64)"
  put_secret_value_if_missing "$(dev_secret_arn auth_secret)" "$(random_base64)"
  put_secret_value_if_missing "$(dev_secret_arn secret_encryption_key)" "$(random_base64)"
  put_secret_value_if_missing "$(dev_secret_arn checkpoint_encryption_key)" "$(random_base64)"
  put_secret_value_if_missing "$(dev_secret_arn worker_registration_token)" "$(random_hex)"
  info "generated non-GitHub secrets populated"
}

random_hex() {
  dd if=/dev/urandom bs=32 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'
}

dev_github_webhook_secret() {
  mkdir -p "${STATE_DIR}"
  chmod 700 "${STATE_DIR}"
  if [ ! -f "${GITHUB_WEBHOOK_SECRET_FILE}" ]; then
    random_hex >"${GITHUB_WEBHOOK_SECRET_FILE}"
    chmod 0600 "${GITHUB_WEBHOOK_SECRET_FILE}"
  fi
  put_secret_value_if_missing "$(dev_secret_arn github_app_webhook_secret)" "$(cat "${GITHUB_WEBHOOK_SECRET_FILE}")"
  info "GitHub App webhook secret stored in AWS and ${GITHUB_WEBHOOK_SECRET_FILE}"
  printf '%s\n' "${GITHUB_WEBHOOK_SECRET_FILE}"
}

read_secret_file() {
  path=$1
  [ -n "${path}" ] || die "secret file path is required"
  [ -f "${path}" ] || die "secret file does not exist: ${path}"
  cat "${path}"
}

dev_github_secrets() {
  private_key_file="${GITHUB_APP_PRIVATE_KEY_FILE:-}"
  client_secret_file="${GITHUB_APP_CLIENT_SECRET_FILE:-}"
  webhook_secret_file="${GITHUB_APP_WEBHOOK_SECRET_FILE:-${GITHUB_WEBHOOK_SECRET_FILE}}"

  [ -n "${private_key_file}" ] || die "GITHUB_APP_PRIVATE_KEY_FILE is required"
  [ -n "${client_secret_file}" ] || die "GITHUB_APP_CLIENT_SECRET_FILE is required"
  if [ ! -f "${webhook_secret_file}" ]; then
    dev_github_webhook_secret >/dev/null
  fi

  put_secret_value_if_missing "$(dev_secret_arn github_app_private_key)" "$(read_secret_file "${private_key_file}")"
  put_secret_value_if_missing "$(dev_secret_arn github_app_client_secret)" "$(read_secret_file "${client_secret_file}")"
  put_secret_value_if_missing "$(dev_secret_arn github_app_webhook_secret)" "$(read_secret_file "${webhook_secret_file}")"
  info "GitHub App secrets populated"
}

dev_worker_tfvars() {
  ami_id="${WORKER_AMI_ID:-}"
  if [ -z "${ami_id}" ] && [ -f "${AMI_ID_FILE}" ]; then
    ami_id="$(cat "${AMI_ID_FILE}")"
  fi
  [ -n "${ami_id}" ] || die "WORKER_AMI_ID is required, or run worker-image-wait first"
  [ -f "${DEV_TFVARS}" ] || die "${DEV_TFVARS} does not exist; run dev-control-tfvars first"

  ensure_worker_control_url_ready
  set_tfvar "${DEV_TFVARS}" "create_worker" "true"
  set_tfvar "${DEV_TFVARS}" "enable_nat_gateway" "true"
  set_tfvar "${DEV_TFVARS}" "control_assign_public_ip" "false"
  set_tfvar "${DEV_TFVARS}" "worker_ami_id" "$(tf_quote "${ami_id}")"
  set_tfvar "${DEV_TFVARS}" "worker_instance_type" "$(tf_quote "${WORKER_INSTANCE_TYPE:-c8i.xlarge}")"
  set_tfvar "${DEV_TFVARS}" "worker_enable_nested_virtualization" "true"
  set_tfvar "${DEV_TFVARS}" "worker_desired_capacity" "1"
  set_tfvar "${DEV_TFVARS}" "worker_min_size" "1"
  set_tfvar "${DEV_TFVARS}" "worker_max_size" "1"
  set_tfvar "${DEV_TFVARS}" "worker_root_volume_size_gb" "${DEV_WORKER_ROOT_VOLUME_SIZE_GB:-120}"
  set_tfvar "${DEV_TFVARS}" "worker_root_volume_iops" "${DEV_WORKER_ROOT_VOLUME_IOPS:-3000}"
  set_tfvar "${DEV_TFVARS}" "worker_root_volume_throughput" "${DEV_WORKER_ROOT_VOLUME_THROUGHPUT:-125}"
  set_tfvar "${DEV_TFVARS}" "worker_disk_mib" "${DEV_WORKER_DISK_MIB:-null}"
  info "updated ${DEV_TFVARS} for one worker"
}

dev_worker_down_tfvars() {
  [ -f "${DEV_TFVARS}" ] || die "${DEV_TFVARS} does not exist; run dev-worker-tfvars first"

  previous_worker_desired="$(tfvar_value "${DEV_TFVARS}" "worker_desired_capacity" 2>/dev/null || printf '0')"
  set_tfvar "${DEV_TFVARS}" "create_worker" "true"
  set_tfvar "${DEV_TFVARS}" "worker_desired_capacity" "0"
  set_tfvar "${DEV_TFVARS}" "worker_min_size" "0"
  set_tfvar "${DEV_TFVARS}" "worker_max_size" "${WORKER_MAX_SIZE:-1}"
  if [ "${previous_worker_desired}" = "0" ]; then
    set_control_network_after_worker_down
  else
    set_tfvar "${DEV_TFVARS}" "enable_nat_gateway" "true"
  fi
  info "updated ${DEV_TFVARS} to keep worker resources but stop worker instances"
}

dev_migrate() {
  cluster="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw control_cluster_name)"
  task_definition="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw migration_task_definition_arn)"
  security_group="$("${TF_BIN}" -chdir="${DEV_STACK}" output -raw control_security_group_id)"
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
      --arg sg "${security_group}" \
      --arg assign_public_ip "${assign_public_ip_value}" \
      '{awsvpcConfiguration:{subnets:$subnets,securityGroups:[$sg],assignPublicIp:$assign_public_ip}}'
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

json_array_length() {
  jq 'length'
}

dev_destroy_prepare() {
  mkdir -p "${STATE_DIR}"
  name="${DEV_NAME:-helmr-smoke}"
  asg_name="${name}-worker"
  db_identifier="${name}-postgres"
  repository_name="${name}/control"

  if aws autoscaling describe-auto-scaling-groups \
    --region "${AWS_REGION}" \
    --auto-scaling-group-names "${asg_name}" \
    --query 'AutoScalingGroups[0].AutoScalingGroupName' \
    --output text 2>/dev/null | grep -qx "${asg_name}"; then
    hook_name="$(
      aws autoscaling describe-lifecycle-hooks \
        --region "${AWS_REGION}" \
        --auto-scaling-group-name "${asg_name}" \
        --query "LifecycleHooks[?LifecycleTransition=='autoscaling:EC2_INSTANCE_TERMINATING'].LifecycleHookName | [0]" \
        --output text
    )"
    if [ -n "${hook_name}" ] && [ "${hook_name}" != "None" ]; then
      instances="$(
        aws autoscaling describe-auto-scaling-groups \
          --region "${AWS_REGION}" \
          --auto-scaling-group-names "${asg_name}" \
          --query "AutoScalingGroups[0].Instances[?LifecycleState=='Terminating:Wait'].InstanceId" \
          --output text
      )"
      for instance_id in ${instances}; do
        aws autoscaling complete-lifecycle-action \
          --region "${AWS_REGION}" \
          --auto-scaling-group-name "${asg_name}" \
          --lifecycle-hook-name "${hook_name}" \
          --lifecycle-action-result CONTINUE \
          --instance-id "${instance_id}" >/dev/null
        info "completed termination lifecycle action for ${instance_id}"
      done
    fi
  fi

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

  if aws ecr describe-repositories \
    --region "${AWS_REGION}" \
    --repository-names "${repository_name}" >/dev/null 2>&1; then
    image_ids_file="$(mktemp "${STATE_DIR}/ecr-image-ids.XXXXXX.json")"
    trap 'rm -f "${image_ids_file}"' RETURN
    aws ecr list-images \
      --region "${AWS_REGION}" \
      --repository-name "${repository_name}" \
      --filter tagStatus=ANY \
      --query 'imageIds' \
      --output json >"${image_ids_file}"
    if [ "$(json_array_length <"${image_ids_file}")" -gt 0 ]; then
      aws ecr batch-delete-image \
        --region "${AWS_REGION}" \
        --repository-name "${repository_name}" \
        --image-ids "file://${image_ids_file}" >/dev/null
      info "deleted images from ${repository_name}"
    fi
    rm -f "${image_ids_file}"
    trap - RETURN
  fi
}

command=${1:-}
case "${command}" in
  check) check ;;
  bootstrap-init) bootstrap_init ;;
  bootstrap-apply) bootstrap_apply ;;
  bootstrap-output) bootstrap_output ;;
  bootstrap-destroy-prepare) bootstrap_destroy_prepare ;;
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
  dev-github-webhook-secret) dev_github_webhook_secret ;;
  dev-github-secrets) dev_github_secrets ;;
  dev-control-tfvars) dev_control_tfvars ;;
  dev-worker-tfvars) dev_worker_tfvars ;;
  dev-worker-down-tfvars) dev_worker_down_tfvars ;;
  dev-migrate) dev_migrate ;;
  dev-destroy-prepare) dev_destroy_prepare ;;
  -h|--help|help|"") usage ;;
  *) usage >&2; die "unknown command: ${command}" ;;
esac
