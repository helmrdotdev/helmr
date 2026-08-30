#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=release-artifact-contracts.sh
source "${ROOT}/scripts/release-artifact-contracts.sh"
TF_BIN="${TF_BIN:-tofu}"
export AWS_REGION="${AWS_REGION:-us-east-1}"
STATE_REGION="${STATE_REGION:-}"
STATE_KEY="${STATE_KEY:-}"
WORKER_IMAGE_NAME="${WORKER_IMAGE_NAME:-helmr-worker-image}"
WORKER_IMAGE_DISTRIBUTION_REGIONS="${WORKER_IMAGE_DISTRIBUTION_REGIONS:-}"
WORKER_IMAGE_AMI_PUBLIC="${WORKER_IMAGE_AMI_PUBLIC:-}"
WORKER_IMAGE_ROOT_VOLUME_ENCRYPTED="${WORKER_IMAGE_ROOT_VOLUME_ENCRYPTED:-}"
RELEASE_BUILD_NAME="${RELEASE_BUILD_NAME:-helmr-release}"
RELEASE_BUILD_STACK="${RELEASE_BUILD_STACK:-${ROOT}/infra/aws/stacks/release-build}"
WORKER_IMAGE_STACK="${WORKER_IMAGE_STACK:-${ROOT}/infra/aws/stacks/worker-image}"
STATE_DIR="${STATE_DIR:-${ROOT}/.helmr-release-artifacts}"
IMAGE_ARN_FILE="${STATE_DIR}/worker-image-build-version-arn"
WORKER_IMAGE_DEFINITION_FILE="${STATE_DIR}/worker-image-definition.json"
WORKER_IMAGE_RECEIPT_FILE="${STATE_DIR}/worker-image.json"
WORKER_HOST_ARTIFACTS_MANIFEST_FILE="${STATE_DIR}/worker-host-artifacts.json"
WORKER_HOST_BUNDLE_RECEIPT_FILE="${STATE_DIR}/worker-host-bundle.json"
WORKER_RUNTIME_ARTIFACTS_MANIFEST_FILE="${STATE_DIR}/worker-runtime-artifacts.json"
WORKER_RUNTIME_BUNDLE_RECEIPT_FILE="${STATE_DIR}/worker-runtime-bundle.json"
IMAGE_WAIT_INTERVAL_SECONDS="${IMAGE_WAIT_INTERVAL_SECONDS:-60}"
IMAGE_WAIT_TIMEOUT_SECONDS="${IMAGE_WAIT_TIMEOUT_SECONDS:-7200}"

usage() {
  cat <<'EOF'
Usage: scripts/aws-release-artifacts.sh <command>

Commands:
  check
  release-build-init
  release-build-plan
  release-build-apply
  release-build-output
  worker-image-init
  worker-image-apply
  worker-image-start
  worker-image-wait
  worker-image-receipt

This tool builds and publishes Helmr release artifacts.
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

need_state_region() {
  [ -n "${STATE_REGION:-}" ] || die "STATE_REGION is required"
}

tf_init() {
  stack=$1
  need_state_bucket
  backend_args=(
    "-backend-config=bucket=${STATE_BUCKET}"
    "-backend-config=region=${STATE_REGION:-${AWS_REGION}}"
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

release_build_init() {
  need_state_bucket
  need_state_region
  "${TF_BIN}" -chdir="${RELEASE_BUILD_STACK}" init \
    -reconfigure \
    -input=false \
    "-backend-config=bucket=${STATE_BUCKET}" \
    "-backend-config=region=${STATE_REGION}"
}

release_build_plan() {
  release_build_init
  "${TF_BIN}" -chdir="${RELEASE_BUILD_STACK}" plan \
    -input=false \
    -lock=false \
    -var="name=${RELEASE_BUILD_NAME}"
}

release_build_apply() {
  release_build_init
  tf_apply "${RELEASE_BUILD_STACK}" \
    -var="name=${RELEASE_BUILD_NAME}"
}

release_build_output() {
  release_build_init >&2
  artifact_bucket="$("${TF_BIN}" -chdir="${RELEASE_BUILD_STACK}" output -raw release_artifact_bucket_name)"
  printf 'export WORKER_IMAGE_ARTIFACT_BUCKET=%q\n' "${artifact_bucket}"
}

worker_image_artifact_bucket() {
  if [ -n "${WORKER_IMAGE_ARTIFACT_BUCKET:-}" ]; then
    printf '%s\n' "${WORKER_IMAGE_ARTIFACT_BUCKET}"
    return 0
  fi
  if artifact_bucket="$("${TF_BIN}" -chdir="${RELEASE_BUILD_STACK}" output -raw release_artifact_bucket_name 2>/dev/null)"; then
    printf '%s\n' "${artifact_bucket}"
    return 0
  fi
  die "WORKER_IMAGE_ARTIFACT_BUCKET is required; run release-build-apply and export release-build-output"
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

bucket_kms_key_arn() {
  bucket=$1
  aws s3api get-bucket-encryption \
    --region "${STATE_REGION:-${AWS_REGION}}" \
    --bucket "${bucket}" \
    --query 'ServerSideEncryptionConfiguration.Rules[0].ApplyServerSideEncryptionByDefault.KMSMasterKeyID' \
    --output text 2>/dev/null | awk '$0 != "None" { print }'
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
  local bucket bundle_dir bundle_digest bundle_key bundle_status bundle_uri expected_identity host_dir kms_key_arn manifest_digest receipt source_commit work
  mkdir -p "${STATE_DIR}"
  work="$(mktemp -d "${STATE_DIR}/worker-host-bundle.XXXXXX")"
  trap 'rm -rf "${work}"' EXIT
  bundle_dir="${work}/bundle"
  info "building the canonical Worker host artifacts"
  source_commit="$(git -C "${ROOT}" rev-parse HEAD)"
  expected_identity="${RELEASE_TAG} (${source_commit})"
  host_dir="$(HELMR_PLATFORM_VERSION="${RELEASE_TAG}" nix build --impure -L --no-link --print-out-paths "${ROOT}#workerHost")"
  [ "$("${host_dir}/bin/worker" --version)" = "${expected_identity}" ] ||
    die "Worker does not report the release cohort identity"
  nix develop "${ROOT}" -c \
    "${ROOT}/scripts/materialize-worker-host-bundle.sh" "${bundle_dir}" "${host_dir}" >/dev/null
  receipt="${bundle_dir}/worker-host-bundle.json"
  validate_worker_host_bundle_receipt "${receipt}" || die "Worker host bundle receipt is invalid"
  [ "$(jq -r '.sourceCommit' "${receipt}")" = "${source_commit}" ] ||
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

current_worker_image_definition() {
  local source_commit host_source_commit host_bundle_digest host_manifest_digest
  local runtime_source_commit runtime_bundle_digest runtime_manifest_digest
  local component_arn component_definition_digest distribution_configuration_arn distribution_regions
  local image_definition_digest image_pipeline_arn image_recipe_arn prepare_root_digest
  local resolved_parent_image_id root_block_device_mapping ami_public visibility

  validate_worker_host_bundle_receipt "${WORKER_HOST_BUNDLE_RECEIPT_FILE}" ||
    die "Worker host bundle receipt is invalid"
  validate_worker_runtime_bundle_receipt "${WORKER_RUNTIME_BUNDLE_RECEIPT_FILE}" ||
    die "Worker runtime bundle receipt is invalid"
  source_commit="$(git -C "${ROOT}" rev-parse HEAD)"
  host_source_commit="$(jq -r '.sourceCommit' "${WORKER_HOST_BUNDLE_RECEIPT_FILE}")"
  runtime_source_commit="$(jq -r '.sourceCommit' "${WORKER_RUNTIME_BUNDLE_RECEIPT_FILE}")"
  [ "${host_source_commit}" = "${source_commit}" ] ||
    die "Worker host bundle was not produced by the current checkout; run worker-image-apply"
  [ "${runtime_source_commit}" = "${source_commit}" ] ||
    die "Worker runtime bundle was not produced by the current checkout; run worker-image-apply"

  host_bundle_digest="$(jq -r '.bundle.digest' "${WORKER_HOST_BUNDLE_RECEIPT_FILE}")"
  host_manifest_digest="$(jq -r '.manifest.digest' "${WORKER_HOST_BUNDLE_RECEIPT_FILE}")"
  runtime_bundle_digest="$(jq -r '.bundle.digest' "${WORKER_RUNTIME_BUNDLE_RECEIPT_FILE}")"
  runtime_manifest_digest="$(jq -r '.runtimeArtifactsManifest.digest' "${WORKER_RUNTIME_BUNDLE_RECEIPT_FILE}")"
  [ "${host_manifest_digest}" = "sha256:$(sha256_file "${WORKER_HOST_ARTIFACTS_MANIFEST_FILE}")" ] ||
    die "Worker host artifact manifest does not match its bundle receipt"
  [ "${runtime_manifest_digest}" = "sha256:$(sha256_file "${WORKER_RUNTIME_ARTIFACTS_MANIFEST_FILE}")" ] ||
    die "Worker runtime artifact manifest does not match its bundle receipt"

  component_arn="$("${TF_BIN}" -chdir="${WORKER_IMAGE_STACK}" output -raw component_arn)"
  component_definition_digest="$("${TF_BIN}" -chdir="${WORKER_IMAGE_STACK}" output -raw component_definition_digest)"
  distribution_configuration_arn="$("${TF_BIN}" -chdir="${WORKER_IMAGE_STACK}" output -raw distribution_configuration_arn)"
  distribution_regions="$("${TF_BIN}" -chdir="${WORKER_IMAGE_STACK}" output -json distribution_regions | jq -c 'sort | unique')"
  image_definition_digest="$("${TF_BIN}" -chdir="${WORKER_IMAGE_STACK}" output -raw image_definition_digest)"
  image_pipeline_arn="$("${TF_BIN}" -chdir="${WORKER_IMAGE_STACK}" output -raw image_pipeline_arn)"
  image_recipe_arn="$("${TF_BIN}" -chdir="${WORKER_IMAGE_STACK}" output -raw image_recipe_arn)"
  prepare_root_digest="$("${TF_BIN}" -chdir="${WORKER_IMAGE_STACK}" output -raw prepare_root_digest)"
  resolved_parent_image_id="$("${TF_BIN}" -chdir="${WORKER_IMAGE_STACK}" output -raw resolved_parent_image_id)"
  root_block_device_mapping="$("${TF_BIN}" -chdir="${WORKER_IMAGE_STACK}" output -json root_block_device_mapping | jq -cS .)"
  ami_public="$("${TF_BIN}" -chdir="${WORKER_IMAGE_STACK}" output -raw ami_public)"
  case "${ami_public}" in
    true) visibility=public ;;
    false) visibility=private ;;
    *) die "ami_public output must be true or false" ;;
  esac

  jq -cnS \
    --arg component_arn "${component_arn}" \
    --arg component_definition_digest "${component_definition_digest}" \
    --arg distribution_configuration_arn "${distribution_configuration_arn}" \
    --argjson distribution_regions "${distribution_regions}" \
    --arg host_bundle_digest "${host_bundle_digest}" \
    --arg host_manifest_digest "${host_manifest_digest}" \
    --arg host_source_commit "${host_source_commit}" \
    --arg image_definition_digest "${image_definition_digest}" \
    --arg image_pipeline_arn "${image_pipeline_arn}" \
    --arg image_recipe_arn "${image_recipe_arn}" \
    --arg prepare_root_digest "${prepare_root_digest}" \
    --arg resolved_parent_image_id "${resolved_parent_image_id}" \
    --argjson root_block_device_mapping "${root_block_device_mapping}" \
    --arg runtime_bundle_digest "${runtime_bundle_digest}" \
    --arg runtime_manifest_digest "${runtime_manifest_digest}" \
    --arg runtime_source_commit "${runtime_source_commit}" \
    --arg visibility "${visibility}" '
    {
      schema: "helmr.worker-image-definition-state.v0",
      componentARN: $component_arn,
      componentDefinitionDigest: $component_definition_digest,
      distributionConfigurationARN: $distribution_configuration_arn,
      distributionRegions: $distribution_regions,
      imageDefinitionDigest: $image_definition_digest,
      imagePipelineARN: $image_pipeline_arn,
      imageRecipeARN: $image_recipe_arn,
      prepareRootDigest: $prepare_root_digest,
      resolvedParentImageID: $resolved_parent_image_id,
      rootBlockDeviceMapping: $root_block_device_mapping,
      visibility: $visibility,
      hostArtifacts: {
        sourceCommit: $host_source_commit,
        bundleDigest: $host_bundle_digest,
        manifestDigest: $host_manifest_digest
      },
      runtimeArtifacts: {
        sourceCommit: $runtime_source_commit,
        bundleDigest: $runtime_bundle_digest,
        manifestDigest: $runtime_manifest_digest
      }
    }'
}

prepare_worker_runtime_bundle() (
  local artifacts_dir bucket bundle_dir bundle_digest bundle_key bundle_status bundle_uri kms_key_arn manifest_digest receipt work
  mkdir -p "${STATE_DIR}"
  artifacts_dir="${ROOT}/images/guest/out"
  work="$(mktemp -d "${STATE_DIR}/worker-runtime-bundle.XXXXXX")"
  trap 'rm -rf "${work}"' EXIT
  bundle_dir="${work}/bundle"
  info "building the canonical Worker runtime artifacts"
  nix develop "${ROOT}#images" -c make -C "${ROOT}" images >&2
  nix develop "${ROOT}#images" -c \
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
  local definition marker
  [ -n "${RELEASE_TAG:-}" ] || die "RELEASE_TAG is required to publish Worker artifacts"
  require_clean_product_checkout
  mkdir -p "${STATE_DIR}"
  rm -f "${WORKER_IMAGE_DEFINITION_FILE}"
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
  definition_args=(
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
    definition_args+=(-var="host_artifacts_bundle_kms_key_arn=${host_artifacts_bundle_kms_key_arn}")
  fi
  if [ -n "${runtime_artifacts_bundle_kms_key_arn}" ]; then
    definition_args+=(-var="runtime_artifacts_bundle_kms_key_arn=${runtime_artifacts_bundle_kms_key_arn}")
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
  if ((${#definition_args[@]})); then apply_args+=("${definition_args[@]}"); fi
  tf_apply "${WORKER_IMAGE_STACK}" "${apply_args[@]}"
  definition="$(current_worker_image_definition)"
  marker="$(mktemp "${STATE_DIR}/worker-image-definition.XXXXXX")"
  printf '%s\n' "${definition}" >"${marker}"
  validate_worker_image_definition "${marker}" || {
    rm -f "${marker}"
    die "applied Worker image definition is invalid"
  }
  chmod 0600 "${marker}"
  mv "${marker}" "${WORKER_IMAGE_DEFINITION_FILE}"
  info "Worker image definition recorded at ${WORKER_IMAGE_DEFINITION_FILE}"
}

worker_image_receipt_matches_definition() {
  local receipt_file=$1 definition_file=$2
  jq -e --slurpfile definition "${definition_file}" '
    .componentDefinitionDigest == $definition[0].componentDefinitionDigest and
    .imageDefinitionDigest == $definition[0].imageDefinitionDigest and
    .imageRecipeARN == $definition[0].imageRecipeARN and
    .prepareRootDigest == $definition[0].prepareRootDigest and
    .resolvedParentImageID == $definition[0].resolvedParentImageID and
    .hostArtifacts.bundleDigest == $definition[0].hostArtifacts.bundleDigest and
    .hostArtifacts.manifestDigest == $definition[0].hostArtifacts.manifestDigest and
    .runtimeArtifacts.bundleDigest == $definition[0].runtimeArtifacts.bundleDigest and
    .runtimeArtifacts.manifestDigest == $definition[0].runtimeArtifacts.manifestDigest
  ' "${receipt_file}" >/dev/null
}

worker_image_receipt_matches_distribution() {
  local receipt_file=$1 definition_file=$2
  jq -e --slurpfile definition "${definition_file}" '
    .visibility == $definition[0].visibility and
    (.amis | keys) == $definition[0].distributionRegions
  ' "${receipt_file}" >/dev/null
}

require_current_worker_image_definition() {
  local current
  [ -f "${WORKER_IMAGE_DEFINITION_FILE}" ] ||
    die "Worker image definition is missing; run worker-image-apply first"
  validate_worker_image_definition "${WORKER_IMAGE_DEFINITION_FILE}" ||
    die "Worker image definition is malformed; run worker-image-apply"
  current="$(current_worker_image_definition)"
  [ "$(jq -cS . <<<"${current}")" = "$(jq -cS . "${WORKER_IMAGE_DEFINITION_FILE}")" ] ||
    die "Worker image definition is stale; run worker-image-apply"
}

validate_worker_image_receipt_live() {
  local receipt_file=$1 definition_file=$2
  local image_arn image_json recipe_arn recipe_json component_json distribution_arn distribution_json
  local component_arn parent_image root_mapping visibility expected_public expected_regions
  local region ami_id ami_json

  validate_worker_image_receipt "${receipt_file}" || return 1
  worker_image_receipt_matches_definition "${receipt_file}" "${definition_file}" || return 1
  worker_image_receipt_matches_distribution "${receipt_file}" "${definition_file}" || return 1

  image_arn="$(jq -r '.imageBuildVersionARN' "${receipt_file}")"
  recipe_arn="$(jq -r '.imageRecipeARN' "${receipt_file}")"
  component_arn="$(jq -r '.componentARN' "${definition_file}")"
  distribution_arn="$(jq -r '.distributionConfigurationARN' "${definition_file}")"
  parent_image="$(jq -r '.resolvedParentImageID' "${receipt_file}")"
  root_mapping="$(jq -c '.rootBlockDeviceMapping' "${definition_file}")"
  visibility="$(jq -r '.visibility' "${receipt_file}")"
  expected_regions="$(jq -c '.distributionRegions' "${definition_file}")"
  case "${visibility}" in
    public) expected_public=true ;;
    private) expected_public=false ;;
    *) return 1 ;;
  esac

  image_json="$(aws imagebuilder get-image \
    --region "${AWS_REGION}" \
    --image-build-version-arn "${image_arn}" \
    --output json)" || return 1
  jq -e \
    --arg build_arn "${image_arn}" \
    --arg recipe_arn "${recipe_arn}" \
    --argjson expected_amis "$(jq -c '.amis' "${receipt_file}")" '
      ([.image.outputResources.amis[]? | select(.region != null and .image != null) | {key: .region, value: .image}]) as $outputs |
      .image.arn == $build_arn and
      .image.state.status == "AVAILABLE" and
      .image.imageRecipe.arn == $recipe_arn and
      ($outputs | length) == ($expected_amis | length) and
      ($outputs | from_entries) == $expected_amis
    ' >/dev/null <<<"${image_json}" || return 1

  recipe_json="$(aws imagebuilder get-image-recipe \
    --region "${AWS_REGION}" \
    --image-recipe-arn "${recipe_arn}" \
    --output json)" || return 1
  jq -e \
    --arg component_arn "${component_arn}" \
    --arg component_definition_digest "$(jq -r '.componentDefinitionDigest' "${receipt_file}")" \
    --arg image_definition_digest "$(jq -r '.imageDefinitionDigest' "${receipt_file}")" \
    --arg parent_image "${parent_image}" \
    --arg recipe_arn "${recipe_arn}" \
    --argjson root_mapping "${root_mapping}" '
      .imageRecipe.arn == $recipe_arn and
      .imageRecipe.parentImage == $parent_image and
      .imageRecipe.tags.HelmrComponentDefinitionDigest == $component_definition_digest and
      .imageRecipe.tags.HelmrImageDefinitionDigest == $image_definition_digest and
      .imageRecipe.tags.HelmrResolvedParentImageID == $parent_image and
      [.imageRecipe.components[]?.componentArn] == [$component_arn] and
      [.imageRecipe.blockDeviceMappings[]? | {
        deviceName,
        ebs: {
          deleteOnTermination: .ebs.deleteOnTermination,
          encrypted: .ebs.encrypted,
          volumeSize: .ebs.volumeSize,
          volumeType: .ebs.volumeType
        }
      }] == [$root_mapping]
    ' >/dev/null <<<"${recipe_json}" || return 1

  component_json="$(aws imagebuilder get-component \
    --region "${AWS_REGION}" \
    --component-build-version-arn "${component_arn}" \
    --output json)" || return 1
  jq -e \
    --arg component_arn "${component_arn}" \
    --arg component_definition_digest "$(jq -r '.componentDefinitionDigest' "${receipt_file}")" '
      .component.arn == $component_arn and
      .component.tags.HelmrComponentDefinitionDigest == $component_definition_digest
    ' >/dev/null <<<"${component_json}" || return 1

  distribution_json="$(aws imagebuilder get-distribution-configuration \
    --region "${AWS_REGION}" \
    --distribution-configuration-arn "${distribution_arn}" \
    --output json)" || return 1
  jq -e \
    --arg distribution_arn "${distribution_arn}" \
    --argjson expected_public "${expected_public}" \
    --argjson expected_regions "${expected_regions}" '
      .distributionConfiguration.arn == $distribution_arn and
      ([.distributionConfiguration.distributions[]?.region] | sort | unique) == $expected_regions and
      all(.distributionConfiguration.distributions[]?;
        (((.amiDistributionConfiguration.launchPermission.userGroups // []) | index("all")) != null) == $expected_public)
    ' >/dev/null <<<"${distribution_json}" || return 1

  while IFS=$'\t' read -r region ami_id; do
    ami_json="$(aws ec2 describe-images \
      --region "${region}" \
      --owners self \
      --image-ids "${ami_id}" \
      --output json)" || return 1
    jq -e \
      --arg ami_id "${ami_id}" \
      --arg component_definition_digest "$(jq -r '.componentDefinitionDigest' "${receipt_file}")" \
      --arg host_bundle_digest "$(jq -r '.hostArtifacts.bundleDigest' "${receipt_file}")" \
      --arg host_manifest_digest "$(jq -r '.hostArtifacts.manifestDigest' "${receipt_file}")" \
      --arg image_definition_digest "$(jq -r '.imageDefinitionDigest' "${receipt_file}")" \
      --arg parent_image "${parent_image}" \
      --arg prepare_root_digest "$(jq -r '.prepareRootDigest' "${receipt_file}")" \
      --arg runtime_bundle_digest "$(jq -r '.runtimeArtifacts.bundleDigest' "${receipt_file}")" \
      --arg runtime_manifest_digest "$(jq -r '.runtimeArtifacts.manifestDigest' "${receipt_file}")" \
      --argjson expected_public "${expected_public}" '
        (.Images // []) as $images |
        (($images[0].Tags // []) | map({key: .Key, value: .Value}) | from_entries) as $tags |
        ($images | length) == 1 and
        $images[0].ImageId == $ami_id and
        $images[0].State == "available" and
        $images[0].ImageType == "machine" and
        $images[0].Public == $expected_public and
        $tags.HelmrComponentDefinitionDigest == $component_definition_digest and
        $tags.HelmrImageDefinitionDigest == $image_definition_digest and
        $tags.HelmrResolvedParentImageID == $parent_image and
        $tags.HelmrPrepareRootDigest == $prepare_root_digest and
        $tags.HelmrHostBundleDigest == $host_bundle_digest and
        $tags.HelmrHostArtifactsDigest == $host_manifest_digest and
        $tags.HelmrRuntimeBundleDigest == $runtime_bundle_digest and
        $tags.HelmrRuntimeArtifactsDigest == $runtime_manifest_digest
      ' >/dev/null <<<"${ami_json}" || return 1
  done < <(jq -r '.amis | to_entries[] | [.key, .value] | @tsv' "${receipt_file}")
}

worker_image_start() {
  local pipeline_arn token image_arn
  require_clean_product_checkout
  require_current_worker_image_definition
  mkdir -p "${STATE_DIR}"
  if [ -f "${WORKER_IMAGE_RECEIPT_FILE}" ]; then
    validate_worker_image_receipt "${WORKER_IMAGE_RECEIPT_FILE}" ||
      die "existing Worker image receipt is malformed; remove it explicitly before rebuilding"
    if worker_image_receipt_matches_definition "${WORKER_IMAGE_RECEIPT_FILE}" "${WORKER_IMAGE_DEFINITION_FILE}"; then
      if worker_image_receipt_matches_distribution "${WORKER_IMAGE_RECEIPT_FILE}" "${WORKER_IMAGE_DEFINITION_FILE}"; then
        validate_worker_image_receipt_live "${WORKER_IMAGE_RECEIPT_FILE}" "${WORKER_IMAGE_DEFINITION_FILE}" ||
          die "existing Worker image matches the current definition but its live AWS state is invalid"
        image_arn="$(jq -r '.imageBuildVersionARN' "${WORKER_IMAGE_RECEIPT_FILE}")"
        printf '%s\n' "${image_arn}" >"${IMAGE_ARN_FILE}"
        info "reusing fully validated Worker image: ${image_arn}"
        printf '%s\n' "${image_arn}"
        return 0
      fi
      info "Worker image distribution policy changed; starting a new pipeline output"
    else
      info "Worker image definition changed; starting a new pipeline output"
    fi
  fi
  pipeline_arn="$(jq -r '.imagePipelineARN' "${WORKER_IMAGE_DEFINITION_FILE}")"
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
  local image_arn definition image_json status reason deadline ami_ids_json receipt
  mkdir -p "${STATE_DIR}"
  require_clean_product_checkout
  require_current_worker_image_definition
  image_arn="${1:-${WORKER_IMAGE_BUILD_VERSION_ARN:-}}"
  if [ -z "${image_arn}" ] && [ -f "${IMAGE_ARN_FILE}" ]; then
    image_arn="$(cat "${IMAGE_ARN_FILE}")"
  fi
  [ -n "${image_arn}" ] || die "image build version ARN is required; run worker-image-start first"
  definition="$(jq -c . "${WORKER_IMAGE_DEFINITION_FILE}")"

  if [ -f "${WORKER_IMAGE_RECEIPT_FILE}" ]; then
    validate_worker_image_receipt "${WORKER_IMAGE_RECEIPT_FILE}" ||
      die "existing Worker image receipt is malformed"
    if [ "$(jq -r '.imageBuildVersionARN' "${WORKER_IMAGE_RECEIPT_FILE}")" = "${image_arn}" ] &&
      worker_image_receipt_matches_definition "${WORKER_IMAGE_RECEIPT_FILE}" "${WORKER_IMAGE_DEFINITION_FILE}" &&
      worker_image_receipt_matches_distribution "${WORKER_IMAGE_RECEIPT_FILE}" "${WORKER_IMAGE_DEFINITION_FILE}"; then
      validate_worker_image_receipt_live "${WORKER_IMAGE_RECEIPT_FILE}" "${WORKER_IMAGE_DEFINITION_FILE}" ||
        die "reused Worker image no longer matches its live AWS state"
      info "Worker image receipt remains valid: ${WORKER_IMAGE_RECEIPT_FILE}"
      printf '%s\n' "${image_arn}"
      return 0
    fi
  fi

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
            jq -cS '[.image.outputResources.amis[]? | select(.region != null and .image != null) | {key: .region, value: .image}] | from_entries'
        )"
        [ "$(printf '%s\n' "${ami_ids_json}" | jq 'length')" -gt 0 ] || die "image is AVAILABLE but no AMIs were returned"
        [ "$(printf '%s\n' "${image_json}" | jq '[.image.outputResources.amis[]? | select(.region != null and .image != null)] | length')" = "$(jq 'length' <<<"${ami_ids_json}")" ] ||
          die "image is AVAILABLE but contains duplicate regional AMI outputs"
        [ "$(jq -c 'keys' <<<"${ami_ids_json}")" = "$(jq -c '.distributionRegions' <<<"${definition}")" ] ||
          die "available Worker image regions do not match the applied distribution policy"
        receipt="$(mktemp "${STATE_DIR}/worker-image.XXXXXX")"
        jq -cnS \
          --argjson amis "${ami_ids_json}" \
          --arg build_arn "${image_arn}" \
          --argjson definition "${definition}" '
          {
            schema: "helmr.worker-image.v0",
            amis: $amis,
            visibility: $definition.visibility,
            imageBuildVersionARN: $build_arn,
            imageRecipeARN: $definition.imageRecipeARN,
            componentDefinitionDigest: $definition.componentDefinitionDigest,
            imageDefinitionDigest: $definition.imageDefinitionDigest,
            prepareRootDigest: $definition.prepareRootDigest,
            resolvedParentImageID: $definition.resolvedParentImageID,
            hostArtifacts: $definition.hostArtifacts,
            runtimeArtifacts: $definition.runtimeArtifacts
          }' >"${receipt}"
        validate_worker_image_receipt "${receipt}" || {
          rm -f "${receipt}"
          die "Image Builder produced an invalid Worker image receipt"
        }
        validate_worker_image_receipt_live "${receipt}" "${WORKER_IMAGE_DEFINITION_FILE}" || {
          rm -f "${receipt}"
          die "Image Builder output does not close to the applied Worker image definition"
        }
        chmod 0600 "${receipt}"
        mv "${receipt}" "${WORKER_IMAGE_RECEIPT_FILE}"
        info "Worker image receipt recorded at ${WORKER_IMAGE_RECEIPT_FILE}"
        printf '%s\n' "${image_arn}"
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

worker_image_receipt() {
  [ -f "${WORKER_IMAGE_RECEIPT_FILE}" ] || die "Worker image receipt not found; run worker-image-wait first"
  validate_worker_image_receipt "${WORKER_IMAGE_RECEIPT_FILE}" || die "Worker image receipt is invalid"
  jq -c . "${WORKER_IMAGE_RECEIPT_FILE}"
}

command=${1:-}
case "${command}" in
  check) check ;;
  release-build-init) release_build_init ;;
  release-build-plan) release_build_plan ;;
  release-build-apply) release_build_apply ;;
  release-build-output) release_build_output ;;
  worker-image-init) tf_init "${WORKER_IMAGE_STACK}" ;;
  worker-image-apply) worker_image_apply ;;
  worker-image-start) worker_image_start ;;
  worker-image-wait) shift; worker_image_wait "$@" ;;
  worker-image-receipt) worker_image_receipt ;;
  -h|--help|help|"") usage ;;
  *) usage >&2; die "unknown command: ${command}" ;;
esac
