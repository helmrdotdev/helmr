package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type runLeaseRenewalAuthority struct {
	runLeaseID             pgtype.UUID
	runID                  pgtype.UUID
	workspaceID            pgtype.UUID
	attemptNumber          int32
	leaseSequence          int64
	runtimeInstanceID      pgtype.UUID
	workspaceMountID       pgtype.UUID
	workspaceLeaseID       pgtype.UUID
	ownershipGeneration    int64
	writerGeneration       int64
	mountFencingGeneration int64
	expiresAt              pgtype.Timestamptz
}

func TestRunLeaseRenewalUpdatesBothLeasesAtomically(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	authority := fixture.runningRunLeaseRenewalAuthority(t, ctx)
	newExpiry := authority.expiresAt.Time.Add(time.Minute)

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := fixture.queries.WithTx(tx)
	now, err := queries.GetRunLeaseRenewalTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runLease, err := queries.RenewRunLeaseExpiry(ctx, RenewRunLeaseExpiryParams{
		RenewedAt: now, ExpiresAt: pgvalue.Timestamptz(newExpiry),
		ID: authority.runLeaseID, RunID: authority.runID, WorkspaceID: authority.workspaceID,
		AttemptNumber: authority.attemptNumber, LeaseSequence: authority.leaseSequence,
		PreviousExpiresAt: authority.expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	workspaceLease, err := queries.RenewRunWorkspaceLeaseExpiry(ctx, RenewRunWorkspaceLeaseExpiryParams{
		RenewedAt: now, ExpiresAt: pgvalue.Timestamptz(newExpiry),
		ID: authority.workspaceLeaseID, WorkspaceID: authority.workspaceID,
		RuntimeInstanceID: authority.runtimeInstanceID, WorkspaceMountID: authority.workspaceMountID,
		OwnerRunLeaseID: authority.runLeaseID, OwnershipGeneration: authority.ownershipGeneration,
		WriterGeneration: authority.writerGeneration, MountFencingGeneration: authority.mountFencingGeneration,
		PreviousExpiresAt: authority.expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !runLease.PreviousExpiresAt.Time.Equal(authority.expiresAt.Time) ||
		!runLease.RenewedAt.Time.Equal(now.Time) ||
		!runLease.ExpiresAt.Time.Equal(newExpiry) ||
		!workspaceLease.RenewedAt.Time.Equal(now.Time) ||
		!workspaceLease.ExpiresAt.Time.Equal(newExpiry) {
		t.Fatalf("Run Lease = %+v, Workspace Lease = %+v", runLease, workspaceLease)
	}
}

func TestRunLeaseRenewalRollsBackWhenWorkspaceLeaseCannotAdvance(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	authority := fixture.runningRunLeaseRenewalAuthority(t, ctx)

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queries := fixture.queries.WithTx(tx)
	now, err := queries.GetRunLeaseRenewalTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.RenewRunLeaseExpiry(ctx, RenewRunLeaseExpiryParams{
		RenewedAt: now, ExpiresAt: pgvalue.Timestamptz(authority.expiresAt.Time.Add(time.Minute)),
		ID: authority.runLeaseID, RunID: authority.runID, WorkspaceID: authority.workspaceID,
		AttemptNumber: authority.attemptNumber, LeaseSequence: authority.leaseSequence,
		PreviousExpiresAt: authority.expiresAt,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = queries.RenewRunWorkspaceLeaseExpiry(ctx, RenewRunWorkspaceLeaseExpiryParams{
		RenewedAt: now, ExpiresAt: pgvalue.Timestamptz(authority.expiresAt.Time.Add(time.Minute)),
		ID: authority.workspaceLeaseID, WorkspaceID: authority.workspaceID,
		RuntimeInstanceID: authority.runtimeInstanceID, WorkspaceMountID: authority.workspaceMountID,
		OwnerRunLeaseID: authority.runLeaseID, OwnershipGeneration: authority.ownershipGeneration,
		WriterGeneration:       authority.writerGeneration + 1,
		MountFencingGeneration: authority.mountFencingGeneration,
		PreviousExpiresAt:      authority.expiresAt,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Workspace Lease renewal error = %v, want no rows", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	var expiry time.Time
	var previous *time.Time
	if err := fixture.pool.QueryRow(ctx, `
		SELECT expires_at, previous_expires_at FROM run_leases WHERE id = $1
	`, authority.runLeaseID).Scan(&expiry, &previous); err != nil {
		t.Fatal(err)
	}
	if !expiry.Equal(authority.expiresAt.Time) || previous != nil {
		t.Fatalf("Run Lease expiry = %s, previous = %v", expiry, previous)
	}
}

func (fixture runLeaseClaimFixture) runningRunLeaseRenewalAuthority(
	t *testing.T,
	ctx context.Context,
) runLeaseRenewalAuthority {
	t.Helper()
	params := fixture.runningRunLogParams(t, ctx)
	locators, err := fixture.queries.GetLiveRunLeaseLocators(ctx, GetLiveRunLeaseLocatorsParams{
		ID: params.RunLeaseID, LeaseSequence: params.LeaseSequence,
		WorkerGroupID: params.WorkerGroupID, WorkerInstanceID: params.WorkerInstanceID,
		WorkerEpoch: params.WorkerEpoch})
	if err != nil {
		t.Fatal(err)
	}
	authority := runLeaseRenewalAuthority{
		runLeaseID: params.RunLeaseID, runID: locators.RunID, workspaceID: locators.WorkspaceID,
		attemptNumber: locators.AttemptNumber, leaseSequence: params.LeaseSequence,
		runtimeInstanceID: locators.RuntimeInstanceID, workspaceMountID: locators.WorkspaceMountID,
		workspaceLeaseID: locators.WorkspaceLeaseID,
	}
	if err := fixture.pool.QueryRow(ctx, `
		SELECT run_leases.expires_at,
		       workspace_leases.ownership_generation,
		       workspace_leases.writer_generation,
		       workspace_leases.mount_fencing_generation
		  FROM run_leases
		  JOIN workspace_leases ON workspace_leases.id = $2
		 WHERE run_leases.id = $1
	`, authority.runLeaseID, authority.workspaceLeaseID).Scan(
		&authority.expiresAt,
		&authority.ownershipGeneration,
		&authority.writerGeneration,
		&authority.mountFencingGeneration,
	); err != nil {
		t.Fatal(err)
	}
	return authority
}
