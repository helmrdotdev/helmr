#!/usr/bin/env bash
# Publication behavior and trust admission are exercised by tests/release/*.py.
set -euo pipefail
repo_root=$(cd "$(dirname "$0")/.." && pwd)
workflow="$repo_root/.github/workflows/release.yaml"
canonical_json_check="$repo_root/scripts/check-canonical-json.sh"
controlplane_builder="$repo_root/scripts/build-controlplane-image.sh"
require_text() { rg -F -- "$1" "$2" >/dev/null || { echo "$3" >&2; exit 1; }; }
require_text "'!v*-preview.*'" "$workflow" 'generated preview tags must be excluded'
if rg -q 'workflow_run:|ci:full|superseded|precheck' "$workflow"; then
  echo 'release must be explicitly requested, without automatic supersession' >&2
  exit 1
fi
require_text 'workflow_dispatch:' "$workflow" 'manual checkpoint trigger missing'
require_text 'uses: ./.github/workflows/build-artifacts.yaml' "$workflow" 'release must build its own exact cohort'
require_text "needs.integration.result == 'success'" "$workflow" 'integration must pass before publication'
require_text "needs.build.result == 'success'" "$workflow" 'artifact build must pass before publication'
require_text 'publisher_run: ${{ github.run_id }}' "$workflow" \
  'publish job must export publisher run for readback verification'
require_text 'group: release-checkpoint' "$workflow" 'checkpoint releases must serialize'
require_text 'queue: max' "$workflow" 'requested checkpoints must not evict pending work'
require_text 'cancel-in-progress: false' "$workflow" 'publication must not be cancelled by a later request'
require_text 'needs.publish-preview.result == '\''success'\''' "$workflow" \
  'verify and complete-preview must gate on successful preview publish'
require_text 'needs.publish-tag.result == '\''success'\''' "$workflow" \
  'verify and complete-tag must gate on successful tag publish'
require_text 'environment: preview' "$workflow" \
  'preview jobs must use literal preview environment'
require_text 'environment: release' "$workflow" \
  'formal tag jobs must use literal release environment'
bash "$repo_root/tests/release/test_assume_preview_role.sh"
python3 <<PY
import re
import sys
from pathlib import Path

text = Path("$workflow").read_text()
jobs = ('publish-preview', 'complete-preview', 'discovery')
consumers = ('main.py finalize', 'main.py discover', 'main.py stage')
assume = 'assume-preview-role.sh'
markers = [(m.group(1), m.start()) for m in re.finditer(r'^  ([a-z][a-z0-9_-]*):\n', text, re.M)]

def block(name):
    idx = next(i for i, (job, _) in enumerate(markers) if job == name)
    start = markers[idx][1]
    end = markers[idx + 1][1] if idx + 1 < len(markers) else len(text)
    return text[start:end]

for job in ('admission', 'publish-preview', 'publish-tag', 'complete-preview', 'complete-tag', 'discovery'):
    body = block(job)
    shells = re.findall(r'nix develop \.#([^ ]+) -c', body)
    if not shells or set(shells) != {'release'}:
        sys.exit(f'{job}: trusted release steps must use the pinned publication tools')
    if 'consumer(root' in body or 'HELMR_PUBLIC_CONSUMER' in body:
        sys.exit(f'{job}: candidate consumer must remain in the separate unprivileged job')
    if 'actions/cache@' in body or 'setup-node@' in body:
        sys.exit(f'{job}: release tools must come from the pinned Nix closure without executable caches')

verify = block('verify')
if set(re.findall(r'nix develop \.#([^ ]+) -c', verify)) != {'release-consumer'}:
    sys.exit('verify: public download and candidate execution must use the separate consumer tools')
if 'environment:' in verify or 'id-token:' in verify or 'write' in verify:
    sys.exit('verify: candidate execution must not have publication authority')
if 'HELMR_PUBLIC_CONSUMER' not in verify or 'consumer(root' not in verify:
    sys.exit('verify: downloaded public consumer must execute')
if 'nix develop .#images -c' in text:
    sys.exit('release workflow must not realize the source build shell')
build = Path("$repo_root/.github/workflows/build-artifacts.yaml").read_text()
if not re.search(r'nix develop [^\n]+#images["\x27]? -c', build):
    sys.exit('actual artifact construction must retain the full images shell')

for job in jobs:
    body = block(job)
    steps = re.split(r'\n      - name:', body)[1:]
    assume_step = consumer_step = None
    for index, step in enumerate(steps, start=1):
        run = step.split('run:', 1)[-1] if 'run:' in step else ''
        if assume in run:
            if any(marker in run for marker in consumers):
                sys.exit(f'{job}: assume helper must not run in the same step as preview consumer commands')
            assume_step = index
        if any(marker in run for marker in consumers):
            consumer_step = index
    if consumer_step is None:
        sys.exit(f'{job}: missing preview consumer step')
    if assume_step is None or assume_step >= consumer_step:
        sys.exit(f'{job}: preview consumer step must follow a separate earlier assume step')
print('ok - preview assume steps precede consumers')
PY
if [ "$(rg -c '^concurrency:' "$workflow" || true)" != 1 ]; then
  printf 'release workflow must declare exactly one top-level producer concurrency\n' >&2
  exit 1
fi
actionlint_config="$repo_root/.github/actionlint.yaml"
require_text 'unexpected key "queue" for "concurrency" section' "$actionlint_config" \
  'actionlint must ignore only the queue field false positive on release.yaml'
require_text '.github/workflows/release.yaml:' "$actionlint_config" \
  'actionlint ignore must be scoped to release.yaml only'
require_text 'name: build-artifacts-${{ github.run_id }}-${{ matrix.part }}' \
  "$repo_root/.github/workflows/build-artifacts.yaml" 'component upload name differs from retry lookup'
require_text 'name: build-artifacts-${{ github.run_id }}-cli' \
  "$repo_root/.github/workflows/build-artifacts.yaml" 'CLI upload name differs from retry lookup'
require_text 'PREVIEW_PUBLISHER_ROLE_ARN' "$workflow" \
  'preview publication must assume the dedicated publisher role'
require_text 'main.py verify' "$workflow" \
  'preview verification must reconstruct cohort bytes from public object store'
require_text 'channels/preview.json' "$repo_root/scripts/release/preview_store.py" \
  'preview pointer path must remain channels/preview.json'
require_text 'assume-preview-role.sh' "$workflow" \
  'preview jobs must share one OIDC assume-role script'
require_text 'assume-role-with-web-identity' "$repo_root/scripts/release/assume-preview-role.sh" \
  'preview publisher credentials must use native AWS CLI OIDC'
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
