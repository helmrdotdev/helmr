package agent

import (
	"context"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func TestComputerCollectionRetainsDeploymentDependency(t *testing.T) {
	f := newFixture(t)
	digest := graphRetentionObject(t, f, 300, 0)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `INSERT INTO deployment_objects(environment_id,deployment_id,org_id,project_id,digest,size_bytes,media_type) SELECT environment_id,$3,org_id,project_id,digest,size_bytes,media_type FROM computer_objects WHERE environment_id=$1 AND digest=$2`, f.env, digest, f.deployment); err != nil {
		t.Fatal(err)
	}
	// An uncommitted Deployment adoption holds the membership FK. The collector
	// loses its bounded wait and rolls back the object deletion as one unit.
	if err := reclaimComputerObject(t.Context(), f.pool, f.env, digest); err != nil {
		t.Fatal(err)
	}
	var present bool
	if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM computer_objects WHERE environment_id=$1 AND digest=$2)`, f.env, digest).Scan(&present); err != nil || !present {
		t.Fatalf("failed membership release lost retry owner: %v %v", present, err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := reclaimComputerObject(t.Context(), f.pool, f.env, digest); err != nil {
		t.Fatal(err)
	}
	var collected, retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT
 NOT EXISTS(SELECT 1 FROM computer_objects WHERE environment_id=$1 AND digest=$2),
 EXISTS(SELECT 1 FROM deployment_objects d JOIN cas_objects c ON c.org_id=d.org_id AND c.digest=d.digest WHERE d.environment_id=$1 AND d.digest=$2)`, f.env, digest).Scan(&collected, &retained); err != nil || !collected || !retained {
		t.Fatalf("Computer collected=%v Deployment retained=%v error=%v", collected, retained, err)
	}
	if _, err := f.pool.Exec(t.Context(), `DELETE FROM cas_objects WHERE digest=$1`, digest); err == nil {
		t.Fatal("Deployment FK did not protect membership")
	}
}

func TestComputerMembershipCollectorsSeeEachOthersCommit(t *testing.T) {
	f := newFixture(t)
	digest := graphRetentionObject(t, f, 301, 0)
	other := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO environments(history_retention_mode,id,org_id,project_id,slug,name,color_hex) SELECT 'until_environment_deletion',$2,org_id,project_id,'other','Other',color_hex FROM environments WHERE id=$1;
 INSERT INTO computer_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at)
 SELECT $2,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at FROM computer_objects WHERE environment_id=$1 AND digest=$3`, pgx.QueryExecModeSimpleProtocol, f.env, other, digest)
	first, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(context.Background())
	second, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback(context.Background())
	var org uuid.UUID
	if err := first.QueryRow(t.Context(), `DELETE FROM computer_objects WHERE environment_id=$1 AND digest=$2 RETURNING org_id`, f.env, digest).Scan(&org); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Exec(t.Context(), `DELETE FROM computer_objects WHERE environment_id=$1 AND digest=$2`, other, digest); err != nil {
		t.Fatal(err)
	}
	if err := releaseComputerCASMembership(t.Context(), first, org, digest); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- releaseComputerCASMembership(t.Context(), second, org, digest) }()
	awaitRetentionLockWait(t, f, second)
	if err := first.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := second.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var removed bool
	if err := f.pool.QueryRow(t.Context(), `SELECT NOT EXISTS(SELECT 1 FROM cas_objects WHERE org_id=$1 AND digest=$2)`, org, digest).Scan(&removed); err != nil || !removed {
		t.Fatalf("last owner left membership: %v %v", removed, err)
	}
}

func TestComputerMembershipCollectionDoesNotExpireUnrelatedUploads(t *testing.T) {
	f := newFixture(t)
	digest := graphRetentionObject(t, f, 302, 0)
	// A separate accepted tenant membership is not a Computer-owned orphan.
	standalone := "sha256:0000000000000000000000000000000000000000000000000000000000000303"
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($2,1);
 INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT org_id,$2,1,'application/octet-stream' FROM environments WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, standalone)
	if err := reclaimComputerObject(t.Context(), f.pool, f.env, digest); err != nil {
		t.Fatal(err)
	}
	var retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM cas_objects WHERE digest=$1)`, standalone).Scan(&retained); err != nil || !retained {
		t.Fatalf("unrelated membership expired: %v %v", retained, err)
	}
}

func TestComputerMembershipReleasePreservesOtherOrganization(t *testing.T) {
	f := newFixture(t)
	digest := graphRetentionObject(t, f, 304, 0)
	org, project, env := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO organizations(id,name,slug) VALUES($2,'Other','other');
 INSERT INTO projects(id,org_id,default_region_id,slug,name) VALUES($3,$2,'test','other','Other');
 INSERT INTO environments(history_retention_mode,id,org_id,project_id,slug,name,color_hex) VALUES('until_environment_deletion',$4,$2,$3,'other','Other','#112233');
 INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT $2,digest,size_bytes,media_type FROM computer_objects WHERE environment_id=$1 AND digest=$5;
 INSERT INTO computer_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at)
 SELECT $4,digest,$2,$3,size_bytes,media_type,kind,rank,inspection,certified_at FROM computer_objects WHERE environment_id=$1 AND digest=$5`, pgx.QueryExecModeSimpleProtocol, f.env, org, project, env, digest)
	if err := reclaimComputerObject(t.Context(), f.pool, f.env, digest); err != nil {
		t.Fatal(err)
	}
	var correct bool
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM cas_objects WHERE digest=$1)=1 AND EXISTS(SELECT 1 FROM cas_objects WHERE org_id=$2 AND digest=$1) AND EXISTS(SELECT 1 FROM computer_objects WHERE environment_id=$3 AND digest=$1)`, digest, org, env).Scan(&correct); err != nil || !correct {
		t.Fatalf("cross-organization owner lost: %v %v", correct, err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE cas_blobs SET retired_at=clock_timestamp(),next_reclaim_at=clock_timestamp() WHERE digest=$1`, digest); err == nil {
		t.Fatal("other organization no longer pinned physical availability")
	}
	if err := reclaimComputerObject(t.Context(), f.pool, env, digest); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT NOT EXISTS(SELECT 1 FROM cas_objects WHERE digest=$1)`, digest).Scan(&correct); err != nil || !correct {
		t.Fatalf("last organization membership retained: %v %v", correct, err)
	}
}
