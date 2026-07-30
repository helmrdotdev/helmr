#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
image_uri="${1:-}"
platform="${CONTROL_IMAGE_PLATFORM:-linux/amd64}"
os="${platform%%/*}"
arch="${platform#*/}"
arch="${arch%%/*}"
context="${CONTROL_IMAGE_CONTEXT:-$repo_root/dist/control-image}"
build_contract="$repo_root/images/control-image-build.json"
build_version="${HELMR_BUILD_VERSION:-}"
ldflags="-s -w"

if [ -z "$image_uri" ]; then
  echo "usage: scripts/build-control-image.sh <image-uri>" >&2
  exit 1
fi
case "$os/$arch" in
  linux/amd64) ;;
  *)
    echo "unsupported CONTROL_IMAGE_PLATFORM: $platform" >&2
    exit 1
    ;;
esac
base_image="$(jq -er '.baseImage | select(test("@sha256:[0-9a-f]{64}$"))' "$build_contract")"
jq -e --arg platform "$platform" '
  .formatVersion == 0 and
  (.platforms | index($platform)) != null
' "$build_contract" >/dev/null || {
  echo "CONTROL_IMAGE_PLATFORM is not allowed by images/control-image-build.json: $platform" >&2
  exit 1
}
if [ -n "$build_version" ]; then
  ldflags="$ldflags -X github.com/helmrdotdev/helmr/internal/version.Version=$build_version"
fi

rm -rf "$context"
mkdir -p "$context"

cd "$repo_root"
bun install --frozen-lockfile --ignore-scripts
make console-build

for command in helmr-control helmr-dispatcher; do
  GOFLAGS='' GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build \
    -tags embed_console \
    -trimpath \
    -ldflags="$ldflags" \
    -o "$context/$command" \
    "./cmd/$command"
done

cat >"$context/Dockerfile" <<EOF
FROM ${base_image}
COPY helmr-control /usr/local/bin/helmr-control
COPY helmr-dispatcher /usr/local/bin/helmr-dispatcher
ENTRYPOINT ["/usr/local/bin/helmr-control"]
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
source_commit="$(git -C "$repo_root" rev-parse HEAD)"
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
  --arg source_commit "$source_commit" \
  '{
    baseImage: $base_image,
    buildVersion: $build_version,
    formatVersion: 1,
    localImageId: $local_image_id,
    platform: $platform,
    sourceCommit: $source_commit,
    toolchain: {
      kind: "nix-flake-lock",
      sha256: $flake_lock_sha256
    }
  }' >"$context/build-inputs.json"
