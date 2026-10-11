#!/usr/bin/env bash
# Seed runtime-phase records only in the disposable browser database. This does
# not run a Worker or publish a Save. Reads, answers and controls use the real API.
# The fourth ID identifies the Ask fixture, or the Secret for expose-secret.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
if [[ -n "${HELMR_E2E_BASE_URL:-}" || -n "${DATABASE_URL:-}" ]]; then
  echo "Conversation fixture requires the managed browser stack" >&2
  exit 1
fi
[[ $# = 5 ]] || exit 1
for id in "${@:1:4}"; do
  [[ "$id" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] || exit 1
done
case "$5" in text|choice|finalizing|output|disable-member|restore-member|expose-secret) ;; *) exit 1;; esac
export HELMR_DEV_DIR="${ROOT}/.helmr-dev-e2e"
export HELMR_DEV_POSTGRES_PORT="$(( ${HELMR_E2E_PORT:-4173} + 1 ))"
source "${ROOT}/dev/local/instance.sh"
helmr_dev_init "${ROOT}"
kill -0 "$(helmr_dev_lock_holder_pid)"
pg_ctl -D "${HELMR_DEV_PGDATA}" status >/dev/null
actual_data_dir="$(psql -X -At -h "${HELMR_DEV_PGSOCKET}" -p "${HELMR_DEV_POSTGRES_PORT}" -d postgres -c 'SHOW data_directory')"
[[ "$actual_data_dir" = "${HELMR_DEV_PGDATA}" ]] || exit 1
psql -X -v ON_ERROR_STOP=1 -h "${HELMR_DEV_PGSOCKET}" -p "${HELMR_DEV_POSTGRES_PORT}" -d postgres \
  -v environment_id="$1" -v session_id="$2" -v turn_id="$3" -v reference_id="$4" -v mode="$5" <<'SQL'
BEGIN;
CREATE TEMP TABLE fixture ON COMMIT DROP AS
SELECT e.id AS environment_id,e.org_id,s.id AS session_id,s.computer_id,t.id AS turn_id,t.caller_id AS user_id
FROM environments e JOIN sessions s ON s.environment_id=e.id JOIN turns t ON t.environment_id=e.id AND t.session_id=s.id
WHERE e.id=:'environment_id' AND e.slug LIKE 'browser-agent-%' AND s.id=:'session_id' AND t.id=:'turn_id'
 AND t.caller_kind='user' AND t.admission_method='start';
DO $$ BEGIN IF (SELECT count(*) FROM fixture) <> 1 THEN RAISE EXCEPTION 'not an owned browser Session'; END IF; END $$;
SELECT :'mode' IN ('text','choice') AS seed, :'mode'='finalizing' AS finalizing,
 :'mode'='expose-secret' AS expose_secret, :'mode'='output' AS output, :'mode' IN ('disable-member','restore-member') AS membership \gset
\if :seed
INSERT INTO regions(id,display_name) VALUES('browser-'||:'session_id','Browser fixture');
INSERT INTO worker_group_tokens(id,token_hash) VALUES(:'reference_id',sha256(convert_to(:'reference_id','UTF8')));
INSERT INTO worker_groups(id,token_id,region_id,name) VALUES(:'reference_id',:'reference_id','browser-'||:'session_id','browser-fixture');
INSERT INTO vm_platforms(id,arch,contract,descriptor_digest,firecracker_digest,firecracker_version,snapshot_format_version,host_kernel_release,cpu_template_kind,kernel_digest,initramfs_digest,rootfs_digest)
VALUES('sha256:'||repeat('c',64),'x86_64','helmr.vm-runtime.v0','sha256:'||repeat('c',64),'sha256:'||repeat('c',64),'1.16.1','6.0.0','fixture','none','sha256:'||repeat('c',64),'sha256:'||repeat('c',64),'sha256:'||repeat('c',64)) ON CONFLICT DO NOTHING;
INSERT INTO worker_pools(id,worker_group_id,name,status,sealed_at,vm_platform_id,capacity_cpu_millis,capacity_memory_bytes,capacity_guest_ephemeral_disk_bytes,per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,max_vm_slots)
VALUES(:'reference_id',:'reference_id','browser-fixture','active',clock_timestamp(),'sha256:'||repeat('c',64),1000,1073741824,1073741824,1000,1073741824,1073741824,1);
INSERT INTO worker_pool_cpu_shapes(worker_pool_id,vcpu_count,cpu_config_digest) VALUES(:'reference_id',1,'sha256:'||repeat('c',64));
INSERT INTO worker_hosts(id,resource_id,worker_group_id,worker_pool_id,status,current_epoch,current_service_id,epoch_started_at,activated_at,epoch_cpu_millis,epoch_memory_bytes,per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,vm_platform_id,max_vm_slots,max_vm_starts,cpu_environment,cpu_environment_digest)
VALUES(:'reference_id','browser-'||:'session_id',:'reference_id',:'reference_id','active',1,:'reference_id',clock_timestamp(),clock_timestamp(),1000,1073741824,1000,1073741824,1073741824,'sha256:'||repeat('c',64),1,1,'{}','sha256:'||repeat('c',64));
INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,status,computer_instance_id,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at)
SELECT environment_id,computer_id,1,:'reference_id',1,clock_timestamp()+interval '1 hour','active',computer_id,sha256(convert_to(:'reference_id','UTF8')),1000,536870912,1073741824,'sha256:'||repeat('c',64),1,'sha256:'||repeat('c',64),clock_timestamp(),clock_timestamp() FROM fixture;
INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status)
SELECT environment_id,session_id,1,computer_id,1,'ready' FROM fixture;
UPDATE turns SET status='running',process_epoch=1,started_at=clock_timestamp() WHERE environment_id=:'environment_id' AND id=:'turn_id';
WITH allocated AS (UPDATE sessions SET next_event_seq=next_event_seq+1 WHERE environment_id=:'environment_id' AND id=:'session_id' RETURNING next_event_seq-1 AS seq)
INSERT INTO session_events(environment_id,session_id,turn_id,seq,kind,data)
SELECT :'environment_id',:'session_id',:'turn_id',seq,'turn.output',convert_to('[{"type":"text","text":"First visible progress"},{"type":"json","value":{"context":null}}]','UTF8') FROM allocated;
CREATE TEMP TABLE question ON COMMIT DROP AS SELECT convert_to(CASE WHEN :'mode'='choice' THEN
 '{"prompt":[{"type":"text","text":"Choose an answer"}],"answer":{"type":"choice","multiple":true,"allowText":true,"options":[{"id":"a","label":"First","value":{"v":1}},{"id":"b","label":"Second","value":null}]}}'
 ELSE '{"prompt":[{"type":"text","text":"Answer this question"}],"answer":{"type":"text"}}' END,'UTF8') AS value;
WITH allocated AS (UPDATE sessions SET next_event_seq=next_event_seq+1 WHERE environment_id=:'environment_id' AND id=:'session_id' RETURNING next_event_seq-1 AS seq),
created AS (INSERT INTO session_events(environment_id,session_id,turn_id,seq,kind,data)
 SELECT :'environment_id',:'session_id',:'turn_id',seq,'ask.created',convert_to(jsonb_build_object('ask_id',:'reference_id','question',convert_from(value,'UTF8')::jsonb)::text,'UTF8') FROM allocated CROSS JOIN question RETURNING seq)
INSERT INTO turn_asks(environment_id,session_id,turn_id,id,process_epoch,created_event_seq,question_digest,question)
SELECT :'environment_id',:'session_id',:'turn_id',:'reference_id',1,seq,sha256(value),value FROM created CROSS JOIN question;
\endif
\if :finalizing
UPDATE turns SET status='finalizing',processing_closed_at=clock_timestamp(),result_recorded_at=clock_timestamp(),
 result='{"private":"PRIVATE_RETURN"}',result_digest=sha256(convert_to('{"private":"PRIVATE_RETURN"}','UTF8')),drain_evidence='browser phase fixture',
 response_id=uuidv7(),response=convert_to('[{"type":"text","text":"RESPONSE_REQUIRES_OWN_SAVE"}]','UTF8'),
 response_digest=sha256(convert_to('[{"type":"text","text":"RESPONSE_REQUIRES_OWN_SAVE"}]','UTF8')),response_staged_at=clock_timestamp()
WHERE environment_id=:'environment_id' AND id=:'turn_id' AND status='running' RETURNING id AS finalizing_turn_id \gset
INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq,turn_id)
SELECT environment_id,turn_id,computer_id,1,1,turn_id FROM fixture;
\endif
\if :output
WITH allocated AS (UPDATE sessions SET next_event_seq=next_event_seq+1 WHERE environment_id=:'environment_id' AND id=:'session_id' RETURNING next_event_seq-1 AS seq)
INSERT INTO session_events(environment_id,session_id,turn_id,seq,kind,data)
SELECT :'environment_id',:'session_id',:'turn_id',seq,'turn.output',convert_to('[{"type":"text","text":"Second visible progress"}]','UTF8') FROM allocated;
\endif
\if :expose_secret
INSERT INTO secret_exposures(environment_id,session_id,process_epoch,secret_id,version_id,revocation_generation)
SELECT f.environment_id,f.session_id,t.process_epoch,s.id,s.current_version_id,s.revocation_generation
FROM fixture f JOIN turns t ON (t.environment_id,t.id)=(f.environment_id,f.turn_id)
JOIN secrets s ON s.environment_id=f.environment_id AND s.id=:'reference_id' AND s.status='active'
WHERE t.status='running' RETURNING secret_id AS exposed_secret_id \gset
\endif
\if :membership
UPDATE org_members m SET disabled_at=CASE WHEN :'mode'='disable-member' THEN clock_timestamp() ELSE NULL END
FROM fixture f WHERE m.org_id=f.org_id AND m.user_id=f.user_id;
\endif
COMMIT;
SQL
