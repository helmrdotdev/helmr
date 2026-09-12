package db

import (
	"bytes"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func ownershipTimerWait(t *testing.T, f runLeaseClaimFixture, work runLeaseWork) RunWait {
	t.Helper()
	startTaskCompletionWork(t, t.Context(), f, work)
	var version int64
	if err := f.pool.QueryRow(t.Context(), "SELECT state_version FROM runs WHERE id=$1", work.runID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	w, err := f.queries.RegisterTimerRunWait(t.Context(), RegisterTimerRunWaitParams{
		ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(f.environmentID),
		DueAt:                          pgvalue.Timestamptz(time.Now().Add(-time.Second)),
		IdleTimeoutMs:                  pgtype.Int8{Int64: 30000, Valid: true},
		RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("ownership-wait")),
		AttemptNumber:                  1, CurrentRunLeaseID: pgvalue.UUID(work.leaseID),
		CheckpointDueAt: pgvalue.Timestamptz(time.Now().Add(-time.Second)),
		ResumeAttachID:  pgvalue.UUID(uuid.NewV7()), Metadata: []byte("{}"), Tags: []string{},
		RunID: pgvalue.UUID(work.runID), ExpectedRunningStateVersion: version,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func ownershipRunWaitSnapshot(t *testing.T, f runLeaseClaimFixture, w RunWait) []byte {
	t.Helper()
	var result []byte
	if err := f.pool.QueryRow(t.Context(), "SELECT jsonb_build_array(to_jsonb(r),to_jsonb(w)) FROM runs r JOIN run_waits w ON w.run_id=r.id WHERE w.id=$1", w.ID).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestOwnershipHotWaitRejectsBeforeAnyMutation(t *testing.T) {
	f := newRunLeaseClaimFixture(t, t.Context())
	work := f.addWork(t, t.Context(), "starting", time.Now().Add(-time.Minute))
	w := ownershipTimerWait(t, f, work)
	valid := CompleteHotRunWaitParams{ID: w.ID, RunID: w.RunID, ExpectedRunStateVersion: w.ExpectedRunStateVersion, CurrentRunLeaseID: w.CurrentRunLeaseID, AttemptNumber: w.AttemptNumber}
	for _, test := range []struct {
		name   string
		mutate func(*CompleteHotRunWaitParams)
	}{
		{"missing wait", func(p *CompleteHotRunWaitParams) { p.ID = pgvalue.UUID(uuid.NewV7()) }},
		{"wrong version", func(p *CompleteHotRunWaitParams) { p.ExpectedRunStateVersion++ }},
		{"wrong attempt", func(p *CompleteHotRunWaitParams) { p.AttemptNumber++ }},
		{"wrong lease", func(p *CompleteHotRunWaitParams) { p.CurrentRunLeaseID = pgvalue.UUID(uuid.NewV7()) }},
		{"record on timer", func(p *CompleteHotRunWaitParams) { p.CompletedActorRecordID = pgvalue.UUID(uuid.NewV7()) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := ownershipRunWaitSnapshot(t, f, w)
			p := valid
			test.mutate(&p)
			tx, err := f.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err := New(tx).CompleteHotRunWait(t.Context(), p); !errors.Is(err, pgx.ErrNoRows) {
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
			f := newRunLeaseClaimFixture(t, ctx)
			work := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
			w := ownershipTimerWait(t, f, work)
			checkpointID := uuid.NewV7()
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			dbtest.MustExec(t, ctx, tx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", w.RunID)
			conn, err := f.pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			pid := conn.Conn().PgConn().PID()
			done := make(chan error, 1)
			go func() {
				q := New(conn)
				if fail {
					_, err := q.FailHotRunWait(ctx, FailHotRunWaitParams{ID: w.ID, RunID: w.RunID, ExpectedRunStateVersion: w.ExpectedRunStateVersion, CurrentRunLeaseID: w.CurrentRunLeaseID, AttemptNumber: w.AttemptNumber, ReasonCode: pgvalue.Text("timer_failed")})
					done <- err
				} else {
					_, err := q.CompleteHotRunWait(ctx, CompleteHotRunWaitParams{ID: w.ID, RunID: w.RunID, ExpectedRunStateVersion: w.ExpectedRunStateVersion, CurrentRunLeaseID: w.CurrentRunLeaseID, AttemptNumber: w.AttemptNumber})
					done <- err
				}
			}()
			deadline := time.Now().Add(10 * time.Second)
			for {
				var blocked bool
				if err := f.pool.QueryRow(ctx, "SELECT cardinality(pg_blocking_pids($1))>0", pid).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("completion did not reach Run lock")
				}
				time.Sleep(time.Millisecond)
			}
			dbtest.MustExec(t, ctx, tx, `
   INSERT INTO run_checkpoints(id,run_id,attempt_number,run_wait_id,source_run_lease_id,source_workspace_lease_id,workspace_id,base_workspace_version_id)
   SELECT $1,$2,1,$3,$4,id,workspace_id,base_version_id FROM workspace_leases WHERE owner_run_lease_id=$4
  `, checkpointID, work.runID, w.ID, work.leaseID)
			if _, err := New(tx).RequestRunWaitCheckpoint(ctx, RequestRunWaitCheckpointParams{ID: w.ID, RunID: w.RunID, AttemptNumber: w.AttemptNumber, CurrentRunLeaseID: w.CurrentRunLeaseID, SuspendCheckpointID: pgvalue.UUID(checkpointID)}); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("stale transition=%v", err)
			}
			got, err := f.queries.GetRunWait(ctx, GetRunWaitParams{ID: w.ID, RunID: w.RunID, AttemptNumber: w.AttemptNumber})
			if err != nil {
				t.Fatal(err)
			}
			var state RunStatus
			var version int64
			if err := f.pool.QueryRow(ctx, "SELECT status,state_version FROM runs WHERE id=$1", w.RunID).Scan(&state, &version); err != nil {
				t.Fatal(err)
			}
			if state != RunStatusWaiting || version != w.ExpectedRunStateVersion || got.SuspensionState != RunWaitStateCheckpointing || got.ConditionState != WaitStatePending {
				t.Fatalf("partial mutation run=%s/%d wait=%+v", state, version, got)
			}
		})
	}
}

func TestOwnershipActorInputStoredRoleGatesAllSuspensions(t *testing.T) {
	for _, state := range []string{"hot", "checkpointing", "parked"} {
		t.Run(state, func(t *testing.T) {
			ctx := t.Context()
			f := newRunLeaseClaimFixture(t, ctx)
			work := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
			sessionID := f.convertToActor(t, ctx, work, `{"enabled":false}`)
			w := ownershipTimerWait(t, f, work)
			dbtest.MustExec(t, ctx, f.pool, "UPDATE run_waits SET kind='actor_input',due_at=NULL,session_id=$2,after_input_sequence=2 WHERE id=$1", w.ID, sessionID)
			checkpointID := uuid.NewV7()
			if state != "hot" {
				dbtest.MustExec(t, ctx, f.pool, `
    INSERT INTO run_checkpoints(id,run_id,attempt_number,run_wait_id,source_run_lease_id,source_workspace_lease_id,workspace_id,base_workspace_version_id)
    SELECT $1,$2,1,$3,$4,id,workspace_id,base_version_id FROM workspace_leases WHERE owner_run_lease_id=$4
   `, checkpointID, work.runID, w.ID, work.leaseID)
				dbtest.MustExec(t, ctx, f.pool, "UPDATE run_waits SET suspension_state='checkpointing',suspend_checkpoint_id=$2 WHERE id=$1", w.ID, checkpointID)
			}
			if state == "parked" {
				dbtest.MustExec(t, ctx, f.pool, "UPDATE run_waits SET suspension_state='parked',prior_run_lease_id=current_run_lease_id,current_run_lease_id=NULL WHERE id=$1", w.ID)
				dbtest.MustExec(t, ctx, f.pool, "UPDATE runs SET current_run_lease_id=NULL WHERE id=$1", w.RunID)
			}
			inputID, outputID := uuid.NewV7(), uuid.NewV7()
			dbtest.MustExec(t, ctx, f.pool, "INSERT INTO session_records(id,environment_id,session_id,direction,sequence,data) VALUES ($1,$2,$3,'input',10,'{}')", inputID, f.environmentID, sessionID)
			dbtest.MustExec(t, ctx, f.pool, "INSERT INTO session_records(id,environment_id,session_id,direction,sequence,data,producer_run_id,producer_attempt_number) VALUES ($1,$2,$3,'output',10,'{}',$4,1)", outputID, f.environmentID, sessionID, work.runID)
			other := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
			otherSession := uuid.NewV7()
			dbtest.MustExec(t, ctx, f.pool, "INSERT INTO sessions(id,environment_id,actor_declared_id,deployment_definition_id,workspace_id,run_queue_name,run_max_active_duration_ms,run_retry_policy) SELECT $1,s.environment_id,s.actor_declared_id,s.deployment_definition_id,r.workspace_id,s.run_queue_name,s.run_max_active_duration_ms,s.run_retry_policy FROM sessions s CROSS JOIN runs r WHERE s.id=$2 AND r.id=$3", otherSession, sessionID, other.runID)
			otherInput := uuid.NewV7()
			dbtest.MustExec(t, ctx, f.pool, "INSERT INTO session_records(id,environment_id,session_id,direction,sequence,data) VALUES ($1,$2,$3,'input',10,'{}')", otherInput, f.environmentID, otherSession)
			complete := func(q *Queries, id pgtype.UUID) (RunWait, error) {
				switch state {
				case "hot":
					return q.CompleteHotRunWait(ctx, CompleteHotRunWaitParams{ID: w.ID, RunID: w.RunID, ExpectedRunStateVersion: w.ExpectedRunStateVersion, CurrentRunLeaseID: w.CurrentRunLeaseID, AttemptNumber: 1, CompletedActorRecordID: id})
				case "checkpointing":
					return q.CompleteCheckpointingRunWait(ctx, CompleteCheckpointingRunWaitParams{ID: w.ID, RunID: w.RunID, ExpectedRunStateVersion: w.ExpectedRunStateVersion, CurrentRunLeaseID: w.CurrentRunLeaseID, CompletedActorRecordID: id})
				default:
					return q.CompleteParkedRunWait(ctx, CompleteParkedRunWaitParams{ID: w.ID, RunID: w.RunID, ExpectedRunStateVersion: w.ExpectedRunStateVersion, PriorRunLeaseID: w.CurrentRunLeaseID, AttemptNumber: 1, SuspendCheckpointID: pgvalue.UUID(checkpointID), CompletedActorRecordID: id})
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
					tx, err := f.pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(ctx)
					if _, err := complete(New(tx), test.id); !errors.Is(err, pgx.ErrNoRows) {
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
			if got.CompletedActorRecordID != pgvalue.UUID(inputID) || got.ConditionState != WaitStateCompleted {
				t.Fatalf("completion=%+v", got)
			}
		})
	}
}

func TestOwnershipFailureTransitionsRejectBeforeMutation(t *testing.T) {
	for _, state := range []string{"hot", "checkpointing", "parked"} {
		t.Run(state, func(t *testing.T) {
			ctx := t.Context()
			f := newRunLeaseClaimFixture(t, ctx)
			work := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
			w := ownershipTimerWait(t, f, work)
			checkpointID := pgvalue.UUID(uuid.NewV7())
			if state != "hot" {
				dbtest.MustExec(t, ctx, f.pool, `INSERT INTO run_checkpoints(id,run_id,attempt_number,run_wait_id,source_run_lease_id,source_workspace_lease_id,workspace_id,base_workspace_version_id) SELECT $1,$2,1,$3,$4,id,workspace_id,base_version_id FROM workspace_leases WHERE owner_run_lease_id=$4`, checkpointID, work.runID, w.ID, work.leaseID)
				dbtest.MustExec(t, ctx, f.pool, "UPDATE run_waits SET suspension_state='checkpointing',suspend_checkpoint_id=$2 WHERE id=$1", w.ID, checkpointID)
			}
			if state == "parked" {
				dbtest.MustExec(t, ctx, f.pool, "UPDATE run_waits SET suspension_state='parked',prior_run_lease_id=current_run_lease_id,current_run_lease_id=NULL WHERE id=$1", w.ID)
				dbtest.MustExec(t, ctx, f.pool, "UPDATE runs SET current_run_lease_id=NULL WHERE id=$1", w.RunID)
			}
			fail := func(q *Queries, id pgtype.UUID, version int64) (RunWait, error) {
				switch state {
				case "hot":
					return q.FailHotRunWait(ctx, FailHotRunWaitParams{ID: id, RunID: w.RunID, ExpectedRunStateVersion: version, CurrentRunLeaseID: w.CurrentRunLeaseID, AttemptNumber: 1, ReasonCode: pgvalue.Text("timeout")})
				case "checkpointing":
					return q.FailCheckpointingRunWait(ctx, FailCheckpointingRunWaitParams{ID: id, RunID: w.RunID, ExpectedRunStateVersion: version, CurrentRunLeaseID: w.CurrentRunLeaseID, ReasonCode: pgvalue.Text("timeout")})
				default:
					return q.FailParkedRunWait(ctx, FailParkedRunWaitParams{ID: id, RunID: w.RunID, ExpectedRunStateVersion: version, PriorRunLeaseID: w.CurrentRunLeaseID, AttemptNumber: 1, SuspendCheckpointID: checkpointID, ReasonCode: pgvalue.Text("timeout")})
				}
			}
			for _, test := range []struct {
				id      pgtype.UUID
				version int64
			}{
				{pgvalue.UUID(uuid.NewV7()), w.ExpectedRunStateVersion}, {w.ID, w.ExpectedRunStateVersion + 1},
			} {
				before := ownershipRunWaitSnapshot(t, f, w)
				tx, err := f.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fail(New(tx), test.id, test.version); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("rejection=%v", err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, ownershipRunWaitSnapshot(t, f, w)) {
					t.Fatal("failed rejection durably moved Run/Wait")
				}
			}
			if _, err := fail(f.queries, w.ID, w.ExpectedRunStateVersion); err != nil {
				t.Fatal(err)
			}
			before := ownershipRunWaitSnapshot(t, f, w)
			if _, err := fail(f.queries, w.ID, w.ExpectedRunStateVersion); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("repeat=%v", err)
			}
			if !bytes.Equal(before, ownershipRunWaitSnapshot(t, f, w)) {
				t.Fatal("repeated failure mutated rows")
			}
		})
	}
}
