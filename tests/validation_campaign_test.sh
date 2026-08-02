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
smoke_state_root="${tmp}/aws-smoke"
mkdir -p "${product}/dev/workflows/tasks/smoke" "${product}/dev/aws/validation-cases" "${product}/images" "${ops}/docs/validation"
mkdir -p "${smoke_state_root}"

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
case_id="$(jq -er '.id' <<<"${HELMR_VALIDATION_CASE}")"
if [ -n "${HELMR_VALIDATION_CASE_LOG:-}" ]; then
  printf '%s\n' "${case_id}" >>"${HELMR_VALIDATION_CASE_LOG}"
fi
if [ "${case_id}" = "${HELMR_VALIDATION_FAIL_CASE:-}" ]; then
  jq -n --argjson checks "$(jq -c '.producer.checks' <<<"${HELMR_VALIDATION_CASE}")" \
    '{schema:"helmrdotdev.validation-case-source-result.v2",status:"failed",reason:"injected_failure",
      checks:[$checks[]|{id:.,status:"failed"}],
      objects:{run_ids:[],workspace_ids:[],deployment_ids:[],schedule_ids:[],token_ids:[],actor_ids:[]},
      observations:{}}' \
    >"${HELMR_VALIDATION_CASE_RESULT_FILE}"
  exit 1
fi
jq -n --argjson checks "$(jq -c '.producer.checks' <<<"${HELMR_VALIDATION_CASE}")" \
  '{schema:"helmrdotdev.validation-case-source-result.v2",status:"passed",reason:null,
    checks:[$checks[]|{id:.,status:"passed"}],
    objects:{run_ids:["019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"],workspace_ids:[],deployment_ids:[],schedule_ids:[],token_ids:[],actor_ids:[]},
    observations:{attempt:1}}' \
  >"${HELMR_VALIDATION_CASE_RESULT_FILE}"
EOF
chmod +x "${product}/dev/aws/validation-cases/test.sh"
profile="${product}/dev/aws/release-validation-profile.json"
jq -n '{
  schema:"helmrdotdev.aws-release-validation-profile.v1",
  cases:[
    {id:"build-on-build-worker",category:"build",task:"runtime-smoke",repetitions:1,timeout_seconds:60,producer:{path:"dev/aws/validation-cases/test.sh",checks:["build-completed","build-group-only"]}},
    {id:"run-on-run-worker",category:"run",task:"runtime-smoke",repetitions:1,timeout_seconds:60,producer:{path:"dev/aws/validation-cases/test.sh",checks:["run-completed","run-group-only"]}},
    {id:"build-failure-isolation",category:"build_failure_isolation",task:null,repetitions:1,timeout_seconds:60,producer:{path:"dev/aws/validation-cases/test.sh",checks:["run-unaffected"]}},
    {id:"network-deny",category:"network_deny",task:null,repetitions:1,timeout_seconds:60,producer:{path:"dev/aws/validation-cases/test.sh",checks:["deny-counter-increased"]}},
    {id:"worker-restart",category:"worker_restart",task:null,repetitions:1,timeout_seconds:60,producer:{path:"dev/aws/validation-cases/test.sh",checks:["authority-recovered"]}},
    {id:"identity-fencing",category:"identity_fencing",task:null,repetitions:1,timeout_seconds:60,producer:{path:"dev/aws/validation-cases/test.sh",checks:["old-epoch-fenced","startup-recovery-recorded"]}},
    {id:"queue-preservation",category:"queue_preservation",task:null,repetitions:1,timeout_seconds:60,producer:{path:"dev/aws/validation-cases/test.sh",checks:["queue-conserved"]}},
    {id:"protected-drain",category:"protected_drain",task:null,repetitions:1,timeout_seconds:60,producer:{path:"dev/aws/validation-cases/test.sh",checks:["drain-before-termination"]}},
    {id:"provider-loss",category:"provider_loss",task:null,repetitions:1,timeout_seconds:60,producer:{path:"dev/aws/validation-cases/test.sh",checks:["capacity-deficit-visible"]}}
  ]
}' >"${profile}"
cp "${repo_root}/dev/aws/run-auth-readiness.sh" "${product}/dev/aws/run-auth-readiness.sh"
cp "${repo_root}/dev/aws/worker-price-fixture.json" "${product}/dev/aws/worker-price-fixture.json"
cp "${repo_root}/flake.lock" "${product}/flake.lock"
cp "${repo_root}/images/control-image-build.json" "${product}/images/control-image-build.json"
git -C "${product}" add .
git -C "${product}" commit -qm fixture
source_commit="$(git -C "${product}" rev-parse HEAD)"
fixture_tree="$(git -C "${product}" rev-parse HEAD:dev/workflows)"
build_policy_digest="sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
printf '%s\n' "${build_policy_digest}" >"${smoke_state_root}/build-policy-digest"
control_base_image="$(jq -r '.baseImage' "${product}/images/control-image-build.json")"
if command -v sha256sum >/dev/null 2>&1; then
  flake_lock_sha256="$(sha256sum "${product}/flake.lock" | awk '{print $1}')"
else
  flake_lock_sha256="$(shasum -a 256 "${product}/flake.lock" | awk '{print $1}')"
fi
worker_image_build_version_arn="arn:aws:imagebuilder:us-east-1:000000000000:image/helmr-test-worker/0.1.2/1"
worker_image_recipe_arn="arn:aws:imagebuilder:us-east-1:000000000000:image-recipe/helmr-test-worker/0.1.2"
jq -cn \
  --arg base_image "${control_base_image}" \
  --arg commit "${source_commit}" \
  --arg flake_lock_sha256 "${flake_lock_sha256}" \
  '{
    buildInputs:{
      baseImage:$base_image,
      buildVersion:"",
      formatVersion:1,
      localImageId:"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      platform:"linux/amd64",
      sourceCommit:$commit,
      toolchain:{kind:"nix-flake-lock",sha256:$flake_lock_sha256}
    },
    formatVersion:1,
    image:{digest:"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",repository:"helmr-dev/control-releases"},
    sourceCommit:$commit
  }' >"${smoke_state_root}/control-image-provenance.json"
jq -cn \
  --arg build_arn "${worker_image_build_version_arn}" \
  --arg commit "${source_commit}" \
  --arg recipe_arn "${worker_image_recipe_arn}" \
  '{
    ami:{id:"ami-0123456789abcdef0",region:"us-east-1"},
    formatVersion:0,
    imageBuildVersionARN:$build_arn,
    imageRecipeARN:$recipe_arn,
    sourceCommit:$commit
  }' >"${smoke_state_root}/worker-image-provenance.json"
if command -v sha256sum >/dev/null 2>&1; then
  control_image_provenance_sha="$(sha256sum "${smoke_state_root}/control-image-provenance.json" | awk '{print $1}')"
  worker_image_provenance_sha="$(sha256sum "${smoke_state_root}/worker-image-provenance.json" | awk '{print $1}')"
else
  control_image_provenance_sha="$(shasum -a 256 "${smoke_state_root}/control-image-provenance.json" | awk '{print $1}')"
  worker_image_provenance_sha="$(shasum -a 256 "${smoke_state_root}/worker-image-provenance.json" | awk '{print $1}')"
fi
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
  run_payload_sha="$(printf '%s\n' '{"scenario":"run-placement","expectedEnvironment":"staging","smokeCase":"actor-continuation"}' | jq -cS . | sha256sum | awk '{print $1}')"
else
  build_payload_sha="$(printf '%s\n' '{"scenario":"build-placement","expectedEnvironment":"staging","smokeCase":"runtime"}' | jq -cS . | shasum -a 256 | awk '{print $1}')"
  run_payload_sha="$(printf '%s\n' '{"scenario":"run-placement","expectedEnvironment":"staging","smokeCase":"actor-continuation"}' | jq -cS . | shasum -a 256 | awk '{print $1}')"
fi
control_tfvars_fixture="${tmp}/control.tfvars"
worker_tfvars_fixture="${tmp}/worker.tfvars"
cat >"${control_tfvars_fixture}" <<'EOF'
create_worker = false
name = "managed-worker"
aws_region = "us-east-1"
enable_nat_gateway = false
control_image = "000000000000.dkr.ecr.us-east-1.amazonaws.com/helmr-dev/control-releases@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
control_image_repository_arn = "arn:aws:ecr:us-east-1:000000000000:repository/helmr-dev/control-releases"
worker_ami_id = "ami-0123456789abcdef0"
worker_instance_type = "c8i.xlarge"
build_worker_instance_type = null
worker_max_size = 1
build_worker_max_size = 1
worker_min_size = 0
build_worker_min_size = 0
worker_observation_ttl_seconds = 120
worker_launch_timeout_seconds = 900
EOF
cat >"${worker_tfvars_fixture}" <<'EOF'
create_worker = true
name = "managed-worker"
aws_region = "us-east-1"
enable_nat_gateway = true
control_image = "000000000000.dkr.ecr.us-east-1.amazonaws.com/helmr-dev/control-releases@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
control_image_repository_arn = "arn:aws:ecr:us-east-1:000000000000:repository/helmr-dev/control-releases"
worker_ami_id = "ami-0123456789abcdef0"
worker_instance_type = "c8i.xlarge"
build_worker_instance_type = null
worker_max_size = 1
build_worker_max_size = 1
worker_min_size = 0
build_worker_min_size = 0
worker_observation_ttl_seconds = 120
worker_launch_timeout_seconds = 900
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
  --arg control_image_provenance_sha "${control_image_provenance_sha}" \
  --arg worker_tfvars_sha "${worker_tfvars_sha}" \
  --arg worker_image_build_version_arn "${worker_image_build_version_arn}" \
  --arg worker_image_provenance_sha "${worker_image_provenance_sha}" \
  --arg build_policy_digest "${build_policy_digest}" \
  --arg producer_sha "${producer_sha}" '
  {
    schema:"helmrdotdev.aws-validation-campaign.v2",
    governance:{repo:"ops"},
    source:{repo:"helmr",commit:$source_commit},
    harness:{version:1,sha256:$harness_sha},
    artifacts:{build_policy_digest:$build_policy_digest,control_image_repository:"helmr-dev/control-releases",control_image_digest:"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",control_image_provenance_sha256:$control_image_provenance_sha,control_tfvars_sha256:$control_tfvars_sha,worker_tfvars_sha256:$worker_tfvars_sha,worker_ami_id:"ami-0123456789abcdef0",worker_image_build_version_arn:$worker_image_build_version_arn,worker_image_provenance_sha256:$worker_image_provenance_sha,worker_instance_type:"c8i.xlarge",build_worker_instance_type:"c8i.xlarge"},
    environment:{provider:"aws",region:"us-east-1",dev_name:"managed-worker",state_key:"dev/managed-worker.tfstate",account_id_env:"AWS_ACCOUNT_ID"},
    workload:{
      fixtures_root:"dev/workflows",fixture_tree:$fixture_tree,project:"helmr",environments:["staging","production"],
      cases:[
        {id:"build-on-build-worker",category:"build",task:"runtime-smoke",payload:{scenario:"build-placement",expectedEnvironment:"staging",smokeCase:"runtime"},payload_sha256:$build_payload_sha,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["build-completed","build-group-only"]},repetitions:1,timeout_seconds:60},
        {id:"run-on-run-worker",category:"run",task:"runtime-smoke",payload:{scenario:"run-placement",expectedEnvironment:"staging",smokeCase:"actor-continuation"},payload_sha256:$run_payload_sha,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["run-completed","run-group-only"]},repetitions:1,timeout_seconds:60},
        {id:"build-failure-isolation",category:"build_failure_isolation",task:null,payload:null,payload_sha256:null,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["run-unaffected"]},repetitions:1,timeout_seconds:60},
        {id:"network-deny",category:"network_deny",task:null,payload:null,payload_sha256:null,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["deny-counter-increased"]},repetitions:1,timeout_seconds:60},
        {id:"worker-restart",category:"worker_restart",task:null,payload:null,payload_sha256:null,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["authority-recovered"]},repetitions:1,timeout_seconds:60},
        {id:"identity-fencing",category:"identity_fencing",task:null,payload:null,payload_sha256:null,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["old-epoch-fenced","startup-recovery-recorded"]},repetitions:1,timeout_seconds:60},
        {id:"queue-preservation",category:"queue_preservation",task:null,payload:null,payload_sha256:null,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["queue-conserved"]},repetitions:1,timeout_seconds:60},
        {id:"protected-drain",category:"protected_drain",task:null,payload:null,payload_sha256:null,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["drain-before-termination"]},repetitions:1,timeout_seconds:60},
        {id:"provider-loss",category:"provider_loss",task:null,payload:null,payload_sha256:null,producer:{path:"dev/aws/validation-cases/test.sh",sha256:$producer_sha,checks:["capacity-deficit-visible"]},repetitions:1,timeout_seconds:60}
      ]
    },
    cost_guard:{run_worker_max:1,build_worker_max:1,nat_gateway_max:1,max_bundle_bytes:52428800},
    evidence:{bucket_output:"source_artifact_bucket_name",claim_prefix:"helmr/validation-claims",prefix:"helmr/validation-evidence",namespace:"managed-worker-20260714-a",retention_days:30},
    retries:{infrastructure_max_attempts:2,workload_attempts:1},
    stages:["preflight","control_up","awaiting_human","auth_ready","worker_up","workload","pre_shutdown_publish","cleanup","closed","post_shutdown_publish"]
  }' >"${manifest}"
git -C "${ops}" add .
git -C "${ops}" commit -qm manifest

mkdir -p "${tmp}/early-bin"
cat >"${tmp}/early-bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"sts get-caller-identity"*)
    printf '000000000000\n'
    ;;
  *"ecr describe-images"*)
    printf '{"imageDetails":[{"imageDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}\n'
    ;;
  *"imagebuilder get-image"*)
    printf '{"image":{"state":{"status":"AVAILABLE"},"imageRecipe":{"arn":"arn:aws:imagebuilder:us-east-1:000000000000:image-recipe/helmr-test-worker/0.1.2"},"outputResources":{"amis":[{"image":"ami-0123456789abcdef0","region":"us-east-1"}]}}}\n'
    ;;
  *"ec2 describe-images"*)
    printf '{"Images":[{"ImageId":"ami-0123456789abcdef0","Tags":[{"Key":"HelmrSourceCommit","Value":"%s"}]}]}\n' "${MOCK_SOURCE_COMMIT}"
    ;;
  *) exit 2 ;;
esac
EOF
chmod +x "${tmp}/early-bin/aws"
cat >"${tmp}/early-bin/tofu" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"output -raw control_release_repository_name"*) printf 'helmr-dev/control-releases\n' ;;
  *"output -raw control_release_repository_arn"*) printf 'arn:aws:ecr:us-east-1:000000000000:repository/helmr-dev/control-releases\n' ;;
  *"output -raw control_release_repository_url"*) printf '000000000000.dkr.ecr.us-east-1.amazonaws.com/helmr-dev/control-releases\n' ;;
  *) exit 2 ;;
esac
EOF
chmod +x "${tmp}/early-bin/tofu"

campaign() {
  PATH="${tmp}/early-bin:${PATH}" \
  MOCK_SOURCE_COMMIT="${source_commit}" \
  AWS_ACCOUNT_ID="000000000000" \
  DEV_NAME="managed-worker" \
  STATE_KEY="dev/managed-worker.tfstate" \
  DEV_TFVARS="${CAMPAIGN_TFVARS_OVERRIDE:-${control_tfvars_fixture}}" \
  HELMR_VALIDATION_PRODUCT_ROOT="${product}" \
  HELMR_VALIDATION_STATE_ROOT="${state_root}" \
  HELMR_AWS_SMOKE_STATE_ROOT="${smoke_state_root}" \
    "${script}" "$@"
}

campaign validate "${manifest}"

mismatched_repository_tfvars="${tmp}/mismatched-repository.tfvars"
sed 's#repository/helmr-dev/control-releases#repository/other/control-releases#' \
  "${control_tfvars_fixture}" >"${mismatched_repository_tfvars}"
if CAMPAIGN_TFVARS_OVERRIDE="${mismatched_repository_tfvars}" campaign init "${manifest}" >/dev/null 2>&1; then
  fail "campaign init must reject an ECS pull grant for a different ECR repository"
fi

mismatched_image_tfvars="${tmp}/mismatched-image.tfvars"
sed 's#helmr-dev/control-releases@sha256:#other/control-releases@sha256:#' \
  "${control_tfvars_fixture}" >"${mismatched_image_tfvars}"
if CAMPAIGN_TFVARS_OVERRIDE="${mismatched_image_tfvars}" campaign init "${manifest}" >/dev/null 2>&1; then
  fail "campaign init must reject a Control image outside the foundation repository"
fi

campaign init "${manifest}" >/dev/null
[ "$(campaign status "${manifest}" | jq -r '.status')" = "ready" ] || fail "initialized campaign status"

result="${tmp}/result.json"

jq '.unexpected=true' "${manifest}" >"${tmp}/invalid.json"
if campaign validate "${tmp}/invalid.json" >/dev/null 2>&1; then
  fail "unknown manifest fields should fail"
fi

jq 'del(.workload.cases[] | select(.category == "provider_loss"))' "${manifest}" >"${tmp}/missing-category.json"
if campaign validate "${tmp}/missing-category.json" >/dev/null 2>&1; then
  fail "missing required workload category should fail"
fi

jq '.workload.cases[0].producer.checks += [.workload.cases[0].producer.checks[0]]' "${manifest}" >"${tmp}/duplicate-check.json"
if campaign validate "${tmp}/duplicate-check.json" >/dev/null 2>&1; then
  fail "duplicate producer checks should fail"
fi

jq '.workload.cases |= [.[1],.[0]] + .[2:]' "${manifest}" >"${tmp}/reordered-cases.json"
if campaign validate "${tmp}/reordered-cases.json" >/dev/null 2>&1; then
  fail "reordered release cases should fail"
fi

jq '.workload.cases[0].producer.checks[0]="substituted-check"' "${manifest}" >"${tmp}/substituted-check.json"
if campaign validate "${tmp}/substituted-check.json" >/dev/null 2>&1; then
  fail "substituted release checks should fail"
fi

valid_case_result="${tmp}/valid-case-result.json"
jq -n '{
  schema:"helmrdotdev.validation-case-source-result.v2",
  status:"passed",
  reason:null,
  checks:[
    {id:"build-completed",status:"passed"},
    {id:"build-group-only",status:"passed"}
  ],
  objects:{
    run_ids:["019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"],
    workspace_ids:[],deployment_ids:[],schedule_ids:[],token_ids:[],actor_ids:[]
  },
  observations:{attempt:1}
}' >"${valid_case_result}"
normalized="${tmp}/normalized-case-result.json"
campaign normalize-case-evidence "${manifest}" build-on-build-worker \
  "${valid_case_result}" 0 "${normalized}"
jq -e '.status == "passed" and .reason == null' "${normalized}" >/dev/null ||
  fail "valid producer evidence should remain passed"

assert_invalid_case_result() {
  local input=$1 label=$2
  campaign normalize-case-evidence "${manifest}" build-on-build-worker \
    "${input}" 0 "${normalized}"
  jq -e '
    .status == "failed" and
    .reason == "invalid_producer_result" and
    all(.checks[]; .status == "failed")
  ' "${normalized}" >/dev/null || fail "${label}"
}

jq '.unexpected=true' "${valid_case_result}" >"${tmp}/unknown-case-field.json"
assert_invalid_case_result "${tmp}/unknown-case-field.json" \
  "unknown producer evidence fields should canonicalize to failed"
jq '.objects.run_ids=["00000000-0000-0000-0000-000000000000"]' \
  "${valid_case_result}" >"${tmp}/private-case-id.json"
assert_invalid_case_result "${tmp}/private-case-id.json" \
  "private IDs should canonicalize to failed evidence"
jq '.observations.blob=("x" * 9000)' \
  "${valid_case_result}" >"${tmp}/oversized-case-result.json"
assert_invalid_case_result "${tmp}/oversized-case-result.json" \
  "oversized producer evidence should canonicalize to failed"
jq '.checks[0].id="unexpected-check"' \
  "${valid_case_result}" >"${tmp}/mismatched-case-checks.json"
assert_invalid_case_result "${tmp}/mismatched-case-checks.json" \
  "mismatched producer checks should canonicalize to failed"

campaign normalize-case-evidence "${manifest}" build-on-build-worker \
  "${tmp}/missing-case-result.json" 1 "${normalized}"
jq -e '.status == "failed" and .reason == "producer_result_missing"' \
  "${normalized}" >/dev/null ||
  fail "missing producer evidence should be explicit"
campaign normalize-case-evidence "${manifest}" build-on-build-worker \
  "${valid_case_result}" 7 "${normalized}"
jq -e '
  .status == "failed" and
  .reason == "producer_exit_conflict" and
  all(.checks[]; .status == "failed")
' "${normalized}" >/dev/null ||
  fail "nonzero producer exit should override passed evidence"

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
  *"output -raw control_release_repository_name"*) printf 'helmr-dev/control-releases\n' ;;
  *"output -raw control_release_repository_arn"*) printf 'arn:aws:ecr:us-east-1:000000000000:repository/helmr-dev/control-releases\n' ;;
  *"output -raw control_release_repository_url"*) printf '000000000000.dkr.ecr.us-east-1.amazonaws.com/helmr-dev/control-releases\n' ;;
  *"output -raw source_artifact_bucket_name"*) printf 'artifact-bucket\n' ;;
  *"output -raw source_artifact_kms_key_arn"*) printf 'arn:aws:kms:us-east-1:000000000000:key/test\n' ;;
  *"output -json"*)
    if grep -q '^create_worker = true$' "${DEV_TFVARS}"; then run='"managed-worker-run-worker"'; build='"managed-worker-build-worker"'; nat='"nat-0123456789abcdef0"'; else run=null; build=null; nat=null; fi
    printf '{"control_cluster_name":{"value":"managed-worker-control"},"control_service_name":{"value":"control"},"dispatcher_service_name":{"value":"dispatcher"},"postgres_identifier":{"value":"helmr-db"},"worker_autoscaling_group_name":{"value":%s},"build_worker_autoscaling_group_name":{"value":%s},"worker_protect_from_scale_in":{"value":true},"build_worker_protect_from_scale_in":{"value":true},"execution_nat_gateway_id":{"value":%s}}\n' "${run}" "${build}" "${nat}"
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
  *"ecr describe-images"*) printf '{"imageDetails":[{"imageDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}\n' ;;
  *"imagebuilder get-image"*) printf '{"image":{"state":{"status":"AVAILABLE"},"imageRecipe":{"arn":"arn:aws:imagebuilder:us-east-1:000000000000:image-recipe/helmr-test-worker/0.1.2"},"outputResources":{"amis":[{"image":"ami-0123456789abcdef0","region":"us-east-1"}]}}}\n' ;;
  *"ec2 describe-images"*) printf '{"Images":[{"ImageId":"ami-0123456789abcdef0","Tags":[{"Key":"HelmrSourceCommit","Value":"%s"}]}]}\n' "${MOCK_SOURCE_COMMIT}" ;;
  *"get-bucket-versioning"*) printf '{"Status":"Enabled"}\n' ;;
  *"sts get-caller-identity"*) printf '000000000000\n' ;;
  *"get-public-access-block"*) printf '{"PublicAccessBlockConfiguration":{"BlockPublicAcls":true,"IgnorePublicAcls":true,"BlockPublicPolicy":true,"RestrictPublicBuckets":true}}\n' ;;
  *"get-bucket-encryption"*) printf '{"ServerSideEncryptionConfiguration":{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"aws:kms","KMSMasterKeyID":"arn:aws:kms:us-east-1:000000000000:key/test"}}]}}\n' ;;
  *"get-bucket-lifecycle-configuration"*) printf '{"Rules":[{"ID":"expire-validation-evidence","Status":"Enabled","Filter":{"Prefix":"helmr/validation-evidence/"},"Expiration":{"Days":30},"NoncurrentVersionExpiration":{"NoncurrentDays":30}}]}\n' ;;
  *"ecs describe-services"*) if [ -e "${MOCK_UNHEALTHY_CONTROL_FILE}" ]; then running=0; else running=1; fi; printf '{"failures":[],"services":[{"serviceName":"control","desiredCount":1,"runningCount":%s,"pendingCount":0,"taskDefinition":"control-task","deployments":[{"status":"PRIMARY","rolloutState":"COMPLETED"}]},{"serviceName":"dispatcher","desiredCount":1,"runningCount":%s,"pendingCount":0,"taskDefinition":"dispatcher-task","deployments":[{"status":"PRIMARY","rolloutState":"COMPLETED"}]}]}\n' "${running}" "${running}" ;;
  *"ecs describe-task-definition"*) if [[ "${command_line}" == *dispatcher-task* ]]; then name=dispatcher; else name=control; fi; printf '{"taskDefinition":{"containerDefinitions":[{"name":"%s","image":"000000000000.dkr.ecr.us-east-1.amazonaws.com/helmr-dev/control-releases@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}}\n' "${name}" ;;
  *"rds describe-db-instances"*"--db-instance-identifier"*) printf '{"DBInstances":[{"DBInstanceIdentifier":"helmr-db","DBInstanceStatus":"available","DBInstanceClass":"db.t4g.micro","EngineVersion":"16.3","InstanceCreateTime":"2026-07-14T00:00:00Z"}]}\n' ;;
  *"rds describe-db-instances"*) printf '{"DBInstances":[]}\n' ;;
  *"elasticache describe-replication-groups"*"--replication-group-id"*) printf '{"ReplicationGroups":[{"ReplicationGroupId":"managed-worker-dispatch","Status":"available","Engine":"valkey","MemberClusters":["cache-1"],"ReplicationGroupCreateTime":"2026-07-14T00:00:00Z"}]}\n' ;;
  *"elasticache describe-replication-groups"*) printf '{"ReplicationGroups":[]}\n' ;;
  *"autoscaling describe-auto-scaling-groups"*)
    if [ "${HELMR_VALIDATION_SAMPLE_PHASE:-}" = final ] &&
      [ -e "${MOCK_ASG_FINAL_SAMPLE_ERROR_FILE}" ]; then
      mv "${MOCK_ASG_FINAL_SAMPLE_ERROR_FILE}" "${MOCK_ASG_FINAL_SAMPLE_ERROR_FILE}.used"
      exit 1
    fi
    if [ -e "${MOCK_ASG_SAMPLE_ERROR_FILE}" ]; then
      mv "${MOCK_ASG_SAMPLE_ERROR_FILE}" "${MOCK_ASG_SAMPLE_ERROR_FILE}.used"
      exit 1
    fi
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
  MOCK_SOURCE_COMMIT="${source_commit}" \
  MOCK_S3_DIR="${tmp}/s3" \
  MOCK_DESTROYED_FILE="${tmp}/destroyed" \
  MOCK_UNHEALTHY_CONTROL_FILE="${tmp}/unhealthy-control" \
  MOCK_ASG_DRIFT_FILE="${tmp}/asg-drift" \
  MOCK_ASG_SAMPLE_ERROR_FILE="${tmp}/asg-sample-error" \
  MOCK_ASG_FINAL_SAMPLE_ERROR_FILE="${tmp}/asg-final-sample-error" \
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
  HELMR_AWS_SMOKE_STATE_ROOT="${smoke_state_root}" \
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
PATH="${tmp}/early-bin:${PATH}" \
MOCK_SOURCE_COMMIT="${source_commit}" \
AWS_ACCOUNT_ID="000000000000" \
DEV_NAME="managed-worker" \
STATE_KEY="dev/managed-worker.tfstate" \
DEV_TFVARS="${control_tfvars_fixture}" \
HELMR_VALIDATION_PRODUCT_ROOT="${product}" \
HELMR_VALIDATION_STATE_ROOT="${alternate_state}" \
HELMR_AWS_SMOKE_STATE_ROOT="${smoke_state_root}" \
  "${script}" init "${manifest_b}" >/dev/null
if PATH="${tmp}/bin:${PATH}" MOCK_S3_DIR="${tmp}/s3" BOOTSTRAP_STACK="${tmp}/bootstrap" \
  AWS_ACCOUNT_ID="000000000000" DEV_NAME="managed-worker" STATE_KEY="dev/managed-worker.tfstate" \
  HELMR_VALIDATION_PRODUCT_ROOT="${product}" HELMR_VALIDATION_STATE_ROOT="${alternate_state}" \
  HELMR_AWS_SMOKE_STATE_ROOT="${smoke_state_root}" \
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
      jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"control_up",status:"passed",reason:null,observations:{control_image:"helmr-dev/control-releases@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",control_desired_count:1,dispatcher_desired_count:1,nat_gateway_count:0,run_worker_count:0,build_worker_count:0,rds_instance_id:"helmr-db",valkey_replication_group_id:"helmr-cache"},cases:[]}' >"${result}"
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
case_evidence="${state_root}/managed-worker-20260714-b/case-evidence/run-on-run-worker-01.json"
jq -e '.schema == "helmrdotdev.validation-case-source-result.v2" and .objects.run_ids == ["019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"]' "${case_evidence}" >/dev/null ||
  fail "bounded case evidence should be retained"
cp "${case_evidence}" "${case_evidence}.original"
jq '.observations.attempt=2' "${case_evidence}.original" >"${case_evidence}"
if campaign_b validate-evidence "${manifest_b}" >/dev/null 2>"${tmp}/stderr"; then
  fail "case evidence hash mismatch should fail validation"
fi
grep -Fq 'case evidence hash mismatch' "${tmp}/stderr" ||
  fail "case evidence hash mismatch reason"
mv "${case_evidence}.original" "${case_evidence}"
unreferenced_evidence="${state_root}/managed-worker-20260714-b/case-evidence/unreferenced-01.json"
cp "${case_evidence}" "${unreferenced_evidence}"
if campaign_b validate-evidence "${manifest_b}" >/dev/null 2>"${tmp}/stderr"; then
  fail "unreferenced case evidence should fail validation"
fi
grep -Fq 'unreferenced case evidence file' "${tmp}/stderr" ||
  fail "unreferenced case evidence reason"
rm -f "${unreferenced_evidence}"
mv "${case_evidence}" "${case_evidence}.missing"
if campaign_b validate-evidence "${manifest_b}" >/dev/null 2>"${tmp}/stderr"; then
  fail "missing linked case evidence should fail validation"
fi
grep -Fq 'references missing case evidence' "${tmp}/stderr" ||
  fail "missing linked case evidence reason"
mv "${case_evidence}.missing" "${case_evidence}"
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
tar -tzf "${post_bundle}" | grep -Fq 'case-evidence/run-on-run-worker-01.json' ||
  fail "post-shutdown bundle should contain bounded case evidence"
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

manifest_f="${ops}/docs/validation/managed-worker-campaign-f.json"
jq '.evidence.namespace="managed-worker-20260714-f"' "${manifest}" >"${manifest_f}"
git -C "${ops}" add .
git -C "${ops}" commit -qm sixth-manifest
campaign_b init "${manifest_f}" >/dev/null
campaign_b claim "${manifest_f}"
jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"preflight",status:"passed",reason:null,observations:{},cases:[]}' >"${result}"
campaign_b start "${manifest_f}" preflight
campaign_b complete "${manifest_f}" preflight "${result}"
cp "${control_tfvars_fixture}" "${tfvars}"
campaign_b run-collect "${manifest_f}" control_up -- true
jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"awaiting_human",status:"passed",reason:null,observations:{},cases:[]}' >"${result}"
campaign_b start "${manifest_f}" awaiting_human
campaign_b complete "${manifest_f}" awaiting_human "${result}"
campaign_b auth "${manifest_f}"
cp "${worker_tfvars_fixture}" "${tfvars}"
if [ -e "${tmp}/destroyed" ]; then
  mv "${tmp}/destroyed" "${tmp}/destroyed.previous"
fi
campaign_b run-collect "${manifest_f}" worker_up -- true
touch "${tmp}/asg-sample-error"
if campaign_b workload "${manifest_f}" >/dev/null 2>"${tmp}/stderr"; then
  fail "worker sampling error should fail the workload"
fi
sampling_result="${state_root}/managed-worker-20260714-f/results/05-workload.json"
[ -f "${sampling_result}" ] || {
  cat "${tmp}/stderr" >&2
  fail "worker sampling failure should retain a workload result"
}
jq -e '
  .status == "failed" and
  .reason == "worker_sampling_failed" and
  all(.cases[]; .status == "passed")
' "${sampling_result}" >/dev/null ||
  fail "worker sampling error should not corrupt producer case evidence"
campaign_b validate-evidence "${manifest_f}"

manifest_g="${ops}/docs/validation/managed-worker-campaign-g.json"
jq '.evidence.namespace="managed-worker-20260714-g"' "${manifest}" >"${manifest_g}"
git -C "${ops}" add .
git -C "${ops}" commit -qm seventh-manifest
campaign_b init "${manifest_g}" >/dev/null
campaign_b claim "${manifest_g}"
jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"preflight",status:"passed",reason:null,observations:{},cases:[]}' >"${result}"
campaign_b start "${manifest_g}" preflight
campaign_b complete "${manifest_g}" preflight "${result}"
cp "${control_tfvars_fixture}" "${tfvars}"
campaign_b run-collect "${manifest_g}" control_up -- true
jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"awaiting_human",status:"passed",reason:null,observations:{},cases:[]}' >"${result}"
campaign_b start "${manifest_g}" awaiting_human
campaign_b complete "${manifest_g}" awaiting_human "${result}"
campaign_b auth "${manifest_g}"
cp "${worker_tfvars_fixture}" "${tfvars}"
campaign_b run-collect "${manifest_g}" worker_up -- true
touch "${tmp}/asg-final-sample-error"
if campaign_b workload "${manifest_g}" >/dev/null 2>"${tmp}/stderr"; then
  fail "final worker sampling error should fail the workload"
fi
[ -f "${tmp}/asg-final-sample-error.used" ] ||
  fail "final worker sampling failure injection was not consumed"
final_sampling_result="${state_root}/managed-worker-20260714-g/results/05-workload.json"
jq -e '
  .status == "failed" and
  .reason == "worker_sampling_failed" and
  all(.cases[]; .status == "passed")
' "${final_sampling_result}" >/dev/null ||
  fail "final sampling error should retain structured producer evidence"
campaign_b validate-evidence "${manifest_g}"

manifest_h="${ops}/docs/validation/managed-worker-campaign-h.json"
jq '.evidence.namespace="managed-worker-20260714-h"' "${manifest}" >"${manifest_h}"
git -C "${ops}" add .
git -C "${ops}" commit -qm eighth-manifest
campaign_b init "${manifest_h}" >/dev/null
campaign_b claim "${manifest_h}"
jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"preflight",status:"passed",reason:null,observations:{},cases:[]}' >"${result}"
campaign_b start "${manifest_h}" preflight
campaign_b complete "${manifest_h}" preflight "${result}"
cp "${control_tfvars_fixture}" "${tfvars}"
campaign_b run-collect "${manifest_h}" control_up -- true
jq -n '{schema:"helmrdotdev.validation-stage-result.v1",stage:"awaiting_human",status:"passed",reason:null,observations:{},cases:[]}' >"${result}"
campaign_b start "${manifest_h}" awaiting_human
campaign_b complete "${manifest_h}" awaiting_human "${result}"
campaign_b auth "${manifest_h}"
cp "${worker_tfvars_fixture}" "${tfvars}"
campaign_b run-collect "${manifest_h}" worker_up -- true
case_log="${tmp}/executed-cases"
if HELMR_VALIDATION_FAIL_CASE=network-deny HELMR_VALIDATION_CASE_LOG="${case_log}" \
  campaign_b workload "${manifest_h}" >/dev/null 2>"${tmp}/stderr"; then
  fail "producer failure should fail the workload"
fi
[ "$(tail -1 "${case_log}")" = "network-deny" ] ||
  fail "cases after a producer failure must not execute"
if grep -Fq 'provider-loss' "${case_log}"; then
  fail "destructive cases must not execute after an earlier failure"
fi
aborted_evidence="${state_root}/managed-worker-20260714-h/case-evidence/provider-loss-01.json"
jq -e '
  .status == "failed" and .reason == "prior_case_failed" and
  all(.checks[]; .status == "failed")
' "${aborted_evidence}" >/dev/null ||
  fail "unexecuted destructive case must retain explicit failed evidence"
campaign_b validate-evidence "${manifest_h}"

printf 'ok - validation campaign tests\n'
