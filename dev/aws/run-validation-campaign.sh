#!/usr/bin/env bash
# shellcheck disable=SC2016
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ROOT="${HELMR_VALIDATION_PRODUCT_ROOT:-${SCRIPT_ROOT}}"
STATE_ROOT="${HELMR_VALIDATION_STATE_ROOT:-${ROOT}/.helmr-validation}"
BOOTSTRAP_STACK="${BOOTSTRAP_STACK:-${ROOT}/infra/aws/modules/bootstrap}"
TF_BIN="${TF_BIN:-tofu}"
AWS_REGION="${AWS_REGION:-us-east-1}"
DEV_TFVARS="${DEV_TFVARS:-${ROOT}/infra/aws/stacks/dev/full-run-smoke.tfvars}"
DEV_STACK="${DEV_STACK:-${ROOT}/infra/aws/stacks/dev}"
EXPECTED_STAGES='["preflight","control_up","awaiting_human","auth_ready","worker_up","workload","pre_shutdown_publish","cleanup","closed","post_shutdown_publish"]'
REQUIRED_CASE_CATEGORIES='["build","run","build_failure_isolation","worker_restart","identity_fencing","queue_preservation","protected_drain","provider_loss","final_zero"]'
TRAP_MANIFEST=""
TRAP_STAGE=""
TRAP_RESULT=""
RUN_OWNER_PID=""
ALLOW_PUBLISH_COMPLETE=0
ALLOW_COLLECTOR_COMPLETE=0
ALLOW_COLLECTOR_STAGE=0
ALLOW_AUTH_COMPLETE=0
ALLOW_WORKLOAD_COMPLETE=0
PRICE_FIXTURE="${PRICE_FIXTURE:-${ROOT}/dev/aws/worker-price-fixture.json}"

usage() {
  cat <<'EOF'
usage: dev/aws/run-validation-campaign.sh COMMAND [ARGS]

Commands:
  validate MANIFEST
  init MANIFEST
  status MANIFEST
  start MANIFEST STAGE
  complete MANIFEST STAGE RESULT_JSON
  run MANIFEST STAGE RESULT_JSON -- COMMAND [ARG...]
  recover MANIFEST
  claim MANIFEST
  publish MANIFEST pre-shutdown|post-shutdown
  run-collect MANIFEST control_up|worker_up|cleanup -- COMMAND [ARG...]
  auth MANIFEST
  close MANIFEST
  workload MANIFEST

The campaign is resumable. Source or manifest drift blocks forward stages but
never blocks evidence publication or cleanup. Commands must write a structured
stage result; unrestricted stdout/stderr is not added to the evidence bundle.
EOF
}

die() {
  printf 'validation campaign: %s\n' "$*" >&2
  exit 1
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || die "missing command: $1"
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

sha256_stdin() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print $1}'
  else
    shasum -a 256 | awk '{print $1}'
  fi
}

sha256_base64_file() {
  need_command python3
  python3 - "$1" <<'PY'
import base64
import hashlib
import pathlib
import sys

print(base64.b64encode(hashlib.sha256(pathlib.Path(sys.argv[1]).read_bytes()).digest()).decode())
PY
}

canonical_sha256() {
  jq -cS . "$1" | sha256_stdin
}

manifest_namespace() {
  jq -er '.evidence.namespace' "$1"
}

campaign_dir() {
  printf '%s/%s\n' "${STATE_ROOT}" "$(manifest_namespace "$1")"
}

state_file() {
  printf '%s/state.json\n' "$(campaign_dir "$1")"
}

ledger_file() {
  printf '%s/ledger.json\n' "$(campaign_dir "$1")"
}

atomic_json_write() {
  local target=$1
  local tmp="${target}.tmp"
  cat >"${tmp}"
  jq -e . "${tmp}" >/dev/null
  chmod 0600 "${tmp}"
  mv "${tmp}" "${target}"
}

validate_manifest() {
  local manifest=$1
  [ -f "${manifest}" ] || die "manifest does not exist: ${manifest}"
  jq -e \
    --argjson expected_stages "${EXPECTED_STAGES}" \
    --argjson required_categories "${REQUIRED_CASE_CATEGORIES}" '
    type == "object" and
    keys == ["artifacts","cost_guard","environment","evidence","governance","harness","retries","schema","source","stages","workload"] and
    .schema == "helmrdotdev.aws-validation-campaign.v1" and
    (.governance | type == "object" and keys == ["repo"] and .repo == "ops") and
    (.source | type == "object" and keys == ["commit","repo"] and .repo == "helmr" and (.commit | test("^[0-9a-f]{40}$"))) and
    (.harness | type == "object" and keys == ["sha256","version"] and .version == 1 and (.sha256 | test("^[0-9a-f]{64}$"))) and
    (.artifacts | type == "object" and has("build_worker_instance_type") and has("control_image_digest") and has("control_image_repository") and
      has("control_tfvars_sha256") and has("worker_ami_id") and has("worker_instance_type") and has("worker_tfvars_sha256") and
      (.control_image_repository | test("^[a-z0-9._/-]+$")) and
      (.control_image_digest | test("^sha256:[0-9a-f]{64}$")) and
      (.control_tfvars_sha256 | test("^[0-9a-f]{64}$")) and (.worker_tfvars_sha256 | test("^[0-9a-f]{64}$")) and
      (.worker_ami_id | test("^ami-[0-9a-f]{8,17}$")) and
      (.worker_instance_type | test("^[a-z][a-z0-9.]+$")) and (.build_worker_instance_type | test("^[a-z][a-z0-9.]+$"))) and
    (.environment | type == "object" and keys == ["account_id_env","dev_name","provider","region","state_key"] and
      .provider == "aws" and .region == "us-east-1" and .account_id_env == "AWS_ACCOUNT_ID" and
      (.dev_name | test("^[a-z][a-z0-9-]{0,31}$")) and
      (.state_key | test("^[A-Za-z0-9._/-]+$") and (contains("..") | not) and (startswith("/") | not))) and
    (.workload | type == "object" and keys == ["cases","environments","fixture_tree","fixtures_root","project"] and
      .fixtures_root == "dev/workflows" and .project == "helmr" and .environments == ["staging","production"] and
      (.fixture_tree | test("^[0-9a-f]{40,64}$")) and
      (.cases | type == "array" and length >= 1 and
        all(.[]; type == "object" and has("category") and has("id") and has("payload") and has("payload_sha256") and has("producer") and has("repetitions") and has("task") and
          (.id | test("^[a-z][a-z0-9-]{0,63}$")) and
          (.category | test("^[a-z][a-z0-9_]{0,63}$")) and
          (.task == null or (.task | type == "string" and test("^[a-z][a-z0-9-]{0,63}$"))) and
          ((.task == null and .payload == null and .payload_sha256 == null) or
           (.task != null and (.payload | type == "object" and (.smokeCase | type == "string" and test("^[a-z][a-z0-9-]{0,63}$"))) and (.payload_sha256 | type == "string" and test("^[0-9a-f]{64}$")))) and
          (.producer | type == "object" and has("path") and has("sha256") and
            (.path | test("^dev/aws/validation-cases/[a-z0-9-]+\\.sh$") and (contains("..") | not)) and
            (.sha256 | test("^[0-9a-f]{64}$"))) and
          (.repetitions | type == "number" and . >= 1 and . <= 10)) and
        ([.[].id] | unique | length) == length)) and
    (.cost_guard | type == "object" and keys == ["build_worker_max","max_bundle_bytes","nat_gateway_max","run_worker_max"] and
      .run_worker_max == 1 and .build_worker_max == 1 and .nat_gateway_max == 1 and
      (.max_bundle_bytes | type == "number" and . >= 1048576 and . <= 52428800)) and
    (.evidence | type == "object" and keys == ["bucket_output","claim_prefix","namespace","prefix","retention_days"] and
      .bucket_output == "source_artifact_bucket_name" and .prefix == "helmr/validation-evidence" and .claim_prefix == "helmr/validation-claims" and
      (.namespace | test("^[a-z][a-z0-9-]{7,95}$")) and .retention_days == 30) and
    (.retries | type == "object" and keys == ["infrastructure_max_attempts","workload_attempts"] and
      (.infrastructure_max_attempts | type == "number" and . >= 1 and . <= 3) and
      (.workload_attempts | type == "number" and . >= 1 and . <= 3)) and
    .stages == $expected_stages
  ' "${manifest}" >/dev/null || die "manifest failed strict schema validation"

  local source_commit fixture_tree harness_sha
  source_commit="$(jq -r '.source.commit' "${manifest}")"
  fixture_tree="$(jq -r '.workload.fixture_tree' "${manifest}")"
  harness_sha="$(jq -r '.harness.sha256' "${manifest}")"
  [ "$(git -C "${ROOT}" rev-parse HEAD)" = "${source_commit}" ] || die "manifest source commit does not match product HEAD"
  [ "$(git -C "${ROOT}" rev-parse "HEAD:dev/workflows")" = "${fixture_tree}" ] || die "manifest fixture tree does not match dev/workflows"
  [ "$(sha256_file "${BASH_SOURCE[0]}")" = "${harness_sha}" ] || die "manifest harness hash does not match this script"

  while IFS= read -r task; do
    [ -z "${task}" ] && continue
    rg -l --glob '*.ts' "id:[[:space:]]*[\"']${task}[\"']" "${ROOT}/dev/workflows/tasks" >/dev/null ||
      die "manifest task does not resolve in dev/workflows: ${task}"
  done < <(jq -r '.workload.cases[].task // empty' "${manifest}" | sort -u)

  local producer_path producer_sha
  while IFS=$'\t' read -r producer_path producer_sha; do
    [ -f "${ROOT}/${producer_path}" ] || die "manifest workload producer does not exist: ${producer_path}"
    [ "$(sha256_file "${ROOT}/${producer_path}")" = "${producer_sha}" ] || die "manifest workload producer hash does not match: ${producer_path}"
    git -C "${ROOT}" cat-file -e "HEAD:${producer_path}" 2>/dev/null || die "manifest workload producer is not committed: ${producer_path}"
  done < <(jq -r '.workload.cases[].producer | [.path,.sha256] | @tsv' "${manifest}" | sort -u)

  local case_count case_index payload_bytes payload_sha expected_payload_sha
  case_count="$(jq '.workload.cases | length' "${manifest}")"
  for ((case_index = 0; case_index < case_count; case_index++)); do
    if [ "$(jq -r ".workload.cases[${case_index}].payload == null" "${manifest}")" = "true" ]; then
      continue
    fi
    payload_bytes="$(jq -cS ".workload.cases[${case_index}].payload" "${manifest}" | wc -c | tr -d ' ')"
    [ "${payload_bytes}" -le 8192 ] || die "manifest case payload exceeds 8192 bytes"
    payload_sha="$(jq -cS ".workload.cases[${case_index}].payload" "${manifest}" | sha256_stdin)"
    expected_payload_sha="$(jq -r ".workload.cases[${case_index}].payload_sha256" "${manifest}")"
    [ "${payload_sha}" = "${expected_payload_sha}" ] || die "manifest case payload hash does not match canonical payload"
  done
}

governance_context() {
  local manifest=$1
  local manifest_dir governance_root relative_path tracked_sha disk_sha
  manifest_dir="$(cd "$(dirname "${manifest}")" && pwd)"
  governance_root="$(git -C "${manifest_dir}" rev-parse --show-toplevel 2>/dev/null)" || die "manifest must be in a Git checkout"
  [ "$(basename "${governance_root}")" = "ops" ] || die "manifest governance checkout must be ops"
  [ -z "$(git -C "${governance_root}" status --porcelain)" ] || die "governance checkout must be clean"
  relative_path="$(python3 -c 'import os, sys; print(os.path.relpath(os.path.realpath(sys.argv[2]), os.path.realpath(sys.argv[1])))' "${governance_root}" "${manifest}")"
  git -C "${governance_root}" cat-file -e "HEAD:${relative_path}" 2>/dev/null || die "manifest must be committed at governance HEAD"
  tracked_sha="$(git -C "${governance_root}" show "HEAD:${relative_path}" | sha256_stdin)"
  disk_sha="$(sha256_file "${manifest}")"
  [ "${tracked_sha}" = "${disk_sha}" ] || die "manifest bytes differ from governance HEAD"
  printf '%s\t%s\t%s\n' "${governance_root}" "$(git -C "${governance_root}" rev-parse HEAD)" "${relative_path}"
}

verify_frozen() {
  local manifest=$1
  local mode=${2:-forward}
  local state original_path original_sha governance_root governance_commit governance_path
  state="$(state_file "${manifest}")"
  [ -f "${state}" ] || die "campaign is not initialized"
  if [ "${mode}" = "recovery" ]; then
    return 0
  fi
  original_path="$(jq -r '.manifest.path' "${state}")"
  original_sha="$(jq -r '.manifest.raw_sha256' "${state}")"
  governance_root="$(jq -r '.governance.root' "${state}")"
  governance_commit="$(jq -r '.governance.commit' "${state}")"
  governance_path="$(jq -r '.governance.path' "${state}")"
  [ -f "${original_path}" ] || die "frozen manifest path is missing"
  [ "$(sha256_file "${original_path}")" = "${original_sha}" ] || die "frozen manifest bytes drifted"
  [ "$(git -C "${ROOT}" rev-parse HEAD)" = "$(jq -r '.source.commit' "${state}")" ] || die "product HEAD drifted"
  [ -z "$(git -C "${ROOT}" status --porcelain)" ] || die "product checkout is dirty"
  [ "$(sha256_file "${BASH_SOURCE[0]}")" = "$(jq -r '.harness.sha256' "${state}")" ] || die "validation harness drifted"
  [ "$(git -C "${governance_root}" rev-parse HEAD)" = "${governance_commit}" ] || die "governance HEAD drifted"
  [ -z "$(git -C "${governance_root}" status --porcelain)" ] || die "governance checkout is dirty"
  [ "$(git -C "${governance_root}" show "HEAD:${governance_path}" | sha256_stdin)" = "${original_sha}" ] ||
    die "governance manifest blob drifted"
}

verify_runtime_context() {
  local manifest=$1 expected_region expected_dev_name expected_state_key
  expected_region="$(jq -r '.environment.region' "${manifest}")"
  expected_dev_name="$(jq -r '.environment.dev_name' "${manifest}")"
  expected_state_key="$(jq -r '.environment.state_key' "${manifest}")"
  [ "${AWS_REGION}" = "${expected_region}" ] || die "AWS_REGION does not match the campaign manifest"
  [ "${DEV_NAME:-}" = "${expected_dev_name}" ] || die "DEV_NAME does not match the campaign manifest"
  [ "${STATE_KEY:-}" = "${expected_state_key}" ] || die "STATE_KEY does not match the campaign manifest"
}

verify_dev_backend() {
  local manifest=$1 metadata expected_key expected_region
  metadata="${DEV_STACK}/.terraform/terraform.tfstate"
  [ -f "${metadata}" ] || die "dev stack backend metadata is missing; initialize the manifest-bound backend first"
  expected_key="$(jq -r '.environment.state_key' "${manifest}")"
  expected_region="$(jq -r '.environment.region' "${manifest}")"
  jq -e --arg key "${expected_key}" --arg region "${expected_region}" '
    .backend.type == "s3" and .backend.config.key == $key and .backend.config.region == $region and
    (.backend.config.bucket | type == "string" and length > 2) and
    ((.backend.config.workspace_key_prefix // "env:") == "env:")
  ' "${metadata}" >/dev/null || die "initialized dev backend does not match the campaign state key and region"
  [ "$("${TF_BIN}" -chdir="${DEV_STACK}" workspace show)" = "default" ] || die "validation campaign requires the default OpenTofu workspace"
}

tfvar_raw() {
  local key=$1
  [ -f "${DEV_TFVARS}" ] || die "dev tfvars do not exist: ${DEV_TFVARS}"
  awk -v key="${key}" '
    $0 ~ "^[[:space:]]*" key "[[:space:]]*=" {
      sub("^[[:space:]]*" key "[[:space:]]*=[[:space:]]*", "")
      sub(/[[:space:]]*#.*/, "")
      print
      found=1
      exit
    }
    END { if (!found) exit 1 }
  ' "${DEV_TFVARS}" || die "missing tfvar: ${key}"
}

verify_control_cost_guard() {
  [ "$(sha256_file "${DEV_TFVARS}")" = "$(jq -r '.artifacts.control_tfvars_sha256' "$1")" ] || die "control tfvars differ from the frozen campaign configuration"
  [ "$(tfvar_raw name | jq -r .)" = "$(jq -r '.environment.dev_name' "$1")" ] || die "tfvars stack name differs from the campaign dev name"
  [ "$(tfvar_raw aws_region | jq -r .)" = "$(jq -r '.environment.region' "$1")" ] || die "tfvars region differs from the campaign region"
  [ "$(tfvar_raw create_worker)" = "false" ] || die "control stage must not create workers"
  [ "$(tfvar_raw enable_nat_gateway)" = "false" ] || die "control stage must not create a NAT gateway"
}

verify_worker_cost_guard() {
  local manifest=$1 controller run_max build_max nat_max actual_image normalized_image expected_image build_instance
  [ "$(sha256_file "${DEV_TFVARS}")" = "$(jq -r '.artifacts.worker_tfvars_sha256' "${manifest}")" ] || die "worker tfvars differ from the frozen campaign configuration"
  [ "$(tfvar_raw name | jq -r .)" = "$(jq -r '.environment.dev_name' "${manifest}")" ] || die "tfvars stack name differs from the campaign dev name"
  [ "$(tfvar_raw aws_region | jq -r .)" = "$(jq -r '.environment.region' "${manifest}")" ] || die "tfvars region differs from the campaign region"
  run_max="$(jq -r '.cost_guard.run_worker_max' "${manifest}")"
  build_max="$(jq -r '.cost_guard.build_worker_max' "${manifest}")"
  nat_max="$(jq -r '.cost_guard.nat_gateway_max' "${manifest}")"
  [ "$(tfvar_raw create_worker)" = "true" ] || die "worker stage must create worker infrastructure"
  [ "$(tfvar_raw worker_max_size)" -le "${run_max}" ] || die "run worker ASG exceeds campaign ceiling"
  [ "$(tfvar_raw build_worker_max_size)" -le "${build_max}" ] || die "build worker ASG exceeds campaign ceiling"
  if [ "$(tfvar_raw enable_nat_gateway)" = "true" ]; then
    [ "${nat_max}" -ge 1 ] || die "NAT gateway exceeds campaign ceiling"
  fi
  actual_image="$(tfvar_raw control_image | jq -r .)"
  normalized_image="$(printf '%s' "${actual_image}" | sed -E 's#^[0-9]{12}\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com/##')"
  expected_image="$(jq -r '.artifacts.control_image_repository + "@" + .artifacts.control_image_digest' "${manifest}")"
  [ "${normalized_image}" = "${expected_image}" ] || die "control image differs from campaign manifest"
  [ "$(tfvar_raw worker_ami_id | jq -r .)" = "$(jq -r '.artifacts.worker_ami_id' "${manifest}")" ] || die "worker AMI differs from campaign manifest"
  [ "$(tfvar_raw worker_instance_type | jq -r .)" = "$(jq -r '.artifacts.worker_instance_type' "${manifest}")" ] || die "worker instance type differs from campaign manifest"
  build_instance="$(tfvar_raw build_worker_instance_type | jq -r .)"
  [ "${build_instance}" != "null" ] || build_instance="$(tfvar_raw worker_instance_type | jq -r .)"
  [ "${build_instance}" = "$(jq -r '.artifacts.build_worker_instance_type' "${manifest}")" ] || die "build worker instance type differs from campaign manifest"
  controller="$(tfvar_raw worker_fleet_controller)"
  jq -e --argjson run_max "${run_max}" --argjson build_max "${build_max}" '
    .run_warm_workers == 0 and .build_warm_workers == 0 and
    .run_max_workers <= $run_max and .build_max_workers <= $build_max and
    .max_scale_out_per_cycle <= ($run_max + $build_max) and
    .max_pending_workers <= ($run_max + $build_max) and .emergency_stop == false
  ' <<<"${controller}" >/dev/null || die "fleet controller exceeds campaign cost guard"
}

verify_aws_identity() {
  local manifest=$1 account_env expected_account actual_account
  verify_runtime_context "${manifest}"
  account_env="$(jq -r '.environment.account_id_env' "${manifest}")"
  expected_account="${!account_env:-}"
  [[ "${expected_account}" =~ ^[0-9]{12}$ ]] || die "${account_env} must contain the expected 12-digit AWS account id"
  actual_account="$(aws sts get-caller-identity --query Account --output text)"
  [ "${actual_account}" = "${expected_account}" ] || die "active AWS account does not match the private campaign expectation"
}

append_ledger() {
  local manifest=$1 event=$2 stage=$3 status=$4 detail=${5:-null}
  local ledger tmp
  ledger="$(ledger_file "${manifest}")"
  tmp="${ledger}.tmp"
  jq \
    --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg event "${event}" \
    --arg stage "${stage}" \
    --arg status "${status}" \
    --argjson detail "${detail}" \
    '. + [{at:$at,event:$event,stage:$stage,status:$status,detail:$detail}]' \
    "${ledger}" >"${tmp}"
  chmod 0600 "${tmp}"
  mv "${tmp}" "${ledger}"
}

set_state() {
  local manifest=$1 filter=$2
  local state tmp
  shift 2
  state="$(state_file "${manifest}")"
  tmp="${state}.tmp"
  jq "$@" "${filter}" "${state}" >"${tmp}"
  chmod 0600 "${tmp}"
  mv "${tmp}" "${state}"
}

init_campaign() {
  local manifest=$1 namespace dir governance_root governance_commit governance_path
  local manifest_abs manifest_raw manifest_canonical
  manifest_abs="$(cd "$(dirname "${manifest}")" && pwd)/$(basename "${manifest}")"
  validate_manifest "${manifest_abs}"
  [ -z "$(git -C "${ROOT}" status --porcelain)" ] || die "product checkout must be clean"
  IFS=$'\t' read -r governance_root governance_commit governance_path < <(governance_context "${manifest_abs}")
  namespace="$(manifest_namespace "${manifest_abs}")"
  dir="${STATE_ROOT}/${namespace}"
  [ ! -e "${dir}" ] || die "campaign namespace already exists locally"
  umask 077
  mkdir -p "${dir}/results" "${dir}/publishes"
  chmod 0700 "${STATE_ROOT}" "${dir}" "${dir}/results" "${dir}/publishes"
  cp "${manifest_abs}" "${dir}/manifest.json"
  chmod 0600 "${dir}/manifest.json"
  manifest_raw="$(sha256_file "${manifest_abs}")"
  manifest_canonical="$(canonical_sha256 "${manifest_abs}")"
  jq -n \
    --arg namespace "${namespace}" \
    --arg path "${manifest_abs}" \
    --arg raw_sha "${manifest_raw}" \
    --arg canonical_sha "${manifest_canonical}" \
    --arg source_commit "$(jq -r '.source.commit' "${manifest_abs}")" \
    --arg harness_sha "$(jq -r '.harness.sha256' "${manifest_abs}")" \
    --arg governance_root "${governance_root}" \
    --arg governance_commit "${governance_commit}" \
    --arg governance_path "${governance_path}" \
    '{schema:"helmrdotdev.aws-validation-campaign-state.v1",namespace:$namespace,status:"ready",verdict:"pending",next_stage_index:0,running_stage:null,running_pid:null,claim:null,deployment:null,manifest:{path:$path,raw_sha256:$raw_sha,canonical_sha256:$canonical_sha},source:{commit:$source_commit},harness:{sha256:$harness_sha},governance:{root:$governance_root,commit:$governance_commit,path:$governance_path}}' |
    atomic_json_write "${dir}/state.json"
  printf '[]\n' | atomic_json_write "${dir}/ledger.json"
  append_ledger "${manifest_abs}" initialized preflight ready
  printf 'campaign_namespace=%s\n' "${namespace}"
  printf 'manifest_sha256=%s\n' "${manifest_raw}"
}

stage_index() {
  jq -nr --argjson stages "${EXPECTED_STAGES}" --arg stage "$1" '$stages | index($stage) // -1'
}

with_campaign_lock() {
  local manifest=$1
  shift
  local lock owner=""
  lock="$(campaign_dir "${manifest}")/.lock"
  if ! mkdir "${lock}" 2>/dev/null; then
    [ -f "${lock}/pid" ] && owner="$(cat "${lock}/pid")"
    if [[ "${owner}" =~ ^[1-9][0-9]*$ ]] && kill -0 "${owner}" 2>/dev/null; then
      die "campaign state is locked by another process"
    fi
    rm -f "${lock}/pid"
    rmdir "${lock}" 2>/dev/null || die "campaign state lock is not recoverable"
    mkdir "${lock}" 2>/dev/null || die "campaign state is locked by another process"
  fi
  printf '%s\n' "$$" >"${lock}/pid"
  (
    trap 'rm -f "${lock}/pid"; rmdir "${lock}" 2>/dev/null || true' EXIT
    "$@"
  )
}

start_stage_unlocked() {
  local manifest=$1 stage=$2 state expected index mode=forward attempt max_attempts contract
  state="$(state_file "${manifest}")"
  [ -f "${state}" ] || die "campaign is not initialized"
  index="$(stage_index "${stage}")"
  [ "${index}" -ge 0 ] || die "unknown stage: ${stage}"
  case "${stage}" in
    control_up|worker_up|cleanup|closed)
      [ "${ALLOW_COLLECTOR_STAGE}" = "1" ] || die "${stage} must be executed by a harness-owned collector command"
      ;;
    auth_ready)
      [ "${ALLOW_AUTH_COMPLETE}" = "1" ] || die "auth_ready must be executed by the harness-owned auth command"
      ;;
    workload)
      [ "${ALLOW_WORKLOAD_COMPLETE}" = "1" ] || die "workload must be executed by the harness-owned workload command"
      ;;
  esac
  case "${stage}" in
    cleanup|closed|pre_shutdown_publish|post_shutdown_publish) mode=recovery ;;
  esac
  verify_frozen "${manifest}" "${mode}"
  contract="$(campaign_dir "${manifest}")/manifest.json"
  if [ "${stage}" != "preflight" ]; then
    verify_runtime_context "${contract}"
  fi
  case "${stage}" in
    preflight|control_up|awaiting_human|auth_ready|worker_up|workload)
      [ "$(jq -r '.claim != null' "${state}")" = "true" ] || die "evidence namespace must be claimed before ${stage}"
      ;;
  esac
  case "${stage}" in
    control_up|worker_up|workload|cleanup)
      verify_aws_identity "${contract}"
      ;;
  esac
  case "${stage}" in
    control_up) verify_control_cost_guard "${contract}" ;;
    worker_up|workload) verify_worker_cost_guard "${contract}" ;;
  esac
  [ "$(jq -r '.running_stage // empty' "${state}")" = "" ] || die "another stage is already running"
  attempt="$(( $(jq --arg stage "${stage}" '[.[] | select(.event == "started" and .stage == $stage)] | length' "$(ledger_file "${manifest}")") + 1 ))"
  if [ "${stage}" != "workload" ] && [ "${stage}" != "cleanup" ] && [ "${stage}" != "closed" ]; then
    max_attempts="$(jq -r '.retries.infrastructure_max_attempts' "${contract}")"
    [ "${attempt}" -le "${max_attempts}" ] || die "stage ${stage} exceeds the infrastructure retry contract"
  fi
  expected="$(jq -r --argjson stages "${EXPECTED_STAGES}" '.next_stage_index as $i | $stages[$i]' "${state}")"
  if [ "${stage}" = "cleanup" ]; then
    [ "$(jq -r '.status' "${state}")" != "closed" ] || die "campaign is already closed"
    if [ "${stage}" != "${expected}" ]; then
      set_state "${manifest}" '.verdict="failed"'
    fi
  elif [ "${stage}" = "pre_shutdown_publish" ] && [ "$(jq -r '.claim != null' "${state}")" = "true" ]; then
    [ "$(jq -r '.status' "${state}")" != "closed" ] || die "campaign is already closed"
    if [ "${stage}" != "${expected}" ]; then
      set_state "${manifest}" '.verdict="failed"'
    fi
  else
    [ "${stage}" = "${expected}" ] || die "expected stage ${expected}, got ${stage}"
  fi
  set_state "${manifest}" '.running_stage=$stage | .running_pid=$pid | .status="running"' --arg stage "${stage}" --argjson pid "${RUN_OWNER_PID:-null}"
  append_ledger "${manifest}" started "${stage}" running "$(jq -cn --argjson attempt "${attempt}" '{attempt:$attempt}')"
}

start_stage() {
  local manifest=$1
  shift
  with_campaign_lock "${manifest}" start_stage_unlocked "${manifest}" "$@"
}

validate_stage_result() {
  local manifest=$1 stage=$2 result=$3 mode=${4:-runtime}
  jq -e --arg stage "${stage}" '
    type == "object" and
    keys == ["cases","observations","reason","schema","stage","status"] and
    .schema == "helmrdotdev.validation-stage-result.v1" and .stage == $stage and
    (.status == "passed" or .status == "failed") and
    ((.status == "passed" and .reason == null) or
     (.status == "failed" and (.reason | type == "string" and test("^[a-z0-9._-]{1,80}$")))) and
    (.observations | type == "object") and
    (.cases | type == "array") and
    (if .status == "failed" and .stage != "workload" then .observations == {} and .cases == [] else true end)
  ' "${result}" >/dev/null || die "invalid structured result for stage ${stage}"
  if [ "$(jq -r '.status' "${result}")" = "failed" ] && [ "${stage}" != "workload" ]; then
    return 0
  fi
  case "${stage}" in
    preflight|awaiting_human)
      jq -e '.observations == {} and .cases == []' "${result}" >/dev/null || die "${stage} result has disallowed observations"
      ;;
    auth_ready)
      jq -e '
        .cases == [] and (.observations | keys == ["authenticated_cli_probe","environment_slugs","exit_code","project_slug"]) and
        .observations.project_slug == "helmr" and .observations.environment_slugs == ["staging","production"] and
        .observations.authenticated_cli_probe == true and .observations.exit_code == 0
      ' "${result}" >/dev/null || die "auth_ready result has disallowed observations"
      ;;
    control_up)
      jq -e --arg image "$(jq -r '.artifacts.control_image_repository + "@" + .artifacts.control_image_digest' "${manifest}")" '
        .cases == [] and (.observations | has("control_image") and has("services") and has("rds") and has("valkey")) and
        .observations.control_image == $image and
        (.observations.collected_at | fromdateiso8601 | type == "number") and
        (.observations.services | has("control") and has("dispatcher")) and all(.observations.services[]; .desired == 1 and .running == 1 and .pending == 0 and .rollout == "COMPLETED") and
        .observations.nat_gateway_count == 0 and .observations.run_worker_count == 0 and .observations.build_worker_count == 0 and
        (.observations.rds | has("id") and has("status")) and .observations.rds.status == "available" and
        (.observations.rds.id | test("^[a-z][a-z0-9-]{0,62}$")) and (.observations.rds.created_at | fromdateiso8601 | type == "number") and
        (.observations.valkey | has("id") and has("status") and has("node_count")) and .observations.valkey.status == "available" and
        (.observations.valkey.id | test("^[a-z][a-z0-9-]{0,39}$")) and (.observations.valkey.node_count >= 1) and
        (.observations.valkey.created_at | fromdateiso8601 | type == "number")
      ' "${result}" >/dev/null || die "control_up result violates the strict inventory schema"
      [ "${mode}" = "evidence" ] || verify_control_cost_guard "${manifest}"
      ;;
    worker_up)
      jq -e --arg ami "$(jq -r '.artifacts.worker_ami_id' "${manifest}")" --arg instance "$(jq -r '.artifacts.worker_instance_type' "${manifest}")" --arg build_instance "$(jq -r '.artifacts.build_worker_instance_type' "${manifest}")" \
        --argjson run_max "$(jq -r '.cost_guard.run_worker_max' "${manifest}")" --argjson build_max "$(jq -r '.cost_guard.build_worker_max' "${manifest}")" \
        --arg price_sha "$(canonical_sha256 "${PRICE_FIXTURE}")" --argjson worker_hourly "$(jq '.microusd_per_hour' "${PRICE_FIXTURE}")" '
        .cases == [] and (.observations | has("build") and has("nat") and has("pricing") and has("run") and has("worker_ami_id")) and
        .observations.worker_ami_id == $ami and (.observations.collected_at | fromdateiso8601 | type == "number") and
        all([.observations.run,.observations.build][];
          (has("asg") and has("desired") and has("instance_type") and has("instances") and has("launch_template_id") and has("max") and has("min")) and
          (.created_at | fromdateiso8601 | type == "number") and
          .min == 0 and .desired >= 0 and .desired <= .max and .lifecycle_hooks == true and .scale_in_protected == true and
          (.launch_template_id | test("^lt-[0-9a-f]{8,17}$")) and (.launch_template_version >= 1) and
          all(.instances[]; test("^i-[0-9a-f]{8,17}$"))) and
        .observations.run.instance_type == $instance and .observations.build.instance_type == $build_instance and
        .observations.run.max <= $run_max and .observations.build.max <= $build_max and
        (.observations.nat | has("id") and has("status")) and .observations.nat.status == "available" and
        (.observations.nat.id | test("^nat-[0-9a-f]{8,17}$")) and (.observations.nat.created_at | fromdateiso8601 | type == "number") and
        (.observations.pricing | has("provider") and has("region") and has("currency") and has("fixture_sha256") and has("worker_microusd_per_hour")) and
        .observations.pricing.provider == "aws" and .observations.pricing.region == "us-east-1" and .observations.pricing.currency == "USD" and
        .observations.pricing.fixture_sha256 == $price_sha and .observations.pricing.worker_microusd_per_hour == $worker_hourly and
        (.observations.pricing.effective_at | fromdateiso8601 | type == "number") and
        (.observations.pricing.queried_at | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z|[+-][0-9]{2}:[0-9]{2})$")) and (.observations.pricing.worker_microusd_per_hour > 0) and
        (.observations.rds_instance_id | test("^[a-z][a-z0-9-]{0,62}$")) and (.observations.valkey_replication_group_id | test("^[a-z][a-z0-9-]{0,39}$"))
      ' "${result}" >/dev/null || die "worker_up result violates the immutable artifact or cost contract"
      [ "${mode}" = "evidence" ] || verify_worker_cost_guard "${manifest}"
      ;;
    workload)
    jq -e --slurpfile manifest "${manifest}" '
      (((.status == "passed" and .reason == null) or (.status == "failed" and .reason == "workload_case_failed")) and
        (.observations | keys == ["build_worker_peak","finished_at","nat_bytes_in_from_destination","nat_bytes_out_to_destination","run_worker_peak","started_at","worker_observed_intervals"]) and
        (.observations.started_at | fromdateiso8601 | type == "number") and (.observations.finished_at | fromdateiso8601 | type == "number") and
        (.observations.run_worker_peak | type == "number" and . >= 0 and floor == .) and
        (.observations.build_worker_peak | type == "number" and . >= 0 and floor == .) and
        (.observations.nat_bytes_in_from_destination | type == "number" and . >= 0 and floor == .) and
        (.observations.nat_bytes_out_to_destination | type == "number" and . >= 0 and floor == .) and
        (.observations.worker_observed_intervals | type == "array" and length <= 20 and all(.[];
          keys == ["first_seen","id","last_seen","role"] and (.id | test("^i-[0-9a-f]{8,17}$")) and (.role == "run" or .role == "build") and
          (.first_seen | fromdateiso8601 | type == "number") and (.last_seen | fromdateiso8601 | type == "number"))) and
        .observations.run_worker_peak <= $manifest[0].cost_guard.run_worker_max and
        .observations.build_worker_peak <= $manifest[0].cost_guard.build_worker_max) and
      ([.cases[].id] | unique | sort) == ([$manifest[0].workload.cases[].id] | sort) and
      ([.cases[].id] | length) == ([$manifest[0].workload.cases[].id] | length) and
      all(.cases[]; . as $result_case |
        (keys == ["attempts","id","status"]) and
        (.status == "passed" or .status == "failed") and
        (.id as $id | $manifest[0].workload.cases[] | select(.id == $id) as $case |
          ($result_case.attempts | type == "array" and length == $case.repetitions and
            ([.[].index] == [range(1; length + 1)]) and
            all(.[]; keys == ["evidence_sha256","index","producer_sha256","reason","status"] and
              (.index | type == "number") and (.status == "passed" or .status == "failed") and
              ((.status == "passed" and .reason == null) or (.status == "failed" and (.reason | test("^[a-z0-9._-]{1,80}$")))) and
              (.evidence_sha256 | test("^[0-9a-f]{64}$")) and .producer_sha256 == $case.producer.sha256)) and
          ($result_case.status == (if all($result_case.attempts[]; .status == "passed") then "passed" else "failed" end)))) and
      (.status == (if all(.cases[]; .status == "passed") then "passed" else "failed" end))
    ' "${result}" >/dev/null || die "workload result does not cover the exact manifest case set"
      [ "${mode}" = "evidence" ] || verify_worker_cost_guard "${manifest}"
      ;;
    pre_shutdown_publish|post_shutdown_publish)
      jq -e --arg stage "${stage}" --arg namespace "$(manifest_namespace "${manifest}")" '
        .observations as $o |
        .cases == [] and ($o | keys == ["bytes","checkpoint","checksum_sha256","logical_key","sha256","version_id"]) and
        $o.checkpoint == (if $stage == "pre_shutdown_publish" then "pre-shutdown" else "post-shutdown" end) and
        ($o.logical_key | startswith("helmr/validation-evidence/" + $namespace + "/" + $o.checkpoint + "/")) and
        ($o.logical_key | endswith("/" + $o.sha256 + ".tar.gz")) and
        (.observations.sha256 | test("^[0-9a-f]{64}$")) and (.observations.checksum_sha256 | test("^[A-Za-z0-9+/]{43}=$")) and
        (.observations.version_id | type == "string" and length >= 1 and length <= 1024) and
        (.observations.bytes | type == "number" and . >= 1)
      ' "${result}" >/dev/null || die "publish result has disallowed observations"
      ;;
    cleanup)
      jq -e '
        .cases == [] and (.observations | keys == ["counts","observed_at"]) and
        (.observations.observed_at | fromdateiso8601 | type == "number") and
        (.observations.counts | has("active_nat_gateways") and has("build_workers") and has("ecs_services") and has("rds_instances") and has("run_workers") and has("terraform_resources")) and
        all((.observations.counts | del(.tagged_resources))[]; . == 0)
      ' "${result}" >/dev/null || die "cleanup result must prove a zero-resource inventory"
      ;;
    closed)
      jq -e '.cases == [] and (.observations | keys == ["verdict","zero_resources"]) and (.observations.verdict == "passed" or .observations.verdict == "failed") and .observations.zero_resources == true' "${result}" >/dev/null || die "closed result has disallowed observations"
      ;;
  esac
}

complete_stage_unlocked() {
  local manifest=$1 stage=$2 result=$3 state index result_copy status next_index prepublish_index cleanup_index closed_index postpublish_index contract
  state="$(state_file "${manifest}")"
  contract="$(campaign_dir "${manifest}")/manifest.json"
  case "${stage}" in
    pre_shutdown_publish|post_shutdown_publish)
      [ "${ALLOW_PUBLISH_COMPLETE}" = "1" ] || die "publish stages can only be completed by the publish command"
      ;;
    control_up|worker_up|cleanup|closed)
      [ "${ALLOW_COLLECTOR_COMPLETE}" = "1" ] || die "${stage} can only be completed by a harness-owned inventory collector"
      ;;
    auth_ready)
      [ "${ALLOW_AUTH_COMPLETE}" = "1" ] || die "auth_ready can only be completed by the harness-owned auth command"
      ;;
    workload)
      [ "${ALLOW_WORKLOAD_COMPLETE}" = "1" ] || die "workload can only be completed by the harness-owned workload command"
      ;;
  esac
  [ "$(jq -r '.running_stage // empty' "${state}")" = "${stage}" ] || die "stage ${stage} is not running"
  [ -f "${result}" ] || die "structured result is missing: ${result}"
  validate_stage_result "${contract}" "${stage}" "${result}"
  if [ "${stage}" = "worker_up" ] && [ "$(jq -r '.status' "${result}")" = "passed" ]; then
    jq -e --slurpfile state "${state}" '
      .observations.rds_instance_id == $state[0].deployment.rds_instance_id and
      .observations.valkey_replication_group_id == $state[0].deployment.valkey_replication_group_id
    ' "${result}" >/dev/null || die "worker stage changed the frozen RDS or Valkey deployment tuple"
  fi
  if [ "$(jq -r '.status' "${result}")" = "passed" ]; then
    case "${stage}" in
      pre_shutdown_publish|cleanup|closed|post_shutdown_publish) ;;
      *)
        if ! (verify_frozen "${manifest}" forward); then
          jq -n --arg stage "${stage}" '{schema:"helmrdotdev.validation-stage-result.v1",stage:$stage,status:"failed",reason:"source_drift",observations:{},cases:[]}' >"${result}"
        fi
        ;;
    esac
  fi
  index="$(stage_index "${stage}")"
  result_copy="$(campaign_dir "${manifest}")/results/$(printf '%02d' "${index}")-${stage}.json"
  cp "${result}" "${result_copy}"
  chmod 0600 "${result_copy}"
  status="$(jq -r '.status' "${result}")"
  if [ "${status}" = "passed" ]; then
    next_index=$((index + 1))
    if [ "${stage}" = "control_up" ]; then
      set_state "${manifest}" '.deployment={control_image:$o.control_image,rds_instance_id:$o.rds.id,valkey_replication_group_id:$o.valkey.id}' --argjson o "$(jq -c '.observations' "${result}")"
    elif [ "${stage}" = "worker_up" ]; then
      set_state "${manifest}" '.deployment += {worker_ami_id:$o.worker_ami_id,run_worker_instance_type:$o.run.instance_type,build_worker_instance_type:$o.build.instance_type,run_launch_template_id:$o.run.launch_template_id,run_launch_template_version:$o.run.launch_template_version,build_launch_template_id:$o.build.launch_template_id,build_launch_template_version:$o.build.launch_template_version}' --argjson o "$(jq -c '.observations' "${result}")"
    fi
    if [ "${stage}" = "closed" ]; then
      [ "$(jq -r '.observations.verdict' "${result}")" = "$(jq -r 'if .verdict == "pending" then "passed" else .verdict end' "${state}")" ] || die "closed verdict differs from campaign state"
      set_state "${manifest}" '.running_stage=null | .running_pid=null | .status="ready" | .verdict=(if .verdict == "pending" then "passed" else .verdict end) | .next_stage_index=$next' --argjson next "${next_index}"
    elif [ "${stage}" = "post_shutdown_publish" ]; then
      set_state "${manifest}" '.running_stage=null | .running_pid=null | .status="closed" | .next_stage_index=$next' --argjson next "${next_index}"
    elif [ "${stage}" = "workload" ]; then
      set_state "${manifest}" '.running_stage=null | .running_pid=null | .status="ready" | .verdict=(if .verdict == "pending" then "passed" else .verdict end) | .next_stage_index=$next' --argjson next "${next_index}"
    else
      set_state "${manifest}" '.running_stage=null | .running_pid=null | .status="ready" | .next_stage_index=$next' --argjson next "${next_index}"
    fi
    append_ledger "${manifest}" completed "${stage}" passed
  else
    prepublish_index="$(stage_index pre_shutdown_publish)"
    cleanup_index="$(stage_index cleanup)"
    closed_index="$(stage_index closed)"
    postpublish_index="$(stage_index post_shutdown_publish)"
    if [ "${index}" -lt "${prepublish_index}" ]; then
      next_index="${prepublish_index}"
    elif [ "${stage}" = "pre_shutdown_publish" ]; then
      next_index="${cleanup_index}"
    elif [ "${stage}" = "cleanup" ]; then
      next_index="${cleanup_index}"
    elif [ "${stage}" = "closed" ]; then
      next_index="${closed_index}"
    else
      next_index="${postpublish_index}"
    fi
    if [ "${stage}" = "post_shutdown_publish" ]; then
      set_state "${manifest}" '.running_stage=null | .running_pid=null | .status="cleanup_required" | .next_stage_index=$next' --argjson next "${next_index}"
    else
      set_state "${manifest}" '.running_stage=null | .running_pid=null | .status="cleanup_required" | .verdict="failed" | .next_stage_index=$next' --argjson next "${next_index}"
    fi
    append_ledger "${manifest}" completed "${stage}" failed "$(jq -c '{reason:.reason}' "${result}")"
  fi
}

complete_stage() {
  local manifest=$1
  shift
  with_campaign_lock "${manifest}" complete_stage_unlocked "${manifest}" "$@"
}

record_interruption() {
  local manifest=$1 stage=$2 result=$3 signal=$4 reason
  trap - INT TERM HUP
  reason="signal_$(printf '%s' "${signal}" | tr '[:upper:]' '[:lower:]')"
  jq -n --arg stage "${stage}" --arg reason "${reason}" \
    '{schema:"helmrdotdev.validation-stage-result.v1",stage:$stage,status:"failed",reason:$reason,observations:{},cases:[]}' >"${result}"
  case "${stage}" in control_up|worker_up|cleanup|closed) ALLOW_COLLECTOR_COMPLETE=1 ;; esac
  complete_stage "${manifest}" "${stage}" "${result}" || true
  exit 130
}

run_stage() {
  local manifest=$1 stage=$2 result=$3 contract
  shift 3
  [ "${1:-}" = "--" ] || die "run requires -- before the command"
  shift
  [ "$#" -gt 0 ] || die "run requires a command"
  RUN_OWNER_PID="$$"
  start_stage "${manifest}" "${stage}"
  TRAP_MANIFEST="${manifest}"
  TRAP_STAGE="${stage}"
  TRAP_RESULT="${result}"
  trap 'record_interruption "$TRAP_MANIFEST" "$TRAP_STAGE" "$TRAP_RESULT" INT' INT
  trap 'record_interruption "$TRAP_MANIFEST" "$TRAP_STAGE" "$TRAP_RESULT" TERM' TERM
  trap 'record_interruption "$TRAP_MANIFEST" "$TRAP_STAGE" "$TRAP_RESULT" HUP' HUP
  set +e
  "$@"
  local command_status=$?
  set -e
  trap - INT TERM HUP
  if [ ! -f "${result}" ]; then
    jq -n --arg stage "${stage}" \
      '{schema:"helmrdotdev.validation-stage-result.v1",stage:$stage,status:"failed",reason:"command_result_missing",observations:{},cases:[]}' >"${result}"
  elif [ "${command_status}" != "0" ] && [ "$(jq -r '.status // empty' "${result}" 2>/dev/null || true)" = "passed" ]; then
    jq -n --arg stage "${stage}" \
      '{schema:"helmrdotdev.validation-stage-result.v1",stage:$stage,status:"failed",reason:"command_result_conflict",observations:{},cases:[]}' >"${result}"
  fi
  contract="$(campaign_dir "${manifest}")/manifest.json"
  if ! (validate_stage_result "${contract}" "${stage}" "${result}" evidence); then
    jq -n --arg stage "${stage}" '{schema:"helmrdotdev.validation-stage-result.v1",stage:$stage,status:"failed",reason:"invalid_command_result",observations:{},cases:[]}' >"${result}"
  fi
  complete_stage "${manifest}" "${stage}" "${result}"
  [ "${command_status}" = "0" ] || return "${command_status}"
  [ "$(jq -r '.status' "${result}")" = "passed" ] || return 1
}

recover_campaign() {
  local manifest=$1 state stage result running_pid
  state="$(state_file "${manifest}")"
  [ -f "${state}" ] || die "campaign is not initialized"
  stage="$(jq -r '.running_stage // empty' "${state}")"
  [ -n "${stage}" ] || die "campaign has no interrupted running stage"
  running_pid="$(jq -r '.running_pid // empty' "${state}")"
  if [[ "${running_pid}" =~ ^[1-9][0-9]*$ ]] && kill -0 "${running_pid}" 2>/dev/null; then
    die "running stage owner is still alive"
  fi
  result="$(campaign_dir "${manifest}")/results/recovered-${stage}.json"
  jq -n --arg stage "${stage}" \
    '{schema:"helmrdotdev.validation-stage-result.v1",stage:$stage,status:"failed",reason:"operator_recovery",observations:{},cases:[]}' >"${result}"
  case "${stage}" in pre_shutdown_publish|post_shutdown_publish) ALLOW_PUBLISH_COMPLETE=1 ;; esac
  case "${stage}" in control_up|worker_up|cleanup|closed) ALLOW_COLLECTOR_COMPLETE=1 ;; esac
  case "${stage}" in auth_ready) ALLOW_AUTH_COMPLETE=1 ;; esac
  case "${stage}" in workload) ALLOW_WORKLOAD_COMPLETE=1 ;; esac
  complete_stage "${manifest}" "${stage}" "${result}"
  ALLOW_PUBLISH_COMPLETE=0
  ALLOW_COLLECTOR_COMPLETE=0
  ALLOW_AUTH_COMPLETE=0
  ALLOW_WORKLOAD_COMPLETE=0
}

run_auth_stage() {
  local manifest=$1 result command_status
  result="$(campaign_dir "${manifest}")/results/auth-readiness.tmp"
  RUN_OWNER_PID="$$"
  ALLOW_AUTH_COMPLETE=1
  start_stage "${manifest}" auth_ready
  set +e
  HELMR_VALIDATION_RESULT_FILE="${result}" PROJECT="$(jq -r '.workload.project' "$(campaign_dir "${manifest}")/manifest.json")" \
    STAGING_ENV="$(jq -r '.workload.environments[0]' "$(campaign_dir "${manifest}")/manifest.json")" \
    PRODUCTION_ENV="$(jq -r '.workload.environments[1]' "$(campaign_dir "${manifest}")/manifest.json")" \
    "${ROOT}/dev/aws/run-auth-readiness.sh"
  command_status=$?
  set -e
  if [ ! -f "${result}" ]; then
    jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"auth_ready",status:"failed",reason:"auth_result_missing",observations:{},cases:[]}' >"${result}"
  elif [ "${command_status}" != 0 ] && [ "$(jq -r '.status // empty' "${result}" 2>/dev/null || true)" = passed ]; then
    jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"auth_ready",status:"failed",reason:"auth_result_conflict",observations:{},cases:[]}' >"${result}"
  fi
  complete_stage "${manifest}" auth_ready "${result}"
  ALLOW_AUTH_COMPLETE=0
  rm -f "${result}"
  return "${command_status}"
}

normalize_control_image() {
  sed -E 's#^[0-9]{12}\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com/##'
}

tf_output_value() {
  local outputs=$1 name=$2
  jq -c --arg name "${name}" '.[$name].value' "${outputs}"
}

asg_inventory() {
  local name=$1
  if [ "${name}" = "null" ] || [ -z "${name}" ]; then
    printf '{"AutoScalingGroups":[]}\n'
  else
    aws autoscaling describe-auto-scaling-groups --auto-scaling-group-names "${name}"
  fi
}

collect_up_result() {
  local manifest=$1 stage=$2 result=$3 contract dir outputs cluster control_service dispatcher_service services
  local control_task dispatcher_task control_image dispatcher_image rds_id valkey_id rds valkey
  local run_asg_name build_asg_name run_asg build_asg nat_id nat nat_count
  local run_lt_id run_lt_version build_lt_id build_lt_version run_lt build_lt ami run_instance build_instance
  local run_hooks build_hooks collected_at price_sha
  contract="$(campaign_dir "${manifest}")/manifest.json"
  verify_aws_identity "${contract}"
  verify_dev_backend "${contract}"
  dir="$(campaign_dir "${manifest}")"
  outputs="${dir}/tofu-outputs.tmp"
  "${TF_BIN}" -chdir="${DEV_STACK}" output -json >"${outputs}"
  cluster="$(tf_output_value "${outputs}" control_cluster_name | jq -r .)"
  control_service="$(tf_output_value "${outputs}" control_service_name | jq -r .)"
  dispatcher_service="$(tf_output_value "${outputs}" dispatcher_service_name | jq -r .)"
  services="$(aws ecs describe-services --cluster "${cluster}" --services "${control_service}" "${dispatcher_service}")"
  jq -e --arg control "${control_service}" --arg dispatcher "${dispatcher_service}" '
    (.failures | length) == 0 and (.services | length) == 2 and
    ([.services[].serviceName] | sort) == ([$control,$dispatcher] | sort) and
    all(.services[];
      .desiredCount == 1 and .runningCount == 1 and .pendingCount == 0 and
      (.deployments | length) == 1 and .deployments[0].status == "PRIMARY" and
      .deployments[0].rolloutState == "COMPLETED")
  ' <<<"${services}" >/dev/null || die "control services are not fully healthy"
  control_task="$(aws ecs describe-task-definition --task-definition "$(jq -er --arg name "${control_service}" '.services[] | select(.serviceName == $name) | .taskDefinition' <<<"${services}")")"
  dispatcher_task="$(aws ecs describe-task-definition --task-definition "$(jq -er --arg name "${dispatcher_service}" '.services[] | select(.serviceName == $name) | .taskDefinition' <<<"${services}")")"
  control_image="$(jq -er '.taskDefinition.containerDefinitions[] | select(.name == "control") | .image' <<<"${control_task}" | normalize_control_image)"
  dispatcher_image="$(jq -er '.taskDefinition.containerDefinitions[] | select(.name == "dispatcher") | .image' <<<"${dispatcher_task}" | normalize_control_image)"
  [ "${control_image}" = "${dispatcher_image}" ] || die "control and dispatcher use different images"
  rds_id="$(tf_output_value "${outputs}" postgres_identifier | jq -r .)"
  valkey_id="${DEV_NAME}-dispatch"
  rds="$(aws rds describe-db-instances --db-instance-identifier "${rds_id}")"
  jq -e '(.DBInstances | length) == 1 and .DBInstances[0].DBInstanceStatus == "available"' <<<"${rds}" >/dev/null || die "RDS is not available"
  valkey="$(aws elasticache describe-replication-groups --replication-group-id "${valkey_id}")"
  jq -e '(.ReplicationGroups | length) == 1 and .ReplicationGroups[0].Status == "available" and (.ReplicationGroups[0].MemberClusters | length) >= 1' <<<"${valkey}" >/dev/null || die "Valkey is not available"
  run_asg_name="$(tf_output_value "${outputs}" worker_autoscaling_group_name | jq -r .)"
  build_asg_name="$(tf_output_value "${outputs}" build_worker_autoscaling_group_name | jq -r .)"
  run_asg="$(asg_inventory "${run_asg_name}")"
  build_asg="$(asg_inventory "${build_asg_name}")"
  nat_id="$(tf_output_value "${outputs}" nat_gateway_id | jq -r .)"
  if [ "${nat_id}" = "null" ]; then
    nat='{"NatGateways":[]}'
    nat_count=0
  else
    nat="$(aws ec2 describe-nat-gateways --nat-gateway-ids "${nat_id}")"
    nat_count="$(jq '[.NatGateways[] | select(.State == "available")] | length' <<<"${nat}")"
  fi
  collected_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  if [ "${stage}" = "control_up" ]; then
    [ "${nat_count}" = 0 ] || die "control-only deployment unexpectedly created a NAT gateway"
    [ "$(jq '.AutoScalingGroups | length' <<<"${run_asg}")" = 0 ] && [ "$(jq '.AutoScalingGroups | length' <<<"${build_asg}")" = 0 ] || die "control-only deployment unexpectedly created worker groups"
    jq -n --arg at "${collected_at}" --arg image "${control_image}" --arg rds_id "${rds_id}" --arg valkey_id "${valkey_id}" \
      --arg rds_class "$(jq -er '.DBInstances[0].DBInstanceClass' <<<"${rds}")" --arg rds_engine "$(jq -er '.DBInstances[0].EngineVersion' <<<"${rds}")" \
      --arg rds_created "$(jq -er '.DBInstances[0].InstanceCreateTime' <<<"${rds}")" --arg valkey_engine "$(jq -er '.ReplicationGroups[0].Engine' <<<"${valkey}")" \
      --arg valkey_created "$(jq -er '.ReplicationGroups[0].ReplicationGroupCreateTime' <<<"${valkey}")" --argjson valkey_nodes "$(jq '.ReplicationGroups[0].MemberClusters | length' <<<"${valkey}")" \
      '{schema:"helmrdotdev.validation-stage-result.v1",stage:"control_up",status:"passed",reason:null,observations:{collected_at:$at,control_image:$image,services:{control:{desired:1,running:1,pending:0,rollout:"COMPLETED"},dispatcher:{desired:1,running:1,pending:0,rollout:"COMPLETED"}},rds:{id:$rds_id,status:"available",class:$rds_class,engine_version:$rds_engine,created_at:$rds_created},valkey:{id:$valkey_id,status:"available",engine:$valkey_engine,node_count:$valkey_nodes,created_at:$valkey_created},nat_gateway_count:0,run_worker_count:0,build_worker_count:0},cases:[]}' >"${result}"
  else
    [ "$(jq '.AutoScalingGroups | length' <<<"${run_asg}")" = 1 ] && [ "$(jq '.AutoScalingGroups | length' <<<"${build_asg}")" = 1 ] || die "collector could not resolve both worker groups"
    jq -e --argjson run_max "$(tfvar_raw worker_max_size)" '
      .AutoScalingGroups[0] | .MinSize == 0 and .MaxSize == $run_max and .DesiredCapacity <= $run_max and
      all(.Instances[]; .ProtectedFromScaleIn == true)
    ' <<<"${run_asg}" >/dev/null || die "run worker group violates its live capacity or scale-in protection contract"
    jq -e --argjson build_max "$(tfvar_raw build_worker_max_size)" '
      .AutoScalingGroups[0] | .MinSize == 0 and .MaxSize == $build_max and .DesiredCapacity <= $build_max and
      all(.Instances[]; .ProtectedFromScaleIn == true)
    ' <<<"${build_asg}" >/dev/null || die "build worker group violates its live capacity or scale-in protection contract"
    [ "$(tf_output_value "${outputs}" worker_protect_from_scale_in | jq -r .)" = true ] || die "run worker group does not start protected from scale in"
    [ "$(tf_output_value "${outputs}" build_worker_protect_from_scale_in | jq -r .)" = true ] || die "build worker group does not start protected from scale in"
    run_hooks="$(aws autoscaling describe-lifecycle-hooks --auto-scaling-group-name "${run_asg_name}")"
    build_hooks="$(aws autoscaling describe-lifecycle-hooks --auto-scaling-group-name "${build_asg_name}")"
    for hook_set in "${run_hooks}" "${build_hooks}"; do
      jq -e '(.LifecycleHooks | length) == 2 and
        (any(.LifecycleHooks[]; .LifecycleTransition == "autoscaling:EC2_INSTANCE_LAUNCHING" and .DefaultResult == "ABANDON" and .HeartbeatTimeout > 0)) and
        (any(.LifecycleHooks[]; .LifecycleTransition == "autoscaling:EC2_INSTANCE_TERMINATING" and .DefaultResult == "CONTINUE" and .HeartbeatTimeout > 0))
      ' <<<"${hook_set}" >/dev/null || die "worker group lifecycle hooks violate the drain/readiness contract"
    done
    run_lt_id="$(jq -er '.AutoScalingGroups[0].LaunchTemplate.LaunchTemplateId' <<<"${run_asg}")"
    run_lt_version="$(jq -er '.AutoScalingGroups[0].LaunchTemplate.Version | tonumber' <<<"${run_asg}")"
    build_lt_id="$(jq -er '.AutoScalingGroups[0].LaunchTemplate.LaunchTemplateId' <<<"${build_asg}")"
    build_lt_version="$(jq -er '.AutoScalingGroups[0].LaunchTemplate.Version | tonumber' <<<"${build_asg}")"
    run_lt="$(aws ec2 describe-launch-template-versions --launch-template-id "${run_lt_id}" --versions "${run_lt_version}")"
    build_lt="$(aws ec2 describe-launch-template-versions --launch-template-id "${build_lt_id}" --versions "${build_lt_version}")"
    ami="$(jq -er '.LaunchTemplateVersions[0].LaunchTemplateData.ImageId' <<<"${run_lt}")"
    [ "$(jq -r '.LaunchTemplateVersions[0].LaunchTemplateData.ImageId' <<<"${build_lt}")" = "${ami}" ] || die "run and build launch templates use different AMIs"
    run_instance="$(jq -er '.LaunchTemplateVersions[0].LaunchTemplateData.InstanceType' <<<"${run_lt}")"
    build_instance="$(jq -er '.LaunchTemplateVersions[0].LaunchTemplateData.InstanceType' <<<"${build_lt}")"
    price_sha="$(canonical_sha256 "${PRICE_FIXTURE}")"
    jq -n --arg at "${collected_at}" --arg ami "${ami}" --arg run_instance "${run_instance}" --arg build_instance "${build_instance}" \
      --arg run_lt "${run_lt_id}" --argjson run_lt_version "${run_lt_version}" --arg build_lt "${build_lt_id}" --argjson build_lt_version "${build_lt_version}" \
      --arg run_asg "${run_asg_name}" --arg build_asg "${build_asg_name}" --arg nat_id "${nat_id}" --arg nat_created "$(jq -er '.NatGateways[0].CreateTime' <<<"${nat}")" \
      --arg rds "${rds_id}" --arg valkey "${valkey_id}" --arg price_sha "${price_sha}" --arg effective "$(jq -r '.effective_date' "${PRICE_FIXTURE}")" --arg queried "$(jq -r '.queried_at' "${PRICE_FIXTURE}")" \
      --argjson hourly "$(jq '.microusd_per_hour' "${PRICE_FIXTURE}")" --argjson run_group "$(jq '.AutoScalingGroups[0] | {created_at:.CreatedTime,min:.MinSize,max:.MaxSize,desired:.DesiredCapacity,instances:[.Instances[].InstanceId]}' <<<"${run_asg}")" \
      --argjson build_group "$(jq '.AutoScalingGroups[0] | {created_at:.CreatedTime,min:.MinSize,max:.MaxSize,desired:.DesiredCapacity,instances:[.Instances[].InstanceId]}' <<<"${build_asg}")" \
      '{schema:"helmrdotdev.validation-stage-result.v1",stage:"worker_up",status:"passed",reason:null,observations:{collected_at:$at,worker_ami_id:$ami,run:{asg:$run_asg,created_at:$run_group.created_at,instance_type:$run_instance,launch_template_id:$run_lt,launch_template_version:$run_lt_version,min:$run_group.min,max:$run_group.max,desired:$run_group.desired,instances:$run_group.instances,lifecycle_hooks:true,scale_in_protected:true},build:{asg:$build_asg,created_at:$build_group.created_at,instance_type:$build_instance,launch_template_id:$build_lt,launch_template_version:$build_lt_version,min:$build_group.min,max:$build_group.max,desired:$build_group.desired,instances:$build_group.instances,lifecycle_hooks:true,scale_in_protected:true},nat:{id:$nat_id,status:"available",created_at:$nat_created},rds_instance_id:$rds,valkey_replication_group_id:$valkey,pricing:{provider:"aws",region:"us-east-1",currency:"USD",purchase_model:"on-demand",fixture_sha256:$price_sha,effective_at:$effective,queried_at:$queried,worker_microusd_per_hour:$hourly}},cases:[]}' >"${result}"
  fi
  rm -f "${outputs}"
}

collect_zero_inventory() {
  local terraform_count nat_count run_count build_count rds_count valkey_count ecs_count cluster tagged_count lb_count target_group_count endpoint_count
  verify_dev_backend "$(campaign_dir "$1")/manifest.json"
  terraform_count="$("${TF_BIN}" -chdir="${DEV_STACK}" state list | awk 'NF {count++} END {print count+0}')"
  nat_count="$(aws ec2 describe-nat-gateways --filter "Name=tag:Name,Values=${DEV_NAME}-nat" | jq '[.NatGateways[] | select(.State != "deleted" and .State != "failed")] | length')"
  run_count="$(aws ec2 describe-instances --filters "Name=tag:Name,Values=${DEV_NAME}-run-worker" "Name=instance-state-name,Values=pending,running,stopping,stopped" | jq '[.Reservations[].Instances[]] | length')"
  build_count="$(aws ec2 describe-instances --filters "Name=tag:Name,Values=${DEV_NAME}-build-worker" "Name=instance-state-name,Values=pending,running,stopping,stopped" | jq '[.Reservations[].Instances[]] | length')"
  [ "$(asg_inventory "${DEV_NAME}-run-worker" | jq '.AutoScalingGroups | length')" = "0" ] || run_count=$((run_count + 1))
  [ "$(asg_inventory "${DEV_NAME}-build-worker" | jq '.AutoScalingGroups | length')" = "0" ] || build_count=$((build_count + 1))
  rds_count="$(aws rds describe-db-instances | jq --arg id "${DEV_NAME}-postgres" '[.DBInstances[] | select(.DBInstanceIdentifier == $id)] | length')"
  valkey_count="$(aws elasticache describe-replication-groups | jq --arg id "${DEV_NAME}-dispatch" '[.ReplicationGroups[] | select(.ReplicationGroupId == $id)] | length')"
  cluster="$(aws ecs describe-clusters --clusters "${DEV_NAME}-control")"
  if [ "$(jq '[.clusters[] | select(.status == "ACTIVE")] | length' <<<"${cluster}")" = "0" ]; then
    ecs_count=0
  else
    ecs_count="$(aws ecs list-services --cluster "${DEV_NAME}-control" | jq '.serviceArns | length')"
  fi
  tagged_count="$(aws resourcegroupstaggingapi get-resources --tag-filters "Key=Stack,Values=${DEV_NAME}" | jq '.ResourceTagMappingList | length')"
  lb_count="$(aws elbv2 describe-load-balancers | jq --arg name "${DEV_NAME}" '[.LoadBalancers[] | select(.LoadBalancerName | startswith($name))] | length')"
  target_group_count="$(aws elbv2 describe-target-groups | jq --arg name "${DEV_NAME}" '[.TargetGroups[] | select(.TargetGroupName | startswith($name))] | length')"
  endpoint_count="$(aws ec2 describe-vpc-endpoints --filters "Name=tag:Stack,Values=${DEV_NAME}" | jq '[.VpcEndpoints[] | select(.State != "deleted" and .State != "failed")] | length')"
  jq -n --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --argjson nat "${nat_count}" --argjson run "${run_count}" --argjson build "${build_count}" --argjson rds "${rds_count}" --argjson valkey "${valkey_count}" --argjson ecs "${ecs_count}" --argjson terraform "${terraform_count}" --argjson tagged "${tagged_count}" --argjson lb "${lb_count}" --argjson tg "${target_group_count}" --argjson endpoints "${endpoint_count}" \
    '{observed_at:$at,counts:{active_nat_gateways:$nat,run_workers:$run,build_workers:$build,rds_instances:$rds,valkey_clusters:$valkey,ecs_services:$ecs,terraform_resources:$terraform,tagged_resources:$tagged,load_balancers:$lb,target_groups:$tg,vpc_endpoints:$endpoints}}'
}

run_collected_stage() {
  local manifest=$1 stage=$2
  shift 2
  [ "${1:-}" = "--" ] || die "run-collect requires -- before the command"
  shift
  [ "$#" -gt 0 ] || die "run-collect requires a command"
  case "${stage}" in control_up|worker_up|cleanup) ;; *) die "run-collect stage must be control_up, worker_up, or cleanup" ;; esac
  RUN_OWNER_PID="$$"
  ALLOW_COLLECTOR_STAGE=1
  start_stage "${manifest}" "${stage}"
  ALLOW_COLLECTOR_STAGE=0
  set +e
  "$@"
  local command_status=$?
  set -e
  local result
  result="$(campaign_dir "${manifest}")/results/collector-${stage}.json"
  if [ "${command_status}" != "0" ]; then
    jq -n --arg stage "${stage}" '{schema:"helmrdotdev.validation-stage-result.v1",stage:$stage,status:"failed",reason:"collector_command_failed",observations:{},cases:[]}' >"${result}"
  elif [ "${stage}" = "cleanup" ]; then
    verify_aws_identity "$(campaign_dir "${manifest}")/manifest.json"
    jq -n --argjson observations "$(collect_zero_inventory "${manifest}")" '{schema:"helmrdotdev.validation-stage-result.v1",stage:"cleanup",status:"passed",reason:null,observations:$observations,cases:[]}' >"${result}"
  else
    collect_up_result "${manifest}" "${stage}" "${result}"
  fi
  ALLOW_COLLECTOR_COMPLETE=1
  complete_stage "${manifest}" "${stage}" "${result}"
  ALLOW_COLLECTOR_COMPLETE=0
  rm -f "${result}"
  [ "${command_status}" = "0" ] || return "${command_status}"
}

close_campaign() {
  local manifest=$1 result observations verdict
  RUN_OWNER_PID="$$"
  ALLOW_COLLECTOR_STAGE=1
  start_stage "${manifest}" closed
  ALLOW_COLLECTOR_STAGE=0
  verify_aws_identity "$(campaign_dir "${manifest}")/manifest.json"
  observations="$(collect_zero_inventory "${manifest}")"
  # The Resource Groups Tagging API retains mappings for deleted resources and
  # non-billable ECS task-definition history. Explicit service inventories and
  # OpenTofu state are the live-resource authority; keep tagged count as context.
  jq -e 'all((.counts | del(.tagged_resources))[]; . == 0)' <<<"${observations}" >/dev/null || die "live collector found resources after cleanup"
  verdict="$(jq -r 'if .verdict == "pending" then "passed" else .verdict end' "$(state_file "${manifest}")")"
  result="$(campaign_dir "${manifest}")/results/collector-closed.json"
  jq -n --arg verdict "${verdict}" '{schema:"helmrdotdev.validation-stage-result.v1",stage:"closed",status:"passed",reason:null,observations:{verdict:$verdict,zero_resources:true},cases:[]}' >"${result}"
  ALLOW_COLLECTOR_COMPLETE=1
  complete_stage "${manifest}" closed "${result}"
  ALLOW_COLLECTOR_COMPLETE=0
  rm -f "${result}"
}

bucket_name() {
  "${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" output -raw source_artifact_bucket_name
}

kms_key_arn() {
  "${TF_BIN}" -chdir="${BOOTSTRAP_STACK}" output -raw source_artifact_kms_key_arn
}

verify_evidence_bucket() {
  local bucket=$1 kms=$2 retention=$3
  [ "$(aws s3api get-bucket-versioning --bucket "${bucket}" | jq -r '.Status')" = "Enabled" ] || die "evidence bucket versioning is not enabled"
  aws s3api get-public-access-block --bucket "${bucket}" | jq -e \
    '.PublicAccessBlockConfiguration | .BlockPublicAcls and .IgnorePublicAcls and .BlockPublicPolicy and .RestrictPublicBuckets' >/dev/null ||
    die "evidence bucket public access block is incomplete"
  aws s3api get-bucket-encryption --bucket "${bucket}" | jq -e --arg kms "${kms}" \
    '.ServerSideEncryptionConfiguration.Rules | any(.ApplyServerSideEncryptionByDefault.SSEAlgorithm == "aws:kms" and .ApplyServerSideEncryptionByDefault.KMSMasterKeyID == $kms)' >/dev/null ||
    die "evidence bucket KMS policy does not match bootstrap output"
  aws s3api get-bucket-lifecycle-configuration --bucket "${bucket}" | jq -e --argjson retention "${retention}" '
    .Rules | any(.ID == "expire-validation-evidence" and .Status == "Enabled" and .Filter.Prefix == "helmr/validation-evidence/" and
      .Expiration.Days >= $retention and .NoncurrentVersionExpiration.NoncurrentDays >= $retention)
  ' >/dev/null || die "evidence lifecycle is shorter than manifest retention"
}

claim_namespace() {
  local manifest=$1 state dir bucket kms prefix namespace claim claim_sha checksum output version head
  verify_frozen "${manifest}" forward
  state="$(state_file "${manifest}")"
  [ "$(jq -r '.claim == null' "${state}")" = "true" ] || die "evidence namespace is already claimed locally"
  need_command aws
  verify_aws_identity "${manifest}"
  bucket="$(bucket_name)"
  kms="$(kms_key_arn)"
  prefix="$(jq -r '.evidence.claim_prefix' "${manifest}")"
  namespace="$(manifest_namespace "${manifest}")"
  verify_evidence_bucket "${bucket}" "${kms}" "$(jq -r '.evidence.retention_days' "${manifest}")"
  dir="$(campaign_dir "${manifest}")"
  claim="${dir}/claim.json"
  jq -n \
    --arg manifest_sha "$(jq -r '.manifest.raw_sha256' "${state}")" \
    --arg source_commit "$(jq -r '.source.commit' "${state}")" \
    --arg governance_commit "$(jq -r '.governance.commit' "${state}")" \
    '{schema:"helmrdotdev.aws-validation-evidence-claim.v1",manifest_sha256:$manifest_sha,source_commit:$source_commit,governance_commit:$governance_commit}' >"${claim}"
  chmod 0600 "${claim}"
  claim_sha="$(sha256_file "${claim}")"
  checksum="$(sha256_base64_file "${claim}")"
  output="${dir}/claim-put.tmp"
  aws s3api put-object \
    --bucket "${bucket}" \
    --key "${prefix}/${namespace}/claim.json" \
    --body "${claim}" \
    --if-none-match '*' \
    --checksum-algorithm SHA256 \
    --checksum-sha256 "${checksum}" \
    --server-side-encryption aws:kms \
    --ssekms-key-id "${kms}" \
    --metadata "sha256=${claim_sha}" >"${output}"
  chmod 0600 "${output}"
  version="$(jq -er '.VersionId | select(type == "string" and length > 0)' "${output}")" || die "claim upload did not return a version id"
  [ "$(jq -r '.ChecksumSHA256' "${output}")" = "${checksum}" ] || die "claim upload response checksum differs"
  head="$(aws s3api head-object --bucket "${bucket}" --key "${prefix}/${namespace}/claim.json" --version-id "${version}" --checksum-mode ENABLED)"
  jq -e --arg version "${version}" --arg checksum "${checksum}" --arg sha "${claim_sha}" --arg kms "${kms}" --argjson bytes "$(wc -c <"${claim}" | tr -d ' ')" '
    .VersionId == $version and .ChecksumSHA256 == $checksum and .Metadata.sha256 == $sha and
    .ServerSideEncryption == "aws:kms" and .SSEKMSKeyId == $kms and .ContentLength == $bytes
  ' <<<"${head}" >/dev/null || die "stored claim version failed integrity verification"
  rm -f "${output}"
  set_state "${manifest}" '.claim={key:$key,version_id:$version,sha256:$sha,checksum_sha256:$checksum}' \
    --arg key "${prefix}/${namespace}/claim.json" --arg version "${version}" --arg sha "${claim_sha}" --arg checksum "${checksum}"
  append_ledger "${manifest}" claimed preflight passed
}

validate_evidence_tree() {
  local manifest=$1 dir file base stage
  dir="$(campaign_dir "${manifest}")"
  [ "$(sha256_file "${dir}/manifest.json")" = "$(jq -r '.manifest.raw_sha256' "${dir}/state.json")" ] || die "evidence manifest copy drifted"
  jq -e '
    type == "object" and keys == ["claim","deployment","governance","harness","manifest","namespace","next_stage_index","running_pid","running_stage","schema","source","status","verdict"] and
    .schema == "helmrdotdev.aws-validation-campaign-state.v1" and
    (.namespace | test("^[a-z][a-z0-9-]{7,95}$")) and
    (.status | IN("ready","running","cleanup_required","closed")) and (.verdict | IN("pending","passed","failed")) and
    (.next_stage_index | type == "number" and . >= 0 and . <= 10) and
    (.running_stage == null or (.running_stage | test("^[a-z_]{1,40}$"))) and
    (.running_pid == null or (.running_pid | type == "number" and . >= 1 and floor == .)) and
    (.claim == null or (.claim | keys == ["checksum_sha256","key","sha256","version_id"] and
      (.checksum_sha256 | test("^[A-Za-z0-9+/]{43}=$")) and (.key | test("^helmr/validation-claims/[a-z0-9-]+/claim\\.json$")) and
      (.sha256 | test("^[0-9a-f]{64}$")) and (.version_id | type == "string" and length >= 1 and length <= 1024))) and
    (.deployment == null or
      (.deployment | (keys == ["control_image","rds_instance_id","valkey_replication_group_id"] or
        keys == ["build_launch_template_id","build_launch_template_version","build_worker_instance_type","control_image","rds_instance_id","run_launch_template_id","run_launch_template_version","run_worker_instance_type","valkey_replication_group_id","worker_ami_id"])) and
      (.deployment.control_image | test("@sha256:[0-9a-f]{64}$")) and
      (.deployment.rds_instance_id | test("^[a-z][a-z0-9-]{0,62}$")) and
      (.deployment.valkey_replication_group_id | test("^[a-z][a-z0-9-]{0,39}$")) and
      (.deployment.run_launch_template_id == null or (.deployment.run_launch_template_id | test("^lt-[0-9a-f]{8,17}$"))) and
      (.deployment.run_launch_template_version == null or (.deployment.run_launch_template_version | type == "number" and . >= 1)) and
      (.deployment.build_launch_template_id == null or (.deployment.build_launch_template_id | test("^lt-[0-9a-f]{8,17}$"))) and
      (.deployment.build_launch_template_version == null or (.deployment.build_launch_template_version | type == "number" and . >= 1)) and
      (.deployment.worker_ami_id == null or (.deployment.worker_ami_id | test("^ami-[0-9a-f]{8,17}$"))) and
      (.deployment.run_worker_instance_type == null or (.deployment.run_worker_instance_type | test("^[a-z][a-z0-9.]+$"))) and
      (.deployment.build_worker_instance_type == null or (.deployment.build_worker_instance_type | test("^[a-z][a-z0-9.]+$")))) and
    (.manifest | keys == ["canonical_sha256","path","raw_sha256"] and (.canonical_sha256 | test("^[0-9a-f]{64}$")) and (.raw_sha256 | test("^[0-9a-f]{64}$"))) and
    (.source | keys == ["commit"] and (.commit | test("^[0-9a-f]{40}$"))) and
    (.harness | keys == ["sha256"] and (.sha256 | test("^[0-9a-f]{64}$"))) and
    (.governance | keys == ["commit","path","root"] and (.commit | test("^[0-9a-f]{40}$")))
  ' "${dir}/state.json" >/dev/null || die "campaign state contains disallowed evidence fields"
  jq -e '
    type == "array" and length <= 200 and all(.[];
      keys == ["at","detail","event","stage","status"] and
      (.at | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")) and
      (.event | IN("initialized","claimed","started","completed")) and (.stage | test("^[a-z_]{1,40}$")) and
      (.status | test("^[a-z_]{1,40}$")) and
      (.detail == null or (.detail | (keys == ["attempt"] and (.attempt | type == "number" and . >= 1 and . <= 10000)) or
        (keys == ["reason"] and (.reason | test("^[a-z0-9._-]{1,80}$"))))))
  ' "${dir}/ledger.json" >/dev/null || die "campaign ledger contains disallowed evidence fields"
  if [ -f "${dir}/claim.json" ]; then
    jq -e 'keys == ["governance_commit","manifest_sha256","schema","source_commit"] and .schema == "helmrdotdev.aws-validation-evidence-claim.v1" and ([.governance_commit,.source_commit] | all(test("^[0-9a-f]{40}$"))) and (.manifest_sha256 | test("^[0-9a-f]{64}$"))' "${dir}/claim.json" >/dev/null || die "claim contains disallowed evidence fields"
  fi
  for file in "${dir}"/results/*.json "${dir}"/publishes/*.json; do
    [ -e "${file}" ] || continue
    base="$(basename "${file}")"
    stage="$(jq -r '.stage // empty' "${file}")"
    case "${base}" in
      [0-9][0-9]-*.json|pre-shutdown.json|post-shutdown.json) ;;
      *) die "unexpected structured evidence result: ${base}" ;;
    esac
    validate_stage_result "${manifest}" "${stage}" "${file}" evidence
  done
}

make_bundle() {
  local manifest=$1 checkpoint=$2 output=$3 dir
  dir="$(campaign_dir "${manifest}")"
  validate_evidence_tree "${manifest}"
  find "${dir}" -type f \( -name '*.json' -o -name '*.tmp' \) | while IFS= read -r file; do
    case "${file}" in
      *.tmp) die "temporary file present in evidence tree" ;;
    esac
    jq -e . "${file}" >/dev/null || die "non-JSON evidence file: ${file}"
  done
  python3 - "${dir}" "${output}" <<'PY'
import gzip
import io
import json
import pathlib
import tarfile
import sys

root = pathlib.Path(sys.argv[1])
output = pathlib.Path(sys.argv[2])
allowed = []
root_names = {"manifest.json", "state.json", "ledger.json", "claim.json"}
stage_names = {
    "00-preflight.json", "01-control_up.json", "02-awaiting_human.json",
    "03-auth_ready.json", "04-worker_up.json", "05-workload.json",
    "06-pre_shutdown_publish.json", "07-cleanup.json",
    "08-closed.json", "09-post_shutdown_publish.json",
}
publish_names = {"pre-shutdown.json", "post-shutdown.json"}
for path in root.rglob("*.json"):
    rel = path.relative_to(root)
    valid = (
        (len(rel.parts) == 1 and rel.name in root_names) or
        (len(rel.parts) == 2 and rel.parts[0] == "results" and rel.name in stage_names) or
        (len(rel.parts) == 2 and rel.parts[0] == "publishes" and rel.name in publish_names)
    )
    if not valid:
        raise SystemExit(f"unexpected JSON evidence path: {rel}")
    if path.stat().st_size > 65536:
        raise SystemExit(f"JSON evidence file exceeds 64 KiB: {rel}")
    value = json.loads(path.read_text())
    if rel.name == "state.json":
        value["manifest"].pop("path", None)
        value["governance"].pop("root", None)
        data = (json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n").encode()
    else:
        data = path.read_bytes()
    allowed.append((str(rel), data))
payload = io.BytesIO()
with tarfile.open(fileobj=payload, mode="w") as archive:
    for name, data in sorted(allowed):
        info = tarfile.TarInfo(name)
        info.size = len(data)
        info.mode = 0o600
        info.mtime = 0
        info.uid = info.gid = 0
        archive.addfile(info, io.BytesIO(data))
with output.open("wb") as raw:
    with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as zipped:
        zipped.write(payload.getvalue())
PY
  chmod 0600 "${output}"
}

publish_bundle_work() {
  local manifest=$1 checkpoint=$2 stage=$3
  local state dir contract bundle bytes max_bytes sha checksum bucket kms prefix namespace key put put_status version head result
  state="$(state_file "${manifest}")"
  dir="$(campaign_dir "${manifest}")"
  contract="${dir}/manifest.json"
  verify_aws_identity "${contract}"
  [ "$(jq -r '.claim != null' "${state}")" = "true" ] || die "evidence namespace has not been claimed"
  bundle="${dir}/${checkpoint}.tar.gz"
  make_bundle "${contract}" "${checkpoint}" "${bundle}"
  bytes="$(wc -c <"${bundle}" | tr -d ' ')"
  max_bytes="$(jq -r '.cost_guard.max_bundle_bytes' "${contract}")"
  [ "${bytes}" -le "${max_bytes}" ] || die "evidence bundle exceeds manifest byte ceiling"
  sha="$(sha256_file "${bundle}")"
  checksum="$(sha256_base64_file "${bundle}")"
  bucket="$(bucket_name)"
  kms="$(kms_key_arn)"
  prefix="$(jq -r '.evidence.prefix' "${contract}")"
  namespace="$(manifest_namespace "${contract}")"
  key="${prefix}/${namespace}/${checkpoint}/${sha}.tar.gz"
  verify_evidence_bucket "${bucket}" "${kms}" "$(jq -r '.evidence.retention_days' "${contract}")"
  set +e
  put="$(aws s3api put-object --bucket "${bucket}" --key "${key}" --body "${bundle}" --if-none-match '*' \
    --checksum-algorithm SHA256 --checksum-sha256 "${checksum}" \
    --server-side-encryption aws:kms --ssekms-key-id "${kms}" --metadata "sha256=${sha}")"
  put_status=$?
  set -e
  if [ "${put_status}" = "0" ]; then
    version="$(jq -er '.VersionId | select(type == "string" and length > 0)' <<<"${put}")" || die "evidence upload did not return a version id"
    [ "$(jq -r '.ChecksumSHA256' <<<"${put}")" = "${checksum}" ] || die "evidence upload response checksum differs"
    head="$(aws s3api head-object --bucket "${bucket}" --key "${key}" --version-id "${version}" --checksum-mode ENABLED)"
  else
    head="$(aws s3api head-object --bucket "${bucket}" --key "${key}" --checksum-mode ENABLED)" || die "evidence create failed and no resumable object exists"
    version="$(jq -er '.VersionId | select(type == "string" and length > 0)' <<<"${head}")" || die "resumable evidence object has no version id"
  fi
  jq -e --arg version "${version}" --arg checksum "${checksum}" --arg sha "${sha}" --arg kms "${kms}" --argjson bytes "${bytes}" '
    .VersionId == $version and .ChecksumSHA256 == $checksum and .Metadata.sha256 == $sha and
    .ServerSideEncryption == "aws:kms" and .SSEKMSKeyId == $kms and .ContentLength == $bytes
  ' <<<"${head}" >/dev/null || die "stored evidence version failed integrity verification"
  result="${dir}/publishes/${checkpoint}.json"
  jq -n --arg stage "${stage}" --arg checkpoint "${checkpoint}" --arg key "${key}" --arg sha "${sha}" --arg checksum "${checksum}" --arg version "${version}" --argjson bytes "${bytes}" \
    '{schema:"helmrdotdev.validation-stage-result.v1",stage:$stage,status:"passed",reason:null,observations:{checkpoint:$checkpoint,logical_key:$key,sha256:$sha,checksum_sha256:$checksum,version_id:$version,bytes:$bytes},cases:[]}' >"${result}"
  rm -f "${bundle}"
  printf 'evidence_key=%s\n' "${key}"
  printf 'evidence_sha256=%s\n' "${sha}"
  printf 'evidence_bytes=%s\n' "${bytes}"
}

publish_bundle() {
  local manifest=$1 checkpoint=$2 stage dir result publish_status
  case "${checkpoint}" in
    pre-shutdown) stage=pre_shutdown_publish ;;
    post-shutdown) stage=post_shutdown_publish ;;
    *) die "checkpoint must be pre-shutdown or post-shutdown" ;;
  esac
  RUN_OWNER_PID="$$"
  start_stage "${manifest}" "${stage}"
  dir="$(campaign_dir "${manifest}")"
  result="${dir}/publishes/${checkpoint}.json"
  set +e
  (publish_bundle_work "${manifest}" "${checkpoint}" "${stage}")
  publish_status=$?
  set -e
  if [ "${publish_status}" != "0" ]; then
    rm -f "${dir}/${checkpoint}.tar.gz"
    jq -n --arg stage "${stage}" \
      '{schema:"helmrdotdev.validation-stage-result.v1",stage:$stage,status:"failed",reason:"evidence_publish_failed",observations:{},cases:[]}' >"${result}"
  fi
  ALLOW_PUBLISH_COMPLETE=1
  complete_stage "${manifest}" "${stage}" "${result}"
  ALLOW_PUBLISH_COMPLETE=0
  return "${publish_status}"
}

sample_worker_groups() {
  local run_asg=$1 build_asg=$2 output=$3 at run build
  at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  run="$(asg_inventory "${run_asg}")"
  build="$(asg_inventory "${build_asg}")"
  jq -cn --arg at "${at}" --argjson run "$(jq '[.AutoScalingGroups[].Instances[]] | length' <<<"${run}")" --argjson build "$(jq '[.AutoScalingGroups[].Instances[]] | length' <<<"${build}")" \
    --argjson run_instances "$(jq '[.AutoScalingGroups[].Instances[].InstanceId]' <<<"${run}")" --argjson build_instances "$(jq '[.AutoScalingGroups[].Instances[].InstanceId]' <<<"${build}")" \
    '{at:$at,run:$run,build:$build,run_instances:$run_instances,build_instances:$build_instances}' >>"${output}"
}

nat_metric_sum() {
  local nat_id=$1 metric=$2 started=$3 finished=$4
  aws cloudwatch get-metric-statistics --namespace AWS/NATGateway --metric-name "${metric}" \
    --dimensions "Name=NatGatewayId,Value=${nat_id}" --start-time "${started}" --end-time "${finished}" \
    --period 60 --statistics Sum | jq '[.Datapoints[].Sum] | add // 0 | floor'
}

run_workload() {
  local manifest=$1 contract dir outputs run_asg build_asg nat_id result cases_file samples sentinel sampler_pid
  local started finished case_json case_id producer producer_sha repetitions attempt raw status reason command_status evidence_sha
  contract="$(campaign_dir "${manifest}")/manifest.json"
  verify_frozen "${manifest}" forward
  verify_aws_identity "${contract}"
  verify_worker_cost_guard "${contract}"
  verify_dev_backend "${contract}"
  RUN_OWNER_PID="$$"
  ALLOW_WORKLOAD_COMPLETE=1
  start_stage "${manifest}" workload
  dir="$(campaign_dir "${manifest}")"
  outputs="${dir}/workload-outputs.tmp"
  cases_file="${dir}/workload-cases.tmp"
  samples="${dir}/workload-samples.tmp"
  sentinel="${dir}/workload-sampling"
  result="${dir}/results/workload-collector.tmp"
  "${TF_BIN}" -chdir="${DEV_STACK}" output -json >"${outputs}"
  run_asg="$(tf_output_value "${outputs}" worker_autoscaling_group_name | jq -r .)"
  build_asg="$(tf_output_value "${outputs}" build_worker_autoscaling_group_name | jq -r .)"
  nat_id="$(tf_output_value "${outputs}" nat_gateway_id | jq -r .)"
  : >"${samples}"
  printf '[]\n' >"${cases_file}"
  : >"${sentinel}"
  (
    while [ -e "${sentinel}" ]; do
      sample_worker_groups "${run_asg}" "${build_asg}" "${samples}" || printf '{"sampling_error":true}\n' >>"${samples}"
      sleep 2
    done
  ) &
  sampler_pid=$!
  started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  while IFS= read -r case_json; do
    case_id="$(jq -r '.id' <<<"${case_json}")"
    producer="$(jq -r '.producer.path' <<<"${case_json}")"
    producer_sha="$(jq -r '.producer.sha256' <<<"${case_json}")"
    [ "$(sha256_file "${ROOT}/${producer}")" = "${producer_sha}" ] || die "workload producer drifted: ${producer}"
    repetitions="$(jq -r '.repetitions' <<<"${case_json}")"
    local attempts='[]'
    for ((attempt = 1; attempt <= repetitions; attempt++)); do
      raw="${dir}/producer-result.tmp"
      rm -f "${raw}"
      set +e
      HELMR_VALIDATION_CASE="${case_json}" HELMR_VALIDATION_CASE_RESULT_FILE="${raw}" HELMR_VALIDATION_CASE_ATTEMPT="${attempt}" \
        "${ROOT}/${producer}"
      command_status=$?
      set -e
      if [ ! -f "${raw}" ]; then
        jq -n '{schema:"helmrdotdev.validation-case-source-result.v1",status:"failed",reason:"producer_result_missing",checks:[]}' >"${raw}"
      fi
      if ! jq -e '
        type == "object" and has("schema") and has("status") and has("reason") and
        .schema == "helmrdotdev.validation-case-source-result.v1" and (.status == "passed" or .status == "failed") and
        ((.status == "passed" and .reason == null) or (.status == "failed" and (.reason | test("^[a-z0-9._-]{1,80}$"))))
      ' "${raw}" >/dev/null; then
        jq -n '{schema:"helmrdotdev.validation-case-source-result.v1",status:"failed",reason:"invalid_producer_result"}' >"${raw}"
        command_status=1
      fi
      status="$(jq -r '.status' "${raw}")"
      reason="$(jq -c '.reason' "${raw}")"
      if [ "${command_status}" != 0 ] && [ "${status}" = passed ]; then
        status=failed
        reason='"producer_exit_conflict"'
      fi
      evidence_sha="$(sha256_file "${raw}")"
      attempts="$(jq -c --argjson index "${attempt}" --arg status "${status}" --argjson reason "${reason}" --arg evidence "${evidence_sha}" --arg producer "${producer_sha}" '. + [{index:$index,status:$status,reason:$reason,evidence_sha256:$evidence,producer_sha256:$producer}]' <<<"${attempts}")"
    done
    local case_status=passed
    jq -e 'all(.[]; .status == "passed")' <<<"${attempts}" >/dev/null || case_status=failed
    jq --arg id "${case_id}" --arg status "${case_status}" --argjson attempts "${attempts}" '. + [{id:$id,status:$status,attempts:$attempts}]' "${cases_file}" >"${cases_file}.next"
    mv "${cases_file}.next" "${cases_file}"
  done < <(jq -c '.workload.cases[]' "${contract}")
  finished="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  rm -f "${sentinel}"
  wait "${sampler_pid}"
  sample_worker_groups "${run_asg}" "${build_asg}" "${samples}"
  local run_peak build_peak nat_in nat_out intervals workload_status workload_reason
  if jq -e 'select(.sampling_error == true)' "${samples}" >/dev/null; then
    jq '.[0].status="failed" | .[0].attempts[0].status="failed" | .[0].attempts[0].reason="worker_sampling_failed"' "${cases_file}" >"${cases_file}.next"
    mv "${cases_file}.next" "${cases_file}"
    run_peak=0
    build_peak=0
    nat_in=0
    nat_out=0
    intervals='[]'
  else
    run_peak="$(jq -s 'map(.run) | max // 0' "${samples}")"
    build_peak="$(jq -s 'map(.build) | max // 0' "${samples}")"
    nat_in="$(nat_metric_sum "${nat_id}" BytesInFromDestination "${started}" "${finished}")"
    nat_out="$(nat_metric_sum "${nat_id}" BytesOutToDestination "${started}" "${finished}")"
    intervals="$(jq -s '[.[] as $sample | (($sample.run_instances[]? | {id:.,role:"run",at:$sample.at}),($sample.build_instances[]? | {id:.,role:"build",at:$sample.at}))] | sort_by(.role,.id,.at) | group_by(.role,.id) | map({id:.[0].id,role:.[0].role,first_seen:.[0].at,last_seen:.[-1].at})' "${samples}")"
  fi
  workload_status=passed
  workload_reason=null
  jq -e 'all(.[]; .status == "passed")' "${cases_file}" >/dev/null || { workload_status=failed; workload_reason='"workload_case_failed"'; }
  jq -n --arg status "${workload_status}" --argjson reason "${workload_reason}" --arg started "${started}" --arg finished "${finished}" \
    --argjson cases "$(cat "${cases_file}")" --argjson run_peak "${run_peak}" --argjson build_peak "${build_peak}" --argjson nat_in "${nat_in}" --argjson nat_out "${nat_out}" --argjson intervals "${intervals}" \
    '{schema:"helmrdotdev.validation-stage-result.v1",stage:"workload",status:$status,reason:$reason,observations:{started_at:$started,finished_at:$finished,run_worker_peak:$run_peak,build_worker_peak:$build_peak,nat_bytes_in_from_destination:$nat_in,nat_bytes_out_to_destination:$nat_out,worker_observed_intervals:$intervals},cases:$cases}' >"${result}"
  ALLOW_WORKLOAD_COMPLETE=1
  complete_stage "${manifest}" workload "${result}"
  ALLOW_WORKLOAD_COMPLETE=0
  rm -f "${outputs}" "${cases_file}" "${samples}" "${raw}" "${result}"
  [ "$(jq -r '.verdict' "$(state_file "${manifest}")")" = passed ]
}

main() {
  need_command jq
  command -v sha256sum >/dev/null 2>&1 || need_command shasum
  local command=${1:-}
  case "${command}" in
    validate) [ "$#" = 2 ] || { usage >&2; exit 2; }; validate_manifest "$2" ;;
    init) [ "$#" = 2 ] || { usage >&2; exit 2; }; init_campaign "$2" ;;
    status) [ "$#" = 2 ] || { usage >&2; exit 2; }; jq . "$(state_file "$2")" ;;
    start) [ "$#" = 3 ] || { usage >&2; exit 2; }; start_stage "$2" "$3" ;;
    complete) [ "$#" = 4 ] || { usage >&2; exit 2; }; complete_stage "$2" "$3" "$4" ;;
    run) [ "$#" -ge 6 ] || { usage >&2; exit 2; }; shift; run_stage "$@" ;;
    recover) [ "$#" = 2 ] || { usage >&2; exit 2; }; recover_campaign "$2" ;;
    claim) [ "$#" = 2 ] || { usage >&2; exit 2; }; claim_namespace "$2" ;;
    publish) [ "$#" = 3 ] || { usage >&2; exit 2; }; publish_bundle "$2" "$3" ;;
    run-collect) [ "$#" -ge 5 ] || { usage >&2; exit 2; }; shift; run_collected_stage "$@" ;;
    auth) [ "$#" = 2 ] || { usage >&2; exit 2; }; run_auth_stage "$2" ;;
    close) [ "$#" = 2 ] || { usage >&2; exit 2; }; close_campaign "$2" ;;
    workload) [ "$#" = 2 ] || { usage >&2; exit 2; }; run_workload "$2" ;;
    -h|--help) usage ;;
    *) usage >&2; exit 2 ;;
  esac
}

main "$@"
