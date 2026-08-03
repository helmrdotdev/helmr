#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
script="${repo_root}/scripts/aws-release-artifacts.sh"
control_build_script="${repo_root}/scripts/build-control-image.sh"
control_build_contract="${repo_root}/images/control-image-build.json"

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

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
stdout="${tmp}/stdout"
stderr="${tmp}/stderr"

platform_release="${tmp}/platform-release"
platform_bin="${tmp}/platform-bin"
platform_state="${tmp}/platform-state"
platform_input_marker="${tmp}/platform-input"
mkdir -p "${platform_release}/objects/sha256" "${platform_bin}" "${platform_state}"
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

if STATE_DIR="${platform_state}" TF_BIN="${platform_bin}/tofu" \
  MOCK_PLATFORM_RELEASE="${platform_release}" \
  MOCK_PLATFORM_RELEASE_INPUT_MARKER="${platform_input_marker}" \
  PATH="${platform_bin}:${PATH}" \
  "${script}" platform-release-publish >"${stdout}" 2>"${stderr}"; then
  fail "platform-release-publish should surface publisher failure"
fi
[ -s "${platform_input_marker}" ] || fail "publisher must receive the sealed release tree"
[ ! -e "$(cat "${platform_input_marker}")" ] || fail "failed publisher must remove its sealed release tree"

control_bin="${tmp}/control-bin"
control_context="${tmp}/control-context"
mkdir -p "${control_bin}"
for command in bun make go; do
  printf '#!/usr/bin/env bash\nexit 0\n' >"${control_bin}/${command}"
done
cat >"${control_bin}/docker" <<'EOF'
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
chmod 0755 "${control_bin}"/*
PATH="${control_bin}:${PATH}" CONTROL_IMAGE_CONTEXT="${control_context}" CONTROL_IMAGE_PLATFORM=linux/amd64 \
  "${control_build_script}" example.invalid/helmr-control:test
base_image="$(jq -r '.baseImage' "${control_build_contract}")"
assert_contains "${control_context}/Dockerfile" "FROM ${base_image}" "digest-pinned Control base"
PATH="${control_bin}:${PATH}" "${repo_root}/scripts/verify-control-image-build.sh" \
  "${control_context}/build-inputs.json" example.invalid/helmr-control:test

drifted="${tmp}/drifted-build-inputs.json"
jq '.sourceCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' \
  "${control_context}/build-inputs.json" >"${drifted}"
if PATH="${control_bin}:${PATH}" "${repo_root}/scripts/verify-control-image-build.sh" \
  "${drifted}" example.invalid/helmr-control:test >/dev/null 2>&1; then
  fail "Control image verification must reject source-commit drift"
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
chmod 0755 "${worker_bin}"/*
source_commit="$(git -C "${repo_root}" rev-parse HEAD)"
ami_json="${tmp}/worker-ami.json"
jq -cn --arg commit "${source_commit}" '{Images:[{ImageId:"ami-0bbbbbbbbbbbbbbbb",Tags:[{Key:"HelmrSourceCommit",Value:$commit}]}]}' >"${ami_json}"
image_json="${tmp}/worker-image.json"
cat >"${image_json}" <<'JSON'
{"image":{"state":{"status":"AVAILABLE"},"imageRecipe":{"arn":"arn:aws:imagebuilder:us-west-2:123456789012:image-recipe/example/1.0.0"},"outputResources":{"amis":[{"region":"us-east-1","image":"ami-0aaaaaaaaaaaaaaaa"},{"region":"us-west-2","image":"ami-0bbbbbbbbbbbbbbbb"}]}}}
JSON
AWS_REGION=us-west-2 STATE_DIR="${worker_state}" MOCK_IMAGE_JSON="${image_json}" MOCK_AMI_JSON="${ami_json}" \
  TF_BIN="${worker_bin}/tofu" PATH="${worker_bin}:${PATH}" \
  "${script}" worker-image-wait arn:aws:imagebuilder:us-west-2:123456789012:image/example/1.0.0/1 >"${stdout}" 2>"${stderr}"
assert_equal "ami-0bbbbbbbbbbbbbbbb" "$(cat "${stdout}")" "current-region Worker AMI"
[ -s "${worker_state}/worker-image-provenance.json" ] || fail "Worker AMI provenance receipt"

printf 'ok - Product AWS release artifact tests\n'
