package computerdbtest

import (
	"context"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5/pgconn"
)

// InsertCommittedComputerRoot supplies an initial publication and its reclaimed
// publisher to tests that start after initialization. It does not verify uploads.
func InsertCommittedComputerRoot(t *testing.T, ctx context.Context, executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, versionID, environmentID, computerID any) {
	t.Helper()
	dbtest.MustExec(t, ctx, executor, `UPDATE computers SET writer_generation=greatest(writer_generation,1) WHERE environment_id=$1 AND id=$2`, environmentID, computerID)
	publisherID, groupID, poolID, workerID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	digest := dbtest.Digest(publisherID.String())
	dbtest.MustExec(t, ctx, executor, `INSERT INTO worker_group_tokens(id,token_hash) VALUES($1,$2)`, groupID, dbtest.Hash(groupID.String()))
	dbtest.MustExec(t, ctx, executor, `INSERT INTO worker_groups(id,token_id,region_id,name,status)
 SELECT $1,$1,region_id,$2,'disabled' FROM computers WHERE id=$3`, groupID, "publisher-"+groupID.String(), computerID)
	dbtest.MustExec(t, ctx, executor, `INSERT INTO worker_pools(id,worker_group_id,name) VALUES($1,$2,'publisher')`, poolID, groupID)
	dbtest.MustExec(t, ctx, executor, `INSERT INTO worker_hosts(id,resource_id,worker_group_id,worker_pool_id,status,lost_at)
 VALUES($1,$2,$3,$4,'lost',now())`, workerID, "publisher-"+workerID.String(), groupID, poolID)
	dbtest.MustExec(t, ctx, executor, `INSERT INTO vm_platforms(id,arch,contract,descriptor_digest,
 firecracker_digest,firecracker_version,snapshot_format_version,host_kernel_release,cpu_template_kind,kernel_digest,initramfs_digest,rootfs_digest)
 VALUES($1,'x86_64','helmr.vm-runtime.v0',$1,$1,'1.16.1','6.0.0','fixture','none',$1,$1,$1)`, digest)
	dbtest.MustExec(t, ctx, executor, `INSERT INTO computer_instances(id,org_id,project_id,environment_id,region_id,worker_group_id,worker_host_id,
 vm_platform_id,
        computer_spec_id,worker_epoch,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,
 reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,computer_id,preparation_expires_at,desired_state,desired_version,desired_reason,
 observed_state,terminal_at,reclaimed_at,reclaim_evidence,terminal_reason_code,
 writer_generation,writer_token_hash,writer_expires_at,admission_state,mount_state,unmounted_at)
 SELECT $1,e.org_id,e.project_id,c.environment_id,c.region_id,$2,$3,$4,
        c.computer_spec_id,1,1,$4,1000,4096,4096,1,c.id,
 now(),'closed',2,'initialization_completed','closed',now(),now(),'{}','initialization_completed',
 1,decode(repeat('01',32),'hex'),now(),'closed','unmounted',now()

 FROM computers c JOIN environments e ON e.id=c.environment_id WHERE c.id=$5`, publisherID, groupID, workerID, digest, computerID)
	dbtest.MustExec(t, ctx, executor, `INSERT INTO computer_disk_versions(id,environment_id,computer_id,root_pack_digest,logical_bytes,
 status,writer_generation,published_at,publisher_computer_instance_id,publisher_desired_version,publication_request_fingerprint)
 VALUES($1,$2,$3,$4,4096,'committed',0,now(),$5,1,$6)`, versionID, environmentID, computerID, digest, publisherID, dbtest.Hash(publisherID.String()))
}
