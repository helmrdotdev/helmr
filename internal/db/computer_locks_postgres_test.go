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

func TestComputerDiskAuthorityProjectsPrivateVersionWithinExactComputer(t *testing.T) {
	ctx := t.Context()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now())
	var computerID, instanceID, baseComputerDiskVersionID uuid.UUID
	var writerGeneration int64
	if err := fixture.pool.QueryRow(ctx, `SELECT r.computer_id,l.computer_instance_id,r.base_computer_disk_version_id,l.writer_generation FROM runs r JOIN run_leases l ON l.id=r.current_run_lease_id WHERE r.id=$1`, work.runID).Scan(&computerID, &instanceID, &baseComputerDiskVersionID, &writerGeneration); err != nil {
		t.Fatal(err)
	}
	privateVersionID := uuid.NewV7()
	dbtest.MustExec(t, ctx, fixture.pool, `INSERT INTO computer_disk_versions(id,environment_id,computer_id,parent_version_id,root_pack_digest,logical_bytes,status,source_computer_instance_id,writer_generation) VALUES($1,$2,$3,$4,$5,4096,'private',$6,$7)`, privateVersionID, fixture.environmentID, computerID, baseComputerDiskVersionID, dbtest.Digest("private-computer-target"), instanceID, writerGeneration)

	row, err := fixture.queries.GetComputerDiskVersionAuthority(ctx, GetComputerDiskVersionAuthorityParams{
		OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ComputerID: pgvalue.UUID(computerID),
		VersionID: pgvalue.UUID(privateVersionID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(row.VersionID) != privateVersionID || row.ParentVersionID != pgvalue.UUID(baseComputerDiskVersionID) {
		t.Fatalf("private reset target authority = %+v", row)
	}

	other := fixture.addWork(t, ctx, "starting", time.Now())
	var otherComputerID uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `SELECT computer_id FROM runs WHERE id = $1`, other.runID).Scan(&otherComputerID); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.queries.GetComputerDiskVersionAuthority(ctx, GetComputerDiskVersionAuthorityParams{
		OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ComputerID: pgvalue.UUID(otherComputerID),
		VersionID: pgvalue.UUID(privateVersionID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-Computer private reset target error = %v, want no rows", err)
	}
}

func TestChildComputerPairLocksConvergeForOppositeDirections(t *testing.T) {
	fixture := newRunLeaseClaimFixture(t, t.Context())
	firstRun := fixture.addWork(t, t.Context(), "assigned", time.Now())
	secondRun := fixture.addWork(t, t.Context(), "assigned", time.Now())
	var firstComputer, secondComputer uuid.UUID
	if err := fixture.pool.QueryRow(
		t.Context(),
		"SELECT computer_id FROM runs WHERE id = $1",
		firstRun.runID,
	).Scan(&firstComputer); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(
		t.Context(),
		"SELECT computer_id FROM runs WHERE id = $1",
		secondRun.runID,
	).Scan(&secondComputer); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	lock := func(computerIDs []uuid.UUID) {
		tx, err := fixture.pool.Begin(ctx)
		if err != nil {
			results <- err
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		<-start
		rows, err := New(tx).LockChildComputerPair(ctx, LockChildComputerPairParams{
			EnvironmentID: pgvalue.UUID(fixture.environmentID),
			ComputerIds: []pgtype.UUID{
				pgvalue.UUID(computerIDs[0]),
				pgvalue.UUID(computerIDs[1]),
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
	go lock([]uuid.UUID{firstComputer, secondComputer})
	go lock([]uuid.UUID{secondComputer, firstComputer})
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}
