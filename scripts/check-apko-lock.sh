#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
arch="${ARCH:-x86_64}"

[ "${arch}" = "x86_64" ] || {
  printf 'unsupported ARCH: %s\n' "${arch}" >&2
  exit 1
}

for config in "${repo_root}"/images/*/apko.yaml; do
  [ -f "${config}" ] || continue
  role_dir="$(dirname "${config}")"
  lock="${role_dir}/apko.${arch}.lock.json"
  [ -f "${lock}" ] || {
    printf 'missing apko lock: %s\n' "${lock}" >&2
    exit 1
  }

  generated="$(mktemp "${TMPDIR:-/tmp}/helmr-apko-lock.XXXXXX.json")"
  trap 'rm -f "${generated}"' EXIT
  (
    cd "${role_dir}"
    apko lock "$(basename "${config}")" --arch "${arch}" --output "${generated}" >/dev/null
  )
  jq -n -e --slurpfile expected "${lock}" --slurpfile actual "${generated}" \
    '$expected[0].version == $actual[0].version and $expected[0].config == $actual[0].config' >/dev/null ||
    {
      printf 'apko lock config is stale: %s\n' "${lock}" >&2
      exit 1
    }
  rm -f "${generated}"
  trap - EXIT
done
