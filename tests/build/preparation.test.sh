#!/usr/bin/env bash
# Real authored preparation and descendant cleanup in disposable Linux namespaces.
# This does not claim Firecracker boot or control-plane allocation qualification.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
tmp=$(mktemp -d)
image="helmr-preparation-proof-$$"
cleanup() {
  if [[ -s "$tmp/container.id" ]]; then docker rm -f "$(cat "$tmp/container.id")" >/dev/null 2>&1 || true; fi
  docker image rm "$image" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
(cd "$repo_root" && bun scripts/build-platform-entries.ts runtime --out-dir "$tmp/entries")
cp "$tmp/entries/internal/runtime/"{entry.mjs,module-preload.mjs} "$tmp/"
docker_arch=$(docker version --format '{{.Server.Arch}}')
case "$docker_arch" in amd64|arm64) ;; *) echo "unsupported Docker architecture: $docker_arch" >&2; exit 1;; esac
CGO_ENABLED=0 GOOS=linux GOARCH="$docker_arch" go -C "$repo_root" test -c ./internal/guestd -o "$tmp/guestd.test"
node_version=$(jq -er '.node.version' "$repo_root/internal/version/runtime-dependencies.json")
cat > "$tmp/Dockerfile" <<'DOCKERFILE'
ARG NODE_VERSION
FROM node:${NODE_VERSION}-bookworm-slim AS computer
RUN mkdir -p /workspace /run /opt/helmr /tmp && chmod 1777 /tmp && chmod 777 /workspace
FROM node:${NODE_VERSION}-bookworm-slim
COPY --from=computer / /fixture-root/
RUN mkdir -p /var/lib/helmr/program/runtime/helmr /var/lib/helmr/program/runtime/bin /var/lib/helmr/program/artifact/helmr/app
COPY --from=computer /usr/local/bin/node /var/lib/helmr/program/runtime/bin/node
COPY entry.mjs module-preload.mjs /var/lib/helmr/program/runtime/helmr/
COPY guestd.test /guestd.test
ENV HELMR_PRIVILEGED_PROGRAM_TEST=1 HELMR_PREPARATION_TEST_ROOT=/fixture-root
ENTRYPOINT ["/bin/sh", "-ceu", "cp /etc/resolv.conf /run/resolv.conf; /guestd.test -test.run '^TestPreparationExecutesAuthoredCodeAndJoinsDescendants$' -test.v -test.count=1 -test.timeout=1m"]
DOCKERFILE
docker build --platform "linux/$docker_arch" --build-arg "NODE_VERSION=$node_version" --tag "$image" "$tmp" > "$tmp/build.log" 2>&1 &
build_pid=$!
wait "$build_pid" || { tail -n 50 "$tmp/build.log" >&2; exit 1; }
docker run --rm --cidfile "$tmp/container.id" --platform "linux/$docker_arch" --privileged --cgroupns=private "$image" | tee "$tmp/run.log" &
run_pid=$!
wait "$run_pid"
grep -E '^--- PASS: TestPreparationExecutesAuthoredCodeAndJoinsDescendants ' "$tmp/run.log" >/dev/null
if grep -E '^\s*--- (SKIP|FAIL)' "$tmp/run.log"; then exit 1; fi
