package run

import (
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"testing"
	"time"
	"uuid"
)

func TestLostTaskRetryPreservesSeparateComputerChild(t *testing.T) {
	f := newPostgresFixture(t)
	ctx := t.Context()
	parent := f.addRun(t, "starting", time.Now().Add(-time.Minute))
	child := f.addRun(t, "starting", time.Now().Add(-time.Minute))
	claim := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'task.child.invoke',decode(repeat('12',32),'hex'),decode(repeat('14',32),'hex'),now())`, claim, f.environmentID)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE runs SET parent_run_id=$2,parent_owns_lifecycle=true,cause_kind='child',claim_id=$3 WHERE id=$1`, child.runID, parent.runID, claim)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE runs SET status='running',active_started_at=now()-interval '1 second',retry_policy='{"enabled":true,"maxAttempts":2,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}' WHERE id=$1`, parent.runID)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE run_leases SET status='running',started_at=claimed_at,start_deadline_at=now()-interval '2 milliseconds',expires_at=now()-interval '1 millisecond' WHERE id=$1`, parent.leaseID)
	var before []byte
	if err := f.pool.QueryRow(ctx, `SELECT jsonb_build_array(to_jsonb(r),to_jsonb(a),to_jsonb(l),to_jsonb(c)) FROM runs r JOIN run_attempts a ON a.run_id=r.id JOIN run_leases l ON l.id=r.current_run_lease_id JOIN computers c ON c.id=r.computer_id WHERE r.id=$1`, child.runID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	graph, err := LockExecutionLeaseRecovery(ctx, tx, OwnedFinalizationRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, RunID: parent.runID})
	if err != nil {
		t.Fatal(err)
	}
	root := graph.descendants[0]
	ok, err := graph.RecoverExecutionLeaseLoss(ctx, ExecutionLeaseRecoveryRequest{RunID: parent.runID, ComputerID: root.computerID, AttemptNumber: 1, RunLeaseID: parent.leaseID})
	if err != nil || !ok {
		t.Fatalf("loss retry: %t %v", ok, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var same bool
	if err = f.pool.QueryRow(ctx, `SELECT jsonb_build_array(to_jsonb(r),to_jsonb(a),to_jsonb(l),to_jsonb(c))=$2::jsonb FROM runs r JOIN run_attempts a ON a.run_id=r.id JOIN run_leases l ON l.id=r.current_run_lease_id JOIN computers c ON c.id=r.computer_id WHERE r.id=$1`, child.runID, before).Scan(&same); err != nil || !same {
		t.Fatalf("separate child changed: %t %v", same, err)
	}
	// A final owner cancellation still owns and terminates the surviving child.
	canceler, err := NewCanceler(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = canceler.Cancel(ctx, CancellationRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, RunID: parent.runID}); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = f.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, child.runID).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("owned child survives final cancel: %s %v", status, err)
	}
}

func TestRetryReadinessSkipsUnreconciledOlderScope(t *testing.T) {
	f := newPostgresFixture(t)
	ctx := t.Context()
	var work []leasedRun
	for range 2 {
		r := f.addRun(t, "starting", time.Now().Add(-time.Minute))
		work = append(work, r)
		dbtest.MustExec(t, ctx, f.pool, `UPDATE runs SET status='running',active_started_at=now()-interval '1 second',retry_policy='{"enabled":true,"maxAttempts":2,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}' WHERE id=$1`, r.runID)
		dbtest.MustExec(t, ctx, f.pool, `UPDATE run_leases SET status='running',started_at=claimed_at,start_deadline_at=now()-interval '2 milliseconds',expires_at=now()-interval '1 millisecond' WHERE id=$1`, r.leaseID)
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		graph, err := LockExecutionLeaseRecovery(ctx, tx, OwnedFinalizationRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, RunID: r.runID})
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		ok, err := graph.RecoverExecutionLeaseLoss(ctx, ExecutionLeaseRecoveryRequest{RunID: r.runID, ComputerID: graph.descendants[0].computerID, AttemptNumber: 1, RunLeaseID: r.leaseID})
		if err != nil || !ok {
			_ = tx.Rollback(ctx)
			t.Fatalf("loss=%t %v", ok, err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	dbtest.MustExec(t, ctx, f.pool, `UPDATE runs SET retry_at=now()-interval '1 hour' WHERE id=$1`, work[0].runID)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE runs SET retry_at=now()-interval '1 minute' WHERE id=$1`, work[1].runID)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE computer_instances SET admission_state='closed',mount_state='unmounted',unmounted_at=now(),observed_state='closed',observed_version=observed_version+1,observed_desired_version=desired_version,terminal_at=now(),terminal_reason_code='execution_lost',reclaimed_at=now(),reclaim_evidence='{"method":"host_reconciled"}' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work[1].leaseID)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE run_leases SET process_reconciled_at=now() WHERE id=$1`, work[1].leaseID)
	rows, err := NewRetryReconciler(f.pool).ReadyRunRetries(ctx, 1)
	if err != nil || len(rows) != 1 || rows[0].ID.Bytes != [16]byte(work[1].runID) {
		t.Fatalf("blocked older root starved ready retry: %+v %v", rows, err)
	}
}

func TestPrestartChildLossPreservesAttemptBudgetAndParent(t *testing.T) {
	f := newPostgresFixture(t)
	ctx := t.Context()
	parent := f.addRun(t, "starting", time.Now().Add(-time.Minute))
	child := f.addRun(t, "starting", time.Now().Add(-time.Minute))
	claim := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'task.child.invoke',$3,$3,now())`, claim, f.environmentID, dbtest.Hash(claim.String()))
	dbtest.MustExec(t, ctx, f.pool, `UPDATE runs SET parent_run_id=$2,parent_owns_lifecycle=true,cause_kind='child',claim_id=$3,active_elapsed_ms=123,retry_policy='{"enabled":false}' WHERE id=$1`, child.runID, parent.runID, claim)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE run_leases SET start_deadline_at=now()-interval '2 milliseconds',expires_at=now()-interval '1 millisecond' WHERE id=$1`, child.leaseID)
	var parentBefore string
	if err := f.pool.QueryRow(ctx, `SELECT to_jsonb(r)::text FROM runs r WHERE id=$1`, parent.runID).Scan(&parentBefore); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	graph, err := LockExecutionLeaseRecovery(ctx, tx, OwnedFinalizationRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, RunID: child.runID})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := graph.RecoverExecutionLeaseLoss(ctx, ExecutionLeaseRecoveryRequest{RunID: child.runID, ComputerID: graph.descendants[0].computerID, AttemptNumber: 1, RunLeaseID: child.leaseID})
	if err != nil || !ok {
		t.Fatalf("prestart child loss=%v %v", ok, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var preserved bool
	if err = f.pool.QueryRow(ctx, `SELECT r.status='queued' AND r.current_attempt_number=1 AND r.current_run_lease_id IS NULL AND r.retry_at IS NULL AND r.active_elapsed_ms=123 AND a.terminal_at IS NULL AND a.entrypoint_entered_at IS NULL AND (SELECT count(*) FROM run_attempts WHERE run_id=r.id)=1 AND (SELECT to_jsonb(p)::text=$2 FROM runs p WHERE id=r.parent_run_id) FROM runs r JOIN run_attempts a ON a.run_id=r.id AND a.number=1 WHERE r.id=$1`, child.runID, parentBefore).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("prestart retry changed budget, attempt or parent: %v %v", preserved, err)
	}
}
