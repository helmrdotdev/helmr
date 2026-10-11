#!/usr/bin/env bash
# Disposable Linux native/cgroup qualification. Model replies are local; the
# optional CP mode uses the outer test server for MCP and authority renewal.
# Neither mode claims VM snapshot/restore proof.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
tmp=$(mktemp -d)
image="helmr-native-continuation-$$"
cleanup() {
  if [[ -s "$tmp/container.id" ]]; then
    docker rm -f "$(cat "$tmp/container.id")" >/dev/null 2>&1 || true
  fi
  docker image rm "$image" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
test_pattern="^TestAgentNativeComputerContinuation$"
container_options=()
if [[ -n "${HELMR_NATIVE_CP_CONFIG:-}" ]]; then
  test_pattern="^TestAgentNativeManagedMCP$"
  container_options+=(--mount "type=bind,src=$HELMR_NATIVE_CP_CONFIG,dst=/cp.json,readonly" -e HELMR_NATIVE_CP_CONFIG=/cp.json -e "HELMR_NATIVE_TARGET_SESSION=$(jq -er .TargetSession "$HELMR_NATIVE_CP_CONFIG")")
fi
bun build "$repo_root/tests/fixtures/native-continuation/agent.ts" --target=node --external '@anthropic-ai/claude-agent-sdk' --outfile "$tmp/agent.mjs"
bun build "$repo_root/tests/fixtures/native-continuation/model.ts" --target=node --outfile "$tmp/model.mjs"
(cd "$repo_root" && bun scripts/build-platform-entries.ts runtime --out-dir "$tmp/entries")
cp "$tmp/entries/internal/runtime/"{entry.mjs,module-preload.mjs} "$tmp/"
docker_arch=$(docker version --format '{{.Server.Arch}}')
case "$docker_arch" in amd64|arm64) ;; *) echo "unsupported Docker architecture: $docker_arch" >&2; exit 1;; esac
CGO_ENABLED=0 GOOS=linux GOARCH="$docker_arch" go -C "$repo_root" test -c ./internal/guestd -o "$tmp/guestd.test"
cp "$repo_root/tests/fixtures/native-continuation/"{package.json,package-lock.json} "$tmp/"
node_version=$(jq -er '.node.version' "$repo_root/internal/version/runtime-dependencies.json")
cat > "$tmp/Dockerfile" <<'DOCKERFILE'
ARG NODE_VERSION
FROM node:${NODE_VERSION}-bookworm-slim AS computer
RUN mkdir -p /workspace /run /opt/helmr /tmp && chmod 1777 /tmp && chmod 777 /workspace
WORKDIR /fixture-package
COPY package.json package-lock.json .
RUN npm ci --ignore-scripts --no-audit --no-fund
FROM node:${NODE_VERSION}-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends squashfs-tools && rm -rf /var/lib/apt/lists/*
COPY --from=computer / /fixture-root/
RUN mkdir -p /var/lib/helmr/program/runtime/helmr /var/lib/helmr/program/runtime/bin /var/lib/helmr/program/artifact/helmr/app
COPY --from=computer /usr/local/bin/node /var/lib/helmr/program/runtime/bin/node
COPY --from=computer /fixture-package/node_modules /var/lib/helmr/program/artifact/node_modules
COPY package.json /var/lib/helmr/program/artifact/package.json
COPY entry.mjs module-preload.mjs /var/lib/helmr/program/runtime/helmr/
COPY agent.mjs /var/lib/helmr/program/artifact/helmr/app/entry-0.mjs
RUN printf '%s' '{"apiVersion":"helmr.definition-index.v1","agents":[{"id":"codex","computerDefinitionId":"native-continuation","modulePath":"helmr/app/entry-0.mjs","exportName":"codex"},{"id":"claude","computerDefinitionId":"native-continuation","modulePath":"helmr/app/entry-0.mjs","exportName":"claude"}],"computers":[{"id":"native-continuation","modulePath":"helmr/app/entry-0.mjs","exportName":"codex","throughAgent":true}]}' > /var/lib/helmr/program/artifact/helmr/definition-index.json
COPY model.mjs /model.mjs
COPY guestd.test /guestd.test
ENV HELMR_PRIVILEGED_PROGRAM_TEST=1 HELMR_NATIVE_COMPUTER_ROOT=/fixture-root
ENTRYPOINT ["/bin/sh", "-ceu", "cp /etc/resolv.conf /run/resolv.conf; node /model.mjs /fixture-root/workspace/native-models.json & model_pid=$!; trap 'kill $model_pid 2>/dev/null || true' EXIT; attempt=0; while [ ! -s /fixture-root/workspace/native-models.json ]; do kill -0 $model_pid; attempt=$((attempt+1)); [ $attempt -lt 200 ]; sleep 0.05; done; /guestd.test -test.run \"$HELMR_NATIVE_TEST\" -test.v -test.count=1 -test.timeout=3m"]
DOCKERFILE
# Waiting on a background job lets Bash run TERM/INT traps immediately, even
# when the Docker CLI is waiting on the daemon or forwarding a container signal.
docker build --platform "linux/$docker_arch" --build-arg "NODE_VERSION=$node_version" --tag "$image" "$tmp" > "$tmp/build.log" 2>&1 &
build_pid=$!
wait "$build_pid" || { tail -n 70 "$tmp/build.log" >&2; exit 1; }
docker run --rm --cidfile "$tmp/container.id" -e "HELMR_NATIVE_TEST=$test_pattern" "${container_options[@]}" --platform "linux/$docker_arch" --privileged --cgroupns=private "$image" | tee "$tmp/run.log" &
run_pid=$!
wait "$run_pid"
test_name=${test_pattern#^}
test_name=${test_name%$}
grep -E "^--- PASS: $test_name " "$tmp/run.log" >/dev/null
if grep -E '^\s*--- (SKIP|FAIL)' "$tmp/run.log"; then exit 1; fi
