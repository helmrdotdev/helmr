#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

die() {
  printf 'materialize Worker runtime bundle: %s\n' "$*" >&2
  exit 1
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    die "sha256sum or shasum is required"
  fi
}

[ "$#" -eq 2 ] || die "usage: $0 OUTPUT_DIR RUNTIME_ARTIFACTS_DIR"
output=$1
artifacts_dir=$2
[ ! -e "${output}" ] || die "output already exists: ${output}"
[ -d "${artifacts_dir}" ] || die "runtime artifacts directory does not exist: ${artifacts_dir}"

files=(initramfs rootfs.squashfs runtime-artifacts.json vmlinuz)
for name in "${files[@]}"; do
  path="${artifacts_dir}/${name}"
  [ ! -L "${path}" ] && [ -f "${path}" ] || die "runtime artifact must be a regular non-symlink file: ${path}"
done

parent="$(dirname "${output}")"
mkdir -p "${parent}"
work="$(mktemp -d "${parent}/.worker-runtime-bundle.XXXXXX")"
trap 'rm -rf "${work}"' EXIT
result="${work}/result"
mkdir "${result}"

install -m 0600 "${artifacts_dir}/runtime-artifacts.json" "${result}/runtime-artifacts.json"
tar \
  --sort=name \
  --mtime='@0' \
  --owner=0 \
  --group=0 \
  --numeric-owner \
  --sparse \
  -C "${artifacts_dir}" \
  -cf "${result}/runtime-artifacts.tar" \
  "${files[@]}"

bundle_digest="sha256:$(sha256_file "${result}/runtime-artifacts.tar")"
manifest_digest="sha256:$(sha256_file "${result}/runtime-artifacts.json")"
source_commit="$(git -C "${root}" rev-parse HEAD)"
jq -cn \
  --arg bundle_digest "${bundle_digest}" \
  --arg manifest_digest "${manifest_digest}" \
  --arg source_commit "${source_commit}" '
  {
    schema: "helmr.worker-runtime-bundle.v0",
    sourceCommit: $source_commit,
    bundle: {path: "runtime-artifacts.tar", digest: $bundle_digest},
    runtimeArtifactsManifest: {path: "runtime-artifacts.json", digest: $manifest_digest}
  }
' >"${result}/worker-runtime-bundle.json"
chmod 0600 "${result}/"*

mv "${result}" "${output}"
trap - EXIT
rmdir "${work}"
printf '%s\n' "${output}"
