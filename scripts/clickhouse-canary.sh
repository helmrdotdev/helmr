#!/usr/bin/env bash
set -euo pipefail

url="${CLICKHOUSE_URL:-http://127.0.0.1:8123}"
user="${CLICKHOUSE_USER:-default}"
password="${CLICKHOUSE_PASSWORD:-}"
migration_dir="${HELMR_CLICKHOUSE_MIGRATIONS:-internal/clickhouse/schema/migrations}"

curl_args=(-fsS --user "${user}:${password}")

new_uuid() {
  if command -v uuidgen >/dev/null 2>&1; then
    uuidgen | tr '[:upper:]' '[:lower:]'
    return
  fi
  python3 - <<'PY'
import uuid
print(uuid.uuid4())
PY
}

for migration in "${migration_dir}"/*.sql; do
  while IFS= read -r -d $'\036' statement; do
    if [[ -z "${statement//[[:space:]]/}" ]]; then
      continue
    fi
    curl "${curl_args[@]}" --data-binary "${statement}" "${url}/"
  done < <(awk 'BEGIN { RS = ";"; ORS = "\036" } NF { print }' "${migration}")
done

environment_id="${HELMR_CLICKHOUSE_CANARY_ENVIRONMENT_ID:-00000000-0000-0000-0000-000000000003}"
command_id="${HELMR_CLICKHOUSE_CANARY_COMMAND_ID:-$(new_uuid)}"

curl "${curl_args[@]}" "${url}/" --data-binary @- <<SQL
INSERT INTO helmr_telemetry.computer_command_logs
    (environment_id, command_id, producer_epoch, stream, sequence, through_sequence, kind, observed_at_unix_nano, data, dropped_bytes, complete, byte_offset, through_byte_offset, accepted_at, expires_at)
SELECT
    '${environment_id}', '${command_id}', 1, 'stdout', 1, 1, 'end', 1, '', 0, true, 0, 0, now64(6) AS accepted_at, accepted_at + INTERVAL 90 DAY;
SQL

count="$(
  curl "${curl_args[@]}" "${url}/" --data-binary "SELECT count() FROM helmr_telemetry.computer_command_logs FINAL WHERE environment_id = '${environment_id}' AND command_id = '${command_id}' AND kind = 'end'"
)"

if [[ "${count}" != "1" ]]; then
  echo "clickhouse canary count=${count}, want 1" >&2
  exit 1
fi

printf 'clickhouse canary ok environment_id=%s command_id=%s\n' "${environment_id}" "${command_id}"
