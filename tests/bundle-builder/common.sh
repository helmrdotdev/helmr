#!/usr/bin/env bash
# Private lane setup. The parent has already built the image, CLI, SDK, Runtime
# and guestd test binary once. Only BuildKit state and scenario outputs are local.
# shellcheck disable=SC2034 # Variables and helpers are consumed by the lane.
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
shared=${1:?shared bundle builder fixture directory}
lane=$(basename -- "${BASH_SOURCE[1]}" .sh)
tmp="$shared/$lane"
mkdir -p "$tmp"
builder_image=$(cat "$shared/builder-image-ref")
export RUNTIME_RELEASE_DIR="$shared/runtime-release"
export HELMR_GUESTD_TEST_BINARY="$shared/guestd.test"
# shellcheck source=tests/buildx-fixture.sh
source "$repo_root/tests/buildx-fixture.sh"
# shellcheck source=tests/buildkit-steps.sh
source "$repo_root/tests/buildkit-steps.sh"
trap cleanup_buildx_fixture EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
start_buildx_fixture "$tmp"
buildx_name=$fixture_builder
docker buildx create \
  --name "$buildx_name" \
  --driver docker-container \
  --driver-opt network=host \
  --buildkitd-config "$shared/buildkitd.toml" \
  >/dev/null
docker buildx inspect --bootstrap "$buildx_name" >/dev/null

prepare_host_sdk() {
  local target="$1"
  mkdir -p "$target/node_modules/@helmr" "$target/node_modules/@bufbuild"
  cp -R "$repo_root/dist/npm/sdk/package" "$target/node_modules/@helmr/sdk"
  cp -R "$repo_root/dist/npm/proto/package" "$target/node_modules/@helmr/proto"
  cp -RL "$repo_root/sdk/typescript/node_modules/@bufbuild/protobuf" "$target/node_modules/@bufbuild/protobuf"
  grep -Fxq node_modules "$target/.helmrignore" 2>/dev/null || printf 'node_modules\n' >>"$target/.helmrignore"
}

# write_config takes the build settings as a TypeScript object literal.
write_config() {
  local target="$1" build="${2:-}"
  [ -n "$build" ] || build='{}'
  cat >"$target/helmr.config.ts" <<TS
import { defineConfig } from "@helmr/sdk"
export default defineConfig({ dirs: ["tasks"], ignorePatterns: [], build: $build })
TS
}
