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
	if err := f.pool.QueryRow(ctx, `SELECT jsonb_build_array(to_jsonb(r),to_jsonb(a),to_jsonb(l),to_jsonb(c)) FROM runs r JOIN run_attempts a ON a.run_id=r.id JOIN run_leases l ON l.id=r.current_run_lease_id JOIN computers c ON c.id=r.workspace_id WHERE r.id=$1`, child.runID).Scan(&before); err != nil {
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
	ok, err := graph.RecoverExecutionLeaseLoss(ctx, ExecutionLeaseRecoveryRequest{RunID: parent.runID, WorkspaceID: root.workspaceID, AttemptNumber: 1, RunLeaseID: parent.leaseID})
	if err != nil || !ok {
		t.Fatalf("loss retry: %t %v", ok, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var same bool
	if err = f.pool.QueryRow(ctx, `SELECT jsonb_build_array(to_jsonb(r),to_jsonb(a),to_jsonb(l),to_jsonb(c))=$2::jsonb FROM runs r JOIN run_attempts a ON a.run_id=r.id JOIN run_leases l ON l.id=r.current_run_lease_id JOIN computers c ON c.id=r.workspace_id WHERE r.id=$1`, child.runID, before).Scan(&same); err != nil || !same {
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

func TestRetryRecoverySkipsUnreclaimedOlderComputer(t *testing.T) {
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
		ok, err := graph.RecoverExecutionLeaseLoss(ctx, ExecutionLeaseRecoveryRequest{RunID: r.runID, WorkspaceID: graph.descendants[0].workspaceID, AttemptNumber: 1, RunLeaseID: r.leaseID})
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
	dbtest.MustExec(t, ctx, f.pool, `UPDATE runtime_instances SET observed_state='closed',observed_version=observed_version+1,observed_desired_version=desired_version,terminal_at=now(),terminal_reason_code='execution_lost',reclaimed_at=now(),reclaim_evidence='{"method":"host_reconciled"}' WHERE id=(SELECT runtime_instance_id FROM run_leases WHERE id=$1)`, work[1].leaseID)
	rows, err := NewRetryReconciler(f.pool).ReadyRunRetries(ctx, 1)
	if err != nil || len(rows) != 1 || rows[0].ID.Bytes != [16]byte(work[1].runID) {
		t.Fatalf("blocked older root starved ready retry: %+v %v", rows, err)
	}
}
