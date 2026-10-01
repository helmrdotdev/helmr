package dispatch_test

import (
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/dispatch/dispatchtest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestComputerRestoreCommitsWholeSetAndOneIntent(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "empty"}[idle], func(t *testing.T) {
			f, authority, fence := dispatchtest.Restore(t, idle)
			for pass := 0; pass < 3; pass++ {
				if pass == 2 {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='open' WHERE id=$1`, fence.ID)
				}
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				cp, err := authority.CommitRestore(t.Context(), tx, fence)
				if err != nil {
					tx.Rollback(t.Context())
					t.Fatal(err)
				}
				if cp.ResumeComputerInstanceID != pgvalue.UUID(fence.ID) || !cp.ResumeCommittedAt.Valid {
					t.Fatal("restore destination not committed")
				}
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			var leases, intents, resuming int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM run_leases WHERE computer_instance_id=$1`, fence.ID).Scan(&leases); err != nil {
				t.Fatal(err)
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM control_outbox WHERE topic=$1 AND payload->>'computer_instance_id'=$2`, computer.RestoreActivationTopic, fence.ID.String()).Scan(&intents); err != nil {
				t.Fatal(err)
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM run_waits w JOIN run_leases l ON l.id=w.current_run_lease_id JOIN runs r ON r.id=w.run_id WHERE l.computer_instance_id=$1 AND w.suspension_status='resuming' AND w.expected_run_revision=r.revision AND r.current_run_lease_id=l.id`, fence.ID).Scan(&resuming); err != nil {
				t.Fatal(err)
			}
			want := 2
			if idle {
				want = 0
			}
			if leases != want || resuming != want || intents != 1 {
				t.Fatalf("leases=%d waits=%d intents=%d", leases, resuming, intents)
			}
		})
	}
}

func TestComputerRestoreRollsBackPartialActivation(t *testing.T) {
	for _, failure := range []string{"later grant rejected", "intent insert failed", "checkpoint expired"} {
		t.Run(failure, func(t *testing.T) {
			f, authority, fence := dispatchtest.Restore(t, false)
			if failure == "intent insert failed" {
				dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_restore_intent() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'intent rejected'; END $$`)
				dbtest.MustExec(t, t.Context(), f.Pool, `CREATE TRIGGER reject_restore_intent BEFORE INSERT ON control_outbox FOR EACH ROW EXECUTE FUNCTION reject_restore_intent()`)
			} else if failure == "checkpoint expired" {
				dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION expire_restore_checkpoint() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE computer_checkpoints SET expires_at=clock_timestamp()-interval '1 second' WHERE id=(NEW.payload->>'checkpoint_id')::uuid; RETURN NEW; END $$`)
				dbtest.MustExec(t, t.Context(), f.Pool, `CREATE TRIGGER expire_restore_checkpoint BEFORE INSERT ON control_outbox FOR EACH ROW EXECUTE FUNCTION expire_restore_checkpoint()`)
			} else {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET queue_concurrency_limit=1`)
			}
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = authority.CommitRestore(t.Context(), tx, fence); err == nil {
				tx.Rollback(t.Context())
				t.Fatal("partial activation accepted")
			}
			if err = tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			var untouched bool
			err = f.Pool.QueryRow(t.Context(), `SELECT c.resume_committed_at IS NULL AND c.resume_computer_instance_id IS NULL AND NOT EXISTS(SELECT 1 FROM run_leases WHERE computer_instance_id=i.id) AND NOT EXISTS(SELECT 1 FROM control_outbox WHERE topic=$2) FROM computer_instances i JOIN computer_checkpoints c ON c.id=i.source_checkpoint_id WHERE i.id=$1`, fence.ID, computer.RestoreActivationTopic).Scan(&untouched)
			if err != nil || !untouched {
				t.Fatalf("partial commit=%v %v", untouched, err)
			}
		})
	}
}

func TestComputerRestoreRechecksGrantsAfterBlockedIntent(t *testing.T) {
	f, authority, fence := dispatchtest.Restore(t, false)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION block_restore_intent() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 UPDATE run_leases SET start_deadline_at=clock_timestamp()+interval '100 milliseconds',expires_at=clock_timestamp()+interval '200 milliseconds' WHERE computer_instance_id=(NEW.payload->>'computer_instance_id')::uuid;
 PERFORM pg_advisory_xact_lock(91827364); RETURN NEW; END $$`)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE TRIGGER block_restore_intent BEFORE INSERT ON control_outbox FOR EACH ROW EXECUTE FUNCTION block_restore_intent()`)
	blocker, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), blocker, `SELECT pg_advisory_xact_lock(91827364)`)
	result := make(chan error, 1)
	go func() {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			result <- err
			return
		}
		defer tx.Rollback(t.Context())
		_, err = authority.CommitRestore(t.Context(), tx, fence)
		if err == nil {
			err = tx.Commit(t.Context())
		}
		result <- err
	}()
	blocked := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err = f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%INSERT INTO control_outbox%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("intent did not reach blocking write")
	}
	time.Sleep(250 * time.Millisecond)
	if err = blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err == nil {
		t.Fatal("activation committed with expired member grants")
	}
	var untouched bool
	err = f.Pool.QueryRow(t.Context(), `SELECT c.resume_committed_at IS NULL AND NOT EXISTS(SELECT 1 FROM run_leases WHERE computer_instance_id=i.id) AND NOT EXISTS(SELECT 1 FROM control_outbox WHERE topic=$2) FROM computer_instances i JOIN computer_checkpoints c ON c.id=i.source_checkpoint_id WHERE i.id=$1`, fence.ID, computer.RestoreActivationTopic).Scan(&untouched)
	if err != nil || !untouched {
		t.Fatalf("expired activation persisted=%v %v", untouched, err)
	}
}

func TestComputerRestoreAcknowledgesEntireSet(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "empty"}[idle], func(t *testing.T) {
			f, authority, fence := dispatchtest.Restore(t, idle)
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			cp, err := authority.CommitRestore(t.Context(), tx, fence)
			if err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			discovered, err := db.New(f.Pool).DiscoverWorkerRunLeaseWork(t.Context(), db.DiscoverWorkerRunLeaseWorkParams{WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerEpoch: 1, RowLimit: 10})
			if err != nil || len(discovered) != 0 {
				t.Fatalf("frozen grants appeared in fresh discovery: %v %v", discovered, err)
			}
			grants := installedRestoreGrants(t, f, fence)
			for pass := 0; pass < 2; pass++ {
				tx, err = f.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				i, err := dispatch.AcknowledgeRestore(t.Context(), tx, fence, cp.ID, cp.WriterGeneration+1, grants)
				if err != nil {
					tx.Rollback(t.Context())
					t.Fatal(err)
				}
				if i.AdmissionState != "open" {
					t.Fatal("admission did not open")
				}
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			var delivered int
			if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM control_outbox WHERE topic=$1 AND status='delivered' AND delivered_at IS NOT NULL`, computer.RestoreActivationTopic).Scan(&delivered); err != nil || delivered != 1 {
				t.Fatalf("activation intent delivery=%d err=%v", delivered, err)
			}
			var activated int
			err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM run_leases l JOIN runs r ON r.current_run_lease_id=l.id JOIN run_waits w ON w.current_run_lease_id=l.id WHERE l.computer_instance_id=$1 AND l.status='running' AND r.status='waiting' AND r.active_started_at IS NOT NULL AND w.suspension_status='resuming' AND w.expected_run_revision=r.revision`, fence.ID).Scan(&activated)
			if err != nil || activated != len(grants) {
				t.Fatalf("activated=%d err=%v", activated, err)
			}
			for _, g := range grants {
				var wait, checkpoint pgtype.UUID
				var execution run.ExecutionFence
				err = f.Pool.QueryRow(t.Context(), `SELECT w.id,w.suspend_checkpoint_id,l.id,l.lease_sequence,l.worker_group_id,l.worker_host_id,l.worker_epoch,h.claim_version,wg.claim_version FROM run_leases l JOIN run_waits w ON w.current_run_lease_id=l.id JOIN worker_hosts h ON h.id=l.worker_host_id JOIN worker_groups wg ON wg.id=l.worker_group_id WHERE l.id=$1`, g.LeaseID).Scan(&wait, &checkpoint, &execution.LeaseID, &execution.LeaseSequence, &execution.WorkerGroupID, &execution.WorkerHostID, &execution.WorkerEpoch, &execution.HostClaimVersion, &execution.GroupClaimVersion)
				if err != nil {
					t.Fatal(err)
				}
				resumed, err := run.AcknowledgeWaitResume(t.Context(), f.Pool, execution, wait, checkpoint)
				if err != nil {
					t.Fatal(err)
				}
				if resumed.SuspensionStatus != db.RunWaitStatusHot {
					t.Fatalf("pending member status=%s", resumed.SuspensionStatus)
				}
			}

			if _, err = db.New(f.Pool).DrainWorkerHost(t.Context(), db.DrainWorkerHostParams{ID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: 1}); err != nil {
				t.Fatal(err)
			}
			tx, err = f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			replayed, err := dispatch.AcknowledgeRestore(t.Context(), tx, fence, cp.ID, cp.WriterGeneration+1, grants)
			if err != nil {
				tx.Rollback(t.Context())
				t.Fatal(err)
			}
			if replayed.AdmissionState != "draining" {
				t.Fatal("receipt changed draining admission")
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			tx, err = f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = authority.CommitRestore(t.Context(), tx, fence); err != nil {
				tx.Rollback(t.Context())
				t.Fatal(err)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}

		})
	}
}

func installedRestoreGrants(t *testing.T, f runtest.Fixture, fence computer.InstanceRef) []dispatch.RestoreGrant {
	t.Helper()
	rows, err := f.Pool.Query(t.Context(), `SELECT run_id,id,lease_sequence FROM run_leases WHERE computer_instance_id=$1 ORDER BY run_id`, fence.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var grants []dispatch.RestoreGrant
	for rows.Next() {
		var g dispatch.RestoreGrant
		if err = rows.Scan(&g.RunID, &g.LeaseID, &g.LeaseSequence); err != nil {
			t.Fatal(err)
		}
		grants = append(grants, g)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return grants
}

func TestComputerRestoreAcknowledgementRejectsPartialAuthority(t *testing.T) {
	for _, failure := range []string{"missing member", "duplicate member", "wrong lease", "wrong sequence", "wrong checkpoint", "wrong generation", "expired peer", "cancelled peer", "preparation expired", "delivery recording failed", "draining Worker", "draining Group"} {
		t.Run(failure, func(t *testing.T) {
			f, authority, fence := dispatchtest.Restore(t, false)
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			cp, err := authority.CommitRestore(t.Context(), tx, fence)
			if err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			grants := installedRestoreGrants(t, f, fence)
			checkpoint, generation := cp.ID, cp.WriterGeneration+1
			switch failure {
			case "missing member":
				grants = grants[:1]
			case "duplicate member":
				grants[1] = grants[0]
			case "wrong lease":
				grants[1].LeaseID = pgvalue.UUID(uuid.NewV7())
			case "wrong sequence":
				grants[1].LeaseSequence++
			case "wrong checkpoint":
				checkpoint = pgvalue.UUID(uuid.NewV7())
			case "wrong generation":
				generation++
			case "expired peer":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET created_at=clock_timestamp()-interval '3 seconds',start_deadline_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, grants[1].LeaseID)
			case "cancelled peer":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='cancel_requested' WHERE id=$1`, grants[1].RunID)
			case "delivery recording failed":
				dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_restore_delivery() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'delivery rejected'; END $$`)
				dbtest.MustExec(t, t.Context(), f.Pool, `CREATE TRIGGER reject_restore_delivery BEFORE UPDATE ON control_outbox FOR EACH ROW EXECUTE FUNCTION reject_restore_delivery()`)
			case "draining Worker":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp() WHERE id=$1`, f.WorkerID)
			case "draining Group":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_groups SET status='draining',primary_pool_id=NULL,claim_version=claim_version+1 WHERE id=$1`, runtest.WorkerGroupID)
			case "preparation expired":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET preparation_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, fence.ID)
			}
			tx, err = f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			_, err = dispatch.AcknowledgeRestore(t.Context(), tx, fence, checkpoint, generation, grants)
			tx.Rollback(t.Context())
			if err == nil {
				t.Fatal("invalid activation accepted")
			}
			var unchanged bool
			err = f.Pool.QueryRow(t.Context(), `SELECT admission_state='restoring' AND NOT EXISTS(SELECT 1 FROM run_leases WHERE computer_instance_id=i.id AND status='running') AND NOT EXISTS(SELECT 1 FROM runs r JOIN run_leases l ON l.id=r.current_run_lease_id WHERE l.computer_instance_id=i.id AND r.active_started_at IS NOT NULL) FROM computer_instances i WHERE i.id=$1`, fence.ID).Scan(&unchanged)
			if err != nil || !unchanged {
				t.Fatalf("partial activation=%v err=%v", !unchanged, err)
			}
		})
	}
}

func TestComputerRestoreAcknowledgementActorTurn(t *testing.T) {
	for _, state := range []string{"rebind", "settling", "cancel requested", "cancel before grant"} {
		t.Run(state, func(t *testing.T) {
			var turn uuid.UUID
			f, authority, fence := dispatchtest.Restore(t, false, func(f runtest.Fixture, work runtest.RunLease) {
				session := f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
				turn = uuid.NewV7()
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_turns(id,environment_id,session_id,sequence,data) VALUES($1,$2,$3,2,'{}')`, turn, f.EnvironmentID, session)
				_, err := db.New(f.Pool).ActivateSessionTurn(t.Context(), db.ActivateSessionTurnParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), SessionID: pgvalue.UUID(session), TurnID: pgvalue.UUID(turn), RunID: pgvalue.UUID(work.RunID), AttemptNumber: pgtype.Int4{Int32: 1, Valid: true}, InputSequence: 2})
				if err != nil {
					t.Fatal(err)
				}
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_turns SET ready_run_lease_id=$2 WHERE id=$1`, turn, work.LeaseID)
				bind := db.BindRunWaitTurnParams{SessionID: pgvalue.UUID(session), TurnID: pgvalue.UUID(turn)}
				if err = f.Pool.QueryRow(t.Context(), `SELECT w.id,s.run_generation FROM run_waits w JOIN sessions s ON s.current_run_id=w.run_id WHERE w.run_id=$1`, work.RunID).Scan(&bind.WaitID, &bind.RunGeneration); err != nil {
					t.Fatal(err)
				}
				if _, err = db.New(f.Pool).BindRunWaitTurn(t.Context(), bind); err != nil {
					t.Fatal(err)
				}
			})
			if state == "cancel before grant" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET cancel_requested_at=clock_timestamp() WHERE id=(SELECT session_id FROM session_turns WHERE id=$1)`, turn)
			}
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			cp, err := authority.CommitRestore(t.Context(), tx, fence)
			if state == "cancel before grant" {
				if err == nil {
					t.Fatal("cancelled Session received activation permission")
				}
				tx.Rollback(t.Context())
				var unchanged bool
				err = f.Pool.QueryRow(t.Context(), `SELECT c.resume_committed_at IS NULL AND NOT EXISTS(SELECT 1 FROM run_leases WHERE computer_instance_id=i.id) FROM computer_instances i JOIN computer_checkpoints c ON c.id=i.source_checkpoint_id WHERE i.id=$1`, fence.ID).Scan(&unchanged)
				if err != nil || !unchanged {
					t.Fatalf("cancelled Session consumed checkpoint: %v %v", unchanged, err)
				}
				return
			}

			if err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			grants := installedRestoreGrants(t, f, fence)
			if state == "settling" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_turns SET settlement_started_at=clock_timestamp(),ready_run_lease_id=NULL WHERE id=$1`, turn)
			}
			if state == "cancel requested" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET cancel_requested_at=clock_timestamp() WHERE id=(SELECT session_id FROM session_turns WHERE id=$1)`, turn)
			}
			tx, err = f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			_, err = dispatch.AcknowledgeRestore(t.Context(), tx, fence, cp.ID, cp.WriterGeneration+1, grants)
			if state != "rebind" {
				if err == nil {
					t.Fatal("stopped Actor activated")
				}
				tx.Rollback(t.Context())
				var unchanged bool
				if err = f.Pool.QueryRow(t.Context(), `SELECT admission_state='restoring' AND NOT EXISTS(SELECT 1 FROM run_leases WHERE computer_instance_id=i.id AND status='running') FROM computer_instances i WHERE i.id=$1`, fence.ID).Scan(&unchanged); err != nil || !unchanged {
					t.Fatalf("stopped Actor changed activation: %v %v", unchanged, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				var ready bool
				if err = f.Pool.QueryRow(t.Context(), `SELECT t.ready_run_lease_id=r.current_run_lease_id FROM session_turns t JOIN runs r ON r.id=t.run_id WHERE t.id=$1`, turn).Scan(&ready); err != nil || !ready {
					t.Fatalf("Turn not rebound: %v %v", ready, err)
				}
			}
		})
	}
}

func TestComputerRestoreAcknowledgementRechecksActiveBudget(t *testing.T) {
	f, authority, fence := dispatchtest.Restore(t, false)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	cp, err := authority.CommitRestore(t.Context(), tx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	grants := installedRestoreGrants(t, f, fence)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION block_restore_delivery() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 UPDATE runs SET active_elapsed_ms=max_active_duration_ms-50 WHERE current_run_lease_id IN(SELECT id FROM run_leases WHERE computer_instance_id=(NEW.payload->>'computer_instance_id')::uuid);
 PERFORM pg_advisory_xact_lock(91827365); RETURN NEW; END $$`)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE TRIGGER block_restore_delivery BEFORE UPDATE ON control_outbox FOR EACH ROW EXECUTE FUNCTION block_restore_delivery()`)
	blocker, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), blocker, `SELECT pg_advisory_xact_lock(91827365)`)
	result := make(chan error, 1)
	go func() {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			result <- err
			return
		}
		defer tx.Rollback(t.Context())
		_, err = dispatch.AcknowledgeRestore(t.Context(), tx, fence, cp.ID, cp.WriterGeneration+1, grants)
		if err == nil {
			err = tx.Commit(t.Context())
		}
		result <- err
	}()
	blocked := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if err = f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%UPDATE control_outbox SET status=%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("activation never reached outbox write")
	}
	time.Sleep(100 * time.Millisecond)
	if err = blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err == nil {
		t.Fatal("activation exceeded active budget while waiting for delivery write")
	}
	var unchanged bool
	err = f.Pool.QueryRow(t.Context(), `SELECT i.admission_state='restoring' AND NOT EXISTS(SELECT 1 FROM run_leases WHERE computer_instance_id=i.id AND status='running') AND NOT EXISTS(SELECT 1 FROM control_outbox WHERE topic=$2 AND status='delivered') FROM computer_instances i WHERE i.id=$1`, fence.ID, computer.RestoreActivationTopic).Scan(&unchanged)
	if err != nil || !unchanged {
		t.Fatalf("partial activation: %v %v", unchanged, err)
	}
}

func TestComputerRestoreReconciliation(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "empty"}[idle], func(t *testing.T) {
			f, authority, fence := dispatchtest.Restore(t, idle)
			for pass := 0; pass < 2; pass++ {
				n, err := authority.ReconcileComputerInstances(t.Context(), 10)
				want := 1
				if pass == 1 {
					want = 0
				}
				if err != nil || n != want {
					t.Fatalf("pass %d: count=%d want=%d err=%v", pass, n, want, err)
				}
			}
			var committed bool
			err := f.Pool.QueryRow(t.Context(), `SELECT cp.resume_computer_instance_id=i.id AND cp.resume_committed_at IS NOT NULL AND i.admission_state='restoring' AND (SELECT count(*) FROM control_outbox WHERE topic=$2 AND status='pending')=1 FROM computer_instances i JOIN computer_checkpoints cp ON cp.id=i.source_checkpoint_id WHERE i.id=$1`, fence.ID, computer.RestoreActivationTopic).Scan(&committed)
			if err != nil || !committed {
				t.Fatalf("committed=%v err=%v", committed, err)
			}
		})
	}
}

func TestComputerRestoreReconciliationRetriesQueueCapacity(t *testing.T) {
	f, authority, fence := dispatchtest.Restore(t, false)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET queue_concurrency_limit=1`)
	for range 2 {
		n, err := authority.ReconcileComputerInstances(t.Context(), 10)
		if err != nil || n != 0 {
			t.Fatalf("capacity retry: count=%d err=%v", n, err)
		}
	}
	var untouched bool
	err := f.Pool.QueryRow(t.Context(), `SELECT cp.resume_committed_at IS NULL AND NOT EXISTS(SELECT 1 FROM run_leases WHERE computer_instance_id=i.id) AND NOT EXISTS(SELECT 1 FROM control_outbox WHERE topic=$2) FROM computer_instances i JOIN computer_checkpoints cp ON cp.id=i.source_checkpoint_id WHERE i.id=$1`, fence.ID, computer.RestoreActivationTopic).Scan(&untouched)
	if err != nil || !untouched {
		t.Fatalf("partial grants=%v err=%v", !untouched, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET queue_concurrency_limit=2`)
	if n, err := authority.ReconcileComputerInstances(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("retry after capacity release: count=%d err=%v", n, err)
	}
}

func TestComputerRestoreReconciliationPreparationDeadline(t *testing.T) {
	for _, activated := range []bool{false, true} {
		name := "frozen"
		if activated {
			name = "activated"
		}
		t.Run(name, func(t *testing.T) {
			f, authority, fence := dispatchtest.Restore(t, true)
			if activated {
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(t.Context())
				cp, err := authority.CommitRestore(t.Context(), tx, fence)
				if err != nil {
					t.Fatal(err)
				}
				var generation int64
				if err = tx.QueryRow(t.Context(), `SELECT writer_generation FROM computer_instances WHERE id=$1`, fence.ID).Scan(&generation); err != nil {
					t.Fatal(err)
				}
				if _, err = dispatch.AcknowledgeRestore(t.Context(), tx, fence, cp.ID, generation, nil); err != nil {
					t.Fatal(err)
				}
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET preparation_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, fence.ID)
			if _, err := authority.ReconcileComputerInstances(t.Context(), 10); err != nil {
				t.Fatal(err)
			}
			var desired, reason string
			var committed, reclaimed bool
			err := f.Pool.QueryRow(t.Context(), `SELECT i.desired_state,i.desired_reason,cp.resume_committed_at IS NOT NULL,i.reclaimed_at IS NOT NULL FROM computer_instances i JOIN computer_checkpoints cp ON cp.id=i.source_checkpoint_id WHERE i.id=$1`, fence.ID).Scan(&desired, &reason, &committed, &reclaimed)
			want := "closed"
			if activated {
				want = "ready"
			}
			if err != nil || desired != want || committed != activated || reclaimed {
				t.Fatalf("desired=%s reason=%s committed=%v reclaimed=%v err=%v", desired, reason, committed, reclaimed, err)
			}
			if !activated && reason != "computer_preparation_expired" {
				t.Fatalf("expiry reason=%s", reason)
			}
		})
	}
}

func TestComputerRestoreReconciliationRollsBackFailedIntent(t *testing.T) {
	f, authority, fence := dispatchtest.Restore(t, false)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_restore_intent() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'intent rejected'; END $$`)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE TRIGGER reject_restore_intent BEFORE INSERT ON control_outbox FOR EACH ROW EXECUTE FUNCTION reject_restore_intent()`)
	if n, err := authority.ReconcileComputerInstances(t.Context(), 10); err == nil || n != 0 {
		t.Fatalf("failed intent: count=%d err=%v", n, err)
	}
	var untouched bool
	err := f.Pool.QueryRow(t.Context(), `SELECT cp.resume_committed_at IS NULL AND NOT EXISTS(SELECT 1 FROM run_leases WHERE computer_instance_id=i.id) FROM computer_instances i JOIN computer_checkpoints cp ON cp.id=i.source_checkpoint_id WHERE i.id=$1`, fence.ID).Scan(&untouched)
	if err != nil || !untouched {
		t.Fatalf("untouched=%v err=%v", untouched, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DROP TRIGGER reject_restore_intent ON control_outbox`)
	if n, err := authority.ReconcileComputerInstances(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("intent retry: count=%d err=%v", n, err)
	}
}

func TestComputerRestoreReconciliationExpiresCommittedIntent(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		name := "pending"
		if claimed {
			name = "claimed"
		}
		t.Run(name, func(t *testing.T) {
			f, authority, fence := dispatchtest.Restore(t, false)
			if n, err := authority.ReconcileComputerInstances(t.Context(), 10); err != nil || n != 1 {
				t.Fatalf("commit: count=%d err=%v", n, err)
			}
			if claimed {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE control_outbox SET status='claimed',claimed_by='restore-test',claim_expires_at=clock_timestamp()+interval '1 minute',attempts=1 WHERE topic=$1`, computer.RestoreActivationTopic)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET preparation_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, fence.ID)
			if _, err := authority.ReconcileComputerInstances(t.Context(), 10); err != nil {
				t.Fatal(err)
			}
			var closed bool
			err := f.Pool.QueryRow(t.Context(), `SELECT i.desired_state='closed' AND cp.resume_committed_at IS NOT NULL AND cp.resume_computer_instance_id=i.id AND o.status='dead_lettered' AND o.claimed_by IS NULL AND o.claim_expires_at IS NULL AND o.last_error='Computer restore destination expired' AND o.delivered_at IS NULL FROM computer_instances i JOIN computer_checkpoints cp ON cp.id=i.source_checkpoint_id JOIN control_outbox o ON o.topic=$2 AND o.payload->>'computer_instance_id'=i.id::text WHERE i.id=$1`, fence.ID, computer.RestoreActivationTopic).Scan(&closed)
			if err != nil || !closed {
				t.Fatalf("terminal intent=%v err=%v", closed, err)
			}
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err = authority.CommitRestore(t.Context(), tx, fence); err == nil {
				t.Fatal("expired destination reactivated")
			}
		})
	}
}

func TestComputerRestoreReconciliationExpiryIntentFailureRollsBack(t *testing.T) {
	f, authority, fence := dispatchtest.Restore(t, true)
	if _, err := authority.ReconcileComputerInstances(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_restore_expiry() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'expiry intent rejected'; END $$`)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE TRIGGER reject_restore_expiry BEFORE UPDATE ON control_outbox FOR EACH ROW EXECUTE FUNCTION reject_restore_expiry()`)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET preparation_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, fence.ID)
	if _, err := authority.ReconcileComputerInstances(t.Context(), 10); err == nil {
		t.Fatal("intent update failure swallowed")
	}
	var unchanged bool
	err := f.Pool.QueryRow(t.Context(), `SELECT i.desired_state='ready' AND i.admission_state='restoring' AND i.desired_version=$2 AND o.status='pending' FROM computer_instances i JOIN control_outbox o ON o.topic=$3 AND o.payload->>'computer_instance_id'=i.id::text WHERE i.id=$1`, fence.ID, fence.DesiredVersion, computer.RestoreActivationTopic).Scan(&unchanged)
	if err != nil || !unchanged {
		t.Fatalf("expiry atomic=%v err=%v", unchanged, err)
	}
}

func TestComputerRestoreAfterWakeDuringCapture(t *testing.T) {
	f, ref, manifest, objects := computertest.ReadyCapture(t, false)
	var wake db.ResolveCheckpointingTokenWaitParams
	err := f.Pool.QueryRow(t.Context(), `SELECT w.id,w.run_id,w.expected_run_revision,w.current_run_lease_id FROM run_waits w JOIN computer_checkpoint_runs m ON m.run_wait_id=w.id WHERE m.checkpoint_id=$1 ORDER BY w.run_id LIMIT 1`, ref.CheckpointID).Scan(&wake.WaitID, &wake.RunID, &wake.ExpectedRunRevision, &wake.CurrentRunLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	wake.ConditionStatus = "completed"
	wake.ConditionResult = []byte(`{"resume":true}`)
	if _, err := db.New(f.Pool).ResolveCheckpointingTokenWait(t.Context(), wake); err != nil {
		t.Fatal(err)
	}
	f, authority, fence := dispatchtest.RestoreReadyCapture(t, f, ref, manifest, objects)
	var parked bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.status='waiting' AND w.suspension_status='resume_pending' FROM runs r JOIN run_waits w ON w.run_id=r.id WHERE w.id=$1`, wake.WaitID).Scan(&parked); err != nil || !parked {
		t.Fatalf("waiting wakeup=%v err=%v", parked, err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	cp, err := authority.CommitRestore(t.Context(), tx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	grants := installedRestoreGrants(t, f, fence)
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := dispatch.AcknowledgeRestore(t.Context(), tx, fence, cp.ID, cp.WriterGeneration+1, grants); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var preserved bool
	err = f.Pool.QueryRow(t.Context(), `SELECT r.status='waiting' AND w.suspension_status='resuming' AND w.expected_run_revision=r.revision AND w.condition_status='completed' AND w.condition_result='{"resume":true}'::jsonb AND l.status='running' FROM run_waits w JOIN runs r ON r.id=w.run_id JOIN run_leases l ON l.id=w.current_run_lease_id WHERE w.id=$1`, wake.WaitID).Scan(&preserved)
	if err != nil || !preserved {
		t.Fatalf("restored resolved condition=%v err=%v", preserved, err)
	}
}
