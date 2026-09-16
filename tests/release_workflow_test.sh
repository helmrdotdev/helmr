#!/usr/bin/env bash
# Publication behavior and trust admission are exercised by tests/release/*.py.
set -euo pipefail
repo_root=$(cd "$(dirname "$0")/.." && pwd)
workflow="$repo_root/.github/workflows/release.yaml"
canonical_json_check="$repo_root/scripts/check-canonical-json.sh"
controlplane_builder="$repo_root/scripts/build-controlplane-image.sh"
require_text() { rg -F -- "$1" "$2" >/dev/null || { echo "$3" >&2; exit 1; }; }
require_text "'!v*-preview.*'" "$workflow" 'generated preview tags must be excluded'
require_text 'workflow_run:' "$workflow" 'automatic main preview trigger missing'
require_text 'workflow_dispatch:' "$workflow" 'manual exact PR head trigger missing'
for caller in "$workflow" "$repo_root/.github/workflows/ci.yaml"; do
  require_text 'uses: ./.github/workflows/build-artifacts.yaml' "$caller" \
    'CI and release must use the same artifact build workflow'
done
require_text 'name: build-artifacts-${{ github.run_id }}-${{ matrix.part }}' \
  "$repo_root/.github/workflows/build-artifacts.yaml" 'component upload name differs from retry lookup'
require_text 'name: build-artifacts-${{ github.run_id }}-cli' \
  "$repo_root/.github/workflows/build-artifacts.yaml" 'CLI upload name differs from retry lookup'
if rg 'aws-actions|worker-ami|platform-release-dev' "$workflow"; then exit 1; fi
python3 -m unittest discover -s "$repo_root/tests/release" -v
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
printf '{"formatVersion":0}' >"$tmp/canonical.json"
"$canonical_json_check" "$tmp/canonical.json"
printf '"value"' >"$tmp/canonical-string.json"
"$canonical_json_check" "$tmp/canonical-string.json"
: >"$tmp/empty.json"
if "$canonical_json_check" "$tmp/empty.json" >/dev/null 2>&1; then
  printf 'canonical JSON check accepted an empty stream\n' >&2
  exit 1
fi
printf '{}[]' >"$tmp/multiple.json"
if "$canonical_json_check" "$tmp/multiple.json" >/dev/null 2>&1; then
  printf 'canonical JSON check accepted multiple values\n' >&2
  exit 1
fi

require_text "COPY control-plane /usr/local/bin/control-plane" "$controlplane_builder" \
  "Control Plane image omits the Control Plane binary"
require_text "COPY dispatcher /usr/local/bin/dispatcher" "$controlplane_builder" \
  "Control Plane image omits the Dispatcher binary"
require_text 'ENTRYPOINT ["/usr/local/bin/control-plane"]' "$controlplane_builder" \
  "Control Plane image does not start the Control Plane binary"
require_text "COPY runtime.descriptor.json /usr/local/share/helmr/runtime.descriptor.json" "$controlplane_builder" \
  "Control Plane image omits the canonical Runtime descriptor"
require_text "COPY zoneinfo/ /usr/share/zoneinfo/" "$controlplane_builder" \
  "Control Plane image omits the pinned timezone rules"
require_text "COPY tzdb_names.txt /usr/local/share/helmr/tzdb_names.txt" "$controlplane_builder" \
  "Control Plane image omits the canonical timezone manifest"
require_text 'DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH = "/usr/local/share/helmr/runtime.descriptor.json"' \
  "$repo_root/infra/aws/modules/controlplane/main.tf" \
  "Control Plane task does not select the packaged Runtime descriptor"
require_text '"${docker_bin}" create "${image_uri}"' "$repo_root/scripts/verify-controlplane-image-build.sh" \
  "Control Plane image verifier does not inspect the distroless filesystem"
require_text '"${docker_bin}" cp' "$repo_root/scripts/verify-controlplane-image-build.sh" \
  "Control Plane image verifier does not extract the Runtime descriptor"

printf 'ok - common release tests\n'
