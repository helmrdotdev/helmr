#!/usr/bin/env bash
# Launches the platform Node through guestd's managed Program path inside real
# Workspace root filesystems, using the admitted Program from a built bundle
# and the Runtime artifact. A privileged container supplies Linux namespaces;
# this exercises guestd's launch code, not Firecracker or checkpoint/resume.
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
bundle=${HELMR_NATIVE_BUNDLE:?path to a bundle built from tests/fixtures/native-environment}
tmp=$(mktemp -d)
image="helmr-guestd-native-e2e-$$"
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
runtime_digest=$(jq -er '.digest' "$runtime_release/runtime.descriptor.json")
[ "$(jq -er '.runtime.artifact.digest' "$bundle/bundle.json")" = "$runtime_digest" ] ||
  { echo "bundle was not built against this Runtime artifact" >&2; exit 1; }
program_digest=$(jq -er '.program.artifact.digest | sub("^sha256:"; "")' "$bundle/bundle.json")

mkdir "$tmp/context"
cp "$runtime_release/runtime.squashfs" "$tmp/context/runtime.squashfs"
cp "$bundle/objects/sha256/$program_digest" "$tmp/context/program.squashfs"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C "$repo_root" test -c -o "$tmp/context/guestd.test" ./internal/guestd

# Each Workspace root is an ordinary image. Only some install the library the
# fixture's source-built addon links; none contains build tools.
cat >"$tmp/context/Dockerfile" <<'DOCKERFILE'
FROM debian:bookworm-slim AS bookworm-lib
RUN apt-get update && apt-get install -y --no-install-recommends libsqlite3-0 && rm -rf /var/lib/apt/lists/*
FROM bookworm-lib AS bookworm-lib-nocache
RUN rm /etc/ld.so.cache
FROM ubuntu:20.04 AS ubuntu2004-lib
RUN apt-get update && apt-get install -y --no-install-recommends libsqlite3-0 && rm -rf /var/lib/apt/lists/*
FROM ubuntu:25.10 AS ubuntu2510-lib
RUN apt-get update && apt-get install -y --no-install-recommends libsqlite3-0 && rm -rf /var/lib/apt/lists/*
FROM debian:bookworm-slim AS bookworm-nolib
FROM alpine:3.22 AS alpine

FROM debian:bookworm-slim AS harness
RUN apt-get update && apt-get install -y --no-install-recommends squashfs-tools && rm -rf /var/lib/apt/lists/*
COPY runtime.squashfs program.squashfs /artifacts/
RUN mkdir -p /var/lib/helmr/program \
 && unsquashfs -d /var/lib/helmr/program/runtime /artifacts/runtime.squashfs >/dev/null \
 && unsquashfs -d /var/lib/helmr/program/artifact /artifacts/program.squashfs >/dev/null
COPY --from=bookworm-lib / /workspaces/bookworm-lib/
COPY --from=bookworm-lib-nocache / /workspaces/bookworm-lib-nocache/
COPY --from=ubuntu2004-lib / /workspaces/ubuntu2004-lib/
COPY --from=ubuntu2510-lib / /workspaces/ubuntu2510-lib/
COPY --from=bookworm-nolib / /workspaces/bookworm-nolib/
COPY --from=alpine / /workspaces/alpine/
COPY guestd.test /guestd.test
ENV HELMR_GUESTD_NATIVE_WORKSPACES=/workspaces
# guestd reads the resolver the guest init provides at /run/resolv.conf.
ENTRYPOINT ["/bin/sh", "-ceu", "cp /etc/resolv.conf /run/resolv.conf && exec /guestd.test -test.run '^TestManagedNodeNativeLibraries$' -test.v -test.count=1"]
DOCKERFILE

docker build --platform linux/amd64 --tag "$image" "$tmp/context" >"$tmp/build.log" 2>&1 ||
  { tail -n 60 "$tmp/build.log" >&2; exit 1; }
docker run --rm --platform linux/amd64 --privileged "$image" | tee "$tmp/run.log"
# A skipped test is not evidence.
grep -E '^--- PASS: TestManagedNodeNativeLibraries ' "$tmp/run.log" >/dev/null
if grep -E '^\s*--- SKIP' "$tmp/run.log"; then
  echo "guestd native library test skipped" >&2
  exit 1
fi
printf 'ok - guestd managed Node native library e2e\n'
