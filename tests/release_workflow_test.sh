#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
workflow="$repo_root/.github/workflows/release.yaml"
platform_builder="$repo_root/scripts/build-platform-release.sh"
canonical_json_check="$repo_root/scripts/check-canonical-json.sh"
control_builder="$repo_root/scripts/build-control-image.sh"
worker_module="$repo_root/infra/aws/modules/worker-image"
worker_module_main="$worker_module/main.tf"
worker_image_builder="$worker_module/templates/build-worker-image.sh.tftpl"

require_text() {
  text="$1"
  file="$2"
  message="$3"
  if ! rg -F -- "$text" "$file" >/dev/null; then
    printf '%s\n' "$message" >&2
    exit 1
  fi
}

reject_text() {
  text="$1"
  shift
  message="${!#}"
  set -- "${@:1:$(($# - 1))}"
  if rg -F -- "$text" "$@" >/dev/null; then
    printf '%s\n' "$message" >&2
    exit 1
  fi
}

require_text "name: platform release" "$workflow" \
  "release workflow does not build the Platform release"
require_text "name: development platform release" "$workflow" \
  "release workflow has no branch-scoped development Platform release"
require_text "environment: release-production" "$workflow" \
  "production Platform release is not approval-gated"
require_text "environment: dev-runtime" "$workflow" \
  "development Platform release is not isolated"
require_text "id-token: write" "$workflow" \
  "Platform release signing has no OIDC authority"
require_text "./scripts/build-platform-release.sh dist/platform-release" "$workflow" \
  "release jobs do not share the deterministic Platform release builder"
require_text "--pattern 'platform-release*'" "$workflow" \
  "repair does not consume the published Platform release"
require_text "cosign verify-blob" "$workflow" \
  "repair does not verify the published Platform release"
require_text "tar -xOf dist/platform-release/platform-release.tar ./build-policy.digest" "$workflow" \
  "repair trusts unsigned build-policy provenance instead of the signed archive"
require_text "refs/tags/\$RELEASE_TAG" "$workflow" \
  "repair verification is not bound to the exact tag workflow identity"
require_text "platform-release/platform-release.tar" "$workflow" \
  "GitHub release omits the Platform release archive"
require_text "platform-release/platform-release.sigstore.json" "$workflow" \
  "GitHub release omits Platform release signature evidence"
require_text "platform-release/platform-release-provenance.json" "$workflow" \
  "GitHub release omits Platform release provenance"
require_text 'VERIFY_RELEASE_ARTIFACTS: "1"' "$workflow" \
  "AWS release manifest does not verify published image and AMI visibility"
# shellcheck disable=SC2016
require_text 'REQUIRED_WORKER_AMI_REGIONS: ${{ env.WORKER_AMI_REGIONS }}' "$workflow" \
  "AWS release manifest does not verify every configured Worker AMI region"

require_text '#platformRelease' "$platform_builder" \
  "Platform release is not built from the Nix-pinned package"
require_text "--sort=name" "$platform_builder" \
  "Platform release archive ordering is not deterministic"
require_text "--mtime='@0'" "$platform_builder" \
  "Platform release archive timestamps are not normalized"
require_text "cosign sign-blob" "$platform_builder" \
  "Platform release archive is not signed"
require_text "build-policy.digest" "$platform_builder" \
  "Platform release provenance omits the exact Build Policy digest"

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

require_text "./scripts/build-control-image.sh \"\$IMAGE_URI\"" "$workflow" \
  "Control image is not built from its source-only builder"
require_text "COPY helmr-control /usr/local/bin/helmr-control" "$control_builder" \
  "Control image omits the Control binary"
require_text "COPY helmr-dispatcher /usr/local/bin/helmr-dispatcher" "$control_builder" \
  "Control image omits the Dispatcher binary"

require_text "scripts/aws-release-artifacts.sh worker-image-start" "$workflow" \
  "Worker release does not build a fresh AMI"
reject_text "scripts/aws-dev-smoke.sh" "$workflow" \
  "Product release workflow still depends on the removed Managed Cloud orchestrator"
require_text "workerAMIs" "$workflow" \
  "Worker release artifact omits region-to-AMI identity"
require_text "gpgv" "$worker_image_builder" \
  "Worker AMI omits the Node signature verifier"
require_text "mksquashfs" "$worker_image_builder" \
  "Worker AMI omits the Platform tree composer"

reject_text "runtime-release" "$workflow" "$control_builder" "$worker_module_main" "$worker_image_builder" \
  "release flow still contains the retired Runtime release catalog"
reject_text "manager-release" "$workflow" "$control_builder" "$worker_module_main" "$worker_image_builder" \
  "release flow still contains the retired Manager release catalog"
reject_text "standardToolchain" "$workflow" "$control_builder" "$worker_module_main" "$worker_image_builder" \
  "release flow still contains the retired standard-toolchain catalog"
reject_text "managerRelease" "$workflow" "$control_builder" "$worker_module_main" "$worker_image_builder" \
  "release flow still builds the retired Manager release"
reject_text "release_package_" "$worker_module_main" "$worker_image_builder" \
  "Worker AMI still accepts a baked release package"
reject_text "CONTROL_IMAGE_RUNTIME_RELEASE_DIR" "$control_builder" \
  "Control image still accepts a Runtime catalog input"
reject_text "CONTROL_IMAGE_MANAGER_RELEASE_DIR" "$control_builder" \
  "Control image still accepts a Manager catalog input"

printf 'ok - release workflow tests\n'
