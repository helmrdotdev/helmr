package controlplane

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/dispatch/dispatchtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func restorePlanFixture(t *testing.T, idle, committed bool, setup ...func(runtest.Fixture, runtest.RunLease)) (runtest.Fixture, workerActor, workerapi.ComputerRestorePlanRequest, computer.FencingKey) {
	t.Helper()
	f, authority, fence := dispatchtest.Restore(t, idle, setup...)
	key, err := computer.NewFencingKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{ID: fence.RuntimeID, EnvironmentID: pgvalue.UUID(f.EnvironmentID)})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := key.Derive(computer.FenceInput{InstanceID: uuid.MustParse(pgvalue.UUIDString(i.ID)), ComputerID: uuid.MustParse(pgvalue.UUIDString(i.ComputerID)), WriterGeneration: i.WriterGeneration})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_token_hash=decode(replace($2,'sha256:',''),'hex') WHERE id=$1`, i.ID, capability.Hash)
	w := workerActor{WorkerHostID: f.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1}
	if err := f.Pool.QueryRow(t.Context(), `SELECT w.claim_version,g.claim_version FROM worker_hosts w JOIN worker_groups g ON g.id=w.worker_group_id WHERE w.id=$1`, f.WorkerID).Scan(&w.ClaimVersion, &w.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	if committed {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := authority.CommitComputerRestore(t.Context(), tx, fence); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	return f, w, workerapi.ComputerRestorePlanRequest{EnvironmentID: f.EnvironmentID.String(), ComputerInstanceID: pgvalue.UUIDString(i.ID), WriterGeneration: i.WriterGeneration}, key
}

func readRestorePlan(t *testing.T, f runtest.Fixture, w workerActor, r workerapi.ComputerRestorePlanRequest, k computer.FencingKey) (*workerapi.ComputerRestorePlan, error) {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	plan, err := loadComputerRestorePlan(t.Context(), tx, w, r, k)
	if err == nil {
		err = tx.Commit(t.Context())
	}
	return plan, err
}

func TestComputerRestorePlanCommittedWholeSet(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "empty"}[idle], func(t *testing.T) {
			f, w, r, k := restorePlanFixture(t, idle, true)
			for range 2 {
				plan, err := readRestorePlan(t, f, w, r, k)
				if err != nil {
					t.Fatal(err)
				}
				want := 2
				if idle {
					want = 0
				}
				if plan == nil || len(plan.Members) != want || plan.ComputerInstanceID != r.ComputerInstanceID || plan.WriterGeneration != r.WriterGeneration || plan.WriteCapability == "" {
					t.Fatalf("incomplete restore plan (members expected %d)", want)
				}
				for _, member := range plan.Members {
					if member.AttemptNumber != 1 || member.Lease.ID == "" || member.Lease.LeaseSequence != 2 || member.ExpiresAt.IsZero() {
						t.Fatal("incomplete member authority")
					}
				}
			}
		})
	}
}
func TestComputerRestorePlanWaitsForCommit(t *testing.T) {
	f, w, r, k := restorePlanFixture(t, false, false)
	plan, err := readRestorePlan(t, f, w, r, k)
	if err != nil || plan != nil {
		t.Fatalf("uncommitted plan present=%v err=%v", plan != nil, err)
	}
}
func TestComputerRestorePlanRejectsIncompleteOrStaleAuthority(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"expired member", `UPDATE run_leases SET created_at=clock_timestamp()-interval '3 seconds',start_deadline_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second' WHERE computer_instance_id=$1`},
		{"missed start deadline", `UPDATE run_leases SET start_deadline_at=clock_timestamp()-interval '1 second' WHERE computer_instance_id=$1`},
		{"advanced writer", `UPDATE computers SET writer_generation=writer_generation+1 WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`},
		{"changed wait", `UPDATE run_waits SET suspension_status='hot',prior_run_lease_id=NULL WHERE current_run_lease_id IN (SELECT id FROM run_leases WHERE computer_instance_id=$1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, w, r, k := restorePlanFixture(t, false, true)
			dbtest.MustExec(t, t.Context(), f.Pool, tc.sql, r.ComputerInstanceID)
			_, err := readRestorePlan(t, f, w, r, k)
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("stale plan err=%v", err)
			}
		})
	}
}

func TestComputerRestorePlanRechecksDeadlineAfterMemberLock(t *testing.T) {
	f, w, r, k := restorePlanFixture(t, false, true)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	// Leave the row unchanged while blocking the reader. PostgreSQL can evaluate
	// its time predicate before waiting for this lock.
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE run_leases SET start_deadline_at=clock_timestamp()+interval '1 second' WHERE computer_instance_id=$1`, r.ComputerInstanceID)
	if _, err := blocker.Exec(ctx, `SELECT id FROM run_leases WHERE computer_instance_id=$1 ORDER BY run_id FOR UPDATE`, r.ComputerInstanceID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var planErr error
	go func() { defer close(done); _, planErr = loadComputerRestorePlan(ctx, tx, w, r, k) }()
	defer func() { cancel(); <-done }()
	for {
		var blocked bool
		if err := f.Pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-done:
			t.Fatalf("plan finished before lease lock: %v", planErr)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	for {
		var expired bool
		if err := f.Pool.QueryRow(ctx, `SELECT bool_and(start_deadline_at<=clock_timestamp()) FROM run_leases WHERE computer_instance_id=$1`, r.ComputerInstanceID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if !errors.Is(planErr, pgx.ErrNoRows) {
			t.Fatalf("expired grant plan: %v", planErr)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func activateRestorePlanFixture(t *testing.T, f runtest.Fixture, w workerActor, plan *workerapi.ComputerRestorePlan) {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	grants := make([]dispatch.ComputerRestoreGrant, 0, len(plan.Members))
	for _, member := range plan.Members {
		grants = append(grants, dispatch.ComputerRestoreGrant{RunID: pgvalue.UUID(uuid.MustParse(member.RunID)), LeaseID: pgvalue.UUID(uuid.MustParse(member.Lease.ID)), LeaseSequence: member.Lease.LeaseSequence})
	}
	_, err = dispatch.AcknowledgeComputerRestore(t.Context(), tx, dispatch.ComputerPreparationFence{RuntimeID: pgvalue.UUID(uuid.MustParse(plan.ComputerInstanceID)), WorkerID: pgvalue.UUID(w.WorkerHostID), WorkerGroupID: pgvalue.UUID(w.WorkerGroupID), WorkerEpoch: w.WorkerEpoch, DesiredVersion: plan.DesiredVersion}, pgvalue.UUID(uuid.MustParse(plan.CheckpointID)), plan.WriterGeneration, grants)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestRestoredClaimDiscoversAndAttachesWithoutStartingAnotherProgram(t *testing.T) {
	f, w, r, k := restorePlanFixture(t, false, true)
	plan, err := readRestorePlan(t, f, w, r, k)
	if err != nil {
		t.Fatal(err)
	}
	activateRestorePlanFixture(t, f, w, plan)
	work, err := db.New(f.Pool).DiscoverWorkerRunLeaseWork(t.Context(), db.DiscoverWorkerRunLeaseWorkParams{WorkerHostID: pgvalue.UUID(w.WorkerHostID), WorkerGroupID: pgvalue.UUID(w.WorkerGroupID), WorkerEpoch: w.WorkerEpoch, RowLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(work) != len(plan.Members) {
		t.Fatalf("restored leases discovered=%d", len(work))
	}
	server := &Server{tx: f.Pool}
	for _, member := range plan.Members {
		for range 2 {
			a, secrets, err := server.claimRunLease(t.Context(), w, pgvalue.UUID(uuid.MustParse(member.Lease.ID)), member.Lease.LeaseSequence)
			if err != nil {
				t.Fatal(err)
			}
			if a.resumeWait == nil || a.run.Status != "waiting" || a.runLease.Status != "running" || len(secrets) != 0 {
				t.Fatal("restored claim changed execution or reinjected secrets")
			}
			response, err := projectRestoredRunLeaseClaim(a, k)
			if err != nil {
				t.Fatal(err)
			}
			if response.ProgramResume == nil || response.ProgramResume.CheckpointID != plan.CheckpointID || len(response.ProgramStart) != 0 || len(response.Secrets) != 0 {
				t.Fatal("restore projection could start another Program")
			}
		}
	}
}
func TestRestoredClaimRejectsUnactivatedAndAlreadyAcknowledgedMembers(t *testing.T) {
	for _, activated := range []bool{false, true} {
		t.Run(map[bool]string{false: "not activated", true: "already attached"}[activated], func(t *testing.T) {
			f, w, r, k := restorePlanFixture(t, false, true)
			plan, err := readRestorePlan(t, f, w, r, k)
			if err != nil {
				t.Fatal(err)
			}
			member := plan.Members[0]
			if activated {
				activateRestorePlanFixture(t, f, w, plan)
				a, _, err := (&Server{tx: f.Pool}).claimRunLease(t.Context(), w, pgvalue.UUID(uuid.MustParse(member.Lease.ID)), member.Lease.LeaseSequence)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(context.Background())
				_, err = run.AcknowledgeWaitResume(t.Context(), tx, run.ExecutionFence{LeaseID: a.runLease.ID, LeaseSequence: member.Lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(w.WorkerGroupID), WorkerHostID: pgvalue.UUID(w.WorkerHostID), WorkerEpoch: w.WorkerEpoch, GroupClaimVersion: w.GroupClaimVersion, HostClaimVersion: w.ClaimVersion}, a.resumeWait.ID, a.runtime.SourceCheckpointID)
				if err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err = (&Server{tx: f.Pool}).claimRunLease(t.Context(), w, pgvalue.UUID(uuid.MustParse(member.Lease.ID)), member.Lease.LeaseSequence)
			if !errors.Is(err, errStaleRunLeaseClaim) {
				t.Fatalf("unsafe restore claim: %v", err)
			}
		})
	}
}

func TestRestoredClaimSurvivesDrain(t *testing.T) {
	f, w, r, k := restorePlanFixture(t, false, true)
	plan, err := readRestorePlan(t, f, w, r, k)
	if err != nil {
		t.Fatal(err)
	}
	activateRestorePlanFixture(t, f, w, plan)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='draining' WHERE id=$1`, r.ComputerInstanceID)
	_, _, err = (&Server{tx: f.Pool}).claimRunLease(t.Context(), w, pgvalue.UUID(uuid.MustParse(plan.Members[0].Lease.ID)), plan.Members[0].Lease.LeaseSequence)
	if err != nil {
		t.Fatal(err)
	}
}
func TestRestoredActorCanAcknowledgeStopBeforeAndAfterClaim(t *testing.T) {
	for _, activeTurn := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "active Turn"}[activeTurn], func(t *testing.T) {
			for _, stopBeforeClaim := range []bool{true, false} {
				t.Run(map[bool]string{true: "before claim", false: "before ack"}[stopBeforeClaim], func(t *testing.T) {
					var session uuid.UUID
					f, w, r, k := restorePlanFixture(t, false, true, func(f runtest.Fixture, work runtest.RunLease) {
						session = restoredActorFixture(t, f, work, activeTurn)
					})
					plan, err := readRestorePlan(t, f, w, r, k)
					if err != nil {
						t.Fatal(err)
					}
					activateRestorePlanFixture(t, f, w, plan)
					var leaseID uuid.UUID
					if err := f.Pool.QueryRow(t.Context(), `SELECT r.current_run_lease_id FROM runs r JOIN sessions s ON s.current_run_id=r.id WHERE s.id=$1`, session).Scan(&leaseID); err != nil {
						t.Fatal(err)
					}
					stop := func() {
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET dispatch_hold_id=$2,dispatch_hold_reason='interrupt_requested',dispatch_hold_run_id=current_run_id,dispatch_hold_attempt_number=1,dispatch_hold_run_generation=run_generation WHERE id=$1`, session, uuid.NewV7())
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_turns SET interrupt_requested_at=now() WHERE id=(SELECT active_turn_id FROM sessions WHERE id=$1)`, session)
					}
					if stopBeforeClaim {
						stop()
					}
					a, _, err := (&Server{tx: f.Pool}).claimRunLease(t.Context(), w, pgvalue.UUID(leaseID), 2)
					if err != nil {
						t.Fatal(err)
					}
					if !stopBeforeClaim {
						stop()
					}
					tx, err := f.Pool.Begin(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(context.Background())
					wait, err := run.AcknowledgeWaitResume(t.Context(), tx, run.ExecutionFence{LeaseID: a.runLease.ID, LeaseSequence: 2, WorkerGroupID: pgvalue.UUID(w.WorkerGroupID), WorkerHostID: pgvalue.UUID(w.WorkerHostID), WorkerEpoch: w.WorkerEpoch, GroupClaimVersion: w.GroupClaimVersion, HostClaimVersion: w.ClaimVersion}, a.resumeWait.ID, a.runtime.SourceCheckpointID)
					if err != nil {
						t.Fatal(err)
					}
					if wait.SuspensionStatus != "released" || wait.ConditionReasonCode.String != "session_stopped" {
						t.Fatalf("stopped wait was not released: state=%s reason=%s", wait.SuspensionStatus, wait.ConditionReasonCode.String)
					}
					if err := tx.Commit(t.Context()); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func restoredActorFixture(t *testing.T, f runtest.Fixture, work runtest.RunLease, active bool) uuid.UUID {
	t.Helper()
	session := f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
	if active {
		turn := uuid.NewV7()
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_turns(id,environment_id,session_id,sequence,data,status,run_generation,run_id,attempt_number,ready_run_lease_id) SELECT $2,environment_id,id,committed_input_sequence+1,'{}','running',run_generation,current_run_id,1,$3 FROM sessions WHERE id=$1`, session, turn, work.LeaseID)
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET active_turn_id=$2 WHERE id=$1`, session, turn)
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET turn_session_id=$2,turn_id=$3,turn_run_generation=(SELECT run_generation FROM sessions WHERE id=$2) WHERE run_id=$1`, work.RunID, session, turn)
	}
	return session
}

func TestRestoredActorRejectsStaleStopScope(t *testing.T) {
	for _, mutation := range []struct{ name, sql string }{
		{"hold generation", `UPDATE sessions SET dispatch_hold_run_generation=run_generation+1 WHERE id=$1`},
		{"hold attempt", `UPDATE sessions SET dispatch_hold_attempt_number=2 WHERE id=$1`},
		{"active Turn", `UPDATE sessions SET active_turn_id=NULL WHERE id=$1`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			var session uuid.UUID
			f, w, r, k := restorePlanFixture(t, false, true, func(f runtest.Fixture, work runtest.RunLease) { session = restoredActorFixture(t, f, work, true) })
			plan, err := readRestorePlan(t, f, w, r, k)
			if err != nil {
				t.Fatal(err)
			}
			activateRestorePlanFixture(t, f, w, plan)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET dispatch_hold_id=$2,dispatch_hold_reason='interrupt_requested',dispatch_hold_run_id=current_run_id,dispatch_hold_attempt_number=1,dispatch_hold_run_generation=run_generation WHERE id=$1`, session, uuid.NewV7())
			if mutation.name == "hold attempt" {
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO run_attempts(run_id,number,entrypoint_kind,computer_id,base_computer_disk_version_id,session_input_start_sequence) SELECT r.id,2,r.entrypoint_kind,r.computer_id,r.base_computer_disk_version_id,r.session_input_start_sequence FROM runs r JOIN sessions s ON s.current_run_id=r.id WHERE s.id=$1`, session)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, mutation.sql, session)
			var leaseID uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT r.current_run_lease_id FROM runs r JOIN sessions s ON s.current_run_id=r.id WHERE s.id=$1`, session).Scan(&leaseID); err != nil {
				t.Fatal(err)
			}
			_, _, err = (&Server{tx: f.Pool}).claimRunLease(t.Context(), w, pgvalue.UUID(leaseID), 2)
			if !errors.Is(err, errStaleRunLeaseClaim) {
				t.Fatalf("stale stop claim=%v", err)
			}
		})
	}
}

func TestComputerRestorePlanAfterParkedWakeup(t *testing.T) {
	for _, wakeCount := range []int{1, 2} {
		t.Run(fmt.Sprintf("woken-%d", wakeCount), func(t *testing.T) {
			f, worker, request, key := restorePlanFixture(t, false, false)
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			rows, err := tx.Query(t.Context(), `SELECT w.run_id,w.expected_run_revision,w.attempt_number,w.id,w.prior_run_lease_id,w.suspend_checkpoint_id
 FROM run_waits w JOIN computer_checkpoint_runs m ON m.run_wait_id=w.id
 JOIN computer_instances i ON i.source_checkpoint_id=m.checkpoint_id WHERE i.id=$1 ORDER BY w.run_id LIMIT $2`, request.ComputerInstanceID, wakeCount)
			if err != nil {
				t.Fatal(err)
			}
			var wakeups []db.ResolveParkedTokenWaitParams
			for rows.Next() {
				var wake db.ResolveParkedTokenWaitParams
				if err := rows.Scan(&wake.RunID, &wake.ExpectedRunRevision, &wake.AttemptNumber, &wake.WaitID, &wake.PriorRunLeaseID, &wake.SuspendCheckpointID); err != nil {
					t.Fatal(err)
				}
				wake.ConditionStatus = "completed"
				wake.ConditionResult = []byte(`{"resume":true}`)
				wake.ConditionError = nil
				wakeups = append(wakeups, wake)
			}
			err = rows.Err()
			rows.Close()
			if err != nil || len(wakeups) != wakeCount {
				t.Fatalf("wakeups=%d err=%v", len(wakeups), err)
			}
			for _, wake := range wakeups {
				if _, err := db.New(tx).ResolveParkedTokenWait(t.Context(), wake); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			var queued int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM runs r JOIN run_waits w ON w.run_id=r.id WHERE r.status='queued' AND w.suspension_status='resume_pending' AND w.expected_run_revision=r.revision`).Scan(&queued); err != nil || queued != wakeCount {
				t.Fatalf("queued=%d err=%v", queued, err)
			}
			authority, err := dispatch.NewRunAuthority(f.Pool, key)
			if err != nil {
				t.Fatal(err)
			}
			fence := dispatch.ComputerPreparationFence{RuntimeID: pgvalue.UUID(uuid.MustParse(request.ComputerInstanceID)), WorkerID: pgvalue.UUID(worker.WorkerHostID), WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerEpoch: worker.WorkerEpoch, DesiredVersion: 1}
			tx, err = f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			cp, err := authority.CommitComputerRestore(t.Context(), tx, fence)
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			plan, err := readRestorePlan(t, f, worker, request, key)
			if err != nil || plan == nil || len(plan.Members) != 2 {
				t.Fatalf("whole restored set missing: plan=%+v err=%v", plan, err)
			}
			var grants []dispatch.ComputerRestoreGrant
			for _, member := range plan.Members {
				grants = append(grants, dispatch.ComputerRestoreGrant{RunID: pgvalue.UUID(uuid.MustParse(member.RunID)), LeaseID: pgvalue.UUID(uuid.MustParse(member.Lease.ID)), LeaseSequence: member.Lease.LeaseSequence})
			}
			tx, err = f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := dispatch.AcknowledgeComputerRestore(t.Context(), tx, fence, cp.ID, request.WriterGeneration, grants); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			var resumed, completed, pending int
			err = f.Pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE w.condition_status='completed' AND w.condition_result='{"resume":true}'::jsonb),count(*) FILTER (WHERE w.condition_status='pending')
 FROM run_waits w JOIN runs r ON r.id=w.run_id JOIN run_leases l ON l.id=w.current_run_lease_id
 WHERE l.computer_instance_id=$1 AND l.status='running' AND r.status='waiting' AND w.suspension_status='resuming' AND w.expected_run_revision=r.revision`, request.ComputerInstanceID).Scan(&resumed, &completed, &pending)
			if err != nil || resumed != 2 || completed != wakeCount || pending != 2-wakeCount {
				t.Fatalf("resumed=%d completed=%d pending=%d err=%v", resumed, completed, pending, err)
			}
		})
	}
}
