package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTokenWaitRegistrationImmediatelyMatchesTerminalTokenAfterEmptyReconcile(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	authority := startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	if _, err := fixture.queries.CompleteToken(ctx, tokenCompletionParams(
		fixture, tokenID, "sha256:late-registration", `{"approved":true}`,
	)); err != nil {
		t.Fatal(err)
	}
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, tokenID, 100)
	if err != nil || batch.Examined != 0 {
		t.Fatalf("empty reconcile = %+v, %v", batch, err)
	}
	var expectedRunVersion int64
	if err := fixture.pool.QueryRow(ctx, `SELECT state_version FROM runs WHERE id = $1`, work.runID).Scan(&expectedRunVersion); err != nil {
		t.Fatal(err)
	}
	waitID := uuid.Must(uuid.NewV7())
	registration := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, waitID)
	registered, err := reconciler.RegisterWait(ctx, registration)
	if err != nil {
		t.Fatal(err)
	}
	if registered.ConditionState != db.WaitStateCompleted || registered.SuspensionState != db.RunWaitStateReleased ||
		registered.RunStateVersion != expectedRunVersion+2 || string(registered.Result) != `{"approved": true}` {
		t.Fatalf("registration = %+v", registered)
	}
	replayed, err := reconciler.RegisterWait(ctx, registration)
	if err != nil || replayed.WaitID != registered.WaitID || replayed.ConditionState != registered.ConditionState ||
		replayed.SuspensionState != registered.SuspensionState || string(replayed.Result) != string(registered.Result) {
		t.Fatalf("registration replay = %+v, %v; first = %+v", replayed, err, registered)
	}
	var runStatus db.RunStatus
	var runVersion int64
	var condition db.WaitState
	var suspension db.RunWaitState
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.status, runs.state_version, run_waits.condition_state, run_waits.suspension_state
		  FROM runs JOIN run_waits ON run_waits.run_id = runs.id
		 WHERE runs.id = $1 AND run_waits.id = $2
	`, work.runID, waitID).Scan(&runStatus, &runVersion, &condition, &suspension); err != nil {
		t.Fatal(err)
	}
	if runStatus != db.RunStatusRunning || runVersion != expectedRunVersion+2 || condition != db.WaitStateCompleted || suspension != db.RunWaitStateReleased {
		t.Fatalf("durable registration = run %s/%d condition %s suspension %s workspace %s", runStatus, runVersion, condition, suspension, authority.workspaceID)
	}
}

func TestTokenWaitRegistrationBeforeCompletionIsReconciled(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	var runVersion int64
	if err := fixture.pool.QueryRow(ctx, `SELECT state_version FROM runs WHERE id = $1`, work.runID).Scan(&runVersion); err != nil {
		t.Fatal(err)
	}
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	waitID := uuid.Must(uuid.NewV7())
	registration := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, waitID)
	registered, err := reconciler.RegisterWait(ctx, registration)
	if err != nil {
		t.Fatal(err)
	}
	if registered.ConditionState != db.WaitStatePending || registered.SuspensionState != db.RunWaitStateHot ||
		registered.RunStateVersion != runVersion+1 {
		t.Fatalf("pending registration = %+v", registered)
	}
	if _, err := fixture.queries.CompleteToken(ctx, tokenCompletionParams(
		fixture, tokenID, "sha256:registration-first", `{"approved":true}`,
	)); err != nil {
		t.Fatal(err)
	}
	batch, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, tokenID, 100)
	if err != nil || batch.Examined != 1 || batch.Resolved != 1 {
		t.Fatalf("registration-first reconcile = %+v, %v", batch, err)
	}
	var status db.RunStatus
	var condition db.WaitState
	var suspension db.RunWaitState
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.status, run_waits.condition_state, run_waits.suspension_state
		  FROM runs JOIN run_waits ON run_waits.run_id = runs.id
		 WHERE runs.id = $1 AND run_waits.id = $2
	`, work.runID, waitID).Scan(&status, &condition, &suspension); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusRunning || condition != db.WaitStateCompleted || suspension != db.RunWaitStateReleased {
		t.Fatalf("registration-first state = run %s condition %s suspension %s", status, condition, suspension)
	}
}

func TestTokenCompletionReconcilesEveryWaitingRunInBoundedBatches(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	first := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	second := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, first)
	startTaskCompletionWork(t, ctx, fixture, second)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, work := range []runLeaseWork{first, second} {
		request := tokenWaitRegistrationRequest(
			t,
			ctx,
			fixture,
			work,
			tokenID,
			uuid.Must(uuid.NewV7()),
		)
		registered, err := reconciler.RegisterWait(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if registered.ConditionState != db.WaitStatePending ||
			registered.SuspensionState != db.RunWaitStateHot {
			t.Fatalf("pending registration = %+v", registered)
		}
	}
	if _, err := fixture.queries.CompleteToken(ctx, tokenCompletionParams(
		fixture,
		tokenID,
		"sha256:multi-run-fan-out",
		`{"approved":true}`,
	)); err != nil {
		t.Fatal(err)
	}

	for batchNumber := 1; batchNumber <= 2; batchNumber++ {
		batch, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, tokenID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if batch.Examined != 1 || batch.Resolved != 1 || batch.Deferred != 0 {
			t.Fatalf("batch %d = %+v", batchNumber, batch)
		}
	}
	replay, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, tokenID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Examined != 0 || replay.Resolved != 0 || replay.Deferred != 0 {
		t.Fatalf("replay batch = %+v", replay)
	}

	var completedWaits, runningRuns int
	if err := fixture.pool.QueryRow(ctx, `
SELECT count(*),
       count(*) FILTER (WHERE runs.status = 'running')
  FROM run_waits
  JOIN runs ON runs.id = run_waits.run_id
 WHERE run_waits.environment_id = $1
   AND run_waits.token_id = $2
   AND run_waits.condition_state = 'completed'
   AND run_waits.suspension_state = 'released'
   AND run_waits.condition_result = '{"approved":true}'::jsonb
`, fixture.environmentID, tokenID).Scan(&completedWaits, &runningRuns); err != nil {
		t.Fatal(err)
	}
	if completedWaits != 2 || runningRuns != 2 {
		t.Fatalf("fan-out = completed Waits %d, running Runs %d", completedWaits, runningRuns)
	}
}

func TestTokenWaitSchemaRejectsCrossEnvironmentReference(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	otherEnvironmentID := uuid.Must(uuid.NewV7())
	mustRunLeaseExec(t, ctx, fixture.pool, `
		INSERT INTO environments (
		    id, org_id, project_id, slug, name, color_hex
		) VALUES ($1, $2, $3, $4, 'Other Environment', '#3366ff')
	`, otherEnvironmentID, fixture.orgID, fixture.projectID,
		"other-"+shortRunLeaseID(otherEnvironmentID))
	otherTokenID := uuid.Must(uuid.NewV7())
	if _, err := fixture.queries.CreateToken(ctx, db.CreateTokenParams{
		ID:    pgvalue.UUID(otherTokenID),
		OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID:             pgvalue.UUID(otherEnvironmentID),
		ExpiresAt:                 pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		CallbackSecretFingerprint: make([]byte, 32),
		Metadata:                  []byte(`{}`), Tags: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	var workspaceID uuid.UUID
	if err := fixture.pool.QueryRow(
		ctx,
		`SELECT workspace_id FROM runs WHERE id = $1`,
		work.runID,
	).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO run_waits (
		    id, environment_id, run_id, workspace_id, kind, token_id,
		    token_registration_run_state_version, expected_run_state_version,
		    attempt_number, current_run_lease_id, resume_attach_id
		) VALUES ($1, $2, $3, $4, 'token', $5, 0, 1, 1, $6, $7)
	`, uuid.Must(uuid.NewV7()), fixture.environmentID, work.runID, workspaceID,
		otherTokenID, work.leaseID, uuid.Must(uuid.NewV7())); err == nil {
		t.Fatal("cross-Environment Token Wait reference was accepted")
	}
}

func TestFailedCreatingCheckpointFailsAttemptAndClosesSource(t *testing.T) {
	testFailedCreatingCheckpointFailsAttemptAndClosesSource(t, checkpointFailureTestMode{})
}

func TestFailedCreatingCheckpointSchedulesPinnedTaskRetry(t *testing.T) {
	testFailedCreatingCheckpointFailsAttemptAndClosesSource(t, checkpointFailureTestMode{retry: true})
}

func TestFailedCreatingActorCheckpointSchedulesPinnedRetry(t *testing.T) {
	testFailedCreatingCheckpointFailsAttemptAndClosesSource(t, checkpointFailureTestMode{actor: true, retry: true})
}

func TestFailedCreatingActorCheckpointExhaustionFailsActor(t *testing.T) {
	testFailedCreatingCheckpointFailsAttemptAndClosesSource(t, checkpointFailureTestMode{actor: true})
}

func TestFailedCreatingActorCheckpointMaxDurationExpiresRun(t *testing.T) {
	testFailedCreatingCheckpointFailsAttemptAndClosesSource(t, checkpointFailureTestMode{actor: true, maxDuration: true})
}

type checkpointFailureTestMode struct {
	retry       bool
	actor       bool
	maxDuration bool
}

func testFailedCreatingCheckpointFailsAttemptAndClosesSource(t *testing.T, mode checkpointFailureTestMode) {
	t.Helper()
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	var actorID uuid.UUID
	if mode.actor {
		retryPolicy := `{"enabled":false}`
		if mode.retry {
			retryPolicy = `{"enabled":true,"maxAttempts":3,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}`
		}
		actorID = fixture.base.ConvertToActor(
			t,
			ctx,
			runtest.RunLease{LeaseID: work.leaseID, RunID: work.runID},
			retryPolicy,
		)
	}
	authority := startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	registration := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, uuid.Must(uuid.NewV7()))
	if mode.actor {
		registration.ActorSpeculativeInputSequence = pgtype.Int8{Int64: 2, Valid: true}
	}
	registered, err := reconciler.RegisterWait(ctx, registration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `UPDATE run_waits SET checkpoint_due_at = transaction_timestamp() WHERE id = $1`, registered.WaitID); err != nil {
		t.Fatal(err)
	}
	var baseVersionID uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `SELECT base_version_id FROM workspace_leases WHERE id = $1`, registration.WorkspaceLeaseID).Scan(&baseVersionID); err != nil {
		t.Fatal(err)
	}
	checkpointID := uuid.Must(uuid.NewV7())
	if _, err := fixture.queries.CreateRunCheckpoint(ctx, db.CreateRunCheckpointParams{
		ID: pgvalue.UUID(checkpointID), Kind: db.RunCheckpointKindSuspend,
		RunID: pgvalue.UUID(registration.RunID), AttemptNumber: registration.AttemptNumber,
		RunWaitID: pgvalue.UUID(registered.WaitID), SourceRunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID),
		SourceWorkspaceLeaseID: pgvalue.UUID(registration.WorkspaceLeaseID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
		BaseWorkspaceVersionID: pgvalue.UUID(baseVersionID), RestoreManifest: []byte(`{}`),
		ActorSpeculativeInputSequence: registration.ActorSpeculativeInputSequence,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.BeginRunLeaseCheckpoint(ctx, db.BeginRunLeaseCheckpointParams{
		ID: pgvalue.UUID(registration.CurrentRunLeaseID), RunID: pgvalue.UUID(registration.RunID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), AttemptNumber: registration.AttemptNumber,
		LeaseSequence: registration.LeaseSequence,
	}); err != nil {
		t.Fatal(err)
	}
	wait, err := fixture.queries.RequestRunWaitCheckpoint(ctx, db.RequestRunWaitCheckpointParams{
		SuspendCheckpointID: pgvalue.UUID(checkpointID), RunID: pgvalue.UUID(registration.RunID),
		AttemptNumber: registration.AttemptNumber, ID: pgvalue.UUID(registered.WaitID),
		CurrentRunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID),
	})
	if err != nil {
		t.Fatal(err)
	}
	failureFingerprint := tokenWaitTestDigest("checkpoint-failed-" + checkpointID.String())
	failureError := []byte(`{"code":"checkpoint_failed"}`)
	failedAt, err := fixture.queries.GetTaskCompletionTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.CloseRunActiveIntervalForCheckpointFailure(ctx, db.CloseRunActiveIntervalForCheckpointFailureParams{
		FailedAt: failedAt, ID: pgvalue.UUID(registration.RunID), OrgID: pgvalue.UUID(fixture.orgID),
		ProjectID: pgvalue.UUID(fixture.projectID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), AttemptNumber: registration.AttemptNumber,
		RunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.InvalidateFailedRunCheckpoint(ctx, db.InvalidateFailedRunCheckpointParams{
		FailedAt: failedAt, FailedRequestFingerprint: pgvalue.Text(failureFingerprint),
		CheckpointID: pgvalue.UUID(checkpointID), RunID: pgvalue.UUID(registration.RunID),
		AttemptNumber: registration.AttemptNumber, RunWaitID: pgvalue.UUID(registered.WaitID),
		RunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.FailCheckpointRunLease(ctx, db.FailCheckpointRunLeaseParams{
		FailedAt: failedAt, Error: failureError, FailedRequestFingerprint: pgvalue.Text(failureFingerprint),
		RunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID), RunID: pgvalue.UUID(registration.RunID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), AttemptNumber: registration.AttemptNumber,
		LeaseSequence: registration.LeaseSequence,
	}); err != nil {
		t.Fatal(err)
	}
	if mode.actor {
		if _, err := fixture.queries.CompleteActorAttempt(ctx, db.CompleteActorAttemptParams{
			TerminalActorInputSequence: pgtype.Int8{}, TerminalOutcome: pgvalue.Text("failed"),
			ReasonCode: pgvalue.Text("checkpoint_failed"), Error: failureError,
			CompletedAt: failedAt, RunID: pgvalue.UUID(registration.RunID), Number: registration.AttemptNumber,
			WorkspaceID: pgvalue.UUID(authority.workspaceID),
		}); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := fixture.queries.CompleteTaskAttempt(ctx, db.CompleteTaskAttemptParams{
			TerminalOutcome: pgvalue.Text("failed"), ReasonCode: pgvalue.Text("checkpoint_failed"), Error: failureError,
			CompletedAt: failedAt, RunID: pgvalue.UUID(registration.RunID), Number: registration.AttemptNumber,
			WorkspaceID: pgvalue.UUID(authority.workspaceID),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.queries.FailCheckpointRunWait(ctx, db.FailCheckpointRunWaitParams{
		CheckpointRequestVersion: wait.CheckpointRequestVersion, FailedAt: failedAt, Error: failureError,
		RunWaitID: pgvalue.UUID(registered.WaitID), RunID: pgvalue.UUID(registration.RunID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), AttemptNumber: registration.AttemptNumber,
		RunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID), CheckpointID: pgvalue.UUID(checkpointID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.ReleaseTaskWorkspaceLease(ctx, db.ReleaseTaskWorkspaceLeaseParams{
		CompletedAt: failedAt, ID: pgvalue.UUID(authority.workspaceLeaseID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), WorkspaceMountID: pgvalue.UUID(authority.mountID),
		RuntimeInstanceID: pgvalue.UUID(authority.runtimeID), OwnerRunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID),
		BaseVersionID: pgvalue.UUID(authority.physicalVersionID), OwnershipGeneration: 1,
		WriterGeneration: 1, MountFencingGeneration: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.RequestCheckpointFailureRuntimeClose(ctx, db.RequestCheckpointFailureRuntimeCloseParams{
		FailedAt: failedAt, WorkspaceMountID: pgvalue.UUID(authority.mountID), OrgID: pgvalue.UUID(fixture.orgID),
		ProjectID: pgvalue.UUID(fixture.projectID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: registration.WorkerEpoch, MountFencingGeneration: 2,
		RuntimeInstanceID: pgvalue.UUID(authority.runtimeID),
	}); err != nil {
		t.Fatal(err)
	}
	if mode.retry {
		if mode.actor {
			if _, err := fixture.queries.CreateActorCheckpointFailureRetryAttempt(ctx, db.CreateActorCheckpointFailureRetryAttemptParams{
				Number: registration.AttemptNumber + 1, ExpectedRunGeneration: 1,
				RunID: pgvalue.UUID(registration.RunID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
				PreviousAttemptNumber: registration.AttemptNumber, RunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.queries.DelayActorCheckpointFailureRetry(ctx, db.DelayActorCheckpointFailureRetryParams{
				NextAttemptNumber: registration.AttemptNumber + 1,
				RetryAt:           pgvalue.Timestamptz(failedAt.Time.Add(time.Second)), FailedAt: failedAt,
				ID: pgvalue.UUID(registration.RunID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
				ActorID: pgvalue.UUID(actorID), PreviousAttemptNumber: registration.AttemptNumber,
				RunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID),
			}); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := fixture.queries.CreateCheckpointFailureRetryAttempt(ctx, db.CreateCheckpointFailureRetryAttemptParams{
				Number: registration.AttemptNumber + 1, RunID: pgvalue.UUID(registration.RunID),
				WorkspaceID: pgvalue.UUID(authority.workspaceID), PreviousAttemptNumber: registration.AttemptNumber,
				RunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.queries.DelayCheckpointFailureRetry(ctx, db.DelayCheckpointFailureRetryParams{
				NextAttemptNumber: registration.AttemptNumber + 1,
				RetryAt:           pgvalue.Timestamptz(failedAt.Time.Add(time.Second)), FailedAt: failedAt,
				ID: pgvalue.UUID(registration.RunID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
				PreviousAttemptNumber: registration.AttemptNumber, RunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID),
			}); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		if mode.actor {
			runStatus := db.RunStatusSystemFailed
			reason := "checkpoint_failed"
			actorState := "failed"
			failureCode := pgvalue.Text("platform_failure")
			failureRunID := pgvalue.UUID(registration.RunID)
			if mode.maxDuration {
				runStatus = db.RunStatusExpired
				reason = "max_active_duration_exceeded"
				failureCode = pgvalue.Text("run_expired")
			}
			if _, err := fixture.queries.FinishCheckpointFailedActorRun(ctx, db.FinishCheckpointFailedActorRunParams{
				Status: runStatus, ReasonCode: pgvalue.Text(reason), Error: failureError, FailedAt: failedAt,
				ID: pgvalue.UUID(registration.RunID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
				ActorID: pgvalue.UUID(actorID), AttemptNumber: registration.AttemptNumber,
				RunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.queries.ReconcileActorTerminalRun(ctx, db.ReconcileActorTerminalRunParams{
				State: actorState, FailureCode: failureCode, FailureRunID: failureRunID, CompletedAt: failedAt,
				EnvironmentID: pgvalue.UUID(fixture.environmentID), ID: pgvalue.UUID(actorID),
				WorkspaceID: pgvalue.UUID(authority.workspaceID), RunID: pgvalue.UUID(registration.RunID),
				ExpectedRunGeneration: 1,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.queries.ReleaseActorWorkspaceOwner(ctx, db.ReleaseActorWorkspaceOwnerParams{
				CompletedAt: failedAt, ID: pgvalue.UUID(authority.workspaceID),
				EnvironmentID: pgvalue.UUID(fixture.environmentID),
				ActorID:       pgvalue.UUID(actorID), OwnershipGeneration: 1, WriterGeneration: 1,
			}); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := fixture.queries.ReleaseTaskWorkspaceOwner(ctx, db.ReleaseTaskWorkspaceOwnerParams{
				CompletedAt: failedAt, ID: pgvalue.UUID(authority.workspaceID), OrgID: pgvalue.UUID(fixture.orgID),
				ProjectID: pgvalue.UUID(fixture.projectID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
				RunID: pgvalue.UUID(registration.RunID), OwnershipGeneration: 1, WriterGeneration: 1,
				ExpectedHeadVersionID: pgvalue.UUID(authority.baseVersionID),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.queries.FinishCheckpointFailedTaskRun(ctx, db.FinishCheckpointFailedTaskRunParams{
				Status: db.RunStatusSystemFailed, ReasonCode: pgvalue.Text("checkpoint_failed"), Error: failureError,
				FailedAt: failedAt, ID: pgvalue.UUID(registration.RunID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
				AttemptNumber: registration.AttemptNumber, RunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	replay, err := fixture.queries.GetCheckpointFailedReplay(ctx, pgvalue.UUID(checkpointID))
	if err != nil {
		t.Fatal(err)
	}
	if replay.RunID != pgvalue.UUID(registration.RunID) || replay.RunWaitID != pgvalue.UUID(registered.WaitID) ||
		replay.SourceRunLeaseID != pgvalue.UUID(registration.CurrentRunLeaseID) ||
		!replay.FailedRequestFingerprint.Valid || replay.FailedRequestFingerprint.String != failureFingerprint {
		t.Fatalf("failed checkpoint replay = %+v", replay)
	}
	var runStatus db.RunStatus
	var leaseState db.RunLeaseState
	var condition db.WaitState
	var suspension db.RunWaitState
	var checkpointState db.RunCheckpointState
	var attemptOutcome pgtype.Text
	var workspaceLeaseState db.WorkspaceLeaseState
	var runtimeDesired db.RuntimeDesiredState
	var mountState db.WorkspaceMountState
	var ownerRunID pgtype.UUID
	var currentLeaseID pgtype.UUID
	var activeStartedAt pgtype.Timestamptz
	var currentAttemptNumber int32
	var retryAt pgtype.Timestamptz
	var nextAttemptCount int
	if err := fixture.pool.QueryRow(ctx, `
SELECT runs.status, run_leases.state, run_waits.condition_state, run_waits.suspension_state, run_checkpoints.state,
       run_attempts.terminal_outcome, workspace_leases.state, runtime_instances.desired_state,
       workspace_mounts.state, workspaces.owner_run_id, runs.current_run_lease_id, runs.active_started_at,
       runs.current_attempt_number, runs.retry_at,
       (SELECT count(*) FROM run_attempts AS next_attempt WHERE next_attempt.run_id = runs.id AND next_attempt.number = 2)
  FROM runs
  JOIN run_leases ON run_leases.id = $2
  JOIN run_waits ON run_waits.id = $3
  JOIN run_checkpoints ON run_checkpoints.id = $4
  JOIN run_attempts ON run_attempts.run_id = runs.id AND run_attempts.number = 1
  JOIN workspace_leases ON workspace_leases.id = $5
  JOIN runtime_instances ON runtime_instances.id = $6
  JOIN workspace_mounts ON workspace_mounts.id = $7
  JOIN workspaces ON workspaces.id = runs.workspace_id
 WHERE runs.id = $1`, registration.RunID, registration.CurrentRunLeaseID, registered.WaitID, checkpointID,
		authority.workspaceLeaseID, authority.runtimeID, authority.mountID,
	).Scan(&runStatus, &leaseState, &condition, &suspension, &checkpointState, &attemptOutcome,
		&workspaceLeaseState, &runtimeDesired, &mountState, &ownerRunID, &currentLeaseID, &activeStartedAt,
		&currentAttemptNumber, &retryAt, &nextAttemptCount); err != nil {
		t.Fatal(err)
	}
	expectedStatus := db.RunStatusSystemFailed
	if mode.retry {
		expectedStatus = db.RunStatusRetryDelayed
	} else if mode.maxDuration {
		expectedStatus = db.RunStatusExpired
	}
	if runStatus != expectedStatus || leaseState != db.RunLeaseStateFailed || condition != db.WaitStateCancelled ||
		suspension != db.RunWaitStateFailed || checkpointState != db.RunCheckpointStateInvalid ||
		!attemptOutcome.Valid || attemptOutcome.String != "failed" || workspaceLeaseState != db.WorkspaceLeaseStateReleased ||
		runtimeDesired != db.RuntimeDesiredStateClosed || mountState != db.WorkspaceMountStateUnmounting ||
		currentLeaseID.Valid || activeStartedAt.Valid ||
		(mode.retry && (currentAttemptNumber != 2 || !retryAt.Valid || nextAttemptCount != 1)) ||
		(!mode.retry && (currentAttemptNumber != 1 || retryAt.Valid || nextAttemptCount != 0)) ||
		(!mode.actor && mode.retry && !ownerRunID.Valid) || (!mode.actor && !mode.retry && ownerRunID.Valid) {
		t.Fatalf("failed checkpoint state = mode=%+v run=%s lease=%s condition=%s suspension=%s checkpoint=%s attempt=%v workspace_lease=%s runtime=%s mount=%s owner=%v current_lease=%v active=%v current_attempt=%d retry_at=%v next_attempts=%d",
			mode,
			runStatus, leaseState, condition, suspension, checkpointState, attemptOutcome, workspaceLeaseState,
			runtimeDesired, mountState, ownerRunID, currentLeaseID, activeStartedAt, currentAttemptNumber, retryAt, nextAttemptCount)
	}
	if mode.actor {
		var actorState string
		var actorCurrentRunID, failureRunID, ownerActorID pgtype.UUID
		var failureCode pgtype.Text
		var runGeneration int64
		var nextStart, nextHigh pgtype.Int8
		var nextBase, workspaceHead pgtype.UUID
		if err := fixture.pool.QueryRow(ctx, `
SELECT actors.state, actors.current_run_id, actors.run_generation,
       actors.failure_code, actors.failure_run_id, workspaces.owner_actor_id,
       next_attempt.actor_start_input_sequence, runs.actor_start_input_high_watermark,
       next_attempt.base_workspace_version_id, workspaces.head_version_id
  FROM actors
  JOIN workspaces ON workspaces.id = actors.workspace_id
  JOIN runs ON runs.id = $2 AND runs.actor_id = actors.id
  LEFT JOIN run_attempts AS next_attempt
    ON next_attempt.run_id = $2 AND next_attempt.number = 2
 WHERE actors.id = $1`, actorID, registration.RunID).Scan(
			&actorState, &actorCurrentRunID, &runGeneration, &failureCode, &failureRunID, &ownerActorID,
			&nextStart, &nextHigh, &nextBase, &workspaceHead,
		); err != nil {
			t.Fatal(err)
		}
		if mode.retry {
			if actorState != "open" || actorCurrentRunID != pgvalue.UUID(registration.RunID) || runGeneration != 1 ||
				failureCode.Valid || failureRunID.Valid || ownerActorID != pgvalue.UUID(actorID) ||
				!nextStart.Valid || nextStart.Int64 != 1 || !nextHigh.Valid || nextHigh.Int64 != 2 || nextBase != workspaceHead {
				t.Fatalf("Actor retry state = state=%s current=%v generation=%d failure=%v/%v owner=%v next=%v/%v base=%v head=%v",
					actorState, actorCurrentRunID, runGeneration, failureCode, failureRunID, ownerActorID,
					nextStart, nextHigh, nextBase, workspaceHead)
			}
		} else {
			wantState := "failed"
			wantFailure := "platform_failure"
			if mode.maxDuration {
				wantFailure = "run_expired"
			}
			if actorState != wantState || actorCurrentRunID.Valid || runGeneration != 2 || ownerActorID.Valid ||
				failureCode.String != wantFailure || failureCode.Valid != (wantFailure != "") ||
				failureRunID.Valid != (wantFailure != "") {
				t.Fatalf("terminal Actor state = state=%s current=%v generation=%d failure=%v/%v owner=%v",
					actorState, actorCurrentRunID, runGeneration, failureCode, failureRunID, ownerActorID)
			}
		}
	}
}

func TestPendingRootTokenWaitCheckpointReadyCommitsAtomicParkingFacts(t *testing.T) {
	testPendingRootTokenWaitCheckpointReadyCommitsAtomicParkingFacts(t, false)
}

func TestPendingRootActorTokenWaitCheckpointReadyCommitsAtomicParkingFacts(t *testing.T) {
	testPendingRootTokenWaitCheckpointReadyCommitsAtomicParkingFacts(t, true)
}

func testPendingRootTokenWaitCheckpointReadyCommitsAtomicParkingFacts(t *testing.T, actor bool) {
	t.Helper()
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	if actor {
		fixture.base.ConvertToActor(
			t,
			ctx,
			runtest.RunLease{LeaseID: work.leaseID, RunID: work.runID},
			`{"enabled":false}`,
		)
	}
	authority := startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	registration := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, uuid.Must(uuid.NewV7()))
	if actor {
		registration.ActorSpeculativeInputSequence = pgtype.Int8{Int64: 1, Valid: true}
	}
	registered, err := reconciler.RegisterWait(ctx, registration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `UPDATE run_waits SET checkpoint_due_at = transaction_timestamp() WHERE id = $1`, registered.WaitID); err != nil {
		t.Fatal(err)
	}
	checkpointID := uuid.Must(uuid.NewV7())
	privateVersionID := uuid.Must(uuid.NewV7())
	workspaceArtifactID := uuid.Must(uuid.NewV7())
	workspaceDigest := tokenWaitTestDigest("checkpoint-workspace-artifact-" + checkpointID.String())
	workspaceTreeDigest := tokenWaitTestDigest("checkpoint-workspace-tree-" + checkpointID.String())
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	queries := db.New(tx)
	if _, err := queries.CreateRunCheckpoint(ctx, db.CreateRunCheckpointParams{
		ID: pgvalue.UUID(checkpointID), Kind: db.RunCheckpointKindSuspend,
		RunID: pgvalue.UUID(registration.RunID), AttemptNumber: registration.AttemptNumber,
		RunWaitID: pgvalue.UUID(registered.WaitID), SourceRunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID),
		SourceWorkspaceLeaseID: pgvalue.UUID(registration.WorkspaceLeaseID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
		BaseWorkspaceVersionID: pgvalue.UUID(authority.physicalVersionID), RestoreManifest: []byte(`{}`),
		ActorSpeculativeInputSequence: registration.ActorSpeculativeInputSequence,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.BeginRunLeaseCheckpoint(ctx, db.BeginRunLeaseCheckpointParams{
		ID: pgvalue.UUID(registration.CurrentRunLeaseID), RunID: pgvalue.UUID(registration.RunID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), AttemptNumber: registration.AttemptNumber,
		LeaseSequence: registration.LeaseSequence,
	}); err != nil {
		t.Fatal(err)
	}
	wait, err := queries.RequestRunWaitCheckpoint(ctx, db.RequestRunWaitCheckpointParams{
		SuspendCheckpointID: pgvalue.UUID(checkpointID), RunID: pgvalue.UUID(registration.RunID),
		AttemptNumber: registration.AttemptNumber, ID: pgvalue.UUID(registered.WaitID),
		CurrentRunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		OrgID: pgvalue.UUID(fixture.orgID), Digest: workspaceDigest, SizeBytes: 10,
		MediaType: "application/vnd.helmr.workspace.v0.tar",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: pgvalue.UUID(workspaceArtifactID), OrgID: pgvalue.UUID(fixture.orgID),
		ProjectID: pgvalue.UUID(fixture.projectID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		Digest: workspaceDigest, Kind: db.ArtifactKindWorkspaceVersion, SizeBytes: 10,
		MediaType: "application/vnd.helmr.workspace.v0.tar",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreatePrivateCheckpointWorkspaceVersion(ctx, db.CreatePrivateCheckpointWorkspaceVersionParams{
		ID:            pgvalue.UUID(privateVersionID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
		ParentVersionID: pgvalue.UUID(authority.physicalVersionID), ArtifactID: pgvalue.UUID(workspaceArtifactID),
		ContentDigest: workspaceTreeDigest, SizeBytes: 10, EntryCount: 1,
		SourceWorkspaceLeaseID: pgvalue.UUID(authority.workspaceLeaseID), OwnershipGeneration: 1, WriterGeneration: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkRunCheckpointReady(ctx, db.MarkRunCheckpointReadyParams{
		PrivateWorkspaceVersionID: pgvalue.UUID(privateVersionID),
		RestoreManifest:           []byte(`{"recovery_point":{"runtime":{"backend":"firecracker"}}}`),
		ReadyRequestFingerprint:   pgvalue.Text(tokenWaitTestDigest("checkpoint-ready-" + checkpointID.String())),
		RunID:                     pgvalue.UUID(registration.RunID), AttemptNumber: registration.AttemptNumber, ID: pgvalue.UUID(checkpointID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CloseRunActiveIntervalForCheckpoint(ctx, db.CloseRunActiveIntervalForCheckpointParams{
		ID: pgvalue.UUID(registration.RunID), OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
		AttemptNumber: registration.AttemptNumber, RunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID),
	}); err != nil {
		t.Fatal(err)
	}
	checkpointedAt := pgvalue.Timestamptz(time.Now().UTC())
	if _, err := queries.UpdateTaskWorkspaceMountFrontier(ctx, db.UpdateTaskWorkspaceMountFrontierParams{
		NewVersionID: pgvalue.UUID(privateVersionID), CompletedAt: checkpointedAt,
		ID: pgvalue.UUID(authority.mountID), OrgID: pgvalue.UUID(fixture.orgID),
		ProjectID: pgvalue.UUID(fixture.projectID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), RuntimeInstanceID: pgvalue.UUID(authority.runtimeID),
		BaseVersionID: pgvalue.UUID(authority.physicalVersionID), MountFencingGeneration: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CheckpointRunLease(ctx, db.CheckpointRunLeaseParams{
		CheckpointedAt: checkpointedAt, ID: pgvalue.UUID(registration.CurrentRunLeaseID),
		RunID: pgvalue.UUID(registration.RunID), WorkspaceID: pgvalue.UUID(authority.workspaceID),
		AttemptNumber: registration.AttemptNumber, LeaseSequence: registration.LeaseSequence,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReleaseCheckpointWorkspaceLease(ctx, db.ReleaseCheckpointWorkspaceLeaseParams{
		CheckpointedAt: checkpointedAt, ID: pgvalue.UUID(authority.workspaceLeaseID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), WorkspaceMountID: pgvalue.UUID(authority.mountID),
		RuntimeInstanceID: pgvalue.UUID(authority.runtimeID), OwnerRunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID),
		BaseVersionID: pgvalue.UUID(authority.physicalVersionID), OwnershipGeneration: 1,
		WriterGeneration: 1, MountFencingGeneration: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CommitPendingCheckpointReady(ctx, db.CommitPendingCheckpointReadyParams{
		CheckpointedAt: checkpointedAt, RunID: pgvalue.UUID(registration.RunID),
		WorkspaceID: pgvalue.UUID(authority.workspaceID), AttemptNumber: registration.AttemptNumber,
		RunLeaseID: pgvalue.UUID(registration.CurrentRunLeaseID), ExpectedRunStateVersion: wait.ExpectedRunStateVersion,
		CheckpointRequestVersion: wait.CheckpointRequestVersion, RunWaitID: pgvalue.UUID(registered.WaitID),
		CheckpointID: pgvalue.UUID(checkpointID),
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var runStatus db.RunStatus
	var currentLease pgtype.UUID
	var leaseState db.RunLeaseState
	var workspaceLeaseState db.WorkspaceLeaseState
	var suspension db.RunWaitState
	var priorLease pgtype.UUID
	var checkpointState db.RunCheckpointState
	var mountVersion uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `
SELECT runs.status, runs.current_run_lease_id, run_leases.state, workspace_leases.state,
       run_waits.suspension_state, run_waits.prior_run_lease_id, run_checkpoints.state,
       workspace_mounts.materialized_version_id
  FROM runs
  JOIN run_leases ON run_leases.id = $2
  JOIN workspace_leases ON workspace_leases.id = $3
  JOIN run_waits ON run_waits.id = $4
  JOIN run_checkpoints ON run_checkpoints.id = $5
  JOIN workspace_mounts ON workspace_mounts.id = $6
 WHERE runs.id = $1`, registration.RunID, registration.CurrentRunLeaseID, authority.workspaceLeaseID,
		registered.WaitID, checkpointID, authority.mountID,
	).Scan(&runStatus, &currentLease, &leaseState, &workspaceLeaseState, &suspension, &priorLease, &checkpointState, &mountVersion); err != nil {
		t.Fatal(err)
	}
	if runStatus != db.RunStatusWaiting || currentLease.Valid || leaseState != db.RunLeaseStateCheckpointed ||
		workspaceLeaseState != db.WorkspaceLeaseStateReleased || suspension != db.RunWaitStateParked ||
		!priorLease.Valid || uuid.UUID(priorLease.Bytes) != registration.CurrentRunLeaseID ||
		checkpointState != db.RunCheckpointStateReady || mountVersion != privateVersionID {
		t.Fatalf("ready checkpoint state = run=%s/%v lease=%s workspace_lease=%s wait=%s/%v checkpoint=%s mount=%s",
			runStatus, currentLease, leaseState, workspaceLeaseState, suspension, priorLease, checkpointState, mountVersion)
	}
	if actor {
		committedAt := pgvalue.Timestamptz(time.Now().UTC())
		if _, err := fixture.queries.InvalidateRestoredActorCheckpoint(ctx, db.InvalidateRestoredActorCheckpointParams{
			CommittedAt: committedAt, RestoreCheckpointID: pgvalue.UUID(checkpointID),
			RunID: pgvalue.UUID(registration.RunID), AttemptNumber: registration.AttemptNumber,
			WorkspaceID: pgvalue.UUID(authority.workspaceID), PrivateWorkspaceVersionID: pgvalue.UUID(privateVersionID),
			TargetInputSequence: 2,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.queries.PublishRestoredActorCheckpointWorkspaceVersion(
			ctx, db.PublishRestoredActorCheckpointWorkspaceVersionParams{
				CommittedAt: committedAt, VersionID: pgvalue.UUID(privateVersionID),
				WorkspaceID: pgvalue.UUID(authority.workspaceID), ExpectedParentVersionID: pgvalue.UUID(authority.physicalVersionID),
				OwnershipGeneration: 1, WriterGeneration: 1, RestoreCheckpointID: pgvalue.UUID(checkpointID),
				RunID: pgvalue.UUID(registration.RunID), AttemptNumber: registration.AttemptNumber,
			},
		); err != nil {
			t.Fatal(err)
		}
		var consumedState db.RunCheckpointState
		var versionState db.WorkspaceVersionState
		if err := fixture.pool.QueryRow(ctx, `
SELECT run_checkpoints.state, workspace_versions.state
  FROM run_checkpoints
  JOIN workspace_versions ON workspace_versions.id = run_checkpoints.private_workspace_version_id
 WHERE run_checkpoints.id = $1`, checkpointID).Scan(&consumedState, &versionState); err != nil {
			t.Fatal(err)
		}
		if consumedState != db.RunCheckpointStateInvalid || versionState != db.WorkspaceVersionStateCommitted {
			t.Fatalf("consumed restored checkpoint = checkpoint %s version %s", consumedState, versionState)
		}
	}
}

func TestTokenWaitRegistrationConcurrentReplayConverges(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	request := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, uuid.Must(uuid.NewV7()))
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	type registrationOutcome struct {
		result WaitRegistrationResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan registrationOutcome, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			result, err := reconciler.RegisterWait(ctx, request)
			outcomes <- registrationOutcome{result: result, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(outcomes)
	for outcome := range outcomes {
		if outcome.err != nil || outcome.result.WaitID != request.WaitID ||
			outcome.result.ConditionState != db.WaitStatePending ||
			outcome.result.SuspensionState != db.RunWaitStateHot {
			t.Fatalf("concurrent registration = %+v, %v", outcome.result, outcome.err)
		}
	}
	var waitCount int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM run_waits WHERE id = $1`, request.WaitID).Scan(&waitCount); err != nil {
		t.Fatal(err)
	}
	if waitCount != 1 {
		t.Fatalf("concurrent registration created %d Waits", waitCount)
	}
}

func TestTokenWaitRegistrationReplaySurvivesParkedCompletion(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	request := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, uuid.Must(uuid.NewV7()))
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := reconciler.RegisterWait(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	var workspaceID, workspaceLeaseID, baseVersionID uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.workspace_id, workspace_leases.id, runs.base_workspace_version_id
		  FROM runs
		  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = runs.current_run_lease_id
		 WHERE runs.id = $1
	`, work.runID).Scan(&workspaceID, &workspaceLeaseID, &baseVersionID); err != nil {
		t.Fatal(err)
	}
	checkpointID := uuid.Must(uuid.NewV7())
	mustRunLeaseExec(t, ctx, fixture.pool, `
		INSERT INTO run_checkpoints (
		    id, kind, run_id, attempt_number, run_wait_id,
		    source_run_lease_id, source_workspace_lease_id, workspace_id,
		    base_workspace_version_id, private_workspace_version_id,
		    state, restore_manifest, ready_request_fingerprint, ready_at
		) VALUES (
		    $1, 'suspend', $2, 1, $3, $4, $5, $6, $7, $7,
		    'ready', '{"test":true}'::jsonb, 'sha256:test-ready', transaction_timestamp()
		)
	`, checkpointID, work.runID, request.WaitID, work.leaseID, workspaceLeaseID, workspaceID, baseVersionID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET state = 'checkpointed', checkpointed_at = transaction_timestamp(),
		       terminal_at = transaction_timestamp(), terminal_reason_code = 'checkpointed'
		 WHERE id = $1
	`, work.leaseID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE workspace_leases
		   SET state = 'released', released_at = transaction_timestamp(), terminal_at = transaction_timestamp()
		 WHERE id = $1
	`, workspaceLeaseID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE runs SET current_run_lease_id = NULL, active_started_at = NULL WHERE id = $1
	`, work.runID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE run_waits
		   SET suspension_state = 'parked', current_run_lease_id = NULL,
		       prior_run_lease_id = $1, suspend_checkpoint_id = $2
		 WHERE id = $3
	`, work.leaseID, checkpointID, request.WaitID)
	if _, err := fixture.queries.CancelToken(ctx, tokenCancellationParams(fixture, tokenID)); err != nil {
		t.Fatal(err)
	}
	batch, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, tokenID, 100)
	if err != nil || batch.Resolved != 1 {
		t.Fatalf("parked completion = %+v, %v", batch, err)
	}
	replayed, err := reconciler.RegisterWait(ctx, request)
	if err != nil || replayed.WaitID != request.WaitID || replayed.ConditionState != db.WaitStateCancelled ||
		replayed.SuspensionState != db.RunWaitStateResumePending ||
		replayed.RunStateVersion != registered.RunStateVersion+1 {
		t.Fatalf("parked registration replay = %+v, %v; first = %+v", replayed, err, registered)
	}
	recomputed := request
	recomputed.TimeoutAt = pgvalue.Timestamptz(time.Now().Add(10 * time.Minute))
	recomputed.CheckpointDueAt = pgvalue.Timestamptz(time.Now().Add(time.Minute))
	if replayed, err := reconciler.RegisterWait(ctx, recomputed); err != nil || replayed.WaitID != request.WaitID {
		t.Fatalf("recomputed-deadline registration replay = %+v, %v", replayed, err)
	}
	changed := request
	changed.RequestFingerprint = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := reconciler.RegisterWait(ctx, changed); !errors.Is(err, ErrWaitAuthority) {
		t.Fatalf("changed registration replay error = %v", err)
	}
}

func TestTokenWaitRegistrationAllowsDrainingInFlightWorker(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	request := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, uuid.Must(uuid.NewV7()))
	mustRunLeaseExec(t, ctx, fixture.pool, `UPDATE worker_groups SET state = 'draining' WHERE id = $1`, request.WorkerGroupID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE worker_instances SET state = 'draining', draining_at = transaction_timestamp() WHERE id = $1
	`, request.WorkerInstanceID)
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := reconciler.RegisterWait(ctx, request)
	if err != nil || registered.WaitID != request.WaitID || registered.ConditionState != db.WaitStatePending {
		t.Fatalf("draining worker registration = %+v, %v", registered, err)
	}
}

func TestTokenWaitRegistrationRejectsExpiredPhysicalAuthority(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	waitID := uuid.Must(uuid.NewV7())
	request := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, waitID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET start_deadline_at = transaction_timestamp() - interval '2 seconds',
		       expires_at = transaction_timestamp() - interval '1 second'
		 WHERE id = $1
	`, work.leaseID)
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.RegisterWait(ctx, request); !errors.Is(err, ErrWaitAuthority) {
		t.Fatalf("expired registration error = %v", err)
	}
	var status db.RunStatus
	var waitCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT status, (SELECT count(*) FROM run_waits WHERE id = $2)
		  FROM runs WHERE id = $1
	`, work.runID, waitID).Scan(&status, &waitCount); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusRunning || waitCount != 0 {
		t.Fatalf("expired authority mutated run=%s waits=%d", status, waitCount)
	}
}

func TestTokenWaitRegistrationAcceptsChildRun(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	parent := fixture.addWork(t, ctx, "starting", time.Now().Add(-2*time.Minute))
	child := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, child)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE runs
		   SET cause_kind = 'child', parent_run_id = $1, parent_owns_lifecycle = false
		 WHERE id = $2
	`, parent.runID, child.runID)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	waitID := uuid.Must(uuid.NewV7())
	request := tokenWaitRegistrationRequest(t, ctx, fixture, child, tokenID, waitID)
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := reconciler.RegisterWait(ctx, request)
	if err != nil {
		t.Fatalf("register child Run Wait: %v", err)
	}
	var count int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM run_waits WHERE id = $1`, waitID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if registered.WaitID != waitID || registered.ConditionState != db.WaitStatePending || count != 1 {
		t.Fatalf("child registration = %+v, waits=%d", registered, count)
	}
}

func tokenWaitRegistrationRequest(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	work runLeaseWork,
	tokenID uuid.UUID,
	waitID uuid.UUID,
) WaitRegistration {
	t.Helper()
	request := WaitRegistration{
		EnvironmentID: fixture.environmentID, RunID: work.runID, TokenID: tokenID,
		WaitID: waitID, ResumeAttachID: uuid.Must(uuid.NewV7()), AttemptNumber: 1,
		CurrentRunLeaseID:  work.leaseID,
		RequestFingerprint: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.state_version,
		       run_leases.lease_sequence, run_leases.worker_group_id,
		       run_leases.worker_instance_id, run_leases.worker_epoch,
		       run_leases.worker_protocol_version, run_leases.runtime_instance_id,
		       run_leases.runtime_identity_id, run_leases.region_id,
		       run_leases.network_slot_id, run_leases.network_slot_generation,
		       workspace_leases.workspace_mount_id, workspace_leases.id,
		       workspace_leases.ownership_generation, workspace_leases.writer_generation,
		       workspace_leases.mount_fencing_generation
		  FROM runs
		  JOIN run_leases ON run_leases.id = runs.current_run_lease_id
		  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
		 WHERE runs.id = $1
	`, work.runID).Scan(
		&request.ExpectedRunStateVersion,
		&request.LeaseSequence, &request.WorkerGroupID,
		&request.WorkerInstanceID, &request.WorkerEpoch,
		&request.WorkerProtocolVersion, &request.RuntimeInstanceID,
		&request.RuntimeIdentityID, &request.RegionID,
		&request.NetworkSlotID, &request.NetworkSlotGeneration,
		&request.WorkspaceMountID, &request.WorkspaceLeaseID,
		&request.OwnershipGeneration, &request.WriterGeneration,
		&request.MountFencingGeneration,
	); err != nil {
		t.Fatal(err)
	}
	return request
}

func TestTokenWaitReconcilerTransitionsHotCheckpointingAndParkedWaits(t *testing.T) {
	ctx := context.Background()

	t.Run("hot completion releases without Lease churn", func(t *testing.T) {
		fixture := newRunLeaseClaimFixture(t, ctx)
		setup := newTokenWaitReconcileSetup(t, ctx, fixture, db.RunWaitStateHot, time.Now().Add(time.Hour))
		if _, err := fixture.queries.CompleteToken(ctx, tokenCompletionParams(
			fixture,
			setup.tokenID,
			"sha256:hot",
			`{"approved":true}`,
		)); err != nil {
			t.Fatal(err)
		}

		batch := reconcileTokenWaitBatch(t, ctx, fixture, setup.tokenID)
		if batch.Examined != 1 || batch.Resolved != 1 || batch.Deferred != 0 {
			t.Fatalf("batch = %+v", batch)
		}
		assertTokenWaitReconcileState(t, ctx, fixture, setup, tokenWaitReconcileWant{
			runStatus: db.RunStatusRunning, runVersion: 3,
			conditionState: db.WaitStateCompleted, suspensionState: db.RunWaitStateReleased,
			currentLeaseID: pgvalue.UUID(setup.leaseID), priorLeaseID: pgtype.UUID{},
			result: `{"approved": true}`, reasonCode: "", resumeVersion: 0,
		})
		assertTokenWaitResumeIntents(t, ctx, fixture, setup, 0)

		batch = reconcileTokenWaitBatch(t, ctx, fixture, setup.tokenID)
		if batch.Examined != 0 || batch.Resolved != 0 {
			t.Fatalf("replay batch = %+v", batch)
		}
	})

	t.Run("checkpointing expiry records only terminal condition", func(t *testing.T) {
		fixture := newRunLeaseClaimFixture(t, ctx)
		setup := newTokenWaitReconcileSetup(t, ctx, fixture, db.RunWaitStateCheckpointing, time.Now().Add(-time.Minute))
		expired, err := fixture.queries.ExpireDueTokens(ctx, db.ExpireDueTokensParams{
			OutboxMessageIds: pgvalue.NewUUIDv7Batch(100),
			LimitCount:       100,
		})
		if err != nil || len(expired) != 1 {
			t.Fatalf("expire Token = rows %d error %v", len(expired), err)
		}

		batch := reconcileTokenWaitBatch(t, ctx, fixture, setup.tokenID)
		if batch.Examined != 1 || batch.Resolved != 1 || batch.Deferred != 1 {
			t.Fatalf("batch = %+v", batch)
		}
		assertTokenWaitReconcileState(t, ctx, fixture, setup, tokenWaitReconcileWant{
			runStatus: db.RunStatusWaiting, runVersion: 2,
			conditionState: db.WaitStateFailed, suspensionState: db.RunWaitStateCheckpointing,
			currentLeaseID: pgvalue.UUID(setup.leaseID), priorLeaseID: pgtype.UUID{},
			reasonCode: "token_expired", resumeVersion: 0,
		})
		assertTokenWaitResumeIntents(t, ctx, fixture, setup, 0)

		batch = reconcileTokenWaitBatch(t, ctx, fixture, setup.tokenID)
		if batch.Examined != 1 || batch.Resolved != 0 || batch.Deferred != 1 {
			t.Fatalf("deferred replay batch = %+v", batch)
		}
	})

	t.Run("parked cancellation queues exactly one resume", func(t *testing.T) {
		fixture := newRunLeaseClaimFixture(t, ctx)
		setup := newTokenWaitReconcileSetup(t, ctx, fixture, db.RunWaitStateParked, time.Now().Add(time.Hour))
		if _, err := fixture.queries.CancelToken(ctx, tokenCancellationParams(fixture, setup.tokenID)); err != nil {
			t.Fatal(err)
		}

		batch := reconcileTokenWaitBatch(t, ctx, fixture, setup.tokenID)
		if batch.Examined != 1 || batch.Resolved != 1 {
			t.Fatalf("batch = %+v", batch)
		}
		assertTokenWaitReconcileState(t, ctx, fixture, setup, tokenWaitReconcileWant{
			runStatus: db.RunStatusQueued, runVersion: 3,
			conditionState: db.WaitStateCancelled, suspensionState: db.RunWaitStateResumePending,
			currentLeaseID: pgtype.UUID{}, priorLeaseID: pgvalue.UUID(setup.leaseID),
			reasonCode: "token_cancelled", resumeVersion: 1,
		})
		assertTokenWaitResumeIntents(t, ctx, fixture, setup, 1)

		batch = reconcileTokenWaitBatch(t, ctx, fixture, setup.tokenID)
		if batch.Examined != 0 || batch.Resolved != 0 {
			t.Fatalf("replay batch = %+v", batch)
		}
		assertTokenWaitResumeIntents(t, ctx, fixture, setup, 1)
	})
}

func TestTokenWaitReconcilerAppliesWaitTimeoutAcrossSuspensionStates(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name              string
		suspension        db.RunWaitState
		runStatus         db.RunStatus
		runVersion        int64
		resultState       db.RunWaitState
		currentLeaseID    func(tokenWaitReconcileSetup) pgtype.UUID
		priorLeaseID      func(tokenWaitReconcileSetup) pgtype.UUID
		resumeVersion     int64
		resumeIntentCount int
	}{
		{
			name: "hot", suspension: db.RunWaitStateHot,
			runStatus: db.RunStatusRunning, runVersion: 3,
			resultState: db.RunWaitStateReleased,
			currentLeaseID: func(setup tokenWaitReconcileSetup) pgtype.UUID {
				return pgvalue.UUID(setup.leaseID)
			},
			priorLeaseID: func(tokenWaitReconcileSetup) pgtype.UUID { return pgtype.UUID{} },
		},
		{
			name: "checkpointing", suspension: db.RunWaitStateCheckpointing,
			runStatus: db.RunStatusWaiting, runVersion: 2,
			resultState: db.RunWaitStateCheckpointing,
			currentLeaseID: func(setup tokenWaitReconcileSetup) pgtype.UUID {
				return pgvalue.UUID(setup.leaseID)
			},
			priorLeaseID: func(tokenWaitReconcileSetup) pgtype.UUID { return pgtype.UUID{} },
		},
		{
			name: "parked", suspension: db.RunWaitStateParked,
			runStatus: db.RunStatusQueued, runVersion: 3,
			resultState:    db.RunWaitStateResumePending,
			currentLeaseID: func(tokenWaitReconcileSetup) pgtype.UUID { return pgtype.UUID{} },
			priorLeaseID: func(setup tokenWaitReconcileSetup) pgtype.UUID {
				return pgvalue.UUID(setup.leaseID)
			},
			resumeVersion: 1, resumeIntentCount: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRunLeaseClaimFixture(t, ctx)
			setup := newTokenWaitReconcileSetup(
				t, ctx, fixture, test.suspension, time.Now().Add(time.Hour),
			)
			mustRunLeaseExec(t, ctx, fixture.pool, `
				UPDATE run_waits
				   SET timeout_at = transaction_timestamp() - interval '1 millisecond'
				 WHERE id = $1
			`, setup.waitID)
			reconciler, err := NewWaitReconciler(fixture.pool)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := reconciler.ReconcileTimeouts(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			if resolved != 1 {
				t.Fatalf("resolved = %d", resolved)
			}
			assertTokenWaitReconcileState(t, ctx, fixture, setup, tokenWaitReconcileWant{
				runStatus: test.runStatus, runVersion: test.runVersion,
				conditionState: db.WaitStateFailed, suspensionState: test.resultState,
				currentLeaseID: test.currentLeaseID(setup),
				priorLeaseID:   test.priorLeaseID(setup),
				reasonCode:     "wait_timeout", resumeVersion: test.resumeVersion,
			})
			assertTokenWaitResumeIntents(t, ctx, fixture, setup, test.resumeIntentCount)
			resolved, err = reconciler.ReconcileTimeouts(ctx, 100)
			if err != nil || resolved != 0 {
				t.Fatalf("replay resolved = %d error=%v", resolved, err)
			}
		})
	}
}

func TestTokenWaitReconcilerRollsBackParkedTransitionWhenResumeIntentFails(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	setup := newTokenWaitReconcileSetup(t, ctx, fixture, db.RunWaitStateParked, time.Now().Add(time.Hour))
	if _, err := fixture.queries.CompleteToken(ctx, tokenCompletionParams(
		fixture,
		setup.tokenID,
		"sha256:rollback",
		`{"approved":true}`,
	)); err != nil {
		t.Fatal(err)
	}
	mustRunLeaseExec(t, ctx, fixture.pool, `
		CREATE FUNCTION reject_run_resume_intent() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.topic = 'run.resume' THEN
				RAISE EXCEPTION 'injected Run resume failure';
			END IF;
			RETURN NEW;
		END
		$$
	`)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		CREATE TRIGGER reject_run_resume_intent
		BEFORE INSERT ON outbox_messages
		FOR EACH ROW EXECUTE FUNCTION reject_run_resume_intent()
	`)

	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, setup.tokenID, 100); err == nil {
		t.Fatal("parked reconciliation succeeded despite injected outbox failure")
	}
	assertTokenWaitReconcileState(t, ctx, fixture, setup, tokenWaitReconcileWant{
		runStatus: db.RunStatusWaiting, runVersion: 2,
		conditionState: db.WaitStatePending, suspensionState: db.RunWaitStateParked,
		currentLeaseID: pgtype.UUID{}, priorLeaseID: pgvalue.UUID(setup.leaseID),
		resumeVersion: 0,
	})
	assertTokenWaitResumeIntents(t, ctx, fixture, setup, 0)
}

type tokenWaitReconcileSetup struct {
	tokenID      uuid.UUID
	waitID       uuid.UUID
	runID        uuid.UUID
	workspaceID  uuid.UUID
	leaseID      uuid.UUID
	checkpointID pgtype.UUID
}

func newTokenWaitReconcileSetup(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	suspension db.RunWaitState,
	tokenTimeout time.Time,
) tokenWaitReconcileSetup {
	t.Helper()
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	setup := tokenWaitReconcileSetup{
		tokenID: createTokenTerminalTestToken(t, ctx, fixture, tokenTimeout),
		waitID:  uuid.Must(uuid.NewV7()), runID: work.runID, leaseID: work.leaseID,
	}
	if err := fixture.pool.QueryRow(ctx, `SELECT workspace_id FROM runs WHERE id = $1`, work.runID).Scan(&setup.workspaceID); err != nil {
		t.Fatal(err)
	}
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET state = 'running',
		       started_at = claimed_at
		 WHERE id = $1
	`, setup.leaseID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE runs
		   SET status = 'waiting',
		       state_version = 2,
		       started_at = transaction_timestamp(),
		       active_started_at = transaction_timestamp()
		 WHERE id = $1
	`, setup.runID)
	insertTokenWaitFixture(t, ctx, fixture, setup.waitID, setup.runID, setup.workspaceID, setup.tokenID, setup.leaseID, 2)

	switch suspension {
	case db.RunWaitStateHot:
	case db.RunWaitStateCheckpointing:
		mustRunLeaseExec(t, ctx, fixture.pool, `
			UPDATE run_waits
			   SET suspension_state = 'checkpointing',
			       checkpoint_request_version = 1
			 WHERE id = $1
		`, setup.waitID)
	case db.RunWaitStateParked:
		var workspaceLeaseID, baseVersionID uuid.UUID
		if err := fixture.pool.QueryRow(ctx, `
			SELECT workspace_leases.id, runs.base_workspace_version_id
			  FROM workspace_leases
			  JOIN runs ON runs.id = $1
			 WHERE workspace_leases.owner_run_lease_id = $2
		`, setup.runID, setup.leaseID).Scan(&workspaceLeaseID, &baseVersionID); err != nil {
			t.Fatal(err)
		}
		checkpointID := uuid.Must(uuid.NewV7())
		setup.checkpointID = pgvalue.UUID(checkpointID)
		mustRunLeaseExec(t, ctx, fixture.pool, `
			INSERT INTO run_checkpoints (
			    id, kind, run_id, attempt_number, run_wait_id,
			    source_run_lease_id, source_workspace_lease_id, workspace_id,
			    base_workspace_version_id, private_workspace_version_id,
			    state, restore_manifest, ready_request_fingerprint, ready_at
			) VALUES (
			    $1, 'suspend', $2, 1, $3, $4, $5, $6, $7, $7,
			    'ready', '{"test":true}'::jsonb, 'sha256:test-ready', transaction_timestamp()
			)
		`, checkpointID, setup.runID, setup.waitID, setup.leaseID, workspaceLeaseID, setup.workspaceID, baseVersionID)
		mustRunLeaseExec(t, ctx, fixture.pool, `
			UPDATE run_leases
			   SET state = 'checkpointed',
			       checkpointed_at = transaction_timestamp(),
			       terminal_at = transaction_timestamp(),
			       terminal_reason_code = 'checkpointed'
			 WHERE id = $1
		`, setup.leaseID)
		mustRunLeaseExec(t, ctx, fixture.pool, `
			UPDATE workspace_leases
			   SET state = 'released',
			       released_at = transaction_timestamp(),
			       terminal_at = transaction_timestamp()
			 WHERE id = $1
		`, workspaceLeaseID)
		mustRunLeaseExec(t, ctx, fixture.pool, `
			UPDATE runs
			   SET current_run_lease_id = NULL,
			       active_started_at = NULL
			 WHERE id = $1
		`, setup.runID)
		mustRunLeaseExec(t, ctx, fixture.pool, `
			UPDATE run_waits
			   SET suspension_state = 'parked',
			       current_run_lease_id = NULL,
			       prior_run_lease_id = $1,
			       suspend_checkpoint_id = $2
			 WHERE id = $3
		`, setup.leaseID, checkpointID, setup.waitID)
	default:
		t.Fatalf("unsupported test suspension %s", suspension)
	}
	return setup
}

func insertTokenWaitFixture(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	waitID uuid.UUID,
	runID uuid.UUID,
	workspaceID uuid.UUID,
	tokenID uuid.UUID,
	leaseID uuid.UUID,
	expectedRunStateVersion int64,
) {
	t.Helper()
	mustRunLeaseExec(t, ctx, fixture.pool, `
		INSERT INTO run_waits (
			id, environment_id, run_id, workspace_id, kind, token_id,
			token_registration_run_state_version, expected_run_state_version,
			attempt_number, current_run_lease_id, resume_attach_id
		) VALUES ($1, $2, $3, $4, 'token', $5, $6 - 1, $6, 1, $7, $8)
	`, waitID, fixture.environmentID, runID, workspaceID, tokenID,
		expectedRunStateVersion, leaseID, uuid.Must(uuid.NewV7()))
}

func reconcileTokenWaitBatch(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	tokenID uuid.UUID,
) WaitBatch {
	t.Helper()
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, tokenID, 100)
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

type tokenWaitReconcileWant struct {
	runStatus       db.RunStatus
	runVersion      int64
	conditionState  db.WaitState
	suspensionState db.RunWaitState
	currentLeaseID  pgtype.UUID
	priorLeaseID    pgtype.UUID
	result          string
	reasonCode      string
	resumeVersion   int64
}

func assertTokenWaitReconcileState(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	setup tokenWaitReconcileSetup,
	want tokenWaitReconcileWant,
) {
	t.Helper()
	var runStatus db.RunStatus
	var runVersion int64
	var runLeaseID pgtype.UUID
	if err := fixture.pool.QueryRow(ctx, `
		SELECT status, state_version, current_run_lease_id
		  FROM runs
		 WHERE id = $1
	`, setup.runID).Scan(&runStatus, &runVersion, &runLeaseID); err != nil {
		t.Fatal(err)
	}
	var conditionState db.WaitState
	var suspensionState db.RunWaitState
	var currentLeaseID, priorLeaseID pgtype.UUID
	var result []byte
	var reasonCode pgtype.Text
	var resumeVersion int64
	if err := fixture.pool.QueryRow(ctx, `
		SELECT condition_state, suspension_state, current_run_lease_id,
		       prior_run_lease_id, condition_result, condition_reason_code,
		       resume_request_version
		  FROM run_waits
		 WHERE id = $1
	`, setup.waitID).Scan(
		&conditionState,
		&suspensionState,
		&currentLeaseID,
		&priorLeaseID,
		&result,
		&reasonCode,
		&resumeVersion,
	); err != nil {
		t.Fatal(err)
	}
	if runStatus != want.runStatus || runVersion != want.runVersion || runLeaseID != want.currentLeaseID ||
		conditionState != want.conditionState || suspensionState != want.suspensionState ||
		currentLeaseID != want.currentLeaseID || priorLeaseID != want.priorLeaseID ||
		string(result) != want.result || reasonCode.String != want.reasonCode ||
		resumeVersion != want.resumeVersion {
		t.Fatalf("state = run %s/v%d/lease %v wait %s/%s/current %v/prior %v/result %s/reason %q/resume v%d; want %+v",
			runStatus, runVersion, runLeaseID, conditionState, suspensionState,
			currentLeaseID, priorLeaseID, result, reasonCode.String, resumeVersion, want)
	}
}

func assertTokenWaitResumeIntents(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	setup tokenWaitReconcileSetup,
	want int,
) {
	t.Helper()
	var count int
	var payloadMatches bool
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*)::integer,
		       coalesce(bool_and(
		           partition_key = $1::uuid::text
		           AND payload = jsonb_build_object(
		               'environmentId', $2::uuid::text,
		               'runId', $3::uuid::text,
		               'runWaitId', $4::uuid::text,
		               'resumeRequestVersion', 1
		           )
		       ), true)
		  FROM outbox_messages
		 WHERE topic = 'run.resume'
		   AND payload ->> 'runId' = $3::uuid::text
	`, setup.workspaceID, fixture.environmentID, setup.runID, setup.waitID).Scan(&count, &payloadMatches); err != nil {
		t.Fatal(err)
	}
	if count != want || !payloadMatches {
		t.Fatalf("Run resume intents = count %d payload_matches %v, want %d/true", count, payloadMatches, want)
	}
}

func TestTokenWaitReconcilerRejectsPendingTokenAuthority(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	setup := newTokenWaitReconcileSetup(t, ctx, fixture, db.RunWaitStateHot, time.Now().Add(time.Hour))
	reconciler, err := NewWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reconciler.ReconcileBatch(ctx, fixture.environmentID, setup.tokenID, 100)
	if !errors.Is(err, ErrWaitAuthority) {
		t.Fatalf("pending Token authority error = %v", err)
	}
	var condition db.WaitState
	if scanErr := fixture.pool.QueryRow(ctx, `SELECT condition_state FROM run_waits WHERE id = $1`, setup.waitID).Scan(&condition); scanErr != nil {
		t.Fatal(scanErr)
	}
	if condition != db.WaitStatePending {
		t.Fatalf("pending Token changed Wait to %s", condition)
	}
}

func tokenWaitTestDigest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

type runLeaseClaimFixture struct {
	pool          *pgxpool.Pool
	queries       *db.Queries
	orgID         uuid.UUID
	projectID     uuid.UUID
	environmentID uuid.UUID
	deploymentID  uuid.UUID
	workerID      uuid.UUID
	base          runtest.Fixture
}

type runLeaseWork struct {
	leaseID uuid.UUID
	runID   uuid.UUID
}

type taskCompletionWork struct {
	workspaceID       uuid.UUID
	baseVersionID     uuid.UUID
	physicalVersionID uuid.UUID
	runtimeID         uuid.UUID
	mountID           uuid.UUID
	workspaceLeaseID  uuid.UUID
}

func newRunLeaseClaimFixture(t *testing.T, _ context.Context) runLeaseClaimFixture {
	t.Helper()
	base := runtest.New(t)
	return runLeaseClaimFixture{
		pool:          base.Pool,
		queries:       db.New(base.Pool),
		orgID:         base.OrgID,
		projectID:     base.ProjectID,
		environmentID: base.EnvironmentID,
		deploymentID:  base.DeploymentID,
		workerID:      base.WorkerID,
		base:          base,
	}
}

func (fixture runLeaseClaimFixture) addWork(
	t *testing.T,
	_ context.Context,
	state string,
	assignedAt time.Time,
) runLeaseWork {
	t.Helper()
	work := fixture.base.AddRunLease(t, state, assignedAt)
	return runLeaseWork{leaseID: work.LeaseID, runID: work.RunID}
}

func startTaskCompletionWork(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	work runLeaseWork,
) taskCompletionWork {
	t.Helper()
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET state = 'running', started_at = claimed_at
		 WHERE id = $1 AND state = 'starting'
	`, work.leaseID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE runs
		   SET status = 'running', state_version = state_version + 1,
		       started_at = (SELECT started_at FROM run_leases WHERE id = $1),
		       active_started_at = (SELECT started_at FROM run_leases WHERE id = $1)
		 WHERE id = $2 AND status = 'queued' AND current_run_lease_id = $1
	`, work.leaseID, work.runID)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		UPDATE run_attempts
		   SET entrypoint_entered_at = (SELECT started_at FROM run_leases WHERE id = $1)
		 WHERE run_id = $2 AND number = 1 AND entrypoint_entered_at IS NULL
	`, work.leaseID, work.runID)
	var authority taskCompletionWork
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.workspace_id, runs.base_workspace_version_id,
		       run_leases.runtime_instance_id, workspace_leases.workspace_mount_id,
		       workspace_leases.id, workspace_leases.base_version_id
		  FROM runs
		  JOIN run_leases ON run_leases.id = runs.current_run_lease_id
		  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
		 WHERE runs.id = $1
	`, work.runID).Scan(
		&authority.workspaceID,
		&authority.baseVersionID,
		&authority.runtimeID,
		&authority.mountID,
		&authority.workspaceLeaseID,
		&authority.physicalVersionID,
	); err != nil {
		t.Fatal(err)
	}
	return authority
}

func mustRunLeaseExec(t *testing.T, ctx context.Context, executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, query string, args ...any) {
	t.Helper()
	runtest.MustExec(t, ctx, executor, query, args...)
}

func shortRunLeaseID(id uuid.UUID) string {
	return runtest.ShortID(id)
}
