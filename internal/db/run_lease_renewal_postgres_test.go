package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestRunLeaseRenewalUpdatesBothLeasesAtomically(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	authority := fixture.runningRunLogParams(t, ctx)
	newExpiry := authority.ExpiresAt.Time.Add(time.Minute)

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
		ID: authority.RunLeaseID, RunID: authority.RunID, WorkspaceID: authority.WorkspaceID,
		AttemptNumber: authority.AttemptNumber, LeaseSequence: authority.LeaseSequence,
		PreviousExpiresAt: authority.ExpiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	workspaceLease, err := queries.RenewRunWorkspaceLeaseExpiry(ctx, RenewRunWorkspaceLeaseExpiryParams{
		RenewedAt: now, ExpiresAt: pgvalue.Timestamptz(newExpiry),
		ID: authority.WorkspaceLeaseID, WorkspaceID: authority.WorkspaceID,
		RuntimeInstanceID: authority.RuntimeInstanceID, WorkspaceMountID: authority.WorkspaceMountID,
		OwnerRunLeaseID: authority.RunLeaseID, OwnershipGeneration: authority.OwnershipGeneration,
		WriterGeneration: authority.WriterGeneration, MountFencingGeneration: authority.MountFencingGeneration,
		PreviousExpiresAt: authority.ExpiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !runLease.PreviousExpiresAt.Time.Equal(authority.ExpiresAt.Time) ||
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
	authority := fixture.runningRunLogParams(t, ctx)

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
		RenewedAt: now, ExpiresAt: pgvalue.Timestamptz(authority.ExpiresAt.Time.Add(time.Minute)),
		ID: authority.RunLeaseID, RunID: authority.RunID, WorkspaceID: authority.WorkspaceID,
		AttemptNumber: authority.AttemptNumber, LeaseSequence: authority.LeaseSequence,
		PreviousExpiresAt: authority.ExpiresAt,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = queries.RenewRunWorkspaceLeaseExpiry(ctx, RenewRunWorkspaceLeaseExpiryParams{
		RenewedAt: now, ExpiresAt: pgvalue.Timestamptz(authority.ExpiresAt.Time.Add(time.Minute)),
		ID: authority.WorkspaceLeaseID, WorkspaceID: authority.WorkspaceID,
		RuntimeInstanceID: authority.RuntimeInstanceID, WorkspaceMountID: authority.WorkspaceMountID,
		OwnerRunLeaseID: authority.RunLeaseID, OwnershipGeneration: authority.OwnershipGeneration,
		WriterGeneration:       authority.WriterGeneration + 1,
		MountFencingGeneration: authority.MountFencingGeneration,
		PreviousExpiresAt:      authority.ExpiresAt,
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
	`, authority.RunLeaseID).Scan(&expiry, &previous); err != nil {
		t.Fatal(err)
	}
	if !expiry.Equal(authority.ExpiresAt.Time) || previous != nil {
		t.Fatalf("Run Lease expiry = %s, previous = %v", expiry, previous)
	}
}
