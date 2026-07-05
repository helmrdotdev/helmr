#!/usr/bin/env bash
set -euo pipefail

url="${HELMR_CLICKHOUSE_URL:-http://127.0.0.1:8123}"
user="${HELMR_CLICKHOUSE_USER:-default}"
password="${HELMR_CLICKHOUSE_PASSWORD:-}"
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
  curl "${curl_args[@]}" --data-binary @"${migration}" "${url}/"
done

cell_id="${HELMR_CELL_ID:-}"
if [[ -z "${cell_id}" ]]; then
  echo "HELMR_CELL_ID is required" >&2
  exit 1
fi
org_id="${HELMR_CLICKHOUSE_CANARY_ORG_ID:-00000000-0000-0000-0000-000000000001}"
project_id="${HELMR_CLICKHOUSE_CANARY_PROJECT_ID:-00000000-0000-0000-0000-000000000002}"
environment_id="${HELMR_CLICKHOUSE_CANARY_ENVIRONMENT_ID:-00000000-0000-0000-0000-000000000003}"
run_id="${HELMR_CLICKHOUSE_CANARY_RUN_ID:-$(new_uuid)}"
idem="canary:${cell_id}:${run_id}"

curl "${curl_args[@]}" "${url}/" --data-binary @- <<SQL
INSERT INTO helmr_telemetry.run_logs
    (cell_id, org_id, project_id, environment_id, run_id, attempt_number, stream_name, seq, observed_seq, content, size_bytes, idempotency_key, retention_class, redaction_class, source, observed_at)
VALUES
    ('${cell_id}', '${org_id}', '${project_id}', '${environment_id}', '${run_id}', 1, 'stdout', 1, 1, 'ok', 2, '${idem}', 'hot', 'internal', 'canary', now64(3));
SQL

count="$(
  curl "${curl_args[@]}" "${url}/" --data-binary "SELECT count() FROM helmr_telemetry.run_logs FINAL WHERE cell_id = '${cell_id}' AND run_id = '${run_id}' AND idempotency_key = '${idem}' FORMAT TabSeparatedRaw"
)"

if [[ "${count}" != "1" ]]; then
  echo "clickhouse canary count=${count}, want 1" >&2
  exit 1
fi

printf 'clickhouse canary ok cell_id=%s run_id=%s\n' "${cell_id}" "${run_id}"
