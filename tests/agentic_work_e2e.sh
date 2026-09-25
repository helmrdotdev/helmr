#!/usr/bin/env bash
# Runs the agentic-work fixture's declared tasks in the Computer seed its own
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
  docker image rm -f "$image" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

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
mkdir -p "$tmp/context/objects/sha256"
cp "$workspace" "$tmp/context/objects/sha256/$(basename "$workspace")"
jq -e '.workspaceImages[] | select(.declaredId == "agentic-work") | .artifact' "$bundle/bundle.json" >"$tmp/context/seed.json"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C "$repo_root" test -c -o "$tmp/context/guestd.test" ./internal/guestd

cat >"$tmp/context/Dockerfile" <<'DOCKERFILE'
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends squashfs-tools mount && rm -rf /var/lib/apt/lists/*
COPY runtime.squashfs program.squashfs /artifacts/
RUN mkdir -p /var/lib/helmr/program \
 && unsquashfs -d /var/lib/helmr/program/runtime /artifacts/runtime.squashfs >/dev/null \
 && unsquashfs -d /var/lib/helmr/program/artifact /artifacts/program.squashfs >/dev/null
COPY objects /artifacts/objects
COPY seed.json /artifacts/seed.json
COPY guestd.test /guestd.test
ENV HELMR_GUESTD_AGENTIC_COMPUTER_SEED=/artifacts/seed.json
# guestd reads the resolver the guest init provides at /run/resolv.conf.
ENTRYPOINT ["/bin/sh", "-ceu", "cp /etc/resolv.conf /run/resolv.conf && exec /guestd.test -test.run '^TestManagedNodeAgenticWork$' -test.v -test.count=1 -test.timeout=40m"]
DOCKERFILE

docker build --platform linux/amd64 --tag "$image" "$tmp/context" >"$tmp/build.log" 2>&1 ||
  { tail -n 60 "$tmp/build.log" >&2; exit 1; }
# Decode the sparse Computer disk in tmpfs, then mount it through a disposable loop device.
docker run --rm --platform linux/amd64 --privileged --tmpfs /tmp:exec,size=8g "$image" | tee "$tmp/run.log"
grep -E '^--- PASS: TestManagedNodeAgenticWork ' "$tmp/run.log" >/dev/null
if grep -E '^\s*--- (SKIP|FAIL)' "$tmp/run.log"; then
  echo "agentic work test did not run every scenario" >&2
  exit 1
fi
printf 'ok - guestd managed Node agentic work e2e\n'
