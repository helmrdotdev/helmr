package dispatch

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestInitializingComputerPreparesButCannotBecomeReadyOrExecute(t *testing.T) {
	f := newRunPlacementFixture(t)
	candidate := registerPreparationCandidate(t, f)
	// Allocation/admission is already complete; no persistent disk exists yet.
	params := runPlacementRuntimeReadyParams(t, f, candidate.RuntimeInstanceID)
	if _, err := db.New(f.pool).MarkRuntimeInstanceReady(f.ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("initializing Computer became ready: %v", err)
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = lockRunPlacementAuthority(f.ctx, tx, f.candidate(), false)
	_ = tx.Rollback(f.ctx)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("initializing Computer passed execution authority: %v", err)
	}

	// This tests the DB publication boundary, not remote verification or boot.
	artifactID := pgvalue.UUID(uuid.NewV7())
	dbtest.MustExec(t, f.ctx, f.pool, `WITH lifetime AS (INSERT INTO cas_object_lifetimes (digest) VALUES ($2) ON CONFLICT DO NOTHING) INSERT INTO cas_objects (org_id,digest,size_bytes,media_type) VALUES ($1,$2,$3,$4)`, f.orgID, candidate.Digest, candidate.SizeBytes, candidate.MediaType)
	dbtest.MustExec(t, f.ctx, f.pool, `INSERT INTO artifacts (id,org_id,project_id,environment_id,digest,kind,size_bytes,media_type)
    VALUES ($1,$2,$3,$4,$5,'workspace_version',$6,$7)`, artifactID, f.orgID, f.projectID, f.environmentID, candidate.Digest, candidate.SizeBytes, candidate.MediaType)
	if _, err := db.New(f.pool).PublishComputerInitialization(f.ctx, db.PublishComputerInitializationParams{
		ID: candidate.ID, EnvironmentID: candidate.EnvironmentID, ComputerID: candidate.ComputerID, ArtifactID: artifactID,
	}); err != nil {
		t.Fatal(err)
	}
	// Publication does not itself start user code; readiness and mount are still required.
	var leases int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_leases WHERE run_id=$1`, f.runID).Scan(&leases); err != nil || leases != 0 {
		t.Fatalf("publication granted execution: %d %v", leases, err)
	}
	if _, err := db.New(f.pool).MarkRuntimeInstanceReady(f.ctx, params); err != nil {
		t.Fatal(err)
	}
	mounting, err := f.authority.PlaceReadyRun(f.ctx, f.candidate())
	if err != nil || mounting.LeaseCreated || !mounting.WorkspaceMountID.Valid {
		t.Fatalf("mount: %+v %v", mounting, err)
	}
	markRunPlacementMountReady(t, f, mounting.WorkspaceMountID)
	granted, err := f.authority.PlaceReadyRun(f.ctx, f.candidate())
	if err != nil || !granted.LeaseCreated {
		t.Fatalf("grant after publication and readiness: %+v %v", granted, err)
	}
	if workspaceHeadVersion(t, f) != candidate.VersionID {
		t.Fatal("publication replaced initial base identity")
	}
}

func TestInitializingComputerAcceptsExplicitExecWithoutGrant(t *testing.T) {
	f := newRunPlacementFixture(t)
	root := workspaceHeadVersion(t, f)
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE workspace_versions SET status='initializing', artifact_id=NULL,
content_digest=NULL,size_bytes=0,published_at=NULL WHERE id=$1`, root)
	processID := createPendingWorkspaceExec(t, f)
	q := db.New(f.pool)
	if _, err := q.LockWorkspaceAdmissionAuthority(f.ctx, db.LockWorkspaceAdmissionAuthorityParams{
		EnvironmentID: pgvalue.UUID(f.environmentID), ID: pgvalue.UUID(f.workspaceID),
	}); err != nil {
		t.Fatalf("initializing exec admission: %v", err)
	}
	candidates, err := q.ListPendingWorkspaceExecCapacityCandidates(f.ctx, db.ListPendingWorkspaceExecCapacityCandidatesParams{RegionID: "us-east-1", RowLimit: 10})
	if err != nil || len(candidates) != 1 || candidates[0].ProcessID != pgvalue.UUID(processID) {
		t.Fatalf("initializing exec discovery: %+v %v", candidates, err)
	}
	reserved, err := f.authority.PlaceWorkspaceExec(f.ctx, ReadyWorkspaceExecCandidate{OrgID: pgvalue.UUID(f.orgID), ProcessID: pgvalue.UUID(processID), ExpectedRevision: 1})
	if err != nil || !reserved.RuntimeInstanceID.Valid || reserved.ProcessBound {
		t.Fatalf("initializing exec preparation: %+v %v", reserved, err)
	}
	if _, err := q.MarkRuntimeInstanceReady(f.ctx, runPlacementRuntimeReadyParams(t, f, reserved.RuntimeInstanceID)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("initializing exec ready: %v", err)
	}
	if _, err := q.AdvanceWorkspaceExecWriter(f.ctx, db.AdvanceWorkspaceExecWriterParams{
		OrgID: pgvalue.UUID(f.orgID), ProjectID: pgvalue.UUID(f.projectID), EnvironmentID: pgvalue.UUID(f.environmentID),
		WorkspaceID: pgvalue.UUID(f.workspaceID), BaseWorkspaceVersionID: root,
		ExpectedOwnershipGeneration: 1, ExpectedWriterGeneration: 0, OwnershipGeneration: 2, WriterGeneration: 1,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("initializing exec writer granted: %v", err)
	}
}
