package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func TestCreateSameWorkspaceChildRunQueryMatchesGreenfieldSchema(t *testing.T) {
	fixture := runtest.New(t)
	_, err := db.New(fixture.Pool).CreateSameWorkspaceChildRunFromParentDeployment(
		t.Context(),
		db.CreateSameWorkspaceChildRunFromParentDeploymentParams{
			RunWaitID:              pgvalue.UUID(uuid.NewV7()),
			EntrypointDeclaredID:   pgvalue.Text("test-task"),
			ClaimID:                pgvalue.UUID(uuid.NewV7()),
			ParentRunLeaseID:       pgvalue.UUID(uuid.NewV7()),
			SuspendCheckpointID:    pgvalue.UUID(uuid.NewV7()),
			BaseWorkspaceVersionID: pgvalue.UUID(uuid.NewV7()),
			EnvironmentID:          pgvalue.UUID(fixture.EnvironmentID),
			ParentRunID:            pgvalue.UUID(uuid.NewV7()),
			ParentAttemptNumber:    1,
			ID:                     pgvalue.UUID(uuid.NewV7()),
			QueueName:              "default",
			QueueOriginAt:          pgvalue.Timestamptz(time.Now()),
			QueueScoreAt:           pgvalue.Timestamptz(time.Now()),
			MaxActiveDurationMs:    60_000,
			RetryPolicy:            []byte(`{"enabled":false}`),
			RootSpanID:             "1111111111111111",
		},
	)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("empty-authority query error = %v, want no rows", err)
	}
}

func checkpointFailureFixture(t *testing.T) (runtest.Fixture, runtest.RunLease, workerapi.CheckpointFailedRequest, workerActor) {
	t.Helper()
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1`, work.RunID)
	waitID := uuid.NewV7()
	checkpointID := uuid.NewV7()
	resumeAttachID := uuid.NewV7()
	var workspaceID, workspaceLeaseID, baseWorkspaceVersionID uuid.UUID
	if err := fixture.Pool.QueryRow(t.Context(), `
SELECT runs.workspace_id, workspace_leases.id, workspace_leases.base_workspace_version_id
  FROM runs
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = runs.current_run_lease_id
 WHERE runs.id = $1`, work.RunID).Scan(&workspaceID, &workspaceLeaseID, &baseWorkspaceVersionID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE run_leases
   SET status = 'checkpointing', started_at = claimed_at
 WHERE id = $1`, work.LeaseID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE runs
   SET status = 'waiting', revision = 2,
       started_at = transaction_timestamp() - interval '10 seconds',
       active_started_at = transaction_timestamp() - interval '10 seconds',
       retry_policy = '{"enabled":false}'::jsonb
 WHERE id = $1`, work.RunID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind, due_at,
    expected_run_revision, attempt_number, current_run_lease_id,
    checkpoint_request_version, resume_attach_id, suspension_status
) VALUES (
    $1, $2, $3, $4, 'timer', transaction_timestamp() + interval '1 hour',
    2, 1, $5, 1, $6, 'checkpointing'
)`, waitID, fixture.EnvironmentID, work.RunID, workspaceID, work.LeaseID, resumeAttachID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
INSERT INTO run_checkpoints (
    id, run_id, attempt_number, run_wait_id, source_run_lease_id,
    source_workspace_lease_id, workspace_id, base_workspace_version_id, status
) VALUES ($1, $2, 1, $3, $4, $5, $6, $7, 'creating')`,
		checkpointID, work.RunID, waitID, work.LeaseID, workspaceLeaseID, workspaceID, baseWorkspaceVersionID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE run_waits SET suspend_checkpoint_id = $2 WHERE id = $1`, waitID, checkpointID)

	var workerClaimVersion, groupClaimVersion int64
	if err := fixture.Pool.QueryRow(t.Context(), `
SELECT worker_instances.claim_version, worker_groups.claim_version
  FROM worker_instances
  JOIN worker_groups ON worker_groups.id = worker_instances.worker_group_id
 WHERE worker_instances.id = $1`, fixture.WorkerID).Scan(&workerClaimVersion, &groupClaimVersion); err != nil {
		t.Fatal(err)
	}
	return fixture, work, workerapi.CheckpointFailedRequest{
		Lease:          workerapi.RunLeaseFence{ID: work.LeaseID.String(), LeaseSequence: 1},
		RequestVersion: 1, RunWaitID: waitID.String(), CheckpointID: checkpointID.String(),
		Error: "snapshot failed",
	}, workerActor{
		WorkerInstanceID: fixture.WorkerID, WorkerGroupID: runtest.WorkerGroupID,
		WorkerEpoch: 1, ClaimVersion: workerClaimVersion, GroupClaimVersion: groupClaimVersion,
	}
}

func callCheckpointFailure(t *testing.T, fixture runtest.Fixture, receipt workerapi.CheckpointFailedRequest, worker workerActor) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/run/checkpoints/failed", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(t.Context(), workerContextKey{}, worker))
	response := httptest.NewRecorder()
	server := &Server{db: db.New(fixture.Pool), tx: fixture.Pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	server.workerMarkCheckpointFailed(response, request)
	return response
}

func TestWorkerCheckpointFailureHonorsTaskRetryPolicy(t *testing.T) {
	for i, policy := range []string{`{"enabled":false}`, `{"enabled":true,"maxAttempts":3,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}`, `{"enabled":true}`} {
		t.Run(policy, func(t *testing.T) {
			fixture, work, receipt, worker := checkpointFailureFixture(t)
			dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE runs SET retry_policy=$2 WHERE id=$1`, work.RunID, policy)
			var originalHead string
			if err := fixture.Pool.QueryRow(t.Context(), `SELECT w.head_version_id::text FROM computers w JOIN runs r ON r.workspace_id=w.id WHERE r.id=$1`, work.RunID).Scan(&originalHead); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				response := callCheckpointFailure(t, fixture, receipt, worker)
				if response.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
			}
			var status, computer, dirty, reason, message, head string
			var attempts int
			var noRetry, noOwner bool
			err := fixture.Pool.QueryRow(t.Context(), `SELECT r.status,w.status,w.dirty_state,coalesce(r.failure->>'code',(SELECT terminal_reason_code FROM run_attempts WHERE run_id=r.id AND number=1)),coalesce(r.failure->>'message',(SELECT terminal_error->>'message' FROM run_attempts WHERE run_id=r.id AND number=1)),w.head_version_id::text,
 (SELECT count(*) FROM run_attempts a WHERE a.run_id=r.id),r.retry_at IS NULL,w.owner_run_id IS NULL
 FROM runs r JOIN computers w ON w.id=r.workspace_id WHERE r.id=$1`, work.RunID).Scan(&status, &computer, &dirty, &reason, &message, &head, &attempts, &noRetry, &noOwner)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus, wantAttempts, wantNoRetry := "system_failed", 1, true
			if i == 1 {
				wantStatus, wantAttempts, wantNoRetry = "retry_delayed", 2, false
			}
			if status != wantStatus || computer != "recovery_required" || dirty != "dirty_state_lost" || reason != "checkpoint_failed" || message != receipt.Error || head != originalHead || attempts != wantAttempts || noRetry != wantNoRetry || noOwner != wantNoRetry {
				t.Fatalf("state=%s/%s/%s reason=%s message=%s head=%s attempts=%d noRetry=%t noOwner=%t", status, computer, dirty, reason, message, head, attempts, noRetry, noOwner)
			}
			var condition, suspension, writer string
			var cleared bool
			if err := fixture.Pool.QueryRow(t.Context(), `SELECT w.condition_status,w.suspension_status,l.status,r.current_run_lease_id IS NULL AND r.active_started_at IS NULL
            FROM runs r JOIN run_waits w ON w.run_id=r.id JOIN workspace_leases l ON l.owner_run_lease_id=$2 WHERE r.id=$1`, work.RunID, work.LeaseID).Scan(&condition, &suspension, &writer, &cleared); err != nil {
				t.Fatal(err)
			}
			if condition != "failed" || suspension != "failed" || writer != "fenced" || !cleared {
				t.Fatalf("wait=%s/%s writer=%s cleared=%t", condition, suspension, writer, cleared)
			}

			receipt.Error = "different failure"
			if response := callCheckpointFailure(t, fixture, receipt, worker); response.Code != http.StatusConflict {
				t.Fatalf("conflicting replay=%d", response.Code)
			}
		})
	}
}

func TestCheckpointFailureAndSourceFailureCommutePostgres(t *testing.T) {
	for _, order := range []string{"source_first", "checkpoint_first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			f, work, receipt, worker := checkpointFailureFixture(t)
			q := db.New(f.Pool)
			var p db.FailWorkspaceMountParams
			p.OrgID, p.WorkerInstanceID, p.WorkerEpoch = pgvalue.UUID(f.OrgID), pgvalue.UUID(f.WorkerID), 1
			p.ReasonCode, p.Error = pgvalue.Text("worker_mount_failed"), []byte(`{"message":"stop failed"}`)
			if err := f.Pool.QueryRow(t.Context(), `SELECT m.id,m.runtime_instance_id,m.fencing_generation FROM workspace_mounts m JOIN run_leases l ON l.runtime_instance_id=m.runtime_instance_id WHERE l.id=$1`, work.LeaseID).Scan(&p.ID, &p.RuntimeInstanceID, &p.FencingGeneration); err != nil {
				t.Fatal(err)
			}
			failSource := func() {
				if _, err := q.FailWorkspaceMount(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			}
			if order == "source_first" {
				failSource()
			}
			if order == "concurrent" {
				start := make(chan struct{})
				sourceDone := make(chan error, 1)
				receiptDone := make(chan *httptest.ResponseRecorder, 1)
				go func() { <-start; _, err := q.FailWorkspaceMount(t.Context(), p); sourceDone <- err }()
				go func() { <-start; receiptDone <- callCheckpointFailure(t, f, receipt, worker) }()
				close(start)
				if err := <-sourceDone; err != nil {
					t.Fatal(err)
				}
				if r := <-receiptDone; r.Code != http.StatusOK {
					t.Fatalf("concurrent settlement: %d %s", r.Code, r.Body.String())
				}
			}
			for range 2 {
				r := callCheckpointFailure(t, f, receipt, worker)
				if r.Code != http.StatusOK {
					t.Fatalf("checkpoint failure: %d %s", r.Code, r.Body.String())
				}
			}
			if order == "checkpoint_first" {
				failSource()
			}
			var observedBefore int64
			if err := f.Pool.QueryRow(t.Context(), `SELECT observed_version FROM runtime_instances WHERE id=$1`, p.RuntimeInstanceID).Scan(&observedBefore); err != nil {
				t.Fatal(err)
			}
			failSource() // Replay must preserve the first failure and version.
			var checkpoint, lease, desired, observed, mount string
			var version int64
			var unreclaimed bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT c.status,l.status,r.desired_state,r.observed_state,m.status,r.observed_version,r.reclaimed_at IS NULL FROM run_checkpoints c JOIN run_leases l ON l.id=c.source_run_lease_id JOIN runtime_instances r ON r.id=l.runtime_instance_id JOIN workspace_mounts m ON m.runtime_instance_id=l.runtime_instance_id WHERE c.id=$1`, uuid.MustParse(receipt.CheckpointID)).Scan(&checkpoint, &lease, &desired, &observed, &mount, &version, &unreclaimed); err != nil {
				t.Fatal(err)
			}
			if checkpoint != "invalid" || lease != "failed" || desired != "closed" || observed != "failed" || mount != "failed" || version != observedBefore || !unreclaimed {
				t.Fatalf("settlement = %s/%s/%s/%s/%s version=%d->%d unreclaimed=%t", checkpoint, lease, desired, observed, mount, observedBefore, version, unreclaimed)
			}
			for _, stale := range []string{"epoch", "fence"} {
				bad := p
				if stale == "epoch" {
					bad.WorkerEpoch++
				} else {
					bad.FencingGeneration++
				}
				if _, err := q.FailWorkspaceMount(t.Context(), bad); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("stale %s accepted: %v", stale, err)
				}
			}
		})
	}
}

func TestCheckpointFailureRejectsExpiredSourceAuthorityPostgres(t *testing.T) {
	f, work, receipt, worker := checkpointFailureFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET observed_state='failed',observed_version=observed_version+1,terminal_at=now(),terminal_reason_code='guest_exited' WHERE id=(SELECT runtime_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=claimed_at+interval '1 second',expires_at=now()-interval '1 second' WHERE id=$1`, work.LeaseID)
	response := callCheckpointFailure(t, f, receipt, worker)
	if response.Code != http.StatusConflict {
		t.Fatalf("expired authority accepted: %d %s", response.Code, response.Body.String())
	}
	var state string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM run_checkpoints WHERE id=$1`, uuid.MustParse(receipt.CheckpointID)).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "creating" {
		t.Fatalf("stale failure mutated checkpoint: %s", state)
	}
}

func TestSharedComputerCheckpointFailureStopsOwnerAndChildPostgres(t *testing.T) {
	f := newSameWorkspaceCompletionPostgresFixture(t, true)
	ctx := t.Context()
	f.server.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	childLease := uuid.MustParse(f.request.Lease.ID)
	checkpointID, waitID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `UPDATE run_leases SET status='checkpointing',finalization_operation_id=NULL,finalization_kind=NULL,finalization_started_at=NULL,finalization_request_fingerprint=NULL,finalization_root=NULL WHERE id=$1`, childLease)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE runs SET status='waiting',active_started_at=transaction_timestamp() WHERE id=$1`, f.childRunID)
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO run_waits (id,environment_id,run_id,workspace_id,kind,due_at,expected_run_revision,attempt_number,current_run_lease_id,checkpoint_request_version,resume_attach_id,suspension_status)
 SELECT $1,environment_id,id,workspace_id,'timer',transaction_timestamp()+interval '1 hour',revision,1,$2,1,$3,'checkpointing' FROM runs WHERE id=$4`, waitID, childLease, uuid.NewV7(), f.childRunID)
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO run_checkpoints (id,run_id,attempt_number,run_wait_id,source_run_lease_id,source_workspace_lease_id,workspace_id,base_workspace_version_id,status)
 SELECT $1,$2,1,$3,$4,id,workspace_id,base_workspace_version_id,'creating' FROM workspace_leases WHERE id=$5`, checkpointID, f.childRunID, waitID, childLease, f.workspaceLeaseID)
	dbtest.MustExec(t, ctx, f.pool, `UPDATE run_waits SET suspend_checkpoint_id=$2 WHERE id=$1`, waitID, checkpointID)
	receipt := workerapi.CheckpointFailedRequest{Lease: f.request.Lease, RunWaitID: waitID.String(), CheckpointID: checkpointID.String(), RequestVersion: 1, Error: "disk capture failed"}
	for range 2 {
		body, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/worker/v1/run/checkpoints/failed", bytes.NewReader(body))
		req = req.WithContext(context.WithValue(ctx, workerContextKey{}, f.worker))
		response := httptest.NewRecorder()
		f.server.workerMarkCheckpointFailed(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d %s", response.Code, response.Body.String())
		}
	}
	for _, id := range []uuid.UUID{f.parentRunID, f.childRunID} {
		var status, reason, computer, dirty string
		var attempts, ready int
		var ownerGone bool
		if err := f.pool.QueryRow(ctx, `SELECT r.status,r.failure->>'code',w.status,w.dirty_state,w.owner_run_id IS NULL,
 (SELECT count(*) FROM run_attempts a WHERE a.run_id=r.id),
 (SELECT count(*) FROM run_checkpoints c WHERE c.run_id=r.id AND c.status='ready')
 FROM runs r JOIN computers w ON w.id=r.workspace_id WHERE r.id=$1`, id).Scan(&status, &reason, &computer, &dirty, &ownerGone, &attempts, &ready); err != nil {
			t.Fatal(err)
		}
		want := "computer_recovery_required"
		if id == f.childRunID {
			want = "checkpoint_failed"
		}
		if status != "system_failed" || reason != want || computer != "recovery_required" || dirty != "dirty_state_lost" || !ownerGone || attempts != 1 || ready != 0 {
			t.Fatalf("run=%s state=%s/%s/%s/%s ownerGone=%t attempts=%d ready=%d", id, status, reason, computer, dirty, ownerGone, attempts, ready)
		}
	}
}

func TestCheckpointFailureRollsBackReceiptAndComputerPostgres(t *testing.T) {
	f, work, receipt, worker := checkpointFailureFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_failed_run() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='system_failed' THEN RAISE EXCEPTION 'injected terminal failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_failed_run BEFORE UPDATE ON runs FOR EACH ROW EXECUTE FUNCTION reject_failed_run()`)
	r := callCheckpointFailure(t, f, receipt, worker)
	if r.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d %s", r.Code, r.Body.String())
	}
	var status, computer, checkpoint string
	var noReceipt bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.status,w.status,c.status,c.failed_request_fingerprint IS NULL FROM runs r JOIN computers w ON w.id=r.workspace_id JOIN run_checkpoints c ON c.run_id=r.id WHERE r.id=$1`, work.RunID).Scan(&status, &computer, &checkpoint, &noReceipt); err != nil {
		t.Fatal(err)
	}
	if status != "waiting" || computer != "active" || checkpoint != "creating" || !noReceipt {
		t.Fatalf("partial commit=%s/%s/%s receipt absent=%t", status, computer, checkpoint, noReceipt)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DROP TRIGGER reject_failed_run ON runs`)
	if r := callCheckpointFailure(t, f, receipt, worker); r.Code != http.StatusOK {
		t.Fatalf("retry=%d %s", r.Code, r.Body.String())
	}
}

func TestCheckpointFailurePreservesExceededActiveBudgetPostgres(t *testing.T) {
	f, work, receipt, worker := checkpointFailureFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET max_active_duration_ms=5000 WHERE id=$1`, work.RunID)
	r := callCheckpointFailure(t, f, receipt, worker)
	if r.Code != http.StatusOK {
		t.Fatalf("status=%d %s", r.Code, r.Body.String())
	}
	var status, code, message, attemptReason string
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.status,r.failure->>'code',r.failure->>'message',a.terminal_reason_code FROM runs r JOIN run_attempts a ON a.run_id=r.id AND a.number=1 WHERE r.id=$1`, work.RunID).Scan(&status, &code, &message, &attemptReason); err != nil {
		t.Fatal(err)
	}
	if status != "expired" || code != "max_active_duration_exceeded" || attemptReason != code || message != receipt.Error {
		t.Fatalf("state=%s/%s/%s/%s", status, code, message, attemptReason)
	}
}
