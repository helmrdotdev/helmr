package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/dispatch/dispatchtest"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCheckpointDiskVersionRetainsExactInstanceProvenance(t *testing.T) {
	f, worker, request, uploaded := dispatchtest.ReadyCapture(t, false)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	cp, err := dispatch.CompleteComputerCheckpoint(t.Context(), tx, worker, request, uploaded)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT v.environment_id=c.environment_id AND v.computer_id=c.computer_id AND v.source_computer_instance_id=c.source_computer_instance_id AND v.writer_generation=i.writer_generation AND v.status='private' FROM computer_disk_versions v JOIN computer_checkpoints c ON c.private_computer_disk_version_id=v.id JOIN computer_instances i ON i.id=c.source_computer_instance_id WHERE v.id=$1`, cp.PrivateComputerDiskVersionID).Scan(&exact); err != nil || !exact {
		t.Fatalf("checkpoint provenance=%v %v", exact, err)
	}
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SAVEPOINT provenance`)
	_, err = tx.Exec(t.Context(), `UPDATE computer_disk_versions SET writer_generation=writer_generation+1 WHERE id=$1`, cp.PrivateComputerDiskVersionID)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23503" {
		t.Fatalf("stale source generation accepted: %v", err)
	}
	dbtest.MustExec(t, t.Context(), tx, `ROLLBACK TO SAVEPOINT provenance`)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}
