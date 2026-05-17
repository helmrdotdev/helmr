#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

"$repo_root/scripts/write-aws-release-manifest.sh" \
  "ghcr.io/helmrdotdev/helmr-control@sha256:abc123" \
  '{"us-east-1":"ami-0123456789abcdef0","ap-northeast-1":"ami-0fedcba9876543210"}' \
  "$tmp/aws-artifacts.json"

control_image="$(jq -r '.control_image' "$tmp/aws-artifacts.json")"
[ "$control_image" = "ghcr.io/helmrdotdev/helmr-control@sha256:abc123" ] || fail "control image mismatch"

worker_ami="$(jq -r '.worker_amis["ap-northeast-1"]' "$tmp/aws-artifacts.json")"
[ "$worker_ami" = "ami-0fedcba9876543210" ] || fail "worker AMI mismatch"

if "$repo_root/scripts/write-aws-release-manifest.sh" "image" '[]' "$tmp/invalid.json" 2>/dev/null; then
  fail "array worker AMI JSON should fail"
fi

printf 'ok - release manifest tests\n'
