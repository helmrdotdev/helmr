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

inner="$tmp/db-query-inner.sh"
awk '
	BEGIN { in_command = 0 }
	/^command='\''$/ { in_command = 1; next }
	in_command && /^'\''$/ { exit }
	in_command { print }
' "$repo_root/dev/aws/db-query.sh" >"$inner"

mkdir -p "$tmp/bin"
cat >"$tmp/bin/psql" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
cp "${PGSERVICEFILE:?PGSERVICEFILE is required}" "${CAPTURED_SERVICE_FILE:?CAPTURED_SERVICE_FILE is required}"
printf '%s\n' "${PGSERVICE:?PGSERVICE is required}" >"${CAPTURED_SERVICE_NAME:?CAPTURED_SERVICE_NAME is required}"
cat >"${CAPTURED_SQL_STDIN:?CAPTURED_SQL_STDIN is required}"
EOF
chmod +x "$tmp/bin/psql"

service_file="$tmp/service.conf"
service_name="$tmp/service-name.txt"
sql_stdin="$tmp/sql-stdin.sql"

PATH="$tmp/bin:$PATH" \
	DATABASE_URL='postgresql://user%40name:p%40ss%3Aword@[::1]:6543/main_db?connect_timeout=7&target_session_attrs=read-write&channel_binding=prefer' \
	SQL='select 1;' \
	CAPTURED_SERVICE_FILE="$service_file" \
	CAPTURED_SERVICE_NAME="$service_name" \
	CAPTURED_SQL_STDIN="$sql_stdin" \
	sh "$inner"

assert_contains "$service_file" "[helmr_db_query]" "service header"
assert_contains "$service_file" "host=::1" "IPv6 host should be unbracketed"
assert_contains "$service_file" "port=6543" "port should be preserved"
assert_contains "$service_file" "dbname=main_db" "database name should be preserved"
assert_contains "$service_file" "user=user@name" "encoded user should decode percent escapes"
assert_contains "$service_file" "password=p@ss:word" "encoded password should decode percent escapes"
assert_contains "$service_file" "target_session_attrs=read-write" "unknown libpq parameter should be preserved"
assert_contains "$service_file" "channel_binding=prefer" "second unknown libpq parameter should be preserved"
assert_contains "$service_name" "helmr_db_query" "service name"
assert_contains "$sql_stdin" "select 1;" "SQL should be sent on stdin"

PATH="$tmp/bin:$PATH" \
	DATABASE_URL='postgresql://ignored:ignored@ignored-host/ignored_db?host=%2Fvar%2Frun%2Fpostgresql&dbname=real_db&application_name=helmr+smoke&options=-c%20statement_timeout%3D1000' \
	SQL='select 2;' \
	CAPTURED_SERVICE_FILE="$service_file" \
	CAPTURED_SERVICE_NAME="$service_name" \
	CAPTURED_SQL_STDIN="$sql_stdin" \
	sh "$inner"

assert_contains "$service_file" "host=/var/run/postgresql" "query host should override URI host"
assert_contains "$service_file" "dbname=real_db" "query dbname should override URI dbname"
assert_contains "$service_file" "application_name=helmr+smoke" "plus should remain literal"
assert_contains "$service_file" "options=-c statement_timeout=1000" "percent-encoded options should decode"
assert_not_contains "$service_file" "host=ignored-host" "URI host should not survive query host override"
assert_not_contains "$service_file" "dbname=ignored_db" "URI dbname should not survive query dbname override"

if PATH="$tmp/bin:$PATH" \
	DATABASE_URL='postgresql://u:p@host/db?bad-key=value' \
	SQL='select 3;' \
	CAPTURED_SERVICE_FILE="$service_file" \
	CAPTURED_SERVICE_NAME="$service_name" \
	CAPTURED_SQL_STDIN="$sql_stdin" \
	sh "$inner" >"$tmp/invalid-key.out" 2>"$tmp/invalid-key.err"; then
	fail "invalid query parameter key should fail"
fi
assert_contains "$tmp/invalid-key.err" "invalid DATABASE_URL query parameter: bad-key" "invalid key error"

if PATH="$tmp/bin:$PATH" \
	DATABASE_URL='postgresql://u:p@[::1]extra/db' \
	SQL='select 4;' \
	CAPTURED_SERVICE_FILE="$service_file" \
	CAPTURED_SERVICE_NAME="$service_name" \
	CAPTURED_SQL_STDIN="$sql_stdin" \
	sh "$inner" >"$tmp/invalid-ipv6.out" 2>"$tmp/invalid-ipv6.err"; then
	fail "invalid bracketed host should fail"
fi
assert_contains "$tmp/invalid-ipv6.err" "invalid bracketed DATABASE_URL host" "invalid IPv6 host error"

printf 'ok - db query service file tests\n'
