package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbpool"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

type finalizeClaimTracer struct{ groups, hosts atomic.Int64 }

func (tr *finalizeClaimTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT claim_version,status FROM worker_groups") {
		tr.groups.Add(1)
	}
	if strings.HasPrefix(data.SQL, "SELECT claim_version,current_epoch,status FROM worker_hosts") {
		tr.hosts.Add(1)
	}
	return ctx
}
func (*finalizeClaimTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestRuntimeFinalizeLocksSupplyOnceOnCompletionAndReplay(t *testing.T) {
	f := newFixture(t)
	a := f.enqueue(t, "result")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	if err := session.RuntimeCloseProcessing(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), f.execution(), a.TurnID); err != nil {
		t.Fatal(err)
	}
	tracer := &finalizeClaimTracer{}
	config := f.pool.Config()
	config.ConnConfig.Tracer = tracer
	pool, err := dbpool.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for range 2 {
		tracer.groups.Store(0)
		tracer.hosts.Store(0)
		if outcome, err := session.RuntimeFinalize(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, pool, *f.host(), f.execution(), a.TurnID, json.RawMessage(`1`), "joined"); err != nil || outcome == nil {
			t.Fatalf("finalize %s %v", outcome, err)
		}
		if tracer.groups.Load() != 1 || tracer.hosts.Load() != 1 {
			t.Fatalf("repeated supply locks: group=%d host=%d", tracer.groups.Load(), tracer.hosts.Load())
		}
	}
}

func TestRuntimeFinalizeRechecksLeaseExpiryAfterOwnerWait(t *testing.T) {
	f := newFixture(t)
	a := f.enqueue(t, "result")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	if err := session.RuntimeCloseProcessing(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), f.execution(), a.TurnID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SELECT id FROM computers WHERE id=$1 FOR NO KEY UPDATE`, f.computer)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		_, err := session.RuntimeFinalize(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), f.execution(), a.TurnID, json.RawMessage(`1`), "joined")
		done <- err
	}()
	defer func() { cancel(); <-joined }()
	waitSessionLifecycleLock(t, f)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE computer_id=$1`, f.computer)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, session.ErrNotReady) {
		t.Fatalf("expired authority admitted result: %v", err)
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT result_digest IS NULL AND NOT EXISTS(SELECT 1 FROM computer_saves WHERE turn_id=$1) FROM turns WHERE id=$1`, a.TurnID).Scan(&valid); err != nil || !valid {
		t.Fatalf("expired result persisted: %v %v", valid, err)
	}
}
