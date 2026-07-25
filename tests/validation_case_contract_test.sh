#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
profile="${root}/dev/aws/release-validation-profile.json"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

jq -e '
  .schema == "helmrdotdev.aws-release-validation-profile.v1" and
  .cases[-1].id == "provider-loss" and
  ([.cases[].id] | unique | length) == (.cases | length) and
  all(.cases[];
    .repetitions == 1 and
    (.timeout_seconds >= 60 and .timeout_seconds <= 3600) and
    (.producer.checks | length > 0)
  )
' "${profile}" >/dev/null || fail "exact release profile"

while IFS= read -r producer; do
  bash -n "${root}/${producer}" || fail "producer syntax: ${producer}"
done < <(jq -r '.cases[].producer.path' "${profile}")
bash -n "${root}/dev/aws/validation-cases/case-lib.sh" ||
  fail "producer library syntax"

index=0
while IFS= read -r profile_case; do
  case_id="$(jq -r '.id' <<<"${profile_case}")"
  producer="$(jq -r '.producer.path' <<<"${profile_case}")"
  result="${tmp}/${index}.json"
  case_json="$(
    jq -c '
      . + {
        payload:null,
        payload_sha256:null,
        producer:(.producer + {sha256:("0" * 64)})
      }
    ' <<<"${profile_case}"
  )"
  HELMR_VALIDATION_DRY_RUN=1 \
  HELMR_VALIDATION_CASE="${case_json}" \
  HELMR_VALIDATION_CASE_RESULT_FILE="${result}" \
    "${root}/${producer}" || fail "producer dry run: ${case_id}"
  jq -e --argjson expected "$(jq -c '.producer.checks | sort' <<<"${profile_case}")" '
    .schema == "helmrdotdev.validation-case-source-result.v2" and
    .status == "passed" and .reason == null and
    ([.checks[].id] | sort) == $expected and
    all(.checks[]; .status == "passed") and
    .observations.dry_run == true and
    .observations.cleanup_verified == true
  ' "${result}" >/dev/null || fail "producer result contract: ${case_id}"
  index=$((index + 1))
done < <(jq -c '.cases[]' "${profile}")

if rg -n \
  'aws[[:space:]]+(autoscaling[[:space:]]+(update|delete|create|put)|ec2[[:space:]]+(create|delete|modify)|iam[[:space:]]+(create|delete|put|attach|detach)|rds[[:space:]]+(create|delete|modify))' \
  "${root}/dev/aws/validation-cases"/*.sh >/dev/null; then
  fail "producer contains an unapproved infrastructure mutation"
fi
if rg -n 'aws[[:space:]]+ec2[[:space:]]+terminate-instances' \
  "${root}/dev/aws/validation-cases" \
  --glob '*.sh' --glob '!provider-loss.sh' >/dev/null; then
  fail "only the explicitly approved provider-loss case may terminate an instance"
fi

rg -F 'nft list counter inet helmr_network_policy run_denied' \
  "${root}/dev/aws/validation-cases/network-deny.sh" >/dev/null ||
  fail "network producer must read the named deny counter"
rg -F '[ "${delta}" -gt 0 ]' \
  "${root}/dev/aws/validation-cases/network-deny.sh" >/dev/null ||
  fail "network producer must require a positive deny counter delta"
rg -F '[ "${HELMR_ALLOW_PROVIDER_LOSS:-0}" = 1 ]' \
  "${root}/dev/aws/validation-cases/provider-loss.sh" >/dev/null ||
  fail "provider loss must require explicit approval"
rg -F "lease.terminal_reason_code = 'workspace_image_failed'" \
  "${root}/dev/aws/validation-cases/build-failure-isolation.sh" >/dev/null ||
  fail "failing build must require the expected terminal reason"
rg -F "lease.terminal_error->>'message' LIKE '%intentional build failure%'" \
  "${root}/dev/aws/validation-cases/build-failure-isolation.sh" >/dev/null ||
  fail "failing build must require fixture-specific failure evidence"
for producer in build-placement.sh build-failure-isolation.sh; do
  rg -F 'AND NOT EXISTS (' \
    "${root}/dev/aws/validation-cases/${producer}" >/dev/null ||
    fail "${producer} must reject any disallowed build placement"
done

printf 'ok - validation case contracts\n'
