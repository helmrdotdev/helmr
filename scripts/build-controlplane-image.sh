#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
image_uri="${1:-}"
platform="${CONTROLPLANE_IMAGE_PLATFORM:-linux/amd64}"
os="${platform%%/*}"
arch="${platform#*/}"
arch="${arch%%/*}"
context="${CONTROLPLANE_IMAGE_CONTEXT:-$repo_root/dist/controlplane-image}"
build_contract="$repo_root/images/controlplane-image-build.json"
nix_builder_image="nixos/nix:2.31.2@sha256:c7cc6c8cb5d81bed19997247629604708fda95c99c43ac362daa05b6a68e8a24"
build_version="${RELEASE_TAG:-${HELMR_BUILD_VERSION:-}}"
source_commit="$(git -C "$repo_root" rev-parse HEAD)"
ldflags="-s -w"

if [ -z "$image_uri" ]; then
  echo "usage: scripts/build-controlplane-image.sh <image-uri>" >&2
  exit 1
fi
[ -z "$(git -C "$repo_root" status --porcelain --untracked-files=all)" ] || {
  echo "Control Plane image requires a clean Product checkout" >&2
  exit 1
}
case "$os/$arch" in
  linux/amd64) ;;
  *)
    echo "unsupported CONTROLPLANE_IMAGE_PLATFORM: $platform" >&2
    exit 1
    ;;
esac
base_image="$(jq -er '.baseImage | select(test("@sha256:[0-9a-f]{64}$"))' "$build_contract")"
jq -e --arg platform "$platform" '
  .formatVersion == 0 and
  (.platforms | index($platform)) != null
' "$build_contract" >/dev/null || {
  echo "CONTROLPLANE_IMAGE_PLATFORM is not allowed by images/controlplane-image-build.json: $platform" >&2
  exit 1
}
if [ -n "$build_version" ]; then
  ldflags="$ldflags -X github.com/helmrdotdev/helmr/internal/version.Version=$build_version"
fi
ldflags="$ldflags -X github.com/helmrdotdev/helmr/internal/version.SourceCommit=$source_commit"

rm -rf "$context"
mkdir -p "$context"

cd "$repo_root"
bun install --frozen-lockfile --ignore-scripts
make console-build
[ -z "$(git -C "$repo_root" status --porcelain --untracked-files=all)" ] || {
  echo "Control Plane console build differs from the committed Product source" >&2
  exit 1
}
source_dir="$(mktemp -d "${TMPDIR:-/tmp}/helmr-controlplane-image-source.XXXXXX")"
chmod 0700 "$source_dir"
cleanup_source() {
  rm -rf "$source_dir"
}
trap cleanup_source EXIT
git -C "$repo_root" archive --format=tar HEAD | tar -xf - -C "$source_dir"
[ ! -e "$source_dir/.git" ] || {
  echo "Control Plane image source export contains Git metadata" >&2
  exit 1
}
docker run --rm \
  --platform linux/amd64 \
  --mount "type=bind,source=${source_dir},target=/work,readonly" \
  -w /work \
  "$nix_builder_image" \
  sh -ceu '
    release="$(nix --extra-experimental-features "nix-command flakes" \
      build --no-link --print-out-paths \
      --option sandbox false \
      --option filter-syscalls false \
      path:/work#packages.x86_64-linux.runtimeRelease)"
    timezone_data="$(nix --extra-experimental-features "nix-command flakes" \
      build --no-link --print-out-paths \
      --option sandbox false \
      --option filter-syscalls false \
      path:/work#packages.x86_64-linux.timezoneData)"
    stage="$(mktemp -d)"
    cp "$release/runtime.descriptor.json" "$stage/runtime.descriptor.json"
    cp -a "$timezone_data/zoneinfo" "$stage/zoneinfo"
    cp "$timezone_data/tzdb_names.txt" "$stage/tzdb_names.txt"
    tar -C "$stage" -cf - runtime.descriptor.json zoneinfo tzdb_names.txt
  ' | tar -C "$context" -xf -
rm -rf "$source_dir"
trap - EXIT
chmod 0444 "$context/runtime.descriptor.json" "$context/tzdb_names.txt"
chmod -R u+rwX,go+rX,go-w "$context/zoneinfo"

for command in helmr-controlplane helmr-dispatcher; do
  GOFLAGS='' GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build \
    -tags embed_console \
    -trimpath \
    -ldflags="$ldflags" \
    -o "$context/$command" \
    "./cmd/$command"
done
if [ -n "$build_version" ]; then
  expected_identity="$build_version ($source_commit)"
  for command in helmr-controlplane helmr-dispatcher; do
    [ "$("$context/$command" --version)" = "$expected_identity" ] || {
      echo "$command does not report the release cohort identity" >&2
      exit 1
    }
  done
fi

cat >"$context/Dockerfile" <<EOF
FROM ${base_image}
COPY helmr-controlplane /usr/local/bin/helmr-controlplane
COPY helmr-dispatcher /usr/local/bin/helmr-dispatcher
COPY runtime.descriptor.json /usr/local/share/helmr/runtime.descriptor.json
COPY zoneinfo/ /usr/share/zoneinfo/
COPY tzdb_names.txt /usr/local/share/helmr/tzdb_names.txt
ENTRYPOINT ["/usr/local/bin/helmr-controlplane"]
EOF

docker build \
  --platform "$platform" \
  -t "$image_uri" \
  "$context"

local_image_id="$(docker image inspect --format '{{.Id}}' "$image_uri")"
printf '%s\n' "$local_image_id" | grep -Eq '^sha256:[0-9a-f]{64}$' || {
  echo "docker did not return a content-addressed local image identity for $image_uri" >&2
  exit 1
}
if command -v sha256sum >/dev/null 2>&1; then
  runtime_descriptor_sha256="$(sha256sum "$context/runtime.descriptor.json" | awk '{print $1}')"
  timezone_manifest_sha256="$(sha256sum "$context/tzdb_names.txt" | awk '{print $1}')"
else
  runtime_descriptor_sha256="$(shasum -a 256 "$context/runtime.descriptor.json" | awk '{print $1}')"
  timezone_manifest_sha256="$(shasum -a 256 "$context/tzdb_names.txt" | awk '{print $1}')"
fi
if command -v sha256sum >/dev/null 2>&1; then
  flake_lock_sha256="$(sha256sum "$repo_root/flake.lock" | awk '{print $1}')"
else
  flake_lock_sha256="$(shasum -a 256 "$repo_root/flake.lock" | awk '{print $1}')"
fi
jq -cn \
  --arg base_image "$base_image" \
  --arg build_version "$build_version" \
  --arg flake_lock_sha256 "$flake_lock_sha256" \
  --arg local_image_id "$local_image_id" \
  --arg platform "$platform" \
  --arg runtime_descriptor_sha256 "$runtime_descriptor_sha256" \
  --arg source_commit "$source_commit" \
  --arg timezone_manifest_sha256 "$timezone_manifest_sha256" \
  '{
    baseImage: $base_image,
    buildVersion: $build_version,
    formatVersion: 1,
    localImageId: $local_image_id,
    platform: $platform,
    runtimeDescriptorSha256: $runtime_descriptor_sha256,
    sourceCommit: $source_commit,
    timezoneManifestSha256: $timezone_manifest_sha256,
    toolchain: {
      kind: "nix-flake-lock",
      sha256: $flake_lock_sha256
    }
  }' >"$context/build-inputs.json"
