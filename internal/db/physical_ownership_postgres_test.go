package db_test

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
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
	org, other, digest := uuid.NewV7(), uuid.NewV7(), dbtest.Digest("membership-owner")
	rejectSchemaRow(t, tx, "23503", `INSERT INTO cas_blobs(org_id,digest,size_bytes) VALUES($1,$2,1)`, org, digest)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO organizations(id,slug,name) VALUES($1,'membership-owner','Membership owner'),($2,'membership-other','Membership other')`, org, other)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO cas_blobs(org_id,digest,size_bytes) VALUES($1,$2,1)`, org, digest)
	// The same digest of another organization is a different object.
	rejectSchemaRow(t, tx, "23503", `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,1,'application/octet-stream')`, other, digest)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,1,'application/octet-stream')`, org, digest)
	rejectSchemaRow(t, tx, "23503", `DELETE FROM organizations WHERE id=$1`, org)
	dbtest.MustExec(t, t.Context(), tx, `DELETE FROM cas_objects WHERE org_id=$1 AND digest=$2`, org, digest)
	// The released lifetime still belongs to its organization's keyspace.
	rejectSchemaRow(t, tx, "23503", `DELETE FROM organizations WHERE id=$1`, org)
	var retained bool
	if err := tx.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM cas_blobs WHERE org_id=$1 AND digest=$2)`, org, digest).Scan(&retained); err != nil || !retained {
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
	const assign = `UPDATE image_builds SET status='running',executor_epoch=$3,worker_group_id=(SELECT worker_group_id FROM worker_hosts WHERE id=l.worker_host_id),worker_host_id=l.worker_host_id,worker_epoch=l.worker_epoch,allocation_id=$2,channel_credential_digest=l.channel_credential_digest,reserved_memory_bytes=l.reserved_memory_bytes,reserved_scratch_bytes=l.reserved_scratch_bytes,vm_platform_id=l.vm_platform_id,vm_vcpu_count=l.vm_vcpu_count,cpu_config_digest=l.cpu_config_digest FROM computer_leases l WHERE image_builds.environment_id=$1 AND image_builds.id=$2 AND l.environment_id=$1 AND l.computer_id=$4`
	rejectSchemaRow(t, tx, "23514", assign, f.Environment, build, 2, f.Computer)
	dbtest.MustExec(t, t.Context(), tx, assign, f.Environment, build, 1, f.Computer)
	// The producing group is recorded with the executor and is the host's group.
	other, token := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO regions(id,display_name) VALUES('producing-group','Producing')`)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO worker_group_tokens(id,token_hash) VALUES($1,decode(repeat('07',32),'hex'))`, token)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO worker_groups(id,token_id,region_id,name) VALUES($1,$2,'producing-group','other')`, other, token)
	rejectSchemaRow(t, tx, "23514", `UPDATE image_builds SET worker_group_id=NULL WHERE environment_id=$1 AND id=$2`, f.Environment, build)
	rejectSchemaRow(t, tx, "23503", `UPDATE image_builds SET worker_group_id=$3 WHERE environment_id=$1 AND id=$2`, f.Environment, build, other)
	rejectSchemaRow(t, tx, "23503", diagnostic, f.Environment, build, 2)
	dbtest.MustExec(t, t.Context(), tx, diagnostic, f.Environment, build, 1)
	var starting bool
	if err := tx.QueryRow(t.Context(), `SELECT starting FROM (`+db.AllocationUsageSQL+`) allocation_usage WHERE environment_id=$1 AND owner_id=$2 AND kind='preparation' AND epoch=1`, f.Environment, build).Scan(&starting); err != nil || !starting {
		t.Fatalf("assigned build is not starting: %v %v", starting, err)
	}
	dbtest.MustExec(t, t.Context(), tx, `UPDATE image_builds SET status='failed',error_code='timeout' WHERE environment_id=$1 AND id=$2`, f.Environment, build)
	var count int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM (`+db.AllocationUsageSQL+`) allocation_usage WHERE environment_id=$1 AND owner_id=$2`, f.Environment, build).Scan(&count); err != nil || count != 1 {
		t.Fatalf("logical failure released physical occupancy: %d %v", count, err)
	}
	dbtest.MustExec(t, t.Context(), tx, `UPDATE image_builds SET fenced_at=clock_timestamp(),fence_evidence='executor stopped' WHERE environment_id=$1 AND id=$2`, f.Environment, build)
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM (`+db.AllocationUsageSQL+`) allocation_usage WHERE environment_id=$1 AND owner_id=$2`, f.Environment, build).Scan(&count); err != nil || count != 0 {
		t.Fatalf("fenced build retained occupancy: %d %v", count, err)
	}
}

func TestSchemaComputerPinNamesOwnOrganizationGroupInRegion(t *testing.T) {
	f := sessiontest.New(t)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	var org, project uuid.UUID
	var region string
	if err := tx.QueryRow(t.Context(), `SELECT e.org_id, e.project_id, p.default_region_id FROM environments e JOIN projects p ON p.id=e.project_id WHERE e.id=$1`, f.Environment).Scan(&org, &project, &region); err != nil {
		t.Fatal(err)
	}
	otherOrg, otherRegion := uuid.NewV7(), "pin-other"
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO organizations (id, name, slug) VALUES ($1, 'Other', $2)`, otherOrg, otherOrg.String())
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO regions (id, display_name) VALUES ($1, 'Other')`, otherRegion)
	group := func(owner *uuid.UUID, region string) uuid.UUID {
		t.Helper()
		id, token := uuid.NewV7(), uuid.NewV7()
		dbtest.MustExec(t, t.Context(), tx, `INSERT INTO worker_group_tokens (id, token_hash) VALUES ($1::uuid, sha256(convert_to($1::uuid::text, 'UTF8')))`, token)
		dbtest.MustExec(t, t.Context(), tx, `INSERT INTO worker_groups (id, owner_org_id, token_id, region_id, name, status) VALUES ($1::uuid, $2, $3, $4, $1::uuid::text, 'disabled')`, id, owner, token, region)
		return id
	}
	own, west, foreign, deployment := group(&org, region), group(&org, otherRegion), group(&otherOrg, region), group(nil, region)
	const pin = `UPDATE computers SET worker_group_org_id=$3, worker_group_project_id=$4, worker_group_region_id=$5, worker_group_id=$6 WHERE environment_id=$1 AND id=$2`
	// A pin names a group of another organization, another region or the
	// deployment never; deployment capacity is the absence of a pin.
	for _, group := range []uuid.UUID{foreign, west, deployment, uuid.NewV7()} {
		rejectSchemaRow(t, tx, "23503", pin, f.Environment, f.Computer, org, project, region, group)
	}
	rejectSchemaRow(t, tx, "23503", pin, f.Environment, f.Computer, otherOrg, project, region, foreign)
	rejectSchemaRow(t, tx, "23503", pin, f.Environment, f.Computer, org, project, otherRegion, west)
	rejectSchemaRow(t, tx, "23514", `UPDATE computers SET worker_group_id=$3 WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer, own)
	dbtest.MustExec(t, t.Context(), tx, pin, f.Environment, f.Computer, org, project, region, own)
	rejectSchemaRow(t, tx, "23503", `UPDATE worker_groups SET owner_org_id=$2 WHERE id=$1`, own, otherOrg)
	rejectSchemaRow(t, tx, "23503", `DELETE FROM worker_groups WHERE id=$1`, own)
}
