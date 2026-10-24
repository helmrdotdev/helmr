#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/migrations"
printf 'SELECT 1;\n' >"$tmp/migrations/fixture.sql"

# Exercise script completion without contacting a service. Real projection tests
# own the ClickHouse schema and insert/read boundary.
cat >"$tmp/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
query=''
while [ "$#" -gt 0 ]; do
  if [ "$1" = --data-binary ]; then
    query=$2
    shift
  fi
  shift
done
if [ "$query" = @- ]; then query=$(cat); fi
printf '%s\n' "$query" >>"$CANARY_QUERY_LOG"
if [[ "$query" == 'SELECT count()'* ]]; then printf '%s\n' "$CANARY_COUNT"; fi
EOF
chmod +x "$tmp/bin/curl"

export PATH="$tmp/bin:$PATH"
export CLICKHOUSE_URL=http://canary.invalid
export CLICKHOUSE_USER=fixture CLICKHOUSE_PASSWORD=fixture
export HELMR_CLICKHOUSE_MIGRATIONS="$tmp/migrations"
export HELMR_CLICKHOUSE_CANARY_ENVIRONMENT_ID=00000000-0000-0000-0000-000000000003
export HELMR_CLICKHOUSE_CANARY_COMMAND_ID=00000000-0000-0000-0000-000000000004
export CANARY_QUERY_LOG="$tmp/queries"

if ! CANARY_COUNT=1 bash "$repo_root/scripts/clickhouse-canary.sh" >"$tmp/success" 2>&1; then
  cat "$tmp/success" >&2
  exit 1
fi
grep -Fx 'clickhouse canary ok environment_id=00000000-0000-0000-0000-000000000003 command_id=00000000-0000-0000-0000-000000000004' "$tmp/success"
grep -F 'INSERT INTO helmr_telemetry.computer_command_logs' "$tmp/queries" >/dev/null
grep -F "WHERE environment_id = '$HELMR_CLICKHOUSE_CANARY_ENVIRONMENT_ID' AND command_id = '$HELMR_CLICKHOUSE_CANARY_COMMAND_ID'" "$tmp/queries" >/dev/null

if CANARY_COUNT=0 bash "$repo_root/scripts/clickhouse-canary.sh" >"$tmp/failure" 2>&1; then
  echo 'canary unexpectedly accepted a missing row' >&2
  exit 1
fi
grep -Fx 'clickhouse canary count=0, want 1' "$tmp/failure"
if grep -F 'clickhouse canary ok' "$tmp/failure" >/dev/null; then
  echo 'canary printed success after a failed count' >&2
  exit 1
fi
printf 'ok - ClickHouse canary completion and failed-count checks\n'
