package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestComputerMountTargetAuthorityProjectsPrivateVersionWithinExactWorkspace(t *testing.T) {
	ctx := t.Context()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now())
	var workspaceID, workspaceLeaseID, baseWorkspaceVersionID uuid.UUID
	var ownershipGeneration, writerGeneration int64
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.workspace_id, workspace_leases.id, workspace_leases.base_workspace_version_id,
		       workspace_leases.ownership_generation, workspace_leases.writer_generation
		  FROM runs
		  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = runs.current_run_lease_id
		 WHERE runs.id = $1
	`, work.runID).Scan(
		&workspaceID, &workspaceLeaseID, &baseWorkspaceVersionID,
		&ownershipGeneration, &writerGeneration,
	); err != nil {
		t.Fatal(err)
	}

	artifactID := uuid.NewV7()
	privateVersionID := uuid.NewV7()
	digest := dbtest.Digest("private-workspace-reset-target")
	dbtest.MustExec(t, ctx, fixture.pool, `
		WITH lifetime AS (INSERT INTO cas_blobs (digest, size_bytes) VALUES ($2, 1) ON CONFLICT DO NOTHING) INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, 'application/vnd.helmr.workspace.v0.tar')
	`, fixture.orgID, digest)
	dbtest.MustExec(t, ctx, fixture.pool, `
		INSERT INTO artifacts (
			id, org_id, project_id, environment_id, digest, kind,
			size_bytes, media_type, created_by_worker_instance_id
		) VALUES ($1, $2, $3, $4, $5, 'workspace_version', 1,
		          'application/vnd.helmr.workspace.v0.tar', $6)
	`, artifactID, fixture.orgID, fixture.projectID, fixture.environmentID, digest, fixture.workerID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		INSERT INTO computer_versions (
			id, environment_id, computer_id, parent_version_id, root_pack_digest,
			logical_bytes, status, source_workspace_lease_id,
			ownership_generation, writer_generation
		) VALUES ($1, $2, $3, $4, $5,
		          1, 'private', $6, $7, $8)
	`, privateVersionID, fixture.environmentID, workspaceID, baseWorkspaceVersionID, digest, workspaceLeaseID, ownershipGeneration, writerGeneration)

	row, err := fixture.queries.GetComputerVersionAuthority(ctx, GetComputerVersionAuthorityParams{
		OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), WorkspaceID: pgvalue.UUID(workspaceID),
		VersionID: pgvalue.UUID(privateVersionID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(row.VersionID) != privateVersionID || row.ParentVersionID != pgvalue.UUID(baseWorkspaceVersionID) {
		t.Fatalf("private reset target authority = %+v", row)
	}

	other := fixture.addWork(t, ctx, "starting", time.Now())
	var otherWorkspaceID uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `SELECT workspace_id FROM runs WHERE id = $1`, other.runID).Scan(&otherWorkspaceID); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.queries.GetComputerVersionAuthority(ctx, GetComputerVersionAuthorityParams{
		OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), WorkspaceID: pgvalue.UUID(otherWorkspaceID),
		VersionID: pgvalue.UUID(privateVersionID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-Workspace private reset target error = %v, want no rows", err)
	}
}

func TestChildWorkspacePairLocksConvergeForOppositeDirections(t *testing.T) {
	fixture := newRunLeaseClaimFixture(t, t.Context())
	firstRun := fixture.addWork(t, t.Context(), "assigned", time.Now())
	secondRun := fixture.addWork(t, t.Context(), "assigned", time.Now())
	var firstWorkspace, secondWorkspace uuid.UUID
	if err := fixture.pool.QueryRow(
		t.Context(),
		"SELECT workspace_id FROM runs WHERE id = $1",
		firstRun.runID,
	).Scan(&firstWorkspace); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(
		t.Context(),
		"SELECT workspace_id FROM runs WHERE id = $1",
		secondRun.runID,
	).Scan(&secondWorkspace); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	lock := func(workspaceIDs []uuid.UUID) {
		tx, err := fixture.pool.Begin(ctx)
		if err != nil {
			results <- err
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		<-start
		rows, err := New(tx).LockChildWorkspacePair(ctx, LockChildWorkspacePairParams{
			EnvironmentID: pgvalue.UUID(fixture.environmentID),
			WorkspaceIds: []pgtype.UUID{
				pgvalue.UUID(workspaceIDs[0]),
				pgvalue.UUID(workspaceIDs[1]),
			},
		})
		if err == nil && len(rows) != 2 {
			err = fmt.Errorf("locked %d computers, want 2", len(rows))
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		results <- err
	}
	go lock([]uuid.UUID{firstWorkspace, secondWorkspace})
	go lock([]uuid.UUID{secondWorkspace, firstWorkspace})
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}
