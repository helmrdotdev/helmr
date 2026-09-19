#!/usr/bin/env bash
# Runs the agentic-work fixture's declared tasks in the Workspace image its own
# SDK declaration built, through guestd's managed Program launch path. The
# handlers drive real tools: Git, Chromium through Playwright, Python with
# NumPy, Sharp, and child processes. A privileged container supplies Linux
# namespaces; the guest task protocol, Firecracker and checkpoint/resume are
# not part of this check, and no model is involved.
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
bundle=${HELMR_AGENTIC_BUNDLE:?path to a bundle built from tests/fixtures/agentic-work}
tmp=$(mktemp -d)
image="helmr-agentic-work-e2e-$$"
cleanup() {
  if [ -s "$tmp/container.cid" ]; then
    docker rm -f "$(cat "$tmp/container.cid")" >/dev/null 2>&1 || true
  fi
  docker image rm -f "$image" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [ -n "${RUNTIME_RELEASE_DIR:-}" ]; then
  runtime_release="$RUNTIME_RELEASE_DIR"
else
  nix build "$repo_root#runtimeRelease" --out-link "$tmp/runtime-release"
  runtime_release="$tmp/runtime-release"
fi
[ "$(jq -er '.runtime.artifact.digest' "$bundle/bundle.json")" = "$(jq -er '.digest' "$runtime_release/runtime.descriptor.json")" ] ||
  { echo "bundle was not built against this Runtime artifact" >&2; exit 1; }
object() { printf '%s/objects/sha256/%s' "$bundle" "${1#sha256:}"; }
program=$(object "$(jq -er '.program.artifact.digest' "$bundle/bundle.json")")
workspace=$(object "$(jq -er '.workspaceImages[] | select(.declaredId == "agentic-work") | .artifact.digest' "$bundle/bundle.json")")

mkdir "$tmp/context"
cp "$runtime_release/runtime.squashfs" "$tmp/context/runtime.squashfs"
cp "$program" "$tmp/context/program.squashfs"
cp "$workspace" "$tmp/context/workspace.oci.tar"
if [ -n "${HELMR_GUESTD_TEST_BINARY:-}" ]; then
  cp "$HELMR_GUESTD_TEST_BINARY" "$tmp/context/guestd.test"
else
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C "$repo_root" test -c -o "$tmp/context/guestd.test" ./internal/guestd
fi

cat >"$tmp/context/Dockerfile" <<'DOCKERFILE'
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends squashfs-tools && rm -rf /var/lib/apt/lists/*
COPY runtime.squashfs program.squashfs /artifacts/
RUN mkdir -p /var/lib/helmr/program \
 && unsquashfs -d /var/lib/helmr/program/runtime /artifacts/runtime.squashfs >/dev/null \
 && unsquashfs -d /var/lib/helmr/program/artifact /artifacts/program.squashfs >/dev/null
COPY workspace.oci.tar /artifacts/workspace.oci.tar
COPY guestd.test /guestd.test
ENV HELMR_GUESTD_AGENTIC_WORKSPACE_IMAGE=/artifacts/workspace.oci.tar
# guestd reads the resolver the guest init provides at /run/resolv.conf.
ENTRYPOINT ["/bin/sh", "-ceu", "cp /etc/resolv.conf /run/resolv.conf && exec /guestd.test -test.run '^TestManagedNodeAgenticWork$' -test.v -test.count=1 -test.timeout=40m"]
DOCKERFILE

docker build --platform linux/amd64 --tag "$image" "$tmp/context" >"$tmp/build.log" 2>&1 ||
  { tail -n 60 "$tmp/build.log" >&2; exit 1; }
# The unpacked Workspace root needs a real filesystem, not the container overlay.
docker run --rm --cidfile "$tmp/container.cid" --platform linux/amd64 --privileged --tmpfs /tmp:exec,size=8g "$image" | tee "$tmp/run.log"
grep -E '^--- PASS: TestManagedNodeAgenticWork ' "$tmp/run.log" >/dev/null
if grep -E '^\s*--- (SKIP|FAIL)' "$tmp/run.log"; then
  echo "agentic work test did not run every scenario" >&2
  exit 1
fi
printf 'ok - guestd managed Node agentic work e2e\n'
