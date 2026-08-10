#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

die() {
  printf 'materialize Worker host bundle: %s\n' "$*" >&2
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

file_size() {
  if stat -c '%s' "$1" >/dev/null 2>&1; then
    stat -c '%s' "$1"
  else
    stat -f '%z' "$1"
  fi
}

[ "$#" -eq 2 ] || die "usage: $0 OUTPUT_DIR WORKER_HOST_DIR"
output=$1
host_dir=$2
[ ! -e "${output}" ] || die "output already exists: ${output}"
[ -d "${host_dir}/bin" ] || die "Worker host bin directory does not exist: ${host_dir}/bin"

files=(firecracker helmr-worker jailer)
for name in "${files[@]}"; do
  path="${host_dir}/bin/${name}"
  [ ! -L "${path}" ] && [ -f "${path}" ] && [ -x "${path}" ] ||
    die "Worker host executable must be a regular executable non-symlink file: ${path}"
done

if command -v file >/dev/null 2>&1; then
  file "${host_dir}/bin/helmr-worker" | grep -F 'ELF 64-bit' >/dev/null ||
    die "Worker executable is not a 64-bit ELF binary"
  file "${host_dir}/bin/helmr-worker" | grep -Eq 'x86-64|x86_64' ||
    die "Worker executable is not built for x86_64"
fi

source_commit="$(git -C "${root}" rev-parse HEAD)"
version_file="${host_dir}/share/helmr-worker/source-commit"
[ ! -L "${version_file}" ] && [ -f "${version_file}" ] ||
  die "Worker host source commit must be a regular non-symlink file: ${version_file}"
worker_version="$(tr -d '\n' <"${version_file}")"
[ "${worker_version}" = "${source_commit}" ] ||
  die "Worker host source commit ${worker_version} does not match ${source_commit}"

parent="$(dirname "${output}")"
mkdir -p "${parent}"
work="$(mktemp -d "${parent}/.worker-host-bundle.XXXXXX")"
trap 'rm -rf "${work}"' EXIT
result="${work}/result"
payload="${work}/payload"
mkdir "${result}" "${payload}"

file_entries='[]'
for name in "${files[@]}"; do
  install -m 0755 "${host_dir}/bin/${name}" "${payload}/${name}"
  file_entries="$(
    jq -cn \
      --argjson files "${file_entries}" \
      --arg path "${name}" \
      --arg digest "sha256:$(sha256_file "${payload}/${name}")" \
      --argjson size_bytes "$(file_size "${payload}/${name}")" \
      '$files + [{path: $path, mode: "0755", size_bytes: $size_bytes, digest: $digest}]'
  )"
done

jq -cn \
  --arg source_commit "${source_commit}" \
  --arg worker_version "${worker_version}" \
  --argjson files "${file_entries}" '
  {
    schema: "helmr.worker-host-artifacts.v0",
    arch: "amd64",
    sourceCommit: $source_commit,
    workerVersion: $worker_version,
    files: $files
  }
' >"${payload}/worker-host-artifacts.json"
chmod 0644 "${payload}/worker-host-artifacts.json"

members=(firecracker helmr-worker jailer worker-host-artifacts.json)
tar \
  --sort=name \
  --mtime='@0' \
  --owner=0 \
  --group=0 \
  --numeric-owner \
  -C "${payload}" \
  -cf "${result}/worker-host-artifacts.tar" \
  "${members[@]}"

bundle_digest="sha256:$(sha256_file "${result}/worker-host-artifacts.tar")"
manifest_digest="sha256:$(sha256_file "${payload}/worker-host-artifacts.json")"
install -m 0600 "${payload}/worker-host-artifacts.json" "${result}/worker-host-artifacts.json"
jq -cn \
  --arg bundle_digest "${bundle_digest}" \
  --arg manifest_digest "${manifest_digest}" \
  --arg source_commit "${source_commit}" \
  --arg worker_version "${worker_version}" '
  {
    schema: "helmr.worker-host-bundle.v0",
    sourceCommit: $source_commit,
    workerVersion: $worker_version,
    bundle: {path: "worker-host-artifacts.tar", digest: $bundle_digest},
    manifest: {path: "worker-host-artifacts.json", digest: $manifest_digest}
  }
' >"${result}/worker-host-bundle.json"
chmod 0600 "${result}/"*

rm -rf "${payload}"
mv "${result}" "${output}"
trap - EXIT
rmdir "${work}"
printf '%s\n' "${output}"
