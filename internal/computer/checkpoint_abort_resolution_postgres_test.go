package computer_test

import (
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCaptureAbortDeliversConditionsResolvedWhileHeld(t *testing.T) {
	for _, kind := range []string{"timer", "token", "child", "actor_input"} {
		for _, abortFirst := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/checkpointing", true: "/resuming_capture"}[abortFirst], func(t *testing.T) {
				var work runtest.RunLease
				var completedTurn, child pgtype.UUID
				f, ref, _, key := captureAbortFixture(t, false, func(f runtest.Fixture, target runtest.RunLease) {
					work = target
					switch kind {
					case "token":
						id := uuid.NewV7()
						dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO tokens(id,org_id,project_id,environment_id,expires_at,callback_secret_fingerprint) VALUES($1,$2,$3,$4,now()+interval '1 hour',decode(repeat('01',32),'hex'))`, id, f.OrgID, f.ProjectID, f.EnvironmentID)
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET kind='token',due_at=NULL,token_id=$2,token_registration_run_revision=expected_run_revision WHERE run_id=$1`, target.RunID, id)
					case "child":
						child = pgvalue.UUID(f.AddRunLease(t, "running", time.Now().Add(-time.Minute)).RunID)
						claim := uuid.NewV7()
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET cause_kind='child',parent_run_id=$2,parent_owns_lifecycle=false WHERE id=$1`, child, target.RunID)
						dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'child',decode(repeat('01',32),'hex'),decode(repeat('02',32),'hex'),clock_timestamp())`, claim, f.EnvironmentID)
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET kind='child',due_at=NULL,child_run_id=$2,child_target_declared_id='child',child_claim_id=$3,child_request='{}' WHERE run_id=$1`, target.RunID, child, claim)
					case "actor_input":
						session := f.ConvertToActor(t, t.Context(), target, `{"enabled":false}`)
						completedTurn = pgvalue.UUID(uuid.NewV7())
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET kind='actor_input',due_at=NULL,session_id=$2,after_input_sequence=2 WHERE run_id=$1`, target.RunID, session)
						dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_turns(id,environment_id,session_id,sequence,data) VALUES($1,$2,$3,3,'{}')`, completedTurn, f.EnvironmentID, session)
					}
				})
				if abortFirst {
					if _, err := computer.AbortCapture(t.Context(), f.Pool, key, ref); err != nil {
						t.Fatal(err)
					}
				}
				var w db.RunWait
				if err := f.Pool.QueryRow(t.Context(), `SELECT id,expected_run_revision FROM run_waits WHERE run_id=$1`, work.RunID).Scan(&w.ID, &w.ExpectedRunRevision); err != nil {
					t.Fatal(err)
				}
				q := db.New(f.Pool)
				result := []byte(`{"retained":"condition"}`)
				var err error
				switch kind {
				case "token":
					_, err = q.ResolveCheckpointingTokenWait(t.Context(), db.ResolveCheckpointingTokenWaitParams{WaitID: w.ID, RunID: pgvalue.UUID(work.RunID), ExpectedRunRevision: w.ExpectedRunRevision, CurrentRunLeaseID: pgvalue.UUID(work.LeaseID), ConditionStatus: "completed", ConditionResult: result})
				case "child":
					_, err = q.CompleteCheckpointingChildRunWait(t.Context(), db.CompleteCheckpointingChildRunWaitParams{ID: w.ID, RunID: pgvalue.UUID(work.RunID), ChildRunID: child, ExpectedRunRevision: w.ExpectedRunRevision, CurrentRunLeaseID: pgvalue.UUID(work.LeaseID), ConditionResult: result})
				default:
					_, err = q.CompleteCheckpointingRunWait(t.Context(), db.CompleteCheckpointingRunWaitParams{ID: w.ID, RunID: pgvalue.UUID(work.RunID), ExpectedRunRevision: w.ExpectedRunRevision, CurrentRunLeaseID: pgvalue.UUID(work.LeaseID), ConditionResult: result, CompletedTurnID: completedTurn})
				}
				if err != nil {
					t.Fatal(err)
				}
				plan, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
				if err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if _, err := computer.CompleteCaptureAbort(t.Context(), f.Pool, ref, plan.Instance.DesiredVersion, nil); err != nil {
						t.Fatal(err)
					}
				}
				var delivered bool
				err = f.Pool.QueryRow(t.Context(), `SELECT r.status='running' AND r.current_attempt_number=1 AND r.current_run_lease_id=$2 AND r.revision=$3+1 AND w.expected_run_revision=r.revision AND w.suspension_status='released' AND w.suspension_terminal_at IS NOT NULL AND w.suspend_checkpoint_id IS NULL AND w.condition_status='completed' AND w.condition_result=$4::jsonb AND w.completed_turn_id IS NOT DISTINCT FROM $5::uuid FROM runs r JOIN run_waits w ON w.run_id=r.id WHERE w.id=$1`, w.ID, work.LeaseID, w.ExpectedRunRevision, result, completedTurn).Scan(&delivered)
				if err != nil || !delivered {
					t.Fatalf("resolved condition delivered=%v: %v", delivered, err)
				}
			})
		}
	}
}

func TestCaptureAbortStopsRecoveredAttemptAndResumesHealthyMember(t *testing.T) {
	f, ref, _, key := captureAbortFixture(t, false)
	plan, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
	if err != nil {
		t.Fatal(err)
	}
	lost := plan.Members[0]
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET active_started_at=clock_timestamp()-interval '1 minute',retry_policy='{"enabled":true,"maxAttempts":2,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}' WHERE id=$1`, lost.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=created_at,expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, lost.LeaseID)
	if n, err := run.RecoverExecutionLeases(t.Context(), f.Pool, 10); err != nil || n != 1 {
		t.Fatalf("recover=%d %v", n, err)
	}
	replay, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Members[0].Cancelled || replay.Members[1].Cancelled {
		t.Fatalf("dispositions=%+v", replay.Members)
	}
	if _, err = computer.CompleteCaptureAbort(t.Context(), f.Pool, ref, replay.Instance.DesiredVersion, []uuid.UUID{uuid.MustParse(lost.LeaseID)}); err != nil {
		t.Fatal(err)
	}
	var resumed bool
	err = f.Pool.QueryRow(t.Context(), `SELECT r.status='retry_delayed' AND r.terminal_at IS NULL AND l.terminal_at IS NOT NULL AND a.terminal_at IS NOT NULL AND i.admission_state='open' FROM runs r JOIN run_leases l ON l.id=$2 JOIN run_attempts a ON a.run_id=r.id AND a.number=$3 JOIN computer_instances i ON i.id=$4 WHERE r.id=$1`, lost.RunID, lost.LeaseID, lost.AttemptNumber, ref.InstanceID).Scan(&resumed)
	if err != nil || !resumed {
		t.Fatalf("retry survives abort=%v %v", resumed, err)
	}
}
