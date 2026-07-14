#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
script="${repo_root}/dev/aws/run-validation-campaign.sh"

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
product="${tmp}/helmr"
ops="${tmp}/ops"
state_root="${tmp}/state"
mkdir -p "${product}/dev/workflows/tasks/smoke" "${product}/dev/aws/validation-cases" "${ops}/docs/validation"

for repo in "${product}" "${ops}"; do
  git -C "${repo}" init -q
  git -C "${repo}" config user.email test@example.com
  git -C "${repo}" config user.name test
  git -C "${repo}" config commit.gpgsign false
done

printf 'export const task = { id: "runtime-smoke" };\n' >"${product}/dev/workflows/tasks/smoke/runtime.ts"
cat >"${product}/dev/aws/validation-cases/test.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
jq -n --argjson checks "$(jq -c '.producer.checks' <<<"${HELMR_VALIDATION_CASE}")" \
  '{schema:"helmrdotdev.validation-case-source-result.v1",status:"passed",reason:null,checks:[$checks[]|{id:.,status:"passed"}]}' \
  >"${HELMR_VALIDATION_CASE_RESULT_FILE}"
EOF
chmod +x "${product}/dev/aws/validation-cases/test.sh"
cp "${repo_root}/dev/aws/run-auth-readiness.sh" "${product}/dev/aws/run-auth-readiness.sh"
cp "${repo_root}/dev/aws/worker-price-fixture.json" "${product}/dev/aws/worker-price-fixture.json"
git -C "${product}" add .
git -C "${product}" commit -qm fixture
source_commit="$(git -C "${product}" rev-parse HEAD)"
fixture_tree="$(git -C "${product}" rev-parse HEAD:dev/workflows)"
if command -v sha256sum >/dev/null 2>&1; then
  harness_sha="$(sha256sum "${script}" | awk '{print $1}')"
else
  harness_sha="$(shasum -a 256 "${script}" | awk '{print $1}')"
fi
if command -v sha256sum >/dev/null 2>&1; then
  producer_sha="$(sha256sum "${product}/dev/aws/validation-cases/test.sh" | awk '{print $1}')"
else
  producer_sha="$(shasum -a 256 "${product}/dev/aws/validation-cases/test.sh" | awk '{print $1}')"
fi
if command -v sha256sum >/dev/null 2>&1; then
  build_payload_sha="$(printf '%s\n' '{"scenario":"build-placement","expectedEnvironment":"staging","smokeCase":"runtime"}' | jq -cS . | sha256sum | awk '{print $1}')"
  run_payload_sha="$(printf '%s\n' '{"scenario":"run-placement","expectedEnvironment":"staging","smokeCase":"session-continuation"}' | jq -cS . | sha256sum | awk '{print $1}')"
else
  build_payload_sha="$(printf '%s\n' '{"scenario":"build-placement","expectedEnvironment":"staging","smokeCase":"runtime"}' | jq -cS . | shasum -a 256 | awk '{print $1}')"
  run_payload_sha="$(printf '%s\n' '{"scenario":"run-placement","expectedEnvironment":"staging","smokeCase":"session-continuation"}' | jq -cS . | shasum -a 256 | awk '{print $1}')"
fi
control_tfvars_fixture="${tmp}/control.tfvars"
worker_tfvars_fixture="${tmp}/worker.tfvars"
cat >"${control_tfvars_fixture}" <<'EOF'
create_worker = false
name = "managed-worker"
aws_region = "us-east-1"
enable_nat_gateway = false
control_image = "000000000000.dkr.ecr.us-east-1.amazonaws.com/helmr/control@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
worker_ami_id = "ami-0123456789abcdef0"
worker_instance_type = "c8i.xlarge"
build_worker_instance_type = null
worker_max_size = 1
build_worker_max_size = 1
worker_fleet_controller = {}
EOF
cat >"${worker_tfvars_fixture}" <<'EOF'
create_worker = true
name = "managed-worker"
aws_region = "us-east-1"
enable_nat_gateway = true
control_image = "000000000000.dkr.ecr.us-east-1.amazonaws.com/helmr/control@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
worker_ami_id = "ami-0123456789abcdef0"
worker_instance_type = "c8i.xlarge"
build_worker_instance_type = null
worker_max_size = 1
build_worker_max_size = 1
worker_fleet_controller = {"run_warm_workers":0,"build_warm_workers":0,"run_max_workers":1,"build_max_workers":1,"max_scale_out_per_cycle":1,"max_pending_workers":1,"emergency_stop":false}
EOF
if command -v sha256sum >/dev/null 2>&1; then
  control_tfvars_sha="$(sha256sum "${control_tfvars_fixture}" | awk '{print $1}')"
  worker_tfvars_sha="$(sha256sum "${worker_tfvars_fixture}" | awk '{print $1}')"
else
  control_tfvars_sha="$(shasum -a 256 "${control_tfvars_fixture}" | awk '{print $1}')"
  worker_tfvars_sha="$(shasum -a 256 "${worker_tfvars_fixture}" | awk '{print $1}')"
fi
manifest="${ops}/docs/validation/managed-worker-campaign.json"

jq -n \
  --arg source_commit "${source_commit}" \
  --arg fixture_tree "${fixture_tree}" \
  --arg harness_sha "${harness_sha}" \
  --arg build_payload_sha "${build_payload_sha}" \
  --arg run_payload_sha "${run_payload_sha}" \
  --arg control_tfvars_sha "${control_tfvars_sha}" \
  --arg worker_tfvars_sha "${worker_tfvars_sha}" \
  --arg producer_sha "${producer_sha}" '
  {
    schema:"helmrdotdev.aws-validation-campaign.v1",
    governance:{repo:"ops"},
    source:{repo:"helmr",commit:$source_commit},
    harness:{version:1,sha256:$harness_sha},
    artifacts:{control_image_repository:"helmr/control",control_image_digest:"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",control_tfvars_sha256:$control_tfvars_sha,worker_tfvars_sha256:$worker_tfvars_sha,worker_ami_id:"ami-0123456789abcdef0",worker_instance_type:"c8i.xlarge",build_worker_instance_type:"c8i.xlarge",worker_price_fixture_sha256:"8b4b97a437b6f9a5f23d87e38538c2e86367eb7c6bec54770adac0b7512b2400",worker_microusd_per_hour:200571},
    environment:{provider:"aws",region:"us-east-1",dev_name:"managed-worker",state_key:"dev/managed-worker.tfstate",account_id_env:"AWS_ACCOUNT_ID"},
    workload:{
      fixtures_root:"dev/workflows",fixture_tree:$fixture_tree,project:"helmr",environments:["staging","production"],
      cases:[
        {id:"build-on-build-worker",category:"build",task:"runtime-smoke",payload:{scenario:"build-placement",expectedEnvironment:"staging",smokeCase:"runtime"},payload_sha256:$build_payload_sha,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["build-completed","build-group-only"]},repetitions:1},
        {id:"run-on-run-worker",category:"run",task:"runtime-smoke",payload:{scenario:"run-placement",expectedEnvironment:"staging",smokeCase:"session-continuation"},payload_sha256:$run_payload_sha,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["run-completed","run-group-only"]},repetitions:1},
        {id:"build-failure-isolation",category:"build_failure_isolation",task:null,payload:null,payload_sha256:null,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["run-unaffected"]},repetitions:1},
        {id:"worker-restart",category:"worker_restart",task:null,payload:null,payload_sha256:null,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["authority-recovered"]},repetitions:1},
        {id:"identity-fencing",category:"identity_fencing",task:null,payload:null,payload_sha256:null,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["stale-epoch-rejected"]},repetitions:1},
        {id:"queue-preservation",category:"queue_preservation",task:null,payload:null,payload_sha256:null,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["queue-conserved"]},repetitions:1},
        {id:"protected-drain",category:"protected_drain",task:null,payload:null,payload_sha256:null,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["drain-before-termination"]},repetitions:1},
        {id:"provider-loss",category:"provider_loss",task:null,payload:null,payload_sha256:null,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["capacity-deficit-visible"]},repetitions:1},
        {id:"final-zero",category:"final_zero",task:null,payload:null,payload_sha256:null,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["workers-zero"]},repetitions:1}
      ]
    },
    cost_guard:{run_worker_max:1,build_worker_max:1,nat_gateway_max:1,max_bundle_bytes:52428800},
    evidence:{bucket_output:"source_artifact_bucket_name",claim_prefix:"helmr/validation-claims",prefix:"helmr/validation-evidence",namespace:"managed-worker-20260714-a",retention_days:30},
    retries:{infrastructure_max_attempts:2,workload_attempts:1},
    stages:["preflight","control_up","awaiting_human","auth_ready","worker_up","workload","pre_shutdown_publish","cleanup","closed","post_shutdown_publish"]
  }' >"${manifest}"
git -C "${ops}" add .
git -C "${ops}" commit -qm manifest

campaign() {
  DEV_NAME="managed-worker" \
  STATE_KEY="dev/managed-worker.tfstate" \
  HELMR_VALIDATION_PRODUCT_ROOT="${product}" \
  HELMR_VALIDATION_STATE_ROOT="${state_root}" \
    "${script}" "$@"
}

campaign validate "${manifest}"
campaign init "${manifest}" >/dev/null
[ "$(campaign status "${manifest}" | jq -r '.status')" = "ready" ] || fail "initialized campaign status"

result="${tmp}/result.json"

jq '.unexpected=true' "${manifest}" >"${tmp}/invalid.json"
if campaign validate "${tmp}/invalid.json" >/dev/null 2>&1; then
  fail "unknown manifest fields should fail"
fi

if campaign start "${manifest}" preflight >"${tmp}/stdout" 2>"${tmp}/stderr"; then
  fail "formal stages should require an evidence claim"
fi
grep -Fq 'evidence namespace must be claimed' "${tmp}/stderr" || fail "claim gate reason"

manifest_b="${ops}/docs/validation/managed-worker-campaign-b.json"
jq '.evidence.namespace="managed-worker-20260714-b"' "${manifest}" >"${manifest_b}"
git -C "${ops}" add .
git -C "${ops}" commit -qm second-manifest

mkdir -p "${tmp}/bin" "${tmp}/s3"
cat >"${tmp}/bin/tofu" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *"output -raw source_artifact_bucket_name"*) printf 'artifact-bucket\n' ;;
  *"output -raw source_artifact_kms_key_arn"*) printf 'arn:aws:kms:us-east-1:000000000000:key/test\n' ;;
  *"output -json"*)
    if grep -q '^create_worker = true$' "${DEV_TFVARS}"; then run='"managed-worker-run-worker"'; build='"managed-worker-build-worker"'; nat='"nat-0123456789abcdef0"'; else run=null; build=null; nat=null; fi
    printf '{"control_cluster_name":{"value":"managed-worker-control"},"control_service_name":{"value":"control"},"dispatcher_service_name":{"value":"dispatcher"},"postgres_identifier":{"value":"helmr-db"},"worker_autoscaling_group_name":{"value":%s},"build_worker_autoscaling_group_name":{"value":%s},"worker_protect_from_scale_in":{"value":true},"build_worker_protect_from_scale_in":{"value":true},"nat_gateway_id":{"value":%s}}\n' "${run}" "${build}" "${nat}"
    ;;
  *"state list"*) exit 0 ;;
  *"workspace show"*) printf 'default\n' ;;
  *) exit 2 ;;
esac
EOF
cat >"${tmp}/bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
command_line="$*"
case "${command_line}" in
  *"get-bucket-versioning"*) printf '{"Status":"Enabled"}\n' ;;
  *"sts get-caller-identity"*) printf '000000000000\n' ;;
  *"get-public-access-block"*) printf '{"PublicAccessBlockConfiguration":{"BlockPublicAcls":true,"IgnorePublicAcls":true,"BlockPublicPolicy":true,"RestrictPublicBuckets":true}}\n' ;;
  *"get-bucket-encryption"*) printf '{"ServerSideEncryptionConfiguration":{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"aws:kms","KMSMasterKeyID":"arn:aws:kms:us-east-1:000000000000:key/test"}}]}}\n' ;;
  *"get-bucket-lifecycle-configuration"*) printf '{"Rules":[{"ID":"expire-validation-evidence","Status":"Enabled","Filter":{"Prefix":"helmr/validation-evidence/"},"Expiration":{"Days":30},"NoncurrentVersionExpiration":{"NoncurrentDays":30}}]}\n' ;;
  *"ecs describe-services"*) if [ -e "${MOCK_UNHEALTHY_CONTROL_FILE}" ]; then running=0; else running=1; fi; printf '{"failures":[],"services":[{"serviceName":"control","desiredCount":1,"runningCount":%s,"pendingCount":0,"taskDefinition":"control-task","deployments":[{"status":"PRIMARY","rolloutState":"COMPLETED"}]},{"serviceName":"dispatcher","desiredCount":1,"runningCount":%s,"pendingCount":0,"taskDefinition":"dispatcher-task","deployments":[{"status":"PRIMARY","rolloutState":"COMPLETED"}]}]}\n' "${running}" "${running}" ;;
  *"ecs describe-task-definition"*) if [[ "${command_line}" == *dispatcher-task* ]]; then name=dispatcher; else name=control; fi; printf '{"taskDefinition":{"containerDefinitions":[{"name":"%s","image":"000000000000.dkr.ecr.us-east-1.amazonaws.com/helmr/control@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}}\n' "${name}" ;;
  *"rds describe-db-instances"*"--db-instance-identifier"*) printf '{"DBInstances":[{"DBInstanceIdentifier":"helmr-db","DBInstanceStatus":"available","DBInstanceClass":"db.t4g.micro","EngineVersion":"16.3","InstanceCreateTime":"2026-07-14T00:00:00Z"}]}\n' ;;
  *"rds describe-db-instances"*) printf '{"DBInstances":[]}\n' ;;
  *"elasticache describe-replication-groups"*"--replication-group-id"*) printf '{"ReplicationGroups":[{"ReplicationGroupId":"managed-worker-dispatch","Status":"available","Engine":"valkey","MemberClusters":["cache-1"],"ReplicationGroupCreateTime":"2026-07-14T00:00:00Z"}]}\n' ;;
  *"elasticache describe-replication-groups"*) printf '{"ReplicationGroups":[]}\n' ;;
  *"autoscaling describe-auto-scaling-groups"*)
    if [[ "${command_line}" == *"managed-worker-run-worker"* ]]; then id=lt-0123456789abcdef0; elif [[ "${command_line}" == *"managed-worker-build-worker"* ]]; then id=lt-1123456789abcdef0; else printf '{"AutoScalingGroups":[]}\n'; exit 0; fi
    if [ -e "${MOCK_DESTROYED_FILE}" ]; then printf '{"AutoScalingGroups":[]}\n'; elif grep -q '^create_worker = true$' "${DEV_TFVARS}"; then if [ -e "${MOCK_ASG_DRIFT_FILE}" ]; then max=2; else max=1; fi; printf '{"AutoScalingGroups":[{"AutoScalingGroupName":"mock","MinSize":0,"MaxSize":%s,"DesiredCapacity":0,"CreatedTime":"2026-07-14T00:10:00Z","Instances":[],"LaunchTemplate":{"LaunchTemplateId":"%s","Version":"1"}}]}\n' "${max}" "${id}"; else printf '{"AutoScalingGroups":[]}\n'; fi
    ;;
  *"autoscaling describe-lifecycle-hooks"*) if [ -e "${MOCK_HOOK_DRIFT_FILE}" ]; then printf '{"LifecycleHooks":[]}\n'; else printf '{"LifecycleHooks":[{"LifecycleTransition":"autoscaling:EC2_INSTANCE_LAUNCHING","DefaultResult":"ABANDON","HeartbeatTimeout":600},{"LifecycleTransition":"autoscaling:EC2_INSTANCE_TERMINATING","DefaultResult":"CONTINUE","HeartbeatTimeout":1800}]}\n'; fi ;;
  *"ec2 describe-nat-gateways"*"--nat-gateway-ids"*) printf '{"NatGateways":[{"NatGatewayId":"nat-0123456789abcdef0","State":"available","CreateTime":"2026-07-14T00:10:00Z"}]}\n' ;;
  *"ec2 describe-nat-gateways"*) printf '{"NatGateways":[]}\n' ;;
  *"ec2 describe-launch-template-versions"*) printf '{"LaunchTemplateVersions":[{"LaunchTemplateData":{"ImageId":"ami-0123456789abcdef0","InstanceType":"c8i.xlarge"}}]}\n' ;;
  *"ec2 describe-instances"*) printf '{"Reservations":[]}\n' ;;
  *"ecs describe-clusters"*) printf '{"clusters":[],"failures":[{"arn":"managed-worker-control"}]}\n' ;;
  *"ecs list-services"*) printf '{"serviceArns":[]}\n' ;;
  *"resourcegroupstaggingapi get-resources"*) printf '{"ResourceTagMappingList":[]}\n' ;;
  *"elbv2 describe-load-balancers"*) printf '{"LoadBalancers":[]}\n' ;;
  *"elbv2 describe-target-groups"*) printf '{"TargetGroups":[]}\n' ;;
  *"ec2 describe-vpc-endpoints"*) printf '{"VpcEndpoints":[]}\n' ;;
  *"cloudwatch get-metric-statistics"*) printf '{"Datapoints":[{"Sum":1024}]}\n' ;;
  *"put-object"*)
    key=""; body=""; metadata=""; checksum=""; kms=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --key) key=$2; shift 2 ;;
        --body) body=$2; shift 2 ;;
        --metadata) metadata=$2; shift 2 ;;
        --checksum-sha256) checksum=$2; shift 2 ;;
        --ssekms-key-id) kms=$2; shift 2 ;;
        *) shift ;;
      esac
    done
    marker="${MOCK_S3_DIR}/$(printf '%s' "${key}" | tr '/' '_')"
    if [[ "${command_line}" == *"--if-none-match *"* ]] && [ -e "${marker}" ]; then
      exit 1
    fi
    cp "${body}" "${marker}"
    printf '%s\n' "${metadata#sha256=}" >"${marker}.sha"
    printf '%s\n' "${checksum}" >"${marker}.checksum"
    printf '%s\n' "${kms}" >"${marker}.kms"
    wc -c <"${body}" | tr -d ' ' >"${marker}.bytes"
    printf '{"VersionId":"v1","ChecksumSHA256":"%s","ServerSideEncryption":"aws:kms"}\n' "${checksum}"
    ;;
  *"head-object"*)
    key=""
    while [ "$#" -gt 0 ]; do
      case "$1" in --key) key=$2; shift 2 ;; *) shift ;; esac
    done
    marker="${MOCK_S3_DIR}/$(printf '%s' "${key}" | tr '/' '_')"
    sha="$(cat "${marker}.sha")"
    checksum="$(cat "${marker}.checksum")"
    kms="$(cat "${marker}.kms")"
    bytes="$(cat "${marker}.bytes")"
    printf '{"VersionId":"v1","ChecksumSHA256":"%s","Metadata":{"sha256":"%s"},"ServerSideEncryption":"aws:kms","SSEKMSKeyId":"%s","ContentLength":%s}\n' "${checksum}" "${sha}" "${kms}" "${bytes}"
    ;;
  *) exit 2 ;;
esac
EOF
cat >"${tmp}/bin/helmr" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *"project list --json"*) printf '{"projects":[{"slug":"helmr"}]}\n' ;;
  *"env list --project helmr --json"*) printf '[{"slug":"staging"},{"slug":"production"}]\n' ;;
  *) exit 2 ;;
esac
EOF
chmod +x "${tmp}/bin/tofu" "${tmp}/bin/aws" "${tmp}/bin/helmr"
dev_stack="${tmp}/dev-stack"
mkdir -p "${dev_stack}/.terraform"
jq -n '{backend:{type:"s3",config:{bucket:"state-bucket",key:"dev/managed-worker.tfstate",region:"us-east-1",workspace_key_prefix:"env:"}}}' >"${dev_stack}/.terraform/terraform.tfstate"

tfvars="${tmp}/full-run-smoke.tfvars"
cp "${control_tfvars_fixture}" "${tfvars}"

campaign_b() {
  PATH="${tmp}/bin:${PATH}" \
  MOCK_S3_DIR="${tmp}/s3" \
  MOCK_DESTROYED_FILE="${tmp}/destroyed" \
  MOCK_UNHEALTHY_CONTROL_FILE="${tmp}/unhealthy-control" \
  MOCK_ASG_DRIFT_FILE="${tmp}/asg-drift" \
  MOCK_HOOK_DRIFT_FILE="${tmp}/hook-drift" \
  BOOTSTRAP_STACK="${tmp}/bootstrap" \
  DEV_STACK="${dev_stack}" \
  PRICE_FIXTURE="${repo_root}/dev/aws/worker-price-fixture.json" \
  HELMR_BIN="${tmp}/bin/helmr" \
  HELMR_API_URL="https://dev.helmr.test" \
  HELMR_AUTH_PREFLIGHT_BIN="true" \
  DEV_TFVARS="${tfvars}" \
  AWS_ACCOUNT_ID="000000000000" \
  DEV_NAME="managed-worker" \
  STATE_KEY="dev/managed-worker.tfstate" \
  HELMR_VALIDATION_PRODUCT_ROOT="${product}" \
  HELMR_VALIDATION_STATE_ROOT="${state_root}" \
    "${script}" "$@"
}

campaign_b init "${manifest_b}" >/dev/null
campaign_b claim "${manifest_b}"
if campaign_b claim "${manifest_b}" >/dev/null 2>&1; then
  fail "claimed namespace should not be reusable"
fi

lock_dir="${state_root}/managed-worker-20260714-b/.lock"
mkdir "${lock_dir}"
printf '%s\n' "$$" >"${lock_dir}/pid"
if campaign_b start "${manifest_b}" preflight >/dev/null 2>"${tmp}/stderr"; then
  fail "campaign lock should reject concurrent state mutation"
fi
grep -Fq 'locked by another process' "${tmp}/stderr" || fail "campaign lock reason"
rm -f "${lock_dir}/pid"
rmdir "${lock_dir}"

pass_preflight_result="${tmp}/preflight.json"
jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"preflight",status:"passed",reason:null,observations:{},cases:[]}' >"${pass_preflight_result}"
campaign_b start "${manifest_b}" preflight
campaign_b complete "${manifest_b}" preflight "${pass_preflight_result}"
printf 'dirty\n' >>"${product}/dev/workflows/tasks/smoke/runtime.ts"
if campaign_b run-collect "${manifest_b}" control_up -- true >"${tmp}/stdout" 2>"${tmp}/stderr"; then
  fail "forward stage should reject product drift"
fi
grep -Fq 'product checkout is dirty' "${tmp}/stderr" || fail "drift rejection reason"
git -C "${product}" checkout -q -- .

alternate_state="${tmp}/alternate-state"
HELMR_VALIDATION_PRODUCT_ROOT="${product}" \
HELMR_VALIDATION_STATE_ROOT="${alternate_state}" \
  "${script}" init "${manifest_b}" >/dev/null
if PATH="${tmp}/bin:${PATH}" MOCK_S3_DIR="${tmp}/s3" BOOTSTRAP_STACK="${tmp}/bootstrap" \
  AWS_ACCOUNT_ID="000000000000" DEV_NAME="managed-worker" STATE_KEY="dev/managed-worker.tfstate" \
  HELMR_VALIDATION_PRODUCT_ROOT="${product}" HELMR_VALIDATION_STATE_ROOT="${alternate_state}" \
  "${script}" claim "${manifest_b}" >/dev/null 2>&1; then
  fail "S3 namespace claim must be atomic across local state roots"
fi

pass_stage() {
  local stage=$1
  case "${stage}" in
    preflight|awaiting_human)
      jq -n --arg stage "${stage}" '{schema:"helmrdotdev.validation-stage-result.v1",stage:$stage,status:"passed",reason:null,observations:{},cases:[]}' >"${result}"
      ;;
    control_up)
      jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"control_up",status:"passed",reason:null,observations:{control_image:"helmr/control@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",control_desired_count:1,dispatcher_desired_count:1,nat_gateway_count:0,run_worker_count:0,build_worker_count:0,rds_instance_id:"helmr-db",valkey_replication_group_id:"helmr-cache"},cases:[]}' >"${result}"
      ;;
    auth_ready)
      jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"auth_ready",status:"passed",reason:null,observations:{project_slug:"helmr",environment_slugs:["staging","production"],authenticated_cli_probe:true,exit_code:0},cases:[]}' >"${result}"
      ;;
    worker_up)
      jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"worker_up",status:"passed",reason:null,observations:{worker_ami_id:"ami-0123456789abcdef0",worker_instance_type:"c8i.xlarge",launch_template_id:"lt-0123456789abcdef0",launch_template_version:1,run_worker_count:0,build_worker_count:0,active_nat_gateway_count:1,rds_instance_id:"helmr-db",valkey_replication_group_id:"helmr-cache"},cases:[]}' >"${result}"
      ;;
    cleanup)
      jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"cleanup",status:"passed",reason:null,observations:{active_nat_gateway_count:0,run_worker_count:0,build_worker_count:0,rds_instance_count:0,valkey_cluster_count:0,ecs_service_count:0},cases:[]}' >"${result}"
      ;;
    closed)
      jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"closed",status:"passed",reason:null,observations:{verdict:"passed",zero_resources:true},cases:[]}' >"${result}"
      ;;
  esac
  campaign_b start "${manifest_b}" "${stage}"
  campaign_b complete "${manifest_b}" "${stage}" "${result}"
}

campaign_b run-collect "${manifest_b}" control_up -- true
pass_stage awaiting_human
if campaign_b start "${manifest_b}" auth_ready >/dev/null 2>&1; then
  fail "auth readiness should require the harness-owned command"
fi
campaign_b auth "${manifest_b}"

cp "${worker_tfvars_fixture}" "${tfvars}"
sed 's/worker_max_size = 1/worker_max_size = 2/' "${tfvars}" >"${tfvars}.too-large"
mv "${tfvars}.too-large" "${tfvars}"
if campaign_b run-collect "${manifest_b}" worker_up -- true >/dev/null 2>"${tmp}/stderr"; then
  fail "worker ASG above the manifest ceiling should fail"
fi
grep -Fq 'worker tfvars differ from the frozen campaign configuration' "${tmp}/stderr" || fail "worker ceiling reason"
cp "${worker_tfvars_fixture}" "${tfvars}"
campaign_b run-collect "${manifest_b}" worker_up -- true

if campaign_b start "${manifest_b}" workload >/dev/null 2>&1; then
  fail "workload should require the harness-owned runner"
fi
printf '# drift\n' >>"${product}/dev/aws/validation-cases/test.sh"
if campaign_b workload "${manifest_b}" >/dev/null 2>"${tmp}/stderr"; then
  fail "workload should reject a drifted producer"
fi
grep -Eq 'product checkout is dirty|producer drifted' "${tmp}/stderr" || fail "producer drift reason"
git -C "${product}" checkout -q -- .
campaign_b workload "${manifest_b}"
workload_result="${state_root}/managed-worker-20260714-b/results/05-workload.json"
jq -e '(.cases | length) == 9 and all(.cases[]; .status == "passed") and .observations.nat_bytes_in_from_destination == 1024' "${workload_result}" >/dev/null ||
  fail "harness-owned workload result"
fake_publish="${tmp}/fake-publish.json"
jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"pre_shutdown_publish",status:"passed",reason:null,observations:{bytes:1,checkpoint:"pre-shutdown",checksum_sha256:"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",logical_key:"helmr/validation-evidence/managed-worker-20260714-b/pre-shutdown/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.tar.gz",sha256:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",version_id:"fake"},cases:[]}' >"${fake_publish}"
if campaign_b complete "${manifest_b}" pre_shutdown_publish "${fake_publish}" >/dev/null 2>"${tmp}/stderr"; then
  fail "direct completion must not bypass S3 publication"
fi
grep -Fq 'publish stages can only be completed by the publish command' "${tmp}/stderr" || fail "publish ownership reason"
campaign_b publish "${manifest_b}" pre-shutdown >"${tmp}/publish-output"
grep -Fq 'evidence_key=helmr/validation-evidence/managed-worker-20260714-b/pre-shutdown/' "${tmp}/publish-output" ||
  fail "pre-shutdown evidence key"

campaign_b run-collect "${manifest_b}" cleanup -- touch "${tmp}/destroyed"
printf 'post-cleanup drift\n' >>"${product}/dev/workflows/tasks/smoke/runtime.ts"
campaign_b close "${manifest_b}"
campaign_b publish "${manifest_b}" post-shutdown >/dev/null
[ -f "${state_root}/managed-worker-20260714-b/results/08-closed.json" ] || fail "closed result should precede final evidence"
post_bundle="$(find "${tmp}/s3" -maxdepth 1 -type f -name 'helmr_validation-evidence_managed-worker-20260714-b_post-shutdown_*.tar.gz' | head -1)"
[ -n "${post_bundle}" ] || fail "post-shutdown bundle should be stored"
tar -tzf "${post_bundle}" | grep -Fq 'results/08-closed.json' || fail "post-shutdown bundle should contain durable closure"
[ "$(campaign_b status "${manifest_b}" | jq -r '.status')" = "closed" ] || fail "campaign should close"
git -C "${product}" checkout -q -- .

manifest_c="${ops}/docs/validation/managed-worker-campaign-c.json"
jq '.evidence.namespace="managed-worker-20260714-c"' "${manifest}" >"${manifest_c}"
git -C "${ops}" add .
git -C "${ops}" commit -qm third-manifest
campaign_b init "${manifest_c}" >/dev/null
campaign_b claim "${manifest_c}"
missing_result="${tmp}/missing-result.json"
if campaign_b run "${manifest_c}" preflight "${missing_result}" -- false >/dev/null 2>&1; then
  fail "failed command should fail the stage"
fi
[ "$(campaign_b status "${manifest_c}" | jq -r '.status')" = "cleanup_required" ] ||
  fail "failed command should require cleanup"
[ "$(campaign_b status "${manifest_c}" | jq -r '.next_stage_index')" = "6" ] ||
  fail "failed command should preserve pre-shutdown publication"
[ "$(jq -r '.reason' "${missing_result}")" = "command_result_missing" ] ||
  fail "missing command result should be explicit"

manifest_d="${ops}/docs/validation/managed-worker-campaign-d.json"
jq '.evidence.namespace="managed-worker-20260714-d"' "${manifest}" >"${manifest_d}"
git -C "${ops}" add .
git -C "${ops}" commit -qm fourth-manifest
campaign_b init "${manifest_d}" >/dev/null
campaign_b claim "${manifest_d}"
campaign_b start "${manifest_d}" preflight
state_d="${state_root}/managed-worker-20260714-d/state.json"
jq --argjson pid "$$" '.running_pid=$pid' "${state_d}" >"${state_d}.tmp"
mv "${state_d}.tmp" "${state_d}"
if campaign_b recover "${manifest_d}" >/dev/null 2>"${tmp}/stderr"; then
  fail "recovery should not race a live stage owner"
fi
grep -Fq 'running stage owner is still alive' "${tmp}/stderr" || fail "live stage owner reason"
jq '.running_pid=null' "${state_d}" >"${state_d}.tmp"
mv "${state_d}.tmp" "${state_d}"
campaign_b recover "${manifest_d}"
[ "$(campaign_b status "${manifest_d}" | jq -r '.status')" = "cleanup_required" ] ||
  fail "recovery should reopen cleanup"
[ "$(campaign_b status "${manifest_d}" | jq -r '.next_stage_index')" = "6" ] ||
  fail "recovery should preserve pre-shutdown publication"

manifest_e="${ops}/docs/validation/managed-worker-campaign-e.json"
jq '.evidence.namespace="managed-worker-20260714-e"' "${manifest}" >"${manifest_e}"
git -C "${ops}" add .
git -C "${ops}" commit -qm fifth-manifest
campaign_b init "${manifest_e}" >/dev/null
campaign_b claim "${manifest_e}"
conflicting_result="${tmp}/conflicting-result.json"
jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"preflight",status:"passed",reason:null,observations:{},cases:[]}' >"${conflicting_result}"
if campaign_b run "${manifest_e}" preflight "${conflicting_result}" -- false >/dev/null 2>&1; then
  fail "nonzero command should fail even when its result claims passed"
fi
[ "$(jq -r '.reason' "${conflicting_result}")" = "command_result_conflict" ] ||
  fail "command/result conflict should be persisted"
[ "$(campaign_b status "${manifest_e}" | jq -r '.running_stage == null')" = "true" ] ||
  fail "command/result conflict should not strand a running stage"

printf 'ok - validation campaign tests\n'
