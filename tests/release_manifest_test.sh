#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
script="${repo_root}/scripts/write-aws-release-manifest.sh"
tmp=$(mktemp -d)
trap 'rm -rf "${tmp}"' EXIT

controlplane_image="ghcr.io/helmrdotdev/helmr-controlplane@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
platform_release='{"archive":{"digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","mediaType":"application/vnd.helmr.platform-release.v0+tar","sizeBytes":4096},"formatVersion":0,"sourceCommit":"dddddddddddddddddddddddddddddddddddddddd","sourceRef":"refs/tags/v0.1.0"}'
component_digest="sha256:1111111111111111111111111111111111111111111111111111111111111111"
image_digest="sha256:2222222222222222222222222222222222222222222222222222222222222222"
prepare_root_digest="sha256:3333333333333333333333333333333333333333333333333333333333333333"
component_arn="arn:aws:imagebuilder:us-east-1:123456789012:component/helmr-worker-component-${component_digest#sha256:}/1.0.0/1"
recipe_arn="arn:aws:imagebuilder:us-east-1:123456789012:image-recipe/helmr-worker-recipe-${image_digest#sha256:}/1.0.0"
build_arn="arn:aws:imagebuilder:us-east-1:123456789012:image/helmr-worker-recipe-${image_digest#sha256:}/1.0.0/1"
worker_image="$(jq -cnS \
  --arg component_digest "${component_digest}" \
  --arg image_digest "${image_digest}" \
  --arg prepare_root_digest "${prepare_root_digest}" \
  --arg component_arn "${component_arn}" \
  --arg recipe_arn "${recipe_arn}" \
  --arg build_arn "${build_arn}" '
  {
    schema:"helmr.worker-image.v0",
    amis:{
      "ap-northeast-1":"ami-00000000000000001",
      "us-east-1":"ami-00000000000000002",
      "us-west-2":"ami-00000000000000003"
    },
    visibility:"public",
    imageBuildVersionARN:$build_arn,
    imageRecipeARN:$recipe_arn,
    componentDefinitionDigest:$component_digest,
    imageDefinitionDigest:$image_digest,
    prepareRootDigest:$prepare_root_digest,
    resolvedParentImageID:"ami-00000000000000004",
    hostArtifacts:{
      sourceCommit:"7777777777777777777777777777777777777777",
      bundleDigest:"sha256:8888888888888888888888888888888888888888888888888888888888888888",
      manifestDigest:"sha256:9999999999999999999999999999999999999999999999999999999999999999"
    },
    runtimeArtifacts:{
      sourceCommit:"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
      bundleDigest:"sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
      manifestDigest:"sha256:abababababababababababababababababababababababababababababababab"
    }
  }
')"

"${script}" "${controlplane_image}" "${worker_image}" "${platform_release}" "${tmp}/aws-artifacts.json"
jq -e \
  --arg image "${controlplane_image}" \
  --argjson worker_image "${worker_image}" \
  --argjson release "${platform_release}" '
  keys == ["controlplaneImage", "platformRelease", "schema", "workerImage"] and
  .schema == "helmr.aws-release.v0" and
  .controlplaneImage == $image and
  .workerImage == $worker_image and
  .platformRelease == $release
' "${tmp}/aws-artifacts.json" >/dev/null

if "${script}" "ghcr.io/helmrdotdev/helmr-controlplane:latest" "${worker_image}" "${platform_release}" "${tmp}/tagged-image.json" 2>/dev/null; then
  echo "tagged Control Plane image was accepted" >&2
  exit 1
fi
if REQUIRED_WORKER_AMI_REGIONS=us-east-1,eu-west-1 \
  "${script}" "${controlplane_image}" "${worker_image}" "${platform_release}" "${tmp}/missing-region.json" 2>/dev/null; then
  echo "incomplete Worker AMI set was accepted" >&2
  exit 1
fi

mkdir -p "${tmp}/bin"
cat >"${tmp}/bin/docker" <<'EOF'
#!/usr/bin/env bash
exit "${MOCK_CONTROLPLANE_IMAGE_FAIL:-0}"
EOF
cat >"${tmp}/bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
service=${1:-}
operation=${2:-}
shift 2
arg() {
  local wanted=$1
  shift
  while [ "$#" -gt 0 ]; do
    if [ "$1" = "${wanted}" ]; then printf '%s\n' "$2"; return; fi
    shift
  done
}
case "${service}:${operation}" in
  imagebuilder:get-image)
    jq -cn \
      --arg build "${MOCK_BUILD_ARN}" \
      --arg recipe "${MOCK_RECIPE_ARN}" \
      '{image:{arn:$build,state:{status:"AVAILABLE"},imageRecipe:{arn:$recipe},outputResources:{amis:[
        {region:"ap-northeast-1",image:"ami-00000000000000001"},
        {region:"us-east-1",image:"ami-00000000000000002"},
        {region:"us-west-2",image:"ami-00000000000000003"}
      ]}}}'
    ;;
  imagebuilder:get-image-recipe)
    jq -cn \
      --arg arn "${MOCK_RECIPE_ARN}" \
      --arg component "${MOCK_COMPONENT_ARN}" \
      --arg component_digest "${MOCK_COMPONENT_DIGEST}" \
      --arg image_digest "${MOCK_IMAGE_DIGEST}" '
      {imageRecipe:{arn:$arn,parentImage:"ami-00000000000000004",components:[{componentArn:$component}],tags:{
        HelmrComponentDefinitionDigest:$component_digest,
        HelmrImageDefinitionDigest:$image_digest,
        HelmrResolvedParentImageID:"ami-00000000000000004"
      }}}
    '
    ;;
  imagebuilder:get-component)
    jq -cn --arg arn "${MOCK_COMPONENT_ARN}" --arg digest "${MOCK_COMPONENT_DIGEST}" \
      '{component:{arn:$arn,tags:{HelmrComponentDefinitionDigest:$digest}}}'
    ;;
  ec2:describe-images)
    region="$(arg --region "$@")"
    ami="$(arg --image-ids "$@")"
    public=true
    [ "${MOCK_PRIVATE_REGION:-}" != "${region}" ] || public=false
    jq -cn \
      --arg ami "${ami}" \
      --arg component_digest "${MOCK_COMPONENT_DIGEST}" \
      --arg image_digest "${MOCK_IMAGE_DIGEST}" \
      --arg prepare_root_digest "${MOCK_PREPARE_ROOT_DIGEST}" \
      --arg host_bundle_digest sha256:8888888888888888888888888888888888888888888888888888888888888888 \
      --arg host_manifest_digest sha256:9999999999999999999999999999999999999999999999999999999999999999 \
      --arg runtime_bundle_digest sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff \
      --arg runtime_manifest_digest sha256:abababababababababababababababababababababababababababababababab \
      --argjson public "${public}" '
      {Images:[{ImageId:$ami,State:"available",ImageType:"machine",Public:$public,Tags:[
        {Key:"HelmrComponentDefinitionDigest",Value:$component_digest},
        {Key:"HelmrImageDefinitionDigest",Value:$image_digest},
        {Key:"HelmrResolvedParentImageID",Value:"ami-00000000000000004"},
        {Key:"HelmrPrepareRootDigest",Value:$prepare_root_digest},
        {Key:"HelmrHostBundleDigest",Value:$host_bundle_digest},
        {Key:"HelmrHostArtifactsDigest",Value:$host_manifest_digest},
        {Key:"HelmrRuntimeBundleDigest",Value:$runtime_bundle_digest},
        {Key:"HelmrRuntimeArtifactsDigest",Value:$runtime_manifest_digest}
      ]}]}
    '
    ;;
  *) exit 1 ;;
esac
EOF
chmod 0755 "${tmp}/bin/docker" "${tmp}/bin/aws"

verify_env=(
  MOCK_BUILD_ARN="${build_arn}"
  MOCK_RECIPE_ARN="${recipe_arn}"
  MOCK_COMPONENT_ARN="${component_arn}"
  MOCK_COMPONENT_DIGEST="${component_digest}"
  MOCK_IMAGE_DIGEST="${image_digest}"
  MOCK_PREPARE_ROOT_DIGEST="${prepare_root_digest}"
)
env "${verify_env[@]}" VERIFY_RELEASE_ARTIFACTS=1 PATH="${tmp}/bin:${PATH}" \
  "${script}" "${controlplane_image}" "${worker_image}" "${platform_release}" "${tmp}/verified.json"
if env "${verify_env[@]}" VERIFY_RELEASE_ARTIFACTS=1 MOCK_PRIVATE_REGION=us-west-2 PATH="${tmp}/bin:${PATH}" \
  "${script}" "${controlplane_image}" "${worker_image}" "${platform_release}" "${tmp}/bad-visibility.json" 2>/dev/null; then
  echo "wrong live Worker AMI visibility was accepted" >&2
  exit 1
fi

printf 'ok - AWS release manifest tests\n'
