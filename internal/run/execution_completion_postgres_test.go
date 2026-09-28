package run

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
)

func completeExecutionTest(t *testing.T, f runtest.Fixture, r TaskCompletion, commit bool) error {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	err = CompleteTaskExecution(t.Context(), tx, r)
	if err == nil && commit {
		err = tx.Commit(t.Context())
	}
	return err
}
func completionRequest(r ExecutionFinalization) TaskCompletion {
	return TaskCompletion{Fence: r.Fence, OperationID: r.OperationID, Fingerprint: dbtest.Digest("complete-task"), Kind: "succeeded", Output: json.RawMessage(`{"ok":true}`)}
}
func TestTaskExecutionCompletionPhysicalIndependenceAndReplay(t *testing.T) {
	f, begin := finalizationExecutionFixture(t, false)
	if _, err := finalizeExecutionTest(t, f, begin, true); err != nil {
		t.Fatal(err)
	}
	r := completionRequest(begin)
	var before string
	if err := f.Pool.QueryRow(t.Context(), `SELECT row_to_json(i)::text FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id WHERE l.id=$1`, r.Fence.LeaseID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := completeExecutionTest(t, f, r, false); err != nil {
		t.Fatal(err)
	}
	var open bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT l.status='finalizing' AND r.status='running' FROM run_leases l JOIN runs r ON r.id=l.run_id WHERE l.id=$1`, r.Fence.LeaseID).Scan(&open); err != nil || !open {
		t.Fatalf("rollback=%v %v", open, err)
	}
	for range 2 {
		if err := completeExecutionTest(t, f, r, true); err != nil {
			t.Fatal(err)
		}
	}
	var after string
	var settled bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT row_to_json(i)::text,l.status='completed' AND l.process_reconciled_at IS NULL AND r.status='succeeded' AND r.output='{"ok":true}'::jsonb AND a.terminal_outcome='succeeded' FROM run_leases l JOIN computer_instances i ON i.id=l.computer_instance_id JOIN runs r ON r.id=l.run_id JOIN run_attempts a ON a.run_id=r.id AND a.number=l.attempt_number WHERE l.id=$1`, r.Fence.LeaseID).Scan(&after, &settled); err != nil || !settled || before != after {
		t.Fatalf("settled=%v physical=%v %v", settled, before == after, err)
	}
	r.Fingerprint = dbtest.Digest("changed-completion")
	if err := completeExecutionTest(t, f, r, true); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("conflicting replay=%v", err)
	}
}
func TestTaskExecutionCompletionResolvesSharedParentWithoutCapture(t *testing.T) {
	f, parent, child := childFinalizationFixture(t)
	if err := completeExecutionTest(t, f, completionRequest(child), true); err != nil {
		t.Fatal(err)
	}
	var live bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.status='running' AND l.status='running' AND i.desired_state='ready' AND i.admission_state='open' AND i.capture_checkpoint_id IS NULL AND w.suspension_status='released' AND w.condition_status='completed' AND w.condition_result->>'ok'='true' AND l.process_reconciled_at IS NULL FROM runs r JOIN run_leases l ON l.id=r.current_run_lease_id JOIN computer_instances i ON i.id=l.computer_instance_id JOIN run_waits w ON w.run_id=r.id WHERE r.id=$1`, parent.RunID).Scan(&live); err != nil || !live {
		t.Fatalf("resident parent=%v %v", live, err)
	}
}
func TestTaskExecutionCompletionRejectsChangedAuthority(t *testing.T) {
	for _, kind := range []string{"operation", "lease expiry", "writer expiry", "ancestor cancellation", "actor"} {
		t.Run(kind, func(t *testing.T) {
			var f runtest.Fixture
			var begin ExecutionFinalization
			if kind == "ancestor cancellation" {
				var parent ExecutionFinalization
				f, parent, begin = childFinalizationFixture(t)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='cancel_requested' WHERE id=$1`, parent.RunID)
			} else {
				f, begin = finalizationExecutionFixture(t, kind == "actor")
				if _, err := finalizeExecutionTest(t, f, begin, true); err != nil {
					t.Fatal(err)
				}
			}
			r := completionRequest(begin)
			switch kind {
			case "operation":
				r.OperationID = pgvalue.UUID(uuid.NewV7())
			case "lease expiry":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET created_at=clock_timestamp()-interval '2 minutes',start_deadline_at=clock_timestamp()-interval '1 minute',claimed_at=clock_timestamp()-interval '50 seconds',started_at=clock_timestamp()-interval '40 seconds',finalization_started_at=clock_timestamp()-interval '30 seconds',expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, r.Fence.LeaseID)
			case "writer expiry":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, r.Fence.LeaseID)
			}
			if err := completeExecutionTest(t, f, r, true); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("rejection=%v", err)
			}
		})
	}
}
func TestTaskExecutionCompletionFailureAndRetryCleanup(t *testing.T) {
	for _, kind := range []string{"failed", "payload_invalid", "retry"} {
		t.Run(kind, func(t *testing.T) {
			f, begin := finalizationExecutionFixture(t, false)
			var originalBase, newHead uuid.UUID
			diskSnapshot := func() string {
				t.Helper()
				var value string
				if err := f.Pool.QueryRow(t.Context(), `SELECT jsonb_build_object('head',c.head_disk_version_id,'versions',(SELECT jsonb_agg(to_jsonb(v) ORDER BY v.id) FROM computer_disk_versions v WHERE v.computer_id=c.id))::text FROM computers c JOIN runs r ON r.computer_id=c.id WHERE r.id=$1`, begin.RunID).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			if kind == "retry" {
				if err := f.Pool.QueryRow(t.Context(), `SELECT base_computer_disk_version_id FROM run_attempts WHERE run_id=$1 AND number=1`, begin.RunID).Scan(&originalBase); err != nil {
					t.Fatal(err)
				}
				newHead = uuid.NewV7()
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(context.Background())
				dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
				dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_disk_versions(id,environment_id,computer_id,parent_version_id,root_pack_digest,logical_bytes,status,writer_generation,source_computer_instance_id,publisher_computer_instance_id,publisher_desired_version,publisher_save_sequence,publication_request_fingerprint,published_at)
                 SELECT $2,v.environment_id,v.computer_id,v.id,v.root_pack_digest,v.logical_bytes,'committed',i.writer_generation,i.id,i.id,i.desired_version,1,$3,now() FROM run_leases l JOIN computer_instances i ON i.id=l.computer_instance_id JOIN computers c ON c.id=i.computer_id JOIN computer_disk_versions v ON v.id=c.head_disk_version_id WHERE l.id=$1`, begin.Fence.LeaseID, newHead, dbtest.Hash(newHead.String()))
				dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_disk_version_roots(environment_id,computer_id,version_id,locator) SELECT environment_id,computer_id,$2,locator FROM computer_disk_version_roots WHERE version_id=$1`, originalBase, newHead)
				dbtest.MustExec(t, t.Context(), tx, `UPDATE computers SET head_disk_version_id=$2 WHERE id=(SELECT computer_id FROM runs WHERE id=$1)`, begin.RunID, newHead)
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET retry_policy='{"enabled":true,"maxAttempts":2,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}' WHERE id=$1`, begin.RunID)
			}
			if _, err := finalizeExecutionTest(t, f, begin, true); err != nil {
				t.Fatal(err)
			}
			diskBefore := diskSnapshot()
			r := completionRequest(begin)
			r.Kind = kind
			if kind == "retry" {
				r.Kind = "failed"
			}
			r.Output = nil
			r.Error = json.RawMessage(`{"message":"work failed","details":{}}`)
			if err := completeExecutionTest(t, f, r, true); err != nil {
				t.Fatal(err)
			}
			if diskSnapshot() != diskBefore {
				t.Fatal("Task completion changed committed disk state")
			}
			var status string
			if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM runs WHERE id=$1`, begin.RunID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if kind != "retry" {
				if status != "failed" {
					t.Fatalf("status=%s", status)
				}
				return
			}
			if status != "retry_delayed" {
				t.Fatalf("status=%s", status)
			}
			var blocked bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT l.process_reconciled_at IS NULL AND r.current_attempt_number=2 AND a.base_computer_disk_version_id=c.head_disk_version_id AND c.head_disk_version_id=$3::uuid AND (SELECT base_computer_disk_version_id=$4::uuid FROM run_attempts WHERE run_id=r.id AND number=1) AND $3::uuid<>$4::uuid FROM runs r JOIN run_leases l ON l.id=$2 JOIN run_attempts a ON a.run_id=r.id AND a.number=2 JOIN computers c ON c.id=r.computer_id WHERE r.id=$1`, begin.RunID, r.Fence.LeaseID, newHead, originalBase).Scan(&blocked); err != nil || !blocked {
				t.Fatalf("retry cleanup=%v %v", blocked, err)
			}
		})
	}
}

func TestTaskExecutionCompletionRechecksExpiryAfterWrite(t *testing.T) {
	f, begin := finalizationExecutionFixture(t, false)
	if _, err := finalizeExecutionTest(t, f, begin, true); err != nil {
		t.Fatal(err)
	}
	// Model a storage write consuming the remaining grant after initial validation.
	// The trigger does not change any policy or schema constraint.
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION expire_completion_writer() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.status='completed' THEN UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=NEW.computer_instance_id; END IF; RETURN NEW; END $$`)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE TRIGGER expire_completion_writer BEFORE UPDATE ON run_leases FOR EACH ROW EXECUTE FUNCTION expire_completion_writer()`)
	if err := completeExecutionTest(t, f, completionRequest(begin), true); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired write accepted=%v", err)
	}
	var rolledBack bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT l.status='finalizing' AND r.status='running' AND a.terminal_at IS NULL AND i.writer_expires_at>clock_timestamp() FROM run_leases l JOIN runs r ON r.id=l.run_id JOIN run_attempts a ON a.run_id=r.id AND a.number=l.attempt_number JOIN computer_instances i ON i.id=l.computer_instance_id WHERE l.id=$1`, begin.Fence.LeaseID).Scan(&rolledBack); err != nil || !rolledBack {
		t.Fatalf("atomic rollback=%v %v", rolledBack, err)
	}
}
func TestTaskExecutionCompletionReplayAfterWriterExpires(t *testing.T) {
	f, begin := finalizationExecutionFixture(t, false)
	if _, err := finalizeExecutionTest(t, f, begin, true); err != nil {
		t.Fatal(err)
	}
	r := completionRequest(begin)
	if err := completeExecutionTest(t, f, r, true); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, r.Fence.LeaseID)
	if err := completeExecutionTest(t, f, r, true); err != nil {
		t.Fatalf("durable replay=%v", err)
	}
}
func TestTaskExecutionCompletionKeepsFailureDetailsExact(t *testing.T) {
	f, begin := finalizationExecutionFixture(t, false)
	if _, err := finalizeExecutionTest(t, f, begin, true); err != nil {
		t.Fatal(err)
	}
	r := completionRequest(begin)
	r.Kind = "failed"
	r.Output = nil
	r.Error = json.RawMessage(`{"message":"failed","details":{"large":9007199254740993}}`)
	if err := completeExecutionTest(t, f, r, true); err != nil {
		t.Fatal(err)
	}
	var large string
	if err := f.Pool.QueryRow(t.Context(), `SELECT failure->'details'->>'large' FROM runs WHERE id=$1`, begin.RunID).Scan(&large); err != nil || large != "9007199254740993" {
		t.Fatalf("details=%s %v", large, err)
	}
}

func TestTaskExecutionCompletionConcurrentReplay(t *testing.T) {
	f, begin := finalizationExecutionFixture(t, false)
	if _, err := finalizeExecutionTest(t, f, begin, true); err != nil {
		t.Fatal(err)
	}
	r := completionRequest(begin)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() { <-start; results <- completeExecutionTest(t, f, r, true) }()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var events int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM telemetry_outbox WHERE run_id=$1 AND kind='run.completed'`, begin.RunID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("terminal events=%d %v", events, err)
	}
}

func TestTaskExecutionCompletionRacesCancellation(t *testing.T) {
	f, begin := finalizationExecutionFixture(t, false)
	if _, err := finalizeExecutionTest(t, f, begin, true); err != nil {
		t.Fatal(err)
	}
	c, err := NewCanceler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	completionDone := make(chan error, 1)
	cancelDone := make(chan error, 1)
	go func() { <-start; completionDone <- completeExecutionTest(t, f, completionRequest(begin), true) }()
	go func() {
		<-start
		_, err := c.Cancel(t.Context(), CancellationRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RunID: pgvalue.MustUUIDValue(begin.RunID)})
		cancelDone <- err
	}()
	close(start)
	completionErr, cancelErr := <-completionDone, <-cancelDone
	if completionErr != nil && !errors.Is(completionErr, pgx.ErrNoRows) {
		t.Fatalf("complete=%v", completionErr)
	}
	if cancelErr != nil && !errors.Is(cancelErr, ErrCancellationConflict) {
		t.Fatalf("cancel=%v", cancelErr)
	}
	var status string
	var safe bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT r.status,l.status IN ('completed','cancelled') AND a.terminal_outcome IN ('succeeded','cancelled') AND i.desired_state='ready' AND i.reclaimed_at IS NULL AND l.process_reconciled_at IS NULL FROM runs r JOIN run_leases l ON l.id=$2 JOIN run_attempts a ON a.run_id=r.id AND a.number=1 JOIN computer_instances i ON i.id=l.computer_instance_id WHERE r.id=$1`, begin.RunID, begin.Fence.LeaseID).Scan(&status, &safe); err != nil || !safe {
		t.Fatalf("status=%s safe=%v %v", status, safe, err)
	}
	if status == "succeeded" && completionErr != nil || status == "cancelled" && cancelErr != nil {
		t.Fatalf("status=%s complete=%v cancel=%v", status, completionErr, cancelErr)
	}
}

func TestTaskExecutionCompletionRejectsUnavailableAdmission(t *testing.T) {
	for _, kind := range []string{"retry policy", "revoked Secret"} {
		t.Run(kind, func(t *testing.T) {
			f, begin := finalizationExecutionFixture(t, false)
			if _, err := finalizeExecutionTest(t, f, begin, true); err != nil {
				t.Fatal(err)
			}
			if kind == "retry policy" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET retry_policy='{"enabled":true,"maxAttempts":-1}' WHERE id=$1`, begin.RunID)
			} else {
				sid, vid := uuid.NewV7(), uuid.NewV7()
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(context.Background())
				dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
				dbtest.MustExec(t, t.Context(), tx, `INSERT INTO secrets(id,environment_id,name,status,revocation_generation,revoked_at) VALUES($1,$2,'TEST_TOKEN','revoked',1,now())`, sid, f.EnvironmentID)
				dbtest.MustExec(t, t.Context(), tx, `INSERT INTO secret_versions(id,secret_id,version,nonce,ciphertext) VALUES($1,$2,1,decode(repeat('00',12),'hex'),decode(repeat('00',16),'hex'))`, vid, sid)
				dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_secrets(computer_id,environment_id,placement_kind,placement_target,secret_id,mode) SELECT computer_id,environment_id,'env','TEST_TOKEN',$2,'raw' FROM runs WHERE id=$1`, begin.RunID, sid)
				dbtest.MustExec(t, t.Context(), tx, `INSERT INTO secret_resolutions(id,computer_id,run_id,attempt_number,placement_kind,placement_target,secret_id,secret_version_id,revocation_generation) SELECT gen_random_uuid(),computer_id,id,1,'env','TEST_TOKEN',$2,$3,0 FROM runs WHERE id=$1`, begin.RunID, sid, vid)
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			r := completionRequest(begin)
			r.Kind = "failed"
			r.Output = nil
			r.Error = json.RawMessage(`{"message":"failed","details":{}}`)
			if err := completeExecutionTest(t, f, r, true); !errors.Is(err, ErrTaskCompletionAdmission) {
				t.Fatalf("admission rejection=%v", err)
			}
			var unchanged bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT r.status='running' AND l.status='finalizing' AND l.terminal_at IS NULL FROM runs r JOIN run_leases l ON l.id=r.current_run_lease_id WHERE r.id=$1`, begin.RunID).Scan(&unchanged); err != nil || !unchanged {
				t.Fatalf("rejected result changed state=%v %v", unchanged, err)
			}
		})
	}
}

func TestTaskCompletionSettlesOwnedQueuedChildrenOnlyWhenTerminal(t *testing.T) {
	for _, retrying := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminal", true: "retry"}[retrying], func(t *testing.T) {
			f, begin := finalizationExecutionFixture(t, false)
			child, claim := uuid.NewV7(), uuid.NewV7()
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
			dbtest.MustExec(t, t.Context(), tx, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'task.child.invoke',$3,$3,now())`, claim, f.EnvironmentID, dbtest.Hash(claim.String()))
			dbtest.MustExec(t, t.Context(), tx, `INSERT INTO runs(id,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,computer_id,base_computer_disk_version_id,payload,queue_name,queue_origin_at,queue_score_at,max_active_duration_ms,retry_policy,trace_id,root_span_id,parent_run_id,parent_owns_lifecycle,claim_id)
    SELECT $2,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,'child',computer_id,base_computer_disk_version_id,payload,queue_name,clock_timestamp(),clock_timestamp(),max_active_duration_ms,retry_policy,trace_id,root_span_id,id,true,$3 FROM runs WHERE id=$1`, begin.RunID, child, claim)
			dbtest.MustExec(t, t.Context(), tx, `INSERT INTO run_attempts(run_id,number,entrypoint_kind,computer_id,base_computer_disk_version_id) SELECT id,1,entrypoint_kind,computer_id,base_computer_disk_version_id FROM runs WHERE id=$1`, child)
			if retrying {
				dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET retry_policy='{"enabled":true,"maxAttempts":2,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}' WHERE id=$1`, begin.RunID)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			snapshot := func() string {
				t.Helper()
				var value string
				if err := f.Pool.QueryRow(t.Context(), `SELECT jsonb_build_array(to_jsonb(r),to_jsonb(a))::text FROM runs r JOIN run_attempts a ON a.run_id=r.id AND a.number=1 WHERE r.id=$1`, child).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := snapshot()
			if _, err = finalizeExecutionTest(t, f, begin, true); err != nil {
				t.Fatal(err)
			}
			request := completionRequest(begin)
			if retrying {
				request.Kind = "failed"
				request.Output = nil
				request.Error = json.RawMessage(`{"message":"retry","details":{}}`)
			}
			if err = completeExecutionTest(t, f, request, true); err != nil {
				t.Fatal(err)
			}
			if retrying {
				if snapshot() != before {
					t.Fatal("retry mutated owned queued child")
				}
				var attempt int32
				if err = f.Pool.QueryRow(t.Context(), `SELECT current_attempt_number FROM runs WHERE id=$1`, begin.RunID).Scan(&attempt); err != nil || attempt != 2 {
					t.Fatalf("retry attempt=%d %v", attempt, err)
				}
			} else {
				var cancelled bool
				if err = f.Pool.QueryRow(t.Context(), `SELECT r.status='cancelled' AND a.terminal_at IS NOT NULL FROM runs r JOIN run_attempts a ON a.run_id=r.id AND a.number=1 WHERE r.id=$1`, child).Scan(&cancelled); err != nil || !cancelled {
					t.Fatalf("owned child remains active: %v %v", cancelled, err)
				}
			}
		})
	}
}

func TestSharedChildCompletionRetryKeepsParentWaitAndWriter(t *testing.T) {
	f, parent, child := childFinalizationFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET retry_policy='{"enabled":true,"maxAttempts":2,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}' WHERE id=$1`, child.RunID)
	snapshot := func() string {
		t.Helper()
		var value string
		if err := f.Pool.QueryRow(t.Context(), `SELECT jsonb_build_array(to_jsonb(r),to_jsonb(l),to_jsonb(w),to_jsonb(i))::text FROM runs r JOIN run_leases l ON l.id=r.current_run_lease_id JOIN run_waits w ON w.current_run_lease_id=l.id JOIN computer_instances i ON i.id=l.computer_instance_id WHERE r.id=$1`, parent.RunID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot()
	request := completionRequest(child)
	request.Kind = "failed"
	request.Output = nil
	request.Error = json.RawMessage(`{"message":"retry","details":{}}`)
	if err := completeExecutionTest(t, f, request, true); err != nil {
		t.Fatal(err)
	}
	if snapshot() != before {
		t.Fatal("child retry changed parent wait or shared physical writer")
	}
	var scheduled bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='retry_delayed' AND current_attempt_number=2 FROM runs WHERE id=$1`, child.RunID).Scan(&scheduled); err != nil || !scheduled {
		t.Fatalf("child retry not scheduled: %v %v", scheduled, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET retry_at=clock_timestamp()-interval '1 second' WHERE id=$1`, child.RunID)
	q := db.New(f.Pool)
	rows, err := q.ReadyRunRetries(t.Context(), 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("retry bypassed child cleanup: %+v %v", rows, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET process_reconciled_at=clock_timestamp() WHERE id=$1`, child.Fence.LeaseID)
	rows, err = q.ReadyRunRetries(t.Context(), 10)
	if err != nil || len(rows) != 1 || rows[0].ID != child.RunID {
		t.Fatalf("shared retry after cleanup=%+v %v", rows, err)
	}
	if snapshot() != before {
		t.Fatal("cleanup of child changed parent authority")
	}
}

func TestSeparateChildCompletionDoesNotRevivePreviousParentAttempt(t *testing.T) {
	f, parent := finalizationExecutionFixture(t, false)
	work := f.AddRunLease(t, "assigned", time.Now())
	claim, wait := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'task.child.invoke',$3,$3,now())`, claim, f.EnvironmentID, dbtest.Hash(claim.String()))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET parent_run_id=$2,parent_owns_lifecycle=true,cause_kind='child',claim_id=$3 WHERE id=$1`, work.RunID, parent.RunID, claim)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='waiting',retry_policy='{"enabled":true,"maxAttempts":2,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}' WHERE id=$1`, parent.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO run_waits(id,environment_id,run_id,computer_id,kind,child_run_id,child_target_declared_id,child_claim_id,child_request,expected_run_revision,attempt_number,current_run_lease_id) SELECT $2,environment_id,id,computer_id,'child',$3,'test-task',$4,'{}',revision,current_attempt_number,current_run_lease_id FROM runs WHERE id=$1`, parent.RunID, wait, work.RunID, claim)
	fence := parent.Fence
	fence.LeaseID = pgvalue.UUID(work.LeaseID)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	a, err := ClaimExecution(t.Context(), tx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = StartExecution(t.Context(), tx, fence); err != nil {
		t.Fatal(err)
	}
	if err = EnterExecution(t.Context(), tx, fence, a.Run.EntrypointKind, a.Run.EntrypointDeclaredID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	child := ExecutionFinalization{Fence: fence, RunID: pgvalue.UUID(work.RunID), AttemptNumber: 1, OperationID: pgvalue.UUID(uuid.NewV7()), Fingerprint: dbtest.Digest("separate-child")}
	if _, err = finalizeExecutionTest(t, f, child, true); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=clock_timestamp()-interval '2 milliseconds',expires_at=clock_timestamp()-interval '1 millisecond' WHERE id=$1`, parent.Fence.LeaseID)
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	graph, err := LockExecutionLeaseRecovery(t.Context(), tx, OwnedFinalizationRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RunID: pgvalue.MustUUIDValue(parent.RunID)})
	if err != nil {
		t.Fatal(err)
	}
	root := graph.descendants[0]
	ok, err := graph.RecoverExecutionLeaseLoss(t.Context(), ExecutionLeaseRecoveryRequest{RunID: pgvalue.MustUUIDValue(parent.RunID), ComputerID: root.computerID, AttemptNumber: 1, RunLeaseID: pgvalue.MustUUIDValue(parent.Fence.LeaseID)})
	if err != nil || !ok {
		t.Fatalf("parent retry=%v %v", ok, err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var value string
		if err := f.Pool.QueryRow(t.Context(), `SELECT jsonb_build_array(to_jsonb(r),to_jsonb(w))::text FROM runs r JOIN run_waits w ON w.run_id=r.id WHERE r.id=$1 AND w.id=$2`, parent.RunID, wait).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot()
	if err = completeExecutionTest(t, f, completionRequest(child), true); err != nil {
		t.Fatal(err)
	}
	if snapshot() != before {
		t.Fatal("late child completion changed previous parent wait")
	}
	var settled bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT c.status='succeeded' AND p.status='retry_delayed' AND p.current_attempt_number=2 AND w.condition_status='failed' FROM runs c JOIN runs p ON p.id=c.parent_run_id JOIN run_waits w ON w.id=$2 WHERE c.id=$1`, work.RunID, wait).Scan(&settled); err != nil || !settled {
		t.Fatalf("incorrect late completion=%v %v", settled, err)
	}
}
