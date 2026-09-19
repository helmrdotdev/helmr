package token

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Pause only the first replay result, after PostgreSQL has completed its read.
// All queries and transactions still use the real database.
type replayReadBarrier struct {
	WaitDB
	read   chan error
	resume chan struct{}
}

func (b *replayReadBarrier) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.WaitDB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &replayReadTx{Tx: tx, barrier: b}, nil
}

type replayReadTx struct {
	pgx.Tx
	barrier *replayReadBarrier
	paused  bool
}

func (tx *replayReadTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := tx.Tx.QueryRow(ctx, sql, args...)
	if !tx.paused && strings.HasPrefix(sql, "-- name: GetTokenWaitRegistrationReplay ") {
		tx.paused = true
		return replayReadRow{Row: row, ctx: ctx, barrier: tx.barrier}
	}
	return row
}

type replayReadRow struct {
	pgx.Row
	ctx     context.Context
	barrier *replayReadBarrier
}

func (row replayReadRow) Scan(dest ...any) error {
	err := row.Row.Scan(dest...)
	row.barrier.read <- err
	select {
	case <-row.barrier.resume:
		return err
	case <-row.ctx.Done():
		return row.ctx.Err()
	}
}

func TestTokenWaitRegistrationReplayConcurrentCommit(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "identical"
		if changed {
			name = "changed_request"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fixture := newRunLeaseClaimFixture(t, ctx)
			work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
			startTaskCompletionWork(t, ctx, fixture, work)
			tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
			request := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, uuid.NewV7())
			var initialVersion int64
			if err := fixture.pool.QueryRow(ctx, "SELECT revision FROM runs WHERE id=$1", work.runID).Scan(&initialVersion); err != nil {
				t.Fatal(err)
			}
			barrier := &replayReadBarrier{WaitDB: fixture.pool, read: make(chan error, 1), resume: make(chan struct{})}
			second, err := NewWaitReconciler(barrier)
			if err != nil {
				t.Fatal(err)
			}
			first, err := NewWaitReconciler(fixture.pool)
			if err != nil {
				t.Fatal(err)
			}
			other := request
			if changed {
				other.Metadata = []byte(`{"changed":true}`)
			}
			type outcome struct {
				result WaitRegistrationResult
				err    error
			}
			outcomes := make(chan outcome, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				result, err := second.RegisterWait(ctx, other)
				outcomes <- outcome{result, err}
			}()
			// Cancellation releases the barrier and joins the caller even on Fatal.
			defer func() { cancel(); <-done }()
			select {
			case err := <-barrier.read:
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("first replay read = %v, want absent", err)
				}
			case got := <-outcomes:
				t.Fatalf("registration finished before replay barrier: %+v", got)
			}
			registered, err := first.RegisterWait(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			close(barrier.resume)
			got := <-outcomes
			if registered.WaitID != request.WaitID || registered.RunRevision != initialVersion+1 || registered.ConditionStatus != db.WaitStatusPending || registered.SuspensionStatus != db.RunWaitStatusHot {
				t.Fatalf("initial registration = %+v, initial version %d", registered, initialVersion)
			}
			if changed {
				if !errors.Is(got.err, ErrWaitAuthority) || got.err.Error() != ErrWaitAuthority.Error()+": token wait registration replay does not match" {
					t.Fatalf("changed replay error = %v", got.err)
				}
			} else if got.err != nil || !reflect.DeepEqual(got.result, registered) {
				t.Fatalf("concurrent replay = %+v, %v; first = %+v", got.result, got.err, registered)
			}
			var count int
			var version int64
			if err := fixture.pool.QueryRow(ctx, `SELECT revision, (SELECT count(*) FROM run_waits WHERE id=$2) FROM runs WHERE id=$1`, work.runID, request.WaitID).Scan(&version, &count); err != nil {
				t.Fatal(err)
			}
			if count != 1 || version != registered.RunRevision {
				t.Fatalf("durable wait count/version = %d/%d; want 1/%d", count, version, registered.RunRevision)
			}
			replayed, err := first.RegisterWait(ctx, request)
			if err != nil || !reflect.DeepEqual(replayed, registered) {
				t.Fatalf("durable replay = %+v, %v; first = %+v", replayed, err, registered)
			}
		})
	}
}

func TestTokenWaitRegistrationReplayClassifiesConflicts(t *testing.T) {
	ctx := t.Context()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, work)
	otherWork := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	request := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, uuid.NewV7())
	request.Metadata, request.Tags = []byte(`{}`), []string{}
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	query := func(request WaitRegistration) (db.GetTokenWaitRegistrationReplayRow, error) {
		return fixture.queries.GetTokenWaitRegistrationReplay(ctx, db.GetTokenWaitRegistrationReplayParams{
			WaitID: pgvalue.UUID(request.WaitID), RunLeaseID: pgvalue.UUID(request.RunLeaseID),
			TokenID: pgvalue.UUID(request.TokenID), ResumeAttachID: pgvalue.UUID(request.ResumeAttachID),
			RequestFingerprint: request.RequestFingerprint, Metadata: request.Metadata, Tags: request.Tags,
			LeaseSequence: request.LeaseSequence, WorkerGroupID: pgvalue.UUID(request.WorkerGroupID),
			WorkerInstanceID: pgvalue.UUID(request.WorkerInstanceID), WorkerEpoch: request.WorkerEpoch,
			ActorSpeculativeInputSequence: request.ActorSpeculativeInputSequence,
		})
	}
	if row, err := query(request); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("absent replay = %+v, %v", row, err)
	}
	registered, err := reconciler.RegisterWait(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	row, err := query(request)
	if err != nil || !row.Matches || row.WaitID != pgvalue.UUID(request.WaitID) ||
		!row.RunRevision.Valid || row.RunRevision.Int64 != registered.RunRevision ||
		!row.ConditionStatus.Valid || row.ConditionStatus.String != string(registered.ConditionStatus) ||
		!row.SuspensionStatus.Valid || row.SuspensionStatus.String != string(registered.SuspensionStatus) {
		t.Fatalf("matched replay = %+v, %v; registered = %+v", row, err, registered)
	}
	for _, name := range []string{"metadata", "missing_lease", "wrong_lease", "zero_cursor", "non_token"} {
		t.Run(name, func(t *testing.T) {
			changed := request
			switch name {
			case "metadata":
				changed.Metadata = []byte(`{"changed":true}`)
			case "missing_lease":
				changed.RunLeaseID = uuid.NewV7()
			case "wrong_lease":
				changed.RunLeaseID = otherWork.leaseID
			case "zero_cursor":
				changed.ActorSpeculativeInputSequence = pgtype.Int8{Int64: 0, Valid: true}
			case "non_token":
				dbtest.MustExec(t, ctx, fixture.pool, `UPDATE run_waits SET kind='timer', token_id=NULL, token_registration_run_revision=NULL, timeout_at=NULL, due_at=now() WHERE id=$1`, request.WaitID)
			}
			const snapshot = `SELECT jsonb_build_object('wait', to_jsonb(w), 'run', to_jsonb(r)) FROM run_waits w JOIN runs r ON r.id=w.run_id WHERE w.id=$1`
			var before, after []byte
			if err := fixture.pool.QueryRow(ctx, snapshot, request.WaitID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			// A shared authority sentinel alone would also accept a NULL scan failure.
			row, err := query(changed)
			if err != nil || row.Matches || row.WaitID != pgvalue.UUID(request.WaitID) {
				t.Fatalf("conflict classification = %+v, %v; want addressed ID and Matches=false", row, err)
			}
			if row.RunRevision.Valid || row.ConditionStatus.Valid || row.SuspensionStatus.Valid {
				t.Fatalf("conflict unexpectedly has replay values: %+v", row)
			}
			if _, err := reconciler.RegisterWait(ctx, changed); !errors.Is(err, ErrWaitAuthority) || err.Error() != ErrWaitAuthority.Error()+": token wait registration replay does not match" {
				t.Fatalf("conflict operation = %v", err)
			}
			if err := fixture.pool.QueryRow(ctx, snapshot, request.WaitID).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("conflicting replay mutated durable wait/run")
			}
		})
	}
}
