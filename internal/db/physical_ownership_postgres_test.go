package db_test

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestSchemaComputerControlCheckpointOwnership(t *testing.T) {
	f := sessiontest.New(t)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	save, checkpoint, control, other := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq) VALUES($1,$2,$3,1,1)`, f.Environment, save, f.Computer)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO checkpoints(environment_id,id,computer_id,source_lease_epoch,control_version,disk_save_id,status,capture_request,capture_expires_at,capture_digest) VALUES($1,$2,$3,1,1,$4,'capturing',decode('01','hex'),clock_timestamp()+interval '1 minute',decode(repeat('02',32),'hex'))`, f.Environment, checkpoint, f.Computer, save)
	// A newly accepted control has no checkpoint until capture is bound.
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_controls(environment_id,id,computer_id,kind,caller_kind,caller_id,idempotency_key,deadline_at) VALUES($1,$2,$3,'hibernate','user',$4,'capture',clock_timestamp()+interval '1 minute')`, f.Environment, control, f.Computer, f.User)
	rejectSchemaRow(t, tx, "23503", `UPDATE computer_controls SET checkpoint_id=$3 WHERE environment_id=$1 AND id=$2`, f.Environment, control, uuid.NewV7())
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,storage_reservation_bytes) SELECT environment_id,$3,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,storage_reservation_bytes FROM computers WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer, other)
	rejectSchemaRow(t, tx, "23503", `INSERT INTO computer_controls(environment_id,id,computer_id,kind,caller_kind,caller_id,idempotency_key,deadline_at,checkpoint_id) VALUES($1,$2,$3,'hibernate','user',$4,'foreign',clock_timestamp()+interval '1 minute',$5)`, f.Environment, uuid.NewV7(), other, f.User, checkpoint)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_controls SET checkpoint_id=$3 WHERE environment_id=$1 AND id=$2`, f.Environment, control, checkpoint)
	rejectSchemaRow(t, tx, "23503", `DELETE FROM checkpoints WHERE environment_id=$1 AND id=$2`, f.Environment, checkpoint)
	dbtest.MustExec(t, t.Context(), tx, `DELETE FROM computer_controls WHERE environment_id=$1 AND id=$2`, f.Environment, control)
	dbtest.MustExec(t, t.Context(), tx, `DELETE FROM checkpoints WHERE environment_id=$1 AND id=$2`, f.Environment, checkpoint)
}

func TestSchemaCASMembershipRequiresOrganizationUntilRelease(t *testing.T) {
	f := sessiontest.New(t)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	org, digest := uuid.NewV7(), dbtest.Digest("membership-owner")
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,1)`, digest)
	rejectSchemaRow(t, tx, "23503", `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,1,'application/octet-stream')`, org, digest)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO organizations(id,slug,name) VALUES($1,'membership-owner','Membership owner')`, org)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,1,'application/octet-stream')`, org, digest)
	rejectSchemaRow(t, tx, "23503", `DELETE FROM organizations WHERE id=$1`, org)
	dbtest.MustExec(t, t.Context(), tx, `DELETE FROM cas_objects WHERE org_id=$1 AND digest=$2`, org, digest)
	dbtest.MustExec(t, t.Context(), tx, `DELETE FROM organizations WHERE id=$1`, org)
	var retained bool
	if err := tx.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM cas_blobs WHERE digest=$1)`, digest).Scan(&retained); err != nil || !retained {
		t.Fatalf("membership release removed physical blob: retained=%v err=%v", retained, err)
	}
}

func TestSchemaImageBuildAssignmentAndDiagnosticOwnership(t *testing.T) {
	f := sessiontest.New(t)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	build := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO image_builds(environment_id,id,image_spec_id,idempotency_key,deadline_at) VALUES($1,$2,$3,'assignment',clock_timestamp()+interval '10 minutes')`, f.Environment, build, f.Deployment)
	const diagnostic = `INSERT INTO diagnostic_chunks(environment_id,image_build_id,image_build_epoch,stream,sequence,through_sequence,byte_offset,through_byte_offset,kind,observed_at_unix_nano,data,dropped_bytes,complete) VALUES($1,$2,$3,'stdout',1,1,0,1,'data',1,decode('01','hex'),0,false)`
	rejectSchemaRow(t, tx, "23514", diagnostic, f.Environment, build, 0)
	const assign = `UPDATE image_builds SET status='running',executor_epoch=$3,worker_host_id=l.worker_host_id,worker_epoch=l.worker_epoch,allocation_id=$2,channel_credential_digest=l.channel_credential_digest,reserved_memory_bytes=l.reserved_memory_bytes,reserved_scratch_bytes=l.reserved_scratch_bytes,vm_platform_id=l.vm_platform_id,vm_vcpu_count=l.vm_vcpu_count,cpu_config_digest=l.cpu_config_digest FROM computer_leases l WHERE image_builds.environment_id=$1 AND image_builds.id=$2 AND l.environment_id=$1 AND l.computer_id=$4`
	rejectSchemaRow(t, tx, "23514", assign, f.Environment, build, 2, f.Computer)
	dbtest.MustExec(t, t.Context(), tx, assign, f.Environment, build, 1, f.Computer)
	rejectSchemaRow(t, tx, "23503", diagnostic, f.Environment, build, 2)
	dbtest.MustExec(t, t.Context(), tx, diagnostic, f.Environment, build, 1)
	var starting bool
	if err := tx.QueryRow(t.Context(), `SELECT starting FROM allocation_usage WHERE environment_id=$1 AND owner_id=$2 AND kind='preparation' AND epoch=1`, f.Environment, build).Scan(&starting); err != nil || !starting {
		t.Fatalf("assigned build is not starting: %v %v", starting, err)
	}
	dbtest.MustExec(t, t.Context(), tx, `UPDATE image_builds SET status='failed',error_code='timeout' WHERE environment_id=$1 AND id=$2`, f.Environment, build)
	var count int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM allocation_usage WHERE environment_id=$1 AND owner_id=$2`, f.Environment, build).Scan(&count); err != nil || count != 1 {
		t.Fatalf("logical failure released physical occupancy: %d %v", count, err)
	}
	dbtest.MustExec(t, t.Context(), tx, `UPDATE image_builds SET fenced_at=clock_timestamp(),fence_evidence='executor stopped' WHERE environment_id=$1 AND id=$2`, f.Environment, build)
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM allocation_usage WHERE environment_id=$1 AND owner_id=$2`, f.Environment, build).Scan(&count); err != nil || count != 0 {
		t.Fatalf("fenced build retained occupancy: %d %v", count, err)
	}
}
