#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
script="$repo_root/scripts/aws-dev-smoke.sh"
control_build_script="$repo_root/scripts/build-control-image.sh"
control_build_contract="$repo_root/images/control-image-build.json"
control_module="$repo_root/infra/aws/modules/control/main.tf"
dev_stack="$repo_root/infra/aws/stacks/dev/main.tf"

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

assert_contains() {
  local file="$1"
  local needle="$2"
  local label="$3"
  grep -Fq -- "$needle" "$file" || fail "$label: expected '$needle' in $file"
}

assert_not_contains() {
  local file="$1"
  local needle="$2"
  local label="$3"
  ! grep -Fq -- "$needle" "$file" || fail "$label: did not expect '$needle' in $file"
}

assert_equal() {
  local expected="$1"
  local actual="$2"
  local label="$3"
  [ "$actual" = "$expected" ] || fail "$label: expected '$expected', got '$actual'"
}

assert_tfvar_count() {
  local file="$1"
  local key="$2"
  local expected="$3"
  local label="$4"
  local actual
  actual="$(grep -Ec "^[[:space:]]*${key}[[:space:]]*=" "$file" || true)"
  [ "$actual" = "$expected" ] || fail "$label: expected $expected assignments for $key, got $actual"
}

sha256_stdin() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print $1}'
  else
    shasum -a 256 | awk '{print $1}'
  fi
}

replace_tfvar() {
  local file="$1"
  local key="$2"
  local value="$3"
  local replacement="${file}.replacement"
  awk -v key="$key" -v value="$value" '
    $0 ~ "^[[:space:]]*" key "[[:space:]]*=" { print key " = " value; next }
    { print }
  ' "$file" >"$replacement"
  mv "$replacement" "$file"
}

write_tfvars() {
  local file="$1"
  local public_url="$2"
  local certificate_arn="$3"
  local enable_cloudfront="$4"
  local cloudfront_origin="$5"

  cat >"$file" <<EOF
public_url = $public_url
certificate_arn = $certificate_arn
enable_cloudfront = $enable_cloudfront
cloudfront_origin_domain_name = $cloudfront_origin
EOF
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

tfvars="$tmp/dev.tfvars"
stdout="$tmp/stdout"
stderr="$tmp/stderr"

assert_not_contains "$control_module" 'resource "aws_ecr_repository"' "Control module must not own release-image storage"
assert_not_contains "$dev_stack" 'resource "aws_ecr_repository"' "ephemeral dev stack must not own release-image storage"
assert_not_contains "$script" 'CONTROL_IMAGE_REPOSITORY' "Control repository override must be absent"
assert_contains "$script" 'bootstrap_contract_value CONTROL_RELEASE_REPOSITORY_URL control_release_repository_url' "Control publication must resolve the durable foundation repository"
assert_contains "$script" 'with_platform_publisher aws ecr get-login-password' "Control publication must use the bounded release-publisher role"
assert_contains "$script" 'with_platform_publisher nix develop' "Platform publication must use the bounded release-publisher role"
# shellcheck disable=SC2016
assert_contains "$script" 'install -m0400 "${object}"' "Platform publication must seal Nix objects before publisher handoff"
# shellcheck disable=SC2016
assert_contains "$script" 'trap '\''rm -rf "${publish_input}"'\'' EXIT' "Platform publication must remove its sealed staging tree"
# shellcheck disable=SC2016
assert_contains "$script" 'docker --config "${docker_config}" push' "Control publication must isolate temporary ECR credentials"
# shellcheck disable=SC2016
assert_contains "$control_build_script" 'FROM ${base_image}' "Control builds must consume the checked-in digest-pinned base"
jq -e '.baseImage | test("@sha256:[0-9a-f]{64}$")' "$control_build_contract" >/dev/null ||
  fail "Control base image must be pinned by digest"
assert_contains "$script" 'run control-image-build and control-image-push first' "dev stack generation must require pre-published Control provenance"

platform_release="$tmp/platform-release"
platform_release_bin="$tmp/platform-release-bin"
platform_release_state="$tmp/platform-release-state"
platform_release_input_marker="$tmp/platform-release-input"
mkdir -p "$platform_release/objects/sha256" "$platform_release_bin" "$platform_release_state"
printf '{}' >"$platform_release/platform-release.json"
printf 'object' >"$platform_release/objects/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
printf 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' >"$platform_release/build-policy.digest"
chmod 0444 "$platform_release/objects/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
cat >"$platform_release_bin/git" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
cat >"$platform_release_bin/tofu" <<'EOF'
#!/usr/bin/env bash
case "${*: -1}" in
  platform_publisher_role_arn) printf 'arn:aws:iam::123456789012:role/platform-publisher\n' ;;
  platform_store_uri) printf 's3://platform-store/objects/sha256\n' ;;
  *) exit 1 ;;
esac
EOF
cat >"$platform_release_bin/aws" <<'EOF'
#!/usr/bin/env bash
jq -cn '{Credentials:{AccessKeyId:"test",SecretAccessKey:"test",SessionToken:"test"}}'
EOF
cat >"$platform_release_bin/nix" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  build)
    printf '%s\n' "$MOCK_PLATFORM_RELEASE"
    ;;
  develop)
    while [ "$#" -gt 0 ]; do
      if [ "$1" = "--input" ]; then
        input="${2:-}"
        break
      fi
      shift
    done
    [ -n "${input:-}" ]
    object="$(find "$input/objects/sha256" -maxdepth 1 -type f -print -quit)"
    if stat -f '%Lp' "$object" >/dev/null 2>&1; then
      mode="$(stat -f '%Lp' "$object")"
    else
      mode="$(stat -c '%a' "$object")"
    fi
    [ "$mode" = 400 ]
    printf '%s\n' "$input" >"$MOCK_PLATFORM_RELEASE_INPUT_MARKER"
    exit 42
    ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$platform_release_bin/"*
if STATE_DIR="$platform_release_state" \
  TF_BIN="$platform_release_bin/tofu" \
  MOCK_PLATFORM_RELEASE="$platform_release" \
  MOCK_PLATFORM_RELEASE_INPUT_MARKER="$platform_release_input_marker" \
  PATH="$platform_release_bin:$PATH" \
  "$script" platform-release-publish >"$stdout" 2>"$stderr"; then
  fail "platform-release-publish should surface publisher failure"
fi
[ -s "$platform_release_input_marker" ] || fail "publisher must receive the sealed release tree"
[ ! -e "$(cat "$platform_release_input_marker")" ] || fail "failed publisher must remove its sealed release tree"

control_build_bin="$tmp/control-build-bin"
control_build_context="$tmp/control-build-context"
mkdir -p "$control_build_bin"
for command in bun make go; do
  cat >"$control_build_bin/$command" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
  chmod +x "$control_build_bin/$command"
done
cat >"$control_build_bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  build) ;;
  image)
    [ "${2:-}" = "inspect" ]
    printf '%s\n' "${MOCK_LOCAL_IMAGE_ID:-sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}"
    ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$control_build_bin/docker"
PATH="$control_build_bin:$PATH" \
  CONTROL_IMAGE_CONTEXT="$control_build_context" \
  CONTROL_IMAGE_PLATFORM=linux/amd64 \
  "$control_build_script" example.invalid/helmr-control:test
base_image="$(jq -r '.baseImage' "$control_build_contract")"
assert_contains "$control_build_context/Dockerfile" "FROM ${base_image}" "generated Control Dockerfile must use the digest-pinned base"
if command -v sha256sum >/dev/null 2>&1; then
  flake_lock_sha256="$(sha256sum "$repo_root/flake.lock" | awk '{print $1}')"
else
  flake_lock_sha256="$(shasum -a 256 "$repo_root/flake.lock" | awk '{print $1}')"
fi
jq -e \
  --arg base_image "$base_image" \
  --arg flake_lock_sha256 "$flake_lock_sha256" \
  --arg source_commit "$(git -C "$repo_root" rev-parse HEAD)" '
  . == {
    baseImage: $base_image,
    buildVersion: "",
    formatVersion: 1,
    localImageId: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    platform: "linux/amd64",
    sourceCommit: $source_commit,
    toolchain: {kind: "nix-flake-lock", sha256: $flake_lock_sha256}
  }
' "$control_build_context/build-inputs.json" >/dev/null ||
  fail "Control build-input receipt must close over base, platform, and Nix toolchain"
PATH="$control_build_bin:$PATH" \
  "$repo_root/scripts/verify-control-image-build.sh" \
  "$control_build_context/build-inputs.json" \
  example.invalid/helmr-control:test
drifted_build_inputs="$tmp/drifted-build-inputs.json"
jq '.sourceCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' \
  "$control_build_context/build-inputs.json" >"$drifted_build_inputs"
if PATH="$control_build_bin:$PATH" \
  "$repo_root/scripts/verify-control-image-build.sh" \
  "$drifted_build_inputs" \
  example.invalid/helmr-control:test >/dev/null 2>&1; then
  fail "Control image publication must reject source-commit drift"
fi
if PATH="$control_build_bin:$PATH" \
  MOCK_LOCAL_IMAGE_ID=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  "$repo_root/scripts/verify-control-image-build.sh" \
  "$control_build_context/build-inputs.json" \
  example.invalid/helmr-control:test >/dev/null 2>&1; then
  fail "Control image publication must reject local image substitution"
fi

write_tfvars "$tfvars" '"http://localhost"' null false null
if WORKER_AMI_ID=ami-0123456789abcdef0 DEV_TFVARS="$tfvars" "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"; then
  fail "dev-worker-tfvars should require certificate_arn before enabling workers"
fi
assert_contains "$stderr" "requires DEV_CERTIFICATE_ARN or an existing certificate_arn tfvar" "missing certificate guard"

write_tfvars "$tfvars" '"http://localhost"' '"arn:aws:acm:us-east-1:123456789012:certificate/example"' false null
if WORKER_AMI_ID=ami-0123456789abcdef0 DEV_TFVARS="$tfvars" "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"; then
  fail "dev-worker-tfvars should reject loopback public_url"
fi
assert_contains "$stderr" "requires public_url to use a non-loopback hostname" "loopback public_url guard"

write_tfvars "$tfvars" '"https://viewer.example.com"' '"arn:aws:acm:us-east-1:123456789012:certificate/example"' true '"localhost"'
if WORKER_AMI_ID=ami-0123456789abcdef0 DEV_TFVARS="$tfvars" "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"; then
  fail "dev-worker-tfvars should reject loopback CloudFront origin"
fi
assert_contains "$stderr" "requires cloudfront_origin_domain_name to use a non-loopback hostname" "loopback cloudfront origin guard"

write_tfvars "$tfvars" '"https://viewer.example.com"' '"arn:aws:acm:us-east-1:123456789012:certificate/example"' true '"https://origin.example.com:443/path"'
if WORKER_AMI_ID=ami-0123456789abcdef0 DEV_TFVARS="$tfvars" "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"; then
  fail "dev-worker-tfvars should reject URL-shaped CloudFront origin"
fi
assert_contains "$stderr" "requires cloudfront_origin_domain_name to be a DNS hostname without scheme, path, or port" "URL-shaped cloudfront origin guard"

write_tfvars "$tfvars" '"http://localhost"' null false null
cat >>"$tfvars" <<'EOF'
build_worker_instance_type = "m8i.2xlarge"
build_worker_root_volume_size_gb = 500
build_worker_root_volume_iops = 12000
build_worker_root_volume_throughput = 500
build_worker_capacity_vcpus = 8
build_worker_capacity_memory_mib = 32768
build_worker_execution_slots = 8
EOF
WORKER_AMI_ID=ami-0123456789abcdef0 \
  DEV_TFVARS="$tfvars" \
  DEV_PUBLIC_URL=https://control.example.com \
  DEV_CERTIFICATE_ARN=arn:aws:acm:us-east-1:123456789012:certificate/example \
  "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"
assert_contains "$tfvars" 'public_url = "https://control.example.com"' "public URL override"
assert_contains "$tfvars" 'certificate_arn = "arn:aws:acm:us-east-1:123456789012:certificate/example"' "certificate override"
assert_contains "$tfvars" 'create_worker = true' "worker enabled"
assert_contains "$tfvars" 'worker_disk_mib = 98304' "configured worker disk ceiling"
assert_contains "$tfvars" 'worker_capacity_vcpus = 4' "configured worker CPU capacity"
assert_contains "$tfvars" 'worker_capacity_memory_mib = 8192' "configured worker memory capacity"
assert_contains "$tfvars" 'worker_execution_slots = 1' "configured worker execution slots"
assert_contains "$tfvars" 'build_worker_vm_vcpus = 3' "fixed image-build VM CPU"
assert_contains "$tfvars" 'build_worker_vm_memory_mib = 4096' "fixed image-build VM memory"
assert_contains "$tfvars" 'build_worker_vm_scratch_disk_mib = 32768' "fixed image-build VM scratch disk"
assert_contains "$tfvars" 'build_worker_instance_type = null' "build worker instance type inherits priced shape"
assert_contains "$tfvars" 'build_worker_root_volume_size_gb = null' "build worker volume inherits priced shape"
assert_contains "$tfvars" 'build_worker_root_volume_iops = null' "build worker IOPS inherits priced shape"
assert_contains "$tfvars" 'build_worker_root_volume_throughput = null' "build worker throughput inherits priced shape"
assert_contains "$tfvars" 'build_worker_capacity_vcpus = null' "build worker CPU inherits configured shape"
assert_contains "$tfvars" 'build_worker_capacity_memory_mib = null' "build worker memory inherits configured shape"
assert_contains "$tfvars" 'build_worker_execution_slots = null' "build worker slots inherit configured shape"

WORKER_AMI_ID=ami-0123456789abcdef0 \
  DEV_TFVARS="$tfvars" \
  DEV_WORKER_MAX_SIZE=2 \
  DEV_WORKER_EXECUTION_SLOTS=2 \
  DEV_ALLOW_EXTENDED_WORKER_CAPACITY=true \
  "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"
assert_contains "$tfvars" 'allow_extended_worker_capacity = true' "extended-capacity approval"
assert_contains "$tfvars" 'worker_max_size = 2' "configured run worker ceiling"
assert_contains "$tfvars" 'worker_execution_slots = 2' "configured worker concurrency"
assert_contains "$tfvars" 'worker_observation_ttl_seconds = 120' "configured readiness freshness"
assert_contains "$tfvars" 'worker_launch_timeout_seconds = 900' "configured launch timeout"

WORKER_AMI_ID=ami-0123456789abcdef0 \
  DEV_TFVARS="$tfvars" \
  "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"
assert_contains "$tfvars" 'allow_extended_worker_capacity = false' "ordinary profile restores the extended-capacity guard"
assert_contains "$tfvars" 'worker_max_size = 1' "ordinary profile restores run worker ceiling"
assert_contains "$tfvars" 'worker_execution_slots = 1' "ordinary profile restores one slot"

if WORKER_AMI_ID=ami-0123456789abcdef0 \
  DEV_TFVARS="$tfvars" \
  DEV_WORKER_MAX_SIZE=2 \
  "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"; then
  fail "extended Worker capacity should require explicit approval"
fi
assert_contains "$stderr" "DEV_ALLOW_EXTENDED_WORKER_CAPACITY=true is required" "extended-capacity approval guard"
replace_tfvar "$tfvars" worker_min_size 0
replace_tfvar "$tfvars" build_worker_min_size 0

WORKER_AMI_ID=ami-0bbbbbbbbbbbbbbbb \
  DEV_TFVARS="$tfvars" \
  "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"
assert_contains "$tfvars" 'worker_ami_id = "ami-0bbbbbbbbbbbbbbbb"' "worker AMI advances directly through ASG instance refresh"
assert_tfvar_count "$tfvars" worker_ami_id 1 "worker AMI replacement should not duplicate"

cat >"$tfvars" <<'EOF'
public_url="http://localhost"
certificate_arn=null
enable_cloudfront=false
cloudfront_origin_domain_name=null
EOF
WORKER_AMI_ID=ami-0123456789abcdef0 \
  DEV_TFVARS="$tfvars" \
  DEV_PUBLIC_URL=https://compact.example.com \
  DEV_CERTIFICATE_ARN=arn:aws:acm:us-east-1:123456789012:certificate/example \
  "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"
assert_contains "$tfvars" 'public_url = "https://compact.example.com"' "compact public URL replacement"
assert_contains "$tfvars" 'certificate_arn = "arn:aws:acm:us-east-1:123456789012:certificate/example"' "compact certificate replacement"
assert_tfvar_count "$tfvars" public_url 1 "compact public URL replacement should not duplicate"
assert_tfvar_count "$tfvars" certificate_arn 1 "compact certificate replacement should not duplicate"

cat >"$tfvars" <<'EOF'
aws_region="us-west-2"
name="compact-smoke"
public_url="https://old.example.com"
create_worker=false
EOF
DEV_TFVARS="$tfvars" \
  AWS_REGION=us-west-2 \
  DEV_CONTROL_IMAGE=123456789012.dkr.ecr.us-west-2.amazonaws.com/helmr-control:test \
  DEV_PUBLIC_URL=https://replacement.example.com \
  DEV_GITHUB_OAUTH_CLIENT_ID=Iv1.example \
  DEV_BOOTSTRAP_OWNER_EMAIL=owner@example.com \
  PLATFORM_STORE_URI=s3://platform-store/runtime \
  PLATFORM_STORE_BUCKET_ARN=arn:aws:s3:::platform-store \
  PLATFORM_STORE_KMS_KEY_ARN=arn:aws:kms:us-west-2:123456789012:key/runtime \
  CONTROL_RELEASE_REPOSITORY_ARN=arn:aws:ecr:us-west-2:123456789012:repository/helmr-dev/control-releases \
  DEV_BUILD_POLICY_DIGEST=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  DEV_CLICKHOUSE_URL=https://example.clickhouse.cloud:8443 \
  DEV_CLICKHOUSE_PASSWORD_SECRET_ARN=arn:aws:secretsmanager:us-west-2:123456789012:secret:clickhouse \
  "$script" dev-control-tfvars >"$stdout" 2>"$stderr"
assert_contains "$tfvars" 'public_url = "https://replacement.example.com"' "compact tfvar replacement"
assert_tfvar_count "$tfvars" public_url 1 "compact tfvar replacement should not duplicate"
assert_not_contains "$tfvars" "https://old.example.com" "compact old value removal"

cat >"$tfvars" <<'EOF'
aws_region="us-west-2"
name="worker-smoke"
public_url="https://worker.example.com"
create_worker=true
worker_observation_ttl_seconds=120
EOF
worker_tfvars_before="$(sha256_stdin <"$tfvars")"
if DEV_TFVARS="$tfvars" \
  AWS_REGION=us-west-2 \
  DEV_CONTROL_IMAGE=123456789012.dkr.ecr.us-west-2.amazonaws.com/helmr-control:test \
  DEV_PUBLIC_URL=https://replacement.example.com \
  DEV_GITHUB_OAUTH_CLIENT_ID=Iv1.example \
  PLATFORM_STORE_URI=s3://platform-store/runtime \
  PLATFORM_STORE_BUCKET_ARN=arn:aws:s3:::platform-store \
  PLATFORM_STORE_KMS_KEY_ARN=arn:aws:kms:us-west-2:123456789012:key/runtime \
  CONTROL_RELEASE_REPOSITORY_ARN=arn:aws:ecr:us-west-2:123456789012:repository/helmr-dev/control-releases \
  DEV_BUILD_POLICY_DIGEST=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  DEV_CLICKHOUSE_URL=https://example.clickhouse.cloud:8443 \
  DEV_CLICKHOUSE_PASSWORD_SECRET_ARN=arn:aws:secretsmanager:us-west-2:123456789012:secret:clickhouse \
  "$script" dev-control-tfvars >"$stdout" 2>"$stderr"; then
  fail "dev-control-tfvars should reject removing active worker fleets"
fi
assert_contains "$stderr" "cannot remove active worker fleets" "active worker guard"
[ "$(sha256_stdin <"$tfvars")" = "$worker_tfvars_before" ] || fail "active worker refusal must leave tfvars unchanged"

mkdir -p "$tmp/bin"
cat >"$tmp/bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

case "${1:-}:${2:-}" in
  imagebuilder:get-image) cat "$MOCK_IMAGE_JSON" ;;
  ec2:describe-images) cat "$MOCK_AMI_JSON" ;;
  *) exit 1 ;;
esac
EOF
cat >"$tmp/bin/tofu" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"output -raw image_recipe_arn"*)
    printf '%s\n' 'arn:aws:imagebuilder:us-west-2:123456789012:image-recipe/example/1.0.0'
    ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$tmp/bin/aws"
chmod +x "$tmp/bin/tofu"

state_dir="$tmp/state"
mkdir -p "$state_dir"
source_commit="$(git -C "$repo_root" rev-parse HEAD)"
MOCK_AMI_JSON="$tmp/worker-ami.json"
jq -cn \
  --arg ami "ami-0bbbbbbbbbbbbbbbb" \
  --arg commit "$source_commit" '
  {
    Images:[{
      ImageId:$ami,
      Tags:[
        {Key:"HelmrSourceCommit",Value:$commit}
      ]
    }]
  }' >"$MOCK_AMI_JSON"
MOCK_IMAGE_JSON="$tmp/image-missing-region.json"
cat >"$MOCK_IMAGE_JSON" <<'JSON'
{
  "image": {
    "state": {
      "status": "AVAILABLE"
    },
    "imageRecipe": {
      "arn": "arn:aws:imagebuilder:us-west-2:123456789012:image-recipe/example/1.0.0"
    },
    "outputResources": {
      "amis": [
        {
          "region": "us-east-1",
          "image": "ami-0aaaaaaaaaaaaaaaa"
        }
      ]
    }
  }
}
JSON

if AWS_REGION=us-west-2 STATE_DIR="$state_dir" MOCK_IMAGE_JSON="$MOCK_IMAGE_JSON" MOCK_AMI_JSON="$MOCK_AMI_JSON" TF_BIN="$tmp/bin/tofu" PATH="$tmp/bin:$PATH" "$script" worker-image-wait arn:aws:imagebuilder:us-west-2:123456789012:image/example/1.0.0/1 >"$stdout" 2>"$stderr"; then
  fail "worker-image-wait should fail when AWS_REGION is absent from Image Builder AMIs"
fi
assert_contains "$stderr" "does not include an AMI for AWS_REGION=us-west-2" "missing AMI region guard"

MOCK_IMAGE_JSON="$tmp/image-current-region.json"
cat >"$MOCK_IMAGE_JSON" <<'JSON'
{
  "image": {
    "state": {
      "status": "AVAILABLE"
    },
    "imageRecipe": {
      "arn": "arn:aws:imagebuilder:us-west-2:123456789012:image-recipe/example/1.0.0"
    },
    "outputResources": {
      "amis": [
        {
          "region": "us-east-1",
          "image": "ami-0aaaaaaaaaaaaaaaa"
        },
        {
          "region": "us-west-2",
          "image": "ami-0bbbbbbbbbbbbbbbb"
        }
      ]
    }
  }
}
JSON

AWS_REGION=us-west-2 STATE_DIR="$state_dir" MOCK_IMAGE_JSON="$MOCK_IMAGE_JSON" MOCK_AMI_JSON="$MOCK_AMI_JSON" TF_BIN="$tmp/bin/tofu" PATH="$tmp/bin:$PATH" "$script" worker-image-wait arn:aws:imagebuilder:us-west-2:123456789012:image/example/1.0.0/1 >"$stdout" 2>"$stderr"
assert_equal "ami-0bbbbbbbbbbbbbbbb" "$(cat "$stdout")" "worker-image-wait current region AMI"
assert_equal "ami-0bbbbbbbbbbbbbbbb" "$(cat "$state_dir/worker-ami-id")" "recorded worker AMI"
[ -s "$state_dir/worker-image-provenance.json" ] || fail "worker-image-wait provenance receipt"

destroy_bin="$tmp/destroy-bin"
destroy_log="$tmp/destroy.log"
mkdir -p "$destroy_bin"
cat >"$destroy_bin/tofu" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"output -raw control_url"*) printf '%s\n' 'https://control.example.test' ;;
  *"output -raw worker_autoscaling_group_name"*) printf '%s\n' 'split-smoke-run-worker' ;;
  *"output -raw build_worker_autoscaling_group_name"*) printf '%s\n' 'split-smoke-build-worker' ;;
  *"output -raw worker_group_id"*) printf '%s\n' 'run-workers' ;;
  *"output -raw build_worker_group_id"*) printf '%s\n' 'run-workers-build' ;;
  *"output -json secret_arns"*) printf '%s\n' '{"operator_token":"arn:aws:secretsmanager:us-east-1:123456789012:secret:operator"}' ;;
  *) exit 1 ;;
esac
EOF
cat >"$destroy_bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$MOCK_DESTROY_LOG"
service="${1:-}"
operation="${2:-}"
args="$*"
case "$service:$operation" in
  sts:get-caller-identity)
    printf '123456789012\n'
    ;;
  autoscaling:describe-auto-scaling-groups)
    if [ "${MOCK_ASG_DESCRIBE_FAIL:-0}" = "1" ]; then
      exit 42
    fi
    asg=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --auto-scaling-group-names) asg="${2:-}"; shift 2 ;;
        *) shift ;;
      esac
    done
    if [ "${MOCK_ACTIVE_ASG:-0}" = "1" ] && ! grep -q "autoscaling terminate-instance-in-auto-scaling-group .*--instance-id i-active" "$MOCK_DESTROY_LOG"; then
      if [[ "$args" == *"--output text"* ]]; then
        printf 'i-active\n'
      else
        printf '{"AutoScalingGroups":[{"AutoScalingGroupName":"%s","DesiredCapacity":1,"Instances":[{"InstanceId":"i-active","LifecycleState":"InService","ProtectedFromScaleIn":true}]}]}\n' "$asg"
      fi
    else
      printf '{"AutoScalingGroups":[{"AutoScalingGroupName":"%s","DesiredCapacity":0,"Instances":[]}]}\n' "$asg"
    fi
    ;;
  secretsmanager:get-secret-value)
    printf '%s\n' 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'
    ;;
  autoscaling:terminate-instance-in-auto-scaling-group)
    ;;
  rds:describe-db-instances)
    printf 'False\n'
    ;;
  s3api:head-bucket)
    exit 1
    ;;
  *)
    exit 1
    ;;
esac
EOF
cat >"$destroy_bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'curl %s\n' "$*" >>"$MOCK_DESTROY_LOG"
[ "${MOCK_OPERATOR_FAIL:-0}" != "1" ] || exit 22
method=GET
output_file=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --request) method="${2:-}"; shift 2 ;;
    --output) output_file="${2:-}"; shift 2 ;;
    http://*|https://*) url="$1"; shift ;;
    *) shift ;;
  esac
done
case "$url" in
  *'/api/operator/worker-instances?'*)
    response='{"worker_instances":[{"id":"01984b4c-7c5e-7b7c-8e9f-a1b2c3d4e5f6","resource_id":"i-active","worker_group_id":"run-workers","state":"active","claim_version":5,"current_epoch":3,"supports_run":true,"supports_build":false,"created_at":"2026-08-01T00:00:00Z","updated_at":"2026-08-01T00:00:00Z"}]}'
    ;;
  *'/drain')
    [ "$method" = POST ] || exit 2
    response='{"id":"01984b4c-7c5e-7b7c-8e9f-a1b2c3d4e5f6","resource_id":"i-active","worker_group_id":"run-workers","state":"draining","claim_version":6,"current_epoch":3,"supports_run":true,"supports_build":false,"created_at":"2026-08-01T00:00:00Z","updated_at":"2026-08-01T00:00:00Z"}'
    ;;
  *'/api/operator/worker-instances/'*)
    response='{"id":"01984b4c-7c5e-7b7c-8e9f-a1b2c3d4e5f6","resource_id":"i-active","worker_group_id":"run-workers","state":"termination_ready","claim_version":7,"current_epoch":3,"supports_run":true,"supports_build":false,"termination_ready_at":"2026-08-01T00:00:00Z","created_at":"2026-08-01T00:00:00Z","updated_at":"2026-08-01T00:00:00Z"}'
    ;;
  *) exit 2 ;;
esac
if [ -n "$output_file" ]; then
  printf '%s\n' "$response" >"$output_file"
else
  printf '%s\n' "$response"
fi
EOF
chmod +x "$destroy_bin/aws" "$destroy_bin/tofu" "$destroy_bin/curl"
MOCK_DESTROY_LOG="$destroy_log" \
  DEV_NAME=Split-Smoke \
  STATE_DIR="$tmp/destroy-state" \
  TF_BIN="$destroy_bin/tofu" \
  PATH="$destroy_bin:$PATH" \
  "$script" dev-destroy-prepare >"$stdout" 2>"$stderr"
assert_contains "$destroy_log" "autoscaling describe-auto-scaling-groups --region us-east-1 --auto-scaling-group-names split-smoke-run-worker" "run worker destroy preparation"
assert_contains "$destroy_log" "autoscaling describe-auto-scaling-groups --region us-east-1 --auto-scaling-group-names split-smoke-build-worker" "build worker destroy preparation"
assert_not_contains "$destroy_log" "complete-lifecycle-action" "destroy must not bypass worker drain proof"
assert_not_contains "$destroy_log" "split-smoke-worker" "removed shared worker compatibility name"
assert_contains "$destroy_log" "rds describe-db-instances --region us-east-1 --db-instance-identifier split-smoke-postgres" "normalized database cleanup name"
assert_not_contains "$destroy_log" "ecr " "ephemeral dev teardown must not inspect or mutate the foundation release repository"
assert_contains "$destroy_log" "s3api head-bucket --bucket split-smoke-123456789012-us-east-1-cas" "normalized CAS cleanup name"

: >"$destroy_log"
MOCK_ACTIVE_ASG=1 \
  MOCK_DESTROY_LOG="$destroy_log" \
  DEV_NAME=Split-Smoke \
  STATE_DIR="$tmp/destroy-state" \
  TF_BIN="$destroy_bin/tofu" \
  PATH="$destroy_bin:$PATH" \
  "$script" dev-destroy-prepare >"$stdout" 2>"$stderr"
assert_contains "$destroy_log" "/api/operator/worker-instances/01984b4c-7c5e-7b7c-8e9f-a1b2c3d4e5f6/drain" "active Worker receives exact Control drain request"
assert_contains "$destroy_log" "/api/operator/worker-instances/01984b4c-7c5e-7b7c-8e9f-a1b2c3d4e5f6" "destroy reads exact termination receipt"
assert_contains "$destroy_log" "autoscaling terminate-instance-in-auto-scaling-group --region us-east-1 --instance-id i-active --should-decrement-desired-capacity" "drained Worker host is terminated exactly"
assert_not_contains "$destroy_log" "ssm " "planned scale-in must not use SSM"

: >"$destroy_log"
if MOCK_ACTIVE_ASG=1 \
  MOCK_OPERATOR_FAIL=1 \
  MOCK_DESTROY_LOG="$destroy_log" \
  DEV_NAME=Split-Smoke \
  STATE_DIR="$tmp/destroy-state" \
  TF_BIN="$destroy_bin/tofu" \
  PATH="$destroy_bin:$PATH" \
  "$script" dev-destroy-prepare >"$stdout" 2>"$stderr"; then
  fail "dev-destroy-prepare should fail closed when exact Worker drain cannot start"
fi
assert_not_contains "$destroy_log" "autoscaling terminate-instance-in-auto-scaling-group" "failed drain cannot reduce physical capacity"
assert_contains "$stderr" "failed to resolve logical Worker for i-active" "exact drain failure guard"

: >"$destroy_log"
if MOCK_ASG_DESCRIBE_FAIL=1 \
  MOCK_DESTROY_LOG="$destroy_log" \
  DEV_NAME=Split-Smoke \
  STATE_DIR="$tmp/destroy-state" \
  TF_BIN="$destroy_bin/tofu" \
  PATH="$destroy_bin:$PATH" \
  "$script" dev-destroy-prepare >"$stdout" 2>"$stderr"; then
  fail "dev-destroy-prepare should fail closed when an ASG lookup fails"
fi
assert_contains "$stderr" "failed to inspect worker Auto Scaling group split-smoke-run-worker before destroy" "ASG lookup failure guard"

printf 'ok - aws dev smoke tests\n'
