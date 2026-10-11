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

org_id="${HELMR_CLICKHOUSE_CANARY_ORG_ID:-00000000-0000-0000-0000-000000000001}"
project_id="${HELMR_CLICKHOUSE_CANARY_PROJECT_ID:-00000000-0000-0000-0000-000000000002}"
environment_id="${HELMR_CLICKHOUSE_CANARY_ENVIRONMENT_ID:-00000000-0000-0000-0000-000000000003}"
subject_id="${HELMR_CLICKHOUSE_CANARY_SUBJECT_ID:-$(new_uuid)}"
idem="canary:${org_id}:${subject_id}"

curl "${curl_args[@]}" "${url}/" --data-binary @- <<SQL
INSERT INTO helmr_telemetry.events
    (org_id, project_id, environment_id, subject_kind, subject_id, event_kind, seq, message, body, idempotency_key, retention_class, redaction_class, source, observed_at, accepted_at)
VALUES
    ('${org_id}', '${project_id}', '${environment_id}', 'canary', '${subject_id}', 'canary.ready', 1, 'ok', '{}', '${idem}', 'standard', 'internal', 'canary', now64(3), now64(3));
SQL

count="$(
  curl "${curl_args[@]}" "${url}/" --data-binary "SELECT count() FROM helmr_telemetry.events FINAL WHERE org_id = '${org_id}' AND subject_kind = 'canary' AND subject_id = '${subject_id}' AND idempotency_key = '${idem}' FORMAT TabSeparatedRaw"
)"

if [[ "${count}" != "1" ]]; then
  echo "clickhouse canary count=${count}, want 1" >&2
  exit 1
fi

printf 'clickhouse canary ok org_id=%s subject_id=%s\n' "${org_id}" "${subject_id}"
