#!/usr/bin/env bash
# Seed a definition-free deployment for Agent deletion acceptance.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
if [[ -n "${HELMR_E2E_BASE_URL:-}" || -n "${DATABASE_URL:-}" ]]; then
  echo "Agent catalog fixture requires the managed browser stack" >&2
  exit 1
fi
[[ $# = 2 ]] || exit 1
[[ "$1" =~ ^browser-agent-[a-z0-9-]+$ ]] || exit 1
for id in "${@:2}"; do
  [[ "$id" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] || exit 1
done
export HELMR_DEV_DIR="${ROOT}/.helmr-dev-e2e"
export HELMR_DEV_POSTGRES_PORT="$(( ${HELMR_E2E_PORT:-4173} + 1 ))"
source "${ROOT}/dev/local/instance.sh"
helmr_dev_init "${ROOT}"
kill -0 "$(helmr_dev_lock_holder_pid)"
pg_ctl -D "${HELMR_DEV_PGDATA}" status >/dev/null
actual_data_dir="$(psql -X -At -h "${HELMR_DEV_PGSOCKET}" -p "${HELMR_DEV_POSTGRES_PORT}" -d postgres -c 'SHOW data_directory')"
[[ "$actual_data_dir" = "${HELMR_DEV_PGDATA}" ]] || exit 1
psql -X -q -At -v ON_ERROR_STOP=1 -h "${HELMR_DEV_PGSOCKET}" -p "${HELMR_DEV_POSTGRES_PORT}" -d postgres \
  -v environment="$1" -v deployment_id="$2" <<'SQL'
BEGIN;
SELECT e.id AS environment_id FROM environments e JOIN projects p ON p.id=e.project_id WHERE p.slug='console-demo' AND e.slug=:'environment' \gset
INSERT INTO deployments(environment_id,id,bundle_digest) VALUES(:'environment_id',:'deployment_id','sha256:'||repeat('d',64));
COMMIT;
SQL
