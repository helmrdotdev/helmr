#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILDER_IMAGE="nixos/nix:2.31.2@sha256:c7cc6c8cb5d81bed19997247629604708fda95c99c43ac362daa05b6a68e8a24"

if [ "$#" != 1 ]; then
  printf 'usage: scripts/materialize-linux-worker-host-bundle.sh OUTPUT_DIRECTORY\n' >&2
  exit 2
fi
output=$1
[ ! -e "${output}" ] || { printf 'Worker host bundle output already exists: %s\n' "${output}" >&2; exit 1; }
[ -z "$(git -C "${ROOT}" status --porcelain --untracked-files=all)" ] || {
  printf 'Worker host bundle requires a clean Product checkout\n' >&2
  exit 1
}
command -v docker >/dev/null 2>&1 || { printf 'docker is required to build the Linux/AMD64 Worker host bundle\n' >&2; exit 1; }
command -v tar >/dev/null 2>&1 || { printf 'tar is required to build the Linux/AMD64 Worker host bundle\n' >&2; exit 1; }

mkdir -p "$(dirname "${output}")"
output="$(cd "$(dirname "${output}")" && pwd)/$(basename "${output}")"
source_dir="$(mktemp -d "${TMPDIR:-/tmp}/helmr-worker-host-source.XXXXXX")"
host_dir="$(mktemp -d "${TMPDIR:-/tmp}/helmr-worker-host-package.XXXXXX")"
chmod 0700 "${source_dir}" "${host_dir}"
make_private_trees_removable() {
  find "${source_dir}" "${host_dir}" -type d -exec chmod u+w {} +
}
cleanup() {
  status=$?
  make_private_trees_removable >/dev/null 2>&1 || true
  rm -rf "${source_dir}" "${host_dir}"
  exit "${status}"
}
trap cleanup EXIT

git -C "${ROOT}" archive --format=tar HEAD | tar -xf - -C "${source_dir}"
[ ! -e "${source_dir}/.git" ] || { printf 'Worker host source export contains Git metadata\n' >&2; exit 1; }

docker run --rm \
  --platform linux/amd64 \
  --mount "type=bind,source=${source_dir},target=/work,readonly" \
  -w /work \
  "${BUILDER_IMAGE}" \
  sh -ceu '
    host="$(nix --extra-experimental-features "nix-command flakes" \
      build --no-link --print-out-paths \
      --option sandbox false \
      --option filter-syscalls false \
      path:/work#packages.x86_64-linux.workerHost)"
    tar -C "${host}" -cf - .
  ' | tar -xof - -C "${host_dir}"

"${ROOT}/scripts/materialize-worker-host-bundle.sh" "${output}" "${host_dir}" >/dev/null
make_private_trees_removable
rm -rf "${source_dir}" "${host_dir}"
trap - EXIT
printf '%s\n' "${output}"
