#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
image_uri="${1:-}"
platform="${CONTROL_IMAGE_PLATFORM:-linux/amd64}"
os="${platform%%/*}"
arch="${platform#*/}"
arch="${arch%%/*}"
context="${CONTROL_IMAGE_CONTEXT:-$repo_root/dist/control-image}"
build_version="${HELMR_BUILD_VERSION:-}"
ldflags="-s -w"

if [ -z "$image_uri" ]; then
  echo "usage: scripts/build-control-image.sh <image-uri>" >&2
  exit 1
fi
case "$os/$arch" in
  linux/amd64 | linux/arm64) ;;
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

cat >"$context/Dockerfile" <<'EOF'
FROM gcr.io/distroless/static-debian12:nonroot
COPY helmr-control /usr/local/bin/helmr-control
COPY helmr-dispatcher /usr/local/bin/helmr-dispatcher
ENTRYPOINT ["/usr/local/bin/helmr-control"]
EOF

docker build \
  --platform "$platform" \
  -t "$image_uri" \
  "$context"
