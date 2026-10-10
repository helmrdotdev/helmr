package agenttest

import (
	"crypto/sha256"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const ChannelCredential = "test-computer-channel"

type Fixture struct {
	Pool                                                                   *pgxpool.Pool
	Environment, Session, Computer, User, Worker, Agent, Deployment, Group uuid.UUID
}

func New(t *testing.T) Fixture {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	f := Fixture{Pool: database.Pool, Environment: uuid.NewV7(), Session: uuid.NewV7(), Computer: uuid.NewV7(), User: uuid.NewV7(), Worker: uuid.NewV7(), Agent: uuid.NewV7(), Deployment: uuid.NewV7()}
	org, project, token, group, pool := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	f.Group = group
	initial := newInitialRoot(t, f.Environment)
	channelDigest := sha256.Sum256([]byte(ChannelCredential))
	_, err := f.Pool.Exec(t.Context(), `
      INSERT INTO regions(id,display_name) VALUES('test','Test');
      INSERT INTO organizations(id,name,slug) VALUES($1,'Org','org');
      INSERT INTO users(id,display_name) VALUES($2,'Developer');
      INSERT INTO org_members(org_id,user_id,role) VALUES($1,$2,'developer');
      INSERT INTO projects(id,org_id,default_region_id,slug,name) VALUES($3,$1,'test','project','Project');
      INSERT INTO environments(history_retention_mode,id,org_id,project_id,slug,name,color_hex) VALUES('until_environment_deletion',$4,$1,$3,'test','Test','#112233');
      UPDATE environments SET max_outstanding_admissions=1024,max_causal_depth=32,max_resident_computers=1024,max_cpu_millis=1024000,max_memory_bytes=1099511627776,max_reserved_storage_bytes=1099511627776,preparation_timeout_ms=600000,admission_rate_per_second=1000000,admission_burst=1000000,admission_tokens=1000000,admission_refilled_at=clock_timestamp() WHERE id=$4;
      INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($4,$5,$6);
      UPDATE environments SET current_deployment_id=$5 WHERE id=$4;
      INSERT INTO computer_preparation_specs(environment_id,id,spec_digest,spec,seed) VALUES($4,$5,$6,'{}','{}');
      INSERT INTO computer_definitions(environment_id,deployment_id,definition_key,preparation_spec_id,resources) VALUES($4,$5,'fixture-computer',$5,'{}');
      INSERT INTO agents(environment_id,id,name) VALUES($4,$7,'agent');
      INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,definition_key,computer_definition_key,setup,triggers) VALUES($4,$7,$5,'agent','fixture-computer',false,'{}');
      INSERT INTO computer_data_keys(id,environment_id,wrapping_key_id,wrapped_key) VALUES($15,$4,'test',decode('01','hex'));
      INSERT INTO cas_blobs(digest,size_bytes) VALUES($16,$17);
      INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$16,$17,'application/octet-stream');
      INSERT INTO computer_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at) VALUES($4,$16,$1,$3,$17,'application/octet-stream','root',$18,$19,clock_timestamp());
      INSERT INTO computer_object_keys(environment_id,digest,key_id,is_direct) VALUES($4,$16,$15,true);
      INSERT INTO computer_disk_roots(environment_id,id,locator) VALUES($4,$20,$21);
      INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest) VALUES($4,$8,$20,decode(substring($22::text from 8),'hex'));
      INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth) VALUES('until_environment_deletion',$4,$9,$7,$5,$8,$9,0);
      INSERT INTO worker_group_tokens(id,token_hash) VALUES($10,decode(repeat('00',32),'hex'));
      INSERT INTO worker_groups(id,token_id,region_id,name) VALUES($11,$10,'test','group');
      INSERT INTO vm_platforms(id,arch,contract,descriptor_digest,firecracker_digest,firecracker_version,snapshot_format_version,host_kernel_release,cpu_template_kind,kernel_digest,initramfs_digest,rootfs_digest) VALUES($6,'x86_64','helmr.vm-runtime.v0',$6,$6,'1.16.1','6.0.0','test','none',$6,$6,$6);
      INSERT INTO worker_pools(id,worker_group_id,name,status,sealed_at,vm_platform_id,capacity_cpu_millis,capacity_memory_bytes,capacity_guest_ephemeral_disk_bytes,per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,max_vm_slots) VALUES($12,$11,'pool','active',clock_timestamp(),$6,1000,1073741824,1073741824,1000,1073741824,1073741824,1);
      INSERT INTO worker_pool_cpu_shapes(worker_pool_id,vcpu_count,cpu_config_digest) VALUES($12,1,$6);
      INSERT INTO worker_hosts(id,resource_id,worker_group_id,worker_pool_id,status,current_epoch,current_service_id,epoch_started_at,activated_at,epoch_cpu_millis,epoch_memory_bytes,per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,vm_platform_id,max_vm_slots,max_vm_starts,cpu_environment,cpu_environment_digest) VALUES($13,'host',$11,$12,'active',1,$13,clock_timestamp(),clock_timestamp(),1000,1073741824,1000,1073741824,1073741824,$6,1,1,'{}'::jsonb,$6);
      INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,status,computer_instance_id,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at) VALUES($4,$8,1,$13,1,clock_timestamp()+interval '1 hour','active',$8,$14,1000,536870912,1073741824,'sha256:1111111111111111111111111111111111111111111111111111111111111111',1,'sha256:1111111111111111111111111111111111111111111111111111111111111111',clock_timestamp(),clock_timestamp());
      INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status) VALUES($4,$9,1,$8,1,'ready');
    `, pgx.QueryExecModeSimpleProtocol, org, f.User, project, f.Environment, f.Deployment, "sha256:"+strings.Repeat("1", 64), f.Agent, f.Computer, f.Session, token, group, pool, f.Worker, channelDigest[:], initial.key, initial.root.Pack.Digest, initial.root.Pack.SizeBytes, initial.root.Pack.Rank, string(initial.inspection), initial.id, string(initial.locator), initial.identity)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
