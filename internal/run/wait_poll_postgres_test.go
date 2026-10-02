package run

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// afterPollRead commits a concurrent mutation after the first database result
// has been consumed, before PollWait can issue another statement.
type afterPollRead struct {
	db.DBTX
	after func()
}

func (s *afterPollRead) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return pollReadRow{Row: s.DBTX.QueryRow(ctx, sql, args...), store: s}
}

type pollReadRow struct {
	pgx.Row
	store *afterPollRead
}

func (r pollReadRow) Scan(dest ...any) error {
	if err := r.Row.Scan(dest...); err != nil {
		return err
	}
	if after := r.store.after; after != nil {
		r.store.after = nil
		after()
	}
	return nil
}

func TestPollWaitConcurrentSessionInterrupt(t *testing.T) {
	ctx := t.Context()
	f := newPostgresFixture(t)
	work := f.addRun(t, "assigned", time.Now().Add(-time.Minute))
	sessionID := f.convertToActor(t, ctx, work, `{"enabled":false}`)
	turnID := activateCancellationTurn(t, f, work, sessionID)
	r, err := f.queries.GetRun(ctx, db.GetRunParams{EnvironmentID: pgvalue.UUID(f.environmentID), ID: pgvalue.UUID(work.runID)})
	if err != nil {
		t.Fatal(err)
	}
	wait, err := f.queries.RegisterTimerRunWait(ctx, db.RegisterTimerRunWaitParams{
		ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: r.EnvironmentID, RunID: r.ID,
		AttemptNumber: 1, CurrentRunLeaseID: pgvalue.UUID(work.leaseID), ExpectedRunningRevision: r.Revision,
		DueAt: pgvalue.Timestamptz(time.Now().Add(time.Hour)), IdleTimeoutMs: pgtype.Int8{Int64: 30000, Valid: true},
		RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("interrupt-poll")), Metadata: []byte(`{}`), Tags: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := f.queries.GetSession(ctx, db.GetSessionParams{EnvironmentID: r.EnvironmentID, ID: pgvalue.UUID(sessionID)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.queries.BindRunWaitTurn(ctx, db.BindRunWaitTurnParams{SessionID: session.ID, TurnID: pgvalue.UUID(turnID), RunGeneration: pgtype.Int8{Int64: session.RunGeneration, Valid: true}, WaitID: wait.ID})
	if err != nil {
		t.Fatal(err)
	}
	scope := WaitPollScope{RunID: r.ID, AttemptNumber: 1, ComputerID: r.ComputerID, LeaseID: pgvalue.UUID(work.leaseID)}
	reader := &afterPollRead{DBTX: f.pool, after: func() {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		graph, err := LockOwnedFinalization(ctx, tx, OwnedFinalizationRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, RunID: work.runID})
		if err != nil {
			t.Fatal(err)
		}
		q := db.New(tx)
		locked, err := q.LockSessionTurnAuthority(ctx, db.LockSessionTurnAuthorityParams{EnvironmentID: r.EnvironmentID, ID: session.ID})
		if err != nil {
			t.Fatal(err)
		}
		held, err := HoldSessionExecution(ctx, q, locked, 1, "interrupt_requested")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = graph.RequestHeldSessionStop(ctx, pgvalue.MustUUIDValue(held.DispatchHoldID)); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}}
	// A poll whose snapshot precedes interruption may still return waiting; it
	// must never combine that hot wait with the later revoked Turn.
	first, stopped, err := PollWait(ctx, db.New(reader), scope, wait.ID)
	if err != nil || stopped || first.SuspensionStatus != db.RunWaitStatusHot {
		t.Fatalf("pre-interrupt snapshot: wait=%s stopped=%v err=%v", first.SuspensionStatus, stopped, err)
	}
	second, stopped, err := PollWait(ctx, f.queries, scope, wait.ID)
	if err != nil || !stopped || second.SuspensionStatus != db.RunWaitStatusReleased || second.ConditionReasonCode.String != "session_stopped" {
		t.Fatalf("post-interrupt snapshot: wait=%+v stopped=%v err=%v", second, stopped, err)
	}
}
