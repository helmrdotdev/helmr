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

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/bin"
cat >"$tmp/bin/psql" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
cp "${PGSERVICEFILE:?PGSERVICEFILE is required}" "${CAPTURED_SERVICE_FILE:?CAPTURED_SERVICE_FILE is required}"
printf '%s\n' "${PGSERVICE:?PGSERVICE is required}" >"${CAPTURED_SERVICE_NAME:?CAPTURED_SERVICE_NAME is required}"
printf 'psql called\n' >>"${CAPTURED_PSQL_CALLS:?CAPTURED_PSQL_CALLS is required}"
cat >"${CAPTURED_SQL_STDIN:?CAPTURED_SQL_STDIN is required}"
EOF
chmod +x "$tmp/bin/psql"

service_file="$tmp/service.conf"
service_name="$tmp/service-name.txt"
sql_stdin="$tmp/sql-stdin.sql"
psql_calls="$tmp/psql-calls.txt"
run_id="22222222-2222-2222-2222-222222222222"

PATH="$tmp/bin:$PATH" \
	HELMR_DATABASE_URL='postgresql://user%40name:p%40ss%3Aword@[::1]:6543/main_db?connect_timeout=7&target_session_attrs=read-write&channel_binding=prefer' \
	CAPTURED_SERVICE_FILE="$service_file" \
	CAPTURED_SERVICE_NAME="$service_name" \
	CAPTURED_SQL_STDIN="$sql_stdin" \
	CAPTURED_PSQL_CALLS="$psql_calls" \
	"$repo_root/dev/aws/run-path-report.sh" "$run_id" >"$tmp/path-report.out"

assert_contains "$service_file" "[helmr_path_report]" "service header"
assert_contains "$service_file" "host=::1" "IPv6 host should be unbracketed"
assert_contains "$service_file" "port=6543" "port should be preserved"
assert_contains "$service_file" "dbname=main_db" "database name should be preserved"
assert_contains "$service_file" "user=user@name" "encoded user should decode percent escapes"
assert_contains "$service_file" "password=p@ss:word" "encoded password should decode percent escapes"
assert_contains "$service_file" "target_session_attrs=read-write" "unknown libpq parameter should be preserved"
assert_contains "$service_file" "channel_binding=prefer" "second unknown libpq parameter should be preserved"
assert_contains "$service_name" "helmr_path_report" "service name"
assert_contains "$sql_stdin" "WHERE id = '${run_id}'::uuid" "run id should be bound into report SQL"
assert_contains "$sql_stdin" "run_leases" "run lease evidence query should be present"
assert_contains "$sql_stdin" "run_waits" "run wait evidence query should be present"
assert_contains "$sql_stdin" "run_checkpoints" "checkpoint evidence query should be present"
assert_contains "$sql_stdin" "run_checkpoint_artifacts" "checkpoint artifact evidence query should be present"
assert_contains "$sql_stdin" "runtime_instances" "runtime evidence query should be present"
assert_contains "$sql_stdin" "worker_network_slots" "network fence evidence query should be present"
assert_contains "$sql_stdin" "workspace_mounts" "workspace mount evidence query should be present"
assert_contains "$sql_stdin" "checkpoint_request_version" "typed checkpoint request evidence should be present"
assert_contains "$sql_stdin" "resume_request_version" "typed resume request evidence should be present"
assert_contains "$sql_stdin" "source_run_lease_id" "checkpoint source fence should be present"
assert_contains "$sql_stdin" "observed_state" "runtime observed authority should be present"
assert_contains "$sql_stdin" "network_slot_generation" "network slot fence should be present"
assert_contains "$sql_stdin" "path_hints" "path hint classification query should be present"
assert_not_contains "$sql_stdin" "worker_commands" "generic worker command evidence should be absent"
assert_not_contains "$sql_stdin" "run_checkpoint_restores" "obsolete restore authority should be absent"
assert_not_contains "$sql_stdin" "restore_run_checkpoint_id" "obsolete lease restore pointer should be absent"
assert_not_contains "$sql_stdin" "owner_runtime_instance_id" "obsolete wait owner should be absent"
assert_not_contains "$sql_stdin" "dispatch_generation" "obsolete dispatch fence should be absent"

PATH="$tmp/bin:$PATH" \
	HELMR_DATABASE_URL='postgresql://ignored:ignored@ignored-host/ignored_db?host=%2Fvar%2Frun%2Fpostgresql&dbname=real_db&application_name=helmr+smoke&options=-c%20statement_timeout%3D1000' \
	CAPTURED_SERVICE_FILE="$service_file" \
	CAPTURED_SERVICE_NAME="$service_name" \
	CAPTURED_SQL_STDIN="$sql_stdin" \
	CAPTURED_PSQL_CALLS="$psql_calls" \
	"$repo_root/dev/aws/run-path-report.sh" "$run_id" >"$tmp/path-report-overrides.out"

assert_contains "$service_file" "host=/var/run/postgresql" "query host should override URI host"
assert_contains "$service_file" "dbname=real_db" "query dbname should override URI dbname"
assert_contains "$service_file" "application_name=helmr+smoke" "plus should remain literal"
assert_contains "$service_file" "options=-c statement_timeout=1000" "percent-encoded options should decode"
assert_not_contains "$service_file" "host=ignored-host" "URI host should not survive query host override"
assert_not_contains "$service_file" "dbname=ignored_db" "URI dbname should not survive query dbname override"

if PATH="$tmp/bin:$PATH" \
	HELMR_DATABASE_URL='postgresql://u:p@host/db?bad-key=value' \
	CAPTURED_SERVICE_FILE="$service_file" \
	CAPTURED_SERVICE_NAME="$service_name" \
	CAPTURED_SQL_STDIN="$sql_stdin" \
	CAPTURED_PSQL_CALLS="$psql_calls" \
	"$repo_root/dev/aws/run-path-report.sh" "$run_id" >"$tmp/invalid-key.out" 2>"$tmp/invalid-key.err"; then
	fail "invalid query parameter key should fail"
fi
assert_contains "$tmp/invalid-key.err" "invalid DATABASE_URL query parameter: bad-key" "invalid key error"

rm -f "$psql_calls"
if PATH="$tmp/bin:$PATH" \
	HELMR_DATABASE_URL='postgresql://u:p@host/db' \
	CAPTURED_SERVICE_FILE="$service_file" \
	CAPTURED_SERVICE_NAME="$service_name" \
	CAPTURED_SQL_STDIN="$sql_stdin" \
	CAPTURED_PSQL_CALLS="$psql_calls" \
	"$repo_root/dev/aws/run-path-report.sh" not-a-run >"$tmp/bad-uuid.out" 2>"$tmp/bad-uuid.err"; then
	fail "invalid run id should fail"
fi
assert_contains "$tmp/bad-uuid.err" "RUN_ID must be a UUID" "invalid UUID error"
if [ -e "$psql_calls" ]; then
	fail "invalid run id should fail before psql is invoked"
fi

if env -u HELMR_DATABASE_URL -u DATABASE_URL -u HELMR_PATH_REPORT_ALLOW_ECS_TASK \
	"$repo_root/dev/aws/run-path-report.sh" "$run_id" >"$tmp/no-db.out" 2>"$tmp/no-db.err"; then
	fail "local path should require a database URL unless ECS fallback is explicit"
fi
assert_contains "$tmp/no-db.err" "run-path-report requires HELMR_DATABASE_URL/DATABASE_URL" "missing local DB guidance"

printf 'ok - run path report local tests\n'
