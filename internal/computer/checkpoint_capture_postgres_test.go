package computer_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestComputerCaptureCompleteSet(t *testing.T) {
	f, _, _, request := computertest.Capture(t)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	cp, err := computer.BeginCapture(t.Context(), tx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	members, err := db.New(f.Pool).ListComputerCheckpointRuns(t.Context(), db.ListComputerCheckpointRunsParams{EnvironmentID: pgvalue.UUID(request.EnvironmentID), CheckpointID: cp.ID})
	if err != nil || len(members) != 2 {
		t.Fatalf("members=%v err=%v", members, err)
	}
	var count int
	err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoint_runs m JOIN run_leases l ON l.id=m.source_run_lease_id JOIN run_waits w ON w.id=m.run_wait_id JOIN computer_instances i ON i.id=m.source_computer_instance_id
 WHERE m.checkpoint_id=$1 AND l.status='checkpointing' AND w.suspension_status='checkpointing' AND w.suspend_checkpoint_id=m.checkpoint_id AND i.admission_state='checkpointing' AND i.desired_version=$2 AND i.capture_checkpoint_id=m.checkpoint_id`, cp.ID, request.DesiredVersion+1).Scan(&count)
	if err != nil || count != 2 {
		t.Fatalf("sealed members=%d err=%v", count, err)
	}
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = computer.BeginCapture(t.Context(), tx, request); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("repeated capture: %v", err)
	}
}

func TestComputerCaptureRejectsUnsafeSet(t *testing.T) {
	for _, test := range []struct {
		name, sql string
	}{
		{name: "working peer", sql: `UPDATE runs SET status='running' WHERE id=$1`},
		{name: "expired grant", sql: `UPDATE run_leases SET start_deadline_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1`},
		{name: "ready condition", sql: `UPDATE run_waits SET due_at=clock_timestamp()-interval '1 second' WHERE run_id=$1`},
		{name: "unentered peer", sql: `UPDATE run_attempts SET entrypoint_entered_at=NULL WHERE run_id=$1`},
		{name: "stale revision", sql: `UPDATE runs SET revision=revision+1 WHERE id=$1`},
		{name: "expired writer", sql: `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE run_id=$1)`},
		{name: "stale worker", sql: `UPDATE worker_hosts SET observed_at=clock_timestamp()-interval '1 hour' WHERE id=(SELECT worker_host_id FROM run_leases WHERE run_id=$1)`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, _, peer, request := computertest.Capture(t)
			dbtest.MustExec(t, t.Context(), f.Pool, test.sql, peer.RunID)
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err = computer.BeginCapture(t.Context(), tx, request); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("unsafe capture: %v", err)
			}
			if err = tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			var untouched bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT admission_state='open' AND capture_checkpoint_id IS NULL AND desired_version=$2 AND NOT EXISTS(SELECT 1 FROM computer_checkpoints WHERE id=$3) FROM computer_instances WHERE id=$1`, request.InstanceID, request.DesiredVersion, request.CheckpointID).Scan(&untouched); err != nil || !untouched {
				t.Fatalf("partial capture: %v %v", untouched, err)
			}
		})
	}
}

func TestComputerCaptureIdleAndActorCursor(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		f, _, _, request := computertest.Capture(t)
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled',process_reconciled_at=now() WHERE computer_instance_id=$1`, request.InstanceID)
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		cp, err := computer.BeginCapture(t.Context(), tx, request)
		if err != nil {
			t.Fatal(err)
		}
		members, err := db.New(tx).ListComputerCheckpointRuns(t.Context(), db.ListComputerCheckpointRunsParams{EnvironmentID: pgvalue.UUID(request.EnvironmentID), CheckpointID: cp.ID})
		if err != nil || len(members) != 0 {
			t.Fatalf("idle members=%v err=%v", members, err)
		}
		if err = tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("actor between turns", func(t *testing.T) {
		f, work, _, request := computertest.Capture(t)
		f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		cp, err := computer.BeginCapture(t.Context(), tx, request)
		if err != nil {
			t.Fatal(err)
		}
		members, err := db.New(tx).ListComputerCheckpointRuns(t.Context(), db.ListComputerCheckpointRunsParams{EnvironmentID: pgvalue.UUID(request.EnvironmentID), CheckpointID: cp.ID})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range members {
			if m.RunID == pgvalue.UUID(work.RunID) {
				if !m.ActorSpeculativeInputSequence.Valid || m.ActorSpeculativeInputSequence.Int64 != 1 {
					t.Fatalf("cursor=%v", m.ActorSpeculativeInputSequence)
				}
			} else if m.ActorSpeculativeInputSequence.Valid {
				t.Fatal("task has Actor cursor")
			}
		}
		if err = tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestComputerCaptureActiveTurn(t *testing.T) {
	f, work, _, request := computertest.Capture(t)
	sessionID := f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
	turnID := uuid.NewV7()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO session_turns(id,environment_id,session_id,sequence,data,status,run_generation,run_id,attempt_number,ready_run_lease_id)
 SELECT $2,environment_id,id,committed_input_sequence+1,'{}','running',run_generation,current_run_id,1,$3 FROM sessions WHERE id=$1`, sessionID, turnID, work.LeaseID)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE sessions SET active_turn_id=$2 WHERE id=$1`, sessionID, turnID)
	bind := db.BindRunWaitTurnParams{SessionID: pgvalue.UUID(sessionID), TurnID: pgvalue.UUID(turnID)}
	if err = tx.QueryRow(t.Context(), `SELECT w.id,s.run_generation FROM run_waits w JOIN sessions s ON s.current_run_id=w.run_id WHERE w.run_id=$1`, work.RunID).Scan(&bind.WaitID, &bind.RunGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err = db.New(tx).BindRunWaitTurn(t.Context(), bind); err != nil {
		t.Fatal(err)
	}
	var unreadied bool
	if err = tx.QueryRow(t.Context(), `SELECT ready_run_lease_id IS NULL FROM session_turns WHERE id=$1`, turnID).Scan(&unreadied); err != nil || !unreadied {
		t.Fatalf("managed wait retained message readiness: unreadied=%v err=%v", unreadied, err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	cp, err := computer.BeginCapture(t.Context(), tx, request)
	if err != nil {
		t.Fatal(err)
	}
	var cursor int64
	if err = tx.QueryRow(t.Context(), `SELECT actor_speculative_input_sequence FROM computer_checkpoint_runs WHERE checkpoint_id=$1 AND run_id=$2`, cp.ID, work.RunID).Scan(&cursor); err != nil || cursor != 2 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_turns SET interrupt_requested_at=now() WHERE id=$1`, turnID)
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = computer.BeginCapture(t.Context(), tx, request); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("interrupted Turn captured: %v", err)
	}
}

func TestComputerCaptureCommandProcessMustBeReconciled(t *testing.T) {
	f, work, _, request := computertest.Capture(t)
	id, claim := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'computer.command.start',$3,$3,now())`, claim, f.EnvironmentID, dbtest.Hash(claim.String()))
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id)
 SELECT $2,environment_id,computer_id,$3,ARRAY['true'],'{}',''::bytea,60000,'api_key',run_id::text FROM run_leases WHERE id=$1`, work.LeaseID, id, claim)
	for _, stage := range []string{"pending", "running", "cancelled", "reconciled"} {
		switch stage {
		case "running":
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='running',computer_instance_id=i.id,writer_generation=i.writer_generation FROM computer_instances i WHERE computer_commands.id=$1 AND i.id=$2`, id, request.InstanceID)
		case "cancelled":
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled' WHERE id=$1`, id)
		case "reconciled":
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET process_reconciled_at=now() WHERE id=$1`, id)
		}
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_, err = computer.BeginCapture(t.Context(), tx, request)
		tx.Rollback(t.Context())
		if stage == "reconciled" {
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s command captured: %v", stage, err)
		}
	}
}

func TestComputerCaptureRechecksDeadlineAfterWaitLock(t *testing.T) {
	f, work, _, request := computertest.Capture(t)
	hold, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), hold, `SELECT id FROM run_waits WHERE run_id=$1 FOR UPDATE`, work.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, request.InstanceID)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pidCh := make(chan int32, 1)
	done := make(chan error, 1)
	go func() {
		tx, e := f.Pool.Begin(ctx)
		if e != nil {
			done <- e
			return
		}
		defer tx.Rollback(context.Background())
		var pid int32
		if e = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); e != nil {
			done <- e
			return
		}
		pidCh <- pid
		_, e = computer.BeginCapture(ctx, tx, request)
		done <- e
	}()
	var pid int32
	select {
	case pid = <-pidCh:
	case e := <-done:
		t.Fatal(e)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for {
		var blocked, expired bool
		if err = f.Pool.QueryRow(ctx, `SELECT coalesce((SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1),false),writer_expires_at<clock_timestamp() FROM computer_instances WHERE id=$2`, pid, request.InstanceID).Scan(&blocked, &expired); err != nil {
			t.Fatal(err)
		}
		if blocked && expired {
			break
		}
		select {
		case e := <-done:
			t.Fatalf("capture did not wait: %v", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err = hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-done:
		if !errors.Is(e, pgx.ErrNoRows) {
			t.Fatalf("expired writer captured: %v", e)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestComputerCaptureDiscoveryFences(t *testing.T) {
	f, _, _, request := computertest.Capture(t)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	cp, err := computer.BeginCapture(t.Context(), tx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	params := db.GetComputerInstanceCaptureCheckpointParams{ComputerInstanceID: pgvalue.UUID(request.InstanceID), EnvironmentID: pgvalue.UUID(request.EnvironmentID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, DesiredVersion: request.DesiredVersion + 1, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds}
	if got, err := db.New(f.Pool).GetComputerInstanceCaptureCheckpoint(t.Context(), params); err != nil || got.ID != cp.ID {
		t.Fatalf("capture discovery: %v %v", got, err)
	}
	for _, field := range []string{"environment", "group", "worker", "epoch", "version", "instance"} {
		stale := params
		switch field {
		case "environment":
			stale.EnvironmentID = pgvalue.UUID(uuid.NewV7())
		case "group":
			stale.WorkerGroupID = pgvalue.UUID(uuid.NewV7())
		case "worker":
			stale.WorkerHostID = pgvalue.UUID(uuid.NewV7())
		case "epoch":
			stale.WorkerEpoch++
		case "version":
			stale.DesiredVersion++
		case "instance":
			stale.ComputerInstanceID = pgvalue.UUID(uuid.NewV7())
		}
		if _, err := db.New(f.Pool).GetComputerInstanceCaptureCheckpoint(t.Context(), stale); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("stale %s discovered: %v", field, err)
		}
	}
	for _, test := range []struct{ name, sql string }{
		{"writer expired", `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`},
		{"writer superseded", `UPDATE computers SET writer_generation=writer_generation+1 WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`},
		{"membership changed", `UPDATE computer_instances SET membership_revision=membership_revision+1 WHERE id=$1`},
		{"worker stale", `UPDATE worker_hosts SET observed_at=clock_timestamp()-interval '1 hour' WHERE id=(SELECT worker_host_id FROM computer_instances WHERE id=$1)`},
		{"checkpoint expired", `UPDATE computer_checkpoints SET expires_at=clock_timestamp()-interval '1 second' WHERE source_computer_instance_id=$1`},
		{"stop requested", `UPDATE computers SET desired_state='stopped' WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`},
	} {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, t.Context(), tx, test.sql, request.InstanceID)
		_, err = db.New(tx).GetComputerInstanceCaptureCheckpoint(t.Context(), params)
		tx.Rollback(t.Context())
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s discovered: %v", test.name, err)
		}
	}
}

func TestIdleComputerCaptureRequiresEveryMemberDue(t *testing.T) {
	for _, tc := range []struct {
		name, change string
		want         bool
	}{
		{"all due", "", true},
		{"peer still warm", `UPDATE run_waits SET idle_timeout_ms=3600000 WHERE run_id=$1`, false},
		{"peer disables suspension", `UPDATE run_waits SET idle_timeout_ms=NULL WHERE run_id=$1`, false},
		{"peer condition ready", `UPDATE run_waits SET due_at=clock_timestamp()-interval '1 second' WHERE run_id=$1`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, peer, request := computertest.Capture(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET idle_timeout_ms=1,created_at=clock_timestamp()-interval '1 minute'`)
			if tc.change != "" {
				dbtest.MustExec(t, t.Context(), f.Pool, tc.change, peer.RunID)
			}
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			_, err = computer.BeginIdleCapture(t.Context(), tx, request)
			if tc.want {
				if err != nil {
					t.Fatal(err)
				}
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("capture = %v", err)
				}
				if err = tx.Rollback(t.Context()); err != nil {
					t.Fatal(err)
				}
				var untouched bool
				err = f.Pool.QueryRow(t.Context(), `SELECT admission_state='open' AND capture_checkpoint_id IS NULL AND desired_version=$2 AND NOT EXISTS(SELECT 1 FROM computer_checkpoints WHERE id=$3) FROM computer_instances WHERE id=$1`, request.InstanceID, request.DesiredVersion, request.CheckpointID).Scan(&untouched)
				if err != nil || !untouched {
					t.Fatalf("rejected capture changed authority: %v %v", untouched, err)
				}
			}
		})
	}
}

func TestIdleComputerCaptureEmptyCooldown(t *testing.T) {
	for _, old := range []bool{false, true} {
		t.Run(map[bool]string{false: "recent", true: "idle"}[old], func(t *testing.T) {
			f, _, _, request := computertest.Capture(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled',process_reconciled_at=now() WHERE computer_instance_id=$1`, request.InstanceID)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET last_activity_at=clock_timestamp()-CASE WHEN $2 THEN interval '1 minute' ELSE interval '0 seconds' END WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, request.InstanceID, old)
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			_, err = computer.BeginIdleCapture(t.Context(), tx, request)
			if old && err != nil || !old && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("old=%v capture=%v", old, err)
			}
		})
	}
}

const pauseWorkerGroup = `UPDATE worker_groups SET status='paused',claim_version=claim_version+1 WHERE id=$1`

// A paused Worker Group stops admission only; resident Computers still capture.
func TestComputerCaptureContinuesOnPausedGroup(t *testing.T) {
	f, _, _, request := computertest.Capture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, pauseWorkerGroup, runtest.WorkerGroupID)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	cp, err := computer.BeginCapture(t.Context(), tx, request)
	if err != nil {
		t.Fatalf("capture on paused Group: %v", err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	params := db.GetComputerInstanceCaptureCheckpointParams{ComputerInstanceID: pgvalue.UUID(request.InstanceID), EnvironmentID: pgvalue.UUID(request.EnvironmentID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, DesiredVersion: request.DesiredVersion + 1, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds}
	if got, err := db.New(f.Pool).GetComputerInstanceCaptureCheckpoint(t.Context(), params); err != nil || got.ID != cp.ID {
		t.Fatalf("capture discovery on paused Group: %v %v", got.ID, err)
	}
}

func TestComputerCaptureWorkerFreshRejectsUnobservedWorker(t *testing.T) {
	f, _, _, _ := computertest.Capture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET observed_at=NULL WHERE id=$1`, f.WorkerID)
	fresh, err := db.New(f.Pool).GetComputerCaptureWorkerFresh(t.Context(), db.GetComputerCaptureWorkerFreshParams{ID: pgvalue.UUID(f.WorkerID), WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds})
	if err != nil || fresh {
		t.Fatalf("unobserved Worker fresh=%v err=%v", fresh, err)
	}
}
