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
	"github.com/helmrdotdev/helmr/internal/lifetime"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestComputerUnusedRetentionPreservesBusyAndExplicitNever(t *testing.T) {
	f := sessiontest.New(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE computers SET delete_after_unused_ms=3600000,unused_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.Computer)
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
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE sessions SET status='closed' WHERE id=$1`, f.Session)
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
	var scope Scope
	scope.EnvironmentID = f.Environment
	if err := f.Pool.QueryRow(ctx, `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&scope.OrgID, &scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateRetention(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Principal(t), scope, f.Computer, lifetime.Policy{Set: true}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE computers SET unused_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.Computer)
	if _, err := reconcileUnused(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Environment, f.Computer); err != nil {
		t.Fatal(err)
	}
	read()
	if deleted {
		t.Fatal("never failed to preserve Computer")
	}
	ms := int64(1)
	if _, err := UpdateRetention(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Principal(t), scope, f.Computer, lifetime.Policy{Set: true, Milliseconds: &ms}); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileUnused(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Environment, f.Computer); err != nil {
		t.Fatal(err)
	}
	read()
	if !deleted {
		t.Fatal("safe Computer with inert lease did not delete")
	}
	if _, err := UpdateRetention(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Principal(t), scope, f.Computer, lifetime.Policy{Set: true}); err == nil {
		t.Fatal("deleted Computer was revived")
	}
}

func TestComputerUnusedRestartsAfterSaveBetweenScans(t *testing.T) {
	f := sessiontest.New(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE sessions SET status='closed' WHERE id=$1`, f.Session)
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE session_id=$1`, f.Session)
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE computers SET delete_after_unused_ms=60000,unused_since=clock_timestamp()-interval '1 day' WHERE id=$1`, f.Computer)
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq,status,failure_evidence) VALUES($1,$2,$3,1,1,'failed','settled capture failure')`, f.Environment, uuid.NewV7(), f.Computer)
	if _, err := reconcileUnused(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Environment, f.Computer); err != nil {
		t.Fatal(err)
	}
	var renewed bool
	if err := f.Pool.QueryRow(ctx, `SELECT deleted_at IS NULL AND unused_since>clock_timestamp()-interval '10 seconds' FROM computers WHERE id=$1`, f.Computer).Scan(&renewed); err != nil || !renewed {
		t.Fatalf("Save did not restart unused interval: %v %v", renewed, err)
	}
}

func TestComputerUnusedScanSkipsFutureQuietClock(t *testing.T) {
	f := sessiontest.New(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE computers SET delete_after_unused_ms=86400000,unused_since=clock_timestamp() WHERE id=$1`, f.Computer)
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
