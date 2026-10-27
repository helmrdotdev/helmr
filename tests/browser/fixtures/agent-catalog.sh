#!/usr/bin/env bash
# Seed an Agent catalog and synthetic admission capacity in the disposable browser database.
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
INSERT INTO image_specs(environment_id,id,spec_digest,spec,seed)
VALUES(:'environment_id',:'deployment_id','sha256:'||repeat('b',64),'{}','{"profile":"linux-amd64-ext4-v1"}');
INSERT INTO templates(environment_id,id,key) VALUES(:'environment_id',:'deployment_id','browser-computer');
INSERT INTO template_definitions(environment_id,deployment_id,template_id,image_spec_id,cpu_millis,memory_bytes,network_policy,prepare_network_policy)
VALUES(:'environment_id',:'deployment_id',:'deployment_id',:'deployment_id',1000,536870912,'{"kind":"ordinary"}','{"kind":"block"}');
INSERT INTO agents(environment_id,id,key) VALUES(:'environment_id',:'agent_id','browser-agent');
INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy)
VALUES(:'environment_id',:'agent_id',:'deployment_id',:'deployment_id',false,'{"kind":"unrestricted"}');
UPDATE environments SET current_deployment_id=:'deployment_id' WHERE id=:'environment_id';
-- Admission checks physical placement even though browser tests do not run a
-- Worker. One shared synthetic host supplies bounded capacity for these fixtures.
SELECT g.id AS capacity_id FROM worker_groups g JOIN projects p ON p.default_region_id=g.region_id WHERE p.slug='console-demo' AND g.status='active' \gset
INSERT INTO vm_platforms(id,descriptor_digest,firecracker_digest,firecracker_version,snapshot_format_version,host_kernel_release,kernel_digest,initramfs_digest,rootfs_digest)
VALUES('sha256:'||repeat('c',64),'sha256:'||repeat('c',64),'sha256:'||repeat('c',64),'1.16.1','6.0.0','fixture','sha256:'||repeat('c',64),'sha256:'||repeat('c',64),'sha256:'||repeat('c',64)) ON CONFLICT DO NOTHING;
INSERT INTO worker_pools(id,worker_group_id,name,status,sealed_at,vm_platform_id,capacity_cpu_millis,capacity_memory_bytes,capacity_guest_ephemeral_disk_bytes,per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,max_vm_slots)
VALUES(:'capacity_id',:'capacity_id','browser-admission','active',clock_timestamp(),'sha256:'||repeat('c',64),128000,137438953472,137438953472,1000,1073741824,1073741824,128) ON CONFLICT DO NOTHING;
INSERT INTO worker_pool_cpu_shapes(worker_pool_id,vcpu_count,cpu_config_digest) VALUES(:'capacity_id',1,'sha256:'||repeat('c',64)) ON CONFLICT DO NOTHING;
UPDATE worker_groups SET primary_pool_id=:'capacity_id' WHERE id=:'capacity_id';
INSERT INTO worker_hosts(id,resource_id,worker_group_id,worker_pool_id,status,current_epoch,current_service_id,epoch_started_at,activated_at,epoch_cpu_millis,epoch_memory_bytes,epoch_guest_ephemeral_disk_bytes,per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,vm_platform_id,max_vm_slots,max_vm_starts,cpu_environment,cpu_environment_digest,observed_at)
VALUES(:'capacity_id','browser-admission',:'capacity_id',:'capacity_id','active',1,:'capacity_id',clock_timestamp(),clock_timestamp(),128000,137438953472,137438953472,1000,1073741824,1073741824,'sha256:'||repeat('c',64),128,128,'{}','sha256:'||repeat('c',64),clock_timestamp())
ON CONFLICT (id) DO UPDATE SET observed_at=clock_timestamp();
UPDATE worker_hosts h SET resource_observation_sequence=1,
  resource_policy_revision=p.resource_policy_revision,resource_observed_at=clock_timestamp(),
  local_optional_cpu_millis=0,local_optional_memory_bytes=0,optional_disk_bytes=0,
  local_optional_vm_slots=0,local_optional_tasks=0,optional_reclaim_pending=false,
  disk_slots_for_real_work=h.max_vm_slots,ready_leaf_keys='[]'::jsonb
FROM worker_pools p WHERE h.worker_pool_id=p.id AND h.id=:'capacity_id';

COMMIT;
SELECT :'environment_id';
SQL
