#!/usr/bin/env bash
set -euo pipefail

control_image="${1:-}"
worker_amis_json="${2:-}"
output="${3:-aws-artifacts.json}"
required_worker_ami_regions="${REQUIRED_WORKER_AMI_REGIONS:-us-east-1,us-west-2,ap-northeast-1}"
verify_release_artifacts="${VERIFY_RELEASE_ARTIFACTS:-0}"

if [ -z "$control_image" ] || [ -z "$worker_amis_json" ]; then
  echo "usage: scripts/write-aws-release-manifest.sh <control-image> <worker-amis-json> [output]" >&2
  echo "set VERIFY_RELEASE_ARTIFACTS=1 to verify image and AMI visibility before writing" >&2
  exit 1
fi

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || die "missing command: $1"
}

is_truthy() {
  case "$1" in
    1 | true | TRUE | yes | YES) return 0 ;;
    *) return 1 ;;
  esac
}

if [[ ! "$control_image" =~ @sha256:[0-9a-f]{64}$ ]]; then
  echo "control image must be pinned by digest as @sha256:<64 lowercase hex characters>" >&2
  exit 1
fi

jq -e --arg required_worker_ami_regions "$required_worker_ami_regions" '
  . as $worker_amis
  |
  ($required_worker_ami_regions | split(",") | map(gsub("^\\s+|\\s+$"; "")) | map(select(length > 0))) as $required_regions
  | type == "object"
  and all($required_regions[]; . as $region | ($worker_amis | has($region)) and ($worker_amis[$region] | type == "string" and test("^ami-[0-9a-f]{8,}$")))
  and all(keys[]; test("^[a-z]{2}-[a-z-]+-[0-9]+$"))
  and all(.[]; type == "string" and test("^ami-[0-9a-f]{8,}$"))
' >/dev/null <<<"$worker_amis_json"

verify_control_image() {
  if command -v docker >/dev/null 2>&1; then
    if docker buildx imagetools inspect "$control_image" >/dev/null 2>&1; then
      return 0
    fi
    if docker manifest inspect "$control_image" >/dev/null 2>&1; then
      return 0
    fi
  fi

  if command -v skopeo >/dev/null 2>&1 && skopeo inspect "docker://${control_image}" >/dev/null 2>&1; then
    return 0
  fi

  die "control image is not inspectable: ${control_image}"
}

verify_worker_amis() {
  need_command aws

  while IFS=$'\t' read -r region ami_id; do
    described_ami_id="$(
      aws ec2 describe-images \
        --region "$region" \
        --image-ids "$ami_id" \
        --query 'Images[0].ImageId' \
        --output text
    )" || die "worker AMI is not visible in ${region}: ${ami_id}"

    [ "$described_ami_id" = "$ami_id" ] || die "worker AMI lookup returned ${described_ami_id} in ${region}, expected ${ami_id}"
  done < <(jq -r 'to_entries[] | [.key, .value] | @tsv' <<<"$worker_amis_json")
}

if is_truthy "$verify_release_artifacts"; then
  verify_control_image
  verify_worker_amis
fi

jq -n \
  --arg control_image "$control_image" \
  --argjson worker_amis "$worker_amis_json" \
  '{
    control_image: $control_image,
    worker_amis: $worker_amis
  }' >"$output"
