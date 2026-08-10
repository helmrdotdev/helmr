#!/usr/bin/env bash
# shellcheck disable=SC2329
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

# shellcheck disable=SC2016
assert_contains "${script}" 'host_bundle="$(prepare_worker_host_bundle)"' \
  "Worker image apply host bundle binding"
assert_contains "${script}" 'runtime_bundle="$(prepare_worker_runtime_bundle)"' \
  "Worker image apply runtime bundle binding"
assert_contains "${script}" 'runtime_artifacts_bundle_s3_uri=' \
  "Worker image apply runtime bundle transport"
assert_contains "${script}" 'HelmrRuntimeBundleDigest' \
  "Worker AMI runtime bundle provenance"
assert_contains "${script}" 'HelmrRuntimeArtifactsDigest' \
  "Worker AMI runtime artifact provenance"
assert_contains "${script}" 'HelmrHostBundleDigest' \
  "Worker AMI host bundle provenance"
assert_contains "${script}" 'HelmrHostArtifactsDigest' \
  "Worker AMI host artifact provenance"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
stdout="${tmp}/stdout"
stderr="${tmp}/stderr"

apply_args_file="${tmp}/worker-image-apply.args"
(
  set -- help
  export STATE_DIR="${tmp}/worker-image-apply"
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
            runtimeArtifactsManifest: {path: "runtime-artifacts.json", digest: $manifest_digest},
            runtimeProfile: {
              id: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
              arch: "x86_64",
              contract: "helmr.vm-runtime.v0",
              kernel_digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
              initramfs_digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
              rootfs_digest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
            }
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
printf '{}' >"${platform_release}/platform-release.json"
printf 'object' >"${platform_release}/objects/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
printf 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' >"${platform_release}/build-policy.digest"

cat >"${platform_bin}/git" <<'EOF'
#!/usr/bin/env bash
exit 0
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
  develop)
    [ "${AWS_REGION:-}" = us-east-1 ]
    [ "${AWS_DEFAULT_REGION:-}" = us-east-1 ]
    [ -z "${AWS_PROFILE+x}" ]
    [ "${AWS_ACCESS_KEY_ID:-}" = test ]
    [ "${AWS_SECRET_ACCESS_KEY:-}" = test ]
    [ "${AWS_SESSION_TOKEN:-}" = test ]
    while [ "$#" -gt 0 ]; do
      if [ "$1" = "--input" ]; then
        input=${2:-}
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
    ;;
  *) exit 1 ;;
esac
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
mkdir -p "${controlplane_bin}"
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
  *) exit 1 ;;
esac
EOF
chmod 0755 "${controlplane_bin}"/*
PATH="${controlplane_bin}:${PATH}" CONTROLPLANE_IMAGE_CONTEXT="${controlplane_context}" CONTROLPLANE_IMAGE_PLATFORM=linux/amd64 \
  "${controlplane_build_script}" example.invalid/helmr-controlplane:test
base_image="$(jq -r '.baseImage' "${controlplane_build_contract}")"
assert_contains "${controlplane_context}/Dockerfile" "FROM ${base_image}" "digest-pinned Control Plane base"
PATH="${controlplane_bin}:${PATH}" "${repo_root}/scripts/verify-controlplane-image-build.sh" \
  "${controlplane_context}/build-inputs.json" example.invalid/helmr-controlplane:test

drifted="${tmp}/drifted-build-inputs.json"
jq '.sourceCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' \
  "${controlplane_context}/build-inputs.json" >"${drifted}"
if PATH="${controlplane_bin}:${PATH}" "${repo_root}/scripts/verify-controlplane-image-build.sh" \
  "${drifted}" example.invalid/helmr-controlplane:test >/dev/null 2>&1; then
  fail "Control Plane image verification must reject source-commit drift"
fi

worker_bin="${tmp}/worker-bin"
worker_state="${tmp}/worker-state"
mkdir -p "${worker_bin}" "${worker_state}"
cat >"${worker_bin}/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}:${2:-}" in
  imagebuilder:get-image) cat "${MOCK_IMAGE_JSON}" ;;
  ec2:describe-images) cat "${MOCK_AMI_JSON}" ;;
  *) exit 1 ;;
esac
EOF
cat >"${worker_bin}/tofu" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *"output -raw image_recipe_arn"*) printf 'arn:aws:imagebuilder:us-west-2:123456789012:image-recipe/example/1.0.0\n' ;;
  *) exit 1 ;;
esac
EOF
cat >"${worker_bin}/nix" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' '{
  "id":"sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
  "arch":"x86_64",
  "contract":"helmr.vm-runtime.v0",
  "kernel_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "initramfs_digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "rootfs_digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
}'
EOF
chmod 0755 "${worker_bin}"/*
source_commit="$(git -C "${repo_root}" rev-parse HEAD)"
printf '%s\n' '{"schema":"helmr.worker-host-artifacts.v0"}' >"${worker_state}/worker-host-artifacts.json"
printf '%s\n' '{"schema":"helmr.runtime-artifacts.v0"}' >"${worker_state}/worker-runtime-artifacts.json"
if command -v sha256sum >/dev/null 2>&1; then
  host_artifacts_manifest_digest="sha256:$(sha256sum "${worker_state}/worker-host-artifacts.json" | awk '{print $1}')"
  runtime_artifacts_manifest_digest="sha256:$(sha256sum "${worker_state}/worker-runtime-artifacts.json" | awk '{print $1}')"
else
  host_artifacts_manifest_digest="sha256:$(shasum -a 256 "${worker_state}/worker-host-artifacts.json" | awk '{print $1}')"
  runtime_artifacts_manifest_digest="sha256:$(shasum -a 256 "${worker_state}/worker-runtime-artifacts.json" | awk '{print $1}')"
fi
host_artifacts_bundle_digest="sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
runtime_artifacts_bundle_digest="sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
jq -cn \
  --arg bundle_digest "${host_artifacts_bundle_digest}" \
  --arg commit "${source_commit}" \
  --arg manifest_digest "${host_artifacts_manifest_digest}" '
  {
    schema: "helmr.worker-host-bundle.v0",
    sourceCommit: $commit,
    workerVersion: $commit,
    bundle: {path: "worker-host-artifacts.tar", digest: $bundle_digest},
    manifest: {path: "worker-host-artifacts.json", digest: $manifest_digest}
  }
' >"${worker_state}/worker-host-bundle.json"
jq -cn \
  --arg bundle_digest "${runtime_artifacts_bundle_digest}" \
  --arg commit "${source_commit}" \
  --arg manifest_digest "${runtime_artifacts_manifest_digest}" '
  {
    schema: "helmr.worker-runtime-bundle.v0",
    sourceCommit: $commit,
    bundle: {path: "runtime-artifacts.tar", digest: $bundle_digest},
    runtimeArtifactsManifest: {path: "runtime-artifacts.json", digest: $manifest_digest},
    runtimeProfile: {
      id: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
      arch: "x86_64",
      contract: "helmr.vm-runtime.v0",
      kernel_digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      initramfs_digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      rootfs_digest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
    }
  }
' >"${worker_state}/worker-runtime-bundle.json"
ami_json="${tmp}/worker-ami.json"
jq -cn \
  --arg commit "${source_commit}" \
  --arg host_bundle_digest "${host_artifacts_bundle_digest}" \
  --arg host_digest "${host_artifacts_manifest_digest}" \
  --arg runtime_bundle_digest "${runtime_artifacts_bundle_digest}" \
  --arg runtime_digest "${runtime_artifacts_manifest_digest}" '
  {Images:[{ImageId:"ami-0bbbbbbbbbbbbbbbb",Tags:[
    {Key:"HelmrSourceCommit",Value:$commit},
    {Key:"HelmrHostBundleDigest",Value:$host_bundle_digest},
    {Key:"HelmrHostArtifactsDigest",Value:$host_digest},
    {Key:"HelmrRuntimeBundleDigest",Value:$runtime_bundle_digest},
    {Key:"HelmrRuntimeArtifactsDigest",Value:$runtime_digest}
  ]}]}
' >"${ami_json}"
image_json="${tmp}/worker-image.json"
cat >"${image_json}" <<'JSON'
{"image":{"state":{"status":"AVAILABLE"},"imageRecipe":{"arn":"arn:aws:imagebuilder:us-west-2:123456789012:image-recipe/example/1.0.0"},"outputResources":{"amis":[{"region":"us-east-1","image":"ami-0aaaaaaaaaaaaaaaa"},{"region":"us-west-2","image":"ami-0bbbbbbbbbbbbbbbb"}]}}}
JSON
AWS_REGION=us-west-2 STATE_DIR="${worker_state}" MOCK_IMAGE_JSON="${image_json}" MOCK_AMI_JSON="${ami_json}" \
  TF_BIN="${worker_bin}/tofu" PATH="${worker_bin}:${PATH}" \
  "${script}" worker-image-wait arn:aws:imagebuilder:us-west-2:123456789012:image/example/1.0.0/1 >"${stdout}" 2>"${stderr}"
assert_equal "ami-0bbbbbbbbbbbbbbbb" "$(cat "${stdout}")" "current-region Worker AMI"
[ -s "${worker_state}/worker-image-provenance.json" ] || fail "Worker AMI provenance receipt"
jq -e \
  --arg host_bundle_digest "${host_artifacts_bundle_digest}" \
  --arg host_digest "${host_artifacts_manifest_digest}" \
  --arg runtime_bundle_digest "${runtime_artifacts_bundle_digest}" \
  --arg runtime_digest "${runtime_artifacts_manifest_digest}" \
  --arg commit "${source_commit}" '
  .formatVersion == 1 and
  .hostArtifactsBundleDigest == $host_bundle_digest and
  .hostArtifactsManifestDigest == $host_digest and
  .runtimeArtifactsBundleDigest == $runtime_bundle_digest and
  .runtimeArtifactsManifestDigest == $runtime_digest and
  .runtimeProfile.arch == "x86_64" and
  .sourceCommit == $commit and
  .workerVersion == $commit
' "${worker_state}/worker-image-provenance.json" >/dev/null || fail "Worker AMI provenance receipt"

printf 'ok - Product AWS release artifact tests\n'
