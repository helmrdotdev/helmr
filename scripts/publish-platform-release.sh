#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILDER_IMAGE="nixos/nix:2.31.2@sha256:c7cc6c8cb5d81bed19997247629604708fda95c99c43ac362daa05b6a68e8a24"

if [ "$#" != 5 ]; then
  printf 'usage: scripts/publish-platform-release.sh STORE_URI RELEASE_TAG ARCHIVE PROVENANCE SIGSTORE_BUNDLE\n' >&2
  exit 2
fi
store_uri=$1
release_tag=$2
archive=$3
provenance=$4
sigstore_bundle=$5
git check-ref-format --allow-onelevel "${release_tag}" >/dev/null
for input_file in "${archive}" "${provenance}" "${sigstore_bundle}"; do
  [ -f "${input_file}" ] || { printf 'platform release input is missing: %s\n' "${input_file}" >&2; exit 1; }
done
[ -z "$(git -C "${ROOT}" status --porcelain --untracked-files=all)" ] || {
  printf 'platform release publication requires a clean Product checkout\n' >&2
  exit 1
}
command -v docker >/dev/null 2>&1 || { printf 'docker is required to publish the linux/amd64 platform release\n' >&2; exit 1; }
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

archive="$(cd "$(dirname "${archive}")" && pwd)/$(basename "${archive}")"
provenance="$(cd "$(dirname "${provenance}")" && pwd)/$(basename "${provenance}")"
sigstore_bundle="$(cd "$(dirname "${sigstore_bundle}")" && pwd)/$(basename "${sigstore_bundle}")"
cosign verify-blob \
  --bundle "${sigstore_bundle}" \
  --certificate-identity "https://github.com/helmrdotdev/helmr/.github/workflows/release.yaml@refs/tags/${release_tag}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "${archive}"

if command -v sha256sum >/dev/null 2>&1; then
  archive_sha256="sha256:$(sha256sum "${archive}" | awk '{print $1}')"
else
  archive_sha256="sha256:$(shasum -a 256 "${archive}" | awk '{print $1}')"
fi
archive_size="$(wc -c <"${archive}" | tr -d ' ')"
jq -e \
  --arg digest "${archive_sha256}" \
  --argjson size "${archive_size}" \
  --arg sourceCommit "${source_commit}" \
  --arg sourceRef "refs/tags/${release_tag}" \
  '.formatVersion == 0 and .archive.digest == $digest and .archive.sizeBytes == $size and .sourceCommit == $sourceCommit and .sourceRef == $sourceRef' \
  "${provenance}" >/dev/null || {
  printf 'Platform release provenance does not match the signed archive and checked-out tag\n' >&2
  exit 1
}

input="$(mktemp -d)"
trap 'rm -rf "${input}"' EXIT
tar -xf "${archive}" -C "${input}"
[ -f "${input}/platform-release.json" ] || { printf 'signed Platform release omits platform-release.json\n' >&2; exit 1; }
docker run --rm \
  --platform linux/amd64 \
  --env AWS_ACCESS_KEY_ID \
  --env AWS_SECRET_ACCESS_KEY \
  --env AWS_SESSION_TOKEN \
  --env AWS_REGION \
  --env AWS_DEFAULT_REGION \
  --env "PLATFORM_STORE_URI=${store_uri}" \
  --mount "type=bind,source=${ROOT},target=/work,readonly" \
  --mount "type=bind,source=${input},target=/input,readonly" \
  -w /work \
  "${BUILDER_IMAGE}" \
  sh -ceu '
    nix --extra-experimental-features "nix-command flakes" \
      --option sandbox false \
      --option filter-syscalls false \
      develop /work \
      -c go -C /work run ./cmd/helmr-controlplane release publish \
        --store "${PLATFORM_STORE_URI}" \
        --input /input
  '
