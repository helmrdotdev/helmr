#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="${root}/scripts/materialize-platform-release.sh"

rg -q 'nixos/nix:[^@[:space:]]+@sha256:[0-9a-f]{64}' "${script}"
rg -F 'git -C "${ROOT}" archive --format=tar HEAD | tar -xf - -C "${source_dir}"' "${script}" >/dev/null
rg -F 'path:/work#packages.x86_64-linux.platformRelease' "${script}" >/dev/null
rg -F 'path:/work#checks.x86_64-linux.platform-release-publish-contract' "${script}" >/dev/null
rg -F 'source=${source_dir},target=/work,readonly' "${script}" >/dev/null
if rg -F 'source=${ROOT},target=/work' "${script}" >/dev/null; then
  printf 'not ok - platform release must not mount the repository metadata into the builder\n' >&2
  exit 1
fi
rg -F 'tar -C "${release}" -cf - .' "${script}" >/dev/null
rg -F "' | tar -xof - -C \"\${output}\"" "${script}" >/dev/null
if rg -F 'target=/output' "${script}" >/dev/null || rg -F 'chown ' "${script}" >/dev/null; then
  printf 'not ok - platform release output must be materialized by the host user\n' >&2
  exit 1
fi
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
