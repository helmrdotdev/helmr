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
  'go test ./internal/firecracker -run TestRunNetworkPolicyContractCountsEveryDenyPath -count=1' \
  go test ./internal/firecracker -run TestRunNetworkPolicyContractCountsEveryDenyPath -count=1
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
run_check campaign-fault-producers \
  'bash tests/validation_case_contract_test.sh' \
  bash tests/validation_case_contract_test.sh
run_check failing-build-source \
  'sync local packages and prove the failing fixture reaches deployment creation' \
  bash -c "dev/workflows/scripts/sync-local-sdk.sh && go test ./cmd/helmr -run TestFailingBuildFixtureReachesDeploymentCreation -count=1"
run_check campaign-exact-release-profile \
  'exact ordered release profile and producer contract' \
  bash tests/validation_case_contract_test.sh
run_check network-deny-evidence-producer \
  'named nft deny counter tests plus bounded evidence-producer contract' \
  bash -c "go test ./internal/firecracker -run 'TestRunNetworkPolicyContract|TestRunNetworkCounterContract' -count=1 && bash tests/validation_case_contract_test.sh"
run_check same-workspace-call \
  'same-Workspace child handoff tests plus release smoke selector contract' \
  bash -c "go test ./internal/control ./internal/executor ./internal/dispatch -run 'SameWorkspace|Same.Workspace' -count=1 && bash tests/release_smoke_selector_test.sh"
run_check actor-console \
  'Actor detail route data contract and Console typecheck' \
  bash -c "bun test packages/console/src/lib/actors.test.ts && bun run --cwd packages/console typecheck"
run_check external-token-wait-registration \
  'runtime Token ref tests and smoke client typecheck' \
  bash -c "bun test sdk/typescript/src/tokens.test.ts && bun run --cwd dev/client typecheck && bun run --cwd dev/workflows typecheck"
run_check identity-fencing-contract \
  'deterministic stale Lease and prior worker epoch rejection tests' \
  bash -c "go test ./internal/control -run TestRenewRunLeaseRejectsPriorWorkerEpoch -count=1 && go test ./internal/dispatch -run TestStaleWorkerFencerOldEpochCannotFenceNewEpoch -count=1"
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
