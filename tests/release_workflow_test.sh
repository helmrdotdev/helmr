#!/usr/bin/env bash
# shellcheck disable=SC2016
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
workflow="$repo_root/.github/workflows/release.yaml"
platform_builder="$repo_root/scripts/build-platform-release.sh"
canonical_json_check="$repo_root/scripts/check-canonical-json.sh"
controlplane_builder="$repo_root/scripts/build-controlplane-image.sh"
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
  file="$2"
  message="$3"
  if rg -F -- "$text" "$file" >/dev/null; then
    printf '%s\n' "$message" >&2
    exit 1
  fi
}

job_text() {
  job="$1"
  awk -v header="  ${job}:" '
    $0 == header { found = 1 }
    found && $0 != header && $0 ~ /^  [A-Za-z0-9_-]+:$/ { exit }
    found { print }
  ' "$workflow"
}

require_job_text() {
  job="$1"
  text="$2"
  message="$3"
  if ! job_text "$job" | rg -F -- "$text" >/dev/null; then
    printf '%s\n' "$message" >&2
    exit 1
  fi
}

reject_job_text() {
  job="$1"
  text="$2"
  message="$3"
  if job_text "$job" | rg -F -- "$text" >/dev/null; then
    printf '%s\n' "$message" >&2
    exit 1
  fi
}

require_text "name: platform release" "$workflow" \
  "release workflow does not build the Platform release"
if rg -F -e '"capacity/v*"' -e "name: capacity module" "$workflow" >/dev/null; then
  printf '%s\n' "release workflow still contains the retired capacity module lane" >&2
  exit 1
fi
require_text "name: development platform release" "$workflow" \
  "release workflow has no branch-scoped development Platform release"
require_text '      - "v*"' "$workflow" \
  "release workflow does not trigger from the Platform tag"
if [ "$(sed -n '/^  push:/,/^  workflow_dispatch:/p' "$workflow" | rg -c '^      - ')" -ne 1 ]; then
  printf '%s\n' "release workflow has more than one tag trigger" >&2
  exit 1
fi
reject_text 'sdk/typescript/v*' "$workflow" \
  "release workflow still triggers from an SDK-specific tag"
reject_text "environment: release-production" "$workflow" \
  "release workflow still uses the retired production Environment"
reject_text "environment: dev-runtime" "$workflow" \
  "release workflow still uses the retired development Environment"
reject_job_text "platform-release-dev" "environment:" \
  "development Platform signing must be environmentless"
require_job_text "release-admission" "environment: release" \
  "release admission is not gated by the release Environment"
for setting in RELEASE_AWS_ROLE_ARN RELEASE_AWS_STATE_BUCKET RELEASE_WORKER_IMAGE_ARTIFACT_BUCKET; do
  require_job_text "release-admission" "${setting} is required" \
    "release admission does not fail closed on ${setting}"
done
for job in platform-release bundle-builder-image worker-ami publish github-release typescript-sdk-npm-publish; do
  require_job_text "$job" "environment: release" \
    "$job is not gated by the release Environment"
done
if rg '^[[:space:]]+environment:' "$workflow" | rg -v 'environment: release$' >/dev/null; then
  printf '%s\n' "release is not the only workflow Environment" >&2
  exit 1
fi
require_text "id-token: write" "$workflow" \
  "Platform release signing has no OIDC authority"
require_text "./scripts/build-platform-release.sh dist/platform-release" "$workflow" \
  "release jobs do not share the deterministic Platform release builder"
reject_text "repair" "$workflow" \
  "release workflow still contains the retired same-tag repair path"
require_text "name: version cohort" "$workflow" \
  "release workflow does not gate publication on one version cohort"
require_job_text "version-cohort" "github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v')" \
  "release workflow permits a non-push event to enter the release graph"
if ! job_text "version-cohort" |
  awk 'index($0, "nix develop --command tests/version_cohort_test.sh") { exit } { print }' |
  rg -F 'grep -Eq "$PLATFORM_RELEASE_TAG_PATTERN"' >/dev/null; then
  printf '%s\n' "release tag validation does not precede the Nix cohort check" >&2
  exit 1
fi
require_text 'RELEASE_TAG="$GITHUB_REF_NAME" nix develop --command tests/version_cohort_test.sh "$GITHUB_REF_NAME" "$(git rev-parse HEAD)"' "$workflow" \
  "release cohort check is not bound to the tag and full source commit"
require_text 'HELMR_PLATFORM_VERSION="${RELEASE_TAG}" nix build --impure' "$repo_root/scripts/aws-release-artifacts.sh" \
  "Worker release does not pass the canonical cohort identity"
require_text "SourceCommit=\${source_commit}" "$workflow" \
  "CLI release does not stamp the full source commit"
require_text "platform-release/platform-release.tar" "$workflow" \
  "GitHub release omits the Platform release archive"
require_text "platform-release/platform-release.sigstore.json" "$workflow" \
  "GitHub release omits Platform release signature evidence"
require_text "platform-release/platform-release-provenance.json" "$workflow" \
  "GitHub release omits Platform release provenance"
require_text "name: bundle builder image" "$workflow" \
  "release workflow does not publish the canonical bundle builder"
require_text 'BUILDER_IMAGE_REPOSITORY: ghcr.io/${{ github.repository }}/bundle-builder' "$workflow" \
  "bundle builder does not use the approved nested package name"
require_text "nix build .#bundleBuilderImage" "$workflow" \
  "bundle builder is not sourced from the pinned Product derivation"
require_text "docker buildx imagetools inspect" "$workflow" \
  "bundle builder publication does not resolve the registry digest"
require_text "main.deploymentBundleBuilderImage=\${BUNDLE_BUILDER_IMAGE}" "$workflow" \
  "CLI release is not bound to the exact bundle builder digest"
require_text "dist/bundle-builder/bundle-builder.json" "$workflow" \
  "GitHub release omits the bundle builder release identity"
require_text 'VERIFY_RELEASE_ARTIFACTS: "1"' "$workflow" \
  "AWS release manifest does not verify published image and AMI visibility"
# shellcheck disable=SC2016
require_text 'REQUIRED_WORKER_AMI_REGIONS: ${{ env.WORKER_AMI_REGIONS }}' "$workflow" \
  "AWS release manifest does not verify every configured Worker AMI region"
require_job_text "worker-ami" "WORKER_IMAGE_NAME_BASE:" \
  "Worker Image Builder does not default to the approved helmr-worker name"
require_job_text "worker-ami" "'helmr-worker'" \
  "Worker Image Builder does not default to the approved helmr-worker name"
reject_job_text "worker-ami" "release-worker-ami-cleanup.sh" \
  "tag publication still invokes Worker AMI cleanup"
reject_job_text "worker-ami" "RELEASE_WORKER_AMI_KEEP" \
  "tag publication still receives the retired AMI retention value"

for job in platform-release bundle-builder-image controlplane-image worker-ami typescript-sdk-packages; do
  require_job_text "$job" "needs: release-admission" \
    "release admission does not dominate $job"
done
for job in version-cohort release-admission platform-release bundle-builder-image cli controlplane-image worker-ami publish verify-public-images github-release typescript-sdk-packages typescript-sdk-npm-publish; do
  first_step="$(
    job_text "$job" |
      awk '
        $0 == "    steps:" { in_steps = 1; next }
        in_steps && /^      - / {
          if (found) exit
          found = 1
        }
        found { print }
      '
  )"
  for text in \
    "- name: Reject a release rerun" \
    "if: github.run_attempt != 1" \
    "release tags are single-attempt; create a new prerelease tag after any failed or partial publication" \
    "exit 1"; do
    if ! printf '%s\n' "$first_step" | rg -F -- "$text" >/dev/null; then
      printf '%s\n' "$job does not reject a rerun in its first step: $text" >&2
      exit 1
    fi
  done
  reject_job_text "$job" "github.run_attempt == 1" \
    "$job is skipped instead of visibly rejecting a rerun"
done
reject_job_text "platform-release-dev" "github.run_attempt" \
  "development signing is incorrectly restricted to a first workflow attempt"
require_job_text "verify-public-images" "needs:" \
  "anonymous image verification has no publication dependency"
require_job_text "verify-public-images" "- publish" \
  "anonymous image verification does not wait for authenticated publication"
require_job_text "verify-public-images" 'DOCKER_CONFIG="$RUNNER_TEMP/anonymous-docker"' \
  "image verification does not use an isolated Docker configuration"
reject_job_text "verify-public-images" "docker login" \
  "anonymous image verification logs in to a registry"
reject_job_text "verify-public-images" "environment:" \
  "anonymous image verification must not receive the release Environment"
require_job_text "github-release" "- verify-public-images" \
  "GitHub Release does not wait for anonymous image verification"
require_job_text "github-release" 'name: Helmr ${{ env.RELEASE_TAG }}' \
  "GitHub Release does not use the approved display name"
require_job_text "github-release" "dist/worker/worker-image.json" \
  "GitHub Release omits worker-image.json"
require_job_text "publish" "./.github/actions/setup-nix" \
  "AWS manifest signing does not use the pinned Nix toolchain"
require_job_text "publish" "id-token: write" \
  "AWS manifest signing has no keyless OIDC authority"
require_job_text "publish" "nix develop .#images --command" \
  "AWS manifest signing does not use the pinned image toolchain"
require_job_text "publish" "cosign sign-blob" \
  "AWS release manifest is not signed"
require_job_text "publish" "--bundle dist/aws-artifacts.sigstore.json" \
  "AWS release manifest signing does not write the exact companion bundle"
require_job_text "publish" "dist/aws-artifacts.json" \
  "AWS release manifest signing does not use the exact manifest path"
reject_job_text "publish" "cosign verify-blob" \
  "producer workflow reverifies the freshly signed AWS release manifest"
reject_job_text "platform-release-dev" "aws-artifacts" \
  "development Platform signing also signs an AWS release manifest"
require_job_text "github-release" "dist/aws-artifacts.sigstore.json" \
  "GitHub Release omits the AWS release manifest signature evidence"

publish_job="$(job_text publish)"
manifest_line="$(printf '%s\n' "$publish_job" | rg -n -F 'scripts/write-aws-release-manifest.sh' | cut -d: -f1)"
sign_line="$(printf '%s\n' "$publish_job" | rg -n -F 'cosign sign-blob' | cut -d: -f1)"
artifact_line="$(printf '%s\n' "$publish_job" | rg -n -F 'name: aws-release-manifest' | cut -d: -f1)"
if [ -z "$manifest_line" ] || [ -z "$sign_line" ] || [ -z "$artifact_line" ] ||
  [ "$manifest_line" -ge "$sign_line" ] || [ "$sign_line" -ge "$artifact_line" ]; then
  printf '%s\n' "AWS release manifest must be finalized, signed, then uploaded" >&2
  exit 1
fi
github_release_before_action="$(
  job_text "github-release" |
    awk 'index($0, "softprops/action-gh-release@") { exit } { print }'
)"
for text in \
  'GITHUB_TOKEN: ${{ github.token }}' \
  '"https://api.github.com/repos/$GITHUB_REPOSITORY/releases/tags/$RELEASE_TAG"' \
  '404) ;;' \
  '200) echo "GitHub Release $RELEASE_TAG already exists; a new prerelease tag is required" >&2; exit 1 ;;' \
  '*) echo "failed to check GitHub Release $RELEASE_TAG: HTTP $status" >&2; exit 1 ;;'; do
  require_job_text "release-admission" "$text" \
    "release admission does not reject an existing GitHub Release: $text"
  if ! printf '%s\n' "$github_release_before_action" | rg -F -- "$text" >/dev/null; then
    printf '%s\n' "final GitHub Release existence guard is missing or follows publication: $text" >&2
    exit 1
  fi
done
require_job_text "typescript-sdk-packages" 'ref: ${{ github.sha }}' \
  "TypeScript SDK packages are not checked out from the immutable cohort commit"
reject_job_text "typescript-sdk-packages" 'ref: ${{ github.ref }}' \
  "TypeScript SDK packages still check out the mutable release ref"
require_job_text "typescript-sdk-npm-publish" "- github-release" \
  "npm publication does not wait for the GitHub Release"
require_job_text "typescript-sdk-npm-publish" "jq -e '.isDraft == false'" \
  "npm publication does not verify the non-draft GitHub Release"
require_job_text "typescript-sdk-npm-publish" 'npm view "${package_name}@${SDK_VERSION}"' \
  "npm publication does not check for an existing package version"
require_job_text "typescript-sdk-npm-publish" 'is already published; a new prerelease tag is required' \
  "npm publication does not reject an existing package version"
reject_job_text "typescript-sdk-npm-publish" "skipping" \
  "npm publication still succeeds when a package version already exists"
require_job_text "typescript-sdk-npm-publish" "publish_package @helmr/proto" \
  "npm publication omits @helmr/proto"
require_job_text "typescript-sdk-npm-publish" "publish_package @helmr/sdk" \
  "npm publication omits @helmr/sdk"
reject_text "typescript-sdk-release:" "$workflow" \
  "release workflow still creates an SDK-specific GitHub Release"
require_text 'dist/helmr-${RELEASE_TAG}-${os}-${arch}.tar.gz' "$workflow" \
  "CLI release archives are not versioned"

require_text '#platformRelease' "$platform_builder" \
  "Platform release is not built from the Nix-pinned package"
require_text "--sort=name" "$platform_builder" \
  "Platform release archive ordering is not deterministic"
require_text "--mtime='@0'" "$platform_builder" \
  "Platform release archive timestamps are not normalized"
require_text "cosign sign-blob" "$platform_builder" \
  "Platform release archive is not signed"

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

require_text "./scripts/build-controlplane-image.sh \"\$IMAGE_URI\"" "$workflow" \
  "Control Plane image is not built from its source-only builder"
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

require_text "scripts/aws-release-artifacts.sh worker-image-start" "$workflow" \
  "Worker release does not select or build a fully validated AMI"
require_text 'WORKER_IMAGE_ARTIFACT_BUCKET: ${{ vars.RELEASE_WORKER_IMAGE_ARTIFACT_BUCKET }}' "$workflow" \
  "Worker release does not receive the immutable artifact bucket"
require_text 'RELEASE_WORKER_IMAGE_ARTIFACT_BUCKET is required' "$workflow" \
  "Worker release does not reject a missing immutable artifact bucket"
require_text "scripts/aws-release-artifacts.sh worker-image-receipt" "$workflow" \
  "Worker release does not emit the closed Worker image receipt"
require_text "dist/worker-image.json" "$workflow" \
  "Worker release artifact does not preserve the closed Worker image receipt"
require_text '"$(cat dist/worker/worker-image.json)"' "$workflow" \
  "final AWS release manifest does not embed the Worker image receipt"
require_text "gpgv" "$worker_image_builder" \
  "Worker AMI omits the Node signature verifier"
require_text "squashfs-tools" "$worker_image_builder" \
  "Worker AMI omits the Platform tree composer package"
require_text "mksquashfs version 4.6.1" "$worker_module_main" \
  "Worker AMI does not validate the exact Platform tree composer contract"

printf 'ok - release workflow tests\n'
