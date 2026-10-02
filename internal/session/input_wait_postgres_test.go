package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch/dispatchtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func pendingInputWait(t *testing.T, f *sessiontest.Execution) db.RunWait {
	t.Helper()
	wait, err := RegisterInputWait(t.Context(), f.Pool, f.Fence(), inputWait(f, 0))
	if err != nil || wait.ConditionStatus != "pending" {
		t.Fatalf("register wait: %+v %v", wait, err)
	}
	return wait
}

func enqueueWaitInput(t *testing.T, f *sessiontest.Execution, data string) uuid.UUID {
	t.Helper()
	receipt, err := ApplyAdmission(t.Context(), f.Pool, AdmissionRequest{Target: executionTarget(f), Mode: EnqueueOnly, Data: json.RawMessage(data)})
	if err != nil {
		t.Fatal(err)
	}
	return receipt.TurnID
}

func expireInputWait(t *testing.T, f *sessiontest.Execution, wait db.RunWait) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET timeout_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, wait.ID)
}

func assertInputWait(t *testing.T, f *sessiontest.Execution, wait db.RunWait, status, reason string, turnID uuid.UUID) {
	t.Helper()
	got, err := db.New(f.Pool).GetRunWait(t.Context(), db.GetRunWaitParams{RunID: wait.RunID, AttemptNumber: wait.AttemptNumber, ID: wait.ID})
	if err != nil || got.ConditionStatus != status || got.ConditionReasonCode.String != reason || got.CompletedTurnID != nullableTurn(turnID) {
		t.Fatalf("wait: %+v err=%v, want %s/%s turn %s", got, err, status, reason, turnID)
	}
}

func nullableTurn(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil() {
		return pgtype.UUID{}
	}
	return pgvalue.UUID(id)
}

func TestSessionInputWaitReadyInputPrecedesCloseAndTimeoutPostgres(t *testing.T) {
	for _, trigger := range []string{"input", "close", "timeout"} {
		for _, closing := range []bool{false, true} {
			if trigger == "close" && !closing {
				continue
			}
			t.Run(trigger+"/closing="+map[bool]string{false: "false", true: "true"}[closing], func(t *testing.T) {
				f := newExecution(t, nil, true)
				wait := pendingInputWait(t, f)
				// Acceptance after the nominal deadline still precedes an uncommitted timeout.
				expireInputWait(t, f, wait)
				first := enqueueWaitInput(t, f, `"first"`)
				second := enqueueWaitInput(t, f, `"second"`)
				if closing {
					if _, err := ApplyClose(t.Context(), f.Pool, ControlRequest{Target: executionTarget(f)}); err != nil {
						t.Fatal(err)
					}
				}
				r, _ := NewReconciler(f.Pool)
				switch trigger {
				case "input":
					// A later notification must still deliver the FIFO head.
					if _, err := r.ReconcileInput(t.Context(), f.EnvironmentID, f.SessionID, second); err != nil {
						t.Fatal(err)
					}
				case "close":
					if _, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.SessionID); err != nil {
						t.Fatal(err)
					}
				case "timeout":
					if count, err := r.ReconcileTimeouts(t.Context(), 1); err != nil || count != 0 {
						t.Fatalf("timeout count=%d err=%v", count, err)
					}
				}
				assertInputWait(t, f, wait, "completed", "", first)
				if _, err := r.ReconcileInput(t.Context(), f.EnvironmentID, f.SessionID, first); err != nil {
					t.Fatal(err)
				}
				var starts int
				if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM session_events WHERE session_id=$1 AND kind='turn.started'`, f.SessionID).Scan(&starts); err != nil || starts != 1 {
					t.Fatalf("duplicate activation: starts=%d err=%v", starts, err)
				}
			})
		}
	}
}

func TestSessionInputWaitDrainedClosePrecedesTimeoutPostgres(t *testing.T) {
	for _, trigger := range []string{"close", "timeout", "registration"} {
		t.Run(trigger, func(t *testing.T) {
			f := newExecution(t, nil, true)
			var wait db.RunWait
			if trigger != "registration" {
				wait = pendingInputWait(t, f)
				expireInputWait(t, f, wait)
			}
			if _, err := ApplyClose(t.Context(), f.Pool, ControlRequest{Target: executionTarget(f)}); err != nil {
				t.Fatal(err)
			}
			r, _ := NewReconciler(f.Pool)
			switch trigger {
			case "close":
				if _, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.SessionID); err != nil {
					t.Fatal(err)
				}
			case "timeout":
				if count, err := r.ReconcileTimeouts(t.Context(), 1); err != nil || count != 0 {
					t.Fatalf("count=%d err=%v", count, err)
				}
			case "registration":
				request := inputWait(f, 0)
				request.TimeoutAt = pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}
				var err error
				wait, err = RegisterInputWait(t.Context(), f.Pool, f.Fence(), request)
				if err != nil {
					t.Fatal(err)
				}
			}
			assertInputWait(t, f, wait, "failed", "session_closed", uuid.Nil())
		})
	}
}

func TestSessionInputWaitTimeoutKeepsLaterInputPostgres(t *testing.T) {
	f := newExecution(t, nil, true)
	wait := pendingInputWait(t, f)
	expireInputWait(t, f, wait)
	r, _ := NewReconciler(f.Pool)
	if count, err := r.ReconcileTimeouts(t.Context(), 1); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	turn := enqueueWaitInput(t, f, `"later"`)
	if _, err := r.ReconcileInput(t.Context(), f.EnvironmentID, f.SessionID, turn); err != nil {
		t.Fatal(err)
	}
	assertInputWait(t, f, wait, "failed", "wait_timeout", uuid.Nil())
	next, err := RegisterInputWait(t.Context(), f.Pool, f.Fence(), inputWait(f, 0))
	if err != nil {
		t.Fatal(err)
	}
	assertInputWait(t, f, next, "completed", "", turn)
}

func TestSessionInputWaitClosingHeadDoesNotStarveTimeoutPostgres(t *testing.T) {
	f := newExecution(t, nil, true)
	head := pendingInputWait(t, f)
	expireInputWait(t, f, head)
	turn := enqueueWaitInput(t, f, `1`)
	if _, err := ApplyClose(t.Context(), f.Pool, ControlRequest{Target: executionTarget(f)}); err != nil {
		t.Fatal(err)
	}
	other := executionOn(t, f.Fixture, nil, true)
	tail := pendingInputWait(t, other)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET timeout_at=clock_timestamp()-interval '30 seconds' WHERE id=$1`, tail.ID)
	r, _ := NewReconciler(f.Pool)
	if count, err := r.ReconcileTimeouts(t.Context(), 1); err != nil || count != 0 {
		t.Fatalf("first count=%d err=%v", count, err)
	}
	assertInputWait(t, f, head, "completed", "", turn)
	if count, err := r.ReconcileTimeouts(t.Context(), 1); err != nil || count != 1 {
		t.Fatalf("second count=%d err=%v", count, err)
	}
	assertInputWait(t, other, tail, "failed", "wait_timeout", uuid.Nil())
}

type inputWaitFaultDB struct {
	db.TxDB
	fault           func(string) error
	commitErr       error
	commitFailureAt int
	begins          int
	tx              pgx.Tx
}

func (f *inputWaitFaultDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := f.TxDB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	f.begins++
	f.tx = tx
	return inputWaitFaultTx{Tx: tx, owner: f}, nil
}

type inputWaitFaultTx struct {
	pgx.Tx
	owner *inputWaitFaultDB
}

func (tx inputWaitFaultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx.owner.fault != nil {
		if err := tx.owner.fault(sql); err != nil {
			return inputWaitFaultRow{err}
		}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func (tx inputWaitFaultTx) Commit(ctx context.Context) error {
	if tx.owner.commitErr != nil && (tx.owner.commitFailureAt == 0 || tx.owner.begins == tx.owner.commitFailureAt) {
		return tx.owner.commitErr
	}
	return tx.Tx.Commit(ctx)
}

type inputWaitFaultRow struct{ err error }

func (r inputWaitFaultRow) Scan(...any) error { return r.err }

func TestSessionInputWaitTimeoutTransactionFailurePostgres(t *testing.T) {
	for _, stage := range []string{"panic", "run read", "commit"} {
		t.Run(stage, func(t *testing.T) {
			f := newExecution(t, nil, true)
			wait := pendingInputWait(t, f)
			expireInputWait(t, f, wait)
			cause := errors.New("injected backend failure")
			faultDB := &inputWaitFaultDB{TxDB: f.Pool}
			if stage == "commit" {
				faultDB.commitErr = cause
			} else {
				faultDB.fault = func(sql string) error {
					if strings.Contains(sql, "FROM runs") && strings.Contains(sql, "FOR UPDATE") {
						if stage == "panic" {
							panic(cause)
						}
						return cause
					}
					return nil
				}
			}
			r, _ := NewReconciler(faultDB)
			var recovered any
			var count int
			var err error
			func() {
				defer func() { recovered = recover() }()
				count, err = r.ReconcileTimeouts(t.Context(), 1)
			}()
			leaked := f.Pool.Stat().AcquiredConns() != 0
			// Release a leaked connection on the broken implementation before failing.
			if faultDB.tx != nil {
				_ = faultDB.tx.Rollback(context.Background())
			}
			if leaked || count != 0 {
				t.Fatalf("connection leaked=%v committed count=%d", leaked, count)
			}
			if stage == "panic" {
				if recovered != cause {
					t.Fatalf("panic=%v", recovered)
				}
			} else if !errors.Is(err, cause) {
				t.Fatalf("lost backend cause: %v", err)
			}
			assertInputWait(t, f, wait, "pending", "", uuid.Nil())
		})
	}
}

// suspendedInputWait crosses the real capture/restore database operations. The
// stored VM objects are fixture objects; this does not execute a guest or a VM.
func suspendedInputWait(t *testing.T, state string) (*sessiontest.Execution, db.RunWait) {
	t.Helper()
	if state == "hot" || state == "checkpointing" {
		f := newExecution(t, nil, true)
		wait := pendingInputWait(t, f)
		if state == "checkpointing" {
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET suspension_status='checkpointing' WHERE id=$1`, wait.ID)
		}
		return f, wait
	}
	var sessionID, runID uuid.UUID
	prepare := func(f runtest.Fixture, work runtest.RunLease) {
		sessionID = f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
		runID = work.RunID
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET committed_input_sequence=0,next_input_sequence=1 WHERE id=$1`, sessionID)
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET session_input_start_sequence=0,session_input_high_watermark=0 WHERE id=$1`, runID)
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET session_input_start_sequence=0 WHERE run_id=$1`, runID)
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET kind='session_input',due_at=NULL,session_id=$2,after_input_sequence=0 WHERE run_id=$1`, runID, sessionID)
	}
	var base runtest.Fixture
	if state == "parked" {
		f, ref, manifest, objects := computertest.ReadyCapture(t, false, prepare)
		computertest.Complete(t, f, ref, manifest, objects)
		base = f
	} else {
		f, authority, destination := dispatchtest.Restore(t, false, prepare)
		if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
			_, err := authority.CommitRestore(t.Context(), tx, destination)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		base = f
	}
	f := &sessiontest.Execution{Fixture: base, SessionID: sessionID, RunID: runID}
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM sessions WHERE id=$1`, sessionID).Scan(&f.ComputerID); err != nil {
		t.Fatal(err)
	}
	var waitID pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM run_waits WHERE run_id=$1`, runID).Scan(&waitID); err != nil {
		t.Fatal(err)
	}
	wait, err := db.New(f.Pool).GetRunWait(t.Context(), db.GetRunWaitParams{RunID: pgvalue.UUID(runID), AttemptNumber: 1, ID: waitID})
	if err != nil || string(wait.SuspensionStatus) != state {
		t.Fatalf("capture state: %+v %v, want %s", wait, err, state)
	}
	return f, wait
}

func TestSessionInputWaitSuspendedResolutionPostgres(t *testing.T) {
	for _, state := range []string{"hot", "checkpointing", "parked", "resuming"} {
		for _, result := range []string{"input", "closed", "timeout"} {
			t.Run(state+"/"+result, func(t *testing.T) {
				f, wait := suspendedInputWait(t, state)
				expireInputWait(t, f, wait)
				var turn uuid.UUID
				status, reason := "failed", "wait_timeout"
				if result == "input" {
					turn = enqueueWaitInput(t, f, `{"value":42}`)
					status, reason = "completed", ""
				}
				if result != "timeout" {
					if _, err := ApplyClose(t.Context(), f.Pool, ControlRequest{Target: executionTarget(f)}); err != nil {
						t.Fatal(err)
					}
					if result == "closed" {
						reason = "session_closed"
					}
				}
				r, _ := NewReconciler(f.Pool)
				wantCount := 0
				if result == "timeout" {
					wantCount = 1
				}
				if count, err := r.ReconcileTimeouts(t.Context(), 1); err != nil || count != wantCount {
					t.Fatalf("count=%d err=%v", count, err)
				}
				assertInputWait(t, f, wait, status, reason, turn)
				got, err := db.New(f.Pool).GetRunWait(t.Context(), db.GetRunWaitParams{RunID: wait.RunID, AttemptNumber: 1, ID: wait.ID})
				wantState := state
				if state == "hot" {
					wantState = "released"
				} else if state == "parked" {
					wantState = "resume_pending"
				}
				if err != nil || string(got.SuspensionStatus) != wantState {
					t.Fatalf("resolution crossed suspension boundary: %+v %v", got, err)
				}
			})
		}
	}
}

func TestSessionInputWaitTimeoutRechecksAcceptedInputAfterLockPostgres(t *testing.T) {
	f := newExecution(t, nil, true)
	wait := pendingInputWait(t, f)
	expireInputWait(t, f, wait)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	admission, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Rollback(context.Background())
	dbtest.MustExec(t, ctx, admission, `SELECT id FROM computers WHERE id=$1 FOR UPDATE`, f.ComputerID)
	r, _ := NewReconciler(f.Pool)
	type outcome struct {
		count int
		err   error
	}
	done := make(chan outcome, 1)
	go func() { n, err := r.ReconcileTimeouts(ctx, 1); done <- outcome{n, err} }()
	waitForBlockedBy(t, f.Pool, int(admission.Conn().PgConn().PID()))
	receipt, err := Admit(ctx, admission, AdmissionRequest{Target: executionTarget(f), Mode: EnqueueOnly, Data: json.RawMessage(`1`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Close(ctx, admission, ControlRequest{Target: executionTarget(f)}); err != nil {
		t.Fatal(err)
	}
	if err := admission.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.count != 0 {
		t.Fatalf("timeout=%+v", got)
	}
	assertInputWait(t, f, wait, "completed", "", receipt.TurnID)
}

func TestSessionInputWaitHoldAndCancellationFencePostgres(t *testing.T) {
	for _, stop := range []string{"hold", "cancel"} {
		t.Run(stop, func(t *testing.T) {
			f := newExecution(t, nil, true)
			wait := pendingInputWait(t, f)
			expireInputWait(t, f, wait)
			turn := enqueueWaitInput(t, f, `1`)
			if stop == "cancel" {
				if _, err := ApplyCancel(t.Context(), f.Pool, ControlRequest{Target: executionTarget(f)}); err != nil {
					t.Fatal(err)
				}
			} else {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET dispatch_hold_id=$2,dispatch_hold_reason='recovery_required',dispatch_hold_run_id=current_run_id,dispatch_hold_attempt_number=1,dispatch_hold_run_generation=run_generation WHERE id=$1`, f.SessionID, uuid.NewV7())
			}
			r, _ := NewReconciler(f.Pool)
			if _, err := r.ReconcileInput(t.Context(), f.EnvironmentID, f.SessionID, turn); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.SessionID); err != nil {
				t.Fatal(err)
			}
			if count, err := r.ReconcileTimeouts(t.Context(), 1); err != nil || count != 0 {
				t.Fatalf("count=%d err=%v", count, err)
			}
			var activated bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='running' FROM session_turns WHERE id=$1`, turn).Scan(&activated); err != nil || activated {
				t.Fatalf("stop activated input: %v %v", activated, err)
			}
			if stop == "hold" {
				assertInputWait(t, f, wait, "pending", "", uuid.Nil())
			}
		})
	}
}

func TestSessionInputWaitRegistrationResolutionPostgres(t *testing.T) {
	for _, result := range []string{"input", "closed", "timeout", "pending"} {
		t.Run(result, func(t *testing.T) {
			f := newExecution(t, nil, true)
			var turn uuid.UUID
			status, reason := "failed", "wait_timeout"
			if result == "input" {
				turn = enqueueWaitInput(t, f, `1`)
				status, reason = "completed", ""
			}
			if result == "input" || result == "closed" {
				if _, err := ApplyClose(t.Context(), f.Pool, ControlRequest{Target: executionTarget(f)}); err != nil {
					t.Fatal(err)
				}
				if result == "closed" {
					reason = "session_closed"
				}
			}
			request := inputWait(f, 0)
			request.TimeoutAt = pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}
			if result == "pending" {
				request.TimeoutAt.Time = time.Now().Add(time.Hour)
				status, reason = "pending", ""
			}
			wait, err := RegisterInputWait(t.Context(), f.Pool, f.Fence(), request)
			if err != nil {
				t.Fatal(err)
			}
			assertInputWait(t, f, wait, status, reason, turn)
			replay, err := RegisterInputWait(t.Context(), f.Pool, f.Fence(), request)
			if err != nil || replay.ConditionStatus != wait.ConditionStatus || replay.ConditionTerminalAt != wait.ConditionTerminalAt || replay.CompletedTurnID != wait.CompletedTurnID {
				t.Fatalf("registration replay changed result: %+v %v", replay, err)
			}
		})
	}
}

func TestSessionInputWaitPendingRegistrationReplayPostgres(t *testing.T) {
	for _, result := range []string{"input", "closed", "timeout"} {
		t.Run(result, func(t *testing.T) {
			f := newExecution(t, nil, true)
			request := inputWait(f, 0)
			request.TimeoutAt = pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
			wait, err := RegisterInputWait(t.Context(), f.Pool, f.Fence(), request)
			if err != nil || wait.ConditionStatus != "pending" {
				t.Fatalf("register pending wait: %+v %v", wait, err)
			}
			expireInputWait(t, f, wait)
			var turn uuid.UUID
			status, reason := "failed", "wait_timeout"
			if result == "input" {
				turn = enqueueWaitInput(t, f, `1`)
				status, reason = "completed", ""
			}
			if result != "timeout" {
				if _, err := ApplyClose(t.Context(), f.Pool, ControlRequest{Target: executionTarget(f)}); err != nil {
					t.Fatal(err)
				}
				if result == "closed" {
					reason = "session_closed"
				}
			}
			resolved, err := RegisterInputWait(t.Context(), f.Pool, f.Fence(), request)
			if err != nil || resolved.ConditionStatus != status || resolved.ConditionReasonCode.String != reason || resolved.CompletedTurnID != nullableTurn(turn) {
				t.Fatalf("pending registration replay: %+v %v", resolved, err)
			}
			assertInputWait(t, f, wait, status, reason, turn)
			replay, err := RegisterInputWait(t.Context(), f.Pool, f.Fence(), request)
			if err != nil || replay.ConditionTerminalAt != resolved.ConditionTerminalAt || replay.CompletedTurnID != resolved.CompletedTurnID {
				t.Fatalf("terminal replay changed result: %+v %v", replay, err)
			}
			var starts int
			wantStarts := 0
			if result == "input" {
				wantStarts = 1
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM session_events WHERE session_id=$1 AND kind='turn.started'`, f.SessionID).Scan(&starts); err != nil || starts != wantStarts {
				t.Fatalf("replay activation: starts=%d want=%d err=%v", starts, wantStarts, err)
			}
		})
	}
}

func TestSessionInputWaitCountOnlyCommittedTimeoutsPostgres(t *testing.T) {
	f := newExecution(t, nil, true)
	first := pendingInputWait(t, f)
	expireInputWait(t, f, first)
	other := executionOn(t, f.Fixture, nil, true)
	second := pendingInputWait(t, other)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET timeout_at=clock_timestamp()-interval '30 seconds' WHERE id=$1`, second.ID)
	cause := errors.New("second commit failed")
	faultDB := &inputWaitFaultDB{TxDB: f.Pool, commitErr: cause, commitFailureAt: 2}
	r, _ := NewReconciler(faultDB)
	if count, err := r.ReconcileTimeouts(t.Context(), 2); count != 1 || !errors.Is(err, cause) {
		t.Fatalf("committed count=%d err=%v", count, err)
	}
	assertInputWait(t, f, first, "failed", "wait_timeout", uuid.Nil())
	assertInputWait(t, other, second, "pending", "", uuid.Nil())
	if f.Pool.Stat().AcquiredConns() != 0 {
		t.Fatal("failed commit retained a connection")
	}
}
