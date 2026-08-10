#!/usr/bin/env bash
set -euo pipefail

controlplane_image="${1:-}"
worker_amis_json="${2:-}"
worker_image_provenance_json="${3:-}"
platform_release_json="${4:-}"
output="${5:-aws-artifacts.json}"
required_worker_ami_regions="${REQUIRED_WORKER_AMI_REGIONS:-us-east-1,us-west-2,ap-northeast-1}"
verify_release_artifacts="${VERIFY_RELEASE_ARTIFACTS:-0}"

if [ -z "$controlplane_image" ] || [ -z "$worker_amis_json" ] || [ -z "$worker_image_provenance_json" ] || [ -z "$platform_release_json" ]; then
  echo "usage: scripts/write-aws-release-manifest.sh <controlplane-image> <worker-amis-json> <worker-image-provenance-json> <platform-release-json> [output]" >&2
  echo "set VERIFY_RELEASE_ARTIFACTS=1 to verify image and AMI visibility before writing" >&2
  exit 1
fi

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || die "missing command: $1"
}

is_truthy() {
  case "$1" in
    1 | true | TRUE | yes | YES) return 0 ;;
    *) return 1 ;;
  esac
}

if [[ ! "$controlplane_image" =~ @sha256:[0-9a-f]{64}$ ]]; then
  echo "controlplane image must be pinned by digest as @sha256:<64 lowercase hex characters>" >&2
  exit 1
fi

jq -e --arg required_worker_ami_regions "$required_worker_ami_regions" '
  . as $worker_amis
  |
  ($required_worker_ami_regions | split(",") | map(gsub("^\\s+|\\s+$"; "")) | map(select(length > 0))) as $required_regions
  | type == "object"
  and all($required_regions[]; . as $region | ($worker_amis | has($region)) and ($worker_amis[$region] | type == "string" and test("^ami-[0-9a-f]{8,}$")))
  and all(keys[]; test("^[a-z]{2}-[a-z-]+-[0-9]+$"))
  and all(.[]; type == "string" and test("^ami-[0-9a-f]{8,}$"))
' >/dev/null <<<"$worker_amis_json"

jq -e '
  keys == ["archive", "buildPolicyDigest", "formatVersion", "sourceCommit", "sourceRef"]
  and .formatVersion == 0
  and (.archive | keys == ["digest", "mediaType", "sizeBytes"])
  and (.archive.digest | test("^sha256:[0-9a-f]{64}$"))
  and .archive.mediaType == "application/vnd.helmr.platform-release.v0+tar"
  and (.archive.sizeBytes | type == "number" and . > 0 and floor == .)
  and (.buildPolicyDigest | test("^sha256:[0-9a-f]{64}$"))
  and (.sourceCommit | test("^[0-9a-f]{40}$"))
  and (.sourceRef | test("^refs/(tags|heads)/[^[:space:]]+$"))
' >/dev/null <<<"$platform_release_json"

jq -e --argjson worker_amis "$worker_amis_json" --argjson platform_release "$platform_release_json" '
  keys == ["ami", "formatVersion", "hostArtifactsBundleDigest", "hostArtifactsManifestDigest", "imageBuildVersionARN", "imageRecipeARN", "runtimeArtifactsBundleDigest", "runtimeArtifactsManifestDigest", "runtimeProfile", "sourceCommit", "workerVersion"]
  and .formatVersion == 1
  and (.ami | keys == ["id", "region"])
  and (.ami.id | test("^ami-[0-9a-f]{8,}$"))
  and (.ami.region | test("^[a-z]{2}-[a-z-]+-[0-9]+$"))
  and ($worker_amis[.ami.region] == .ami.id)
  and (.imageBuildVersionARN | type == "string" and length > 0)
  and (.imageRecipeARN | type == "string" and length > 0)
  and (.hostArtifactsBundleDigest | test("^sha256:[0-9a-f]{64}$"))
  and (.hostArtifactsManifestDigest | test("^sha256:[0-9a-f]{64}$"))
  and (.runtimeArtifactsBundleDigest | test("^sha256:[0-9a-f]{64}$"))
  and (.runtimeArtifactsManifestDigest | test("^sha256:[0-9a-f]{64}$"))
  and (.runtimeProfile | keys == ["arch", "contract", "id", "initramfs_digest", "kernel_digest", "rootfs_digest"])
  and .runtimeProfile.arch == "x86_64"
  and .runtimeProfile.contract == "helmr.vm-runtime.v0"
  and (.runtimeProfile.id | test("^sha256:[0-9a-f]{64}$"))
  and (.runtimeProfile.kernel_digest | test("^sha256:[0-9a-f]{64}$"))
  and (.runtimeProfile.initramfs_digest | test("^sha256:[0-9a-f]{64}$"))
  and (.runtimeProfile.rootfs_digest | test("^sha256:[0-9a-f]{64}$"))
  and .sourceCommit == $platform_release.sourceCommit
  and .workerVersion == .sourceCommit
' >/dev/null <<<"$worker_image_provenance_json"

verify_controlplane_image() {
  if command -v docker >/dev/null 2>&1; then
    if docker buildx imagetools inspect "$controlplane_image" >/dev/null 2>&1; then
      return 0
    fi
    if docker manifest inspect "$controlplane_image" >/dev/null 2>&1; then
      return 0
    fi
  fi

  if command -v skopeo >/dev/null 2>&1 && skopeo inspect "docker://${controlplane_image}" >/dev/null 2>&1; then
    return 0
  fi

  die "controlplane image is not inspectable: ${controlplane_image}"
}

verify_worker_amis() {
  need_command aws

  while IFS=$'\t' read -r region ami_id; do
    described_ami_id="$(
      aws ec2 describe-images \
        --region "$region" \
        --image-ids "$ami_id" \
        --query 'Images[0].ImageId' \
        --output text
    )" || die "worker AMI is not visible in ${region}: ${ami_id}"

    [ "$described_ami_id" = "$ami_id" ] || die "worker AMI lookup returned ${described_ami_id} in ${region}, expected ${ami_id}"
  done < <(jq -r 'to_entries[] | [.key, .value] | @tsv' <<<"$worker_amis_json")
}

if is_truthy "$verify_release_artifacts"; then
  verify_controlplane_image
  verify_worker_amis
fi

jq -n \
  --arg controlplane_image "$controlplane_image" \
  --argjson worker_amis "$worker_amis_json" \
  --argjson worker_image_provenance "$worker_image_provenance_json" \
  --argjson platform_release "$platform_release_json" \
  '{
    controlplane_image: $controlplane_image,
    format_version: 1,
    platform_release: $platform_release,
    worker_amis: $worker_amis,
    worker_image_provenance: $worker_image_provenance
  }' >"$output"
