package computer

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"io"
	"log/slog"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

// setUnusedRetention sets the unused period on the Computer's origin Template
// definition; nil means never.
func setUnusedRetention(t *testing.T, f sessiontest.Fixture, ms *int64) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE template_definitions d SET delete_after_unused_ms=$2 FROM computers c WHERE c.id=$1 AND (d.environment_id,d.template_id,d.deployment_id)=(c.environment_id,c.origin_template_id,c.origin_deployment_id)`, f.Computer, ms)
}

func TestComputerUnusedRetentionPreservesBusyAndNever(t *testing.T) {
	f := sessiontest.New(t)
	ctx := t.Context()
	hour := int64(3600000)
	setUnusedRetention(t, f, &hour)
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE computers SET unused_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.Computer)
	if _, err := reconcileUnused(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Environment, f.Computer); err != nil {
		t.Fatal(err)
	}
	var deleted bool
	var since *time.Time
	read := func() {
		t.Helper()
		if err := f.Pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL,unused_since FROM computers WHERE id=$1`, f.Computer).Scan(&deleted, &since); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if deleted || since != nil {
		t.Fatal("open member did not reset unused clock")
	}
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE sessions SET status='closed',close_how='close' WHERE id=$1`, f.Session)
	if _, err := reconcileUnused(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Environment, f.Computer); err != nil {
		t.Fatal(err)
	}
	read()
	if deleted || since != nil {
		t.Fatal("unfenced process did not block deletion")
	}
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1`, f.Session)
	if _, err := reconcileUnused(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Environment, f.Computer); err != nil {
		t.Fatal(err)
	}
	read()
	if deleted || since == nil {
		t.Fatal("safe unused Computer did not start grace")
	}
	setUnusedRetention(t, f, nil)
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE computers SET unused_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.Computer)
	if _, err := reconcileUnused(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Environment, f.Computer); err != nil {
		t.Fatal(err)
	}
	read()
	if deleted {
		t.Fatal("never failed to preserve Computer")
	}
	ms := int64(1)
	setUnusedRetention(t, f, &ms)
	if _, err := reconcileUnused(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Environment, f.Computer); err != nil {
		t.Fatal(err)
	}
	read()
	if !deleted {
		t.Fatal("safe Computer with inert lease did not delete")
	}
}

func TestComputerUnusedDoesNotRestartAfterInternalSave(t *testing.T) {
	f := sessiontest.New(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE sessions SET status='closed',close_how='close' WHERE id=$1`, f.Session)
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1`, f.Session)
	minute := int64(60000)
	setUnusedRetention(t, f, &minute)
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE computers SET unused_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.Computer)
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq,status,failure_evidence) VALUES($1,$2,$3,1,1,'failed','settled capture failure')`, f.Environment, uuid.NewV7(), f.Computer)
	if _, err := reconcileUnused(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Environment, f.Computer); err != nil {
		t.Fatal(err)
	}
	var deleted bool
	if err := f.Pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM computers WHERE id=$1`, f.Computer).Scan(&deleted); err != nil || !deleted {
		t.Fatalf("internal save extended the unused interval: %v %v", deleted, err)
	}
}

func TestComputerUnusedPendingSaveDefersWithoutRestartingClock(t *testing.T) {
	f := sessiontest.New(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE sessions SET status='closed',close_how='close' WHERE id=$1`, f.Session)
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1`, f.Session)
	minute := int64(60000)
	setUnusedRetention(t, f, &minute)
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE computers SET unused_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.Computer)
	save := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq) VALUES($1,$2,$3,1,1)`, f.Environment, save, f.Computer)
	if _, err := reconcileUnused(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Environment, f.Computer); err != nil {
		t.Fatal(err)
	}
	var deleted, aged bool
	if err := f.Pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL,unused_since<clock_timestamp()-interval '1 hour' FROM computers WHERE id=$1`, f.Computer).Scan(&deleted, &aged); err != nil || deleted || !aged {
		t.Fatalf("pending save: deleted=%v clock kept=%v %v", deleted, aged, err)
	}
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE computer_saves SET status='failed',failure_evidence='settled capture failure' WHERE id=$1`, save)
	if _, err := reconcileUnused(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Environment, f.Computer); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM computers WHERE id=$1`, f.Computer).Scan(&deleted); err != nil || !deleted {
		t.Fatalf("settled save granted a new unused period: %v %v", deleted, err)
	}
}

func TestComputerUnusedScanSkipsFutureQuietClock(t *testing.T) {
	f := sessiontest.New(t)
	ctx := t.Context()
	day := int64(86400000)
	setUnusedRetention(t, f, &day)
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE computers SET unused_since=clock_timestamp() WHERE id=$1`, f.Computer)
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM computers WHERE id=$1 FOR NO KEY UPDATE`, f.Computer); err != nil {
		t.Fatal(err)
	}
	scan, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if _, _, err := reconcileRetention(scan, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, slog.New(slog.NewTextHandler(io.Discard, nil)), retentionPosition{}); err != nil {
		t.Fatalf("future quiet clock unnecessarily locked: %v", err)
	}
}
