package run_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

// leaseFence is the fixture worker host's current fence on a lease.
func leaseFence(t *testing.T, f runtest.Fixture, work runtest.RunLease) run.ExecutionFence {
	t.Helper()
	fence := run.ExecutionFence{LeaseID: pgvalue.UUID(work.LeaseID), LeaseSequence: 1, WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerEpoch: 1}
	if err := f.Pool.QueryRow(t.Context(), `SELECT h.claim_version,g.claim_version FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id WHERE h.id=$1`, f.WorkerID).Scan(&fence.HostClaimVersion, &fence.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	return fence
}

// taskExecutionFixture claims, starts and enters a Task lease through the
// worker lease operations, and returns a success completion for it.
func taskExecutionFixture(t *testing.T) (runtest.Fixture, runtest.RunLease, run.ExecutionFence, run.TaskCompletion) {
	t.Helper()
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	fence := leaseFence(t, f, work)
	claim, err := run.ClaimLease(t.Context(), f.Pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.StartLease(t.Context(), f.Pool, fence); err != nil {
		t.Fatal(err)
	}
	if err = run.EnterEntrypoint(t.Context(), f.Pool, fence, claim.Run().EntrypointKind, claim.Run().EntrypointDeclaredID); err != nil {
		t.Fatal(err)
	}
	completion := run.TaskCompletion{Fence: fence, OperationID: pgvalue.UUID(uuid.NewV7()), Fingerprint: dbtest.Digest("task-completion"), Kind: "succeeded", Output: json.RawMessage(`{"answer":42}`)}
	return f, work, fence, completion
}

func beginTaskFinalization(t *testing.T, f runtest.Fixture, work runtest.RunLease, fence run.ExecutionFence, operation pgtype.UUID) (run.Finalization, error) {
	t.Helper()
	return run.BeginFinalization(t.Context(), f.Pool, run.ExecutionFinalization{Fence: fence, RunID: pgvalue.UUID(work.RunID), AttemptNumber: 1, OperationID: operation, Fingerprint: dbtest.Digest("finalization")})
}

func TestClaimLeaseKeepsEpochAndStateFences(t *testing.T) {
	for _, test := range []struct {
		name, sql string
		refresh   bool
	}{
		{"active claims", `UPDATE worker_hosts SET claim_version=claim_version+1`, true},
		{"draining claims", `UPDATE worker_hosts SET status='draining',draining_at=now(),claim_version=claim_version+1`, true},
		{"new epoch", `UPDATE worker_hosts SET current_epoch=2,claim_version=claim_version+1`, false},
		{"lost", `UPDATE worker_hosts SET status='lost',lost_at=now(),claim_version=claim_version+1`, false},
		{"termination ready", `UPDATE worker_hosts SET status='termination_ready',draining_at=now(),termination_ready_at=now(),claim_version=claim_version+1`, false},
		{"active Group claims", `UPDATE worker_groups SET claim_version=claim_version+1`, true},
		{"paused Group", `UPDATE worker_groups SET status='paused',claim_version=claim_version+1`, false},
		{"disabled Group", `UPDATE worker_groups SET status='disabled',claim_version=claim_version+1`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := runtest.New(t)
			work := f.AddRunLease(t, "assigned", time.Now())
			fence := leaseFence(t, f, work)
			target := f.WorkerID
			if strings.Contains(test.sql, "worker_groups") {
				target = runtest.WorkerGroupID
			}
			dbtest.MustExec(t, t.Context(), f.Pool, test.sql+" WHERE id=$1", target)
			_, err := run.ClaimLease(t.Context(), f.Pool, fence)
			want := run.ErrStale
			if test.refresh {
				want = workergroup.ErrStaleClaims
			}
			if !errors.Is(err, want) {
				t.Fatalf("claim error=%v want=%v", err, want)
			}
		})
	}
}

func TestClaimLeaseReplaysFreshClaimWithItsSecrets(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	fence := leaseFence(t, f, work)
	first, err := run.ClaimLease(t.Context(), f.Pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	if _, restored := first.ResumeWait(); restored || first.Lease().Status != db.RunLeaseStatusStarting {
		t.Fatalf("fresh claim = lease %s restored %v", first.Lease().Status, restored)
	}
	replay, err := run.ClaimLease(t.Context(), f.Pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Lease().ClaimedAt.Time.Equal(first.Lease().ClaimedAt.Time) || replay.Lease().ID != first.Lease().ID {
		t.Fatal("claim replay changed the claimed lease")
	}
	stale := fence
	stale.LeaseSequence++
	if _, err = run.ClaimLease(t.Context(), f.Pool, stale); !errors.Is(err, run.ErrStale) {
		t.Fatalf("stale claim=%v", err)
	}
	missing := fence
	missing.LeaseID = pgvalue.UUID(uuid.NewV7())
	if _, err = run.ClaimLease(t.Context(), f.Pool, missing); !errors.Is(err, run.ErrStale) {
		t.Fatalf("missing lease claim=%v", err)
	}
}

func TestStartLeaseRejectsStaleReceipt(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	fence := leaseFence(t, f, work)
	if err := run.StartLease(t.Context(), f.Pool, fence); !errors.Is(err, run.ErrStale) {
		t.Fatalf("unclaimed start=%v", err)
	}
	if _, err := run.ClaimLease(t.Context(), f.Pool, fence); err != nil {
		t.Fatal(err)
	}
	stale := fence
	stale.HostClaimVersion++
	if err := run.StartLease(t.Context(), f.Pool, stale); !errors.Is(err, workergroup.ErrStaleClaims) {
		t.Fatalf("stale claims start=%v", err)
	}
	for range 2 {
		if err := run.StartLease(t.Context(), f.Pool, fence); err != nil {
			t.Fatal(err)
		}
	}
	var running bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='running' FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&running); err != nil || !running {
		t.Fatalf("start did not run the lease: %v %v", running, err)
	}
}

func TestEnterEntrypointTransaction(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	fence := leaseFence(t, f, work)
	claim, err := run.ClaimLease(t.Context(), f.Pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.StartLease(t.Context(), f.Pool, fence); err != nil {
		t.Fatal(err)
	}
	kind, declared := claim.Run().EntrypointKind, claim.Run().EntrypointDeclaredID
	if err = run.EnterEntrypoint(t.Context(), f.Pool, fence, kind, "different"); !errors.Is(err, run.ErrStale) {
		t.Fatalf("wrong entrypoint: %v", err)
	}
	var entered bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT entrypoint_entered_at IS NOT NULL FROM run_attempts WHERE run_id=$1 AND number=1`, work.RunID).Scan(&entered); err != nil || entered {
		t.Fatalf("rejected entry mutated receipt: %v %v", entered, err)
	}
	if err = run.EnterEntrypoint(t.Context(), f.Pool, fence, kind, declared); err != nil {
		t.Fatal(err)
	}
	var original, timeAfterReplay time.Time
	if err = f.Pool.QueryRow(t.Context(), `SELECT entrypoint_entered_at FROM run_attempts WHERE run_id=$1 AND number=1`, work.RunID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if err = run.EnterEntrypoint(t.Context(), f.Pool, fence, kind, declared); err != nil {
		t.Fatal(err)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT entrypoint_entered_at FROM run_attempts WHERE run_id=$1 AND number=1`, work.RunID).Scan(&timeAfterReplay); err != nil || !original.Equal(timeAfterReplay) {
		t.Fatalf("receipt changed on replay: %v", err)
	}
	stale := fence
	stale.HostClaimVersion++
	if err = run.EnterEntrypoint(t.Context(), f.Pool, stale, kind, declared); !errors.Is(err, workergroup.ErrStaleClaims) {
		t.Fatalf("stale claims: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, claim.Instance().ID)
	if err = run.EnterEntrypoint(t.Context(), f.Pool, fence, kind, declared); !errors.Is(err, run.ErrStale) {
		t.Fatalf("expired writer: %v", err)
	}
}

func renewalFixture(t *testing.T) (runtest.Fixture, runtest.RunLease, run.ExecutionFence, time.Time) {
	t.Helper()
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now(),max_active_duration_ms=3600000 WHERE id=$1`, work.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=clock_timestamp()-interval '1 millisecond',expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1`, work.LeaseID)
	var expiry time.Time
	if err := f.Pool.QueryRow(t.Context(), `SELECT expires_at FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	return f, work, leaseFence(t, f, work), expiry
}

func TestRenewLeaseRetainsOnlyPreviousReceiptAndReadsAttemptBase(t *testing.T) {
	f, work, fence, expiry := renewalFixture(t)
	first, err := run.RenewLease(t.Context(), f.Pool, fence, expiry)
	if err != nil {
		t.Fatal(err)
	}
	second, err := run.RenewLease(t.Context(), f.Pool, fence, first.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if !second.ExpiresAt.After(first.ExpiresAt) {
		t.Fatal("second renewal did not advance")
	}
	replay, err := run.RenewLease(t.Context(), f.Pool, fence, first.ExpiresAt)
	if err != nil || !replay.ExpiresAt.Equal(second.ExpiresAt) {
		t.Fatalf("last receipt replay: %+v %v", replay, err)
	}
	if _, err = run.RenewLease(t.Context(), f.Pool, fence, expiry); !errors.Is(err, run.ErrStale) {
		t.Fatalf("two-renewals-old receipt accepted: %v", err)
	}
	var base string
	if err = f.Pool.QueryRow(t.Context(), `SELECT base_computer_disk_version_id::text FROM run_attempts WHERE run_id=$1 AND number=1`, work.RunID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if pgvalue.UUIDString(second.BaseComputerDiskVersionID) != base {
		t.Fatalf("renewal changed the attempt base: %+v", second)
	}
}

func TestRenewLeaseDoesNotWriteWhenHorizonDoesNotAdvance(t *testing.T) {
	f, work, fence, _ := renewalFixture(t)
	var expiry time.Time
	if err := f.Pool.QueryRow(t.Context(), `UPDATE run_leases SET expires_at=clock_timestamp()+interval '10 minutes' WHERE id=$1 RETURNING expires_at`, work.LeaseID).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := f.Pool.QueryRow(t.Context(), `SELECT to_jsonb(l)::text FROM run_leases l WHERE id=$1`, work.LeaseID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	renewed, err := run.RenewLease(t.Context(), f.Pool, fence, expiry)
	if err != nil || !renewed.ExpiresAt.Equal(expiry) {
		t.Fatalf("no-extension=%+v %v", renewed, err)
	}
	var same bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT to_jsonb(l)::text=$2 FROM run_leases l WHERE id=$1`, work.LeaseID, before).Scan(&same); err != nil || !same {
		t.Fatalf("no-extension wrote lease: %v %v", same, err)
	}
}

func TestRenewLeaseAllowsDrainingOwner(t *testing.T) {
	f, _, fence, expiry := renewalFixture(t)
	drained, err := db.New(f.Pool).DrainWorkerHost(t.Context(), db.DrainWorkerHostParams{ID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	fence.HostClaimVersion = drained.ClaimVersion
	renewed, err := run.RenewLease(t.Context(), f.Pool, fence, expiry)
	if err != nil || !renewed.ExpiresAt.After(expiry) {
		t.Fatalf("draining renewal=%+v %v", renewed, err)
	}
}

func TestRenewLeaseRejectsStaleAuthorityWithoutWriting(t *testing.T) {
	for _, kind := range []string{"sequence", "epoch", "elapsed budget", "active deadline"} {
		t.Run(kind, func(t *testing.T) {
			f, work, fence, expiry := renewalFixture(t)
			switch kind {
			case "sequence":
				fence.LeaseSequence++
			case "epoch":
				fence.WorkerEpoch++
			case "elapsed budget":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET active_elapsed_ms=max_active_duration_ms WHERE id=$1`, work.RunID)
			case "active deadline":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET active_started_at=clock_timestamp()-interval '2 hours' WHERE id=$1`, work.RunID)
			}
			var before string
			if err := f.Pool.QueryRow(t.Context(), `SELECT to_jsonb(l)::text FROM run_leases l WHERE id=$1`, work.LeaseID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := run.RenewLease(t.Context(), f.Pool, fence, expiry); !errors.Is(err, run.ErrStale) {
				t.Fatalf("stale renewal=%v", err)
			}
			var same bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT to_jsonb(l)::text=$2 FROM run_leases l WHERE id=$1`, work.LeaseID, before).Scan(&same); err != nil || !same {
				t.Fatalf("rejected renewal wrote lease: %v %v", same, err)
			}
		})
	}
}

func TestRenewLeaseRejectsExpiryDuringComputerLockWait(t *testing.T) {
	f, work, fence, expiry := renewalFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	var pid int32
	if err = blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, blocker, `SELECT id FROM computers WHERE id=(SELECT computer_id FROM runs WHERE id=$1) FOR UPDATE`, work.RunID)
	done := make(chan error, 1)
	go func() { _, e := run.RenewLease(ctx, f.Pool, fence, expiry); done <- e }()
	for {
		var blocked, expired bool
		if err = f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity a WHERE $1=ANY(pg_blocking_pids(a.pid))),expires_at<clock_timestamp() FROM run_leases WHERE id=$2`, pid, work.LeaseID).Scan(&blocked, &expired); err != nil {
			t.Fatal(err)
		}
		if blocked && expired {
			break
		}
		select {
		case e := <-done:
			t.Fatalf("renewal did not wait: %v", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err = blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-done:
		if !errors.Is(e, run.ErrStale) {
			t.Fatalf("expired renewal=%v", e)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var untouched bool
	if err = f.Pool.QueryRow(ctx, `SELECT expires_at=$2 AND previous_expires_at IS NULL FROM run_leases WHERE id=$1`, work.LeaseID, expiry).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("expired renewal changed receipt: %v %v", untouched, err)
	}
}

func TestBeginFinalizationReconcilesOnlyProvenLease(t *testing.T) {
	f, work, fence, completion := taskExecutionFixture(t)
	peer := f.AddRunLease(t, "assigned", time.Now())
	stale := fence
	stale.WorkerEpoch++
	if _, err := beginTaskFinalization(t, f, work, stale, completion.OperationID); !errors.Is(err, run.ErrStale) {
		t.Fatalf("stale worker finalization=%v", err)
	}
	other := work
	other.RunID = uuid.NewV7()
	if _, err := beginTaskFinalization(t, f, other, fence, completion.OperationID); !errors.Is(err, run.ErrStale) {
		t.Fatalf("other Run finalization=%v", err)
	}
	var clean bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT process_reconciled_at IS NULL FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&clean); err != nil || !clean {
		t.Fatalf("stale proof changed reconciliation: %v %v", clean, err)
	}
	var first time.Time
	for n := 0; n < 2; n++ {
		begun, err := beginTaskFinalization(t, f, work, fence, completion.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		if begun.StartedAt.IsZero() || begun.ExpiresAt.IsZero() {
			t.Fatalf("finalization grant=%+v", begun)
		}
		var at time.Time
		if err = f.Pool.QueryRow(t.Context(), `SELECT process_reconciled_at FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			first = at
		} else if !at.Equal(first) {
			t.Fatal("replay changed proof timestamp")
		}
	}
	claims := fence
	claims.HostClaimVersion++
	if _, err := beginTaskFinalization(t, f, work, claims, completion.OperationID); !errors.Is(err, workergroup.ErrStaleClaims) || errors.Is(err, run.ErrStale) {
		t.Fatalf("stale claims finalization=%v", err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT process_reconciled_at IS NULL FROM run_leases WHERE id=$1`, peer.LeaseID).Scan(&clean); err != nil || !clean {
		t.Fatalf("peer reconciled: %v %v", clean, err)
	}
	if err := run.CompleteTask(t.Context(), f.Pool, db.New(f.Pool), completion); err != nil {
		t.Fatal(err)
	}
	var computerID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM runs WHERE id=$1`, work.RunID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	if _, err := computer.Delete(t.Context(), f.Pool, computer.Deletion{Scope: computer.Scope{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID}, ComputerID: computerID, IdempotencyKey: "delete-completed-member"}); err != nil {
		t.Fatalf("completed proven member blocked Computer deletion: %v", err)
	}
}

func TestCompleteTaskPreservesReceiptAndStaleOutcomes(t *testing.T) {
	f, work, fence, completion := taskExecutionFixture(t)
	replays := db.New(f.Pool)
	if err := run.CompleteTask(t.Context(), f.Pool, replays, completion); !errors.Is(err, run.ErrStale) || errors.Is(err, run.ErrTaskCompletionReplayDiffers) {
		t.Fatalf("running lease completion=%v", err)
	}
	if _, err := beginTaskFinalization(t, f, work, fence, completion.OperationID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET retry_policy='{"enabled":true}' WHERE id=$1`, work.RunID)
	failed := completion
	failed.Kind, failed.Output, failed.Error = "failed", nil, json.RawMessage(`{"message":"failed","details":{}}`)
	failed.Fingerprint = dbtest.Digest("task-failure")
	if err := run.CompleteTask(t.Context(), f.Pool, replays, failed); !errors.Is(err, run.ErrTaskCompletionAdmission) || errors.Is(err, run.ErrStale) {
		t.Fatalf("invalid pinned retry policy=%v", err)
	}
	var unchanged bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.status='running' AND l.status='finalizing' AND l.terminal_at IS NULL FROM runs r JOIN run_leases l ON l.id=r.current_run_lease_id WHERE r.id=$1`, work.RunID).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("invalid admission settled Task: %v %v", unchanged, err)
	}
	claims := completion
	claims.Fence.HostClaimVersion++
	if err := run.CompleteTask(t.Context(), f.Pool, replays, claims); !errors.Is(err, workergroup.ErrStaleClaims) {
		t.Fatalf("stale claims completion=%v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET retry_policy='{"enabled":false}' WHERE id=$1`, work.RunID)
	if err := run.CompleteTask(t.Context(), f.Pool, replays, completion); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var value string
		if err := f.Pool.QueryRow(t.Context(), `SELECT to_jsonb(l)::text FROM run_leases l WHERE id=$1`, work.LeaseID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	receipt := snapshot()
	previousEpoch := completion
	previousEpoch.Fence.WorkerEpoch++
	if err := run.CompleteTask(t.Context(), f.Pool, replays, previousEpoch); err != nil {
		t.Fatalf("previous-epoch replay=%v", err)
	}
	changed := completion
	changed.Fingerprint = dbtest.Digest("changed-completion")
	if err := run.CompleteTask(t.Context(), f.Pool, replays, changed); !errors.Is(err, run.ErrTaskCompletionReplayDiffers) {
		t.Fatalf("changed replay=%v", err)
	}
	if snapshot() != receipt {
		t.Fatal("replay changed durable terminal receipt")
	}
}

func metadataUpdate(t *testing.T, fence run.ExecutionFence, operation uuid.UUID, key string, value json.RawMessage) run.MetadataUpdate {
	t.Helper()
	mutation, err := run.NewMetadataMutation("set", key, value, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return metadataMutationUpdate(fence, operation, mutation)
}

func metadataMutationUpdate(fence run.ExecutionFence, operation uuid.UUID, mutation run.MetadataMutation) run.MetadataUpdate {
	return run.MetadataUpdate{
		Fence: fence, OperationID: operation, Mutation: mutation, FenceFingerprint: dbtest.Digest("metadata-fence"),
		Event: func() (json.RawMessage, error) {
			return json.Marshal(map[string]any{"operation": mutation.Operation(), "operation_id": operation.String(), "key": mutation.Key()})
		},
	}
}

func TestUpdateMetadataAppliesOnceAndRejectsStaleReceipts(t *testing.T) {
	f, work, fence, _ := taskExecutionFixture(t)
	operation := uuid.NewV7()
	update := metadataUpdate(t, fence, operation, "phase", json.RawMessage(`"running"`))
	for range 2 {
		if err := run.UpdateMetadata(t.Context(), f.Pool, update); err != nil {
			t.Fatal(err)
		}
	}
	var phase string
	var events int
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.metadata->>'phase',(SELECT count(*) FROM telemetry_outbox e WHERE e.idempotency_key=$2) FROM runs r WHERE r.id=$1`, work.RunID, "metadata:"+operation.String()).Scan(&phase, &events); err != nil {
		t.Fatal(err)
	}
	if phase != "running" || events != 1 {
		t.Fatalf("metadata phase=%s events=%d", phase, events)
	}
	var receipt string
	if err := f.Pool.QueryRow(t.Context(), `SELECT receipt::text FROM idempotency_claims WHERE status='completed' AND receipt->>'runId'=$1`, work.RunID.String()).Scan(&receipt); err != nil || !strings.Contains(receipt, `"revision"`) {
		t.Fatalf("metadata receipt=%s %v", receipt, err)
	}
	amount := 1.0
	increment, err := run.NewMetadataMutation("increment", "phase", nil, nil, &amount)
	if err != nil {
		t.Fatal(err)
	}
	rejected := metadataMutationUpdate(fence, uuid.NewV7(), increment)
	if err := run.UpdateMetadata(t.Context(), f.Pool, rejected); err == nil || err.Error() != `run metadata key "phase" is not a finite number` {
		t.Fatalf("rejected mutation=%v", err)
	}
	stale := metadataUpdate(t, fence, uuid.NewV7(), "phase", json.RawMessage(`"done"`))
	stale.Fence.LeaseSequence++
	if err := run.UpdateMetadata(t.Context(), f.Pool, stale); !errors.Is(err, run.ErrStale) {
		t.Fatalf("stale receipt=%v", err)
	}
	claims := metadataUpdate(t, fence, uuid.NewV7(), "phase", json.RawMessage(`"done"`))
	claims.Fence.HostClaimVersion++
	if err := run.UpdateMetadata(t.Context(), f.Pool, claims); !errors.Is(err, workergroup.ErrStaleClaims) {
		t.Fatalf("stale claims=%v", err)
	}
}

func TestUpdateMetadataRejectsWaitingWithoutLosingAuthority(t *testing.T) {
	for _, leaseStatus := range []string{"running", "checkpointing"} {
		t.Run(leaseStatus, func(t *testing.T) {
			f, work, fence, _ := taskExecutionFixture(t)
			completed := metadataUpdate(t, fence, uuid.NewV7(), "phase", json.RawMessage(`"before"`))
			if err := run.UpdateMetadata(t.Context(), f.Pool, completed); err != nil {
				t.Fatal(err)
			}
			wait, err := run.RegisterTimerWait(t.Context(), f.Pool, timerWait(fence, uuid.NewV7(), dbtest.Digest("metadata-wait")))
			if err != nil {
				t.Fatal(err)
			}
			if leaseStatus == "checkpointing" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='checkpointing' WHERE id=$1`, work.LeaseID)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET suspension_status='checkpointing' WHERE id=$1`, wait.ID)
			}
			snapshot := func() string {
				t.Helper()
				var state string
				err := f.Pool.QueryRow(t.Context(), `SELECT jsonb_build_array(to_jsonb(r),to_jsonb(w),to_jsonb(l),to_jsonb(a),(SELECT count(*) FROM idempotency_claims))::text FROM runs r JOIN run_waits w ON w.run_id=r.id JOIN run_leases l ON l.id=r.current_run_lease_id JOIN run_attempts a ON a.run_id=r.id AND a.number=r.current_attempt_number WHERE r.id=$1`, work.RunID).Scan(&state)
				if err != nil {
					t.Fatal(err)
				}
				return state
			}
			before := snapshot()
			rejected := metadataUpdate(t, fence, uuid.NewV7(), "phase", json.RawMessage(`"during"`))
			err = run.UpdateMetadata(t.Context(), f.Pool, rejected)
			if err == nil || errors.Is(err, run.ErrStale) || err.Error() != "run metadata cannot be updated while a managed wait is pending" {
				t.Fatalf("waiting mutation=%v", err)
			}
			if err := run.UpdateMetadata(t.Context(), f.Pool, completed); err != nil {
				t.Fatalf("completed replay=%v", err)
			}
			stale := rejected
			stale.Fence.LeaseSequence++
			if err := run.UpdateMetadata(t.Context(), f.Pool, stale); !errors.Is(err, run.ErrStale) {
				t.Fatalf("stale receipt=%v", err)
			}
			if after := snapshot(); after != before {
				t.Fatal("rejected mutation or completed replay changed execution, wait, metadata or claims")
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET due_at=now()-interval '1 second' WHERE id=$1`, wait.ID)
			reconciler, err := run.NewTimerWaitReconciler(f.Pool)
			if err != nil {
				t.Fatal(err)
			}
			if count, err := reconciler.ReconcileDue(t.Context(), 10); err != nil || count != 1 {
				t.Fatalf("wait resolution count=%d err=%v", count, err)
			}
			if leaseStatus == "running" {
				if err := run.UpdateMetadata(t.Context(), f.Pool, rejected); err != nil {
					t.Fatalf("retry after wait=%v", err)
				}
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=created_at,expires_at=clock_timestamp() WHERE id=$1`, work.LeaseID)
			expired := metadataUpdate(t, fence, uuid.NewV7(), "phase", json.RawMessage(`"expired"`))
			if err := run.UpdateMetadata(t.Context(), f.Pool, expired); !errors.Is(err, run.ErrStale) {
				t.Fatalf("expired receipt=%v", err)
			}
		})
	}
}
