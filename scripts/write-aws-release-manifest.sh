#!/usr/bin/env bash
set -euo pipefail

control_image="${1:-}"
worker_amis_json="${2:-}"
output="${3:-aws-artifacts.json}"
required_worker_ami_regions="${REQUIRED_WORKER_AMI_REGIONS:-us-east-1,us-west-2,ap-northeast-1}"

if [ -z "$control_image" ] || [ -z "$worker_amis_json" ]; then
  echo "usage: scripts/write-aws-release-manifest.sh <control-image> <worker-amis-json> [output]" >&2
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

jq -n \
  --arg control_image "$control_image" \
  --argjson worker_amis "$worker_amis_json" \
  '{
    control_image: $control_image,
    worker_amis: $worker_amis
  }' >"$output"
