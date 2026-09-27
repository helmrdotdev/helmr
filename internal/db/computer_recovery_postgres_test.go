package db_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestComputerSourceFailureEpisodeIsStablePostgres(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now().Add(-time.Minute))
	var instance, computer, head uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT i.id,i.computer_id,c.head_disk_version_id FROM run_leases l JOIN computer_instances i ON i.id=l.computer_instance_id JOIN computers c ON c.id=i.computer_id WHERE l.id=$1`, work.LeaseID).Scan(&instance, &computer, &head); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='allocated',ready_at=NULL,observed_version=0,observed_desired_version=0 WHERE id=$1`, instance)
	p := db.MarkComputerInstanceFailedParams{ID: pgvalue.UUID(instance), WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, DesiredVersion: 1, ExpectedObservedVersion: 0, ReasonCode: pgvalue.Text(workerapi.RuntimeFailureComputerSource), Error: []byte(`{"code":"source_unavailable"}`)}
	report := func() error {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(context.Background())
		if _, err = dispatch.RecordComputerInstanceFailure(t.Context(), tx, runtest.WorkerGroupID, p); err != nil {
			return err
		}
		return tx.Commit(t.Context())
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() { <-start; results <- report() }()
	}
	close(start)
	accepted, stale := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, pgx.ErrNoRows):
			stale++
		default:
			t.Fatal(err)
		}
	}
	if accepted != 1 || stale != 1 {
		t.Fatalf("accepted=%d stale=%d", accepted, stale)
	}
	snapshot := func() string {
		var value string
		if err := f.Pool.QueryRow(t.Context(), `SELECT jsonb_build_array(recovery_id,recovery_disk_version_id,recovery_reason,recovery_started_at,recovery_payload_required)::text FROM computers WHERE id=$1`, computer).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot()
	if err := report(); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("duplicate report=%v", err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("duplicate changed episode: %s -> %s", before, after)
	}
	var source uuid.UUID
	var retained bool
	var reason string
	if err := f.Pool.QueryRow(t.Context(), `SELECT recovery_disk_version_id,recovery_payload_required IS TRUE,recovery_reason FROM computers WHERE id=$1 AND recovery_id IS NOT NULL AND recovery_started_at IS NOT NULL`, computer).Scan(&source, &retained, &reason); err != nil {
		t.Fatal(err)
	}
	if source != head || !retained || reason != "computer_source_unavailable" {
		t.Fatalf("source=%s retained=%v reason=%s", source, retained, reason)
	}
	reject := func(query string, arg any, code, constraint string) {
		t.Helper()
		_, err := f.Pool.Exec(t.Context(), query, computer, arg)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != code || pgerr.ConstraintName != constraint {
			t.Fatalf("expected %s/%s got %v", code, constraint, err)
		}
	}
	reject(`UPDATE computers SET recovery_disk_version_id=$2 WHERE id=$1`, nil, "23514", "computers_recovery_tuple_check")
	other := f.AddRunLease(t, "assigned", time.Now())
	var foreignHead uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT base_computer_disk_version_id FROM runs WHERE id=$1`, other.RunID).Scan(&foreignHead); err != nil {
		t.Fatal(err)
	}
	reject(`UPDATE computers SET recovery_disk_version_id=$2 WHERE id=$1`, foreignHead, "23503", "computers_recovery_version_fk")
}
