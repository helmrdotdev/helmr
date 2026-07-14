#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
script="$repo_root/scripts/aws-dev-smoke.sh"

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
assert_contains "$tfvars" 'worker_disk_mib = 98304' "certified worker disk ceiling"
assert_contains "$tfvars" 'worker_capacity_vcpus = 4' "certified worker CPU capacity"
assert_contains "$tfvars" 'worker_capacity_memory_mib = 8192' "certified worker memory capacity"
assert_contains "$tfvars" 'worker_execution_slots = 1' "certified worker execution slots"
assert_contains "$tfvars" 'build_worker_instance_type = null' "build worker instance type inherits priced shape"
assert_contains "$tfvars" 'build_worker_root_volume_size_gb = null' "build worker volume inherits priced shape"
assert_contains "$tfvars" 'build_worker_root_volume_iops = null' "build worker IOPS inherits priced shape"
assert_contains "$tfvars" 'build_worker_root_volume_throughput = null' "build worker throughput inherits priced shape"
assert_contains "$tfvars" 'build_worker_capacity_vcpus = null' "build worker CPU inherits certified shape"
assert_contains "$tfvars" 'build_worker_capacity_memory_mib = null' "build worker memory inherits certified shape"
assert_contains "$tfvars" 'build_worker_execution_slots = null' "build worker slots inherit certified shape"
replace_tfvar "$tfvars" worker_min_size 0
replace_tfvar "$tfvars" build_worker_min_size 0

WORKER_AMI_ID=ami-0bbbbbbbbbbbbbbbb \
  DEV_TFVARS="$tfvars" \
  "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"
assert_contains "$tfvars" 'worker_ami_id = "ami-0123456789abcdef0"' "worker AMI stays unchanged during allowlist stage"
assert_contains "$tfvars" 'worker_allowed_ami_ids = ["ami-0123456789abcdef0","ami-0bbbbbbbbbbbbbbbb"]' "new worker AMI allowlist stage"
assert_contains "$stderr" "apply once, then rerun dev-worker-tfvars" "worker AMI rollout stage guidance"
assert_tfvar_count "$tfvars" worker_ami_id 1 "worker AMI replacement should not duplicate"

mock_tf="$tmp/mock-tofu"
applied_allowed_amis="$tmp/applied-worker-amis.json"
rollout_bin="$tmp/rollout-bin"
rollout_state="$tmp/rollout-state"
mkdir -p "$rollout_bin" "$rollout_state"
cat >"$mock_tf" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "${2:-}" = "apply" ]; then
  exit "${MOCK_APPLY_FAIL:-0}"
fi
case "${4:-}" in
  worker_allowed_ami_ids) cat "$MOCK_APPLIED_ALLOWED_AMIS" ;;
  control_cluster_name) printf 'cluster\n' ;;
  control_service_name) printf 'control-service\n' ;;
  migration_task_definition_arn) printf 'migration-task\n' ;;
  control_task_security_group_ids) printf '["sg-control"]\n' ;;
  control_task_subnet_ids) printf '["subnet-control"]\n' ;;
  control_assign_public_ip) printf 'false\n' ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$mock_tf"
cat >"$rollout_bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}:${2:-}" in
  ecs:wait) ;;
  ecs:describe-services)
    if [ -n "${MOCK_TRANSIENT_DEPLOYMENT_STATE:-}" ] && [ ! -e "${MOCK_TRANSIENT_DEPLOYMENT_STATE}" ]; then
      : >"${MOCK_TRANSIENT_DEPLOYMENT_STATE}"
      printf '%s\n' '{"failures":[],"services":[{"desiredCount":1,"runningCount":1,"pendingCount":0,"taskDefinition":"arn:taskdef:control:2","deployments":[{"status":"PRIMARY","rolloutState":"COMPLETED"},{"status":"DRAINING","rolloutState":"COMPLETED"}]}]}'
      exit 0
    fi
    printf '%s\n' '{"failures":[],"services":[{"desiredCount":1,"runningCount":1,"pendingCount":0,"taskDefinition":"arn:taskdef:control:2","deployments":[{"status":"PRIMARY","rolloutState":"COMPLETED"}]}]}'
    ;;
  ecs:list-tasks) printf '%s\n' '{"taskArns":["arn:task:control-1"]}' ;;
  ecs:run-task) printf 'arn:task:migration-1\n' ;;
  ecs:describe-tasks)
    if printf '%s\n' "$*" | grep -q 'migration-1'; then
      printf '0\n'
    else
      printf '%s\n' '{"failures":[],"tasks":[{"lastStatus":"RUNNING","taskDefinitionArn":"arn:taskdef:control:2"}]}'
    fi
    ;;
  ecs:describe-task-definition)
    if [ "${MOCK_BUILD_POLICY_MISSING_AMI:-0}" = "1" ]; then
      worker_groups='[{"id":"run","ami_ids":["ami-0123456789abcdef0","ami-0bbbbbbbbbbbbbbbb"]},{"id":"build","ami_ids":["ami-0123456789abcdef0"]}]'
    else
      worker_groups='[{"id":"run","ami_ids":["ami-0123456789abcdef0","ami-0bbbbbbbbbbbbbbbb"]},{"id":"build","ami_ids":["ami-0123456789abcdef0","ami-0bbbbbbbbbbbbbbbb"]}]'
    fi
    jq -cn --arg worker_groups "$worker_groups" \
      '{taskDefinition:{containerDefinitions:[{name:"control",environment:[{name:"HELMR_WORKER_GROUPS",value:$worker_groups}]}]}}'
    ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$rollout_bin/aws"
printf '[]\n' >"$applied_allowed_amis"

WORKER_AMI_ID=ami-0bbbbbbbbbbbbbbbb \
  DEV_TFVARS="$tfvars" \
  TF_BIN="$mock_tf" \
  MOCK_APPLIED_ALLOWED_AMIS="$applied_allowed_amis" \
  STATE_DIR="$rollout_state" \
  PATH="$rollout_bin:$PATH" \
  "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"
assert_contains "$tfvars" 'worker_ami_id = "ami-0123456789abcdef0"' "worker AMI cannot advance before staging apply"
assert_contains "$stderr" "apply once, then rerun dev-worker-tfvars" "unapplied stage remains blocked"

printf '["ami-0123456789abcdef0", "ami-0bbbbbbbbbbbbbbbb"]\n' >"$applied_allowed_amis"
if MOCK_APPLY_FAIL=1 \
  DEV_TFVARS="$tfvars" \
  TF_BIN="$mock_tf" \
  MOCK_APPLIED_ALLOWED_AMIS="$applied_allowed_amis" \
  STATE_DIR="$rollout_state" \
  PATH="$rollout_bin:$PATH" \
  "$script" dev-apply >"$stdout" 2>"$stderr"; then
  fail "failed dev apply must not write a stable deployment marker"
fi
[ ! -e "$rollout_state/dev-apply-success.json" ] || fail "failed dev apply marker"

if MOCK_BUILD_POLICY_MISSING_AMI=1 \
  DEV_TFVARS="$tfvars" \
  TF_BIN="$mock_tf" \
  MOCK_APPLIED_ALLOWED_AMIS="$applied_allowed_amis" \
  STATE_DIR="$rollout_state" \
  PATH="$rollout_bin:$PATH" \
  "$script" dev-apply >"$stdout" 2>"$stderr"; then
  fail "dev apply marker must require every worker group to accept the staged AMI"
fi
[ ! -e "$rollout_state/dev-apply-success.json" ] || fail "mismatched worker group policy marker"

WORKER_AMI_ID=ami-0bbbbbbbbbbbbbbbb \
  DEV_TFVARS="$tfvars" \
  TF_BIN="$mock_tf" \
  MOCK_APPLIED_ALLOWED_AMIS="$applied_allowed_amis" \
  STATE_DIR="$rollout_state" \
  PATH="$rollout_bin:$PATH" \
  "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"
assert_contains "$tfvars" 'worker_ami_id = "ami-0123456789abcdef0"' "worker AMI cannot advance without successful apply marker"

if ! MOCK_APPLY_FAIL=0 \
  DEV_TFVARS="$tfvars" \
  TF_BIN="$mock_tf" \
  MOCK_APPLIED_ALLOWED_AMIS="$applied_allowed_amis" \
  STATE_DIR="$rollout_state" \
  PATH="$rollout_bin:$PATH" \
  "$script" dev-apply >"$stdout" 2>"$stderr"; then
  cat "$stderr" >&2
  fail "successful dev apply should record stable control policy"
fi
[ -f "$rollout_state/dev-apply-success.json" ] || fail "successful dev apply marker"

rm -f "$rollout_state/dev-apply-success.json"
transient_deployment_state="$tmp/transient-deployment-state"
if ! MOCK_TRANSIENT_DEPLOYMENT_STATE="$transient_deployment_state" \
  DEV_CONTROL_STABILITY_POLL_SECONDS=0 \
  DEV_TFVARS="$tfvars" \
  TF_BIN="$mock_tf" \
  MOCK_APPLIED_ALLOWED_AMIS="$applied_allowed_amis" \
  STATE_DIR="$rollout_state" \
  PATH="$rollout_bin:$PATH" \
  "$script" dev-apply >"$stdout" 2>"$stderr"; then
  cat "$stderr" >&2
  fail "dev apply should wait for the draining control deployment to disappear"
fi
[ -f "$rollout_state/dev-apply-success.json" ] || fail "stable marker after transient control deployment"

WORKER_AMI_ID=ami-0bbbbbbbbbbbbbbbb \
  DEV_TFVARS="$tfvars" \
  TF_BIN="$mock_tf" \
  MOCK_APPLIED_ALLOWED_AMIS="$applied_allowed_amis" \
  STATE_DIR="$rollout_state" \
  PATH="$rollout_bin:$PATH" \
  "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"
assert_contains "$tfvars" 'worker_ami_id = "ami-0bbbbbbbbbbbbbbbb"' "new worker AMI after applied allowlist stage"
assert_contains "$tfvars" 'worker_allowed_ami_ids = ["ami-0123456789abcdef0","ami-0bbbbbbbbbbbbbbbb"]' "rolling worker AMI overlap"

WORKER_AMI_ID=ami-0bbbbbbbbbbbbbbbb \
  DEV_TFVARS="$tfvars" \
  TF_BIN="$mock_tf" \
  MOCK_APPLIED_ALLOWED_AMIS="$applied_allowed_amis" \
  STATE_DIR="$rollout_state" \
  PATH="$rollout_bin:$PATH" \
  "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"
assert_contains "$tfvars" 'worker_allowed_ami_ids = ["ami-0123456789abcdef0"]' "repeated worker update preserves AMI overlap"

if WORKER_AMI_ID=ami-0cccccccccccccccc \
  DEV_WORKER_ALLOWED_AMI_IDS='["not-an-ami"]' \
  DEV_TFVARS="$tfvars" \
  TF_BIN="$mock_tf" \
  MOCK_APPLIED_ALLOWED_AMIS="$applied_allowed_amis" \
  STATE_DIR="$rollout_state" \
  PATH="$rollout_bin:$PATH" \
  "$script" dev-worker-tfvars >"$stdout" 2>"$stderr"; then
  fail "dev-worker-tfvars should reject invalid AMI overlap"
fi
assert_contains "$stderr" "must be a JSON array of AWS AMI IDs" "invalid worker AMI overlap guard"

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
control_url="https://stale.example.com"
worker_control_url="https://worker-stale.example.com"
enable_private_control_dns=true
create_worker=false
EOF
DEV_TFVARS="$tfvars" \
  AWS_REGION=us-west-2 \
  DEV_CONTROL_IMAGE=123456789012.dkr.ecr.us-west-2.amazonaws.com/helmr-control:test \
  DEV_PUBLIC_URL=https://replacement.example.com \
  DEV_GITHUB_OAUTH_CLIENT_ID=Iv1.example \
  DEV_BOOTSTRAP_OWNER_EMAIL=owner@example.com \
  DEV_CLICKHOUSE_URL=https://example.clickhouse.cloud:8443 \
  DEV_CLICKHOUSE_PASSWORD_SECRET_ARN=arn:aws:secretsmanager:us-west-2:123456789012:secret:clickhouse \
  "$script" dev-control-tfvars >"$stdout" 2>"$stderr"
assert_contains "$tfvars" 'public_url = "https://replacement.example.com"' "compact tfvar replacement"
assert_tfvar_count "$tfvars" public_url 1 "compact tfvar replacement should not duplicate"
assert_tfvar_count "$tfvars" control_url 0 "compact control_url removal"
assert_tfvar_count "$tfvars" worker_control_url 0 "compact worker_control_url removal"
assert_tfvar_count "$tfvars" enable_private_control_dns 0 "compact enable_private_control_dns removal"
assert_not_contains "$tfvars" "https://old.example.com" "compact old value removal"
assert_contains "$tfvars" 'worker_fleet_controller = {}' "control mode has no worker fleet policy"

cat >"$tfvars" <<'EOF'
aws_region="us-west-2"
name="worker-smoke"
public_url="https://worker.example.com"
create_worker=true
worker_fleet_controller={run_max_workers=1}
EOF
worker_tfvars_before="$(sha256_stdin <"$tfvars")"
if DEV_TFVARS="$tfvars" \
  AWS_REGION=us-west-2 \
  DEV_CONTROL_IMAGE=123456789012.dkr.ecr.us-west-2.amazonaws.com/helmr-control:test \
  DEV_PUBLIC_URL=https://replacement.example.com \
  DEV_GITHUB_OAUTH_CLIENT_ID=Iv1.example \
  "$script" dev-control-tfvars >"$stdout" 2>"$stderr"; then
  fail "dev-control-tfvars should reject removing active worker fleets"
fi
assert_contains "$stderr" "cannot remove active worker fleets" "active worker guard"
[ "$(sha256_stdin <"$tfvars")" = "$worker_tfvars_before" ] || fail "active worker refusal must leave tfvars unchanged"

mkdir -p "$tmp/bin"
cat >"$tmp/bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

cat "$MOCK_IMAGE_JSON"
EOF
chmod +x "$tmp/bin/aws"

state_dir="$tmp/state"
mkdir -p "$state_dir"
MOCK_IMAGE_JSON="$tmp/image-missing-region.json"
cat >"$MOCK_IMAGE_JSON" <<'JSON'
{
  "image": {
    "state": {
      "status": "AVAILABLE"
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

if AWS_REGION=us-west-2 STATE_DIR="$state_dir" MOCK_IMAGE_JSON="$MOCK_IMAGE_JSON" PATH="$tmp/bin:$PATH" "$script" worker-image-wait arn:aws:imagebuilder:us-west-2:123456789012:image/example/1.0.0/1 >"$stdout" 2>"$stderr"; then
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

AWS_REGION=us-west-2 STATE_DIR="$state_dir" MOCK_IMAGE_JSON="$MOCK_IMAGE_JSON" PATH="$tmp/bin:$PATH" "$script" worker-image-wait arn:aws:imagebuilder:us-west-2:123456789012:image/example/1.0.0/1 >"$stdout" 2>"$stderr"
assert_equal "ami-0bbbbbbbbbbbbbbbb" "$(cat "$stdout")" "worker-image-wait current region AMI"
assert_equal "ami-0bbbbbbbbbbbbbbbb" "$(cat "$state_dir/worker-ami-id")" "recorded worker AMI"

destroy_bin="$tmp/destroy-bin"
destroy_log="$tmp/destroy.log"
mkdir -p "$destroy_bin"
cat >"$destroy_bin/tofu" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
cat >"$destroy_bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$MOCK_DESTROY_LOG"
service="${1:-}"
operation="${2:-}"
case "$service:$operation" in
  sts:get-caller-identity)
    printf '123456789012\n'
    ;;
  autoscaling:describe-auto-scaling-groups)
    if [ "${MOCK_ASG_DESCRIBE_FAIL:-0}" = "1" ] || { [ "${MOCK_POST_STOP_DESCRIBE_FAIL:-0}" = "1" ] && grep -q 'ecs update-service .*--desired-count 0' "$MOCK_DESTROY_LOG"; }; then
      exit 42
    fi
    asg=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --auto-scaling-group-names) asg="${2:-}"; shift 2 ;;
        *) shift ;;
      esac
    done
    if [ "${MOCK_ACTIVE_ASG:-0}" = "1" ] || { [ "${MOCK_POST_STOP_ACTIVE:-0}" = "1" ] && grep -q 'ecs update-service .*--desired-count 0' "$MOCK_DESTROY_LOG"; }; then
      printf '{"AutoScalingGroups":[{"AutoScalingGroupName":"%s","DesiredCapacity":1,"Instances":[{"InstanceId":"i-active","LifecycleState":"InService","ProtectedFromScaleIn":true}]}]}\n' "$asg"
    else
      printf '{"AutoScalingGroups":[{"AutoScalingGroupName":"%s","DesiredCapacity":0,"Instances":[]}]}\n' "$asg"
    fi
    ;;
  ecs:describe-services)
    printf '%s\n' '{"failures":[],"services":[{"status":"ACTIVE","desiredCount":1,"runningCount":1}]}'
    ;;
  rds:describe-db-instances)
    printf 'False\n'
    ;;
  ecs:update-service|ecs:wait)
    ;;
  ecr:describe-repositories|s3api:head-bucket)
    exit 1
    ;;
  *)
    exit 1
    ;;
esac
EOF
chmod +x "$destroy_bin/aws" "$destroy_bin/tofu"
MOCK_DESTROY_LOG="$destroy_log" \
  DEV_NAME=Split-Smoke \
  STATE_DIR="$tmp/destroy-state" \
  TF_BIN="$destroy_bin/tofu" \
  PATH="$destroy_bin:$PATH" \
  "$script" dev-destroy-prepare >"$stdout" 2>"$stderr"
assert_contains "$destroy_log" "autoscaling describe-auto-scaling-groups --region us-east-1 --auto-scaling-group-names split-smoke-run-worker" "run worker destroy preparation"
assert_contains "$destroy_log" "ecs update-service --region us-east-1 --cluster split-smoke-control --service dispatcher --desired-count 0" "fleet controller stopped after worker drain proof"
assert_contains "$destroy_log" "autoscaling describe-auto-scaling-groups --region us-east-1 --auto-scaling-group-names split-smoke-build-worker" "build worker destroy preparation"
assert_not_contains "$destroy_log" "complete-lifecycle-action" "destroy must not bypass worker drain proof"
assert_not_contains "$destroy_log" "split-smoke-worker" "removed shared worker compatibility name"
assert_contains "$destroy_log" "rds describe-db-instances --region us-east-1 --db-instance-identifier split-smoke-postgres" "normalized database cleanup name"
assert_contains "$destroy_log" "ecr describe-repositories --region us-east-1 --repository-names split-smoke/control" "normalized repository cleanup name"
assert_contains "$destroy_log" "s3api head-bucket --bucket split-smoke-123456789012-us-east-1-cas" "normalized CAS cleanup name"

: >"$destroy_log"
if MOCK_ACTIVE_ASG=1 \
  DEV_DESTROY_WORKER_DRAIN_TIMEOUT_SECONDS=0 \
  MOCK_DESTROY_LOG="$destroy_log" \
  DEV_NAME=Split-Smoke \
  STATE_DIR="$tmp/destroy-state" \
  TF_BIN="$destroy_bin/tofu" \
  PATH="$destroy_bin:$PATH" \
  "$script" dev-destroy-prepare >"$stdout" 2>"$stderr"; then
  fail "dev-destroy-prepare should refuse active protected workers that have not drained"
fi
assert_contains "$destroy_log" "ecs describe-services --region us-east-1 --cluster split-smoke-control --services dispatcher" "active worker drain requires fleet controller"
assert_not_contains "$destroy_log" "ecs update-service" "fleet controller remains running until workers reach zero"
assert_contains "$stderr" "worker fleets did not drain to zero before destroy" "active protected worker drain guard"

: >"$destroy_log"
if MOCK_POST_STOP_ACTIVE=1 \
  MOCK_DESTROY_LOG="$destroy_log" \
  DEV_NAME=Split-Smoke \
  STATE_DIR="$tmp/destroy-state" \
  TF_BIN="$destroy_bin/tofu" \
  PATH="$destroy_bin:$PATH" \
  "$script" dev-destroy-prepare >"$stdout" 2>"$stderr"; then
  fail "dev-destroy-prepare should restore the controller if capacity reappears during shutdown"
fi
assert_contains "$destroy_log" "ecs update-service --region us-east-1 --cluster split-smoke-control --service dispatcher --desired-count 0" "fleet controller stop attempted after zero proof"
assert_contains "$destroy_log" "ecs update-service --region us-east-1 --cluster split-smoke-control --service dispatcher --desired-count 1" "fleet controller restored after post-stop race"
assert_contains "$stderr" "dispatcher was restored so the normal drain path can finish" "post-stop worker race guard"

: >"$destroy_log"
if MOCK_POST_STOP_DESCRIBE_FAIL=1 \
  MOCK_DESTROY_LOG="$destroy_log" \
  DEV_NAME=Split-Smoke \
  STATE_DIR="$tmp/destroy-state" \
  TF_BIN="$destroy_bin/tofu" \
  PATH="$destroy_bin:$PATH" \
  "$script" dev-destroy-prepare >"$stdout" 2>"$stderr"; then
  fail "dev-destroy-prepare should restore the controller if post-stop zero proof is unavailable"
fi
assert_contains "$destroy_log" "ecs update-service --region us-east-1 --cluster split-smoke-control --service dispatcher --desired-count 1" "fleet controller restored after post-stop API failure"
assert_contains "$stderr" "worker zero could not be proved after stopping the fleet controller; dispatcher was restored" "post-stop zero proof failure guard"

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
