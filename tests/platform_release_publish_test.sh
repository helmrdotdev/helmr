#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="${root}/scripts/publish-platform-release.sh"

rg -F 'nixos/nix:2.31.2@sha256:c7cc6c8cb5d81bed19997247629604708fda95c99c43ac362daa05b6a68e8a24' "${script}" >/dev/null
rg -F 'target=/work,readonly' "${script}" >/dev/null
rg -F 'target=/input,readonly' "${script}" >/dev/null
rg -F -- '--env AWS_ACCESS_KEY_ID' "${script}" >/dev/null
rg -F 'go -C /work run ./cmd/helmr-control release publish' "${script}" >/dev/null
if rg -F -- '--privileged' "${script}" >/dev/null || rg -F 'seccomp=unconfined' "${script}" >/dev/null; then
  printf 'not ok - platform publisher must not receive elevated container privileges\n' >&2
  exit 1
fi

printf 'ok - platform release publisher contract\n'
