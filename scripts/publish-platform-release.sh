#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILDER_IMAGE="nixos/nix:2.31.2@sha256:c7cc6c8cb5d81bed19997247629604708fda95c99c43ac362daa05b6a68e8a24"

if [ "$#" != 2 ]; then
  printf 'usage: scripts/publish-platform-release.sh STORE_URI INPUT_DIRECTORY\n' >&2
  exit 2
fi
store_uri=$1
input=$2
[ -d "${input}" ] || { printf 'platform release input is not a directory: %s\n' "${input}" >&2; exit 1; }
[ -z "$(git -C "${ROOT}" status --porcelain --untracked-files=all)" ] || {
  printf 'platform release publication requires a clean Product checkout\n' >&2
  exit 1
}
command -v docker >/dev/null 2>&1 || { printf 'docker is required to publish the linux/amd64 platform release\n' >&2; exit 1; }

input="$(cd "${input}" && pwd)"
docker run --rm \
  --platform linux/amd64 \
  --env AWS_ACCESS_KEY_ID \
  --env AWS_SECRET_ACCESS_KEY \
  --env AWS_SESSION_TOKEN \
  --env AWS_REGION \
  --env AWS_DEFAULT_REGION \
  --env "PLATFORM_STORE_URI=${store_uri}" \
  --mount "type=bind,source=${ROOT},target=/work,readonly" \
  --mount "type=bind,source=${input},target=/input,readonly" \
  -w /work \
  "${BUILDER_IMAGE}" \
  sh -ceu '
    nix --extra-experimental-features "nix-command flakes" \
      --option sandbox false \
      --option filter-syscalls false \
      develop /work \
      -c go -C /work run ./cmd/helmr-controlplane release publish \
        --store "${PLATFORM_STORE_URI}" \
        --input /input
  '
