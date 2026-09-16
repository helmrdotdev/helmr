#!/usr/bin/env bash
set -euo pipefail
root=${RELEASE_SOURCE_ROOT:-$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)}
: "${RELEASE_TAG:?}" "${RELEASE_SOURCE_COMMIT:?}" "${BUNDLE_BUILDER_IMAGE:?}"
[[ "$RELEASE_SOURCE_COMMIT" =~ ^[0-9a-f]{40}$ ]]
[[ "$BUNDLE_BUILDER_IMAGE" =~ @sha256:[0-9a-f]{64}$ ]]
output=${1:?output directory required}
mkdir -p "$output"
output=$(cd "$output" && pwd)
cd "$root"
scripts/build-runtime-entry.sh --check
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os=${target%/*} arch=${target#*/}
  stage=$(mktemp -d)
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags="-s -w -X github.com/helmrdotdev/helmr/internal/version.Version=$RELEASE_TAG -X github.com/helmrdotdev/helmr/internal/version.SourceCommit=$RELEASE_SOURCE_COMMIT -X main.deploymentBundleBuilderImage=$BUNDLE_BUILDER_IMAGE" \
    -o "$stage/helmr" ./cmd/helmr
  tar --format=ustar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 -C "$stage" -cf - helmr | gzip -n >"$output/helmr-$os-$arch.tar.gz"
  rm -rf "$stage"
done
# Dependency-light installer projection; the authoritative index binds this file.
(cd "$output"; LC_ALL=C sha256sum helmr-*.tar.gz > checksums.txt)
