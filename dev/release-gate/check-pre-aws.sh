#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
result='[]'

record() {
  local id=$1 status=$2 evidence=$3
  result="$(jq -c \
    --arg id "${id}" \
    --arg status "${status}" \
    --arg evidence "${evidence}" \
    '. + [{id:$id,status:$status,evidence:$evidence}]' <<<"${result}")"
}

run_check() {
  local id=$1 evidence=$2
  shift 2
  if (cd "${ROOT}" && "$@") >/dev/null 2>&1; then
    record "${id}" passed "${evidence}"
  else
    record "${id}" blocked "${evidence}"
  fi
}

# These checks execute the shipped contracts. A source marker, test name, or
# declaration string is not release evidence because it can survive in dead
# code or comments.
run_check ipv6-host-policy \
  'go test ./internal/firecracker -run TestNFTNetworkPolicyScript -count=1' \
  go test ./internal/firecracker -run TestNFTNetworkPolicyScript -count=1
run_check workflow-samples-typecheck \
  'bun run --cwd dev/workflows typecheck' \
  bun run --cwd dev/workflows typecheck
run_check client-smoke-typecheck \
  'bun run --cwd dev/client typecheck' \
  bun run --cwd dev/client typecheck
run_check release-smoke-contract \
  'bash tests/release_smoke_selector_test.sh' \
  bash tests/release_smoke_selector_test.sh
run_check bounded-campaign-evidence \
  'bash tests/validation_campaign_test.sh' \
  bash tests/validation_campaign_test.sh
run_check public-run-path-report \
  'bash tests/run_path_report_local_test.sh && bash tests/path_report_wrapper_test.sh' \
  bash -c 'bash tests/run_path_report_local_test.sh && bash tests/path_report_wrapper_test.sh'
run_check packed-sdk-consumer \
  'bash scripts/check-packed-sdk-consumer.sh' \
  bash scripts/check-packed-sdk-consumer.sh
run_check cli-resource-boundary \
  'go test ./cmd/helmr -run "TestGreenfieldCommandSurface|TestTaskStart" -count=1' \
  go test ./cmd/helmr -run 'TestGreenfieldCommandSurface|TestTaskStart' -count=1
run_check actor-cli \
  'go test ./cmd/helmr ./internal/client -run Actor -count=1' \
  go test ./cmd/helmr ./internal/client -run Actor -count=1
run_check schedule-cli \
  'go test ./cmd/helmr ./internal/client -run Schedule -count=1' \
  go test ./cmd/helmr ./internal/client -run Schedule -count=1

# These are known product or certification gaps. Keep them explicit until a
# positive executable contract replaces each blocker; absence of an old stub
# or presence of a command/route string must never make the gate pass.
record campaign-fault-producers blocked \
  'missing executable fault-producer contract tests'
record campaign-exact-release-profile blocked \
  'manifest validates categories but does not yet freeze exact v0 case/check IDs'
record network-deny-runtime-evidence blocked \
  'missing known-reachable private endpoint or nft deny-counter evidence'
record same-workspace-call blocked \
  'same-Workspace child Task handoff is not implemented'
record actor-console blocked \
  'Actor Console inspection surface is not implemented'
record external-token-wait-registration blocked \
  'runtime cannot register a Wait for an externally created Token'

status=passed
reason_json=null
if jq -e 'any(.[]; .status == "blocked")' <<<"${result}" >/dev/null; then
  status=blocked
  reason_json='"product_release_blockers"'
fi
output="$(
  jq -n \
    --arg status "${status}" \
    --argjson reason "${reason_json}" \
    --argjson checks "${result}" \
    '{schema:"helmrdotdev.pre-aws-release-gate.v1",status:$status,reason:$reason,checks:$checks}'
)"
printf '%s\n' "${output}" | jq .

if [ -n "${HELMR_PRE_AWS_GATE_RESULT_FILE:-}" ]; then
  mkdir -p "$(dirname "${HELMR_PRE_AWS_GATE_RESULT_FILE}")"
  printf '%s\n' "${output}" >"${HELMR_PRE_AWS_GATE_RESULT_FILE}.tmp"
  chmod 0600 "${HELMR_PRE_AWS_GATE_RESULT_FILE}.tmp"
  mv "${HELMR_PRE_AWS_GATE_RESULT_FILE}.tmp" "${HELMR_PRE_AWS_GATE_RESULT_FILE}"
fi

[ "${status}" = passed ]
