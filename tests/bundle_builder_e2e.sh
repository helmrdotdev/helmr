#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
case "${1:-}" in
  ""|--parallel) mode=parallel ;;
  --serial) mode=serial ;;
  *) echo "usage: $0 [--parallel|--serial]" >&2; exit 2 ;;
esac
[ "$#" -le 1 ] || { echo "usage: $0 [--parallel|--serial]" >&2; exit 2; }
started=$SECONDS
tmp=$(mktemp -d)
shared=$tmp
lanes_pid=
registry_name="helmr-bundle-builder-registry-$$"
cleanup() {
  if [ -n "$lanes_pid" ]; then
    kill -TERM -- "-$lanes_pid" 2>/dev/null || true
    wait "$lanes_pid" 2>/dev/null || true
  fi
  if [ -s "$tmp/registry.cid" ]; then
    docker rm -f "$(cat "$tmp/registry.cid")" >/dev/null 2>&1 || true
  fi
  if [ "${KEEP_BUNDLE_E2E_TMP:-0}" = 1 ]; then
    printf 'bundle builder e2e artifacts: %s\n' "$tmp" >&2
  else
    rm -rf "$tmp"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Reject a remote daemon before shared setup mutates it. Each lane repeats this
# check when it creates its isolated fixture context.
selected_context=${DOCKER_CONTEXT:-$(docker context show)}
case "$(docker context inspect "$selected_context" --format '{{.Endpoints.docker.Host}}')" in
  unix://*) ;;
  *) echo 'local Docker fixture requires a Unix-socket daemon' >&2; exit 1 ;;
esac

docker run --detach --rm \
  --cidfile "$tmp/registry.cid" \
  --name "$registry_name" \
  --publish 127.0.0.1::5000 \
  "registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373" \
  >/dev/null
registry_port="$(docker port "$registry_name" 5000/tcp | sed -n 's/^127\.0\.0\.1://p')"
[ -n "$registry_port" ]
registry_endpoint="127.0.0.1:$registry_port"
if [ "$(uname -s)" = "Darwin" ]; then
  builder_registry_endpoint="host.docker.internal:$registry_port"
else
  builder_registry_endpoint="$registry_endpoint"
fi
for _ in $(seq 1 50); do
  if curl --fail --silent "http://$registry_endpoint/v2/" >/dev/null; then
    break
  fi
  sleep 0.2
done
curl --fail --silent "http://$registry_endpoint/v2/" >/dev/null

if [ -n "${BUNDLE_BUILDER_IMAGE_ARCHIVE:-}" ]; then
  [ -f "$BUNDLE_BUILDER_IMAGE_ARCHIVE" ]
  builder_archive="$BUNDLE_BUILDER_IMAGE_ARCHIVE"
else
  nix build "$repo_root#bundleBuilderImage" --out-link "$tmp/builder-image"
  builder_archive="$tmp/builder-image"
fi
docker load -i "$builder_archive" >/dev/null
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.source"}}' bundle-builder:0)" = 'https://github.com/helmrdotdev/helmr' ]
# The builder is an ordinary Debian environment: standard ELF loader, native
# toolchain, and the Product-selected official Node as the only node on PATH.
docker run --rm --platform linux/amd64 --entrypoint node --workdir / \
  --volume "$repo_root:/product:ro" bundle-builder:0 --input-type=module -e '
    import { execFileSync } from "node:child_process"
    import { existsSync } from "node:fs"
    import assert from "node:assert/strict"
    import { requireVersion, managerInterpreter } from "/product/scripts/check-node-toolchain.mjs"
    requireVersion(process.versions.node, "builder image Node")
    assert.equal(process.execPath, "/usr/local/bin/node")
    assert.equal(existsSync("/usr/bin/node"), false, "distribution Node must not exist")
    assert.equal(existsSync("/lib64/ld-linux-x86-64.so.2"), true, "standard ELF loader")
    for (const command of ["npm", "npx", "corepack"]) {
      console.log(JSON.stringify(managerInterpreter(command)))
      execFileSync(command, ["--version"], {stdio:"inherit"})
    }
    for (const command of ["bun", "gcc", "g++", "make", "python3", "git", "pkg-config"]) {
      execFileSync(command, ["--version"], {stdio:"ignore"})
    }
    // Objects compiled here must load under the Runtime: its glibc may not be older.
    const version = text => text.match(/(\d+)\.(\d+)/).slice(1).map(Number)
    const builder = version(execFileSync("ldd", ["--version"], {encoding:"utf8"}).split("\n")[0].split(" ").at(-1))
    const runtime = version(execFileSync("/opt/helmr/runtime/lib/ld-linux-x86-64.so.2", ["--version"], {encoding:"utf8"}).split("\n")[0].match(/version (\S+)/)[1])
    assert.ok(builder[0] < runtime[0] || (builder[0] === runtime[0] && builder[1] <= runtime[1]), `builder glibc ${builder} exceeds Runtime glibc ${runtime}`)
    console.log(JSON.stringify({builderGlibc: builder.join("."), runtimeGlibc: runtime.join(".")}))
  '
printf '%s\n' '{"default":[{"type":"insecureAcceptAnything"}]}' >"$tmp/containers-policy.json"
skopeo --policy "$tmp/containers-policy.json" copy \
  --dest-tls-verify=false \
  docker-daemon:bundle-builder:0 \
  "docker://$registry_endpoint/bundle-builder:test" \
  >/dev/null
builder_digest="$(
  skopeo --policy "$tmp/containers-policy.json" inspect \
    --tls-verify=false \
    --format '{{.Digest}}' \
    "docker://$registry_endpoint/bundle-builder:test"
)"
[[ "$builder_digest" =~ ^sha256:[0-9a-f]{64}$ ]]
cat >"$shared/buildkitd.toml" <<EOF
[registry."$builder_registry_endpoint"]
  http = true
  insecure = true
EOF
builder_image="$builder_registry_endpoint/bundle-builder@$builder_digest"
printf '%s\n' "$builder_image" >"$shared/builder-image-ref"
go -C "$repo_root" build \
  -trimpath \
  -ldflags="-X main.deploymentBundleBuilderImage=$builder_image" \
  -o "$tmp/helmr" \
  ./cmd/helmr

# helmr.config.ts is evaluated once on this host, so a project prepares the
# packages its config imports here. The fixtures use the current packed SDK;
# host node_modules stay out of the captured source and out of the Program.
(cd "$repo_root" && bun install --frozen-lockfile --ignore-scripts >/dev/null && scripts/build-npm-packages.sh >/dev/null)

# Both runtime consumers use exactly the same Runtime and guestd binary.
if [ -n "${RUNTIME_RELEASE_DIR:-}" ]; then
  ln -s "$(cd -- "$RUNTIME_RELEASE_DIR" && pwd)" "$shared/runtime-release"
else
  nix build "$repo_root#runtimeRelease" --out-link "$shared/runtime-release"
fi
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C "$repo_root" test -c -o "$shared/guestd.test" ./internal/guestd
printf 'bundle builder shared setup: duration=%ss archive_bytes=%s\n' "$((SECONDS-started))" "$(wc -c <"$builder_archive" | tr -d ' ')"
set -m
python3 "$repo_root/tests/bundle-builder/run-lanes.py" "$shared" "$mode" &
lanes_pid=$!
lane_status=0
wait "$lanes_pid" || lane_status=$?
lanes_pid=
printf 'bundle builder total: mode=%s duration=%ss\n' "$mode" "$((SECONDS-started))"
[ "$lane_status" -eq 0 ] || exit "$lane_status"
printf 'ok - canonical bundle builder package-manager, workspace-image, native-environment and agentic-work e2e\n'
