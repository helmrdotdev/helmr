package db_test

import (
	"bytes"
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func ownershipTimerWait(t *testing.T, f ownershipWaitFixture, work runtest.RunLease) db.RunWait {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, work.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='running',started_at=claimed_at WHERE id=$1`, work.LeaseID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1`, work.RunID)
	var version int64
	if err := f.Pool.QueryRow(t.Context(), "SELECT revision FROM runs WHERE id=$1", work.RunID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	w, err := f.queries.RegisterTimerRunWait(t.Context(), db.RegisterTimerRunWaitParams{
		ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(f.EnvironmentID),
		DueAt:                          pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		IdleTimeoutMs:                  pgtype.Int8{Int64: 30000, Valid: true},
		RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("ownership-wait")),
		AttemptNumber:                  1, CurrentRunLeaseID: pgvalue.UUID(work.LeaseID),
		Metadata: []byte("{}"), Tags: []string{},
		RunID: pgvalue.UUID(work.RunID), ExpectedRunningRevision: version,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func ownershipRunWaitSnapshot(t *testing.T, f ownershipWaitFixture, w db.RunWait) []byte {
	t.Helper()
	var result []byte
	if err := f.Pool.QueryRow(t.Context(), "SELECT jsonb_build_array(to_jsonb(r),to_jsonb(w)) FROM runs r JOIN run_waits w ON w.run_id=r.id WHERE w.id=$1", w.ID).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestOwnershipHotWaitRejectsBeforeAnyMutation(t *testing.T) {
	f := newOwnershipWaitFixture(t)
	work := f.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	w := ownershipTimerWait(t, f, work)
	valid := db.CompleteHotRunWaitParams{ID: w.ID, RunID: w.RunID, ExpectedRunRevision: w.ExpectedRunRevision, CurrentRunLeaseID: w.CurrentRunLeaseID, AttemptNumber: w.AttemptNumber}
	for _, test := range []struct {
		name   string
		mutate func(*db.CompleteHotRunWaitParams)
	}{
		{"missing wait", func(p *db.CompleteHotRunWaitParams) { p.ID = pgvalue.UUID(uuid.NewV7()) }},
		{"wrong version", func(p *db.CompleteHotRunWaitParams) { p.ExpectedRunRevision++ }},
		{"wrong attempt", func(p *db.CompleteHotRunWaitParams) { p.AttemptNumber++ }},
		{"wrong lease", func(p *db.CompleteHotRunWaitParams) { p.CurrentRunLeaseID = pgvalue.UUID(uuid.NewV7()) }},
		{"record on timer", func(p *db.CompleteHotRunWaitParams) { p.CompletedTurnID = pgvalue.UUID(uuid.NewV7()) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := ownershipRunWaitSnapshot(t, f, w)
			p := valid
			test.mutate(&p)
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err := db.New(tx).CompleteHotRunWait(t.Context(), p); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("rejection=%v", err)
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			if after := ownershipRunWaitSnapshot(t, f, w); !bytes.Equal(before, after) {
				t.Fatalf("durable partial mutation: before=%s after=%s", before, after)
			}
		})
	}
	if _, err := f.queries.CompleteHotRunWait(t.Context(), valid); err != nil {
		t.Fatal(err)
	}
	before := ownershipRunWaitSnapshot(t, f, w)
	if _, err := f.queries.CompleteHotRunWait(t.Context(), valid); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("repeat=%v", err)
	}
	if !bytes.Equal(before, ownershipRunWaitSnapshot(t, f, w)) {
		t.Fatal("repeat mutated rows")
	}
}

func TestOwnershipLastLockedWaitRejectsCheckpointRequestRace(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "complete"
		if fail {
			name = "fail"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			f := newOwnershipWaitFixture(t)
			work := f.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
			w := ownershipTimerWait(t, f, work)
			checkpointID := uuid.NewV7()
			tx, err := f.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			ownershipBeginCapture(t, f, work, tx, pgvalue.UUID(checkpointID))
			conn, err := f.Pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			pid := conn.Conn().PgConn().PID()
			done := make(chan error, 1)
			go func() {
				q := db.New(conn)
				if fail {
					_, err := q.FailHotRunWait(ctx, db.FailHotRunWaitParams{ID: w.ID, RunID: w.RunID, ExpectedRunRevision: w.ExpectedRunRevision, CurrentRunLeaseID: w.CurrentRunLeaseID, AttemptNumber: w.AttemptNumber, ReasonCode: pgvalue.Text("timer_failed")})
					done <- err
				} else {
					_, err := q.CompleteHotRunWait(ctx, db.CompleteHotRunWaitParams{ID: w.ID, RunID: w.RunID, ExpectedRunRevision: w.ExpectedRunRevision, CurrentRunLeaseID: w.CurrentRunLeaseID, AttemptNumber: w.AttemptNumber})
					done <- err
				}
			}()
			deadline := time.Now().Add(10 * time.Second)
			for {
				var blocked bool
				if err := f.Pool.QueryRow(ctx, "SELECT cardinality(pg_blocking_pids($1))>0", pid).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("completion did not reach db.Run lock")
				}
				time.Sleep(time.Millisecond)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("stale transition=%v", err)
			}
			got, err := f.queries.GetRunWait(ctx, db.GetRunWaitParams{ID: w.ID, RunID: w.RunID, AttemptNumber: w.AttemptNumber})
			if err != nil {
				t.Fatal(err)
			}
			var state db.RunStatus
			var version int64
			if err := f.Pool.QueryRow(ctx, "SELECT status,revision FROM runs WHERE id=$1", w.RunID).Scan(&state, &version); err != nil {
				t.Fatal(err)
			}
			if state != db.RunStatusWaiting || version != w.ExpectedRunRevision || got.SuspensionStatus != db.RunWaitStatusCheckpointing || got.ConditionStatus != db.WaitStatusPending {
				t.Fatalf("partial mutation run=%s/%d wait=%+v", state, version, got)
			}
		})
	}
}

func TestOwnershipActorInputStoredRoleGatesAllSuspensions(t *testing.T) {
	for _, state := range []string{"hot", "checkpointing", "parked"} {
		t.Run(state, func(t *testing.T) {
			ctx := t.Context()
			f := newOwnershipWaitFixture(t)
			work := f.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
			sessionID := f.ConvertToActor(t, ctx, work, `{"enabled":false}`)
			w := ownershipTimerWait(t, f, work)
			dbtest.MustExec(t, ctx, f.Pool, "UPDATE run_waits SET kind='actor_input',due_at=NULL,session_id=$2,after_input_sequence=2 WHERE id=$1", w.ID, sessionID)
			checkpointID := uuid.NewV7()
			if state != "hot" {
				ownershipCapture(t, f, work, pgvalue.UUID(checkpointID))
			}
			if state == "parked" {
				dbtest.MustExec(t, ctx, f.Pool, "UPDATE run_waits SET suspension_status='parked',prior_run_lease_id=current_run_lease_id,current_run_lease_id=NULL WHERE id=$1", w.ID)
				dbtest.MustExec(t, ctx, f.Pool, "UPDATE runs SET current_run_lease_id=NULL WHERE id=$1", w.RunID)
			}
			inputID, outputID := uuid.NewV7(), uuid.NewV7()
			dbtest.MustExec(t, ctx, f.Pool, "INSERT INTO session_turns(id,environment_id,session_id,sequence,data) VALUES ($1,$2,$3,10,'{}')", inputID, f.EnvironmentID, sessionID)
			dbtest.MustExec(t, ctx, f.Pool, "INSERT INTO session_events(id,environment_id,session_id,computer_id,sequence,kind,data,producer_run_id,producer_attempt_number,run_generation) SELECT $1,$2,$3,computer_id,10,'output','{}',$4,1,1 FROM sessions WHERE id=$3", outputID, f.EnvironmentID, sessionID, work.RunID)
			other := f.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
			otherSession := uuid.NewV7()
			dbtest.MustExec(t, ctx, f.Pool, "INSERT INTO sessions(id,environment_id,actor_declared_id,deployment_definition_id,computer_id,run_queue_name,run_max_active_duration_ms,run_retry_policy) SELECT $1,s.environment_id,s.actor_declared_id,s.deployment_definition_id,r.computer_id,s.run_queue_name,s.run_max_active_duration_ms,s.run_retry_policy FROM sessions s CROSS JOIN runs r WHERE s.id=$2 AND r.id=$3", otherSession, sessionID, other.RunID)
			otherInput := uuid.NewV7()
			dbtest.MustExec(t, ctx, f.Pool, "INSERT INTO session_turns(id,environment_id,session_id,sequence,data) VALUES ($1,$2,$3,10,'{}')", otherInput, f.EnvironmentID, otherSession)
			complete := func(q *db.Queries, id pgtype.UUID) (db.RunWait, error) {
				switch state {
				case "hot":
					return q.CompleteHotRunWait(ctx, db.CompleteHotRunWaitParams{ID: w.ID, RunID: w.RunID, ExpectedRunRevision: w.ExpectedRunRevision, CurrentRunLeaseID: w.CurrentRunLeaseID, AttemptNumber: 1, CompletedTurnID: id})
				case "checkpointing":
					return q.CompleteCheckpointingRunWait(ctx, db.CompleteCheckpointingRunWaitParams{ID: w.ID, RunID: w.RunID, ExpectedRunRevision: w.ExpectedRunRevision, CurrentRunLeaseID: w.CurrentRunLeaseID, CompletedTurnID: id})
				default:
					return q.CompleteParkedRunWait(ctx, db.CompleteParkedRunWaitParams{ID: w.ID, RunID: w.RunID, ExpectedRunRevision: w.ExpectedRunRevision, PriorRunLeaseID: w.CurrentRunLeaseID, AttemptNumber: 1, SuspendCheckpointID: pgvalue.UUID(checkpointID), CompletedTurnID: id})
				}
			}
			for _, test := range []struct {
				name string
				id   pgtype.UUID
			}{
				{"null", pgtype.UUID{}}, {"missing", pgvalue.UUID(uuid.NewV7())}, {"same session output", pgvalue.UUID(outputID)}, {"different session input", pgvalue.UUID(otherInput)},
			} {
				t.Run(test.name, func(t *testing.T) {
					before := ownershipRunWaitSnapshot(t, f, w)
					tx, err := f.Pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(ctx)
					if _, err := complete(db.New(tx), test.id); !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("rejection=%v", err)
					}
					if err := tx.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(before, ownershipRunWaitSnapshot(t, f, w)) {
						t.Fatal("rejected record caused durable mutation")
					}
				})
			}
			got, err := complete(f.queries, pgvalue.UUID(inputID))
			if err != nil {
				t.Fatal(err)
			}
			if got.CompletedTurnID != pgvalue.UUID(inputID) || got.ConditionStatus != db.WaitStatusCompleted {
				t.Fatalf("completion=%+v", got)
			}
		})
	}
}

func TestOwnershipFailureTransitionsRejectBeforeMutation(t *testing.T) {
	for _, state := range []string{"hot", "checkpointing", "parked"} {
		t.Run(state, func(t *testing.T) {
			ctx := t.Context()
			f := newOwnershipWaitFixture(t)
			work := f.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
			w := ownershipTimerWait(t, f, work)
			checkpointID := pgvalue.UUID(uuid.NewV7())
			if state != "hot" {
				ownershipCapture(t, f, work, checkpointID)
			}
			if state == "parked" {
				dbtest.MustExec(t, ctx, f.Pool, "UPDATE run_waits SET suspension_status='parked',prior_run_lease_id=current_run_lease_id,current_run_lease_id=NULL WHERE id=$1", w.ID)
				dbtest.MustExec(t, ctx, f.Pool, "UPDATE runs SET current_run_lease_id=NULL WHERE id=$1", w.RunID)
			}
			fail := func(q *db.Queries, id pgtype.UUID, version int64) (db.RunWait, error) {
				switch state {
				case "hot":
					return q.FailHotRunWait(ctx, db.FailHotRunWaitParams{ID: id, RunID: w.RunID, ExpectedRunRevision: version, CurrentRunLeaseID: w.CurrentRunLeaseID, AttemptNumber: 1, ReasonCode: pgvalue.Text("timeout")})
				case "checkpointing":
					return q.FailCheckpointingRunWait(ctx, db.FailCheckpointingRunWaitParams{ID: id, RunID: w.RunID, ExpectedRunRevision: version, CurrentRunLeaseID: w.CurrentRunLeaseID, ReasonCode: pgvalue.Text("timeout")})
				default:
					return q.FailParkedRunWait(ctx, db.FailParkedRunWaitParams{ID: id, RunID: w.RunID, ExpectedRunRevision: version, PriorRunLeaseID: w.CurrentRunLeaseID, AttemptNumber: 1, SuspendCheckpointID: checkpointID, ReasonCode: pgvalue.Text("timeout")})
				}
			}
			for _, test := range []struct {
				id      pgtype.UUID
				version int64
			}{
				{pgvalue.UUID(uuid.NewV7()), w.ExpectedRunRevision}, {w.ID, w.ExpectedRunRevision + 1},
			} {
				before := ownershipRunWaitSnapshot(t, f, w)
				tx, err := f.Pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fail(db.New(tx), test.id, test.version); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("rejection=%v", err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, ownershipRunWaitSnapshot(t, f, w)) {
					t.Fatal("failed rejection durably moved db.Run/Wait")
				}
			}
			if _, err := fail(f.queries, w.ID, w.ExpectedRunRevision); err != nil {
				t.Fatal(err)
			}
			before := ownershipRunWaitSnapshot(t, f, w)
			if _, err := fail(f.queries, w.ID, w.ExpectedRunRevision); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("repeat=%v", err)
			}
			if !bytes.Equal(before, ownershipRunWaitSnapshot(t, f, w)) {
				t.Fatal("repeated failure mutated rows")
			}
		})
	}
}

type ownershipWaitFixture struct {
	runtest.Fixture
	queries *db.Queries
}

func newOwnershipWaitFixture(t *testing.T) ownershipWaitFixture {
	f := runtest.New(t)
	return ownershipWaitFixture{Fixture: f, queries: db.New(f.Pool)}
}
func ownershipCapture(t *testing.T, f ownershipWaitFixture, work runtest.RunLease, id pgtype.UUID) {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	ownershipBeginCapture(t, f, work, tx, id)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func ownershipBeginCapture(t *testing.T, f ownershipWaitFixture, work runtest.RunLease, tx pgx.Tx, id pgtype.UUID) {
	t.Helper()
	p := computer.Capture{CheckpointID: pgvalue.MustUUIDValue(id), EnvironmentID: f.EnvironmentID}
	if err := tx.QueryRow(t.Context(), `SELECT i.id,i.writer_generation,i.membership_revision,i.desired_version FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id WHERE l.id=$1`, work.LeaseID).Scan(&p.InstanceID, &p.WriterGeneration, &p.MembershipRevision, &p.DesiredVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := computer.BeginCapture(t.Context(), tx, p); err != nil {
		t.Fatal(err)
	}
}
