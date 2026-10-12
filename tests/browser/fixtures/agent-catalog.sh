#!/usr/bin/env bash
# Seed only an Agent catalog in the disposable managed browser database.
# Session admission and controls are exercised through the real HTTP API.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
if [[ -n "${HELMR_E2E_BASE_URL:-}" || -n "${DATABASE_URL:-}" ]]; then
  echo "Agent catalog fixture requires the managed browser stack" >&2
  exit 1
fi
[[ $# = 3 ]] || exit 1
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
  -v environment="$1" -v deployment_id="$2" -v agent_id="$3" <<'SQL'
BEGIN;
SELECT e.id AS environment_id FROM environments e JOIN projects p ON p.id=e.project_id WHERE p.slug='console-demo' AND e.slug=:'environment' \gset
UPDATE environments SET max_outstanding_admissions=100,max_causal_depth=8,
  max_resident_computers=10,max_cpu_millis=10000,max_memory_bytes=10737418240,
  max_reserved_storage_bytes=1099511627776,preparation_timeout_ms=600000,
  admission_rate_per_second=100,admission_burst=100,admission_tokens=100,
  admission_refilled_at=clock_timestamp()
WHERE id=:'environment_id' AND slug LIKE 'browser-agent-%';
INSERT INTO deployments(environment_id,id,bundle_digest)
SELECT id,:'deployment_id','sha256:'||repeat('b',64) FROM environments
WHERE id=:'environment_id' AND slug LIKE 'browser-agent-%';
INSERT INTO computer_preparation_specs(environment_id,id,spec_digest,spec,seed)
VALUES(:'environment_id',:'deployment_id','sha256:'||repeat('b',64),'{}','{"profile":"linux-amd64-ext4-v1"}');
INSERT INTO templates(environment_id,id,key) VALUES(:'environment_id',:'deployment_id','browser-computer');
INSERT INTO template_definitions(environment_id,deployment_id,template_id,preparation_spec_id,resources)
VALUES(:'environment_id',:'deployment_id',:'deployment_id',:'deployment_id','{"milliCpu":1000,"memoryMiB":512}');
INSERT INTO agents(environment_id,id,key) VALUES(:'environment_id',:'agent_id','browser-agent');
INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,setup,triggers)
VALUES(:'environment_id',:'agent_id',:'deployment_id',:'deployment_id',false,'{}');
UPDATE environments SET current_deployment_id=:'deployment_id' WHERE id=:'environment_id';
COMMIT;
SELECT :'environment_id';
SQL
