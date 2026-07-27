package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestAttemptSecretDeliveryLocksCompleteWorkspacePlacementSet(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "assigned", time.Now())

	var workspaceID uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `SELECT workspace_id FROM runs WHERE id = $1`, work.runID).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	secretID := uuid.Must(uuid.NewV7())
	oldVersionID := uuid.Must(uuid.NewV7())
	currentVersionID := uuid.Must(uuid.NewV7())
	resolutionID := uuid.Must(uuid.NewV7())

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO secrets (
			id, environment_id, name, current_version_id, revocation_generation
		)
		VALUES ($1, $2, 'delivery-secret', $3, 4)
	`, secretID, fixture.environmentID, currentVersionID)
	for version, versionID := range []uuid.UUID{oldVersionID, currentVersionID} {
		mustRunLeaseExec(t, ctx, tx, `
			INSERT INTO secret_versions (
				id, secret_id, version, key_id, nonce, ciphertext,
				value_authenticator, authenticator_key_version
			)
			VALUES (
				$1, $2, $3::bigint, 'test-key',
				decode(lpad(($3::bigint)::text, 24, '0'), 'hex'),
				decode(repeat('02', 16), 'hex'),
				decode(repeat('03', 32), 'hex'),
				1
			)
		`, versionID, secretID, version+1)
	}
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO workspace_secrets (
			workspace_id, environment_id, placement_kind, placement_target, secret_id
		)
		VALUES
			($1, $2, 'env', 'TOKEN', $3),
			($1, $2, 'file', '/run/helmr/token', $3)
	`, workspaceID, fixture.environmentID, secretID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO secret_resolutions (
			id, workspace_id, run_id, attempt_number, placement_kind, placement_target,
			secret_id, secret_version_id, revocation_generation
		)
		VALUES ($1, $2, $3, 1, 'env', 'TOKEN', $4, $5, 4)
	`, resolutionID, workspaceID, work.runID, secretID, oldVersionID)

	rows, err := New(tx).LockAttemptSecretDelivery(ctx, LockAttemptSecretDeliveryParams{
		RunID:         pgvalue.UUID(work.runID),
		AttemptNumber: pgtype.Int4{Int32: 1, Valid: true},
		WorkspaceID:   pgvalue.UUID(workspaceID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].WorkspaceSecret.PlacementKind != "env" ||
		rows[0].ResolutionID != pgvalue.UUID(resolutionID) ||
		rows[0].ResolutionSecretVersionID != pgvalue.UUID(oldVersionID) ||
		rows[0].Secret.CurrentVersionID != pgvalue.UUID(currentVersionID) {
		t.Fatalf("resolved row = %+v", rows[0])
	}
	if rows[1].WorkspaceSecret.PlacementKind != "file" ||
		rows[1].ResolutionID.Valid ||
		rows[1].ResolutionSecretVersionID.Valid {
		t.Fatalf("missing-resolution row = %+v", rows[1])
	}

	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO workspace_secrets (
			workspace_id, environment_id, placement_kind, placement_target, secret_id
		)
		SELECT $1, $2, 'env', 'TOKEN_' || ordinal::text, $3
		  FROM generate_series(1, 63) AS ordinal
	`, workspaceID, fixture.environmentID, secretID)
	rows, err = New(tx).LockAttemptSecretDelivery(ctx, LockAttemptSecretDeliveryParams{
		RunID:         pgvalue.UUID(work.runID),
		AttemptNumber: pgtype.Int4{Int32: 1, Valid: true},
		WorkspaceID:   pgvalue.UUID(workspaceID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 65 {
		t.Fatalf("bounded rows = %d, want 65", len(rows))
	}
}
