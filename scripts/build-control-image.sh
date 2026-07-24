#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
image_uri="${1:-}"
runtime_release_dir="${CONTROL_IMAGE_RUNTIME_RELEASE_DIR:-}"
manager_release_dir="${CONTROL_IMAGE_MANAGER_RELEASE_DIR:-}"

if [ -z "$image_uri" ]; then
  echo "usage: scripts/build-control-image.sh <image-uri>" >&2
  exit 1
fi
if [ -z "$runtime_release_dir" ]; then
  echo "CONTROL_IMAGE_RUNTIME_RELEASE_DIR is required" >&2
  exit 1
fi
if [ -z "$manager_release_dir" ]; then
  echo "CONTROL_IMAGE_MANAGER_RELEASE_DIR is required" >&2
  exit 1
fi
runtime_release_dir=$(cd -- "$runtime_release_dir" 2>/dev/null && pwd -P) || {
  echo "CONTROL_IMAGE_RUNTIME_RELEASE_DIR must be an existing directory" >&2
  exit 1
}
manager_release_dir=$(cd -- "$manager_release_dir" 2>/dev/null && pwd -P) || {
  echo "CONTROL_IMAGE_MANAGER_RELEASE_DIR must be an existing directory" >&2
  exit 1
}

platform="${CONTROL_IMAGE_PLATFORM:-linux/amd64}"
os="${platform%%/*}"
arch="${platform#*/}"
arch="${arch%%/*}"
context="${CONTROL_IMAGE_CONTEXT:-$repo_root/dist/control-image}"
control_binary="$context/helmr-control"
dispatcher_binary="$context/helmr-dispatcher"
build_version="${HELMR_BUILD_VERSION:-}"
ldflags="-s -w"

case "$os/$arch" in
  linux/amd64|linux/arm64) ;;
  *)
    echo "unsupported CONTROL_IMAGE_PLATFORM: $platform" >&2
    exit 1
    ;;
esac

if [ -n "$build_version" ]; then
  ldflags="$ldflags -X github.com/helmrdotdev/helmr/internal/version.Version=$build_version"
fi

rm -rf "$context"
mkdir -p "$context"
mkdir -p "$context/runtime-release"
mkdir -p "$context/toolchain-release"
mkdir -p "$context/manager-release"
for name in catalog.json catalog.sigstore.json trusted-root.json; do
  source_path="$runtime_release_dir/$name"
  if [ ! -f "$source_path" ] || [ -L "$source_path" ]; then
    echo "verified runtime release is missing regular file: $name" >&2
    exit 1
  fi
  install -m 0444 "$source_path" "$context/runtime-release/$name"
done
for name in catalog.json catalog.sigstore.json trusted-root.json; do
  source_path="$manager_release_dir/$name"
  if [ ! -f "$source_path" ] || [ -L "$source_path" ]; then
    echo "verified Manager release is missing regular file: $name" >&2
    exit 1
  fi
  install -m 0444 "$source_path" "$context/manager-release/$name"
done
for name in catalog.json catalog.sigstore.json trusted-root.json; do
  source_path="$runtime_release_dir/toolchain-release/$name"
  if [ ! -f "$source_path" ] || [ -L "$source_path" ]; then
    echo "verified standard-toolchain release is missing regular file: $name" >&2
    exit 1
  fi
  install -m 0444 "$source_path" "$context/toolchain-release/$name"
done

cd "$repo_root"
bun install --frozen-lockfile --ignore-scripts
make console-build

GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build \
  -tags embed_console \
  -trimpath \
  -ldflags="$ldflags" \
  -o "$control_binary" \
  ./cmd/helmr-control
GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build \
  -tags embed_console \
  -trimpath \
  -ldflags="$ldflags" \
  -o "$dispatcher_binary" \
  ./cmd/helmr-dispatcher

cat >"$context/Dockerfile" <<'EOF'
FROM gcr.io/distroless/static-debian12:nonroot
COPY helmr-control /usr/local/bin/helmr-control
COPY helmr-dispatcher /usr/local/bin/helmr-dispatcher
COPY --chown=0:0 --chmod=0444 runtime-release/catalog.json /usr/lib/helmr/runtime-release/catalog.json
COPY --chown=0:0 --chmod=0444 runtime-release/catalog.sigstore.json /usr/lib/helmr/runtime-release/catalog.sigstore.json
COPY --chown=0:0 --chmod=0444 runtime-release/trusted-root.json /usr/lib/helmr/runtime-release/trusted-root.json
COPY --chown=0:0 --chmod=0444 toolchain-release/catalog.json /usr/lib/helmr/toolchain-release/catalog.json
COPY --chown=0:0 --chmod=0444 toolchain-release/catalog.sigstore.json /usr/lib/helmr/toolchain-release/catalog.sigstore.json
COPY --chown=0:0 --chmod=0444 toolchain-release/trusted-root.json /usr/lib/helmr/toolchain-release/trusted-root.json
COPY --chown=0:0 --chmod=0444 manager-release/catalog.json /usr/lib/helmr/manager-release/catalog.json
COPY --chown=0:0 --chmod=0444 manager-release/catalog.sigstore.json /usr/lib/helmr/manager-release/catalog.sigstore.json
COPY --chown=0:0 --chmod=0444 manager-release/trusted-root.json /usr/lib/helmr/manager-release/trusted-root.json
ENTRYPOINT ["/usr/local/bin/helmr-control"]
EOF

docker build --platform "$platform" -t "$image_uri" "$context"
