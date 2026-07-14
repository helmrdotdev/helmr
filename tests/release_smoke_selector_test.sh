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
	grep -Fq "$needle" "$file" || fail "$label: expected '$needle' in $file"
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

run_expect_status 2 env SMOKE_CASES=root-api-start-and-wait SKIP_DEPLOY=1 bash "$script"
assert_contains "$stderr" "SMOKE_CASES=root-api-start-and-wait requires HELMR_API_KEY" "root API precondition"

run_expect_status 2 env SMOKE_CASES=production-secrets SKIP_PRODUCTION=1 SKIP_DEPLOY=1 bash "$script"
assert_contains "$stderr" "SMOKE_CASES=production-secrets cannot run while SKIP_PRODUCTION=1" "production precondition"

printf 'ok - release smoke selector tests\n'
