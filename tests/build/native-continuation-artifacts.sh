#!/usr/bin/env bash
# Prepare x86 KVM fixture artifacts, with current runtime entry code and a pinned
# Runtime's Node/libraries. This is a mechanism fixture, not release certification.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
output=${1:?new output directory required}
runtime_image=${HELMR_NATIVE_RUNTIME_IMAGE:?explicit local image containing /opt/helmr/runtime required}
mkdir "$output"
output=$(cd "$output" && pwd)
tmp=$(mktemp -d)
image="helmr-native-artifacts-$$"
container=""
cleanup() {
  if [ -n "$container" ]; then docker rm "$container" >/dev/null; fi
  docker image rm "$image" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT
runtime_id=$(docker image inspect "$runtime_image" --format '{{.Id}}')
[ "$(docker image inspect "$runtime_id" --format '{{.Architecture}}')" = amd64 ]
container=$(docker create --platform linux/amd64 "$runtime_id")
mkdir "$tmp/runtime"
docker cp "$container:/opt/helmr/runtime/." - | tar -x --no-same-permissions --no-same-owner -C "$tmp/runtime"
chmod -R u+w "$tmp/runtime"
docker rm "$container" >/dev/null
container=""
(cd "$repo_root" && bun scripts/build-platform-entries.ts runtime --out-dir "$tmp/entries")
cp "$tmp/entries/internal/runtime/"{entry.mjs,module-preload.mjs} "$tmp/runtime/helmr/"
go -C "$repo_root" run ./tests/fixtures/native-continuation/runtime-metadata "$tmp/runtime"
bun build "$repo_root/tests/fixtures/native-continuation/agent.ts" --target=node --external '@anthropic-ai/claude-agent-sdk' --outfile "$tmp/agent.mjs"
bun build "$repo_root/tests/fixtures/native-continuation/model.ts" --target=node --outfile "$output/model.mjs"
cp "$repo_root/tests/fixtures/native-continuation/"{package.json,package-lock.json} "$tmp/"
node_version=$(jq -er '.node.version' "$repo_root/internal/version/runtime-dependencies.json")
cat > "$tmp/Dockerfile" <<'DOCKERFILE'
ARG NODE_VERSION
FROM node:${NODE_VERSION}-bookworm-slim AS computer
RUN mkdir -p /workspace /run /opt/helmr /tmp && chmod 1777 /tmp && chmod 777 /workspace
FROM node:${NODE_VERSION}-bookworm-slim AS program
WORKDIR /program
COPY package.json package-lock.json .
RUN npm ci --ignore-scripts --no-audit --no-fund
COPY agent.mjs helmr/app/entry-0.mjs
RUN printf '%s' '{"apiVersion":"helmr.definition-index.v1","agents":[{"id":"codex","computerDefinitionId":"native-continuation","modulePath":"helmr/app/entry-0.mjs","exportName":"codex"},{"id":"claude","computerDefinitionId":"native-continuation","modulePath":"helmr/app/entry-0.mjs","exportName":"claude"}],"computers":[{"id":"native-continuation","modulePath":"helmr/app/entry-0.mjs","exportName":"codex","throughAgent":true}]}' > helmr/definition-index.json
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends squashfs-tools e2fsprogs && rm -rf /var/lib/apt/lists/*
COPY --from=computer / /computer/
COPY --from=program /program /program
COPY runtime /runtime
RUN mkdir /out && mksquashfs /runtime /out/runtime.squashfs -noappend -all-root -processors 1 >/dev/null && mksquashfs /program /out/program.squashfs -noappend -all-root -processors 1 >/dev/null && truncate -s 2G /out/computer.ext4 && mke2fs -q -t ext4 -F -d /computer /out/computer.ext4
DOCKERFILE
docker build --platform linux/amd64 --build-arg "NODE_VERSION=$node_version" --tag "$image" "$tmp" > "$output/build.log" 2>&1 || { tail -n 60 "$output/build.log" >&2; exit 1; }
container=$(docker create --platform linux/amd64 "$image")
docker cp "$container:/out/." "$output/"
printf '%s\n' "$runtime_id" > "$output/runtime-image.txt"
(cd "$output" && shasum -a 256 runtime.squashfs program.squashfs computer.ext4 model.mjs runtime-image.txt > SHA256SUMS)
