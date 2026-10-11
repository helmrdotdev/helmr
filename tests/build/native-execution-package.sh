#!/usr/bin/env bash
# Prepare one checksum-bound execution fixture on x86 Linux with local Docker.
# Run in nix develop .#images. Builds are local; no KVM or remote storage is used.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
output=${1:?new absolute candidate directory}
[[ $(uname -s) == Linux && $(uname -m) == x86_64 && "$output" == /* ]]
cd "$repo_root"
[[ -z $(git status --porcelain) ]] || { echo 'candidate requires a clean source commit' >&2; exit 1; }
commit=$(git rev-parse HEAD)
performance_fixture=${HELMR_NATIVE_PERFORMANCE:-0}
[[ "$performance_fixture" == 0 || "$performance_fixture" == 1 ]]
mkdir "$output"
tmp=$(mktemp -d)
registry="helmr-native-execution-$$"
registry_created=false
loaded_builder=""
boot_tmp=""
old_builder=$(docker image inspect bundle-builder:0 --format '{{.Id}}' 2>/dev/null || true)
# shellcheck source=tests/build/buildx-fixture.sh
source tests/build/buildx-fixture.sh
cleanup() {
  local status=$? current_builder
  trap - EXIT
  set +e
  if $registry_created; then docker rm -fv "$registry" >/dev/null || status=1; fi
  if [[ -n "$loaded_builder" ]]; then
    if current_builder=$(docker image inspect bundle-builder:0 --format '{{.Id}}'); then
      if [[ "$current_builder" == "$loaded_builder" ]]; then
        if [[ -n "$old_builder" ]]; then docker tag "$old_builder" bundle-builder:0 || status=1
        else docker image rm bundle-builder:0 >/dev/null || status=1; fi
      fi
    else status=1; fi
  fi
  cleanup_buildx_fixture || status=1
  if [[ -n "$boot_tmp" ]]; then rm -rf "$boot_tmp" || status=1; fi
  rm -rf "$tmp" || status=1
  exit "$status"
}
trap cleanup EXIT
start_buildx_fixture "$tmp"
nix build .#bundleBuilderImage --out-link "$tmp/builder-image" > "$output/builder.log" 2>&1
nix build .#runtimeRelease --out-link "$tmp/runtime-release" > "$output/runtime.log" 2>&1
docker load -i "$tmp/builder-image" >/dev/null
loaded_builder=$(docker image inspect bundle-builder:0 --format '{{.Id}}')
docker run --detach --rm --name "$registry" --publish 127.0.0.1::5000 \
  registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373 >/dev/null
registry_created=true
port=$(docker port "$registry" 5000/tcp | sed -n 's/^127\.0\.0\.1://p')
[[ -n "$port" ]]
registry_host=${HELMR_NATIVE_REGISTRY_HOST:-127.0.0.1}
case "$registry_host" in 127.0.0.1|host.docker.internal) ;; *) echo 'unsupported local registry host' >&2; exit 1 ;; esac
endpoint="$registry_host:$port"
for _ in {1..100}; do
  if curl --fail --silent "http://$endpoint/v2/" >/dev/null; then break; fi
  sleep 0.1
done
curl --fail --silent "http://$endpoint/v2/" >/dev/null
printf '%s\n' '{"default":[{"type":"insecureAcceptAnything"}]}' > "$tmp/policy.json"
skopeo --policy "$tmp/policy.json" copy --dest-tls-verify=false \
  docker-daemon:bundle-builder:0 "docker://$endpoint/builder:fixture" >/dev/null
digest=$(skopeo --policy "$tmp/policy.json" inspect --tls-verify=false --format '{{.Digest}}' "docker://$endpoint/builder:fixture")
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]
cat > "$tmp/buildkitd.toml" <<EOF
[registry."$endpoint"]
  http = true
  insecure = true
EOF
docker buildx create --name "$fixture_builder" --driver docker-container \
  --driver-opt network=host --buildkitd-config "$tmp/buildkitd.toml" >/dev/null
docker buildx inspect --bootstrap "$fixture_builder" >/dev/null
bun install --frozen-lockfile --ignore-scripts > "$output/dependencies.log" 2>&1
scripts/build-npm-packages.sh >> "$output/dependencies.log" 2>&1
(cd examples/issue-fixer && bun install --frozen-lockfile --ignore-scripts) >> "$output/dependencies.log" 2>&1
make platform-entries > "$output/entries.log" 2>&1
go build -trimpath -ldflags="-X main.deploymentBundleBuilderImage=$endpoint/builder@$digest" -o "$tmp/helmr" ./cmd/helmr
mkdir -p "$tmp/project/tasks"
# Close the repository-relative fixture helpers into a declaration source module.
# The ordinary compiler still discovers, analyzes and builds this application;
# no definition index, Program manifest or Computer seed is fabricated here.
bun build tests/fixtures/native-continuation/production.ts --target=node \
  --define "process.env.HELMR_NATIVE_PERFORMANCE=\"$performance_fixture\"" \
  --external '@anthropic-ai/claude-agent-sdk' --outfile "$tmp/project/tasks/agents.mjs"
if [[ "$performance_fixture" == 1 ]]; then
  mkdir -p "$tmp/project/performance/dependencies"
  cp -R tests/fixtures/native-continuation/performance/. "$tmp/project/performance/"
  cp tests/fixtures/native-continuation/{package.json,package-lock.json} "$tmp/project/performance/dependencies/"
  git archive --format=tar "$commit" > "$tmp/project/performance/repository.tar"
  python3 - "$tmp/project/performance" "$commit" <<'PY'
import hashlib,json,sys
from pathlib import Path
p=Path(sys.argv[1])
files={str(f.relative_to(p)):hashlib.sha256(f.read_bytes()).hexdigest() for f in sorted(p.rglob('*')) if f.is_file()}
(p/'source.json').write_text(json.dumps({'product_commit':sys.argv[2],'files':files},sort_keys=True)+'\n')
PY
fi
cp tests/fixtures/native-continuation/{package.json,package-lock.json} "$tmp/project/"
cat > "$tmp/project/helmr.config.ts" <<'CONFIG'
export default { dirs: ["tasks"], build: { external: ["@openai/codex", "@anthropic-ai/claude-agent-sdk"] } }
CONFIG
"$tmp/helmr" build "$tmp/project" --output "$output/bundle" > "$output/bundle.log" 2>&1
cp "$tmp/runtime-release/"{runtime.squashfs,runtime.descriptor.json} "$output/"
chmod 0400 "$output/runtime.squashfs"
bun build tests/fixtures/native-continuation/production-driver.ts --target=node --outfile "$output/driver.mjs"
bun build tests/fixtures/native-continuation/model.ts --target=node --outfile "$output/model.mjs"
mkdir -p images/guest/out
boot_tmp=$(mktemp -d images/guest/out/native-execution.XXXXXX)
boot_relative="out/$(basename "$boot_tmp")"
make -C images/guest OUT="$boot_relative/out" GUESTD="$boot_relative/guestd" > "$output/boot.log" 2>&1
mkdir -p "$output/images/guest/out"
cp "$boot_tmp/out/"{vmlinuz,initramfs,rootfs.squashfs,runtime-artifacts.json} "$output/images/guest/out/"
rm -rf "$boot_tmp"
worker_tags=()
if [[ "$performance_fixture" == 1 ]]; then worker_tags=(-tags computerproof); fi
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 go build "${worker_tags[@]}" -trimpath -o "$output/agent-worker" ./cmd/worker
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 go test -tags computerproof -c ./internal/controlplane -o "$output/agent-execution.test"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 go test -c ./internal/nbd -o "$output/nbd-recovery.test"
cp tests/build/native-execution-kvm.sh "$output/run.sh"
git archive --format=tar "$commit" > "$output/source.tar"
[[ $(git rev-parse HEAD) == "$commit" && -z $(git status --porcelain) ]] || { echo 'source changed during candidate build' >&2; exit 1; }
python3 - "$output" "$commit" "$digest" "$(readlink -f "$tmp/builder-image")" "$(readlink -f "$tmp/runtime-release")" "$performance_fixture" <<'PY'
import hashlib,json,sys
from pathlib import Path
p=Path(sys.argv[1])
def digest(path):
 h=hashlib.sha256()
 with path.open('rb') as f:
  for b in iter(lambda:f.read(1024*1024),b''): h.update(b)
 return h.hexdigest()
(p/'source.json').write_text(json.dumps({'product_commit':sys.argv[2],'builder_digest':sys.argv[3],'native_performance':sys.argv[6]=='1',
 'builder_derivation_output':sys.argv[4],'runtime_derivation_output':sys.argv[5],
 'source_archive_sha256':digest(p/'source.tar'),'worker_sha256':digest(p/'agent-worker'),
 'test_sha256':digest(p/'agent-execution.test'),'nbd_test_sha256':digest(p/'nbd-recovery.test')},indent=2)+'\n')
files=sorted(x for x in p.rglob('*') if x.is_file())
(p/'SHA256SUMS').write_text(''.join(digest(x)+'  '+str(x.relative_to(p))+'\n' for x in files))
PY
chmod 0444 "$output/images/guest/out/"{vmlinuz,initramfs,rootfs.squashfs}
printf 'candidate commit %s: %s\n' "$commit" "$output"
