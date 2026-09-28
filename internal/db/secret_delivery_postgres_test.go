package db

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestAttemptSecretDeliveryLocksCompleteComputerPlacementSet(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "assigned", time.Now())

	var computerID uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `SELECT computer_id FROM runs WHERE id = $1`, work.runID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	secretID := uuid.NewV7()
	oldVersionID := uuid.NewV7()
	currentVersionID := uuid.NewV7()
	resolutionID := uuid.NewV7()

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO secrets (
			id, environment_id, name, current_version_id, revocation_generation
		)
		VALUES ($1, $2, 'delivery-secret', $3, 4)
	`, secretID, fixture.environmentID, currentVersionID)
	for version, versionID := range []uuid.UUID{oldVersionID, currentVersionID} {
		dbtest.MustExec(t, ctx, tx, `
			INSERT INTO secret_versions (
				id, secret_id, version, nonce, ciphertext
			)
			VALUES (
				$1, $2, $3::bigint,
				decode(lpad(($3::bigint)::text, 24, '0'), 'hex'),
				decode(repeat('02', 16), 'hex')
			)
		`, versionID, secretID, version+1)
	}
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO computer_secrets (mode,
			computer_id, environment_id, placement_kind, placement_target, secret_id
		)
		VALUES
			('raw', $1, $2, 'env', 'TOKEN', $3),
			('raw', $1, $2, 'file', '/run/secrets/token', $3)
	`, computerID, fixture.environmentID, secretID)
	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO secret_resolutions (
			id, computer_id, run_id, attempt_number, placement_kind, placement_target,
			secret_id, secret_version_id, revocation_generation
		)
		VALUES ($1, $2, $3, 1, 'env', 'TOKEN', $4, $5, 4)
	`, resolutionID, computerID, work.runID, secretID, oldVersionID)

	rows, err := New(tx).LockAttemptSecretDelivery(ctx, LockAttemptSecretDeliveryParams{
		RunID:         pgvalue.UUID(work.runID),
		AttemptNumber: pgtype.Int4{Int32: 1, Valid: true},
		ComputerID:    pgvalue.UUID(computerID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].ComputerSecret.PlacementKind != "env" ||
		rows[0].ResolutionID != pgvalue.UUID(resolutionID) ||
		rows[0].ResolutionSecretVersionID != pgvalue.UUID(oldVersionID) ||
		rows[0].Secret.CurrentVersionID != pgvalue.UUID(currentVersionID) {
		t.Fatalf("resolved row = %+v", rows[0])
	}
	if rows[1].ComputerSecret.PlacementKind != "file" ||
		rows[1].ResolutionID.Valid ||
		rows[1].ResolutionSecretVersionID.Valid {
		t.Fatalf("missing-resolution row = %+v", rows[1])
	}

	dbtest.MustExec(t, ctx, tx, `
		INSERT INTO computer_secrets (mode,
			computer_id, environment_id, placement_kind, placement_target, secret_id
		)
		SELECT 'raw', $1, $2, 'env', 'TOKEN_' || ordinal::text, $3
		  FROM generate_series(1, 63) AS ordinal
	`, computerID, fixture.environmentID, secretID)
	rows, err = New(tx).LockAttemptSecretDelivery(ctx, LockAttemptSecretDeliveryParams{
		RunID:         pgvalue.UUID(work.runID),
		AttemptNumber: pgtype.Int4{Int32: 1, Valid: true},
		ComputerID:    pgvalue.UUID(computerID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 65 {
		t.Fatalf("bounded rows = %d, want 65", len(rows))
	}
}
