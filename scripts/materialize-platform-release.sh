#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILDER_IMAGE="nixos/nix:2.31.2@sha256:c7cc6c8cb5d81bed19997247629604708fda95c99c43ac362daa05b6a68e8a24"

if [ "$#" != 1 ]; then
  printf 'usage: scripts/materialize-platform-release.sh OUTPUT_DIRECTORY\n' >&2
  exit 2
fi
output=$1
[ ! -e "${output}" ] || { printf 'platform release output already exists: %s\n' "${output}" >&2; exit 1; }
[ -z "$(git -C "${ROOT}" status --porcelain --untracked-files=all)" ] || {
  printf 'platform release requires a clean Product checkout\n' >&2
  exit 1
}
command -v docker >/dev/null 2>&1 || { printf 'docker is required to build the linux/amd64 platform release\n' >&2; exit 1; }
command -v tar >/dev/null 2>&1 || { printf 'tar is required to materialize the platform release\n' >&2; exit 1; }

mkdir -p "$(dirname "${output}")"
output="$(cd "$(dirname "${output}")" && pwd)/$(basename "${output}")"
mkdir "${output}"
chmod 0700 "${output}"
source_dir="$(mktemp -d "${TMPDIR:-/tmp}/helmr-platform-release-source.XXXXXX")"
chmod 0700 "${source_dir}"
cleanup_on_error() {
  status=$?
  rm -rf "${source_dir}"
  if [ "${status}" -ne 0 ]; then
    find "${output}" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
    rmdir "${output}" 2>/dev/null || true
  fi
  exit "${status}"
}
trap cleanup_on_error EXIT

git -C "${ROOT}" archive --format=tar HEAD | tar -xf - -C "${source_dir}"
[ ! -e "${source_dir}/.git" ] || { printf 'platform release source export contains Git metadata\n' >&2; exit 1; }

docker run --rm \
  --platform linux/amd64 \
  --mount "type=bind,source=${source_dir},target=/work,readonly" \
  -w /work \
  "${BUILDER_IMAGE}" \
  sh -ceu '
    nix --extra-experimental-features "nix-command flakes" \
      build --no-link \
      --option sandbox false \
      --option filter-syscalls false \
      path:/work#checks.x86_64-linux.platform-release-publish-contract
    release="$(nix --extra-experimental-features "nix-command flakes" \
      build --no-link --print-out-paths \
      --option sandbox false \
      --option filter-syscalls false \
      path:/work#packages.x86_64-linux.platformRelease)"
    tar -C "${release}" -cf - .
  ' | tar -xof - -C "${output}"

find "${output}" -type d -exec chmod u+rwx {} +
find "${output}" -type f -exec chmod u+rw {} +

[ -f "${output}/platform-release.json" ] || { printf 'platform release manifest is missing\n' >&2; exit 1; }
"${ROOT}/scripts/check-canonical-json.sh" "${output}/platform-release.json"
rm -rf "${source_dir}"
trap - EXIT
printf '%s\n' "${output}"
