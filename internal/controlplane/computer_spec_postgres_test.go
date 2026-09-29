package controlplane

import (
	"errors"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestComputerSpecPostgresRetainedForComputerLifetime(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	var specID uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `SELECT computer_spec_id FROM computers WHERE id=$1`, fixture.computerIDs[0]).Scan(&specID); err != nil {
		t.Fatal(err)
	}
	_, err := fixture.pool.Exec(t.Context(), `UPDATE computer_specs SET seed_artifact_id=NULL WHERE id=$1`, specID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("release live Computer seed = %v", err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE computers SET status='deleted',desired_state='deleted',head_disk_version_id=NULL,sandbox_declared_id=NULL,deleted_at=now() WHERE id=$1`, fixture.computerIDs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE computer_specs SET seed_artifact_id=NULL WHERE id=$1`, specID); err != nil {
		t.Fatal(err)
	}
	var retained uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `SELECT computer_spec_id FROM computers WHERE id=$1`, fixture.computerIDs[0]).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != specID {
		t.Fatal("deletion lost Computer specification identity")
	}
}
