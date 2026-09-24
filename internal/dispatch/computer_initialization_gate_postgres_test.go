package dispatch

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestInitializingComputerPreparesButCannotBecomeReadyOrExecute(t *testing.T) {
	f := newRunPlacementFixture(t)
	runtimeID, versionID := prepareInitialGeneration(t, f)
	// Allocation/admission is already complete; no persistent disk exists yet.
	params := runPlacementRuntimeReadyParams(t, f, runtimeID)
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

	// The fixture already has a certified root; commit its version status as
	// the current generation publisher does. Runtime retention is still required.
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE computer_versions v SET status='committed',published_at=clock_timestamp(),content_digest=r.root_digest,size_bytes=r.logical_bytes,publisher_runtime_instance_id=$2,publisher_desired_version=1,publication_request_fingerprint=decode(repeat('a1',32),'hex') FROM computer_version_roots r WHERE v.id=$1 AND r.version_id=v.id`, versionID, runtimeID)
	if _, err := db.New(f.pool).MarkRuntimeInstanceReady(f.ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unretained generation passed readiness gate: %v", err)
	}
	// The fixture already has a generation root. Pin it as the generation publisher does.
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE runtime_instances SET computer_source_version_id=reserved_workspace_version_id WHERE id=$1`, runtimeID)
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
	if workspaceHeadVersion(t, f) != versionID {
		t.Fatal("publication replaced initial base identity")
	}
}

func TestInitializingComputerAcceptsExplicitExecWithoutGrant(t *testing.T) {
	f := newRunPlacementFixture(t)
	root := workspaceHeadVersion(t, f)
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE computer_versions SET status='initializing', artifact_id=NULL,
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
