#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="${root}/scripts/materialize-platform-release.sh"

rg -F 'nixos/nix:2.31.2@sha256:c7cc6c8cb5d81bed19997247629604708fda95c99c43ac362daa05b6a68e8a24' "${script}" >/dev/null
rg -F 'git -C "${ROOT}" archive --format=tar HEAD | tar -xf - -C "${source_dir}"' "${script}" >/dev/null
rg -F 'path:/work#packages.x86_64-linux.platformRelease' "${script}" >/dev/null
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

git init -q "${tmp}/repo"
git -C "${tmp}/repo" config user.email test@example.invalid
git -C "${tmp}/repo" config user.name test
printf 'tracked\n' >"${tmp}/repo/input"
git -C "${tmp}/repo" add input
git -C "${tmp}/repo" commit -qm initial
git -C "${tmp}/repo" worktree add -q --detach "${tmp}/worktree"
git_dir="$(git -C "${tmp}/worktree" rev-parse --absolute-git-dir)"
mkfifo "${git_dir}/unsupported-entry"
mkdir "${tmp}/export"
git -C "${tmp}/worktree" archive --format=tar HEAD | tar -xf - -C "${tmp}/export"
[ "$(cat "${tmp}/export/input")" = tracked ]
[ ! -e "${tmp}/export/.git" ]

printf 'ok - platform release materializer contract\n'
