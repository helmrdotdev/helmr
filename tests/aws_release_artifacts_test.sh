#!/usr/bin/env bash
# shellcheck disable=SC2016,SC2030,SC2031,SC2329
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
script="${repo_root}/scripts/aws-release-artifacts.sh"
controlplane_build_script="${repo_root}/scripts/build-controlplane-image.sh"
controlplane_build_contract="${repo_root}/images/controlplane-image-build.json"

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

assert_contains() {
  local file=$1 needle=$2 label=$3
  grep -Fq -- "${needle}" "${file}" || fail "${label}: expected '${needle}' in ${file}"
}

assert_equal() {
  local expected=$1 actual=$2 label=$3
  [ "${actual}" = "${expected}" ] || fail "${label}: expected '${expected}', got '${actual}'"
}

if grep -Fq '#smoke-linux' "${script}"; then
  fail "Worker release materialization must not require the Linux-only smoke shell"
fi
if grep -Fq 'go -C "${ROOT}" run ./cmd/helmr-controlplane release publish' "${script}"; then
  fail "Platform release publication must not keep a host publisher fallback"
fi
assert_contains "${script}" 'HelmrRuntimeBundleDigest' \
  "Worker AMI runtime bundle provenance"
assert_contains "${script}" 'HelmrRuntimeArtifactsDigest' \
  "Worker AMI runtime artifact provenance"
assert_contains "${script}" 'HelmrHostBundleDigest' \
  "Worker AMI host bundle provenance"
assert_contains "${script}" 'HelmrHostArtifactsDigest' \
  "Worker AMI host artifact provenance"
assert_contains "${controlplane_build_script}" 'git -C "$repo_root" archive --format=tar HEAD | tar -xf - -C "$source_dir"' \
  "Control Plane image source snapshot"
assert_contains "${controlplane_build_script}" 'path:/work#packages.x86_64-linux.runtimeRelease' \
  "Control Plane image Runtime build uses a path input"
assert_contains "${controlplane_build_script}" 'path:/work#packages.x86_64-linux.timezoneData' \
  "Control Plane image timezone build uses a path input"
assert_contains "${script}" 'Worker does not report the release cohort identity' \
  "Worker release reported identity"
if grep -Fq 'source=${repo_root},target=/work' "${controlplane_build_script}"; then
  fail "Control Plane image must not mount Product Git metadata into the Linux builder"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
stdout="${tmp}/stdout"
stderr="${tmp}/stderr"

if (
  set -- help
  export STATE_DIR="${tmp}/worker-image-missing-release-tag"
  # shellcheck source=/dev/null
  source "${script}" >/dev/null
  require_clean_product_checkout() { fail "missing release tag must fail before checkout or AWS work"; }
  worker_image_apply
) >"${stdout}" 2>"${stderr}"; then
  fail "Worker image apply must require a release tag"
fi
assert_contains "${stderr}" "RELEASE_TAG is required to publish Worker artifacts" \
  "Worker image release tag guard"

apply_args_file="${tmp}/worker-image-apply.args"
(
  set -- help
  export RELEASE_TAG=v0.0.0-test STATE_DIR="${tmp}/worker-image-apply"
  # shellcheck source=/dev/null
  source "${script}" >/dev/null
  require_clean_product_checkout() { :; }
  nix() { :; }
  prepare_worker_host_bundle() {
    jq -cn '{
      bundleDigest:"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
      kmsKeyARN:"",
      manifestDigest:"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      objectARN:"arn:aws:s3:::test-artifacts/helmr/worker-host-bundles/bundle.tar",
      s3URI:"s3://test-artifacts/helmr/worker-host-bundles/bundle.tar"
    }'
  }
  prepare_worker_runtime_bundle() {
    jq -cn '{
      bundleDigest:"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      kmsKeyARN:"",
      manifestDigest:"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      objectARN:"arn:aws:s3:::test-artifacts/helmr/worker-runtime-bundles/bundle.tar",
      s3URI:"s3://test-artifacts/helmr/worker-runtime-bundles/bundle.tar"
    }'
  }
  current_worker_image_definition() { printf '{}\n'; }
  validate_worker_image_definition() { :; }
  tf_apply() { printf '%s\n' "$@" >"${apply_args_file}"; }
  worker_image_apply
)
assert_contains "${apply_args_file}" '-var=host_artifacts_bundle_s3_uri=s3://test-artifacts/helmr/worker-host-bundles/bundle.tar' \
  "host bundle URI"
assert_contains "${apply_args_file}" '-var=host_artifacts_bundle_object_arn=arn:aws:s3:::test-artifacts/helmr/worker-host-bundles/bundle.tar' \
  "host bundle object"
assert_contains "${apply_args_file}" '-var=runtime_artifacts_bundle_s3_uri=s3://test-artifacts/helmr/worker-runtime-bundles/bundle.tar' \
  "runtime bundle URI"
assert_contains "${apply_args_file}" '-var=runtime_artifacts_bundle_object_arn=arn:aws:s3:::test-artifacts/helmr/worker-runtime-bundles/bundle.tar' \
  "runtime bundle object"

bundle_state="${tmp}/runtime-bundle-state"
bundle_stdout="${tmp}/runtime-bundle-stdout"
bundle_stderr="${tmp}/runtime-bundle-stderr"
mkdir -p "${bundle_state}"
(
  set -- help
  export WORKER_IMAGE_ARTIFACT_BUCKET=test-artifacts
  # shellcheck disable=SC2031
  export STATE_DIR="${bundle_state}"
  # shellcheck source=/dev/null
  source "${script}" >/dev/null
  nix() {
    case "${4:-}" in
      make)
        printf 'runtime build progress\n'
        ;;
      */materialize-worker-runtime-bundle.sh)
        local bundle_dir=$5 bundle_digest manifest_digest source_commit
        mkdir -p "${bundle_dir}"
        printf 'runtime bundle\n' >"${bundle_dir}/runtime-artifacts.tar"
        printf '{"schema":"helmr.runtime-artifacts.v0"}\n' >"${bundle_dir}/runtime-artifacts.json"
        bundle_digest="sha256:$(sha256_file "${bundle_dir}/runtime-artifacts.tar")"
        manifest_digest="sha256:$(sha256_file "${bundle_dir}/runtime-artifacts.json")"
        source_commit="$(git -C "${repo_root}" rev-parse HEAD)"
        jq -cn \
          --arg bundle_digest "${bundle_digest}" \
          --arg manifest_digest "${manifest_digest}" \
          --arg source_commit "${source_commit}" '
          {
            schema: "helmr.worker-runtime-bundle.v0",
            sourceCommit: $source_commit,
            bundle: {path: "runtime-artifacts.tar", digest: $bundle_digest},
            runtimeArtifactsManifest: {path: "runtime-artifacts.json", digest: $manifest_digest}
          }
        ' >"${bundle_dir}/worker-runtime-bundle.json"
        ;;
      *)
        return 1
        ;;
    esac
  }
  aws() {
    case "${1:-}:${2:-}" in
      s3api:head-object)
        printf 'An error occurred (404) when calling the HeadObject operation: Not Found\n' >&2
        return 254
        ;;
      s3:cp)
        printf 'runtime upload progress\n'
        ;;
      s3api:get-bucket-encryption)
        printf 'arn:aws:kms:us-east-1:123456789012:key/test\n'
        ;;
      *)
        return 1
        ;;
    esac
  }
  prepare_worker_runtime_bundle
) >"${bundle_stdout}" 2>"${bundle_stderr}"
runtime_bundle="$(cat "${bundle_stdout}")"
jq -e -s '
  length == 1 and
  (.[0] | keys | sort) == ["bundleDigest", "kmsKeyARN", "manifestDigest", "objectARN", "s3URI"] and
  (.[0].bundleDigest | test("^sha256:[0-9a-f]{64}$")) and
  (.[0].manifestDigest | test("^sha256:[0-9a-f]{64}$")) and
  .[0].kmsKeyARN == "arn:aws:kms:us-east-1:123456789012:key/test" and
  .[0].objectARN == ("arn:aws:s3:::test-artifacts/helmr/worker-runtime-bundles/" + (.[0].bundleDigest | sub("^sha256:"; "")) + ".tar") and
  .[0].s3URI == ("s3://test-artifacts/helmr/worker-runtime-bundles/" + (.[0].bundleDigest | sub("^sha256:"; "")) + ".tar")
' <<<"${runtime_bundle}" >/dev/null || fail "Worker runtime bundle must return one exact JSON object"
assert_contains "${bundle_stderr}" "runtime build progress" "Worker runtime build progress stream"
assert_contains "${bundle_stderr}" "runtime upload progress" "Worker runtime upload progress stream"

platform_release="${tmp}/platform-release"
platform_bin="${tmp}/platform-bin"
mkdir -p "${platform_release}/objects/sha256" "${platform_bin}"
printf '{"formatVersion":0,"runtime":{"architecture":"x86_64","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","formatVersion":0,"mediaType":"application/vnd.helmr.runtime.v0+squashfs","runtimeContract":"helmr.runtime.v0","sizeBytes":6}}' >"${platform_release}/platform-release.json"
printf 'object' >"${platform_release}/objects/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

cat >"${platform_bin}/git" <<'EOF'
#!/usr/bin/env bash
case " $* " in
  *' archive '*) tar -cf - -T /dev/null ;;
  *) exit 0 ;;
esac
EOF
cat >"${platform_bin}/tofu" <<'EOF'
#!/usr/bin/env bash
case "${*: -1}" in
  platform_publisher_role_arn) printf 'arn:aws:iam::123456789012:role/platform-publisher\n' ;;
  platform_store_uri) printf 's3://platform-store/objects\n' ;;
  *) exit 1 ;;
esac
EOF
cat >"${platform_bin}/aws" <<'EOF'
#!/usr/bin/env bash
jq -cn '{Credentials:{AccessKeyId:"test",SecretAccessKey:"test",SessionToken:"test"}}'
EOF
cat >"${platform_bin}/nix" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  build)
    printf '%s\n' "${MOCK_PLATFORM_RELEASE}"
    ;;
  *) exit 1 ;;
esac
EOF
cat >"${platform_bin}/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "${AWS_REGION:-}" = us-east-1 ]
[ "${AWS_DEFAULT_REGION:-}" = us-east-1 ]
[ -z "${AWS_PROFILE+x}" ]
[ "${AWS_ACCESS_KEY_ID:-}" = test ]
[ "${AWS_SECRET_ACCESS_KEY:-}" = test ]
[ "${AWS_SESSION_TOKEN:-}" = test ]
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--mount" ] && [[ "${2:-}" = *,target=/input,readonly ]]; then
    input=${2#*source=}
    input=${input%%,*}
    break
  fi
  shift
done
[ -n "${input:-}" ]
object="$(find "${input}/objects/sha256" -maxdepth 1 -type f -print -quit)"
if stat -f '%Lp' "${object}" >/dev/null 2>&1; then
  mode="$(stat -f '%Lp' "${object}")"
else
  mode="$(stat -c '%a' "${object}")"
fi
[ "${mode}" = 400 ]
printf '%s\n' "${input}" >"${MOCK_PLATFORM_RELEASE_INPUT_MARKER}"
exit 42
EOF
chmod 0755 "${platform_bin}"/*

for default_region in unset us-west-2; do
  platform_state="${tmp}/platform-state-${default_region}"
  platform_input_marker="${tmp}/platform-input-${default_region}"
  mkdir -p "${platform_state}"
  if [ "${default_region}" = unset ]; then
    region_env=(-u AWS_REGION -u AWS_DEFAULT_REGION)
  else
    region_env=(-u AWS_REGION AWS_DEFAULT_REGION="${default_region}")
  fi
  if env "${region_env[@]}" \
    STATE_DIR="${platform_state}" TF_BIN="${platform_bin}/tofu" \
    MOCK_PLATFORM_RELEASE="${platform_release}" \
    MOCK_PLATFORM_RELEASE_INPUT_MARKER="${platform_input_marker}" \
    PATH="${platform_bin}:${PATH}" \
    "${script}" platform-release-publish >"${stdout}" 2>"${stderr}"; then
    fail "platform-release-publish should surface publisher failure"
  fi
  [ -s "${platform_input_marker}" ] || fail "publisher must receive the sealed release tree"
  [ ! -e "$(cat "${platform_input_marker}")" ] || fail "failed publisher must remove its sealed release tree"
done

controlplane_bin="${tmp}/controlplane-bin"
controlplane_context="${tmp}/controlplane-context"
controlplane_runtime_release="${tmp}/controlplane-runtime-release"
controlplane_timezone_data="${tmp}/controlplane-timezone-data"
mkdir -p "${controlplane_bin}"
mkdir -p "${controlplane_runtime_release}"
mkdir -p "${controlplane_timezone_data}/zoneinfo"
printf '{"formatVersion":0}\n' >"${controlplane_runtime_release}/runtime.descriptor.json"
printf 'UTC\n' >"${controlplane_timezone_data}/tzdb_names.txt"
printf 'mock timezone rule\n' >"${controlplane_timezone_data}/zoneinfo/UTC"
for command in bun make go; do
  printf '#!/usr/bin/env bash\nexit 0\n' >"${controlplane_bin}/${command}"
done
cat >"${controlplane_bin}/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  build) ;;
  image)
    [ "${2:-}" = inspect ]
    printf '%s\n' "${MOCK_LOCAL_IMAGE_ID:-sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}"
    ;;
  run)
    printf '%s\n' "$*" | grep -F 'target=/work,readonly' >/dev/null
    stage="$(mktemp -d)"
    cp "${MOCK_RUNTIME_DESCRIPTOR_PATH:?}" "$stage/runtime.descriptor.json"
    cp -a "${MOCK_TIMEZONE_DATA_PATH:?}/zoneinfo" "$stage/zoneinfo"
    cp "${MOCK_TIMEZONE_DATA_PATH:?}/tzdb_names.txt" "$stage/tzdb_names.txt"
    tar -C "$stage" -cf - runtime.descriptor.json zoneinfo tzdb_names.txt
    rm -rf "$stage"
    ;;
  create)
    printf 'mock-controlplane-container\n'
    ;;
  cp)
    case "${2:?}" in
      *runtime.descriptor.json) cp "${MOCK_RUNTIME_DESCRIPTOR_PATH:?}" "${3:?}" ;;
      *tzdb_names.txt) cp "${MOCK_TIMEZONE_DATA_PATH:?}/tzdb_names.txt" "${3:?}" ;;
      *) exit 1 ;;
    esac
    ;;
  rm)
    ;;
  *) exit 1 ;;
esac
EOF
chmod 0755 "${controlplane_bin}"/*
PATH="${controlplane_bin}:${PATH}" MOCK_RUNTIME_DESCRIPTOR_PATH="${controlplane_runtime_release}/runtime.descriptor.json" MOCK_TIMEZONE_DATA_PATH="${controlplane_timezone_data}" CONTROLPLANE_IMAGE_CONTEXT="${controlplane_context}" CONTROLPLANE_IMAGE_PLATFORM=linux/amd64 \
  "${controlplane_build_script}" example.invalid/helmr-controlplane:test
base_image="$(jq -r '.baseImage' "${controlplane_build_contract}")"
assert_contains "${controlplane_context}/Dockerfile" "FROM ${base_image}" "digest-pinned Control Plane base"
PATH="${controlplane_bin}:${PATH}" MOCK_RUNTIME_DESCRIPTOR_PATH="${controlplane_runtime_release}/runtime.descriptor.json" MOCK_TIMEZONE_DATA_PATH="${controlplane_timezone_data}" "${repo_root}/scripts/verify-controlplane-image-build.sh" \
  "${controlplane_context}/build-inputs.json" example.invalid/helmr-controlplane:test

release_build_inputs="${tmp}/release-build-inputs.json"
jq '.buildVersion = "v0.0.0-test"' "${controlplane_context}/build-inputs.json" >"${release_build_inputs}"
RELEASE_TAG=v0.0.0-test PATH="${controlplane_bin}:${PATH}" MOCK_RUNTIME_DESCRIPTOR_PATH="${controlplane_runtime_release}/runtime.descriptor.json" MOCK_TIMEZONE_DATA_PATH="${controlplane_timezone_data}" "${repo_root}/scripts/verify-controlplane-image-build.sh" \
  "${release_build_inputs}" example.invalid/helmr-controlplane:test

drifted="${tmp}/drifted-build-inputs.json"
jq '.sourceCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' \
  "${controlplane_context}/build-inputs.json" >"${drifted}"
if PATH="${controlplane_bin}:${PATH}" MOCK_RUNTIME_DESCRIPTOR_PATH="${controlplane_runtime_release}/runtime.descriptor.json" MOCK_TIMEZONE_DATA_PATH="${controlplane_timezone_data}" "${repo_root}/scripts/verify-controlplane-image-build.sh" \
  "${drifted}" example.invalid/helmr-controlplane:test >/dev/null 2>&1; then
  fail "Control Plane image verification must reject source-commit drift"
fi

worker_state="${tmp}/worker-state"
mkdir -p "${worker_state}"
source_commit="$(git -C "${repo_root}" rev-parse HEAD)"
host_artifacts_manifest_digest="sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
host_artifacts_bundle_digest="sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
runtime_artifacts_manifest_digest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
runtime_artifacts_bundle_digest="sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
component_digest="sha256:4444444444444444444444444444444444444444444444444444444444444444"
image_digest="sha256:5555555555555555555555555555555555555555555555555555555555555555"
prepare_root_digest="sha256:6666666666666666666666666666666666666666666666666666666666666666"
component_arn="arn:aws:imagebuilder:us-west-2:123456789012:component/example-component-${component_digest#sha256:}/1.0.0/1"
recipe_arn="arn:aws:imagebuilder:us-west-2:123456789012:image-recipe/example-recipe-${image_digest#sha256:}/1.0.0"
build_arn="arn:aws:imagebuilder:us-west-2:123456789012:image/example-recipe-${image_digest#sha256:}/1.0.0/1"
jq -cnS \
  --arg component_arn "${component_arn}" \
  --arg component_digest "${component_digest}" \
  --arg host_bundle_digest "${host_artifacts_bundle_digest}" \
  --arg host_manifest_digest "${host_artifacts_manifest_digest}" \
  --arg image_digest "${image_digest}" \
  --arg prepare_root_digest "${prepare_root_digest}" \
  --arg recipe_arn "${recipe_arn}" \
  --arg runtime_bundle_digest "${runtime_artifacts_bundle_digest}" \
  --arg runtime_manifest_digest "${runtime_artifacts_manifest_digest}" \
  --arg source_commit "${source_commit}" '
  {
    schema:"helmr.worker-image-definition-state.v0",
    componentARN:$component_arn,
    componentDefinitionDigest:$component_digest,
    distributionConfigurationARN:"arn:aws:imagebuilder:us-west-2:123456789012:distribution-configuration/example",
    distributionRegions:["us-east-1","us-west-2"],
    imageDefinitionDigest:$image_digest,
    imagePipelineARN:"arn:aws:imagebuilder:us-west-2:123456789012:image-pipeline/example",
    imageRecipeARN:$recipe_arn,
    prepareRootDigest:$prepare_root_digest,
    resolvedParentImageID:"ami-00000000000000001",
    rootBlockDeviceMapping:{deviceName:"/dev/sda1",ebs:{deleteOnTermination:true,encrypted:true,volumeSize:24,volumeType:"gp3"}},
    visibility:"private",
    hostArtifacts:{sourceCommit:$source_commit,bundleDigest:$host_bundle_digest,manifestDigest:$host_manifest_digest},
    runtimeArtifacts:{sourceCommit:$source_commit,bundleDigest:$runtime_bundle_digest,manifestDigest:$runtime_manifest_digest}
  }
' >"${worker_state}/worker-image-definition.json"
mock_image_json="${tmp}/worker-image.json"
jq -cn --arg arn "${build_arn}" --arg recipe "${recipe_arn}" '
  {image:{arn:$arn,state:{status:"AVAILABLE"},imageRecipe:{arn:$recipe},outputResources:{amis:[
    {region:"us-east-1",image:"ami-00000000000000002"},
    {region:"us-west-2",image:"ami-00000000000000003"}
  ]}}}
' >"${mock_image_json}"
if ! (
  set -- help
  export AWS_REGION=us-west-2 STATE_DIR="${worker_state}"
  # shellcheck source=/dev/null
  source "${script}" >/dev/null
  require_clean_product_checkout() { :; }
  require_current_worker_image_definition() { :; }
  validate_worker_image_receipt_live() { :; }
  aws() {
    [ "${1:-}:${2:-}" = imagebuilder:get-image ] || return 1
    cat "${mock_image_json}"
  }
  worker_image_wait "${build_arn}"
) >"${stdout}" 2>"${stderr}"; then
  cat "${stderr}" >&2
  fail "Worker image wait should write a closed receipt"
fi
assert_equal "${build_arn}" "$(cat "${stdout}")" "Worker image build ARN"
[ -s "${worker_state}/worker-image.json" ] || fail "Worker image receipt"
jq -e \
  --arg host_bundle_digest "${host_artifacts_bundle_digest}" \
  --arg host_digest "${host_artifacts_manifest_digest}" \
  --arg runtime_bundle_digest "${runtime_artifacts_bundle_digest}" \
  --arg runtime_digest "${runtime_artifacts_manifest_digest}" '
  .schema == "helmr.worker-image.v0" and
  .amis == {"us-east-1":"ami-00000000000000002","us-west-2":"ami-00000000000000003"} and
  .visibility == "private" and
  .hostArtifacts.bundleDigest == $host_bundle_digest and
  .hostArtifacts.manifestDigest == $host_digest and
  (.runtimeArtifacts | keys == ["bundleDigest", "manifestDigest", "sourceCommit"]) and
  .runtimeArtifacts.bundleDigest == $runtime_bundle_digest and
  .runtimeArtifacts.manifestDigest == $runtime_digest
' "${worker_state}/worker-image.json" >/dev/null || fail "Worker image receipt"

(
  set -- help
  export STATE_DIR="${worker_state}"
  # shellcheck source=/dev/null
  source "${script}" >/dev/null
  require_clean_product_checkout() { :; }
  require_current_worker_image_definition() { :; }
  validate_worker_image_receipt_live() { :; }
  aws() { fail "valid receipt should skip pipeline execution"; }
  worker_image_start
) >"${stdout}" 2>"${stderr}"
assert_equal "${build_arn}" "$(cat "${stdout}")" "reused Worker image build ARN"

jq '.visibility = "public"' "${worker_state}/worker-image-definition.json" >"${worker_state}/worker-image-definition.next"
mv "${worker_state}/worker-image-definition.next" "${worker_state}/worker-image-definition.json"
new_build_arn="arn:aws:imagebuilder:us-west-2:123456789012:image/example-recipe-${image_digest#sha256:}/1.0.0/2"
(
  set -- help
  export STATE_DIR="${worker_state}"
  # shellcheck source=/dev/null
  source "${script}" >/dev/null
  require_clean_product_checkout() { :; }
  require_current_worker_image_definition() { :; }
  aws() { printf '%s\n' "${new_build_arn}"; }
  worker_image_start
) >"${stdout}" 2>"${stderr}"
assert_equal "${new_build_arn}" "$(cat "${stdout}")" "distribution change build ARN"

printf 'ok - Product AWS release artifact tests\n'
