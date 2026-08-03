#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
script="$repo_root/dev/workflows/scripts/run-release-smoke.sh"

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

assert_equal() {
	local expected="$1"
	local actual="$2"
	local label="$3"
	[ "$actual" = "$expected" ] || fail "$label: expected '$expected', got '$actual'"
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
stdout="$tmp/stdout"
stderr="$tmp/stderr"

run_expect_status() {
	local expected_status="$1"
	shift
	set +e
	"$@" >"$stdout" 2>"$stderr"
	local status=$?
	set -e
	assert_equal "$expected_status" "$status" "$* status"
}

result_json="$tmp/result.json"
run_expect_status 2 env HELMR_SMOKE_RESULT_FILE="$result_json" SMOKE_CASES=unknown SKIP_DEPLOY=1 bash "$script"
assert_contains "$stderr" "unknown SMOKE_CASES entry: unknown" "unknown selector error"
assert_contains "$stderr" "known SMOKE_CASES entries:" "unknown selector should print known entries"
assert_equal "helmrdotdev.release-smoke-result.v1" "$(jq -r '.schema' "$result_json")" "structured smoke result schema"
assert_equal "failed" "$(jq -r '.status' "$result_json")" "structured smoke terminal status"
assert_equal "2" "$(jq -r '.exit_code' "$result_json")" "structured smoke exit code"

run_expect_status 2 env SMOKE_CASES=production-secrets SKIP_PRODUCTION=1 SKIP_DEPLOY=1 bash "$script"
assert_contains "$stderr" "SMOKE_CASES=production-secrets cannot run while SKIP_PRODUCTION=1" "production precondition"

fake_log="$tmp/fake-helmr.log"
fake_helmr="$repo_root/tests/fixtures/fake-release-smoke-helmr.sh"
run_expect_status 0 env \
	FAKE_HELMR_LOG="$fake_log" \
	HELMR_BIN="$fake_helmr" \
	HELMR_SMOKE_RESULT_FILE="$result_json" \
	SMOKE_CASES=child-tasks \
	SKIP_DEPLOY=1 \
	bash "$script"
assert_contains "$stdout" "PASS staging-child-tasks" "child Task lifecycle pass"
assert_contains "$fake_log" "workspace create helmr-child-task-target-smoke" "child target Workspace creation"
assert_contains "$fake_log" "--workspace workspace-caller" "child caller Workspace binding"
assert_contains "$fake_log" '"mode":"call-success"' "child call success payload"
assert_contains "$fake_log" '"mode":"same-workspace-call"' "same-Workspace child call payload"
assert_contains "$fake_log" '"mode":"call-failure"' "child call failure payload"
assert_contains "$fake_log" '"mode":"start-detached"' "detached child payload"
assert_contains "$fake_log" "run wait run-detached-child" "detached child terminal wait"
assert_equal '["child-tasks"]' "$(jq -c '.executed_cases' "$result_json")" "child Task executed case"
assert_equal "passed" "$(jq -r '.status' "$result_json")" "child Task structured terminal status"

: >"$fake_log"
run_expect_status 1 env \
	FAKE_HELMR_FAIL_MODE=call-failure \
	FAKE_HELMR_LOG="$fake_log" \
	HELMR_BIN="$fake_helmr" \
	HELMR_SMOKE_RESULT_FILE="$result_json" \
	SMOKE_CASES=child-tasks \
	SKIP_DEPLOY=1 \
	bash "$script"
assert_contains "$fake_log" "workspace delete --id workspace-caller" "failed child caller cleanup"
assert_contains "$fake_log" "workspace delete --id workspace-target" "failed child target cleanup"
assert_equal "failed" "$(jq -r '.status' "$result_json")" "failed child structured terminal status"

: >"$fake_log"
run_expect_status 1 env \
	FAKE_HELMR_FAIL_MODE=delete-target \
	FAKE_HELMR_LOG="$fake_log" \
	HELMR_BIN="$fake_helmr" \
	HELMR_SMOKE_RESULT_FILE="$result_json" \
	SMOKE_CASES=child-tasks \
	SKIP_DEPLOY=1 \
	bash "$script"
assert_contains "$fake_log" "workspace delete --id workspace-target" "target delete failure attempt"
assert_equal "failed" "$(jq -r '.status' "$result_json")" "delete failure structured terminal status"

: >"$fake_log"
run_expect_status 1 env \
	FAKE_HELMR_FAIL_MODE=run-events \
	FAKE_HELMR_LOG="$fake_log" \
	HELMR_BIN="$fake_helmr" \
	HELMR_SMOKE_RESULT_FILE="$result_json" \
	SMOKE_CASES=child-tasks \
	SKIP_DEPLOY=1 \
	bash "$script"
assert_contains "$fake_log" "run events run-call-success" "Run events failure attempt"
assert_contains "$fake_log" "workspace delete --id workspace-caller" "inspection failure caller cleanup"
assert_contains "$fake_log" "workspace delete --id workspace-target" "inspection failure target cleanup"
assert_equal "failed" "$(jq -r '.status' "$result_json")" "inspection failure structured terminal status"

contract_fake="$repo_root/tests/fixtures/fake-release-smoke-contract-helmr.sh"

: >"$fake_log"
run_expect_status 0 env \
	FAKE_HELMR_LOG="$fake_log" \
	HELMR_BIN="$contract_fake" \
	HELMR_SMOKE_RESULT_FILE="$result_json" \
	SMOKE_CASES=network \
	SKIP_DEPLOY=1 \
	bash "$script"
assert_contains "$stdout" "PASS staging-network" "network contract pass"
assert_contains "$fake_log" "run get 019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" "network output inspection"
assert_equal '["network"]' "$(jq -c '.executed_cases' "$result_json")" "network executed case"

: >"$fake_log"
run_expect_status 0 env \
	FAKE_HELMR_LOG="$fake_log" \
	HELMR_BIN="$contract_fake" \
	HELMR_SMOKE_RESULT_FILE="$result_json" \
	SMOKE_CASES=concurrent-wait \
	SKIP_DEPLOY=1 \
	bash "$script"
assert_contains "$stdout" "PASS staging-concurrent-wait" "concurrent Wait contract pass"

: >"$fake_log"
run_expect_status 0 env \
	FAKE_HELMR_LOG="$fake_log" \
	HELMR_BIN="$contract_fake" \
	HELMR_SMOKE_RESULT_FILE="$result_json" \
	SMOKE_CASES=invalid-payload \
	SKIP_DEPLOY=1 \
	bash "$script"
assert_contains "$stdout" "PASS staging-invalid-payload failed as expected" "payload error contract pass"

: >"$fake_log"
run_expect_status 1 env \
	FAKE_CONTRACT_ERROR_CODE=wrong_error \
	FAKE_HELMR_LOG="$fake_log" \
	HELMR_BIN="$contract_fake" \
	HELMR_SMOKE_RESULT_FILE="$result_json" \
	SMOKE_CASES=invalid-payload \
	SKIP_DEPLOY=1 \
	bash "$script"
assert_contains "$stderr" "expected error code task_payload_invalid, got wrong_error" "payload error code mismatch"
assert_contains "$fake_log" "workspace delete --id 019c10d5-a6f7-7af1-8f5f-bb97bcc0dc3f" "payload mismatch cleanup"

: >"$fake_log"
run_expect_status 0 env \
	FAKE_HELMR_LOG="$fake_log" \
	HELMR_BIN="$contract_fake" \
	HELMR_SMOKE_RESULT_FILE="$result_json" \
	SMOKE_CASES=expected-error \
	SKIP_DEPLOY=1 \
	bash "$script"
assert_contains "$stdout" "PASS staging-expected-error failed as expected" "Task failure contract pass"

: >"$fake_log"
run_expect_status 0 env \
	FAKE_HELMR_LOG="$fake_log" \
	HELMR_BIN="$contract_fake" \
	HELMR_SMOKE_RESULT_FILE="$result_json" \
	SMOKE_CASES=missing-secrets \
	SKIP_DEPLOY=1 \
	bash "$script"
assert_contains "$stdout" "PASS staging-missing-secrets rejected before run creation" "missing Secret rejection"

printf 'ok - release smoke selector tests\n'
