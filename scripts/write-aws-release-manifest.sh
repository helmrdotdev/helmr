#!/usr/bin/env bash
set -euo pipefail

controlplane_image="${1:-}"
worker_image_json="${2:-}"
platform_release_json="${3:-}"
output="${4:-aws-artifacts.json}"
required_worker_ami_regions="${REQUIRED_WORKER_AMI_REGIONS:-us-east-1,us-west-2,ap-northeast-1}"
verify_release_artifacts="${VERIFY_RELEASE_ARTIFACTS:-0}"

if [ -z "${controlplane_image}" ] || [ -z "${worker_image_json}" ] || [ -z "${platform_release_json}" ]; then
  echo "usage: scripts/write-aws-release-manifest.sh <controlplane-image> <worker-image-json> <platform-release-json> [output]" >&2
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

[[ "${controlplane_image}" =~ @sha256:[0-9a-f]{64}$ ]] ||
  die "controlplane image must be pinned by digest as @sha256:<64 lowercase hex characters>"

jq -e --arg required_worker_ami_regions "${required_worker_ami_regions}" '
  . as $worker_image |
  ($required_worker_ami_regions | split(",") | map(gsub("^\\s+|\\s+$"; "")) | map(select(length > 0)) | sort | unique) as $required_regions |
  (keys | sort) == [
    "amis",
    "componentDefinitionDigest",
    "hostArtifacts",
    "imageBuildVersionARN",
    "imageDefinitionDigest",
    "imageRecipeARN",
    "prepareRootDigest",
    "resolvedParentImageID",
    "runtimeArtifacts",
    "schema",
    "visibility"
  ] and
  .schema == "helmr.worker-image.v0" and
  (.amis | type == "object" and length > 0) and
  all($required_regions[]; . as $region | ($worker_image.amis | has($region))) and
  all(.amis | keys[]; test("^[a-z]{2}-[a-z-]+-[0-9]+$")) and
  all(.amis[]; test("^ami-([0-9a-f]{8}|[0-9a-f]{17})$")) and
  (.visibility == "public" or .visibility == "private") and
  (.imageBuildVersionARN | test("^arn:[^:]+:imagebuilder:[^:]+:[0-9]{12}:image/.+/1[.]0[.]0/[0-9]+$")) and
  (.imageRecipeARN | test("^arn:[^:]+:imagebuilder:[^:]+:[0-9]{12}:image-recipe/.+/1[.]0[.]0$")) and
  (.componentDefinitionDigest | test("^sha256:[0-9a-f]{64}$")) and
  (.imageDefinitionDigest | test("^sha256:[0-9a-f]{64}$")) and
  (.prepareRootDigest | test("^sha256:[0-9a-f]{64}$")) and
  (.resolvedParentImageID | test("^ami-([0-9a-f]{8}|[0-9a-f]{17})$")) and
  (.hostArtifacts | keys == ["bundleDigest", "manifestDigest", "sourceCommit"]) and
  (.hostArtifacts.sourceCommit | test("^[0-9a-f]{40}$")) and
  all(.hostArtifacts.bundleDigest, .hostArtifacts.manifestDigest; test("^sha256:[0-9a-f]{64}$")) and
  (.runtimeArtifacts | keys == ["bundleDigest", "manifestDigest", "sourceCommit"]) and
  (.runtimeArtifacts.sourceCommit | test("^[0-9a-f]{40}$")) and
  all(.runtimeArtifacts.bundleDigest, .runtimeArtifacts.manifestDigest; test("^sha256:[0-9a-f]{64}$"))
' >/dev/null <<<"${worker_image_json}" || die "Worker image receipt is invalid"

jq -e '
  keys == ["archive", "buildPolicyDigest", "formatVersion", "sourceCommit", "sourceRef"] and
  .formatVersion == 0 and
  (.archive | keys == ["digest", "mediaType", "sizeBytes"]) and
  (.archive.digest | test("^sha256:[0-9a-f]{64}$")) and
  .archive.mediaType == "application/vnd.helmr.platform-release.v0+tar" and
  (.archive.sizeBytes | type == "number" and . > 0 and floor == .) and
  (.buildPolicyDigest | test("^sha256:[0-9a-f]{64}$")) and
  (.sourceCommit | test("^[0-9a-f]{40}$")) and
  (.sourceRef | test("^refs/(tags|heads)/[^[:space:]]+$"))
' >/dev/null <<<"${platform_release_json}" || die "Platform release receipt is invalid"

verify_controlplane_image() {
  if command -v docker >/dev/null 2>&1; then
    if docker buildx imagetools inspect "${controlplane_image}" >/dev/null 2>&1; then
      return 0
    fi
    if docker manifest inspect "${controlplane_image}" >/dev/null 2>&1; then
      return 0
    fi
  fi
  if command -v skopeo >/dev/null 2>&1 && skopeo inspect "docker://${controlplane_image}" >/dev/null 2>&1; then
    return 0
  fi
  die "controlplane image is not inspectable: ${controlplane_image}"
}

verify_worker_image() {
  local build_arn build_region build_json recipe_arn recipe_json component_arn component_json
  local parent_image visibility expected_public region ami_id ami_json
  need_command aws
  build_arn="$(jq -r '.imageBuildVersionARN' <<<"${worker_image_json}")"
  build_region="$(printf '%s\n' "${build_arn}" | cut -d: -f4)"
  recipe_arn="$(jq -r '.imageRecipeARN' <<<"${worker_image_json}")"
  parent_image="$(jq -r '.resolvedParentImageID' <<<"${worker_image_json}")"
  visibility="$(jq -r '.visibility' <<<"${worker_image_json}")"
  case "${visibility}" in
    public) expected_public=true ;;
    private) expected_public=false ;;
    *) die "invalid Worker image visibility" ;;
  esac

  build_json="$(aws imagebuilder get-image --region "${build_region}" --image-build-version-arn "${build_arn}" --output json)" ||
    die "Worker image build is not visible: ${build_arn}"
  jq -e \
    --arg build_arn "${build_arn}" \
    --arg recipe_arn "${recipe_arn}" \
    --argjson amis "$(jq -c '.amis' <<<"${worker_image_json}")" '
      ([.image.outputResources.amis[]? | select(.region != null and .image != null) | {key: .region, value: .image}]) as $outputs |
      .image.arn == $build_arn and
      .image.state.status == "AVAILABLE" and
      .image.imageRecipe.arn == $recipe_arn and
      ($outputs | length) == ($amis | length) and
      ($outputs | from_entries) == $amis
    ' >/dev/null <<<"${build_json}" || die "Worker image build does not match its receipt"

  recipe_json="$(aws imagebuilder get-image-recipe --region "${build_region}" --image-recipe-arn "${recipe_arn}" --output json)" ||
    die "Worker image recipe is not visible: ${recipe_arn}"
  component_arn="$(jq -er '.imageRecipe.components | select(length == 1) | .[0].componentArn' <<<"${recipe_json}")" ||
    die "Worker image recipe must contain one component"
  jq -e \
    --arg component_digest "$(jq -r '.componentDefinitionDigest' <<<"${worker_image_json}")" \
    --arg image_digest "$(jq -r '.imageDefinitionDigest' <<<"${worker_image_json}")" \
    --arg parent_image "${parent_image}" \
    --arg recipe_arn "${recipe_arn}" '
      .imageRecipe.arn == $recipe_arn and
      .imageRecipe.parentImage == $parent_image and
      .imageRecipe.tags.HelmrComponentDefinitionDigest == $component_digest and
      .imageRecipe.tags.HelmrImageDefinitionDigest == $image_digest and
      .imageRecipe.tags.HelmrResolvedParentImageID == $parent_image and
      (.imageRecipe.arn | endswith(("-" + ($image_digest | sub("^sha256:"; ""))) + "/1.0.0"))
    ' >/dev/null <<<"${recipe_json}" || die "Worker image recipe does not match its receipt"

  component_json="$(aws imagebuilder get-component --region "${build_region}" --component-build-version-arn "${component_arn}" --output json)" ||
    die "Worker image component is not visible: ${component_arn}"
  jq -e \
    --arg component_arn "${component_arn}" \
    --arg component_digest "$(jq -r '.componentDefinitionDigest' <<<"${worker_image_json}")" '
      .component.arn == $component_arn and
      .component.tags.HelmrComponentDefinitionDigest == $component_digest and
      (.component.arn | contains("-" + ($component_digest | sub("^sha256:"; "")) + "/1.0.0/"))
    ' >/dev/null <<<"${component_json}" || die "Worker image component does not match its receipt"

  while IFS=$'\t' read -r region ami_id; do
    ami_json="$(aws ec2 describe-images --region "${region}" --owners self --image-ids "${ami_id}" --output json)" ||
      die "Worker AMI is not visible in ${region}: ${ami_id}"
    jq -e \
      --arg ami_id "${ami_id}" \
      --arg component_digest "$(jq -r '.componentDefinitionDigest' <<<"${worker_image_json}")" \
      --arg host_bundle_digest "$(jq -r '.hostArtifacts.bundleDigest' <<<"${worker_image_json}")" \
      --arg host_manifest_digest "$(jq -r '.hostArtifacts.manifestDigest' <<<"${worker_image_json}")" \
      --arg image_digest "$(jq -r '.imageDefinitionDigest' <<<"${worker_image_json}")" \
      --arg parent_image "${parent_image}" \
      --arg prepare_root_digest "$(jq -r '.prepareRootDigest' <<<"${worker_image_json}")" \
      --arg runtime_bundle_digest "$(jq -r '.runtimeArtifacts.bundleDigest' <<<"${worker_image_json}")" \
      --arg runtime_manifest_digest "$(jq -r '.runtimeArtifacts.manifestDigest' <<<"${worker_image_json}")" \
      --argjson expected_public "${expected_public}" '
        (.Images // []) as $images |
        (($images[0].Tags // []) | map({key: .Key, value: .Value}) | from_entries) as $tags |
        ($images | length) == 1 and
        $images[0].ImageId == $ami_id and
        $images[0].State == "available" and
        $images[0].ImageType == "machine" and
        $images[0].Public == $expected_public and
        $tags.HelmrComponentDefinitionDigest == $component_digest and
        $tags.HelmrImageDefinitionDigest == $image_digest and
        $tags.HelmrResolvedParentImageID == $parent_image and
        $tags.HelmrPrepareRootDigest == $prepare_root_digest and
        $tags.HelmrHostBundleDigest == $host_bundle_digest and
        $tags.HelmrHostArtifactsDigest == $host_manifest_digest and
        $tags.HelmrRuntimeBundleDigest == $runtime_bundle_digest and
        $tags.HelmrRuntimeArtifactsDigest == $runtime_manifest_digest
      ' >/dev/null <<<"${ami_json}" || die "Worker AMI does not match its receipt in ${region}: ${ami_id}"
  done < <(jq -r '.amis | to_entries[] | [.key, .value] | @tsv' <<<"${worker_image_json}")
}

if is_truthy "${verify_release_artifacts}"; then
  verify_controlplane_image
  verify_worker_image
fi

jq -nS \
  --arg controlplane_image "${controlplane_image}" \
  --argjson worker_image "${worker_image_json}" \
  --argjson platform_release "${platform_release_json}" '
  {
    schema: "helmr.aws-release.v0",
    controlplaneImage: $controlplane_image,
    platformRelease: $platform_release,
    workerImage: $worker_image
  }' >"${output}"
