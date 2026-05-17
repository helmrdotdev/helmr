#!/usr/bin/env bash
set -euo pipefail

control_image="${1:-}"
worker_amis_json="${2:-}"
output="${3:-aws-artifacts.json}"

if [ -z "$control_image" ] || [ -z "$worker_amis_json" ]; then
  echo "usage: scripts/write-aws-release-manifest.sh <control-image> <worker-amis-json> [output]" >&2
  exit 1
fi

jq -e '
  type == "object"
  and all(keys[]; type == "string" and length > 0)
  and all(.[]; type == "string" and length > 0)
' >/dev/null <<<"$worker_amis_json"

jq -n \
  --arg control_image "$control_image" \
  --argjson worker_amis "$worker_amis_json" \
  '{
    control_image: $control_image,
    worker_amis: $worker_amis
  }' >"$output"
