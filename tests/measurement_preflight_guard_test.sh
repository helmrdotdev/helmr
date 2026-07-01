#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

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

assert_not_contains() {
	local file="$1"
	local needle="$2"
	local label="$3"
	! grep -Fq "$needle" "$file" || fail "$label: did not expect '$needle' in $file"
}

assert_equal() {
	local expected="$1"
	local actual="$2"
	local label="$3"
	[ "$actual" = "$expected" ] || fail "$label: expected '$expected', got '$actual'"
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fake_root="$tmp/repo"
mkdir -p "$fake_root/dev/aws"
cp "$repo_root/dev/aws/run-measurement-preflight.sh" "$fake_root/dev/aws/run-measurement-preflight.sh"
cat >"$fake_root/dev/aws/db-query.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$1" >"${CAPTURED_PREFLIGHT_SQL:?CAPTURED_PREFLIGHT_SQL is required}"
EOF
chmod +x "$fake_root/dev/aws/run-measurement-preflight.sh" "$fake_root/dev/aws/db-query.sh"

stdout="$tmp/stdout"
stderr="$tmp/stderr"
sql_file="$tmp/preflight.sql"

run_expect_status() {
	local expected_status="$1"
	shift
	set +e
	"$@" >"$stdout" 2>"$stderr"
	local status=$?
	set -e
	assert_equal "$expected_status" "$status" "$* status"
}

run_expect_status 2 "$fake_root/dev/aws/run-measurement-preflight.sh"
assert_contains "$stderr" "measurement preflight requires HELMR_MEASUREMENT_PREFLIGHT_ALLOW_ECS_TASK=1" "ECS opt-in guard"

run_expect_status 2 env PROJECT='bad slug' "$fake_root/dev/aws/run-measurement-preflight.sh"
assert_contains "$stderr" "PROJECT must contain only letters, numbers, dot, underscore, or dash" "project slug guard"

run_expect_status 2 env HELMR_MEASUREMENT_REQUIRED_MILLI_CPU=0 "$fake_root/dev/aws/run-measurement-preflight.sh"
assert_contains "$stderr" "required CPU, memory, and execution slots must be positive" "positive resource guard"

run_expect_status 2 env WORKER_HEARTBEAT_MAX_AGE_SECONDS=not-a-number "$fake_root/dev/aws/run-measurement-preflight.sh"
assert_contains "$stderr" "WORKER_HEARTBEAT_MAX_AGE_SECONDS must be a non-negative integer" "heartbeat numeric guard"

CAPTURED_PREFLIGHT_SQL="$sql_file" \
	HELMR_MEASUREMENT_PREFLIGHT_ALLOW_ECS_TASK=1 \
	HELMR_MEASUREMENT_REQUIRED_MILLI_CPU=3000 \
	HELMR_MEASUREMENT_REQUIRED_MEMORY_MIB=4096 \
	HELMR_MEASUREMENT_REQUIRED_DISK_MIB=32768 \
	HELMR_MEASUREMENT_REQUIRED_EXECUTION_SLOTS=2 \
	"$fake_root/dev/aws/run-measurement-preflight.sh" >"$stdout" 2>"$stderr"

assert_contains "$sql_file" "effective_available_milli_cpu >= 3000" "CPU requirement should be embedded in scheduler capacity check"
assert_contains "$sql_file" "effective_available_memory_mib >= 4096" "memory requirement should be embedded in scheduler capacity check"
assert_contains "$sql_file" "effective_available_disk_mib >= 32768" "disk requirement should be embedded in scheduler capacity check"
assert_contains "$sql_file" "effective_available_slots >= 2" "slot requirement should be embedded in scheduler capacity check"
assert_contains "$sql_file" "required vector % milli CPU, % memory MiB, % disk MiB, % slot(s)', 3000, 4096, 32768, 2" "required vector diagnostic should include all dimensions"
assert_contains "$sql_file" "IF 0 = 1 THEN" "deployment freshness should be optional by default"
assert_not_contains "$stderr" "measurement preflight requires HELMR_MEASUREMENT_PREFLIGHT_ALLOW_ECS_TASK=1" "allowed fake db-query should pass opt-in gate"

CAPTURED_PREFLIGHT_SQL="$sql_file" \
	HELMR_MEASUREMENT_PREFLIGHT_ALLOW_ECS_TASK=1 \
	"$fake_root/dev/aws/run-measurement-preflight.sh" --require-deployments >"$stdout" 2>"$stderr"
assert_contains "$sql_file" "IF 1 = 1 THEN" "require-deployments should enable deployment freshness gate"

printf 'ok - measurement preflight guard tests\n'
