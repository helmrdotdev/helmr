#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
receipt="${1:-}"
image_uri="${2:-}"
platform="${CONTROLPLANE_IMAGE_PLATFORM:-linux/amd64}"
build_version="${HELMR_BUILD_VERSION:-}"
docker_bin="${DOCKER_BIN:-docker}"

if [ -z "${receipt}" ] || [ -z "${image_uri}" ]; then
  printf 'usage: scripts/verify-controlplane-image-build.sh <build-inputs.json> <image-uri>\n' >&2
  exit 1
fi
[ -f "${receipt}" ] || {
  printf 'Control Plane image build-input receipt is missing: %s\n' "${receipt}" >&2
  exit 1
}

base_image="$(jq -er '.baseImage' "${repo_root}/images/controlplane-image-build.json")"
source_commit="$(git -C "${repo_root}" rev-parse HEAD)"
local_image_id="$("${docker_bin}" image inspect --format '{{.Id}}' "${image_uri}")"
printf '%s\n' "${local_image_id}" | grep -Eq '^sha256:[0-9a-f]{64}$' || {
  printf 'Control Plane image does not have a content-addressed local identity\n' >&2
  exit 1
}
descriptor_tmp="$(mktemp -d)"
container_id=""
cleanup() {
  if [ -n "${container_id}" ]; then
    "${docker_bin}" rm -f "${container_id}" >/dev/null 2>&1 || true
  fi
  rm -rf "${descriptor_tmp}"
}
trap cleanup EXIT
container_id="$("${docker_bin}" create "${image_uri}")"
"${docker_bin}" cp \
  "${container_id}:/usr/local/share/helmr/runtime.descriptor.json" \
  "${descriptor_tmp}/runtime.descriptor.json"
if command -v sha256sum >/dev/null 2>&1; then
  image_runtime_descriptor="$(sha256sum "${descriptor_tmp}/runtime.descriptor.json" | awk '{print $1}')"
else
  image_runtime_descriptor="$(shasum -a 256 "${descriptor_tmp}/runtime.descriptor.json" | awk '{print $1}')"
fi
"${docker_bin}" rm -f "${container_id}" >/dev/null
container_id=""
printf '%s\n' "${image_runtime_descriptor}" | grep -Eq '^[0-9a-f]{64}$' || {
  printf 'Control Plane image does not contain the canonical Runtime descriptor\n' >&2
  exit 1
}
if command -v sha256sum >/dev/null 2>&1; then
  flake_lock_sha256="$(sha256sum "${repo_root}/flake.lock" | awk '{print $1}')"
else
  flake_lock_sha256="$(shasum -a 256 "${repo_root}/flake.lock" | awk '{print $1}')"
fi

jq -e \
  --arg base_image "${base_image}" \
  --arg build_version "${build_version}" \
  --arg flake_lock_sha256 "${flake_lock_sha256}" \
  --arg local_image_id "${local_image_id}" \
  --arg platform "${platform}" \
  --arg runtime_descriptor_sha256 "${image_runtime_descriptor}" \
  --arg source_commit "${source_commit}" '
  . == {
    baseImage: $base_image,
    buildVersion: $build_version,
    formatVersion: 1,
    localImageId: $local_image_id,
    platform: $platform,
    runtimeDescriptorSha256: $runtime_descriptor_sha256,
    sourceCommit: $source_commit,
    toolchain: {
      kind: "nix-flake-lock",
      sha256: $flake_lock_sha256
    }
  }
' "${receipt}" >/dev/null || {
  printf 'Control Plane image build-input receipt does not match the selected local image and checkout\n' >&2
  exit 1
}
trap - EXIT
cleanup
