#!/usr/bin/env bash
# Local fixture; registry only on loopback. Public verification uses consumer.py directly.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
assets=${1:?built assets directory}
work=$(mktemp -d)
registry="helmr-release-consumer-$$"
builder="helmr-release-consumer-$$"
cleanup() {
  result=$?
  docker buildx rm "$builder" >/dev/null 2>&1 || true
  docker rm -f "$registry" >/dev/null 2>&1 || true
  if [ "$result" -eq 0 ]; then
    rm -rf "$work"
  else
    printf 'failed consumer evidence retained at %s\n' "$work" >&2
  fi
}
trap cleanup EXIT
docker run --detach --rm --name "$registry" --publish 127.0.0.1::5000 \
  registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373 >/dev/null
port=$(docker port "$registry" 5000/tcp | sed -n 's/^127\.0\.0\.1://p')
endpoint="127.0.0.1:$port"
builder_endpoint=$endpoint
if [ "$(uname -s)" = Darwin ]; then builder_endpoint="host.docker.internal:$port"; fi
for _ in $(seq 1 50); do
  if curl --fail --silent "http://$endpoint/v2/" >/dev/null; then break; fi
  sleep 0.2
done
skopeo --insecure-policy copy --preserve-digests --dest-tls-verify=false "dir:$assets/builder-image" "docker://$endpoint/helmr/bundle-builder:test"
digest=$(skopeo inspect --tls-verify=false --format '{{.Digest}}' "docker://$endpoint/helmr/bundle-builder:test")
[ "$digest" = "$(jq -r '.image | split("@")[1]' "$assets/bundle-builder.json")" ] || { echo "local registry builder digest differs: $digest" >&2; exit 1; }
cat >"$work/buildkitd.toml" <<EOF
[registry."$builder_endpoint"]
  http = true
  insecure = true
EOF
docker buildx create --name "$builder" --driver docker-container --driver-opt network=host --buildkitd-config "$work/buildkitd.toml" >/dev/null
export BUILDX_BUILDER=$builder
docker buildx inspect --bootstrap >/dev/null
case "$(uname -s)/$(uname -m)" in
  Darwin/arm64) target=darwin/arm64 ;;
  Darwin/x86_64) target=darwin/amd64 ;;
  Linux/x86_64) target=linux/amd64 ;;
  *) echo 'unsupported native fixture CLI platform' >&2; exit 1 ;;
esac
BUNDLE_BUILDER_IMAGE="$builder_endpoint/helmr/bundle-builder@$digest" \
  "$root/scripts/build-cli-binaries.sh" "$work/cli"
PYTHONPATH="$root/scripts/release" python3 - "$assets" "$work/cli/helmr-${target/\//-}.tar.gz" "$work/consumer" "$builder_endpoint/helmr/bundle-builder@$digest" <<'PY'
import sys
from consumer import consumer
consumer(*sys.argv[1:])
PY
