package dbtest

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func MustExec(t *testing.T, ctx context.Context, executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, query string, args ...any) {
	t.Helper()
	if _, err := executor.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func Digest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return sha256sum.FormatDigest(sum[:])
}

func Hash(seed string) []byte {
	sum := sha256.Sum256([]byte(seed))
	return sum[:]
}

func ShortID(id uuid.UUID) string {
	return strings.ReplaceAll(id.String(), "-", "")[20:]
}

type CheckpointArtifactIDs struct {
	RuntimeConfig uuid.UUID
	VMState       uuid.UUID
	Memory        uuid.UUID
	ScratchDisk   uuid.UUID
}

func InsertCheckpointArtifacts(t *testing.T, ctx context.Context, executor interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, runID uuid.UUID, seed string) CheckpointArtifactIDs {
	t.Helper()
	ids := CheckpointArtifactIDs{
		RuntimeConfig: uuid.NewV7(),
		VMState:       uuid.NewV7(),
		Memory:        uuid.NewV7(),
		ScratchDisk:   uuid.NewV7(),
	}
	digests := []string{
		Digest(seed + "-runtime-config"),
		Digest(seed + "-vm-state"),
		Digest(seed + "-memory"),
		Digest(seed + "-scratch-disk"),
	}
	if err := executor.QueryRow(ctx, `
		WITH authority AS (
			SELECT org_id, project_id, environment_id FROM runs WHERE id = $1
		), descriptors(id, digest, kind, media_type) AS (
			VALUES
				($2::uuid, $6::text, 'run_checkpoint_config'::artifact_kind, 'application/vnd.helmr.checkpoint.runtime-config.v0+json'),
				($3::uuid, $7::text, 'run_checkpoint_vm_state'::artifact_kind, 'application/vnd.helmr.firecracker.vm-state.v0'),
				($4::uuid, $8::text, 'run_checkpoint_memory'::artifact_kind, 'application/vnd.helmr.firecracker.memory.v0+filepack'),
				($5::uuid, $9::text, 'run_checkpoint_scratch_disk'::artifact_kind, 'application/vnd.helmr.firecracker.scratch-disk.v0+filepack')
		), lifetimes AS (
			INSERT INTO cas_blobs (digest, size_bytes) SELECT digest, 1 FROM descriptors ON CONFLICT DO NOTHING
		), inserted_cas AS (
			INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
			SELECT authority.org_id, descriptors.digest, 1, descriptors.media_type
			  FROM authority CROSS JOIN descriptors
			ON CONFLICT (org_id, digest) DO NOTHING
		)
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		SELECT descriptors.id, authority.org_id, authority.project_id, authority.environment_id,
		       descriptors.digest, descriptors.kind, 1, descriptors.media_type
		  FROM authority CROSS JOIN descriptors
		RETURNING artifacts.id
	`, runID, ids.RuntimeConfig, ids.VMState, ids.Memory, ids.ScratchDisk,
		digests[0], digests[1], digests[2], digests[3]).Scan(new(uuid.UUID)); err != nil {
		t.Fatal(err)
	}
	return ids
}

// InsertCommittedComputerRoot supplies an initial publication and its reclaimed
// publisher to tests that start after initialization. It does not verify uploads.
func InsertCommittedComputerRoot(t *testing.T, ctx context.Context, executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, versionID, environmentID, computerID any) {
	t.Helper()
	publisherID, groupID, poolID, workerID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	digest := Digest(publisherID.String())
	MustExec(t, ctx, executor, `INSERT INTO worker_group_tokens(id,token_hash) VALUES($1,$2)`, groupID, Hash(groupID.String()))
	MustExec(t, ctx, executor, `INSERT INTO worker_groups(id,token_id,region_id,name,status)
 SELECT $1,$1,region_id,$2,'disabled' FROM computers WHERE id=$3`, groupID, "publisher-"+groupID.String(), computerID)
	MustExec(t, ctx, executor, `INSERT INTO worker_pools(id,worker_group_id,name) VALUES($1,$2,'publisher')`, poolID, groupID)
	MustExec(t, ctx, executor, `INSERT INTO worker_instances(id,resource_id,worker_group_id,worker_pool_id,status,lost_at)
 VALUES($1,$2,$3,$4,'lost',now())`, workerID, "publisher-"+workerID.String(), groupID, poolID)
	MustExec(t, ctx, executor, `INSERT INTO runtime_identities(id,runtime_arch,vm_runtime_contract,vm_runtime_descriptor_digest,
 firecracker_digest,firecracker_version,snapshot_format_version,host_kernel_release,cpu_template_kind,kernel_digest,initramfs_digest,rootfs_digest)
 VALUES($1,'x86_64','helmr.vm-runtime.v0',$1,$1,'1.16.1','6.0.0','fixture','none',$1,$1,$1)`, digest)
	MustExec(t, ctx, executor, `INSERT INTO runtime_instances(id,org_id,project_id,environment_id,region_id,worker_group_id,worker_instance_id,
 runtime_identity_id,deployment_definition_id,worker_epoch,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,
 reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,workspace_id,preparation_expires_at,desired_state,desired_version,desired_reason,
 observed_state,terminal_at,reclaimed_at,reclaim_evidence,terminal_reason_code)
 SELECT $1,e.org_id,e.project_id,c.environment_id,c.region_id,$2,$3,$4,c.deployment_definition_id,1,1,$4,1000,4096,4096,1,c.id,
 now(),'closed',2,'initialization_completed','closed',now(),now(),'{}','initialization_completed'
 FROM computers c JOIN environments e ON e.id=c.environment_id WHERE c.id=$5`, publisherID, groupID, workerID, digest, computerID)
	MustExec(t, ctx, executor, `INSERT INTO computer_versions(id,environment_id,computer_id,root_pack_digest,logical_bytes,
 status,ownership_generation,writer_generation,published_at,publisher_runtime_instance_id,publisher_desired_version,publication_request_fingerprint)
 VALUES($1,$2,$3,$4,4096,'committed',0,0,now(),$5,1,$6)`, versionID, environmentID, computerID, digest, publisherID, Hash(publisherID.String()))
}
