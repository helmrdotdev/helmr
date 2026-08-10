#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TF_BIN="${TF_BIN:-tofu}"
AWS_REGION="${AWS_REGION:-us-east-1}"
STATE_REGION="${STATE_REGION:-${AWS_REGION}}"
STATE_KEY="${STATE_KEY:-}"
WORKER_IMAGE_NAME="${WORKER_IMAGE_NAME:-helmr-worker-image}"
WORKER_IMAGE_VERSION="${WORKER_IMAGE_VERSION:-}"
WORKER_IMAGE_DISTRIBUTION_REGIONS="${WORKER_IMAGE_DISTRIBUTION_REGIONS:-}"
WORKER_IMAGE_AMI_PUBLIC="${WORKER_IMAGE_AMI_PUBLIC:-}"
WORKER_IMAGE_ROOT_VOLUME_ENCRYPTED="${WORKER_IMAGE_ROOT_VOLUME_ENCRYPTED:-}"
BOOTSTRAP_NAME="${BOOTSTRAP_NAME:-helmr-release}"
BOOTSTRAP_STACK="${BOOTSTRAP_STACK:-${ROOT}/infra/aws/modules/bootstrap}"
WORKER_IMAGE_STACK="${WORKER_IMAGE_STACK:-${ROOT}/infra/aws/stacks/worker-image}"
STATE_DIR="${STATE_DIR:-${ROOT}/.helmr-release-artifacts}"
IMAGE_ARN_FILE="${STATE_DIR}/worker-image-build-version-arn"
AMI_ID_FILE="${STATE_DIR}/worker-ami-id"
AMI_IDS_FILE="${STATE_DIR}/worker-ami-ids.json"
BUILD_POLICY_DIGEST_FILE="${STATE_DIR}/build-policy-digest"
WORKER_IMAGE_PROVENANCE_FILE="${STATE_DIR}/worker-image-provenance.json"
WORKER_HOST_ARTIFACTS_MANIFEST_FILE="${STATE_DIR}/worker-host-artifacts.json"
WORKER_HOST_BUNDLE_RECEIPT_FILE="${STATE_DIR}/worker-host-bundle.json"
WORKER_RUNTIME_ARTIFACTS_MANIFEST_FILE="${STATE_DIR}/worker-runtime-artifacts.json"
WORKER_RUNTIME_BUNDLE_RECEIPT_FILE="${STATE_DIR}/worker-runtime-bundle.json"
CONTROLPLANE_IMAGE_PROVENANCE_FILE="${STATE_DIR}/controlplane-image-provenance.json"
CONTROLPLANE_IMAGE_URI_FILE="${STATE_DIR}/controlplane-image-uri"
IMAGE_WAIT_INTERVAL_SECONDS="${IMAGE_WAIT_INTERVAL_SECONDS:-60}"
IMAGE_WAIT_TIMEOUT_SECONDS="${IMAGE_WAIT_TIMEOUT_SECONDS:-7200}"

usage() {
  cat <<'EOF'
Usage: scripts/aws-release-artifacts.sh <command>

Commands:
  check
  bootstrap-init
  bootstrap-apply
  bootstrap-output
  platform-release-publish
  worker-image-init
  worker-image-apply
  worker-image-start
  worker-image-wait
  worker-image-amis
  worker-image-provenance
  controlplane-image-build
  controlplane-image-push

This Product-owned tool builds and publishes release artifacts. Managed Cloud
environment composition and validation live in the private cloud repository.
EOF
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

info() {
  printf '==> %s\n' "$*" >&2
}

sha256_file() {
  local path=$1
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${path}" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${path}" | awk '{print $1}'
  else
    die "sha256sum or shasum is required"
  fi
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || die "missing command: $1"
}

require_clean_product_checkout() {
  [ -z "$(git -C "${ROOT}" status --porcelain --untracked-files=all)" ] ||
    die "authenticated release artifacts require a clean checkout at the exact source commit"
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
  printf 'export WORKER_IMAGE_ARTIFACT_BUCKET=%q\n' "${artifact_bucket}"
  for output_name in \
    controlplane_release_repository_url \
    controlplane_release_repository_arn \
    platform_publisher_role_arn \
    platform_store_uri \
    platform_store_bucket_arn \
    platform_store_kms_key_arn; do
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
    AWS_REGION="${AWS_REGION}" \
    AWS_DEFAULT_REGION="${AWS_REGION}" \
    "$@"
}

worker_image_artifact_bucket() {
  if [ -n "${WORKER_IMAGE_ARTIFACT_BUCKET:-}" ]; then
    printf '%s\n' "${WORKER_IMAGE_ARTIFACT_BUCKET}"
    return 0
  fi
  if artifact_bucket="$("${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" output -raw source_artifact_bucket_name 2>/dev/null)"; then
    printf '%s\n' "${artifact_bucket}"
    return 0
  fi
  die "WORKER_IMAGE_ARTIFACT_BUCKET is required; run bootstrap-apply and export bootstrap-output"
}

tf_bool() {
  value="$(printf '%s\n' "$1" | tr '[:upper:]' '[:lower:]')"
  case "${value}" in
    1|true|yes|on) printf 'true\n' ;;
    0|false|no|off) printf 'false\n' ;;
    *) die "invalid boolean value: $1" ;;
  esac
}

s3_object_arn() {
  uri=$1
  case "${uri}" in
    s3://*/*)
      without_scheme=${uri#s3://}
      bucket=${without_scheme%%/*}
      key=${without_scheme#*/}
      printf 'arn:aws:s3:::%s/%s\n' "${bucket}" "${key}"
      ;;
    *)
      die "artifact URI must be an s3:// URI: ${uri}"
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
  with_platform_publisher nix develop "${ROOT}" -c go run ./cmd/helmr-controlplane release publish \
    --store "${platform_store_uri}" \
    --input "${publish_input}"
  build_policy_digest="$(cat "${release}/build-policy.digest")"
  printf '%s\n' "${build_policy_digest}" >"${BUILD_POLICY_DIGEST_FILE}"
  info "Platform release published: ${build_policy_digest}"
  printf '%s\n' "${build_policy_digest}"
)

worker_runtime_profile() {
  local artifacts_manifest=$1
  nix develop "${ROOT}#smoke-linux" -c \
    go -C "${ROOT}" run ./cmd/helmr-worker runtime-profile \
    --runtime-artifacts-manifest "${artifacts_manifest}"
}

validate_worker_host_bundle_receipt() {
  jq -e '
    (keys | sort) == ["bundle", "manifest", "schema", "sourceCommit", "workerVersion"] and
    .schema == "helmr.worker-host-bundle.v0" and
    (.sourceCommit | test("^[0-9a-f]{40}$")) and
    .workerVersion == .sourceCommit and
    .bundle.path == "worker-host-artifacts.tar" and
    (.bundle.digest | test("^sha256:[0-9a-f]{64}$")) and
    .manifest.path == "worker-host-artifacts.json" and
    (.manifest.digest | test("^sha256:[0-9a-f]{64}$"))
  ' "$1" >/dev/null
}

worker_image_artifact_exists() {
  local bucket=$1 key=$2 error status
  error="$(mktemp "${STATE_DIR}/worker-image-head.XXXXXX")"
  if aws s3api head-object \
    --region "${AWS_REGION}" \
    --bucket "${bucket}" \
    --key "${key}" >/dev/null 2>"${error}"; then
    rm -f "${error}"
    return 0
  else
    status=$?
  fi
  if grep -Fq '(404)' "${error}" && grep -Fq 'HeadObject' "${error}"; then
    rm -f "${error}"
    return 10
  fi
  cat "${error}" >&2
  rm -f "${error}"
  return "${status}"
}

prepare_worker_host_bundle() (
  local bucket bundle_dir bundle_digest bundle_key bundle_status bundle_uri host_dir kms_key_arn manifest_digest receipt work
  mkdir -p "${STATE_DIR}"
  work="$(mktemp -d "${STATE_DIR}/worker-host-bundle.XXXXXX")"
  trap 'rm -rf "${work}"' EXIT
  bundle_dir="${work}/bundle"
  info "building the canonical Worker host artifacts"
  host_dir="$(nix build -L --no-link --print-out-paths "${ROOT}#workerHost")"
  nix develop "${ROOT}" -c \
    "${ROOT}/scripts/materialize-worker-host-bundle.sh" "${bundle_dir}" "${host_dir}" >/dev/null
  receipt="${bundle_dir}/worker-host-bundle.json"
  validate_worker_host_bundle_receipt "${receipt}" || die "Worker host bundle receipt is invalid"
  [ "$(jq -r '.sourceCommit' "${receipt}")" = "$(git -C "${ROOT}" rev-parse HEAD)" ] ||
    die "Worker host bundle was not produced from the current source commit"
  bundle_digest="$(jq -r '.bundle.digest' "${receipt}")"
  manifest_digest="$(jq -r '.manifest.digest' "${receipt}")"
  [ "${bundle_digest}" = "sha256:$(sha256_file "${bundle_dir}/worker-host-artifacts.tar")" ] ||
    die "Worker host bundle digest does not match its receipt"
  [ "${manifest_digest}" = "sha256:$(sha256_file "${bundle_dir}/worker-host-artifacts.json")" ] ||
    die "Worker host artifact manifest digest does not match its receipt"

  bucket="$(worker_image_artifact_bucket)"
  bundle_key="helmr/worker-host-bundles/${bundle_digest#sha256:}.tar"
  bundle_uri="s3://${bucket}/${bundle_key}"
  bundle_status=0
  worker_image_artifact_exists "${bucket}" "${bundle_key}" || bundle_status=$?
  case "${bundle_status}" in
    0)
      info "Worker host bundle reused: ${bundle_uri}"
      ;;
    10)
      aws s3 cp --region "${AWS_REGION}" "${bundle_dir}/worker-host-artifacts.tar" "${bundle_uri}" >&2
      info "Worker host bundle uploaded: ${bundle_uri}"
      ;;
    *)
      return "${bundle_status}"
      ;;
  esac
  install -m 0600 "${bundle_dir}/worker-host-artifacts.json" "${WORKER_HOST_ARTIFACTS_MANIFEST_FILE}"
  install -m 0600 "${receipt}" "${WORKER_HOST_BUNDLE_RECEIPT_FILE}"
  kms_key_arn="$(bucket_kms_key_arn "${bucket}")"
  jq -cn \
    --arg bundle_digest "${bundle_digest}" \
    --arg kms_key_arn "${kms_key_arn}" \
    --arg manifest_digest "${manifest_digest}" \
    --arg object_arn "$(s3_object_arn "${bundle_uri}")" \
    --arg s3_uri "${bundle_uri}" \
    '{
      bundleDigest: $bundle_digest,
      kmsKeyARN: $kms_key_arn,
      manifestDigest: $manifest_digest,
      objectARN: $object_arn,
      s3URI: $s3_uri
    }'
)

validate_worker_runtime_bundle_receipt() {
  jq -e '
    (keys | sort) == ["bundle", "runtimeArtifactsManifest", "runtimeProfile", "schema", "sourceCommit"] and
    .schema == "helmr.worker-runtime-bundle.v0" and
    (.sourceCommit | test("^[0-9a-f]{40}$")) and
    .bundle.path == "runtime-artifacts.tar" and
    (.bundle.digest | test("^sha256:[0-9a-f]{64}$")) and
    .runtimeArtifactsManifest.path == "runtime-artifacts.json" and
    (.runtimeArtifactsManifest.digest | test("^sha256:[0-9a-f]{64}$")) and
    (.runtimeProfile | keys | sort) == ["arch", "contract", "id", "initramfs_digest", "kernel_digest", "rootfs_digest"] and
    .runtimeProfile.arch == "x86_64" and
    .runtimeProfile.contract == "helmr.vm-runtime.v0" and
    all(.runtimeProfile.id, .runtimeProfile.kernel_digest, .runtimeProfile.initramfs_digest, .runtimeProfile.rootfs_digest; test("^sha256:[0-9a-f]{64}$"))
  ' "$1" >/dev/null
}

prepare_worker_runtime_bundle() (
  local artifacts_dir bucket bundle_dir bundle_digest bundle_key bundle_status bundle_uri kms_key_arn manifest_digest receipt work
  mkdir -p "${STATE_DIR}"
  artifacts_dir="${ROOT}/images/guest/out"
  work="$(mktemp -d "${STATE_DIR}/worker-runtime-bundle.XXXXXX")"
  trap 'rm -rf "${work}"' EXIT
  bundle_dir="${work}/bundle"
  info "building the canonical Worker runtime artifacts"
  nix develop "${ROOT}#smoke-linux" -c make -C "${ROOT}" images >&2
  nix develop "${ROOT}#smoke-linux" -c \
    "${ROOT}/scripts/materialize-worker-runtime-bundle.sh" "${bundle_dir}" "${artifacts_dir}" >/dev/null
  receipt="${bundle_dir}/worker-runtime-bundle.json"
  validate_worker_runtime_bundle_receipt "${receipt}" || die "Worker runtime bundle receipt is invalid"
  [ "$(jq -r '.sourceCommit' "${receipt}")" = "$(git -C "${ROOT}" rev-parse HEAD)" ] ||
    die "Worker runtime bundle was not produced from the current source commit"
  bundle_digest="$(jq -r '.bundle.digest' "${receipt}")"
  manifest_digest="$(jq -r '.runtimeArtifactsManifest.digest' "${receipt}")"
  [ "${bundle_digest}" = "sha256:$(sha256_file "${bundle_dir}/runtime-artifacts.tar")" ] ||
    die "Worker runtime bundle digest does not match its receipt"
  [ "${manifest_digest}" = "sha256:$(sha256_file "${bundle_dir}/runtime-artifacts.json")" ] ||
    die "Worker runtime artifact manifest digest does not match its receipt"

  bucket="$(worker_image_artifact_bucket)"
  bundle_key="helmr/worker-runtime-bundles/${bundle_digest#sha256:}.tar"
  bundle_uri="s3://${bucket}/${bundle_key}"
  bundle_status=0
  worker_image_artifact_exists "${bucket}" "${bundle_key}" || bundle_status=$?
  case "${bundle_status}" in
    0)
      info "Worker runtime bundle reused: ${bundle_uri}"
      ;;
    10)
      aws s3 cp --region "${AWS_REGION}" "${bundle_dir}/runtime-artifacts.tar" "${bundle_uri}" >&2
      info "Worker runtime bundle uploaded: ${bundle_uri}"
      ;;
    *)
      return "${bundle_status}"
      ;;
  esac
  install -m 0600 "${bundle_dir}/runtime-artifacts.json" "${WORKER_RUNTIME_ARTIFACTS_MANIFEST_FILE}"
  install -m 0600 "${receipt}" "${WORKER_RUNTIME_BUNDLE_RECEIPT_FILE}"
  kms_key_arn="$(bucket_kms_key_arn "${bucket}")"
  jq -cn \
    --arg bundle_digest "${bundle_digest}" \
    --arg kms_key_arn "${kms_key_arn}" \
    --arg manifest_digest "${manifest_digest}" \
    --arg object_arn "$(s3_object_arn "${bundle_uri}")" \
    --arg s3_uri "${bundle_uri}" \
    '{
      bundleDigest: $bundle_digest,
      kmsKeyARN: $kms_key_arn,
      manifestDigest: $manifest_digest,
      objectARN: $object_arn,
      s3URI: $s3_uri
    }'
)

worker_image_apply() {
  require_clean_product_checkout
  nix develop "${ROOT}#images" -c "${ROOT}/scripts/check-apko-lock.sh"
  host_bundle="$(prepare_worker_host_bundle)"
  host_artifacts_bundle_digest="$(jq -er '.bundleDigest' <<<"${host_bundle}")"
  host_artifacts_bundle_kms_key_arn="$(jq -er '.kmsKeyARN' <<<"${host_bundle}")"
  host_artifacts_bundle_object_arn="$(jq -er '.objectARN' <<<"${host_bundle}")"
  host_artifacts_bundle_s3_uri="$(jq -er '.s3URI' <<<"${host_bundle}")"
  host_artifacts_manifest_digest="$(jq -er '.manifestDigest' <<<"${host_bundle}")"
  runtime_bundle="$(prepare_worker_runtime_bundle)"
  runtime_artifacts_bundle_digest="$(jq -er '.bundleDigest' <<<"${runtime_bundle}")"
  runtime_artifacts_bundle_kms_key_arn="$(jq -er '.kmsKeyARN' <<<"${runtime_bundle}")"
  runtime_artifacts_bundle_object_arn="$(jq -er '.objectARN' <<<"${runtime_bundle}")"
  runtime_artifacts_bundle_s3_uri="$(jq -er '.s3URI' <<<"${runtime_bundle}")"
  runtime_artifacts_manifest_digest="$(jq -er '.manifestDigest' <<<"${runtime_bundle}")"
  source_ref="$(git -C "${ROOT}" rev-parse HEAD)"
  version_args=(
    -var="image_version=$(worker_image_version)"
    -var="source_ref=${source_ref}"
    -var="host_artifacts_bundle_digest=${host_artifacts_bundle_digest}"
    -var="host_artifacts_bundle_object_arn=${host_artifacts_bundle_object_arn}"
    -var="host_artifacts_bundle_s3_uri=${host_artifacts_bundle_s3_uri}"
    -var="host_artifacts_manifest_digest=${host_artifacts_manifest_digest}"
    -var="runtime_artifacts_bundle_digest=${runtime_artifacts_bundle_digest}"
    -var="runtime_artifacts_bundle_object_arn=${runtime_artifacts_bundle_object_arn}"
    -var="runtime_artifacts_bundle_s3_uri=${runtime_artifacts_bundle_s3_uri}"
    -var="runtime_artifacts_manifest_digest=${runtime_artifacts_manifest_digest}"
  )
  if [ -n "${host_artifacts_bundle_kms_key_arn}" ]; then
    version_args+=(-var="host_artifacts_bundle_kms_key_arn=${host_artifacts_bundle_kms_key_arn}")
  fi
  if [ -n "${runtime_artifacts_bundle_kms_key_arn}" ]; then
    version_args+=(-var="runtime_artifacts_bundle_kms_key_arn=${runtime_artifacts_bundle_kms_key_arn}")
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
  apply_args=(
    -var="aws_region=${AWS_REGION}"
    -var="name=${WORKER_IMAGE_NAME}"
  )
  if ((${#distribution_args[@]})); then apply_args+=("${distribution_args[@]}"); fi
  if ((${#public_args[@]})); then apply_args+=("${public_args[@]}"); fi
  if ((${#encryption_args[@]})); then apply_args+=("${encryption_args[@]}"); fi
  if ((${#version_args[@]})); then apply_args+=("${version_args[@]}"); fi
  tf_apply "${WORKER_IMAGE_STACK}" "${apply_args[@]}"
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

worker_image_wait() (
  mkdir -p "${STATE_DIR}"
  image_arn="${1:-${WORKER_IMAGE_BUILD_VERSION_ARN:-}}"
  if [ -z "${image_arn}" ] && [ -f "${IMAGE_ARN_FILE}" ]; then
    image_arn="$(cat "${IMAGE_ARN_FILE}")"
  fi
  [ -n "${image_arn}" ] || die "image build version ARN is required; run worker-image-start first"
  [ -f "${WORKER_HOST_ARTIFACTS_MANIFEST_FILE}" ] ||
    die "Worker host provenance is missing; run worker-image-apply before worker-image-wait"
  [ -f "${WORKER_HOST_BUNDLE_RECEIPT_FILE}" ] ||
    die "Worker host bundle receipt is missing; run worker-image-apply before worker-image-wait"
  [ -f "${WORKER_RUNTIME_ARTIFACTS_MANIFEST_FILE}" ] ||
    die "Worker runtime provenance is missing; run worker-image-apply before worker-image-wait"
  [ -f "${WORKER_RUNTIME_BUNDLE_RECEIPT_FILE}" ] ||
    die "Worker runtime bundle receipt is missing; run worker-image-apply before worker-image-wait"
  validate_worker_runtime_bundle_receipt "${WORKER_RUNTIME_BUNDLE_RECEIPT_FILE}" ||
    die "Worker runtime bundle receipt is invalid"
  validate_worker_host_bundle_receipt "${WORKER_HOST_BUNDLE_RECEIPT_FILE}" ||
    die "Worker host bundle receipt is invalid"
  source_commit="$(git -C "${ROOT}" rev-parse HEAD)"
  [ "$(jq -r '.sourceCommit' "${WORKER_HOST_BUNDLE_RECEIPT_FILE}")" = "${source_commit}" ] ||
    die "Worker host bundle receipt does not match the current source commit"
  [ "$(jq -r '.sourceCommit' "${WORKER_RUNTIME_BUNDLE_RECEIPT_FILE}")" = "${source_commit}" ] ||
    die "Worker runtime bundle receipt does not match the current source commit"
  host_artifacts_bundle_digest="$(jq -r '.bundle.digest' "${WORKER_HOST_BUNDLE_RECEIPT_FILE}")"
  host_artifacts_manifest_digest="$(jq -r '.manifest.digest' "${WORKER_HOST_BUNDLE_RECEIPT_FILE}")"
  [ "${host_artifacts_manifest_digest}" = "sha256:$(sha256_file "${WORKER_HOST_ARTIFACTS_MANIFEST_FILE}")" ] ||
    die "Worker host artifact manifest does not match its bundle receipt"
  runtime_artifacts_bundle_digest="$(jq -r '.bundle.digest' "${WORKER_RUNTIME_BUNDLE_RECEIPT_FILE}")"
  runtime_artifacts_manifest_digest="$(jq -r '.runtimeArtifactsManifest.digest' "${WORKER_RUNTIME_BUNDLE_RECEIPT_FILE}")"
  [ "${runtime_artifacts_manifest_digest}" = "sha256:$(sha256_file "${WORKER_RUNTIME_ARTIFACTS_MANIFEST_FILE}")" ] ||
    die "Worker runtime artifact manifest does not match its bundle receipt"
  runtime_profile_file="$(mktemp "${STATE_DIR}/worker-runtime-profile.XXXXXX")"
  trap 'rm -f "${runtime_profile_file}"' EXIT
  worker_runtime_profile "${WORKER_RUNTIME_ARTIFACTS_MANIFEST_FILE}" >"${runtime_profile_file}"
  jq -e --slurpfile profile "${runtime_profile_file}" '.runtimeProfile == $profile[0]' \
    "${WORKER_RUNTIME_BUNDLE_RECEIPT_FILE}" >/dev/null ||
    die "Worker runtime profile does not match its bundle receipt"

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
        ami_json="$(
          aws ec2 describe-images \
            --region "${AWS_REGION}" \
            --owners self \
            --image-ids "${ami_id}" \
            --output json
        )"
        jq -e \
          --arg ami "${ami_id}" \
          --arg commit "${source_commit}" \
          --arg host_artifacts_bundle_digest "${host_artifacts_bundle_digest}" \
          --arg host_artifacts_manifest_digest "${host_artifacts_manifest_digest}" \
          --arg runtime_artifacts_bundle_digest "${runtime_artifacts_bundle_digest}" \
          --arg runtime_artifacts_manifest_digest "${runtime_artifacts_manifest_digest}" '
          (.Images // []) as $images |
          (($images[0].Tags // []) | map({key: .Key, value: .Value}) | from_entries) as $tags |
          ($images | length) == 1 and
          $images[0].ImageId == $ami and
          $tags.HelmrSourceCommit == $commit and
          $tags.HelmrHostBundleDigest == $host_artifacts_bundle_digest and
          $tags.HelmrHostArtifactsDigest == $host_artifacts_manifest_digest and
          $tags.HelmrRuntimeBundleDigest == $runtime_artifacts_bundle_digest and
          $tags.HelmrRuntimeArtifactsDigest == $runtime_artifacts_manifest_digest
        ' <<<"${ami_json}" >/dev/null ||
          die "available Worker AMI is not bound to the exact source and runtime artifacts"
        jq -cn \
          --arg ami "${ami_id}" \
          --arg build_arn "${image_arn}" \
          --arg recipe_arn "${recipe_arn}" \
          --arg region "${AWS_REGION}" \
          --arg source_commit "${source_commit}" \
          --arg host_artifacts_bundle_digest "${host_artifacts_bundle_digest}" \
          --arg host_artifacts_manifest_digest "${host_artifacts_manifest_digest}" \
          --arg runtime_artifacts_bundle_digest "${runtime_artifacts_bundle_digest}" \
          --arg runtime_artifacts_manifest_digest "${runtime_artifacts_manifest_digest}" \
          --slurpfile runtime_profile "${runtime_profile_file}" \
          '{
            ami: {id: $ami, region: $region},
            formatVersion: 1,
            hostArtifactsBundleDigest: $host_artifacts_bundle_digest,
            hostArtifactsManifestDigest: $host_artifacts_manifest_digest,
            imageBuildVersionARN: $build_arn,
            imageRecipeARN: $recipe_arn,
            runtimeArtifactsBundleDigest: $runtime_artifacts_bundle_digest,
            runtimeArtifactsManifestDigest: $runtime_artifacts_manifest_digest,
            runtimeProfile: $runtime_profile[0],
            sourceCommit: $source_commit,
            workerVersion: $source_commit
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
)

worker_image_amis() {
  [ -f "${AMI_IDS_FILE}" ] || die "worker AMI region map not found; run worker-image-wait first"
  jq -c . "${AMI_IDS_FILE}"
}

worker_image_provenance() {
  [ -f "${WORKER_IMAGE_PROVENANCE_FILE}" ] || die "worker image provenance not found; run worker-image-wait first"
  jq -c . "${WORKER_IMAGE_PROVENANCE_FILE}"
}

controlplane_image_repository() {
  bootstrap_contract_value CONTROLPLANE_RELEASE_REPOSITORY_URL controlplane_release_repository_url
}

controlplane_image_uri() {
  repository="$(controlplane_image_repository)"
  tag="${CONTROLPLANE_IMAGE_TAG:-$(git -C "${ROOT}" rev-parse --short=12 HEAD)-$(date -u +%Y%m%d%H%M%S)-$$}"
  printf '%s:%s\n' "${repository}" "${tag}"
}

controlplane_image_digest_uri() {
  image_uri=$1
  [ "${image_uri#*@}" = "${image_uri}" ] || die "controlplane-image-push requires a tag image URI, got digest-pinned image: ${image_uri}"

  repository="${image_uri%:*}"
  tag="${image_uri##*:}"
  repository_name="${repository#*/}"
  [ -n "${repository_name}" ] && [ "${repository_name}" != "${repository}" ] || die "controlplane image URI must include an ECR registry: ${image_uri}"
  [ -n "${tag}" ] && [ "${tag}" != "${image_uri}" ] || die "controlplane image URI must include a tag: ${image_uri}"

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

controlplane_image_context() {
  printf '%s\n' "${STATE_DIR}/controlplane-image"
}

controlplane_image_build() {
  need_command docker
  require_clean_product_checkout
  image_uri="$(controlplane_image_uri)"
  context="$(controlplane_image_context)"

  # shellcheck disable=SC2016
  nix develop "${ROOT}#images" -c env \
    CONTROLPLANE_IMAGE_CONTEXT="${context}" \
    IMAGE_URI="${image_uri}" \
    bash -ceu '
      cd "$1"
      ./scripts/build-controlplane-image.sh "$IMAGE_URI"
    ' bash "${ROOT}"

  printf '%s\n' "${image_uri}" >"${CONTROLPLANE_IMAGE_URI_FILE}"
  info "controlplane image built: ${image_uri}"
  printf '%s\n' "${image_uri}"
}

controlplane_image_push() {
  need_command aws
  need_command docker
  require_clean_product_checkout
  image_uri="${CONTROLPLANE_IMAGE_URI:-}"
  if [ -z "${image_uri}" ] && [ -f "${CONTROLPLANE_IMAGE_URI_FILE}" ]; then
    image_uri="$(cat "${CONTROLPLANE_IMAGE_URI_FILE}")"
  fi
  [ -n "${image_uri}" ] || die "CONTROLPLANE_IMAGE_URI is required, or run controlplane-image-build first"
  build_inputs_file="$(controlplane_image_context)/build-inputs.json"
  [ -f "${build_inputs_file}" ] || die "Control Plane image build-input receipt is missing; run controlplane-image-build first"
  expected_source_commit="$(git -C "${ROOT}" rev-parse HEAD)"
  "${ROOT}/scripts/verify-controlplane-image-build.sh" "${build_inputs_file}" "${image_uri}" ||
    die "Control Plane image build-input verification failed"
  registry="${image_uri%%/*}"
  (
    docker_config="$(mktemp -d "${STATE_DIR}/docker-config.XXXXXX")"
    trap 'docker --config "${docker_config}" logout "${registry}" >/dev/null 2>&1 || true; rm -rf "${docker_config}"' EXIT
    with_platform_publisher aws ecr get-login-password --region "${AWS_REGION}" |
      docker --config "${docker_config}" login --username AWS --password-stdin "${registry}"
    docker --config "${docker_config}" push "${image_uri}"
  )
  digest_image_uri="$(controlplane_image_digest_uri "${image_uri}")"
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
    }' >"${CONTROLPLANE_IMAGE_PROVENANCE_FILE}"
  chmod 0600 "${CONTROLPLANE_IMAGE_PROVENANCE_FILE}"
  printf '%s\n' "${digest_image_uri}" >"${CONTROLPLANE_IMAGE_URI_FILE}"
  info "controlplane image pushed: ${digest_image_uri}"
  printf '%s\n' "${digest_image_uri}"
}

command=${1:-}
case "${command}" in
  check) check ;;
  bootstrap-init) bootstrap_init ;;
  bootstrap-apply) bootstrap_apply ;;
  bootstrap-output) bootstrap_output ;;
  platform-release-publish) platform_release_publish ;;
  worker-image-init) tf_init "${WORKER_IMAGE_STACK}" ;;
  worker-image-apply) worker_image_apply ;;
  worker-image-start) worker_image_start ;;
  worker-image-wait) shift; worker_image_wait "$@" ;;
  worker-image-amis) worker_image_amis ;;
  worker-image-provenance) worker_image_provenance ;;
  controlplane-image-build) controlplane_image_build ;;
  controlplane-image-push) controlplane_image_push ;;
  -h|--help|help|"") usage ;;
  *) usage >&2; die "unknown command: ${command}" ;;
esac
