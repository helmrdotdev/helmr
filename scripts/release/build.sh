#!/usr/bin/env bash
# Fixed release artifact build parts. Run only in unprivileged jobs.
set -euo pipefail
part=${1:?part} output=${2:?output}
root=$(pwd)
: "${RELEASE_SOURCE_COMMIT:?}" "${RELEASE_SOURCE_REF:?}" "${RELEASE_TAG:?}" "${RELEASE_BUILD_ID:?}"
[ "$(git rev-parse HEAD)" = "$RELEASE_SOURCE_COMMIT" ]
[ -z "$(git status --porcelain)" ] || { echo 'release requires clean selected source' >&2; exit 1; }
for key in AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN GH_TOKEN GITHUB_TOKEN ACTIONS_ID_TOKEN_REQUEST_TOKEN NODE_AUTH_TOKEN; do
  [ -z "${!key:-}" ] || { echo "build received authority: $key" >&2; exit 1; }
done
mkdir -p "$output"
output=$(cd "$output" && pwd)
export PACKAGE_VERSION=${RELEASE_TAG#v} HELMR_PLATFORM_VERSION=$RELEASE_TAG HELMR_SOURCE_COMMIT=$RELEASE_SOURCE_COMMIT
case "$part" in
  sdk)
    bun install --frozen-lockfile --ignore-scripts
    scripts/build-npm-packages.sh
    stage=$(mktemp -d)
    scripts/pack-npm-packages.sh "$stage"
    cp "$stage"/helmr-sdk-*.tgz "$output/sdk.tgz"
    cp "$stage"/helmr-proto-*.tgz "$output/proto.tgz"
    rm -rf "$stage"
    ;;
  builder)
    nix build .#bundleBuilderImage --out-link "$output/builder-docker"
    skopeo --insecure-policy copy "docker-archive:$output/builder-docker" "dir:$output/builder-image"
    digest="sha256:$(sha256sum "$output/builder-image/manifest.json" | cut -d' ' -f1)"
    runtime=$(nix build .#runtimeRelease --no-link --print-out-paths)
    compiler=$(nix build .#compiler --no-link --print-out-paths)
    jq -cnS --arg image "ghcr.io/helmrdotdev/helmr/bundle-builder@$digest" --arg sourceCommit "$RELEASE_SOURCE_COMMIT" \
      --argjson runtime "$(cat "$runtime/runtime.descriptor.json")" --argjson compiler "$(cat "$compiler/compiler.descriptor.json")" \
      '{formatVersion:0,image:$image,sourceCommit:$sourceCommit,runtime:$runtime,compiler:$compiler}' >"$output/bundle-builder.json"
    rm "$output/builder-docker"
    ;;
  platform) scripts/build-platform-release.sh "$output" ;;
  host)
    scripts/materialize-linux-worker-host-bundle.sh "$output/host"
    mv "$output/host/"* "$output/"
    rmdir "$output/host"
    ;;
  guest)
    scripts/check-apko-lock.sh
    make images
    scripts/materialize-worker-runtime-bundle.sh "$output/guest" "$root/images/guest/out"
    mv "$output/guest/"* "$output/"
    rmdir "$output/guest"
    ;;
  controlplane)
    export CONTROLPLANE_IMAGE_CONTEXT="$output/context"
    HELMR_BUILD_VERSION=$RELEASE_TAG scripts/build-controlplane-image.sh helmr/controlplane:build
    skopeo --insecure-policy copy docker-daemon:helmr/controlplane:build "dir:$output/controlplane-image"
    digest="sha256:$(sha256sum "$output/controlplane-image/manifest.json" | cut -d' ' -f1)"
    jq -cnS --arg image "ghcr.io/helmrdotdev/helmr/control-plane@$digest" --arg sourceCommit "$RELEASE_SOURCE_COMMIT" \
      --argjson runtime "$(cat "$output/context/runtime.descriptor.json")" \
      --argjson inputs "$(cat "$output/context/build-inputs.json")" \
      '{formatVersion:0,image:$image,sourceCommit:$sourceCommit,runtime:$runtime,buildInputs:$inputs}' >"$output/controlplane.json"
    tar --format=ustar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 -C "$output/context" -cf - control-plane | gzip -n >"$output/control-plane-linux-amd64.tar.gz"
    rm -rf "$output/context"
    ;;
  cli)
    : "${BUNDLE_BUILDER_IMAGE:?}"
    bun install --frozen-lockfile --ignore-scripts
    scripts/build-cli-binaries.sh "$output"
    ;;
  *) echo 'unknown artifact build part' >&2; exit 2 ;;
esac
