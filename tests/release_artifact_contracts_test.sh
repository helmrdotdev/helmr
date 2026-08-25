#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../scripts/release-artifact-contracts.sh
source "${repo_root}/scripts/release-artifact-contracts.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

assert_contract() {
  local validator=$1 fixture=$2 invalid=$3
  "${validator}" "${fixture}" || {
    printf 'valid fixture failed %s\n' "${validator}" >&2
    exit 1
  }
  jq '.extra = true' "${fixture}" >"${invalid}"
  if "${validator}" "${invalid}"; then
    printf 'extra contract field passed %s\n' "${validator}" >&2
    exit 1
  fi
  jq '.schema = "wrong"' "${fixture}" >"${invalid}"
  if "${validator}" "${invalid}"; then
    printf 'wrong schema passed %s\n' "${validator}" >&2
    exit 1
  fi
}

digest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
commit="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
component_arn="arn:aws:imagebuilder:us-east-1:123456789012:component/example/1.0.0/1"
distribution_arn="arn:aws:imagebuilder:us-east-1:123456789012:distribution-configuration/example"
pipeline_arn="arn:aws:imagebuilder:us-east-1:123456789012:image-pipeline/example"
recipe_arn="arn:aws:imagebuilder:us-east-1:123456789012:image-recipe/example/1.0.0"
image_arn="arn:aws:imagebuilder:us-east-1:123456789012:image/example/1.0.0/1"

jq -cn \
  --arg commit "${commit}" \
  --arg digest "${digest}" '
  {
    schema:"helmr.worker-host-bundle.v0",
    sourceCommit:$commit,
    bundle:{path:"worker-host-artifacts.tar",digest:$digest},
    manifest:{path:"worker-host-artifacts.json",digest:$digest}
  }
' >"${tmp}/host.json"

jq -cn \
  --arg commit "${commit}" \
  --arg digest "${digest}" '
  {
    schema:"helmr.worker-runtime-bundle.v0",
    sourceCommit:$commit,
    bundle:{path:"runtime-artifacts.tar",digest:$digest},
    runtimeArtifactsManifest:{path:"runtime-artifacts.json",digest:$digest}
  }
' >"${tmp}/runtime.json"

jq -cn \
  --arg commit "${commit}" \
  --arg component_arn "${component_arn}" \
  --arg digest "${digest}" \
  --arg distribution_arn "${distribution_arn}" \
  --arg pipeline_arn "${pipeline_arn}" \
  --arg recipe_arn "${recipe_arn}" '
  {
    schema:"helmr.worker-image-definition-state.v0",
    componentARN:$component_arn,
    componentDefinitionDigest:$digest,
    distributionConfigurationARN:$distribution_arn,
    distributionRegions:["us-east-1"],
    hostArtifacts:{bundleDigest:$digest,manifestDigest:$digest,sourceCommit:$commit},
    imageDefinitionDigest:$digest,
    imagePipelineARN:$pipeline_arn,
    imageRecipeARN:$recipe_arn,
    prepareRootDigest:$digest,
    resolvedParentImageID:"ami-00000000000000001",
    rootBlockDeviceMapping:{
      deviceName:"/dev/sda1",
      ebs:{deleteOnTermination:true,encrypted:true,volumeSize:24,volumeType:"gp3"}
    },
    runtimeArtifacts:{bundleDigest:$digest,manifestDigest:$digest,sourceCommit:$commit},
    visibility:"private"
  }
' >"${tmp}/definition.json"

jq -cn \
  --arg commit "${commit}" \
  --arg digest "${digest}" \
  --arg image_arn "${image_arn}" \
  --arg recipe_arn "${recipe_arn}" '
  {
    schema:"helmr.worker-image.v0",
    amis:{"us-east-1":"ami-00000000000000002"},
    componentDefinitionDigest:$digest,
    hostArtifacts:{bundleDigest:$digest,manifestDigest:$digest,sourceCommit:$commit},
    imageBuildVersionARN:$image_arn,
    imageDefinitionDigest:$digest,
    imageRecipeARN:$recipe_arn,
    prepareRootDigest:$digest,
    resolvedParentImageID:"ami-00000000000000001",
    runtimeArtifacts:{bundleDigest:$digest,manifestDigest:$digest,sourceCommit:$commit},
    visibility:"private"
  }
' >"${tmp}/receipt.json"

assert_contract validate_worker_host_bundle_receipt "${tmp}/host.json" "${tmp}/invalid.json"
assert_contract validate_worker_runtime_bundle_receipt "${tmp}/runtime.json" "${tmp}/invalid.json"
assert_contract validate_worker_image_definition "${tmp}/definition.json" "${tmp}/invalid.json"
assert_contract validate_worker_image_receipt "${tmp}/receipt.json" "${tmp}/invalid.json"

printf 'ok - release artifact contract tests\n'
