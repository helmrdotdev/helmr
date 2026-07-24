#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
workflow="$repo_root/.github/workflows/release.yaml"
descriptor="$repo_root/.github/runtime-release.json"
control_builder="$repo_root/scripts/build-control-image.sh"

section() {
	start="$1"
	end="$2"
	sed -n "/^  ${start}:/,/^  ${end}:/p" "$workflow"
}

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

if ! cmp -s "$descriptor" <(printf '%s' "$(jq -cS . "$descriptor")"); then
	printf 'runtime release descriptor is not canonical JSON\n' >&2
	exit 1
fi

jq -e '
	keys == ["formatVersion", "predecessor", "release"] and
	.formatVersion == 0 and
	(.release | test("^v[0-9]+[.][0-9]+[.][0-9]+(-[0-9A-Za-z-]+([.][0-9A-Za-z-]+)*)?$")) and
	(
		.predecessor == null or
		(
			(.predecessor | keys) == ["digest", "release", "sizeBytes"] and
			(.predecessor.release | test("^v[0-9]+[.][0-9]+[.][0-9]+(-[0-9A-Za-z-]+([.][0-9A-Za-z-]+)*)?$")) and
			(.predecessor.digest | test("^sha256:[0-9a-f]{64}$")) and
			(.predecessor.sizeBytes | type == "number" and . > 0 and floor == .)
		)
	)
' "$descriptor" >/dev/null

tag_job="$(mktemp)"
dispatch_job="$(mktemp)"
control_job="$(mktemp)"
worker_job="$(mktemp)"
publish_job="$(mktemp)"
workflow_header="$(mktemp)"
trap 'rm -f "$tag_job" "$dispatch_job" "$control_job" "$worker_job" "$publish_job" "$workflow_header"' EXIT
section runtime-release-tag runtime-release-dispatch >"$tag_job"
section runtime-release-dispatch cli >"$dispatch_job"
section control-image typescript-sdk-packages >"$control_job"
section worker-ami publish >"$worker_job"
sed -n '/^  publish:/,$p' "$workflow" >"$publish_job"
sed -n '/^concurrency:/,/^jobs:/p' "$workflow" >"$workflow_header"

require_text "queue: max" "$workflow_header" \
	"same-tag workflow reruns can be dropped before runtime composition"
require_text "cancel-in-progress: false" "$workflow_header" \
	"same-tag workflow reruns can cancel an in-progress runtime composition"
require_text "group: official-runtime-release" "$tag_job" \
	"official runtime composition is not serialized across platform tags"
require_text "queue: max" "$tag_job" \
	"official runtime composition drops older pending platform tags"
require_text "cancel-in-progress: false" "$tag_job" \
	"official runtime composition can be cancelled"
require_text "id-token: write" "$tag_job" \
	"platform-tag runtime composition lacks keyless signing authority"
require_text "cosign attest-blob" "$tag_job" \
	"platform-tag runtime composition does not sign the complete statement"
require_text "--statement \"\$release_dir/attestation.json\"" "$tag_job" \
	"runtime signing is not bound to the complete in-toto statement"
require_text "--toolchain-source \"\$standard_toolchain\"" "$tag_job" \
	"standard-toolchain catalog is not composed from the captured Nix release"
require_text ".#standardToolchain" "$tag_job" \
	"platform release does not build the standard-toolchain candidate"
reject_text ".#dependencyTools" "$workflow" \
	"platform release still builds the retired Manager/toolset release"
reject_text "--tool-registry" "$workflow" \
	"platform release still consumes a serving-time dependency tool registry"
require_text "--statement \"\$release_dir/toolchain-release/attestation.json\"" "$tag_job" \
	"standard-toolchain signing is not bound to the complete in-toto statement"
require_text "published lineage head" "$tag_job" \
	"runtime composition does not reject a stale checked-in predecessor"
require_text "expected_digest" "$tag_job" \
	"runtime composition does not exact-check the predecessor digest"
require_text "expected_size" "$tag_job" \
	"runtime composition does not exact-check the predecessor length"
require_text "gh release upload \"\$RELEASE_TAG\" \"\$dist/runtime-release.tar\"" "$tag_job" \
	"complete runtime release is not published as its fixed asset"
reject_text "--clobber" "$workflow" \
	"release workflow permits replacement of create-only runtime assets"

require_text "permissions:" "$dispatch_job" \
	"manual runtime consumption has no explicit permission boundary"
require_text "contents: read" "$dispatch_job" \
	"manual runtime consumption cannot read the exact published asset"
reject_text "id-token: write" "$dispatch_job" \
	"manual dispatch can obtain signing authority"
reject_text "cosign " "$dispatch_job" \
	"manual dispatch can sign a runtime release"
reject_text " compose " "$dispatch_job" \
	"manual dispatch can compose a runtime release"
require_text "ref: refs/tags/\${{ inputs.tag }}" "$dispatch_job" \
	"manual dispatch does not check out the exact existing release tag"
reject_text "ref: \${{ env.RELEASE_TAG }}" "$workflow" \
	"platform release checkout can prefer a same-name branch over the release tag"
require_text "--pattern runtime-release.tar" "$dispatch_job" \
	"manual dispatch does not consume the fixed complete distribution"
require_text "--trusted-root \"\$trusted_root\"" "$tag_job" \
	"tag rerun can trust the archive embedded root"
require_text "--trusted-root \"\$trusted_root\"" "$dispatch_job" \
	"manual dispatch can trust the archive embedded root"
if [ "$(rg -F -- '--trusted-root "$trusted_root"' "$workflow" | wc -l | tr -d ' ')" -lt 6 ]; then
	printf 'Manager verification does not use the checkout-pinned trust root in both release paths\n' >&2
	exit 1
fi
if [ "$(rg -F -- 'cmp -s "$manager_dist/trusted-root.json" "$trusted_root"' "$workflow" | wc -l | tr -d ' ')" -ne 2 ]; then
	printf 'Manager archives are not exact-checked against the pinned trust root in both release paths\n' >&2
	exit 1
fi

require_text "name: runtime-release-assets" "$control_job" \
	"control image does not consume the verified runtime release"
require_text "name: manager-release-assets" "$control_job" \
	"control image does not consume the verified Manager release"
require_text "verify-archive" "$control_job" \
	"control image does not re-verify its complete runtime distribution"
require_text "CONTROL_IMAGE_RUNTIME_RELEASE_DIR=\"\$verified\"" "$control_job" \
	"control image build is not bound to verifier output"
require_text "CONTROL_IMAGE_MANAGER_RELEASE_DIR=\"\$GITHUB_WORKSPACE/dist/manager-release\"" "$control_job" \
	"control image build is not bound to the Manager release"
require_text "name: runtime-release-assets" "$worker_job" \
	"worker AMI does not consume the verified runtime release"
require_text "name: manager-release-assets" "$worker_job" \
	"worker AMI does not consume the verified Manager release"
require_text "verify-worker" "$worker_job" \
	"worker AMI does not re-verify its architecture package"
require_text "--snapshot \"\$verified_runtime\"" "$worker_job" \
	"worker AMI does not snapshot the verifier-owned runtime package"
require_text "python3 scripts/package-worker-release.py" "$worker_job" \
	"worker AMI does not compose the verified runtime and Manager release"
require_text "WORKER_IMAGE_RELEASE_PACKAGE=\"\$verified_package\"" "$worker_job" \
	"worker AMI does not stage the composed verified package"
require_text "WORKER_IMAGE_RELEASE_PACKAGE_SHA256=\"\${digest#sha256:}\"" "$worker_job" \
	"worker package staging is not bound to the verifier digest"
require_text "WORKER_IMAGE_RELEASE_PACKAGE_SIZE_BYTES=\"\$size_bytes\"" "$worker_job" \
	"worker package staging is not bound to the verifier length"
require_text "scripts/aws-dev-smoke.sh worker-release-stage" "$worker_job" \
	"worker AMI does not stage the exact versioned runtime package"
require_text "worker-runtime-release.json" "$worker_job" \
	"worker artifact omits the staged runtime package identity"
require_text "name: worker-release-artifact" "$worker_job" \
	"closed Worker image and transport identity are not retained together"
reject_text "worker_amis_json:" "$workflow" \
	"manual release input accepts AMI IDs without package provenance"
reject_text "WORKER_AMIS_INPUT" "$worker_job" \
	"worker release can bypass the verified package build with unbound AMI IDs"
reject_text "Use provided worker AMIs" "$worker_job" \
	"worker release contains an unprovenanceable provided-AMI branch"
require_text "scripts/aws-dev-smoke.sh worker-image-start" "$worker_job" \
	"worker release does not always build AMIs from the verified package"
require_text "dist/worker/worker-artifacts.json" "$publish_job" \
	"AWS release manifest is not bound to the Worker image recipe transport"

require_text "CONTROL_IMAGE_RUNTIME_RELEASE_DIR is required" "$control_builder" \
	"control image builder accepts an unbound runtime release"
require_text "CONTROL_IMAGE_MANAGER_RELEASE_DIR is required" "$control_builder" \
	"control image builder accepts an unbound Manager release"
require_text "COPY --chown=0:0 --chmod=0444 runtime-release/catalog.json" "$control_builder" \
	"control image catalog is not installed root-owned and read-only"
require_text "COPY --chown=0:0 --chmod=0444 runtime-release/catalog.sigstore.json" "$control_builder" \
	"control image Sigstore bundle is not installed root-owned and read-only"
require_text "COPY --chown=0:0 --chmod=0444 runtime-release/trusted-root.json" "$control_builder" \
	"control image trusted root is not installed root-owned and read-only"
require_text "COPY --chown=0:0 --chmod=0444 toolchain-release/catalog.json" "$control_builder" \
	"control image standard-toolchain catalog is not installed root-owned and read-only"
require_text "COPY --chown=0:0 --chmod=0444 toolchain-release/catalog.sigstore.json" "$control_builder" \
	"control image standard-toolchain bundle is not installed root-owned and read-only"
require_text "COPY --chown=0:0 --chmod=0444 toolchain-release/trusted-root.json" "$control_builder" \
	"control image standard-toolchain trusted root is not installed root-owned and read-only"
require_text "COPY --chown=0:0 --chmod=0444 manager-release/catalog.json" "$control_builder" \
	"control image Manager catalog is not installed root-owned and read-only"
require_text "COPY --chown=0:0 --chmod=0444 manager-release/catalog.sigstore.json" "$control_builder" \
	"control image Manager bundle is not installed root-owned and read-only"
require_text "COPY --chown=0:0 --chmod=0444 manager-release/trusted-root.json" "$control_builder" \
	"control image Manager trusted root is not installed root-owned and read-only"

if ! rg -F "scripts/build-config-inspector.sh" "$workflow" >/dev/null; then
	printf 'release workflow does not refresh the config inspector before CLI builds\n' >&2
	exit 1
fi

if ! rg -F "git diff --exit-code -- internal/projectconfig/js" "$workflow" >/dev/null; then
	printf 'release workflow does not verify config inspector artifacts are current\n' >&2
	exit 1
fi

if ! rg -F 'git status --porcelain -- internal/projectconfig/js' "$workflow" >/dev/null; then
	printf 'release workflow does not reject untracked config inspector artifacts\n' >&2
	exit 1
fi

if rg -F 'tar -C "$out_dir" -czf "dist/helmr-${os}-${arch}.tar.gz" helmr adapter' "$workflow" >/dev/null; then
	printf 'release workflow still packages external adapter sidecar files\n' >&2
	exit 1
fi

if ! rg -F 'tar -C "$out_dir" -czf "dist/helmr-${os}-${arch}.tar.gz" helmr' "$workflow" >/dev/null; then
	printf 'release workflow does not package the single helmr binary archive\n' >&2
	exit 1
fi

if ! rg -F 'scripts/release-worker-ami-cleanup.sh "$WORKER_IMAGE_NAME_BASE" "$WORKER_AMI_REGIONS" "$RELEASE_WORKER_AMI_KEEP"' "$workflow" >/dev/null; then
	printf 'release workflow does not clean old public worker AMIs before creating a new release AMI\n' >&2
	exit 1
fi

printf 'ok - release workflow tests\n'
