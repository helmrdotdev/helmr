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
  dev/release-gate/run-go-tests.sh \
  '^TestRunNetworkPolicyContractCountsEveryDenyPath$' ./internal/firecracker
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
  bash -c "dev/workflows/scripts/sync-local-sdk.sh && HELMR_TEST_PREPARED_FAILING_BUILD_FIXTURE=1 dev/release-gate/run-go-tests.sh '^TestFailingBuildFixtureReachesDeploymentCreation$' ./cmd/helmr"
run_check campaign-exact-release-profile \
  'exact ordered release profile and producer contract' \
  bash tests/validation_case_contract_test.sh
run_check network-deny-evidence-producer \
  'named nft deny counter tests plus bounded evidence-producer contract' \
  bash -c "dev/release-gate/run-go-tests.sh '^TestRunNetworkPolicyContract' ./internal/firecracker && dev/release-gate/run-go-tests.sh '^TestRunNetworkCounterContract' ./internal/firecracker && bash tests/validation_case_contract_test.sh"
run_check same-workspace-call \
  'same-Workspace controlplane, executor, dispatch, and guest Program handoff contracts plus release smoke selector' \
  bash -c "nix develop -c dev/release-gate/run-go-tests.sh 'SameWorkspace|Same.Workspace' ./internal/controlplane ./internal/dispatch && dev/release-gate/run-go-tests.sh '^TestChildAttachStartsNewProgramOnRetainedMount$' ./internal/executor && dev/release-gate/run-go-tests.sh 'ManagedProgramChildAdmission|ProgramCgroupLeaf|WorkspaceProgramAdmission|RestoredWorkspaceRebind' ./internal/guestd && bash tests/release_smoke_selector_test.sh"
run_check actor-console \
  'Actor detail route data contract and Console typecheck' \
  bash -c "bun test packages/console/src/lib/actors.test.ts && bun run --cwd packages/console typecheck"
run_check external-token-wait-registration \
  'runtime Token ref tests and smoke client typecheck' \
  bash -c "bun test sdk/typescript/src/tokens.test.ts && bun run --cwd dev/client typecheck && bun run --cwd dev/workflows typecheck"
run_check identity-fencing-contract \
  'deterministic stale Lease and prior worker epoch rejection tests' \
  bash -c "dev/release-gate/run-go-tests.sh '^TestRenewRunLeaseRejectsPriorWorkerEpoch$' ./internal/controlplane && dev/release-gate/run-go-tests.sh '^TestStaleWorkerFencerOldEpochCannotFenceNewEpoch$' ./internal/dispatch"
run_check worker-mutation-lock-contract \
  'minimal Lease fences, renewal CAS/replay, Token Wait linearization, canonical child Workspace locks, and uncertain session unlock discard' \
  bash -c "dev/release-gate/run-go-tests.sh '^TestRenewRunLeaseReplaysOnlyTheImmediatelyPreviousExpiry$' ./internal/controlplane && dev/release-gate/run-go-tests.sh '^TestRenewRunLeaseRejectsUnexpectedExpiry$' ./internal/controlplane && dev/release-gate/run-go-tests.sh '^TestRenewRunLeaseRejectsAuthorityThatExpiredWhileWaitingForLocks$' ./internal/controlplane && nix develop -c dev/release-gate/run-go-tests.sh '^TestRunLeaseRenewalUpdatesBothLeasesAtomically$' ./internal/db && nix develop -c dev/release-gate/run-go-tests.sh '^TestRunLeaseRenewalRollsBackWhenWorkspaceLeaseCannotAdvance$' ./internal/db && nix develop -c dev/release-gate/run-go-tests.sh '^TestChildWorkspacePairLocksConvergeForOppositeDirections$' ./internal/db && nix develop -c dev/release-gate/run-go-tests.sh '^TestTokenWaitRegistrationConcurrentReplayConverges$' ./internal/token && nix develop -c dev/release-gate/run-go-tests.sh '^TestTokenWaitRegistrationReplaySurvivesParkedCompletion$' ./internal/token && nix develop -c dev/release-gate/run-go-tests.sh '^TestAcquireHoldsAndReleasesEveryKey$' ./internal/sessionlock && nix develop -c dev/release-gate/run-go-tests.sh '^TestGuardDiscardsConnectionWhenReleaseCannotBeConfirmed$' ./internal/sessionlock"
run_check postgres-primitive-schema \
  'PostgreSQL 18 migration/down-migration contract with no application-owned functions, triggers, views, rules, generated business columns, Run-stream tables, or workspace_process_records' \
  nix develop -c dev/release-gate/run-go-tests.sh '^TestUpWithPostgres$' ./internal/db/schema
run_check surface-attestation-contract \
  'typed Deployment declaration and worker/runtime evidence query contract' \
  bash tests/surface_attestation_test.sh
run_check public-run-path-report \
  'bash tests/run_path_report_local_test.sh && bash tests/path_report_wrapper_test.sh' \
  bash -c 'bash tests/run_path_report_local_test.sh && bash tests/path_report_wrapper_test.sh'
run_check packed-sdk-consumer \
  'bash scripts/check-packed-sdk-consumer.sh' \
  bash scripts/check-packed-sdk-consumer.sh
run_check cli-resource-boundary \
  'go test ./cmd/helmr -run "TestGreenfieldCommandSurface|TestTaskStart" -count=1' \
  bash -c "dev/release-gate/run-go-tests.sh '^TestGreenfieldCommandSurface$' ./cmd/helmr && dev/release-gate/run-go-tests.sh '^TestTaskStart' ./cmd/helmr"
run_check actor-cli \
  'go test ./cmd/helmr ./internal/client -run Actor -count=1' \
  dev/release-gate/run-go-tests.sh 'Actor' ./cmd/helmr ./internal/client
run_check actor-runtime-contract \
  'typed Actor proto-to-executor worker bridge, semantic failure, and stale Run source Lease fence' \
  bash -c "dev/release-gate/run-go-tests.sh '^TestActorRuntimeVerticalContract$' ./internal/executor && dev/release-gate/run-go-tests.sh '^TestWorkerActor' ./internal/executor && dev/release-gate/run-go-tests.sh '^TestAuthorizeWorkerRunSourceRequiresWorkerAndLiveFence$' ./internal/controlplane"
run_check workspace-runtime-contract \
  'typed SDK/proto Workspace surface, drained checkpoint pause, executor worker bridge, renewed assignment retry, run-pinned create fence, and error classification' \
  bash -c "bun test runtime/typescript/src/program.test.ts && dev/release-gate/run-go-tests.sh '^TestRelayProgramDefersCheckpointPauseUntilRuntimeOperationsDrain$' ./internal/guestd && dev/release-gate/run-go-tests.sh '^TestWorkspaceRuntimeVerticalContract$' ./internal/executor && dev/release-gate/run-go-tests.sh '^TestWorkerWorkspaceRequests' ./internal/executor && dev/release-gate/run-go-tests.sh '^TestWorkspaceRuntimeRetryUsesRenewedAssignment$' ./internal/executor && nix develop -c dev/release-gate/run-go-tests.sh '^TestRunPinnedWorkspaceCreateUsesSourceDeploymentAndFencesBeforeClaim$' ./internal/controlplane && nix develop -c dev/release-gate/run-go-tests.sh '^TestRunSourcedWorkspaceSelfExecAndDeleteAreBusyWithoutSideEffects$' ./internal/controlplane && nix develop -c dev/release-gate/run-go-tests.sh '^TestWorkerWorkspaceExecFailureDoesNotClassifyUnknownInfrastructureError$' ./internal/controlplane"
run_check schedule-cli \
  'go test ./cmd/helmr ./internal/client -run Schedule -count=1' \
  dev/release-gate/run-go-tests.sh 'Schedule' ./cmd/helmr ./internal/client

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
