package run

import (
	"context"
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

func executionClaimFixture(t *testing.T) (runtest.Fixture, runtest.RunLease, ExecutionFence) {
	t.Helper()
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	request := ExecutionFence{LeaseID: pgvalue.UUID(work.LeaseID), LeaseSequence: 1, WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerEpoch: 1}
	if err := f.Pool.QueryRow(t.Context(), `SELECT h.claim_version,g.claim_version FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id WHERE h.id=$1`, f.WorkerID).Scan(&request.HostClaimVersion, &request.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	return f, work, request
}
func claimExecutionTest(t *testing.T, f runtest.Fixture, r ExecutionFence, commit bool) (ExecutionAuthority, error) {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		return ExecutionAuthority{}, err
	}
	defer tx.Rollback(context.Background())
	result, err := ClaimExecution(t.Context(), tx, r)
	if err == nil && commit {
		err = tx.Commit(t.Context())
	}
	return result, err
}
func TestExecutionClaimReplayAndRollback(t *testing.T) {
	for _, actor := range []bool{false, true} {
		t.Run(map[bool]string{false: "Task", true: "Actor"}[actor], func(t *testing.T) {
			f, work, request := executionClaimFixture(t)
			if actor {
				f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
			}
			result, err := claimExecutionTest(t, f, request, false)
			if err != nil {
				t.Fatal(err)
			}
			var status string
			if err = f.Pool.QueryRow(t.Context(), `SELECT status FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&status); err != nil || status != "assigned" {
				t.Fatalf("rollback=%s %v", status, err)
			}
			first, err := claimExecutionTest(t, f, request, true)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := claimExecutionTest(t, f, request, true)
			if err != nil {
				t.Fatal(err)
			}
			if first.Lease.Status != db.RunLeaseStatusStarting || first.Instance.ID != result.Instance.ID || replay.Lease.ClaimedAt != first.Lease.ClaimedAt || replay.Lease.ID != first.Lease.ID {
				t.Fatal("claim replay changed execution identity")
			}
			var unchanged bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT writer_generation=$2 AND desired_state='ready' AND observed_state='ready' AND reclaimed_at IS NULL FROM computer_instances WHERE id=$1`, first.Instance.ID, first.Instance.WriterGeneration).Scan(&unchanged); err != nil || !unchanged {
				t.Fatalf("claim altered physical ownership: %v %v", unchanged, err)
			}
		})
	}
}
func TestExecutionClaimRejectsChangedAuthority(t *testing.T) {
	for _, test := range []struct {
		name, sql string
		claims    bool
	}{
		{"closed Computer", `UPDATE computers SET desired_state='stopped'`, false},
		{"new Program unacknowledged", `UPDATE computer_instances SET desired_version=desired_version+1`, false},
		{"closed admission", `UPDATE computer_instances SET admission_state='draining' WHERE reclaimed_at IS NULL`, false},
		{"expired writer", `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second'`, false},
		{"stale Worker", `UPDATE worker_hosts SET observed_at=clock_timestamp()-interval '1 hour'`, false},
		{"expired grant", `UPDATE run_leases SET created_at=clock_timestamp()-interval '1 hour',start_deadline_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second'`, false},
		{"expired start", `UPDATE run_leases SET start_deadline_at=clock_timestamp()-interval '1 second'`, false},
		{"Worker claims", `UPDATE worker_hosts SET claim_version=claim_version+1`, true},
		{"Group claims", `UPDATE worker_groups SET claim_version=claim_version+1`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, _, r := executionClaimFixture(t)
			dbtest.MustExec(t, t.Context(), f.Pool, test.sql)
			_, err := claimExecutionTest(t, f, r, true)
			expected := error(pgx.ErrNoRows)
			if test.claims {
				expected = ErrExecutionWorkerClaims
			}
			if !errors.Is(err, expected) {
				t.Fatalf("claim error=%v, want %v", err, expected)
			}
		})
	}
}

func TestSharedInstanceClaimsDoNotTransferOwnership(t *testing.T) {
	f, work, request := executionClaimFixture(t)
	peer, peerLease := uuid.NewV7(), uuid.NewV7()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO runs(id,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,computer_id,base_computer_disk_version_id,payload,queue_name,queue_origin_at,queue_score_at,max_active_duration_ms,retry_policy,root_span_id)
 SELECT $2,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,computer_id,base_computer_disk_version_id,payload,queue_name,queue_origin_at,queue_score_at,max_active_duration_ms,retry_policy,root_span_id FROM runs WHERE id=$1`, work.RunID, peer)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO run_attempts(run_id,number,entrypoint_kind,computer_id,base_computer_disk_version_id) SELECT $2,1,entrypoint_kind,computer_id,base_computer_disk_version_id FROM runs WHERE id=$1`, work.RunID, peer)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO run_leases(id,org_id,project_id,environment_id,run_id,computer_id,region_id,lease_sequence,attempt_number,worker_group_id,worker_host_id,worker_epoch,computer_instance_id,writer_generation,deployment_id,requested_cpu_millis,requested_memory_bytes,requested_guest_ephemeral_disk_bytes,requested_execution_slots,status,created_at,start_deadline_at,expires_at)
 SELECT $2,org_id,project_id,environment_id,$3,computer_id,region_id,1,1,worker_group_id,worker_host_id,worker_epoch,computer_instance_id,writer_generation,deployment_id,requested_cpu_millis,requested_memory_bytes,requested_guest_ephemeral_disk_bytes,requested_execution_slots,status,created_at,start_deadline_at,expires_at FROM run_leases WHERE id=$1`, work.LeaseID, peerLease, peer)
	dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET current_run_lease_id=$2,first_lease_at=now() WHERE id=$1`, peer, peerLease)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	first, err := claimExecutionTest(t, f, request, true)
	if err != nil {
		t.Fatal(err)
	}
	secondRequest := request
	secondRequest.LeaseID = pgvalue.UUID(peerLease)
	second, err := claimExecutionTest(t, f, secondRequest, true)
	if err != nil {
		t.Fatal(err)
	}
	if first.Instance.ID != second.Instance.ID || first.Lease.ID == second.Lease.ID || first.Instance.WriterGeneration != second.Instance.WriterGeneration {
		t.Fatal("shared claim transferred the physical writer")
	}
	canceler, err := NewCanceler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = canceler.Cancel(t.Context(), CancellationRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RunID: work.RunID}); err != nil {
		t.Fatal(err)
	}
	if _, err = claimExecutionTest(t, f, request, true); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cancelled claim replay=%v", err)
	}
	if _, err = claimExecutionTest(t, f, secondRequest, true); err != nil {
		t.Fatalf("peer cancellation revoked shared claim: %v", err)
	}
}

func TestExecutionClaimRejectsCancellingOwnedParent(t *testing.T) {
	f, child, request := executionClaimFixture(t)
	parent := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	claimID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'task.child.invoke',$3,$3,now())`, claimID, f.EnvironmentID, dbtest.Hash(claimID.String()))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET cause_kind='child',claim_id=$3,parent_run_id=$2,parent_owns_lifecycle=true WHERE id=$1`, child.RunID, parent.RunID, claimID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='cancel_requested' WHERE id=$1`, parent.RunID)
	if _, err := claimExecutionTest(t, f, request, true); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cancelling parent allowed child claim: %v", err)
	}
}

func TestExecutionClaimCancellationRace(t *testing.T) {
	f, work, request := executionClaimFixture(t)
	canceler, err := NewCanceler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	claimDone := make(chan error, 1)
	cancelDone := make(chan error, 1)
	go func() {
		<-start
		tx, e := f.Pool.Begin(ctx)
		if e != nil {
			claimDone <- e
			return
		}
		defer tx.Rollback(context.Background())
		_, e = ClaimExecution(ctx, tx, request)
		if e == nil {
			e = tx.Commit(ctx)
		}
		claimDone <- e
	}()
	go func() {
		<-start
		_, e := canceler.Cancel(ctx, CancellationRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RunID: work.RunID})
		cancelDone <- e
	}()
	close(start)
	if err = <-claimDone; err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim race: %v", err)
	}
	if err = <-cancelDone; err != nil {
		t.Fatalf("cancel race: %v", err)
	}
	if _, err = claimExecutionTest(t, f, request, true); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cancelled execution claim survived: %v", err)
	}
}

func TestExecutionClaimDeadlineExpiresWhileWaitingForComputer(t *testing.T) {
	f, work, request := executionClaimFixture(t)
	hold, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(context.Background())
	if _, err = hold.Exec(t.Context(), `SELECT id FROM computers WHERE id=(SELECT computer_id FROM run_leases WHERE id=$1) FOR UPDATE`, work.LeaseID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=clock_timestamp()+interval '2 seconds' WHERE id=$1`, work.LeaseID)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pids := make(chan int32, 1)
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
		pids <- pid
		_, e = ClaimExecution(ctx, tx, request)
		done <- e
	}()
	var pid int32
	select {
	case pid = <-pids:
	case e := <-done:
		t.Fatal(e)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for {
		var blocked, expired bool
		if err = f.Pool.QueryRow(ctx, `SELECT coalesce((SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1),false),start_deadline_at<clock_timestamp() FROM run_leases WHERE id=$2`, pid, work.LeaseID).Scan(&blocked, &expired); err != nil {
			t.Fatal(err)
		}
		if blocked && expired {
			break
		}
		select {
		case e := <-done:
			t.Fatalf("claim finished before lock release: %v", e)
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
			t.Fatalf("expired start claim=%v", e)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
