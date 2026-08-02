#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="${root}/scripts/materialize-platform-release.sh"

rg -F 'nixos/nix:2.31.2@sha256:c7cc6c8cb5d81bed19997247629604708fda95c99c43ac362daa05b6a68e8a24' "${script}" >/dev/null
rg -F '/work#packages.x86_64-linux.platformRelease' "${script}" >/dev/null
rg -F 'target=/work,readonly' "${script}" >/dev/null
rg -F 'chown -R "${HOST_UID}:${HOST_GID}" /output' "${script}" >/dev/null
if rg -F -- '--privileged' "${script}" >/dev/null || rg -F 'seccomp=unconfined' "${script}" >/dev/null; then
  printf 'not ok - platform release builder must not receive elevated container privileges\n' >&2
  exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
mkdir "${tmp}/existing"
if "${script}" "${tmp}/existing" >"${tmp}/stdout" 2>"${tmp}/stderr"; then
  printf 'not ok - existing output must fail closed\n' >&2
  exit 1
fi
grep -Fq 'output already exists' "${tmp}/stderr"

printf 'ok - platform release materializer contract\n'
