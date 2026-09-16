#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ "$#" != 6 ]; then
  printf 'usage: scripts/publish-platform-release.sh STORE_URI RELEASE_TAG INDEX INDEX_SIGNATURE ARCHIVE PROVENANCE\n' >&2
  exit 2
fi
store_uri=$1
release_tag=$2
index=$3
sigstore_bundle=$4
archive=$5
provenance=$6
git check-ref-format --allow-onelevel "${release_tag}" >/dev/null
for input_file in "${index}" "${archive}" "${provenance}" "${sigstore_bundle}"; do
  [ -f "${input_file}" ] || { printf 'platform release input is missing: %s\n' "${input_file}" >&2; exit 1; }
done
[ -z "$(git -C "${ROOT}" status --porcelain --untracked-files=all)" ] || {
  printf 'platform release publication requires a clean Product checkout\n' >&2
  exit 1
}
command -v cosign >/dev/null 2>&1 || { printf 'cosign is required to verify the Platform release\n' >&2; exit 1; }
tag_commit="$(git -C "${ROOT}" rev-parse --verify "refs/tags/${release_tag}^{commit}")" || {
  printf 'checked-out Product repository does not contain release tag %s\n' "${release_tag}" >&2
  exit 1
}
source_commit="$(git -C "${ROOT}" rev-parse HEAD)"
[ "${tag_commit}" = "${source_commit}" ] || {
  printf 'Product checkout is not at release tag %s\n' "${release_tag}" >&2
  exit 1
}

work="$(mktemp -d)"
chmod 0700 "${work}"
trap 'rm -rf "${work}"' EXIT
python3 "${ROOT}/scripts/release/platform.py" "$index" "$sigstore_bundle" "$archive" "$provenance" "$release_tag" "$source_commit" "$work/input"
"${ROOT}/scripts/publish-materialized-platform-release.sh" "${store_uri}" "$work/input"
